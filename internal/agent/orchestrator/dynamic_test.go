package orchestrator

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/guard"
	einollm "github.com/x6nux/yanshi/internal/llm/eino"
	"github.com/x6nux/yanshi/internal/tools"
)

// wfs23ClientTool builds a client_ping dynamic tool whose invoke returns a
// unique marker, recording the args it received.
func wfs23ClientTool(t *testing.T, invoke func(ctx context.Context, argsJSON string) (string, error)) *tools.GuardedTool {
	t.Helper()
	tool, err := tools.NewClientTool(tools.ClientToolSpec{
		Name:        "client_ping",
		Description: "Ping the client host and return its latency.",
		Parameters:  []byte(`{"type":"object","properties":{"target":{"type":"string"}}}`),
	}, invoke)
	require.NoError(t, err)
	return tool
}

// TestWFS23InjectedToolIsDispatchedAndExecuted is the acceptance spine, run
// through a REAL adk runner: the injected spec reaches the model's tool list,
// the model can call it, the call executes through the invoke callback, and
// the result feeds back — the same guard pipeline as a built-in tool.
func TestWFS23InjectedToolIsDispatchedAndExecuted(t *testing.T) {
	step1 := schema.AssistantMessage("", []schema.ToolCall{
		{ID: "c1", Type: "function", Function: schema.FunctionCall{
			Name: "client_ping", Arguments: `{"target":"host1"}`,
		}},
	})
	step2 := schema.AssistantMessage("done", nil)
	mdl := einollm.NewFakeModelWithMessages([]*schema.Message{step1, step2}, nil)
	mdl.RecordTools = true

	var gotArgs string
	dyn := wfs23ClientTool(t, func(ctx context.Context, argsJSON string) (string, error) {
		gotArgs = argsJSON
		return "pong 42ms", nil
	})

	o, err := New(Config{
		Model: mdl,
		Tools: []BaseTool{},
		Profile: guard.PermissionProfile{
			Tools: guard.ToolsPerm{Allow: []string{"client_*"}},
		},
	})
	require.NoError(t, err)

	iter := o.EventsWithHistoryOpts(context.Background(),
		[]*schema.Message{schema.UserMessage("ping host1")},
		TurnOpts{DynamicTools: []BaseTool{dyn}})
	var results []string
	for {
		ev, ok := iter.Next()
		if !ok {
			break
		}
		require.NoError(t, ev.Err)
		if ev.Output == nil || ev.Output.MessageOutput == nil {
			continue
		}
		mv := ev.Output.MessageOutput
		var msg *schema.Message
		if mv.IsStreaming && mv.MessageStream != nil {
			m, err := mv.GetMessage()
			if err != nil || m == nil {
				continue
			}
			msg = m
		} else {
			msg = mv.Message
		}
		if msg != nil && msg.Role == schema.Tool {
			results = append(results, msg.Content)
		}
	}

	// The model SAW the injected schema.
	require.NotEmpty(t, mdl.ReceivedToolsHistory)
	names := map[string]bool{}
	for _, ti := range mdl.ReceivedToolsHistory[0] {
		names[ti.Name] = true
	}
	assert.True(t, names["client_ping"], "the injected spec must be in the model's tool list")

	// The model CALLED it and the client's answer came back.
	assert.Equal(t, `{"target":"host1"}`, gotArgs, "the invoke callback receives the model's args")
	require.NotEmpty(t, results)
	assert.Contains(t, strings.Join(results, "\n"), "pong 42ms")
}

// TestWFS23ToolregCoversInjectedAndRefusesPhantom pins the runtime-check
// clause end to end: with the turn bound (as withTurnContext does), the
// injected name is registered — and a hallucinated client_ name that was
// never injected is refused by toolreg WITHOUT any callback consult (no
// dialog). The distinguishable denial text is the observable.
func TestWFS23ToolregCoversInjectedAndRefusesPhantom(t *testing.T) {
	dyn := wfs23ClientTool(t, func(ctx context.Context, argsJSON string) (string, error) {
		return "pong", nil
	})

	o, err := New(Config{
		Model: einollm.NewFakeModel([]string{"ok"}, nil),
		Tools: []BaseTool{},
		Profile: guard.PermissionProfile{
			Tools: guard.ToolsPerm{Allow: []string{"client_*"}},
		},
	})
	require.NoError(t, err)

	ctx := o.WithTurnContextForTest(context.Background(), TurnOpts{DynamicTools: []BaseTool{dyn}})

	// Injected name: registered, passes the structural layer (the profile
	// allows client_* too, so the whole authorization succeeds).
	require.NoError(t, tools.Authorize(ctx, guard.Action{Tool: "client_ping"}, `{}`))

	// Never-injected name: structural refusal naming the tool — this is the
	// "拒且不弹窗" half. A dialog would have gone through the (absent)
	// callback and failed closed with the generic permission text instead.
	err = tools.Authorize(ctx, guard.Action{Tool: "client_pear"}, `{}`)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unregistered tool",
		"the refusal must be the structural toolreg one (no dialog), got %v", err)
}

// TestWFS23DynamicToolsDoNotReachSubAgents pins the escape-gate decision: the
// connection donated its tools to the PARENT turn; a delegated sub-agent turn
// runs with its own (empty) dynamic set because withTurnContext's unconditional
// shadow-bind makes the ctx-inherited value invisible to the sub-agent.
func TestWFS23DynamicToolsDoNotReachSubAgents(t *testing.T) {
	step1 := schema.AssistantMessage("sub done", nil)
	mdl := einollm.NewFakeModelWithMessages([]*schema.Message{step1}, nil)
	mdl.RecordTools = true

	dyn := wfs23ClientTool(t, func(ctx context.Context, argsJSON string) (string, error) {
		return "pong", nil
	})
	// A built-in tool the sub-agent is delegated, so the sub turn has
	// something to run with at all.
	fake := tools.NewGuardedTool("fs_read", "Read", "read a file", 10_000_000, nil,
		tools.SyncStream(func(context.Context, string) (string, error) { return "x", nil }))

	o, err := New(Config{
		Model: mdl,
		Tools: []BaseTool{fake},
		Profile: guard.PermissionProfile{
			Tools: guard.ToolsPerm{Allow: []string{"client_*", "fs_read"}},
		},
	})
	require.NoError(t, err)

	parentCtx := o.WithTurnContextForTest(context.Background(), TurnOpts{DynamicTools: []BaseTool{dyn}})
	out, err := o.runSubAgentTurn(parentCtx, "do things", []string{"fs_read"}, "", 0)
	require.NoError(t, err)
	assert.Equal(t, "sub done", out)

	require.NotEmpty(t, mdl.ReceivedToolsHistory)
	sub := map[string]bool{}
	for _, ti := range mdl.ReceivedToolsHistory[0] {
		sub[ti.Name] = true
	}
	assert.False(t, sub["client_ping"],
		fmt.Sprintf("a connection-donated tool must NOT leak into a sub-agent's dispatch (sub saw %v)", sub))
	assert.True(t, sub["fs_read"], "the delegated tool must be there")
}
