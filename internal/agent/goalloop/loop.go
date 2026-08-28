package goalloop

import (
	"context"
	"fmt"
	"strings"
)

// Config holds the components and budget for the Goal Loop.
type Config struct {
	Planner     Planner
	Implementer Implementer
	Evaluators  []Evaluator
	Judge       Judge
	Budget      Budget
	// Sink is the shared token accumulator every LLM-calling component writes
	// to (G02), and the sole source of the loop's spend figure. A nil Sink means
	// spend is zero and MaxTokens can never trip — which is why cmd/yanshi
	// allocates one before the fake/real branch rather than inside it.
	Sink *UsageSink
	// Tier is the difficulty tier this run was dispatched at (G03). It only
	// affects the exhaustion message (EscalationHint) — it does not change the
	// pipeline, which is set by the caller via the components above. Zero value
	// (TierQuickFix) is safe.
	Tier Tier
	// State persists the run's progress — objective, both budgets, iterations
	// executed and tokens spent — so a restarted process resumes where it
	// stopped instead of replaying the goal from iteration 1 with a budget
	// reset to zero (W-D-16). A nil State disables persistence and leaves Run's
	// behaviour exactly as it was before.
	//
	// Only the multi-iteration path (T3-T4) has a resume point to persist. The
	// lightweight T0-T2 path is a single orchestrator turn with no "where it
	// stopped" to come back to, so it deliberately does not carry one.
	State StateStore
	// BudgetExplicit marks which Budget limits the operator typed for this run,
	// deciding who wins when a resumed run's persisted budget disagrees with
	// the one above. See BudgetSet and resolveResumeBudget. The zero value —
	// nothing explicit — hands the decision to the persisted budget.
	BudgetExplicit BudgetSet
}

// Loop is the Goal Loop controller. It repeatedly runs
// Plan -> Implement -> Evaluate -> Judge until the goal is complete,
// the budget is exhausted, or the context is cancelled.
type Loop struct {
	cfg        Config
	iterations int
}

// New creates a Loop with the given Config.
func New(cfg Config) *Loop {
	return &Loop{cfg: cfg}
}

// spent returns the current total token spend: the live accumulated total
// across every model call that writes to the shared UsageSink.
//
// A nil Sink yields 0, so the budget silently never trips. That is the correct
// reading rather than a gap — nothing has reported any spend — but it makes
// wiring the sink load-bearing, which is why it is allocated before the
// fake/real branch in cmd/yanshi rather than inside the real one.
//
// The predecessor fell back to a static Budget.SpentTokens field when the sink
// was nil. Nothing in production ever assigned that field, so the fallback
// existed solely to let tests hand-write a spend figure — a second, simpler
// code path that only test code could reach, and therefore the path most
// likely to stay green while the real one broke.
func (l *Loop) spent() int {
	if l.cfg.Sink == nil {
		return 0
	}
	return l.cfg.Sink.Snapshot().Total()
}

// overBudget reports whether the token budget has been crossed. A zero
// MaxTokens disables the token budget entirely (iteration budget still applies).
func (l *Loop) overBudget() bool {
	return l.cfg.Budget.MaxTokens > 0 && l.spent() > l.cfg.Budget.MaxTokens
}

// usageSnapshot returns the accumulated usage to stamp onto a terminal Decision.
func (l *Loop) usageSnapshot() Usage {
	if l.cfg.Sink != nil {
		return l.cfg.Sink.Snapshot()
	}
	return Usage{}
}

// budgetExceededDecision builds the terminal Decision for a token-budget stop,
// carrying the spend and the canonical stop reason so the CLI can persist it.
func (l *Loop) budgetExceededDecision(at string) Decision {
	return Decision{
		Complete:   false,
		Summary:    fmt.Sprintf("budget exceeded %s (%d tokens > %d)", at, l.spent(), l.cfg.Budget.MaxTokens),
		StopReason: StopReasonTokenBudget,
		Usage:      l.usageSnapshot(),
	}
}

// Event represents a phase event emitted during the loop.
// Phase is one of "State", "Plan", "Implement", "Evaluate", "Judge", "Done".
// "State" carries resume and persistence notices (iteration 0 when emitted
// before the first cycle) and never indicates a failed run.
type Event struct {
	Phase     string
	Iteration int
	Detail    string
}

// Iterations returns the number of iterations the loop has executed.
func (l *Loop) Iterations() int {
	return l.iterations
}

