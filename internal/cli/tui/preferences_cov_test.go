package tui

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCov_LoadPreferences_ReadError covers the non-NotExist read-error path
// (reading a directory errors and is not IsNotExist).
func TestCov_LoadPreferences_ReadError(t *testing.T) {
	_, err := loadPreferences(t.TempDir()) // a directory, not a file
	assert.Error(t, err)
}

// TestCov_PersistPreferences_MkdirAllError covers the MkdirAll error branch
// (a file blocks the directory that must be created).
func TestCov_PersistPreferences_MkdirAllError(t *testing.T) {
	tmp := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(tmp, "sub"), []byte("x"), 0o600))
	err := persistPreferences(filepath.Join(tmp, "sub", "prefs.json"), Preferences{})
	assert.Error(t, err)
}

// TestCov_MergeTUIPrefs_KeymapReset covers the KeymapReset sparse merge: a
// higher-priority layer's tombstone sets the effective reset flag.
func TestCov_MergeTUIPrefs_KeymapReset(t *testing.T) {
	reset := true
	out := mergeTUIPrefs(Preferences{KeymapReset: &reset}, Preferences{}, Preferences{}, Preferences{})
	assert.True(t, out.KeymapReset, "flag layer's KeymapReset merged")
}

// TestCov_PreferencesPath_NoConfigDir covers the UserConfigDir-error fallback
// to "." (unlike permModeFile/frecencyPath which return "").
func TestCov_PreferencesPath_NoConfigDir(t *testing.T) {
	// Force os.UserConfigDir to fail on every platform: clear the Windows
	// AppData vars plus posix XDG_CONFIG_HOME and HOME (macOS/Linux need
	// $HOME). preferencesPath then falls back to ".".
	t.Setenv("APPDATA", "")
	t.Setenv("LOCALAPPDATA", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", "")
	assert.Equal(t, filepath.Join(".", "yanshi", "prefs.json"), preferencesPath())
}

// TestCov_PreferencesFromEnv_InvalidBool covers the YANSHI_HIGH_CONTRAST parse-
// failure path (an invalid boolean fails loudly rather than silently).
func TestCov_PreferencesFromEnv_InvalidBool(t *testing.T) {
	_, err := PreferencesFromEnv(func(k string) string {
		if k == "YANSHI_HIGH_CONTRAST" {
			return "maybe"
		}
		return ""
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "YANSHI_HIGH_CONTRAST")
}
