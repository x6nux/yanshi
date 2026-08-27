//go:build windows

package sandbox

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// This file is the Windows enforcement backend. It is a Job Object backend and
// nothing more, and the "nothing more" is the part that must not be lost:
//
//   - It DOES contain process lifetime. Every child prepared through this
//     sandbox is assigned to one job created with
//     JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE, so closing the job handle — which
//     also happens when this process exits or is killed, because the kernel
//     closes handles for us — terminates the whole descendant tree. Windows has
//     no process groups and no cgroups; a killed cmd.exe otherwise leaves its
//     grandchildren orphaned with no reliable way to enumerate them. This is a
//     real guarantee the platform does not otherwise offer, and it is what
//     CanKillTree=true means here.
//
//   - It does NOT contain access. A process inside a job holds the same token
//     it would have held outside one, so it reads and writes exactly what the
//     parent could. The configured AccessTier and NetworkDeny are therefore
//     reported as unenforced, and Effective stays DegradedHostGuard. See
//     windowsJobReport for why that is the correct answer and not excessive
//     modesty.
//
// # Why AppContainer is not here
//
// reference/QwenPaw's windows_appcontainer_sandbox.py is the design that DOES
// isolate access: CreateAppContainerProfile for a package SID, inheritable ACEs
// granting that SID access to the workspace via SetEntriesInAclW +
// SetNamedSecurityInfoW, then a hand-built PROC_THREAD_ATTRIBUTE_LIST carrying
// PROC_THREAD_ATTRIBUTE_SECURITY_CAPABILITIES into CreateProcessW with
// EXTENDED_STARTUPINFO_PRESENT.
//
// The blocker is the last step, and it is structural rather than a matter of
// effort. os/exec offers no way to supply a STARTUPINFOEX: syscall.SysProcAttr
// on Windows exposes CreationFlags, Token, the two SecurityAttributes,
// AdditionalInheritedHandles and ParentProcess — and the runtime builds its own
// _STARTUPINFOEXW with a two-slot attribute list it fills itself
// (PROC_THREAD_ATTRIBUTE_PARENT_PROCESS and _HANDLE_LIST). There is no seam for
// a third attribute. Delivering AppContainer therefore means calling
// CreateProcessW directly and reimplementing what os/exec does around it —
// handle inheritance, the three std handles, quoting, and the *os.Process the
// rest of yanshi's shell layer is built on. That is a new spawn backend, not a
// sandbox adapter, and it would bypass the pipe and console plumbing in
// internal/shell that every tool depends on.
//
// A second reason not to rush it: AppContainer's isolation is enforced through
// ACLs on the workspace granting a per-profile SID. Those ACEs persist on disk
// after the process exits, and QwenPaw needs an atexit sweep plus a
// failed-cleanup directory for the cases where removal fails. Getting that
// wrong leaves permission grants scattered across an operator's source tree.
//
// So this backend claims what it delivers. The honest consequence is that
// filesystem and network policy on Windows remain the host guard's job, and the
// report says so in the field bootstrap logs.

// jobProbeTimeout bounds the enforcement self-check.
//
// The check spawns one real process and waits for the kernel to kill it, so
// unlike a pure API-return check it can in principle hang — a job object whose
// termination is blocked by a stuck kernel driver would never report the child
// as dead. Bootstrap constructs this backend synchronously, so an unbounded wait
// there is a hung startup, and the fail-closed answer to "the mechanism did not
// respond" is to report degraded rather than to keep waiting.
const jobProbeTimeout = 10 * time.Second

// jobProbePoll is how often the probe re-checks whether the child died.
//
// Kill-on-job-close is asynchronous: CloseHandle returns as soon as the kernel
// has queued the termination, so an immediate liveness check races it. Polling
// is used rather than WaitForSingleObject on the child handle because the probe
// holds the child through os/exec, and mixing a raw wait with the runtime's own
// reaping of the same process is how a Wait ends up returning to the wrong
// caller.
const jobProbePoll = 5 * time.Millisecond

