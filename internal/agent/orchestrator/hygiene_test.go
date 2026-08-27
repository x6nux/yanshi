package orchestrator

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/schema"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/guard"
	"github.com/x6nux/yanshi/internal/tools"
)

// bigToolResult builds a tool result comfortably above the degrade floor.
func bigToolResult(id string) *schema.Message {
	var b strings.Builder
	for b.Len() < tools.DegradeMaxBytes*6 {
		b.WriteString("a line of output from the tool that ran, again and again\n")
	}
	return &schema.Message{
		Role: schema.Tool, ToolCallID: id, ToolName: "run_tests", Content: b.String(),
	}
}

// hygieneCtx binds a work root (so degradation can spill) and optionally a
// background manager.
func hygieneCtx(t *testing.T, mgr *tools.BackgroundManager) context.Context {
	t.Helper()
	ctx := tools.WithWorkRoot(context.Background(), t.TempDir())
	return tools.WithBackgroundManager(ctx, mgr)
}

// TestDegradeHistoryKeepsTheRecentOnes is the whole shape of T4: old results
// shrink, the ones the model is working with right now do not.
func TestDegradeHistoryKeepsTheRecentOnes(t *testing.T) {
	ctx := hygieneCtx(t, nil)
	const n = 6
	msgs := make([]*schema.Message, 0, n)
	for i := 0; i < n; i++ {
		msgs = append(msgs, bigToolResult(string(rune('a'+i))))
	}
	originals := make([]int, n)
	for i, m := range msgs {
		originals[i] = len(m.Content)
	}

	out, changed := degradeHistory(ctx, msgs)
	require.True(t, changed)
	require.Len(t, out, n)

	keep := tools.DegradeKeepRecent
	for i := 0; i < n-keep; i++ {
		assert.Less(t, len(out[i].Content), originals[i],
			"result %d is no longer recent and must have been degraded", i)
		assert.Contains(t, out[i].Content, "[spilled:",
			"a degraded result must carry a recovery pointer")
	}
	for i := n - keep; i < n; i++ {
		assert.Equal(t, originals[i], len(out[i].Content),
			"result %d is within the keep-recent window; the model is acting on it", i)
	}

	// The INPUT slice is untouched: the caller may still hold it (the recorder
	// middleware captures the ADK's slice), and rewriting in place would
	// mutate a view somebody else owns.
	for i, m := range msgs {
		assert.Equal(t, originals[i], len(m.Content), "input message %d was mutated", i)
	}
}

// TestDegradeHistoryLeavesNonToolMessagesAlone. A user turn and an assistant's
// reasoning are recoverable from nowhere — there is no spill file and no
// re-runnable call — so shrinking them would be pure loss.
func TestDegradeHistoryLeavesNonToolMessagesAlone(t *testing.T) {
	ctx := hygieneCtx(t, nil)
	long := strings.Repeat("requirements the user actually typed and must not lose. ", 200)
	msgs := []*schema.Message{
		{Role: schema.User, Content: long},
		{Role: schema.Assistant, Content: long},
		// A Tool-role message with NO call id is not a pairable tool result;
		// EnforceToolCallPairs cannot reason about it, so this pass must not
		// touch it either.
		{Role: schema.Tool, Content: long},
		bigToolResult("t1"), bigToolResult("t2"), bigToolResult("t3"),
	}
	out, changed := degradeHistory(ctx, msgs)
	require.True(t, changed, "the leading paired result is outside keep-recent and should degrade")
	assert.Equal(t, long, out[0].Content)
	assert.Equal(t, long, out[1].Content)
	assert.Equal(t, long, out[2].Content, "an unpaired Tool message is not a tool result")
	assert.Less(t, len(out[3].Content), len(long)*0+len(msgs[3].Content))
}

