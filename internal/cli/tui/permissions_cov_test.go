package tui

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/guard"
)

// TestCov_SendMode_AutoZeroThreshold covers the threshold==0 + Auto branch:
// sendMode resolves the default into a LOCAL var for the wire frame (it does
// not write back to m.autoThreshold — that stays the 0 "unset" sentinel).
func TestCov_SendMode_AutoZeroThreshold(t *testing.T) {
	m := newModel(&fakeSession{}, "/proj")
	m.permMode = guard.ModeAuto
	m.autoThreshold = 0
	mm, _ := m.sendMode()
	got := mm.(model)
	assert.Equal(t, 0, got.autoThreshold, "model field stays 0; the default is resolved only for the wire")
}

// TestCov_CycleMode_YoloSkipToAuto covers the yoloConfirm>0 double-cycle that
// lands on Auto with a zero threshold (the gate-skipping path).
func TestCov_CycleMode_YoloSkipToAuto(t *testing.T) {
	m := newModel(&fakeSession{}, "/proj")
	m.permMode = guard.ModeDefault
	m.yoloConfirm = 1
	m.autoThreshold = 0
	mm, _ := m.cycleMode()
	got := mm.(model)
	assert.Equal(t, 0, got.yoloConfirm, "yolo gate cleared")
	assert.Equal(t, guard.ModeAuto, got.permMode, "Default double-cycles to Auto")
	assert.Equal(t, guard.DefaultAutoThreshold, got.autoThreshold)
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
// loadSavedMode/loadSavedThreshold fallback branches (read error, corrupt
// JSON, invalid mode, threshold out of range). persistPermMode is flipped on
// (it is disabled for the test binary by perm_mode_test.go's init).
func TestCov_PermModePersistence(t *testing.T) {
	orig := persistPermMode
	persistPermMode = true
	t.Cleanup(func() { persistPermMode = orig })

	// path=="" → savePermMode no-ops; loads return defaults.
	t.Setenv("APPDATA", "")
	t.Setenv("LOCALAPPDATA", "")
	savePermMode(guard.ModeAuto, 4) // path=="" branch
	if permModeFile() == "" {
		assert.Equal(t, guard.ModeDefault, loadSavedMode())
		assert.Equal(t, 0, loadSavedThreshold())
	}

	// File absent → ReadFile error → defaults.
	tmp := t.TempDir()
	t.Setenv("APPDATA", tmp)
	assert.Equal(t, guard.ModeDefault, loadSavedMode())
	assert.Equal(t, 0, loadSavedThreshold())

	// Corrupt JSON → Unmarshal error → defaults.
	dir := filepath.Join(tmp, "yanshi")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	f := filepath.Join(dir, "perm_mode.json")
	require.NoError(t, os.WriteFile(f, []byte("not json"), 0o644))
	assert.Equal(t, guard.ModeDefault, loadSavedMode())
	assert.Equal(t, 0, loadSavedThreshold())

	// Valid JSON, unrecognized mode → NormalizeMode !ok → default.
	require.NoError(t, os.WriteFile(f, []byte(`{"mode":"bogus","threshold":3}`), 0o644))
	assert.Equal(t, guard.ModeDefault, loadSavedMode())

	// Threshold out of range (< 1 and > 10) → 0.
	require.NoError(t, os.WriteFile(f, []byte(`{"mode":"auto","threshold":0}`), 0o644))
	assert.Equal(t, 0, loadSavedThreshold())
	require.NoError(t, os.WriteFile(f, []byte(`{"mode":"auto","threshold":11}`), 0o644))
	assert.Equal(t, 0, loadSavedThreshold())
}