// jobobject is the Windows backend.
//
// The job handle is created once at construction and lives until Close, because
// that lifetime IS the mechanism: kill-on-job-close fires when the last handle
// closes, so a per-Prepare job would kill each child as soon as it was set up.
//
// mu guards handle against a Close concurrent with a PostStart. Both are
// reachable from different goroutines in production — Close runs on the
// shutdown path while a launch may still be in flight — and assigning a process
// to a handle another goroutine just closed is a use-after-free at the Win32
// layer, not a Go-visible error.
type jobobject struct {
	cfg    Config
	report CapabilityReport

	mu     sync.Mutex
	handle windows.Handle

	warnOnce sync.Once
}

// Compile-time proof that this backend satisfies both halves of the contract.
// Without the second assertion a signature drift in PostStartSandbox would not
// break the build here — it would make the type assertion at the launch site
// fail at RUNTIME, silently, and every child would run uncontained while the
// report kept claiming CanKillTree.
var (
	_ Sandbox          = (*jobobject)(nil)
	_ PostStartSandbox = (*jobobject)(nil)
)

// newPlatformSandbox builds the Windows backend, running the enforcement
// self-check before deciding what to report.
//
// The self-check runs at CONSTRUCTION for the same two reasons as darwin's:
// bootstrap logs the security posture during startup and an operator reading
// that line needs it to have been true when printed, and Prepare has no channel
// to report "I could not contain this" other than failing every spawn.
//
// When the check fails this returns a backend whose report says so, whose
// CanKillTree is false, and whose Prepare and PostStart do nothing. It does not
// claim containment it cannot deliver.
func newPlatformSandbox(cfg Config) Sandbox {
	handle, probe := probeJobObject()
	sb := &jobobject{cfg: cfg, handle: handle, report: windowsJobReport(cfg, probe)}
	if !probe.enforcing() && handle != 0 {
		// A job we will not use must not be held: it is a kernel handle, and one
		// created with kill-on-job-close that we keep but never assign anything
		// to is a booby trap for any future code that reaches for s.handle.
		_ = windows.CloseHandle(handle)
		sb.handle = 0
	}
	return sb
}

// probeJobObject determines whether this host really gives us a containing job,
// returning the job handle to keep on success.
//
// # Why it spawns a process
//
// Because the API return codes cannot answer the question. CreateJobObject and
// SetInformationJobObject both succeed on a host where the job is then ignored
// — that is what happens today inside a Windows container or under some
// nested-job configurations, where the outer job's limits win and
// AssignProcessToJobObject can fail with ERROR_ACCESS_DENIED at the moment it
// matters rather than at setup. It is also what a hypothetical future Windows
// that stubbed jobs out would look like.
//
// So the probe asserts the OBSERVABLE outcome: a real child, verified alive
// after assignment, must DIE when the job handle closes. That is the exact
// analogue of the darwin backend's second probe asserting a denial rather than a
// success, and it exists because the first kind of check passes on a host where
// nothing is enforced.
//
// # Why a second, throwaway job
//
// The probe kills what it contains, so it cannot use the job the sandbox will
// keep. It creates its own, assigns a child, closes it, and confirms the death;
// the returned handle is a separate, unused job for the caller to keep.
func probeJobObject() (windows.Handle, jobProbe) {
	keep, err := createKillOnCloseJob()
	if err != nil {
		return 0, jobProbe{Detail: err.Error()}
	}
	probe := runJobProbe()
	return keep, probe
}

