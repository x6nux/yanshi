// Package tui tests for model.go's NewProgram wiring (RE-B, the fix-e1 review
// of W-E-01): the sole production call site of both ApplyColorProfile and the
// alt-screen gate had zero coverage above the pure-function level — mutating
// either (widening the alt-screen gate to unconditional, or deleting the
// ApplyColorProfile call) left the full test suite green. These two tests
// exercise the real wiring, not a re-implementation of it: TestNewProgram_
// AppliesRealCapability calls the actual NewProgram (which reads os.Getenv,
// not an injected fake) and asserts the process-global lipgloss profile it
// leaves behind; TestProgramOptions_AltScreenGatedByCapability calls the
// extracted programOptions directly, since tea.Program exposes no accessor
// for which options it was constructed with.
package tui

import (
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"github.com/x6nux/yanshi/internal/cli"
)

// TestProgramOptions_AltScreenGatedByCapability proves the alt-screen and
// mouse-cell-motion bubbletea options are present only when
// TermCapability.AltScreen is true (RE-D joined mouse to the same gate — see
// programOptions' body comment) — the gate a prior mutation ("if true ||
// cap.AltScreen") could remove without any test noticing.
func TestProgramOptions_AltScreenGatedByCapability(t *testing.T) {
	with := programOptions(cli.TermCapability{AltScreen: true})
	without := programOptions(cli.TermCapability{AltScreen: false})
	if len(with) != 2 {
		t.Fatalf("AltScreen=true: got %d options, want 2 (mouse + alt-screen)", len(with))
	}
	if len(without) != 0 {
		t.Fatalf("AltScreen=false: got %d options, want 0 (both gated off) — alt-screen gate not honored", len(without))
	}
}

// TestNewProgram_AppliesRealCapability proves NewProgram actually calls
// ApplyColorProfile with the capability it detects from the real process
// environment (os.Getenv, via cli.DetectCapability) — not a hardcoded or
// stale profile. A prior mutation deleting the ApplyColorProfile(cap.Profile)
// call left every test green because no test observed lipgloss's
// process-global color profile after calling NewProgram.
//
// The baseline is deliberately set to a profile NO_COLOR=1 must NOT produce
// (TrueColor), so a deleted call is distinguishable from a call that merely
// happens to already agree with the baseline.
func TestNewProgram_AppliesRealCapability(t *testing.T) {
	withColorProfile(t, termenv.TrueColor)
	// W-E-06: NewProgram also calls SetHyperlinksEnabled(cap.AltScreen) —
	// another process-global side effect alongside the color profile above.
	prevHyperlinks := hyperlinksEnabled.Load()
	t.Cleanup(func() { hyperlinksEnabled.Store(prevHyperlinks) })

	t.Setenv("TERM", "xterm-256color") // must not be "dumb" — that outranks NO_COLOR
	t.Setenv("NO_COLOR", "1")

	p := NewProgram(&cli.Session{}, "/proj", Preferences{})
	if p == nil {
		t.Fatal("NewProgram returned nil")
	}
	if got := lipgloss.ColorProfile(); got != termenv.Ascii {
		t.Fatalf("NewProgram did not apply the detected capability: lipgloss profile = %s, want Ascii (NO_COLOR=1)", got.Name())
	}
}
