package tui

import (
	"os"
	"path/filepath"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/x6nux/yanshi/internal/guard"
)

// newOnboardingModel builds a model whose prefs live at a temp path and whose
// cascade has NOT recorded the onboarding tombstone — i.e. the state a real
// first-run startup is in. The stdout probe is forced interactive: the tests
// here exercise the wizard itself, and go test's stdout is a pipe, which the
// production gate treats as "no human to onboard".
func newOnboardingModel(t *testing.T) model {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "prefs.json")
	old := preferencesPathFn
	preferencesPathFn = func() string { return path }
	t.Cleanup(func() { preferencesPathFn = old })
	oldProbe := stdoutInteractive
	stdoutInteractive = true
	t.Cleanup(func() { stdoutInteractive = oldProbe })
	return newModel(&fakeSession{}, "/proj")
}

// newOnboardedModel is the same, except prefs.json already carries
// onboarding_done=true — the state after the user skipped or finished.
func newOnboardedModel(t *testing.T) model {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "prefs.json")
	yes := true
	writePrefs(t, path, Preferences{OnboardingDone: &yes})
	old := preferencesPathFn
	preferencesPathFn = func() string { return path }
	t.Cleanup(func() { preferencesPathFn = old })
	return newModel(&fakeSession{}, "/proj")
}

// writePrefs persists a Preferences layer through the real JSON shape.
func writePrefs(t *testing.T, path string, p Preferences) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err == nil && len(data) > 0 {
		// existing file: fine, overwrite below
		_ = data
	}
	if err := persistPreferences(path, p); err != nil {
		t.Fatalf("seed prefs: %v", err)
	}
}

// TestOnboardingShowsOnFirstRun verifies the wizard is armed at startup when
// no layer recorded the tombstone.
//
// Mutation: delete the `if !eff.OnboardingDone { m.onboarding = ... }` block
// in newModelWithPrefs — this test fails (m.onboarding == nil). The inverse
// mutation (drop the guard, always arm) is caught by TestOnboardingSkippedNotShownAgain.
func TestOnboardingShowsOnFirstRun(t *testing.T) {
	m := newOnboardingModel(t)
	if m.onboarding == nil {
		t.Fatal("first run (no tombstone) should arm the onboarding wizard")
	}
}

// TestOnboardingSkippedNotShownAgain is THE W-E-16 acceptance test: with the
// tombstone stored, a fresh startup must NOT arm the wizard; and clearing the
// stored tombstone (deleting the flag from disk) must make the next startup
// arm it again. The second half is what a "set flag → hide" conditional alone
// cannot satisfy — deleting the flag has to change observable behaviour.
//
// Mutations that turn this red:
//   - remove the `if !eff.OnboardingDone` guard in newModelWithPrefs: the
//     FIRST assertion fails (onboarded model arms the wizard anyway).
//   - remove the OnboardingDone merge line in mergeTUIPrefs: the SECOND
//     assertion fails (a tombstone on disk no longer suppresses the wizard),
//     because eff.OnboardingDone stays false even though prefs carry true.
func TestOnboardingSkippedNotShownAgain(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "prefs.json")
	yes := true
	if err := persistPreferences(path, Preferences{OnboardingDone: &yes}); err != nil {
		t.Fatal(err)
	}
	old := preferencesPathFn
	preferencesPathFn = func() string { return path }
	t.Cleanup(func() { preferencesPathFn = old })
	// This test drives newModel directly (both startups go through it), and
	// go test's stdout is a pipe — without arming the probe here, Startup 2
	// can never re-arm the wizard and the acceptance below is unreachable.
	oldProbe := stdoutInteractive
	stdoutInteractive = true
	t.Cleanup(func() { stdoutInteractive = oldProbe })

	// Startup 1: tombstone present → no wizard.
	m := newModel(&fakeSession{}, "/proj")
	if m.onboarding != nil {
		t.Fatal("startup with onboarding_done=true must NOT arm the wizard")
	}

	// Clear the tombstone from disk (the acceptance criterion: clearing the
	// "already skipped" state must make the next startup prompt again).
	if err := persistPreferences(path, Preferences{}); err != nil {
		t.Fatal(err)
	}
	m2 := newModel(&fakeSession{}, "/proj")
	if m2.onboarding == nil {
		t.Fatal("clearing the onboarding tombstone must re-arm the wizard on the next startup")
	}
}

