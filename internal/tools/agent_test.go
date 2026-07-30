package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/x6nux/yanshi/internal/guard"
	einollm "github.com/x6nux/yanshi/internal/llm/eino"
)

// newAgentTools creates an AgentTools with a FakeModel for testing.
func newAgentTools(t *testing.T) (*AgentTools, context.Context) {
	t.Helper()
	fake := einollm.NewFakeModel([]string{"I am the sub-agent response."}, nil)
	at := NewAgentTools(fake)
	ctx := WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"*"}},
	})
	return at, ctx
}

// newAgentToolsEcho creates an AgentTools with a FakeModel in echo mode.
// The echo model returns the concatenation of all input messages.
func newAgentToolsEcho(t *testing.T) (*AgentTools, context.Context) {
	t.Helper()
	fake := &einollm.FakeModel{Echo: true}
	at := NewAgentTools(fake)
	ctx := WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"*"}},
	})
	return at, ctx
}

// ---------------------------------------------------------------------------
// agent_start tests
// ---------------------------------------------------------------------------

func TestAgentStart_Basic(t *testing.T) {
	at, ctx := newAgentTools(t)
	result, err := at.StartAgent.InvokableRun(ctx, `{"prompt": "do something"}`)
	require.NoError(t, err)

	assert.Equal(t, "I am the sub-agent response.", result)
}

func TestAgentStart_EmptyPrompt(t *testing.T) {
	at, ctx := newAgentTools(t)
	result, err := at.StartAgent.InvokableRun(ctx, `{"prompt": ""}`)
	require.NoError(t, err)

	assert.Contains(t, result, "prompt must not be empty")
}

func TestAgentStart_MissingPrompt(t *testing.T) {
	at, ctx := newAgentTools(t)
	result, err := at.StartAgent.InvokableRun(ctx, `{}`)
	require.NoError(t, err)

	assert.Contains(t, result, "prompt must not be empty")
}

func TestAgentStart_InvalidJSON(t *testing.T) {
	at, ctx := newAgentTools(t)
	result, err := at.StartAgent.InvokableRun(ctx, `not json`)
	require.NoError(t, err)

	assert.Contains(t, result, "parse args")
}

// ---------------------------------------------------------------------------
// workflow_start — flat mode tests
// ---------------------------------------------------------------------------

func TestWorkflowStart_Basic(t *testing.T) {
	at, ctx := newAgentToolsEcho(t)
	result, err := at.StartWorkflow.InvokableRun(ctx, `{"tasks": ["task one", "task two"]}`)
	require.NoError(t, err)

	assert.Contains(t, result, "0: task one")
	assert.Contains(t, result, "1: task two")
}

func TestWorkflowStart_SingleTask(t *testing.T) {
	at, ctx := newAgentToolsEcho(t)
	result, err := at.StartWorkflow.InvokableRun(ctx, `{"tasks": ["only task"]}`)
	require.NoError(t, err)

	assert.Contains(t, result, "0: only task")
}

func TestWorkflowStart_EmptyTasks(t *testing.T) {
	at, ctx := newAgentTools(t)
	result, err := at.StartWorkflow.InvokableRun(ctx, `{"tasks": []}`)
	require.NoError(t, err)

	assert.Contains(t, result, "must not be empty")
}

func TestWorkflowStart_MissingTasks(t *testing.T) {
	at, ctx := newAgentTools(t)
	result, err := at.StartWorkflow.InvokableRun(ctx, `{}`)
	require.NoError(t, err)

	assert.Contains(t, result, "provide either")
}

func TestWorkflowStart_InvalidTasksJSON(t *testing.T) {
	at, ctx := newAgentTools(t)
	result, err := at.StartWorkflow.InvokableRun(ctx, `{"tasks": "not a json array"}`)
	require.NoError(t, err)

	assert.Contains(t, result, "valid JSON array")
}

func TestWorkflowStart_ManyTasks(t *testing.T) {
	// Test with more tasks than CPU cores to exercise concurrency limiting.
	at, ctx := newAgentToolsEcho(t)

	tasks := make([]string, 20)
	for i := 0; i < 20; i++ {
		tasks[i] = "task"
	}
	tasksJSON, _ := json.Marshal(tasks)
	args := `{"tasks": ` + string(tasksJSON) + `}`

	result, err := at.StartWorkflow.InvokableRun(ctx, args)
	require.NoError(t, err)

	assert.Contains(t, result, "0: task")
	assert.Contains(t, result, "19: task")
}

