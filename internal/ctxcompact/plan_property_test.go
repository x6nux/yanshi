package ctxcompact

import (
	"math/rand/v2"
	"testing"

	"github.com/cloudwego/eino/schema"
)

const propSeed = 42

// historyGen builds one trial's history from a seeded rng.
type historyGen func(rng *rand.Rand, n int) []*schema.Message

func planPropertyGen(t *testing.T, numTrials, maxLen int, gens []historyGen, fn func(t *testing.T, msgs []*schema.Message)) {
	t.Helper()
	if len(gens) == 0 {
		gens = []historyGen{genHistory}
	}
	for trial := 0; trial < numTrials; trial++ {
		seed := uint64(propSeed*1000 + trial)
		rng := rand.New(rand.NewPCG(seed, 0))
		n := rng.IntN(maxLen) + 5
		msgs := gens[trial%len(gens)](rng, n)
		t.Run("", func(t *testing.T) {
			fn(t, msgs)
		})
	}
}

// pinnedSetIsConsistent reports whether every tool_call in pinned has its
// matching tool_result also pinned, and every tool_result has its call.
// Plan normally guarantees this via EnforceToolCallPairs, except when it
// short-circuits on lastMessageIsSummary (bug⑦).
//
// This is an ORACLE, never a precondition. Using it as a skip guard is what
// made the three pairing properties vacuous: gutting EnforceToolCallPairs made
// Plan emit inconsistent pin sets, every trial hit the guard, and `go test`
// reported PASS over zero executed trials. Preconditions here must be
// computable from the generated input alone — see skipAlreadyCompacted.
func pinnedSetIsConsistent(msgs []*schema.Message, pinned map[int]bool) bool {
	for i, m := range msgs {
		if !pinned[i] || m == nil {
			continue
		}
		for _, tc := range m.ToolCalls {
			if tc.ID == "" {
				continue
			}
			hasResult := false
			for j, r := range msgs {
				if pinned[j] && r.ToolCallID == tc.ID {
					hasResult = true
					break
				}
			}
			if !hasResult {
				return false
			}
		}
		if m.ToolCallID != "" {
			hasCall := false
			for j, c := range msgs {
				if pinned[j] && c != nil {
					for _, tc := range c.ToolCalls {
						if tc.ID == m.ToolCallID {
							hasCall = true
							break
						}
					}
				}
				if hasCall {
					break
				}
			}
			if !hasCall {
				return false
			}
		}
	}
	return true
}

// skipAlreadyCompacted is the precondition shared by every property in this
// package: Plan short-circuits on a history that already ends in a summary
// (bug⑦) and pins every index including orphans, so neither the pairing
// invariant nor the pin/summarize split applies to those trials.
//
// It reads the GENERATED INPUT (genHistory appends a sentinel with p=0.15),
// never Plan's output — the guard must stay independent of the code under test
// so a broken EnforceToolCallPairs cannot skip the whole suite into a vacuous
// pass. It also fails outright if the skip rate is high enough that the
// property is no longer really being exercised: see minExecutedTrials.
func skipAlreadyCompacted(t *testing.T, msgs []*schema.Message) {
	t.Helper()
	if lastMessageIsSummary(msgs) {
		t.Skip("history ends with a summary: Plan short-circuits, the invariant is N/A")
	}
}

