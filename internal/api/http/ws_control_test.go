package http

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/agent/orchestrator"
	einollm "github.com/x6nux/yanshi/internal/llm/eino"
	"github.com/x6nux/yanshi/internal/proto"
)

// recvFrame reads one ServerFrame from the WS within a short deadline.
func recvFrame(t *testing.T, c *websocket.Conn) proto.ServerFrame {
	t.Helper()
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, data, err := c.ReadMessage()
	require.NoError(t, err)
	var f proto.ServerFrame
	require.NoError(t, json.Unmarshal(data, &f))
	return f
}

// recvTurn drains frames until done/error, returning the concatenated assistant
// text. Fails the test on an error frame.
func recvTurn(t *testing.T, c *websocket.Conn) string {
	t.Helper()
	var out strings.Builder
	for {
		f := recvFrame(t, c)
		switch f.Type {
		case "done":
			return out.String()
		case "error":
			t.Fatalf("unexpected error frame: %s", f.Text)
		case "agent_chunk":
			out.WriteString(f.Text)
		}
	}
}

// assistantWithUsage builds an assistant message carrying token usage, so a
// FakeModel can exercise ClassifyEventsWithUsage through the real ADK runner
// (usage survives the pipeline — see TestProbe_UsageThroughRunner).
func assistantWithUsage(text string, prompt, completion int) *schema.Message {
	return &schema.Message{
		Role:    schema.Assistant,
		Content: text,
		ResponseMeta: &schema.ResponseMeta{
			Usage: &schema.TokenUsage{
				PromptTokens:     prompt,
				CompletionTokens: completion,
				TotalTokens:      prompt + completion,
			},
		},
	}
}

// dialWS dials the chat WS endpoint exposed by server URL u.
func dialWS(t *testing.T, u string) *websocket.Conn {
	t.Helper()
	return dial(t, "ws"+strings.TrimPrefix(u, "http")+"/api/v1/chat/ws")
}

// TestChatWS_ListModels verifies list_models returns the provider names sorted.
func TestChatWS_ListModels(t *testing.T) {
	models := map[string]model.BaseChatModel{
		"beta":  einollm.NewFakeModel([]string{"b"}, nil),
		"alpha": einollm.NewFakeModel([]string{"a"}, nil),
	}
	o, err := orchestrator.New(orchestrator.Config{Model: models["alpha"]})
	require.NoError(t, err)
	s := New(Config{Token: "t"})
	s.ChatWS(o, models, nil)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	c := dialWS(t, ts.URL)
	defer c.Close()
	require.NoError(t, c.WriteJSON(proto.NewListModels()))

	f := recvFrame(t, c)
	assert.Equal(t, "models", f.Type)
	assert.Equal(t, []string{"alpha", "beta"}, f.Names, "names must be sorted")
}

// TestChatWS_SetModel_SwitchesForSubsequentTurns proves set_model changes the
// model used for later user_message turns: after switching to "alt", the next
// turn replies from alt (not the default).
func TestChatWS_SetModel_SwitchesForSubsequentTurns(t *testing.T) {
	defaultM := einollm.NewFakeModel([]string{"from-default"}, nil)
	altM := einollm.NewFakeModel([]string{"from-alt"}, nil)
	models := map[string]model.BaseChatModel{"alt": altM}
	o, err := orchestrator.New(orchestrator.Config{Model: defaultM})
	require.NoError(t, err)
	s := New(Config{Token: "t"})
	s.ChatWS(o, models, nil)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	c := dialWS(t, ts.URL)
	defer c.Close()

	require.NoError(t, c.WriteJSON(proto.NewSetModel("alt")))
	st := recvFrame(t, c)
	assert.Equal(t, "status", st.Type)
	assert.Equal(t, "alt", st.Model, "status reflects selected model")

	require.NoError(t, c.WriteJSON(proto.NewUserMessage("hi")))
	out := recvTurn(t, c)
	assert.Contains(t, out, "from-alt", "turn must run on the switched model")
	assert.NotContains(t, out, "from-default")
}

// TestChatWS_SetModel_UnknownIgnored verifies an unknown model name is ignored:
// the session model stays empty and a status frame is still returned.
func TestChatWS_SetModel_UnknownIgnored(t *testing.T) {
	models := map[string]model.BaseChatModel{"a": einollm.NewFakeModel([]string{"a"}, nil)}
	o, err := orchestrator.New(orchestrator.Config{Model: models["a"]})
	require.NoError(t, err)
	s := New(Config{Token: "t"})
	s.ChatWS(o, models, nil)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	c := dialWS(t, ts.URL)
	defer c.Close()

	require.NoError(t, c.WriteJSON(proto.NewSetModel("nope")))
	st := recvFrame(t, c)
	assert.Equal(t, "status", st.Type)
	assert.Equal(t, "a", st.Model, "unknown model ignored; the default model is shown")
	assert.NotEqual(t, "nope", st.Model, "unknown model must not be selected")
}

