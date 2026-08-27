// internal/api/http/ws_permtimeout.go
//
// Wall-clock expiry for interactive permission prompts, and the unattended
// degradation that follows from it.
//
// The WebSocket permission callback blocks the whole turn while it waits for a
// human. Without an expiry that wait is unbounded: one prompt nobody answers
// wedges `yanshi goal` forever. With only an expiry it is still bad — every
// subsequent prompt burns the full timeout again, so an unattended run
// advances at one tool call per timeout for as long as the budget lasts.
//
// So there are two rules here, and the second one is the reason this file
// exists separately from the tracker's delivery logic:
//
//  1. An expired prompt is DENIED. Never allowed. A timeout is the absence of
//     an authorization gesture, and absence is not consent — the whole guard
//     stack is fail-closed and this is the one place where "nobody said
//     anything" could most easily be read as "go ahead".
//  2. After N consecutive expiries with no interaction at all, the connection
//     is latched as unattended and further prompts are refused IMMEDIATELY
//     rather than waited on. Any client interaction unlatches it.
//
// Rule 2 changes only WHEN the deny happens, never WHETHER: latched or not,
// the answer to an unanswered prompt is deny. That is what makes the latch
// safe to flip on a heuristic — being wrong about "nobody is watching" costs
// the user a wait they would have lost anyway, not an approval they never gave.

package http

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/x6nux/yanshi/internal/tools"
)

// DefaultPermissionTimeout is how long an interactive permission prompt waits
// for a human before expiring (and denying). It matches the value this handler
// used when the wait was an inline time.After in the callback, so an operator
// who configures nothing sees the behaviour they had.
const DefaultPermissionTimeout = 60 * time.Second

// DefaultPermissionUnattendedAfter is how many consecutive expiries, with no
// intervening client interaction, latch the connection as unattended.
//
// Three rather than one: a single expiry is routinely a human who stepped away
// mid-thought and is about to come back, and latching on it would make the
// prompt they return to disappear before they can answer it. Three consecutive
// expiries is minutes of silence at the default timeout, which no
// briefly-distracted operator produces.
const DefaultPermissionUnattendedAfter = 3

// PermissionTimeoutPolicy configures the approval countdown and the unattended
// latch for one server. The zero value is valid and means "use the defaults";
// resolve() is what applies them, so callers never have to.
type PermissionTimeoutPolicy struct {
	// Timeout is the per-prompt budget. Non-positive means the default.
	Timeout time.Duration
	// UnattendedAfter is the consecutive-expiry count that latches the
	// connection as unattended. Non-positive means the default. There is
	// deliberately no "never latch" value: the latch cannot change any
	// verdict, only its latency, so disabling it buys nothing and costs a
	// wedged unattended run.
	UnattendedAfter int
}

// resolve returns the policy with defaults applied to unset fields.
func (p PermissionTimeoutPolicy) resolve() PermissionTimeoutPolicy {
	if p.Timeout <= 0 {
		p.Timeout = DefaultPermissionTimeout
	}
	if p.UnattendedAfter <= 0 {
		p.UnattendedAfter = DefaultPermissionUnattendedAfter
	}
	return p
}

// permOutcome is how one interactive prompt ended. It exists so the caller can
// tell the three shapes apart for logging and for the transcript notice — the
// DECISION is deny for all but permAnswered, so the outcome is the only thing
// that distinguishes "the user said no" from "nobody was there".
type permOutcome int

const (
	// permAnswered: the client delivered a decision. The decision is the
	// client's, whatever it was.
	permAnswered permOutcome = iota
	// permExpired: the prompt outlived its deadline. Denied.
	permExpired
	// permAborted: the turn's context ended (user cancel / disconnect) while
	// the prompt was outstanding. Denied, and NOT counted toward the
	// unattended latch — a cancel is a user acting, not a user absent.
	permAborted
	// permRefusedUnattended: the connection was already latched as unattended,
	// so the prompt was denied without waiting at all.
	permRefusedUnattended
)

// unattendedState tracks the consecutive-expiry latch for one connection.
//
// It is a separate type from permTracker because its lifecycle is the
// CONNECTION's, not any single request's: the counter has to survive the
// request that incremented it, and the reset has to be reachable from the
// reader goroutine (which sees client interaction) without touching the
// pending-request map the callback goroutine owns.
type unattendedState struct {
	mu sync.Mutex
	// consecutive counts expiries since the last interaction of any kind.
	consecutive int
	// latched is true once consecutive reached the policy threshold. It stays
	// true until noteInteraction clears it.
	latched bool
	cfg     PermissionTimeoutPolicy
}

