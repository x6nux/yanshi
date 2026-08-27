package secrets

import (
	"strings"
	"testing"
)

func TestIsCredentialEnvName(t *testing.T) {
	cases := []struct {
		name string
		env  string
		want bool
		why  string
	}{
		// Suffix families named in the work package.
		{"openai api key", "OPENAI_API_KEY", true, "*_API_KEY family"},
		{"vendor api key", "MYVENDOR_API_KEY", true, "*_API_KEY on an unknown vendor"},
		{"generic token", "CIRCLECI_TOKEN", true, "*_TOKEN family"},
		{"generic secret", "APP_SECRET", true, "*_SECRET family"},
		{"generic password", "DB_PASSWORD", true, "*_PASSWORD family"},
		{"generic credential", "SERVICE_CREDENTIAL", true, "*_CREDENTIAL family"},
		{"private key", "DEPLOY_PRIVATE_KEY", true, "*_PRIVATE_KEY family"},
		{"run-together token", "GITLABTOKEN", true, "suffix rule covers unseparated words"},
		{"run-together apikey", "MYAPIKEY", true, "suffix rule covers unseparated words"},

		// Vendor namespaces.
		{"aws namespace", "AWS_SECRET_ACCESS_KEY", true, "AWS_* namespace"},
		{"aws session", "AWS_SESSION_TOKEN", true, "AWS_* namespace covers the whole space"},
		{"aws profile", "AWS_PROFILE", true, "AWS_* is namespace-wide by design"},
		{"anthropic", "ANTHROPIC_API_KEY", true, "ANTHROPIC_* namespace"},
		{"openai org", "OPENAI_ORG_ID", true, "OPENAI_* is namespace-wide by design"},
		{"gh token", "GH_TOKEN", true, "GH_* namespace"},
		{"github token", "GITHUB_TOKEN", true, "GITHUB_* namespace"},

		// Exact names no shape rule would reach.
		{"kubeconfig", "KUBECONFIG", true, "exact-name table"},
		{"netrc", "NETRC", true, "exact-name table"},
		{"gcloud adc", "GOOGLE_APPLICATION_CREDENTIALS", true, "exact-name table"},

		// Case and separator normalisation.
		{"lowercase", "openai_api_key", true, "matching is case-insensitive"},
		{"dashed", "X-API-KEY", true, "'-' folds to '_'"},
		{"mixed case", "Gh_Token", true, "matching is case-insensitive"},

		// Structural variables: removing these breaks the child outright.
		{"path", "PATH", false, "structural"},
		{"home", "HOME", false, "structural"},
		{"lang", "LANG", false, "structural"},
		{"term", "TERM", false, "structural"},
		{"pwd", "PWD", false, "structural, and 'PWD' is not the PASSWD word"},
		{"gomodcache", "GOMODCACHE", false, "structural"},
		{"userprofile", "USERPROFILE", false, "structural on Windows"},

		// Ordinary variables that merely contain a credential-ish substring.
		{"keyboard", "KEYBOARD_LAYOUT", false, "word match, not substring: KEYBOARD is not KEY"},
		{"monkey", "MONKEY_MODE", false, "substring 'KEY' must not fire"},
		{"editor", "EDITOR", false, "ordinary"},
		{"ci", "CI", false, "ordinary"},
		{"node env", "NODE_ENV", false, "ordinary"},
		{"log level", "LOG_LEVEL", false, "ordinary"},
		{"empty", "", false, "no name is not a credential"},

		// Words that ARE separate components.
		{"key alone", "SIGNING_KEY", true, "KEY as its own word"},
		{"auth", "SSH_AUTH_SOCK", true, "AUTH: the agent socket signs with every held key"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsCredentialEnvName(tc.env); got != tc.want {
				t.Fatalf("IsCredentialEnvName(%q) = %v, want %v (%s)", tc.env, got, tc.want, tc.why)
			}
		})
	}
}

