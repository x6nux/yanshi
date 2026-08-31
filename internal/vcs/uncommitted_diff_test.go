package vcs

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestUncommittedDiff_AddedModifiedDeleted drives the three op kinds
// UncommittedDiff must distinguish: a brand-new path (added, empty OldText),
// an existing path re-edited (modified, both texts populated), and a path
// removed after being committed once (deleted, empty NewText) — the same
// three cases commitScope folds when the changeset is committed.
func TestUncommittedDiff_AddedModifiedDeleted(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "keep.go"), []byte("package main"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "gone.go"), []byte("package main // to delete"), 0o644))

	v := newTestVCS(t)
	repoID, err := v.InitRepo(root)
	require.NoError(t, err)

	require.NoError(t, v.RecordEditMain(repoID, "u", filepath.Join(root, "keep.go"), []byte("package main // edited")))
	require.NoError(t, v.RecordEditMain(repoID, "u", filepath.Join(root, "new.go"), []byte("package added")))
	require.NoError(t, v.RecordDeleteMain(repoID, "u", filepath.Join(root, "gone.go")))

	diffs, err := v.UncommittedDiff("main", repoID)
	require.NoError(t, err)
	byPath := make(map[string]UncommittedFile, len(diffs))
	for _, d := range diffs {
		byPath[d.Path] = d
	}
	require.Contains(t, byPath, "keep.go")
	require.Contains(t, byPath, "new.go")
	require.Contains(t, byPath, "gone.go")

	mod := byPath["keep.go"]
	assert.Equal(t, "modified", mod.Op)
	assert.Equal(t, "package main", mod.OldText)
	assert.Equal(t, "package main // edited", mod.NewText)

	added := byPath["new.go"]
	assert.Equal(t, "added", added.Op)
	assert.Empty(t, added.OldText, "added file has no prior blob")
	assert.Equal(t, "package added", added.NewText)

	deleted := byPath["gone.go"]
	assert.Equal(t, "deleted", deleted.Op)
	assert.Equal(t, "package main // to delete", deleted.OldText)
	assert.Empty(t, deleted.NewText, "deleted file has no new blob")
}

// TestUncommittedDiff_UnknownScopeEmpty proves an unknown/empty scope yields
// a non-nil empty slice (not an error) — mirrors Uncommitted's empty-map
// behavior for the same input shape, so callers (the ws_diff handler) can
// treat "no VCS yet" and "nothing pending" identically without a type switch.
func TestUncommittedDiff_UnknownScopeEmpty(t *testing.T) {
	v := newTestVCS(t)
	diffs, err := v.UncommittedDiff("main", "nonexistent-repo")
	require.NoError(t, err)
	assert.NotNil(t, diffs)
	assert.Empty(t, diffs)
}

// TestUncommittedDiff_Sorted proves the result is sorted by path, so the
// /diff command can render a stable, deterministic file list.
func TestUncommittedDiff_Sorted(t *testing.T) {
	root := t.TempDir()
	v := newTestVCS(t)
	repoID, err := v.InitRepo(root)
	require.NoError(t, err)

	require.NoError(t, v.RecordEditMain(repoID, "u", filepath.Join(root, "zebra.go"), []byte("z")))
	require.NoError(t, v.RecordEditMain(repoID, "u", filepath.Join(root, "apple.go"), []byte("a")))
	require.NoError(t, v.RecordEditMain(repoID, "u", filepath.Join(root, "mango.go"), []byte("m")))

	diffs, err := v.UncommittedDiff("main", repoID)
	require.NoError(t, err)
	require.Len(t, diffs, 3)
	assert.Equal(t, []string{"apple.go", "mango.go", "zebra.go"}, []string{diffs[0].Path, diffs[1].Path, diffs[2].Path})
}

// TestUncommittedDiff_BinaryContentLeftEmpty proves a non-UTF8 blob is
// excluded from the Text fields (guarding against feeding raw binary bytes
// into difflib.Compute) while Op is still reported, so the /diff command can
// list a binary path without corrupting the terminal.
func TestUncommittedDiff_BinaryContentLeftEmpty(t *testing.T) {
	root := t.TempDir()
	v := newTestVCS(t)
	repoID, err := v.InitRepo(root)
	require.NoError(t, err)

	binary := []byte{0xff, 0xfe, 0x00, 0x01, 0x02}
	require.NoError(t, v.RecordEditMain(repoID, "u", filepath.Join(root, "blob.bin"), binary))

	diffs, err := v.UncommittedDiff("main", repoID)
	require.NoError(t, err)
	require.Len(t, diffs, 1)
	assert.Equal(t, "added", diffs[0].Op)
	assert.Empty(t, diffs[0].NewText, "binary content must not surface as diff text")
}