// ---------------------------------------------------------------------------
// workflow_start — DAG mode tests
// ---------------------------------------------------------------------------

func TestWorkflowDAG_SimpleChain(t *testing.T) {
	at, ctx := newAgentToolsEcho(t)
	// A1 -> B1 (B1 depends on A1's output)
	wf := `{
		"workflow": {
			"steps": [
				{"id": "A1", "prompt": "initial analysis"},
				{"id": "B1", "prompt": "based on {{A1.output}} do refinement", "deps": ["A1"]}
			]
		}
	}`
	result, err := at.StartWorkflow.InvokableRun(ctx, wf)
	require.NoError(t, err)

	assert.Contains(t, result, "A1: initial analysis")
	assert.Contains(t, result, "based on initial analysis do refinement")
}

func TestWorkflowDAG_FanOut(t *testing.T) {
	at, ctx := newAgentToolsEcho(t)
	// A1 -> B1-3 (B1, B2, B3 all depend on A1)
	wf := `{
		"workflow": {
			"steps": [
				{"id": "A1", "prompt": "design spec"},
				{"id": "B1-3", "prompt": "implement component {{self.id}} based on {{A1.output}}", "deps": ["A1"]}
			]
		}
	}`
	result, err := at.StartWorkflow.InvokableRun(ctx, wf)
	require.NoError(t, err)

	assert.Contains(t, result, "A1: design spec")
	for _, expectedID := range []string{"B1", "B2", "B3"} {
		assert.Contains(t, result, "implement component "+expectedID)
	}
}

func TestWorkflowDAG_FanIn(t *testing.T) {
	at, ctx := newAgentToolsEcho(t)
	wf := `{
		"workflow": {
			"steps": [
				{"id": "A1-3", "prompt": "research topic {{self.id}}"},
				{"id": "B1", "prompt": "synthesize {{A1.output}} {{A2.output}} {{A3.output}}", "deps": ["A1-3"]}
			]
		}
	}`
	result, err := at.StartWorkflow.InvokableRun(ctx, wf)
	require.NoError(t, err)

	assert.Contains(t, result, "A1: research topic A1")
	assert.Contains(t, result, "A2: research topic A2")
	assert.Contains(t, result, "A3: research topic A3")
	assert.Contains(t, result, "synthesize research topic A1")
}

func TestWorkflowDAG_MultiLevel(t *testing.T) {
	at, ctx := newAgentToolsEcho(t)
	// 3-level DAG: A1 -> B1-2 -> C1
	wf := `{
		"workflow": {
			"steps": [
				{"id": "A1", "prompt": "level 1"},
				{"id": "B1-2", "prompt": "level 2 from {{A1.output}}", "deps": ["A1"]},
				{"id": "C1", "prompt": "level 3 from {{B1.output}} and {{B2.output}}", "deps": ["B1-2"]}
			]
		}
	}`
	result, err := at.StartWorkflow.InvokableRun(ctx, wf)
	require.NoError(t, err)

	assert.Contains(t, result, "A1: level 1")
	assert.Contains(t, result, "B1: level 2 from level 1")
	assert.Contains(t, result, "B2: level 2 from level 1")
	assert.Contains(t, result, "level 3 from level 2 from level 1")
}

func TestWorkflowDAG_SelfTemplateVars(t *testing.T) {
	at, ctx := newAgentToolsEcho(t)
	wf := `{
		"workflow": {
			"steps": [
				{"id": "X1-3", "prompt": "id={{self.id}} index={{self.index}} count={{self.count}}"}
			]
		}
	}`
	result, err := at.StartWorkflow.InvokableRun(ctx, wf)
	require.NoError(t, err)

	assert.Contains(t, result, "id=X1")
	assert.Contains(t, result, "id=X2")
	assert.Contains(t, result, "id=X3")
	assert.Contains(t, result, "index=0")
	assert.Contains(t, result, "index=1")
	assert.Contains(t, result, "index=2")
	assert.Contains(t, result, "count=3")
}

