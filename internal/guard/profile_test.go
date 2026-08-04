package guard

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPermissionProfileDefaultsSubagent(t *testing.T) {
	var p PermissionProfile
	require.True(t, p.Subagent.AllowsAnyModel())
	require.Equal(t, "high", p.Subagent.ReasoningCap())
	require.NoError(t, p.Subagent.CheckReasoning("high"))
}

func TestSubagentPermModelAllowlist(t *testing.T) {
	p := PermissionProfile{
		Subagent: SubagentPerm{Models: []string{"gpt-4o-mini"}, MaxReasoning: "medium"},
	}
	require.NoError(t, p.Subagent.CheckModel("gpt-4o-mini"))
	require.Error(t, p.Subagent.CheckModel("claude-haiku"))
	require.NoError(t, p.Subagent.CheckReasoning("low"))
	require.NoError(t, p.Subagent.CheckReasoning("medium"))
	require.Error(t, p.Subagent.CheckReasoning("high"))
	require.Error(t, p.Subagent.CheckReasoning("bogus"))
}

// TestShellPolicyCatalogMatchesCheckShell is the BEHAVIOURAL half of holding
// ShellPolicies() and checkShell's switch together: it drives the real Check
// (not checkShell directly) so the assertion reflects what a caller observes.
//
// It covers ONE direction. Every catalog entry is run through Check and must
// not reach the unknown-policy branch, so a catalog value checkShell dropped
// fails here. The second loop is not the other direction — it is a fixed list
// of near-miss spellings, and four literals cannot notice that checkShell grew
// a case ShellPolicies() omits. That drift is the one that makes a config the
// guard CAN enforce fail to load, and it is caught by set equality against the
// parsed source in TestShellPolicyCatalogEqualsCheckShellSwitch
// (verdictcatalog_test.go). Neither test subsumes the other: this one pins the
// verdict tiers a caller sees, that one pins the vocabularies.
func TestShellPolicyCatalogMatchesCheckShell(t *testing.T) {
	g := New()
	profile := func(policy string) PermissionProfile {
		return PermissionProfile{
			Tools: ToolsPerm{Allow: []string{"*"}},
			Shell: ShellPerm{Policy: policy, Patterns: []string{"go test"}},
			Net:   NetPerm{Allow: true},
		}
	}
	act := Action{Tool: "shell_run", Shell: "go test"}

	for _, policy := range ShellPolicies() {
		require.NoError(t, ValidateShellPolicy(policy),
			"catalog entry %q must validate", policy)
		d := g.Check(profile(policy), act)
		require.NotContains(t, d.Reason, "unknown shell policy",
			"catalog entry %q reached checkShell's unknown-policy branch", policy)
		require.False(t, d.Verdict == HardDeny && !d.Overridable,
			"catalog entry %q must not yield a structural HardDeny, got %+v", policy, d)
	}

	// "allow" is the value docs/user-guide/guard.md used to advertise. It is
	// not a policy, and both halves must say so.
	for _, bogus := range []string{"allow", "Allowlist", "permit", "allow-all"} {
		require.Error(t, ValidateShellPolicy(bogus), "%q must not validate", bogus)
		d := g.Check(profile(bogus), act)
		require.Contains(t, d.Reason, "unknown shell policy",
			"%q must reach checkShell's unknown-policy branch", bogus)
		require.Equal(t, HardDeny, d.Verdict)
		require.False(t, d.Overridable,
			"the unknown-policy denial is structural; yolo/auto must not be able to override it")
	}
}
