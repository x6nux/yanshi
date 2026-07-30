package i18n

import (
	"strings"
	"testing"
)

// TestBundle_GetExistingKey verifies that Get returns the catalog value
// when the key exists (rather than falling back to the key itself).
func TestBundle_GetExistingKey(t *testing.T) {
	b, err := NewBundle("en")
	if err != nil {
		t.Fatal(err)
	}
	got := b.Get("tui.input.placeholder")
	if got == "" || got == "tui.input.placeholder" {
		t.Fatalf("Get(existing key) = %q, expected a translation", got)
	}
}

// TestBundle_GetF covers format substitution via GetF.
func TestBundle_GetF(t *testing.T) {
	b, err := NewBundle("en")
	if err != nil {
		t.Fatal(err)
	}
	// GetF with a key that exists and substitution variables.
	vars := map[string]string{"name": "test-format"}
	result := b.GetF("tui.command.help.title", vars)
	// The en catalog entry for tui.command.help.title might not contain
	// {name}, but GetF should at least not crash and should return
	// a non-empty string.
	if result == "" {
		t.Fatal("GetF returned empty")
	}
	// Test with a key that has no placeholder (no-op).
	result2 := b.GetF("nonexistent.key", vars)
	if result2 != "nonexistent.key" {
		t.Fatalf("GetF missing key = %q", result2)
	}
}

// TestBundle_GetFWithActualSubstitution tests a case where variables
// are actually substituted into the string.
func TestBundle_GetFWithActualSubstitution(t *testing.T) {
	b, err := NewBundle("en")
	if err != nil {
		t.Fatal(err)
	}
	// Use a key that we know exists, and test that placeholder substitution
	// works. The tui.command.help.row key is "{name} — {help}" in the
	// en catalog.
	rowKey := "tui.command.help.row"
	orig := b.Get(rowKey)
	if orig == "" || orig == rowKey {
		t.Skipf("row key not found/enabled in catalog: %q", orig)
	}
	vars := map[string]string{"name": "test", "help": "help text"}
	result := b.GetF(rowKey, vars)
	if !strings.Contains(result, "test") || !strings.Contains(result, "help text") {
		t.Fatalf("GetF substitution failed: %q (from base %q)", result, orig)
	}
}

// TestBundle_GetFMultipleOverlappingKeys tests ReplaceAll behavior
// when keys are substrings of each other.
func TestBundle_GetFMultipleOverlappingKeys(t *testing.T) {
	b := &Bundle{
		persistent: "en",
		effective:  "en",
		messages:   map[string]string{"greet": "Hello {a} and {ab}"},
	}
	result := b.GetF("greet", map[string]string{"a": "X", "ab": "YZ"})
	// ReplaceAll in order: {a} first → "Hello X and {ab}", then {ab} → "Hello X and YZ"
	if result != "Hello X and YZ" {
		t.Fatalf("expected correct substitution order, got %q", result)
	}
}

// TestDetectLocale_NoEnvVars covers the fallback-to-en path when neither
// LC_ALL nor LANG is set.
func TestDetectLocale_NoEnvVars(t *testing.T) {
	t.Setenv("LC_ALL", "")
	t.Setenv("LANG", "")
	result := detectLocale()
	if result != "en" {
		t.Fatalf("expected en when no env vars set, got %s", result)
	}
}

// TestNormalizeLocale_AtModifier covers the @modifier stripping branch.
func TestNormalizeLocale_AtModifier(t *testing.T) {
	cases := []struct {
		raw  string
		want string
	}{
		{raw: "zh_CN@euro", want: "zh-Hans"},
		{raw: "en_US@euro", want: "en"},
		{raw: "zh_TW@euro", want: "en"},
	}
	for _, c := range cases {
		t.Run(c.raw, func(t *testing.T) {
			got := normalizeLocale(c.raw)
			if got != c.want {
				t.Fatalf("normalizeLocale(%q) = %q, want %q", c.raw, got, c.want)
			}
		})
	}
}

