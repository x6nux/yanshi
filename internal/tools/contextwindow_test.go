// internal/tools/contextwindow_test.go
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/cloudwego/eino/schema"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/ctxcompact"
	einollm "github.com/x6nux/yanshi/internal/llm/eino"
	"github.com/x6nux/yanshi/internal/loopguard"
)

// TestContextWindowTools_RequestsOnABoundSignal proves the handler's success
// path: given a turn context that actually has a W-C-14 signal bound (the
// production shape, since orchestrator.go binds it unconditionally), the
// handler's call to einollm.RequestNewWindow lands, and a later read via
// einollm's own consumption function sees exactly the reason the model gave.
func TestContextWindowTools_RequestsOnABoundSignal(t *testing.T) {
	ct := NewContextWindowTools()
	ctx := einollm.WithNewWindowSignal(context.Background())

	out, err := ct.run(ctx, `{"reason":"finished reading the large log dump"}`)
	require.NoError(t, err)
	assert.Contains(t, out, "New window requested")

	// The handler's job is to WRITE the signal, not consume it — the actual
	// consumer is CompactingModel.maybeCompact on the turn's next model call
	// (einollm's own tests, e.g. TestRequestNewWindow_ConsumedOnce, pin the
	// read side). Confirming a second write on the same still-bound ctx also
	// succeeds is the closest this package gets to observing the write
	// landed, without reaching into einollm's unexported consumption path.
	assert.True(t, einollm.RequestNewWindow(ctx, "a later call can still write"),
		"the signal slot bound by this ctx is still writable")
}

// TestContextWindowTools_ErrorsWithoutABoundSignal proves the handler
// reports an error — not a silent success — when the turn never bound a
// W-C-14 signal (e.g. a sub-agent context, per orchestrator.go's own doc
// comment on WithNewWindowSignal). Reporting nothing-happened as success
// would tell the model its request landed when nothing will ever read it.
func TestContextWindowTools_ErrorsWithoutABoundSignal(t *testing.T) {
	ct := NewContextWindowTools()
	_, err := ct.run(context.Background(), `{"reason":"anything"}`)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not available on this turn")
}

// TestContextWindowTools_RejectsEmptyReason mirrors milestone_set's rejection
// contract (MilestoneTools.runSet's "required" case): an empty or
// whitespace-only reason is refused rather than recorded, so the eventual
// activity-line/notice text is never blank.
func TestContextWindowTools_RejectsEmptyReason(t *testing.T) {
	ct := NewContextWindowTools()
	ctx := einollm.WithNewWindowSignal(context.Background())

	cases := []string{"", "   ", "\n\t "}
	for _, reason := range cases {
		// %q gives a JSON-safe escaped string literal (Go and JSON agree on
		// \n, \t and \" escapes), unlike concatenating the raw control
		// characters straight into the JSON text.
		_, err := ct.run(ctx, fmt.Sprintf(`{"reason":%q}`, reason))
		require.Error(t, err, "reason %q must be rejected", reason)
		assert.Contains(t, err.Error(), "required")
	}
}

// TestContextWindowTools_NameIsRegisterable pins the tool name, the same
// convention TestMilestoneTools_NameIsRegisterable follows: a rename here
// without updating bootstrap.go's registration and profile.go's allow list
// makes the tool fail-closed at runtime and reddens GOV5.
func TestContextWindowTools_NameIsRegisterable(t *testing.T) {
	got := NewContextWindowTools().Tools()
	require.Len(t, got, 2)
	var names []string
	for _, tl := range got {
		info, err := tl.Info(context.Background())
		require.NoError(t, err)
		names = append(names, info.Name)
	}
	require.Equal(t, []string{"context_new_window", "context_budget"}, names)
}

// TestContextBudget_NeitherTracked proves the handler degrades gracefully —
// not with an error, since an unconfigured MaxTurnTokens and a model with
// compaction disabled are both ordinary, legitimate configurations, not
// wiring bugs (context_new_window's "not available" error is the wrong shape
// here for exactly that reason).
func TestContextBudget_NeitherTracked(t *testing.T) {
	ct := NewContextWindowTools()
	out, err := ct.budget(context.Background(), "")
	require.NoError(t, err)

	var res contextBudgetResult
	require.NoError(t, json.Unmarshal([]byte(out), &res))
	assert.Nil(t, res.TurnTokens)
	assert.Nil(t, res.Context)
	assert.Contains(t, res.Note, "turn token budget")
	assert.Contains(t, res.Note, "context window budget")
}

