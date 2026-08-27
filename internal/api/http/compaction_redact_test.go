// internal/api/http/compaction_redact_test.go
//
// Wiring tests for C11 on the transport side: the process-wide secrets
// redactor must actually REACH ctxcompact, and must not reach it as a
// typed-nil interface.
//
// These exist because "the feature works when called" and "the feature is
// called" are different claims, and only the first one had a test. The C11
// implementation shipped with a full unit suite while all three production
// call sites still used the option-less entry points, so nothing redacted
// anything. That is this repo's most-repeated defect shape (written but zero
// readers), and a test that pins the call site is the only thing that catches
// it -- the unit tests stay green either way.

package http

import (
	"testing"

	"github.com/x6nux/yanshi/internal/secrets"
)

// TestCompactionOptionsCarriesTheRedactor proves the server hands its redactor
// to ctxcompact. Un-wiring compactionOptions (returning a zero Options) makes
// this fail; before this test existed, that un-wiring was the shipped state.
func TestCompactionOptionsCarriesTheRedactor(t *testing.T) {
	red := secrets.NewRedactor()
	red.Register("sk-live-must-not-leak")
	s := &Server{redactor: red}

	opts := s.compactionOptions()
	if opts.Redactor == nil {
		t.Fatal("compactionOptions dropped the redactor: compaction would ship " +
			"registered secrets to the summary model, and the summary is pinned")
	}
	got := opts.Redactor.Redact("token=sk-live-must-not-leak done")
	if got != "token=[REDACTED] done" {
		t.Fatalf("the wired redactor is not the registered one: got %q", got)
	}
}

// TestCompactionOptionsWithoutRedactorIsUsable pins the nil case as a genuine
// nil interface rather than a non-nil interface wrapping a nil pointer.
//
// This is the failure the nil check in compactionOptions exists to prevent, and
// it is not hypothetical: s.redactor is nil whenever no secrets backend is
// configured, (*secrets.Redactor).Redact takes an RLock on a field of its
// receiver, and ctxcompact skips redaction only when the interface itself is
// nil. Assigning the typed nil straight through therefore panics on the
// pre-turn path of every chat request on such a deployment. Calling Redact
// here is the point of the test: a typed-nil would panic on this line.
func TestCompactionOptionsWithoutRedactorIsUsable(t *testing.T) {
	s := &Server{}

	opts := s.compactionOptions()
	if opts.Redactor != nil {
		t.Fatalf("a nil *secrets.Redactor must not become a non-nil interface; "+
			"ctxcompact's own nil guard cannot see through it, got %#v", opts.Redactor)
	}
	if opts.Redactor != nil {
		_ = opts.Redactor.Redact("x")
	}
}
