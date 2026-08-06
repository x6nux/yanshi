package goalloop

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// CounterEvaluator fails until its call count reaches passAt, then passes.
type CounterEvaluator struct {
	passAt int
	mu     sync.Mutex
	calls  int
}

func (e *CounterEvaluator) Evaluate(_ context.Context, _ Goal, _ Plan, _ string) (EvalVerdict, error) {
	e.mu.Lock()
	e.calls++
	n := e.calls
	e.mu.Unlock()

	if n >= e.passAt {
		return EvalVerdict{
			Evaluator: "counter",
			Pass:      true,
			Evidence:  fmt.Sprintf("call %d >= passAt %d", n, e.passAt),
		}, nil
	}
	return EvalVerdict{
		Evaluator: "counter",
		Pass:      false,
		Evidence:  fmt.Sprintf("call %d < passAt %d", n, e.passAt),
		Gaps:      []string{fmt.Sprintf("counter: call %d < passAt %d", n, e.passAt)},
	}, nil
}

func (e *CounterEvaluator) Calls() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.calls
}

// ---------------------------------------------------------------------------
// Integration test: fail-once-then-pass (RED → GREEN)
// ---------------------------------------------------------------------------

func TestLoop_FailOnceThenPass(t *testing.T) {
	t.Parallel()

	planner := FakePlanner{
		Tests: []AcceptanceTest{{Name: "t", Command: "true"}},
		Steps: []string{"s1"},
	}
	impl := &FakeImplementer{Result: "done", FailFirst: 1}
	eval := &CounterEvaluator{passAt: 2}
	judge := AggregateJudge{}

	loop := New(Config{
		Planner:     planner,
		Implementer: impl,
		Evaluators:  []Evaluator{eval},
		Judge:       judge,
		Budget:      Budget{MaxIterations: 5},
	})

	var events []Event
	decision, err := loop.Run(
		context.Background(),
		Goal{Text: "implement foo", Workdir: ""},
		func(e Event) { events = append(events, e) },
	)

	require.NoError(t, err)
	assert.True(t, decision.Complete, "decision should be complete")
	assert.Equal(t, 2, loop.Iterations(), "should take exactly 2 iterations")

	// Event sequence:
	// Iteration 1: Plan, Implement(error), Evaluate, Judge(gaps)
	// Iteration 2: Plan, Implement, Evaluate, Judge(complete), Done
	require.Len(t, events, 9, "expected 4 events for iter 1 + 5 for iter 2")

	// --- Iteration 1 (fails) ---
	assert.Equal(t, "Plan", events[0].Phase)
	assert.Equal(t, 1, events[0].Iteration)

	assert.Equal(t, "Implement", events[1].Phase)
	assert.Equal(t, 1, events[1].Iteration)
	assert.Contains(t, events[1].Detail, "error", "iter 1 implement should have error")

	assert.Equal(t, "Evaluate", events[2].Phase)
	assert.Equal(t, 1, events[2].Iteration)

	assert.Equal(t, "Judge", events[3].Phase)
	assert.Equal(t, 1, events[3].Iteration)
	assert.Contains(t, events[3].Detail, "gaps", "iter 1 judge should have gaps")

	// --- Iteration 2 (succeeds) ---
	assert.Equal(t, "Plan", events[4].Phase)
	assert.Equal(t, 2, events[4].Iteration)

	assert.Equal(t, "Implement", events[5].Phase)
	assert.Equal(t, 2, events[5].Iteration)
	assert.NotContains(t, events[5].Detail, "error", "iter 2 implement should succeed")

	assert.Equal(t, "Evaluate", events[6].Phase)
	assert.Equal(t, 2, events[6].Iteration)

	assert.Equal(t, "Judge", events[7].Phase)
	assert.Equal(t, 2, events[7].Iteration)

	assert.Equal(t, "Done", events[8].Phase)
	assert.Equal(t, 2, events[8].Iteration)
}

// ---------------------------------------------------------------------------
// Context cancellation
// ---------------------------------------------------------------------------

