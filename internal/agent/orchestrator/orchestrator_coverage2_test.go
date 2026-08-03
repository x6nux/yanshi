package orchestrator

import (
	"context"
	"errors"
	"testing"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/agent/registry"
	einollm "github.com/x6nux/yanshi/internal/llm/eino"
	obslog "github.com/x6nux/yanshi/internal/observe/log"
	"github.com/x6nux/yanshi/internal/proto"
	"github.com/x6nux/yanshi/internal/task/work"
	"github.com/x6nux/yanshi/internal/tools"
)

// TestNew_NilModelError proves that New returns an error when Model is nil.
func TestNew_NilModelError(t *testing.T) {
	_, err := New(Config{Model: nil})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Model is required")
}

// TestNew_EmptyInstructionDefaults proves empty Instruction becomes DefaultInstruction.
func TestNew_EmptyInstructionDefaults(t *testing.T) {
	fm := einollm.NewFakeModel([]string{"ok"}, nil)
	o, err := New(Config{Model: fm})
	require.NoError(t, err)
	assert.Contains(t, o.baseInstruction, "yanshi")
}

// badInfoTool is a BaseTool whose Info() always returns an error.
type badInfoTool struct{}

func (badInfoTool) InvokableRun(ctx context.Context, input string, opts ...tool.Option) (string, error) {
	return "", nil
}
func (badInfoTool) Info(ctx context.Context) (*schema.ToolInfo, error) {
	return nil, errors.New("info unavailable")
}

// TestCollectToolNames_InfoError proves tools whose Info() errors are silently skipped.
func TestCollectToolNames_InfoError(t *testing.T) {
	bogus := &badInfoTool{}
	names := collectToolNames([]tool.BaseTool{bogus})
	assert.Empty(t, names)
}

// TestEnsureTurnIDs_PreservesExistingTags proves existing TraceID/TurnID are preserved.
func TestEnsureTurnIDs_PreservesExistingTags(t *testing.T) {
	ctx := obslog.WithIDs(context.Background(), obslog.IDs{
		TraceID: "trace-a",
		TurnID:  "turn-b",
	})
	got := ensureTurnIDs(ctx)
	ids := obslog.IDsFromContext(got)
	assert.Equal(t, "trace-a", ids.TraceID)
	assert.Equal(t, "turn-b", ids.TurnID)
}

// TestEnsureTurnIDs_FillsEmptyTags proves missing TraceID/TurnID are filled.
func TestEnsureTurnIDs_FillsEmptyTags(t *testing.T) {
	ctx := ensureTurnIDs(context.Background())
	ids := obslog.IDsFromContext(ctx)
	assert.NotEmpty(t, ids.TraceID)
	assert.NotEmpty(t, ids.TurnID)
}

// TestFlushRunners_ClearsCache proves FlushRunners removes all cached runners.
func TestFlushRunners_ClearsCache(t *testing.T) {
	fm := einollm.NewFakeModel([]string{"ok"}, nil)
	o, err := New(Config{Model: fm})
	require.NoError(t, err)

	r1 := o.runnerFor(fm, false)
	require.NotNil(t, r1)

	o.FlushRunners()

	r2 := o.runnerFor(fm, false)
	require.NotNil(t, r2)
	assert.NotSame(t, r1, r2, "FlushRunners cleared the cache")
}

// TestRunnerFor_BuildErrorReturnsNil proves that when adk.NewChatModelAgent fails, runnerFor returns nil.
func TestRunnerFor_BuildErrorReturnsNil(t *testing.T) {
	o := &Orchestrator{agentTools: nil, maxIters: 1}
	got := o.runnerFor(nil, false)
	assert.Nil(t, got)
}

// TestFilterPlanTools_EmptyList handles empty list.
func TestFilterPlanTools_EmptyList(t *testing.T) {
	got := filterPlanTools(nil)
	assert.Empty(t, got)
}

