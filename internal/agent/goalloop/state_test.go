package goalloop

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
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

// readGoalState decodes the persisted row for goal through st. It is what the
// "stored in SQLite" half of the acceptance is checked against, and it is
// deliberately read through a handle the writing Loop never held.
func readGoalState(t *testing.T, st *store.Store, goal Goal) GoalState {
	t.Helper()
	blob, ok, err := st.KVGet(goalStateKey(goal.Workdir))
	require.NoError(t, err)
	require.True(t, ok, "goal state must be in SQLite, readable by another handle")
	var s GoalState
	require.NoError(t, json.Unmarshal([]byte(blob), &s))
	return s
}

// budgetLoop builds a Loop that bills perIteration tokens per cycle and never
// passes evaluation, so it runs until one of the two budgets stops it.
func budgetLoop(budget Budget, explicit BudgetSet, sink *UsageSink, st StateStore, perIteration int) *Loop {
	return New(Config{
		Planner:        &chargingPlanner{sink: sink, perCall: Usage{TotalTokens: perIteration}},
		Implementer:    &FakeImplementer{Result: "done"},
		Evaluators:     []Evaluator{&CounterEvaluator{passAt: 100}},
		Judge:          AggregateJudge{},
		Budget:         budget,
		Sink:           sink,
		State:          st,
		BudgetExplicit: explicit,
	})
}

