// internal/llm/eino/m10_usage_wire_test.go
//
// M10 over the wire: does a real provider call actually produce a usage record,
// with the numbers the provider reported?
//
// This capability has already produced one FALSE NEGATIVE during verification,
// and the shape of it is worth recording. Running `yanshi exec --fake-model` and
// finding usage_log empty looks exactly like a broken feature — but --fake-model
// bypasses the provider path entirely, so an empty table was the correct
// behaviour and the conclusion "M10 is dead" was wrong. Confirming it needed a
// run where a real provider adapter really answered.
//
// Against the stub, through the real binary, three turns produced six rows (two
// provider calls per turn — the turn plus the completion judge), each carrying
// provider=openai, the configured model id, the session id, and the stub's
// reported 11/7 tokens.
//
// The tests below pin the same thing at package level, where they can also
// cover the parts a manual run cannot conveniently reach: the streaming path
// (one row per call, from the LAST usage-carrying chunk rather than a sum over
// chunks), and the rule that a failed call records nothing.
package eino

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

// recordingSink collects usage records in memory.
type recordingSink struct {
	mu   sync.Mutex
	recs []UsageRecord
	// err, when set, is returned by every RecordUsage call.
	err error
}

// RecordUsage appends rec.
func (s *recordingSink) RecordUsage(_ context.Context, rec UsageRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recs = append(s.recs, rec)
	return s.err
}

// all returns a copy of the collected records.
func (s *recordingSink) all() []UsageRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]UsageRecord(nil), s.recs...)
}

// compile-time interface check.
var _ UsageSink = (*recordingSink)(nil)

