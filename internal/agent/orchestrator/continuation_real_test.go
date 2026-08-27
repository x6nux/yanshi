package orchestrator

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/guard"
	einollm "github.com/x6nux/yanshi/internal/llm/eino"
)

// This file verifies L5 -- "continue a prematurely stopped turn without
// replaying what already ran" -- against REAL side effects.
//
// The continuation tests elsewhere assert on message slices: that the recorded
// history is reused, that a nudge is appended, that no tool_call is left
// outstanding. All of that can be true while the second attempt still runs the
// tool again, because a message slice records what was SAID, not what HAPPENED.
//
// The distinction is the entire feature. The caveat it was built to remove is
// that a re-run turn executes shell_run and fs_write a second time, and the
// only observation that can tell a fixed version from a broken one is a
// side effect counted on disk. So here the tool is a real fs_write, the file
// is real, and the assertion is its final contents.

// appendingTool is a real InvokableTool that APPENDS a line to a file every
// time it runs.
//
// Appending rather than writing is deliberate: an idempotent fs_write would
// leave the same file contents after one execution and after two, which is the
// exact ambiguity this test exists to remove. With an append, "ran twice" is
// visible in the file and cannot be confused with "ran once".
type appendingTool struct {
	path string
}

func (a *appendingTool) Info(context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{
		Name: "fs_append",
		Desc: "append a line to the log file",
		ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
			"line": {Type: schema.String, Desc: "text to append"},
		}),
	}, nil
}

func (a *appendingTool) InvokableRun(_ context.Context, argsJSON string, _ ...tool.Option) (string, error) {
	f, err := os.OpenFile(a.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if _, err := fmt.Fprintf(f, "%s\n", argsJSON); err != nil {
		return "", err
	}
	return "appended", nil
}

// drainAll consumes a turn's event stream to completion. The turn does not
// run to the end unless its events are read.
func drainAll(iter *adk.AsyncIterator[*adk.AgentEvent]) {
	for {
		if _, ok := iter.Next(); !ok {
			return
		}
	}
}

// lineCount returns how many lines the tool wrote, i.e. how many times it ran.
func lineCount(t *testing.T, path string) int {
	t.Helper()
	body, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return 0
	}
	require.NoError(t, err)
	var n int
	for _, b := range body {
		if b == '\n' {
			n++
		}
	}
	return n
}

// TestL5Real_ContinuationDoesNotReExecuteTheSideEffect is the L5 acceptance,
// measured on disk.
//
// Attempt 1: the model calls fs_append once, then stops early with a bare
// "ok" -- the premature stop the continuation path exists to rescue.
// Attempt 2: the turn is CONTINUED from the recorded history, and the model
// finishes with text only.
//
// One line must be in the file. Two means the continuation replayed a
// completed side effect, which for a real shell_run would mean running a
// deployment, a migration or a destructive command a second time.
func TestL5Real_ContinuationDoesNotReExecuteTheSideEffect(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "effects.log")
	appender := &appendingTool{path: logPath}

	// Attempt 1: one tool call, then a premature stop.
	first := einollm.NewFakeModelWithMessages([]*schema.Message{
		schema.AssistantMessage("", []schema.ToolCall{{
			ID: "c1", Type: "function",
			Function: schema.FunctionCall{Name: "fs_append", Arguments: `{"line":"one"}`},
		}}),
		schema.AssistantMessage("ok", nil),
	}, nil)

	o, err := New(Config{
		Model:    first,
		Tools:    []BaseTool{appender},
		Profile:  guard.PermissionProfile{Tools: guard.ToolsPerm{Allow: []string{"*"}}},
		WorkRoot: dir,
		MaxIters: 10,
	})
	require.NoError(t, err)

	// Capture the recorded history the way the transport does: from the turn's
	// own context, which is only reachable through a bound recorder.
	// The recorder is bound by the CALLER before the turn, exactly as ws.go's
	// turn loop does; the orchestrator's middleware populates it during the
	// run and the caller reads it back from the same ctx afterwards.
	ctx := WithNewTurnRecorder(context.Background())
	drainAll(o.EventsWithHistory(ctx, []*schema.Message{schema.UserMessage("append one line")}))
	recorded := RecordedTurnMessages(ctx)

	require.Equal(t, 1, lineCount(t, logPath), "attempt 1 must have run the tool exactly once")
	require.NotEmpty(t, recorded,
		"no recorded history: the continuation path would fall back to a full replay, "+
			"which is the behaviour L5 removes")

	// The recorded history must contain the completed pair, otherwise the
	// second attempt cannot know the call already happened.
	var sawToolResult bool
	for _, m := range recorded {
		if m.Role == schema.Tool && m.ToolName == "fs_append" {
			sawToolResult = true
		}
	}
	require.True(t, sawToolResult,
		"the capture omits the tool RESULT, so a continuation would present the call "+
			"as outstanding work and the model would redo it")

	// Attempt 2: continue from the recorded history plus a nudge. The model
	// now answers with text only -- but if the continuation had rewound the
	// history, the ADK would re-issue the pending tool call regardless of what
	// this model says.
	second := einollm.NewFakeModelWithMessages([]*schema.Message{
		schema.AssistantMessage("finished the task", nil),
	}, nil)
	o2, err := New(Config{
		Model:    second,
		Tools:    []BaseTool{appender},
		Profile:  guard.PermissionProfile{Tools: guard.ToolsPerm{Allow: []string{"*"}}},
		WorkRoot: dir,
		MaxIters: 10,
	})
	require.NoError(t, err)

	continued := append(append([]*schema.Message(nil), recorded...),
		schema.UserMessage("The tool calls and results above already happened in this "+
			"same turn — they are done, do not repeat them. Continue."))
	drainAll(o2.EventsWithHistory(context.Background(), continued))

	require.Equal(t, 1, lineCount(t, logPath),
		"the continued attempt RE-EXECUTED the tool: the file has %d lines, want 1. "+
			"For a real shell_run this is a command run twice", lineCount(t, logPath))
}

// TestL5Real_FullReplayDoesReExecute is the CONTROL, and without it the test
// above proves nothing.
//
// It runs the same second attempt from the turn's STARTING history -- the
// pre-L5 behaviour -- and shows the side effect happening a second time. If
// this test ever stops showing two lines, the fixture no longer reproduces the
// hazard and the assertion above has quietly become vacuous.
func TestL5Real_FullReplayDoesReExecute(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "effects.log")
	appender := &appendingTool{path: logPath}

	scripted := func() *einollm.FakeModel {
		return einollm.NewFakeModelWithMessages([]*schema.Message{
			schema.AssistantMessage("", []schema.ToolCall{{
				ID: "c1", Type: "function",
				Function: schema.FunctionCall{Name: "fs_append", Arguments: `{"line":"one"}`},
			}}),
			schema.AssistantMessage("ok", nil),
		}, nil)
	}
	base := []*schema.Message{schema.UserMessage("append one line")}

	for attempt := 0; attempt < 2; attempt++ {
		o, err := New(Config{
			Model:    scripted(),
			Tools:    []BaseTool{appender},
			Profile:  guard.PermissionProfile{Tools: guard.ToolsPerm{Allow: []string{"*"}}},
			WorkRoot: dir,
			MaxIters: 10,
		})
		require.NoError(t, err)
		// Replaying the STARTING history is what the old path did.
		drainAll(o.EventsWithHistory(context.Background(), base))
	}

	require.Equal(t, 2, lineCount(t, logPath),
		"replaying the starting history must run the side effect twice; if it does not, "+
			"this control no longer demonstrates the hazard L5 removes")
}
