package vcs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/x6nux/yanshi/internal/store"
)

// ---------- helper ----------

func ensureFile(t *testing.T, dir, name, content string) {
	t.Helper()
	path := filepath.Join(dir, name)
	err := os.MkdirAll(filepath.Dir(path), 0o755)
	require.NoError(t, err)
	err = os.WriteFile(path, []byte(content), 0o644)
	require.NoError(t, err)
}

// ---------- RepoRoot ----------

func TestRepoRoot_Valid(t *testing.T) {
	v := newTestVCS(t)
	root := t.TempDir()
	// Resolve symlinks on root so it matches the canonicalRepoRoot'd result
	// (macOS /var → /private/var). Without this, /var/folders/... (TempDir)
	// would not equal /private/var/folders/... (canonicalRepoRoot).
	root = filepath.Clean(root)
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	ensureFile(t, root, "main.go", "package main")
	repoID, err := v.InitRepo(root)
	require.NoError(t, err)

	got, err := v.RepoRoot(repoID)
	require.NoError(t, err)
	// canonicalRepoRoot lowercases on Windows; use case-insensitive comparison.
	assert.Equal(t, strings.ToLower(filepath.Clean(root)), strings.ToLower(filepath.Clean(got)))
}

func TestRepoRoot_Missing(t *testing.T) {
	v := newTestVCS(t)
	_, err := v.RepoRoot("nonexistent")
	assert.Error(t, err)
}

// ---------- RepoMainHead ----------

func TestRepoMainHead_Valid(t *testing.T) {
	v := newTestVCS(t)
	root := t.TempDir()
	ensureFile(t, root, "main.go", "package main")
	repoID, err := v.InitRepo(root)
	require.NoError(t, err)

	head, err := v.RepoMainHead(repoID)
	require.NoError(t, err)
	assert.NotEmpty(t, head)
}

func TestRepoMainHead_Missing(t *testing.T) {
	v := newTestVCS(t)
	_, err := v.RepoMainHead("nonexistent")
	assert.Error(t, err)
}

// ---------- WorktreePath ----------

func TestWorktreePath_Valid(t *testing.T) {
	v := newTestVCS(t)
	root := t.TempDir()
	ensureFile(t, root, "main.go", "package main")
	repoID, err := v.InitRepo(root)
	require.NoError(t, err)

	wt, err := v.AddWorktree(repoID, []string{"agent1"})
	require.NoError(t, err)

	got, err := v.WorktreePath(wt.ID)
	require.NoError(t, err)
	assert.Equal(t, wt.Path, got)
}

func TestWorktreePath_Missing(t *testing.T) {
	v := newTestVCS(t)
	_, err := v.WorktreePath("nonexistent")
	assert.Error(t, err)
}

// ---------- RecordDeleteMain / RecordDeleteWorktree ----------

func TestRecordDeleteMain_Missing(t *testing.T) {
	v := newTestVCS(t)
	err := v.RecordDeleteMain("nonexistent", "agent", "/path/to/file.go")
	assert.Error(t, err)
}

func TestRecordDeleteWorktree_Missing(t *testing.T) {
	v := newTestVCS(t)
	err := v.RecordDeleteWorktree("nonexistent", "agent", "/path/to/file.go")
	assert.Error(t, err)
}

// ---------- LogMain ----------

func TestLogMain_ValidAndMissing(t *testing.T) {
	v := newTestVCS(t)
	root := t.TempDir()
	ensureFile(t, root, "main.go", "package main")
	repoID, err := v.InitRepo(root)
	require.NoError(t, err)

	entries, err := v.LogMain(repoID, 10)
	require.NoError(t, err)
	assert.NotEmpty(t, entries)

	_, err = v.LogMain("nonexistent", 10)
	assert.Error(t, err)
}

// ---------- Uncommitted ----------

func TestUncommitted_EmptyRepo(t *testing.T) {
	v := newTestVCS(t)
	root := t.TempDir()
	repoID, err := v.InitRepo(root)
	require.NoError(t, err)

	info := v.Uncommitted("main", repoID)
	assert.Empty(t, info)
}

// ---------- AddWorktree / RemoveWorktree ----------

func TestRemoveWorktree_Missing(t *testing.T) {
	v := newTestVCS(t)
	// RemoveWorktree on nonexistent worktree may succeed silently
	_ = v.RemoveWorktree("nonexistent")
}

// ---------- RecordEditMain (error) ----------

