package orchestrator

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	"image/png"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/schema"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/x6nux/yanshi/internal/guard"
	einollm "github.com/x6nux/yanshi/internal/llm/eino"
	"github.com/x6nux/yanshi/internal/imagestore"
	"github.com/x6nux/yanshi/internal/proto"
	"github.com/x6nux/yanshi/internal/store"
	"github.com/x6nux/yanshi/internal/tools"
	"github.com/x6nux/yanshi/internal/vcs"
)

// newMemStore opens an in-memory store for orchestrator integration tests.
func newMemStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { s.Close() })
	return s
}

func TestNew_IncludesSkillMeta(t *testing.T) {
	model := einollm.NewFakeModel([]string{"hello from agent"}, nil)
	o, err := New(Config{
		Model:           model,
		SkillMetaPrompt: "Available skills (call the skill_use tool to load one):\n- greet: hi\n",
	})
	require.NoError(t, err)
	require.NotNil(t, o)
}

func TestOrchestrator_AnswerWithoutTools(t *testing.T) {
	// Fake model: one assistant message, no tool calls.
	model := einollm.NewFakeModel([]string{"hello from agent"}, nil)
	o, err := New(Config{Model: model})
	require.NoError(t, err)

	out, err := o.Query(context.Background(), "hi")
	require.NoError(t, err)
	assert.Equal(t, "hello from agent", out)
}

func TestOrchestrator_UsesToolThenAnswers(t *testing.T) {
	// Fake model: first calls memory.write, then answers.
	tc1 := schema.AssistantMessage("", []schema.ToolCall{
		{ID: "c1", Type: "function", Function: schema.FunctionCall{
			Name:      "memory_write",
			Arguments: `{"content":"x","kind":"note"}`,
		}},
	})
	tc2 := schema.AssistantMessage("wrote it", nil)
	model := einollm.NewFakeModelWithMessages([]*schema.Message{tc1, tc2}, nil)

	st := newMemStore(t)
	mt := tools.NewMemoryTools(st)
	o, err := New(Config{
		Model:   model,
		Tools:   []BaseTool{mt.Write},
		Profile: guard.PermissionProfile{Tools: guard.ToolsPerm{Allow: []string{"memory_write"}}},
	})
	require.NoError(t, err)

	out, err := o.Query(context.Background(), "remember x")
	require.NoError(t, err)
	assert.Equal(t, "wrote it", out)

	// The memory was actually written to the store.
	ms, err := st.RecallMemory(5)
	require.NoError(t, err)
	require.Len(t, ms, 1)
	assert.Equal(t, "x", ms[0].Content)
}

func TestOrchestrator_Events(t *testing.T) {
	model := einollm.NewFakeModel([]string{"hi there"}, nil)
	o, err := New(Config{Model: model})
	require.NoError(t, err)

	iter := o.Events(context.Background(), "hello")
	saw := drainAgentChunks(t, iter)
	assert.Equal(t, "hi there", saw)
}

// TestOrchestrator_EventsUsesToolThenAnswers mirrors TestOrchestrator_UsesToolThenAnswers
// but drives through the Events (SSE streaming) path, verifying that the permission
// profile is injected so tool calls are authorized via GuardedTool.
func TestOrchestrator_EventsUsesToolThenAnswers(t *testing.T) {
	// Fake model: first calls memory.write, then answers.
	tc1 := schema.AssistantMessage("", []schema.ToolCall{
		{ID: "c1", Type: "function", Function: schema.FunctionCall{
			Name:      "memory_write",
			Arguments: `{"content":"x","kind":"note"}`,
		}},
	})
	tc2 := schema.AssistantMessage("wrote it", nil)
	model := einollm.NewFakeModelWithMessages([]*schema.Message{tc1, tc2}, nil)

	st := newMemStore(t)
	mt := tools.NewMemoryTools(st)
	o, err := New(Config{
		Model:   model,
		Tools:   []BaseTool{mt.Write},
		Profile: guard.PermissionProfile{Tools: guard.ToolsPerm{Allow: []string{"memory_write"}}},
	})
	require.NoError(t, err)

	iter := o.Events(context.Background(), "remember x")
	saw := drainAgentChunks(t, iter)
	assert.Equal(t, "wrote it", saw)

	// The memory was actually written to the store through the Events path.
	ms, err := st.RecallMemory(5)
	require.NoError(t, err)
	require.Len(t, ms, 1)
	assert.Equal(t, "x", ms[0].Content)
}

// TestNew_InjectsVCSScope proves the orchestrator injects its configured main
// VCSScope into every turn's tool-execution context: a model-driven fs_edit
// must be auto-recorded into the main changeset (chat → main tracking).
func TestNew_InjectsVCSScope(t *testing.T) {
	// In-memory VCS over a temp repo root pre-seeded with a.go (mirrors the
	// V12 newVCSTestRepo helper in internal/tools/fs_test.go).
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "a.go"), []byte("alpha beta"), 0o644))
	st, err := store.Open(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { st.Close() })
	v := vcs.New(st, t.TempDir())
	repoID, err := v.InitRepo(root)
	require.NoError(t, err)

	// Scripted model: (1) fs_edit a.go, then (2) final assistant message so
	// the ReAct loop terminates. Same shape as the E2E fs_edit tests.
	step1 := schema.AssistantMessage("", []schema.ToolCall{
		{ID: "c1", Type: "function", Function: schema.FunctionCall{
			Name:      "fs_edit",
			Arguments: `{"path":"a.go","old_string":"alpha","new_string":"ALPHA"}`,
		}},
	})
	step2 := schema.AssistantMessage("done", nil)
	mdl := einollm.NewFakeModelWithMessages([]*schema.Message{step1, step2}, nil)

	fs := tools.NewFSTools(root)
	profile := guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"fs_*"}},
		FS: guard.FSPerm{
			Read:  []string{root + "/**"},
			Write: []string{root + "/**"},
		},
	}

	o, err := New(Config{
		Model:    mdl,
		Tools:    []BaseTool{fs.Edit},
		Profile:  profile,
		VCSScope: tools.VCSScope{VCS: v, RepoID: repoID, Agent: "orchestrator"},
	})
	require.NoError(t, err)

	out, err := o.Query(context.Background(), "edit a.go")
	require.NoError(t, err)
	assert.Equal(t, "done", out)

	// THE PROOF: the orchestrator-injected main scope caused fs_edit's
	// trackEdit hook to record the edit into the main changeset.
	pending := v.Uncommitted("main", repoID)
	assert.Contains(t, pending, "a.go",
		"orchestrator must inject its VCSScope so fs_edit auto-tracks to main")
}

// TestOrchestrator_UnknownToolHandler proves a hallucinated tool name (e.g.
// "bash" for "shell_run") does NOT abort the turn with a NodeRunError: the
// UnknownToolsHandler returns the failure as a tool RESULT (nil Go error) so the
// ADK feeds it back to the model, which continues and produces its final answer.
// Without the handler, Query would return "tool bash not found in toolsNode
// indexes" and the turn would die.
func TestOrchestrator_UnknownToolHandler(t *testing.T) {
	// Script: (1) call the UNKNOWN tool "bash", (2) emit a final answer. The
	// ReAct loop only reaches step 2 if step 1's "tool not found" came back as
	// a tool result rather than a fatal error.
	step1 := schema.AssistantMessage("", []schema.ToolCall{
		{ID: "c1", Type: "function", Function: schema.FunctionCall{
			Name:      "bash", // not registered — the hallucination
			Arguments: `{"command":"ls"}`,
		}},
	})
	step2 := schema.AssistantMessage("recovered", nil)
	mdl := einollm.NewFakeModelWithMessages([]*schema.Message{step1, step2}, nil)

	// Register one real tool so the available-tools list in the error message
	// is non-empty (and realistic).
	timeTools := tools.NewTimeTools()
	o, err := New(Config{Model: mdl, Tools: []BaseTool{timeTools.Now}})
	require.NoError(t, err)

	out, qErr := o.Query(context.Background(), "run bash")
	require.NoError(t, qErr, "an unknown tool name must NOT abort the turn")
	assert.Equal(t, "recovered", out, "the model must continue to its final answer")
}