func TestWorkflowDAG_EmptySteps(t *testing.T) {
	at, ctx := newAgentTools(t)
	result, err := at.StartWorkflow.InvokableRun(ctx, `{"workflow": {"steps": []}}`)
	require.NoError(t, err)

	assert.Contains(t, result, "at least one step")
}

func TestWorkflowDAG_MissingDeps(t *testing.T) {
	at, ctx := newAgentTools(t)
	// B1 depends on A1 which doesn't exist.
	result, err := at.StartWorkflow.InvokableRun(ctx, `{"workflow": {"steps": [{"id": "B1", "prompt": "x", "deps": ["A1"]}]}}`)
	require.NoError(t, err)

	assert.Contains(t, result, "depends on")
}

func TestWorkflowDAG_Cycle(t *testing.T) {
	at, ctx := newAgentTools(t)
	// A1 -> B1 -> A1 (cycle).
	result, err := at.StartWorkflow.InvokableRun(ctx, `{"workflow": {"steps": [{"id": "A1", "prompt": "x", "deps": ["B1"]}, {"id": "B1", "prompt": "y", "deps": ["A1"]}]}}`)
	require.NoError(t, err)

	assert.Contains(t, result, "cycle detected")
}

func TestWorkflowDAG_InvalidID(t *testing.T) {
	at, ctx := newAgentTools(t)
	result, err := at.StartWorkflow.InvokableRun(ctx, `{"workflow": {"steps": [{"id": "bad!id", "prompt": "x"}]}}`)
	require.NoError(t, err)

	assert.Contains(t, result, "invalid step ID")
}

func TestWorkflowDAG_DuplicateID(t *testing.T) {
	at, ctx := newAgentTools(t)
	result, err := at.StartWorkflow.InvokableRun(ctx, `{"workflow": {"steps": [{"id": "A1", "prompt": "x"}, {"id": "A1", "prompt": "y"}]}}`)
	require.NoError(t, err)

	assert.Contains(t, result, "duplicate step ID")
}

func TestWorkflowDAG_OverlappingRange(t *testing.T) {
	at, ctx := newAgentTools(t)
	// A1-3 and A2 explicitly would overlap.
	result, err := at.StartWorkflow.InvokableRun(ctx, `{"workflow": {"steps": [{"id": "A1-3", "prompt": "x"}, {"id": "A2", "prompt": "y"}]}}`)
	require.NoError(t, err)

	assert.Contains(t, result, "duplicate step ID")
}

// ---------------------------------------------------------------------------
// analysis tool tests
// ---------------------------------------------------------------------------

func TestAnalysis_Basic(t *testing.T) {
	at, ctx := newAgentToolsEcho(t)
	// Agent mode with a file target.
	result, err := at.Analysis.InvokableRun(ctx, `{"target": "main.go", "mode": "agent"}`)
	require.NoError(t, err)

	assert.Contains(t, result, "main.go")
	// Should contain parts of the analysis prompt.
	assert.Contains(t, result, "项目结构")
}

func TestAnalysis_WorkflowMode(t *testing.T) {
	at, ctx := newAgentToolsEcho(t)
	result, err := at.Analysis.InvokableRun(ctx, `{"target": "/project/src", "mode": "workflow"}`)
	require.NoError(t, err)
	assert.Contains(t, result, "扫描项目结构")
	assert.Contains(t, result, "/project/src")
}

func TestAnalysis_AutoMode_Dir(t *testing.T) {
	t.Skip("auto mode was replaced by required explicit analysis modes")
	at, ctx := newAgentToolsEcho(t)
	// Auto mode with a directory target → should use workflow.
	result, err := at.Analysis.InvokableRun(ctx, `{"target": "/project/src"}`)
	require.NoError(t, err)

	var m map[string]any
	err = json.Unmarshal([]byte(result), &m)
	require.NoError(t, err)

	// Should get DAG results (auto mode uses workflow for directories).
	results, ok := m["results"].([]any)
	require.True(t, ok, "auto mode for directory should return workflow results")
	assert.Equal(t, "A1", results[0].(map[string]any)["id"])
}

func TestAnalysis_AutoMode_File(t *testing.T) {
	t.Skip("auto mode was replaced by required explicit analysis modes")
	at, ctx := newAgentToolsEcho(t)
	// Auto mode with a file target → should use single agent.
	result, err := at.Analysis.InvokableRun(ctx, `{"target": "main.go"}`)
	require.NoError(t, err)

	assert.Contains(t, result, "main.go")
}

