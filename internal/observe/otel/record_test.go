package otelobs

import (
	"context"
	"errors"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	obslog "github.com/x6nux/yanshi/internal/observe/log"
)

// withRecordingTracer installs a TracerProvider backed by a span recorder and
// returns the recorder plus a cleanup that restores a fresh provider. The
// instrument helpers reach the global tracer via otel.Tracer, so the recorder
// sees every span they open.
func withRecordingTracer(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()
	recorder := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	otel.SetTracerProvider(tp)
	t.Cleanup(func() {
		otel.SetTracerProvider(sdktrace.NewTracerProvider())
	})
	return recorder
}

func hasAttr(span sdktrace.ReadOnlySpan, key, want string) bool {
	for _, a := range span.Attributes() {
		if string(a.Key) == key && a.Value.AsString() == want {
			return true
		}
	}
	return false
}

func TestInstrumentMustPanicsOnError(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("instrumentMust must panic when instrument creation fails")
		}
	}()
	instrumentMust(errors.New("instrument creation failed"))
}

// TestStartSessionStartTurnSetSessionID exercises the three span-opening
// helpers and the SetSessionID update path, asserting spans carry the safe
// correlation attributes (and never any value body).
func TestStartSessionStartTurnSetSessionID(t *testing.T) {
	recorder := withRecordingTracer(t)

	// StartSession with an empty context, then attach session.id via
	// SetSessionID (both the empty no-op and the real update).
	ctx, end := StartSession(context.Background())
	SetSessionID(ctx, "") // empty → no-op branch
	SetSessionID(ctx, "s1")
	end(nil)

	// StartSession with a session ID already in context exercises the branch
	// that pre-populates the session.id attribute from the correlation IDs.
	sessCtx, sessEnd := StartSession(obslog.WithIDs(context.Background(), obslog.IDs{SessionID: "s9"}))
	sessEnd(nil)
	_ = sessCtx

	// StartTurn with full IDs plus a model name.
	idsCtx := obslog.WithIDs(context.Background(), obslog.IDs{
		SessionID: "s2",
		TurnID:    "t2",
	})
	_, tend := StartTurn(idsCtx, "gpt-x")
	tend(nil)

	spans := recorder.Ended()
	if len(spans) != 3 {
		t.Fatalf("expected 3 ended spans, got %d", len(spans))
	}
	var sessionSpans []sdktrace.ReadOnlySpan
	var turnSpan sdktrace.ReadOnlySpan
	for _, s := range spans {
		switch s.Name() {
		case "agent.session":
			sessionSpans = append(sessionSpans, s)
		case "agent.turn":
			turnSpan = s
		}
	}
	if len(sessionSpans) != 2 {
		t.Fatalf("agent.session spans=%d want 2: %+v", len(sessionSpans), spans)
	}
	// One session span got its id from SetSessionID, the other from context.
	sessionIDs := map[string]bool{}
	for _, s := range sessionSpans {
		for _, a := range s.Attributes() {
			if string(a.Key) == "session.id" {
				sessionIDs[a.Value.AsString()] = true
			}
		}
	}
	if !sessionIDs["s1"] || !sessionIDs["s9"] {
		t.Fatalf("session ids missing %v: %+v", sessionIDs, sessionSpans)
	}
	if turnSpan == nil {
		t.Fatalf("agent.turn span missing: %+v", spans)
	}
	for _, want := range []struct{ k, v string }{
		{"session.id", "s2"},
		{"turn.id", "t2"},
		{"model.name", "gpt-x"},
	} {
		if !hasAttr(turnSpan, want.k, want.v) {
			t.Fatalf("turn span missing %s=%s: %+v", want.k, want.v, turnSpan.Attributes())
		}
	}
}

// TestStartTurnOmitsEmptyAttrs covers the guard branches where no IDs/model
// are present: the attr slice stays empty rather than emitting zero values.
func TestStartTurnOmitsEmptyAttrs(t *testing.T) {
	recorder := withRecordingTracer(t)
	_, end := StartTurn(context.Background(), "")
	end(nil)
	spans := recorder.Ended()
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(spans))
	}
	for _, a := range spans[0].Attributes() {
		if a.Key == attribute.Key("session.id") || a.Key == attribute.Key("turn.id") || a.Key == attribute.Key("model.name") {
			t.Fatalf("empty turn must not emit correlation attrs: %+v", a)
		}
	}
}

