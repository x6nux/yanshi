package ctxcompact

import (
	"context"
	"errors"
	"testing"

	"github.com/cloudwego/eino/schema"
	"github.com/stretchr/testify/assert"
)

// TestIsTransient_NilError covers nil error returning false.
func TestIsTransient_NilError(t *testing.T) {
	assert.False(t, isTransient(nil))
}

// TestIsTransient_ContextCancel covers context.Canceled not being transient.
func TestIsTransient_ContextCancel(t *testing.T) {
	assert.False(t, isTransient(context.Canceled))
	assert.False(t, isTransient(context.DeadlineExceeded))
}

// TestIsTransient_NonTransient covers errors that are not retryable.
func TestIsTransient_NonTransient(t *testing.T) {
	assert.False(t, isTransient(assert.AnError))
}

// TestIsTransient_TimeoutKeyword covers timeout being transient.
func TestIsTransient_TimeoutKeyword(t *testing.T) {
	assert.True(t, isTransient(errors.New("request timeout")))
	assert.True(t, isTransient(errors.New("connection reset")))
	assert.True(t, isTransient(errors.New("received 429")))
}

// TestIsTransient_EOF covers EOF being transient.
func TestIsTransient_EOF(t *testing.T) {
	assert.True(t, isTransient(errors.New("eof")))
	assert.True(t, isTransient(errors.New("broken pipe")))

}

// TestSplitIsSafe_OutOfRange covers i <= 0 and i >= len(msgs).
func TestSplitIsSafe_OutOfRange(t *testing.T) {
	msgs := []*schema.Message{
		{Role: schema.User, Content: "a"},
		{Role: schema.Assistant, Content: "b"},
	}
	assert.True(t, splitIsSafe(msgs, 0), "i=0 is <= 0 → safe")
	assert.True(t, splitIsSafe(msgs, 2), "i=2 is >= len(msgs) → safe")
	assert.True(t, splitIsSafe(msgs, 1), "i=1 with no tool pairs → safe")
}

// TestSplitIsSafe_WithOrphan checks behavior when left has a tool_call
// whose result is on the right — must return false.
func TestSplitIsSafe_WithOrphanPair(t *testing.T) {
	msgs := []*schema.Message{
		{Role: schema.User, Content: "a"},
		{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{ID: "c1", Function: schema.FunctionCall{Name: "read"}}}},
		{Role: schema.Tool, ToolCallID: "c1", Content: "result"},
	}
	// Split at i=2: left has c1 whose result is at i=2 → unsafe
	assert.False(t, splitIsSafe(msgs, 2), "split after call before result → unsafe")
	// Split at i=1: left (user) has no tool calls → safe
	assert.True(t, splitIsSafe(msgs, 1), "split before tool_call → safe")
}

// TestSplitIsSafe_ResultCallOnRight checks when right is a tool result
// whose call is on the left (but NOT at i-1, so only the right check catches it).
func TestSplitIsSafe_ResultCallOnRight(t *testing.T) {
	msgs := []*schema.Message{
		{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{ID: "c1", Function: schema.FunctionCall{Name: "read"}}}}, // 0 ← call
		{Role: schema.User, Content: "in between"},                               // 1 ← no ToolCalls (left at split)
		{Role: schema.Tool, ToolCallID: "c1", Content: "result"},                 // 2 ← result (right at split)
	}
	// Split at i=2: left=msgs[1] has no ToolCalls (left check passes).
	// Right=msgs[2] has ToolCallID="c1" → scans left for matching call.
	// msgs[0] has ToolCall ID="c1" → match → return false.
	assert.False(t, splitIsSafe(msgs, 2), "right check catches call on left → unsafe")
}

// TestTakeChunk_ZeroBudget returns a single-msg chunk when budget is 0.
func TestTakeChunk_ZeroBudget(t *testing.T) {
	msgs := []*schema.Message{
		{Role: schema.User, Content: "a"},
		{Role: schema.Assistant, Content: "b"},
	}
	chunk, rest := takeChunk(msgs, 0)
	// With budget=0, the first split at i=1 that's safe returns a single-item
	// chunk (the first message fits, adding the second exceeds budget 0).
	assert.Equal(t, 1, len(chunk))
	assert.NotNil(t, rest)
	assert.Equal(t, 1, len(rest))
}

// TestSingleBudget_ModelWindowZero covers ModelWindow <= 0 returning 0.
func TestSingleBudget_ModelWindowZero(t *testing.T) {
	assert.Equal(t, 0, singleBudget(RunOpts{ModelWindow: 0, ChunkThreshold: 0.9}))
	assert.Equal(t, 0, singleBudget(RunOpts{ModelWindow: -1, ChunkThreshold: 0.9}))
}

// TestSingleBudget_ChunkThresholdZero covers ChunkThreshold <= 0 returning ModelWindow.
func TestSingleBudget_ChunkThresholdZero(t *testing.T) {
	assert.Equal(t, 1000, singleBudget(RunOpts{ModelWindow: 1000, ChunkThreshold: 0}))
	assert.Equal(t, 1000, singleBudget(RunOpts{ModelWindow: 1000, ChunkThreshold: -1}))
}

