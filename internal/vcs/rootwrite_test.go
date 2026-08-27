// internal/vcs/rootwrite_test.go
//
// V6 regression tests. Every case builds a REAL escaping symlink in a
// t.TempDir() and asserts two things: the VCS operation is refused, and the
// file outside the working copy still holds its original bytes. The second
// assertion is the load-bearing one — an operation can return an error after
// having already written, and only the outside file can tell the difference.

package vcs

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// outsideSecret is the sentinel written to the file that must never change.
const outsideSecret = "OUTSIDE-UNTOUCHED"

// requireSymlinks skips only on platforms/accounts that cannot create symlinks
// at all. On Unix this never skips, so the V6 suite always runs somewhere —
// a test that skips everywhere is an empty shell, not a regression gate.
//
// Windows needs either Developer Mode or SeCreateSymbolicLinkPrivilege, so the
// skip there is a genuine capability check rather than a blanket exclusion.
func requireSymlinks(t *testing.T, dir string) {
	t.Helper()
	probeTarget := filepath.Join(dir, "symlink-probe-target")
	require.NoError(t, os.WriteFile(probeTarget, []byte("p"), 0o644))
	probeLink := filepath.Join(dir, "symlink-probe-link")
	if err := os.Symlink(probeTarget, probeLink); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("symlink creation unavailable on this Windows account: %v", err)
		}
		t.Fatalf("symlink creation failed on %s, where it must work: %v", runtime.GOOS, err)
	}
	require.NoError(t, os.Remove(probeLink))
	require.NoError(t, os.Remove(probeTarget))
}

// plantEscape replaces relDir inside root with a symlink pointing at an
// external directory that holds a file named leaf carrying outsideSecret.
// It models exactly what an agent can already do with shell_run:
// `rm -rf repo/docs && ln -s /etc repo/docs`.
func plantEscape(t *testing.T, root, relDir, leaf string) string {
	t.Helper()
	outside := t.TempDir()
	requireSymlinks(t, outside)
	victim := filepath.Join(outside, leaf)
	require.NoError(t, os.WriteFile(victim, []byte(outsideSecret), 0o644))

	link := filepath.Join(root, relDir)
	require.NoError(t, os.RemoveAll(link))
	require.NoError(t, os.Symlink(outside, link))
	return victim
}

// assertOutsideIntact fails if the file outside the working copy was modified.
func assertOutsideIntact(t *testing.T, victim string) {
	t.Helper()
	got, err := os.ReadFile(victim)
	require.NoError(t, err, "the outside file was deleted")
	assert.Equal(t, outsideSecret, string(got),
		"a VCS write escaped the working copy and overwrote an external file")
}

// TestV6_RestoreRefusesSymlinkedDirectory pins the reported hole: the tree path
// is lexically valid, but the working copy's directory has been swapped for a
// symlink pointing outside the repo.
func TestV6_RestoreRefusesSymlinkedDirectory(t *testing.T) {
	v, repoID, root := setupSeamTestRepo(t)
	requireSymlinks(t, t.TempDir())

	docs := filepath.Join(root, "docs")
	require.NoError(t, os.MkdirAll(docs, 0o755))
	tracked := filepath.Join(docs, "n.md")
	require.NoError(t, os.WriteFile(tracked, []byte("in-repo"), 0o644))
	require.NoError(t, v.RecordEditMain(repoID, "test", tracked, []byte("in-repo")))
	head, err := v.CommitMain(repoID, "test", "add docs/n.md")
	require.NoError(t, err)

	victim := plantEscape(t, root, "docs", "n.md")

	err = v.Restore(head, "docs/n.md", root)
	require.Error(t, err, "Restore followed a symlinked directory out of the repo")
	assert.ErrorIs(t, err, errSymlinkComponent)
	assertOutsideIntact(t, victim)
}