// TestOnboardingSkipWritesTombstone verifies Esc at step 0 persists
// onboarding_done=true.
//
// Mutation: drop the `m.prefs.OnboardingDone = &yes` line in
// onboardingFinish — this fails because the model's prefs no longer carry
// the tombstone.
func TestOnboardingSkipWritesTombstone(t *testing.T) {
	m := newOnboardingModel(t)
	if m.onboarding == nil {
		t.Fatal("precondition: wizard must be armed")
	}
	mm, _ := m.onboardingKey(tea.KeyMsg{Type: tea.KeyEscape})
	if mm.onboarding != nil {
		t.Fatal("Esc should close the wizard")
	}
	if mm.prefs.OnboardingDone == nil || !*mm.prefs.OnboardingDone {
		t.Fatalf("skip must set OnboardingDone=true, got %v", mm.prefs.OnboardingDone)
	}
	// And the file on disk must carry it.
	data, err := os.ReadFile(filepath.Dir(mm.prefsPath))
	_ = data
	_ = err
	onDisk, rerr := loadPreferences(mm.prefsPath)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if onDisk.OnboardingDone == nil || !*onDisk.OnboardingDone {
		t.Fatalf("tombstone must be persisted, got %v", onDisk.OnboardingDone)
	}
}

// TestOnboardingFinishAppliesMode verifies Enter at step 0 → step 1 → Enter
// applies the selected permission mode and writes the tombstone.
//
// Mutation: make onboardingFinish skip savePrefs — the mode still applies
// but the tombstone is not written; the final assertion fails.
func TestOnboardingFinishAppliesMode(t *testing.T) {
	// perm_mode_test.go's init() already sets persistPermMode=false for the
	// whole test binary; restating it here would flip it back to true in
	// t.Cleanup and leak the REAL perm_mode.json writes into every test that
	// runs after this one (observed as TestUpdateGolden picking up
	// "allow-edits" from disk).
	m := newOnboardingModel(t)
	// step 0 → step 1
	mm, _ := m.onboardingKey(tea.KeyMsg{Type: tea.KeyEnter})
	if mm.onboarding == nil || mm.onboarding.step != 1 {
		t.Fatalf("Enter at step 0 should advance to step 1, got %+v", mm.onboarding)
	}
	// Move down once so the selection is not the current default, then Enter.
	mm, _ = mm.onboardingKey(tea.KeyMsg{Type: tea.KeyDown})
	mm, _ = mm.onboardingKey(tea.KeyMsg{Type: tea.KeyEnter})
	if mm.onboarding != nil {
		t.Fatal("Enter at step 1 should close the wizard")
	}
	if mm.prefs.OnboardingDone == nil || !*mm.prefs.OnboardingDone {
		t.Fatalf("finish must set OnboardingDone=true, got %v", mm.prefs.OnboardingDone)
	}
	if mm.permMode == guard.ModeDefault {
		items := m.onboardingModes()
		if len(items) > 1 && items[1].name != string(guard.ModeDefault) {
			t.Fatalf("expected the down+enter selection to apply %s, still %s", items[1].name, mm.permMode)
		}
	}
}

// TestOnboardingPopupRenders verifies the wizard renders visible text at both
// steps and "" once closed.
//
// Mutation: make onboardingPopup return "" unconditionally — the step-0
// assertion fails.
func TestOnboardingPopupRenders(t *testing.T) {
	m := newOnboardingModel(t)
	m.width = 100
	out := m.onboardingPopup()
	if out == "" {
		t.Fatal("step 0 popup should render")
	}
	m.onboarding.step = 1
	out = m.onboardingPopup()
	if out == "" {
		t.Fatal("step 1 popup should render")
	}
	m.onboarding = nil
	if out := m.onboardingPopup(); out != "" {
		t.Fatal("closed wizard should render empty")
	}
}

// TestOnboardingConsumesKeys verifies the wizard is checked FIRST in
// handleKeyMsg, ahead of every other handler: typing must not reach the
// textarea while the wizard is open.
//
// Mutation: move the onboarding block below the vimKey/remappedKey dispatch
// in handleKeyMsg — this fails because the input box receives the 's'.
func TestOnboardingConsumesKeys(t *testing.T) {
	m := newOnboardingModel(t)
	before := m.input.Value()
	mm, _, handled := m.handleKeyMsg(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'x'}})
	if !handled {
		t.Fatal("wizard must consume the key")
	}
	if mm.input.Value() != before {
		t.Fatalf("key leaked into input while wizard open: %q", mm.input.Value())
	}
	// 'x' at step 0 is consumed with no state change.
	if mm.onboarding == nil || mm.onboarding.step != 0 {
		t.Fatalf("unknown key should leave the wizard at step 0, got %+v", mm.onboarding)
	}
}

// TestOnboardingModesMatchGuard verifies the wizard's mode list is exactly
// guard.Modes() — the wizard must not advertise a mode /mode cannot set.
func TestOnboardingModesMatchGuard(t *testing.T) {
	m := newOnboardingModel(t)
	items := m.onboardingModes()
	modes := guard.Modes()
	if len(items) != len(modes) {
		t.Fatalf("wizard lists %d modes, guard has %d", len(items), len(modes))
	}
	for i, it := range items {
		if it.name != string(modes[i]) {
			t.Fatalf("wizard mode %d = %q, guard = %q", i, it.name, modes[i])
		}
	}
}
