package ctxcompact

import (
	"testing"

	"github.com/cloudwego/eino/schema"
	"github.com/stretchr/testify/assert"
)

func TestEstimateTokens_CountsContentAndOverhead(t *testing.T) {
	// 8 chars -> 8/4 + 8 = 10
	assert.Equal(t, 10, EstimateTokens([]*schema.Message{{Content: "12345678"}}))
}

func TestEstimateTokens_CountsToolCalls(t *testing.T) {
	// bug⑤ regression: ToolCalls arguments must be counted, else ReAct loops undercount
	msg := &schema.Message{
		Role:    schema.Assistant,
		Content: "I'll read the file", // 18 chars -> 18/4+8 = 12
		ToolCalls: []schema.ToolCall{
			{ID: "call_1", Function: schema.FunctionCall{Name: "read_file", Arguments: `{"path":"internal/llm/eino/compacting.go"}`}},
		},
	}
	n := EstimateTokens([]*schema.Message{msg})
	assert.Greater(t, n, 12, "toolcall args must add tokens beyond bare content")
	assert.GreaterOrEqual(t, n, 40, "estimated ~42")
}

func TestEstimateTokens_CountsReasoning(t *testing.T) {
	msg := &schema.Message{
		Role:             schema.Assistant,
		ReasoningContent: "thinking " + string(make([]byte, 40)), // ~50 chars -> 12 tokens
	}
	n := EstimateTokens([]*schema.Message{msg})
	assert.Greater(t, n, 8, "reasoning content must add tokens")
}

// TestEstimateMessageTokens_Nil covers nil message returning 0.
func TestEstimateMessageTokens_Nil(t *testing.T) {
	assert.Equal(t, 0, estimateMessageTokens(nil))
}