// runGeneratedProperty runs fn over numTrials generated histories, skipping the
// already-compacted ones, and fails if fewer than 60% of trials actually
// executed. The floor is the machine check for the "vacuous property" failure
// mode: a property whose precondition is (accidentally or deliberately) never
// satisfiable still reports PASS from `go test`, because a test binary that
// skips every subtest exits 0. Asserting on the executed count turns that
// silence into a red build.
//
// It was originally named runPairingProperty and wired only into the three
// tool-pair properties. That scoping was the bug: the discipline it encodes is
// not about pairing, and the two properties left outside it (pin-set subset,
// token reduction) went on guarding themselves with the code under test's own
// output and stayed green through every gutting. Anything in this package that
// generates histories goes through here now.
//
// The trailing gens are the generators to round-robin across; omitting them
// means genHistory alone. They exist because genHistory's SHAPE, not just its
// count of trials, can make a property vacuous: it never emits a message
// larger than ~20 tokens and never emits a tool_call's results adjacently, so
// the chunking bound went years asserted only against histories that could not
// violate it. Passing genAdversarialHistory here is how a property opts into
// the shapes that can. The entry stays single — one call site per property —
// so the "properties == entry calls" count in the review checklist holds.
func runGeneratedProperty(t *testing.T, numTrials, maxLen int, fn func(t *testing.T, msgs []*schema.Message), gens ...historyGen) {
	t.Helper()
	executed := 0
	planPropertyGen(t, numTrials, maxLen, gens, func(t *testing.T, msgs []*schema.Message) {
		skipAlreadyCompacted(t, msgs)
		executed++
		fn(t, msgs)
	})
	requireTrialFloor(t, "executed", executed, numTrials)
}

// minExecutedTrials is the trial floor for a generated property: 60% of the
// requested trials. genHistory appends the already-compacted sentinel with
// probability 0.15, so the natural skip rate sits near that; 60% leaves room
// for generator drift while still catching a guard that swallows everything.
func minExecutedTrials(numTrials int) int {
	return numTrials * 60 / 100
}

// requireTrialFloor fails unless got trials of kind actually happened.
//
// runGeneratedProperty applies it to the trials that ran at all. The properties
// that ALSO need an inner precondition — "this trial had something to
// summarize" — apply it a second time to their own counter, because that inner
// guard is the one place a property in this package is still allowed to consult
// the code under test: whether Plan finds anything to summarize is not
// computable from the generated input alone. Counting instead of returning
// silently is what keeps that concession honest — a Plan gutted to an empty
// PlanResult, or a Run that never summarizes, drives the count to zero and
// reddens here rather than passing over trials that asserted nothing.
func requireTrialFloor(t *testing.T, kind string, got, numTrials int) {
	t.Helper()
	if min := minExecutedTrials(numTrials); got < min {
		t.Fatalf("only %d/%d trials %s (need ≥%d): the property is vacuous, not "+
			"passing — the code under test is not reaching the assertions",
			got, numTrials, kind, min)
	}
}

// TestProperty_PinSetIsSubsetOfOutput is one of the suite's distinct
// properties: whatever Plan pins must survive Assemble verbatim, at the front,
// in ascending order. Pointer identity is asserted (not content equality) so a
// "helpful" rewrite of a pinned message fails here rather than silently
// changing what the model was promised it would keep.
//
// It runs 50 trials over randomly generated histories from genHistory, which
// deliberately emits orphan tool_calls and orphan tool_results — the shapes
// hand-written fixtures never think to include.
//
// Both floors matter. The outer one (runGeneratedProperty) counts trials that
// were not skipped as already-compacted; the inner one counts trials that
// actually reached Assemble. Without the inner floor this test passed against
// a Plan gutted to `return &PlanResult{}`: every trial ran, found an empty
// SummarizeIndices, returned before asserting anything, and `go test` printed
// 50 passing subtests over zero assertions.
//
// ledger: E2/PROP1#1 ≥3 个属性
// ledger: E2/PROP1#2 随机输入通过
func TestProperty_PinSetIsSubsetOfOutput(t *testing.T) {
	const trials = 50
	assembled := 0
	runGeneratedProperty(t, trials, 60, func(t *testing.T, msgs []*schema.Message) {
		plan := Plan(msgs, PlanOpts{KeepRecent: 3})
		for _, idx := range plan.PinnedIndices {
			if idx < 0 || idx >= len(msgs) {
				t.Fatalf("PinnedIndices contains %d, out of bounds for %d messages", idx, len(msgs))
			}
		}
		if len(plan.SummarizeIndices) == 0 {
			return
		}
		assembled++
		summary := "property test placeholder summary"
		out := Assemble(msgs, plan, summary)

		if len(out) < len(plan.PinnedIndices) {
			t.Fatalf("Assemble output length %d < pinned count %d", len(out), len(plan.PinnedIndices))
		}
		for i, idx := range plan.PinnedIndices {
			if out[i] != msgs[idx] {
				t.Fatalf("out[%d] != msgs[%d]: pointers differ", i, idx)
			}
		}
		for i := 1; i < len(plan.PinnedIndices); i++ {
			if plan.PinnedIndices[i] <= plan.PinnedIndices[i-1] {
				t.Fatalf("PinnedIndices not ascending at index %d: %d <= %d", i, plan.PinnedIndices[i], plan.PinnedIndices[i-1])
			}
		}
	})
	requireTrialFloor(t, "reached Assemble", assembled, trials)
}

