package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/guard"
	einollm "github.com/x6nux/yanshi/internal/llm/eino"
	"github.com/x6nux/yanshi/internal/tools"
)

// This file verifies T4 (immediate per-round degradation) against the shape the
// capability was justified by, driven through a REAL turn.
//
// The shape matters and is easy to get wrong in a test. The pre-existing
// spillover cap fires on ONE result over 64 KiB, and a test that feeds one
// enormous result therefore exercises the OLD behaviour and passes without T4
// existing at all. The failure T4 addresses is a HUNDRED results of ten
// kilobytes: every one is comfortably under the single-result cap, and together
// they are the entire window.
//
// So the fixture here is a hundred 10 KiB results produced by a real tool
// inside a real ReAct loop, and the three assertions are the three claims:
// the total shrank, the most recent results did NOT shrink, and what was cut
// is still retrievable from disk.

// bulkOutputTool is a real InvokableTool that returns a fixed-size payload,
// tagged with the call's own argument so each result is distinguishable.
type bulkOutputTool struct {
	sizeBytes int
}

func (b *bulkOutputTool) Info(context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{
		Name: "shell_run",
		Desc: "produce a large output",
		ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
			"command": {Type: schema.String, Desc: "anything"},
		}),
	}, nil
}

func (b *bulkOutputTool) InvokableRun(_ context.Context, argsJSON string, _ ...tool.Option) (string, error) {
	var a struct {
		Command string `json:"command"`
	}
	_ = json.Unmarshal([]byte(argsJSON), &a)
	// A recognisable head and tail around filler, so a degraded copy can be
	// told apart from a lost one: the marker must survive, the filler need not.
	head := fmt.Sprintf("BEGIN-%s\n", a.Command)
	tail := fmt.Sprintf("\nEND-%s\n", a.Command)
	filler := strings.Repeat("log line of roughly forty bytes here....\n",
		(b.sizeBytes-len(head)-len(tail))/41)
	return head + filler + tail, nil
}

