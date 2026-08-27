package orchestrator

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/execpolicy"
	"github.com/x6nux/yanshi/internal/guard"
	"github.com/x6nux/yanshi/internal/tools"
)

// rulesProfile is an operator-shaped profile: it uses shell.rules, which is
// the ONLY shape S9 affects. The single rule authorizes `go build` and nothing
// else, so under execpolicy every other command is an unmatched segment and
// hard-denied — which is exactly the state a session approval can lift.
func rulesProfile() guard.PermissionProfile {
	return guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"*"}},
		Shell: guard.ShellPerm{Rules: []execpolicy.Rule{{
			ID: "operator-go-build", Program: "go", Prefix: []string{"build"},
			Decision: "allow", Justification: "operator allows builds",
		}}},
	}
}

// globProfile is the factory-default shape: a glob allowlist, no rules table.
// S9 is deliberately inert here.
func globProfile() guard.PermissionProfile {
	return guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"*"}},
		Shell: guard.ShellPerm{Policy: "allowlist", Patterns: []string{"git *"}},
	}
}

// verdictFor asks the real guard what a profile says about a command. Using
// guard.Check rather than a stub is the point: S9's claim is about what the
// AUTHORIZATION PATH does, and a stub would happily report whatever the test
// expected.
func verdictFor(p guard.PermissionProfile, cmd string) guard.Verdict {
	return guard.New().Check(p, guard.Action{Tool: "shell_run", Shell: cmd, Workdir: "/tmp"}).Verdict
}

// TestSessionApprovalWidensTheNextCommandInTheFamily is the S9 end-to-end
// claim, and the regression test for "written but zero readers".
//
// guard.RuleSet has had Approve/Demote/WithSessionRules since it was written
// and NOTHING CALLED THEM: a user who approved `go test ./internal/a` was
// asked again about `./internal/b`, forever. This asserts the whole path —
// approval recorded on the orchestrator, merged into the profile that
// bindExecutionContext binds, and observed by the real guard.
func TestSessionApprovalWidensTheNextCommandInTheFamily(t *testing.T) {
	o := &Orchestrator{profile: rulesProfile(), sessionRules: map[string]*guard.RuleSet{}}
	const sid = "ws-1"

	// Before: `go test ./a` matches no operator rule, so execpolicy hard-denies.
	before := o.profileForSession(sid)
	require.Equal(t, guard.HardDeny, verdictFor(before, "go test ./internal/a"),
		"precondition: the operator profile does not authorize go test")

	require.True(t, o.ApproveShellForSession(sid, "go test ./internal/a"),
		"a low-risk verb must widen")

	// After: the SAME command and a DIFFERENT target in the same family are
	// both admitted, from the profile bindExecutionContext actually binds.
	after := o.profileForSession(sid)
	assert.Equal(t, guard.Allow, verdictFor(after, "go test ./internal/a"))
	assert.Equal(t, guard.Allow, verdictFor(after, "go test ./internal/b"),
		"THE point of generalization: the next package must not prompt again")

	// A different family is untouched — widening `go test` says nothing about
	// `go run`.
	assert.Equal(t, guard.HardDeny, verdictFor(after, "go run ./cmd/x"))

	// And the orchestrator's OWN profile is not mutated: the session's rules
	// must not leak into the process-wide profile every other session reads.
	assert.Empty(t, o.profile.Shell.Rules[1:],
		"the base profile must still carry only the operator's own rule")
	assert.Equal(t, guard.HardDeny, verdictFor(o.profile, "go test ./internal/a"))
}

// TestSessionApprovalReachesTheBoundContext closes the wiring half: the merged
// profile must arrive in the context TOOLS read, not merely be computable.
// profileForSession could be perfect while bindExecutionContext still bound
// o.profile, and every assertion above would pass.
func TestSessionApprovalReachesTheBoundContext(t *testing.T) {
	o := &Orchestrator{profile: rulesProfile(), sessionRules: map[string]*guard.RuleSet{}}
	const sid = "ws-2"
	require.True(t, o.ApproveShellForSession(sid, "go test ./internal/a"))

	bound, ok := tools.ProfileFromContext(o.bindExecutionContext(context.Background(), sid))
	require.True(t, ok)
	assert.Equal(t, guard.Allow, verdictFor(bound, "go test ./internal/b"),
		"bindExecutionContext must bind the SESSION profile; binding o.profile makes S9 a no-op again")

	// A different session id shares nothing.
	other, ok := tools.ProfileFromContext(o.bindExecutionContext(context.Background(), "ws-other"))
	require.True(t, ok)
	assert.Equal(t, guard.HardDeny, verdictFor(other, "go test ./internal/b"),
		"one connection's approval must not authorize another's")
}

