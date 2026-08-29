package orchestrator

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/agent/registry"
	"github.com/x6nux/yanshi/internal/guard"
	einollm "github.com/x6nux/yanshi/internal/llm/eino"
	"github.com/x6nux/yanshi/internal/tools"
)

// TestStrictModeDoesNotReachManagedSubAgents pins the boundary of ModeStrict,
// because two comments in the batch that introduced it disagreed about where
// that boundary is and neither pointed at a test.
//
// ws.go said a strict mode typed mid-turn binds on the next tool call
// "including one already running inside a sub-agent". tools.WithConfirmEveryCall
// said the opposite in its own doc: "an unbound predicate — every sub-agent
// context, every test, the whole SSE path — is false". The second one is right,
// and the mechanism is not subtle: registry.Manager's RootContext is
// context.Background() (bootstrap.go), so a managed sub-agent turn inherits
// NOTHING from the main turn's context except what managedTurnRunner.Run
// re-binds by name — and that list has no confirm predicate in it.
//
// The observable used here is the behaviour, not the predicate: under strict
// with no permission callback bound, Authorize turns an ALLOWED action into a
// refusal. The first assertion establishes that on a context that does carry
// the predicate (otherwise the second assertion would pass for any reason at
// all, including a probe that never ran); the second shows the same call
// succeeding inside a managed sub-agent.
//
// This is not a regression relative to anything: sub-agent tool calls were
// never individually confirmed. It is here so the next reader of "confirm EVERY
// tool call" finds out what EVERY means from something that fails when the
// answer changes, in either direction — wiring the predicate through would turn
// the second assertion red, which is the correct alarm, since with no callback
// bound on that context it would deny sub-agent tool calls outright rather than
// confirm them.
func TestStrictModeDoesNotReachManagedSubAgents(t *testing.T) {
	var ran bool
	var authorizeErr error
	profile := guard.PermissionProfile{Tools: guard.ToolsPerm{Allow: []string{"probe_confirm"}}}
	probe := tools.NewGuardedTool("probe_confirm", "Probe", "records whether strict bound",
		5*time.Second, nil,
		func(ctx context.Context, _ string) <-chan tools.ToolChunk {
			ran = true
			authorizeErr = tools.Authorize(ctx, guard.Action{Tool: "probe_confirm"}, "")
			ch := make(chan tools.ToolChunk, 1)
			ch <- tools.ToolChunk{Result: "checked"}
			close(ch)
			return ch
		})

	strict := tools.WithConfirmEveryCall(context.Background(), func() bool { return true })
	require.Error(t,
		tools.Authorize(tools.WithProfile(strict, profile), guard.Action{Tool: "probe_confirm"}, ""),
		"test setup: with the predicate bound and no callback, an allowed action must be refused — "+
			"otherwise the sub-agent assertion below proves nothing")

	mgr := registry.NewManager(registry.NewManagerOpts{
		RootContext: context.Background(),
		Path:        filepath.Join(t.TempDir(), "state.json"),
	})
	t.Cleanup(mgr.Close)

	orch, err := New(Config{
		Model:           einollm.NewFakeModelWithMessages(probeConfirmCallThenAnswer(), nil),
		Tools:           []BaseTool{probe},
		Profile:         profile,
		SubagentManager: mgr,
	})
	require.NoError(t, err)

	ctx := orch.bindManagedRunner(strict)
	out, err := orch.runSubAgentTurn(ctx, "run the probe", []string{"probe_confirm"}, "", 0)
	require.NoError(t, err)
	assert.Equal(t, "done", out)

	require.True(t, ran, "the probe never ran; the assertion below would be vacuous")
	assert.NoError(t, authorizeErr,
		"strict mode reached a managed sub-agent. If that is now intended, the sub-agent "+
			"path also needs a permission callback — without one this turns every sub-agent "+
			"tool call into a denial rather than a confirmation — and the three places that "+
			"describe strict's scope (guard/mode.go, api/http/ws.go, CLAUDE.md) have to say so.")
}

func probeConfirmCallThenAnswer() []*schema.Message {
	call := schema.AssistantMessage("", []schema.ToolCall{
		{ID: "c1", Type: "function", Function: schema.FunctionCall{
			Name: "probe_confirm", Arguments: `{}`,
		}},
	})
	return []*schema.Message{call, schema.AssistantMessage("done", nil)}
}