// seedTokenExhaustedRun runs a goal until its token budget stops it, leaving a
// real persisted row behind with iterations still unspent. Resume tests build
// on this rather than hand-writing state, so they exercise the writer too.
func seedTokenExhaustedRun(t *testing.T, dbPath string, goal Goal, budget Budget, perIteration int) (*Loop, Decision) {
	t.Helper()
	l := budgetLoop(budget, BudgetSet{}, &UsageSink{}, openStore(t, dbPath), perIteration)
	d, err := l.Run(context.Background(), goal, nil)
	require.NoError(t, err)
	require.Equal(t, StopReasonTokenBudget, d.StopReason)
	require.Less(t, l.Iterations(), budget.MaxIterations, "iterations must still be available")
	return l, d
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

		loop1, decision1 := seedTokenExhaustedRun(t, dbPath, goal, budget, perIteration)

		// The persisted row is the contract: objective plus BOTH budget fields.
		st2 := openStore(t, dbPath)
		persisted := readGoalState(t, st2, goal)
		assert.Equal(t, goal.Text, persisted.Objective)
		assert.Equal(t, budget, persisted.Budget, "both limits must be stored, not just the one that bit")
		assert.Equal(t, decision1.Usage.Total(), persisted.Usage.Total())

		// --- restart, with the roomy budget a config default would supply ---
		// This is the realistic shape of the bug: nobody re-types the tight
		// budget after a crash, so a resumed run that honoured the caller's
		// budget would hand itself a brand new one. Nothing is explicit here,
		// so the persisted budget must survive intact.
		sink2 := &UsageSink{}
		loop2 := budgetLoop(Budget{MaxIterations: 99, MaxTokens: 99999}, BudgetSet{}, sink2, st2, perIteration)
		var ranAt int
		var announced bool
		record := firstIteration(&ranAt)
		decision2, err := loop2.Run(context.Background(), goal, func(e Event) {
			record(e)
			if e.Phase == "State" && strings.Contains(e.Detail, "persisted budget") {
				announced = true
			}
		})
		require.NoError(t, err)
		assert.True(t, announced, "a budget override must be reported, not applied in silence")
		assert.Equal(t, budget, readGoalState(t, st2, goal).Budget,
			"an unset flag must not overwrite the stored budget with a config default")

		assert.Equal(t, StopReasonTokenBudget, decision2.StopReason,
			"the spent tokens must still be spent after a restart")
		// Comparing token totals is NOT enough here, and the probe proved it:
		// an unseeded sink spends the same total again from zero and lands on
		// the same number. What separates the two is that a resumed run must
		// buy nothing at all — no phase runs, and the iteration count does not
		// move — because the budget was already gone before it started.
		assert.Zero(t, ranAt, "an exhausted token budget must buy zero further phases")
		assert.Equal(t, loop1.Iterations(), loop2.Iterations(),
			"an exhausted token budget must buy zero further iterations")
	})

	// An operator who types a new limit must get it. The persisted budget only
	// outranks values nobody chose — otherwise raising a budget after a crash
	// would mean editing a flag, seeing nothing happen, and having to dig the
	// old number out of SQLite.
	t.Run("an explicit flag beats the persisted budget", func(t *testing.T) {
		t.Parallel()
		dbPath := filepath.Join(t.TempDir(), "explicit.db")
		goal := Goal{Text: "widen the retry window", Workdir: "/repo/retry"}
		budget := Budget{MaxIterations: maxIters, MaxTokens: stopAfter*perIteration - 50}

		loop1, decision1 := seedTokenExhaustedRun(t, dbPath, goal, budget, perIteration)
		spent := decision1.Usage.Total()
		left := maxIters - loop1.Iterations()
		require.Positive(t, left)

		// Only -max-tokens is typed. -max-iters is not, so it must still come
		// from the store even though the caller's struct carries a value for it.
		raised := Budget{MaxIterations: 99, MaxTokens: spent + left*perIteration}
		st2 := openStore(t, dbPath)
		sink2 := &UsageSink{}
		loop2 := budgetLoop(raised, BudgetSet{MaxTokens: true}, sink2, st2, perIteration)
		var ranAt int
		var announced bool
		record := firstIteration(&ranAt)
		decision2, err := loop2.Run(context.Background(), goal, func(e Event) {
			record(e)
			if e.Phase == "State" && strings.Contains(e.Detail, "explicit budget") {
				announced = true
			}
		})
		require.NoError(t, err)

		assert.True(t, announced, "the override must be reported in the other direction too")
		assert.Equal(t, loop1.Iterations()+1, ranAt, "the raised budget buys work, from the resume point")
		assert.Equal(t, maxIters, loop2.Iterations(),
			"MaxIterations was not typed, so it must still come from the store, not the caller's 99")
		assert.NotEqual(t, StopReasonTokenBudget, decision2.StopReason)

		after := readGoalState(t, st2, goal)
		assert.Equal(t, raised.MaxTokens, after.Budget.MaxTokens,
			"a typed limit becomes the new persisted fact")
		assert.Equal(t, budget.MaxIterations, after.Budget.MaxIterations,
			"an untyped limit must not be overwritten by the caller's value")
	})

	// Lowering a limit below what the run already spent is defined behaviour,
	// not an error: the run is simply over budget on the new ceiling and stops
	// through the paths that already exist.
	t.Run("an explicit limit below the spend ends the run", func(t *testing.T) {
		t.Parallel()
		goal := Goal{Text: "shrink the image", Workdir: "/repo/image"}
		budget := Budget{MaxIterations: maxIters, MaxTokens: stopAfter*perIteration - 50}

		t.Run("tokens", func(t *testing.T) {
			t.Parallel()
			dbPath := filepath.Join(t.TempDir(), "lowtokens.db")
			loop1, decision1 := seedTokenExhaustedRun(t, dbPath, goal, budget, perIteration)
			lowered := Budget{MaxIterations: maxIters, MaxTokens: decision1.Usage.Total() - 1}

			loop2 := budgetLoop(lowered, BudgetSet{MaxTokens: true}, &UsageSink{}, openStore(t, dbPath), perIteration)
			var ranAt int
			decision2, err := loop2.Run(context.Background(), goal, firstIteration(&ranAt))
			require.NoError(t, err)
			assert.Equal(t, StopReasonTokenBudget, decision2.StopReason)
			assert.Zero(t, ranAt, "no phase may run once the new ceiling is already breached")
			assert.Equal(t, loop1.Iterations(), loop2.Iterations())
		})

		t.Run("iterations", func(t *testing.T) {
			t.Parallel()
			dbPath := filepath.Join(t.TempDir(), "lowiters.db")
			loop1, _ := seedTokenExhaustedRun(t, dbPath, goal, budget, perIteration)
			// One fewer iteration than already ran, and enough tokens that only
			// the iteration limit can be what stops it.
			lowered := Budget{MaxIterations: loop1.Iterations() - 1, MaxTokens: 99999}

			loop2 := budgetLoop(lowered, BudgetSet{MaxIterations: true, MaxTokens: true},
				&UsageSink{}, openStore(t, dbPath), perIteration)
			var ranAt int
			decision2, err := loop2.Run(context.Background(), goal, firstIteration(&ranAt))
			require.NoError(t, err)
			// The exhaustion path, not the token path. StopReason itself is not
			// asserted directly: the default tier turns max_iterations into
			// escalate, which is pre-existing behaviour this test is not about.
			assert.Contains(t, decision2.Summary, "max iterations")
			assert.NotEqual(t, StopReasonTokenBudget, decision2.StopReason)
			assert.Zero(t, ranAt, "the resume point is past the new limit, so nothing runs")
			assert.False(t, decision2.Complete)
		})
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
