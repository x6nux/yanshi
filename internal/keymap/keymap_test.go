package keymap

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// TestBuilder_ValidateAfterBuild (structural fix #16): the Builder collects
// all bindings via Add() and ONLY validates on Build(). This catches
// conflicts that incremental registration would miss (e.g., A registers
// ctrl+k, then B registers ctrl+k — incremental validation on A's add would
// pass because B hasn't been added yet).
func TestBuilder_ValidateAfterBuild(t *testing.T) {
	b := NewBuilder()
	b.Add("ctrl+k", ActionScrollUp)
	b.Add("ctrl+k", ActionScrollDown) // conflict

	_, err := b.Build()
	if err == nil {
		t.Fatal("Build must report conflict")
	}
	if !strings.Contains(err.Error(), "ctrl+k") {
		t.Fatalf("conflict error must name the key: %v", err)
	}
}

func TestBuilder_RejectsUnknownActionAfterCollection(t *testing.T) {
	b := NewBuilder()
	b.Add("ctrl+x", Action("launch_missiles"))
	if _, err := b.Build(); err == nil || !strings.Contains(err.Error(), "launch_missiles") {
		t.Fatalf("unknown action must be diagnosed, got %v", err)
	}
}

// Runtime spelling is owned by the checked-in Bubble Tea fork. NormalizeKey
// delegates to KeyMsg.String instead of maintaining a second KeyType switch.
func TestNormalizeKey_DelegatesToBubbleTeaString(t *testing.T) {
	cases := []tea.KeyMsg{
		{Type: tea.KeyCtrlK},
		{Type: tea.KeyEnter},
		{Type: tea.KeyCtrlEnter},
		{Type: tea.KeyPgUp},
		{Type: tea.KeyRunes, Runes: []rune("a"), Alt: true},
	}
	for _, msg := range cases {
		got, ok := NormalizeKey(msg)
		if !ok {
			t.Fatalf("NormalizeKey(%+v) unexpectedly rejected", msg)
		}
		if want := strings.ToLower(msg.String()); got != want {
			t.Fatalf("NormalizeKey(%+v) = %q, want fork String %q", msg, got, want)
		}
	}
}

func TestNormalizeKey_PasteAndMultiRuneAreNotShortcuts(t *testing.T) {
	cases := []tea.KeyMsg{
		{Type: tea.KeyRunes, Runes: []rune("j"), Paste: true},
		{Type: tea.KeyRunes, Runes: []rune("jk")},
	}
	for _, msg := range cases {
		if got, ok := NormalizeKey(msg); ok || got != "" {
			t.Fatalf("bulk input normalized as shortcut: %q ok=%v", got, ok)
		}
	}
}

func TestBuild_DefaultLookupUsesRealKeyMessages(t *testing.T) {
	m, err := NewDefaultBuilder(nil).Build()
	if err != nil {
		t.Fatal(err)
	}
	if got := m.Lookup(tea.KeyMsg{Type: tea.KeyPgUp}); got != ActionScrollUp {
		t.Fatalf("pgup -> %v want %v", got, ActionScrollUp)
	}
	if got := m.Lookup(tea.KeyMsg{Type: tea.KeyPgDown}); got != ActionScrollDown {
		t.Fatalf("pgdown -> %v want %v", got, ActionScrollDown)
	}
	// ctrl+k must NOT be scroll_up: the TUI opens its action palette with it,
	// and the default table used to claim otherwise. Since the TUI now
	// dispatches through this map, that claim would have moved the palette.
	if got := m.Lookup(tea.KeyMsg{Type: tea.KeyCtrlK}); got != ActionNone {
		t.Fatalf("ctrl+k -> %v: the action palette lives on this key", got)
	}
	if got := m.Lookup(tea.KeyMsg{Type: tea.KeyCtrlZ}); got != ActionNone {
		t.Fatalf("unbound key -> %v want ActionNone", got)
	}
}

// A Go map can contain raw spellings that normalize to the same runtime key.
// Sorting raw keys before AddOverride makes the diagnostic deterministic and
// avoids collapsing them in an intermediate normalized map.
func TestNewDefaultBuilder_DetectsNormalizedOverrideDuplicate(t *testing.T) {
	b := NewDefaultBuilder(map[string]string{
		"CTRL+K": "scroll_up",
		"ctrl+k": "scroll_down",
	})
	m, err := b.Build()
	if err == nil {
		t.Fatal("normalized duplicate must fail validation")
	}
	ds := m.Diagnostics()
	if len(ds) != 1 || ds[0].Kind != "normalized_duplicate" || ds[0].Key != "ctrl+k" {
		t.Fatalf("diagnostics = %#v", ds)
	}
}

func TestBuilder_RejectsInvalidConfigKey(t *testing.T) {
	b := NewDefaultBuilder(map[string]string{"ctrl+not-a-key": "send"})
	m, err := b.Build()
	if err == nil {
		t.Fatal("invalid key must fail validation")
	}
	if len(m.Diagnostics()) != 1 || m.Diagnostics()[0].Kind != "invalid_key" {
		t.Fatalf("diagnostics = %#v", m.Diagnostics())
	}
}
