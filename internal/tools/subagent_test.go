package tools

import (
	"context"
	"testing"

	"github.com/x6nux/yanshi/internal/agent/registry"
)

// TestUsageSinkFromRoundTrip pins the context carrier the sub-agent turn uses
// to report what it spent.
//
// The getter had zero production callers until W3: the orchestrator
// accumulated a sub-agent's token usage and then dropped it, so a delegating
// agent's spend was invisible to the parent's budget — and an agent could
// therefore spend without limit by the simple expedient of delegating.
func TestUsageSinkFromRoundTrip(t *testing.T) {
	if got := UsageSinkFrom(context.Background()); got != nil {
		t.Fatalf("UsageSinkFrom on a bare context = %v; want nil", got)
	}
	var seen []registry.Usage
	ctx := WithUsageSink(context.Background(), func(u registry.Usage) {
		seen = append(seen, u)
	})
	sink := UsageSinkFrom(ctx)
	if sink == nil {
		t.Fatal("UsageSinkFrom returned nil for a context that carries one")
	}
	sink(registry.Usage{PromptTokens: 7, CompletionTokens: 3, TotalTokens: 10})
	if len(seen) != 1 || seen[0].TotalTokens != 10 {
		t.Fatalf("sink delivered %+v; want one usage totalling 10", seen)
	}
}