// TestProperty_ToolCallPairingFixpointHolds is the core pairing invariant: in
// Plan's pinned set, every pinned tool_call has its tool_result pinned too and
// vice versa. A history that keeps one half of a pair is rejected outright by
// several providers, so this invariant is what makes compaction safe to run
// mid-turn at all.
//
// ledger: E2/PROP1#3 工具对配对不变量成立
func TestProperty_ToolCallPairingFixpointHolds(t *testing.T) {
	runGeneratedProperty(t, 50, 60, func(t *testing.T, msgs []*schema.Message) {
		plan := Plan(msgs, PlanOpts{KeepRecent: 3})
		pinned := map[int]bool{}
		for _, i := range plan.PinnedIndices {
			pinned[i] = true
		}

		pinnedCallIDs := map[string]bool{}
		for i := range pinned {
			m := msgs[i]
			if m != nil {
				for _, tc := range m.ToolCalls {
					if tc.ID != "" {
						pinnedCallIDs[tc.ID] = true
					}
				}
			}
		}
		pinnedResultIDs := map[string]bool{}
		for i := range pinned {
			m := msgs[i]
			if m != nil && m.ToolCallID != "" {
				pinnedResultIDs[m.ToolCallID] = true
			}
		}
		for callID := range pinnedCallIDs {
			if _, ok := pinnedResultIDs[callID]; !ok {
				t.Fatalf("tool_call %q is pinned but its result is NOT pinned", callID)
			}
		}
		for resultID := range pinnedResultIDs {
			if _, ok := pinnedCallIDs[resultID]; !ok {
				t.Fatalf("tool_result for %q is pinned but its call is NOT pinned", resultID)
			}
		}
	})
}

// TestProperty_ToolCallPairFixpointIsIdempotent is the second angle on the
// same invariant: re-running EnforceToolCallPairs on an already-consistent set
// changes nothing. Without idempotence the "fixpoint" is not one, and the
// pin set could oscillate across the mid-turn/pre-turn compaction paths that
// both call it.
//
// ledger: E2/PROP1#3 工具对配对不变量成立
func TestProperty_ToolCallPairFixpointIsIdempotent(t *testing.T) {
	runGeneratedProperty(t, 30, 60, func(t *testing.T, msgs []*schema.Message) {
		plan := Plan(msgs, PlanOpts{KeepRecent: 3})
		pinned := map[int]bool{}
		for _, i := range plan.PinnedIndices {
			pinned[i] = true
		}
		if !pinnedSetIsConsistent(msgs, pinned) {
			t.Fatalf("Plan produced an inconsistent pin set on a non-compacted history")
		}

		before := make(map[int]bool, len(pinned))
		for k, v := range pinned {
			before[k] = v
		}
		EnforceToolCallPairs(msgs, pinned)
		if len(pinned) != len(before) {
			t.Fatalf("fixpoint not idempotent: %d -> %d", len(before), len(pinned))
		}
		for k := range before {
			if !pinned[k] {
				t.Fatalf("fixpoint not idempotent: lost index %d", k)
			}
		}
	})
}

