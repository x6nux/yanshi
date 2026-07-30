package shell

import (
	"context"
	"io"
	"time"
)

// State is the lifecycle stage of a Session or Job. The string forms are the
// wire-level tokens the TUI and CLI render against; do not rename without
// bumping the proto contract.
type State string

// Session/job lifecycle states.
const (
	StateStarting State = "starting"
	StateRunning  State = "running"
	StateExited   State = "exited"
	StateCanceled State = "canceled"
	StateStale    State = "stale"
)

// String makes State satisfy fmt.Stringer so log lines, error messages, and
// any place that wraps it in %s / %q get the wire form rather than a typed
// pointer.
func (s State) String() string { return string(s) }

// LaunchSpec describes one process start request handed to a ProcessFactory.
// SecureProcessFactory (Task 14/19) pre-resolves Program/Args via ShellArgv
// before constructing a LaunchSpec; the legacy OSProcessFactory path may use
// ShellName + Command instead. Env is the child environment as KEY=VALUE
// entries (matches SecureProcessSpec.Env shape).
type LaunchSpec struct {
	// ShellName is the shell selector (""|auto|cmd|powershell|bash|zsh|sh),
	// consulted by OSProcessFactory only when Program is empty. The
	// SecureProcessFactory path pre-resolves Program/Args via ShellArgv and
	// leaves ShellName empty.
	ShellName string
	Command   string
	Program   string
	Args      []string
	Dir       string
	// Env is the child environment as KEY=VALUE entries.
	// netpolicy.PrepareEnv populates this; OSProcessFactory.Start applies it via
	// cmd.Env. This is a []string to match SecureProcessSpec.Env.
	Env []string
	PTY bool
}

// Session describes a persistent shell session at a point in time. The
// fields are the JSON contract /jobs and /sessions render against; the
// snake_case tags match the rest of the proto wire (see frame.go).
//
// ExitCode uses sentinel -1 for "still running / unknown" and does NOT use
// omitempty — a zero exit code is a meaningful success value and must survive
// JSON round-trip. EndedAt, by contrast, is a *time.Time so nil means "not
// yet ended" and serializes as absent via omitempty; an empty time.Time{}
// value would still serialize (time.Time is a struct, not a type that
// omitempty recognizes).
type Session struct {
	ID        string     `json:"id"`
	PID       int        `json:"pid"`
	Command   string     `json:"command"`
	State     State      `json:"state"`
	ExitCode  int        `json:"exit_code"`
	PTY       bool       `json:"pty"`
	StartedAt time.Time  `json:"started_at"`
	EndedAt   *time.Time `json:"ended_at,omitempty"`
}

// Job is one invocation tracked by the Manager's job table. Sessions emit a
// Job per start; the TUI's /jobs view renders this verbatim. ID/SessionID
// together uniquely identify a job (the same SessionID may run many jobs over
// its lifetime). ExitCode follows the same convention as Session.
type Job struct {
	ID        string     `json:"id"`
	SessionID string     `json:"session_id"`
	Command   string     `json:"command"`
	State     State      `json:"state"`
	Output    string     `json:"output,omitempty"`
	Error     string     `json:"error,omitempty"`
	ExitCode  int        `json:"exit_code"`
	PID       int        `json:"pid"`
	StartedAt time.Time  `json:"started_at"`
	EndedAt   *time.Time `json:"ended_at,omitempty"`
}

// Console is the byte-level seam the Manager pumps. Read returns io.EOF when
// the underlying process exits; Write blocks if the PTY/pipe back-pressures.
// Resize is a no-op for non-PTY backends (they return ErrPTYUnavailable from
// the platform-specific constructors in Task 18). PTY() reports whether the
// underlying transport is a real PTY (vs. a pipe pair), which the Manager
// surfaces in Session.PTY so the TUI can render differently.
type Console interface {
	io.ReadWriteCloser
	Resize(rows, cols uint16) error
	PTY() bool
}

// Process is the OS-level process seam. Wait MUST return *exec.ExitError on
// non-zero exits so the Manager can extract ExitCode via ExitCode(); nil
// means clean exit (exit code 0). Kill terminates just this process; whether
// the OS also reaps children is reported by Capabilities().CanKillTree.
type Process interface {
	Wait() error
	PID() int
	Kill() error
	Capabilities() ProcessCapabilities
}

// ProcessCapabilities reports what the underlying platform actually offers.
// CanKillTree=false means the Manager MUST NOT advertise tree-kill in its
// public API (the TUI/CLI render against CanKillTree so the operator is not
// lied to about whether children were reaped).
type ProcessCapabilities struct {
	CanKillTree bool
}

// ProcessFactory is the seam the Manager (Task 17) calls to spawn. It returns
// a Process AND a Console so the Manager can pump output without holding the
// process handle directly. The Task 17 Manager uses this; the Task 19
// DefaultSecureFactory implements it on top of SecureProcessFactory + the
// platform Console.
type ProcessFactory interface {
	Start(ctx context.Context, spec LaunchSpec) (Process, Console, error)
}
