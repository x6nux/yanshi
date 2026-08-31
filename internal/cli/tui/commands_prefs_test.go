package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/require"
)

// withTempPrefs points the preferences file at a temp dir for one test and
// restores the real path afterwards. It also seeds the W-E-16 onboarding
// tombstone: an empty temp prefs file is a "first run", which would arm the
// onboarding wizard and have it consume every keystroke the calling test
// sends. Tests that want the wizard itself (onboarding_test.go) construct
// their own untombeded state rather than going through here.
func withTempPrefs(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "prefs.json")
	yes := true
	if err := persistPreferences(path, Preferences{OnboardingDone: &yes}); err != nil {
		t.Fatalf("seed onboarding tombstone: %v", err)
	}
	prev := preferencesPathFn
	preferencesPathFn = func() string { return path }
	t.Cleanup(func() { preferencesPathFn = prev })
	return path
}

// TestPreferenceCommandsAreRegistered pins the whole of C15's user-facing
// surface, which did not exist.
//
// internal/keymap is a complete, tested leaf package; Preferences carries six
// fields with a four-layer merge and atomic persistence; the i18n catalog
// already holds every prompt string these commands emit -- and typing /keymap
// produced "unknown command", because none of the four names was ever added to
// commandTable and mergeTUIPrefs had no production caller at all.
//
// The diagnostics subcommand matters beyond its own feature: doctor's keymap
// failure message (internal/cli::checkKeymapConfig) tells the operator verbatim
// to run "/keymap diagnostics", so until this existed the single piece of
// advice the product gave was itself a dead end.
//
// ledger: D3/C15#3 高对比主题
func TestPreferenceCommandsAreRegistered(t *testing.T) {
	for _, name := range []string{"keymap", "vim", "contrast", "locale"} {
		if _, ok := lookupCommand(name); !ok {
			t.Errorf("/%s is not in commandTable: typing it yields \"unknown command\", "+
				"and doctor points users at /keymap diagnostics", name)
		}
	}
}

// TestKeymapDiagnosticsRendersTheConflictReport drives the subcommand doctor
// promises, against a keymap that actually has something to report.
//
// The first version of this test asserted only that the output was non-empty
// and did not say "unknown command". Both hold when the handler emits an empty
// string (ackEntry decorates it) and when the keymap is the default one, which
// has no diagnostics at all — so it passed without ever rendering a single
// diagnostic. A mutation probe replacing the whole call with "" left it green.
//
// Map.Diagnostics reports four kinds; a conflict is the one an operator hits,
// and it is what checkKeymapConfig sends them here to see.
//
// ledger: D3/C15#4 冲突可诊断
func TestKeymapDiagnosticsRendersTheConflictReport(t *testing.T) {
	// The assertions below match the en catalog verbatim, so the locale has to
	// be pinned: i18n resolves "auto" from LC_ALL/LANG on every bundle load, and
	// on a zh machine tui.command.keymap.none renders in Chinese and this test
	// fails for a reason that has nothing to do with keymap diagnostics.
	t.Setenv("LC_ALL", "C")
	t.Setenv("LANG", "")
	withTempPrefs(t)
	prev := projectBindings
	// Two spellings of the same key: keymap reports a normalized_duplicate,
	// which is exactly the state config validation refuses to start on.
	SetProjectBindings(map[string]string{"CTRL+G": "scroll_up", "ctrl+g": "scroll_down"})
	t.Cleanup(func() { SetProjectBindings(prev) })

	m := newModel(&recordingSession{}, "/proj")
	if len(m.keys.Diagnostics()) == 0 {
		t.Fatal("the fixture produced no diagnostics, so this test cannot observe " +
			"whether they are rendered")
	}

	mm, _ := m.runCommand("/keymap diagnostics")
	m = mm.(model)
	if len(m.entries) == 0 {
		t.Fatal("/keymap diagnostics rendered nothing")
	}
	out := stripANSI(m.entries[len(m.entries)-1].render(120, newSpinner()))
	if strings.Contains(strings.ToLower(out), "unknown command") {
		t.Fatalf("/keymap diagnostics is not routed: %q", out)
	}
	if !strings.Contains(out, "ctrl+g") {
		t.Errorf("the report does not name the offending key: %q", out)
	}
	if strings.Contains(out, "no keymap diagnostics") {
		t.Errorf("a keymap WITH diagnostics reported none: %q", out)
	}

	// And the clean case says so rather than printing an empty block.
	SetProjectBindings(nil)
	clean := newModel(&recordingSession{}, "/proj")
	cm, _ := clean.runCommand("/keymap diagnostics")
	clean = cm.(model)
	cleanOut := stripANSI(clean.entries[len(clean.entries)-1].render(120, newSpinner()))
	if !strings.Contains(cleanOut, "no keymap diagnostics") {
		t.Errorf("a clean keymap should say so explicitly: %q", cleanOut)
	}
}