// TestDegradeHistoryIsANoOpOnSmallHistories. Nothing to shrink means nothing
// allocated and nothing rewritten — including the empty case, which is the one
// that would panic on an off-by-one.
func TestDegradeHistoryIsANoOpOnSmallHistories(t *testing.T) {
	ctx := hygieneCtx(t, nil)
	for _, tc := range []struct {
		name string
		in   []*schema.Message
	}{
		{"empty", nil},
		{"only user turns", []*schema.Message{schema.UserMessage("hi")}},
		{"a nil entry", []*schema.Message{nil, schema.UserMessage("hi")}},
		{"fewer results than keep-recent", []*schema.Message{
			bigToolResult("a"), bigToolResult("b"),
		}},
		{"a short old result", []*schema.Message{
			{Role: schema.Tool, ToolCallID: "a", Content: "ok"},
			bigToolResult("b"), bigToolResult("c"),
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, changed := degradeHistory(ctx, tc.in)
			assert.False(t, changed)
			assert.Equal(t, len(tc.in), len(out))
		})
	}
}

// TestDegradeHistoryIsIdempotentAcrossIterations. The middleware runs on EVERY
// iteration over a growing history, so a pass that re-degraded its own output
// would spill a new file each time and hand back a pointer that resolves to
// strictly less than the previous one.
func TestDegradeHistoryIsIdempotentAcrossIterations(t *testing.T) {
	root := t.TempDir()
	ctx := tools.WithWorkRoot(context.Background(), root)
	msgs := []*schema.Message{
		bigToolResult("a"), bigToolResult("b"), bigToolResult("c"),
	}
	once, changed := degradeHistory(ctx, msgs)
	require.True(t, changed)
	twice, changedAgain := degradeHistory(ctx, once)
	require.False(t, changedAgain, "a second pass must find nothing left to do")
	assert.Equal(t, once[0].Content, twice[0].Content)

	entries, err := os.ReadDir(filepath.Join(root, ".yanshi/tmp/spillover"))
	require.NoError(t, err)
	assert.Len(t, entries, 1, "the second pass must not write a second copy")
}

// TestBackgroundNoticesAreUserMessages is the T3 pairing constraint at the
// place it is enforced.
//
// ctxcompact.EnforceToolCallPairs runs a fixpoint over tool_call/tool_result
// pairing. The offloaded call's tool_result was already delivered (the "moved
// to the background" acknowledgement), so a SECOND tool message for the same
// call would be unpairable — a message the compactor cannot classify and
// providers reject outright. QwenPaw makes the same choice for the same
// reason (tool_calls/_hint.py emits no ToolResultBlock).
func TestBackgroundNoticesAreUserMessages(t *testing.T) {
	mgr := tools.NewBackgroundManager()
	t.Cleanup(func() { mgr.Close() })
	h := mgr.Adopt("run_tests", `{"framework":"go"}`, func() {})
	require.NotNil(t, h)
	h.Finish("ok  github.com/x/y  118.4s", nil)

	notices := backgroundNotices(tools.WithBackgroundManager(context.Background(), mgr))
	require.Len(t, notices, 1)
	assert.Equal(t, schema.User, notices[0].Role,
		"role=tool here is unpairable: EnforceToolCallPairs has no assistant tool_call left to match")
	assert.Empty(t, notices[0].ToolCallID)
	assert.Contains(t, notices[0].Content, "<system-notification>")
	assert.Contains(t, notices[0].Content, "118.4s")

	assert.Nil(t, backgroundNotices(tools.WithBackgroundManager(context.Background(), mgr)),
		"a notice must be delivered exactly once")
	assert.Nil(t, backgroundNotices(context.Background()),
		"no manager bound means no notices, not a panic")
}

