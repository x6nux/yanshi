package netpolicy

import (
	"strings"
	"testing"
)

// credEnvFixture is the host environment used across these tests: structural
// variables the child cannot start without, plus one credential of each shape
// the scrub recognises.
func credEnvFixture() []string {
	return []string{
		"PATH=/usr/bin:/bin",
		"HOME=/home/operator",
		"LANG=en_US.UTF-8",
		"TERM=xterm-256color",
		"OPENAI_API_KEY=sk-" + strings.Repeat("x", 40),
		"AWS_SECRET_ACCESS_KEY=" + strings.Repeat("z", 40),
		"GH_TOKEN=ghp_" + strings.Repeat("A", 36),
		"DATABASE_URL=postgres://appuser:hunter2@db.internal:5432/app",
	}
}

// has reports whether env contains an entry with the given name.
func has(env []string, name string) bool {
	for _, entry := range env {
		if strings.HasPrefix(entry, name+"=") {
			return true
		}
	}
	return false
}

func TestScrubCredentials(t *testing.T) {
	cases := []struct {
		name        string
		policy      CredentialPolicy
		wantDropped []string
	}{
		{"default", CredentialPolicy{}, []string{"AWS_SECRET_ACCESS_KEY", "DATABASE_URL", "GH_TOKEN", "OPENAI_API_KEY"}},
		{"gh allowed", CredentialPolicy{AllowEnv: []string{"GH_TOKEN"}}, []string{"AWS_SECRET_ACCESS_KEY", "DATABASE_URL", "OPENAI_API_KEY"}},
		{"all allowed", CredentialPolicy{AllowEnv: []string{
			"GH_TOKEN", "OPENAI_API_KEY", "AWS_SECRET_ACCESS_KEY", "DATABASE_URL",
		}}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			kept, dropped := ScrubCredentials(credEnvFixture(), tc.policy)
			if strings.Join(dropped, ",") != strings.Join(tc.wantDropped, ",") {
				t.Fatalf("dropped = %v, want %v", dropped, tc.wantDropped)
			}
			// ScrubCredentials must not touch proxy variables; PrepareEnvFor
			// owns those. A caller building its own env relies on getting back
			// what it passed minus the credentials, nothing added.
			if has(kept, "HTTP_PROXY") {
				t.Errorf("ScrubCredentials published a proxy variable: %v", kept)
			}
			for _, name := range []string{"PATH", "HOME", "LANG", "TERM"} {
				if !has(kept, name) {
					t.Errorf("structural variable %s was removed", name)
				}
			}
		})
	}
}