// TestProperty_ToolCallPairFixpointRepairsCorruption is the third angle: it
// deliberately unpins one half of a pair and requires EnforceToolCallPairs to
// restore consistency, so it exercises the repair path rather than only
// observing that Plan's output happens to be consistent.
//
// A trial only counts once corruption was actually injected — histories with
// no pinned tool_call have nothing to corrupt. `injected` is asserted at the
// end for the same reason runGeneratedProperty asserts its executed count: a
// property that silently returns from every trial is vacuous, and `go test`
// cannot tell that apart from a genuine pass.
//
// The assertion is "at least one", NOT the 60% requireTrialFloor applies
// elsewhere, and the difference is measured rather than assumed: only 8 of 30
// generated histories leave a tool_call pinned after Plan, so a proportional
// floor here fails on correct code. Injectability is a property of the
// generator, not of EnforceToolCallPairs; gutting the code under test does not
// change this count, so a floor is not the mutation defence here — the repair
// assertion inside the trial is.
//
// ledger: E2/PROP1#3 工具对配对不变量成立
func TestProperty_ToolCallPairFixpointRepairsCorruption(t *testing.T) {
	injected := 0
	runGeneratedProperty(t, 30, 60, func(t *testing.T, msgs []*schema.Message) {
		plan := Plan(msgs, PlanOpts{KeepRecent: 3})
		pinned := map[int]bool{}
		for _, i := range plan.PinnedIndices {
			pinned[i] = true
		}
		if !pinnedSetIsConsistent(msgs, pinned) {
			t.Fatalf("Plan produced an inconsistent pin set on a non-compacted history")
		}

		var callIdx int
		foundCall := false
		for idx, m := range msgs {
			if pinned[idx] && len(m.ToolCalls) > 0 {
				callIdx = idx
				foundCall = true
				break
			}
		}
		if !foundCall {
			return
		}
		injected++

		delete(pinned, callIdx)
		EnforceToolCallPairs(msgs, pinned)

		if !pinnedSetIsConsistent(msgs, pinned) {
			t.Fatal("pinned set still inconsistent after repair")
		}
	})
	if injected == 0 {
		t.Fatal("no trial injected corruption: the repair path was never exercised")
	}
}

// ---------- P5: the short-circuit branch has invariants of its own ----------

// TestProperty_AlreadyCompactedHistoryPinsEverything covers the trials every
// other property in this file hands back unexamined.
//
// About 15% of generated histories end in a summary sentinel, and on those
// Plan short-circuits (bug⑦: no summary-of-summary). The other properties
// genuinely do not apply there -- pinning every index including orphans breaks
// the pairing invariant by construction -- so runGeneratedProperty skips them
// centrally. Right call for them; wrong outcome for the package, because the
// branch that fires on every long-running session then has no property at all.
//
// It deliberately does NOT go through runGeneratedProperty: that helper calls
// skipAlreadyCompacted before the body runs, so a property needing exactly
// those histories skips every trial and still reports PASS. A first attempt
// did precisely that -- 200 trials, 200 skips, and breaking the short circuit
// reddened nothing. Hence planPropertyGen plus a floor that counts the trials
// THIS property needs rather than the ones the others need.
func TestProperty_AlreadyCompactedHistoryPinsEverything(t *testing.T) {
	const numTrials = 200
	compacted := 0
	planPropertyGen(t, numTrials, 30, nil, func(t *testing.T, msgs []*schema.Message) {
		if !lastMessageIsSummary(msgs) {
			return // the mirror image of skipAlreadyCompacted
		}
		compacted++
		res := Plan(msgs, PlanOpts{KeepRecent: 2})
		if len(res.PinnedIndices) != len(msgs) {
			t.Fatalf("already-compacted history pinned %d of %d messages: the short "+
				"circuit must keep all of them", len(res.PinnedIndices), len(msgs))
		}
		if len(res.SummarizeIndices) != 0 {
			t.Fatalf("already-compacted history queued %d messages for summary: "+
				"the summary would be summarized again", len(res.SummarizeIndices))
		}
	})
	// The sentinel lands with p=0.15, so ~30 of 200; require a third of that
	// so generator drift is caught long before the property goes vacuous.
	if compacted < 10 {
		t.Fatalf("only %d of %d trials were already-compacted histories: this "+
			"property no longer exercises the short-circuit branch", compacted, numTrials)
	}
}
