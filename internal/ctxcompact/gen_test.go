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
