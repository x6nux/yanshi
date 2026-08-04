package ctxcompact

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

// recordingSummarizer implements ModelSummarizer, recording every Generate/Stream call.
type recordingSummarizer struct {
	GenerateCalls [][]*schema.Message
	StreamCalls   [][]*schema.Message
	Return        string
	ReturnErr     error
}

func (r *recordingSummarizer) Generate(ctx context.Context, msgs []*schema.Message, opts ...model.Option) (*schema.Message, error) {
	r.GenerateCalls = append(r.GenerateCalls, msgs)
	if r.ReturnErr != nil {
		return nil, r.ReturnErr
	}
	return &schema.Message{Role: schema.User, Content: r.Return}, nil
}

func (r *recordingSummarizer) Stream(ctx context.Context, msgs []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	r.StreamCalls = append(r.StreamCalls, msgs)
	if r.ReturnErr != nil {
		return nil, r.ReturnErr
	}
	return schema.StreamReaderFromArray[*schema.Message]([]*schema.Message{
		{Role: schema.User, Content: r.Return},
	}), nil
}

// ---------- P3: each summary call's input is bounded ----------

// atomicBoundaryIsSafe reports whether msgs may be cut between i-1 and i
// without separating a tool_call from its result.
//
// It is written from the generated tool ids directly instead of calling the
// production splitIsSafe. That duplication is the point: this predicate is the
// ORACLE for takeChunk's bound, and an oracle that shares a helper with the
// code under test degrades in lockstep with it — gut splitIsSafe to
// `return true` and a delegating oracle would shrink its own bound by exactly
// as much as takeChunk shrinks its chunks, and the property would stay green
// while pairs were being severed.
func atomicBoundaryIsSafe(msgs []*schema.Message, i int) bool {
	if i <= 0 || i >= len(msgs) {
		return true
	}
	left, right := msgs[i-1], msgs[i]
	if left != nil {
		for _, tc := range left.ToolCalls {
			if tc.ID == "" {
				continue
			}
			for j := i; j < len(msgs); j++ {
				if msgs[j] != nil && msgs[j].ToolCallID == tc.ID {
					return false
				}
			}
		}
	}
	if right != nil && right.ToolCallID != "" {
		for j := 0; j < i; j++ {
			if msgs[j] == nil {
				continue
			}
			for _, tc := range msgs[j].ToolCalls {
				if tc.ID == right.ToolCallID {
					return false
				}
			}
		}
	}
	return true
}

// maxAtomicGroupTokens returns the token size of the largest INDIVISIBLE run in
// msgs: the largest block between two consecutive safe cut points. It is the
// real, input-derived ceiling on how far one chunk can overshoot its budget,
// because a chunk can never be smaller than the atomic group it starts in and
// takeChunk stops at the first safe point past the budget.
//
// Note what this quantity does NOT depend on: the model window. That is the
// whole finding — see the doc on TestProperty_EachSummaryCallWithinWindow.
func maxAtomicGroupTokens(msgs []*schema.Message) int {
	maxTok, cur := 0, 0
	for i := range msgs {
		if i > 0 && atomicBoundaryIsSafe(msgs, i) {
			if cur > maxTok {
				maxTok = cur
			}
			cur = 0
		}
		cur += estimateMessageTokens(msgs[i])
	}
	if cur > maxTok {
		maxTok = cur
	}
	return maxTok
}

