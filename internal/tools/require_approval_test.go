package tools

import (
	"context"
	"errors"
	"testing"

	"github.com/x6nux/yanshi/internal/guard"
)

// TestRequireApproval_NoCallbackDenies verifies the fail-closed path: with no
// callback bound (the SSE / static path), RequireApproval returns *DenyErr
// regardless of the static profile.
func TestRequireApproval_NoCallbackDenies(t *testing.T) {
	prof := testProfileAllowAll()
	ctx := WithProfile(context.Background(), prof)
	err := RequireApproval(ctx, PermissionRequest{Tool: "revert_turn", Reason: "destructive"})
	if err == nil {
		t.Fatal("RequireApproval 应当无 callback 时 deny")
	}
	var de *DenyErr
	if !errors.As(err, &de) {
		t.Errorf("返回类型 %T,想要 *DenyErr", err)
	}
}

// TestRequireApproval_CallbackAllowPasses verifies the happy path.
func TestRequireApproval_CallbackAllowPasses(t *testing.T) {
	prof := testProfileAllowAll()
	ctx := WithProfile(context.Background(), prof)
	ctx = WithPermissionCallback(ctx, func(req PermissionRequest) PermissionDecision {
		if req.Tool != "revert_turn" {
			t.Errorf("callback got Tool=%q, want revert_turn", req.Tool)
		}
		return PermissionAllow
	})
	if err := RequireApproval(ctx, PermissionRequest{Tool: "revert_turn"}); err != nil {
		t.Errorf("allow: got %v, want nil", err)
	}
}

// TestRequireApproval_CallbackReceivesForce proves RequireApproval itself marks
// every destructive request as Force, even when the caller omitted the field.
func TestRequireApproval_CallbackReceivesForce(t *testing.T) {
	prof := testProfileAllowAll()
	ctx := WithProfile(context.Background(), prof)
	ctx = WithPermissionCallback(ctx, func(req PermissionRequest) PermissionDecision {
		if !req.Force {
			t.Error("RequireApproval callback received Force=false, want true")
		}
		return PermissionAllow
	})
	if err := RequireApproval(ctx, PermissionRequest{Tool: "revert_turn"}); err != nil {
		t.Fatalf("RequireApproval: %v", err)
	}
}

// TestRequireApproval_CallbackDenyFails verifies Deny → *DenyErr.
func TestRequireApproval_CallbackDenyFails(t *testing.T) {
	prof := testProfileAllowAll()
	ctx := WithProfile(context.Background(), prof)
	ctx = WithPermissionCallback(ctx, func(req PermissionRequest) PermissionDecision {
		return PermissionDeny
	})
	err := RequireApproval(ctx, PermissionRequest{Tool: "revert_turn"})
	if err == nil {
		t.Fatal("RequireApproval 应当 deny")
	}
}

// TestRequireApproval_AlwaysAllowDoesNotStick verifies that AlwaysAllow for a
// forced prompt does NOT record into the session allowlist — the next call
// must STILL prompt.
func TestRequireApproval_AlwaysAllowDoesNotStick(t *testing.T) {
	prof := testProfileAllowAll()
	ctx := WithProfile(context.Background(), prof)
	calls := 0
	cb := func(req PermissionRequest) PermissionDecision {
		calls++
		return PermissionAlwaysAllow
	}
	ctx = WithPermissionCallback(ctx, cb)
	_ = RequireApproval(ctx, PermissionRequest{Tool: "revert_turn"})
	_ = RequireApproval(ctx, PermissionRequest{Tool: "revert_turn"})
	if calls != 2 {
		t.Errorf("AlwaysAllow 应当不持久化;calls=%d, want 2", calls)
	}
}

// testProfileAllowAll returns a permissive profile value for RequireApproval tests
// (the point is that RequireApproval MUST prompt even when the profile allows).
func testProfileAllowAll() guard.PermissionProfile {
	return guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"*"}},
	}
}
