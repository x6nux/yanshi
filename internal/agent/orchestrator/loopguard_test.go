package orchestrator

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/ctxcompact"
	"github.com/x6nux/yanshi/internal/guard"
	einollm "github.com/x6nux/yanshi/internal/llm/eino"
	"github.com/x6nux/yanshi/internal/loopguard"
	"github.com/x6nux/yanshi/internal/proto"
	"github.com/x6nux/yanshi/internal/toolreg"
)

// countingTool is a real InvokableTool that records every invocation. Preferred
// over a mock: the assertion "the tool ran N times" is the entire point of the
// budget tests, and a counter is the smallest thing that can carry it.
type countingTool struct {
	name  string
	calls int
	seen  []string
}

func (c *countingTool) Info(context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{
		Name: c.name,
		Desc: "counting test tool",
		ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
			"q": {Type: schema.String, Desc: "anything"},
		}),
	}, nil
}

func (c *countingTool) InvokableRun(_ context.Context, argsJSON string, _ ...tool.Option) (string, error) {
	c.calls++
	c.seen = append(c.seen, argsJSON)
	return "ok", nil
}

// toolCallMsg builds an assistant message carrying one tool call.
func toolCallMsg(id, name, args string) *schema.Message {
	return schema.AssistantMessage("", []schema.ToolCall{
		{ID: id, Type: "function", Function: schema.FunctionCall{Name: name, Arguments: args}},
	})
}

// repeatToolModel returns a FakeModel that emits the SAME tool call forever —
// the doom loop L1 exists to catch.
func repeatToolModel(name, args string) *einollm.FakeModel {
	m := einollm.NewFakeModelWithMessages([]*schema.Message{toolCallMsg("c", name, args)}, nil)
	m.Repeat = true
	return m
}

// drainFrames collects every frame the turn produced, keyed by type.
func drainFrames(iter *adk.AsyncIterator[*adk.AgentEvent]) (text string, errText string) {
	var sb strings.Builder
	var errs strings.Builder
	ClassifyEvents(iter, func(f proto.ServerFrame) {
		switch f.Type {
		case "agent_chunk":
			sb.WriteString(f.Text)
		case "error":
			errs.WriteString(f.Text)
		}
	})
	return sb.String(), errs.String()
}

func allowAll() guard.PermissionProfile {
	return guard.PermissionProfile{Tools: guard.ToolsPerm{Allow: []string{"*"}}}
}

// --- L6: framework wiring ---

func TestLoopGuardConfigEnabled(t *testing.T) {
	cases := []struct {
		name string
		cfg  LoopGuardConfig
		want bool
	}{
		{"zero value installs nothing", LoopGuardConfig{}, false},
		{"repetition", LoopGuardConfig{RepetitionEnabled: true}, true},
		{"total tool calls", LoopGuardConfig{MaxToolCalls: 5}, true},
		{"per tool", LoopGuardConfig{PerToolCalls: map[string]int{"a": 1}}, true},
		{"timeout", LoopGuardConfig{TurnTimeout: time.Second}, true},
		{"tokens", LoopGuardConfig{MaxTurnTokens: 100}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, tc.cfg.enabled())
		})
	}
}

func TestWithLoopGuardBindsOnlyWhenConfigured(t *testing.T) {
	if _, ok := loopGuardFromContext(WithLoopGuard(context.Background(), LoopGuardConfig{})); ok {
		t.Fatal("zero config must not bind a guard")
	}
	ctx := WithLoopGuard(context.Background(), LoopGuardConfig{MaxTurnTokens: 10})
	g, ok := loopGuardFromContext(ctx)
	require.True(t, ok, "configured guard must bind")
	assert.Equal(t, []string{"token-budget"}, g.handler.Names())
	assert.Nil(t, g.budget, "no tool budget was configured")
}

// TestWithLoopGuard_BindsTheTokenBudgetGateItself is W-C-11's pin at the
// orchestrator wiring layer: WithLoopGuard must bind the SAME
// *loopguard.TokenBudgetGate instance the returned handler enforces
// MaxTurnTokens with, retrievable via loopguard.TokenBudgetGateFromContext —
// not a second gate built from the same config, which is exactly the kind of
// duplicate the context_budget tool must not read from (see
// loopguard.WithTokenBudgetGate's doc comment).
func TestWithLoopGuard_BindsTheTokenBudgetGateItself(t *testing.T) {
	ctx := WithLoopGuard(context.Background(), LoopGuardConfig{MaxTurnTokens: 5000})
	gate, ok := loopguard.TokenBudgetGateFromContext(ctx)
	require.True(t, ok, "a configured MaxTurnTokens must bind a retrievable gate")
	assert.Equal(t, 5000, gate.Max())
	assert.Equal(t, 0, gate.Used(), "fresh turn, nothing spent yet")
}