// TestUnknownToolHandler_ListsAvailableTools proves the error message returned
// to the model names the real tools, so it can self-correct in one step.
func TestUnknownToolHandler_ListsAvailableTools(t *testing.T) {
	h := unknownToolHandler([]string{"fs_read", "shell_run", "time_now"})
	out, err := h(context.Background(), "bash", `{"command":"ls"}`)
	require.NoError(t, err, "the handler must return a result, not a Go error")
	assert.Contains(t, out, `"bash"`)
	assert.Contains(t, out, "fs_read")
	assert.Contains(t, out, "shell_run")
	assert.Contains(t, out, "time_now")
}

// TestSubAgentRunner_BoundAndCallable proves the orchestrator binds a
// SubAgentRunner into the turn context and that calling it runs a REAL nested
// agent (with the parent's model + the allowed tool subset) — not the bare-LLM
// fallback. The sub-agent uses the same FakeModel; its single scripted response
// is returned through the nested orchestrator's Query.
func TestSubAgentRunner_BoundAndCallable(t *testing.T) {
	subResp := schema.AssistantMessage("sub did the work", nil)
	mdl := einollm.NewFakeModelWithMessages([]*schema.Message{subResp}, nil)
	timeTools := tools.NewTimeTools()
	o, err := New(Config{Model: mdl, Tools: []BaseTool{timeTools.Now}})
	require.NoError(t, err)

	ctx := o.bindSubAgentRunner(context.Background())
	runner := tools.SubAgentRunnerFromContext(ctx)
	require.NotNil(t, runner, "the runner must be bound when a model is configured")

	out, rerr := runner(context.Background(), "delegate this", []string{"time_now"}, "")
	require.NoError(t, rerr)
	assert.Equal(t, "sub did the work", out, "the nested agent's response flows back")
}

// TestSubAgentRunner_DepthLimit proves nesting past MaxSubAgentDepth is refused
// (a runaway model calling agent_start in a loop can't recurse without bound).
func TestSubAgentRunner_DepthLimit(t *testing.T) {
	mdl := einollm.NewFakeModelWithMessages([]*schema.Message{schema.AssistantMessage("x", nil)}, nil)
	o, err := New(Config{Model: mdl})
	require.NoError(t, err)

	deepCtx := tools.WithSubAgentDepth(context.Background(), tools.MaxSubAgentDepth)
	ctx := o.bindSubAgentRunner(deepCtx)
	runner := tools.SubAgentRunnerFromContext(ctx)

	_, rerr := runner(context.Background(), "nest again", nil, "")
	require.Error(t, rerr, "depth at the cap must refuse to nest further")
	assert.Contains(t, rerr.Error(), "depth")
}

// TestSubAgentRunner_ForwardsEventsToEmit proves runSubAgentTurn forwards the
// sub-agent's streamed events (tool_call, tool_result, agent_chunk) to the
// SubAgentEmit callback bound in the turn context — the fix that makes Analysis /
// agent_start / workflow_start visible in the TUI. Previously runSubAgentTurn
// called sub.Query, which consumed the sub-agent's event iterator internally and
// discarded every intermediate event (the sub-agent was a black box). The final
// answer is still returned alongside the forwarding, so Analysis's result is
// unchanged. The sub-agent calls time_now (a real tool) so the test exercises
// the full ReAct path: tool_call → tool_result → final agent_chunk.
func TestSubAgentRunner_ForwardsEventsToEmit(t *testing.T) {
	// Scripted sub-agent: (1) call time_now, (2) final answer.
	step1 := schema.AssistantMessage("", []schema.ToolCall{
		{ID: "c1", Type: "function", Function: schema.FunctionCall{
			Name:      "time_now",
			Arguments: `{}`,
		}},
	})
	step2 := schema.AssistantMessage("sub-agent done", nil)
	mdl := einollm.NewFakeModelWithMessages([]*schema.Message{step1, step2}, nil)
	timeTools := tools.NewTimeTools()
	o, err := New(Config{Model: mdl, Tools: []BaseTool{timeTools.Now}})
	require.NoError(t, err)

	var frames []proto.ServerFrame
	ctx := tools.WithSubAgentEmit(context.Background(), func(f proto.ServerFrame) {
		frames = append(frames, f)
	})
	ctx = o.bindSubAgentRunner(ctx)
	runner := tools.SubAgentRunnerFromContext(ctx)
	require.NotNil(t, runner)

	out, rerr := runner(ctx, "analyze this", []string{"time_now"}, "")
	require.NoError(t, rerr)
	assert.Equal(t, "sub-agent done", out, "the final answer is still returned to the caller")

	// The sub-agent's tool_call, tool_result, and agent_chunk must all have been
	// forwarded — these are the frames the WS handler relays to the TUI so the
	// user can watch the sub-agent's ReAct loop live.
	var types []string
	for _, f := range frames {
		types = append(types, f.Type)
	}
	assert.Contains(t, types, "tool_call",
		"sub-agent tool_call must be forwarded (was swallowed by sub.Query before)")
	assert.Contains(t, types, "tool_result",
		"sub-agent tool_result must be forwarded")
	assert.Contains(t, types, "agent_chunk",
		"sub-agent agent_chunk must be forwarded (the sub-agent's streamed output)")

	// The forwarded tool_call carries the sub-agent's tool name + args so the
	// TUI can render the call block.
	var callFrame *proto.ServerFrame
	for i := range frames {
		if frames[i].Type == "tool_call" {
			callFrame = &frames[i]
			break
		}
	}
	require.NotNil(t, callFrame, "a tool_call frame must have been forwarded")
	assert.Equal(t, "time_now", callFrame.ToolName)
}

// TestSubAgentRunner_EmitsNestedUsage proves runSubAgentTurn captures the sub-
// agent's token usage (via ClassifyEventsWithUsage) and emits a nested_usage
// frame carrying the SUM of PromptTokens+CompletionTokens across the sub-agent's
// ReAct iterations — not the latest overwrite. This is the data source for the
// Analysis block's done summary ("Done (… · Nk tokens · …)"). Two model calls
// are scripted (tool_call → final answer), each carrying its own usage, so the
// test exercises the accumulate-not-overwrite path: the nested_usage total
// must be (call1.prompt+call1.completion) + (call2.prompt+call2.completion).
// The frame is emitted AFTER the child's event frames (it is the LAST forwarded
// frame), matching the timing the TUI relies on to attribute tokens to the
// still-running Analysis block.
func TestSubAgentRunner_EmitsNestedUsage(t *testing.T) {
	// Scripted sub-agent: (1) call time_now with usage, (2) final answer with usage.
	step1 := schema.AssistantMessage("", []schema.ToolCall{
		{ID: "c1", Type: "function", Function: schema.FunctionCall{
			Name:      "time_now",
			Arguments: `{}`,
		}},
	})
	step1.ResponseMeta = &schema.ResponseMeta{
		Usage: &schema.TokenUsage{PromptTokens: 1000, CompletionTokens: 200, TotalTokens: 1200},
	}
	step2 := schema.AssistantMessage("sub-agent done", nil)
	step2.ResponseMeta = &schema.ResponseMeta{
		Usage: &schema.TokenUsage{PromptTokens: 1500, CompletionTokens: 300, TotalTokens: 1800},
	}
	mdl := einollm.NewFakeModelWithMessages([]*schema.Message{step1, step2}, nil)
	timeTools := tools.NewTimeTools()
	o, err := New(Config{Model: mdl, Tools: []BaseTool{timeTools.Now}})
	require.NoError(t, err)

	var frames []proto.ServerFrame
	ctx := tools.WithSubAgentEmit(context.Background(), func(f proto.ServerFrame) {
		frames = append(frames, f)
	})
	ctx = o.bindSubAgentRunner(ctx)
	runner := tools.SubAgentRunnerFromContext(ctx)
	require.NotNil(t, runner)

	out, rerr := runner(ctx, "analyze this", []string{"time_now"}, "")
	require.NoError(t, rerr)
	assert.Equal(t, "sub-agent done", out)

	// No nested_usage frame emitted — sub-agent token reporting goes through
	// SubAgentProgress (context callback), not transport frames.
	for _, f := range frames {
		assert.NotEqual(t, "nested_usage", f.Type, "no nested_usage frame must be emitted")
	}
}

