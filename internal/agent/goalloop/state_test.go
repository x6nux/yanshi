package goalloop

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/store"
)

// cancellingImplementer cancels the run's context once it has been called
// after times, standing in for the interruption W-D-16 is about (Ctrl-C, a
// crash, a machine reboot) at a point where the loop is mid-goal.
type cancellingImplementer struct {
	after  int
	cancel context.CancelFunc
	calls  int
}

func (i *cancellingImplementer) Implement(context.Context, Plan, string) (string, error) {
	i.calls++
	if i.calls >= i.after {
		i.cancel()
	}
	return "done", nil
}

// openStore opens a real SQLite store at path. Each call is a separate handle:
// the resume tests depend on that, because a single handle would let the
// assertions pass on nothing more durable than an in-process map.
func openStore(t *testing.T, path string) *store.Store {
	t.Helper()
	st, err := store.Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// firstIteration returns an onEvent callback recording the iteration number of
// the first non-"State" event, i.e. where the loop actually resumed work.
func firstIteration(out *int) func(Event) {
	*out = 0
	return func(e Event) {
		if e.Phase != "State" && *out == 0 {
			*out = e.Iteration
		}
	}
}

// TestGoalLoop_ResumesAfterRestart is the acceptance for W-D-16: the
// objective and BOTH budgets live in SQLite, and a restarted process picks the
// run up where it stopped instead of replaying it from iteration 1 with a
// fresh budget.
//
// Both budgets are asserted separately because they fail differently. A reset
// iteration count is visible in the terminal — the run obviously starts over.
// A reset token count is not: the loop keeps going and simply costs twice as
// much, which is why the subtest below asserts the resumed run does no work at
// all rather than merely that a counter is right.
func TestGoalLoop_ResumesAfterRestart(t *testing.T) {
	t.Parallel()

	const (
		perIteration = 100
		maxIters     = 6
		stopAfter    = 3
	)

	t.Run("iteration budget is not reset", func(t *testing.T) {
		t.Parallel()
		dbPath := filepath.Join(t.TempDir(), "iters.db")
		goal := Goal{Text: "make the parser accept trailing commas", Workdir: "/repo/parser"}
		budget := Budget{MaxIterations: maxIters} // MaxTokens 0: token budget off

		// --- first process: interrupted after stopAfter iterations ---
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		sink1 := &UsageSink{}
		impl := &cancellingImplementer{after: stopAfter, cancel: cancel}
		loop1 := New(Config{
			Planner:     &chargingPlanner{sink: sink1, perCall: Usage{TotalTokens: perIteration}},
			Implementer: impl,
			Evaluators:  []Evaluator{&CounterEvaluator{passAt: 100}},
			Judge:       AggregateJudge{},
			Budget:      budget,
			Sink:        sink1,
			State:       openStore(t, dbPath),
		})
		_, err := loop1.Run(ctx, goal, nil)
		require.ErrorIs(t, err, context.Canceled, "fixture must stop mid-goal, not run to exhaustion")
		require.Equal(t, stopAfter, loop1.Iterations())

		// --- restart: a brand new Loop over a second handle on the same file ---
		sink2 := &UsageSink{}
		loop2 := New(Config{
			Planner:     &chargingPlanner{sink: sink2, perCall: Usage{TotalTokens: perIteration}},
			Implementer: &FakeImplementer{Result: "done"},
			Evaluators:  []Evaluator{&CounterEvaluator{passAt: 100}},
			Judge:       AggregateJudge{},
			Budget:      budget,
			Sink:        sink2,
			State:       openStore(t, dbPath),
		})
		var resumedAt int
		decision, err := loop2.Run(context.Background(), goal, firstIteration(&resumedAt))
		require.NoError(t, err)

		// chargingPlanner bills the sink once per iteration, so the spend added
		// after the restart is also the count of iterations it was allowed.
		assert.Equal(t, stopAfter+1, resumedAt, "must resume at N+1, not replay from 1")
		assert.Equal(t, (maxIters-stopAfter)*perIteration,
			sink2.Snapshot().Total()-stopAfter*perIteration,
			"only the iterations left in the budget may run")
		assert.Equal(t, maxIters, loop2.Iterations())
		assert.False(t, decision.Complete)
		assert.Equal(t, maxIters*perIteration, decision.Usage.Total(),
			"spend from before the restart must be carried forward, not dropped")
	})

	t.Run("token budget is not reset", func(t *testing.T) {
		t.Parallel()
		dbPath := filepath.Join(t.TempDir(), "tokens.db")
		goal := Goal{Text: "port the exporter to the new API", Workdir: "/repo/exporter"}
		// Tight enough that stopAfter iterations of planning overshoot it, so
		// the first process stops on tokens with iterations still to spare.
		budget := Budget{MaxIterations: maxIters, MaxTokens: stopAfter*perIteration - 50}

		sink1 := &UsageSink{}
		loop1 := New(Config{
			Planner:     &chargingPlanner{sink: sink1, perCall: Usage{TotalTokens: perIteration}},
			Implementer: &FakeImplementer{Result: "done"},
			Evaluators:  []Evaluator{&CounterEvaluator{passAt: 100}},
			Judge:       AggregateJudge{},
			Budget:      budget,
			Sink:        sink1,
			State:       openStore(t, dbPath),
		})
		decision1, err := loop1.Run(context.Background(), goal, nil)
		require.NoError(t, err)
		require.Equal(t, StopReasonTokenBudget, decision1.StopReason)
		require.Less(t, loop1.Iterations(), maxIters, "iterations must still be available")

		// The persisted row is the contract: objective plus BOTH budget fields.
		st2 := openStore(t, dbPath)
		blob, ok, err := st2.KVGet(goalStateKey(goal.Workdir))
		require.NoError(t, err)
		require.True(t, ok, "goal state must be in SQLite, readable by another handle")
		var persisted GoalState
		require.NoError(t, json.Unmarshal([]byte(blob), &persisted))
		assert.Equal(t, goal.Text, persisted.Objective)
		assert.Equal(t, budget.MaxIterations, persisted.Budget.MaxIterations)
		assert.Equal(t, budget.MaxTokens, persisted.Budget.MaxTokens)
		assert.Equal(t, decision1.Usage.Total(), persisted.Usage.Total())

		// --- restart ---
		sink2 := &UsageSink{}
		loop2 := New(Config{
			Planner:     &chargingPlanner{sink: sink2, perCall: Usage{TotalTokens: perIteration}},
			Implementer: &FakeImplementer{Result: "done"},
			Evaluators:  []Evaluator{&CounterEvaluator{passAt: 100}},
			Judge:       AggregateJudge{},
			Budget:      budget,
			Sink:        sink2,
			State:       st2,
		})
		decision2, err := loop2.Run(context.Background(), goal, nil)
		require.NoError(t, err)

		assert.Equal(t, StopReasonTokenBudget, decision2.StopReason,
			"the spent tokens must still be spent after a restart")
		assert.Equal(t, decision1.Usage.Total(), sink2.Snapshot().Total(),
			"an exhausted token budget must buy zero further iterations")
	})

	t.Run("a finished goal starts over", func(t *testing.T) {
		t.Parallel()
		dbPath := filepath.Join(t.TempDir(), "done.db")
		goal := Goal{Text: "add the flag", Workdir: "/repo/flag"}
		budget := Budget{MaxIterations: 2}

		run := func(st StateStore) (*Loop, Decision, int) {
			l := New(Config{
				Planner:     FakePlanner{Steps: []string{"s1"}},
				Implementer: &FakeImplementer{Result: "done"},
				Evaluators:  []Evaluator{&CounterEvaluator{passAt: 1}},
				Judge:       AggregateJudge{},
				Budget:      budget,
				Sink:        &UsageSink{},
				State:       st,
			})
			var at int
			d, err := l.Run(context.Background(), goal, firstIteration(&at))
			require.NoError(t, err)
			return l, d, at
		}

		_, d1, _ := run(openStore(t, dbPath))
		require.True(t, d1.Complete)

		// Without the Complete flag the second run would load Iterations == 1
		// and start at 2 — or, once the budget is used up, refuse to run at all.
		_, d2, at := run(openStore(t, dbPath))
		assert.True(t, d2.Complete)
		assert.Equal(t, 1, at, "a completed goal is not resumable; it starts fresh")
	})

	t.Run("a different objective in the same workdir starts over", func(t *testing.T) {
		t.Parallel()
		dbPath := filepath.Join(t.TempDir(), "other.db")
		workdir := "/repo/shared"
		budget := Budget{MaxIterations: 3}

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		loop1 := New(Config{
			Planner:     FakePlanner{Steps: []string{"s1"}},
			Implementer: &cancellingImplementer{after: 1, cancel: cancel},
			Evaluators:  []Evaluator{&CounterEvaluator{passAt: 100}},
			Judge:       AggregateJudge{},
			Budget:      budget,
			Sink:        &UsageSink{},
			State:       openStore(t, dbPath),
		})
		_, err := loop1.Run(ctx, Goal{Text: "first goal", Workdir: workdir}, nil)
		require.ErrorIs(t, err, context.Canceled)

		loop2 := New(Config{
			Planner:     FakePlanner{Steps: []string{"s1"}},
			Implementer: &FakeImplementer{Result: "done"},
			Evaluators:  []Evaluator{&CounterEvaluator{passAt: 1}},
			Judge:       AggregateJudge{},
			Budget:      budget,
			Sink:        &UsageSink{},
			State:       openStore(t, dbPath),
		})
		var at int
		_, err = loop2.Run(context.Background(), Goal{Text: "second goal", Workdir: workdir}, firstIteration(&at))
		require.NoError(t, err)
		assert.Equal(t, 1, at, "a new objective must not inherit the old run's position")
	})
}

// TestGoalLoop_NoStateStoreIsUnchanged pins the zero-value contract: a Config
// without a StateStore must behave exactly as it did before resume existed.
// Persistence that turns itself on by default would silently truncate runs
// whose state happens to be lying around.
func TestGoalLoop_NoStateStoreIsUnchanged(t *testing.T) {
	t.Parallel()
	loop := New(Config{
		Planner:     FakePlanner{Steps: []string{"s1"}},
		Implementer: &FakeImplementer{Result: "done"},
		Evaluators:  []Evaluator{&CounterEvaluator{passAt: 100}},
		Judge:       AggregateJudge{},
		Budget:      Budget{MaxIterations: 2},
	})
	var at int
	decision, err := loop.Run(context.Background(), Goal{Text: "x"}, firstIteration(&at))
	require.NoError(t, err)
	assert.Equal(t, 1, at)
	assert.Equal(t, 2, loop.Iterations())
	assert.False(t, decision.Complete)
}
