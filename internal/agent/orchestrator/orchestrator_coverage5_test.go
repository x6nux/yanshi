package orchestrator

import (
	"context"
	"testing"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/schema"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/proto"
	"github.com/x6nux/yanshi/internal/task/work"
)

// TestEmitReady_EmptyArgs tests emitReady with no args (not emitted).
func TestEmitReady_EmptyArgs(t *testing.T) {
	acc := newToolCallAccumulator()
	acc.update([]schema.ToolCall{
		{ID: "c1", Function: schema.FunctionCall{Name: "noarg_tool"}},
	})
	var frames []proto.ServerFrame
	acc.emitReady(func(f proto.ServerFrame) { frames = append(frames, f) })
	// No args - not valid JSON - not emitted
	assert.Empty(t, frames, "tool call with empty args not emitted by emitReady")
}

// TestEmitReady_AlreadEmitted tests emitReady skips already-emitted calls.
func TestEmitReady_AlreadyEmitted(t *testing.T) {
	acc := newToolCallAccumulator()
	acc.update([]schema.ToolCall{
		{ID: "c1", Index: ptrIndex(0), Function: schema.FunctionCall{Name: "t", Arguments: `{"x":"y"}`}},
	})
	var frames []proto.ServerFrame
	acc.emitReady(func(f proto.ServerFrame) { frames = append(frames, f) })
	require.Len(t, frames, 1, "first emit should fire")

	acc.emitReady(func(f proto.ServerFrame) { frames = append(frames, f) })
	assert.Len(t, frames, 1, "second emit must not re-emit")
}

// TestWorkEventFrame_NonNilTask proves a non-nil task produces task_update.
func TestWorkEventFrame_NonNilTask(t *testing.T) {
	f := workEventFrame(work.Event{
		Kind: work.EventTaskUpdate,
		Task: &work.WorkTask{ID: "t1"},
	})
	assert.Equal(t, "task_update", f.Type)
	assert.Equal(t, "t1", f.TaskID)
}

// TestAfterModelRewriteState_NoRecorder proves no error when no recorder bound.
func TestAfterModelRewriteState_NoRecorder(t *testing.T) {
	r := newMessageRecorder()
	ctx := context.Background()
	// No recorder in context → AfterModelRewriteState is a no-op
	ctx, state, err := r.AfterModelRewriteState(ctx, nil, nil)
	require.NoError(t, err)
	assert.Nil(t, state)
}

// TestClassifyEvents_NilMsgOutputInNonStreaming proves nil msg in non-streaming
// message variant does not crash.
func TestClassifyEvents_NilMsgOutputInNonStreaming_Detailed(t *testing.T) {
	// Test 1: valid message variant with nil Message
	iter, gen := adk.NewAsyncIteratorPair[*adk.AgentEvent]()
	gen.Send(&adk.AgentEvent{
		Output: &adk.AgentOutput{
			MessageOutput: &adk.MessageVariant{
				IsStreaming: false,
				Message:     nil,
			},
		},
	})
	gen.Close()

	var frames []proto.ServerFrame
	ClassifyEvents(iter, func(f proto.ServerFrame) { frames = append(frames, f) })
	assert.Empty(t, frames)
}

// TestAfterModelRewriteState_StoresMessages covers the store-path at
// completion.go:99-101: a non-nil recorder + non-nil state triggers the
// store, capturing the messages.
func TestAfterModelRewriteState_StoresMessages(t *testing.T) {
	rec := &turnRecorder{}
	ctx := WithTurnRecorder(context.Background(), rec)
	msgs := []*schema.Message{{Role: schema.User, Content: "hello"}}
	state := &adk.ChatModelAgentState{Messages: msgs}
	r := &messageRecorder{}
	ctx2, state2, err := r.AfterModelRewriteState(ctx, state, nil)
	require.NoError(t, err)
	assert.NotNil(t, ctx2)
	assert.NotNil(t, state2)
	loaded := rec.load()
	require.Len(t, loaded, 1)
	assert.Equal(t, "hello", loaded[0].Content)
}
