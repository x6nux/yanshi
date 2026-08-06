package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// TestVimModeOwnsKeystrokes is the assertion /vim on needed and did not have.
//
// keymap.VimMachine was a complete, tested state machine with zero production
// consumers. The command persisted a preference, printed "vim mode enabled",
// and changed nothing about editing — the product asserted something untrue to
// the user's face, which is the exact defect class this work package exists to
// remove, freshly reintroduced by the command that turned the feature "on".
func TestVimModeOwnsKeystrokes(t *testing.T) {
	withTempPrefs(t)
	m := newModel(&recordingSession{}, "/proj")

	// Off by default: a plain "j" must reach the textarea.
	before := m.input.Value()
	mm, _, handled := m.handleKeyMsg(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	if handled {
		t.Fatalf("vim is off but j was consumed; input was %q", before)
	}
	_ = mm

	// Turn it on.
	on, _ := m.runCommand("/vim on")
	m = on.(model)
	if m.vim == nil {
		t.Fatal("/vim on did not construct the state machine: the command would be " +
			"printing 'vim mode enabled' with nothing behind it")
	}

	// Esc drops to Normal, and is swallowed rather than typed.
	m, _, handled = m.handleKeyMsg(tea.KeyMsg{Type: tea.KeyEscape})
	if !handled {
		t.Fatal("Esc was not consumed by vim mode")
	}
	if m.VimModeLabel() == "" {
		t.Fatal("no modal indicator: a modal editor whose mode you cannot see is " +
			"worse than none")
	}

	// In Normal, "i" is a transition and must NOT be typed into the textarea.
	valueBefore := m.input.Value()
	m, _, handled = m.handleKeyMsg(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("i")})
	if !handled {
		t.Fatal("i was not consumed in Normal mode")
	}
	if m.input.Value() != valueBefore {
		t.Errorf("the mode-transition key was typed into the prompt: %q -> %q",
			valueBefore, m.input.Value())
	}

	// Turning it off drops the machine, restoring the plain editing path.
	off, _ := m.runCommand("/vim off")
	m = off.(model)
	if m.vim != nil {
		t.Error("/vim off left the state machine in place")
	}
	if _, _, handled := m.handleKeyMsg(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")}); handled {
		t.Error("keys are still being consumed after /vim off")
	}
}

// TestVimDoesNotStealKeysFromPopups pins the ordering that makes vim usable at
// all: j/k scroll the transcript in Normal mode, and a list popup needs the
// same keys to move its own cursor.
func TestVimDoesNotStealKeysFromPopups(t *testing.T) {
	withTempPrefs(t)
	m := newModel(&recordingSession{}, "/proj")
	on, _ := m.runCommand("/vim on")
	m = on.(model)
	m.vim.SetMode(0) // Normal

	m.helpVisible = true
	if _, consumed := m.vimKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")}); consumed {
		t.Error("vim consumed j while the help panel was open; the panel's own " +
			"search would stop receiving characters")
	}
	m.helpVisible = false

	m.paletteItems = []command{{name: "x"}}
	if _, consumed := m.vimKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("k")}); consumed {
		t.Error("vim consumed k while the command palette was open")
	}
}

// TestSessionThemeSurvivesAPreferenceCommand covers the cross-command
// interference remerge introduced: it recomputes the theme from the persisted
// cascade, so /theme followed by any preference command silently reverted the
// colours with nothing on screen connecting the two.
func TestSessionThemeSurvivesAPreferenceCommand(t *testing.T) {
	withTempPrefs(t)
	m := newModel(&recordingSession{}, "/proj")

	mm, _ := m.runCommand("/theme muted")
	m = mm.(model)
	if m.theme != ThemeName("muted") {
		t.Fatalf("theme = %q after /theme muted", m.theme)
	}

	mm, _ = m.runCommand("/vim on")
	m = mm.(model)
	if m.theme != ThemeName("muted") {
		t.Errorf("theme reverted to %q after an unrelated preference command", m.theme)
	}

	// Accessibility wins, though: /contrast on must beat a theme choice or it
	// does not do its job.
	mm, _ = m.runCommand("/contrast on")
	m = mm.(model)
	if m.theme != ThemeHighContrast {
		t.Errorf("theme = %q after /contrast on: high contrast is an accessibility "+
			"switch and must override a colour preference", m.theme)
	}
}

// TestVimModeIndicatorReachesTheFooter is the second wiring assertion.
// VimModeLabel returning the right string proves nothing if the footer never
// calls it — the same "built but not assembled" shape as the state machine it
// reports on.
func TestVimModeIndicatorReachesTheFooter(t *testing.T) {
	withTempPrefs(t)
	m := newModel(&recordingSession{}, "/proj")
	m.width = 200

	plain := stripANSI(m.statusHeader())

	on, _ := m.runCommand("/vim on")
	m = on.(model)
	m.width = 200
	m.vim.SetMode(0) // Normal
	withVim := stripANSI(m.statusHeader())

	if withVim == plain {
		t.Fatal("the footer is unchanged with vim mode on: the indicator never " +
			"reaches the screen")
	}
	if !strings.Contains(withVim, m.VimModeLabel()) {
		t.Errorf("footer does not contain the mode label %q:\n%s",
			m.VimModeLabel(), withVim)
	}
}
