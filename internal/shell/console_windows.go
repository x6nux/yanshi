//go:build windows

package shell

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

// This file is the Windows PTY backend, built on ConPTY
// (CreatePseudoConsole, Windows 10 1809 / build 17763 and later).
//
// # Why it cannot go through os/exec
//
// Attaching a child to a pseudoconsole means passing
// PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE in an extended STARTUPINFOEX, and Go's
// syscall.SysProcAttr on Windows exposes no way to add an arbitrary
// proc-thread attribute — the only one it forwards is PARENT_PROCESS. So the
// spawn here calls windows.CreateProcess directly and wraps the result to
// satisfy the Process interface, rather than borrowing exec.Cmd. Everything
// that exec.Cmd would have done for us (command-line composition, the
// environment block, reaping) is done explicitly below.
//
// # Why the exit watcher exists
//
// ConPTY owns the write end of the output pipe and keeps it open until
// ClosePseudoConsole, INDEPENDENTLY of whether the attached client has exited.
// Manager.pump reads the console to EOF and only then reaps, so leaving the
// close to the caller's teardown deadlocks every finished session. The watcher
// goroutine started by StartPTYProcess waits on the process handle and closes
// the pseudoconsole the moment the client is gone, which is what turns the
// child's exit into an EOF the pump can see.

// ptyBackend names the mechanism, for the capability report.
const ptyBackend = "windows ConPTY (CreatePseudoConsole)"

// PlatformPTYCapability reports the Windows PTY state by actually creating a
// pseudoconsole and closing it again.
//
// A real probe rather than a version check: CreatePseudoConsole is resolved
// lazily from kernel32 and is simply absent before Windows 10 1809, so the
// honest answer on an older host is the error the loader returns, named in the
// Reason. Reading the OS build number instead would put the decision in a table
// that has to be maintained against every Windows Server SKU.
func PlatformPTYCapability() PTYCapability {
	pc, err := newPseudoConsole(defaultPTYRows, defaultPTYCols)
	if err != nil {
		return PTYCapability{
			Platform:  runtime.GOOS,
			Backend:   ptyBackend,
			Reason:    err.Error(),
			Available: false,
		}
	}
	pc.close()
	return PTYCapability{
		Platform:  runtime.GOOS,
		Backend:   ptyBackend,
		Reason:    "created a pseudoconsole and released it",
		Available: true,
	}
}

// defaultPTYRows and defaultPTYCols are the geometry a freshly created
// pseudoconsole is given. See the Unix backend for why a zero size is not a
// neutral default.
const (
	defaultPTYRows = 24
	defaultPTYCols = 80
)

// pseudoConsole owns one ConPTY plus the two pipe ends the parent keeps.
//
// in is what the parent writes (the child's keyboard); out is what the parent
// reads (the child's screen). The two ends handed to ConPTY are closed
// immediately after creation because CreatePseudoConsole duplicates them, and
// holding the originals would keep the output pipe alive after the child exits.
type pseudoConsole struct {
	handle windows.Handle
	in     *os.File
	out    *os.File

	closeOnce sync.Once
	// conptyOnce guards ClosePseudoConsole specifically: both the exit watcher
	// and Console.Close can reach it, and the second call on a freed HPCON is
	// undefined rather than a no-op.
	conptyOnce sync.Once
}

// newPseudoConsole creates a ConPTY and the pipe pair around it.
func newPseudoConsole(rows, cols uint16) (*pseudoConsole, error) {
	childIn, parentIn, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("shell: conpty input pipe: %w", err)
	}
	parentOut, childOut, err := os.Pipe()
	if err != nil {
		_ = childIn.Close()
		_ = parentIn.Close()
		return nil, fmt.Errorf("shell: conpty output pipe: %w", err)
	}
	var hpc windows.Handle
	err = windows.CreatePseudoConsole(
		windows.Coord{X: int16(cols), Y: int16(rows)},
		windows.Handle(childIn.Fd()),
		windows.Handle(childOut.Fd()),
		0,
		&hpc,
	)
	// ConPTY duplicated both ends it was given; the originals are ours to drop
	// whether or not creation succeeded.
	_ = childIn.Close()
	_ = childOut.Close()
	if err != nil {
		_ = parentIn.Close()
		_ = parentOut.Close()
		return nil, fmt.Errorf("shell: CreatePseudoConsole: %w", err)
	}
	return &pseudoConsole{handle: hpc, in: parentIn, out: parentOut}, nil
}

// closeConPTY releases the pseudoconsole exactly once. Releasing it is what
// closes ConPTY's copy of the output pipe, so the parent's read reaches EOF.
func (p *pseudoConsole) closeConPTY() {
	p.conptyOnce.Do(func() { windows.ClosePseudoConsole(p.handle) })
}