// newUnattendedState builds the latch for one connection under policy.
func newUnattendedState(policy PermissionTimeoutPolicy) *unattendedState {
	return &unattendedState{cfg: policy.resolve()}
}

// policy returns the resolved timeout policy this latch runs under, so a caller
// that renders the numbers (the deny notice, the countdown on the frame) reads
// the same values the wait itself uses.
func (u *unattendedState) policy() PermissionTimeoutPolicy { return u.cfg }

// noteInteraction is the reset. It is called for ANY frame the client sends —
// not just permission answers — because the question the latch is answering is
// "is a human present on this connection", and a user who types a message, hits
// cancel or switches mode has answered it as clearly as one who clicks Allow.
//
// Called from the reader goroutine on every decoded frame.
func (u *unattendedState) noteInteraction() {
	u.mu.Lock()
	u.consecutive = 0
	u.latched = false
	u.mu.Unlock()
}

// noteExpiry records one prompt that ran out of time and reports whether that
// expiry latched the connection (true only on the transition, so the caller can
// announce it once instead of on every subsequent prompt).
func (u *unattendedState) noteExpiry() (latchedNow bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.consecutive++
	if u.latched || u.consecutive < u.cfg.UnattendedAfter {
		return false
	}
	u.latched = true
	return true
}

// isLatched reports whether prompts should be refused without waiting.
func (u *unattendedState) isLatched() bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.latched
}

// timeout returns the per-prompt budget.
func (u *unattendedState) timeout() time.Duration { return u.cfg.Timeout }

// awaitDecision runs one interactive prompt's wait and returns the verdict the
// callback must hand back to the guard layer.
//
// Every non-answer path returns tools.PermissionDeny. That is stated once, here,
// rather than at each return site, because this function is the only place in
// the WS transport where a decision can be produced without a client saying
// anything, and a future edit that made one of these branches allow would be a
// silent authorization bypass.
//
// The latch check happens BEFORE the wait, so a latched connection costs the
// turn nothing per prompt. ch is the buffered channel the tracker registered;
// deadlineAt is the absolute expiry the frame already advertised to the client,
// so the wait and the countdown the user is watching end at the same instant.
func (u *unattendedState) awaitDecision(ctx context.Context,
	ch <-chan tools.PermissionDecision, deadlineAt time.Time) (tools.PermissionDecision, permOutcome) {

	if u.isLatched() {
		return tools.PermissionDeny, permRefusedUnattended
	}
	timer := time.NewTimer(time.Until(deadlineAt))
	defer timer.Stop()
	select {
	case d := <-ch:
		// An answer is interaction. Reset here as well as in the reader
		// goroutine: the reader's reset covers frames of every kind, but a
		// connection whose ONLY traffic is permission answers must also
		// unlatch, and routing that through the same call keeps the two
		// resets from disagreeing about what "interaction" means.
		u.noteInteraction()
		return d, permAnswered
	case <-ctx.Done():
		return tools.PermissionDeny, permAborted
	case <-timer.C:
		u.noteExpiry()
		return tools.PermissionDeny, permExpired
	}
}

// permDenyNotice is the human-readable explanation for an auto-denied prompt,
// written to the transcript and the log. It names the tool and the reason so an
// operator reading a goal-loop transcript days later can tell an approval that
// was refused from one that was never seen.
//
// Returns "" for permAnswered (the client answered; there is nothing to
// explain) and for permAborted (the turn is already ending on a cancel or a
// disconnect, and a "denied" line on top of that reads as a second failure).
//
// The policy is a parameter rather than read from the package defaults: a
// server configured with a different timeout or threshold would otherwise
// announce numbers it does not use, which is worse than announcing none.
func permDenyNotice(outcome permOutcome, tool string, policy PermissionTimeoutPolicy) string {
	policy = policy.resolve()
	switch outcome {
	case permExpired:
		return fmt.Sprintf("permission request for %s expired after %s with no answer — "+
			"denied (a timeout is never an approval)", tool, policy.Timeout)
	case permRefusedUnattended:
		return fmt.Sprintf("permission request for %s denied immediately: this session is "+
			"running unattended (no answer to the last %d prompts). Any input restores prompting.",
			tool, policy.UnattendedAfter)
	default:
		return ""
	}
}
