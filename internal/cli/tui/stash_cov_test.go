package tui

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---- LoadStash ----

// TestCov_LoadStash_OpenError covers the non-NotExist open-error path. A NUL
// byte in the path is rejected by the OS with an invalid-argument error (not
// IsNotExist).
func TestCov_LoadStash_OpenError(t *testing.T) {
	_, err := LoadStash("bad\x00name.json")
	assert.Error(t, err)
}

// TestCov_LoadStash_EmptyLineSkipped covers the self-heal skip of blank lines
// between valid JSONL entries.
func TestCov_LoadStash_EmptyLineSkipped(t *testing.T) {
	p := filepath.Join(t.TempDir(), "stash.jsonl")
	content := `{"text":"a","ts":"2020-01-01T00:00:00Z"}` + "\n\n" +
		`{"text":"b","ts":"2020-01-01T00:00:00Z"}` + "\n"
	require.NoError(t, os.WriteFile(p, []byte(content), 0o644))
	s, err := LoadStash(p)
	require.NoError(t, err)
	assert.Len(t, s.List(), 2, "blank line skipped, two valid entries loaded")
}

// ---- Save ----

// TestCov_Save_MkdirAllError covers the MkdirAll error branch: a file sitting
// where a directory must be created.
func TestCov_StashSave_MkdirAllError(t *testing.T) {
	tmp := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(tmp, "sub"), []byte("x"), 0o644))
	s := &Stash{path: filepath.Join(tmp, "sub", "deep", "stash.jsonl")}
	assert.Error(t, s.Save())
}

// ---- stashPath ----

// TestCov_StashPath_NoConfigDir covers the UserConfigDir-error path.
func TestCov_StashPath_NoConfigDir(t *testing.T) {
	t.Setenv("APPDATA", "")
	t.Setenv("LOCALAPPDATA", "")
	if got := stashPath(); got != "" {
		t.Skipf("UserConfigDir did not error on this platform (got %q)", got)
	}
}

// ---- stashListEntry.render ----

// TestCov_StashRender_NarrowWidth covers the limit<8 clamp branch.
func TestCov_StashRender_NarrowWidth(t *testing.T) {
	out := (&stashListEntry{items: []string{"a long draft line"}}).render(10, newSpinner())
	assert.Contains(t, out, "Stash")
}

// ---- cmdStash ----

// TestCov_CmdStash_DropSuccess covers the drop-success path (enqueueSave +
// "dropped stash[N]" toast).
func TestCov_CmdStash_DropSuccess(t *testing.T) {
	m := newModel(&fakeSession{}, "/proj")
	m.stash = &Stash{} // fresh, avoid the real config file
	m.stash.Push("draft one")
	m.stash.Push("draft two")

	mm, cmd := cmdStash(m, []string{"drop", "0"})
	assert.NotNil(t, cmd, "drop returns a toast cmd")
	assert.Len(t, mm.(model).stash.List(), 1, "one draft dropped")
}