// TestT4Real_HundredMediumResultsAreDegradedInARealTurn is the C5/T4 shape
// check: many medium results, not one huge one.
func TestT4Real_HundredMediumResultsAreDegradedInARealTurn(t *testing.T) {
	const calls = 100
	const each = 10 * 1024

	workRoot := t.TempDir()

	// A hundred distinct tool calls, then a final text answer. Distinct
	// arguments so the repetition gate (if anyone enables it later) has no
	// opinion, and so each result is individually identifiable.
	msgs := make([]*schema.Message, 0, calls+1)
	for i := 0; i < calls; i++ {
		msgs = append(msgs, schema.AssistantMessage("", []schema.ToolCall{{
			ID: fmt.Sprintf("c%d", i), Type: "function",
			Function: schema.FunctionCall{
				Name:      "shell_run",
				Arguments: fmt.Sprintf(`{"command":"job-%03d"}`, i),
			},
		}}))
	}
	msgs = append(msgs, schema.AssistantMessage("all done", nil))

	o, err := New(Config{
		Model:    einollm.NewFakeModelWithMessages(msgs, nil),
		Tools:    []BaseTool{&bulkOutputTool{sizeBytes: each}},
		Profile:  guard.PermissionProfile{Tools: guard.ToolsPerm{Allow: []string{"*"}}},
		WorkRoot: workRoot,
		MaxIters: calls + 5,
	})
	require.NoError(t, err)

	ctx := WithNewTurnRecorder(context.Background())
	drainAll(o.EventsWithHistory(ctx, []*schema.Message{schema.UserMessage("run the jobs")}))

	history := RecordedTurnMessages(ctx)
	require.NotEmpty(t, history, "no history was captured; nothing can be asserted")

	var toolMsgs []*schema.Message
	for _, m := range history {
		if m != nil && m.Role == schema.Tool {
			toolMsgs = append(toolMsgs, m)
		}
	}
	require.GreaterOrEqual(t, len(toolMsgs), calls-2,
		"expected ~%d tool results in the captured history, got %d", calls, len(toolMsgs))

	// --- claim 1: the window pressure really came down ------------------
	//
	// Undegraded, a hundred 10 KiB results are ~1 MB. Comparing against the
	// raw product rather than a hardcoded number keeps this honest if the
	// fixture size changes.
	rawBytes := calls * each
	var keptBytes int
	for _, m := range toolMsgs {
		keptBytes += len(m.Content)
	}
	require.Less(t, keptBytes, rawBytes/2,
		"the captured tool results total %d bytes against a raw %d: the hundred-medium-"+
			"results shape was not degraded at all", keptBytes, rawBytes)

	// --- claim 2: the RECENT results were not touched --------------------
	//
	// This is the half that makes degradation safe. The model is mid-decision
	// on the last few results, and cutting those makes it re-read, costing
	// more than the cut saved.
	recent := toolMsgs[len(toolMsgs)-tools.DegradeKeepRecent:]
	for i, m := range recent {
		require.False(t, tools.AlreadyDegraded(m.Content),
			"recent result #%d (of the last %d) was degraded; the working set must be "+
				"left intact", i, tools.DegradeKeepRecent)
		require.Greater(t, len(m.Content), tools.DegradeMaxBytes,
			"recent result #%d is only %d bytes, so it may have been shrunk by something "+
				"else and claim 2 is not actually being tested", i, len(m.Content))
	}

	// --- claim 3: what was cut is still retrievable ----------------------
	//
	// A shrink whose original is nowhere is a deletion. Every degraded body
	// must carry a pointer, and following it must produce the real text.
	var degraded int
	for _, m := range toolMsgs {
		if !tools.AlreadyDegraded(m.Content) {
			continue
		}
		degraded++
		rel := spillPathFromPreview(t, m.Content)
		full, rerr := os.ReadFile(filepath.Join(workRoot, rel))
		require.NoError(t, rerr,
			"a degraded result's recovery pointer (%s) does not resolve: the cut text "+
				"is gone, which is deletion rather than degradation", rel)
		require.Greater(t, len(full), tools.DegradeMaxBytes,
			"the recovered file is smaller than the degrade ceiling, so the pointer "+
				"resolves to another preview rather than the original")
		require.Contains(t, string(full), "BEGIN-job-",
			"the recovered text is not the tool's real output")
	}
	require.Greater(t, degraded, calls/2,
		"only %d of %d results were degraded; most of the window is still raw output",
		degraded, calls)
}

// spillPathFromPreview extracts the recovery path from a degraded body's
// header, which has the documented form
//
//	[spilled: <n> lines / <size> → <path>]
//
// Parsed here rather than reused from ctxcompact because that package's parser
// is unexported; the format itself is the contract, and it is pinned from both
// sides already (internal/ctxcompact::TestSpillMarkerMatchesToolsPreview and
// internal/tools::TestDegradeUsesTheFoldRecognisedMarker). A third reader that
// follows the same header is precisely what a model-facing pointer has to
// survive.
func spillPathFromPreview(t *testing.T, body string) string {
	t.Helper()
	const prefix = "[spilled: "
	i := strings.Index(body, prefix)
	require.GreaterOrEqual(t, i, 0,
		"a degraded body carries no spill header; the model is told the text was "+
			"shrunk and given no way to get it: %.200s", body)
	rest := body[i+len(prefix):]
	end := strings.IndexByte(rest, ']')
	require.GreaterOrEqual(t, end, 0, "unterminated spill header: %.200s", body)
	arrow := strings.LastIndex(rest[:end], "→")
	require.GreaterOrEqual(t, arrow, 0, "spill header carries no path: %.200s", body)
	return strings.TrimSpace(rest[:end][arrow+len("→"):])
}

var _ = adk.AgentEvent{}
