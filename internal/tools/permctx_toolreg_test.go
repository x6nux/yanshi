package tools

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/guard"
	"github.com/x6nux/yanshi/internal/toolreg"
)

// S8 (unregistered-name refusal) is a two-halves feature: internal/toolreg
// produces the set and binds it on the turn context, and this file is the
// ENFORCING half. Without a consumer, toolreg is a fully-tested package with
// 100% coverage that changes no behaviour whatsoever -- the "written but never
// read" shape this repo keeps rediscovering. Every test below therefore drives
// the real Authorize entry points rather than toolreg.Check directly.

// countingCallback returns a permission callback plus a pointer to the number
// of times it was consulted. The COUNT is the load-bearing assertion for S8: a
// refusal that still consulted the callback has already rendered a dialog to
// the operator asking them to approve a tool that does not exist, which is the
// precise hazard, and it is invisible to a test that only checks the returned
// error.
func countingCallback(decision PermissionDecision) (func(PermissionRequest) PermissionDecision, *int) {
	n := 0
	return func(PermissionRequest) PermissionDecision {
		n++
		return decision
	}, &n
}

func TestAuthorize_UnregisteredToolIsRefusedWithoutPrompting(t *testing.T) {
	cases := []struct {
		name     string
		tool     string
		refused  bool
		registry []string
	}{
		{
			name:     "registered name passes the S8 check",
			tool:     "fs_read",
			refused:  false,
			registry: []string{"fs_read", "fs_write"},
		},
		{
			name: "phantom name is refused",
			// fs_mkdir is the real historical example: it sat in guard's
			// no-prompt edit set and in the default profile while no tool of
			// that name had ever been registered.
			tool:     "fs_mkdir",
			refused:  true,
			registry: []string{"fs_read", "fs_write"},
		},
		{
			name:     "empty tool name is refused",
			tool:     "",
			refused:  true,
			registry: []string{"fs_read"},
		},
		{
			name: "no set bound leaves authorization untouched",
			// WithRegistered drops an empty list, so this context carries no
			// set. Sub-agents and every pre-existing caller land here, and the
			// check must be a no-op for them rather than fail closed.
			tool:     "fs_mkdir",
			refused:  false,
			registry: nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ask, calls := countingCallback(PermissionAllow)
			ctx := WithProfile(context.Background(), profileAll)
			ctx = WithPermissionCallback(ctx, ask)
			ctx = toolreg.WithRegistered(ctx, tc.registry)

			err := Authorize(ctx, guard.Action{Tool: tc.tool}, `{}`)

			if !tc.refused {
				require.NoError(t, err)
				return
			}
			require.Error(t, err, "an unregistered name must not be authorized")
			var de *DenyErr
			require.ErrorAs(t, err, &de, "the refusal must be a DenyErr so callers classify it as a denial")
			require.Zero(t, *calls,
				"the permission callback was consulted for a tool that does not exist: "+
					"the operator is being shown an Allow dialog for a name nothing can execute")
		})
	}
}

// TestAuthorizeApprovalRequired_UnregisteredToolIsRefusedWithoutPrompting
// covers the second entry point. It prompts UNCONDITIONALLY -- there is no
// profile branch that can spare it -- so a phantom name here is guaranteed to
// reach the operator, making this the sharper of the two paths rather than an
// afterthought.
func TestAuthorizeApprovalRequired_UnregisteredToolIsRefusedWithoutPrompting(t *testing.T) {
	ask, calls := countingCallback(PermissionAllow)
	ctx := WithProfile(context.Background(), profileAll)
	ctx = WithPermissionCallback(ctx, ask)
	ctx = toolreg.WithRegistered(ctx, []string{"github_merge"})

	err := AuthorizeApprovalRequired(ctx, guard.Action{Tool: "github_teleport"}, `{}`)

	require.Error(t, err)
	require.Zero(t, *calls,
		"an approval-gated phantom name still reached the operator as a dialog")

	// Control: the registered sibling must still prompt and still be allowed,
	// proving the refusal above is specific to the unknown name and not a
	// blanket break of the approval path.
	ask2, calls2 := countingCallback(PermissionAllow)
	ctx2 := WithProfile(context.Background(), profileAll)
	ctx2 = WithPermissionCallback(ctx2, ask2)
	ctx2 = toolreg.WithRegistered(ctx2, []string{"github_merge"})

	require.NoError(t, AuthorizeApprovalRequired(ctx2, guard.Action{Tool: "github_merge"}, `{}`))
	require.Equal(t, 1, *calls2, "the approval path must still consult the operator exactly once")
}

// TestAuthorize_S8RefusalSurvivesTheWidestProfile pins that the refusal is not
// something a permissive policy can talk its way past.
//
// Tools.Allow=["*"] is the widest profile expressible and PermissionAlwaysAllow
// is the most permissive answer a callback can give. This is the configuration
// where a phantom name is most dangerous, because nothing else is left to stop
// it.
//
// Deliberately NOT claimed here: that placing the check first is what makes
// this work. It is not — moving the check below the profile lookup and the plan
// -mode branch keeps this test green, because both orderings return before the
// callback is consulted. That was measured, not assumed. The ordering is
// justified separately by the next test.
func TestAuthorize_S8RefusalSurvivesTheWidestProfile(t *testing.T) {
	ask, calls := countingCallback(PermissionAlwaysAllow)
	ctx := WithProfile(context.Background(), profileAll)
	ctx = WithPermissionCallback(ctx, ask)
	ctx = toolreg.WithRegistered(ctx, []string{"fs_read"})

	err := Authorize(ctx, guard.Action{Tool: "fs_mkdir"}, `{}`)
	require.Error(t, err, `Tools.Allow=["*"] must not authorize a name no tool answers to`)
	require.Zero(t, *calls)
}

// TestAuthorize_S8RunsBeforeTheProfileLookup pins the ordering itself, via the
// one input that can actually distinguish the two placements: a context with a
// registered-set but NO profile.
//
// Authorize's profile lookup fails closed with "no permission profile in
// context". If S8 ran after it, every phantom name in that state would be
// reported as a missing profile — sending whoever debugs it to look at profile
// wiring for a problem that is really a phantom tool name. Ordering is what
// keeps the error message pointing at the actual cause.
func TestAuthorize_S8RunsBeforeTheProfileLookup(t *testing.T) {
	ctx := toolreg.WithRegistered(context.Background(), []string{"fs_read"})

	err := Authorize(ctx, guard.Action{Tool: "fs_mkdir"}, `{}`)

	require.Error(t, err)
	require.Contains(t, err.Error(), "fs_mkdir",
		"the refusal must name the unregistered tool rather than blaming the missing profile")
	require.NotContains(t, err.Error(), "no permission profile",
		"S8 must run before the profile lookup, otherwise a phantom name is misreported "+
			"as a profile-wiring problem")
}