// runJobProbe performs the create/assign/close/observe cycle on a throwaway job.
func runJobProbe() jobProbe {
	job, err := createKillOnCloseJob()
	if err != nil {
		return jobProbe{Detail: err.Error()}
	}
	// Created and LimitsApplied are both established by createKillOnCloseJob
	// returning without error: it does the CreateJobObject call and the
	// SetInformationJobObject call, and reports which one failed in the error
	// text. Splitting them into separate booleans here is what lets the Reason
	// name the failing step.
	p := jobProbe{Created: true, LimitsApplied: true}

	ctx, cancel := context.WithTimeout(context.Background(), jobProbeTimeout)
	defer cancel()

	// The probe child must (a) exist on every Windows install, (b) stay alive
	// long enough to be assigned and observed, and (c) exit on its own if this
	// probe dies before closing the job. `waitfor` is not used because it is
	// absent on some Server SKUs; cmd.exe with a timeout redirected from NUL is
	// present everywhere Windows is. `timeout` refuses to run with redirected
	// input, hence ping's loopback form, which sleeps ~1s per iteration.
	cmd := exec.CommandContext(ctx, comspec(), "/c", "ping -n 30 127.0.0.1 >NUL")
	cmd.SysProcAttr = &windows.SysProcAttr{
		// No console window for a probe that runs during startup. Without this
		// a GUI-launched yanshi flashes a window on every construction.
		CreationFlags: windows.CREATE_NO_WINDOW,
	}
	// The probe's own environment is irrelevant to the result and inheriting the
	// operator's would put credentials into a child for no reason.
	cmd.Env = []string{}
	if err := cmd.Start(); err != nil {
		_ = windows.CloseHandle(job)
		p.Detail = fmt.Sprintf("could not start a probe process to verify containment: %v", err)
		return p
	}
	// Reap in the background unconditionally. The probe is about to have this
	// child killed under it, and an unreaped process handle is a leak on every
	// path out of this function, including the ones that report failure.
	reaped := make(chan struct{})
	go func() { _ = cmd.Wait(); close(reaped) }()

	if err := assignPIDToJob(job, cmd.Process.Pid); err != nil {
		_ = cmd.Process.Kill()
		<-reaped
		_ = windows.CloseHandle(job)
		p.Detail = fmt.Sprintf("a job was created but a process could not be assigned to it: %v", err)
		return p
	}
	// Liveness BEFORE the close is what makes the death afterwards meaningful.
	// Without it a child that failed to start its payload and exited on its own
	// would be indistinguishable from one the job killed, and the probe would
	// report containment on a host that has none.
	if !processAlive(cmd.Process.Pid) {
		_ = cmd.Process.Kill()
		<-reaped
		_ = windows.CloseHandle(job)
		p.Detail = "the probe process was not alive after assignment, so its later " +
			"death could not be attributed to the job"
		return p
	}

	// The moment under test.
	if err := windows.CloseHandle(job); err != nil {
		_ = cmd.Process.Kill()
		<-reaped
		p.Detail = fmt.Sprintf("closing the job handle failed: %v", err)
		return p
	}

	deadline := time.Now().Add(jobProbeTimeout)
	for {
		select {
		case <-reaped:
			// The runtime reaped it, which on Windows means the process object
			// signalled: it is gone, and the only thing that changed was the
			// job handle closing.
			p.KillOnCloseObserved = true
			return p
		default:
		}
		if time.Now().After(deadline) {
			_ = cmd.Process.Kill()
			<-reaped
			p.Detail = fmt.Sprintf("a process assigned to a KILL_ON_JOB_CLOSE job was "+
				"still running %s after the job handle was closed; job objects do not "+
				"contain on this host", jobProbeTimeout)
			return p
		}
		time.Sleep(jobProbePoll)
	}
}

// comspec resolves the command interpreter used by the probe.
//
// COMSPEC is honoured when set because a host may legitimately relocate it, and
// the literal fallback covers the case where the variable was stripped — which
// is the normal state inside yanshi's own launch path, since
// childLaunchPosture rebuilds the child environment.
func comspec() string {
	if v := os.Getenv("COMSPEC"); v != "" {
		return v
	}
	return `C:\Windows\System32\cmd.exe`
}

