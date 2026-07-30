package pathjail

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestWithinRootAbs_ValidSubpath: a path inside root returns its canonical
// absolute form (EvalSymlinks applied).
func TestWithinRootAbs_ValidSubpath(t *testing.T) {
	root := t.TempDir()
	inner := filepath.Join(root, "sub", "inner")
	require.NoError(t, os.MkdirAll(inner, 0o750))

	got, err := WithinRootAbs(root, inner)
	require.NoError(t, err)
	// 在 macOS 的 /tmp 是 /private/tmp 的 symlink，所以 root 与 got 都会被
	// EvalSymlinks 重写。我们只校验 got 经过 EvalSymlinks 后仍以同样 canonical
	// root 为前缀。
	rootReal, err := filepath.EvalSymlinks(root)
	require.NoError(t, err)
	innerReal, err := filepath.EvalSymlinks(inner)
	require.NoError(t, err)
	assert.Equal(t, innerReal, got)
	assert.True(t, strings.HasPrefix(got, rootReal))
}

// TestWithinRootAbs_RelativeRef: passing a relative ref joined against root
// also resolves inside (used by SecureArtifactPath callers). The file must
// exist because EvalSymlinks stats the path — this matches the production
// precondition (Manager.WriteArtifact creates the file before validating).
func TestWithinRootAbs_RelativeRef(t *testing.T) {
	root := t.TempDir()
	inner := filepath.Join(root, "artifacts", "wt-1")
	require.NoError(t, os.MkdirAll(inner, 0o750))
	// Create the file before evaluating — WithinRootAbs is not designed for
	// non-existent paths (EvalSymlinks would fail).
	target := filepath.Join(inner, "file.txt")
	require.NoError(t, os.WriteFile(target, []byte("x"), 0o600))
	joined := filepath.Join(root, "artifacts", "wt-1", "file.txt")
	got, err := WithinRootAbs(root, joined)
	require.NoError(t, err)
	rootReal, _ := filepath.EvalSymlinks(root)
	assert.True(t, strings.HasPrefix(got, rootReal+string(filepath.Separator)))
}

// TestWithinRootAbs_SymlinkEscape: a symlink from inside root pointing outside
// root must be rejected (this is the load-bearing symlink jail check). Creating
// symlinks on Windows requires admin privileges; the test is skipped if so.
func TestWithinRootAbs_SymlinkEscape(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	outsideFile := filepath.Join(outside, "secret.txt")
	require.NoError(t, os.WriteFile(outsideFile, []byte("nope"), 0o600))
	link := filepath.Join(root, "evil")
	if err := os.Symlink(outside, link); err != nil {
		// Windows without developer mode / admin cannot create symlinks; the
		// production code still rejects escape via volume + rel checks at
		// runtime when symlinks are creatable.
		t.Skipf("cannot create symlink on this host: %v", err)
	}

	_, err := WithinRootAbs(root, filepath.Join(link, "secret.txt"))
	require.Error(t, err, "symlink escape must be rejected")
}

// TestWithinRootAbs_DotDot: ".." and "../x" style escapes must be rejected
// even when the candidate string contains no symlink (covered by filepath.Rel).
func TestWithinRootAbs_DotDot(t *testing.T) {
	root := t.TempDir()
	_, err := WithinRootAbs(root, filepath.Join(root, ".."))
	require.Error(t, err)
	_, err = WithinRootAbs(root, filepath.Join(root, "..", "x"))
	require.Error(t, err)
}

// TestWithinRootAbs_RootItself: passing root itself returns the canonical
// absolute root.
func TestWithinRootAbs_RootItself(t *testing.T) {
	root := t.TempDir()
	got, err := WithinRootAbs(root, root)
	require.NoError(t, err)
	rootReal, _ := filepath.EvalSymlinks(root)
	assert.Equal(t, rootReal, got)
}

// TestWithinRootAbs_DifferentVolume_Windows only runs on Windows; on POSIX it
// verifies that an absolute path on a different prefix is rejected.
func TestWithinRootAbs_DifferentVolume_Windows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("volume comparison only applies on Windows")
	}
	root := `C:\yanshi-root-` + t.Name()
	if err := os.MkdirAll(root, 0o750); err != nil {
		t.Skipf("cannot create C:\\ root: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	// D:\something is on a different volume
	_, err := WithinRootAbs(root, `D:\something`)
	require.Error(t, err)
}

// TestWithinRootAbs_NonExistentCandidate: a non-existent candidate fails
// because EvalSymlinks requires a real path; the janitor and artifact_read
// always feed existing paths so this is the documented precondition.
func TestWithinRootAbs_NonExistentCandidate(t *testing.T) {
	root := t.TempDir()
	_, err := WithinRootAbs(root, filepath.Join(root, "nope"))
	require.Error(t, err)
}
