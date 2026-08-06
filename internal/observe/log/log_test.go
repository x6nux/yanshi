package log

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

func TestHandlerRedactsDirectBoundNestedAndErrorAttrs(t *testing.T) {
	var buf bytes.Buffer
	logger := New(Config{Writer: &buf, Format: "json", Level: "debug"})
	logger = logger.With("api_key", "sk-ant-bound")
	logger.Info("request",
		"prompt", "private prompt",
		slog.Group("request", "tool_args", `{"path":"C:/secret"}`),
		"err", errors.New("Bearer hidden-token"),
	)
	got := buf.String()
	for _, secret := range []string{"sk-ant-bound", "private prompt", "C:/secret", "hidden-token"} {
		if strings.Contains(got, secret) {
			t.Fatalf("log leaked %q: %s", secret, got)
		}
	}
	if strings.Count(got, Redacted) < 4 {
		t.Fatalf("expected every sensitive value to be redacted: %s", got)
	}
}

// TestHandlerAddsCorrelationIDsFromContext.
//
// ledger: C4/OBS1#1 关键路径结构化日志
func TestHandlerAddsCorrelationIDsFromContext(t *testing.T) {
	var buf bytes.Buffer
	logger := New(Config{Writer: &buf, Format: "json", Level: "info"})
	ctx := WithIDs(context.Background(), IDs{
		TraceID:   "0123456789abcdef0123456789abcdef",
		SessionID: "session-1",
		TurnID:    "turn-2",
		Tool:      "fs_read",
	})
	logger.InfoContext(ctx, "correlated")
	got := buf.String()
	for _, want := range []string{`"trace_id":"0123456789abcdef0123456789abcdef"`, `"session_id":"session-1"`, `"turn_id":"turn-2"`, `"tool":"fs_read"`} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %s in %s", want, got)
		}
	}
}

// TestParseLevelAndSafeErrorType.
//
// ledger: C4/OBS1#3 级别可配
func TestParseLevelAndSafeErrorType(t *testing.T) {
	if got := ParseLevel("warn"); got != slog.LevelWarn {
		t.Fatalf("ParseLevel(warn) = %v", got)
	}
	if got := SafeErrorType(errors.New("api_key=secret")); got != "*errors.errorString" {
		t.Fatalf("SafeErrorType = %q", got)
	}
	if got := SafeErrorType(nil); got != "" {
		t.Fatalf("SafeErrorType(nil) = %q", got)
	}
}

// TestSuppressedIDsCannotBeReintroduced covers the defect a "just don't call
// WithIDs" gate could not: the caller downstream calls WithIDs anyway.
//
// internal/agent/orchestrator::ensureTurnIDs fills in a fresh trace and turn id
// for any turn that arrives without them -- it exists so the goal loop, ACP and
// headless entry points still emit correlated logs. That makes "the transport
// declined to bind ids" indistinguishable from "nobody has bound them yet", and
// the flag that was supposed to turn correlation OFF instead produced ids with
// no session.id: lines that look joinable and are not.
func TestSuppressedIDsCannotBeReintroduced(t *testing.T) {
	ctx := WithoutIDs(context.Background())
	if !IDsSuppressed(ctx) {
		t.Fatal("WithoutIDs did not take")
	}

	// The exact shape ensureTurnIDs uses.
	ctx = WithIDs(ctx, IDs{TraceID: "aaaa", TurnID: "bbbb"})
	if got := IDsFromContext(ctx); got.TraceID != "" || got.TurnID != "" {
		t.Fatalf("ids came back after suppression: %+v", got)
	}

	// And through a derived context, which is how it actually reaches the
	// orchestrator: several layers of WithValue sit in between.
	derived, cancel := context.WithCancel(ctx)
	defer cancel()
	derived = WithIDs(derived, IDs{SessionID: "sess", TraceID: "cccc"})
	if got := IDsFromContext(derived); got != (IDs{}) {
		t.Fatalf("suppression did not survive derivation: %+v", got)
	}

	// A context that was never suppressed is unaffected.
	plain := WithIDs(context.Background(), IDs{TraceID: "dddd"})
	if IDsFromContext(plain).TraceID != "dddd" {
		t.Fatal("suppression leaked into an unrelated context")
	}
}

// TestSanitizeIDBoundsWhatAClientCanPutInALogLine covers the one identifier
// in the system a caller chooses.
//
// SSE reads thread_id / turn_id off the request body -- a stateless transport
// has no conversation identity of its own, so the client's id is the only
// thing that can link two requests. Taken verbatim it reaches every log line
// and span attribute of the request, and three separate things go wrong: a
// newline forges a log line in text format, an unbounded string is an
// amplification the client controls, and an arbitrary session.id is unbounded
// cardinality in a tracing backend.
func TestSanitizeIDBoundsWhatAClientCanPutInALogLine(t *testing.T) {
	if got := SanitizeID("thread-abc_1.2"); got != "thread-abc_1.2" {
		t.Errorf("a well-formed id must survive intact: %q", got)
	}
	if got := SanitizeID(""); got != "" {
		t.Errorf("empty stays empty: %q", got)
	}

	forged := "abc\",\"level\":\"ERROR\",\"msg\":\"disk failure\nnext line"
	got := SanitizeID(forged)
	for _, bad := range []string{"\n", "\"", ":", " ", ","} {
		if strings.Contains(got, bad) {
			t.Errorf("sanitized id still contains %q: %q", bad, got)
		}
	}

	if got := SanitizeID(strings.Repeat("a", 10_000)); len(got) != MaxIDLength {
		t.Errorf("length must be bounded to %d, got %d", MaxIDLength, len(got))
	}

	// An id made entirely of rejected characters comes back empty rather than
	// as some mangled remnant -- callers already treat empty as "not bound".
	if got := SanitizeID("！！！ ：：："); got != "" {
		t.Errorf("all-rejected id must be empty, got %q", got)
	}
}