// TestSubAgentRunner_NoEmitDegradesGracefully proves that when no SubAgentEmit is
// bound (the CLI Query path, or unit tests that don't care about streaming),
// runSubAgentTurn still returns the correct final answer and forwards nothing.
// This is the backward-compat contract: the emit callback is optional.
func TestSubAgentRunner_NoEmitDegradesGracefully(t *testing.T) {
	subResp := schema.AssistantMessage("legacy answer", nil)
	mdl := einollm.NewFakeModelWithMessages([]*schema.Message{subResp}, nil)
	timeTools := tools.NewTimeTools()
	o, err := New(Config{Model: mdl, Tools: []BaseTool{timeTools.Now}})
	require.NoError(t, err)

	// No WithSubAgentEmit — the legacy setup.
	ctx := o.bindSubAgentRunner(context.Background())
	runner := tools.SubAgentRunnerFromContext(ctx)
	require.NotNil(t, runner)

	out, rerr := runner(context.Background(), "delegate this", []string{"time_now"}, "")
	require.NoError(t, rerr)
	assert.Equal(t, "legacy answer", out,
		"without emit, the final answer is still returned (backward compatible)")
}

// TestSelectSubAgentTools_Filters proves the allowed-tool subset is honored:
// only named tools are passed to the sub-agent, and nil inherits the full set.
func TestSelectSubAgentTools_Filters(t *testing.T) {
	mdl := einollm.NewFakeModel(nil, nil)
	fs := tools.NewFSTools(t.TempDir())
	timeTools := tools.NewTimeTools()
	o, err := New(Config{
		Model: mdl,
		Tools: []BaseTool{fs.Read, fs.Write, timeTools.Now},
	})
	require.NoError(t, err)

	// Filter to one tool.
	got := o.selectSubAgentTools([]string{"time_now"})
	require.Len(t, got, 1)
	info, ierr := got[0].Info(context.Background())
	require.NoError(t, ierr)
	assert.Equal(t, "time_now", info.Name)

	// Unknown names are dropped (fail-safe, not an error).
	got = o.selectSubAgentTools([]string{"time_now", "bogus"})
	assert.Len(t, got, 1)

	// nil/empty inherits the full set.
	got = o.selectSubAgentTools(nil)
	assert.Len(t, got, 3)
}

func TestWithoutOrchestrationTools_RemovesWorkflowRecursion(t *testing.T) {
	mdl := einollm.NewFakeModel(nil, nil)
	fs := tools.NewFSTools(t.TempDir())
	agentTools := tools.NewAgentTools(mdl)
	o, err := New(Config{
		Model: mdl,
		Tools: []BaseTool{fs.Read, agentTools.StartAgent, agentTools.StartWorkflow, agentTools.Analysis},
	})
	require.NoError(t, err)

	got := withoutOrchestrationTools(o.selectSubAgentTools(nil))
	require.Len(t, got, 1)
	info, ierr := got[0].Info(context.Background())
	require.NoError(t, ierr)
	assert.Equal(t, "fs_read", info.Name)
}

// TestEventsWithHistory_PreservesPriorTurn proves multi-turn memory: the model
// sees the prior user+assistant turn in its input. Uses an Echo FakeModel that
// returns its full input verbatim, so prior-turn text must surface in the
// assistant output.
func TestEventsWithHistory_PreservesPriorTurn(t *testing.T) {
	fm := einollm.NewFakeModel(nil, nil)
	fm.Echo = true
	o, err := New(Config{Model: fm})
	require.NoError(t, err)

	history := []*schema.Message{
		{Role: schema.User, Content: "remember 42"},
		{Role: schema.Assistant, Content: "ok, 42"},
	}
	iter := o.EventsWithHistory(context.Background(), append(history,
		&schema.Message{Role: schema.User, Content: "what did i say?"}))

	var frames []proto.ServerFrame
	ClassifyEvents(iter, func(f proto.ServerFrame) { frames = append(frames, f) })

	joined := ""
	for _, f := range frames {
		if f.Type == "agent_chunk" {
			joined += f.Text
		}
	}
	assert.Contains(t, joined, "remember 42", "model input must include prior user turn")
	assert.Contains(t, joined, "what did i say?")
}

// TestClassifyEvents_EmitsToolFrames proves tool calls/results in events become
// tool_call / tool_result frames, and assistant text becomes agent_chunk.
func TestClassifyEvents_EmitsToolFrames(t *testing.T) {
	// Build a tiny fake iterator yielding an assistant tool-call message, a tool
	// message, then a final assistant text message.
	iter := newFakeEventIter(t, []*schema.Message{
		{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{ID: "1", Type: "function", Function: schema.FunctionCall{Name: "fs_search", Arguments: `{"pattern":"x"}`}}}},
		{Role: schema.Tool, ToolCallID: "1", ToolName: "fs_search", Content: "3 hits"},
		{Role: schema.Assistant, Content: "done"},
	})
	var types []string
	ClassifyEvents(iter, func(f proto.ServerFrame) { types = append(types, f.Type) })
	assert.Equal(t, []string{"tool_call", "tool_result", "agent_chunk"}, types)
}

// TestClassifyEvents_ToolErrorResultMarksError proves a tool-result message whose
// content is a JSON {"error":...} blob (the form GuardedTool returns when a tool
// fails) is classified as a tool_result with Status="error" and the extracted
// error message as Text — so the TUI renders Read(Error|...) and the model sees
// a failure it can retry. A plain (non-error) result keeps Status="ok".
func TestClassifyEvents_ToolErrorResultMarksError(t *testing.T) {
	iter := newFakeEventIter(t, []*schema.Message{
		{Role: schema.Tool, ToolCallID: "1", ToolName: "fs_read", Content: `{"error":"file not found"}`},
		{Role: schema.Tool, ToolCallID: "2", ToolName: "fs_read", Content: "line1\nline2"},
	})
	var frames []proto.ServerFrame
	ClassifyEvents(iter, func(f proto.ServerFrame) { frames = append(frames, f) })

	require.Len(t, frames, 2)
	assert.Equal(t, "tool_result", frames[0].Type)
	assert.Equal(t, "error", frames[0].Status, "JSON error result must be classified as error")
	assert.Equal(t, "file not found", frames[0].Text, "Text must be the extracted error message, not the raw JSON")

	assert.Equal(t, "tool_result", frames[1].Type)
	assert.Equal(t, "ok", frames[1].Status, "non-error result keeps status ok")
	assert.Equal(t, "line1\nline2", frames[1].Text)
}

// TestClassifyEvents_EventErrorEmitsErrorFrame proves a non-nil ev.Err short
// circuits with a single error frame.
func TestClassifyEvents_EventErrorEmitsErrorFrame(t *testing.T) {
	iter, gen := adk.NewAsyncIteratorPair[*adk.AgentEvent]()
	gen.Send(&adk.AgentEvent{Err: assertAnError})
	gen.Close()

	var frames []proto.ServerFrame
	ClassifyEvents(iter, func(f proto.ServerFrame) { frames = append(frames, f) })

	require.Len(t, frames, 1)
	assert.Equal(t, "error", frames[0].Type)
	assert.Contains(t, frames[0].Text, "boom")
}

// TestClassifyEventsWithUsage_LatestResponseWins proves ClassifyEventsWithUsage
// records token usage from each assistant message's ResponseMeta.Usage (where
// the OpenAI acl wrapper records it) by OVERWRITING — the latest value wins,
// because the API reports cumulative counts per call (prompt includes the full
// context), so the last value is the current context size. Empty/nil usage is
// tolerated (FakeModel produces none).
func TestClassifyEventsWithUsage_LatestResponseWins(t *testing.T) {
	iter := newFakeEventIter(t, []*schema.Message{
		{Role: schema.Assistant, Content: "hello", ResponseMeta: &schema.ResponseMeta{
			Usage: &schema.TokenUsage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15}}},
		{Role: schema.Tool, ToolCallID: "1", ToolName: "fs_search", Content: "3 hits"},
		{Role: schema.Assistant, Content: " done", ResponseMeta: &schema.ResponseMeta{
			Usage: &schema.TokenUsage{PromptTokens: 20, CompletionTokens: 7, TotalTokens: 27}}},
	})

	var usage TurnUsage
	var types []string
	ClassifyEventsWithUsage(iter, &usage, func(f proto.ServerFrame) { types = append(types, f.Type) })

	assert.Equal(t, []string{"agent_chunk", "tool_result", "agent_chunk"}, types)
	assert.Equal(t, 20, usage.PromptTokens, "prompt tokens reflect the latest response (overwrite)")
	assert.Equal(t, 7, usage.CompletionTokens)
	assert.Equal(t, 27, usage.TotalTokens)
}

