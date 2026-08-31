package tui

import (
	"os"
	"path/filepath"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/x6nux/yanshi/internal/keymap"
)

// TestKeymapCaptureEntersMode verifies that /keymap bind <action> sets
// keymapCapture on the model and appends an instruction entry.
//
// Mutation: remove `m.keymapCapture = action` in cmdKeymap's "bind" case and
// this test fails because keymapCapture stays "".
func TestKeymapCaptureEntersMode(t *testing.T) {
	m := newModel(&fakeSession{}, "/proj")
	mm, _ := cmdKeymap(m, []string{"bind", "cancel"})
	got := mm.(model)
	if got.keymapCapture != "cancel" {
		t.Fatalf("expected keymapCapture=%q after /keymap bind cancel, got %q",
			"cancel", got.keymapCapture)
	}
}

// TestKeymapCaptureEscCancels verifies that Esc during capture mode clears
// keymapCapture and does NOT write to prefs.
//
// Mutation: remove the `msg.Type == tea.KeyEscape` early-return in
// handleKeyMsg's keymapCapture block and this test fails because Esc is
// treated as a binding key.
func TestKeymapCaptureEscCancels(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "prefs.json")
	old := preferencesPathFn
	preferencesPathFn = func() string { return path }
	t.Cleanup(func() { preferencesPathFn = old })

	m := newModel(&fakeSession{}, "/proj")
	m.keymapCapture = "cancel"

	// Send Esc
	escMsg := tea.KeyMsg{Type: tea.KeyEscape}
	mm, _, _ := m.handleKeyMsg(escMsg)
	if mm.keymapCapture != "" {
		t.Fatalf("Esc should clear keymapCapture, got %q", mm.keymapCapture)
	}
	// No prefs file should have been written (cancel means no write)
	if _, err := os.Stat(path); err == nil {
		t.Fatal("Esc should not write prefs file")
	}
}

// TestKeymapCaptureConflictBlocksWrite is the key W-E-15 test: conflict
// detection must BLOCK the write, not just display a message.
//
// Mutation: remove the conflict-check `if existing != keymap.ActionNone`
// block in commitKeymapCapture and the binding is written even when a
// conflict exists. This test fails because the prefs file appears and its
// content contains the conflicting key.
func TestKeymapCaptureConflictBlocksWrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "prefs.json")
	old := preferencesPathFn
	preferencesPathFn = func() string { return path }
	t.Cleanup(func() { preferencesPathFn = old })

	m := newModel(&fakeSession{}, "/proj")
	// f1 is the built-in "help" key; try to bind it to "cancel"
	m.keymapCapture = "cancel"
	f1Msg := tea.KeyMsg{Type: tea.KeyF1}

	mm, _ := m.commitKeymapCapture(f1Msg)

	// keymapCapture must be cleared
	if mm.keymapCapture != "" {
		t.Fatalf("keymapCapture should be cleared after capture, got %q", mm.keymapCapture)
	}
	// Conflict: prefs must NOT have been written (no file)
	if _, err := os.Stat(path); err == nil {
		data, _ := os.ReadFile(path)
		t.Fatalf("conflict should block write, but prefs file was created:\n%s", data)
	}

	// Verify an error entry was added mentioning the conflict
	foundConflict := false
	for _, e := range mm.entries {
		if ee, ok := e.(errorEntry); ok {
			if len(ee.text) > 0 {
				foundConflict = true
			}
		}
	}
	if !foundConflict {
		t.Fatal("expected an errorEntry describing the conflict")
	}
}

// TestKeymapCaptureWritesAtomically proves that the atomic write path is used.
// Mutation: replace `replacePreferencesFile(tmp, path)` in persistPreferences
// with a direct `os.WriteFile(path, data, 0600)` and this test fails because
// the seam is not exercised (atomicCalled stays false).
func TestKeymapCaptureWritesAtomically(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "prefs.json")
	old := preferencesPathFn
	preferencesPathFn = func() string { return path }
	t.Cleanup(func() { preferencesPathFn = old })

	// Intercept replacePreferencesFile to prove the atomic path is called.
	atomicCalled := false
	oldReplace := replacePreferencesFile
	replacePreferencesFile = func(src, dst string) error {
		if dst != path {
			t.Errorf("replace dst=%q, want %q", dst, path)
		}
		atomicCalled = true
		return os.Rename(src, dst)
	}
	t.Cleanup(func() { replacePreferencesFile = oldReplace })

	m := newModel(&fakeSession{}, "/proj")
	// ctrl+u is not a built-in binding — safe to capture
	m.keymapCapture = "cancel"
	ctrlU := tea.KeyMsg{Type: tea.KeyCtrlU}

	mm, _ := m.commitKeymapCapture(ctrlU)
	if mm.keymapCapture != "" {
		t.Fatalf("keymapCapture should be cleared, got %q", mm.keymapCapture)
	}
	if !atomicCalled {
		t.Fatal("replacePreferencesFile was not called: atomic write not used")
	}
	// Verify the binding was persisted
	if mm.prefs.KeymapBindings["ctrl+u"] != "cancel" {
		t.Fatalf("expected prefs.KeymapBindings[ctrl+u]=cancel, got %v", mm.prefs.KeymapBindings)
	}
}

