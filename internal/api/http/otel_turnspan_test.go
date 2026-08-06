package http

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/x6nux/yanshi/internal/agent/orchestrator"
	einollm "github.com/x6nux/yanshi/internal/llm/eino"
	"github.com/x6nux/yanshi/internal/proto"
)

// recordSpans installs a real in-memory SDK tracer provider for the duration
// of one test.
//
// Swapping the GLOBAL provider rather than injecting a seam is deliberate:
// startOperation resolves otel.Tracer() at call time, so this observes the
// exact code path production runs. A seam variable would have been satisfied
// by a call that never reaches a provider at all -- which is precisely the
// state this test exists to rule out.
func recordSpans(t *testing.T) *tracetest.InMemoryExporter {
	t.Helper()
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() {
		otel.SetTracerProvider(prev)
		_ = tp.Shutdown(context.Background())
	})
	return exp
}

// turnSpan returns the single agent.turn span, or fails.
func turnSpan(t *testing.T, exp *tracetest.InMemoryExporter) tracetest.SpanStub {
	t.Helper()
	var found []tracetest.SpanStub
	for _, s := range exp.GetSpans() {
		if s.Name == "agent.turn" {
			found = append(found, s)
		}
	}
	if len(found) != 1 {
		var names []string
		for _, s := range exp.GetSpans() {
			names = append(names, s.Name)
		}
		t.Fatalf("want exactly 1 agent.turn span, got %d; spans recorded: %v", len(found), names)
	}
	return found[0]
}

func spanAttr(s tracetest.SpanStub, key string) string {
	for _, kv := range s.Attributes {
		if kv.Key == attribute.Key(key) {
			return kv.Value.AsString()
		}
	}
	return ""
}

// TestWSTurnOpensATurnSpan pins the wiring half of turn tracing.
//
// otelobs.StartTurn had exactly one production caller: Orchestrator.Query,
// the SYNCHRONOUS path, which neither transport uses. Both WS and SSE run
// turns through EventsWithHistoryOpts, and a comment in orchestrator.go
// asserted those paths "manage their own spans at the WS/SSE drain boundary"
// -- a boundary that was never written. Every turn a real user ran produced
// no span at all, while the code read as though tracing were covered.
//
// The assertion has to be end-to-end through the transport. Calling StartTurn
// directly would prove the function works, which was never in doubt; the
// missing thing is the call. The attributes are checked too, because an
// agent.turn span with no session.id or turn.id cannot be joined against the
// correlated log lines that ws.go already emits -- the span would exist and
// still be useless.
//
// ledger: C4/OBS2#1 trace 链可导出
func TestWSTurnOpensATurnSpan(t *testing.T) {
	exp := recordSpans(t)

	fm := einollm.NewFakeModel([]string{"hello"}, nil)
	o, err := orchestrator.New(orchestrator.Config{Model: fm})
	require.NoError(t, err)
	s := New(Config{Token: "t"})
	// A registry with a name in it: model.name is read from the resolved
	// registry name, and an empty registry legitimately has none.
	s.ChatWS(o, map[string]model.BaseChatModel{"fake-1": fm}, nil)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	c := dial(t, "ws"+strings.TrimPrefix(ts.URL, "http")+"/api/v1/chat/ws")
	defer c.Close()
	require.NoError(t, c.WriteJSON(proto.NewUserMessage("hi")))
	for {
		if recvFrame(t, c).Type == "done" {
			break
		}
	}

	span := turnSpan(t, exp)
	if got := spanAttr(span, "turn.id"); got == "" {
		t.Error("agent.turn carries no turn.id: it cannot be joined to the turn's log lines")
	}
	if got := spanAttr(span, "model.name"); got != "fake-1" {
		t.Errorf("model.name = %q, want the resolved registry name: latency cannot be "+
			"faceted per model without it", got)
	}
}