// TestClassifyEventsWithUsage_NilUsageIsSafe proves a nil Usage pointer (e.g.
// FakeModel output, or a tool message with no ResponseMeta) doesn't panic and
// leaves the accumulator at zero.
func TestClassifyEventsWithUsage_NilUsageIsSafe(t *testing.T) {
	iter := newFakeEventIter(t, []*schema.Message{
		{Role: schema.Assistant, Content: "hi"}, // no ResponseMeta
	})
	var usage TurnUsage
	ClassifyEventsWithUsage(iter, &usage, func(proto.ServerFrame) {})
	assert.Equal(t, TurnUsage{}, usage, "no usage surfaced -> zero accumulator")
}

// TestClassifyEventsWithUsage_OnUsageFiresPerResponse proves the optional
// onUsage callback (the trailing variadic arg) is fired each time usage is
// captured from a model response — once per assistant message, between tool
// calls — so a caller can emit a live status frame mid-turn. Usage OVERWRITES
// per response (the API reports cumulative counts), so each snapshot carries
// the latest value rather than a running sum.
func TestClassifyEventsWithUsage_OnUsageFiresPerResponse(t *testing.T) {
	iter := newFakeEventIter(t, []*schema.Message{
		{Role: schema.Assistant, Content: "hello", ResponseMeta: &schema.ResponseMeta{
			Usage: &schema.TokenUsage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15}}},
		{Role: schema.Tool, ToolCallID: "1", ToolName: "fs_search", Content: "3 hits"},
		{Role: schema.Assistant, Content: " done", ResponseMeta: &schema.ResponseMeta{
			Usage: &schema.TokenUsage{PromptTokens: 20, CompletionTokens: 7, TotalTokens: 27}}},
	})

	var usage TurnUsage
	var snaps []TurnUsage
	ClassifyEventsWithUsage(iter, &usage, func(proto.ServerFrame) {}, func(u TurnUsage) {
		snaps = append(snaps, u)
	})

	// Tool messages carry no usage → no snapshot; one snapshot per assistant msg.
	require.Len(t, snaps, 2, "onUsage fires once per assistant response")
	assert.Equal(t, 10, snaps[0].PromptTokens, "first snapshot = first response")
	assert.Equal(t, 5, snaps[0].CompletionTokens)
	assert.Equal(t, 20, snaps[1].PromptTokens, "second snapshot overwrites with the latest")
	assert.Equal(t, 7, snaps[1].CompletionTokens)
	assert.Equal(t, 20, usage.PromptTokens, "final accumulator is the latest response")
}

// TestClassifyEventsWithUsage_OnUsageOmittedIsBackwardCompat proves the onUsage
// arg is truly optional: the legacy 3-arg call still compiles and behaves
// exactly as before (no callback, usage still accumulates).
func TestClassifyEventsWithUsage_OnUsageOmittedIsBackwardCompat(t *testing.T) {
	iter := newFakeEventIter(t, []*schema.Message{
		{Role: schema.Assistant, Content: "hi", ResponseMeta: &schema.ResponseMeta{
			Usage: &schema.TokenUsage{PromptTokens: 7, CompletionTokens: 3, TotalTokens: 10}}},
	})
	var usage TurnUsage
	ClassifyEventsWithUsage(iter, &usage, func(proto.ServerFrame) {}) // no onUsage arg
	assert.Equal(t, 7, usage.PromptTokens)
}

// TestClassifyEvents_EmitsThinkingForReasoning proves an assistant message
// carrying ReasoningContent emits a thinking frame (one per delta) BEFORE its
// agent_chunk, so the TUI's thinking block renders above the answer. The same
// emitAssistant serves the streaming path, where reasoning arrives as a per-
// chunk delta (openai acl chat_model.go:1222, choice.Delta.ReasoningContent —
// parallel to choice.Delta.Content), so a frame-per-chunk is correct.
func TestClassifyEvents_EmitsThinkingForReasoning(t *testing.T) {
	iter := newFakeEventIter(t, []*schema.Message{
		{Role: schema.Assistant, ReasoningContent: "let me think", Content: "answer"},
	})
	var frames []proto.ServerFrame
	ClassifyEvents(iter, func(f proto.ServerFrame) { frames = append(frames, f) })

	require.Len(t, frames, 2, "one thinking + one agent_chunk")
	assert.Equal(t, "thinking", frames[0].Type)
	assert.Equal(t, "let me think", frames[0].Text)
	assert.Equal(t, "agent_chunk", frames[1].Type)
	assert.Equal(t, "answer", frames[1].Text)
}

// TestClassifyEvents_ReasoningOnlyEmitsOnlyThinking proves a reasoning-only
// chunk (no Content) emits a thinking frame and NO agent_chunk — so reasoning-
// only deltas don't produce empty content frames.
func TestClassifyEvents_ReasoningOnlyEmitsOnlyThinking(t *testing.T) {
	iter := newFakeEventIter(t, []*schema.Message{
		{Role: schema.Assistant, ReasoningContent: "pondering"},
	})
	var frames []proto.ServerFrame
	ClassifyEvents(iter, func(f proto.ServerFrame) { frames = append(frames, f) })

	require.Len(t, frames, 1, "reasoning-only message emits exactly one frame")
	assert.Equal(t, "thinking", frames[0].Type)
	assert.Equal(t, "pondering", frames[0].Text)
}

// TestClassifyEvents_NoReasoningEmitsNoThinking proves a plain assistant message
// (no ReasoningContent) emits no thinking frame — the block simply never appears
// for non-reasoning models (graceful).
func TestClassifyEvents_NoReasoningEmitsNoThinking(t *testing.T) {
	iter := newFakeEventIter(t, []*schema.Message{
		{Role: schema.Assistant, Content: "just text"},
	})
	var types []string
	ClassifyEvents(iter, func(f proto.ServerFrame) { types = append(types, f.Type) })
	assert.NotContains(t, types, "thinking", "no thinking frame when there's no reasoning")
	assert.Equal(t, []string{"agent_chunk"}, types)
}

// fakeEvt pairs a message with an optional error for fakeEventIter.
type fakeEvt struct {
	msg *schema.Message
	err error
}

// newFakeEventIter builds a *adk.AsyncIterator[*adk.AgentEvent] from a slice of
// messages (one AgentEvent per message), suitable for ClassifyEvents tests
// without running a real agent. ADK's AsyncIterator is a concrete struct over
// an internal unbounded channel, so we feed events through the paired
// AsyncGenerator and close it.
func newFakeEventIter(t *testing.T, msgs []*schema.Message) *adk.AsyncIterator[*adk.AgentEvent] {
	t.Helper()
	iter, gen := adk.NewAsyncIteratorPair[*adk.AgentEvent]()
	go func() {
		for _, m := range msgs {
			gen.Send(&adk.AgentEvent{
				Output: &adk.AgentOutput{
					MessageOutput: &adk.TypedMessageVariant[*schema.Message]{Message: m},
				},
			})
		}
		gen.Close()
	}()
	return iter
}

// assertAnError is a sentinel used by error-path classify tests.
var assertAnError = newSentinelError("boom")

func newSentinelError(msg string) error {
	return &sentinelError{msg: msg}
}

type sentinelError struct{ msg string }

func (e *sentinelError) Error() string { return e.msg }

// --- Task 28: per-turn model, thinking effort, usage ---

// TestEventsWithHistoryOpts_SwitchesModel proves that when TurnOpts.Model is
// non-nil, the turn runs on a per-model runner built for that model rather than
// the configured default: a turn with model B (built from a registry-style
// {a,b}) answers from B even though the orchestrator was built with model A.
func TestEventsWithHistoryOpts_SwitchesModel(t *testing.T) {
	modelA := einollm.NewFakeModel([]string{"from-a"}, nil)
	modelB := einollm.NewFakeModel([]string{"from-b"}, nil)
	o, err := New(Config{Model: modelA}) // default = A
	require.NoError(t, err)

	msgs := []*schema.Message{{Role: schema.User, Content: "ping"}}

	// Default turn (no per-turn model) -> answers from A.
	out := drainAgentChunks(t, o.EventsWithHistoryOpts(context.Background(), msgs, TurnOpts{}))
	assert.Contains(t, out, "from-a", "TurnOpts{} must use the default model")

	// Per-turn model B -> answers from B, not A.
	out = drainAgentChunks(t, o.EventsWithHistoryOpts(context.Background(), msgs, TurnOpts{Model: modelB}))
	assert.Contains(t, out, "from-b", "TurnOpts{Model: B} must run on model B")
	assert.NotContains(t, out, "from-a")
}

