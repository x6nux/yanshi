package shell

import (
	"context"
	"encoding/json"
	"io"
	"sync"
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
	// AllowEnv names the credential-bearing environment variables this spawn
	// legitimately needs (e.g. GH_TOKEN for `gh`). Empty — the default — means
	// the child inherits no credentials at all; see childLaunchPosture.env and
	// netpolicy.CredentialPolicy. Mirrors secproc.SecureProcessSpec.AllowEnv so
	// the two launch paths express the same declaration.
	AllowEnv []string
	PTY      bool
	// SeparateStderr asks the factory to keep the child's stderr on its own
	// pipe instead of folding it into the Console. Callers that PARSE the
	// child's stdout (git porcelain, `go test -json`) must set it; callers
	// that DISPLAY the output (the Manager pumping a session into its ring
	// buffer) leave it false and get the interleaved stream a terminal shows.
	//
	// A factory that honors it returns a Console satisfying StderrConsole.
	// Ignoring it is safe but lossy — see StderrConsole.
	SeparateStderr bool

	// ProcessToken is the OS primary-token handle the child must run under, or
	// 0 to run under this process's own token.
	//
	// Windows only, and it exists because sandbox.Prepare speaks *exec.Cmd while
	// the spawn happens inside the factory. childLaunchPosture.prepare hands the
	// backend a stand-in Cmd and copies the mutations back into this struct;
	// before this field, SysProcAttr was not among the things copied, so a
	// backend setting SysProcAttr.Token was writing into a value that was thrown
	// away and every child ran under the unrestricted token while the report
	// claimed otherwise. sandbox/poststart.go documents the same gap for
	// CreationFlags, which is still open.
	//
	// uintptr rather than a typed handle so this struct stays buildable on every
	// platform; the two conversions live in sandboxtoken_windows.go. A zero
	// value is the inherit-everything default on all platforms, so no caller
	// that does not know about it can be broken by it.
	//
	// The handle is OWNED by the sandbox that produced it and is closed when
	// that sandbox is closed. A spec must not outlive it.
	ProcessToken uintptr
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
//
// mu is set by liveSession for concurrent-safe MarshalJSON.
type Session struct {
	mu        sync.Locker `json:"-"` // injected by liveSession; nil for standalone values
	ID        string      `json:"id"`
	PID       int         `json:"pid"`
	Command   string      `json:"command"`
	State     State       `json:"state"`
	ExitCode  int         `json:"exit_code"`
	PTY       bool        `json:"pty"`
	StartedAt time.Time   `json:"started_at"`
	EndedAt   *time.Time  `json:"ended_at,omitempty"`
}

// MarshalJSON serializes the session under mu (when set), preventing races
// with concurrent pump() in liveSession writing State/ExitCode fields.
func (s *Session) MarshalJSON() ([]byte, error) {
	if s.mu != nil {
		s.mu.Lock()
		defer s.mu.Unlock()
	}
	type session Session
	return json.Marshal((*session)(s))
}

// value would still serialize (time.Time is a struct, not a type that
// omitempty recognizes).

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
// Resize returns an error on non-PTY backends rather than nil, so a caller
// cannot come away believing a geometry change reached a child that has no
// terminal. PTY() reports whether the underlying transport is a real PTY (vs. a
// pipe pair), which the Manager surfaces in Session.PTY so the TUI can render
// differently.
type Console interface {
	io.ReadWriteCloser
	Resize(rows, cols uint16) error
	PTY() bool
}

// StderrConsole is the Console a factory returns when it honored
// LaunchSpec.SeparateStderr: Read serves stdout only, and Stderr() hands back
// the child's stderr as its own stream.
//
// It is a separate interface rather than a Console method so the existing
// Console implementations (PTY backends, test doubles) stay valid: a PTY has
// exactly one stream by construction and can never satisfy this. Consumers
// type-assert and fall back to "everything arrived on stdout", which is lossy
// but never wrong — the bytes are not duplicated.
type StderrConsole interface {
	Console
	// Stderr returns the separated stderr stream, or nil if this console
	// merged the two after all (so callers can tell "empty" from "merged").
	Stderr() io.Reader
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

// ProcessFactory is the seam the Manager calls to spawn. It returns a Process
// AND a Console so the Manager can pump output without holding the process
// handle directly.
//
// The production implementation is SecureLaunchFactory (procfactory.go), which
// carries the compile-time assertion. DefaultSecureFactory does NOT implement
// this interface and cannot: its Start takes a secproc.SecureProcessSpec and
// returns *secproc.StartedProcess, so one type cannot carry both signatures
// under the same method name. That is not an accident to be tidied up later —
// it is why shell v2 is a separate launch path from secproc, and therefore why
// each shell v2 tool must call guard Authorize itself instead of relying on
// secproc's Authorize firewall. See procfactory.go's header and CLAUDE.md's
// "子进程发射" paragraph.
type ProcessFactory interface {
	Start(ctx context.Context, spec LaunchSpec) (Process, Console, error)
}
