// internal/ctxcompact/plan_test.go
package ctxcompact

import (
	"testing"

	"github.com/cloudwego/eino/schema"
	"github.com/stretchr/testify/assert"
)

func TestPlan_PinsTail(t *testing.T) {
	msgs := []*schema.Message{
		{Role: schema.User, Content: "a"},
		{Role: schema.Assistant, Content: "b"},
		{Role: schema.User, Content: "c"},
		{Role: schema.Assistant, Content: "d"},
	}
	p := Plan(msgs, PlanOpts{KeepRecent: 1})
	assert.Contains(t, p.PinnedIndices, 2)
	assert.Contains(t, p.PinnedIndices, 3)
}

func TestPlan_PinsAllUserOriginals(t *testing.T) {
	msgs := []*schema.Message{
		{Role: schema.User, Content: "first request"},   // 0
		{Role: schema.Assistant, Content: "noise"},      // 1
		{Role: schema.User, Content: "second request"},  // 2
		{Role: schema.Assistant, Content: "more noise"}, // 3
		{Role: schema.User, Content: "third"},           // 4
	}
	p := Plan(msgs, PlanOpts{KeepRecent: 1})
	assert.Contains(t, p.PinnedIndices, 0, "early user intent preserved verbatim")
	assert.Contains(t, p.PinnedIndices, 2)
}

func TestPlan_PinsErrorAndWorkingSet(t *testing.T) {
	msgs := []*schema.Message{
		{Role: schema.User, Content: "edit internal/ctxcompact/compact.go"}, // 0 ws
		{Role: schema.Assistant, Content: "error: undefined: Foo"},          // 1 error
		{Role: schema.Assistant, Content: "noise"},                          // 2 (summarized)
		{Role: schema.Assistant, Content: "more noise"},                     // 3 (tail)
		{Role: schema.User, Content: "recent"},                              // 4 (tail)
	}
	p := Plan(msgs, PlanOpts{KeepRecent: 1})
	assert.Contains(t, p.PinnedIndices, 0, "working-set mention pinned")
	assert.Contains(t, p.PinnedIndices, 1, "error pinned")
	assert.Contains(t, p.SummarizeIndices, 2, "noise summarized")
}

func TestPlan_ShortCircuitsWhenAlreadySummarized(t *testing.T) {
	msgs := []*schema.Message{
		{Role: schema.User, Content: "x"},
		{Role: schema.User, Content: SummarySentinel + "already compacted"},
	}
	p := Plan(msgs, PlanOpts{KeepRecent: 1})
	assert.Empty(t, p.SummarizeIndices, "no re-compaction when already summarized (bug⑦)")
}

func TestPlan_EnforcesToolPairs(t *testing.T) {
	msgs := []*schema.Message{
		{Role: schema.User, Content: "noise"}, // 0
		{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{ID: "c1", Function: schema.FunctionCall{Name: "r"}}}}, // 1
		{Role: schema.Tool, ToolCallID: "c1", Content: "res"},                                                        // 2
		{Role: schema.User, Content: "recent"},                                                                       // 3
	}
	p := Plan(msgs, PlanOpts{KeepRecent: 1})
	assert.Contains(t, p.PinnedIndices, 1, "call pulled in to pair with pinned result")
}

// TestPlan_EmptyMessages covers the len(msgs)==0 early return.
func TestPlan_EmptyMessages(t *testing.T) {
	p := Plan(nil, PlanOpts{KeepRecent: 2})
	assert.Empty(t, p.PinnedIndices)
	assert.Empty(t, p.SummarizeIndices)
}

// TestPlan_NegativeKeepRecent covers pairCount clamping to 0.
func TestPlan_NegativeKeepRecent(t *testing.T) {
	msgs := []*schema.Message{
		{Role: schema.User, Content: "a"},
		{Role: schema.Assistant, Content: "b"},
	}
	p := Plan(msgs, PlanOpts{KeepRecent: -1})
	// pairCount becomes 0, tailStart = len(msgs) = 2, so tail loop pins nothing.
	// isUserOriginal pins index 0. Index 1 is non-user → unpinned → summarized.
	assert.Contains(t, p.PinnedIndices, 0, "user pinned by isUserOriginal")
	assert.Contains(t, p.SummarizeIndices, 1, "non-tail assistant noise summarized")
}

// TestIsUserOriginal_SummaryMessage covers the IsSummaryMessage branch.
func TestIsUserOriginal_SummaryMessage(t *testing.T) {
	m := &schema.Message{Role: schema.User, Content: SummarySentinel + "summary here"}
	assert.False(t, isUserOriginal(m), "summary message is not a user original")

	// nil message returns false
	assert.False(t, isUserOriginal(nil), "nil is not a user original")

	// tool result with user role but ToolCallID set returns false
	m2 := &schema.Message{Role: schema.User, Content: "result", ToolCallID: "c1"}
	assert.False(t, isUserOriginal(m2), "message with ToolCallID is not user original")
}
