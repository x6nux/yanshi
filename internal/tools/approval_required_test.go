package tools

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/x6nux/yanshi/internal/guard"
)

// TestAuthorizeApprovalRequiredTruthTable verifies the mandatory-approval path
// across the key scenarios. ApprovalRequired tools ALWAYS prompt (cannot be
// short-circuited by profile or approval manager), and always_allow is rejected.
func TestAuthorizeApprovalRequiredTruthTable(t *testing.T) {
	type row struct {
		name          string
		profileBound  bool
		staticAllows  bool
		callback      PermissionDecision
		mandatory     bool
		wantAllow     bool
		callbackCalls int32
	}
	rows := []row{
		{"no profile / mandatory", false, false, "", true, false, 0},
		{"profile allow / mandatory / no callback", true, true, "", true, false, 0},
		{"profile allow / mandatory / callback allow", true, true, PermissionAllow, true, true, 1},
		{"profile allow / mandatory / callback always_allow", true, true, PermissionAlwaysAllow, true, false, 1},
		{"profile allow / mandatory / callback deny", true, true, PermissionDeny, true, false, 1},
		{"profile deny / mandatory / callback always_allow", true, false, PermissionAlwaysAllow, true, false, 1},
		{"profile deny / mandatory / callback deny", true, false, PermissionDeny, true, false, 1},
		{"profile deny / mandatory / callback allow", true, false, PermissionAllow, true, true, 1},
		// Non-mandatory path (existing Authorize): profile allow + no callback → allow
		{"profile allow / non-mandatory / no callback", true, true, "", false, true, 0},
		// Non-mandatory path: profile deny + no callback → deny
		{"profile deny / non-mandatory / no callback", true, false, "", false, false, 0},
	}
	for _, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			if tc.profileBound {
				if !tc.staticAllows {
					ctx = WithProfile(ctx, guard.PermissionProfile{Tools: guard.ToolsPerm{Allow: []string{"nothing"}}})
				} else {
					ctx = WithProfile(ctx, allowAllProfile())
				}
			}
			var calls int32
			if tc.callback != "" {
				decision := tc.callback
				ctx = WithPermissionCallback(ctx, func(req PermissionRequest) PermissionDecision {
					atomic.AddInt32(&calls, 1)
					if req.ApprovalRequired != tc.mandatory {
						t.Fatalf("approval_required got=%v want=%v", req.ApprovalRequired, tc.mandatory)
					}
					return decision
				})
			}
			var err error
			if tc.mandatory {
				err = AuthorizeApprovalRequired(ctx, guard.Action{Tool: "github_comment"}, "{}")
			} else {
				err = Authorize(ctx, guard.Action{Tool: "github_comment"}, "{}")
			}
			if tc.wantAllow && err != nil {
				t.Fatalf("want allow got %v", err)
			}
			if !tc.wantAllow && err == nil {
				t.Fatalf("want deny got allow")
			}
			if got := atomic.LoadInt32(&calls); got != tc.callbackCalls {
				t.Fatalf("callback calls=%d want=%d", got, tc.callbackCalls)
			}
		})
	}
}

// TestApprovalRequiredNoCallbackNeverRunsTool proves that a GuardedTool built
// via NewApprovalGuardedTool NEVER executes its stream function when no callback
// is bound, regardless of the profile.
func TestApprovalRequiredNoCallbackNeverRunsTool(t *testing.T) {
	var runs int32
	tool := NewApprovalGuardedTool("github_comment", "GitHub comment", "approval-only", time.Second, nil,
		SyncStream(func(context.Context, string) (string, error) {
			atomic.AddInt32(&runs, 1)
			return "ran", nil
		}))
	ctx := WithProfile(context.Background(), allowAllProfile())
	out, err := tool.InvokableRun(ctx, `{}`)
	if err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt32(&runs) != 0 {
		t.Fatalf("tool ran %d times", runs)
	}
	if !strings.Contains(out, "permission denied") {
		t.Fatalf("out=%s", out)
	}
}