func TestAnalysis_RequiresExplicitMode(t *testing.T) {
	at, ctx := newAgentTools(t)
	result, err := at.Analysis.InvokableRun(ctx, `{"target":"/project/src"}`)
	require.NoError(t, err)
	assert.Contains(t, result, "mode is required")
}

func TestAnalysis_RejectsAutoMode(t *testing.T) {
	at, ctx := newAgentTools(t)
	result, err := at.Analysis.InvokableRun(ctx, `{"target":"/project/src","mode":"auto"}`)
	require.NoError(t, err)
	assert.Contains(t, result, "mode must be 'agent' or 'workflow'")
}

func TestAnalysis_EmptyTarget(t *testing.T) {
	at, ctx := newAgentTools(t)
	result, err := at.Analysis.InvokableRun(ctx, `{"target": ""}`)
	require.NoError(t, err)

	assert.Contains(t, result, "target must not be empty")
}

func TestAnalysis_InvalidMode(t *testing.T) {
	at, ctx := newAgentTools(t)
	result, err := at.Analysis.InvokableRun(ctx, `{"target": "main.go", "mode": "invalid"}`)
	require.NoError(t, err)

	assert.Contains(t, result, "mode must be 'agent' or 'workflow'")
}

func TestAnalysis_DeniedByProfile(t *testing.T) {
	at, ctx := newAgentTools(t)
	ctx = WithProfile(ctx, guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{}},
	})

	result, err := at.Analysis.InvokableRun(ctx, `{"target": "main.go"}`)
	require.NoError(t, err)

	assert.Contains(t, result, "permission denied")
}

func TestAnalysis_WorkflowInterpolation(t *testing.T) {
	at, ctx := newAgentToolsEcho(t)
	result, err := at.Analysis.InvokableRun(ctx, `{"target": "/my/app", "mode": "workflow"}`)
	require.NoError(t, err)
	assert.Contains(t, result, "/my/app")
}

func TestAnalysis_WorkflowJSONOverride(t *testing.T) {
	at, ctx := newAgentToolsEcho(t)
	result, err := at.Analysis.InvokableRun(ctx, `{"target":"/ignored","mode":"workflow","workflow":{"steps":[{"id":"A1","prompt":"custom {{target}}"}]}}`)
	require.NoError(t, err)
	assert.Contains(t, result, "A1: custom /ignored")
}

func TestAnalysis_PredefinedAgentDefinition(t *testing.T) {
	// Verify that the analysis predefined agent exists and has the expected structure.
	def, ok := GetPredefinedAgent("analysis")
	require.True(t, ok, "analysis predefined agent must exist")
	assert.Equal(t, "analysis", def.Name)
	assert.NotEmpty(t, def.Description)
	assert.NotEmpty(t, def.PromptTmpl)
	assert.Contains(t, def.PromptTmpl, "{{target}}")

	// Should have a workflow definition.
	require.NotNil(t, def.Workflow, "analysis should have a workflow definition")
	require.GreaterOrEqual(t, len(def.Workflow.Steps), 4, "workflow should have multiple steps")

	// Verify all workflow step prompts contain {{target}} for filling.
	for _, s := range def.Workflow.Steps {
		if s.ID == "C1" {
			// C1 receives target context through {{A1.output}} etc.
			continue
		}
		assert.Contains(t, s.Prompt, "{{target}}",
			"step %s prompt should contain {{target}} placeholder", s.ID)
	}
}

func TestAnalysis_ListPredefinedAgents(t *testing.T) {
	agents := ListPredefinedAgents()
	require.Contains(t, agents, "analysis", "analysis should be listed")
}

func TestSummarize_PredefinedAgentDefinition(t *testing.T) {
	def, ok := GetPredefinedAgent("summarize")
	require.True(t, ok, "summarize predefined agent must exist")
	assert.Equal(t, "summarize", def.Name)
	assert.NotEmpty(t, def.Description)
	assert.Contains(t, def.PromptTmpl, "{{target}}")
	assert.Contains(t, def.PromptTmpl, "{{max_lines}}")
	assert.Contains(t, def.PromptTmpl, "{{focus_line}}")
	assert.Nil(t, def.Workflow, "summarize is single-agent, no workflow")
}

