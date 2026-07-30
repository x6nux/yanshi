package auth

import (
	"context"
	"errors"
	"math"
	"path/filepath"
	"testing"
	"time"

	"github.com/x6nux/yanshi/internal/secrets"
)

func newTestManager(t *testing.T) *Manager {
	t.Helper()
	path := filepath.Join(t.TempDir(), "secrets.enc")
	t.Setenv("YANSHI_PASSPHRASE", "test-pass")
	smgr, err := secrets.NewManager(secrets.Config{
		Backend:       "file",
		FilePath:      path,
		PassphraseEnv: "YANSHI_PASSPHRASE",
	})
	if err != nil {
		t.Fatalf("secrets.NewManager: %v", err)
	}
	return NewManager(smgr)
}

func TestManager_ResolveSource(t *testing.T) {
	m := newTestManager(t)
	_ = m.secrets.Store().Set("openai", "main", "sk-from-secret-store")

	cases := []struct {
		name        string
		in          string
		allowLegacy bool
		want        string
		err         bool
	}{
		{"secret ref", "secret://openai/main", false, "sk-from-secret-store", false},
		{"env ref", "env://PATH", false, "", false},
		{"legacy opt-in", "sk-legacy", true, "sk-legacy", false},
		{"raw literal refused", "sk-raw", false, "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			src := CredentialSource{APIKeyRef: c.in, LegacyInsecure: c.allowLegacy}
			got, err := m.ResolveAPIKey(context.Background(), src)
			if c.err {
				if err == nil {
					t.Fatalf("ResolveAPIKey(%q): want error", c.in)
				}
				if !errors.Is(err, secrets.ErrRawLiteralRefused) {
					t.Fatalf("ResolveAPIKey(%q): want ErrRawLiteralRefused, got %v", c.in, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ResolveAPIKey(%q): %v", c.in, err)
			}
			if c.name == "secret ref" && got != c.want {
				t.Fatalf("got %q want %q", got, c.want)
			}
			if c.name == "legacy opt-in" && got != c.want {
				t.Fatalf("got %q want %q", got, c.want)
			}
		})
	}
}

func TestManager_Status_Logout(t *testing.T) {
	m := newTestManager(t)
	_ = m.secrets.Store().Set("openai", "main", "k")
	st, err := m.Status("openai", "main")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !st.Authenticated {
		t.Fatal("expected Authenticated=true")
	}
	if err := m.Logout("openai", "main"); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	st, _ = m.Status("openai", "main")
	if st.Authenticated {
		t.Fatal("expected Authenticated=false after Logout")
	}
}

func TestManager_ProviderAccountUniqueness(t *testing.T) {
	m := newTestManager(t)
	_ = m.secrets.Store().Set("openai", "main", "k1")
	_ = m.secrets.Store().Set("openai", "alt", "k2")
	_ = m.secrets.Store().Set("anthropic", "main", "k3")

	seen := map[string]bool{}
	list, _ := m.ListAccounts("openai")
	for _, a := range list {
		key := "openai" + "/" + a
		if seen[key] {
			t.Fatalf("duplicate %s", key)
		}
		seen[key] = true
	}
	if len(list) != 2 {
		t.Fatalf("ListAccounts(openai) = %v, want 2", list)
	}
}

// TestSafeDurationFromSeconds verifies the overflow guard for OAuth
// expires_in / interval values. Untrusted provider responses can carry
// arbitrary int values; without the guard, multiplying by time.Second (1e9)
// overflows int64 nanoseconds and produces negative or wrapped durations.
func TestSafeDurationFromSeconds(t *testing.T) {
	cases := []struct {
		name     string
		seconds  int
		want     time.Duration
		wantZero bool
	}{
		{name: "zero", seconds: 0, wantZero: true},
		{name: "negative", seconds: -1, wantZero: true},
		{name: "one hour", seconds: 3600, want: 3600 * time.Second},
		{name: "one day", seconds: 86400, want: 86400 * time.Second},
		{name: "ten years", seconds: 315_360_000, want: 315_360_000 * time.Second},
		{name: "max int32", seconds: math.MaxInt32,
			want: time.Duration(math.MaxInt32) * time.Second},
		// Overflow guard: this value would overflow int64 when multiplied by 1e9.
		{name: "overflow clamped", seconds: math.MaxInt64 / 1_000_000_000 + 1,
			want: time.Duration(math.MaxInt64)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := safeDurationFromSeconds(c.seconds)
			if c.wantZero {
				if got != 0 {
					t.Fatalf("safeDurationFromSeconds(%d) = %v, want 0", c.seconds, got)
				}
				return
			}
			if got != c.want {
				t.Fatalf("safeDurationFromSeconds(%d) = %v, want %v", c.seconds, got, c.want)
			}
		})
	}
}
