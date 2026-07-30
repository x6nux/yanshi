//go:build windows

package tui

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestCov_ReplacePrefs_NULSrc covers the src UTF-16 NUL-encoding error branch.
func TestCov_ReplacePrefs_NULSrc(t *testing.T) {
	assert.Error(t, replacePreferencesFileOS("a\x00b", "dst"))
}

// TestCov_ReplacePrefs_NULDst covers the dst UTF-16 NUL-encoding error branch
// (src encodes fine, dst does not).
func TestCov_ReplacePrefs_NULDst(t *testing.T) {
	assert.Error(t, replacePreferencesFileOS("validsrc", "c\x00d"))
}

// TestCov_ReplacePrefs_SrcMissing covers the MoveFileEx ERROR_FILE_NOT_FOUND →
// os.ErrNotExist translation (the source file does not exist).
func TestCov_ReplacePrefs_SrcMissing(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "dst")
	err := replacePreferencesFileOS(filepath.Join(t.TempDir(), "does-not-exist-src"), dst)
	assert.ErrorIs(t, err, os.ErrNotExist)
}
