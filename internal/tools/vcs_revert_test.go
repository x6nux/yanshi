package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/guard"
	"github.com/x6nux/yanshi/internal/vcs"
)

// TestRevertTool_RegisteredInTools confirms NewVCSTools().Tools() includes
// revert_turn (otherwise bootstrap's auto-registration would miss it).
func TestRevertTool_RegisteredInTools(t *testing.T) {
	tt := NewVCSTools()
	found := false
	for _, tool := range tt.Tools() {
		info, err := tool.Info(context.Background())
		if err != nil {
			t.Fatalf("Info: %v", err)
		}
		if info.Name == "revert_turn" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("revert_turn not registered in VCSTools.Tools()")
	}
	if tt.Revert == nil {
		t.Error("VCSTools.Revert field is nil")
	}
}

// allowRevertProfile returns a static profile whose Tools.Allow contains
// "revert_turn" explicitly (vcs_* glob does NOT match — 必修项 K). Uses the REAL
// guard types guard.PermissionProfile / guard.ToolsPerm (CB7).
func allowRevertProfile() guard.PermissionProfile {
	return guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"vcs_*", "revert_turn", "fs_*"}},
	}
}

// TestRevertTool_NoApprovalDeniesMainScope verifies the fail-closed path: with
// no interactive callback bound (SSE-style), runRevert returns a *DenyErr as
// its tool result (not a Go error — GuardedTool converts to result).
func TestRevertTool_NoApprovalDeniesMainScope(t *testing.T) {
	v, repoID, root := newVCSTestRepo(t)
	aPath := filepath.Join(root, "a.go")
	require.NoError(t, os.WriteFile(aPath, []byte("v1"), 0o644))
	if err := v.RecordEditMain(repoID, "u", aPath, []byte("v1")); err != nil {
		t.Fatal(err)
	}
	if _, err := v.CommitMain(repoID, "u", "v1"); err != nil {
		t.Fatal(err)
	}
	preID, err := v.SealMainTurnSeam(repoID, "s1", 0, 0, vcs.SeamPreTurn, "pre")
	if err != nil {
		t.Fatal(err)
	}
	require.NoError(t, os.WriteFile(aPath, []byte("v2"), 0o644))
	if err := v.RecordEditMain(repoID, "u", aPath, []byte("v2")); err != nil {
		t.Fatal(err)
	}
	if _, err := v.CommitMain(repoID, "u", "v2"); err != nil {
		t.Fatal(err)
	}

	tt := NewVCSTools()
	ctx := WithProfile(context.Background(), allowRevertProfile())
	ctx = WithVCS(ctx, VCSScope{VCS: v, RepoID: repoID, Agent: "test"})
	args, _ := json.Marshal(map[string]string{"seam_id": preID})
	result, err := runTool(ctx, tt.Revert, string(args))
	if err != nil {
		t.Fatalf("runTool err: %v", err)
	}
	if !strings.Contains(result, "destructive") && !strings.Contains(result, "denied") {
		t.Errorf("expected deny message, got %q", result)
	}
	got, _ := os.ReadFile(aPath)
	if string(got) != "v2" {
		t.Errorf("without approval a.go was modified: %q", got)
	}
}

