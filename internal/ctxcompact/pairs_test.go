package ctxcompact

import (
	"testing"

	"github.com/cloudwego/eino/schema"
	"github.com/stretchr/testify/assert"
)

// bug② regression: a split point between tool_call and result produces an
// orphan result -> API 400. The fixpoint must align pairs: pinning a result
// pulls in its call, and vice versa.

func TestEnforceToolCallPairs_PinsCallForResult(t *testing.T) {
	msgs := []*schema.Message{
		{Role: schema.User, Content: "noise"},
		{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{ID: "c1", Function: schema.FunctionCall{Name: "read"}}}},
		{Role: schema.Tool, ToolCallID: "c1", Content: "ok"},
	}
	pinned := map[int]bool{2: true} // only result pinned
	EnforceToolCallPairs(msgs, pinned)
	assert.True(t, pinned[1], "call pulled in to pair with pinned result")
}

func TestEnforceToolCallPairs_RemovesOrphanCall(t *testing.T) {
	msgs := []*schema.Message{
		{Role: schema.User, Content: "noise"},
		{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{ID: "orphan", Function: schema.FunctionCall{Name: "read"}}}},
		{Role: schema.Assistant, Content: "recent"},
	}
	pinned := map[int]bool{0: true, 1: true, 2: true}
	EnforceToolCallPairs(msgs, pinned)
	assert.False(t, pinned[1], "orphaned call (no result anywhere) removed")
	assert.True(t, pinned[0])
	assert.True(t, pinned[2])
}

func TestEnforceToolCallPairs_Cascades(t *testing.T) {
	// msg1 has two calls (good+orphan); orphan has no result -> msg1 removed.
	// That orphans good's result (msg2) -> msg2 also removed (cascade).
	msgs := []*schema.Message{
		{Role: schema.User, Content: "start"},
		{Role: schema.Assistant, ToolCalls: []schema.ToolCall{
			{ID: "good", Function: schema.FunctionCall{Name: "r"}},
			{ID: "orphan", Function: schema.FunctionCall{Name: "s"}},
		}},
		{Role: schema.Tool, ToolCallID: "good", Content: "ok"},
		{Role: schema.Assistant, Content: "done"},
	}
	pinned := map[int]bool{1: true, 2: true, 3: true}
	EnforceToolCallPairs(msgs, pinned)
	assert.False(t, pinned[1])
	assert.False(t, pinned[2])
	assert.True(t, pinned[3])
}

// bug② core contract: a dangling tool_result (no matching call anywhere) is
// what makes OpenAI/Anthropic reject with 400. It MUST be removed from pinned.
func TestEnforceToolCallPairs_RemovesOrphanResult(t *testing.T) {
	msgs := []*schema.Message{
		{Role: schema.User, Content: "noise"},
		{Role: schema.Tool, ToolCallID: "ghost", Content: "orphan result"},
		{Role: schema.Assistant, Content: "recent"},
	}
	pinned := map[int]bool{0: true, 1: true, 2: true}
	EnforceToolCallPairs(msgs, pinned)
	assert.False(t, pinned[1], "orphaned result (no call anywhere) removed")
	assert.True(t, pinned[0])
	assert.True(t, pinned[2])
}

// call→result pull-in direction (mirror of PinsCallForResult).
func TestEnforceToolCallPairs_PinsResultForCall(t *testing.T) {
	msgs := []*schema.Message{
		{Role: schema.User, Content: "noise"},
		{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{ID: "c1", Function: schema.FunctionCall{Name: "read"}}}},
		{Role: schema.Tool, ToolCallID: "c1", Content: "ok"},
	}
	pinned := map[int]bool{1: true} // only call pinned
	EnforceToolCallPairs(msgs, pinned)
	assert.True(t, pinned[2], "result pulled in to pair with pinned call")
}

// TestEnforceToolCallPairs_EmptyPinned covers the len(pinned)==0 early return.
func TestEnforceToolCallPairs_EmptyPinned(t *testing.T) {
	msgs := []*schema.Message{{Role: schema.User, Content: "hi"}}
	pinned := map[int]bool{}
	EnforceToolCallPairs(msgs, pinned) // must not panic or loop
	assert.Empty(t, pinned)
}

// TestEnforceToolCallPairs_NilMessageInMap covers skipping nil msgs in the map.
func TestEnforceToolCallPairs_NilMessageInMap(t *testing.T) {
	msgs := []*schema.Message{nil, {Role: schema.Tool, ToolCallID: "c1", Content: "orphan"}, nil}
	pinned := map[int]bool{0: true, 1: true, 2: true}
	EnforceToolCallPairs(msgs, pinned)
	assert.False(t, pinned[1], "orphan result removed without matching call")
	// Nil messages should be handled gracefully (skipped during index building and loop)
}

// TestEnforceToolCallPairs_EmptyToolCallID covers ToolCall with empty ID.
func TestEnforceToolCallPairs_EmptyToolCallID(t *testing.T) {
	msgs := []*schema.Message{
		{Role: schema.Assistant, ToolCalls: []schema.ToolCall{
			{ID: "", Function: schema.FunctionCall{Name: "read"}},
		}},
		{Role: schema.Tool, ToolCallID: "", Content: "ok"},
	}
	pinned := map[int]bool{0: true, 1: true}
	EnforceToolCallPairs(msgs, pinned)
	// Both have empty IDs: the call's empty ID is skipped (tc.ID=="" continue),
	// and the result's empty ToolCallID is skipped too. Neither gets paired or
	// orphan-checked, so both stay pinned (empty pairs are not tracked).
	assert.True(t, pinned[0], "ToolCall with empty ID stays pinned (no pair enforcement)")
	assert.True(t, pinned[1], "ToolResult with empty ID stays pinned (no pair enforcement)")
}

// TestRemove_IdxNotPinned covers remove() returning false when idx is not in pinned.
func TestRemove_IdxNotPinned(t *testing.T) {
	pinned := map[int]bool{0: true}
	permRemoved := map[int]bool{}
	assert.False(t, remove(pinned, permRemoved, 5), "absent idx returns false")
	assert.Empty(t, permRemoved, "nothing added when idx wasn't pinned")
}
