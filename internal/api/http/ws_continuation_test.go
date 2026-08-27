package http

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cloudwego/eino/schema"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/agent/orchestrator"
	"github.com/x6nux/yanshi/internal/proto"
	"github.com/x6nux/yanshi/internal/tools"
)

// recordedTurn is a stand-in for what the ADK recorder captures after an
// attempt that called one side-effecting tool and then stopped: the user's
// request, the assistant's tool call, the tool's result, and a trailing text
// message.
func recordedTurn() []*schema.Message {
	return []*schema.Message{
		schema.UserMessage("write the config"),
		schema.AssistantMessage("", []schema.ToolCall{{
			ID: "c1", Type: "function",
			Function: schema.FunctionCall{Name: "fs_write", Arguments: `{"path":"a.txt"}`},
		}}),
		{Role: schema.Tool, ToolName: "fs_write", ToolCallID: "c1", Content: "written"},
		schema.AssistantMessage("I wrote it.", nil),
	}
}

// TestBuildContinuationHistoryDoesNotRewindPastExecutedTools is the L5 unit
// assertion: a continued attempt starts from what already happened.
//
// The distinguishing observation is that the tool RESULT survives into the
// continuation. A replay-based retry rebuilds from the starting history, where
// no tool message exists, so the model sees the work as still outstanding and
// reissues the call — that is how one fs_write became two. Keeping the result
// present is what tells the model it is done.
func TestBuildContinuationHistoryDoesNotRewindPastExecutedTools(t *testing.T) {
	base := []*schema.Message{schema.UserMessage("write the config")}
	recorded := recordedTurn()

	got := buildContinuationHistory(base, recorded, "I wrote it.", "still missing the timeout field")

	assert.Equal(t, []string{"fs_write"}, executedToolNames(got),
		"the executed tool's result must survive into the continuation, exactly once")

	// The turn is EXTENDED, not restarted: everything recorded is still there,
	// in order, with the nudge appended at the tail.
	require.Len(t, got, len(recorded)+1)
	for i := range recorded {
		assert.Same(t, recorded[i], got[i], "recorded message %d must be carried through unchanged", i)
	}
	last := got[len(got)-1]
	assert.Equal(t, schema.User, last.Role, "the nudge must arrive as a user turn")
	assert.Contains(t, last.Content, "still missing the timeout field",
		"the nudge must carry the judge's reason, or the next attempt has no new signal")
	assert.Contains(t, last.Content, "do not repeat them",
		"the nudge must tell the model the tool results above are already done")
}

// TestBuildContinuationHistoryFallsBackWhenNothingWasRecorded covers the case
// where there is no capture.
//
// This is the ONLY path that still replays the base history, and it is safe for
// a specific reason worth pinning: an empty capture means no model call
// completed, which means no tool ran, which means the replay cannot double-run
// anything. If a future change ever made an empty capture possible AFTER tools
// ran, this test's premise would be the thing to revisit.
func TestBuildContinuationHistoryFallsBackWhenNothingWasRecorded(t *testing.T) {
	base := []*schema.Message{schema.UserMessage("write the config")}

	got := buildContinuationHistory(base, nil, "partial text", "finish it")

	require.Len(t, got, 3)
	assert.Same(t, base[0], got[0])
	assert.Equal(t, schema.Assistant, got[1].Role)
	assert.Equal(t, "partial text", got[1].Content)
	assert.Equal(t, schema.User, got[2].Role)
	assert.Equal(t, "finish it", got[2].Content,
		"with no tool results above, the nudge must not claim there are any")
	assert.Empty(t, executedToolNames(got), "the fallback path has no executed tools by definition")
}

// TestBuildContinuationHistoryNeverMutatesTheCallersSlice guards the leak that
// would corrupt the persisted session.
//
// cs.history is the conversation saved to the DB and replayed on every later
// turn. Go's append can share a backing array, so a continuation that appended
// in place would write a synthetic "you did not finish" user turn into the
// user's real history — visible forever, and fed to the model on every
// subsequent turn.
func TestBuildContinuationHistoryNeverMutatesTheCallersSlice(t *testing.T) {
	// Spare capacity is the precondition for the aliasing bug; without it
	// append allocates and the test would pass for the wrong reason.
	base := make([]*schema.Message, 1, 8)
	base[0] = schema.UserMessage("write the config")
	recorded := make([]*schema.Message, len(recordedTurn()), 8)
	copy(recorded, recordedTurn())

	_ = buildContinuationHistory(base, recorded, "text", "reason")

	assert.Len(t, base, 1, "the base history must not grow")
	assert.Len(t, recorded, len(recordedTurn()), "the recorded slice must not grow")
	assert.Len(t, base[:cap(base)][:2], 2)
	assert.Nil(t, base[:cap(base)][1], "nothing may be written into the caller's spare capacity")
}

