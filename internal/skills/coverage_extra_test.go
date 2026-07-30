package skills

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestLoad_SkipsNonDirEntry covers skills.go:179-180: a regular file directly
// in a skill root must be skipped (only directories can be skill dirs). This is
// reachable on every OS, unlike the ReadDir-error paths which are POSIX-only.
func TestLoad_SkipsNonDirEntry(t *testing.T) {
	root := t.TempDir()
	// A stray regular file in the root alongside a valid skill dir.
	require.NoError(t, os.WriteFile(filepath.Join(root, "stray.txt"), []byte("x"), 0o644))
	writeSkill(t, root, "real", "---\nname: real\ndescription: d\n---\nbody\n")

	reg, err := NewLoader(Builtin(root)).Load()
	require.NoError(t, err)
	s, ok := reg.Get("real")
	require.True(t, ok, "the valid skill must still load despite the stray file sibling")
	assert.Equal(t, "real", s.Name)
}

// TestRejectSymlinks_FindsSymlink covers install.go:241-243 directly: when a
// skill tree contains a symlink, rejectSymlinks reports it by name. Creating a
// symlink requires elevated privilege on Windows, so the test is POSIX-only
// (matching TestInstall_RejectsSymlink's guard). On Windows it skips, leaving
// that branch to the POSIX CI matrix.
func TestRejectSymlinks_FindsSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires elevated permission on Windows")
	}
	dir := t.TempDir()
	target := filepath.Join(t.TempDir(), "outside")
	require.NoError(t, os.WriteFile(target, []byte("secret"), 0o644))
	require.NoError(t, os.Symlink(target, filepath.Join(dir, "link")))

	err := rejectSymlinks(dir)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "symlink rejected")
}

// TestUninstall_AbsAndEscapeGuardsAreUnreachable documents that Uninstall's
// defense-in-depth guards (filepath.Abs errors, isWithin target escape) cannot
// be reached because validName rejects traversal before they run. We assert the
// contract the guards protect: a traversal name is rejected up front, never
// reaching the path math. This keeps the reachable surface honest.
func TestUninstall_AbsAndEscapeGuardsAreUnreachable(t *testing.T) {
	dstRoot := t.TempDir()
	// ".." / absolute paths are rejected by validName before isWithin runs.
	err := Uninstall("..", dstRoot)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid name")
}

// TestInstall_PropagatesRejectSymlinksError covers install.go:144-146 on every
// OS: the branch that returns rejectSymlinks' error. On POSIX the existing
// TestInstall_RejectsSymlink exercises it via a real symlink (which Windows
// can't create without privilege); here a cloner that removes the staging dir
// it was handed makes rejectSymlinks' Walk error (the staging path no longer
// exists), exercising the same error-propagation branch without symlinks.
func TestInstall_PropagatesRejectSymlinksError(t *testing.T) {
	cloner := cloneFunc(func(_ context.Context, _, _, intoDir string) error {
		// Removing the staging dir we were handed makes rejectSymlinks' Walk
		// fail (lstat on a missing root) — the same return-"",err branch.
		return os.RemoveAll(intoDir)
	})
	_, err := Install("github:owner/repo", t.TempDir(), cloner)
	require.Error(t, err)
}

// TestUninstall_RemoveAllError covers install.go:226-228: when os.RemoveAll of
// the target fails (here a child file is held open, which Windows refuses to
// delete), Uninstall wraps and returns the error. This is OS-behavior-driven;
// if a future Go runtime shares delete access and RemoveAll succeeds, the test
// is skipped rather than failing.
func TestUninstall_RemoveAllError(t *testing.T) {
	dstRoot := t.TempDir()
	target := filepath.Join(dstRoot, "locked")
	require.NoError(t, os.MkdirAll(target, 0o755))
	held := filepath.Join(target, "held.txt")
	require.NoError(t, os.WriteFile(held, []byte("x"), 0o644))

	// Hold the file open across the Uninstall call. On Windows the default
	// open does not grant delete-sharing, so RemoveAll cannot unlink it.
	f, err := os.Open(held)
	require.NoError(t, err)
	defer f.Close()

	err = Uninstall("locked", dstRoot)
	if err == nil {
		t.Skip("RemoveAll succeeded (runtime shares delete access); error branch not exercisable here")
	}
	require.Error(t, err)
	assert.Contains(t, err.Error(), "uninstall")
}

// TestInstall_RenameError covers install.go:196-198: when the final atomic
// os.Rename(skillDir, dstAbs) fails, Install wraps and returns the error.
//
// Reliable cross-platform rename failures are blocked by earlier guards (the
// exists-check refuses a pre-existing target; staging sits beside dstRoot so
// there is no cross-volume rename). The remaining reachable failure on Windows
// is an open handle inside the source dir, which MoveFileEx refuses. The cloner
// opens a file synchronously (so it is definitely open before the cloner
// returns) then hands the live handle to a goroutine blocked for the rest of
// the test, keeping it open through Install's rename step. On POSIX an open fd
// does not block rename, so the test skips there.
func TestInstall_RenameError(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("open-handle blocks rename only on Windows")
	}
	hold := make(chan struct{})
	defer close(hold)

	cloner := cloneFunc(func(_ context.Context, _, _, intoDir string) error {
		if err := os.WriteFile(filepath.Join(intoDir, "SKILL.md"),
			[]byte("---\nname: victim\ndescription: d\n---\n"), 0o644); err != nil {
			return err
		}
		fp := filepath.Join(intoDir, "held.txt")
		if err := os.WriteFile(fp, []byte("x"), 0o644); err != nil {
			return err
		}
		f, err := os.Open(fp)
		if err != nil {
			return err
		}
		// Keep the handle open past this call so it is still open at rename time.
		go func() {
			<-hold
			f.Close()
		}()
		return nil
	})

	_, err := Install("github:owner/repo", t.TempDir(), cloner)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "rename")
}