// TestNormalizeLocale_ZHTWandHK covers zh_TW / zh_HK -> en.
func TestNormalizeLocale_ZHTWandHK(t *testing.T) {
	cases := []string{"zh_TW.UTF-8", "zh_HK.UTF-8", "zh-HK", "zh-TW"}
	for _, raw := range cases {
		t.Run(raw, func(t *testing.T) {
			got := normalizeLocale(raw)
			if got != "en" {
				t.Fatalf("normalizeLocale(%q) = %q, want en", raw, got)
			}
		})
	}
}

// TestNormalizeLocale_ZhHant covers zh-Hant prefix -> en.
func TestNormalizeLocale_ZhHant(t *testing.T) {
	got := normalizeLocale("zh-Hant")
	if got != "en" {
		t.Fatalf("normalizeLocale(zh-Hant) = %q, want en", got)
	}
	got = normalizeLocale("zh-Hant-TW")
	if got != "en" {
		t.Fatalf("normalizeLocale(zh-Hant-TW) = %q, want en", got)
	}
}

// TestNormalizeLocale_EmptyRaw covers the empty-string input which
// reaches the default case.
func TestNormalizeLocale_EmptyRaw(t *testing.T) {
	got := normalizeLocale("")
	if got != "en" {
		t.Fatalf("normalizeLocale('') = %q, want en", got)
	}
}

// TestNormalizeLocale_UnknownLanguage covers branches that fall through
// to the default case (anything not matching en, zh-*, C, POSIX).
func TestNormalizeLocale_UnknownLanguage(t *testing.T) {
	got := normalizeLocale("fr")
	if got != "en" {
		t.Fatalf("normalizeLocale(fr) = %q, want en", got)
	}
	got = normalizeLocale("de_DE.UTF-8")
	if got != "en" {
		t.Fatalf("normalizeLocale(de_DE) = %q, want en", got)
	}
}

// TestLoadCatalog_Nonexistent covers the ReadFile error path.

// TestLoadCatalog_BadJSON covers the JSON unmarshal error path.
func TestLoadCatalog_BadJSON(t *testing.T) {
	_, err := loadCatalog("bad")
	if err == nil {
		t.Fatal("expected error for bad JSON catalog")
	}
}
func TestLoadCatalog_Nonexistent(t *testing.T) {
	_, err := loadCatalog("nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent catalog")
	}
}

// TestBundle_UnsupportedLocaleError covers the NewBundle path where
// an unsupported (but not cause-lookup-failure) locale is requested,
// which returns a bundle plus an error.
func TestBundle_UnsupportedLocaleError(t *testing.T) {
	b, err := NewBundle("fr")
	if err == nil {
		t.Fatal("expected error for unsupported locale")
	}
	if b == nil {
		t.Fatal("expected non-nil bundle even with error")
	}
	if b.Effective() != "en" {
		t.Fatalf("effective locale = %q, want en", b.Effective())
	}
	if b.Persistent() != "fr" {
		t.Fatalf("persistent locale = %q, want fr", b.Persistent())
	}
	// Bundle must still be usable.
	if got := b.Get("tui.input.placeholder"); got == "" || got == "tui.input.placeholder" {
		t.Fatalf("fallback bundle Get = %q", got)
	}
}

// TestPersistentAndEffective verifies the getter methods.
func TestPersistentAndEffective(t *testing.T) {
	b, err := NewBundle("auto")
	if err != nil {
		t.Fatal(err)
	}
	if b.Persistent() != "auto" {
		t.Fatalf("Persistent = %q", b.Persistent())
	}
	if b.Effective() != "en" && b.Effective() != "zh-Hans" {
		t.Fatalf("unexpected Effective = %q", b.Effective())
	}
}

// TestNewBundle_EmptyTrimsToAuto covers the "" -> "auto" path.
func TestNewBundle_EmptyTrimsToAuto(t *testing.T) {
	b, err := NewBundle("")
	if err != nil {
		t.Fatal(err)
	}
	if b.Persistent() != "auto" {
		t.Fatalf("Persistent = %q", b.Persistent())
	}
}
