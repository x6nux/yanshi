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

// allowRestoreProfile allows the new vcs_* tools. The vcs_* glob covers all of
// them, which is deliberate: unlike revert_turn, the destructive one here is
// gated by RequireApproval at call time rather than by profile name, so the
// profile does not need to spell it out.
func allowRestoreProfile() guard.PermissionProfile {
	return guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"vcs_*", "revert_turn", "fs_*"}},
	}
}

// restoreToolCtx binds a profile, a main-scope VCS and (optionally) an
// always-allow permission callback.
func restoreToolCtx(t *testing.T, v *vcs.VCS, repoID string, approve bool) context.Context {
	t.Helper()
	ctx := WithProfile(context.Background(), allowRestoreProfile())
	ctx = WithVCS(ctx, VCSScope{VCS: v, RepoID: repoID, Agent: "test"})
	if approve {
		ctx = WithPermissionCallback(ctx, func(PermissionRequest) PermissionDecision {
			return PermissionAllow
		})
	}
	return ctx
}

// vcsCommitFile writes, records and commits one file through the VCS. Named
// with a vcs prefix because internal/tools already has a git-flavoured
// commitFile in git_test.go.
func vcsCommitFile(t *testing.T, v *vcs.VCS, repoID, root, rel, content string) {
	t.Helper()
	abs := filepath.Join(root, filepath.FromSlash(rel))
	require.NoError(t, os.MkdirAll(filepath.Dir(abs), 0o755))
	require.NoError(t, os.WriteFile(abs, []byte(content), 0o644))
	require.NoError(t, v.RecordEditMain(repoID, "test", abs, []byte(content)))
	_, err := v.CommitMain(repoID, "test", "commit "+rel)
	require.NoError(t, err)
}

// decodeToolJSON parses a tool result into a map.
func decodeToolJSON(t *testing.T, s string) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		t.Fatalf("tool result is not JSON: %v\n%s", err, s)
	}
	return out
}

// TestNewVCSTools_RegistersEveryDeclaredTool guards the registration seam: a
// tool built into the struct but left out of Tools() is invisible to
// bootstrap's auto-registration, and S8's fail-closed registry check then
// rejects every call to it at runtime.
func TestNewVCSTools_RegistersEveryDeclaredTool(t *testing.T) {
	tt := NewVCSTools()
	registered := map[string]bool{}
	for _, tool := range tt.Tools() {
		info, err := tool.Info(context.Background())
		require.NoError(t, err)
		if registered[info.Name] {
			t.Errorf("tool %q registered twice", info.Name)
		}
		registered[info.Name] = true
	}
	for _, name := range []string{
		"vcs_commit", "vcs_log", "vcs_diff", "vcs_restore", "vcs_merge", "revert_turn",
		"vcs_preview_restore", "vcs_restore_files", "vcs_timeline", "vcs_worktrees", "vcs_gc",
	} {
		if !registered[name] {
			t.Errorf("%s is not in VCSTools.Tools(); bootstrap would never register it", name)
		}
	}
	// Every struct field must be non-nil, or Tools() hands bootstrap a nil
	// element and Info() panics during startup.
	for i, tool := range tt.Tools() {
		if tool == nil {
			t.Fatalf("Tools()[%d] is nil", i)
		}
	}
}

