package tools

import (
	"context"
	"strings"
	"testing"

	"github.com/x6nux/yanshi/internal/guard"
	"github.com/x6nux/yanshi/internal/imagestore"
)

func TestScreenshotReturnsRef(t *testing.T) {
	store := imagestore.New(imagestore.Config{MaxItems: 20, MaxBytes: 100 << 20})
	tool := NewScreenshotToolWithCapture(store, func(context.Context) ([]byte, string, error) {
		return testPNGBytes(t, 8, 8), "png", nil
	})
	ctx := WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"screenshot"}},
	})
	ctx = WithPermissionCallback(ctx, func(req PermissionRequest) PermissionDecision {
		return PermissionAllow
	})
	ch := tool.Stream(ctx, "{}")
	var result string
	for c := range ch {
		result = c.Result
	}
	if !strings.HasPrefix(result, "[image:") {
		t.Fatalf("result must be a placeholder ref, got %q", result)
	}
	// store 里应能取到该 id
	id := extractID(result, t)
	if _, ok := store.Get(id); !ok {
		t.Fatalf("screenshot image %q not in store", id)
	}
}

func TestScreenshotCaptureFailureReturnsErrorResult(t *testing.T) {
	store := imagestore.New(imagestore.Config{MaxItems: 20, MaxBytes: 100 << 20})
	tool := NewScreenshotToolWithCapture(store, func(context.Context) ([]byte, string, error) {
		return nil, "", context.DeadlineExceeded
	})
	ctx := WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"screenshot"}},
	})
	ctx = WithPermissionCallback(ctx, func(req PermissionRequest) PermissionDecision {
		return PermissionAllow
	})
	ch := tool.Stream(ctx, "{}")
	var result string
	for c := range ch {
		result = c.Result
	}
	if !strings.Contains(result, "✗") {
		t.Fatalf("capture failure must surface as ✗ result: %q", result)
	}
}

func TestScreenshotUsesApprovalGuardedTool(t *testing.T) {
	store := imagestore.New(imagestore.Config{MaxItems: 20, MaxBytes: 100 << 20})
	tool := NewScreenshotToolWithCapture(store, func(context.Context) ([]byte, string, error) {
		return testPNGBytes(t, 1, 1), "png", nil
	})
	gt, ok := tool.(*GuardedTool)
	if !ok {
		t.Fatal("screenshot must be a *GuardedTool")
	}
	if !gt.approvalRequired {
		t.Fatal("screenshot tool must require approval (approvalRequired=true)")
	}
}

func extractID(placeholder string, t *testing.T) string {
	t.Helper()
	l := strings.Index(placeholder, ":")
	r := strings.Index(placeholder, " |")
	if l < 0 || r < 0 {
		t.Fatalf("malformed placeholder %q", placeholder)
	}
	return placeholder[l+1 : r]
}
