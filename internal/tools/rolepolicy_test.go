package tools

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/x6nux/yanshi/internal/guard"
)

func TestRolePolicyRejectsUnsafeShellBeforeAuthorize(t *testing.T) {
	ctx := WithRolePolicy(context.Background(), RolePolicy{
		ReadOnlyShell: true, WritePatterns: nil,
	})
	err := Authorize(ctx, guard.Action{Tool: "shell_run", Shell: "rm -rf /tmp/x"}, `{}`)
	require.ErrorIs(t, err, ErrRolePolicyDenied)
}

func TestRolePolicyAllowsSafeReadOnlyShell(t *testing.T) {
	ctx := WithRolePolicy(context.Background(), RolePolicy{ReadOnlyShell: true})
	require.NoError(t, CheckRolePolicy(ctx, guard.Action{Tool: "shell_run", Shell: "ls -la"}))
}

func TestRolePolicyBlocksWriteOutsidePatterns(t *testing.T) {
	ctx := WithRolePolicy(context.Background(), RolePolicy{
		WritePatterns: []string{"docs/plans/*.md"},
	})
	err := CheckRolePolicy(ctx, guard.Action{
		Tool: "fs_write", FS: guard.FSWant{Op: "write", Paths: []string{"internal/foo.go"}},
	})
	require.ErrorIs(t, err, ErrRolePolicyDenied)
	require.NoError(t, CheckRolePolicy(ctx, guard.Action{
		Tool: "fs_write", FS: guard.FSWant{Op: "write", Paths: []string{"docs/plans/x.md"}},
	}))

	// Empty WritePatterns does not add write restrictions.
	noWriteCtx := WithRolePolicy(context.Background(), RolePolicy{ReadOnlyShell: true})
	require.NoError(t, CheckRolePolicy(noWriteCtx, guard.Action{
		Tool: "fs_write", FS: guard.FSWant{Op: "write", Paths: []string{"internal/foo.go"}},
	}))
}