// TestPreferencesRoundTripAcrossModels is the persistence half: a preference
// set through a command must survive into a freshly constructed model, which
// is the only thing that makes /vim and /contrast worth having.
//
// ledger: D3/C15#2 Vim 开关
func TestPreferencesRoundTripAcrossModels(t *testing.T) {
	path := withTempPrefs(t)
	rec := &recordingSession{}
	m := newModel(rec, "/proj")

	mm, _ := m.runCommand("/vim on")
	m = mm.(model)
	if !m.effective.Vim {
		t.Fatalf("/vim on did not take effect in the live model")
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("nothing was persisted: %v", err)
	}
	if !strings.Contains(string(raw), "vim") {
		t.Fatalf("prefs file does not carry the setting: %s", raw)
	}

	fresh := newModel(&recordingSession{}, "/proj")
	if !fresh.effective.Vim {
		t.Error("a new model did not pick up the persisted preference: the cascade " +
			"is implemented but not consulted at startup")
	}
}

// TestUILocaleReachesTheTUIFromConfigAndEnv covers I18N1's断链: the TUI hardcoded
// i18n.NewBundle("en"), so neither cfg.I18N.UILocale nor YANSHI_UI_LOCALE could
// change a single string on screen.
//
// The two sources are checked separately because they enter at different layers
// (project config vs env) and the env layer must win.
//
// ledger: D3/I18N1#3 自动检测
func TestUILocaleReachesTheTUIFromConfigAndEnv(t *testing.T) {
	withTempPrefs(t)

	t.Run("project config", func(t *testing.T) {
		m := newModelWithPrefs(&recordingSession{}, "/proj", Preferences{UILocale: "zh-Hans"})
		if got := m.bundle.Effective(); got != "zh-Hans" {
			t.Errorf("bundle locale = %q, want zh-Hans from project config", got)
		}
	})

	t.Run("env beats project config", func(t *testing.T) {
		t.Setenv("YANSHI_UI_LOCALE", "en")
		m := newModelWithPrefs(&recordingSession{}, "/proj", Preferences{UILocale: "zh-Hans"})
		if got := m.bundle.Effective(); got != "en" {
			t.Errorf("bundle locale = %q: env must beat project config", got)
		}
	})
}

// TestLocalePersistsTheDeclaredValueNotTheResolvedOne guards the one i18n rule
// that a naive implementation breaks.
//
// "auto" means "re-resolve from LC_ALL/LANG at every startup". Persisting
// Effective() instead of Persistent() would freeze whatever the machine
// happened to resolve to on the day the command ran, turning a follow-the-system
// setting into a hardcoded one, with no sign anything happened.
//
// ledger: D3/I18N1#2 UI 与输出语言独立
func TestLocalePersistsTheDeclaredValueNotTheResolvedOne(t *testing.T) {
	path := withTempPrefs(t)
	m := newModel(&recordingSession{}, "/proj")

	mm, _ := m.runCommand("/locale auto")
	_ = mm

	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	if strings.Contains(string(raw), `"ui_locale":"en"`) ||
		strings.Contains(string(raw), `"ui_locale":"zh-Hans"`) {
		t.Errorf("/locale auto persisted a RESOLVED locale: %s\n"+
			"auto must stay auto, or it silently stops following the system", raw)
	}
}