// TestFilterPlanTools_NoPlanSafeTool tests plan-safe filtering:
// fs_read is plan-safe, memory_write is not plan-safe.
func TestFilterPlanTools_NoPlanSafeTool(t *testing.T) {
	fs := tools.NewFSTools(t.TempDir())
	got := filterPlanTools([]tool.BaseTool{fs.Read})
	require.Len(t, got, 1, "fs_read is plan-safe")

	// A non-plan-safe tool should be filtered out.
	memTools := tools.NewMemoryTools(nil)
	got = filterPlanTools([]tool.BaseTool{memTools.Write})
	require.Len(t, got, 0, "memory_write is not plan-safe")
}

// TestFilterPlanTools_InfoErrorToolIsDropped tests info-error tools are dropped.
func TestFilterPlanTools_InfoErrorToolIsDropped(t *testing.T) {
	got := filterPlanTools([]tool.BaseTool{&badInfoTool{}})
	assert.Empty(t, got)
}

// TestSelectSubAgentTools_EmptyAllowedReturnsAll tests nil/empty allowed returns full set.
func TestSelectSubAgentTools_EmptyAllowedReturnsAll(t *testing.T) {
	fm := einollm.NewFakeModel(nil, nil)
	fs := tools.NewFSTools(t.TempDir())
	o, err := New(Config{Model: fm, Tools: []BaseTool{fs.Read, fs.Write}})
	require.NoError(t, err)

	got := o.selectSubAgentTools(nil)
	require.Len(t, got, 2)
	got = o.selectSubAgentTools([]string{})
	require.Len(t, got, 2)
}

// TestSelectSubAgentTools_NoMatch tests no matching tools returns empty slice.
func TestSelectSubAgentTools_NoMatch(t *testing.T) {
	fm := einollm.NewFakeModel(nil, nil)
	timeTools := tools.NewTimeTools()
	o, err := New(Config{Model: fm, Tools: []BaseTool{timeTools.Now}})
	require.NoError(t, err)

	got := o.selectSubAgentTools([]string{"bogus_tool"})
	assert.Empty(t, got)
}

// TestWithoutOrchestrationTools_DropsByKnownNames tests orchestration tools are removed.
func TestWithoutOrchestrationTools_DropsByKnownNames(t *testing.T) {
	fm := einollm.NewFakeModel(nil, nil)
	fs := tools.NewFSTools(t.TempDir())
	o, err := New(Config{Model: fm, Tools: []BaseTool{fs.Read}})
	require.NoError(t, err)

	got := withoutOrchestrationTools(o.selectSubAgentTools(nil))
	require.Len(t, got, 1)
	info, _ := got[0].Info(context.Background())
	assert.Equal(t, "fs_read", info.Name)
}

// TestWithoutOrchestrationTools_InfoError tests info-error tools are skipped.
func TestWithoutOrchestrationTools_InfoError(t *testing.T) {
	got := withoutOrchestrationTools([]BaseTool{&badInfoTool{}})
	assert.Empty(t, got)
}

// TestContains tests the contains helper function.
func TestContains(t *testing.T) {
	assert.True(t, contains([]string{"a", "b", "c"}, "b"))
	assert.False(t, contains([]string{"a", "b", "c"}, "z"))
	assert.False(t, contains(nil, "x"))
}

// TestMemorySuffix_Accessor tests MemorySuffix() accessor.
func TestMemorySuffix_Accessor(t *testing.T) {
	o := &Orchestrator{memorySuffix: "remember: tea"}
	assert.Equal(t, "remember: tea", o.MemorySuffix())
}

// TestMemorySuffix_Empty tests empty MemorySuffix.
func TestMemorySuffix_Empty(t *testing.T) {
	o := &Orchestrator{}
	assert.Empty(t, o.MemorySuffix())
}

// TestBindSubAgentRunner_NilModelIsNoop proves bindSubAgentRunner is noop when model nil.
func TestBindSubAgentRunner_NilModelIsNoop(t *testing.T) {
	o := &Orchestrator{model: nil}
	ctx := o.bindSubAgentRunner(context.Background())
	runner := tools.SubAgentRunnerFromContext(ctx)
	assert.Nil(t, runner)
}