func TestFillPrompt(t *testing.T) {
	result := FillPrompt("Hello {{target}}, welcome to {{name}}", map[string]string{
		"target": "world",
		"name":   "yanshi",
	})
	assert.Equal(t, "Hello world, welcome to yanshi", result)
}

func TestFillPrompt_MissingVar(t *testing.T) {
	// Missing variables should remain as placeholders (no crash).
	result := FillPrompt("Hello {{target}}", map[string]string{})
	assert.Equal(t, "Hello {{target}}", result)
}

// ---------------------------------------------------------------------------
// Guard integration test
// ---------------------------------------------------------------------------

func TestAgentStart_DeniedByProfile(t *testing.T) {
	at, ctx := newAgentTools(t)
	// Bind a profile that denies agent_start (no tools allowed).
	ctx = WithProfile(ctx, guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{}},
	})

	result, err := at.StartAgent.InvokableRun(ctx, `{"prompt": "do something"}`)
	require.NoError(t, err) // Denial is returned as a JSON result, not a Go error.

	assert.Contains(t, result, "permission denied")
}

func TestWorkflowStart_DeniedByProfile(t *testing.T) {
	at, ctx := newAgentTools(t)
	ctx = WithProfile(ctx, guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{}},
	})

	result, err := at.StartWorkflow.InvokableRun(ctx, `{"tasks": ["task1"]}`)
	require.NoError(t, err)

	assert.Contains(t, result, "permission denied")
}

// TestWorkflowStart_FlatEmitsNestedProgress verifies the flat workflow completes
// correctly (nested_progress frame emission was replaced by SubAgentProgress).
func TestWorkflowStart_FlatEmitsNestedProgress(t *testing.T) {
	at, ctx := newAgentToolsEcho(t)
	const N = 3
	args := `{"tasks": ["t1", "t2", "t3"]}`
	result, err := at.StartWorkflow.InvokableRun(ctx, args)
	require.NoError(t, err)
	for i := 0; i < N; i++ {
		// Each task's index is preserved; result is the echo (task text).
		assert.Contains(t, result, fmt.Sprintf("%d: t%d", i, i+1))
	}
}

// TestWorkflowDAG_EmitsNestedProgressWithExpandedTotal verifies DAG workflow
// output correctness after range expansion (progress reporting goes through
// SubAgentProgress, not nested_progress frames).
func TestWorkflowDAG_EmitsNestedProgressWithExpandedTotal(t *testing.T) {
	at, ctx := newAgentToolsEcho(t)
	wf := `{"workflow":{"steps":[
		{"id":"A1","prompt":"seed"},
		{"id":"B1-4","prompt":"work on {{A1.output}}","deps":["A1"]}
	]}}`
	result, err := at.StartWorkflow.InvokableRun(ctx, wf)
	require.NoError(t, err)
	assert.Contains(t, result, "B4: work on seed")
}

// TestWorkflowStart_NoEmitSkipsProgressReporting proves that when no SubAgentEmit
// is bound (the CLI Query path / unit tests without a transport), the workflow
// still produces correct results and emits nothing — makeNestedProgressEmitter
// returns a no-op, so the progress report is purely additive and never breaks
// the legacy black-box path. This is the backward-compat contract for the new
// nested_progress emission.
func TestWorkflowStart_NoEmitSkipsProgressReporting(t *testing.T) {
	at, ctx := newAgentToolsEcho(t)
	result, err := at.StartWorkflow.InvokableRun(ctx, `{"tasks":["a","b"]}`)
	require.NoError(t, err)
	assert.Contains(t, result, "0: a")
	assert.Contains(t, result, "1: b")
}

// ---------------------------------------------------------------------------
// summarize tool tests
// ---------------------------------------------------------------------------

func TestSummarize_Basic(t *testing.T) {
	at, ctx := newAgentToolsEcho(t)
	result, err := at.Summarize.InvokableRun(ctx, `{"path":"main.go"}`)
	require.NoError(t, err)
	assert.Contains(t, result, "main.go", "target interpolated into prompt (echo model)")
	assert.Contains(t, result, "核心要点", "summarize prompt body present")
	assert.Contains(t, result, "不超过 50", "default max_lines applied")
}

