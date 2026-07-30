package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/x6nux/yanshi/internal/guard"
)

// newVCSContext builds a GuardedTool-ready context that allows vcs_* and binds
// the given VCSScope. Shared by the vcs tool tests.
func newVCSContext(sc VCSScope) context.Context {
	ctx := WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"vcs_*"}},
	})
	return WithVCS(ctx, sc)
}

func TestVCS_CommitTool_Main(t *testing.T) {
	v, repoID, root := newVCSTestRepo(t)
	vt := NewVCSTools()
	ctx := newVCSContext(VCSScope{VCS: v, RepoID: repoID, Agent: "orchestrator"})

	require.NoError(t, v.RecordEditMain(repoID, "orchestrator", filepath.Join(root, "a.go"), []byte("edited")))

	out, err := runTool(ctx, vt.Commit, `{"message":"edit a.go"}`)
	require.NoError(t, err)
	assert.Contains(t, out, "commit")

	// LogMain reflects the new commit.
	log, err := v.LogMain(repoID, 10)
	require.NoError(t, err)
	require.NotEmpty(t, log)
	assert.Equal(t, "edit a.go", log[0].Message)
}

func TestVCS_CommitTool_Worktree(t *testing.T) {
	v, repoID, _ := newVCSTestRepo(t)
	wt, err := v.AddWorktree(repoID, nil)
	require.NoError(t, err)

	vt := NewVCSTools()
	ctx := newVCSContext(VCSScope{VCS: v, WorktreeID: wt.ID, Agent: "worker-1"})

	require.NoError(t, v.RecordEditWorktree(wt.ID, "worker-1", filepath.Join(wt.Path, "a.go"), []byte("wt-edited")))

	out, err := runTool(ctx, vt.Commit, `{"message":"wt edit a.go"}`)
	require.NoError(t, err)
	assert.Contains(t, out, "commit")

	log, err := v.LogWorktree(wt.ID, 10)
	require.NoError(t, err)
	require.NotEmpty(t, log)
	assert.Equal(t, "wt edit a.go", log[0].Message)
}

func TestVCS_LogTool(t *testing.T) {
	v, repoID, root := newVCSTestRepo(t)
	vt := NewVCSTools()
	ctx := newVCSContext(VCSScope{VCS: v, RepoID: repoID, Agent: "orchestrator"})

	require.NoError(t, v.RecordEditMain(repoID, "o", filepath.Join(root, "a.go"), []byte("v2")))
	_, _ = v.CommitMain(repoID, "o", "second")
	require.NoError(t, v.RecordEditMain(repoID, "o", filepath.Join(root, "a.go"), []byte("v3")))
	_, _ = v.CommitMain(repoID, "o", "third")

	out, err := runTool(ctx, vt.Log, `{"limit":10}`)
	require.NoError(t, err)
	assert.Contains(t, out, "third")
	assert.Contains(t, out, "second")
}

func TestVCS_DiffTool(t *testing.T) {
	v, repoID, root := newVCSTestRepo(t)
	vt := NewVCSTools()
	ctx := newVCSContext(VCSScope{VCS: v, RepoID: repoID, Agent: "orchestrator"})

	log, err := v.LogMain(repoID, 5)
	require.NoError(t, err)
	require.NotEmpty(t, log)
	initCommit := log[0].ID

	require.NoError(t, v.RecordEditMain(repoID, "o", filepath.Join(root, "a.go"), []byte("changed")))
	cid2, err := v.CommitMain(repoID, "o", "edit a")
	require.NoError(t, err)

	out, err := runTool(ctx, vt.Diff, fmt.Sprintf(`{"ref_a":%q,"ref_b":%q}`, initCommit, cid2))
	require.NoError(t, err)
	assert.Contains(t, out, "a.go")
	assert.Contains(t, out, "modified")
}

func TestVCS_DiffTool_DefaultHeadVsParent(t *testing.T) {
	v, repoID, root := newVCSTestRepo(t)
	vt := NewVCSTools()
	ctx := newVCSContext(VCSScope{VCS: v, RepoID: repoID, Agent: "orchestrator"})

	// One real edit on top of init → active head has a parent.
	require.NoError(t, v.RecordEditMain(repoID, "o", filepath.Join(root, "a.go"), []byte("changed")))
	_, err := v.CommitMain(repoID, "o", "edit a")
	require.NoError(t, err)

	// No refs → defaults to head vs head's parent; the changed file is reported.
	out, err := runTool(ctx, vt.Diff, `{}`)
	require.NoError(t, err)
	assert.Contains(t, out, "a.go")
}