// TestChatWS_SetModel_NilMapIsSafe verifies set_model against a nil model map
// (the FakeModel path) is a no-op that still replies status.
func TestChatWS_SetModel_NilMapIsSafe(t *testing.T) {
	o, err := orchestrator.New(orchestrator.Config{Model: einollm.NewFakeModel([]string{"x"}, nil)})
	require.NoError(t, err)
	s := New(Config{Token: "t"})
	s.ChatWS(o, nil, nil) // nil models — FakeModel path
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	c := dialWS(t, ts.URL)
	defer c.Close()

	require.NoError(t, c.WriteJSON(proto.NewSetModel("anything")))
	st := recvFrame(t, c)
	assert.Equal(t, "status", st.Type)
	assert.Empty(t, st.Model)
}

// TestChatWS_SetThinking verifies set_thinking stores low/medium/high and that
// "off" clears it.
func TestChatWS_SetThinking(t *testing.T) {
	o, err := orchestrator.New(orchestrator.Config{Model: einollm.NewFakeModel([]string{"x"}, nil)})
	require.NoError(t, err)
	s := New(Config{Token: "t"})
	s.ChatWS(o, nil, nil)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	c := dialWS(t, ts.URL)
	defer c.Close()

	require.NoError(t, c.WriteJSON(proto.NewSetThinking("medium")))
	st := recvFrame(t, c)
	assert.Equal(t, "status", st.Type)
	assert.Equal(t, "medium", st.Thinking)

	require.NoError(t, c.WriteJSON(proto.NewSetThinking("off")))
	st = recvFrame(t, c)
	assert.Empty(t, st.Thinking, "off clears thinking")
}

// TestChatWS_GetStatus_ReflectsUsage proves get_status returns the model,
// thinking, and token usage / turn count after multiple turns. Prompt tokens
// OVERWRITE (the API reports the cumulative context per call, so the latest is
// the current context size), while completion tokens ACCUMULATE across turns
// (each call's output is separate) for a real /cost total.
func TestChatWS_GetStatus_ReflectsUsage(t *testing.T) {
	fm := einollm.NewFakeModelWithMessages([]*schema.Message{
		assistantWithUsage("r1", 10, 5),
		assistantWithUsage("r2", 20, 8),
	}, nil)
	o, err := orchestrator.New(orchestrator.Config{Model: fm})
	require.NoError(t, err)
	s := New(Config{Token: "t"})
	s.ChatWS(o, nil, nil)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	c := dialWS(t, ts.URL)
	defer c.Close()

	// Select a thinking effort so we can confirm it round-trips through status.
	require.NoError(t, c.WriteJSON(proto.NewSetThinking("high")))
	_ = recvFrame(t, c)

	require.NoError(t, c.WriteJSON(proto.NewUserMessage("m1")))
	recvTurn(t, c)
	require.NoError(t, c.WriteJSON(proto.NewUserMessage("m2")))
	recvTurn(t, c)

	require.NoError(t, c.WriteJSON(proto.NewGetStatus()))
	st := recvFrame(t, c)
	assert.Equal(t, "status", st.Type)
	assert.Equal(t, "high", st.Thinking)
	assert.Equal(t, 20, st.TokensIn, "prompt tokens overwrite: last call's context (20), not 10+20")
	assert.Equal(t, 13, st.TokensOut, "completion tokens accumulate across turns: 5 + 8")
	assert.Equal(t, 2, st.Turns)
}

// TestChatWS_Clear verifies clear resets history (an echo model's post-clear
// turn does not contain the pre-clear user message) and zeroes the counters.
func TestChatWS_Clear(t *testing.T) {
	fm := einollm.NewFakeModel(nil, nil)
	fm.Echo = true
	o, err := orchestrator.New(orchestrator.Config{Model: fm})
	require.NoError(t, err)
	s := New(Config{Token: "t"})
	s.ChatWS(o, nil, nil)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	c := dialWS(t, ts.URL)
	defer c.Close()

	require.NoError(t, c.WriteJSON(proto.NewUserMessage("SECRETVALUE")))
	recvTurn(t, c)

	require.NoError(t, c.WriteJSON(proto.NewClear()))
	st := recvFrame(t, c)
	assert.Equal(t, "status", st.Type)
	assert.Zero(t, st.Turns, "clear zeroes turns")

	// After clear, an echo model must not see the pre-clear turn.
	require.NoError(t, c.WriteJSON(proto.NewUserMessage("probe")))
	out := recvTurn(t, c)
	assert.NotContains(t, out, "SECRETVALUE", "clear must wipe server-side history")
	assert.Contains(t, out, "probe")
}

