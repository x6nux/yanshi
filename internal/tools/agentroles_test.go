package tools

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// ledger: B1/M05#1 7 角色可选
func TestRoleCatalogCoversSevenRoles(t *testing.T) {
	names := []string{}
	for _, r := range AgentRoles() {
		names = append(names, r.Name)
	}
	require.ElementsMatch(t, []string{"general", "explore", "plan", "review", "implementer", "verifier", "custom"}, names)
}

// ledger: B1/M05#2 权限矩阵符合
func TestRoleAllowlistOnlyTightensParent(t *testing.T) {
	for _, r := range AgentRoles() {
		require.NotEmpty(t, r.PromptPrefix, "role %s missing prompt prefix", r.Name)
	}
	explore := MustRole("explore")
	require.NotNil(t, explore.Policy)
	require.True(t, explore.Policy.ReadOnlyShell)
	require.Empty(t, explore.Policy.WritePatterns)

	plan := MustRole("plan")
	require.NotNil(t, plan.Policy)
	require.True(t, plan.Policy.ReadOnlyShell)
	require.NotEmpty(t, plan.Policy.WritePatterns)

	review := MustRole("review")
	require.NotNil(t, review.Policy)
	require.True(t, review.Policy.ReadOnlyShell)
	require.Empty(t, review.Policy.WritePatterns)

	for _, name := range []string{"general", "implementer", "custom"} {
		require.Nil(t, MustRole(name).Policy, "%s must not add role-level restriction", name)
	}
}

func TestRolePromptPrefixCarriesOutputContract(t *testing.T) {
	for _, r := range AgentRoles() {
		if r.Name == "custom" {
			continue
		}
		require.Contains(t, r.PromptPrefix, "SUMMARY")
		require.Contains(t, r.PromptPrefix, "CHANGES")
		require.Contains(t, r.PromptPrefix, "EVIDENCE")
		require.Contains(t, r.PromptPrefix, "RISKS")
		require.Contains(t, r.PromptPrefix, "BLOCKERS")
	}
}
