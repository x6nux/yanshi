package http

import (
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/agent/orchestrator"
	einollm "github.com/x6nux/yanshi/internal/llm/eino"
	"github.com/x6nux/yanshi/internal/proto"
)

// sseEvent is one parsed SSE event: its event name and the decoded ServerFrame.
type sseEvent struct {
	event string
	frame proto.ServerFrame
}

// parseSSE decodes an SSE response body into the ordered list of events the
// server emitted. Used to assert event ordering (e.g. status before done).
func parseSSE(t *testing.T, body string) []sseEvent {
	t.Helper()
	var out []sseEvent
	var eventType string
	for _, line := range strings.Split(body, "\n") {
		if e, ok := strings.CutPrefix(line, "event: "); ok {
			eventType = e
			continue
		}
		if d, ok := strings.CutPrefix(line, "data: "); ok {
			var f proto.ServerFrame
			if err := json.Unmarshal([]byte(d), &f); err == nil {
				if f.Type == "" {
					f.Type = eventType
				}
				out = append(out, sseEvent{event: f.Type, frame: f})
			}
			continue
		}
		if strings.TrimSpace(line) == "" {
			eventType = "" // blank line separates events
		}
	}
	return out
}

// postChat POSTs a JSON body to /api/v1/chat and returns the full SSE body.
func postChat(t *testing.T, ts *httptest.Server, body string) string {
	t.Helper()
	resp, err := ts.Client().Post(ts.URL+"/api/v1/chat", "application/json", strings.NewReader(body))
	require.NoError(t, err)
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return string(b)
}

// TestChat_SSE_ModelAndThinking proves a per-request model+thinking selection
// drives the turn and that a status event (carrying the selection + usage)
// precedes done. The default model would answer "from-default"; selecting
// "alt" must answer "from-alt".
func TestChat_SSE_ModelAndThinking(t *testing.T) {
	defaultM := einollm.NewFakeModel([]string{"from-default"}, nil)
	altM := einollm.NewFakeModel([]string{"from-alt"}, nil)
	models := map[string]model.BaseChatModel{"alt": altM}
	o, err := orchestrator.New(orchestrator.Config{Model: defaultM})
	require.NoError(t, err)
	s := New(Config{Token: "t"})
	s.Chat(o, models, nil)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	body := `{"message":"hi","model":"alt","thinking":"medium"}`
	bodyStr := postChat(t, ts, body)

	// The turn must have run on the alt model.
	assert.Contains(t, bodyStr, "from-alt")
	assert.NotContains(t, bodyStr, "from-default")

	events := parseSSE(t, bodyStr)
	// Find the done event index and require a status event before it.
	var statusIdx, doneIdx = -1, -1
	for i, e := range events {
		if e.event == "status" && statusIdx == -1 {
			statusIdx = i
		}
		if e.event == "done" {
			doneIdx = i
		}
	}
	require.NotEqual(t, -1, doneIdx, "response must end with a done event")
	require.NotEqual(t, -1, statusIdx, "response must include a status event")
	assert.Less(t, statusIdx, doneIdx, "status must precede done")

	status := events[statusIdx].frame
	assert.Equal(t, "alt", status.Model, "status echoes the requested model")
	assert.Equal(t, "medium", status.Thinking, "status echoes the requested thinking effort")
}

// TestChat_SSE_UnknownModelFallsBack proves an unknown model name is ignored
// (nil lookup) and the default model runs — no crash, no error frame.
func TestChat_SSE_UnknownModelFallsBack(t *testing.T) {
	defaultM := einollm.NewFakeModel([]string{"from-default"}, nil)
	models := map[string]model.BaseChatModel{"alt": einollm.NewFakeModel([]string{"from-alt"}, nil)}
	o, err := orchestrator.New(orchestrator.Config{Model: defaultM})
	require.NoError(t, err)
	s := New(Config{Token: "t"})
	s.Chat(o, models, nil)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	bodyStr := postChat(t, ts, `{"message":"hi","model":"nope"}`)
	assert.Contains(t, bodyStr, "from-default", "unknown model falls back to the default")
	assert.NotContains(t, bodyStr, "from-alt")

	events := parseSSE(t, bodyStr)
	var status *proto.ServerFrame
	for _, e := range events {
		if e.event == "status" {
			status = &e.frame
		}
	}
	require.NotNil(t, status, "status event must be present")
	// SSE is stateless, so status echoes the requested model name verbatim even
	// when it was unknown and the default ran (the turn body is the source of
	// truth for which model actually answered).
	assert.Equal(t, "nope", status.Model, "status echoes the requested model name")
}

// TestChat_SSE_NilModelsIsSafe proves the handler works with a nil model map
// (the FakeModel path) and still emits status + done.
func TestChat_SSE_NilModelsIsSafe(t *testing.T) {
	o, err := orchestrator.New(orchestrator.Config{Model: einollm.NewFakeModel([]string{"ok"}, nil)})
	require.NoError(t, err)
	s := New(Config{Token: "t"})
	s.Chat(o, nil, nil)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	bodyStr := postChat(t, ts, `{"message":"hi","model":"anything","thinking":"high"}`)
	assert.Contains(t, bodyStr, "ok")

	events := parseSSE(t, bodyStr)
	var hasStatus, hasDone bool
	for _, e := range events {
		if e.event == "status" {
			hasStatus = true
		}
		if e.event == "done" {
			hasDone = true
		}
	}
	assert.True(t, hasStatus, "status event present even with nil models")
	assert.True(t, hasDone)
}

// TestChat_SSE_StatusCarriesUsage proves the status event reports the turn's
// token usage when the model provides it (usage survives the ADK runner).
func TestChat_SSE_StatusCarriesUsage(t *testing.T) {
	fm := einollm.NewFakeModelWithMessages([]*schema.Message{assistantWithUsage("reply", 11, 4)}, nil)
	o, err := orchestrator.New(orchestrator.Config{Model: fm})
	require.NoError(t, err)
	s := New(Config{Token: "t"})
	s.Chat(o, nil, nil)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	bodyStr := postChat(t, ts, `{"message":"hi"}`)
	events := parseSSE(t, bodyStr)
	var status *proto.ServerFrame
	for _, e := range events {
		if e.event == "status" {
			status = &e.frame
		}
	}
	require.NotNil(t, status)
	assert.Equal(t, 11, status.TokensIn)
	assert.Equal(t, 4, status.TokensOut)
}