// Run executes the Goal Loop. It calls onEvent for each phase event.
// If onEvent is nil, no events are emitted. The loop terminates when:
//   - the Judge returns Decision.Complete == true (Done),
//   - the context is cancelled (returns ctx.Err()),
//   - the token budget is exceeded (returns a budget-exceeded Decision), or
//   - MaxIterations is reached without completion (returns an incomplete Decision).
//
// When Config.State is wired, Run resumes a previously interrupted run of the
// same goal in the same working directory: it starts at the next iteration and
// carries the earlier token spend forward, so neither budget is reset by the
// restart.
func (l *Loop) Run(ctx context.Context, g Goal, onEvent func(Event)) (Decision, error) {
	emit := func(phase, detail string, iter int) {
		if onEvent != nil {
			onEvent(Event{Phase: phase, Iteration: iter, Detail: detail})
		}
	}

	first := 1
	saved, resumed, err := l.loadState(g)
	if err != nil {
		emit("State", fmt.Sprintf("unreadable, starting from iteration 1: %v", err), 0)
	} else if resumed {
		first = saved.Iterations + 1
		l.iterations = saved.Iterations
		// Seeding the shared sink is what makes the TOKEN budget resume rather
		// than reset: spent() reads the sink and nothing else, so a fresh
		// process would otherwise report zero spend and grant the run its
		// whole MaxTokens over again.
		if l.cfg.Sink != nil {
			l.cfg.Sink.Add(saved.Usage)
		}
		// Explicit flags beat the persisted budget; everything else loses to
		// it. Either way the loser is named out loud rather than dropped in
		// silence — both directions are surprising to somebody.
		effective := resolveResumeBudget(l.cfg.Budget, saved.Budget, l.cfg.BudgetExplicit)
		switch {
		case effective != saved.Budget:
			emit("State", fmt.Sprintf("explicit budget %+v overrides persisted %+v", effective, saved.Budget), saved.Iterations)
		case effective != l.cfg.Budget:
			emit("State", fmt.Sprintf("persisted budget %+v overrides %+v", effective, l.cfg.Budget), saved.Iterations)
		}
		// Assigning unconditionally is what writes an explicit new limit back
		// to the store, since saveState persists whatever cfg.Budget ends up as.
		l.cfg.Budget = effective
		emit("State", fmt.Sprintf("resuming at iteration %d/%d with %d tokens already spent",
			first, l.cfg.Budget.MaxIterations, saved.Usage.Total()), saved.Iterations)
	}

	// Persist on every exit path, cancellation included — that is the
	// interruption this mechanism exists for, and it is also the one path that
	// returns without producing a Decision.
	complete := false
	defer func() {
		if err := l.saveState(g, complete); err != nil {
			emit("State", fmt.Sprintf("persist failed: %v", err), l.iterations)
		}
	}()

	for iter := first; iter <= l.cfg.Budget.MaxIterations; iter++ {
		// Context cancellation.
		if err := ctx.Err(); err != nil {
			return Decision{}, err
		}

		// Token budget check (G02): driven by the shared sink when wired.
		if l.overBudget() {
			return l.budgetExceededDecision("before iteration"), nil
		}

		l.iterations = iter

		// --- Plan ---
		plan, err := l.cfg.Planner.Plan(ctx, g)
		if err != nil {
			emit("Plan", fmt.Sprintf("error: %v", err), iter)
			return Decision{}, fmt.Errorf("planner error (iteration %d): %w", iter, err)
		}
		emit("Plan", fmt.Sprintf("%d steps, %d tests", len(plan.Steps), len(plan.Tests)), iter)

		// Re-check budget after the planner's LLM call so an oversized plan
		// stops the loop before we pay for an expensive Implement/Evaluate.
		if l.overBudget() {
			return l.budgetExceededDecision("after plan"), nil
		}

		// --- Implement ---
		result, implErr := l.cfg.Implementer.Implement(ctx, plan, g.Workdir)
		if implErr != nil {
			emit("Implement", fmt.Sprintf("error: %v", implErr), iter)
		} else {
			emit("Implement", result, iter)
		}

		// --- Evaluate (sequential for M6) ---
		var verdicts []EvalVerdict
		for _, ev := range l.cfg.Evaluators {
			v, err := ev.Evaluate(ctx, g, plan, g.Workdir)
			if err != nil {
				v = EvalVerdict{
					Evaluator: "error",
					Pass:      false,
					Evidence:  err.Error(),
					Gaps:      []string{fmt.Sprintf("evaluator error: %v", err)},
				}
			}
			verdicts = append(verdicts, v)
		}
		emit("Evaluate", fmt.Sprintf("%d verdicts", len(verdicts)), iter)

		// --- Judge ---
		decision, err := l.cfg.Judge.Judge(ctx, verdicts)
		if err != nil {
			emit("Judge", fmt.Sprintf("error: %v", err), iter)
			return Decision{}, fmt.Errorf("judge error (iteration %d): %w", iter, err)
		}

		if decision.Complete {
			complete = true
			emit("Judge", "complete", iter)
			emit("Done", decision.Summary, iter)
			return decision, nil
		}

		emit("Judge", fmt.Sprintf("gaps: %s", strings.Join(decision.Gaps, "; ")), iter)
	}

	// Exhausted all iterations without completion.
	hint := EscalationHint(l.cfg.Tier)
	summary := fmt.Sprintf("max iterations (%d) reached without completion", l.cfg.Budget.MaxIterations)
	reason := StopReasonMaxIters
	// A sub-top tier that ran out of budget surfaces an explicit upgrade
	// recommendation instead of exiting silently (G03: 升级规则不静默退出).
	if hint != "" {
		summary += "; " + hint
		reason = StopReasonEscalate
	}
	return Decision{
		Complete:   false,
		Summary:    summary,
		StopReason: reason,
		Usage:      l.usageSnapshot(),
	}, nil
}
