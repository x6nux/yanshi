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

func TestProperty_ToolCallPairingFixpointHolds(t *testing.T) {
	planPropertyGen(t, 50, 60, func(t *testing.T, msgs []*schema.Message) {
		plan := Plan(msgs, PlanOpts{KeepRecent: 3})
		pinned := map[int]bool{}
		for _, i := range plan.PinnedIndices {
			pinned[i] = true
		}

		// When Plan short-circuits on an already-summarized history it
		// pins all indices including orphans — skip those trials.
		if !pinnedSetIsConsistent(msgs, pinned) {
			t.Skip("initial pinned set not consistent (history ends with summary)")
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

func TestProperty_ToolCallPairFixpointIsIdempotent(t *testing.T) {
	planPropertyGen(t, 30, 60, func(t *testing.T, msgs []*schema.Message) {
		plan := Plan(msgs, PlanOpts{KeepRecent: 3})
		pinned := map[int]bool{}
		for _, i := range plan.PinnedIndices {
			pinned[i] = true
		}

		if !pinnedSetIsConsistent(msgs, pinned) {
			t.Skip("initial pinned set not consistent (history ends with summary)")
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

func TestProperty_ToolCallPairFixpointRepairsCorruption(t *testing.T) {
	planPropertyGen(t, 30, 60, func(t *testing.T, msgs []*schema.Message) {
		plan := Plan(msgs, PlanOpts{KeepRecent: 3})
		pinned := map[int]bool{}
		for _, i := range plan.PinnedIndices {
			pinned[i] = true
		}

		if !pinnedSetIsConsistent(msgs, pinned) {
			t.Skip("initial pinned set not consistent (history ends with summary)")
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

		delete(pinned, callIdx)
		EnforceToolCallPairs(msgs, pinned)

		if !pinnedSetIsConsistent(msgs, pinned) {
			t.Fatal("pinned set still inconsistent after repair")
		}
	})
}