// TestBuildContinuationHistoryDefaultsTheNudge: an empty reason must still
// produce a usable instruction. A blank user turn would tell the model nothing
// and the continuation would reproduce the same stop.
func TestBuildContinuationHistoryDefaultsTheNudge(t *testing.T) {
	for _, tc := range []struct {
		name     string
		recorded []*schema.Message
	}{
		{name: "with a capture", recorded: recordedTurn()},
		{name: "without a capture", recorded: nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := buildContinuationHistory(
				[]*schema.Message{schema.UserMessage("go")}, tc.recorded, "", "")
			last := got[len(got)-1]
			assert.Equal(t, schema.User, last.Role)
			assert.Contains(t, last.Content, continuationFallbackNudge)
		})
	}
}

// TestPrematureStopContinuationDoesNotReExecuteSideEffectingTools is the L5
// end-to-end gate, and the reason the unit tests above are not sufficient: they
// prove the helper builds the right slice, not that the turn loop USES it.
//
// Setup: the model calls a counting tool, then stops with text. The judge is
// asked whether the turn is complete; the fake judge here says "no" once, so
// the loop continues. Under the old replay behaviour the second attempt
// re-derived the turn from cs.history and called the tool again — the counter
// would read 2. It must read 1.
func TestPrematureStopContinuationDoesNotReExecuteSideEffectingTools(t *testing.T) {
	var calls atomic.Int32
	counter := tools.NewGuardedTool(
		"side_effect", "SideEffect", "Records that it ran.",
		toolTestTimeout,
		emptyParams(),
		tools.SyncStream(func(context.Context, string) (string, error) {
			calls.Add(1)
			return "ran", nil
		}),
	)

	// Attempt 0: call the tool, then stop with text. Attempt 1 (the
	// continuation): text only — the model, seeing its own completed tool
	// result, has no reason to call again. If the loop replayed instead of
	// continuing, the ADK would re-run the whole turn from the start and the
	// FIRST scripted message (the tool call) would be served again.
	mdl := newJudgeScriptedModel(
		[]*schema.Message{
			schema.AssistantMessage("", []schema.ToolCall{{
				ID: "c1", Type: "function",
				Function: schema.FunctionCall{Name: "side_effect", Arguments: `{}`},
			}}),
			schema.AssistantMessage("I think I'm done.", nil),
			schema.AssistantMessage("Now I am really done.", nil),
		},
		// One "incomplete" verdict, then complete.
		[]string{
			`{"complete":false,"reason":"you did not confirm the result"}`,
			`{"complete":true,"reason":""}`,
		},
	)

	url := newWSServerWithTools(t, mdl, []orchestrator.BaseTool{counter})
	c := dial(t, url)
	defer c.Close()

	require.NoError(t, c.WriteJSON(proto.NewUserMessage("do the thing")))
	drainToDone(t, c)

	assert.Equal(t, int32(1), calls.Load(),
		"a continued turn must not re-execute a tool the previous attempt already ran")

	// The continuation really did happen — otherwise the assertion above would
	// pass trivially because there was only ever one attempt.
	assert.GreaterOrEqual(t, mdl.turnCalls(), 3,
		"the judge said incomplete once, so a second attempt must have run")
}

// TestPrematureStopContinuationCarriesTheJudgesReason proves the continued
// attempt is given something new to act on.
//
// A continuation that resent the same context without the judge's reason would
// reproduce the same stop and burn the retry budget for nothing — which is the
// failure the reminder mechanism exists to prevent, and it is invisible unless
// someone inspects what the second attempt actually received.
func TestPrematureStopContinuationCarriesTheJudgesReason(t *testing.T) {
	mdl := newJudgeScriptedModel(
		[]*schema.Message{
			schema.AssistantMessage("first pass", nil),
			schema.AssistantMessage("second pass", nil),
		},
		[]string{
			`{"complete":false,"reason":"the error path is unhandled"}`,
			`{"complete":true,"reason":""}`,
		},
	)

	url := newWSServerWithTools(t, mdl, nil)
	c := dial(t, url)
	defer c.Close()

	require.NoError(t, c.WriteJSON(proto.NewUserMessage("do the thing")))
	drainToDone(t, c)

	last := mdl.lastTurnInput()
	require.NotEmpty(t, last, "the continued attempt's input must have been captured")
	var sawReason bool
	for _, m := range last {
		if m != nil && m.Role == schema.User && strings.Contains(m.Content, "the error path is unhandled") {
			sawReason = true
		}
	}
	assert.True(t, sawReason,
		"the continuation must carry the judge's reason, or the retry has no new signal")
}