func TestLooksLikeCredentialValue(t *testing.T) {
	cases := []struct {
		name  string
		value string
		want  bool
		why   string
	}{
		{"aws access key", "AKIAIOSFODNN7EXAMPLE", true, "AWS key id shape"},
		{"aws temporary", "ASIAIOSFODNN7EXAMPLE", true, "AWS temporary key id shape"},
		{"openai style", "sk-abcdefghijklmnopqrstuvwxyz0123456789", true, "sk- prefix with vendor length"},
		{"stripe live", "sk_live_abcdefghij0123456789", true, "Stripe key shape"},
		{"google api", "AIza" + strings.Repeat("a", 35), true, "Google API key shape"},
		{"github classic", "ghp_" + strings.Repeat("A", 36), true, "GitHub classic token shape"},
		{"github fine grained", "github_pat_" + strings.Repeat("a", 22), true, "GitHub fine-grained PAT"},
		{"gitlab pat", "glpat-abcdefghij0123456789", true, "GitLab PAT shape"},
		{"slack bot", "xoxb-1234567890-abcdefghij", true, "Slack token shape"},
		{"jwt", "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.c2lnbmF0dXJl", true, "JWT shape"},
		{"bearer header", "Bearer abcdefghijklmnopqrst", true, "Authorization header value"},
		{"postgres dsn", "postgres://appuser:hunter2@db.internal:5432/app", true, "inline password in a DSN"},
		{"mongodb srv", "mongodb+srv://u:p4ssw0rd@cluster0.example.net/db", true, "inline password in a DSN"},
		{"pem block", "-----BEGIN RSA PRIVATE KEY-----\nMIIEow==\n-----END RSA PRIVATE KEY-----", true, "PEM private key"},

		// The anchoring contract: an unanchored search would strip PATH.
		{"path with sk dir", "/usr/bin:/opt/sk-tools/bin:/bin", false, "'sk-' appears mid-value; patterns are anchored"},
		{"path with akia dir", "/home/u/AKIAIOSFODNN7EXAMPLE/bin:/bin", false, "AWS shape embedded in a longer value"},
		{"home", "/Users/operator", false, "ordinary path"},
		{"lang", "en_US.UTF-8", false, "ordinary locale"},
		{"pem mention", "-----BEGIN CERTIFICATE-----", false, "a certificate is public; only PRIVATE KEY blocks count"},
		{"short sk", "sk-abc", false, "too short to be a minted key"},
		{"dsn without password", "postgres://db.internal:5432/app", false, "no inline credential"},
		{"empty", "", false, "an unset variable carries nothing to leak"},
		{"blank", "   ", false, "whitespace carries nothing to leak"},
		{"word bearer", "bearer", false, "the bare word is not a header value"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := LooksLikeCredentialValue(tc.value); got != tc.want {
				t.Fatalf("LooksLikeCredentialValue(%q) = %v, want %v (%s)", tc.value, got, tc.want, tc.why)
			}
		})
	}
}

func TestSplitEnvEntry(t *testing.T) {
	cases := []struct {
		entry     string
		wantName  string
		wantValue string
		wantOK    bool
	}{
		{"PATH=/bin", "PATH", "/bin", true},
		{"EMPTY=", "EMPTY", "", true},
		{"URL=http://x/?a=b", "URL", "http://x/?a=b", true},
		{"=leading", "", "leading", true},
		{"MALFORMED", "MALFORMED", "", false},
		{"", "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.entry, func(t *testing.T) {
			name, value, ok := SplitEnvEntry(tc.entry)
			if name != tc.wantName || value != tc.wantValue || ok != tc.wantOK {
				t.Fatalf("SplitEnvEntry(%q) = (%q, %q, %v), want (%q, %q, %v)",
					tc.entry, name, value, ok, tc.wantName, tc.wantValue, tc.wantOK)
			}
		})
	}
}

// envNames extracts the variable names from an environment slice, so
// assertions read as sets of names rather than of whole entries.
func envNames(env []string) map[string]string {
	out := make(map[string]string, len(env))
	for _, entry := range env {
		name, value, ok := SplitEnvEntry(entry)
		if !ok {
			continue
		}
		out[name] = value
	}
	return out
}