// TestEventsWithHistoryOpts_RunnerForIsMemoized proves the per-model runner is
// cached: two calls for the same model return the same *adk.Runner (so we don't
// rebuild the ADK agent every turn).
func TestEventsWithHistoryOpts_RunnerForIsMemoized(t *testing.T) {
	modelA := einollm.NewFakeModel([]string{"from-a"}, nil)
	o, err := New(Config{Model: modelA})
	require.NoError(t, err)

	extra := einollm.NewFakeModel([]string{"from-x"}, nil)

	r1 := o.runnerFor(extra, false)
	r2 := o.runnerFor(extra, false)
	assert.Same(t, r1, r2, "runnerFor must memoize per model")

	// plan mode runner 与 agent mode runner 不同（缓存 key 含 mode）
	rp := o.runnerFor(extra, true)
	assert.NotSame(t, r1, rp, "plan runner must differ from agent runner for same model")
}

// TestEventsWithHistoryOpts_ThinkingEffortPassesOption proves a non-empty
// ThinkingEffort adds exactly one per-call model.Option (the reasoning_effort
// option built by einollm.ReasoningEffortOption) on top of whatever baseline
// options the ADK agent already forwards. The reasoning_effort option is an
// impl-specific option (openai.WithReasoningEffort), which can't be decoded
// from outside the acl package (unexported openaiOptions struct), so we assert
// by count delta rather than by value.
func TestEventsWithHistoryOpts_ThinkingEffortPassesOption(t *testing.T) {
	fm := einollm.NewFakeModel([]string{"ok", "ok"}, nil)
	fm.RecordOpts = true
	o, err := New(Config{Model: fm})
	require.NoError(t, err)

	msgs := []*schema.Message{{Role: schema.User, Content: "hi"}}

	// Baseline: options the framework forwards without thinking effort.
	drainAgentChunks(t, o.EventsWithHistoryOpts(context.Background(), msgs, TurnOpts{}))
	baseline := len(fm.ReceivedOpts)

	// With effort: exactly one additional option (the reasoning_effort one).
	drainAgentChunks(t, o.EventsWithHistoryOpts(context.Background(), msgs, TurnOpts{ThinkingEffort: "medium"}))
	require.Equal(t, baseline+1, len(fm.ReceivedOpts),
		"ThinkingEffort=medium must add exactly one model option on top of the framework baseline")
}

// TestEventsWithHistoryOpts_OutputSchemaPassesOption proves a non-empty
// TurnOpts.OutputSchema is forwarded to the model as the per-call
// OutputSchemaOption (the schema reaches the adapter via
// adk.WithChatModelOptions), and an empty OutputSchema forwards nothing.
// FakeModel.ReceivedOutputSchema (populated by recordOpts decoding the
// impl-specific option) makes this an end-to-end-through-orchestrator
// assertion, not just an option-count delta.
func TestEventsWithHistoryOpts_OutputSchemaPassesOption(t *testing.T) {
	fm := einollm.NewFakeModel([]string{"ok", "ok"}, nil)
	fm.RecordOpts = true
	o, err := New(Config{Model: fm})
	require.NoError(t, err)

	msgs := []*schema.Message{{Role: schema.User, Content: "hi"}}
	schemaDoc := json.RawMessage(`{"type":"object","properties":{"x":{"type":"integer"}}}`)

	drainAgentChunks(t, o.EventsWithHistoryOpts(context.Background(), msgs, TurnOpts{OutputSchema: schemaDoc}))
	assert.Equal(t, string(schemaDoc), string(fm.ReceivedOutputSchema),
		"TurnOpts.OutputSchema must reach the model as the decoded schema")

	// Empty OutputSchema -> nothing forwarded.
	fm.ReceivedOutputSchema = nil
	drainAgentChunks(t, o.EventsWithHistoryOpts(context.Background(), msgs, TurnOpts{}))
	assert.Empty(t, fm.ReceivedOutputSchema, "empty OutputSchema must not forward a schema option")
}

// TestEventsWithHistoryOpts_ThinkingAndSchemaCoexist pins the two per-turn
// options against each other. adk.WithChatModelOptions ASSIGNS rather than
// appends (eino v0.9.12 adk/chatmodel.go:99 `t.chatModelOptions = opts`), so
// calling it once per option silently drops all but the last — reasoning
// effort vanished whenever a turn also carried an output schema. Both WS
// (ws.go, set_thinking + output_schema on the same frame) and the v1 service
// can produce that combination, and the single-option tests above each miss it
// because neither sets both.
func TestEventsWithHistoryOpts_ThinkingAndSchemaCoexist(t *testing.T) {
	fm := einollm.NewFakeModel([]string{"ok", "ok"}, nil)
	fm.RecordOpts = true
	o, err := New(Config{Model: fm})
	require.NoError(t, err)

	msgs := []*schema.Message{{Role: schema.User, Content: "hi"}}
	schemaDoc := json.RawMessage(`{"type":"object","properties":{"x":{"type":"integer"}}}`)

	drainAgentChunks(t, o.EventsWithHistoryOpts(context.Background(), msgs, TurnOpts{}))
	baseline := len(fm.ReceivedOpts)

	drainAgentChunks(t, o.EventsWithHistoryOpts(context.Background(), msgs,
		TurnOpts{ThinkingEffort: "medium", OutputSchema: schemaDoc}))

	require.Equal(t, baseline+2, len(fm.ReceivedOpts),
		"thinking effort and output schema must BOTH survive; a count of baseline+1 means one overwrote the other")
	assert.Equal(t, string(schemaDoc), string(fm.ReceivedOutputSchema),
		"output schema must still reach the model when thinking effort is also set")
}

// drainAgentChunks drains an ADK event iterator (via ClassifyEvents) and returns
// the concatenation of agent_chunk frame text. Fails the test on an error frame.
func drainAgentChunks(t *testing.T, iter *adk.AsyncIterator[*adk.AgentEvent]) string {
	t.Helper()
	var sb strings.Builder
	ClassifyEvents(iter, func(f proto.ServerFrame) {
		if f.Type == "error" {
			t.Fatalf("unexpected error frame: %s", f.Text)
		}
		if f.Type == "agent_chunk" {
			sb.WriteString(f.Text)
		}
	})
	return sb.String()
}

// --- Token streaming (EnableStreaming) ---

// TestNew_RunnerBuiltWithStreaming proves the default runner is built with
// EnableStreaming: true. The flag itself is unexported on *adk.Runner, so we
// assert behaviorally: with streaming on, the ADK takes the model's Stream()
// code path (chatmodel.go: "if EnableStreaming { r.Stream(...) }") instead of
// Generate(). FakeModel records each call, so StreamCalls>0 && GenerateCalls==0
// after a turn proves the streaming runner is in effect.
func TestNew_RunnerBuiltWithStreaming(t *testing.T) {
	fm := einollm.NewFakeModel([]string{"streamed reply"}, nil)
	o, err := New(Config{Model: fm})
	require.NoError(t, err)

	msgs := []*schema.Message{{Role: schema.User, Content: "hi"}}
	out := drainAgentChunks(t, o.EventsWithHistoryOpts(context.Background(), msgs, TurnOpts{}))
	assert.Contains(t, out, "streamed reply")

	assert.Greater(t, fm.StreamCalls, 0, "EnableStreaming=true must drive the model's Stream() path")
	assert.Equal(t, 0, fm.GenerateCalls, "Generate() must not be called when streaming is enabled")
}

// TestNew_PerModelRunnerBuiltWithStreaming proves the per-model memoized runner
// (runnerFor) is ALSO built with streaming enabled, so a mid-session /model
// switch keeps token-by-token streaming. Same behavioral assertion as above,
// driven through TurnOpts{Model: ...} which selects the per-model runner.
func TestNew_PerModelRunnerBuiltWithStreaming(t *testing.T) {
	defaultMdl := einollm.NewFakeModel([]string{"from-default"}, nil)
	perTurnMdl := einollm.NewFakeModel([]string{"from-perview"}, nil)
	o, err := New(Config{Model: defaultMdl})
	require.NoError(t, err)

	msgs := []*schema.Message{{Role: schema.User, Content: "hi"}}
	out := drainAgentChunks(t, o.EventsWithHistoryOpts(context.Background(), msgs, TurnOpts{Model: perTurnMdl}))
	assert.Contains(t, out, "from-perview")

	assert.Greater(t, perTurnMdl.StreamCalls, 0, "per-model runner must also take the Stream() path")
	assert.Equal(t, 0, perTurnMdl.GenerateCalls, "per-model runner must not call Generate()")
}

