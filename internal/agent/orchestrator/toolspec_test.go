package orchestrator

import (
	"context"
	"testing"

	"github.com/cloudwego/eino/schema"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/guard"
	einollm "github.com/x6nux/yanshi/internal/llm/eino"
	"github.com/x6nux/yanshi/internal/tools"
)

// wfs11Toolset builds four GuardedTools whose descriptions separate cleanly
// under retrieval: one matches a kubernetes query, one never will, plus the
// two escape-hatch tools. ran records real executions.
func wfs11Toolset() (ts []BaseTool, ran *bool) {
	ran = new(bool)
	kube := tools.NewGuardedTool("kube_scale", "Scale", "Scale kubernetes deployments and manage cluster workloads.",
		10_000_000, nil, tools.SyncStream(func(context.Context, string) (string, error) {
			return "scaled", nil
		}))
	zeta := tools.NewGuardedTool("zeta_file_writer", "Zeta writer", "Write bytes into a zeta file.",
		10_000_000, nil, tools.SyncStream(func(context.Context, string) (string, error) {
			*ran = true
			return "written", nil
		}))
	disc := tools.NewToolDiscoveryTools([]tools.ToolMeta{
		{Name: "kube_scale", Desc: "Scale kubernetes deployments and manage cluster workloads."},
		{Name: "zeta_file_writer", Desc: "Write bytes into a zeta file."},
	})
	ts = []BaseTool{kube, zeta, disc.List, disc.Load}
	return ts, ran
}

func wfs11Profile() guard.PermissionProfile {
	return guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"kube_scale", "zeta_file_writer", "tools_list", "tools_load"}},
	}
}

func wfs11Names(t *testing.T, fake *einollm.FakeModel, call int) map[string]bool {
	t.Helper()
	require.True(t, len(fake.ReceivedToolsHistory) > call, "model call %d never happened", call)
	names := make(map[string]bool)
	for _, ti := range fake.ReceivedToolsHistory[call] {
		names[ti.Name] = true
	}
	return names
}

// TestWFS11OnDemandGateHidesUntilLoaded is the load-bearing acceptance: through
// a REAL adk runner, a retrieval-missed tool is absent from the first model
// call, is made visible by an explicit tools_load, and is callable afterwards.
// It also pins the escape hatch: tools_list / tools_load are in EVERY call.
//
// 变异：从 orchestratorMiddlewares 的安装点去掉 newToolSpecGate（或删掉
// toolspec.go 的 BeforeModelRewriteState 过滤体），第一个断言即红 —— 模型
// 第一次调用就会看到全量 schema。
func TestWFS11OnDemandGateHidesUntilLoaded(t *testing.T) {
	step1 := schema.AssistantMessage("", []schema.ToolCall{
		{ID: "c1", Type: "function", Function: schema.FunctionCall{
			Name: "tools_load", Arguments: `{"names":["zeta_file_writer"]}`,
		}},
	})
	step2 := schema.AssistantMessage("", []schema.ToolCall{
		{ID: "c2", Type: "function", Function: schema.FunctionCall{
			Name: "zeta_file_writer", Arguments: `{}`,
		}},
	})
	step3 := schema.AssistantMessage("done", nil)
	mdl := einollm.NewFakeModelWithMessages([]*schema.Message{step1, step2, step3}, nil)
	mdl.RecordTools = true

	ts, ran := wfs11Toolset()
	o, err := New(Config{
		Model: mdl, Tools: ts, Profile: wfs11Profile(),
		OnDemand: tools.ToolLoadConfig{Enabled: true, MaxVisible: 1},
	})
	require.NoError(t, err)

	_, err = o.Query(context.Background(), "scale the kubernetes cluster workloads")
	require.NoError(t, err)
	require.True(t, len(mdl.ReceivedToolsHistory) >= 3, "expected 3 model calls, got %d", len(mdl.ReceivedToolsHistory))

	first := wfs11Names(t, mdl, 0)
	assert.True(t, first["kube_scale"], "the retrieval hit must be visible on call 1")
	assert.True(t, first["tools_list"] && first["tools_load"],
		"the escape hatch must be visible on EVERY call, including call 1")
	assert.False(t, first["zeta_file_writer"],
		"the retrieval-missed tool must NOT be in the schema before it is loaded")

	second := wfs11Names(t, mdl, 1)
	assert.True(t, second["zeta_file_writer"],
		"after tools_load the missed tool must be visible (the explicit-load acceptance)")
	assert.True(t, *ran, "the loaded tool must actually be callable")
}

