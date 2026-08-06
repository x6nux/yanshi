package http

import (
	"context"
	"testing"

	"github.com/x6nux/yanshi/internal/agent/orchestrator"
)

// TestAddProviderUsageRecordsToOTel closes a gap that was visible only from
// the seam: addProviderUsage took a context and discarded it
// (`_ context.Context`), so otel.RecordUsage had zero production callers and
// the yanshi.llm.tokens counter never emitted a single point. "Tokens are
// observable" was true of the instrument and false of the system.
//
// The assertion is that the call reaches the recorder, driven through
// addProviderUsage rather than by calling RecordUsage directly -- calling it
// directly would prove only that the function works, which was never in
// question. What was missing is the wiring.
//
// Recording here rather than in the streaming callback is load-bearing: this
// is the one place with the exactly-once guarantee, and providers that report
// cumulative totals per chunk would otherwise be counted many times over.
func TestAddProviderUsageRecordsToOTel(t *testing.T) {
	var got []int
	restore := swapUsageRecorder(func(_ context.Context, _ string, prompt, cached, completion, reasoning int) {
		got = append(got, prompt, cached, completion, reasoning)
	})
	defer restore()

	cs := &connSession{}
	srv := New(Config{Token: "t"})

	cs.addProviderUsage(context.Background(), srv, orchestrator.TurnUsage{
		PromptTokens: 100, CachedTokens: 20, CompletionTokens: 50, ReasoningTokens: 7,
	})
	if len(got) == 0 {
		t.Fatal("usage never reached the recorder: the counter can emit nothing")
	}
	if got[0] != 100 || got[2] != 50 {
		t.Fatalf("token counts mangled on the way: %v", got)
	}

	// A zero usage must not emit: providers send a final empty usage on the
	// last chunk, and a point of all zeros is noise in every dashboard.
	got = nil
	cs.addProviderUsage(context.Background(), srv, orchestrator.TurnUsage{})
	if len(got) != 0 {
		t.Fatalf("an empty usage was recorded: %v", got)
	}
}