// close releases the pseudoconsole and both pipe ends.
func (p *pseudoConsole) close() {
	p.closeOnce.Do(func() {
		p.closeConPTY()
		_ = p.in.Close()
		_ = p.out.Close()
	})
}

// StartPTYProcess spawns spec.Program attached to a pseudoconsole.
//
// The returned Console reads the child's rendered screen (ANSI escape
// sequences included — ConPTY is a terminal emulator and emits VT, which is
// exactly what a caller rendering the stream wants) and writes to its keyboard.
//
// spec.SeparateStderr is ignored for the same reason as on Unix: a console has
// one stream. ptyConsole does not implement StderrConsole, so separatedStderr
// hands callers the empty reader rather than a duplicate of the same bytes.
func StartPTYProcess(ctx context.Context, spec LaunchSpec) (Process, Console, error) {
	if spec.Program == "" {
		return nil, nil, fmt.Errorf("shell: spec.Program required")
	}
	pc, err := newPseudoConsole(defaultPTYRows, defaultPTYCols)
	if err != nil {
		return nil, nil, err
	}
	proc, err := startConPTYClient(ctx, pc, spec)
	if err != nil {
		pc.close()
		return nil, nil, err
	}
	return proc, &ptyConsole{pc: pc}, nil
}

// startConPTYClient performs the CreateProcess half: attribute list, extended
// startup info, spawn, and the exit watcher.
func startConPTYClient(ctx context.Context, pc *pseudoConsole, spec LaunchSpec) (Process, error) {
	attrs, err := windows.NewProcThreadAttributeList(1)
	if err != nil {
		return nil, fmt.Errorf("shell: NewProcThreadAttributeList: %w", err)
	}
	defer attrs.Delete()
	// The attribute value IS the HPCON, passed by value rather than by
	// address (see the CreatePseudoConsole sample in the Windows console docs).
	// The double conversion through a *unsafe.Pointer is what keeps go vet's
	// unsafeptr analyser from reading a handle-to-pointer conversion as pointer
	// arithmetic; the bits are identical either way.
	handle := pc.handle
	if err := attrs.Update(
		windows.PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE,
		*(*unsafe.Pointer)(unsafe.Pointer(&handle)),
		unsafe.Sizeof(handle),
	); err != nil {
		return nil, fmt.Errorf("shell: UpdateProcThreadAttribute(PSEUDOCONSOLE): %w", err)
	}

	si := &windows.StartupInfoEx{
		StartupInfo:             windows.StartupInfo{Cb: uint32(unsafe.Sizeof(windows.StartupInfoEx{}))},
		ProcThreadAttributeList: attrs.List(),
	}
	cmdline, err := windows.UTF16PtrFromString(windows.ComposeCommandLine(append([]string{spec.Program}, spec.Args...)))
	if err != nil {
		return nil, err
	}
	var dir *uint16
	if spec.Dir != "" {
		if dir, err = windows.UTF16PtrFromString(spec.Dir); err != nil {
			return nil, err
		}
	}
	env, err := conptyEnvBlock(spec.Env)
	if err != nil {
		return nil, err
	}

	pi := new(windows.ProcessInformation)
	// inheritHandles is false: the child reaches its console through the
	// pseudoconsole attribute, not through inherited std handles, and
	// inheriting would hand it every other inheritable handle this process
	// holds.
	//
	// The two spawns differ only in the token. CreateProcessAsUser is not used
	// unconditionally because it is documented to need SE_ASSIGNPRIMARYTOKEN and
	// SE_INCREASE_QUOTA, which yanshi does not hold — the exemption that makes
	// spec.ProcessToken usable applies only to a token that is a RESTRICTED
	// VERSION of the caller's own, so passing this process's own token through
	// it would start failing on hosts where the plain call works today.
	flags := uint32(windows.EXTENDED_STARTUPINFO_PRESENT | windows.CREATE_UNICODE_ENVIRONMENT)
	var spawnErr error
	if spec.ProcessToken != 0 {
		spawnErr = windows.CreateProcessAsUser(
			windows.Token(spec.ProcessToken),
			nil, cmdline, nil, nil, false, flags, env, dir, &si.StartupInfo, pi,
		)
	} else {
		spawnErr = windows.CreateProcess(
			nil, cmdline, nil, nil, false, flags, env, dir, &si.StartupInfo, pi,
		)
	}
	if spawnErr != nil {
		return nil, fmt.Errorf("shell: CreateProcess %q: %w", spec.Program, spawnErr)
	}
	_ = windows.CloseHandle(pi.Thread)

	// os.FindProcess opens its own handle, which is what gives Wait the
	// *os.ProcessState the Process contract is written against. It is called
	// while pi.Process is still open, so the pid cannot have been recycled
	// underneath it.
	osProc, findErr := os.FindProcess(int(pi.ProcessId))
	if findErr != nil {
		_ = windows.TerminateProcess(pi.Process, 1)
		_ = windows.CloseHandle(pi.Process)
		return nil, fmt.Errorf("shell: cannot open the process just created: %w", findErr)
	}
	go watchConPTYClient(ctx, pi.Process, pc, osProc)
	return &conptyProcess{proc: osProc}, nil
}

