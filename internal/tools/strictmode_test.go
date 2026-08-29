package tools

import (
	"context"
	"strings"
	"testing"

	"github.com/x6nux/yanshi/internal/guard"
)

// allowEverythingProfile permits the action under test outright, so any prompt
// observed below can only have come from the strict-mode rewrite.
func allowEverythingProfile() guard.PermissionProfile {
	return guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"*"}},
		FS:    guard.FSPerm{Read: []string{"**"}, Write: []string{"**"}},
		Shell: guard.ShellPerm{Policy: "denylist"},
		Net:   guard.NetPerm{Allow: true},
	}
}

// TestStrictModeConfirmsCallsTheProfileAllows is the W-B-20 acceptance, and it
// is the assertion that goes red if the fourth execution level is reverted.
//
// The profile says yes to everything; without WithConfirmEveryCall the callback
// is never consulted, which is the pre-W-B-20 behaviour asserted in the second
// half. Delete the rewrite in Authorize and the first half fails with
// "callback never consulted".
func TestStrictModeConfirmsCallsTheProfileAllows(t *testing.T) {
	action := guard.Action{Tool: "fs_read", FS: guard.FSWant{Op: "read", Paths: []string{"main.go"}}}

	t.Run("strict asks", func(t *testing.T) {
		asked := 0
		var reason string
		ctx := WithProfile(context.Background(), allowEverythingProfile())
		ctx = WithPermissionCallback(ctx, func(req PermissionRequest) PermissionDecision {
			asked++
			reason = req.Reason
			return PermissionAllow
		})
		ctx = WithConfirmEveryCall(ctx, func() bool { return true })
		if err := Authorize(ctx, action, ""); err != nil {
			t.Fatalf("an approved strict-mode call must proceed: %v", err)
		}
		if asked != 1 {
			t.Fatalf("callback consulted %d times, want 1", asked)
		}
		if !strings.Contains(reason, "strict mode") {
			t.Fatalf("the prompt does not say why it appeared: %q", reason)
		}
	})

	t.Run("strict honours a refusal", func(t *testing.T) {
		ctx := WithProfile(context.Background(), allowEverythingProfile())
		ctx = WithPermissionCallback(ctx, func(PermissionRequest) PermissionDecision {
			return PermissionDeny
		})
		ctx = WithConfirmEveryCall(ctx, func() bool { return true })
		if err := Authorize(ctx, action, ""); err == nil {
			t.Fatal("a refused strict-mode call was allowed anyway")
		}
	})

	t.Run("unbound predicate is the old behaviour", func(t *testing.T) {
		asked := 0
		ctx := WithProfile(context.Background(), allowEverythingProfile())
		ctx = WithPermissionCallback(ctx, func(PermissionRequest) PermissionDecision {
			asked++
			return PermissionDeny
		})
		if err := Authorize(ctx, action, ""); err != nil {
			t.Fatalf("an allowed call must not prompt without strict mode: %v", err)
		}
		if asked != 0 {
			t.Fatalf("callback consulted %d times with no predicate bound, want 0", asked)
		}
	})

	t.Run("predicate reporting false is the old behaviour", func(t *testing.T) {
		asked := 0
		ctx := WithProfile(context.Background(), allowEverythingProfile())
		ctx = WithPermissionCallback(ctx, func(PermissionRequest) PermissionDecision {
			asked++
			return PermissionDeny
		})
		ctx = WithConfirmEveryCall(ctx, func() bool { return false })
		if err := Authorize(ctx, action, ""); err != nil || asked != 0 {
			t.Fatalf("a false predicate changed behaviour: err=%v asked=%d", err, asked)
		}
	})
}

// TestStrictModeCannotSoftenAnyDenial is the direction that would make W-B-20 a
// widening instead of a tightening.
//
// The rewrite sits after guard.Check and fires only on Allow, so a structural
// HardDeny must be unreachable from it. If the condition were ever inverted or
// widened to `dec.Verdict != guard.Prompt`, `rm -rf /` would arrive at the
// callback as an answerable question — under the mode whose entire promise is
// that it asks MORE, not that it permits more.
func TestStrictModeCannotSoftenAnyDenial(t *testing.T) {
	ctx := WithProfile(context.Background(), allowEverythingProfile())
	ctx = WithPermissionCallback(ctx, func(PermissionRequest) PermissionDecision {
		t.Fatal("a structural HardDeny reached the permission callback under strict mode")
		return PermissionDeny
	})
	ctx = WithConfirmEveryCall(ctx, func() bool { return true })
	err := Authorize(ctx, guard.Action{Tool: "shell_run", Shell: "rm -rf /", Workdir: "/tmp/x"}, "")
	if err == nil {
		t.Fatal("catastrophic deletion was allowed under strict mode")
	}
}