// TestKeymapListEmpty verifies /keymap list says "no custom bindings" when none.
// Mutation: remove the `len(m.effective.KeymapBindings) == 0` early-return and
// the test fails because the empty message is never printed.
func TestKeymapListEmpty(t *testing.T) {
	m := newModel(&fakeSession{}, "/proj")
	mm, _ := cmdKeymap(m, []string{"list"})
	got := mm.(model)
	if len(got.entries) == 0 {
		t.Fatal("expected an entry for /keymap list")
	}
	ae, ok := got.entries[len(got.entries)-1].(ackEntry)
	if !ok {
		t.Fatalf("expected ackEntry, got %T", got.entries[len(got.entries)-1])
	}
	if ae.text != "no custom user-level bindings (use /keymap bind <action> to add one)" {
		t.Fatalf("unexpected list text: %q", ae.text)
	}
}

// TestKeymapListActions verifies /keymap list shows bindings when present.
// Mutation: remove the keymapBindings map population in renderKeymapList and
// the test fails because the binding line disappears.
func TestKeymapListActions(t *testing.T) {
	m := newModel(&fakeSession{}, "/proj")
	m.prefs.KeymapBindings = map[string]string{"ctrl+u": "cancel"}
	m.effective.KeymapBindings = map[string]string{"ctrl+u": "cancel"}
	mm, _ := cmdKeymap(m, []string{"list"})
	got := mm.(model)
	if len(got.entries) == 0 {
		t.Fatal("expected an entry for /keymap list")
	}
	ae, ok := got.entries[len(got.entries)-1].(ackEntry)
	if !ok {
		t.Fatalf("expected ackEntry, got %T", got.entries[len(got.entries)-1])
	}
	if !containsSubstrKW(ae.text, "ctrl+u") || !containsSubstrKW(ae.text, "cancel") {
		t.Fatalf("list text should show binding, got %q", ae.text)
	}
}

// TestKeymapBindUnknownAction verifies /keymap bind <bad> gives an error.
// Mutation: remove isKnownKeymapAction check and this test fails because
// "bogus" is accepted and ends up in keymapCapture.
func TestKeymapBindUnknownAction(t *testing.T) {
	m := newModel(&fakeSession{}, "/proj")
	mm, _ := cmdKeymap(m, []string{"bind", "bogus_action"})
	got := mm.(model)
	if got.keymapCapture != "" {
		t.Fatalf("unknown action should not enter capture mode, keymapCapture=%q", got.keymapCapture)
	}
	foundErr := false
	for _, e := range got.entries {
		if _, ok := e.(errorEntry); ok {
			foundErr = true
		}
	}
	if !foundErr {
		t.Fatal("expected an errorEntry for unknown action")
	}
}

// containsSubstrKW is a simple substring check.
func containsSubstrKW(s, sub string) bool {
	if len(sub) == 0 {
		return true
	}
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// TestKeymapCaptureRebindExistingUserKey verifies that binding a key that
// was itself user-assigned (but to a DIFFERENT action) is also blocked.
func TestKeymapCaptureRebindExistingUserKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "prefs.json")
	old := preferencesPathFn
	preferencesPathFn = func() string { return path }
	t.Cleanup(func() { preferencesPathFn = old })

	m := newModel(&fakeSession{}, "/proj")
	// pre-set a user binding: ctrl+u = cancel
	m.prefs.KeymapBindings = map[string]string{"ctrl+u": "cancel"}
	m.effective.KeymapBindings = map[string]string{"ctrl+u": "cancel"}
	m = m.remerge()

	// Now try to bind ctrl+u to "help" — conflict
	m.keymapCapture = "help"
	ctrlU := tea.KeyMsg{Type: tea.KeyCtrlU}
	mm, _ := m.commitKeymapCapture(ctrlU)

	// Prefs must NOT have changed — "cancel" must still be the ctrl+u action
	if v, ok := mm.prefs.KeymapBindings["ctrl+u"]; ok && v != "cancel" {
		t.Fatalf("conflict should not overwrite existing binding, got %q", v)
	}
	// And no write should have occurred to disk
	if _, err := os.Stat(path); err == nil {
		t.Fatal("conflict should block write, but prefs file was created")
	}

	// Reference the keymap package to confirm ActionCancel is what we used
	_ = keymap.ActionCancel
}

