package secrets

import (
	"bytes"
	"strings"
	"testing"
)

// TestSafeOutputRegistryIsAnAliasNotACopy pins the invariant SafeOutput's own
// doc comment states: "The Redactor and Logger share the same secret registry:
// registering a secret on Redactor makes SafeLogger redact it on the next
// Printf/Println."
//
// The interesting case is not registering directly on out.Redactor -- that
// works by construction -- but registering AFTER the process-wide registry has
// absorbed a second redactor's contents, which is exactly what bootstrap does
// with the secrets Manager's redactor. If absorbing produces a NEW registry,
// every later Register lands on an object SafeLogger has never heard of and
// the logger silently emits plaintext API keys.
//
// This is a probe test in the sense of docs/superpowers/review-checklist.md:
// it asserts what the documentation claims, not what the code happens to do.
func TestSafeOutputRegistryIsAnAliasNotACopy(t *testing.T) {
	var buf bytes.Buffer
	out := NewSafeOutput(&buf, nil)

	// Absorb another redactor's registry, the way bootstrap folds the secrets
	// Manager's redactor into the process-wide one.
	other := NewRedactor()
	other.Register("mgr-secret-value")
	redactor := out.Redactor
	redactor.Absorb(other)

	if got := redactor.Redact("x mgr-secret-value y"); strings.Contains(got, "mgr-secret-value") {
		t.Fatalf("absorbed secret not redacted: %q", got)
	}

	// The load-bearing half: a secret registered on the process-wide Redactor
	// after the merge must reach the Logger. This is bootstrap.go's
	// `redactor.Register(resolved)` for every resolved provider API key.
	redactor.Register("sk-provider-key")
	out.Logger.Printf("resolved api key: %s\n", "sk-provider-key")

	if strings.Contains(buf.String(), "sk-provider-key") {
		t.Fatalf("SafeLogger leaked a secret registered on its paired Redactor: %q", buf.String())
	}
	if !strings.Contains(buf.String(), "[REDACTED]") {
		t.Fatalf("SafeLogger output missing [REDACTED]: %q", buf.String())
	}
}

// TestRegisterRejectsSubMinimumSecrets pins the MinSecretLength gate. Redact
// is substring replacement, and the Store redacts before the SQL write, so a
// one-character "secret" would rewrite stored conversations irreversibly. The
// gate is deliberately the drop-rather-than-corrupt direction; see the
// constant's doc.
func TestRegisterRejectsSubMinimumSecrets(t *testing.T) {
	r := NewRedactor()
	short := strings.Repeat("a", MinSecretLength-1)
	r.Register(short)
	line := "user asked about " + short + "rdvark migration"
	if got := r.Redact(line); got != line {
		t.Fatalf("sub-minimum secret redacted unrelated text: %q", got)
	}

	// Exactly at the bar is accepted -- the boundary is >=, not >.
	atBar := strings.Repeat("b", MinSecretLength)
	r.Register(atBar)
	if got := r.Redact("key=" + atBar); !strings.Contains(got, "[REDACTED]") {
		t.Fatalf("secret of exactly MinSecretLength was dropped: %q", got)
	}
}
