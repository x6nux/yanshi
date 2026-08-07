package tui

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/guard"
)

// TestCov_CycleMode_YoloSkipToAuto covers the yoloConfirm>0 double-cycle that
// lands on Auto (the gate-skipping path).
func TestCov_CycleMode_YoloSkipToAuto(t *testing.T) {
	m := newModel(&fakeSession{}, "/proj")
	m.permMode = guard.ModeDefault
	m.yoloConfirm = 1
	mm, _ := m.cycleMode()
	got := mm.(model)
	assert.Equal(t, 0, got.yoloConfirm, "yolo gate cleared")
	assert.Equal(t, guard.ModeAuto, got.permMode, "Default double-cycles to Auto")
}

// TestCov_RespondPermission_NoPending covers the no-pending-prompt no-op.
func TestCov_RespondPermission_NoPending(t *testing.T) {
	m := newModel(&fakeSession{}, "/proj")
	// No pending permissions → must not panic or mutate state.
	m.respondPermission("allow")
	assert.Empty(t, m.pendingPermissions)
}

// TestCov_PermModeFile_NoConfigDir covers the UserConfigDir-error path.
func TestCov_PermModeFile_NoConfigDir(t *testing.T) {
	t.Setenv("APPDATA", "")
	t.Setenv("LOCALAPPDATA", "")
	if got := permModeFile(); got != "" {
		t.Skipf("UserConfigDir did not error on this platform (got %q)", got)
	}
}

// TestCov_PermModePersistence covers the savePermMode path=="" no-op and the
// loadSavedMode fallback branches (read error, corrupt JSON, invalid mode).
// persistPermMode is flipped on
// (it is disabled for the test binary by perm_mode_test.go's init).
func TestCov_PermModePersistence(t *testing.T) {
	orig := persistPermMode
	persistPermMode = true
	t.Cleanup(func() { persistPermMode = orig })

	// path=="" → savePermMode no-ops; loads return defaults. Clear every
	// platform's UserConfigDir input (APPDATA on Windows, XDG/HOME on posix).
	t.Setenv("APPDATA", "")
	t.Setenv("LOCALAPPDATA", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", "")
	savePermMode(guard.ModeAuto) // path=="" branch
	if permModeFile() == "" {
		assert.Equal(t, guard.ModeDefault, loadSavedMode())
	}

	// File absent → ReadFile error → defaults.
	tmp := t.TempDir()
	t.Setenv("APPDATA", tmp)
	t.Setenv("XDG_CONFIG_HOME", tmp)
	t.Setenv("HOME", tmp)
	assert.Equal(t, guard.ModeDefault, loadSavedMode())

	// Corrupt JSON → Unmarshal error → defaults. Write the exact path
	// permModeFile() reads (it varies by OS, so do not hardcode tmp/yanshi).
	f := permModeFile()
	require.NotEmpty(t, f)
	require.NoError(t, os.MkdirAll(filepath.Dir(f), 0o755))
	require.NoError(t, os.WriteFile(f, []byte("not json"), 0o644))
	assert.Equal(t, guard.ModeDefault, loadSavedMode())

	// Valid JSON, unrecognized mode → NormalizeMode !ok → default.
	require.NoError(t, os.WriteFile(f, []byte(`{"mode":"bogus"}`), 0o644))
	assert.Equal(t, guard.ModeDefault, loadSavedMode())

	// A file left over from the threshold era still loads: the extra key is
	// ignored rather than failing the whole parse, so an upgrade does not
	// silently reset the user's mode.
	require.NoError(t, os.WriteFile(f, []byte(`{"mode":"auto","threshold":7}`), 0o644))
	assert.Equal(t, guard.ModeAuto, loadSavedMode())
}