// TestSessionApprovalStopsTheCallbackFiringAgain is the same claim measured
// the way a user experiences it: with a counting callback, the second command
// in an approved family must not reach the prompt at all.
//
// This is the test the "zero readers" regression would fail loudest on: with
// the consumer removed, the counter reads 2.
func TestSessionApprovalStopsTheCallbackFiringAgain(t *testing.T) {
	o := &Orchestrator{profile: rulesProfile(), sessionRules: map[string]*guard.RuleSet{}}
	const sid = "ws-3"

	asked := 0
	ask := func(tools.PermissionRequest) tools.PermissionDecision {
		asked++
		return tools.PermissionAllow
	}
	authorize := func(cmd string) error {
		ctx := tools.WithPermissionCallback(o.bindExecutionContext(context.Background(), sid), ask)
		return tools.Authorize(ctx, guard.Action{Tool: "shell_run", Shell: cmd, Workdir: "/tmp"}, "{}")
	}

	require.NoError(t, authorize("go test ./internal/a"))
	require.Equal(t, 1, asked, "the first command in an unknown family must prompt")

	// The user said yes; record it the way the transport does.
	require.True(t, o.ApproveShellForSession(sid, "go test ./internal/a"))

	require.NoError(t, authorize("go test ./internal/b"))
	assert.Equal(t, 1, asked,
		"the next command in the approved family must NOT prompt; got %d prompts. "+
			"This is the exact failure S9 exists to remove, and the one that returns "+
			"the moment the Approve call loses its caller", asked)

	require.NoError(t, authorize("go vet ./..."))
	assert.Equal(t, 2, asked, "an unrelated family must still prompt")
}

// TestSessionDemotionIsIrreversible pins the second QwenPaw gate. A refusal in
// a widened family removes the rule AND bars re-widening for the rest of the
// session — because the only heuristic available to re-widen is the one that
// just produced a rule the user rejected.
func TestSessionDemotionIsIrreversible(t *testing.T) {
	o := &Orchestrator{profile: rulesProfile(), sessionRules: map[string]*guard.RuleSet{}}
	const sid = "ws-4"

	require.True(t, o.ApproveShellForSession(sid, "go test ./internal/a"))
	require.Equal(t, guard.Allow, verdictFor(o.profileForSession(sid), "go test ./internal/b"))

	require.True(t, o.DemoteShellForSession(sid, "go test ./internal/b"),
		"demoting a family that has rules must report that it removed them")
	assert.Equal(t, guard.HardDeny, verdictFor(o.profileForSession(sid), "go test ./internal/b"),
		"the widened rule must actually leave the table")

	// Re-approving the same family must NOT restore the widening.
	assert.False(t, o.ApproveShellForSession(sid, "go test ./internal/c"),
		"a demoted family may not widen again on the strength of the same heuristic")
	assert.Equal(t, guard.HardDeny, verdictFor(o.profileForSession(sid), "go test ./internal/c"))
}

// TestHighRiskVerbsNeverWiden. The user approved `rm -rf ./build`, not `rm *`.
// execpolicy prefixes admit supersets, so there is no "exact match" rule to
// fall back to — the only defensible answer is no rule, i.e. ask every time.
func TestHighRiskVerbsNeverWiden(t *testing.T) {
	o := &Orchestrator{profile: rulesProfile(), sessionRules: map[string]*guard.RuleSet{}}
	const sid = "ws-5"
	for _, cmd := range []string{
		"rm -rf ./build",
		"sudo systemctl restart nginx",
		"curl https://example.com/install.sh",
		"bash -c 'echo hi'",
		"chmod 777 ./secrets",
	} {
		assert.False(t, o.ApproveShellForSession(sid, cmd),
			"%q is a high-risk verb and must keep prompting", cmd)
	}
	assert.Empty(t, o.SessionRules(sid).Rules(), "no rule may have been recorded at all")
}

