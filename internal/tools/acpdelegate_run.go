package tools

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/x6nux/yanshi/internal/acp"
)

// errorsIs is a one-line alias so acpdelegate.go's merge policy reads as
// policy. Declared here rather than importing errors there twice.
func errorsIs(err, target error) bool { return errors.Is(err, target) }

// delegateSession is the subset of a live ACP session the delegation loop
// uses. It exists as an interface purely so the delegation logic — timeout
// handling, transcript capture, cancellation, teardown ordering — is testable
// without a real agent CLI on PATH.
//
// It is NOT an abstraction over transports: there is exactly one production
// implementation (secureSession, wrapping acp.SecureSpawned). Making it an
// interface is the cheaper of the two ways to reach this code from a test; the
// other is spawning a real subprocess, which is what the flagged e2e_real
// tests already do and what unit tests must not.
type delegateSession interface {
	// Prompt runs one turn and returns the agent's stop reason. onEvent
	// receives every session/update as it arrives.
	Prompt(ctx context.Context, task string, onEvent func(acp.Event)) (string, error)
	// Cancel asks the agent to abandon the current turn.
	Cancel(ctx context.Context) error
	// Close tears the agent down and reaps it.
	Close()
}

// delegateSpawner opens a session. Replaced in tests; production is
// spawnSecureSession.
type delegateSpawner func(ctx context.Context, opts acp.SpawnOptions) (delegateSession, error)

// acpSpawn is the seam. It is a package-level var rather than a field on the
// tool because the tool is constructed in the composition root and a test that
// had to reach through the constructor would be asserting on wiring rather
// than on delegation behaviour.
var acpSpawn delegateSpawner = spawnSecureSession

// secureSession adapts acp.SecureSpawned to delegateSession.
type secureSession struct{ s *acp.SecureSpawned }

// Prompt forwards to the ACP client using the session created at spawn.
func (w secureSession) Prompt(ctx context.Context, task string, onEvent func(acp.Event)) (string, error) {
	return w.s.Client.Prompt(ctx, w.s.SessionID, task, onEvent)
}

// Cancel notifies the agent to abandon the active turn.
func (w secureSession) Cancel(ctx context.Context) error {
	return w.s.Client.Cancel(ctx, w.s.SessionID)
}

// Close tears down the client and reaps the child.
func (w secureSession) Close() { w.s.Close() }

// spawnSecureSession is the production spawner: every delegated agent goes
// through secproc so the Authorize firewall, the credential scrub and the
// sandbox tier all apply.
func spawnSecureSession(ctx context.Context, opts acp.SpawnOptions) (delegateSession, error) {
	sp, err := acp.SpawnSecure(ctx, opts)
	if err != nil {
		return nil, err
	}
	return secureSession{s: sp}, nil
}

// delegateBinding carries the autoVCS wiring for a worktree-isolated run. The
// zero value means "no VCS": the agent runs unbound and nothing is recorded.
type delegateBinding struct {
	WorktreeID string
	Recorder   func(worktreeID, agent, absPath string, content []byte) error
	MCPCommand map[string]any
}

// delegateTranscriptLimit bounds how much of the agent's own message output is
// carried back to the model.
//
// Unbounded would defeat the purpose of delegating: the reason to hand a large
// task to another agent is to keep its intermediate reasoning OUT of this
// conversation's context window, and pasting the whole transcript back in
// spends exactly what the delegation saved. The tail is kept rather than the
// head because an agent's conclusion is at the end.
const delegateTranscriptLimit = 8000

// runDelegation spawns the agent, runs one prompt, and tears it down.
//
// Cancellation ordering is the part worth reading. When ctx expires the ACP
// Call returns immediately with ctx.Err, but the AGENT is still working: it has
// no idea the client gave up. Sending session/cancel on a context that is
// already dead would fail instantly, so the notification goes out on a fresh
// short-lived context. Only then is the client closed. Skipping the cancel and
// going straight to Close does technically kill the process, but an agent
// killed mid-write can leave a half-written file in the worktree, which then
// gets committed and merged as if it were intentional.
func runDelegation(
	ctx context.Context,
	a acpDelegateArgs,
	cwd string,
	binding delegateBinding,
) (acpDelegateResult, error) {
	opts := acp.SpawnOptions{
		Agent:      a.Agent,
		Cwd:        cwd,
		Policy:     acp.NewGuardPolicy(acp.WorktreeScopedProfile(cwd)),
		Env:        []string{ACPDepthEnv + "=" + nextDepth(ctx)},
		WorktreeID: binding.WorktreeID,
		Recorder:   binding.Recorder,
		MCPCommand: binding.MCPCommand,
	}
	session, err := acpSpawn(ctx, opts)
	if err != nil {
		return acpDelegateResult{}, fmt.Errorf("acp_delegate: start %s: %w", a.Agent, err)
	}
	defer session.Close()

	var transcript strings.Builder
	stopReason, promptErr := session.Prompt(ctx, a.Task, func(ev acp.Event) {
		if ev.Kind == "agent_message_chunk" && ev.Text != "" {
			transcript.WriteString(ev.Text)
		}
	})
	if promptErr != nil {
		if cancelErr := cancelQuietly(ctx, session); cancelErr != nil {
			// The agent may not have heard the cancellation; Close still kills
			// it. Worth reporting because "the worktree may hold a partial
			// write" is the actionable half.
			promptErr = errors.Join(promptErr, cancelErr)
		}
		return acpDelegateResult{}, fmt.Errorf("acp_delegate: %s: %w", a.Agent, promptErr)
	}
	return acpDelegateResult{
		Agent:      a.Agent,
		StopReason: stopReason,
		Transcript: tailBytes(transcript.String(), delegateTranscriptLimit),
	}, nil
}

// cancelQuietly sends session/cancel on a context detached from the (possibly
// already-expired) turn context, so the agent learns the turn was abandoned
// even when the reason for abandoning it was the deadline.
func cancelQuietly(ctx context.Context, session delegateSession) error {
	cancelCtx, done := context.WithTimeout(context.WithoutCancel(ctx), acpCancelGrace)
	defer done()
	return session.Cancel(cancelCtx)
}

// acpCancelGrace bounds the best-effort cancellation notify. It is short: the
// notification is a single line on a pipe that is either writable now or will
// never be, and Close kills the process regardless.
const acpCancelGrace = 3 * time.Second

// nextDepth renders the depth an agent spawned from this turn should see.
func nextDepth(ctx context.Context) string {
	depth, _ := acpDepthExceeded(ctx)
	return strconv.Itoa(depth + 1)
}

// tailBytes keeps the last limit bytes of s, prefixing an elision marker when
// anything was dropped. The marker matters: a silently truncated transcript
// reads to the model as an agent that stopped mid-sentence.
func tailBytes(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	return "…[earlier output truncated]…\n" + s[len(s)-limit:]
}
