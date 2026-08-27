package orchestrator

import (
	"context"
	"testing"

	"github.com/cloudwego/eino/schema"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRecordedTurnMessagesReturnsTheCapturedConversation is the supply side of
// the L5 continuation: without a real capture here, the transport's continuation
// silently falls back to replaying the turn, which is the exact behaviour L5
// removes. A nil return is not an error the caller can see — it just quietly
// becomes the old bug — so this pins that the capture is actually surfaced.
func TestRecordedTurnMessagesReturnsTheCapturedConversation(t *testing.T) {
	rec := &turnRecorder{}
	ctx := WithTurnRecorder(context.Background(), rec)

	captured := []*schema.Message{
		schema.SystemMessage("you are an agent"),
		schema.UserMessage("write the config"),
		schema.AssistantMessage("", []schema.ToolCall{{
			ID: "c1", Type: "function",
			Function: schema.FunctionCall{Name: "fs_write", Arguments: `{}`},
		}}),
		{Role: schema.Tool, ToolName: "fs_write", ToolCallID: "c1", Content: "written"},
	}
	rec.store(captured)

	got := RecordedTurnMessages(ctx)
	require.Len(t, got, 3, "the system prompt is dropped, everything else is kept")

	// The system message must be gone. The captured slice is a MODEL input, so
	// the instruction is already at its head; feeding it back as an AGENT input
	// prepends the instruction a second time, and the model then sees two
	// system messages — which some providers reject and others simply obey
	// twice.
	assert.NotEqual(t, schema.System, got[0].Role)
	assert.Equal(t, schema.User, got[0].Role)

	// The tool call AND its result must both survive: they are the evidence
	// the work already happened, and dropping either is what makes a model
	// reissue a completed side effect.
	assert.Equal(t, schema.Assistant, got[1].Role)
	require.Len(t, got[1].ToolCalls, 1)
	assert.Equal(t, schema.Tool, got[2].Role)
	assert.Equal(t, "fs_write", got[2].ToolName)
}

// TestRecordedTurnMessagesIsNilWithoutACapture covers the two "nothing to
// continue from" shapes. Both must be nil rather than an empty non-nil slice,
// because the caller branches on emptiness to pick its fallback.
func TestRecordedTurnMessagesIsNilWithoutACapture(t *testing.T) {
	assert.Nil(t, RecordedTurnMessages(context.Background()),
		"no recorder bound at all")

	rec := &turnRecorder{}
	ctx := WithTurnRecorder(context.Background(), rec)
	assert.Nil(t, RecordedTurnMessages(ctx),
		"recorder bound but nothing captured (no model call completed)")

	// A capture consisting only of the system prompt is also nothing to
	// continue from: after stripping there are no messages left.
	rec.store([]*schema.Message{schema.SystemMessage("you are an agent")})
	assert.Nil(t, RecordedTurnMessages(ctx),
		"a system-only capture leaves nothing after stripping")
}

// TestRecordedTurnMessagesDoesNotAliasTheRecorder proves the returned slice can
// be appended to safely.
//
// The caller's whole job is to append a nudge. If the result aliased the
// recorder's storage, that append could write into the capture the NEXT read
// returns — so a second continuation would resume from a history containing the
// previous continuation's nudge, compounding once per attempt.
func TestRecordedTurnMessagesDoesNotAliasTheRecorder(t *testing.T) {
	rec := &turnRecorder{}
	ctx := WithTurnRecorder(context.Background(), rec)
	rec.store([]*schema.Message{
		schema.UserMessage("first"),
		schema.AssistantMessage("second", nil),
	})

	got := RecordedTurnMessages(ctx)
	got = append(got, schema.UserMessage("appended by the caller"))
	require.Len(t, got, 3)

	assert.Len(t, RecordedTurnMessages(ctx), 2,
		"the caller's append must not reach the recorder's own storage")
}

// TestRecordedTurnMessagesSkipsNilEntries: the ADK state is assembled across
// several hooks, and a nil slot would panic on Role in every downstream
// consumer. Filtering here keeps that from becoming a crash in the turn loop.
func TestRecordedTurnMessagesSkipsNilEntries(t *testing.T) {
	rec := &turnRecorder{}
	ctx := WithTurnRecorder(context.Background(), rec)
	rec.store([]*schema.Message{nil, schema.UserMessage("real"), nil})

	got := RecordedTurnMessages(ctx)
	require.Len(t, got, 1)
	assert.Equal(t, "real", got[0].Content)
}
