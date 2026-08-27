package acp

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/x6nux/yanshi/internal/sandbox"
	"github.com/x6nux/yanshi/internal/secproc"
)

// SecureSpawned is what SpawnSecure returns: a ready Client plus the reaper and
// PID of the child. It is the secproc twin of Spawned, which carries an
// *exec.Cmd instead.
//
// Wait and Close are separate because the ordering matters and only the caller
// knows when the session is over: Close drops the client's end of the pipes so
// the agent sees EOF and exits, and Wait then reaps it. Calling Wait first
// blocks until an agent that is waiting for more input gives up on its own.
type SecureSpawned struct {
	// Client is the initialized ACP client; SessionID names the session the
	// handshake created.
	Client    *Client
	SessionID string
	// PID is the child's process id, for diagnostics.
	PID int
	// wait reaps the child. Never nil: SpawnSecure fails closed on a Factory
	// that returned no reaper.
	wait func() error
	// stderr keeps a bounded tail of the child's diagnostics.
	stderr *stderrTail
}

// Stderr returns the tail of the child's stderr collected so far. Used to turn
// "initialize failed" into a message that names the reason the agent printed.
func (s *SecureSpawned) Stderr() string {
	if s.stderr == nil {
		return ""
	}
	return s.stderr.String()
}

// Close shuts the client down (agent sees EOF on stdin) and then reaps the
// child, returning the child's exit error if any.
//
// It deliberately ignores the reap error's *exec.ExitError shape: an ACP agent
// torn down mid-session normally exits non-zero, and treating that as a
// delegation failure would report every successful cancellation as an error.
// Callers that need the exit status have PID and can watch it themselves.
func (s *SecureSpawned) Close() {
	if s.Client != nil {
		s.Client.Close()
	}
	if s.wait != nil {
		_ = s.wait()
	}
}

// SpawnSecure is Spawn routed through internal/secproc.
//
// It exists because an external agent CLI is the textbook untrusted program:
// it is chosen by name from a config file, it is handed the project directory,
// and it executes whatever the model asks it to. secproc.Launch is the single
// entry point where such a program passes the Authorize firewall, gets its
// credential-bearing environment stripped, and picks up the sandbox posture.
// The pre-existing Spawn calls exec.CommandContext directly and therefore does
// none of that; it stays for the goal loop, whose worker is invoked by an
// operator at a shell prompt rather than by a model mid-turn.
//
// Failure modes, in order:
//   - unknown agent name -> LaunchSpec's error
//   - no Factory bound in ctx, or Authorize denied -> secproc.Launch's error,
//     which the caller must surface verbatim (it carries the denial reason)
//   - Factory returned no reaper or no stdin -> fail closed rather than leak an
//     unreapable child or a client that can never write a request
//   - initialize / session/new failure -> the child is torn down before the
//     error is returned, so a half-open agent never survives the call
func SpawnSecure(ctx context.Context, opts SpawnOptions) (*SecureSpawned, error) {
	argv, err := LaunchSpec(opts.Agent)
	if err != nil {
		return nil, err
	}
	started, err := secproc.Launch(ctx, secproc.SecureProcessSpec{
		Tool:    "acp_delegate",
		Program: argv[0],
		Args:    argv[1:],
		Dir:     opts.Cwd,
		Env:     opts.Env,
		// An ACP agent writes files, runs builds and creates worktrees inside
		// the directory it was pointed at. ReadOnly would make every
		// delegation fail on the agent's first edit; FullAccess would hand it
		// the whole machine. WorkspaceWrite is the tier that matches what the
		// worktree-scoped guard profile already permits.
		UseSandboxTier: sandbox.WorkspaceWrite,
	})
	if err != nil {
		return nil, err
	}
	if started.Wait == nil {
		return nil, fmt.Errorf("acp: factory returned a process with no reaper (fail-closed)")
	}
	if started.Stdin == nil {
		// Reap before reporting: the child is already running and nothing else
		// holds a handle to it.
		_ = started.Wait()
		return nil, fmt.Errorf("acp: factory returned a process with no stdin; " +
			"an ACP agent cannot be driven without one (fail-closed)")
	}

	client := NewClient(started.Stdout, started.Stdin)
	if opts.Policy != nil {
		client.SetPolicy(opts.Policy)
	}
	if opts.WorktreeID != "" {
		client.SetVCSTracking(opts.WorktreeID, opts.Recorder)
	}
	spawned := &SecureSpawned{
		Client: client, PID: started.PID, wait: started.Wait,
		stderr: drainStderr(started.Stderr, stderrTailLimit),
	}

	caps := ClientCapabilities{
		FS:       &FSCap{ReadTextFile: true, WriteTextFile: true},
		Terminal: true,
	}
	if _, err := client.Initialize(ctx, caps); err != nil {
		spawned.Close()
		return nil, fmt.Errorf("acp: initialize %q: %w%s", opts.Agent, err, spawned.stderrSuffix())
	}
	sessionID, err := client.NewSession(ctx, opts.Cwd, opts.ExtraDirs, buildMcpServers(opts))
	if err != nil {
		spawned.Close()
		return nil, fmt.Errorf("acp: session/new %q: %w%s", opts.Agent, err, spawned.stderrSuffix())
	}
	spawned.SessionID = sessionID
	return spawned, nil
}

// stderrSuffix renders the child's stderr tail as a parenthesised clause, or
// "" when it printed nothing. Appended to handshake errors: the JSON-RPC layer
// only ever reports "EOF" or a timeout, while the actual cause ("npx: command
// not found", "missing ANTHROPIC_API_KEY") went to stderr.
func (s *SecureSpawned) stderrSuffix() string {
	tail := s.Stderr()
	if tail == "" {
		return ""
	}
	return " (agent stderr: " + tail + ")"
}

// stderrTailLimit bounds the retained stderr. Large enough for a stack trace or
// a usage message, small enough that a crash-looping agent cannot grow the
// parent's heap.
const stderrTailLimit = 4096

// stderrTail is a bounded ring of the last limit bytes written to a child's
// stderr, safe to read while the drain goroutine is still writing.
type stderrTail struct {
	limit int
	mu    sync.Mutex
	buf   []byte
	done  chan struct{}
}

func (t *stderrTail) append(p []byte) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.limit {
		t.buf = t.buf[len(t.buf)-t.limit:]
	}
}

// String returns the retained tail with surrounding whitespace trimmed.
func (t *stderrTail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.TrimSpace(string(t.buf))
}

// drainStderr consumes a child's stderr so a chatty agent cannot wedge on a
// full pipe buffer, keeping at most the last limit bytes for diagnostics.
//
// Discarding it entirely would be simpler and is what the exec-based Spawn
// effectively does with its bytes.Buffer nobody reads; keeping a bounded tail
// is what lets a delegation that failed during the handshake say WHY. The
// bound matters because an agent in a crash loop can produce megabytes.
func drainStderr(r io.Reader, limit int) *stderrTail {
	t := &stderrTail{limit: limit, done: make(chan struct{})}
	if r == nil {
		close(t.done)
		return t
	}
	go func() {
		defer close(t.done)
		buf := make([]byte, 4096)
		for {
			n, err := r.Read(buf)
			if n > 0 {
				t.append(buf[:n])
			}
			if err != nil {
				return
			}
		}
	}()
	return t
}
