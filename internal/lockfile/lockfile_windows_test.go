//go:build windows

package lockfile

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestCov_WriteAcquire_MkdirAllError covers the MkdirAll-error branch in Write
// and Acquire on Windows: LocalAppData is pointed at a temp dir and a file is
// placed where the <base>/yanshi segment must be created.
func TestCov_WriteAcquire_MkdirAllError(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("LOCALAPPDATA", tmp)
	require.NoError(t, os.WriteFile(filepath.Join(tmp, "yanshi"), []byte("x"), 0o644))
	require.Error(t, Write("/proj", Lockfile{PID: 1}))
	_, err := Acquire("/proj", Lockfile{PID: 1})
	require.Error(t, err)
}