func TestScrubEnv(t *testing.T) {
	// One environment covering every branch, scrubbed under several policies.
	base := []string{
		"PATH=/usr/bin:/bin",
		"HOME=/home/operator",
		"LANG=en_US.UTF-8",
		"TERM=xterm-256color",
		"GOMODCACHE=/home/operator/go/pkg/mod",
		"EDITOR=vim",
		"OPENAI_API_KEY=sk-" + strings.Repeat("x", 40),
		"ANTHROPIC_API_KEY=sk-ant-" + strings.Repeat("y", 40),
		"AWS_SECRET_ACCESS_KEY=" + strings.Repeat("z", 40),
		"GH_TOKEN=ghp_" + strings.Repeat("A", 36),
		"GITHUB_TOKEN=ghp_" + strings.Repeat("B", 36),
		"DATABASE_URL=postgres://appuser:hunter2@db.internal:5432/app",
		"MY_SETTING=AKIAIOSFODNN7EXAMPLE",
		"MALFORMED_ENTRY",
	}

	cases := []struct {
		name        string
		policy      EnvScrubPolicy
		wantKept    []string
		wantDropped []string
		why         string
	}{
		{
			name:   "default strips every credential",
			policy: EnvScrubPolicy{},
			wantKept: []string{
				"PATH", "HOME", "LANG", "TERM", "GOMODCACHE", "EDITOR",
			},
			wantDropped: []string{
				"ANTHROPIC_API_KEY", "AWS_SECRET_ACCESS_KEY", "DATABASE_URL",
				"GH_TOKEN", "GITHUB_TOKEN", "MY_SETTING", "OPENAI_API_KEY",
			},
			why: "the zero value is the secure default",
		},
		{
			name:   "allowlist lets the named variables through",
			policy: EnvScrubPolicy{Allow: []string{"GH_TOKEN", "GITHUB_TOKEN"}},
			wantKept: []string{
				"PATH", "HOME", "LANG", "TERM", "GOMODCACHE", "EDITOR",
				"GH_TOKEN", "GITHUB_TOKEN",
			},
			wantDropped: []string{
				"ANTHROPIC_API_KEY", "AWS_SECRET_ACCESS_KEY", "DATABASE_URL",
				"MY_SETTING", "OPENAI_API_KEY",
			},
			why: "the gh escape hatch admits exactly what it names",
		},
		{
			name:   "allowlist is case-insensitive and folds dashes",
			policy: EnvScrubPolicy{Allow: []string{"gh-token"}},
			wantKept: []string{
				"PATH", "HOME", "LANG", "TERM", "GOMODCACHE", "EDITOR", "GH_TOKEN",
			},
			wantDropped: []string{
				"ANTHROPIC_API_KEY", "AWS_SECRET_ACCESS_KEY", "DATABASE_URL",
				"GITHUB_TOKEN", "MY_SETTING", "OPENAI_API_KEY",
			},
			why: "callers must not have to guess the host's spelling",
		},
		{
			name:   "allowlist covers value-detected variables too",
			policy: EnvScrubPolicy{Allow: []string{"DATABASE_URL"}},
			wantKept: []string{
				"PATH", "HOME", "LANG", "TERM", "GOMODCACHE", "EDITOR", "DATABASE_URL",
			},
			wantDropped: []string{
				"ANTHROPIC_API_KEY", "AWS_SECRET_ACCESS_KEY", "GH_TOKEN",
				"GITHUB_TOKEN", "MY_SETTING", "OPENAI_API_KEY",
			},
			why: "an app that genuinely needs its DSN can say so",
		},
		{
			name:   "blank allow entries are ignored",
			policy: EnvScrubPolicy{Allow: []string{"", "   ", "GH_TOKEN"}},
			wantKept: []string{
				"PATH", "HOME", "LANG", "TERM", "GOMODCACHE", "EDITOR", "GH_TOKEN",
			},
			wantDropped: []string{
				"ANTHROPIC_API_KEY", "AWS_SECRET_ACCESS_KEY", "DATABASE_URL",
				"GITHUB_TOKEN", "MY_SETTING", "OPENAI_API_KEY",
			},
			why: "an empty string must not become an allow-anything rule",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := ScrubEnv(base, tc.policy)
			kept := envNames(res.Env)
			for _, name := range tc.wantKept {
				if _, ok := kept[name]; !ok {
					t.Errorf("%s was removed but must survive (%s)", name, tc.why)
				}
			}
			for _, name := range tc.wantDropped {
				if _, ok := kept[name]; ok {
					t.Errorf("%s survived but must be scrubbed (%s)", name, tc.why)
				}
			}
			if got := strings.Join(res.DroppedNames, ","); got != strings.Join(tc.wantDropped, ",") {
				t.Errorf("DroppedNames = %v, want %v", res.DroppedNames, tc.wantDropped)
			}
			// A malformed entry carries no value and must pass through.
			var sawMalformed bool
			for _, entry := range res.Env {
				if entry == "MALFORMED_ENTRY" {
					sawMalformed = true
				}
			}
			if !sawMalformed {
				t.Error("an entry with no '=' must pass through untouched")
			}
		})
	}
}

