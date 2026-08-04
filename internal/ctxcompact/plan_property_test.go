package ctxcompact

import (
	"math/rand/v2"
	"testing"

	"github.com/cloudwego/eino/schema"
)

const propSeed = 42

func planPropertyGen(t *testing.T, numTrials, maxLen int, fn func(t *testing.T, msgs []*schema.Message)) {
	t.Helper()
	for trial := 0; trial < numTrials; trial++ {
		seed := uint64(propSeed*1000 + trial)
		rng := rand.New(rand.NewPCG(seed, 0))
		n := rng.IntN(maxLen) + 5
		msgs := genHistory(rng, n)
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

// skipAlreadyCompacted is the precondition shared by the three pairing
// properties: Plan short-circuits on a history that already ends in a summary
// (bug⑦) and pins every index including orphans, so the pairing invariant does
// not apply to those trials.
//
// It reads the GENERATED INPUT (genHistory appends a sentinel with p=0.15),
// never Plan's output — the guard must stay independent of the code under test
// so a broken EnforceToolCallPairs cannot skip the whole suite into a vacuous
// pass. It also fails outright if the skip rate is high enough that the
// property is no longer really being exercised: see minPairingTrials.
func skipAlreadyCompacted(t *testing.T, msgs []*schema.Message) {
	t.Helper()
	if lastMessageIsSummary(msgs) {
		t.Skip("history ends with a summary: Plan short-circuits, pairing invariant N/A")
	}
}

// runPairingProperty runs fn over numTrials generated histories, skipping the
// already-compacted ones, and fails if fewer than 60% of trials actually
// executed. The floor is the machine check for the "vacuous property" failure
// mode: a property whose precondition is (accidentally or deliberately) never
// satisfiable still reports PASS from `go test`, because a test binary that
// skips every subtest exits 0. Asserting on the executed count turns that
// silence into a red build.
func runPairingProperty(t *testing.T, numTrials, maxLen int, fn func(t *testing.T, msgs []*schema.Message)) {
	t.Helper()
	executed := 0
	planPropertyGen(t, numTrials, maxLen, func(t *testing.T, msgs []*schema.Message) {
		skipAlreadyCompacted(t, msgs)
		executed++
		fn(t, msgs)
	})
	if min := minPairingTrials(numTrials); executed < min {
		t.Fatalf("only %d/%d trials executed (need ≥%d): the property is vacuous, "+
			"not passing — check the precondition guard", executed, numTrials, min)
	}
}

// minPairingTrials is the executed-trial floor for a pairing property: 60% of
// the requested trials. genHistory appends the already-compacted sentinel with
// probability 0.15, so the natural skip rate sits near that; 60% leaves room
// for generator drift while still catching a guard that swallows everything.
func minPairingTrials(numTrials int) int {
	return numTrials * 60 / 100
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
func TestProperty_PinSetIsSubsetOfOutput(t *testing.T) {
	planPropertyGen(t, 50, 60, func(t *testing.T, msgs []*schema.Message) {
		plan := Plan(msgs, PlanOpts{KeepRecent: 3})
		if len(plan.SummarizeIndices) == 0 {
			for _, i := range plan.PinnedIndices {
				if i < 0 || i >= len(msgs) {
					t.Fatalf("PinnedIndices[%d]=%d out of bounds", i, plan.PinnedIndices[i])
				}
			}
			return
		}
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
}

// TestProperty_ToolCallPairingFixpointHolds is the core pairing invariant: in
// Plan's pinned set, every pinned tool_call has its tool_result pinned too and
// vice versa. A history that keeps one half of a pair is rejected outright by
// several providers, so this invariant is what makes compaction safe to run
// mid-turn at all.
func TestProperty_ToolCallPairingFixpointHolds(t *testing.T) {
	runPairingProperty(t, 50, 60, func(t *testing.T, msgs []*schema.Message) {
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
func TestProperty_ToolCallPairFixpointIsIdempotent(t *testing.T) {
	runPairingProperty(t, 30, 60, func(t *testing.T, msgs []*schema.Message) {
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
// end for the same reason runPairingProperty asserts its executed count: a
// property that silently returns from every trial is vacuous, and `go test`
// cannot tell that apart from a genuine pass.
func TestProperty_ToolCallPairFixpointRepairsCorruption(t *testing.T) {
	injected := 0
	runPairingProperty(t, 30, 60, func(t *testing.T, msgs []*schema.Message) {
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
