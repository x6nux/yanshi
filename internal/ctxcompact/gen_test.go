package ctxcompact

import (
	"math/rand/v2"
	"testing"

	"github.com/cloudwego/eino/schema"
)

// ---------- random history generator (P1-P5 share this) ----------

// genHistory produces n messages with a fixed seed for reproducibility.
func genHistory(rng *rand.Rand, n int) []*schema.Message {
	msgs := make([]*schema.Message, 0, n)
	openCalls := make([]string, 0)
	for i := 0; i < n; i++ {
		roll := rng.Float64()
		switch {
		case roll < 0.35:
			msg := &schema.Message{Role: schema.User, Content: randomContent(rng, 20, 80)}
			if rng.Float64() < 0.1 {
				msg.ToolCallID = "orphan-result-" + randomID(rng)
			}
			msgs = append(msgs, msg)
		case roll < 0.65 && i < n-1:
			nCalls := rng.IntN(3) + 1
			calls := make([]schema.ToolCall, 0, nCalls)
			for j := 0; j < nCalls; j++ {
				id := "call-" + randomID(rng)
				calls = append(calls, schema.ToolCall{
					ID: id,
					Function: schema.FunctionCall{
						Name:      randomToolName(rng),
						Arguments: `{"x":1}`,
					},
				})
				if rng.Float64() < 0.6 {
					openCalls = append(openCalls, id)
				}
			}
			msgs = append(msgs, &schema.Message{Role: schema.Assistant, ToolCalls: calls})
		case roll < 0.9 && len(openCalls) > 0:
			callID := openCalls[rng.IntN(len(openCalls))]
			filtered := make([]string, 0, len(openCalls)-1)
			for _, id := range openCalls {
				if id != callID {
					filtered = append(filtered, id)
				}
			}
			openCalls = filtered
			msgs = append(msgs, &schema.Message{Role: schema.Tool, ToolCallID: callID, Content: randomContent(rng, 10, 40)})
		case roll < 0.95:
			if rng.Float64() < 0.5 {
				msgs = append(msgs, &schema.Message{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{ID: "orphan-" + randomID(rng), Function: schema.FunctionCall{Name: "orphan-tool", Arguments: "{}"}}}})
			} else {
				msgs = append(msgs, &schema.Message{Role: schema.Tool, ToolCallID: "missing-call-" + randomID(rng), Content: "orphan result"})
			}
		default:
			// working-set path / error / diff marker
			switch rng.IntN(3) {
			case 0:
				msgs = append(msgs, &schema.Message{Role: schema.Assistant, Content: "see D:/code/foo.go for details"})
			case 1:
				msgs = append(msgs, &schema.Message{Role: schema.Assistant, Content: "Error: something went wrong"})
			case 2:
				msgs = append(msgs, &schema.Message{Role: schema.Assistant, Content: "diff: --git a/main.go b/main.go"})
			}
		}
	}
	if n > 0 && rng.Float64() < 0.15 {
		msgs = append(msgs, &schema.Message{Role: schema.User, Content: SummarySentinel + "prior summary"})
	}
	return msgs
}