// TestPreviewRestore_IsReadOnlyAndYieldsAToken is the V1 tool contract: the
// preview reports, never writes, and hands back the token the apply requires.
func TestPreviewRestore_IsReadOnlyAndYieldsAToken(t *testing.T) {
	v, repoID, root := newVCSTestRepo(t)
	vcsCommitFile(t, v, repoID, root, "f.txt", "v1\n")
	seamID, err := v.SealMainTurnSeam(repoID, "s1", 0, 0, vcs.SeamPreTurn, "pre")
	require.NoError(t, err)
	vcsCommitFile(t, v, repoID, root, "f.txt", "v2\n")

	tt := NewVCSTools()
	// No permission callback: a read-only preview must not need one. If it did,
	// SSE (which has no callback at all) could never preview and the mandatory
	// dry run would be unreachable there.
	ctx := restoreToolCtx(t, v, repoID, false)
	args, _ := json.Marshal(map[string]string{"seam_id": seamID})
	result, err := runTool(ctx, tt.Preview, string(args))
	require.NoError(t, err)

	got := decodeToolJSON(t, result)
	if got["applied"] != false {
		t.Errorf("applied = %v, want false", got["applied"])
	}
	token, _ := got["confirm_token"].(string)
	if token == "" {
		t.Fatalf("preview returned no confirm_token: %s", result)
	}
	if got["overwrite"].(float64) < 1 {
		t.Errorf("preview reported no overwrite for a changed file: %s", result)
	}
	// Read-only means read-only.
	data, err := os.ReadFile(filepath.Join(root, "f.txt"))
	require.NoError(t, err)
	if string(data) != "v2\n" {
		t.Errorf("the preview modified the working copy: %q", data)
	}
}

// TestRestoreFiles_RequiresBothTheTokenAndTheApproval is the whole point of the
// two-step protocol: the model must have previewed AND the human must have
// agreed. Neither substitutes for the other.
func TestRestoreFiles_RequiresBothTheTokenAndTheApproval(t *testing.T) {
	tests := []struct {
		name       string
		withToken  bool
		approve    bool
		wantMarker string
	}{
		{name: "no token, no approval", wantMarker: "confirm_token is required"},
		{name: "no token but approved", approve: true, wantMarker: "confirm_token is required"},
		{name: "token but no approval", withToken: true, wantMarker: "denied"},
		{name: "token and approval", withToken: true, approve: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v, repoID, root := newVCSTestRepo(t)
			vcsCommitFile(t, v, repoID, root, "f.txt", "v1\n")
			seamID, err := v.SealMainTurnSeam(repoID, "s1", 0, 0, vcs.SeamPreTurn, "pre")
			require.NoError(t, err)
			vcsCommitFile(t, v, repoID, root, "f.txt", "v2\n")

			tt := NewVCSTools()
			token := ""
			if tc.withToken {
				previewArgs, _ := json.Marshal(map[string]string{"seam_id": seamID})
				preview, perr := runTool(
					restoreToolCtx(t, v, repoID, false), tt.Preview, string(previewArgs))
				require.NoError(t, perr)
				token, _ = decodeToolJSON(t, preview)["confirm_token"].(string)
				require.NotEmpty(t, token)
			}
			args, _ := json.Marshal(map[string]string{
				"seam_id": seamID, "confirm_token": token,
			})
			result, err := runTool(
				restoreToolCtx(t, v, repoID, tc.approve), tt.RestoreFiles, string(args))
			require.NoError(t, err)

			data, rerr := os.ReadFile(filepath.Join(root, "f.txt"))
			require.NoError(t, rerr)
			if tc.wantMarker == "" {
				if string(data) != "v1\n" {
					t.Errorf("f.txt = %q, want the restored v1", data)
				}
				if decodeToolJSON(t, result)["applied"] != true {
					t.Errorf("result must report applied=true: %s", result)
				}
				return
			}
			if !strings.Contains(result, tc.wantMarker) {
				t.Errorf("result %q does not contain %q", result, tc.wantMarker)
			}
			if string(data) != "v2\n" {
				t.Errorf("a refused restore modified f.txt to %q", data)
			}
		})
	}
}

