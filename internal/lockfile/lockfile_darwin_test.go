//go:build darwin

package lockfile

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestCov_WriteAcquire_MkdirAllError covers the MkdirAll-error branch in Write
// and Acquire on macOS: HOME is pointed at a temp dir so UserCacheDir nests
// under <HOME>/Library/Caches, and a file is placed at <HOME>/Library to block
// the MkdirAll chain.
func TestCov_WriteAcquire_MkdirAllError(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	require.NoError(t, os.WriteFile(filepath.Join(tmp, "Library"), []byte("x"), 0o644))
	require.Error(t, Write("/proj", Lockfile{PID: 1}))
	_, err := Acquire("/proj", Lockfile{PID: 1})
	require.Error(t, err)
}