// TestWFS11DisabledKeepsFullSchema pins the zero-value contract: OnDemand zero
// = no gate = the model sees the full registry on every call, byte-identical
// to the pre-W-F-11 wiring.
func TestWFS11DisabledKeepsFullSchema(t *testing.T) {
	step1 := schema.AssistantMessage("done", nil)
	mdl := einollm.NewFakeModelWithMessages([]*schema.Message{step1}, nil)
	mdl.RecordTools = true

	ts, _ := wfs11Toolset()
	o, err := New(Config{Model: mdl, Tools: ts, Profile: wfs11Profile()})
	require.NoError(t, err)
	_, err = o.Query(context.Background(), "scale the kubernetes cluster workloads")
	require.NoError(t, err)

	first := wfs11Names(t, mdl, 0)
	assert.True(t, first["zeta_file_writer"], "feature off: every registered tool stays visible")
	assert.True(t, first["kube_scale"])
}

// TestWFS11SubAgentDoesNotInheritTheGate pins the escape-gate decision: the
// sub-orchestrator built in runSubAgentTurn does NOT carry OnDemand, so a
// delegated turn sees the FULL schema of its (already parent-filtered) tool
// set — delegation must not lose capabilities a second time through retrieval.
// This is the runSubAgentTurn check the W-F escape gate asks for when a new
// policy layer rides Config.
func TestWFS11SubAgentDoesNotInheritTheGate(t *testing.T) {
	step1 := schema.AssistantMessage("sub done", nil)
	mdl := einollm.NewFakeModelWithMessages([]*schema.Message{step1}, nil)
	mdl.RecordTools = true

	ts, _ := wfs11Toolset()
	// Parent HAS the gate on with MaxVisible=1 — a kubernetes query would
	// hide zeta_file_writer from the parent's own calls.
	o, err := New(Config{
		Model: mdl, Tools: ts, Profile: wfs11Profile(),
		OnDemand: tools.ToolLoadConfig{Enabled: true, MaxVisible: 1},
	})
	require.NoError(t, err)

	ctx := o.WithTurnContextForTest(context.Background(), TurnOpts{})
	out, err := o.runSubAgentTurn(ctx, "scale the kubernetes cluster workloads",
		[]string{"zeta_file_writer"}, "", 0)
	require.NoError(t, err)
	assert.Equal(t, "sub done", out)

	require.NotEmpty(t, mdl.ReceivedToolsHistory)
	sub := wfs11Names(t, mdl, 0)
	assert.True(t, sub["zeta_file_writer"],
		"the delegated tool set must reach the sub-agent's model unfiltered — the gate is a parent-turn concern")
}

// TestWFS11StateIsPerTurn pins that a tool loaded in turn 1 does NOT stay
// visible in turn 2: the load state is bound fresh by withTurnContext, so
// turn 2's first model call is back to always+retrieval.
func TestWFS11StateIsPerTurn(t *testing.T) {
	load := schema.AssistantMessage("", []schema.ToolCall{
		{ID: "c1", Type: "function", Function: schema.FunctionCall{
			Name: "tools_load", Arguments: `{"names":["zeta_file_writer"]}`,
		}},
	})
	mdl := einollm.NewFakeModelWithMessages([]*schema.Message{
		load,                               // turn 1 call 1
		schema.AssistantMessage("t1", nil), // turn 1 final
		schema.AssistantMessage("t2", nil), // turn 2 first (and final) call
	}, nil)
	mdl.RecordTools = true

	ts, _ := wfs11Toolset()
	o, err := New(Config{
		Model: mdl, Tools: ts, Profile: wfs11Profile(),
		OnDemand: tools.ToolLoadConfig{Enabled: true, MaxVisible: 1},
	})
	require.NoError(t, err)
	_, err = o.Query(context.Background(), "scale the kubernetes cluster workloads")
	require.NoError(t, err)

	// Turn 2: same orchestrator, fresh turn. The tool loaded in turn 1 must
	// be gone from this turn's first model call.
	_, err = o.Query(context.Background(), "scale the kubernetes cluster workloads")
	require.NoError(t, err)
	require.True(t, len(mdl.ReceivedToolsHistory) >= 3)
	secondTurnFirst := wfs11Names(t, mdl, 2)
	assert.False(t, secondTurnFirst["zeta_file_writer"],
		"a tool loaded in turn 1 must not leak into turn 2's schema")
}
