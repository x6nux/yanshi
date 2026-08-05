package ctxcompact

import (
	"context"
	"strings"
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
	// KeepRecent and ModelWindow used to be fixed at 3 and 2000. Both decide
	// how much of a history is pinned versus summarized, so a single pair
	// asserts monotonicity only for one shape of that split -- and the shapes
	// where it is hardest to hold are the extremes: a tail so large almost
	// nothing is summarized, or a window so tight the summarizer chunks.
	// Varied per trial rather than as subtests so the trial floor below still
	// counts a single population.
	keepRecents := []int{1, 3, 8}
	windows := []int{800, 2000, 8000}
	trial := -1
	summarized := 0
	runGeneratedProperty(t, trials, 60, func(t *testing.T, msgs []*schema.Message) {
		trial++
		rs := &recordingSummarizer{Return: "compacted summary"}
		result, err := Run(context.Background(), msgs, PlanOpts{KeepRecent: keepRecents[trial%len(keepRecents)]}, RunOpts{
			ModelWindow:      windows[trial%len(windows)],
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

// TestMaybeCompactDeclinesBeforeSpendingACall pins every precondition
// MaybeCompact checks before it is willing to call the summariser.
//
// All three were unpinned inside this package: round 25 replaced the
// threshold comparison with a constant, then dropped the threshold <= 0 term,
// and the package stayed green both times. The pre-turn path is the one that
// runs on every user message, so a gate that stops working there spends a
// model call per turn on a history that did not need compacting.
//
// Each subtest asserts the summariser was never invoked, not merely that the
// history came back unchanged: MaybeCompact returns the input on failure too,
// so an unchanged history alone cannot tell a declined compaction from a
// wasted one.
func TestMaybeCompactDeclinesBeforeSpendingACall(t *testing.T) {
	history := func(n int) []*schema.Message {
		msgs := make([]*schema.Message, n)
		for i := range msgs {
			msgs[i] = &schema.Message{Role: schema.Assistant, Content: strings.Repeat("x", 400)}
		}
		return msgs
	}

	cases := []struct {
		name                     string
		threshold                float64
		window, keepRecent, len_ int
		why                      string
	}{
		{"under threshold", 0.9, 100000, 2, 12,
			"a history far below threshold*window must not be summarised"},
		{"threshold zero", 0, 1000, 2, 12,
			"threshold 0 switches compaction off; it does not mean every history qualifies"},
		{"window zero", 0.5, 0, 2, 12,
			"a zero window makes every threshold zero, so nothing can be judged over it"},
		// 9 messages with keepRecent 4 is exactly the guard's boundary
		// (9 <= 4*2+1). Fewer would let Plan pin everything and decline on its
		// own, which is what an earlier version of this case did -- it passed
		// with the guard deleted because the guard was never what stopped it.
		{"at the too-few-messages boundary", 0.0001, 1000, 4, 9,
			"a history no longer than the pinned tail plus one is not worth a model call"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rs := &recordingSummarizer{Return: "summary"}
			msgs := history(tc.len_)
			out, _, _, did := MaybeCompact(context.Background(), msgs,
				tc.threshold, tc.window, tc.keepRecent, rs, nil)

			if did {
				t.Fatalf("%s: compaction reported as done", tc.why)
			}
			if len(rs.GenerateCalls)+len(rs.StreamCalls) != 0 {
				t.Fatalf("%s: the summariser was called anyway", tc.why)
			}
			if len(out) != len(msgs) {
				t.Fatalf("%s: the history was altered", tc.why)
			}
		})
	}
}