func TestVCS_RestoreTool(t *testing.T) {
	v, repoID, root := newVCSTestRepo(t)
	vt := NewVCSTools()
	ctx := newVCSContext(VCSScope{VCS: v, RepoID: repoID, Agent: "orchestrator"})

	// The init commit's tree holds a.go = "x".
	log, err := v.LogMain(repoID, 5)
	require.NoError(t, err)
	require.NotEmpty(t, log)
	initCommit := log[0].ID

	// Overwrite the working copy, then restore the original from the init commit.
	require.NoError(t, os.WriteFile(filepath.Join(root, "a.go"), []byte("overwritten"), 0o644))

	out, err := runTool(ctx, vt.Restore, fmt.Sprintf(`{"ref":%q,"path":"a.go"}`, initCommit))
	require.NoError(t, err)
	assert.Contains(t, out, "restored")

	got, err := os.ReadFile(filepath.Join(root, "a.go"))
	require.NoError(t, err)
	assert.Equal(t, "x", string(got))
}

func TestVCS_RestoreTool_Worktree(t *testing.T) {
	v, repoID, _ := newVCSTestRepo(t)
	wt, err := v.AddWorktree(repoID, nil)
	require.NoError(t, err)

	vt := NewVCSTools()
	ctx := newVCSContext(VCSScope{VCS: v, WorktreeID: wt.ID, Agent: "worker-1"})

	// Init commit holds a.go = "x".
	log, err := v.LogMain(repoID, 5)
	require.NoError(t, err)
	initCommit := log[0].ID

	// Clobber the file inside the worktree's working dir, then restore into it.
	require.NoError(t, os.WriteFile(filepath.Join(wt.Path, "a.go"), []byte("clobbered"), 0o644))

	_, err = runTool(ctx, vt.Restore, fmt.Sprintf(`{"ref":%q,"path":"a.go"}`, initCommit))
	require.NoError(t, err)

	got, err := os.ReadFile(filepath.Join(wt.Path, "a.go"))
	require.NoError(t, err)
	assert.Equal(t, "x", string(got))
}

func TestVCS_MergeTool_ConflictReported(t *testing.T) {
	v, repoID, _ := newVCSTestRepo(t)
	vt := NewVCSTools()
	ctx := newVCSContext(VCSScope{VCS: v, RepoID: repoID, Agent: "orchestrator"})

	// Two worktrees branched from the same base, both edit a.go differently.
	wt1, err := v.AddWorktree(repoID, nil)
	require.NoError(t, err)
	wt2, err := v.AddWorktree(repoID, nil)
	require.NoError(t, err)

	// wt1 edits a.go and merges first (fast-forward) → main now holds wt1's version.
	require.NoError(t, v.RecordEditWorktree(wt1.ID, "w1", filepath.Join(wt1.Path, "a.go"), []byte("from-wt1")))
	_, err = v.CommitWorktree(wt1.ID, "w1", "wt1 edits a")
	require.NoError(t, err)
	_, _, err = v.MergeToMain(wt1.ID, "orchestrator", false)
	require.NoError(t, err)

	// wt2 edits a.go differently → both main (via wt1) and wt2 changed a.go
	// relative to wt2's base, so merging wt2 conflicts.
	require.NoError(t, v.RecordEditWorktree(wt2.ID, "w2", filepath.Join(wt2.Path, "a.go"), []byte("from-wt2")))
	_, err = v.CommitWorktree(wt2.ID, "w2", "wt2 edits a")
	require.NoError(t, err)

	out, err := runTool(ctx, vt.Merge, fmt.Sprintf(`{"worktree":%q,"force":false}`, wt2.ID))
	require.NoError(t, err, "conflict must not surface as a tool error")
	assert.Contains(t, out, "a.go")
	assert.Contains(t, out, `"merged":""`)
}

func TestVCS_NoScope_Errors(t *testing.T) {
	vt := NewVCSTools()
	ctx := WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"vcs_*"}},
	})
	// No WithVCS → scope resolution fails, surfaced as a tool result.
	out, err := runTool(ctx, vt.Commit, `{"message":"x"}`)
	require.NoError(t, err, "operational error must surface as a result, not a Go error")
	assert.Contains(t, out, "✗")
	assert.Contains(t, out, "no VCS scope")
}
