package vcs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests are cohesive END-TO-END narratives that exercise the full VCS
// flow (init → record edits → commit → log → diff → restore / worktree → merge)
// against an in-memory store + temp repo root. Individual ops are covered by
// the focused tests in vcs_test.go; the goal here is to prove the ops compose
// into the real chat→main, worktree→merge, and conflict refuse/force flows.

// TestE2E_ChatToMain drives the orchestrator/chat narrative: a session edits
// files on main, commits, inspects history, diffs, and restores a prior version.
func TestE2E_ChatToMain(t *testing.T) {
	root := t.TempDir()
	// Seed a small project: app/main.go + README.md.
	require.NoError(t, os.MkdirAll(filepath.Join(root, "app"), 0o755))
	origMain := "package main\n\nfunc main() {}"
	require.NoError(t, os.WriteFile(filepath.Join(root, "app", "main.go"), []byte(origMain), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "README.md"), []byte("# project"), 0o644))

	v := newTestVCS(t)
	repoID, err := v.InitRepo(root)
	require.NoError(t, err)
	initCommit := v.getRepoMust(repoID).MainHead
	require.NotEmpty(t, initCommit)

	// The chat session edits an existing file and adds a brand-new one, then
	// commits both as a single atomic change.
	require.NoError(t, v.RecordEditMain(repoID, "alice", filepath.Join(root, "app", "main.go"), []byte("package main // edited")))
	require.NoError(t, v.RecordEditMain(repoID, "alice", filepath.Join(root, "greet.go"), []byte("package greet")))
	chatCommit, err := v.CommitMain(repoID, "alice", "chat edits")
	require.NoError(t, err)
	require.NotEmpty(t, chatCommit)

	// History: chat commit is newest, init is oldest; the parent chain links them.
	log, err := v.LogMain(repoID, 10)
	require.NoError(t, err)
	require.Len(t, log, 2)
	assert.Equal(t, chatCommit, log[0].ID, "newest commit first")
	assert.Equal(t, "chat edits", log[0].Message)
	assert.Equal(t, "alice", log[0].Author)
	assert.Equal(t, initCommit, log[1].ID, "init commit is oldest")
	assert.Equal(t, "vcs init", log[1].Message)
	assert.Equal(t, initCommit, log[0].ParentID, "chat commit's parent is the init commit")

	// Diff(init → chat): app/main.go modified, greet.go added, README unchanged.
	diffs, err := v.Diff(repoID, initCommit, chatCommit)
	require.NoError(t, err)
	byPath := make(map[string]FileDiff, len(diffs))
	for _, d := range diffs {
		byPath[d.Path] = d
	}
	assert.Equal(t, "modified", byPath["app/main.go"].Op, "app/main.go was edited")
	assert.Equal(t, "added", byPath["greet.go"].Op, "greet.go is a brand-new file")
	assert.NotContains(t, byPath, "README.md", "README was not touched")

	// Restore the init commit's app/main.go into the repo root → original content.
	// B2-RB1: Restore writes into a known working copy (repo root or worktree)
	// and records a pending edit; an arbitrary temp dir is no longer accepted.
	require.NoError(t, v.Restore(initCommit, "app/main.go", root))
	got, err := os.ReadFile(filepath.Join(root, "app", "main.go"))
	require.NoError(t, err)
	assert.Equal(t, origMain, string(got), "restored file matches the pre-edit original")
}

// TestE2E_WorktreeMergeToMain drives the task-agent narrative: a worker branches
// a worktree from main, works in isolation (main is untouched), then fast-forward
// merges back so main advances to the worktree's result.
func TestE2E_WorktreeMergeToMain(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "a.go"), []byte("a1"), 0o644))
	v := newTestVCS(t)
	repoID, err := v.InitRepo(root)
	require.NoError(t, err)
	initCommit := v.getRepoMust(repoID).MainHead

	// A task-agent branches a worktree from main_head.
	wt, err := v.AddWorktree(repoID, []string{"worker-1"})
	require.NoError(t, err)
	assert.Equal(t, initCommit, wt.BaseCommit, "worktree branched from main_head")

	// The agent edits a.go UNDER the worktree's working dir (the realistic flow
	// after the V18 path-resolution fix) and commits on the worktree branch.
	require.NoError(t, v.RecordEditWorktree(wt.ID, "worker-1", filepath.Join(wt.Path, "a.go"), []byte("wt-edit")))
	wtCommit, err := v.CommitWorktree(wt.ID, "worker-1", "wt work")
	require.NoError(t, err)
	require.NotEmpty(t, wtCommit)

	// Worktree edits are isolated: main_head did NOT move while the worker worked.
	assert.Equal(t, initCommit, v.getRepoMust(repoID).MainHead, "main untouched by worktree edits")

	// Merge back. Main hasn't moved since the branch, so base==ours and every
	// change is unambiguously theirs → fast-forward, no conflict, main advances.
	mainBefore := v.getRepoMust(repoID).MainHead
	mergeCommit, conflicts, err := v.MergeToMain(wt.ID, "worker-1", false)
	require.NoError(t, err)
	assert.Empty(t, conflicts, "fast-forward merge has no conflicts")
	assert.NotEqual(t, mainBefore, mergeCommit, "a merge commit was created")
	assert.Equal(t, mergeCommit, v.getRepoMust(repoID).MainHead, "main advanced to the merge commit")

	// History on main: init → merge. The merge commit's parent is init (FF).
	log, err := v.LogMain(repoID, 10)
	require.NoError(t, err)
	require.Len(t, log, 2)
	assert.True(t, strings.HasPrefix(log[0].Message, "merge worktree"), "newest entry is the merge commit")
	assert.Equal(t, "vcs init", log[1].Message)
	assert.Equal(t, initCommit, log[1].ID)
	assert.Equal(t, initCommit, log[0].ParentID, "merge commit's parent is init (fast-forward)")

	// The merge commit records the worktree it pulled in (merged_from attribution).
	mergeC, err := v.getCommit(mergeCommit)
	require.NoError(t, err)
	assert.Equal(t, wt.ID, mergeC.MergedFrom)

	// Diff(init → main_head): a.go changed to "wt-edit".
	diffs, err := v.Diff(repoID, initCommit, v.getRepoMust(repoID).MainHead)
	require.NoError(t, err)
	require.Len(t, diffs, 1)
	assert.Equal(t, "a.go", diffs[0].Path)
	assert.Equal(t, "modified", diffs[0].Op)
	assert.Equal(t, "wt-edit", mustReadBlob(t, v, v.getRepoMust(repoID).MainHead, "a.go"))
}