// TestHelpSurfacesLocalizeCommandHelp closes the second half of I18N1.
//
// `command.helpKey` had exactly ONE consumer — the /help transcript entry —
// while the doc comment on the struct claimed /help and the palette both
// rendered the localized text. Neither did: collectHelpEntries (the F1 panel)
// and paletteBlock (the `/` menu) both read the static English `help` field.
// So /locale zh-Hans produced a half-translated UI, and D3/I18N1 was flipped to
// done on a surface that was two thirds English.
//
// Both surfaces are asserted, in both directions: the localized string must
// appear AND the English one must not, because a fallback that silently keeps
// English satisfies "contains something" forever.
//
// ledger: D3/I18N1#1 至少 en/zh-Hans 切换
func TestHelpSurfacesLocalizeCommandHelp(t *testing.T) {
	withTempPrefs(t)
	m := newModelWithPrefs(&recordingSession{}, "/proj", Preferences{UILocale: "zh-Hans"})
	if got := m.bundle.Effective(); got != "zh-Hans" {
		t.Fatalf("fixture locale = %q; this test cannot observe localization", got)
	}
	const (
		zh = "清空当前会话"
		en = "reset conversation"
	)

	t.Run("help panel", func(t *testing.T) {
		out := ""
		for _, it := range m.collectHelpEntries() {
			out += it.Label + " " + it.Hint + "\n"
		}
		if !strings.Contains(out, zh) {
			t.Errorf("the F1 help panel does not render the localized help for /clear")
		}
		if strings.Contains(out, en) {
			t.Errorf("the F1 help panel still renders the English help for /clear")
		}
	})

	t.Run("command palette", func(t *testing.T) {
		pm := m
		pm.paletteItems = commandTable
		out := stripANSI(pm.paletteBlock())
		if !strings.Contains(out, zh) {
			t.Errorf("the command palette does not render the localized help for /clear")
		}
		if strings.Contains(out, en) {
			t.Errorf("the command palette still renders the English help for /clear")
		}
	})
}

// TestRebindingAKeyChangesWhatItDoes drives a user-configured binding through
// the TUI's real key path, which is the only thing "可重映射" can mean.
//
// The ledger cited internal/keymap::TestBuild_DefaultLookupUsesRealKeyMessages
// for this clause. That test is necessary and not sufficient: it proves the
// map resolves ctrl+g to an action, and internal/keymap was a complete, tested
// leaf package with ZERO production consumers for exactly as long as this
// feature was claimed to work. A correct lookup table nobody consults rebinds
// nothing.
//
// ctrl+g is deliberate: it is absent from builtinKeys, so it exercises the
// remapped path rather than the hardcoded switch, and it has no default
// binding — the observed effect can only come from the configured one.
//
// ledger: D3/C15#1 核心按键可重映射
func TestRebindingAKeyChangesWhatItDoes(t *testing.T) {
	withTempPrefs(t)
	prev := projectBindings
	t.Cleanup(func() { SetProjectBindings(prev) })

	// Unbound by default: pressing it must do nothing.
	SetProjectBindings(nil)
	base := newModel(&recordingSession{}, "/proj")
	base, _, _ = base.handleKeyMsg(tea.KeyMsg{Type: tea.KeyCtrlG})
	if base.helpVisible {
		t.Fatal("ctrl+g toggled help WITHOUT a binding; this test cannot " +
			"distinguish a rebind from a default")
	}

	// Bound to help: the same keystroke now opens the panel.
	SetProjectBindings(map[string]string{"ctrl+g": "help"})
	m := newModel(&recordingSession{}, "/proj")
	m, _, _ = m.handleKeyMsg(tea.KeyMsg{Type: tea.KeyCtrlG})
	if !m.helpVisible {
		t.Error("the configured binding did not reach the key dispatcher: " +
			"keymap resolves it, but the TUI never asks")
	}
}
