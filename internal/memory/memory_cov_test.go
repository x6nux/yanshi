package memory

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCov_ComposeBlock_Empty covers the "Load returned empty → return empty"
// branch (enabled but no memory files present).
func TestCov_ComposeBlock_Empty(t *testing.T) {
	assert.Equal(t, "", ComposeBlock(true, "/nope-user", "/nope-project", 100))
}

// TestCov_Append_MkdirAllError covers the MkdirAll-error branch (a file blocks
// the parent directory that must be created).
func TestCov_Append_MkdirAllError(t *testing.T) {
	tmp := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(tmp, "blocked"), []byte("x"), 0o644))
	assert.Error(t, Append(filepath.Join(tmp, "blocked", "mem.md"), "entry"))
}

// TestCov_Append_OpenFileError covers the OpenFile-error branch (the path is a
// directory, which cannot be opened for appending).
func TestCov_Append_OpenFileError(t *testing.T) {
	assert.Error(t, Append(t.TempDir(), "entry"))
}

// TestCov_ReadTrimmed_Whitespace covers the "file exists but is whitespace-only"
// branch (TrimSpace → empty → ("", false)).
func TestCov_ReadTrimmed_Whitespace(t *testing.T) {
	p := filepath.Join(t.TempDir(), "mem.md")
	require.NoError(t, os.WriteFile(p, []byte("   \n\t \n"), 0o644))
	s, ok := readTrimmed(p)
	assert.False(t, ok)
	assert.Equal(t, "", s)
}