// createKillOnCloseJob creates a job object with kill-on-job-close set.
//
// The error text names WHICH call failed, because the two failures mean
// different things to an operator: a refused CreateJobObject is usually a
// handle-quota or policy problem, while a refused SetInformationJobObject means
// the kernel would not accept the limit structure and the job would exist
// without containing anything.
//
// An unnamed job is created deliberately. A named one would be shared with any
// other process that opened the same name, so two yanshi instances would land
// in one job and either instance's Close would kill the other's children.
func createKillOnCloseJob() (windows.Handle, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return 0, fmt.Errorf("CreateJobObject failed: %w", err)
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
			// KILL_ON_JOB_CLOSE is the entire point: it makes the job's
			// lifetime the children's lifetime. Without it a job is a
			// bookkeeping object and killing the tree would mean enumerating
			// pids, which is the unreliable approach this replaces.
			//
			// BREAKAWAY_OK is deliberately NOT set. With it, a child that
			// passes CREATE_BREAKAWAY_FROM_JOB escapes containment on request,
			// which is precisely what an untrusted child would do.
			LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
		},
	}
	if _, err := windows.SetInformationJobObject(
		job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	); err != nil {
		_ = windows.CloseHandle(job)
		return 0, fmt.Errorf("SetInformationJobObject(KILL_ON_JOB_CLOSE) failed: %w", err)
	}
	return job, nil
}

// assignPIDToJob opens pid and assigns it to job.
//
// The requested access is the documented minimum for the call —
// PROCESS_SET_QUOTA and PROCESS_TERMINATE — rather than PROCESS_ALL_ACCESS.
// Asking for more than is needed is how a call starts failing on a hardened host
// for reasons unrelated to what it is doing.
//
// The handle opened here is closed before returning: it is a second, independent
// handle to the process, and the job membership it establishes is a property of
// the process, not of this handle. The runtime's own handle (held until Wait)
// keeps the pid from being recycled.
func assignPIDToJob(job windows.Handle, pid int) error {
	if pid <= 0 {
		return fmt.Errorf("refusing to assign pid %d to a job", pid)
	}
	h, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		return fmt.Errorf("OpenProcess(%d) failed: %w", pid, err)
	}
	defer windows.CloseHandle(h)
	if err := windows.AssignProcessToJobObject(job, h); err != nil {
		return fmt.Errorf("AssignProcessToJobObject(%d) failed: %w", pid, err)
	}
	return nil
}

// processAlive reports whether pid is still running.
//
// GetExitCodeProcess with STILL_ACTIVE is the check rather than "does OpenProcess
// succeed", because a handle can be opened to an already-exited process object
// for as long as someone holds a handle to it — which is exactly the state the
// probe is in, since the Go runtime is holding one. So the open-succeeds test
// would answer "alive" for a process that is dead, which is the answer that
// would make the probe report containment on a host with none.
//
// A failure to determine the state is reported as NOT alive. In the probe's use
// that is the conservative direction: it turns an unanswerable liveness question
// into "cannot attribute the death to the job", i.e. degraded.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return false
	}
	// STILL_ACTIVE (259) is not exported by x/sys/windows; it is STATUS_PENDING.
	// The well-known caveat applies — a process that genuinely exits with 259 is
	// indistinguishable from a running one — and it does not matter here: the
	// probe controls its own child and that child exits 0 or is terminated.
	return code == 259
}

// Prepare is where the Windows backend does NOT rewrite the command.
//
// Unlike darwin, there is nothing to put in front of the program: the mechanism
// is a handle the started process gets attached to, which happens in PostStart.
// Prepare exists to satisfy the interface, to validate that there is a program
// to run at all, and to emit the one-shot degraded warning.
//
// Returning nil having changed nothing is safe for the same reason it is on
// darwin: Report() already told the truth, so nothing downstream can read
// containment into a backend that has none.
func (s *jobobject) Prepare(_ context.Context, cmd *exec.Cmd, spec CommandSpec) error {
	if !s.report.CanKillTree {
		s.warnDegraded()
		return nil
	}
	if cmd == nil {
		return fmt.Errorf("sandbox: windows Prepare received a nil command")
	}
	if cmd.Path == "" && spec.Path == "" {
		return fmt.Errorf("sandbox: windows Prepare received a command with no program")
	}
	return nil
}

