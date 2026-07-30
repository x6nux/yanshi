package http

import (
	"context"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/agent/orchestrator"
	"github.com/x6nux/yanshi/internal/agent/registry"
	"github.com/x6nux/yanshi/internal/guard"
	einollm "github.com/x6nux/yanshi/internal/llm/eino"
	"github.com/x6nux/yanshi/internal/proto"
	"github.com/x6nux/yanshi/internal/tools"
)

func TestSubagentRelayDetachWaitsForInflightWrite(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	relay := newSubagentRelay(func(proto.ServerFrame) {
		close(entered)
		<-release
	})
	emitDone := make(chan struct{})
	go func() {
		relay.Emit(proto.NewSubagentEvent("ag", "explore", "started", "running", ""))
		close(emitDone)
	}()
	<-entered

	detachDone := make(chan struct{})
	go func() {
		relay.Detach()
		close(detachDone)
	}()
	select {
	case <-detachDone:
		t.Fatal("Detach returned while writer was still in flight")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	<-emitDone
	<-detachDone
}

func TestSubagentRelayDetachDoesNotCancelAgentAndTerminalPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "subagents.v1.json")
	mgr := registry.NewManager(registry.NewManagerOpts{
		RootContext: context.Background(), Path: path, SessionBootID: "boot", MaxConcurrent: 1,
	})
	block := make(chan struct{})
	relay := newSubagentRelay(func(proto.ServerFrame) {})
	sink := registry.EventSink(func(ev registry.Event) {
		relay.Emit(proto.NewSubagentEvent(ev.AgentID, ev.Role, string(ev.Type), string(ev.Status), ev.Text))
	})
	id, err := mgr.Spawn(context.Background(), registry.SpawnRequest{
		AgentType: "subagent", Role: "explore", Prompt: "scan", Emit: sink,
		Runner: registry.RunnerFunc(func(context.Context, string, string) (string, error) {
			<-block
			return "done", nil
		}),
	})
	require.NoError(t, err)

	// Simulate WS disconnect: detach relay, do NOT cancel Manager/agent.
	relay.Detach()
	close(block)
	final, err := mgr.Wait(context.Background(), id, registry.WaitOpts{Timeout: time.Second})
	require.NoError(t, err)
	require.Equal(t, registry.StatusCompleted, final.Status)
	mgr.Close()

	// New boot recovers terminal from disk.
	reopened := registry.NewManager(registry.NewManagerOpts{
		RootContext: context.Background(), Path: path, SessionBootID: "boot2", MaxConcurrent: 1,
	})
	t.Cleanup(reopened.Close)
	persisted, ok := reopened.Result(id)
	require.True(t, ok)
	require.Equal(t, registry.StatusCompleted, persisted.Status)
}

func TestChatWS_ForwardsTypedSubagentEvent(t *testing.T) {
	emitCall := schema.AssistantMessage("", []schema.ToolCall{{
		ID: "emit-1", Type: "function",
		Function: schema.FunctionCall{Name: "emit_subagent_test", Arguments: `{}`},
	}})
	done := schema.AssistantMessage("top done", nil)
	mdl := einollm.NewFakeModelWithMessages([]*schema.Message{emitCall, done}, nil)

	o, err := orchestrator.New(orchestrator.Config{
		Model: mdl, Tools: []orchestrator.BaseTool{newSubagentEmitTestTool(t)},
		Profile: guard.PermissionProfile{Tools: guard.ToolsPerm{Allow: []string{"*"}}},
	})
	require.NoError(t, err)

	s := New(Config{Token: "t"})
	s.ChatWS(o, nil, nil)
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	c := dial(t, "ws"+strings.TrimPrefix(ts.URL, "http")+"/api/v1/chat/ws")
	defer c.Close()
	require.NoError(t, c.WriteJSON(proto.NewUserMessage("emit")))

	for {
		f := readFrame(t, c)
		if f.Type == proto.SubagentEventType {
			require.Equal(t, "ag-test", f.AgentID)
			require.Equal(t, "started", f.Event)
			return
		}
		if f.Type == "done" || f.Type == "error" {
			t.Fatalf("typed event missing before %s", f.Type)
		}
	}
}

// newSubagentEmitTestTool creates a tool that emits one typed subagent event.
func newSubagentEmitTestTool(t *testing.T) *tools.GuardedTool {
	t.Helper()
	return tools.NewGuardedTool(
		"emit_subagent_test", "Test", "emit one typed subagent event", time.Second,
		schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{}),
		func(ctx context.Context, _ string) <-chan tools.ToolChunk {
			ch := make(chan tools.ToolChunk, 1)
			go func() {
				defer close(ch)
				emit := tools.SubAgentEmitFrom(ctx)
				require.NotNil(t, emit)
				emit(proto.NewSubagentEvent("ag-test", "explore", "started", "running", "scan"))
				ch <- tools.ToolChunk{Result: `{"ok":true}`}
			}()
			return ch
		},
	)
}
