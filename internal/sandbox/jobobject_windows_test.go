//go:build windows

package sandbox

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// This file holds the tests that require a real Windows kernel: they create
// real job objects, spawn real processes, and assert what the kernel did.
//
// ⚠️ HONESTY NOTE, and it is not boilerplate. This backend was written and
// reviewed on darwin/arm64. NOTHING IN THIS FILE HAS EVER BEEN EXECUTED. It has
// been type-checked by `GOOS=windows go vet ./internal/sandbox`, which compiles
// the test files, so the API usage is verified against x/sys/windows and against
// this package's own symbols — and nothing more than that. The first real run
// will be on a Windows CI leg.
//
// The consequence for anyone reading a green board: the assertions that HAVE run
// are the ones in jobobject_test.go (report honesty, degradation decisions,
// disclosure). Those are the assertions the rest of the system depends on for
// correctness — a wrong report arms tools.ClassifySandboxViolation against every
// failing command — whereas the ones here verify the mechanism actually
// contains. Both matter; only one has evidence.
//
// Every enforcement assertion below is paired with a CONTROL: an identical child
// that is NOT in a job. Without it, "the process died" proves nothing — it could
// have exited on its own, been reaped by the test's own context cancellation, or
// never started. The control is what makes the job object the only remaining
// explanation.

// probeChildArgv is the argv of a child that lives long enough to observe and
// exits on its own if the test dies first.
//
// ping's loopback form is used rather than `timeout`, which refuses to run when
// its input is redirected, and rather than `waitfor`, which is absent on some
// Server SKUs.
func probeChildArgv() (string, []string) {
	return comspec(), []string{"/c", "ping -n 60 127.0.0.1 >NUL"}
}