// TestResultHygieneAppendsNoticesAndDegrades exercises the middleware itself,
// which is what actually runs in production — the two helpers above could both
// be correct while the hook wired them up backwards or not at all.
func TestResultHygieneAppendsNoticesAndDegrades(t *testing.T) {
	mgr := tools.NewBackgroundManager()
	t.Cleanup(func() { mgr.Close() })
	h := mgr.Adopt("shell_run", `{"command":"make"}`, func() {})
	require.NotNil(t, h)
	h.Finish("build finished", nil)

	ctx := hygieneCtx(t, mgr)
	state := &adk.ChatModelAgentState{Messages: []*schema.Message{
		schema.UserMessage("run the build"),
		bigToolResult("a"), bigToolResult("b"), bigToolResult("c"),
	}}
	before := len(state.Messages[1].Content)

	m := newResultHygiene()
	_, got, err := m.BeforeModelRewriteState(ctx, state, nil)
	require.NoError(t, err)
	require.NotNil(t, got)

	require.Len(t, got.Messages, 5, "the completion notice must have been appended")
	last := got.Messages[len(got.Messages)-1]
	assert.Equal(t, schema.User, last.Role)
	assert.Contains(t, last.Content, "build finished")

	assert.Less(t, len(got.Messages[1].Content), before,
		"the oldest tool result must have been degraded in the same pass")
}

// TestResultHygieneToleratesAnEmptyState. ADK middlewares are called with
// whatever the runner has; a nil state must be a no-op rather than a panic
// that kills the turn.
func TestResultHygieneToleratesAnEmptyState(t *testing.T) {
	m := newResultHygiene()
	ctx, state, err := m.BeforeModelRewriteState(context.Background(), nil, nil)
	require.NoError(t, err)
	assert.Nil(t, state)
	assert.NotNil(t, ctx)

	empty := &adk.ChatModelAgentState{}
	_, got, err := m.BeforeModelRewriteState(context.Background(), empty, nil)
	require.NoError(t, err)
	assert.Empty(t, got.Messages)
}

// TestResultHygieneIsInstalledOnEveryRunner. The middleware being CORRECT and
// the middleware being WIRED are separate facts, and this repository's
// dominant defect is the second one silently missing. Reading the handler
// slice back off a real runner build is the only way to check it without a
// live model.
func TestResultHygieneIsInstalledOnEveryRunner(t *testing.T) {
	found := false
	for _, h := range orchestratorMiddlewares() {
		if _, ok := h.(*resultHygiene); ok {
			found = true
		}
	}
	require.True(t, found,
		"resultHygiene is not in the handler slice: T3 notices are never delivered and "+
			"T4 never runs, while every unit test above still passes")
}

// TestBackgroundManagerReachesToolsFromTheTurnContext closes the other half of
// the wiring: the manager must actually arrive in the context a tool reads.
func TestBackgroundManagerReachesToolsFromTheTurnContext(t *testing.T) {
	mgr := tools.NewBackgroundManager()
	t.Cleanup(func() { mgr.Close() })
	o := &Orchestrator{background: mgr, sessionRules: map[string]*guard.RuleSet{}}
	got, ok := tools.BackgroundManagerFromContext(o.bindExecutionContext(context.Background(), ""))
	require.True(t, ok, "bindExecutionContext must bind the background manager, or no tool can offload")
	assert.Same(t, mgr, got)

	none := &Orchestrator{sessionRules: map[string]*guard.RuleSet{}}
	_, bound := tools.BackgroundManagerFromContext(none.bindExecutionContext(context.Background(), ""))
	assert.False(t, bound, "a nil manager must leave the context clean")
}

// TestBackgroundHardLimitIsGenerous states the constant's relationship to the
// longest legitimate foreground timeout in the registry (run_tests, 10m). An
// offload that expires sooner than the tool's own budget would be a slightly
// later failure rather than a reprieve.
func TestBackgroundHardLimitIsGenerous(t *testing.T) {
	assert.Greater(t, tools.BackgroundHardLimit, 10*time.Minute,
		"the background limit must exceed the longest foreground tool budget")
	assert.Less(t, tools.BackgroundCloseGrace, tools.BackgroundHardLimit,
		"shutdown must not wait out the hard limit")
}
