package ctxcompact

import (
	"context"
	"math/rand/v2"
	"testing"
)

func TestProperty_NoDoubleCompaction(t *testing.T) {
	rng := rand.New(rand.NewPCG(777, 0))
	msgs := genHistory(rng, 30)
	if len(msgs) == 0 {
		return
	}

	rs1 := &recordingSummarizer{Return: "first summary"}
	planOpts := PlanOpts{KeepRecent: 3}
	runOpts := RunOpts{
		ModelWindow:      2000,
		ChunkThreshold:   0.9,
		SummaryWordLimit: 200,
	}

	result1, err := Run(context.Background(), msgs, planOpts, runOpts, rs1, nil)
	if err != nil {
		t.Fatalf("first Run failed: %v", err)
	}
	if len(rs1.GenerateCalls)+len(rs1.StreamCalls) == 0 {
		t.Log("first Run had nothing to summarize — skipping P5")
		return
	}

	rs2 := &recordingSummarizer{Return: "re-summary"}
	result2, err := Run(context.Background(), result1.Messages, planOpts, runOpts, rs2, nil)
	if err != nil {
		t.Fatalf("second Run failed: %v", err)
	}

	calls2 := len(rs2.GenerateCalls) + len(rs2.StreamCalls)
	if calls2 > 0 {
		t.Fatalf("summary-of-summary: second Run made %d summarizer calls, want 0", calls2)
	}

	if len(result2.Messages) != len(result1.Messages) {
		t.Logf("second Run output length %d != first %d (informational)", len(result2.Messages), len(result1.Messages))
	}
}

// TestProperty_RunReducesTokens is a second, independent property: whenever
// Run actually summarizes something, the resulting history must estimate to
// strictly fewer tokens than the input. A compaction that grows the history is
// worse than no compaction — it burns a model call to move closer to the wall.
//
// The input is a randomly generated 40-message history (genHistory over a
// seeded PCG), not a fixture, so the property is asserted against shapes
// nobody chose by hand.
//
// ledger: E2/PROP1#1 ≥3 个属性
// ledger: E2/PROP1#2 随机输入通过
func TestProperty_RunReducesTokens(t *testing.T) {
	rng := rand.New(rand.NewPCG(123, 0))
	msgs := genHistory(rng, 40)
	if len(msgs) == 0 {
		return
	}

	rs := &recordingSummarizer{Return: "compacted summary"}

	result, err := Run(context.Background(), msgs, PlanOpts{KeepRecent: 3}, RunOpts{
		ModelWindow:      2000,
		ChunkThreshold:   0.9,
		SummaryWordLimit: 200,
	}, rs, nil)
	if err != nil {
		if len(rs.GenerateCalls)+len(rs.StreamCalls) == 0 {
			return
		}
		t.Fatalf("Run failed: %v", err)
	}

	before := EstimateTokens(msgs)
	after := EstimateTokens(result.Messages)
	if after >= before && len(rs.GenerateCalls)+len(rs.StreamCalls) > 0 {
		t.Fatalf("Run did not reduce tokens: before=%d, after=%d (summarizer called %d times)", before, after, len(rs.GenerateCalls)+len(rs.StreamCalls))
	}
}
