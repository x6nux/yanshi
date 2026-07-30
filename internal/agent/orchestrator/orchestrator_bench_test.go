package orchestrator

import (
	"context"
	"testing"

	einollm "github.com/x6nux/yanshi/internal/llm/eino"
)

// BenchmarkOrchestratorTurn measures one full orchestrator turn (the ADK ReAct
// loop driven by a deterministic FakeModel). It establishes a baseline for
// per-turn overhead so regressions in the turn machinery show up in benchstat.
func BenchmarkOrchestratorTurn(b *testing.B) {
	model := einollm.NewFakeModel([]string{"hello from agent"}, nil)
	o, err := New(Config{Model: model})
	if err != nil {
		b.Fatal(err)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := o.Query(context.Background(), "hi"); err != nil {
			b.Fatal(err)
		}
	}
}