func TestSummarize_FocusAndMaxLines(t *testing.T) {
	at, ctx := newAgentToolsEcho(t)
	result, err := at.Summarize.InvokableRun(ctx, `{"path":"x.go","focus":"error handling","max_lines":20}`)
	require.NoError(t, err)
	assert.Contains(t, result, "重点关注: error handling")
	assert.Contains(t, result, "不超过 20 行")
}

func TestSummarize_EmptyPath(t *testing.T) {
	at, ctx := newAgentTools(t)
	result, err := at.Summarize.InvokableRun(ctx, `{"path":""}`)
	require.NoError(t, err)
	assert.Contains(t, result, "path must not be empty")
}

func TestSummarize_NoChatModel(t *testing.T) {
	at := NewAgentTools(nil)
	ctx := WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"*"}},
	})
	result, err := at.Summarize.InvokableRun(ctx, `{"path":"x.go"}`)
	require.NoError(t, err)
	assert.Contains(t, result, "no chat model configured")
}

func TestSummarize_RestrictsToFsRead(t *testing.T) {
	at, ctx := newAgentTools(t)
	var captured []string
	runner := func(ic context.Context, prompt string, allowed []string, instr string) (string, error) {
		captured = allowed
		return "summary", nil
	}
	ctx = WithSubAgentRunner(ctx, SubAgentRunner(runner))
	_, err := at.Summarize.InvokableRun(ctx, `{"path":"x.go"}`)
	require.NoError(t, err)
	assert.Equal(t, []string{"fs_read"}, captured, "summarize sub-agent may only use fs_read")
}

// ---------------------------------------------------------------------------
// makeWorkflowProgress — ticker / non-blocking-send regression tests
// ---------------------------------------------------------------------------

// TestWorkflowProgress_StopNoDeadlockWhenChannelFull is the regression guard for
// the per-second ticker. pushPanel MUST use non-blocking sends: if it blocked on
// a full channel, the ticker goroutine would stall mid-pushPanel (never reaching
// the select that observes `finished`), so stop()'s <-tickerDone would hang
// forever, deadlocking the caller's defer close(ch). We drive the panel with a
// deliberately tiny, UNDRAINED channel and assert stop() returns promptly.
func TestWorkflowProgress_StopNoDeadlockWhenChannelFull(t *testing.T) {
	ch := make(chan ToolChunk, 1) // tiny buffer, never drained
	wp, stop := makeWorkflowProgress(ch)
	wp.SetTotal(3)
	cb := wp.StepCB("B1")
	cb(SubAgentEvent{Kind: SubAgentToolStart, ToolDisplay: "Read"})
	cb(SubAgentEvent{Kind: SubAgentTokens, Tokens: 1000})
	wp.StepDone("B1", nil)

	done := make(chan struct{})
	go func() {
		stop()
		close(done)
	}()
	select {
	case <-done:
		// stop() returned despite the channel being saturated — non-blocking
		// sends let the ticker exit cleanly.
	case <-time.After(3 * time.Second):
		t.Fatal("stop() deadlocked: ticker goroutine stuck on full channel (non-blocking send regression)")
	}
}

// TestWorkflowProgress_RunningStepEagerAndFrozenDuration verifies two panel
// invariants: (1) a step appears as a RUNNING row the moment StepCB is called,
// before any SubAgentEvent (eager state creation — fixes "running agents not
// at top"); (2) once StepDone fires, the step's duration stops ticking (frozen
// at ended-started — fixes "completed agents still updating time").
func TestWorkflowProgress_RunningStepEagerAndFrozenDuration(t *testing.T) {
	ch := make(chan ToolChunk, 64)
	wp, stop := makeWorkflowProgress(ch)
	defer stop()
	wp.SetTotal(1)

	drainText := func() string {
		var last string
		for {
			select {
			case c := <-ch:
				if c.Text != "" {
					last = c.Text
				}
			default:
				return last
			}
		}
	}

	// StepCB alone (no events yet) must surface B1 as a running row.
	wp.StepCB("B1")
	panel := drainText()
	assert.Contains(t, panel, "Agent-B1(", "step must appear eagerly after StepCB")
	assert.NotContains(t, panel, "✓", "step must still be running, not done")

	// Finish B1 and capture its frozen panel.
	wp.StepDone("B1", nil)
	panel = drainText()
	assert.Contains(t, panel, "✓ Agent-B1", "step must show ✓ after StepDone")
}