// watchConPTYClient closes the pseudoconsole once the client exits, and kills
// the client when ctx is cancelled.
//
// Both halves are here rather than split because they share the one process
// handle this goroutine owns. Closing the pseudoconsole at client exit is what
// makes the parent's read reach EOF; the ctx branch is the equivalent of
// exec.CommandContext's cancellation, which this path does not get for free.
func watchConPTYClient(ctx context.Context, handle windows.Handle, pc *pseudoConsole, osProc *os.Process) {
	defer windows.CloseHandle(handle)
	defer pc.closeConPTY()
	if ctx == nil {
		_, _ = windows.WaitForSingleObject(handle, windows.INFINITE)
		return
	}
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		_, _ = windows.WaitForSingleObject(handle, windows.INFINITE)
	}()
	select {
	case <-exited:
	case <-ctx.Done():
		_ = osProc.Kill()
		<-exited
	}
}

// conptyEnvBlock renders a KEY=VALUE slice as the doubly-NUL-terminated UTF-16
// block CreateProcess expects, or nil to inherit this process's environment.
//
// nil for an empty slice is deliberate and matches exec.Cmd: a caller that set
// no environment wants the parent's, and handing CreateProcess an empty block
// would give the child NO environment at all — no PATH, no SystemRoot, which on
// Windows breaks even the loader.
func conptyEnvBlock(env []string) (*uint16, error) {
	if len(env) == 0 {
		return nil, nil
	}
	var block []uint16
	for _, entry := range env {
		encoded, err := windows.UTF16FromString(entry)
		if err != nil {
			return nil, fmt.Errorf("shell: environment entry is not valid UTF-16: %w", err)
		}
		block = append(block, encoded...) // UTF16FromString already NUL-terminates
	}
	block = append(block, 0)
	return &block[0], nil
}

// conptyProcess adapts the *os.Process behind a ConPTY client to the Process
// interface.
//
// Wait forwards through (*os.Process).Wait and converts a non-zero exit into
// *exec.ExitError, because that is what the Process contract promises and what
// Manager.exitCodeFrom unwraps. Synthesising the error from a raw
// GetExitCodeProcess call is not an option: exec.ExitError carries an
// *os.ProcessState, which only the os package can mint.
type conptyProcess struct {
	proc *os.Process

	mu    sync.Mutex
	state *os.ProcessState
	err   error
	done  bool
}

func (p *conptyProcess) Wait() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.done {
		p.state, p.err = p.proc.Wait()
		p.done = true
	}
	if p.err != nil {
		return p.err
	}
	if p.state != nil && !p.state.Success() {
		return &exec.ExitError{ProcessState: p.state}
	}
	return nil
}

func (p *conptyProcess) PID() int { return p.proc.Pid }

func (p *conptyProcess) Kill() error { return p.proc.Kill() }

// Capabilities reports the platform's tree-kill answer, which on Windows is
// still no: terminating the client does not reap its descendants until the Job
// Object work lands. ConPTY does not change that — it is a terminal, not a
// containment boundary.
func (p *conptyProcess) Capabilities() ProcessCapabilities {
	return ProcessCapabilities{CanKillTree: CanKillTreeOnPlatform()}
}

// ptyConsole is the Console over a pseudoconsole.
type ptyConsole struct {
	pc *pseudoConsole
}

func (c *ptyConsole) Read(b []byte) (int, error)  { return c.pc.out.Read(b) }
func (c *ptyConsole) Write(b []byte) (int, error) { return c.pc.in.Write(b) }

// Resize changes the console geometry, which ConPTY renders against and the
// client observes through GetConsoleScreenBufferInfo.
func (c *ptyConsole) Resize(rows, cols uint16) error {
	if err := windows.ResizePseudoConsole(c.pc.handle, windows.Coord{X: int16(cols), Y: int16(rows)}); err != nil {
		return fmt.Errorf("shell: ResizePseudoConsole: %w", err)
	}
	return nil
}

func (c *ptyConsole) PTY() bool { return true }

// Close releases the pseudoconsole and both pipes. Safe to call alongside the
// exit watcher, which closes the same pseudoconsole under its own sync.Once.
func (c *ptyConsole) Close() error {
	c.pc.close()
	return nil
}

// CanKillTreeOnPlatform reports whether the OS factory can kill a process and
// all its descendants. On Windows this is still false — the Job Object
// approach (which closes the handle on process exit and cascades the kill to
// children) is the obvious candidate but not yet wired. When the real
// implementation lands, flip this to true.
func CanKillTreeOnPlatform() bool { return false }