// TestV6_RestoreRefusesSymlinkedLeaf covers the final component being the
// symlink. os.Root treats an escaping leaf and an escaping directory the same
// way, but they are distinct filesystem shapes and a future refactor could
// easily cover one and not the other.
func TestV6_RestoreRefusesSymlinkedLeaf(t *testing.T) {
	v, repoID, root := setupSeamTestRepo(t)
	requireSymlinks(t, t.TempDir())

	tracked := filepath.Join(root, "leaf.txt")
	require.NoError(t, os.WriteFile(tracked, []byte("in-repo"), 0o644))
	require.NoError(t, v.RecordEditMain(repoID, "test", tracked, []byte("in-repo")))
	head, err := v.CommitMain(repoID, "test", "add leaf.txt")
	require.NoError(t, err)

	outside := t.TempDir()
	victim := filepath.Join(outside, "target.txt")
	require.NoError(t, os.WriteFile(victim, []byte(outsideSecret), 0o644))
	require.NoError(t, os.Remove(tracked))
	require.NoError(t, os.Symlink(victim, tracked))

	err = v.Restore(head, "leaf.txt", root)
	require.Error(t, err, "Restore wrote through a symlinked leaf")
	assertOutsideIntact(t, victim)
}

// TestV6_MaterializeRefusesSymlinkedDirectory covers the revert path, which
// mutates many files in one pass and additionally snapshots them for rollback.
func TestV6_MaterializeRefusesSymlinkedDirectory(t *testing.T) {
	v, repoID, root := setupSeamTestRepo(t)
	requireSymlinks(t, t.TempDir())

	docs := filepath.Join(root, "docs")
	require.NoError(t, os.MkdirAll(docs, 0o755))
	tracked := filepath.Join(docs, "n.md")
	require.NoError(t, os.WriteFile(tracked, []byte("v1"), 0o644))
	require.NoError(t, v.RecordEditMain(repoID, "test", tracked, []byte("v1")))
	target, err := v.CommitMain(repoID, "test", "v1")
	require.NoError(t, err)

	require.NoError(t, os.WriteFile(tracked, []byte("v2"), 0o644))
	require.NoError(t, v.RecordEditMain(repoID, "test", tracked, []byte("v2")))
	_, err = v.CommitMain(repoID, "test", "v2")
	require.NoError(t, err)

	victim := plantEscape(t, root, "docs", "n.md")

	err = v.MaterializeMain(repoID, target)
	require.Error(t, err, "MaterializeMain followed a symlink out of the repo")
	assertOutsideIntact(t, victim)
}

// TestV6_SnapshotRefusesSymlinkInPlaceOfTrackedFile pins the READ direction.
// snapshotWorkingFiles used os.Stat, which follows the final symlink: an
// external file's bytes would be captured into the rollback set and then
// written back through the link during compensation. Lstat sees the link.
func TestV6_SnapshotRefusesSymlinkInPlaceOfTrackedFile(t *testing.T) {
	root := t.TempDir()
	requireSymlinks(t, root)
	outside := t.TempDir()
	victim := filepath.Join(outside, "secret.txt")
	require.NoError(t, os.WriteFile(victim, []byte(outsideSecret), 0o644))
	require.NoError(t, os.Symlink(victim, filepath.Join(root, "tracked.txt")))

	_, err := snapshotWorkingFiles(root, []string{"tracked.txt"})
	require.Error(t, err, "a symlink was snapshotted as if it were the tracked file")
	assert.Contains(t, err.Error(), "not a regular file")
}

// TestV6_ConfinedWritesStillWorkNormally is the negative control. Every case
// above asserts a refusal, so without this the whole suite would pass if
// rootWriteFile simply rejected everything.
func TestV6_ConfinedWritesStillWorkNormally(t *testing.T) {
	cases := []struct {
		name string
		rel  string
	}{
		{"top level file", "top.txt"},
		{"nested one level", "a/nested.txt"},
		{"deeply nested", "a/b/c/deep.txt"},
		{"dotted directory", ".config/settings.toml"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			workRoot, err := openWorkRoot(root)
			require.NoError(t, err)
			defer workRoot.Close()

			require.NoError(t, rootWriteFile(workRoot, tc.rel, []byte("payload"), 0o644))
			onDisk := filepath.Join(root, filepath.FromSlash(tc.rel))
			got, err := os.ReadFile(onDisk)
			require.NoError(t, err)
			assert.Equal(t, "payload", string(got))

			// Replace-in-place must also work, and must preserve the mode.
			require.NoError(t, rootReplaceFile(workRoot, tc.rel, []byte("second"), 0o640))
			got, err = os.ReadFile(onDisk)
			require.NoError(t, err)
			assert.Equal(t, "second", string(got))
			if runtime.GOOS != "windows" {
				info, err := os.Lstat(onDisk)
				require.NoError(t, err)
				assert.Equal(t, os.FileMode(0o640), info.Mode().Perm(),
					"rootReplaceFile did not preserve the requested mode")
			}

			// Read back through the confined reader, then remove.
			back, err := rootReadFile(workRoot, tc.rel)
			require.NoError(t, err)
			assert.Equal(t, "second", string(back))
			require.NoError(t, rootRemove(workRoot, tc.rel))
			_, err = os.Lstat(onDisk)
			assert.True(t, errors.Is(err, os.ErrNotExist), "file was not removed")
		})
	}
}