// TestE2E_ConflictRefusedThenForced drives the conflict narrative: a second
// worktree branched from an already-advanced main edits the same file main also
// re-edits, so the merge conflicts; without force it is refused (main unchanged),
// and with force theirs wins.
func TestE2E_ConflictRefusedThenForced(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "a.go"), []byte("v1"), 0o644))
	v := newTestVCS(t)
	repoID, err := v.InitRepo(root)
	require.NoError(t, err)

	// wt1: edit a.go → "from-wt1", commit, fast-forward merge to main.
	wt1, err := v.AddWorktree(repoID, nil)
	require.NoError(t, err)
	require.NoError(t, v.RecordEditWorktree(wt1.ID, "w", filepath.Join(wt1.Path, "a.go"), []byte("from-wt1")))
	_, err = v.CommitWorktree(wt1.ID, "w", "wt1 edits a")
	require.NoError(t, err)
	_, _, err = v.MergeToMain(wt1.ID, "m", false)
	require.NoError(t, err)
	assert.Equal(t, "from-wt1", mustReadBlob(t, v, v.getRepoMust(repoID).MainHead, "a.go"),
		"after wt1 FF-merge, main has from-wt1")

	// wt2: branched from the NEW main_head (which already has from-wt1).
	wt2, err := v.AddWorktree(repoID, nil)
	require.NoError(t, err)
	assert.Equal(t, v.getRepoMust(repoID).MainHead, wt2.BaseCommit,
		"wt2 branched from the current main_head (post-wt1 merge)")
	// Sanity: the materialized working copy reflects the branched tree.
	data, err := os.ReadFile(filepath.Join(wt2.Path, "a.go"))
	require.NoError(t, err)
	assert.Equal(t, "from-wt1", string(data))
	// wt2 edits a.go to "from-wt2" and commits.
	require.NoError(t, v.RecordEditWorktree(wt2.ID, "w", filepath.Join(wt2.Path, "a.go"), []byte("from-wt2")))
	_, err = v.CommitWorktree(wt2.ID, "w", "wt2 edits a")
	require.NoError(t, err)

	// Meanwhile main edits a.go AGAIN to "from-main".
	require.NoError(t, v.RecordEditMain(repoID, "m", filepath.Join(root, "a.go"), []byte("from-main")))
	_, err = v.CommitMain(repoID, "m", "main re-edits a")
	require.NoError(t, err)
	mainBeforeWT2Merge := v.getRepoMust(repoID).MainHead
	assert.Equal(t, "from-main", mustReadBlob(t, v, mainBeforeWT2Merge, "a.go"))

	// Merging wt2 without force: both sides changed a.go → conflict, refused.
	_, conflicts, err := v.MergeToMain(wt2.ID, "m", false)
	assert.ErrorIs(t, err, ErrConflicts)
	assert.Equal(t, []string{"a.go"}, conflicts)
	assert.Equal(t, mainBeforeWT2Merge, v.getRepoMust(repoID).MainHead,
		"main_head unchanged when the merge is refused")
	assert.Equal(t, "from-main", mustReadBlob(t, v, v.getRepoMust(repoID).MainHead, "a.go"),
		"main content is still from-main after refusal")

	// Force: theirs (wt2) wins on the conflicted path → main now has from-wt2.
	mergeCommit, conflicts2, err := v.MergeToMain(wt2.ID, "m", true)
	require.NoError(t, err)
	assert.Contains(t, conflicts2, "a.go", "conflict path still reported under force")
	assert.Equal(t, "from-wt2", mustReadBlob(t, v, mergeCommit, "a.go"),
		"theirs (wt2) wins on forced merge")
	assert.Equal(t, mergeCommit, v.getRepoMust(repoID).MainHead,
		"main advanced to the forced merge commit")
}