// genAdversarialHistory produces the two shapes genHistory structurally cannot
// reach, both of which make a single summarizer chunk exceed the model window
// by an amount that has nothing to do with the window size:
//
//  1. A PARALLEL TOOL GROUP — one assistant message carrying N tool_calls
//     followed immediately by all N results. splitIsSafe scans the whole left
//     side for a matching call, so EVERY interior split point of such a group
//     is unsafe and takeChunk must emit the entire group as one chunk. The
//     resulting chunk is "one call message + the sum of its results", which
//     grows LINEARLY IN N and is unbounded in units of the window. This is not
//     an exotic shape: orchestrator classify.go emits exactly it, one result
//     per tool_call, for every parallel tool turn.
//
//  2. A SINGLE OVERSIZED MESSAGE — takeChunk's budget test is
//     `i > 0 && …`, so index 0 is never budget-checked (it cannot be, or the
//     function would return an empty chunk and RunSummary would spin). One
//     message larger than the window therefore over-runs it ALL BY ITSELF,
//     with no tool pair involved at all.
//
// genHistory reaches neither: its contents are 10-80 bytes (≈2-20 tokens) and
// it emits at most 3 tool_calls per assistant message, with each result
// appended independently at p=0.6 rather than adjacently. Measured against a
// 1000-token budget, the shapes below produce chunks at 5x and 10x the window
// where genHistory never clears 1.1x — which is why the "< 2x" bound survived
// review for as long as it did.
//
// SCOPE, and why it is not "wire it into every property". The obvious lesson
// from runGeneratedProperty's history is that a narrow scope is usually the
// bug, so broadening was tried first and MEASURED: passing this generator to
// all eight properties reddens three of them on their inner trial floors —
// TestProperty_NoDoubleCompaction, TestProperty_RunReducesTokens and
// TestProperty_NoEmptySummaryMessage each drop to 17/30 against a floor of 18.
// The cause is real, not a flake: these histories are deliberately short and
// heavy, so Plan pins most of them and leaves nothing to summarize, and those
// floors are calibrated to genHistory's distribution. Relaxing three floors to
// admit a generator they were not written for would trade a measured guarantee
// for an unmeasured one. So the shapes go where the bound they falsify lives —
// the chunking property — and this paragraph records the number, so the next
// reviewer re-deciding this has the measurement instead of the intuition.
func genAdversarialHistory(rng *rand.Rand, n int) []*schema.Message {
	// Each iteration can emit up to 22 messages, so the caller's maxLen (tuned
	// for genHistory's one-message-per-iteration) is clamped here rather than
	// at the call site — otherwise a 60-message request becomes ~1300.
	if n > 6 {
		n = rng.IntN(6) + 1
	}
	msgs := make([]*schema.Message, 0, n)
	for i := 0; i < n; i++ {
		switch rng.IntN(3) {
		case 0:
			// Parallel tool group: one call message, then every result.
			nCalls := rng.IntN(20) + 2
			calls := make([]schema.ToolCall, 0, nCalls)
			ids := make([]string, 0, nCalls)
			for j := 0; j < nCalls; j++ {
				id := "par-" + randomID(rng)
				ids = append(ids, id)
				calls = append(calls, schema.ToolCall{
					ID:       id,
					Function: schema.FunctionCall{Name: randomToolName(rng), Arguments: `{"x":1}`},
				})
			}
			msgs = append(msgs, &schema.Message{Role: schema.Assistant, ToolCalls: calls})
			for _, id := range ids {
				msgs = append(msgs, &schema.Message{
					Role:       schema.Tool,
					ToolCallID: id,
					Content:    randomContent(rng, 400, 1200),
				})
			}
		case 1:
			// Single oversized message, no pair involved.
			msgs = append(msgs, &schema.Message{
				Role:    schema.Assistant,
				Content: randomContent(rng, 4000, 12000),
			})
		default:
			msgs = append(msgs, &schema.Message{Role: schema.User, Content: randomContent(rng, 20, 80)})
		}
	}
	return msgs
}

func randomContent(rng *rand.Rand, minLen, maxLen int) string {
	n := rng.IntN(maxLen-minLen+1) + minLen
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(rng.IntN(26) + 'a')
	}
	return string(b)
}

func randomID(rng *rand.Rand) string {
	return randomContent(rng, 8, 16)
}

func randomToolName(rng *rand.Rand) string {
	tools := []string{"read", "write", "search", "shell_run", "web_fetch", "fs_edit"}
	return tools[rng.IntN(len(tools))]
}

func TestGenHistory_DeterministicWithFixedSeed(t *testing.T) {
	rng1 := rand.New(rand.NewPCG(42, 0))
	rng2 := rand.New(rand.NewPCG(42, 0))
	h1 := genHistory(rng1, 50)
	h2 := genHistory(rng2, 50)
	if len(h1) != len(h2) {
		t.Fatalf("length mismatch: %d vs %d", len(h1), len(h2))
	}
	for i := range h1 {
		if (h1[i].Role != h2[i].Role) ||
			(h1[i].Content != h2[i].Content) ||
			(len(h1[i].ToolCalls) != len(h2[i].ToolCalls)) ||
			(h1[i].ToolCallID != h2[i].ToolCallID) {
			t.Fatalf("msg[%d] mismatch: %#v vs %#v", i, h1[i], h2[i])
		}
	}
}

func TestGenHistory_ProducesDiverseRoles(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 0))
	h := genHistory(rng, 200)
	seen := map[schema.RoleType]int{}
	for _, m := range h {
		seen[m.Role]++
	}
	if len(seen) < 2 {
		t.Fatalf("genHistory should produce at least 2 roles, got %v", seen)
	}
}
