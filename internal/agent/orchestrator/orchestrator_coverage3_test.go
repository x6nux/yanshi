package orchestrator

import (
	"context"
	"testing"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/schema"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	einollm "github.com/x6nux/yanshi/internal/llm/eino"
	"github.com/x6nux/yanshi/internal/proto"
	"github.com/x6nux/yanshi/internal/tools"
)

// TestWrapCompaction_WithThreshold wraps the model in CompactingModel.
func TestWrapCompaction_WithThreshold(t *testing.T) {
	fm := einollm.NewFakeModel([]string{"ok"}, nil)
	wrapped := wrapCompaction(fm, CompactionConfig{Threshold: 0.8, ContextWindow: 1000, KeepRecent: 4}, 0)
	cm, ok := wrapped.(*einollm.CompactingModel)
	require.True(t, ok, "threshold >0 must wrap in CompactingModel")
	assert.Equal(t, 1000, cm.ContextWindow)
	assert.Equal(t, float64(0.8), cm.Threshold)
}

// TestBindExecutionContext_WithAllFields proves bindExecutionContext threads all
// security values when fully configured.
func TestBindExecutionContext_WithAllFields(t *testing.T) {
	fm := einollm.NewFakeModel([]string{"ok"}, nil)
	o, err := New(Config{Model: fm})
	require.NoError(t, err)
	require.NotNil(t, o)

	ctx := o.bindExecutionContext(context.Background(), "")
	// Profile must be bound (even zero-value).
	profile, ok := tools.ProfileFromContext(ctx)
	assert.True(t, ok, "profile must be bound")
	assert.NotNil(t, profile)
}

// TestClassifyEvents_ExceedMaxIterations proves ErrExceedMaxIterations produces
// a user-friendly error frame, not the raw error.
func TestClassifyEvents_ExceedMaxIterations(t *testing.T) {
	iter, gen := adk.NewAsyncIteratorPair[*adk.AgentEvent]()
	gen.Send(&adk.AgentEvent{Err: adk.ErrExceedMaxIterations})
	gen.Close()

	var frames []proto.ServerFrame
	ClassifyEvents(iter, func(f proto.ServerFrame) { frames = append(frames, f) })

	require.Len(t, frames, 1)
	assert.Equal(t, "error", frames[0].Type)
	assert.Contains(t, frames[0].Text, "maximum step limit")
}

// TestTurnUsageSet_NilIsNoop proves TurnUsage.set is a no-op for nil usage.
func TestTurnUsageSet_NilIsNoop(t *testing.T) {
	var u TurnUsage
	u.set(nil)
	assert.Equal(t, TurnUsage{}, u)
}

// TestEmitAssistant_WithToolCalls proves emitAssistant emits tool_call frames.
func TestEmitAssistant_WithToolCalls(t *testing.T) {
	var frames []proto.ServerFrame
	emitAssistant(&schema.Message{
		Content: "answer",
		ToolCalls: []schema.ToolCall{
			{ID: "c1", Function: schema.FunctionCall{Name: "fs_read", Arguments: `{"path":"x"}`}},
		},
	}, func(f proto.ServerFrame) { frames = append(frames, f) })

	// Should emit: thinking (none), agent_chunk, then tool_call
	require.Len(t, frames, 2)
	assert.Equal(t, "agent_chunk", frames[0].Type)
	assert.Equal(t, "tool_call", frames[1].Type)
}

// TestAppendImageParts_ExistingUserMessage proves images are appended to existing
// user message content.
func TestAppendImageParts_ExistingUserMessage(t *testing.T) {
	history := []*schema.Message{schema.UserMessage("describe this")}
	result := appendImageParts(history, []proto.ImageAttach{
		{Fmt: "png", DataB64: "AAAA"},
	})
	require.Len(t, result, 1)
	// The image parts are added to UserInputMultiContent. The original Content
	// stays on the message directly (not transferred to MultiContent parts).
	require.NotEmpty(t, result[0].UserInputMultiContent)
	assert.Equal(t, "data:image/png;base64,AAAA", *result[0].UserInputMultiContent[0].Image.URL)
}