// TestRevertTool_ApprovalRevertsMainScope verifies the happy path.
func TestRevertTool_ApprovalRevertsMainScope(t *testing.T) {
	v, repoID, root := newVCSTestRepo(t)
	aPath := filepath.Join(root, "a.go")
	require.NoError(t, os.WriteFile(aPath, []byte("v1"), 0o644))
	if err := v.RecordEditMain(repoID, "u", aPath, []byte("v1")); err != nil {
		t.Fatal(err)
	}
	if _, err := v.CommitMain(repoID, "u", "v1"); err != nil {
		t.Fatal(err)
	}
	preID, err := v.SealMainTurnSeam(repoID, "s1", 0, 0, vcs.SeamPreTurn, "pre")
	if err != nil {
		t.Fatal(err)
	}
	require.NoError(t, os.WriteFile(aPath, []byte("v2"), 0o644))
	if err := v.RecordEditMain(repoID, "u", aPath, []byte("v2")); err != nil {
		t.Fatal(err)
	}
	if _, err := v.CommitMain(repoID, "u", "v2"); err != nil {
		t.Fatal(err)
	}

	tt := NewVCSTools()
	ctx := WithProfile(context.Background(), allowRevertProfile())
	ctx = WithVCS(ctx, VCSScope{VCS: v, RepoID: repoID, Agent: "test"})
	ctx = WithPermissionCallback(ctx, func(req PermissionRequest) PermissionDecision {
		if req.Tool != "revert_turn" {
			t.Errorf("callback Tool=%q, want revert_turn", req.Tool)
		}
		return PermissionAllow
	})
	args, _ := json.Marshal(map[string]string{"seam_id": preID})
	result, err := runTool(ctx, tt.Revert, string(args))
	if err != nil {
		t.Fatalf("runTool err: %v", err)
	}
	if !strings.Contains(result, "undo") {
		t.Errorf("result missing undo field: %q", result)
	}
	got, _ := os.ReadFile(aPath)
	if string(got) != "v1" {
		t.Errorf("after approval a.go = %q, want v1(reverted)", got)
	}
}

// TestRevertTool_RejectsWorktreeScope verifies that when VCSScope.WorktreeID
// is set, runRevert returns a denial.
func TestRevertTool_RejectsWorktreeScope(t *testing.T) {
	v, repoID, _ := newVCSTestRepo(t)
	wt, err := v.AddWorktree(repoID, []string{"test"})
	if err != nil {
		t.Fatalf("AddWorktree: %v", err)
	}
	tt := NewVCSTools()
	ctx := WithProfile(context.Background(), allowRevertProfile())
	ctx = WithVCS(ctx, VCSScope{VCS: v, RepoID: repoID, WorktreeID: wt.ID, Agent: "test"})
	ctx = WithPermissionCallback(ctx, func(req PermissionRequest) PermissionDecision {
		return PermissionAllow
	})
	args, _ := json.Marshal(map[string]string{"seam_id": "any"})
	result, _ := runTool(ctx, tt.Revert, string(args))
	if !strings.Contains(result, "worktree") && !strings.Contains(result, "main only") {
		t.Errorf("expected worktree reject message, got %q", result)
	}
}

// TestRevertTool_DenyDoesNotModify verifies that a callback Deny leaves the
// working copy untouched.
func TestRevertTool_DenyDoesNotModify(t *testing.T) {
	v, repoID, root := newVCSTestRepo(t)
	aPath := filepath.Join(root, "a.go")
	require.NoError(t, os.WriteFile(aPath, []byte("v1"), 0o644))
	if err := v.RecordEditMain(repoID, "u", aPath, []byte("v1")); err != nil {
		t.Fatal(err)
	}
	if _, err := v.CommitMain(repoID, "u", "v1"); err != nil {
		t.Fatal(err)
	}
	preID, err := v.SealMainTurnSeam(repoID, "s1", 0, 0, vcs.SeamPreTurn, "pre")
	if err != nil {
		t.Fatal(err)
	}
	require.NoError(t, os.WriteFile(aPath, []byte("v2"), 0o644))
	if err := v.RecordEditMain(repoID, "u", aPath, []byte("v2")); err != nil {
		t.Fatal(err)
	}
	if _, err := v.CommitMain(repoID, "u", "v2"); err != nil {
		t.Fatal(err)
	}
	tt := NewVCSTools()
	ctx := WithProfile(context.Background(), allowRevertProfile())
	ctx = WithVCS(ctx, VCSScope{VCS: v, RepoID: repoID, Agent: "test"})
	ctx = WithPermissionCallback(ctx, func(req PermissionRequest) PermissionDecision {
		return PermissionDeny
	})
	args, _ := json.Marshal(map[string]string{"seam_id": preID})
	_, _ = runTool(ctx, tt.Revert, string(args))
	got, _ := os.ReadFile(aPath)
	if string(got) != "v2" {
		t.Errorf("Deny should leave a.go as v2, got %q", got)
	}
}
