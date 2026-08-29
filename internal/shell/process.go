package shell

import (
	"context"
	"fmt"
	"io"
	"os/exec"

	"github.com/x6nux/yanshi/internal/secproc"
)

// OSProcessFactory is the legacy ProcessFactory implementation used when a
// shell session is started directly (e.g. via shell_run in tests). The
// SecureProcessFactory in Task 19 wraps this to inject netpolicy.PrepareEnvFor
// (via childLaunchPosture.env, internal/shell/childlaunch.go) and
// Sandbox.Prepare before delegating here.
//
// PTY requests route through StartPTYProcess, which is platform-specific:
// linux and darwin allocate a real pty pair, windows a ConPTY, and any other
// platform returns ErrPTYUnavailable. Callers that want to know before
// spawning ask PlatformPTYCapability.
type OSProcessFactory struct{}

// Start spawns spec.Program with spec.Args over a pipe pair; spec.PTY routes to
// StartPTYProcess instead. cmd.Env is populated from spec.Env when non-empty so
// the child does not inherit the parent's environment — netpolicy.PrepareEnvFor
// is what populates that slice, so stripping the parent's HTTP_PROXY happens
// once at the boundary rather than via fragile parent-env scrubbing here.
//
// The child's two streams are routed per spec.SeparateStderr: false yields a
// Console carrying both, concurrently interleaved (secproc.MergeOutput);
// true yields a StderrConsole whose Read is stdout only.
//
// This used to be io.MultiReader(stdout, stderr) unconditionally, which is a
// concatenation, not a merge: it made stderr unreadable until stdout closed
// (deadlocking any child that filled its stderr buffer first) and left every
// stdout consumer parsing a stream with diagnostics glued onto its tail.
func (OSProcessFactory) Start(ctx context.Context, spec LaunchSpec) (Process, Console, error) {
	if spec.PTY {
		return StartPTYProcess(ctx, spec)
	}
	program, args := spec.Program, spec.Args
	if program == "" {
		return nil, nil, fmt.Errorf("shell: spec.Program required")
	}
	cmd := exec.CommandContext(ctx, program, args...)
	cmd.Dir = spec.Dir
	// Own process group, so Kill can take the whole tree. See killtree_unix.go.
	setProcessGroup(cmd)
	// CommandContext's default cancel calls Process.Kill directly, bypassing
	// the tree kill entirely — so a ctx-cancelled command would reap the shell
	// and leave its children running, which is the exact failure the process
	// group exists to prevent. Both cancellation paths must go through the
	// same function.
	cmd.Cancel = func() error { return killProcessTree(cmd) }
	// After setProcessGroup, which owns SysProcAttr on Unix: applySandboxToken
	// merges into whatever is there rather than replacing it, but doing it in
	// this order means neither function has to know about the other.
	applySandboxToken(cmd, spec.ProcessToken)
	if len(spec.Env) > 0 {
		cmd.Env = spec.Env
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		return nil, nil, err
	}
	// Start before wiring the readers: the merge copies eagerly in
	// goroutines, and a failed Start must not leave them racing the pipes it
	// is closing.
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		_ = stderr.Close()
		return nil, nil, err
	}
	if spec.SeparateStderr {
		return &osProcess{cmd: cmd}, &pipeConsole{r: stdout, stderr: stderr, w: stdin}, nil
	}
	return &osProcess{cmd: cmd}, &pipeConsole{r: secproc.MergeOutput(stdout, stderr), w: stdin}, nil
}

// Capabilities reports what the OS factory can do without launching a process.
// Callers (and tests) probe CanKillTree here before promising tree-level
// cancellation. M1 (v3): a bidirectional test asserts this on BOTH platforms.
func (OSProcessFactory) Capabilities(context.Context) ProcessCapabilities {
	return ProcessCapabilities{CanKillTree: CanKillTreeOnPlatform()}
}