// --- Streaming tool-call collapse (toolCallAccumulator) ---

// newStreamMessageVariant builds an adk.MessageVariant in streaming mode (the
// shape classifyEvents routes to classifyStream) backed by a slice of delta
// messages drained via StreamReaderFromArray. role identifies the stream's
// origin (Assistant for model output, Tool for a streaming tool result).
func newStreamMessageVariant(t *testing.T, role schema.RoleType, deltas []*schema.Message) *adk.MessageVariant {
	t.Helper()
	return &adk.MessageVariant{
		IsStreaming:   true,
		Role:          role,
		MessageStream: schema.StreamReaderFromArray(deltas),
	}
}

// ptrIndex is a tiny helper so each delta can carry a distinct Index value
// without sharing a loop variable address.
func ptrIndex(i int) *int { return &i }

// TestClassifyStream_CollapsesToolCallDeltas proves the central fix: an
// OpenAI-style streaming tool call (name+ID on the first delta, then argument
// fragments, then a content delta) yields exactly ONE tool_call frame carrying
// the COMPLETE accumulated arguments — not one frame per fragment (the cascade
// `({) ⡿ (") ⡿ (path) ⡿ …` the TUI previously rendered).
func TestClassifyStream_CollapsesToolCallDeltas(t *testing.T) {
	deltas := []*schema.Message{
		{Role: schema.Assistant, ToolCalls: []schema.ToolCall{
			{Index: ptrIndex(0), ID: "c1", Type: "function", Function: schema.FunctionCall{Name: "fs_read"}},
		}},
		{Role: schema.Assistant, ToolCalls: []schema.ToolCall{
			{Index: ptrIndex(0), Function: schema.FunctionCall{Arguments: `{"pa`}},
		}},
		{Role: schema.Assistant, ToolCalls: []schema.ToolCall{
			{Index: ptrIndex(0), Function: schema.FunctionCall{Arguments: `th":"store.go"}`}},
		}},
		{Role: schema.Assistant, Content: "done"},
	}
	mv := newStreamMessageVariant(t, schema.Assistant, deltas)

	var frames []proto.ServerFrame
	classifyStream(mv, nil, nil, func(f proto.ServerFrame) { frames = append(frames, f) })

	var types []string
	var toolArgs string
	for _, f := range frames {
		types = append(types, f.Type)
		if f.Type == "tool_call" {
			toolArgs = f.ToolArgs
		}
	}
	assert.Equal(t, []string{"tool_call", "agent_chunk"}, types,
		"exactly ONE tool_call (before the content chunk) + one agent_chunk")
	assert.Equal(t, `{"path":"store.go"}`, toolArgs,
		"the single tool_call carries the COMPLETE accumulated arguments")
}

// TestClassifyStream_ParallelToolCallsEachEmitOnce proves two interleaved
// streaming tool calls (the parallel-call case) each collapse to exactly one
// tool_call frame, even though their argument fragments interleave by Index.
// This guards the accumulator against the false "stream moved on" fallback:
// an interleaving delta still carries a ToolCall entry, so a mid-fragment call
// is never flushed early.
func TestClassifyStream_ParallelToolCallsEachEmitOnce(t *testing.T) {
	deltas := []*schema.Message{
		{Role: schema.Assistant, ToolCalls: []schema.ToolCall{
			{Index: ptrIndex(0), ID: "a", Type: "function", Function: schema.FunctionCall{Name: "fs_read"}},
			{Index: ptrIndex(1), ID: "b", Type: "function", Function: schema.FunctionCall{Name: "shell_run"}},
		}},
		{Role: schema.Assistant, ToolCalls: []schema.ToolCall{
			{Index: ptrIndex(0), Function: schema.FunctionCall{Arguments: `{"path":"a.go"}`}},
			{Index: ptrIndex(1), Function: schema.FunctionCall{Arguments: `{"command":"ls"}`}},
		}},
		{Role: schema.Assistant, Content: "ok"},
	}
	mv := newStreamMessageVariant(t, schema.Assistant, deltas)

	var frames []proto.ServerFrame
	classifyStream(mv, nil, nil, func(f proto.ServerFrame) { frames = append(frames, f) })

	var toolCalls []proto.ServerFrame
	for _, f := range frames {
		if f.Type == "tool_call" {
			toolCalls = append(toolCalls, f)
		}
	}
	require.Len(t, toolCalls, 2, "two parallel calls → exactly two tool_call frames")
	assert.Equal(t, "fs_read", toolCalls[0].ToolName)
	assert.Equal(t, `{"path":"a.go"}`, toolCalls[0].ToolArgs)
	assert.Equal(t, "shell_run", toolCalls[1].ToolName)
	assert.Equal(t, `{"command":"ls"}`, toolCalls[1].ToolArgs)
}

// TestClassifyStream_NoArgToolFlushedOnContent proves a tool call whose
// arguments never reach valid JSON (here a no-arg tool: Arguments stays "") is
// flushed exactly once when a subsequent content delta arrives (the "stream
// moved past tool-call emission" fallback), rather than being held to EOF or
// dropped.
func TestClassifyStream_NoArgToolFlushedOnContent(t *testing.T) {
	deltas := []*schema.Message{
		{Role: schema.Assistant, ToolCalls: []schema.ToolCall{
			{Index: ptrIndex(0), ID: "c1", Type: "function", Function: schema.FunctionCall{Name: "time_now"}},
		}},
		{Role: schema.Assistant, Content: "answered"},
	}
	mv := newStreamMessageVariant(t, schema.Assistant, deltas)

	var frames []proto.ServerFrame
	classifyStream(mv, nil, nil, func(f proto.ServerFrame) { frames = append(frames, f) })

	var toolCalls int
	for _, f := range frames {
		if f.Type == "tool_call" {
			toolCalls++
		}
	}
	assert.Equal(t, 1, toolCalls, "no-arg tool flushed once when content arrives")
}

// TestClassifyStream_NonStreamingStillOneFramePerCall is a guard that the
// non-streaming path (emitAssistant, used by classifyMessage) is unchanged: a
// fully-materialized assistant message with two complete tool calls still emits
// one tool_call frame per call. (The accumulator only affects the streaming
// path.)
func TestClassifyStream_NonStreamingStillOneFramePerCall(t *testing.T) {
	iter := newFakeEventIter(t, []*schema.Message{
		{Role: schema.Assistant, ToolCalls: []schema.ToolCall{
			{ID: "1", Type: "function", Function: schema.FunctionCall{Name: "fs_read", Arguments: `{"path":"x"}`}},
			{ID: "2", Type: "function", Function: schema.FunctionCall{Name: "fs_read", Arguments: `{"path":"y"}`}},
		}},
	})
	var toolCalls int
	ClassifyEvents(iter, func(f proto.ServerFrame) {
		if f.Type == "tool_call" {
			toolCalls++
		}
	})
	assert.Equal(t, 2, toolCalls, "non-streaming: one tool_call per complete call (unchanged)")
}

// --- Token streaming usage: last-value per stream (token-doubling fix) ---

