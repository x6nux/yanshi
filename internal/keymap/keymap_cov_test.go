package keymap

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCov_NormalizeConfigKey_Aliases covers the escape/return aliases and the
// single-printable-rune branch.
func TestCov_NormalizeConfigKey_Aliases(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"escape", "esc"},
		{"Return", "enter"}, // lowercased
		{"x", "x"},          // single printable rune
	} {
		got, err := normalizeConfigKey(tc.in)
		require.NoError(t, err, tc.in)
		assert.Equal(t, tc.want, got, tc.in)
	}
}

// TestCov_Lookup_NilAndPaste covers the nil-Map and un-normalizable-key paths.
func TestCov_Lookup_NilAndPaste(t *testing.T) {
	var nilMap *Map
	assert.Equal(t, ActionNone, nilMap.Lookup(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}}))

	m, err := NewDefaultBuilder(nil).Build()
	require.NoError(t, err)
	// Paste payload → NormalizeKey fails → ActionNone.
	assert.Equal(t, ActionNone, m.Lookup(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}, Paste: true}))
}

// TestCov_Diagnostics_Nil covers the nil-Map defensive return.
func TestCov_Diagnostics_Nil(t *testing.T) {
	var nilMap *Map
	assert.Nil(t, nilMap.Diagnostics())
}

// TestCov_Build_OverrideFirst covers the "first occurrence is an override"
// branch (overrideRaw recorded, no diagnostic).
func TestCov_Build_OverrideFirst(t *testing.T) {
	_, err := NewDefaultBuilder(map[string]string{"ctrl+z": "send"}).Build()
	require.NoError(t, err)
}

// TestCov_Build_SortSameKind covers the sort comparator's same-Kind Key
// tiebreak (two unknown_action diagnostics).
func TestCov_Build_SortSameKind(t *testing.T) {
	_, err := NewDefaultBuilder(map[string]string{"ctrl+z": "unkA", "ctrl+y": "unkB"}).Build()
	require.Error(t, err)
}

// TestCov_Build_SortDiffKind covers the sort comparator's Kind ordering
// (invalid_key vs unknown_action).
func TestCov_Build_SortDiffKind(t *testing.T) {
	_, err := NewDefaultBuilder(map[string]string{"ctrl+bad": "send", "ctrl+z": "unkX"}).Build()
	require.Error(t, err)
}

// TestCov_Vim_Configured covers the configured-action-wins branch.
func TestCov_Vim_Configured(t *testing.T) {
	v := &VimMachine{}
	r := v.HandleKey("x", ActionScrollDown)
	assert.Equal(t, ActionScrollDown, r.Action)
	assert.True(t, r.Consumed)
}

// TestCov_Vim_NormalDefault covers the Normal-mode default (unknown key
// consumed, no action).
func TestCov_Vim_NormalDefault(t *testing.T) {
	v := &VimMachine{} // zero value = VimModeNormal
	r := v.HandleKey("x", ActionNone)
	assert.True(t, r.Consumed)
	assert.Equal(t, ActionNone, r.Action)
}

// TestCov_Vim_VisualEscAndK covers the Visual-mode esc→Normal and k→scroll-up
// branches.
func TestCov_Vim_VisualEscAndK(t *testing.T) {
	v := &VimMachine{mode: VimModeVisual}
	r := v.HandleKey("esc", ActionNone)
	assert.True(t, r.Consumed)
	assert.Equal(t, VimModeNormal, v.mode)

	v2 := &VimMachine{mode: VimModeVisual}
	r2 := v2.HandleKey("k", ActionNone)
	assert.Equal(t, ActionScrollUp, r2.Action)
}

// TestCov_Vim_InvalidMode covers the default (unknown mode) no-op branch.
func TestCov_Vim_InvalidMode(t *testing.T) {
	v := &VimMachine{mode: VimMode(999)}
	r := v.HandleKey("x", ActionNone)
	assert.False(t, r.Consumed)
}
