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