// TestChunkBudgetFor_ModelWindowZero covers ModelWindow <= 0 returning 0.
func TestChunkBudgetFor_ModelWindowZero(t *testing.T) {
	assert.Equal(t, 0, chunkBudgetFor(RunOpts{ModelWindow: 0}, "", 10))
	assert.Equal(t, 0, chunkBudgetFor(RunOpts{ModelWindow: -1}, "", 10))
}

// TestBuildCarryRequest_NoCarry covers the no-carry (first chunk) path.
func TestBuildCarryRequest_NoCarry(t *testing.T) {
	chunk := []*schema.Message{{Role: schema.User, Content: "hello"}}
	req := buildCarryRequest("", chunk, 100)
	assert.Equal(t, 2, len(req), "no carry: chunk + instruction")
	assert.Equal(t, "hello", req[0].Content)
}

// TestBuildCarryRequest_WithCarry covers the with-carry path.
func TestBuildCarryRequest_WithCarry(t *testing.T) {
	chunk := []*schema.Message{{Role: schema.User, Content: "next chunk"}}
	req := buildCarryRequest("prior summary", chunk, 100)
	assert.Equal(t, 4, len(req), "with carry: carry + ack + chunk + instruction")
	assert.Contains(t, req[0].Content, SummarySentinel)
	assert.Equal(t, "next chunk", req[2].Content)
}

// TestTakeChunk_SplitAtToolPairBoundary covers the case where splitIsSafe
// returns false, so the toolpair is kept intact (over budget).
func TestTakeChunk_SplitAtToolPairBoundary(t *testing.T) {
	// First, confirm the normal split works when no pair is crossed.
	msgs := []*schema.Message{
		{Role: schema.User, Content: "start"},                                                                                 // 0: ~9 tok
		{Role: schema.Assistant, Content: "middle"},                                                                            // 1: ~9 tok
		{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{ID: "c1", Function: schema.FunctionCall{Name: "read"}}}},        // 2: ~25 tok
		{Role: schema.Tool, ToolCallID: "c1", Content: "result"},                                                              // 3: ~9 tok
	}
	chunk, rest := takeChunk(msgs, 20)
	// After 2 messages (9+9=18), adding the third (25) exceeds budget 20.
	// Split at i=2: no tool pair crosses the boundary → safe split.
	assert.Equal(t, 2, len(chunk), "split before the tool_call")
	assert.Equal(t, 2, len(rest), "tool_call and result in rest")

	// Now test pair protection: the tool_call at index 0 and its result at
	// index 1 form a pair. A split at i=1 would separate the call (left) from
	// its result (right) — splitIsSafe must block it, keeping both in the chunk.
	msgs2 := []*schema.Message{
		{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{ID: "c1", Function: schema.FunctionCall{Name: "read"}}}}, // 0: ~25 tok
		{Role: schema.Tool, ToolCallID: "c1", Content: "result"},                        // 1: ~9 tok
	}
	chunk2, rest2 := takeChunk(msgs2, 20)
	// After msg0 (25) exceeds budget at i=1, but splitIsSafe blocks because
	// left (msg0) has ToolCall c1 whose result is at i=1 (right).
	// So the loop continues, tok=25+9=34, and returns all msgs as one chunk.
	assert.Equal(t, 2, len(chunk2), "pair kept intact even over budget")
	assert.Nil(t, rest2, "no remainder when pair integrity prevents any split")
}

// TestSplitIsSafe_NilLeftMessage covers the left == nil branch.
func TestSplitIsSafe_NilLeftMessage(t *testing.T) {
	msgs := []*schema.Message{
		nil,                              // 0: nil — left at i=1
		{Role: schema.User, Content: "b"}, // 1: right at i=1
	}
	// Split at i=1: left=msgs[0]=nil → left check skipped (left==nil).
	// Right=msgs[1] has no ToolCallID → right check skipped → safe.
	assert.True(t, splitIsSafe(msgs, 1), "nil left message → left check skipped")
}

// TestSplitIsSafe_EmptyToolCallInLeft covers the tc.ID=="" continue in left-side check.
func TestSplitIsSafe_EmptyToolCallInLeft(t *testing.T) {
	msgs := []*schema.Message{
		{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{ID: "", Function: schema.FunctionCall{Name: "read"}}}},
		{Role: schema.User, Content: "right"},
	}
	// Split at i=1: left has a ToolCall with empty ID → skipped (continue).
	// Right has no ToolCallID → right check skipped → safe.
	assert.True(t, splitIsSafe(msgs, 1), "ToolCall with empty ID in left → continue → safe")
}

// TestTakeChunk_NilMessageInList handles nil messages within the chunked list.
func TestTakeChunk_NilMessageInList(t *testing.T) {
	msgs := []*schema.Message{
		{Role: schema.User, Content: "a"},
		nil,
		{Role: schema.User, Content: "b"},
	}
	chunk, rest := takeChunk(msgs, 100)
	assert.Equal(t, 3, len(chunk), "nil messages stay in chunk")
	assert.Nil(t, rest)
}
