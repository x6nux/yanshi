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
	// Audit 是 judge 之外的完成审计（W-F-07）。judge 判 Complete 之后审计
	// 独立检查这一轮的记录配不配得上「完成」；配不上就 veto（注入续跑
	// 提示词，循环继续），连续三轮才认定并终态停止。nil = 不审计，行为与
	// 引入前逐字节一致 —— 这也是测试与未接线的调用方的兼容姿态；生产装配
	// （cmd/yanshi 的两条 goal 路径）都接线。审计器跨轮计数，每次 Run 一个
	// 实例（与 Loop 同生命周期，见 CompletionAuditor）。
	Audit *CompletionAuditor
}

// Loop is the Goal Loop controller. It repeatedly runs
// Plan -> Implement -> Evaluate -> Judge until the goal is complete,
// the budget is exhausted, or the context is cancelled.
type Loop struct {
	cfg        Config
	iterations int
	// persistedIter is the last iteration that ran to completion, which is what
	// gets written to the store — as opposed to iterations, which counts the
	// one currently in flight too. Resuming from the in-flight number would
	// skip an iteration that was paid for but never finished.
	persistedIter int
	// directive 是待注入下一轮计划的续跑提示词（W-F-07：完成审计的 veto 或
	// 无进展纠偏）。注入点在 Plan 之后 —— 它以第一条 step 的身份到达实现者
	//（ACPImplementer 把每条 step 作为一次 agent prompt 下发），消费后清空。
	directive string
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
// Phase is one of "State", "Plan", "Implement", "Evaluate", "Judge", "Audit",
// "Done". "State" carries resume and persistence notices (iteration 0 when
// emitted before the first cycle) and never indicates a failed run. "Audit"
// carries the completion audit's veto/adjudication notices (W-F-07) — a vetoed
// round is NOT a failed run either: the loop continues by design.
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
		// Restoring this too is load-bearing: the final flush below writes it
		// back, so leaving it at zero would let a resumed run that stops
		// immediately (an already-blown budget, say) erase the progress it
		// just read.
		l.persistedIter = saved.Iterations
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
		// Both arms can fire at once — one limit typed, the other not — so
		// these are independent ifs rather than a switch. As a switch the
		// second case went unreported in exactly the mixed situation where the
		// operator is most likely to be surprised by it.
		effective := resolveResumeBudget(l.cfg.Budget, saved.Budget, l.cfg.BudgetExplicit)
		if effective != saved.Budget {
			emit("State", fmt.Sprintf("explicit budget %+v overrides persisted %+v", effective, saved.Budget), saved.Iterations)
		}
		if effective != l.cfg.Budget {
			emit("State", fmt.Sprintf("persisted budget %+v overrides %+v", effective, l.cfg.Budget), saved.Iterations)
		}
		// Assigning unconditionally is what writes an explicit new limit back
		// to the store, since saveState persists whatever cfg.Budget ends up as.
		l.cfg.Budget = effective
		emit("State", fmt.Sprintf("resuming at iteration %d/%d with %d tokens already spent",
			first, l.cfg.Budget.MaxIterations, saved.Usage.Total()), saved.Iterations)
	}

	// Final flush. It catches the exits that end a run without finishing an
	// iteration — cancellation, an already-blown budget, a planner or judge
	// error — so the spend they incurred is recorded. It is NOT the primary
	// write: a deferred function does not run when the process is SIGKILLed,
	// OOM-killed or loses power, and those are exactly the interruptions this
	// mechanism exists to come back from. The durable record is written inside
	// the loop, once per completed iteration.
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
		// W-F-07: 上一轮审计的续跑提示词作为第一条 step 注入。放最前是因为
		// 实现者按序执行 steps，证明性的指令必须在它再做别的之前到达；放
		// step 里而不是旁路参数里，是因为 ACPImplementer 的下发单位就是 step，
		// 旁路指令到不了外部 agent 的提示词。
		if l.directive != "" {
			plan.Steps = append([]string{l.directive}, plan.Steps...)
			l.directive = ""
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

		// --- Completion audit (W-F-07): judge 之外的独立一道 ---
		// 只在 judge 判 Complete 时发声。三条出路：接受（照常完成）、认定
		// （终态停止）、veto（本轮按未完成继续，注入续跑提示词）。veto 改的
		// 是循环停不停，不是 judge 的判决记录 —— Decision 不落库 veto 这件事，
		// 落库的是「本轮未完成」，下一轮的注入指令才是 veto 的产物。
		vetoed := false
		if decision.Complete && l.cfg.Audit != nil {
			av := l.cfg.Audit.AuditComplete(AuditRound{
				Iteration:   iter,
				Plan:        plan,
				Implemented: implErr == nil,
				Verdicts:    verdicts,
			})
			switch {
			case av.Accept:
				emit("Audit", "completion verified", iter)
			case av.Final:
				emit("Audit", fmt.Sprintf(
					"unproven completion adjudicated after %d consecutive rounds (last reason: %s)",
					fakeCompletionRounds, av.Reason), iter)
				return Decision{
					Complete: false,
					Summary: fmt.Sprintf("completion claimed without verifiable evidence in %d consecutive rounds (last reason: %s)",
						fakeCompletionRounds, av.Reason),
					StopReason: StopReasonUnprovenCompletion,
					Usage:      l.usageSnapshot(),
				}, nil
			default:
				decision.Complete = false
				vetoed = true
				l.directive = joinDirectives(l.directive, av.Directive)
				emit("Audit", fmt.Sprintf("completion vetoed (%s); continuation prompt injected", av.Reason), iter)
			}
		}
		// 一轮「judge 自己判 incomplete、不是 veto 造成的」打断「连续」——
		// 模型至少诚实报告过一次未完成。veto 轮不算：那轮的 incomplete 是
		// 审计改的，不是模型说的。
		if l.cfg.Audit != nil && !decision.Complete && !vetoed {
			l.cfg.Audit.ResetClaims()
		}
		// 无进展判定每轮都跑（终态轮除外）：签名不变即注入纠偏提示词，
		// 从第一轮无进展起就纠，不等梯度 —— 它是纠偏不是惩罚。
		if l.cfg.Audit != nil {
			if d, stale := l.cfg.Audit.ObserveProgress(AuditRound{
				Iteration:   iter,
				Plan:        plan,
				Implemented: implErr == nil,
				Verdicts:    verdicts,
			}); d != "" {
				l.directive = joinDirectives(l.directive, d)
				emit("Audit", fmt.Sprintf("no progress for %d round(s); corrective prompt injected", stale), iter)
			}
		}

		// The iteration is over, so commit it now rather than at return: a run
		// killed outright between here and the next judgement still comes back
		// having kept this iteration's progress and its spend.
		complete = decision.Complete
		l.persistedIter = iter
		if err := l.saveState(g, complete); err != nil {
			emit("State", fmt.Sprintf("persist failed: %v", err), iter)
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