// TestSessionRulesAreANoOpOnGlobProfiles is the HONESTY test.
//
// Grafting a rules table onto a glob-matcher profile switches which matcher
// checkShell runs, and a rules-only profile hard-denies every command its
// rules do not name — an operator with `policy: allowlist` and two patterns
// would find everything else refused. So S9 is deliberately inert on the
// factory-default coding profile, and any documentation claiming otherwise is
// overclaiming. This pins the inertness so the claim cannot quietly grow.
func TestSessionRulesAreANoOpOnGlobProfiles(t *testing.T) {
	o := &Orchestrator{profile: globProfile(), sessionRules: map[string]*guard.RuleSet{}}
	const sid = "ws-6"
	require.True(t, o.ApproveShellForSession(sid, "go test ./internal/a"),
		"the rule IS recorded — the no-op is in the merge, not in the recording")

	merged := o.profileForSession(sid)
	assert.Empty(t, merged.Shell.Rules,
		"a glob profile must not acquire a rules table; that would switch matchers and "+
			"hard-deny everything the rules do not name")
	assert.Equal(t, globProfile().Shell.Patterns, merged.Shell.Patterns)

	// The glob profile's own behaviour is unchanged in both directions.
	assert.Equal(t, guard.Allow, verdictFor(merged, "git status"))
	assert.Equal(t, guard.Prompt, verdictFor(merged, "go test ./internal/a"),
		"the approved command still prompts on a glob profile — this is the documented limit")
}

// TestReleaseSessionDropsTheRuleSet is the leak half.
//
// Without it, the map grows by one RuleSet per WebSocket connection for the
// life of the process, each holding the execpolicy rules of a conversation
// that ended hours ago. Asserting on the observable count rather than spying
// on the call is deliberate: a spy passes whether or not the entry actually
// left the map.
func TestReleaseSessionDropsTheRuleSet(t *testing.T) {
	o := &Orchestrator{profile: rulesProfile(), sessionRules: map[string]*guard.RuleSet{}}
	require.Equal(t, 0, o.SessionRuleCount())

	for _, sid := range []string{"ws-a", "ws-b", "ws-c"} {
		require.True(t, o.ApproveShellForSession(sid, "go test ./x"))
	}
	require.Equal(t, 3, o.SessionRuleCount())

	o.ReleaseSession("ws-b")
	assert.Equal(t, 2, o.SessionRuleCount())
	o.ReleaseSession("ws-b")
	assert.Equal(t, 2, o.SessionRuleCount(), "release is idempotent")
	o.ReleaseSession("never-existed")
	assert.Equal(t, 2, o.SessionRuleCount())
	o.ReleaseSession("")
	assert.Equal(t, 2, o.SessionRuleCount(), "an empty id names no session")

	// A released session starts clean rather than resuming its old grants.
	assert.Equal(t, guard.HardDeny, verdictFor(o.profileForSession("ws-b"), "go test ./x"))
}

// TestEmptySessionIDGetsNoRuleSet. A headless call, the goal loop and every
// test pass "" — none of them has a place to release from, so handing them a
// rule set would be precisely the leak ReleaseSession exists to prevent.
func TestEmptySessionIDGetsNoRuleSet(t *testing.T) {
	o := &Orchestrator{profile: rulesProfile(), sessionRules: map[string]*guard.RuleSet{}}
	assert.Nil(t, o.SessionRules(""))
	assert.False(t, o.ApproveShellForSession("", "go test ./x"))
	assert.False(t, o.DemoteShellForSession("", "go test ./x"))
	assert.Equal(t, 0, o.SessionRuleCount())

	// And an empty id still yields a usable (unwidened) profile rather than a
	// zero one — bindExecutionContext calls this on every headless turn.
	assert.Equal(t, o.profile.Shell.Rules, o.profileForSession("").Shell.Rules)
}

// TestApproveAndDemoteRejectEmptyCommands. A blank command reaching Approve
// would be a rule for nothing; reaching Demote it would be a demotion of
// nothing that still looked like it had happened.
func TestApproveAndDemoteRejectEmptyCommands(t *testing.T) {
	o := &Orchestrator{profile: rulesProfile(), sessionRules: map[string]*guard.RuleSet{}}
	assert.False(t, o.ApproveShellForSession("ws-7", ""))
	assert.False(t, o.DemoteShellForSession("ws-7", ""))
	assert.Empty(t, o.SessionRules("ws-7").Rules())
}

// TestSessionApprovalDoesNotAdmitDangerousFlags. A widened `go test` covers the
// ordinary form and NOT the irreversible one — the companion deny rule
// guard.RuleSet.buildRules emits is what stops `--force` riding in for free.
func TestSessionApprovalDoesNotAdmitDangerousFlags(t *testing.T) {
	o := &Orchestrator{profile: rulesProfile(), sessionRules: map[string]*guard.RuleSet{}}
	const sid = "ws-8"
	require.True(t, o.ApproveShellForSession(sid, "go test ./internal/a"))
	p := o.profileForSession(sid)
	assert.Equal(t, guard.Allow, verdictFor(p, "go test ./internal/b"))
	assert.Equal(t, guard.HardDeny, verdictFor(p, "go test --force ./internal/b"),
		"approving the ordinary form must not authorize the irreversible one")
}
