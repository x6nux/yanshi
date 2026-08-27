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

func TestPrepareEnvWithPolicy(t *testing.T) {
	cases := []struct {
		name        string
		policy      CredentialPolicy
		wantPresent []string
		wantAbsent  []string
		why         string
	}{
		{
			name:        "default strips every credential",
			policy:      CredentialPolicy{},
			wantPresent: []string{"PATH", "HOME", "LANG", "TERM", "HTTP_PROXY", "https_proxy"},
			wantAbsent:  []string{"OPENAI_API_KEY", "AWS_SECRET_ACCESS_KEY", "GH_TOKEN", "DATABASE_URL"},
			why:         "the zero policy is the secure default",
		},
		{
			name:        "allowlist admits exactly what it names",
			policy:      CredentialPolicy{AllowEnv: []string{"GH_TOKEN"}},
			wantPresent: []string{"PATH", "HOME", "GH_TOKEN", "HTTP_PROXY"},
			wantAbsent:  []string{"OPENAI_API_KEY", "AWS_SECRET_ACCESS_KEY", "DATABASE_URL"},
			why:         "gh needs its token and nothing else in the keyring",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := PrepareEnvWithPolicy(credEnvFixture(), "http://127.0.0.1:9000", tc.policy)
			for _, name := range tc.wantPresent {
				if !has(got, name) {
					t.Errorf("%s missing but must survive (%s)", name, tc.why)
				}
			}
			for _, name := range tc.wantAbsent {
				if has(got, name) {
					t.Errorf("%s present but must be scrubbed (%s)", name, tc.why)
				}
			}
		})
	}
}

// TestPrepareEnvWithPolicyStillManagesProxyVariables pins that adding the
// credential scrub did not cost the proxy behaviour PrepareEnv already had:
// inherited proxy variables are still stripped in every case and the managed
// ones still published in both.
func TestPrepareEnvWithPolicyStillManagesProxyVariables(t *testing.T) {
	in := []string{"PATH=/bin", "http_proxy=http://evil:9999", "HTTPS_PROXY=http://evil:9999"}
	got := PrepareEnvWithPolicy(in, "http://127.0.0.1:9000", CredentialPolicy{})
	joined := strings.Join(got, "\n")
	if strings.Contains(joined, "evil") {
		t.Fatalf("inherited proxy survived the policy path: %v", got)
	}
	for _, want := range []string{
		"HTTP_PROXY=http://127.0.0.1:9000", "HTTPS_PROXY=http://127.0.0.1:9000", "NO_PROXY=",
		"http_proxy=http://127.0.0.1:9000", "https_proxy=http://127.0.0.1:9000", "no_proxy=",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("managed env missing %q", want)
		}
	}
}

// TestPrepareEnvWithPolicyScrubsBeforePublishingProxy pins the ordering
// documented on PrepareEnvWithPolicy. A proxy URL carrying inline basic-auth
// credentials is a shape LooksLikeCredentialValue recognises, so a scrub that
// ran AFTER the managed variables were appended would strip the very variables
// the child needs to reach the proxy.
func TestPrepareEnvWithPolicyScrubsBeforePublishingProxy(t *testing.T) {
	const proxy = "http://proxyuser:proxypass@127.0.0.1:9000"
	got := PrepareEnvWithPolicy([]string{"PATH=/bin"}, proxy, CredentialPolicy{})
	for _, name := range []string{"HTTP_PROXY", "https_proxy"} {
		if !has(got, name) {
			t.Fatalf("%s was scrubbed; the credential pass must run before the proxy is published: %v", name, got)
		}
	}
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
			// ScrubCredentials must not touch proxy variables; PrepareEnv owns
			// those. A caller building its own env relies on getting back what
			// it passed minus the credentials, nothing added.
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

// TestManagedEnvWithPolicyStripsHostCredentials proves the os.Environ() path —
// the one every child-process launcher actually uses — applies the scrub.
// PrepareEnvWithPolicy could be correct while this wrapper forgot to call it,
// and this wrapper is the only one a launcher reaches for.
func TestManagedEnvWithPolicyStripsHostCredentials(t *testing.T) {
	t.Setenv("YANSHI_TEST_OPENAI_API_KEY", "sk-"+strings.Repeat("x", 40))
	t.Setenv("YANSHI_TEST_KEEP", "plain-value")

	got := ManagedEnvWithPolicy("http://127.0.0.1:9000", CredentialPolicy{})
	if has(got, "YANSHI_TEST_OPENAI_API_KEY") {
		t.Error("a host credential reached the child environment")
	}
	if !has(got, "YANSHI_TEST_KEEP") {
		t.Error("an ordinary host variable was removed")
	}
	if !has(got, "PATH") {
		t.Error("PATH was removed; the child could not resolve any program")
	}

	allowed := ManagedEnvWithPolicy("http://127.0.0.1:9000",
		CredentialPolicy{AllowEnv: []string{"YANSHI_TEST_OPENAI_API_KEY"}})
	if !has(allowed, "YANSHI_TEST_OPENAI_API_KEY") {
		t.Error("the allowlist did not admit the named host credential")
	}
}

// TestManagedEnvDoesNotScrub pins the documented difference between the two
// wrappers. ManagedEnv is retained for callers spawning trusted helpers, and
// its doc says so; if it silently started scrubbing, that doc would be wrong
// and callers would have no unscrubbed option left.
func TestManagedEnvDoesNotScrub(t *testing.T) {
	t.Setenv("YANSHI_TEST_ANTHROPIC_API_KEY", "sk-ant-"+strings.Repeat("y", 40))
	got := ManagedEnv("http://127.0.0.1:9000")
	if !has(got, "YANSHI_TEST_ANTHROPIC_API_KEY") {
		t.Fatal("ManagedEnv scrubbed a credential; use ManagedEnvWithPolicy for that " +
			"and update ManagedEnv's doc comment, which promises the opposite")
	}
}
