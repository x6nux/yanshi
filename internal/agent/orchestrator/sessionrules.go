// Session-scoped approval rules (S9) and the per-connection lifetime that
// makes them safe.
//
// internal/guard/generalize.go has had RuleSet / Approve / Demote /
// WithSessionRules since it was written, and until this file NOTHING CALLED
// ANY OF THEM. Approval generalization was a complete, tested, documented
// mechanism with zero consumers: a user who approved `go test ./internal/a`
// was asked again about `./internal/b`, and every argument in that file's
// header about widening safely described behaviour no user could observe.
//
// # The honest scope, stated once and repeated in the API docs
//
// RuleSet.WithSessionRules is a NO-OP on a profile with no Shell.Rules, and
// that is correct rather than a gap: grafting a rules table onto a glob-matcher
// profile switches which matcher checkShell runs, and a rules-only profile
// hard-denies every command its rules do not name. An operator whose profile
// says `policy: denylist` with two patterns would find everything else refused.
//
// So S9 changes behaviour ONLY for profiles that already use `shell.rules` —
// the operator profile. On the factory-default coding profile it is wired,
// live, and deliberately without effect. Anything that describes S9 as
// "approvals are now remembered" without that qualifier is overclaiming;
// TestSessionRulesAreANoOpOnGlobProfiles pins the no-op half so the claim
// cannot quietly grow.
//
// # Lifetime
//
// A RuleSet is keyed by connection session id and lives until the connection
// ends. It is IN-MEMORY and never persisted — see generalize.go's header for
// why an approval is evidence about this conversation rather than a standing
// grant. The map would otherwise be a leak: one entry per WebSocket connection
// for the life of the process, holding execpolicy rules forever. ReleaseSession
// is the explicit teardown, and the transport calls it from the same deferred
// block that closes the connection.
package orchestrator

import (
	"github.com/x6nux/yanshi/internal/guard"
)

// SessionRules returns the RuleSet for a connection session, creating it on
// first use.
//
// It returns nil for an empty id. An empty connection session id means the
// caller is not a connection — a headless tool invocation, the goal loop, a
// test — and those have no place to release a rule set from, so giving them
// one would be the leak this file exists to avoid. A nil *guard.RuleSet is
// safe to use: WithSessionRules on a nil receiver would panic, so every call
// site nil-checks, and bindExecutionContext does.
func (o *Orchestrator) SessionRules(connectionSessionID string) *guard.RuleSet {
	if connectionSessionID == "" {
		return nil
	}
	o.sessionRulesMu.Lock()
	defer o.sessionRulesMu.Unlock()
	if o.sessionRules == nil {
		o.sessionRules = make(map[string]*guard.RuleSet)
	}
	rs, ok := o.sessionRules[connectionSessionID]
	if !ok {
		rs = &guard.RuleSet{}
		o.sessionRules[connectionSessionID] = rs
	}
	return rs
}

// ApproveShellForSession records an approved shell command as a reusable rule
// for this connection's remaining lifetime, and reports whether the approval
// actually widened anything.
//
// A false return is the COMMON and CORRECT outcome, not a failure: high-risk
// verbs are never widened, a demoted family is never widened again, and a
// command that cannot be tokenized produces no rule. See guard.RuleSet.Approve
// for why the fallback is "no rule" rather than an exact-match rule (execpolicy
// prefixes admit supersets, so an "exact" rule for `rm -rf ./build` would
// authorize `rm -rf ./build ./other`).
//
// Called from the transport when the user answers allow to a shell prompt.
func (o *Orchestrator) ApproveShellForSession(connectionSessionID, command string) bool {
	rs := o.SessionRules(connectionSessionID)
	if rs == nil || command == "" {
		return false
	}
	return rs.Approve(command).Widened
}

// DemoteShellForSession is the second QwenPaw gate: a refused command takes its
// whole family back to prompting for the rest of the session, irreversibly.
//
// Called when the user answers deny. Denying a command in a family that a
// previous approval widened is the clearest possible evidence that the widening
// was wrong, and the only new information available is that the heuristic
// produced a bad rule here — so re-widening later on the strength of the same
// heuristic is not an option. Returns whether a family's rules were actually
// removed.
func (o *Orchestrator) DemoteShellForSession(connectionSessionID, command string) bool {
	rs := o.SessionRules(connectionSessionID)
	if rs == nil || command == "" {
		return false
	}
	return rs.Demote(command)
}

// ReleaseSession drops the rule set for a connection.
//
// This is the half a "wire it up" change forgets, and forgetting it turns a
// feature into a slow leak: without it the map grows by one RuleSet per
// WebSocket connection and never shrinks, each holding the execpolicy rules of
// a conversation that ended hours ago. Idempotent, and safe on an id that was
// never used.
func (o *Orchestrator) ReleaseSession(connectionSessionID string) {
	if connectionSessionID == "" {
		return
	}
	o.sessionRulesMu.Lock()
	defer o.sessionRulesMu.Unlock()
	delete(o.sessionRules, connectionSessionID)
}

// SessionRuleCount reports how many sessions currently hold rule sets.
//
// Exported for the leak test: "ReleaseSession is called" is only checkable
// against an observable count, and a spy on the method would pass whether or
// not the entry actually left the map — which is the failure this whole file
// is a reaction to.
func (o *Orchestrator) SessionRuleCount() int {
	o.sessionRulesMu.Lock()
	defer o.sessionRulesMu.Unlock()
	return len(o.sessionRules)
}

// profileForSession returns the acting profile for a connection: the
// orchestrator's own, plus whatever this session's approvals widened.
//
// The merge order is guard.RuleSet.WithSessionRules's, and it is the safe one:
// session rules go AFTER the profile's, execpolicy returns hard_deny the moment
// a deny rule's flags match regardless of position, and "prompt wins over
// allow" among the rest. A session approval can therefore only ever admit
// something the operator's profile left UNMATCHED.
func (o *Orchestrator) profileForSession(connectionSessionID string) guard.PermissionProfile {
	rs := o.SessionRules(connectionSessionID)
	if rs == nil {
		return o.profile
	}
	return rs.WithSessionRules(o.profile)
}