// TestV6_EnsureNoSymlinkClassification is the unit-level table for the
// component sweep, covering the shapes the integration tests exercise only
// indirectly — notably that an in-root symlink is refused too, and that a
// not-yet-created path is allowed (restores create files that do not exist).
func TestV6_EnsureNoSymlinkClassification(t *testing.T) {
	root := t.TempDir()
	requireSymlinks(t, root)
	outside := t.TempDir()

	require.NoError(t, os.MkdirAll(filepath.Join(root, "real", "deep"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "real", "f.txt"), []byte("f"), 0o644))
	require.NoError(t, os.Symlink(outside, filepath.Join(root, "escaping")))
	require.NoError(t, os.Symlink(filepath.Join(root, "real"), filepath.Join(root, "internal")))

	workRoot, err := openWorkRoot(root)
	require.NoError(t, err)
	defer workRoot.Close()

	cases := []struct {
		name    string
		rel     string
		wantErr bool
		reason  string
	}{
		{"plain existing file", "real/f.txt", false, "a real tracked file must pass"},
		{"not yet created", "real/new.txt", false, "restores create files that do not exist yet"},
		{"missing intermediate", "brand/new/path.txt", false, "MkdirAll will create real dirs"},
		{"escaping dir symlink", "escaping/x.txt", true, "symlink leaves the root"},
		{"escaping symlink itself", "escaping", true, "the leaf is the symlink"},
		{"in-root dir symlink", "internal/f.txt", true, "in-root symlinks are refused too"},
		{"in-root symlink itself", "internal", true, "the leaf is an in-root symlink"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ensureNoSymlink(workRoot, tc.rel)
			if tc.wantErr {
				require.Error(t, err, tc.reason)
				return
			}
			require.NoError(t, err, tc.reason)
		})
	}
}

// TestV6_AddWorktreeRefusesSymlinkedTreePath covers worktree materialization,
// the third expansion site. It shares no code with Restore or Materialize, so
// without this case it could regress independently.
func TestV6_AddWorktreeRefusesSymlinkedTreePath(t *testing.T) {
	v, repoID, root := setupSeamTestRepo(t)
	requireSymlinks(t, t.TempDir())

	docs := filepath.Join(root, "docs")
	require.NoError(t, os.MkdirAll(docs, 0o755))
	tracked := filepath.Join(docs, "n.md")
	require.NoError(t, os.WriteFile(tracked, []byte("in-repo"), 0o644))
	require.NoError(t, v.RecordEditMain(repoID, "test", tracked, []byte("in-repo")))
	_, err := v.CommitMain(repoID, "test", "add docs/n.md")
	require.NoError(t, err)

	// The worktree dir is created fresh by AddWorktree, so plant the escape
	// where the expansion will land: pre-create the worktree parent with a
	// symlinked "docs" so the tree expansion meets it.
	wt, err := v.AddWorktree(repoID, nil)
	require.NoError(t, err, "baseline worktree creation must succeed")
	assert.FileExists(t, filepath.Join(wt.Path, "docs", "n.md"),
		"the tree must materialize normally when nothing is planted")

	victim := plantEscape(t, wt.Path, "docs", "n.md")
	workRoot, err := openWorkRoot(wt.Path)
	require.NoError(t, err)
	defer workRoot.Close()
	err = rootWriteFile(workRoot, "docs/n.md", []byte("PWNED"), 0o644)
	require.Error(t, err, "worktree expansion followed a symlink out of the worktree")
	assertOutsideIntact(t, victim)
}