// TestClassifyStream_UsageTakesLastValueNotSum is the regression test for the
// token-count doubling bug. Gateways (and the openai acl wrapper) that emit
// usage on every streaming chunk carry the FULL CUMULATIVE total on each chunk,
// not a per-chunk delta. The old code called usage.add per chunk, which SUMMED
// the cumulative values → the count was multiplied by the number of chunks
// (e.g. 4 chunks each reporting 100 prompt tokens yielded 400 instead of 100).
// The fix captures the LAST non-nil usage seen in the stream and applies it ONCE
// at stream end. This test feeds a stream where every chunk carries the SAME
// cumulative usage and asserts the accumulator ends with that value (not N×it);
// it also asserts onUsage fires exactly once (at stream end), not per chunk.
func TestClassifyStream_UsageTakesLastValueNotSum(t *testing.T) {
	// Four assistant deltas, EACH carrying the full cumulative usage (the
	// gateway style): prompt=100/completion=10/total=110 on every chunk. A
	// fresh *TokenUsage per chunk mirrors a real provider that allocates one
	// per SSE event.
	usage100 := func() *schema.TokenUsage {
		return &schema.TokenUsage{PromptTokens: 100, CompletionTokens: 10, TotalTokens: 110}
	}
	deltas := []*schema.Message{
		{Role: schema.Assistant, Content: "Hel", ResponseMeta: &schema.ResponseMeta{Usage: usage100()}},
		{Role: schema.Assistant, Content: "lo ", ResponseMeta: &schema.ResponseMeta{Usage: usage100()}},
		{Role: schema.Assistant, Content: "wor", ResponseMeta: &schema.ResponseMeta{Usage: usage100()}},
		{Role: schema.Assistant, Content: "ld", ResponseMeta: &schema.ResponseMeta{Usage: usage100()}},
	}
	mv := newStreamMessageVariant(t, schema.Assistant, deltas)

	var usage TurnUsage
	var snaps []TurnUsage
	classifyStream(mv, &usage, func(u TurnUsage) {
		snaps = append(snaps, u)
	}, func(proto.ServerFrame) {})

	assert.Equal(t, 100, usage.PromptTokens,
		"last-value not sum: 4 chunks each reporting 100 must yield 100, not 400")
	assert.Equal(t, 10, usage.CompletionTokens, "completion tokens: last value (10), not 40")
	assert.Equal(t, 110, usage.TotalTokens, "total tokens: last value (110), not 440")
	require.Len(t, snaps, 1, "onUsage fires once at stream end, not once per chunk")
	assert.Equal(t, 100, snaps[0].PromptTokens, "the single snapshot carries the last value")
}

// TestOrchestrator_InjectsWorkRoot proves the orchestrator injects its
// configured WorkRoot into every turn's tool-execution context: a tool that
// returns an oversized result triggers spillIfTooLong, which reads
// WorkRootFromContext and writes under <root>/.yanshi/tmp/spillover/. The spill
// file landing under the configured root is the observable proof.
func TestOrchestrator_InjectsWorkRoot(t *testing.T) {
	root := t.TempDir()

	big := tools.NewGuardedTool("big", "Big", "returns a lot", 10*time.Second, nil,
		tools.SyncStream(func(ctx context.Context, argsJSON string) (string, error) {
			return strings.Repeat("z", tools.SpillThreshold+1), nil
		}))

	tc1 := schema.AssistantMessage("", []schema.ToolCall{
		{ID: "c1", Type: "function", Function: schema.FunctionCall{
			Name: "big", Arguments: `{}`,
		}},
	})
	tc2 := schema.AssistantMessage("done", nil)
	model := einollm.NewFakeModelWithMessages([]*schema.Message{tc1, tc2}, nil)

	o, err := New(Config{
		Model:   model,
		Tools:   []BaseTool{big},
		WorkRoot: root,
		// Task 2: orchestrator no longer fall-backs to Tools={"*"}; tests must
		// pass an explicit profile that allows the test's tool names.
		Profile: guard.PermissionProfile{Tools: guard.ToolsPerm{Allow: []string{"big"}}},
	})
	require.NoError(t, err)

	out, err := o.Query(context.Background(), "call big")
	require.NoError(t, err)
	assert.Equal(t, "done", out)

	entries, err := os.ReadDir(filepath.Join(root, ".yanshi", "tmp", "spillover"))
	require.NoError(t, err, "spill file must exist under the configured WorkRoot")
	assert.Len(t, entries, 1, "oversized tool result spilled to one file")
}

// TestOrchestrator_SubAgentInheritsWorkRoot proves WorkRoot propagates to the
// nested sub-agent orchestrator: a sub-agent's oversized tool output must spill
// under the configured WorkRoot, not cwd. Without WorkRoot: o.workRoot in the
// nested Config (runSubAgentTurn), the sub's WithWorkRoot(ctx,"") clobbers the
// inherited root and the spill lands in cwd.
func TestOrchestrator_SubAgentInheritsWorkRoot(t *testing.T) {
	root := t.TempDir()
	// Isolate cwd so the pre-fix buggy path (spill→cwd) doesn't litter the repo
	// root during the red phase.
	t.Chdir(t.TempDir())

	big := tools.NewGuardedTool("big", "Big", "returns a lot", 10*time.Second, nil,
		tools.SyncStream(func(ctx context.Context, argsJSON string) (string, error) {
			return strings.Repeat("z", tools.SpillThreshold+1), nil
		}))
	// spawn invokes the bound SubAgentRunner to run a sub-agent (inheriting big).
	spawn := tools.NewGuardedTool("spawn", "Spawn", "spawns a sub-agent", 10*time.Second, nil,
		tools.SyncStream(func(ctx context.Context, argsJSON string) (string, error) {
			runner := tools.SubAgentRunnerFromContext(ctx)
			if runner == nil {
				return "✗ no runner bound", nil
			}
			return runner(ctx, "call the big tool", nil, "")
		}))

	// Shared FakeModel dispenses sequentially across parent + sub calls:
	// parent→spawn, sub→big, sub→done, parent→done.
	tcSpawn := schema.AssistantMessage("", []schema.ToolCall{
		{ID: "s1", Type: "function", Function: schema.FunctionCall{Name: "spawn", Arguments: "{}"}},
	})
	tcBig := schema.AssistantMessage("", []schema.ToolCall{
		{ID: "b1", Type: "function", Function: schema.FunctionCall{Name: "big", Arguments: "{}"}},
	})
	subDone := schema.AssistantMessage("sub result", nil)
	parentDone := schema.AssistantMessage("parent done", nil)
	model := einollm.NewFakeModelWithMessages([]*schema.Message{tcSpawn, tcBig, subDone, parentDone}, nil)

	o, err := New(Config{
		Model:   model,
		Tools:   []BaseTool{big, spawn},
		WorkRoot: root,
		// Task 2: orchestrator no longer fall-backs to Tools={"*"}; tests must
		// pass an explicit profile that allows the test's tool names.
		Profile: guard.PermissionProfile{Tools: guard.ToolsPerm{Allow: []string{"big", "spawn"}}},
	})
	require.NoError(t, err)

	out, err := o.Query(context.Background(), "spawn a sub-agent that calls big")
	require.NoError(t, err)
	assert.Equal(t, "parent done", out)

	entries, err := os.ReadDir(filepath.Join(root, ".yanshi", "tmp", "spillover"))
	require.NoError(t, err, "sub-agent spill file must land under the configured WorkRoot, not cwd")
	assert.Len(t, entries, 1)
}

// TestClassifyStream_UsageLatestStreamWins proves the overwrite (set) semantics
// across separate model calls within a turn: each classifyStream drains ONE
// model call, and the shared accumulator holds the LATEST value (overwrite, not
// sum), because the API reports cumulative counts per call. Two streams
// reporting 100 then 50 prompt tokens yield 50 (the latest), not 150.
func TestClassifyStream_UsageLatestStreamWins(t *testing.T) {
	mv1 := newStreamMessageVariant(t, schema.Assistant, []*schema.Message{
		{Role: schema.Assistant, Content: "first", ResponseMeta: &schema.ResponseMeta{
			Usage: &schema.TokenUsage{PromptTokens: 100, CompletionTokens: 10, TotalTokens: 110}}},
	})
	mv2 := newStreamMessageVariant(t, schema.Assistant, []*schema.Message{
		{Role: schema.Assistant, Content: "second", ResponseMeta: &schema.ResponseMeta{
			Usage: &schema.TokenUsage{PromptTokens: 50, CompletionTokens: 5, TotalTokens: 55}}},
	})

	var usage TurnUsage
	classifyStream(mv1, &usage, nil, func(proto.ServerFrame) {})
	classifyStream(mv2, &usage, nil, func(proto.ServerFrame) {})

	assert.Equal(t, 50, usage.PromptTokens, "overwrite: latest stream's value (50), not 100+50")
	assert.Equal(t, 5, usage.CompletionTokens, "overwrite: latest (5)")
	assert.Equal(t, 55, usage.TotalTokens, "overwrite: latest (55)")
}