// TestProperty_EachSummaryCallWithinWindow is a third, independent property,
// and the one that constrains the chunking path: it bounds how many tokens a
// single summarizer call may be handed. Over-window summary calls are how a
// compaction meant to rescue a turn kills it instead.
//
// THE BOUND IS NOT "< 2x THE WINDOW". It used to be asserted that way, and
// that number was wrong in both of its clauses:
//
//   - "the only over-run comes from tool pair integrity" is false. takeChunk's
//     budget test is `i > 0 && tok+mt > budget && splitIsSafe(...)`; index 0 is
//     never budget-checked, because a chunk that came back empty would make
//     RunSummary loop forever. One message bigger than the window therefore
//     over-runs it with no pair in sight — measured at 10.01x for a single
//     oversized message against a 1000-token budget.
//   - "< 2x" is false. splitIsSafe scans the entire left side for a matching
//     call, so in `[call(id1..idN), r1..rN]` EVERY interior cut point is
//     unsafe and the whole group must ship as one chunk. Measured against a
//     1000-token budget: 1.03x at N=1, 5.13x at N=5, 20.49x at N=20. The ratio
//     tracks the parallel tool count, not the window.
//
// The true ceiling is `ModelWindow + <the largest indivisible run in the
// history>`, which is what this property now asserts via maxAtomicGroupTokens.
// It still has teeth: a takeChunk that ignored its budget entirely, or that
// carried a whole multi-group history into one call, blows past it. What it
// deliberately does NOT do is pretend the excess is window-relative.
//
// Why the bound was not tightened in the code instead: the minimum legal chunk
// is "the atomic group at the head of what remains", and its size is a
// property of the INPUT. Capping below it would have to either sever a pair
// (the provider answers 400, which is what the whole carry loop exists to
// avoid) or truncate message content mid-chunk (silent information loss, plus
// a second budget to get wrong). Both are worse than a call that is large but
// well-formed, so the bound is documented rather than enforced. See
// docs/compaction.md.
//
// The generators matter as much as the assertion. genHistory alone cannot
// produce either shape above — its messages are 10-80 bytes and its tool
// results are scattered, so it never clears ~1.1x — which is exactly how a
// "< 2x" invariant stayed green without ever being tested. genAdversarialHistory
// is round-robined in alongside it, and overWindowCalls below fails the whole
// property if not one trial ever exercised the over-window branch: a tolerance
// nothing reaches is indistinguishable from no tolerance at all.
//
// Failures are NOT swallowed. The error branch used to be `t.Logf(...);
// return`, which made the property hold for a RunSummary that fails every
// time: gutting it to `return "", err` produced four log lines and a pass.
// Only ErrNoWindowRoom is tolerated (a window too small to make any progress
// is a real outcome, and window=100 hits it), only some of the windows may
// take that branch, and a success must be followed by at least one real
// summarizer call — three independent ways for a broken RunSummary to redden.
//
// It is the one property here whose shape is genuinely two-dimensional — the
// bound is per WINDOW SIZE as well as per history — so it nests: the window
// sweep is the outer loop and runGeneratedProperty supplies the histories
// inside each window. Its own inline floor ("all but the smallest window must
// summarize something") survives on top of the shared one, because the two
// catch different degradations: the shared floor catches a generator or guard
// that stops producing trials, this one catches a RunSummary that answers
// ErrNoWindowRoom for everything.
//
// ledger: E2/PROP1#1 ≥3 个属性
func TestProperty_EachSummaryCallWithinWindow(t *testing.T) {
	windows := []int{800, 400, 200, 100}
	const trials = 20
	windowsSummarized := 0
	beyondOldBound := 0
	for _, mw := range windows {
		summarized := 0
		t.Run(fmt.Sprintf("window=%d", mw), func(t *testing.T) {
			opts := RunOpts{
				ModelWindow:      mw,
				ChunkThreshold:   0.9,
				SummaryWordLimit: 200,
			}
			runGeneratedProperty(t, trials, 60, func(t *testing.T, msgs []*schema.Message) {
				rs := &recordingSummarizer{Return: "summarized"}
				_, err := RunSummary(context.Background(), msgs, opts, rs, nil)
				if err != nil {
					if !errors.Is(err, ErrNoWindowRoom) {
						t.Fatalf("RunSummary failed for a reason this property does not "+
							"tolerate (only ErrNoWindowRoom is): %v", err)
					}
					t.Logf("window %d is too small for the carry loop to make progress: %v", mw, err)
					return
				}
				summarized++
				allCalls := append(rs.GenerateCalls, rs.StreamCalls...)
				if len(allCalls) == 0 {
					t.Fatal("RunSummary returned success but summarizer was never called")
				}
				atomic := maxAtomicGroupTokens(msgs)
				limit := mw + atomic
				for i, callMsgs := range allCalls {
					tok := EstimateTokens(callMsgs)
					if tok > limit {
						t.Fatalf("call[%d] tok=%d exceeds ModelWindow=%d + largest indivisible "+
							"run=%d. The over-run is no longer attributable to an unsplittable "+
							"group, so takeChunk is over-running its budget gratuitously.",
							i, tok, mw, atomic)
					}
					if tok > 2*mw {
						beyondOldBound++
					}
					if tok > mw {
						t.Logf("call[%d] tok=%d exceeds window=%d by %.2fx (attributable: "+
							"largest indivisible run is %d tok)", i, tok, mw, float64(tok)/float64(mw), atomic)
					}
				}
			}, genHistory, genAdversarialHistory)
		})
		if summarized > 0 {
			windowsSummarized++
		}
	}
	// A RunSummary gutted to return ErrNoWindowRoom itself would slip past the
	// branch above; it does not slip past here. Only the smallest window is
	// allowed to be unsummarizable.
	if want := len(windows) - 1; windowsSummarized < want {
		t.Fatalf("only %d of %d windows produced summarizer calls (want >=%d): the "+
			"in-window bound was asserted against almost nothing", windowsSummarized, len(windows), want)
	}
	// The tolerance above is only meaningful if something reaches it, and
	// "exceeded the window at all" is too weak a floor to prove that: genHistory
	// on its own clears 1.01x now and then, so dropping the adversarial
	// generator left an `overWindowCalls > 0` floor perfectly green. The floor
	// is therefore set at the OLD, disproven bound: at least one call must
	// exceed 2x its window, which is precisely the assertion the previous
	// version of this test made and never once exercised. If this fails, either
	// genAdversarialHistory stopped being passed to runGeneratedProperty or its
	// shapes stopped reaching RunSummary.
	if beyondOldBound == 0 {
		t.Fatal("no summarizer call exceeded 2x its window across the whole sweep: the " +
			"shapes that disprove the old \"< 2x window\" bound are not reaching " +
			"RunSummary, so this property is once again asserting a ceiling nothing " +
			"tests. Check that genAdversarialHistory is still passed to " +
			"runGeneratedProperty.")
	}
}