// TestM10_NonStreamingCallRecordsOneRow proves the numbers on the row come from
// the provider's own usage object rather than from an estimate.
func TestM10_NonStreamingCallRecordsOneRow(t *testing.T) {
	s := newStubProvider(t, nil) // reports prompt=11 completion=7
	inner, _ := buildStubModel(t, s, nil)
	sink := &recordingSink{}
	a := NewAdaptiveModel(inner, AdaptiveConfig{
		ModelID: "stub-model-a", Provider: "openai", UsageSink: sink,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := a.Generate(ctx, []*schema.Message{schema.UserMessage("hi")}); err != nil {
		t.Fatalf("Generate: %v", err)
	}

	recs := sink.all()
	t.Logf("recorded: %+v", recs)
	if len(recs) != 1 {
		t.Fatalf("recorded %d rows for one call, want 1", len(recs))
	}
	r := recs[0]
	if r.PromptTokens != 11 || r.CompletionTokens != 7 {
		t.Errorf("tokens = %d/%d, want the provider's reported 11/7 — a row carrying anything "+
			"else is not accounting, it is a guess", r.PromptTokens, r.CompletionTokens)
	}
	if r.Model != "stub-model-a" {
		t.Errorf("model = %q, want the registry key", r.Model)
	}
	if r.Provider != "openai" {
		t.Errorf("provider = %q, want the adapter kind", r.Provider)
	}
}

// TestM10_StreamingCallRecordsExactlyOneRow is the interesting half.
//
// Providers report usage on a FINAL chunk (OpenAI with stream_options,
// Anthropic's message_delta), and earlier chunks either omit it or carry a
// running total. Summing across chunks would multiply one call's tokens by the
// number of chunks that mentioned them — an error that inflates the bill report
// silently and in a direction nobody double-checks.
func TestM10_StreamingCallRecordsExactlyOneRow(t *testing.T) {
	s := newStubProvider(t, nil)
	inner, _ := buildStubModel(t, s, nil)
	sink := &recordingSink{}
	a := NewAdaptiveModel(inner, AdaptiveConfig{
		ModelID: "stub-model-a", Provider: "openai", UsageSink: sink,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	sr, err := a.Stream(ctx, []*schema.Message{schema.UserMessage("hi")})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	var text string
	for {
		msg, recvErr := sr.Recv()
		if recvErr != nil {
			break
		}
		if msg != nil {
			text += msg.Content
		}
	}
	sr.Close()

	// The row is written by the forwarding goroutine as the stream ends; give it
	// the moment it needs rather than asserting on a race.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && len(sink.all()) == 0 {
		time.Sleep(10 * time.Millisecond)
	}

	recs := sink.all()
	t.Logf("streamed text=%q recorded=%+v", text, recs)
	if len(recs) != 1 {
		t.Fatalf("recorded %d rows for one streamed call, want exactly 1: summing per-chunk usage "+
			"multiplies a single call's tokens by the number of chunks", len(recs))
	}
	if recs[0].PromptTokens != 11 || recs[0].CompletionTokens != 7 {
		t.Errorf("tokens = %d/%d, want 11/7", recs[0].PromptTokens, recs[0].CompletionTokens)
	}
}

// TestM10_FailedCallRecordsNothing pins that accounting follows completed calls
// only. A row for a rejected request would make the ledger disagree with the
// invoice in the direction that looks like overspending.
func TestM10_FailedCallRecordsNothing(t *testing.T) {
	s := newStubProvider(t, alwaysStatus(401,
		`{"error":{"message":"Incorrect API key provided","code":"invalid_api_key"}}`))
	inner, _ := buildStubModel(t, s, nil)
	sink := &recordingSink{}
	a := NewAdaptiveModel(inner, AdaptiveConfig{
		ModelID: "stub-model-a", Provider: "openai", UsageSink: sink,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := a.Generate(ctx, []*schema.Message{schema.UserMessage("hi")}); err == nil {
		t.Fatal("want an error")
	}
	if recs := sink.all(); len(recs) != 0 {
		t.Errorf("recorded %d rows for a failed call: %+v", len(recs), recs)
	}
}

// TestM10_SinkFailureDoesNotFailTheTurn pins the priority order stated in
// UsageSink's contract: losing an accounting row must never fail a turn that
// already succeeded. The user's answer is worth more than the bookkeeping about
// it, and a full disk must not become "yanshi stopped working".
func TestM10_SinkFailureDoesNotFailTheTurn(t *testing.T) {
	s := newStubProvider(t, nil)
	inner, _ := buildStubModel(t, s, nil)
	sink := &recordingSink{err: errors.New("disk full")}
	a := NewAdaptiveModel(inner, AdaptiveConfig{
		ModelID: "stub-model-a", Provider: "openai", UsageSink: sink,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	out, err := a.Generate(ctx, []*schema.Message{schema.UserMessage("hi")})
	if err != nil {
		t.Fatalf("a failing usage sink broke the turn: %v", err)
	}
	if out.Content == "" {
		t.Error("the answer was lost")
	}
	if n := len(sink.all()); n != 1 {
		t.Errorf("the sink was called %d times, want 1 (it must still be attempted)", n)
	}
}

// TestM10_NoSinkIsInert pins the default. A deployment that wants no usage
// history passes nil, and that must cost nothing and change nothing.
func TestM10_NoSinkIsInert(t *testing.T) {
	s := newStubProvider(t, nil)
	inner, _ := buildStubModel(t, s, nil)
	a := NewAdaptiveModel(inner, AdaptiveConfig{ModelID: "stub-model-a", UsageSink: nil})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := a.Generate(ctx, []*schema.Message{schema.UserMessage("hi")}); err != nil {
		t.Fatalf("Generate with no sink: %v", err)
	}
}

// TestM10_EveryCallOfARetriedRequestIsNotDoubleCounted covers the interaction
// with the repair paths: a call that failed and was repaired made two HTTP
// requests but produced one answer, and only the answer is billable output the
// ledger should carry once.
func TestM10_EveryCallOfARetriedRequestIsNotDoubleCounted(t *testing.T) {
	s := newStubProvider(t, func(n int, _ capturedRequest) stubResponse {
		if n == 1 {
			return stubResponse{Status: 400, Body: schemaRejectionBody}
		}
		return stubResponse{}
	})
	inner, _ := buildStubModel(t, s, nil)
	sink := &recordingSink{}
	a := NewAdaptiveModel(inner, AdaptiveConfig{
		ModelID: "stub-model-a", Provider: "openai", UsageSink: sink,
		Quirks: NewQuirkStore(), Sanitize: SanitizeAuto,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := a.Generate(ctx, []*schema.Message{schema.UserMessage("hi")},
		model.WithTools([]*schema.ToolInfo{refSchemaTool(t)})); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	recs := sink.all()
	t.Logf("%d HTTP requests produced %d usage rows", len(s.chatRequests()), len(recs))
	if len(recs) != 1 {
		t.Errorf("recorded %d rows for one successful answer (after one repaired attempt), want 1",
			len(recs))
	}
}