// TestChatWS_Compact verifies the (streaming) compact path (Task 35b): the
// manual /compact command forces compaction regardless of the threshold,
// STREAMING each summary delta as a compact_chunk frame before a
// status{compacted} with the before/after token estimate. Older ASSISTANT
// turns are folded into a single summary message while the most recent
// exchange is retained. It uses two models — a scripted default (for the
// turns + summary) and an echo model (selected after compact) to inspect the
// compacted history.
func TestChatWS_Compact(t *testing.T) {
	// Long ans1/ans2 so compaction actually shrinks tokens under the new Plan
	// (user messages are pinned verbatim; only assistant turns are folded
	// into the summary, so the assistant content must be larger than the
	// summary for TokensAfter < TokensBefore — otherwise MaybeCompact returns
	// did=false despite streaming chunks). DISTINCT content so the test can
	// tell which assistant turn survived (a2 is pinned in the tail) and which
	// was folded (a1 is the only one in SummarizeIndices).
	longAns1 := strings.Repeat("x", 100)
	longAns2 := strings.Repeat("y", 100)
	modelA := einollm.NewFakeModel([]string{longAns1, longAns2, "COMPACTIONSUMMARY"}, nil)
	modelB := einollm.NewFakeModel(nil, nil)
	modelB.Echo = true
	// Both models registered: "default" (modelA) is the compaction target
	// (Compaction.Model below routes the summary to it deterministically);
	// "echo" (modelB) is selected after compact to inspect the surviving
	// history.
	models := map[string]model.BaseChatModel{"echo": modelB, "default": modelA}
	o, err := orchestrator.New(orchestrator.Config{Model: modelA})
	require.NoError(t, err)
	// KeepRecent=1 so the 4-message history splits into a summary +
	// the last pair. Threshold 0 leaves AUTO-compaction disabled (only the
	// manual /compact forces compaction here). Model="default" makes
	// compactionModel pick modelA deterministically even though modelB is
	// also registered.
	s := New(Config{Token: "t", Compaction: CompactionConfig{Threshold: 0, KeepRecent: 1, Model: "default"}})
	s.ChatWS(o, models, nil)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	c := dialWS(t, ts.URL)
	defer c.Close()

	require.NoError(t, c.WriteJSON(proto.NewUserMessage("FIRSTMARKER")))
	recvTurn(t, c)
	require.NoError(t, c.WriteJSON(proto.NewUserMessage("SECONDMARKER")))
	recvTurn(t, c)

	// Manual compact: modelA produces the summary, streamed as compact_chunk,
	// then a status{compacted} with before/after tokens.
	require.NoError(t, c.WriteJSON(proto.NewCompact()))
	var sawChunk bool
	for {
		f := recvFrame(t, c)
		if f.Type == "compact_chunk" {
			sawChunk = true
			assert.Contains(t, f.Text, "COMPACTIONSUMMARY", "summary streams as compact_chunk")
			continue
		}
		if f.Type == "status" {
			assert.True(t, f.Compacted, "status after compact is marked compacted")
			assert.Greater(t, f.TokensBefore, 0, "status carries the before-token estimate")
			assert.Greater(t, f.TokensAfter, 0, "status carries the after-token estimate")
			assert.Equal(t, f.TokensAfter, f.TokensIn, "TokensIn refreshed to post-compaction size so the footer ctx counter updates")
			break
		}
	}
	assert.True(t, sawChunk, "manual compact must stream at least one compact_chunk")

	// Switch to the echo model to inspect what history remains after compact.
	require.NoError(t, c.WriteJSON(proto.NewSetModel("echo")))
	_ = recvFrame(t, c)

	require.NoError(t, c.WriteJSON(proto.NewUserMessage("probe")))
	out := recvTurn(t, c)

	assert.Contains(t, out, "COMPACTIONSUMMARY", "summary message replaces the older assistant turn")
	assert.Contains(t, out, "SECONDMARKER", "recent user turn is retained (pinned verbatim by Plan)")
	assert.Contains(t, out, "FIRSTMARKER", "older user turn is retained (Plan pins user intent verbatim)")
	// longAns2 (a2) sits in the pinned tail (last 2 msgs); longAns1 (a1) is
	// the only message in SummarizeIndices and must be folded into the summary.
	assert.Contains(t, out, longAns2, "recent assistant reply is retained (in pinned tail)")
	assert.NotContains(t, out, longAns1, "older assistant reply folded into the summary")
}
