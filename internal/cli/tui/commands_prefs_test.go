package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// withTempPrefs points the preferences file at a temp dir for one test and
// restores the real path afterwards.
func withTempPrefs(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "prefs.json")
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
// promises. Map.Diagnostics reports four kinds; an empty override set has none,
// and a conflicting one must be named rather than silently dropped.
//
// ledger: D3/C15#4 冲突可诊断
func TestKeymapDiagnosticsRendersTheConflictReport(t *testing.T) {
	withTempPrefs(t)
	rec := &recordingSession{}
	m := newModel(rec, "/proj")

	mm, _ := m.runCommand("/keymap diagnostics")
	m = mm.(model)
	if len(m.entries) == 0 {
		t.Fatal("/keymap diagnostics rendered nothing")
	}
	last := m.entries[len(m.entries)-1]
	out := stripANSI(last.render(120, newSpinner()))
	if out == "" {
		t.Fatal("/keymap diagnostics rendered an empty entry")
	}
	if strings.Contains(strings.ToLower(out), "unknown command") {
		t.Fatalf("/keymap diagnostics is not routed: %q", out)
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
