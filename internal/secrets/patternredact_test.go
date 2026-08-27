package secrets

import (
	"strings"
	"testing"
)

// patternredact_test.go pins the two properties patternredact.go's doc comment
// asserts but cannot enforce by construction.

// patternSamples pairs each credential family with a token that must match. It
// is a flat list rather than an index into credentialPatterns because the point
// is coverage of SHAPES, and one regex (the sk- one) deliberately serves
// several vendors.
var patternSamples = []string{
	"sk-A1b2C3d4E5f6G7h8I9j0K1l2",
	"sk-proj-A1b2C3d4E5f6G7h8I9j0K1l2M3n4O5p6Q7r8S9t0",
	"sk-ant-api03-aBcDeFgHiJkLmNoPqRsTuVwXyZ0123456789",
	"ghp_1234567890abcdefghijklmnopqrstuvwxyz",
	"gho_1234567890abcdefghijklmnopqrstuvwxyz",
	"github_pat_11ABCDEFG0abcdefghijklmnopqrstuvwxyz012345",
	"glpat-xxxxxxxxxxxxxxxxxxxx",
	"xoxb-EXAMPLE-NOT-A-REAL-SLACK-TOKEN",
	"AKIAIOSFODNN7EXAMPLE",
	"ASIAIOSFODNN7EXAMPLE",
	"AIzaSyA1b2C3d4E5f6G7h8I9j0K1l2M3n4O5p6Q",
	"sk_live_NOTAREALSTRIPEKEY",
	"rk_test_51AbCdEfGhIjKlMnOpQrStUvWx",
	"npm_abcdefghijklmnopqrstuvwxyz0123456789",
	"eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dBjftJeZ4CVPmB92K27uhbUJU1p1r_wW1gFWFOEjXk",
	"-----BEGIN RSA PRIVATE KEY-----\nMIIEowIBAAKCAQEA\n-----END RSA PRIVATE KEY-----",
}

// TestEveryPatternSampleIsRedacted is the forward property: each shape in the
// table is actually caught.
func TestEveryPatternSampleIsRedacted(t *testing.T) {
	for _, s := range patternSamples {
		in := "before " + s + " after"
		out := RedactPatterns(in)
		if strings.Contains(out, s) {
			t.Errorf("not redacted: %q -> %q", s, out)
		}
		if !strings.Contains(out, "before ") || !strings.Contains(out, " after") {
			t.Errorf("surrounding text lost for %q: %q", s, out)
		}
	}
}

// TestPatternPrefixesCoverEveryPattern is the check patternPrefixes' own doc
// comment promises.
//
// mayContainCredentialPattern is a fast-path guard: if it returns false the
// regexes never run. A prefix missing from that list therefore does not make
// the corresponding pattern slower, it makes it DEAD — the pattern would sit in
// the file, look like coverage, and never execute. That is the single most
// likely way this file silently stops working, and it is invisible to the test
// above only because that test happens to route through the same guard.
//
// So the correspondence is checked directly: every sample must pass the probe.
func TestPatternPrefixesCoverEveryPattern(t *testing.T) {
	for _, s := range patternSamples {
		if !mayContainCredentialPattern(s) {
			t.Errorf("no entry in patternPrefixes matches %q, so its regex can never run", s)
		}
	}
}

// TestInlineURLCredentialKeepsTheHost pins the one pattern that deliberately
// preserves part of its match. A log line that named neither the host nor the
// user would record only that a connection string existed.
func TestInlineURLCredentialKeepsTheHost(t *testing.T) {
	got := RedactPatterns("postgres://appuser:hunter2SuperSecret@db.internal:5432/app")
	if strings.Contains(got, "hunter2SuperSecret") {
		t.Fatalf("password survived: %q", got)
	}
	for _, keep := range []string{"postgres://", "appuser", "db.internal", "5432", "/app"} {
		if !strings.Contains(got, keep) {
			t.Errorf("redaction removed %q, which is diagnostic rather than secret: %q", keep, got)
		}
	}
}

// TestRedactPatternsIsIdempotent matters because these sinks compose: a value
// can pass through Redact on the way into a message log and again on the way
// out to a wire frame. A second pass must not corrupt the marker left by the
// first.
func TestRedactPatternsIsIdempotent(t *testing.T) {
	for _, s := range patternSamples {
		once := RedactPatterns("x " + s + " y")
		if twice := RedactPatterns(once); twice != once {
			t.Errorf("not idempotent for %q:\n  once:  %q\n  twice: %q", s, once, twice)
		}
	}
}

// TestRegisteredSecretsStillWin checks the two mechanisms compose in the order
// the Redact doc states: a registered secret is replaced whole, even when a
// pattern covers only part of it.
func TestRegisteredSecretsStillWin(t *testing.T) {
	r := NewRedactor()
	const custom = "corp-internal-token-not-a-known-vendor-shape"
	r.Register(custom)
	out := r.Redact("A " + custom + " B sk-proj-A1b2C3d4E5f6G7h8I9j0K1l2M3n4 C")
	if strings.Contains(out, custom) {
		t.Errorf("registered secret survived: %q", out)
	}
	if strings.Contains(out, "sk-proj-A1b2C3d4E5f6G7h8I9j0K1l2M3n4") {
		t.Errorf("pattern-shaped token survived alongside a registered one: %q", out)
	}
}