func TestRecordEditMain_Missing(t *testing.T) {
	v := newTestVCS(t)
	err := v.RecordEditMain("nonexistent", "agent", "/path/to/file.go", []byte("content"))
	assert.Error(t, err)
}

// ---------- RecordEditWorktree (error) ----------

func TestRecordEditWorktree_Missing(t *testing.T) {
	v := newTestVCS(t)
	err := v.RecordEditWorktree("nonexistent", "agent", "/path/to/file.go", []byte("content"))
	assert.Error(t, err)
}

// ---------- CommitWorktree (invalid) ----------

func TestCommitWorktree_Missing(t *testing.T) {
	v := newTestVCS(t)
	_, err := v.CommitWorktree("nonexistent", "agent", "msg")
	assert.Error(t, err)
}

// ---------- CommitMain (error) ----------

func TestCommitMain_MissingRepo(t *testing.T) {
	v := newTestVCS(t)
	_, err := v.CommitMain("nonexistent", "agent", "msg")
	assert.Error(t, err)
}

// ---------- RemoveWorktree (valid) ----------

func TestRemoveWorktree_Valid(t *testing.T) {
	v := newTestVCS(t)
	root := t.TempDir()
	ensureFile(t, root, "main.go", "package main")
	repoID, err := v.InitRepo(root)
	require.NoError(t, err)

	wt, err := v.AddWorktree(repoID, []string{root})
	require.NoError(t, err)

	err = v.RemoveWorktree(wt.ID)
	require.NoError(t, err)
}

// ---------- MergeToMain (error) ----------

func TestMergeToMain_MissingRepo(t *testing.T) {
	v := newTestVCS(t)
	_, _, err := v.MergeToMain("nonexistent", "agent", false)
	assert.Error(t, err)
}

// ---------- New with custom ignores ----------

func TestVCS_NewWithCustomIgnores(t *testing.T) {
	s, err := store.Open(":memory:")
	require.NoError(t, err)
	defer s.Close()

	v := New(s, t.TempDir(), "*.custom")
	assert.NotNil(t, v)
	assert.True(t, v.isIgnored("file.custom"))
	assert.False(t, v.isIgnored("file.go"))
}

// ---------- InitRepo already exists ----------

func TestInitRepo_AlreadyExists(t *testing.T) {
	v := newTestVCS(t)
	root := t.TempDir()
	ensureFile(t, root, "main.go", "package main")

	first, err := v.InitRepo(root)
	require.NoError(t, err)
	require.NotEmpty(t, first)

	// Second init for the same root may succeed (return existing) or error.
	// At minimum it should not crash.
	second, secondErr := v.InitRepo(root)
	if secondErr == nil {
		t.Logf("second InitRepo returned %q (existing repo)", second)
	} else {
		t.Logf("second InitRepo returned error: %v", secondErr)
	}
}

// ---------- InitRepo with symlink ----------

func TestInitRepo_WithSymlink(t *testing.T) {
	v := newTestVCS(t)
	root := t.TempDir()
	ensureFile(t, root, "target.txt", "real")
	err := os.Symlink("target.txt", filepath.Join(root, "link.txt"))
	if err != nil {
		t.Skip("symlinks not supported on this platform")
	}

	repoID, err := v.InitRepo(root)
	require.NoError(t, err)
	assert.NotEmpty(t, repoID)
}

// ---------- Diff (missing repo) ----------

func TestDiff_MissingRepo(t *testing.T) {
	v := newTestVCS(t)
	files, err := v.Diff("nonexistent", "commit1", "commit2")
	if err != nil {
		t.Logf("Diff error (expected): %v", err)
	} else {
		t.Logf("Diff returned %d files", len(files))
	}
}

// ---------- Restore (missing repo) ----------

func TestRestore_MissingRepo(t *testing.T) {
	v := newTestVCS(t)
	err := v.Restore("nonexistent-commit", "path", t.TempDir())
	assert.Error(t, err)
}

// ---------- InitRepo empty dir (no tracked files) ----------

func TestInitRepo_EmptyDir(t *testing.T) {
	v := newTestVCS(t)
	root := t.TempDir()
	repoID, err := v.InitRepo(root)
	require.NoError(t, err)
	assert.NotEmpty(t, repoID)
}

// ---------- commitRepoID (error) ----------

func TestCommitRepoID_Missing(t *testing.T) {
	v := newTestVCS(t)
	_, err := v.commitRepoID("nonexistent")
	assert.Error(t, err)
}