func TestLoop_ContextCancellation(t *testing.T) {
	t.Parallel()

	planner := FakePlanner{Steps: []string{"s1"}}
	impl := &FakeImplementer{Result: "done"}
	eval := &CounterEvaluator{passAt: 100} // never passes
	judge := AggregateJudge{}

	loop := New(Config{
		Planner:     planner,
		Implementer: impl,
		Evaluators:  []Evaluator{eval},
		Judge:       judge,
		Budget:      Budget{MaxIterations: 100},
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before running

	_, err := loop.Run(ctx, Goal{Text: "x"}, func(Event) {})
	require.Error(t, err)
	assert.True(t, errors.Is(err, context.Canceled))
}

// ---------------------------------------------------------------------------
// Max iterations exhausted
// ---------------------------------------------------------------------------

func TestLoop_MaxIterationsExhausted(t *testing.T) {
	t.Parallel()

	planner := FakePlanner{Steps: []string{"s1"}}
	impl := &FakeImplementer{Result: "done"}
	eval := &CounterEvaluator{passAt: 100} // never passes
	judge := AggregateJudge{}

	loop := New(Config{
		Planner:     planner,
		Implementer: impl,
		Evaluators:  []Evaluator{eval},
		Judge:       judge,
		Budget:      Budget{MaxIterations: 3},
	})

	decision, err := loop.Run(context.Background(), Goal{Text: "x"}, func(Event) {})
	require.NoError(t, err)
	assert.False(t, decision.Complete, "should not be complete")
	assert.Equal(t, 3, loop.Iterations())
	assert.Contains(t, decision.Summary, "max iterations")
}

// ---------------------------------------------------------------------------
// Budget exceeded (token budget)
// ---------------------------------------------------------------------------

func TestLoop_BudgetExceeded(t *testing.T) {
	t.Parallel()

	planner := FakePlanner{Steps: []string{"s1"}}
	impl := &FakeImplementer{Result: "done"}
	eval := &CounterEvaluator{passAt: 100} // never passes
	judge := AggregateJudge{}

	loop := New(Config{
		Planner:     planner,
		Implementer: impl,
		Evaluators:  []Evaluator{eval},
		Judge:       judge,
		Budget:      Budget{MaxIterations: 10, MaxTokens: 100},
		// Pre-loaded sink: the spend has to arrive the way production delivers
		// it, not through a field only tests could set.
		Sink: sinkWith(Usage{TotalTokens: 200}),
	})

	decision, err := loop.Run(context.Background(), Goal{Text: "x"}, func(Event) {})
	require.NoError(t, err)
	assert.False(t, decision.Complete)
	assert.Contains(t, decision.Summary, "budget exceeded")
}

// ---------------------------------------------------------------------------
// Nil onEvent callback (should not panic)
// ---------------------------------------------------------------------------

func TestLoop_NilOnEvent(t *testing.T) {
	t.Parallel()

	planner := FakePlanner{Steps: []string{"s1"}}
	impl := &FakeImplementer{Result: "done"}
	eval := &CounterEvaluator{passAt: 1} // passes immediately
	judge := AggregateJudge{}

	loop := New(Config{
		Planner:     planner,
		Implementer: impl,
		Evaluators:  []Evaluator{eval},
		Judge:       judge,
		Budget:      Budget{MaxIterations: 5},
	})

	decision, err := loop.Run(context.Background(), Goal{Text: "x"}, nil)
	require.NoError(t, err)
	assert.True(t, decision.Complete)
	assert.Equal(t, 1, loop.Iterations())
}

// ---------------------------------------------------------------------------
// No vacuous completion: a plan with no acceptance tests must not pass.
// ---------------------------------------------------------------------------

// TestLoop_NoVacuousCompletion verifies that a Loop using TestEvaluator with
// a plan that has no tests cannot declare Complete. The TestEvaluator
// fail-closes on empty tests (Pass:false), so AggregateJudge cannot pass.
// This prevents the "empty plan" vacuous-false-positive described in review #11.
func TestLoop_NoVacuousCompletion(t *testing.T) {
	t.Parallel()

	// FakePlanner returns a plan with steps but NO tests.
	planner := FakePlanner{Steps: []string{"s1"}}
	impl := &FakeImplementer{Result: "done"}
	eval := TestEvaluator{} // fail-closed on no tests
	judge := AggregateJudge{}

	loop := New(Config{
		Planner:     planner,
		Implementer: impl,
		Evaluators:  []Evaluator{eval},
		Judge:       judge,
		Budget:      Budget{MaxIterations: 2},
	})

	decision, err := loop.Run(context.Background(), Goal{Text: "x"}, func(Event) {})
	require.NoError(t, err)
	assert.False(t, decision.Complete, "loop must not declare complete with no acceptance tests")
	assert.Contains(t, decision.Summary, "max iterations")
}

// ---------------------------------------------------------------------------
// Sink-driven token budget (G02) + escalation hint (G03)
// ---------------------------------------------------------------------------

// TestLoop_BudgetStopsOnAccumulatedUsage proves the budget stops the loop on
// spend that arrived through the shared sink — the same sink ACP subprocess
// usage is forwarded into, which is what makes subprocess tokens count against
// the operator's limit rather than escaping it.
//
// ledger: F2/LEAK3#2 budget 含子进程
//
// ledger: M1/G02#2 预算耗尽可靠停止并把原因持久化
func TestLoop_BudgetStopsOnAccumulatedUsage(t *testing.T) {
	t.Parallel()
	// Simulate two evaluators each adding usage to a shared sink, then verify
	// the loop stops BEFORE the next iteration once the sink crosses MaxTokens.
	// We pre-charge the sink past the budget so the check at the top of iter 2
	// fires (the loop checks before planning each iteration).
	sink := &UsageSink{}
	sink.Add(Usage{TotalTokens: 150}) // already over a 100-token budget

	planner := FakePlanner{Steps: []string{"s1"}}
	impl := &FakeImplementer{Result: "done"}
	loop := New(Config{
		Planner:     planner,
		Implementer: impl,
		Evaluators:  []Evaluator{&CounterEvaluator{passAt: 1}},
		Judge:       AggregateJudge{},
		Budget:      Budget{MaxIterations: 5, MaxTokens: 100},
		Sink:        sink,
	})

	decision, err := loop.Run(context.Background(), Goal{Text: "x"}, func(Event) {})
	require.NoError(t, err)
	assert.False(t, decision.Complete)
	assert.Equal(t, StopReasonTokenBudget, decision.StopReason, "must record WHY it stopped")
	assert.Contains(t, decision.Summary, "budget exceeded")
	assert.Greater(t, decision.Usage.Total(), 0, "Decision carries the usage snapshot")
}

func TestLoop_BudgetCheckMidIteration_AfterPlan(t *testing.T) {
	t.Parallel()
	// A planner that charges the sink on Plan, crossing the budget mid-turn.
	// The loop must stop right after Plan (before Implement), not run a full
	// expensive iteration.
	sink := &UsageSink{}
	planner := &chargingPlanner{sink: sink, perCall: Usage{TotalTokens: 200}}
	impl := &FakeImplementer{Result: "done"}

	loop := New(Config{
		Planner:     planner,
		Implementer: impl,
		Evaluators:  []Evaluator{&CounterEvaluator{passAt: 1}},
		Judge:       AggregateJudge{},
		Budget:      Budget{MaxIterations: 5, MaxTokens: 100},
		Sink:        sink,
	})
	decision, err := loop.Run(context.Background(), Goal{Text: "x"}, func(Event) {})
	require.NoError(t, err)
	assert.Equal(t, StopReasonTokenBudget, decision.StopReason)
	assert.Equal(t, 0, impl.Calls(), "must not Implement once the plan blew the budget")
}

// chargingPlanner is a Planner that adds perCall to sink on every Plan call.
type chargingPlanner struct {
	sink    *UsageSink
	perCall Usage
}

func (p *chargingPlanner) Plan(context.Context, Goal) (Plan, error) {
	p.sink.Add(p.perCall)
	return Plan{Goal: "x", Steps: []string{"s1"}, Tests: []AcceptanceTest{{Name: "t", Command: "true"}}}, nil
}

// ledger: M1/G03#3 升级不再静默退出
func TestLoop_MaxIterationsHasEscalationHint(t *testing.T) {
	t.Parallel()
	// A low tier that never passes must surface an escalation hint (not exit
	// silently) and tag StopReason=escalate.
	planner := FakePlanner{Steps: []string{"s1"}}
	impl := &FakeImplementer{Result: "done"}
	loop := New(Config{
		Planner:     planner,
		Implementer: impl,
		Evaluators:  []Evaluator{&CounterEvaluator{passAt: 100}},
		Judge:       AggregateJudge{},
		Budget:      Budget{MaxIterations: 2},
		Tier:        TierQuickFix,
	})
	decision, err := loop.Run(context.Background(), Goal{Text: "x"}, func(Event) {})
	require.NoError(t, err)
	assert.False(t, decision.Complete)
	assert.Contains(t, decision.Summary, "max iterations", "keep the existing substring")
	assert.Contains(t, decision.Summary, "escalat", "append the escalation hint")
	assert.Equal(t, StopReasonEscalate, decision.StopReason)
}

// ledger: M1/G03#3 升级不再静默退出
func TestLoop_T4MaxIterationsNoEscalationHint(t *testing.T) {
	t.Parallel()
	// T4 is the top tier: exhaustion reports max-iterations without an upgrade
	// hint, and StopReason stays max_iterations (not escalate).
	planner := FakePlanner{Steps: []string{"s1"}}
	impl := &FakeImplementer{Result: "done"}
	loop := New(Config{
		Planner:     planner,
		Implementer: impl,
		Evaluators:  []Evaluator{&CounterEvaluator{passAt: 100}},
		Judge:       AggregateJudge{},
		Budget:      Budget{MaxIterations: 1},
		Tier:        TierAutonomous,
	})
	decision, err := loop.Run(context.Background(), Goal{Text: "x"}, func(Event) {})
	require.NoError(t, err)
	assert.NotContains(t, decision.Summary, "escalat")
	assert.Equal(t, StopReasonMaxIters, decision.StopReason)
}

// sinkWith returns a UsageSink pre-loaded with u, for tests that need a spend
// figure already on the books before the loop starts.
func sinkWith(u Usage) *UsageSink {
	s := &UsageSink{}
	s.Add(u)
	return s
}