// startProbeChild spawns a long-lived child and registers its cleanup.
//
// The child is reaped in a goroutine whose completion is observable, because
// every assertion in this file is about WHEN the process died and a pid alone
// cannot answer that: on Windows the id stays valid while a handle is open, so
// polling it reports "alive" long after the process object signalled.
func startProbeChild(t *testing.T) (*exec.Cmd, <-chan struct{}) {
	t.Helper()
	prog, args := probeChildArgv()
	cmd := exec.Command(prog, args...)
	cmd.SysProcAttr = &windows.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
	// probeEnv, not empty: every control in this file rests on the child being
	// alive because we have not killed it, and an empty environment block strips
	// PATH/SystemRoot, so cmd cannot resolve ping and exits within milliseconds.
	// Measured on the first CI windows leg: with `[]string{}` here the control
	// children were dead before their 200ms checks ran, and the degraded-backend
	// test failed because a no-op PostStart "killed" a child that had in fact
	// exited on its own.
	cmd.Env = probeEnv()
	if err := cmd.Start(); err != nil {
		t.Fatalf("could not start a probe child: %v", err)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	t.Cleanup(func() {
		select {
		case <-done:
		default:
			_ = cmd.Process.Kill()
			<-done
		}
	})
	return cmd, done
}

// jobTestsOptionalEnv lets a host on which job objects demonstrably do not
// contain — a nested job inside a Windows container is the real case —
// downgrade the enforcement tests below from a failure to a skip.
//
// It must be set EXPLICITLY. See requireContainingJob.
const jobTestsOptionalEnv = "YANSHI_JOBOBJECT_TESTS_OPTIONAL"

// requireContainingJob fails, rather than skips, when this host does not give
// the backend a containing job (W-B-24).
//
// # Why there is no unconditional "unsupported" branch here
//
// Unlike seccomp and Landlock, job objects have no kernel-config or ABI
// question to ask: every Windows that can run this binary has had them since
// Windows 2000, and this whole file is //go:build windows, so GOOS carries no
// information either. There is therefore no state that legitimately answers
// "this platform does not have the mechanism" — which means every skip in this
// file was, by construction, a run that did not happen rather than a capability
// that is absent.
//
// That matters because W-B-24's acceptance is "在 CI windows leg 上实测进程树限制
// 生效". A leg that quietly stops exercising containment and reports pass leaves
// the item permanently pending while the board says green, which is the exact
// shape B3 had to go back and fix for seccomp.
//
// The one genuine environmental case is a nested job: inside a Windows
// container, or under an outer job that refuses AssignProcessToJobObject, the
// mechanism really is unavailable and no change to this code helps. A runner in
// that state sets jobTestsOptionalEnv where it is configured — one deliberate
// act, recorded next to the runner, instead of a default that answers "fine"
// for every host including the healthy ones.
func requireContainingJob(t *testing.T, rep CapabilityReport) {
	t.Helper()
	if rep.CanKillTree {
		return
	}
	if os.Getenv(jobTestsOptionalEnv) != "" {
		t.Skipf("job objects do not contain here and %s is set: %s", jobTestsOptionalEnv, rep.Reason)
	}
	t.Fatalf("job objects do not contain on this Windows host: %s\n\n"+
		"This is a FAILURE rather than a skip on purpose. There is no version of Windows "+
		"without job objects, so this is not an unsupported platform — it is either the "+
		"backend being broken (the regression these tests exist to catch) or a nested-job "+
		"environment such as a Windows container. Skipping makes both indistinguishable "+
		"from a verified run, and W-B-24's only evidence is this leg. If this runner "+
		"genuinely cannot contain, set %s where the runner is configured.",
		rep.Reason, jobTestsOptionalEnv)
}

// waitDead reports whether the child was reaped within d.
func waitDead(done <-chan struct{}, d time.Duration) bool {
	select {
	case <-done:
		return true
	case <-time.After(d):
		return false
	}
}

// TestJobObjectProbeAcceptsAWorkingHost asserts the self-check passes here.
//
// It is the counterpart to the negative cases in jobobject_test.go: those pin
// that a failed probe degrades, and this pins that the probe is not simply
// refusing everything — a self-check that never succeeds would degrade the
// backend on every host and the degradation tests would still be green.
func TestJobObjectProbeAcceptsAWorkingHost(t *testing.T) {
	handle, probe := probeJobObject()
	if handle != 0 {
		defer windows.CloseHandle(handle)
	}
	if !probe.enforcing() {
		t.Fatalf("the Job Object self-check failed on a real Windows host: %+v", probe)
	}
	if handle == 0 {
		t.Fatal("the probe succeeded but returned no job handle to keep")
	}
}

// TestKillOnJobCloseTerminatesTheChild is the enforcement assertion: it is the
// reason CanKillTree may be true.
//
// The control is a second, identical child outside the job. It must still be
// running when the in-job child is dead — otherwise the death is explained by
// something other than the job (the ping finishing early, the test host being
// starved) and this test proves nothing.
func TestKillOnJobCloseTerminatesTheChild(t *testing.T) {
	job, err := createKillOnCloseJob()
	if err != nil {
		t.Fatalf("createKillOnCloseJob: %v", err)
	}
	contained, containedDone := startProbeChild(t)
	_, controlDone := startProbeChild(t)

	if err := assignPIDToJob(job, contained.Process.Pid); err != nil {
		_ = windows.CloseHandle(job)
		t.Fatalf("assignPIDToJob: %v", err)
	}
	if !processAlive(contained.Process.Pid) {
		_ = windows.CloseHandle(job)
		t.Fatal("the contained child was not alive after assignment; its later death " +
			"could not be attributed to the job")
	}

	if err := windows.CloseHandle(job); err != nil {
		t.Fatalf("CloseHandle(job): %v", err)
	}

	if !waitDead(containedDone, jobProbeTimeout) {
		t.Fatalf("a child in a KILL_ON_JOB_CLOSE job survived the job handle closing; " +
			"CanKillTree=true would be a false claim on this host")
	}
	if waitDead(controlDone, 200*time.Millisecond) {
		t.Fatal("the control child (never assigned to the job) also died, so the " +
			"contained child's death is not attributable to the job object")
	}
}

// TestKillOnJobCloseTerminatesGrandchildren is the guarantee Windows does not
// otherwise offer, and the actual reason this backend exists.
//
// Killing a shell leaves its grandchildren running: there are no process groups
// on Windows, and walking the parent-pid tree races re-parenting. A job object
// contains descendants transitively, so closing it reaps a grandchild whose
// parent has already exited — the case that motivated the whole mechanism.
func TestKillOnJobCloseTerminatesGrandchildren(t *testing.T) {
	job, err := createKillOnCloseJob()
	if err != nil {
		t.Fatalf("createKillOnCloseJob: %v", err)
	}
	// The outer cmd.exe starts a detached grandchild and exits immediately, so
	// by the time the job closes the grandchild has no live ancestor to be found
	// through.
	cmd := exec.Command(comspec(), "/c", "start /b ping -n 60 127.0.0.1 >NUL")
	cmd.SysProcAttr = &windows.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
	// probeEnv: `start /b` resolves ping through the child's PATH, and an empty
	// environment block means the job is likely empty when queried — the
	// `before == 0` branch below would then fire for a reason that has nothing
	// to do with transitive containment.
	cmd.Env = probeEnv()
	if err := cmd.Start(); err != nil {
		_ = windows.CloseHandle(job)
		t.Fatalf("could not start the parent: %v", err)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	if err := assignPIDToJob(job, cmd.Process.Pid); err != nil {
		_ = cmd.Process.Kill()
		<-done
		_ = windows.CloseHandle(job)
		t.Fatalf("assignPIDToJob: %v", err)
	}
	// Let the parent spawn its grandchild and exit.
	<-done

	// The job now contains only the grandchild. Count it before and after so the
	// assertion is about the job's contents rather than about a pid we guessed.
	before, err := jobProcessCount(job)
	if err != nil {
		_ = windows.CloseHandle(job)
		// QueryInformationJobObject on a job this process just created is not a
		// capability question — the handle carries JOB_OBJECT_QUERY by
		// construction. A failure here is a broken host or a wrong call, and
		// skipping it would remove the only check that the job's CONTENTS are
		// what the transitive claim rests on.
		t.Fatalf("cannot query the process list of a job this test just created: %v", err)
	}
	if before == 0 {
		_ = windows.CloseHandle(job)
		if os.Getenv(jobTestsOptionalEnv) != "" {
			t.Skipf("the grandchild did not join the job and %s is set", jobTestsOptionalEnv)
		}
		// createKillOnCloseJob deliberately does NOT set BREAKAWAY_OK, so a
		// grandchild CANNOT legitimately leave the job: `start /b` inherits job
		// membership. An empty job here means either the transitive containment
		// that is this backend's entire reason for existing does not hold, or
		// the parent never spawned. Both must be loud — a skip here reports
		// success for the one guarantee CanKillTree=true is making.
		t.Fatalf("the job contained no process after the parent exited; either the "+
			"grandchild never started or it left a job created without BREAKAWAY_OK, "+
			"which means the transitive containment CanKillTree promises does not hold "+
			"here.\n\nThis is a FAILURE rather than a skip because transitive "+
			"containment is the whole reason this backend exists. If this runner is a "+
			"nested-job environment, set %s where it is configured.", jobTestsOptionalEnv)
	}
	if err := windows.CloseHandle(job); err != nil {
		t.Fatalf("CloseHandle(job): %v", err)
	}
	// The job handle is gone, so the count cannot be re-queried. Assert the
	// grandchild is dead by its own liveness instead.
	deadline := time.Now().Add(jobProbeTimeout)
	for time.Now().Before(deadline) {
		if !processAlive(int(lastJobPID)) {
			return
		}
		time.Sleep(jobProbePoll)
	}
	t.Fatalf("a grandchild (pid %d) survived the job handle closing; the transitive "+
		"containment CanKillTree promises does not hold on this host", lastJobPID)
}

// lastJobPID carries the pid jobProcessCount last observed, so the grandchild
// test can check liveness after the job handle — the only way to enumerate the
// job — has been closed.
var lastJobPID uint32

// jobProcessCount returns how many processes are currently in job, recording the
// last pid seen in lastJobPID.
//
// JOBOBJECT_BASIC_PROCESS_ID_LIST is variable-length and x/sys/windows does not
// declare it, so the buffer is read as raw bytes and the three header fields
// plus the pid array are decoded by offset. The layout is
// {NumberOfAssignedProcesses, NumberOfProcessIdsInList, ProcessIdList[1]} with
// pointer-sized ids.
func jobProcessCount(job windows.Handle) (int, error) {
	const headerWords = 2
	buf := make([]uintptr, headerWords+64)
	var retlen uint32
	err := windows.QueryInformationJobObject(
		job,
		windows.JobObjectBasicProcessIdList,
		uintptr(unsafe.Pointer(&buf[0])),
		uint32(len(buf)*int(unsafe.Sizeof(uintptr(0)))),
		&retlen,
	)
	if err != nil && !strings.Contains(err.Error(), "more data") {
		return 0, err
	}
	inList := int(buf[1])
	if inList > len(buf)-headerWords {
		inList = len(buf) - headerWords
	}
	for i := range inList {
		lastJobPID = uint32(buf[headerWords+i])
	}
	return inList, nil
}

// TestPostStartContainsTheChildThroughThePublicSeam exercises the path
// production uses: construct the backend through New, hand it a started
// process via the PostStartSandbox seam, and confirm Close reaps it.
//
// It goes through New and a type assertion rather than reaching for the concrete
// type, because the type assertion is what the launch site does — and an
// assertion that stops matching (a signature drift in PostStartSandbox, a
// backend that stops implementing it) fails silently at runtime, leaving every
// child uncontained while Report still claims CanKillTree.
func TestPostStartContainsTheChildThroughThePublicSeam(t *testing.T) {
	sb := New(Config{Enabled: true, WorkspaceRoot: t.TempDir(), Tier: WorkspaceWrite})
	rep := sb.Report()
	requireContainingJob(t, rep)
	ps, ok := sb.(PostStartSandbox)
	if !ok {
		t.Fatal("the Windows backend does not implement PostStartSandbox, so the " +
			"launch site's type assertion fails and every child runs uncontained " +
			"while Report claims CanKillTree")
	}

	child, childDone := startProbeChild(t)
	_, controlDone := startProbeChild(t)

	// Prepare must not rewrite anything on this platform.
	prog, args := probeChildArgv()
	cmd := exec.Command(prog, args...)
	before := len(cmd.Args)
	if err := sb.Prepare(context.Background(), cmd, CommandSpec{Path: prog, Tier: WorkspaceWrite}); err != nil {
		t.Fatalf("Prepare failed for an ordinary command: %v", err)
	}
	if cmd.Path != prog || len(cmd.Args) != before {
		t.Fatalf("the Windows backend rewrote the command; it has no wrapper to insert: %v", cmd.Args)
	}

	if err := ps.PostStart(child.Process.Pid); err != nil {
		t.Fatalf("PostStart refused to contain a freshly started child: %v", err)
	}
	if err := sb.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !waitDead(childDone, jobProbeTimeout) {
		t.Fatal("Close did not terminate the contained child, so CanKillTree is a false claim")
	}
	if waitDead(controlDone, 200*time.Millisecond) {
		t.Fatal("the control child died too; the contained child's death is not " +
			"attributable to the job")
	}

	// Close is idempotent: it is reachable from both the shutdown path and a
	// deferred cleanup, and CloseHandle on a closed handle raises rather than
	// returning an error.
	if err := sb.Close(); err != nil {
		t.Fatalf("a second Close must be a no-op, got: %v", err)
	}
}

// TestPostStartAfterCloseTerminatesRatherThanLeaking pins the fail-closed half
// of the PostStart contract.
//
// Once Close has fired, there is no job to join. Returning nil would hand the
// caller a running process the report claims is contained — the over-claim this
// package exists to prevent — so PostStart must terminate it and say so.
func TestPostStartAfterCloseTerminatesRatherThanLeaking(t *testing.T) {
	sb := New(Config{Enabled: true, WorkspaceRoot: t.TempDir(), Tier: ReadOnly})
	requireContainingJob(t, sb.Report())
	ps, ok := sb.(PostStartSandbox)
	if !ok {
		t.Fatal("the Windows backend does not implement PostStartSandbox")
	}
	if err := sb.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	child, childDone := startProbeChild(t)
	err := ps.PostStart(child.Process.Pid)
	if err == nil {
		t.Fatal("PostStart accepted a process it could not contain; the caller would " +
			"proceed believing the child is inside a job")
	}
	if !strings.Contains(err.Error(), "terminated") {
		t.Errorf("the error does not tell the caller what happened to the child: %v", err)
	}
	if !waitDead(childDone, jobProbeTimeout) {
		t.Fatal("PostStart reported failure but left the uncontained child running")
	}
}

// TestDegradedWindowsBackendDoesNotWrapOrFail pins the behaviour on a host where
// job objects do not contain.
//
// It is built directly rather than by waiting for such a host, for the same
// reason darwin's degradation test is: the whole point of the path is that it is
// unreachable on a healthy machine unless it is reached deliberately.
func TestDegradedWindowsBackendDoesNotWrapOrFail(t *testing.T) {
	cfg := Config{Enabled: true, WorkspaceRoot: t.TempDir(), Tier: WorkspaceWrite}
	sb := &jobobject{
		cfg:    cfg,
		report: windowsJobReport(cfg, jobProbe{Created: true, LimitsApplied: true}),
	}
	rep := sb.Report()
	if rep.CanKillTree || rep.Enforced || rep.Effective == OSIsolated {
		t.Fatalf("a degraded backend claimed a capability: %#v", rep)
	}
	// Prepare must leave the launch path usable rather than failing every spawn.
	prog, args := probeChildArgv()
	cmd := exec.Command(prog, args...)
	if err := sb.Prepare(context.Background(), cmd, CommandSpec{Path: prog, Tier: ReadOnly}); err != nil {
		t.Fatalf("a degraded backend must keep the host-guard path usable: %v", err)
	}
	if cmd.Path != prog {
		t.Fatalf("a degraded backend rewrote the command: %q", cmd.Path)
	}
	_ = args
	// PostStart on a degraded backend is a no-op, NOT a kill: there is nothing
	// to contain, and killing the child would turn a degradation into an outage.
	child, childDone := startProbeChild(t)
	if err := ps(sb).PostStart(child.Process.Pid); err != nil {
		t.Fatalf("a degraded PostStart must be a no-op, got: %v", err)
	}
	if waitDead(childDone, 300*time.Millisecond) {
		t.Fatal("a degraded PostStart killed the child; degradation must not become an outage")
	}
	if err := sb.Close(); err != nil {
		t.Fatalf("Close on a degraded backend: %v", err)
	}
}

// ps narrows a *jobobject to the public seam so the tests exercise the same
// interface the launch site does rather than the concrete method set.
func ps(s *jobobject) PostStartSandbox { return s }
