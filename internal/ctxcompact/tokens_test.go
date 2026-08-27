package ctxcompact

import (
	"testing"

	"github.com/cloudwego/eino/schema"
	"github.com/stretchr/testify/assert"
)

// TestEstimateTokens_CountsContentAndOverhead pins the per-message envelope
// cost on top of the content estimate.
//
// It asserts the DECOMPOSITION rather than a literal, because the literal is
// the estimator's calibration and this test is not about calibration. The old
// form asserted `== 10`, derived by hand from chars/4 + 8; when C8 replaced
// chars/4 with the structural estimator the test failed for a change that was
// entirely intended, and the only available repair was to write down whatever
// number the new code produced — which asserts that the code equals itself.
// Asserting content + perMessageOverhead instead keeps a real claim (the
// envelope is charged exactly once per message, and it is not free) that
// survives every future recalibration and still fails if the overhead is
// dropped or double-counted.
func TestEstimateTokens_CountsContentAndOverhead(t *testing.T) {
	const content = "12345678"
	got := EstimateTokens([]*schema.Message{{Content: content}})
	assert.Equal(t, estimateTextTokens(content)+perMessageOverhead, got)
	assert.Greater(t, got, estimateTextTokens(content),
		"the message envelope must cost something; a free envelope undercounts every history by 8 tokens per message")
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