// TestBindManagedRunner_NilSubagentMgrDoesNotBindManager proves when subagentMgr is nil, no Manager bound.
func TestBindManagedRunner_NilSubagentMgrDoesNotBindManager(t *testing.T) {
	fm := einollm.NewFakeModel([]string{"ok"}, nil)
	o, err := New(Config{Model: fm})
	require.NoError(t, err)
	require.Nil(t, o.subagentMgr)

	ctx := o.bindManagedRunner(context.Background())
	mgr := tools.ManagerFromContext(ctx)
	assert.Nil(t, mgr)
}

// TestManagedTurnRunner_RoleInjection verifies RoleFromContext behavior.
func TestManagedTurnRunner_RoleInjection(t *testing.T) {
	role := registry.RoleFromContext(context.Background())
	assert.Empty(t, role)
}

// TestRoleForSubAgent_UnknownRole tests roleForSubagent with non-existent role.
func TestRoleForSubAgent_UnknownRole(t *testing.T) {
	got := roleForSubagent("nonexistent_role")
	assert.Nil(t, got)
}

// TestEventsWithHistory_Basic tests the basic EventsWithHistory path.
func TestEventsWithHistory_Basic(t *testing.T) {
	fm := einollm.NewFakeModel([]string{"history response"}, nil)
	o, err := New(Config{Model: fm})
	require.NoError(t, err)

	msgs := []*schema.Message{{Role: schema.User, Content: "hi"}}
	iter := o.EventsWithHistory(context.Background(), msgs)
	out := drainAgentChunks(t, iter)
	assert.Equal(t, "history response", out)
}

// TestQuery_IteratorError proves iterator error propagates through Query.
func TestQuery_IteratorError(t *testing.T) {
	fm := einollm.NewFakeModel(nil, errors.New("model failure"))
	o, err := New(Config{Model: fm})
	require.NoError(t, err)

	_, err = o.Query(context.Background(), "hi")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "model failure")
}

// TestQuery_NoAssistantMessage proves empty content returns error.
func TestQuery_NoAssistantMessage(t *testing.T) {
	fm := einollm.NewFakeModel([]string{""}, nil)
	o, err := New(Config{Model: fm})
	require.NoError(t, err)

	_, err = o.Query(context.Background(), "hi")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no assistant message produced")
}

// TestFinalOutputAccumulator_EdgeCases proves observe handles nil and non-assistant.
func TestFinalOutputAccumulator_EdgeCases(t *testing.T) {
	var acc finalOutputAccumulator
	acc.observe(nil)
	acc.observe(&schema.Message{Role: schema.User, Content: "hello"})
	assert.Empty(t, acc.lastContent)
}

// TestWrapCompaction_ZeroThreshold returns original model unwrapped.
func TestWrapCompaction_ZeroThreshold(t *testing.T) {
	fm := einollm.NewFakeModel([]string{"ok"}, nil)
	wrapped := wrapCompaction(fm, CompactionConfig{Threshold: 0})
	assert.Equal(t, fm, wrapped)
}

// TestWrapCompaction_NegativeThreshold returns original model unwrapped.
func TestWrapCompaction_NegativeThreshold(t *testing.T) {
	fm := einollm.NewFakeModel([]string{"ok"}, nil)
	wrapped := wrapCompaction(fm, CompactionConfig{Threshold: -1})
	assert.Equal(t, fm, wrapped)
}

// TestWorkEventFrame_NilTaskSafe handles nil task without panic.
func TestWorkEventFrame_NilTaskSafe(t *testing.T) {
	f := workEventFrame(work.Event{Kind: work.EventTaskUpdate, Task: nil})
	// NewTaskUpdate(nil) returns ServerFrame{} (empty type)
	assert.Empty(t, f.Type)
}