// TestRestoreFiles_SelectivelyRestoresGlobs is the V4 tool path end to end.
func TestRestoreFiles_SelectivelyRestoresGlobs(t *testing.T) {
	v, repoID, root := newVCSTestRepo(t)
	vcsCommitFile(t, v, repoID, root, "src/a.go", "package a // good\n")
	vcsCommitFile(t, v, repoID, root, "src/b.go", "package b // good\n")
	vcsCommitFile(t, v, repoID, root, "docs/x.md", "# good\n")
	log, err := v.LogMain(repoID, 1)
	require.NoError(t, err)
	good := log[0].ID
	vcsCommitFile(t, v, repoID, root, "src/a.go", "package a // BROKEN\n")
	vcsCommitFile(t, v, repoID, root, "src/b.go", "package b // BROKEN\n")
	vcsCommitFile(t, v, repoID, root, "docs/x.md", "# BROKEN\n")

	tt := NewVCSTools()
	previewArgs, _ := json.Marshal(map[string]string{"commit": good, "paths": "src/*"})
	preview, err := runTool(restoreToolCtx(t, v, repoID, false), tt.Preview, string(previewArgs))
	require.NoError(t, err)
	token, _ := decodeToolJSON(t, preview)["confirm_token"].(string)
	require.NotEmpty(t, token)

	applyArgs, _ := json.Marshal(map[string]string{
		"commit": good, "paths": "src/*", "confirm_token": token,
	})
	result, err := runTool(restoreToolCtx(t, v, repoID, true), tt.RestoreFiles, string(applyArgs))
	require.NoError(t, err)
	if decodeToolJSON(t, result)["applied"] != true {
		t.Fatalf("selective restore did not apply: %s", result)
	}
	for path, want := range map[string]string{
		"src/a.go":  "package a // good\n",
		"src/b.go":  "package b // good\n",
		"docs/x.md": "# BROKEN\n", // outside the selector: untouched
	} {
		data, rerr := os.ReadFile(filepath.Join(root, filepath.FromSlash(path)))
		require.NoError(t, rerr)
		if string(data) != want {
			t.Errorf("%s = %q, want %q", path, data, want)
		}
	}
}

// TestRestoreFiles_RejectsAMismatchedSelector proves the token is bound to the
// selector too: previewing "src/*" and applying "docs/*" must not be possible.
func TestRestoreFiles_RejectsAMismatchedSelector(t *testing.T) {
	v, repoID, root := newVCSTestRepo(t)
	vcsCommitFile(t, v, repoID, root, "src/a.go", "good\n")
	vcsCommitFile(t, v, repoID, root, "docs/x.md", "good\n")
	log, err := v.LogMain(repoID, 1)
	require.NoError(t, err)
	good := log[0].ID
	vcsCommitFile(t, v, repoID, root, "src/a.go", "BROKEN\n")
	vcsCommitFile(t, v, repoID, root, "docs/x.md", "BROKEN\n")

	tt := NewVCSTools()
	previewArgs, _ := json.Marshal(map[string]string{"commit": good, "paths": "src/*"})
	preview, err := runTool(restoreToolCtx(t, v, repoID, false), tt.Preview, string(previewArgs))
	require.NoError(t, err)
	token, _ := decodeToolJSON(t, preview)["confirm_token"].(string)

	applyArgs, _ := json.Marshal(map[string]string{
		"commit": good, "paths": "docs/*", "confirm_token": token,
	})
	result, err := runTool(restoreToolCtx(t, v, repoID, true), tt.RestoreFiles, string(applyArgs))
	require.NoError(t, err)
	if !strings.Contains(result, "stale") {
		t.Errorf("a selector swap must be rejected as stale, got %s", result)
	}
	data, _ := os.ReadFile(filepath.Join(root, "docs", "x.md"))
	if string(data) != "BROKEN\n" {
		t.Errorf("docs/x.md was restored through a token minted for src/*: %q", data)
	}
}