// TestScrubEnvKeepsStructuralVariablesWithCredentialShapedValues pins the
// half of the trade-off documented on structuralEnvNames. A PATH whose value
// happens to satisfy a vendor regex must survive: the child that loses PATH
// cannot resolve any program at all, and the operator sees "command not found"
// with nothing pointing at the scrub.
func TestScrubEnvKeepsStructuralVariablesWithCredentialShapedValues(t *testing.T) {
	env := []string{
		"PATH=AKIAIOSFODNN7EXAMPLE",
		"HOME=sk-" + strings.Repeat("q", 40),
		"TERM=ghp_" + strings.Repeat("C", 36),
	}
	res := ScrubEnv(env, EnvScrubPolicy{})
	if len(res.DroppedNames) != 0 {
		t.Fatalf("structural variables were dropped: %v", res.DroppedNames)
	}
	if len(res.Env) != len(env) {
		t.Fatalf("env length changed: got %d, want %d", len(res.Env), len(env))
	}
}

// TestScrubEnvDoesNotMutateInput guards the aliasing hazard: callers layer
// caller-supplied entries onto a host baseline with append, so a scrub that
// wrote through its input would corrupt a slice the caller still holds.
func TestScrubEnvDoesNotMutateInput(t *testing.T) {
	env := []string{"PATH=/bin", "OPENAI_API_KEY=sk-" + strings.Repeat("x", 40), "HOME=/root"}
	before := make([]string, len(env))
	copy(before, env)
	res := ScrubEnv(env, EnvScrubPolicy{})
	for i := range env {
		if env[i] != before[i] {
			t.Fatalf("input mutated at %d: got %q, want %q", i, env[i], before[i])
		}
	}
	if len(res.Env) != 2 {
		t.Fatalf("expected 2 surviving entries, got %v", res.Env)
	}
}

// TestScrubEnvEmptyValuedCredentialIsStillDropped covers the seam between the
// two detection halves: an exported-but-empty GH_TOKEN has no value to match,
// so only the NAME rule can catch it. Leaving it in place would let a child
// distinguish "yanshi holds a GitHub token" from "it does not", and is the
// case value-only detection is documented to miss.
func TestScrubEnvEmptyValuedCredentialIsStillDropped(t *testing.T) {
	res := ScrubEnv([]string{"PATH=/bin", "GH_TOKEN="}, EnvScrubPolicy{})
	if len(res.DroppedNames) != 1 || res.DroppedNames[0] != "GH_TOKEN" {
		t.Fatalf("DroppedNames = %v, want [GH_TOKEN]", res.DroppedNames)
	}
}

// TestScrubEnvNilAndEmpty pins the degenerate inputs: both must return an
// empty result rather than nil-panicking, because callers pass the caller's
// spec.Env straight through and almost none of them populate it.
func TestScrubEnvNilAndEmpty(t *testing.T) {
	for _, env := range [][]string{nil, {}} {
		res := ScrubEnv(env, EnvScrubPolicy{})
		if len(res.Env) != 0 || len(res.DroppedNames) != 0 {
			t.Fatalf("ScrubEnv(%v) = %+v, want empty", env, res)
		}
	}
}