// TestClassifyEvents_NilOutputContinues proves nil Output/MessageOutput are skipped.
func TestClassifyEvents_NilOutputContinues(t *testing.T) {
	iter, gen := adk.NewAsyncIteratorPair[*adk.AgentEvent]()
	gen.Send(&adk.AgentEvent{Output: nil})
	gen.Send(&adk.AgentEvent{Output: &adk.AgentOutput{MessageOutput: nil}})
	gen.Close()

	var frames []proto.ServerFrame
	ClassifyEvents(iter, func(f proto.ServerFrame) { frames = append(frames, f) })
	assert.Empty(t, frames)
}

// TestClassifyEvents_NilMessageInNonStreaming proves nil Message in non-streaming is skipped.
func TestClassifyEvents_NilMessageInNonStreaming(t *testing.T) {
	iter, gen := adk.NewAsyncIteratorPair[*adk.AgentEvent]()
	gen.Send(&adk.AgentEvent{
		Output: &adk.AgentOutput{
			MessageOutput: &adk.MessageVariant{IsStreaming: false, Message: nil},
		},
	})
	gen.Close()

	var frames []proto.ServerFrame
	ClassifyEvents(iter, func(f proto.ServerFrame) { frames = append(frames, f) })
	assert.Empty(t, frames)
}

// TestClassifyStream_NilMsgSkips proves nil delta is skipped.
func TestClassifyStream_NilMsgSkips(t *testing.T) {
	deltas := []*schema.Message{nil, schema.AssistantMessage("hello", nil)}
	mv := newStreamMessageVariant(t, schema.Assistant, deltas)
	var frames []proto.ServerFrame
	classifyStream(mv, nil, nil, func(f proto.ServerFrame) { frames = append(frames, f) })
	require.Len(t, frames, 1)
	assert.Equal(t, "agent_chunk", frames[0].Type)
}

// TestClassifyStream_RecvError emits an error frame.
func TestClassifyStream_RecvError(t *testing.T) {
	sr, sw := schema.Pipe[*schema.Message](1)
	go func() {
		_ = sw.Send(nil, errors.New("stream failed"))
		sw.Close()
	}()
	mv := &adk.MessageVariant{
		IsStreaming: true, Role: schema.Assistant, MessageStream: sr,
	}
	var frames []proto.ServerFrame
	classifyStream(mv, nil, nil, func(f proto.ServerFrame) { frames = append(frames, f) })
	require.Len(t, frames, 1)
	assert.Equal(t, "error", frames[0].Type)
	assert.Contains(t, frames[0].Text, "stream failed")
}

// TestClassifyStream_ToolRoleEmitsToolResult proves Tool-role stream emits tool_result.
func TestClassifyStream_ToolRoleEmitsToolResult(t *testing.T) {
	deltas := []*schema.Message{
		{Role: schema.Tool, ToolCallID: "c1", ToolName: "fs_read", Content: "content"},
	}
	mv := newStreamMessageVariant(t, schema.Tool, deltas)
	var frames []proto.ServerFrame
	classifyStream(mv, nil, nil, func(f proto.ServerFrame) { frames = append(frames, f) })
	require.Len(t, frames, 1)
	assert.Equal(t, "tool_result", frames[0].Type)
	assert.Equal(t, "ok", frames[0].Status)
}

// TestToolCallAccumulator_NoIndex tests tool call without Index field.
func TestToolCallAccumulator_NoIndex(t *testing.T) {
	deltas := []*schema.Message{
		{Role: schema.Assistant, ToolCalls: []schema.ToolCall{
			{ID: "c1", Function: schema.FunctionCall{Name: "fs_read", Arguments: `{"path":"x"}`}},
		}},
	}
	mv := newStreamMessageVariant(t, schema.Assistant, deltas)
	var frames []proto.ServerFrame
	classifyStream(mv, nil, nil, func(f proto.ServerFrame) { frames = append(frames, f) })
	toolCalls := 0
	for _, f := range frames {
		if f.Type == "tool_call" {
			toolCalls++
		}
	}
	assert.Equal(t, 1, toolCalls)
}