// ---------- P4: no empty summary when summarizer returns empty ----------

// TestProperty_NoEmptySummaryMessage is the fourth property: a summarizer that
// returns an empty string must not produce an empty history or a tail that is
// not a summary message. Assemble's sentinel prefix is what the pre-turn path
// uses to recognise an already-compacted history, so losing it turns one
// degenerate model reply into repeated re-compaction.
//
// It used to run against a SINGLE fixed history and `t.Skip` on
// `len(plan.SummarizeIndices) == 0` — a precondition read straight off the code
// under test. A Plan gutted to `return &PlanResult{}` satisfied it on every run:
//
//	summarize_property_test.go:124: all messages pinned — nothing to summarize
//	--- SKIP: TestProperty_NoEmptySummaryMessage (0.00s)
//
// `go test` reports SKIP as success, so the ledger's evidence proved nothing.
// The precondition is unavoidable — whether Plan finds anything to summarize is
// not computable from the generated input alone — so it is kept and COUNTED,
// which is the concession requireTrialFloor exists to keep honest.
func TestProperty_NoEmptySummaryMessage(t *testing.T) {
	const trials = 30
	summarizable := 0
	planOpts := PlanOpts{KeepRecent: 3}
	opts := RunOpts{
		ModelWindow:      1000,
		ChunkThreshold:   0.9,
		SummaryWordLimit: 200,
	}
	runGeneratedProperty(t, trials, 60, func(t *testing.T, msgs []*schema.Message) {
		plan := Plan(msgs, planOpts)
		if len(plan.SummarizeIndices) == 0 {
			return // nothing to summarize in this trial; the floor below covers it
		}
		summarizable++

		rs := &recordingSummarizer{Return: ""}
		result, err := Run(context.Background(), msgs, planOpts, opts, rs, nil)
		if err != nil {
			t.Fatalf("Run must not error with empty summarizer output: %v", err)
		}
		if len(result.Messages) == 0 {
			t.Fatal("Run must produce at least the summary message")
		}
		// Even with empty summarizer return, Assemble always appends a
		// sentinel-prefixed message — the output is never truly empty.
		last := result.Messages[len(result.Messages)-1]
		if !IsSummaryMessage(last) {
			t.Fatal("last message must be a summary message")
		}
	})
	requireTrialFloor(t, "had something to summarize", summarizable, trials)
}