// TestRestoreTools_RejectWorktreeScope: the seams, retention roots and orphan
// ledger are all main-line concepts, so a worktree scope gets a message rather
// than a confusing deeper error.
func TestRestoreTools_RejectWorktreeScope(t *testing.T) {
	v, repoID, _ := newVCSTestRepo(t)
	wt, err := v.AddWorktree(repoID, []string{"agent"})
	require.NoError(t, err)
	tt := NewVCSTools()
	ctx := WithProfile(context.Background(), allowRestoreProfile())
	ctx = WithVCS(ctx, VCSScope{VCS: v, RepoID: repoID, WorktreeID: wt.ID, Agent: "test"})
	ctx = WithPermissionCallback(ctx, func(PermissionRequest) PermissionDecision {
		return PermissionAllow
	})

	for name, tool := range map[string]*GuardedTool{
		"vcs_preview_restore": tt.Preview,
		"vcs_restore_files":   tt.RestoreFiles,
		"vcs_timeline":        tt.Timeline,
		"vcs_worktrees":       tt.Worktrees,
		"vcs_gc":              tt.GC,
	} {
		t.Run(name, func(t *testing.T) {
			result, err := runTool(ctx, tool, `{"confirm_token":"x","commit":"y"}`)
			require.NoError(t, err)
			if !strings.Contains(result, "main scope only") {
				t.Errorf("result %q does not reject the worktree scope", result)
			}
		})
	}
}

// TestPreviewRestore_TargetArgumentIsExclusive pins the seam_id/commit rule:
// accepting both and preferring one would let a preview and an apply that
// disagree about which field matters silently target different commits.
func TestPreviewRestore_TargetArgumentIsExclusive(t *testing.T) {
	v, repoID, root := newVCSTestRepo(t)
	vcsCommitFile(t, v, repoID, root, "f.txt", "v1\n")
	seamID, err := v.SealMainTurnSeam(repoID, "s1", 0, 0, vcs.SeamPreTurn, "pre")
	require.NoError(t, err)
	log, err := v.LogMain(repoID, 1)
	require.NoError(t, err)

	tt := NewVCSTools()
	ctx := restoreToolCtx(t, v, repoID, false)
	tests := []struct {
		name string
		args map[string]string
		want string
	}{
		{name: "neither", args: map[string]string{}, want: "pass seam_id or commit"},
		{
			name: "both",
			args: map[string]string{"seam_id": seamID, "commit": log[0].ID},
			want: "not both",
		},
		{name: "unknown seam", args: map[string]string{"seam_id": "nope"}, want: "seam nope"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			args, _ := json.Marshal(tc.args)
			result, err := runTool(ctx, tt.Preview, string(args))
			require.NoError(t, err)
			if !strings.Contains(result, tc.want) {
				t.Errorf("result %q does not contain %q", result, tc.want)
			}
		})
	}
}

// TestTimelineTool_ReportsQuestionsAndSeamIDs covers the V3 tool: the model must
// get back something it can act on (a seam id) alongside something a human can
// recognise (the question).
func TestTimelineTool_ReportsQuestionsAndSeamIDs(t *testing.T) {
	v, repoID, root := newVCSTestRepo(t)
	vcsCommitFile(t, v, repoID, root, "f.txt", "v1\n")
	seamID, err := v.SealMainTurnSeam(repoID, "", 1, 0, vcs.SeamPostTurn, "post")
	require.NoError(t, err)

	tt := NewVCSTools()
	result, err := runTool(restoreToolCtx(t, v, repoID, false), tt.Timeline, `{}`)
	require.NoError(t, err)
	entries, ok := decodeToolJSON(t, result)["timeline"].([]any)
	if !ok || len(entries) != 1 {
		t.Fatalf("timeline = %s, want one entry", result)
	}
	entry := entries[0].(map[string]any)
	if entry["seam_id"] != seamID {
		t.Errorf("seam_id = %v, want %s", entry["seam_id"], seamID)
	}
	if entry["is_head"] != true {
		t.Errorf("the only seam points at head; is_head = %v", entry["is_head"])
	}
	if entry["files_changed"].(float64) < 1 {
		t.Errorf("files_changed = %v, want >= 1", entry["files_changed"])
	}
}