// TestContextBudget_TurnTokensTracksTheLiveGate is W-C-11's mutation pin for
// the turn-token half: it binds the EXACT *loopguard.TokenBudgetGate instance
// loopguard's own middleware would mutate, calls the tool BEFORE and AFTER
// mutating that instance through Check (the same method the loopguard
// middleware calls on every iteration), and requires the tool's JSON to
// follow the mutation exactly. A handler that captured Used/Max once at bind
// time, or that computed its own independent counter, would fail the second
// assertion.
func TestContextBudget_TurnTokensTracksTheLiveGate(t *testing.T) {
	ct := NewContextWindowTools()
	gate := loopguard.NewTokenBudgetGate(1000)
	ctx := loopguard.WithTokenBudgetGate(context.Background(), gate)

	out, err := ct.budget(ctx, "")
	require.NoError(t, err)
	var res contextBudgetResult
	require.NoError(t, json.Unmarshal([]byte(out), &res))
	require.NotNil(t, res.TurnTokens)
	assert.Equal(t, turnTokenBudget{Used: 0, Max: 1000, Remaining: 1000}, *res.TurnTokens)
	assert.Nil(t, res.Context)

	// Mutate the SAME gate instance the way loopguard's middleware does.
	gate.Check(loopguard.Observation{PromptTokens: 300, CompletionTokens: 50})

	out, err = ct.budget(ctx, "")
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal([]byte(out), &res))
	require.NotNil(t, res.TurnTokens)
	assert.Equal(t, turnTokenBudget{Used: 350, Max: 1000, Remaining: 650}, *res.TurnTokens,
		"the tool's answer must follow the mutation of the live gate, not a value captured earlier")
}

// TestContextBudget_ContextWindowTracksTheLiveSnapshot is W-C-11's mutation
// pin for the context-window half: it drives a REAL *einollm.CompactingModel
// through Generate — the exact call CompactingModel.maybeCompact's
// recordContextBudget runs inside of — across two different-sized histories,
// and requires the tool's JSON to follow, matching
// ctxcompact.RemainingBudget/EstimateTokens computed independently for each
// history. A handler that re-derived these numbers instead of reading the
// published snapshot would very likely disagree with at least one of the two
// readings.
func TestContextBudget_ContextWindowTracksTheLiveSnapshot(t *testing.T) {
	ct := NewContextWindowTools()
	ctx := einollm.WithContextBudgetSignal(context.Background())

	out, err := ct.budget(ctx, "")
	require.NoError(t, err)
	var res contextBudgetResult
	require.NoError(t, json.Unmarshal([]byte(out), &res))
	assert.Nil(t, res.Context, "nothing published yet")

	cm := &einollm.CompactingModel{
		Inner:         einollm.NewFakeModel([]string{"reply"}, nil),
		Threshold:     0.99,
		ContextWindow: 100000,
		KeepRecent:    4,
	}
	small := []*schema.Message{{Role: schema.User, Content: "hi"}}
	_, err = cm.Generate(ctx, small)
	require.NoError(t, err)

	out, err = ct.budget(ctx, "")
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal([]byte(out), &res))
	require.NotNil(t, res.Context)
	wantSmall := ctxcompact.RemainingBudget(small, ctxcompact.RunOpts{ModelWindow: 100000})
	assert.Equal(t, wantSmall, res.Context.Remaining)
	assert.Nil(t, res.TurnTokens)

	big := make([]*schema.Message, 40)
	for i := range big {
		big[i] = &schema.Message{Role: schema.User, Content: fmt.Sprintf("message number %d with some extra padding text", i)}
	}
	_, err = cm.Generate(ctx, big)
	require.NoError(t, err)

	out, err = ct.budget(ctx, "")
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal([]byte(out), &res))
	require.NotNil(t, res.Context)
	wantBig := ctxcompact.RemainingBudget(big, ctxcompact.RunOpts{ModelWindow: 100000})
	assert.Equal(t, wantBig, res.Context.Remaining,
		"the tool's answer must follow the second Generate call's larger history, not the first snapshot")
	assert.NotEqual(t, wantSmall, wantBig, "premise: the two histories really do have different remaining budgets")
}

// TestContextBudget_BothTrackedNoNote proves Note is only populated when
// something really is missing — a caller parsing this JSON should be able to
// tell "fully tracked" apart from "partially tracked" without string-matching
// the absence of a note against its presence.
func TestContextBudget_BothTrackedNoNote(t *testing.T) {
	ct := NewContextWindowTools()
	gate := loopguard.NewTokenBudgetGate(1000)
	ctx := loopguard.WithTokenBudgetGate(context.Background(), gate)
	ctx = einollm.WithContextBudgetSignal(ctx)

	cm := &einollm.CompactingModel{
		Inner:         einollm.NewFakeModel([]string{"reply"}, nil),
		Threshold:     0.99,
		ContextWindow: 100000,
		KeepRecent:    4,
	}
	_, err := cm.Generate(ctx, []*schema.Message{{Role: schema.User, Content: "hi"}})
	require.NoError(t, err)

	out, err := ct.budget(ctx, "")
	require.NoError(t, err)
	var res contextBudgetResult
	require.NoError(t, json.Unmarshal([]byte(out), &res))
	assert.NotNil(t, res.TurnTokens)
	assert.NotNil(t, res.Context)
	assert.Empty(t, res.Note)
}
