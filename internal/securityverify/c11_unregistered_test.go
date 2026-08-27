package securityverify

import (
	"context"
	"strings"
	"testing"

	"github.com/x6nux/yanshi/internal/ctxcompact"
	"github.com/x6nux/yanshi/internal/secrets"
)

// c11_unregistered_test.go is the third instance of the same measurement made
// in s6_unregistered_test.go and o10_unregistered_test.go, applied to the sink
// with the LONGEST-LIVED consequence.
//
// The existing C11 tests register their canary with the redactor before
// compacting. That correctly isolates the compaction plumbing — is the redactor
// consulted, is the copy made, are the pins left alone — and says nothing about
// which secrets the redactor recognises.
//
// Why this sink is the worst place for that gap: a secret that reaches the
// summary model is folded into summary text, and the summary becomes a PINNED
// message. It is then re-sent to the provider on every subsequent turn, for the
// remaining life of the session, long after the tool_result it came from was
// compacted away. The other two sinks leak once, to disk; this one leaks
// repeatedly, outbound.
//
// And the credential in question is precisely the kind nothing registers: a
// token a shell tool printed. redactForSummary's own doc names that case —
// "a shell tool that prints an API key puts that key into a tool_result".
// Nothing resolved that key, so nothing registered it.

// TestC11_UnregisteredSecretDoesNotReachTheSummaryModel drives the real
// compaction path with an EMPTY registry.
func TestC11_UnregisteredSecretDoesNotReachTheSummaryModel(t *testing.T) {
	// Never registered anywhere. This is a token the agent encountered, not one
	// yanshi resolved.
	const secret = "ghp_C11UNREG0123456789abcdefghijklmnop"

	red := secrets.NewRedactor() // empty registry, on purpose
	rec := &recordingSummarizer{}
	msgs := leakyHistory(secret)

	out, before, after, ok := ctxcompact.ForceCompactWithOptions(
		context.Background(), msgs, 4000, 4, rec, nil,
		ctxcompact.Options{Redactor: red},
	)
	if !ok {
		t.Fatalf("compaction did not run (tokens %d -> %d); nothing was measured", before, after)
	}
	if len(rec.seen) == 0 {
		t.Fatal("the summarizer was never called; nothing was measured")
	}
	t.Logf("summarizer invoked %d time(s); history %d -> %d messages, tokens %d -> %d",
		len(rec.seen), len(msgs), len(out), before, after)

	sent := rec.everythingSent()
	if strings.Contains(sent, secret) {
		t.Fatalf("LEAK: a credential nobody registered reached the summary model, and a " +
			"summary is pinned — it would be re-sent to the provider every turn from now on")
	}
	if !strings.Contains(sent, "[REDACTED]") {
		t.Error("no redaction marker in the summarizer's input — was the redactor consulted at all?")
	}
}

// TestC11_UnregisteredRedactionStillLeavesThePinsAlone repeats the property the
// registered case pins, for the pattern path.
//
// It is not implied by the registered version. Registered redaction and shape
// redaction are different code paths inside Redact, and the copy-versus-mutate
// discipline lives in redactForSummary, which could in principle be reached
// differently. More practically: this is the assertion that fails if somebody
// ever "fixes" a leak by redacting the caller's slice in place, which would
// silently rewrite history the model is mid-task on.
func TestC11_UnregisteredRedactionStillLeavesThePinsAlone(t *testing.T) {
	const secret = "ghp_C11UNREG0123456789abcdefghijklmnop"
	red := secrets.NewRedactor()
	rec := &recordingSummarizer{}
	msgs := leakyHistory(secret)

	before := make([]string, len(msgs))
	for i, m := range msgs {
		if m != nil {
			before[i] = m.Content
		}
	}

	if _, _, _, ok := ctxcompact.ForceCompactWithOptions(
		context.Background(), msgs, 4000, 4, rec, nil,
		ctxcompact.Options{Redactor: red},
	); !ok {
		t.Fatal("compaction did not run; nothing was measured")
	}

	for i, m := range msgs {
		if m != nil && m.Content != before[i] {
			t.Errorf("caller's message %d was rewritten in place:\n  was: %.80s\n  now: %.80s",
				i, before[i], m.Content)
		}
	}
	// And the secret really was in there to begin with, or the check above is
	// comparing two copies of an already-clean history.
	joined := strings.Join(before, "\n")
	if !strings.Contains(joined, secret) {
		t.Fatal("the fixture never contained the secret; this test would pass vacuously")
	}
}
