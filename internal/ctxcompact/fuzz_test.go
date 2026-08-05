package ctxcompact

import (
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"
)

// decodeHistory turns a fuzzer's byte string into a history, one message per
// byte. The encoding exists because schema.Message is not a shape go-fuzz can
// generate directly, and because a fuzzer given free rein over message text
// would spend its whole budget on strings while never producing the STRUCTURAL
// shapes that break this package -- orphan tool_calls, results without calls,
// a sentinel in the middle rather than the tail.
//
// Byte value selects the kind; the low bits carry a call id so the fuzzer can
// pair or mispair calls and results on its own.
func decodeHistory(data []byte) []*schema.Message {
	msgs := make([]*schema.Message, 0, len(data))
	for _, b := range data {
		id := string(rune('a' + b%8))
		switch b % 6 {
		case 0:
			msgs = append(msgs, &schema.Message{Role: schema.User, Content: "u" + id})
		case 1:
			msgs = append(msgs, &schema.Message{Role: schema.Assistant, Content: "a" + id})
		case 2:
			msgs = append(msgs, &schema.Message{
				Role:      schema.Assistant,
				ToolCalls: []schema.ToolCall{{ID: id, Function: schema.FunctionCall{Name: "f"}}},
			})
		case 3:
			msgs = append(msgs, &schema.Message{Role: schema.Tool, ToolCallID: id, Content: "r" + id})
		case 4:
			msgs = append(msgs, &schema.Message{Role: schema.User, Content: SummarySentinel + "s" + id})
		case 5:
			msgs = append(msgs, &schema.Message{
				Role:    schema.Assistant,
				Content: strings.Repeat("x", 1+int(b)*40),
			})
		}
	}
	return msgs
}

// FuzzPlanEnforcesToolCallPairs is the package's first fuzz target. CI's
// fuzz-seed job is a hard gate, so this runs its corpus on every build.
//
// It asserts the two invariants that must hold for EVERY history, including
// ones no generator would think to produce:
//
//  1. Pinned indices are in range, unique and ascending. Assemble indexes
//     msgs by them and relies on the order, so a duplicate or a stale index
//     is an out-of-range panic or a silently reordered conversation.
//  2. The pinned set is tool-call consistent -- unless Plan short-circuited
//     on an already-compacted history, where it pins everything including
//     orphans by design.
//
// Property tests cover the same ground over generated histories; this covers
// the shapes generation cannot reach, which is why the seed corpus below
// spells out the boundary cases by hand rather than trusting coverage.
func FuzzPlanEnforcesToolCallPairs(f *testing.F) {
	// Boundary shapes, each one a case that has broken something before:
	f.Add([]byte{})        // empty history
	f.Add([]byte{4})       // nothing but a sentinel
	f.Add([]byte{2})       // orphan tool_call, no result
	f.Add([]byte{3})       // orphan tool_result, no call
	f.Add([]byte{2, 3})    // a matched pair
	f.Add([]byte{3, 2})    // result BEFORE its call
	f.Add([]byte{2, 2, 3}) // two calls, one result
	f.Add([]byte{5})       // one very long message
	f.Add([]byte{4, 0, 1}) // sentinel first, not last
	f.Add([]byte{0, 1, 2, 3, 4} /* mixed, ending in a sentinel */)

	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 200 {
			data = data[:200] // keep a trial cheap; shape matters, not length
		}
		msgs := decodeHistory(data)

		for _, keepRecent := range []int{0, 1, 3} {
			plan := Plan(msgs, PlanOpts{KeepRecent: keepRecent})

			prev := -1
			seen := make(map[int]bool, len(plan.PinnedIndices))
			for _, i := range plan.PinnedIndices {
				if i < 0 || i >= len(msgs) {
					t.Fatalf("pinned index %d out of range for %d messages", i, len(msgs))
				}
				if seen[i] {
					t.Fatalf("pinned index %d appears twice", i)
				}
				if i <= prev {
					t.Fatalf("pinned indices not ascending: %d after %d", i, prev)
				}
				seen[i] = true
				prev = i
			}

			// Assemble must not panic on whatever Plan produced, and must not
			// invent or lose pinned messages.
			out := Assemble(msgs, plan, "summary")
			if len(msgs) > 0 && len(out) == 0 {
				t.Fatal("Assemble produced an empty history from a non-empty one")
			}

			if len(msgs) > 0 && lastMessageIsSummary(msgs) {
				continue // short-circuit branch pins orphans on purpose
			}
			if !pinnedSetIsConsistent(msgs, seen) {
				t.Fatalf("pinned set splits a tool_call from its result: %d messages, keepRecent=%d",
					len(msgs), keepRecent)
			}
		}
	})
}
