package ctxcompact

import (
	"context"
	"testing"

	"github.com/cloudwego/eino/schema"
)

// TestProperty_NoDoubleCompaction is the "summary of a summary" property:
// running Run over its own output must not summarize again. Compacting an
// already-compacted history destroys the one message the first pass promised to
// keep, and does it while burning a model call.
//
// It used to run against a SINGLE fixed history and bail with `t.Log(...);
// return` when the first Run made no summarizer calls — a guard computed from
// the code under test's own output, which is the exact shape the review
// checklist's section A calls vacuous. A Run gutted to return its input
// verbatim made zero calls, the guard fired, and the test printed PASS while
// asserting nothing:
//
//	run_property_test.go:31: first Run had nothing to summarize — skipping P5
//	--- PASS: TestProperty_NoDoubleCompaction (0.00s)
//
// It now goes through runGeneratedProperty like every other generated property
// in this package, and the "did the first pass actually compact" condition is a
// cross-trial floor rather than a per-trial escape.
func TestProperty_NoDoubleCompaction(t *testing.T) {
	const trials = 30
	compacted := 0
	planOpts := PlanOpts{KeepRecent: 3}
	runOpts := RunOpts{
		ModelWindow:      2000,
		ChunkThreshold:   0.9,
		SummaryWordLimit: 200,
	}
	runGeneratedProperty(t, trials, 60, func(t *testing.T, msgs []*schema.Message) {
		rs1 := &recordingSummarizer{Return: "first summary"}
		result1, err := Run(context.Background(), msgs, planOpts, runOpts, rs1, nil)
		if err != nil {
			t.Fatalf("first Run failed on a %d-message history: %v", len(msgs), err)
		}
		if len(rs1.GenerateCalls)+len(rs1.StreamCalls) == 0 {
			return // nothing to summarize in this trial; the floor below covers it
		}
		compacted++

		rs2 := &recordingSummarizer{Return: "re-summary"}
		result2, err := Run(context.Background(), result1.Messages, planOpts, runOpts, rs2, nil)
		if err != nil {
			t.Fatalf("second Run failed: %v", err)
		}
		if calls2 := len(rs2.GenerateCalls) + len(rs2.StreamCalls); calls2 > 0 {
			t.Fatalf("summary-of-summary: second Run made %d summarizer calls, want 0", calls2)
		}
		if len(result2.Messages) != len(result1.Messages) {
			t.Logf("second Run output length %d != first %d (informational)", len(result2.Messages), len(result1.Messages))
		}
	})
	requireTrialFloor(t, "compacted on the first Run", compacted, trials)
}

// TestProperty_RunReducesTokens is a second, independent property: whenever
// Run actually summarizes something, the resulting history must estimate to
// strictly fewer tokens than the input. A compaction that grows the history is
// worse than no compaction — it burns a model call to move closer to the wall.
//
// The inputs are randomly generated histories (genHistory over a seeded PCG),
// not fixtures, so the property is asserted against shapes nobody chose by
// hand.
//
// The summarizer-call count is a HARD REQUIREMENT here, not a condition on the
// assertion. It used to be the latter — `if after >= before && calls > 0` —
// which handed the verdict to the code under test: a Run gutted to return its
// input verbatim makes zero summarizer calls, so the conjunction was false in
// every trial and the test passed while proving that compaction does nothing.
// Per-trial the count can legitimately be zero (Plan may find nothing to
// summarize in a short history), so the requirement is enforced as a floor
// across trials rather than a per-trial fatal — see requireTrialFloor.
//
// ledger: E2/PROP1#1 ≥3 个属性
// ledger: E2/PROP1#2 随机输入通过
func TestProperty_RunReducesTokens(t *testing.T) {
	const trials = 30
	summarized := 0
	runGeneratedProperty(t, trials, 60, func(t *testing.T, msgs []*schema.Message) {
		rs := &recordingSummarizer{Return: "compacted summary"}
		result, err := Run(context.Background(), msgs, PlanOpts{KeepRecent: 3}, RunOpts{
			ModelWindow:      2000,
			ChunkThreshold:   0.9,
			SummaryWordLimit: 200,
		}, rs, nil)
		if err != nil {
			t.Fatalf("Run failed on a %d-message history: %v", len(msgs), err)
		}
		calls := len(rs.GenerateCalls) + len(rs.StreamCalls)
		if calls == 0 {
			return // nothing to summarize in this trial; the floor below covers it
		}
		summarized++

		before := EstimateTokens(msgs)
		after := EstimateTokens(result.Messages)
		if after >= before {
			t.Fatalf("Run did not reduce tokens: before=%d, after=%d (summarizer called %d times, %d/%d messages summarized)",
				before, after, calls, len(msgs)-len(result.Messages)+1, len(msgs))
		}
	})
	requireTrialFloor(t, "summarized anything", summarized, trials)
}
