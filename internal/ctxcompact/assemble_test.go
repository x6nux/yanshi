package ctxcompact

import (
	"testing"

	"github.com/cloudwego/eino/schema"
	"github.com/stretchr/testify/assert"
)

// bug④ regression: summary must be user+sentinel at the TAIL, not System (which
// would conflict with the orchestrator's own system prompt).
func TestAssemble_SummaryIsUserSentinelAtTail(t *testing.T) {
	msgs := []*schema.Message{
		{Role: schema.User, Content: "first"},
		{Role: schema.Assistant, Content: "noise"},
		{Role: schema.User, Content: "recent"},
	}
	plan := &PlanResult{PinnedIndices: []int{0, 2}}
	out := Assemble(msgs, plan, "the summary text")
	assert.Equal(t, schema.User, out[len(out)-1].Role, "last is user")
	assert.True(t, IsSummaryMessage(out[len(out)-1]), "last is summary sentinel")
	assert.Contains(t, out[len(out)-1].Content, "the summary text")
	// no System role anywhere in the compacted output
	for _, m := range out {
		assert.NotEqual(t, schema.System, m.Role, "no System summary (bug④)")
	}
}

func TestAssemble_PreservesPinnedOrder(t *testing.T) {
	msgs := []*schema.Message{
		{Role: schema.User, Content: "a"},
		{Role: schema.Assistant, Content: "b"},
		{Role: schema.User, Content: "c"},
	}
	plan := &PlanResult{PinnedIndices: []int{0, 2}}
	out := Assemble(msgs, plan, "s")
	assert.Equal(t, "a", out[0].Content)
	assert.Equal(t, "c", out[1].Content)
}
