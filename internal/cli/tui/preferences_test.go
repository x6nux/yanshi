package tui

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/x6nux/yanshi/internal/i18n"
)

// TestPreferences_FourLevelMerge exercises every sparse preference field
// with the exact priority: flag > env > user prefs > project config >
// defaults. Pointer booleans preserve "unset" versus an explicit false at
// every layer.
func TestPreferences_FourLevelMerge(t *testing.T) {
	f, tt := false, true
	project := Preferences{
		UILocale: "zh-Hans", ThemeName: "project-theme", KeymapName: "project-map",
		HighContrast: &tt, Vim: &tt,
	}
	user := Preferences{
		UILocale: "en", ThemeName: "user-theme", KeymapName: "user-map",
		HighContrast: &f, Vim: &f,
	}
	env := Preferences{
		UILocale: "zh-Hans", ThemeName: "env-theme", KeymapName: "env-map",
		HighContrast: &tt, Vim: &tt,
	}
	flags := Preferences{
		UILocale: "en", ThemeName: "flag-theme", KeymapName: "flag-map",
		HighContrast: &f, Vim: &f,
	}

	got := mergeTUIPrefs(flags, env, user, project)
	want := EffectivePreferences{
		UILocale: "en", ThemeName: "flag-theme", KeymapName: "flag-map",
		// Frecency defaults ON and no layer here sets it; stated explicitly so a
		// future default flip fails this test instead of passing silently.
		HighContrast: false, Vim: false, Frecency: true,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("flag precedence: got %#v want %#v", got, want)
	}

	// Remove flags, then env must win all fields.
	got = mergeTUIPrefs(Preferences{}, env, user, project)
	want = EffectivePreferences{
		UILocale: "zh-Hans", ThemeName: "env-theme", KeymapName: "env-map",
		HighContrast: true, Vim: true,
		Frecency: true,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("env precedence: got %#v want %#v", got, want)
	}

	// Explicit false in user prefs must beat project true.
	got = mergeTUIPrefs(Preferences{}, Preferences{}, user, project)
	want = EffectivePreferences{
		UILocale: "en", ThemeName: "user-theme", KeymapName: "user-map",
		HighContrast: false, Vim: false,
		Frecency: true,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("user precedence: got %#v want %#v", got, want)
	}

	// With every sparse layer empty, defaults are stable.
	got = mergeTUIPrefs(Preferences{}, Preferences{}, Preferences{}, Preferences{})
	want = EffectivePreferences{
		UILocale: "auto", ThemeName: "default", KeymapName: "default",
		HighContrast: false, Vim: false,
		Frecency: true,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("defaults: got %#v want %#v", got, want)
	}
}

// TestPreferences_LoadInjectsPath uses t.TempDir so the test does not touch
// the real prefs file (structural fix #14).
func TestPreferences_LoadInjectsPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "prefs.json")
	_ = os.WriteFile(path, []byte(`{"ui_locale":"en"}`), 0600)
	p, err := loadPreferences(path)
	if err != nil {
		t.Fatal(err)
	}
	if p.UILocale != "en" {
		t.Fatalf("UILocale: %q", p.UILocale)
	}
}

// TestPreferences_PersistAtomic deterministically covers stale temp files,
// replacement failure, old-file preservation, and temp cleanup on every OS.
func TestPreferences_PersistAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "prefs.json")
	old := []byte(`{"ui_locale":"en"}`)
	if err := os.WriteFile(path, old, 0600); err != nil {
		t.Fatal(err)
	}
	// A stale temp file must not collide with the random suffix for this write.
	stale := path + ".tmp.stale"
	if err := os.WriteFile(stale, []byte("stale"), 0600); err != nil {
		t.Fatal(err)
	}

	oldReplace := replacePreferencesFile
	replacePreferencesFile = func(src, dst string) error {
		if dst != path || !strings.HasPrefix(src, path+".tmp.") {
			t.Fatalf("unexpected replace %q -> %q", src, dst)
		}
		return errors.New("injected replace failure")
	}
	t.Cleanup(func() { replacePreferencesFile = oldReplace })

	f := false
	err := persistPreferences(path, Preferences{UILocale: "zh-Hans", Vim: &f})
	if err == nil || !strings.Contains(err.Error(), "injected replace failure") {
		t.Fatalf("expected injected replace failure, got %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != string(old) {
		t.Fatalf("old prefs changed: got %s want %s", data, old)
	}
	matches, err := filepath.Glob(path + ".tmp.*")
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 || matches[0] != stale {
		t.Fatalf("new temp not cleaned or stale file changed: %v", matches)
	}
}

// TestPreferences_CleanupRestoresState ensures tests using t.Cleanup
// restore persistPermMode + path overrides (structural fix #14).
func TestPreferences_CleanupRestoresState(t *testing.T) {
	oldDisable := preferencesPersistDisabled
	preferencesPersistDisabled = true
	t.Cleanup(func() { preferencesPersistDisabled = oldDisable })

	err := persistPreferences("/nonexistent/path/prefs.json", Preferences{})
	if err != nil {
		t.Fatalf("persist should be no-op when disabled: %v", err)
	}
}

func TestPreferences_AutoLocaleRecomputesAfterReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "prefs.json")
	if err := persistPreferences(path, Preferences{UILocale: "auto"}); err != nil {
		t.Fatal(err)
	}

	t.Setenv("LC_ALL", "en_US.UTF-8")
	firstPrefs, err := loadPreferences(path)
	if err != nil {
		t.Fatal(err)
	}
	first, err := i18n.NewBundle(firstPrefs.UILocale)
	if err != nil {
		t.Fatal(err)
	}
	if first.Persistent() != "auto" || first.Effective() != "en" {
		t.Fatalf("first = %q/%q", first.Persistent(), first.Effective())
	}

	t.Setenv("LC_ALL", "zh_CN.UTF-8")
	secondPrefs, err := loadPreferences(path)
	if err != nil {
		t.Fatal(err)
	}
	second, err := i18n.NewBundle(secondPrefs.UILocale)
	if err != nil {
		t.Fatal(err)
	}
	if second.Persistent() != "auto" || second.Effective() != "zh-Hans" {
		t.Fatalf("second = %q/%q", second.Persistent(), second.Effective())
	}
}

func boolPtr(b bool) *bool { return &b }
