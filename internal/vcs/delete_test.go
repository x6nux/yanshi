package vcs

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRecordDelete_MarksPathDeleted proves RecordDeleteMain writes a row with
// op="deleted" and that a subsequent CommitMain folds the deletion into the
// commit tree (the path is gone). This is the autoVCS contract for a deleted
// file: it must NOT persist in the committed snapshot.
func TestRecordDelete_MarksPathDeleted(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "a.go"), []byte("package main"), 0o644))
	v := newTestVCS(t)
	repoID, err := v.InitRepo(root)
	require.NoError(t, err)

	// a.go is tracked in main_head (InitRepo snapshots existing files).
	require.NoError(t, v.RecordDeleteMain(repoID, "orchestrator", filepath.Join(root, "a.go")))

	// The uncommitted changeset carries a.go with op="deleted".
	pending := v.Uncommitted("main", repoID)
	assert.Contains(t, pending, "a.go", "deleted path must be in the changeset")

	var op string
	row := v.store.DB.QueryRow(
		"SELECT op FROM vcs_uncommitted WHERE scope_type='main' AND scope_id=? AND path='a.go'",
		repoID)
	require.NoError(t, row.Scan(&op))
	assert.Equal(t, "deleted", op)

	// Committing folds the delete into the tree: a.go must be absent.
	commitID, err := v.CommitMain(repoID, "orchestrator", "delete a.go")
	require.NoError(t, err)
	assert.NotContains(t, v.commitTree(commitID), "a.go",
		"committed tree must not contain the deleted file")
}

// TestRecordDelete_WorktreeScope proves the worktree-scoped delete is symmetric
// with RecordEditWorktree and does not leak into main.
func TestRecordDelete_WorktreeScope(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "a.go"), []byte("x"), 0o644))
	v := newTestVCS(t)
	repoID, err := v.InitRepo(root)
	require.NoError(t, err)
	repo, _ := v.getRepo(repoID)
	// Insert a worktree row manually (AddWorktree materialization is not needed
	// to test the changeset recording path; mirror TestRecordEdit_WorktreeScope).
	wtID := "wt-del"
	_, err = v.store.DB.Exec(
		"INSERT INTO vcs_worktrees (id, repo_id, path, base_commit, created_at, active) VALUES (?, ?, ?, ?, ?, 1)",
		wtID, repoID, root, repo.MainHead, time.Now().Unix())
	require.NoError(t, err)

	require.NoError(t, v.RecordDeleteWorktree(wtID, "worker-1", filepath.Join(root, "a.go")))
	assert.Contains(t, v.Uncommitted("worktree", wtID), "a.go")
	assert.Empty(t, v.Uncommitted("main", repoID), "worktree delete must not touch main")
}

// TestRecordDelete_OutsideRepoSkipped mirrors RecordEdit's silent-skip behavior:
// a path outside the repo root is a no-op (no error, no row).
func TestRecordDelete_OutsideRepoSkipped(t *testing.T) {
	root := t.TempDir()
	other := t.TempDir()
	v := newTestVCS(t)
	repoID, _ := v.InitRepo(root)
	require.NoError(t, v.RecordDeleteMain(repoID, "orchestrator", filepath.Join(other, "external.go")))
	assert.Empty(t, v.Uncommitted("main", repoID))
}