// TestWithLoopGuard_NoTokenBudgetMeansNoBoundGate proves the negative: when
// MaxTurnTokens is unconfigured (but some other gate is, e.g. repetition
// detection alone), WithLoopGuard still binds a turnGuard, but there is no
// *loopguard.TokenBudgetGate to retrieve.
func TestWithLoopGuard_NoTokenBudgetMeansNoBoundGate(t *testing.T) {
	ctx := WithLoopGuard(context.Background(), LoopGuardConfig{RepetitionEnabled: true})
	_, ok := loopguard.TokenBudgetGateFromContext(ctx)
	assert.False(t, ok, "no MaxTurnTokens configured, so no token-budget gate to bind")
}

func TestBuildHandlerGateSelection(t *testing.T) {
	cases := []struct {
		name string
		cfg  LoopGuardConfig
		want []string
	}{
		{"none", LoopGuardConfig{}, nil},
		{"repetition only", LoopGuardConfig{RepetitionEnabled: true}, []string{"repetition"}},
		{"tokens only", LoopGuardConfig{MaxTurnTokens: 1}, []string{"token-budget"}},
		{"deadline only", LoopGuardConfig{TurnTimeout: time.Second}, []string{"deadline"}},
		{
			"all three in priority order",
			LoopGuardConfig{RepetitionEnabled: true, MaxTurnTokens: 1, TurnTimeout: time.Second},
			[]string{"repetition", "token-budget", "deadline"},
		},
		// Tool budgets are NOT gates (they answer at the tool boundary with a
		// tool result, not at the iteration boundary with a stop).
		{"tool budget contributes no gate", LoopGuardConfig{MaxToolCalls: 5}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			handler, _ := tc.cfg.buildHandler()
			got := handler.Names()
			if len(tc.want) == 0 {
				assert.Empty(t, got)
				return
			}
			// Equal, not ElementsMatch: the priority ORDER is the assertion.
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestBuildHandlerCustomRepetitionStages(t *testing.T) {
	cfg := LoopGuardConfig{RepetitionEnabled: true, RepetitionWarnAfter: 2, RepetitionStopAfter: 3, RepetitionWindow: 2}
	h, _ := cfg.buildHandler()
	same := loopguard.ToolCall{Name: "x", ArgsHash: "h"}
	var last loopguard.Result
	for i := 0; i < 3; i++ {
		last = h.Evaluate(loopguard.Observation{Iteration: i, Calls: []loopguard.ToolCall{same}})
	}
	assert.Equal(t, loopguard.ActionStop, last.Action, "custom stop threshold must be honoured")
}

// TestLoopGuardStateIsPerTurn is the assertion that catches the failure mode
// runnerFor's memoisation creates: one middleware instance serves every turn,
// so anything mutable on it would leak across turns and sessions.
func TestLoopGuardStateIsPerTurn(t *testing.T) {
	cfg := LoopGuardConfig{MaxToolCalls: 1}
	a, okA := loopGuardFromContext(WithLoopGuard(context.Background(), cfg))
	b, okB := loopGuardFromContext(WithLoopGuard(context.Background(), cfg))
	require.True(t, okA)
	require.True(t, okB)
	require.NotSame(t, a, b, "each turn must get its own state")

	a.budget.Consume("t")
	assert.Equal(t, 1, a.budget.Total())
	assert.Equal(t, 0, b.budget.Total(), "one turn's spend must not deplete another's budget")
}

func TestCollectToolCalls(t *testing.T) {
	msgs := []*schema.Message{
		schema.UserMessage("go"),
		toolCallMsg("c1", "fs_read", `{"path":"a"}`),
		nil,
		schema.AssistantMessage("done", nil),
		toolCallMsg("c2", "shell_run", `{"cmd":"ls"}`),
	}
	all := collectToolCalls(msgs, 0)
	require.Len(t, all, 2)
	assert.Equal(t, "fs_read", all[0].Name)
	assert.Equal(t, "shell_run", all[1].Name)
	assert.Equal(t, loopguard.HashArgs(`{"path":"a"}`), all[0].ArgsHash)

	// Only the tail is harvested, so a call is never double-counted.
	assert.Len(t, collectToolCalls(msgs, 2), 1)
	assert.Empty(t, collectToolCalls(msgs, len(msgs)))
	assert.Len(t, collectToolCalls(msgs, -1), 2, "negative offset clamps to 0")

	// A tool call with no name cannot be attributed and is dropped.
	unnamed := []*schema.Message{toolCallMsg("c", "", "{}")}
	assert.Empty(t, collectToolCalls(unnamed, 0))
}

func TestLatestUsage(t *testing.T) {
	withUsage := func(p, c int) *schema.Message {
		m := schema.AssistantMessage("x", nil)
		m.ResponseMeta = &schema.ResponseMeta{Usage: &schema.TokenUsage{PromptTokens: p, CompletionTokens: c}}
		return m
	}
	msgs := []*schema.Message{withUsage(10, 1), nil, schema.AssistantMessage("no meta", nil), withUsage(20, 2)}
	p, c := latestUsage(msgs, 0)
	assert.Equal(t, 20, p)
	assert.Equal(t, 2, c)

	// Only the new tail is scanned: rescanning from zero would keep
	// re-reporting an old call's usage once a provider stopped attaching any.
	p, c = latestUsage(msgs, 1)
	assert.Equal(t, 20, p)
	assert.Equal(t, 2, c)
	p, c = latestUsage(msgs, 2)
	assert.Equal(t, 20, p)
	assert.Equal(t, 2, c)

	p, c = latestUsage(msgs, len(msgs))
	assert.Zero(t, p)
	assert.Zero(t, c)
	p, c = latestUsage([]*schema.Message{schema.AssistantMessage("x", nil)}, 0)
	assert.Zero(t, p)
	assert.Zero(t, c)
	p, c = latestUsage(msgs, -1)
	assert.Equal(t, 20, p)
	assert.Equal(t, 2, c)
}

func TestTurnGuardNudgeCap(t *testing.T) {
	// A gate that never stops and always nudges: without maxNudgesPerGate the
	// conversation fills with identical warnings.
	g := &turnGuard{
		handler: loopguard.NewHandler(alwaysNudge{}),
		started: time.Now(),
		now:     time.Now,
		nudges:  map[string]int{},
	}
	var nudges int
	for i := 0; i < 10; i++ {
		if g.observe(nil).Action == loopguard.ActionModifyPrompt {
			nudges++
		}
	}
	assert.Equal(t, maxNudgesPerGate, nudges, "a single gate must not nudge unboundedly")
}

// alwaysNudge is a gate that asks for a nudge on every iteration.
type alwaysNudge struct{}

func (alwaysNudge) Name() string  { return "always" }
func (alwaysNudge) Priority() int { return 1 }
func (alwaysNudge) Check(loopguard.Observation) loopguard.Result {
	return loopguard.Result{Action: loopguard.ActionModifyPrompt, Continuation: "change course"}
}

// --- L1: repetition, end to end through the real ADK loop ---

// TestLoopGuardE2E_RepetitionStopsRunawayTurn drives a real turn with a model
// that emits the same tool call forever. Without the guard the loop runs to
// MaxIters; with it the turn ends with a named reason.
func TestLoopGuardE2E_RepetitionStopsRunawayTurn(t *testing.T) {
	ct := &countingTool{name: "fs_read"}
	o, err := New(Config{
		Model:    repeatToolModel("fs_read", `{"path":"a.go"}`),
		Tools:    []BaseTool{ct},
		Profile:  allowAll(),
		MaxIters: 50,
		LoopGuard: LoopGuardConfig{
			RepetitionEnabled: true,
		},
	})
	require.NoError(t, err)

	_, errText := drainFrames(o.Events(context.Background(), "read it"))
	require.NotEmpty(t, errText, "a runaway turn must end with an error frame")
	assert.Contains(t, errText, "repetition", "the stop must name the gate")
	assert.Contains(t, errText, "fs_read", "the stop must name the tool")
	assert.Less(t, ct.calls, 50, "the guard must stop well before MaxIters")
}

// TestLoopGuardE2E_NoGuardRunsToMaxIters is the CONTROL for the test above: it
// proves the runaway is real and that the guard, not something else, is what
// stops it. Without this pairing a repetition test passes even if the model
// simply stopped on its own.
func TestLoopGuardE2E_NoGuardRunsToMaxIters(t *testing.T) {
	ct := &countingTool{name: "fs_read"}
	o, err := New(Config{
		Model:    repeatToolModel("fs_read", `{"path":"a.go"}`),
		Tools:    []BaseTool{ct},
		Profile:  allowAll(),
		MaxIters: 12,
		// LoopGuard deliberately absent.
	})
	require.NoError(t, err)
	drainFrames(o.Events(context.Background(), "read it"))
	assert.GreaterOrEqual(t, ct.calls, 10,
		"without the guard the same call repeats until the ADK iteration cap; if this drops, the L1 test above proves nothing")
}

// TestLoopGuardE2E_DistinctArgsAreNotRepetition is the false-positive guard:
// paging through a large file must not be mistaken for a doom loop.
func TestLoopGuardE2E_DistinctArgsAreNotRepetition(t *testing.T) {
	ct := &countingTool{name: "fs_read"}
	msgs := []*schema.Message{
		toolCallMsg("c1", "fs_read", `{"path":"big.go","offset":0}`),
		toolCallMsg("c2", "fs_read", `{"path":"big.go","offset":100}`),
		toolCallMsg("c3", "fs_read", `{"path":"big.go","offset":200}`),
		toolCallMsg("c4", "fs_read", `{"path":"big.go","offset":300}`),
		schema.AssistantMessage("read the whole file", nil),
	}
	o, err := New(Config{
		Model:     einollm.NewFakeModelWithMessages(msgs, nil),
		Tools:     []BaseTool{ct},
		Profile:   allowAll(),
		MaxIters:  20,
		LoopGuard: LoopGuardConfig{RepetitionEnabled: true},
	})
	require.NoError(t, err)

	text, errText := drainFrames(o.Events(context.Background(), "read big.go"))
	assert.Empty(t, errText, "sequential offsets are progress, not repetition")
	assert.Equal(t, "read the whole file", text)
	assert.Equal(t, 4, ct.calls)
}

// --- L2: tool-call budget ---

// TestLoopGuardE2E_ToolBudgetReturnsResultNotError is the load-bearing
// assertion for L2: an over-budget call must come back as a tool RESULT the
// model can read and adapt to, never as a Go error (which the ADK turns into a
// NodeRunError and which tears down the whole turn). Same rule as
// UnknownToolsHandler.
func TestLoopGuardE2E_ToolBudgetReturnsResultNotError(t *testing.T) {
	ct := &countingTool{name: "shell_run"}
	msgs := []*schema.Message{
		toolCallMsg("c1", "shell_run", `{"q":"1"}`),
		toolCallMsg("c2", "shell_run", `{"q":"2"}`),
		toolCallMsg("c3", "shell_run", `{"q":"3"}`),
		schema.AssistantMessage("gave up on shell", nil),
	}
	o, err := New(Config{
		Model:     einollm.NewFakeModelWithMessages(msgs, nil),
		Tools:     []BaseTool{ct},
		Profile:   allowAll(),
		MaxIters:  20,
		LoopGuard: LoopGuardConfig{PerToolCalls: map[string]int{"shell_run": 2}},
	})
	require.NoError(t, err)

	var results []string
	ClassifyEvents(o.Events(context.Background(), "run things"), func(f proto.ServerFrame) {
		if f.Type == "error" {
			t.Fatalf("budget refusal must not surface as a turn error: %s", f.Text)
		}
		if f.Type == "tool_result" {
			results = append(results, f.Text)
		}
	})

	assert.Equal(t, 2, ct.calls, "the tool must run exactly its budget")
	require.GreaterOrEqual(t, len(results), 3)
	assert.Contains(t, results[2], "per-turn budget",
		"the 3rd call's RESULT must explain the budget so the model can adapt")
}

func TestLoopGuardE2E_TotalToolBudget(t *testing.T) {
	a := &countingTool{name: "tool_a"}
	b := &countingTool{name: "tool_b"}
	msgs := []*schema.Message{
		toolCallMsg("c1", "tool_a", `{"q":"1"}`),
		toolCallMsg("c2", "tool_b", `{"q":"2"}`),
		toolCallMsg("c3", "tool_a", `{"q":"3"}`),
		schema.AssistantMessage("done", nil),
	}
	o, err := New(Config{
		Model:     einollm.NewFakeModelWithMessages(msgs, nil),
		Tools:     []BaseTool{a, b},
		Profile:   allowAll(),
		MaxIters:  20,
		LoopGuard: LoopGuardConfig{MaxToolCalls: 2},
	})
	require.NoError(t, err)
	_, errText := drainFrames(o.Events(context.Background(), "go"))
	assert.Empty(t, errText)
	assert.Equal(t, 2, a.calls+b.calls, "the total cap spans all tools")
}

func TestLoopGuardE2E_NoBudgetMeansNoLimit(t *testing.T) {
	ct := &countingTool{name: "shell_run"}
	msgs := []*schema.Message{
		toolCallMsg("c1", "shell_run", `{"q":"1"}`),
		toolCallMsg("c2", "shell_run", `{"q":"2"}`),
		toolCallMsg("c3", "shell_run", `{"q":"3"}`),
		schema.AssistantMessage("done", nil),
	}
	o, err := New(Config{
		Model:    einollm.NewFakeModelWithMessages(msgs, nil),
		Tools:    []BaseTool{ct},
		Profile:  allowAll(),
		MaxIters: 20,
	})
	require.NoError(t, err)
	drainFrames(o.Events(context.Background(), "go"))
	assert.Equal(t, 3, ct.calls, "an unconfigured guard must not throttle anything")
}

// --- L3: deadline ---

// TestLoopGuardDeadlineStopsAtBoundary drives the middleware directly with an
// injected clock, because the point of L3 is WHERE the check happens, and a
// test that sleeps would prove only that time passes.
func TestLoopGuardDeadlineStopsAtBoundary(t *testing.T) {
	start := time.Now()
	clock := start
	handler, _ := LoopGuardConfig{TurnTimeout: time.Minute}.buildHandler()
	g := &turnGuard{
		handler: handler,
		started: start,
		now:     func() time.Time { return clock },
		nudges:  map[string]int{},
	}
	assert.Equal(t, loopguard.ActionContinue, g.observe(nil).Action)

	clock = start.Add(2 * time.Minute)
	res := g.observe(nil)
	require.Equal(t, loopguard.ActionStop, res.Action)
	assert.Equal(t, "deadline", res.Gate)

	err := loopguard.NewStopError(res)
	var se *loopguard.StopError
	require.True(t, errors.As(err, &se))
	assert.Contains(t, se.Reason, "boundary",
		"the reason must explain why the deadline is soft, or a late turn reads as a bug")
}

// TestLoopGuardE2E_DeadlineLeavesHistoryPairable is the reason L3 checks at an
// iteration boundary instead of using a context deadline: a mid-flight cut can
// leave an assistant tool_call with no matching tool_result, which every
// provider rejects on the next request.
func TestLoopGuardE2E_DeadlineLeavesHistoryPairable(t *testing.T) {
	ct := &countingTool{name: "fs_read"}
	o, err := New(Config{
		Model:     repeatToolModel("fs_read", `{"path":"a.go"}`),
		Tools:     []BaseTool{ct},
		Profile:   allowAll(),
		MaxIters:  50,
		LoopGuard: LoopGuardConfig{TurnTimeout: time.Nanosecond},
	})
	require.NoError(t, err)

	ctx := WithNewTurnRecorder(context.Background())
	_, errText := drainFrames(o.EventsWithHistoryOpts(ctx, []*schema.Message{schema.UserMessage("go")}, TurnOpts{}))
	require.Contains(t, errText, "time limit")

	rec, ok := ctx.Value(recorderKey{}).(*turnRecorder)
	require.True(t, ok)
	assertToolCallsArePaired(t, rec.load())
}

// assertToolCallsArePaired verifies every assistant tool_call in msgs has a
// following tool message carrying its id.
func assertToolCallsArePaired(t *testing.T, msgs []*schema.Message) {
	t.Helper()
	answered := map[string]bool{}
	for _, m := range msgs {
		if m != nil && m.Role == schema.Tool && m.ToolCallID != "" {
			answered[m.ToolCallID] = true
		}
	}
	for _, m := range msgs {
		if m == nil {
			continue
		}
		for _, tc := range m.ToolCalls {
			assert.True(t, answered[tc.ID],
				"tool_call %q (%s) has no tool_result; the history is unusable for the next request",
				tc.ID, tc.Function.Name)
		}
	}
}

// --- L4: token budget ---

func TestLoopGuardE2E_TokenBudgetStopsTurn(t *testing.T) {
	fm := repeatToolModel("fs_read", `{"path":"a.go"}`)
	// Distinct args each iteration would be needed to avoid the repetition
	// gate, but it is not installed here — this turn stops on tokens alone.
	fm.Usage = &schema.TokenUsage{PromptTokens: 400, CompletionTokens: 60}
	ct := &countingTool{name: "fs_read"}
	o, err := New(Config{
		Model:     fm,
		Tools:     []BaseTool{ct},
		Profile:   allowAll(),
		MaxIters:  50,
		LoopGuard: LoopGuardConfig{MaxTurnTokens: 700},
	})
	require.NoError(t, err)

	_, errText := drainFrames(o.Events(context.Background(), "go"))
	require.NotEmpty(t, errText)
	assert.Contains(t, errText, "token budget")
	assert.Contains(t, errText, "700", "the stop must state the limit that was hit")
	assert.Less(t, ct.calls, 50, "the budget must stop the turn before MaxIters")
}

// TestLoopGuardE2E_TokenBudgetNotTrippedByCumulativePrompts is the assertion
// that fails if the accumulator is changed to sum raw prompt counts. Providers
// resend the whole prefix on every call, so summing over-counts a long turn and
// the budget would fire at a fraction of its nominal value.
func TestLoopGuardE2E_TokenBudgetNotTrippedByCumulativePrompts(t *testing.T) {
	fm := einollm.NewFakeModelWithMessages([]*schema.Message{
		toolCallMsg("c1", "fs_read", `{"q":"1"}`),
		toolCallMsg("c2", "fs_read", `{"q":"2"}`),
		toolCallMsg("c3", "fs_read", `{"q":"3"}`),
		schema.AssistantMessage("done", nil),
	}, nil)
	// A steady 1000-token prompt with 10 tokens out. Real spend over 4 calls is
	// ~1040. Naive summing would report ~4040 and trip a 2000 budget.
	fm.Usage = &schema.TokenUsage{PromptTokens: 1000, CompletionTokens: 10}
	ct := &countingTool{name: "fs_read"}
	o, err := New(Config{
		Model:     fm,
		Tools:     []BaseTool{ct},
		Profile:   allowAll(),
		MaxIters:  20,
		LoopGuard: LoopGuardConfig{MaxTurnTokens: 2000},
	})
	require.NoError(t, err)

	text, errText := drainFrames(o.Events(context.Background(), "go"))
	assert.Empty(t, errText, "a turn well under budget must not be stopped")
	assert.Equal(t, "done", text)
	assert.Equal(t, 3, ct.calls)
}

// --- Nudge injection ---

// TestLoopGuardE2E_NudgeReachesTheModel proves the modify_prompt path actually
// puts a message in front of the model. A nudge nobody reads is the repo's
// most common defect shape.
func TestLoopGuardE2E_NudgeReachesTheModel(t *testing.T) {
	fm := repeatToolModel("fs_read", `{"path":"a.go"}`)
	fm.RecordMessages = true
	ct := &countingTool{name: "fs_read"}
	o, err := New(Config{
		Model:     fm,
		Tools:     []BaseTool{ct},
		Profile:   allowAll(),
		MaxIters:  50,
		LoopGuard: LoopGuardConfig{RepetitionEnabled: true},
	})
	require.NoError(t, err)
	drainFrames(o.Events(context.Background(), "go"))

	var sawNudge bool
	for _, m := range fm.ReceivedMessages {
		if m != nil && m.Role == schema.User && strings.Contains(m.Content, "Repetitive pattern detected") {
			sawNudge = true
		}
	}
	assert.True(t, sawNudge, "the warning must reach the model's input, not just the logs")
}

// --- S8: unregistered tools are refused without a dialog ---

// TestS8_RegisteredSetIsBoundPerTurn proves the orchestrator publishes the set
// of names that actually exist. Without a live binding the runtime check is
// dead code and only the compile-time gates (GOV5/GOV7) remain.
func TestS8_RegisteredSetIsBoundPerTurn(t *testing.T) {
	ct := &countingTool{name: "fs_read"}
	o, err := New(Config{
		Model:   einollm.NewFakeModel([]string{"hi"}, nil),
		Tools:   []BaseTool{ct},
		Profile: allowAll(),
	})
	require.NoError(t, err)

	ctx := o.withTurnContext(context.Background(), TurnOpts{})
	set, bound := toolreg.FromContext(ctx)
	require.True(t, bound, "every turn must publish its registered tool names")
	assert.Equal(t, []string{"fs_read"}, set.Names())

	require.NoError(t, toolreg.Check(ctx, "fs_read"))
	require.Error(t, toolreg.Check(ctx, "fs_mkdir"), "a phantom name must be refused")
	require.Error(t, toolreg.Check(ctx, ""), "an empty tool name must be refused")
}

// TestWithTurnContextBindsNewWindowSignal is W-C-14's production-call-site
// pin (GOV6): withTurnContext must bind einollm.WithNewWindowSignal on every
// turn, unconditionally — even here, where the orchestrator's Model is a
// bare FakeModel never wrapped in a CompactingModel — because the tool
// handler's own success/failure branch (internal/tools/contextwindow.go's
// run()) is the thing that would silently start reporting failure on every
// call if this bind were ever dropped or made conditional.
func TestWithTurnContextBindsNewWindowSignal(t *testing.T) {
	o, err := New(Config{
		Model:   einollm.NewFakeModel([]string{"hi"}, nil),
		Tools:   []BaseTool{&countingTool{name: "fs_read"}},
		Profile: allowAll(),
	})
	require.NoError(t, err)

	ctx := o.withTurnContext(context.Background(), TurnOpts{})
	assert.True(t, einollm.RequestNewWindow(ctx, "reason"),
		"withTurnContext must bind a new-window signal every turn publishes into")
}

// TestWithTurnContextBindsContextBudgetSignal is W-C-11's production-call-site
// pin (GOV6), the read-direction mirror of
// TestWithTurnContextBindsNewWindowSignal above: withTurnContext must bind
// einollm.WithContextBudgetSignal every turn, and a real CompactingModel
// driven through the resulting ctx (exactly how runnerFor drives the turn's
// model) must be able to publish into it.
//
// ContextBudgetFromContext alone cannot distinguish "signal bound but nothing
// published yet" from "no signal bound at all" — both read (zero, false) by
// design, since the context_budget tool does not need to tell those apart.
// So the only way to prove withTurnContext's bind is real, rather than a
// no-op that happens to look identical from the read side, is to actually
// publish through it: construct a CompactingModel and call Generate on the
// SAME ctx withTurnContext returned, then confirm the snapshot it left behind
// is readable and matches ctxcompact.RemainingBudget computed independently.
func TestWithTurnContextBindsContextBudgetSignal(t *testing.T) {
	o, err := New(Config{
		Model:   einollm.NewFakeModel([]string{"hi"}, nil),
		Tools:   []BaseTool{&countingTool{name: "fs_read"}},
		Profile: allowAll(),
	})
	require.NoError(t, err)

	ctx := o.withTurnContext(context.Background(), TurnOpts{})

	inner := einollm.NewFakeModel([]string{"reply"}, nil)
	cm := &einollm.CompactingModel{
		Inner:         inner,
		Threshold:     0.99, // high enough that this short history never triggers actual compaction
		ContextWindow: 100000,
		KeepRecent:    4,
	}
	msgs := []*schema.Message{{Role: schema.User, Content: "hello"}}

	_, err = cm.Generate(ctx, msgs)
	require.NoError(t, err)

	got, ok := einollm.ContextBudgetFromContext(ctx)
	require.True(t, ok, "withTurnContext's bind must let a CompactingModel driven through this ctx publish a snapshot")
	assert.Equal(t, 100000, got.Window)
	wantRemaining := ctxcompact.RemainingBudget(msgs, ctxcompact.RunOpts{ModelWindow: 100000})
	assert.Equal(t, wantRemaining, got.Remaining)
}

// TestS8_HeadlessContextAlsoBinds: `yanshi pr` and the goal loop reach tools
// through BindHeadlessContext, which must carry the same protection as a turn.
func TestS8_HeadlessContextAlsoBinds(t *testing.T) {
	o, err := New(Config{
		Model:   einollm.NewFakeModel([]string{"hi"}, nil),
		Tools:   []BaseTool{&countingTool{name: "review"}},
		Profile: allowAll(),
	})
	require.NoError(t, err)
	ctx := o.BindHeadlessContext(context.Background())
	require.NoError(t, toolreg.Check(ctx, "review"))
	require.Error(t, toolreg.Check(ctx, "shell_run"))
}

// TestS8_SubAgentGetsNarrowerSet: a sub-agent runs with a filtered tool subset
// and must authorize against THAT, not the parent's wider surface.
func TestS8_SubAgentGetsNarrowerSet(t *testing.T) {
	parent, err := New(Config{
		Model:   einollm.NewFakeModel([]string{"hi"}, nil),
		Tools:   []BaseTool{&countingTool{name: "fs_read"}, &countingTool{name: "shell_run"}},
		Profile: allowAll(),
	})
	require.NoError(t, err)

	sub, err := New(Config{
		Model:   einollm.NewFakeModel([]string{"hi"}, nil),
		Tools:   parent.selectSubAgentTools([]string{"fs_read"}),
		Profile: allowAll(),
	})
	require.NoError(t, err)

	subCtx := sub.withTurnContext(context.Background(), TurnOpts{})
	require.NoError(t, toolreg.Check(subCtx, "fs_read"))
	require.Error(t, toolreg.Check(subCtx, "shell_run"),
		"delegation must not widen the set a sub-agent may authorize against")
}

// TestS8_ProfileWouldHavePrompted is the whole point of S8 stated as a test:
// guard answers an unknown tool name with Prompt (an interactive user may
// legitimately approve a tool the profile has not seen), so without this layer
// a phantom name reaches the user as a dialog with an Allow button.
//
// It also pins the premise. If guard ever starts hard-denying unknown names on
// its own, S8 becomes redundant and this test says so by failing, rather than
// leaving a second gate nobody remembers the reason for.
func TestS8_ProfileWouldHavePrompted(t *testing.T) {
	prof := guard.PermissionProfile{Tools: guard.ToolsPerm{Allow: []string{"fs_read"}}}
	dec := guard.New().Check(prof, guard.Action{Tool: "fs_mkdir"})
	require.Equal(t, guard.Prompt, dec.Verdict,
		"if guard ever hard-denies unknown names on its own, S8's rationale needs rewriting")
	require.True(t, dec.Promptable, "...and it is promptable, i.e. a clickable dialog")

	// With the set bound, the name never gets that far.
	ctx := toolreg.WithRegistered(context.Background(), []string{"fs_read"})
	require.Error(t, toolreg.Check(ctx, "fs_mkdir"))
}

// TestS8_ProducerHalfIsLiveButConsumerIsElsewhere states the exact boundary of
// what this package can prove about S8, so nobody reads the green suite as
// "S8 is enforced".
//
// This package owns the PRODUCER half: every turn context carries the set of
// names that actually exist. That is inert on its own. The ENFORCING half is
// one call to toolreg.Check at the top of internal/tools.Authorize, and until
// that call exists the refusal below is the only thing happening — the real
// authorization path still hands a phantom name to guard, which answers Prompt
// and offers the user an Allow button.
//
// The check is deliberately written as an explicit statement of that split
// rather than as a behavioural assertion this package cannot honestly make.
func TestS8_ProducerHalfIsLiveButConsumerIsElsewhere(t *testing.T) {
	ct := &countingTool{name: "fs_read"}
	o, err := New(Config{
		Model: einollm.NewFakeModelWithMessages([]*schema.Message{
			toolCallMsg("c1", "fs_read", `{"q":"1"}`),
			schema.AssistantMessage("done", nil),
		}, nil),
		Tools:    []BaseTool{ct},
		Profile:  allowAll(),
		MaxIters: 10,
	})
	require.NoError(t, err)

	// Producer: a registered tool still runs, and the turn context publishes
	// the set an enforcer would consult.
	drainFrames(o.Events(context.Background(), "go"))
	require.Equal(t, 1, ct.calls)

	ctx := o.withTurnContext(context.Background(), TurnOpts{})
	err = toolreg.Check(ctx, "fs_mkdir")
	require.Error(t, err, "the published set must be able to refuse a phantom name")
	var ue *toolreg.UnregisteredError
	require.True(t, errors.As(err, &ue))
	require.Equal(t, "fs_mkdir", ue.Tool)

	// Consumer: guard on its own would PROMPT for the same name. This is the
	// gap the tools-side patch closes; asserting it here keeps the reason for
	// that patch attached to a test rather than to a commit message.
	dec := guard.New().Check(allowAll(), guard.Action{Tool: "fs_mkdir"})
	require.True(t, dec.IsAllowed() || dec.Promptable,
		"under Tools.Allow=[\"*\"] guard never refuses an unknown name by itself")
}

// TestLoopGuardE2E_DeadlineStopsOnTheRealClock complements
// TestLoopGuardDeadlineStopsAtBoundary, which injects a clock.
//
// The injected-clock test is the right way to pin WHERE the check happens, but
// it cannot notice a guard wired to a clock nobody advances in production:
// turnGuard.now is set from time.Now by WithLoopGuard, and an edit that left a
// test seam in that position would keep every clock-injected test green while
// no real turn ever timed out.
//
// So this one uses a real, short deadline and real elapsed time. The tool
// sleeps so the loop genuinely crosses the boundary rather than racing it, and
// the assertions are that the turn stopped, said why, and stopped well before
// MaxIters.
func TestLoopGuardE2E_DeadlineStopsOnTheRealClock(t *testing.T) {
	slow := &sleepingTool{name: "fs_read", each: 60 * time.Millisecond}
	o, err := New(Config{
		Model:     repeatToolModel("fs_read", `{"path":"a.go"}`),
		Tools:     []BaseTool{slow},
		Profile:   allowAll(),
		MaxIters:  200,
		LoopGuard: LoopGuardConfig{TurnTimeout: 250 * time.Millisecond},
	})
	require.NoError(t, err)

	start := time.Now()
	_, errText := drainFrames(o.Events(context.Background(), "go"))
	elapsed := time.Since(start)

	require.NotEmpty(t, errText, "a turn past its wall-clock limit must end with an error frame")
	assert.Contains(t, errText, "time limit", "the stop must name the reason")
	assert.Less(t, slow.calls, 200, "the deadline must stop the turn before MaxIters")
	// Generous ceiling: the check is at iteration boundaries, so the overshoot
	// is bounded by one tool call, not by zero.
	assert.Less(t, elapsed, 10*time.Second,
		"the turn ran %v against a 250ms limit; the deadline is not being enforced "+
			"on the real clock", elapsed)
}

// sleepingTool is a real InvokableTool that takes measurable time, so a
// wall-clock deadline has something to expire against.
type sleepingTool struct {
	name  string
	each  time.Duration
	calls int
}

func (s *sleepingTool) Info(context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{
		Name: s.name,
		Desc: "a tool that takes time",
		ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
			"path": {Type: schema.String, Desc: "anything"},
		}),
	}, nil
}

func (s *sleepingTool) InvokableRun(_ context.Context, _ string, _ ...tool.Option) (string, error) {
	s.calls++
	time.Sleep(s.each)
	return "ok", nil
}
