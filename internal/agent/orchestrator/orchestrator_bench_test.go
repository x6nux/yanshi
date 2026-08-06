package orchestrator

import (
	"context"
	"io"
	"log/slog"
	"testing"

	einollm "github.com/x6nux/yanshi/internal/llm/eino"
)

// BenchmarkOrchestratorTurn measures one full orchestrator turn (the ADK ReAct
// loop driven by a deterministic FakeModel). It establishes a baseline for
// per-turn overhead so regressions in the turn machinery show up in benchstat.
func BenchmarkOrchestratorTurn(b *testing.B) {
	// Repeat is required, not a convenience. A FakeModel with one scripted
	// response and Repeat off returns an EMPTY assistant message once the
	// counter runs past it, so the second iteration fails with "no assistant
	// message produced" -- meaning this benchmark only ever worked at
	// -benchtime=1x, the one value that hides it. Any real run (the default
	// 1s, or -benchtime=10x in CI) failed instead of measuring anything.
	model := einollm.NewFakeModel([]string{"hello from agent"}, nil)
	model.Repeat = true
	o, err := New(Config{Model: model})
	if err != nil {
		b.Fatal(err)
	}

	// Benchmarks share the process-wide slog default, and the guard layer logs
	// a permission decision per tool call. Left alone, those INFO lines
	// interleave with the benchmark's own output -- and scripts/bench.sh
	// redirected stderr into the baseline file, so the log lines landed in the
	// data benchstat parses.
	restore := benchSilenceLogs()
	defer restore()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := o.Query(context.Background(), "hi"); err != nil {
			b.Fatal(err)
		}
	}
}

// benchSilenceLogs points the default slog logger at io.Discard for the
// duration of a benchmark and returns a restore func.
//
// Silencing at the sink rather than per-call site: the noise comes from
// guard's audit path, which is production code that SHOULD log, and a
// benchmark has no business changing what production code decides to say.
func benchSilenceLogs() func() {
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	return func() { slog.SetDefault(prev) }
}
