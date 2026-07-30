package tools

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/x6nux/yanshi/internal/guard"
)

func TestValidateOverrideHonorsProfileAndRegistry(t *testing.T) {
	profile := guard.PermissionProfile{
		Subagent: guard.SubagentPerm{
			Models: []string{"gpt-4o-mini"}, MaxReasoning: "medium",
		},
	}
	available := map[string]bool{"gpt-4o-mini": true, "claude-haiku": true}

	require.NoError(t, ValidateOverride(context.Background(), "gpt-4o-mini", "low", profile, available))
	require.Error(t, ValidateOverride(context.Background(), "claude-haiku", "low", profile, available))
	require.Error(t, ValidateOverride(context.Background(), "gpt-4o-mini", "high", profile, available))
	require.Error(t, ValidateOverride(context.Background(), "missing-model", "low", profile, available))
	require.Error(t, ValidateOverride(context.Background(), "gpt-4o-mini", "bogus", profile, available))
}

func TestValidateOverrideEmptyInherits(t *testing.T) {
	profile := guard.PermissionProfile{}
	require.NoError(t, ValidateOverride(context.Background(), "", "", profile, map[string]bool{}))
}

func TestEnsureOverrideIsRevalidatedOnResume(t *testing.T) {
	ctx := context.Background()
	profile := guard.PermissionProfile{
		Subagent: guard.SubagentPerm{Models: []string{"gpt-4o-mini"}, MaxReasoning: "medium"},
	}
	err := EnsureOverrideForResume(ctx, "gpt-4o-mini", "high", profile, map[string]bool{"gpt-4o-mini": true})
	require.Error(t, err)
	require.ErrorIs(t, err, guard.ErrOverrideDenied)
}