// osProcess adapts *exec.Cmd to the Process interface. The capabilities report
// is platform-derived (CanKillTreeOnPlatform) rather than per-instance so the
// Manager can call it before the process is even started.
type osProcess struct {
	cmd *exec.Cmd
}

func (p *osProcess) Wait() error { return p.cmd.Wait() }
func (p *osProcess) PID() int {
	if p.cmd.Process != nil {
		return p.cmd.Process.Pid
	}
	return 0
}
func (p *osProcess) Kill() error {
	return killProcessTree(p.cmd)
}
func (p *osProcess) Capabilities() ProcessCapabilities {
	return ProcessCapabilities{CanKillTree: CanKillTreeOnPlatform()}
}

// pipeConsole is the Console used for pipe-pair spawns. Write goes to the
// child's stdin pipe; Resize fails (there is no terminal to resize, and
// answering nil would let a caller believe a geometry change took effect);
// PTY() reports false so callers know to render in line-buffered rather than
// terminal mode.
//
// stderr is non-nil only for a LaunchSpec.SeparateStderr spawn, in which case
// r carries stdout alone and this console satisfies StderrConsole.
type pipeConsole struct {
	r      io.ReadCloser
	stderr io.ReadCloser
	// w is the child's stdin. Non-PTY spawns used to leave it nil and answer
	// "pipe console is read-only" — so shell_write_stdin was registered, in
	// the factory allow list, and could not deliver a byte. An interactive
	// command (a prompt, a REPL, anything reading a confirmation) was
	// unusable, and the caller got a plausible error naming the console
	// rather than the missing wiring.
	w io.WriteCloser
}

func (c *pipeConsole) Read(b []byte) (int, error) { return c.r.Read(b) }
func (c *pipeConsole) Write(b []byte) (int, error) {
	if c.w == nil {
		return 0, fmt.Errorf("shell: this session has no stdin")
	}
	return c.w.Write(b)
}
func (c *pipeConsole) Resize(uint16, uint16) error {
	return fmt.Errorf("pipe console cannot resize")
}
func (c *pipeConsole) PTY() bool { return false }

// Close releases both halves. The stderr pipe is closed too (when separated)
// so abandoning the console cannot strand the child writing into a pipe
// nobody reads.
func (c *pipeConsole) Close() error {
	// stdin first: a child blocked reading it sees EOF and can exit, where
	// closing the read side first would leave it waiting on input nobody will
	// send.
	if c.w != nil {
		_ = c.w.Close()
	}
	err := c.r.Close()
	if c.stderr != nil {
		if serr := c.stderr.Close(); err == nil {
			err = serr
		}
	}
	return err
}

// Stderr implements StderrConsole. nil means this console merged the streams,
// so the caller must treat Read as carrying both.
func (c *pipeConsole) Stderr() io.Reader {
	if c.stderr == nil {
		return nil
	}
	return c.stderr
}

// ErrPTYUnavailable is the sentinel StartPTYProcess returns on a platform with
// no reviewed PTY adapter (see ptyopen_unixother.go and console_other.go).
//
// It is deliberately NOT what a linux/darwin/windows host returns when its own
// PTY allocation fails: those answer with the errno, wrapped, because "this
// build has no adapter for your GOOS" and "your container has no /dev/ptmx"
// call for completely different operator action. Callers branch on
// errors.Is(err, ErrPTYUnavailable) to decide whether asking again could ever
// work.
var ErrPTYUnavailable = fmt.Errorf("shell: no PTY adapter for this platform")

// PTYCapability is the platform-reported PTY state, answered by a real
// allocation attempt rather than by a build-time constant.
//
// The Backend/Reason strings explain WHY (so the TUI and doctor can render a
// useful "PTY unavailable: <reason>" instead of a generic failure). Available
// means this process could allocate one just now — see PlatformPTYCapability
// for why that is a different question from "this GOOS has an adapter".
type PTYCapability struct {
	Platform  string
	Backend   string
	Reason    string
	Available bool
}
