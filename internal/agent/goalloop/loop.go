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
// Phase is one of "Plan", "Implement", "Evaluate", "Judge", "Done".
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
func (l *Loop) Run(ctx context.Context, g Goal, onEvent func(Event)) (Decision, error) {
	emit := func(phase, detail string, iter int) {
		if onEvent != nil {
			onEvent(Event{Phase: phase, Iteration: iter, Detail: detail})
		}
	}

	for iter := 1; iter <= l.cfg.Budget.MaxIterations; iter++ {
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
