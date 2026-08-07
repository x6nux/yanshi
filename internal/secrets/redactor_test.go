package secrets

import (
	"errors"
	"strings"
	"testing"
)

func TestRedactor_ReplacesAllRegisteredSecrets(t *testing.T) {
	r := NewRedactor()
	r.Register("sk-live-1234567890abcdef")
	r.Register("Bearer abcdef0123456789")

	got := r.Redact("error: api call with sk-live-1234567890abcdef failed; header Bearer abcdef0123456789")
	if strings.Contains(got, "sk-live-") || strings.Contains(got, "abcdef0123456789") {
		t.Fatalf("redactor leaked secret: %q", got)
	}
	if !strings.Contains(got, "[REDACTED]") {
		t.Fatalf("expected [REDACTED] marker, got %q", got)
	}
}

func TestSafeError_PreservesUnwrapChain(t *testing.T) {
	sentinel := errors.New("raw cause with sk-live-1234567890abcdef")
	r := NewRedactor()
	r.Register("sk-live-1234567890abcdef")
	safe := r.SafeError(sentinel)
	if !errors.Is(safe, sentinel) {
		t.Fatalf("errors.Is broken: SafeError must Unwrap to raw cause")
	}
	// Risk: calling Error() on the unwrapped err still leaks. Callers must not
	// re-stringify the cause; only the boundary-rendered text is safe.
	if !strings.Contains(safe.Error(), "[REDACTED]") {
		t.Fatalf("SafeError.Error must be redacted, got %q", safe.Error())
	}
}

func TestSafeLogger_RedactsFormattedSentinel(t *testing.T) {
	const sentinel = "sk-stderr-sentinel-123456"
	var out strings.Builder
	r := NewRedactor()
	r.Register(sentinel)
	log := NewSafeLogger(&out, r)
	log.Printf("device auth failed: %v\n", errors.New("provider echoed "+sentinel))
	if strings.Contains(out.String(), sentinel) {
		t.Fatalf("SafeLogger leaked sentinel: %q", out.String())
	}
	if !strings.Contains(out.String(), "[REDACTED]") {
		t.Fatalf("SafeLogger omitted redaction marker: %q", out.String())
	}
}