// TestGCTool_DefaultsToADryRun is the V2 tool contract: forgetting `confirm` is
// the safe direction, and only the confirming call needs approval.
func TestGCTool_DefaultsToADryRun(t *testing.T) {
	v, repoID, root := newVCSTestRepo(t)
	vcsCommitFile(t, v, repoID, root, "f.txt", "v1\n")

	tt := NewVCSTools()
	// No permission callback: a dry run must not cost a dialog, or the model
	// learns to skip straight to the destructive call.
	result, err := runTool(restoreToolCtx(t, v, repoID, false), tt.GC, `{}`)
	require.NoError(t, err)
	got := decodeToolJSON(t, result)
	if got["dry_run"] != true {
		t.Errorf("omitting confirm must produce a dry run: %s", result)
	}
	if _, ok := got["next_step"]; !ok {
		t.Errorf("a dry run must tell the caller how to confirm: %s", result)
	}

	// The confirming call, with no callback bound, must be denied.
	denied, err := runTool(restoreToolCtx(t, v, repoID, false), tt.GC, `{"confirm":true}`)
	require.NoError(t, err)
	if !strings.Contains(denied, "denied") {
		t.Errorf("confirm=true without an approval callback must be denied: %s", denied)
	}
}

// TestWorktreesTool_ReportsLifecycleAndOrphans covers the V7 tool.
func TestWorktreesTool_ReportsLifecycleAndOrphans(t *testing.T) {
	v, repoID, _ := newVCSTestRepo(t)
	wt, err := v.AddWorktree(repoID, []string{"agent"})
	require.NoError(t, err)
	require.NoError(t, v.ClaimWorktree(wt.ID, 424242))
	v.SetProcessAlive(func(pid int) bool { return pid != 424242 })

	tt := NewVCSTools()
	ctx := restoreToolCtx(t, v, repoID, false)

	all, err := runTool(ctx, tt.Worktrees, `{}`)
	require.NoError(t, err)
	got := decodeToolJSON(t, all)
	if got["orphan_count"].(float64) != 1 {
		t.Errorf("orphan_count = %v, want 1: %s", got["orphan_count"], all)
	}
	list := got["worktrees"].([]any)
	if len(list) != 1 {
		t.Fatalf("worktrees = %s, want one entry", all)
	}
	entry := list[0].(map[string]any)
	if entry["worktree_id"] != wt.ID || entry["orphaned"] != true {
		t.Errorf("entry = %v, want %s flagged orphaned", entry, wt.ID)
	}
	if entry["lifecycle"] != string(vcs.WorktreeActive) {
		t.Errorf("lifecycle = %v, want %q", entry["lifecycle"], vcs.WorktreeActive)
	}

	onlyOrphans, err := runTool(ctx, tt.Worktrees, `{"orphans_only":true}`)
	require.NoError(t, err)
	if len(decodeToolJSON(t, onlyOrphans)["worktrees"].([]any)) != 1 {
		t.Errorf("orphans_only listing = %s", onlyOrphans)
	}
}

// TestSplitSelectors pins the comma parsing, including the trailing-comma case
// that would otherwise become a selector matching nothing — which PlanRestore
// correctly rejects, turning a harmless typo into a failed restore.
func TestSplitSelectors(t *testing.T) {
	tests := []struct {
		in   string
		want []string
	}{
		{in: "", want: nil},
		{in: "a", want: []string{"a"}},
		{in: "a,b", want: []string{"a", "b"}},
		{in: " a , b ", want: []string{"a", "b"}},
		{in: "a,,b", want: []string{"a", "b"}},
		{in: "a,", want: []string{"a"}},
		{in: ",", want: nil},
		{in: "   ", want: nil},
	}
	for _, tc := range tests {
		got := splitSelectors(tc.in)
		if strings.Join(got, "|") != strings.Join(tc.want, "|") {
			t.Errorf("splitSelectors(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}
