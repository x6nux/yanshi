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

// TestStrictModeAsksAboutCommandsItCannotScope covers the class of call strict
// mode turned into a SILENT REFUSAL rather than a question (review b4 Minor-3).
//
// guard evaluates a shell command per segment and allows `echo a && echo b`
// outright under a denylist profile. Strict rewrote that Allow into a Prompt,
// scopeFromAction refused to build a scope for a command with more than one
// executable segment, and the resulting DenyErr returned before any callback —
// asked=0, and an error text in internal vocabulary. The report for W-B-20 said
// the mode "only rewrites Allow into Prompt"; for this shape it rewrote Allow
// into Deny.
//
// A scope error stays a hard refusal for a genuine Prompt (that branch has its
// own history — see scopeFromAction). What is dropped here is only the MEMORY:
// nothing is looked up, nothing is stored, and "always allow" degrades to a
// single allow, because a scope nobody can write down is not one to persist.
func TestStrictModeAsksAboutCommandsItCannotScope(t *testing.T) {
	action := guard.Action{Tool: "shell_run", Shell: "echo a && echo b", Workdir: "/tmp/x"}

	t.Run("the profile allows it outright", func(t *testing.T) {
		ctx := WithProfile(context.Background(), allowEverythingProfile())
		if err := Authorize(ctx, action, ""); err != nil {
			t.Fatalf("test premise broken — this command is no longer an Allow: %v", err)
		}
	})

	for _, answer := range []struct {
		name     string
		decision PermissionDecision
	}{
		{"allow", PermissionAllow},
		{"always allow degrades to allow", PermissionAlwaysAllow},
		{"allow persistent degrades to allow", PermissionAllowPersistent},
	} {
		t.Run(answer.name, func(t *testing.T) {
			asked := 0
			var reason string
			ctx := WithProfile(context.Background(), allowEverythingProfile())
			ctx = WithPermissionCallback(ctx, func(req PermissionRequest) PermissionDecision {
				asked++
				reason = req.Reason
				return answer.decision
			})
			ctx = WithConfirmEveryCall(ctx, func() bool { return true })
			if err := Authorize(ctx, action, ""); err != nil {
				t.Fatalf("strict refused a call the profile allows, without asking: %v", err)
			}
			if asked != 1 {
				t.Fatalf("the operator was asked %d times, want 1", asked)
			}
			if strings.Contains(reason, "approval scope") {
				t.Fatalf("the operator was shown an internal scope error: %q", reason)
			}
		})
	}

	t.Run("a refusal is still a refusal", func(t *testing.T) {
		ctx := WithProfile(context.Background(), allowEverythingProfile())
		ctx = WithPermissionCallback(ctx, func(PermissionRequest) PermissionDecision {
			return PermissionDeny
		})
		ctx = WithConfirmEveryCall(ctx, func() bool { return true })
		if err := Authorize(ctx, action, ""); err == nil {
			t.Fatal("a refused unscopable call was allowed anyway")
		}
	})

	t.Run("a genuine prompt with no scope is still a hard refusal", func(t *testing.T) {
		// Same command, but denied by the profile rather than allowed: this is
		// the branch strict must not have widened.
		asked := 0
		prof := allowEverythingProfile()
		prof.Shell = guard.ShellPerm{Policy: "allowlist", Patterns: []string{"nothing"}}
		ctx := WithProfile(context.Background(), prof)
		ctx = WithPermissionCallback(ctx, func(PermissionRequest) PermissionDecision {
			asked++
			return PermissionAllow
		})
		if err := Authorize(ctx, action, ""); err == nil {
			t.Fatal("a non-strict unscopable call was allowed")
		}
		if asked != 0 {
			t.Fatalf("the non-strict scope-error branch reached the callback %d times", asked)
		}
	})
}
