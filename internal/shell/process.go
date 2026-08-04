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
// SecureProcessFactory in Task 19 wraps this to inject netpolicy.PrepareEnv
// and Sandbox.Prepare before delegating here.
//
// PTY requests route through StartPTYProcess, which is platform-specific and
// currently returns ErrPTYUnavailable on every platform (Phase 0). When a
// real PTY backend lands (Task 18+), it plugs in via the same entry point.
type OSProcessFactory struct{}

// Start spawns spec.Program with spec.Args. stdin is not connected in Phase 0
// (shell v2 callers that need stdin set spec.PTY=true, which fails closed
// for now). cmd.Env is populated from spec.Env when non-empty so the child
// does not inherit the parent's environment — netpolicy.PrepareEnv is what
// populates that slice, so stripping the parent's HTTP_PROXY happens once at
// the boundary rather than via fragile parent-env scrubbing here.
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
	if len(spec.Env) > 0 {
		cmd.Env = spec.Env
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		_ = stdout.Close()
		return nil, nil, err
	}
	// Start before wiring the readers: the merge copies eagerly in
	// goroutines, and a failed Start must not leave them racing the pipes it
	// is closing.
	if err := cmd.Start(); err != nil {
		_ = stdout.Close()
		_ = stderr.Close()
		return nil, nil, err
	}
	if spec.SeparateStderr {
		return &osProcess{cmd: cmd}, &pipeConsole{r: stdout, stderr: stderr}, nil
	}
	return &osProcess{cmd: cmd}, &pipeConsole{r: secproc.MergeOutput(stdout, stderr)}, nil
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
	if p.cmd.Process == nil {
		return nil
	}
	return p.cmd.Process.Kill()
}
func (p *osProcess) Capabilities() ProcessCapabilities {
	return ProcessCapabilities{CanKillTree: CanKillTreeOnPlatform()}
}

// pipeConsole is the read-only Console used for pipe-pair spawns. Write
// returns an error (Phase 0 does not wire stdin for non-PTY spawns); Resize
// is a no-op (no PTY to resize); PTY() reports false so callers know to
// render in line-buffered rather than terminal mode.
//
// stderr is non-nil only for a LaunchSpec.SeparateStderr spawn, in which case
// r carries stdout alone and this console satisfies StderrConsole.
type pipeConsole struct {
	r      io.ReadCloser
	stderr io.ReadCloser
}

func (c *pipeConsole) Read(b []byte) (int, error) { return c.r.Read(b) }
func (c *pipeConsole) Write([]byte) (int, error)  { return 0, fmt.Errorf("pipe console is read-only") }
func (c *pipeConsole) Resize(uint16, uint16) error {
	return fmt.Errorf("pipe console cannot resize")
}
func (c *pipeConsole) PTY() bool { return false }

// Close releases both halves. The stderr pipe is closed too (when separated)
// so abandoning the console cannot strand the child writing into a pipe
// nobody reads.
func (c *pipeConsole) Close() error {
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

// ErrPTYUnavailable is the single sentinel every platform returns from
// StartPTYProcess in Phase 0. Tests assert errors.Is(err, ErrPTYUnavailable)
// so a future backend that forgets to actually wire up the PTY will be
// caught (it would return a different error).
var ErrPTYUnavailable = fmt.Errorf("shell: PTY adapter unavailable in Phase 0")

// PTYCapability is the platform-reported PTY state. Available=false on every
// platform in Phase 0; the Backend/Reason strings explain WHY (so the TUI
// can render a useful "PTY unavailable: <reason>" instead of a generic
// failure). When real PTY backends land, each platform file fills in the
// real Backend/Reason and sets Available=true.
type PTYCapability struct {
	Platform  string
	Backend   string
	Reason    string
	Available bool
}