// TestEmitAssistantContent_BothReasoningAndContent proves thinking precedes agent_chunk.
func TestEmitAssistantContent_BothReasoningAndContent(t *testing.T) {
	var frames []proto.ServerFrame
	emitAssistantContent(&schema.Message{
		ReasoningContent: "thinking", Content: "answer",
	}, func(f proto.ServerFrame) { frames = append(frames, f) })
	require.Len(t, frames, 2)
	assert.Equal(t, "thinking", frames[0].Type)
	assert.Equal(t, "agent_chunk", frames[1].Type)
}

// TestEmitToolResult_EmptyContentMarksError tests empty tool result is error.
func TestEmitToolResult_EmptyContentMarksError(t *testing.T) {
	var frames []proto.ServerFrame
	emitToolResult(&schema.Message{
		ToolCallID: "c1", ToolName: "t", Content: "",
	}, func(f proto.ServerFrame) { frames = append(frames, f) })
	require.Len(t, frames, 1)
	assert.Equal(t, "error", frames[0].Status)
}

// TestJudgeCompletion_NilRecorderAndGenerateError proves fail-safe paths.
func TestJudgeCompletion_NilRecorderAndGenerateError(t *testing.T) {
	fm := einollm.NewFakeModel(nil, errors.New("judge error"))
	o, err := New(Config{Model: fm})
	require.NoError(t, err)

	complete, reason, _ := o.JudgeCompletion(context.Background())
	assert.True(t, complete)
	assert.Empty(t, reason)

	rec := &turnRecorder{}
	rec.store([]*schema.Message{schema.UserMessage("hi")})
	ctx := WithTurnRecorder(context.Background(), rec)
	complete, reason, _ = o.JudgeCompletion(ctx)
	assert.True(t, complete)
	assert.Empty(t, reason)
}

// TestParseJudgeVerdict_OctothorpeOnly proves parseJudgVerdict fails on non-JSON without braces.
func TestParseJudgeVerdict_OctothorpeOnly(t *testing.T) {
	_, err := parseJudgeVerdict("###")
	require.Error(t, err)
}

// TestAfterModelRewriteState_NilState proves nil state is handled gracefully.
func TestAfterModelRewriteState_NilState(t *testing.T) {
	r := newMessageRecorder()
	_, state, err := r.AfterModelRewriteState(context.Background(), nil, nil)
	require.NoError(t, err)
	assert.Nil(t, state)
}

// TestAppendImageParts_NoUserMessage proves new user msg created for empty history.
func TestAppendImageParts_NoUserMessage(t *testing.T) {
	result := appendImageParts(nil, []proto.ImageAttach{
		{Fmt: "png", DataB64: "AAAA"},
	})
	require.Len(t, result, 1)
	require.NotEmpty(t, result[0].UserInputMultiContent)
}

// TestAppendPlaceholders_NilStore proves nil store returns history unchanged.
func TestAppendPlaceholders_NilStore(t *testing.T) {
	o := &Orchestrator{imageStore: nil}
	history := []*schema.Message{schema.UserMessage("hi")}
	result := o.appendPlaceholders(history, []proto.ImageAttach{
		{Source: "test", Fmt: "png", DataB64: "AAAA"},
	})
	assert.Equal(t, history, result)
}

// TestWorkEventFrame_DefaultNilTask tests default case with nil task.
func TestWorkEventFrame_DefaultNilTask(t *testing.T) {
	f := workEventFrame(work.Event{Kind: "unknown", Task: nil})
	// NewTaskUpdate(nil) returns ServerFrame{}, so Type is empty
	assert.Empty(t, f.Type)
}

// TestFinalOutputAccumulator_FinalizeEmpty tests finalize error on empty accumulator.
func TestFinalOutputAccumulator_FinalizeEmpty(t *testing.T) {
	var acc finalOutputAccumulator
	_, err := acc.finalize()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no assistant message produced")
}
