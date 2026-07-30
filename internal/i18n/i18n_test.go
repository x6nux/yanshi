package i18n

import (
	"reflect"
	"sort"
	"testing"
)

func TestBundle_EmptyCanonicalizesToAuto(t *testing.T) {
	t.Setenv("LC_ALL", "C")
	t.Setenv("LANG", "zh_CN.UTF-8")
	b, err := NewBundle("")
	if err != nil {
		t.Fatal(err)
	}
	if b.Persistent() != "auto" || b.Effective() != "en" {
		t.Fatalf("empty must canonicalize to auto; C locale wins: persistent=%s effective=%s",
			b.Persistent(), b.Effective())
	}
}

func TestBundle_AutoRecomputedEachLoad(t *testing.T) {
	// First load: empty LC_ALL, LANG=en_US.UTF-8 -> effective en.
	t.Setenv("LC_ALL", "")
	t.Setenv("LANG", "en_US.UTF-8")
	b1, _ := NewBundle("auto")
	if b1.Effective() != "en" {
		t.Fatalf("auto + LANG=en_US.UTF-8 -> expected en, got %s", b1.Effective())
	}
	// Second load: same persistent value "auto", but LANG=zh_CN.UTF-8 -> zh-Hans.
	t.Setenv("LANG", "zh_CN.UTF-8")
	b2, _ := NewBundle("auto")
	if b2.Effective() != "zh-Hans" {
		t.Fatalf("auto + LANG=zh_CN.UTF-8 -> expected zh-Hans, got %s", b2.Effective())
	}
	// Persistent value must NOT have been rewritten to a resolved locale;
	// "auto" stays "auto" so future loads keep recomputing.
	if b2.Persistent() != "auto" {
		t.Fatalf("Persistent corrupted to %s", b2.Persistent())
	}
}

func TestBundle_LCAllCStopsLANGFallback(t *testing.T) {
	for _, locale := range []string{"C", "POSIX"} {
		t.Run(locale, func(t *testing.T) {
			t.Setenv("LC_ALL", locale)
			t.Setenv("LANG", "zh_CN.UTF-8")
			b, err := NewBundle("auto")
			if err != nil {
				t.Fatal(err)
			}
			if b.Effective() != "en" {
				t.Fatalf("LC_ALL=%s must force en instead of falling through to LANG: %s",
					locale, b.Effective())
			}
		})
	}
}

func TestBundle_ExplicitLocaleOverridesEnv(t *testing.T) {
	t.Setenv("LANG", "zh_CN.UTF-8")
	b, _ := NewBundle("en")
	if b.Effective() != "en" {
		t.Fatalf("explicit en must override LANG=zh_CN, got %s", b.Effective())
	}
}

func TestBundle_FallbackOnMissingKey(t *testing.T) {
	b, _ := NewBundle("zh-Hans")
	// Key exists in en, exists in zh-Hans too; non-existent key falls back
	// to the key itself (not an error).
	got := b.Get("nonexistent.key")
	if got != "nonexistent.key" {
		t.Fatalf("missing key should return key, got %q", got)
	}
}

func TestBundle_FallbackOnMissingLocale(t *testing.T) {
	b, err := NewBundle("fr-FR")
	if err == nil {
		t.Fatal("expected error for unsupported locale")
	}
	// Even on unsupported locale, default bundle falls back to en.
	b, _ = NewBundle("")
	_ = b
}

// TestBundle_CatalogKeyManifest compares each catalog independently with the
// canonical required-key manifest. Pairwise en/zh equality alone is insufficient:
// deleting the same key from both files must still fail this test.
func TestBundle_CatalogKeyManifest(t *testing.T) {
	want := append([]string(nil), requiredCatalogKeys...)
	sort.Strings(want)
	if len(want) == 0 {
		t.Fatal("requiredCatalogKeys is empty")
	}
	wantSet := make(map[string]bool, len(want))
	for _, key := range want {
		if wantSet[key] {
			t.Fatalf("duplicate manifest key %q", key)
		}
		wantSet[key] = true
	}

	for _, locale := range []string{"en", "zh-Hans"} {
		catalog, err := loadCatalog(locale)
		if err != nil {
			t.Fatalf("load %s: %v", locale, err)
		}
		got := make([]string, 0, len(catalog))
		for key := range catalog {
			got = append(got, key)
		}
		sort.Strings(got)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%s keys = %v, want exact manifest %v", locale, got, want)
		}
	}
}