// PostStart binds a started process into the sandbox-wide job.
//
// See PostStartSandbox.PostStart for the contract. Two points specific to this
// implementation:
//
// On failure the process is TERMINATED before the error is returned. The caller
// was told by Report() that children are contained; handing back a running,
// uncontained process would make that report false for this launch, and the
// caller has no way to notice. Termination uses a fresh PROCESS_TERMINATE
// handle rather than os.Process.Kill because this method is given a pid, not a
// handle — and the pid is still unambiguous precisely because the caller has not
// yet reaped.
//
// The lock is held across the assignment so a concurrent Close cannot close the
// handle between the nil check and the Win32 call. Under Close the handle field
// is zeroed, so a PostStart that loses the race sees 0 and reports it instead of
// passing a dangling handle to the kernel.
func (s *jobobject) PostStart(pid int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.report.CanKillTree {
		// Degraded: nothing to attach to. Same rule as Prepare — a host without
		// the mechanism gets a downgrade, not an outage.
		return nil
	}
	if s.handle == 0 {
		if err := terminatePID(pid); err != nil {
			return fmt.Errorf("sandbox: job is closed and pid %d could not be terminated "+
				"(it is running uncontained): %w", pid, err)
		}
		return fmt.Errorf("sandbox: job object already closed; refusing to launch an "+
			"uncontained child (pid %d was terminated)", pid)
	}
	if err := assignPIDToJob(s.handle, pid); err != nil {
		if kerr := terminatePID(pid); kerr != nil {
			return fmt.Errorf("sandbox: %w; and the uncontained child (pid %d) could "+
				"not be terminated either: %v", err, pid, kerr)
		}
		return fmt.Errorf("sandbox: %w (the uncontained child was terminated)", err)
	}
	return nil
}

// terminatePID kills pid, treating an already-dead process as success.
//
// "Already gone" is the outcome the caller wanted, and reporting it as an error
// would turn a successful containment failure recovery into a second, misleading
// error. ERROR_INVALID_PARAMETER from OpenProcess is how Windows says "no such
// process", which is why it is folded in rather than propagated.
func terminatePID(pid int) error {
	if pid <= 0 {
		return nil
	}
	h, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		if err == windows.ERROR_INVALID_PARAMETER {
			return nil
		}
		return err
	}
	defer windows.CloseHandle(h)
	// Exit code 1 rather than 0: a caller inspecting the child's status must not
	// read a forced termination as a clean success.
	if err := windows.TerminateProcess(h, 1); err != nil {
		if err == windows.ERROR_ACCESS_DENIED && !processAlive(pid) {
			// Racing its own exit. Access is denied on an exited process, and
			// the process being gone is the requested state.
			return nil
		}
		return err
	}
	return nil
}

// warnDegraded prints the degraded reason once per sandbox.
//
// Once, not once per spawn: a line in front of the operator for every git
// invocation the model makes is how a real warning becomes scroll-back nobody
// reads. bootstrap already logs the posture through slog at startup; this covers
// sandboxes constructed outside that path.
func (s *jobobject) warnDegraded() {
	s.warnOnce.Do(func() {
		fmt.Fprintf(os.Stderr, "yanshi: sandbox not containing on windows: %s\n",
			strings.TrimSpace(s.report.Reason))
	})
}

// Report returns the capability report computed at construction time.
//
// Cached rather than re-probed: the escalation path in internal/tools asks on
// every spawn, and re-running a probe that spawns a process per question would
// be a measurable cost for an answer that cannot change — whether this host's
// kernel honours job objects is a property of the host, not of the moment.
func (s *jobobject) Report() CapabilityReport { return s.report }

// Close closes the job handle, which is the moment kill-on-job-close fires and
// every surviving child dies.
//
// This is the one Close in this package that does real work, and callers should
// understand what it means: it is not a resource-release courtesy, it is the
// tree kill. A caller that wants children to outlive the sandbox must not hold
// this object.
//
// Idempotent — the handle is zeroed under the lock — because Close is reachable
// from both the normal shutdown path and a deferred cleanup, and CloseHandle on
// an already-closed handle is an invalid-handle exception rather than a
// well-behaved error return.
//
// Note that a crash does NOT leak the tree: the kernel closes handles when a
// process dies, so the job closes and the children still die. That property is
// the reason this mechanism is worth having on a platform where nothing else
// provides it.
func (s *jobobject) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.handle == 0 {
		return nil
	}
	h := s.handle
	s.handle = 0
	return windows.CloseHandle(h)
}