// TestSSETurnOpensATurnSpanAndHonoursTheClientThreadID covers the second
// transport and one field that was accepted and thrown away.
//
// SSE never bound correlation IDs at all, so even the log lines it emitted
// during a turn carried none. Its request struct declared thread_id and
// turn_id, and neither was read anywhere in the file: a client that sent them
// got a 200 and silently no correlation, which is worse than a rejection
// because it looks like it worked. They are the natural source for the span's
// identity -- SSE is stateless, so the client's id is the ONLY thing that can
// link two requests of one conversation.
func TestSSETurnOpensATurnSpanAndHonoursTheClientThreadID(t *testing.T) {
	exp := recordSpans(t)

	fm := einollm.NewFakeModel([]string{"hello"}, nil)
	o, err := orchestrator.New(orchestrator.Config{Model: fm})
	require.NoError(t, err)
	s := New(Config{Token: "t"})
	s.Chat(o, map[string]model.BaseChatModel{"fake-1": fm}, nil)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	body := `{"message":"hi","thread_id":"thread-abc","turn_id":"turn-xyz"}`
	req, _ := http.NewRequest("POST", ts.URL+"/api/v1/chat", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer t")
	req.Header.Set("Content-Type", "application/json")
	resp, err := ts.Client().Do(req)
	require.NoError(t, err)
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	span := turnSpan(t, exp)
	if got := spanAttr(span, "session.id"); got != "thread-abc" {
		t.Errorf("session.id = %q, want the client's thread_id: the field was declared on the "+
			"wire and never read, so a client that sent it got no correlation and no error", got)
	}
	if got := spanAttr(span, "turn.id"); got != "turn-xyz" {
		t.Errorf("turn.id = %q, want the client's turn_id", got)
	}
	if got := spanAttr(span, "model.name"); got != "fake-1" {
		t.Errorf("model.name = %q, want the resolved registry name", got)
	}
}

// TestTurnSpanRecordsFailureNotJustLatency covers the half a "did a span
// appear" assertion cannot.
//
// Both transports declared a turnErr and the SSE one never assigned it, so
// every SSE turn ended with a nil error and reported success -- a model
// failure, a client disconnect and a clean run were indistinguishable in any
// tracing backend. That is the single question a turn span exists to answer,
// and `go vet` sees nothing wrong because the variable IS read, in the defer.
func TestTurnSpanRecordsFailureNotJustLatency(t *testing.T) {
	exp := recordSpans(t)

	fm := einollm.NewFakeModel(nil, errors.New("provider exploded"))
	o, err := orchestrator.New(orchestrator.Config{Model: fm})
	require.NoError(t, err)
	s := New(Config{Token: "t"})
	s.Chat(o, map[string]model.BaseChatModel{"fake-1": fm}, nil)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	req, _ := http.NewRequest("POST", ts.URL+"/api/v1/chat", strings.NewReader(`{"message":"hi"}`))
	req.Header.Set("Authorization", "Bearer t")
	req.Header.Set("Content-Type", "application/json")
	resp, err := ts.Client().Do(req)
	require.NoError(t, err)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "error") {
		t.Fatalf("the turn did not actually fail, so the assertion below is vacuous:\n%s", body)
	}

	span := turnSpan(t, exp)
	if span.Status.Code != codes.Error {
		t.Errorf("a turn that emitted an error frame reports span status %v: "+
			"a failed turn and a clean one look identical", span.Status.Code)
	}
	if spanAttr(span, "error.type") == "" {
		t.Error("no error.type attribute: the backend cannot facet failures by kind")
	}
}

// TestWSTurnSpanRecordsFailure is the WS twin of the SSE case above. The two
// transports have separate turn loops with separate error handling, so a fix
// applied to one proves nothing about the other -- which is how the SSE half
// came to be written and never assigned.
func TestWSTurnSpanRecordsFailure(t *testing.T) {
	exp := recordSpans(t)

	fm := einollm.NewFakeModel(nil, errors.New("provider exploded"))
	o, err := orchestrator.New(orchestrator.Config{Model: fm})
	require.NoError(t, err)
	s := New(Config{Token: "t"})
	s.ChatWS(o, map[string]model.BaseChatModel{"fake-1": fm}, nil)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	c := dial(t, "ws"+strings.TrimPrefix(ts.URL, "http")+"/api/v1/chat/ws")
	defer c.Close()
	require.NoError(t, c.WriteJSON(proto.NewUserMessage("hi")))
	sawError := false
	for {
		f := recvFrame(t, c)
		if f.Type == "error" {
			sawError = true
		}
		if f.Type == "done" {
			break
		}
	}
	if !sawError {
		t.Fatal("the turn did not actually fail, so the assertion below is vacuous")
	}

	span := turnSpan(t, exp)
	if span.Status.Code != codes.Error {
		t.Errorf("a WS turn that emitted an error frame reports span status %v", span.Status.Code)
	}
}
