package http

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/agent/orchestrator"
	"github.com/x6nux/yanshi/internal/guard"
	einollm "github.com/x6nux/yanshi/internal/llm/eino"
	"github.com/x6nux/yanshi/internal/proto"
	"github.com/x6nux/yanshi/internal/task/work"
	"github.com/x6nux/yanshi/internal/tools"
)

// TestChatWS_ToolWorkEventReachesTheClient closes the hop that every other test
// in this chain assumed.
//
// tools.EmitWorkEvent is a no-op unless a WorkEventCallback is bound to the
// turn context, and the orchestrator binds one only when TurnOpts.EmitWorkFrame
// is non-nil. No production caller ever set that field — not ws.go, not
// chat.go, not v1 — so update_plan, checklist_*, task_create, task_cancel and
// task_gate_run all emitted into a discard. Both ends of the chain had tests:
// the tools proved they emit, and internal/cli/tui::TestPlanUpdateFrameReaches
// TheTranscript proved the TUI renders a frame that arrives. Nothing checked
// that one was ever produced from the other, and the middle hop was nil.
func TestChatWS_ToolWorkEventReachesTheClient(t *testing.T) {
	planCall := schema.AssistantMessage("", []schema.ToolCall{{
		ID: "plan-1", Type: "function",
		Function: schema.FunctionCall{Name: "emit_work_event_test", Arguments: `{}`},
	}})
	done := schema.AssistantMessage("top done", nil)
	mdl := einollm.NewFakeModelWithMessages([]*schema.Message{planCall, done}, nil)

	o, err := orchestrator.New(orchestrator.Config{
		Model: mdl, Tools: []orchestrator.BaseTool{newWorkEventTestTool(t)},
		Profile: guard.PermissionProfile{Tools: guard.ToolsPerm{Allow: []string{"*"}}},
	})
	require.NoError(t, err)

	s := New(Config{Token: "t"})
	s.ChatWS(o, nil, nil)
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	c := dial(t, "ws"+strings.TrimPrefix(ts.URL, "http")+"/api/v1/chat/ws")
	defer c.Close()
	require.NoError(t, c.WriteJSON(proto.NewUserMessage("plan it")))

	for {
		f := readFrame(t, c)
		if f.Type == "plan_update" {
			require.Equal(t, "task-9", f.TaskID)
			require.NotNil(t, f.Checklist, "the frame carries no checklist, so the TUI renders an empty plan")
			require.Len(t, f.Checklist.Items, 1)
			require.Equal(t, "write the parser", f.Checklist.Items[0].Content)
			return
		}
		if f.Type == "done" || f.Type == "error" {
			t.Fatalf("no plan_update frame before %s: the tool's work event was discarded", f.Type)
		}
	}
}

// newWorkEventTestTool emits exactly one plan-update work event, the way
// tools.updatePlan does, without needing a work.Manager or a database.
func newWorkEventTestTool(t *testing.T) *tools.GuardedTool {
	t.Helper()
	return tools.NewGuardedTool(
		"emit_work_event_test", "Test", "emit one plan update work event", time.Second,
		schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{}),
		func(ctx context.Context, _ string) <-chan tools.ToolChunk {
			ch := make(chan tools.ToolChunk, 1)
			go func() {
				defer close(ch)
				tools.EmitWorkEvent(ctx, work.Event{
					Kind:   work.EventPlanUpdate,
					TaskID: "task-9",
					Checklist: work.Checklist{Items: []work.ChecklistItem{
						{ID: 1, Content: "write the parser", Status: work.ChecklistInProgress},
					}},
				})
				ch <- tools.ToolChunk{Result: `{"ok":true}`}
			}()
			return ch
		},
	)
}

// TestChatSSE_ToolWorkEventReachesTheClient is the SSE half. Both transports
// share the ServerFrame vocabulary but not the delivery mechanism: SSE has a
// single legal writer (the merge loop), so its work frames go through the
// lifecycle relay instead of being written from the tool goroutine. Wiring one
// transport and not the other is the asymmetry ADR-0004 exists to prevent, and
// nothing in the frame vocabulary would have caught it.
func TestChatSSE_ToolWorkEventReachesTheClient(t *testing.T) {
	planCall := schema.AssistantMessage("", []schema.ToolCall{{
		ID: "plan-1", Type: "function",
		Function: schema.FunctionCall{Name: "emit_work_event_test", Arguments: `{}`},
	}})
	done := schema.AssistantMessage("top done", nil)
	mdl := einollm.NewFakeModelWithMessages([]*schema.Message{planCall, done}, nil)

	o, err := orchestrator.New(orchestrator.Config{
		Model: mdl, Tools: []orchestrator.BaseTool{newWorkEventTestTool(t)},
		Profile: guard.PermissionProfile{Tools: guard.ToolsPerm{Allow: []string{"*"}}},
	})
	require.NoError(t, err)

	s := New(Config{Token: "t"})
	s.Chat(o, nil, nil)
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	body := bytes.NewBufferString(`{"messages":[{"role":"user","content":"plan it"}]}`)
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/chat", body)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer t")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	joined := string(raw)
	require.Contains(t, joined, "event: plan_update",
		"no plan_update event on the SSE stream: the tool's work event was discarded")
	require.Contains(t, joined, `"write the parser"`)
}