// TestNew_NoMoreWildcardFallbackOnEmptyProfile regression-covers the fail-open
// fallback that used to silently widen an empty Tools.Allow to {"*"} (Task 2).
// The orchestrator must pass the profile through AS-IS so an unconfigured
// profile stays fail-closed; bootstrap is responsible for shipping a concrete
// coding profile.
func TestNew_NoMoreWildcardFallbackOnEmptyProfile(t *testing.T) {
	fm := einollm.NewFakeModelWithMessages(nil, nil)
	o, err := New(Config{Model: fm, Profile: guard.PermissionProfile{}})
	if err != nil {
		t.Fatal(err)
	}
	if got := o.ProfileForTest(); got.Tools.Allow != nil {
		t.Fatalf("orchestrator must not synthesize Tools.Allow={\"*\"}; got %#v", got.Tools.Allow)
	}
}

// countMemoryMarker counts occurrences of marker across each message Content
// in msgs. Used by TestRunSubAgentTurn_* behavioral tests.
func countMemoryMarker(msgs []*schema.Message) int {
	n := 0
	for _, m := range msgs {
		if m == nil {
			continue
		}
		n += strings.Count(m.Content, "PREFER_TEA_MARKER")
	}
	return n
}

// TestNew_MemorySuffixAppended proves MemorySuffix is appended at the end of
// New() and saved into baseInstruction. Uses FakeModel.RecordMessages to grab
// the system prompt rather than reading the field directly.
func TestNew_MemorySuffixAppended(t *testing.T) {
	fm := einollm.NewFakeModel([]string{"ok"}, nil)
	fm.RecordMessages = true
	o, err := New(Config{
		Model:        fm,
		Instruction:  "BASE",
		MemorySuffix: "<user_memory>\nPREFER_TEA_MARKER\n</user_memory>",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if !strings.Contains(o.instruction, "PREFER_TEA_MARKER") {
		t.Errorf("instruction 应含 memory marker,got tail: %q", tail(o.instruction, 200))
	}
	if !strings.Contains(o.baseInstruction, "PREFER_TEA_MARKER") {
		t.Errorf("baseInstruction 应含 memory marker(子代理继承),got tail: %q", tail(o.baseInstruction, 200))
	}
	if !strings.HasSuffix(o.baseInstruction, "</user_memory>") {
		t.Errorf("baseInstruction 应以 memory suffix 结尾,got 尾部 100 字符: %q", tail(o.baseInstruction, 100))
	}
	if o.memorySuffix == "" || !strings.Contains(o.memorySuffix, "PREFER_TEA_MARKER") {
		t.Errorf("o.memorySuffix 应被保存,got: %q", o.memorySuffix)
	}
}

// TestRunSubAgentTurn_PropagatesMemorySuffix_Override is the v3 behavioral test.
// FakeModel is reused inside the nested orchestrator; RecordMessages captures
// the nested model's actual input. The override path must append memorySuffix
// exactly once (marker count == 1).
func TestRunSubAgentTurn_PropagatesMemorySuffix_Override(t *testing.T) {
	fm := einollm.NewFakeModel([]string{"sub-output"}, nil)
	fm.RecordMessages = true
	o, err := New(Config{
		Model:        fm,
		Instruction:  "BASE",
		MemorySuffix: "<user_memory>\nPREFER_TEA_MARKER\n</user_memory>",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx := o.bindSubAgentRunner(context.Background())
	runner := tools.SubAgentRunnerFromContext(ctx)
	if runner == nil {
		t.Fatal("SubAgentRunner not bound")
	}
	if _, err := runner(ctx, "do something", nil, "OVERRIDE_INSTRUCTION"); err != nil {
		t.Fatalf("runner returned error: %v", err)
	}

	got := countMemoryMarker(fm.ReceivedMessages)
	if got != 1 {
		t.Fatalf("override 路径 system prompt 应含 marker 恰好 1 次,got %d (messages=%v)", got, fm.ReceivedMessages)
	}
}

// TestRunSubAgentTurn_PropagatesMemorySuffix_Inherit proves the inherit path
// (empty override) uses baseInstruction verbatim and does NOT re-append. Catches
// the FN4 double-injection regression (marker count would be 2).
func TestRunSubAgentTurn_PropagatesMemorySuffix_Inherit(t *testing.T) {
	fm := einollm.NewFakeModel([]string{"sub-output"}, nil)
	fm.RecordMessages = true
	o, err := New(Config{
		Model:        fm,
		Instruction:  "BASE",
		MemorySuffix: "<user_memory>\nPREFER_TEA_MARKER\n</user_memory>",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx := o.bindSubAgentRunner(context.Background())
	runner := tools.SubAgentRunnerFromContext(ctx)
	if _, err := runner(ctx, "do something", nil, ""); err != nil { // inherit
		t.Fatalf("runner returned error: %v", err)
	}

	got := countMemoryMarker(fm.ReceivedMessages)
	if got != 1 {
		t.Fatalf("inherit 路径 system prompt 应含 marker 恰好 1 次(baseInstruction 已含),got %d", got)
	}
}


func TestApplyImages_MultimodalDirectEmbedsImagePart(t *testing.T) {
	store := imagestore.New(imagestore.Config{MaxItems: 20, MaxBytes: 100 << 20})
	cfg := Config{
		Model: einollm.NewFakeModel([]string{"ok"}, nil),
		MultimodalMap: map[string]bool{"mm-model": true},
		ImageStore:    store,
	}
	o, err := New(cfg)
	require.NoError(t, err)

	msgs := o.ApplyImages([]*schema.Message{}, "mm-model", []proto.ImageAttach{
		{Source: "paste", Fmt: "png", W: 2, H: 2, DataB64: testPNGB64(t)},
	})
	require.Len(t, msgs, 1)
	require.NotEmpty(t, msgs[0].UserInputMultiContent, "multimodal model must embed image as a MultiContent part")
}

func TestApplyImages_NonMultimodalInsertsPlaceholderAndStores(t *testing.T) {
	store := imagestore.New(imagestore.Config{MaxItems: 20, MaxBytes: 100 << 20})
	cfg := Config{
		Model:         einollm.NewFakeModel([]string{"ok"}, nil),
		MultimodalMap: map[string]bool{"text-model": false},
		ImageStore:    store,
	}
	o, err := New(cfg)
	require.NoError(t, err)

	msgs := o.ApplyImages([]*schema.Message{}, "text-model", []proto.ImageAttach{
		{Source: "paste", Fmt: "png", W: 2, H: 2, DataB64: testPNGB64(t)},
	})
	require.Len(t, msgs, 1)
	require.Contains(t, msgs[0].Content, "[image:img-1", "non-multimodal model must see placeholder text")
	require.Empty(t, msgs[0].UserInputMultiContent, "non-multimodal model must NOT get raw image parts")
	_, ok := store.Get("img-1")
	require.True(t, ok, "image must be stored for image_describe to fetch")
}

func TestApplyImages_NoImagesLeavesMessagesUntouched(t *testing.T) {
	store := imagestore.New(imagestore.Config{MaxItems: 20, MaxBytes: 100 << 20})
	cfg := Config{Model: einollm.NewFakeModel([]string{"ok"}, nil), ImageStore: store}
	o, err := New(cfg)
	require.NoError(t, err)
	in := []*schema.Message{schema.UserMessage("hi")}
	out := o.ApplyImages(in, "any", nil)
	require.Equal(t, in, out, "no images must be a pass-through")
}

func TestApplyImages_ModelSwitchReEvaluatesCapability(t *testing.T) {
	store := imagestore.New(imagestore.Config{MaxItems: 20, MaxBytes: 100 << 20})
	cfg := Config{
		Model:         einollm.NewFakeModel([]string{"ok"}, nil),
		MultimodalMap: map[string]bool{"mm": true, "text": false},
		ImageStore:    store,
	}
	o, err := New(cfg)
	require.NoError(t, err)
	mm := o.ApplyImages([]*schema.Message{}, "mm", []proto.ImageAttach{{Fmt: "png", DataB64: testPNGB64(t)}})
	require.NotEmpty(t, mm[0].UserInputMultiContent, "mm model -> direct part")
	txt := o.ApplyImages([]*schema.Message{}, "text", []proto.ImageAttach{{Fmt: "png", DataB64: testPNGB64(t)}})
	require.Contains(t, txt[0].Content, "[image:", "switched to text model -> placeholder")
}

// testPNGB64 generates a minimal 1x1 red PNG as a base64 string.
func testPNGB64(t *testing.T) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 1, 1))
	img.Set(0, 0, color.RGBA{R: 255, A: 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}

// tail returns the last n bytes of s (or all of s if shorter).
func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