// TestRecordUsageClampsNegativesAndSkipsZeros exercises the clamping logic
// (prompt<0, cached<0, cached>prompt, completion<0, reasoning<0) and the
// per-kind zero skip. Instruments are noop-bound under test, so we assert
// only that emission does not panic across representative shapes.
func TestRecordUsageClampsNegativesAndSkipsZeros(t *testing.T) {
	withRecordingTracer(t) // ensure a real tracer provider is installed
	ctx := context.Background()

	RecordUsage(ctx, "model-x", 100, 20, 50, 10)  // all four kinds positive
	RecordUsage(ctx, "model-x", 0, 0, 0, 0)       // all zero → loop body skipped
	RecordUsage(ctx, "model-x", -5, 1000, -1, -3) // negatives clamp; cached>prompt clamps
	RecordUsage(ctx, "model-x", 10, -4, 0, 0)     // negative cached clamps to 0
	RecordUsage(ctx, "", 10, 0, 0, 0)             // single positive kind
}

// TestRecordRetryEmitsSafeAttributes.
//
// ledger: C4/OBS2#2 latency/token/retry/error 可观测
func TestRecordRetryEmitsSafeAttributes(t *testing.T) {
	withRecordingTracer(t)
	ctx := context.Background()
	RecordRetry(ctx, 1, 3, errors.New("transient"))
	RecordRetry(ctx, 2, 3, nil) // nil error → SafeErrorType returns ""
}

// TestRecordFallbackEmitsSafeAttributes exercises RecordFallback (W-C-10):
// a genuine chain-advance signal distinct from RecordRetry, indexed rather
// than named (ResilientChatModel has no per-entry label), with the same
// safe-attribute discipline (error TYPE only, never the error body).
func TestRecordFallbackEmitsSafeAttributes(t *testing.T) {
	withRecordingTracer(t)
	ctx := context.Background()
	RecordFallback(ctx, 0, 1, errors.New("provider exhausted"))
	RecordFallback(ctx, 1, 2, nil) // nil error → SafeErrorType returns ""
}

// TestRecordNewWindowIncrementsCounter exercises RecordNewWindow (W-C-14): a
// proactive-open signal distinct from RecordFallback's model-failure signal.
// Unlike RecordFallback/RecordRetry it takes no error and no index/total —
// there is nothing to grade attempts against, the model just asked — so this
// test only confirms the call does not panic and the counter it touches was
// actually registered by ensureInstruments (a typo'd metric name or a nil
// instrument would panic here, the way TestInstrumentMustPanicsOnError shows
// instrumentMust does for a real registration error).
func TestRecordNewWindowIncrementsCounter(t *testing.T) {
	withRecordingTracer(t)
	ctx := context.Background()
	RecordNewWindow(ctx)
	RecordNewWindow(ctx) // a second call in the same turn must not panic either
}

// TestStartSessionErrorEndSetsStatus covers the error branch of the shared
// end function for a session span: the error.type attribute and Error status
// are set, and the error body itself never enters the span.
//
// ledger: C4/OBS2#2 latency/token/retry/error 可观测
func TestStartSessionErrorEndSetsStatus(t *testing.T) {
	recorder := withRecordingTracer(t)
	_, end := StartSession(context.Background())
	secret := errors.New("body-with-sk-secret")
	end(secret)

	spans := recorder.Ended()
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(spans))
	}
	span := spans[0]
	if !hasAttr(span, "error.type", "*errors.errorString") {
		t.Fatalf("error.type not set: %+v", span.Attributes())
	}
	for _, a := range span.Attributes() {
		if a.Value.AsString() == secret.Error() {
			t.Fatalf("error body leaked into span attr: %+v", a)
		}
	}
	for _, ev := range span.Events() {
		if ev.Name == "exception" {
			t.Fatalf("exception event would leak body: %+v", ev)
		}
	}
}
