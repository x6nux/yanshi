package goalloop

// W-F-07 的测试。每条验收对应一个会变红的变异：
//
//	验收「完成审计在 judge 之外独立成一道」→ 删掉 loop.go 里的审计块，
//	本文件全部测试变红（fakeCompleteJudge 证明换任何 judge 审计照样拦）。
//	验收「阻塞需连续三轮才认定」→ 把 fakeCompletionRounds 的比较从 >= 改成
//	>，TestVetoesThenAdjudicates 的 Iterations()==3 断言变红（第 4 轮才认定）。
//	验收「无进展判定生效」→ 删掉 ObserveProgress 注入块，
//	TestNoProgressDetectionInjectsCorrectivePrompt 变红。
//	验收「veto 轮注入续跑提示词」→ 删掉 Plan 之后的 directive 注入块，
//	两条 plan 捕获断言变红。

import (
	"context"
	"strings"
	"sync"
	"testing"
)

// planCapturing 记录实现者实际收到的每一轮 plan，其余行为全部委托给内嵌的
// FakeImplementer。审计的注入是否真的到达实现者，只有看这里才知道。
type planCapturing struct {
	inner *FakeImplementer
	mu    sync.Mutex
	plans []Plan
}

func (p *planCapturing) Implement(ctx context.Context, plan Plan, wd string) (string, error) {
	p.mu.Lock()
	p.plans = append(p.plans, plan)
	p.mu.Unlock()
	return p.inner.Implement(ctx, plan, wd)
}

// fakeCompleteJudge 无条件判 Complete。它不是被测对象，是证据：审计的判定
// 不依赖任何特定 judge —— 换一个宽松到失职的 judge，审计照样在同一把尺子
// 上量（这正是「在 judge 之外独立成一道」的验收形状）。
type fakeCompleteJudge struct{}

func (fakeCompleteJudge) Judge(context.Context, []EvalVerdict) (Decision, error) {
	return Decision{Complete: true, Summary: "complete (stub judge)"}, nil
}

func runAuditLoop(t *testing.T, cfg Config) (Decision, []Event, *planCapturing) {
	t.Helper()
	capture := &planCapturing{inner: &FakeImplementer{Result: "done"}}
	cfg.Implementer = capture
	loop := New(cfg)
	var events []Event
	decision, err := loop.Run(context.Background(), Goal{Text: "g", Workdir: t.TempDir()}, func(e Event) {
		events = append(events, e)
	})
	if err != nil {
		t.Fatalf("loop run: %v", err)
	}
	return decision, events, capture
}

func TestVetoesThenAdjudicates(t *testing.T) {
	// 证据为空的 pass：judge（聚合）判完成，审计连续 veto 两轮、第三轮认定。
	decision, events, capture := runAuditLoop(t, Config{
		Planner: FakePlanner{Steps: []string{"step1"}},
		Evaluators: []Evaluator{FakeEvaluator{Verdict: EvalVerdict{
			Evaluator: "test", Pass: true, Evidence: "   ",
		}}},
		Judge:  AggregateJudge{},
		Budget: Budget{MaxIterations: 10},
		Audit:  NewCompletionAuditor(),
	})

	if decision.Complete {
		t.Fatalf("adjudicated run reported Complete=true: %+v", decision)
	}
	if decision.StopReason != StopReasonUnprovenCompletion {
		t.Fatalf("stop reason = %q, want %q", decision.StopReason, StopReasonUnprovenCompletion)
	}
	if loopIters := iterationsOf(events); loopIters != fakeCompletionRounds {
		t.Fatalf("ran %d iterations, want %d (two vetoes then adjudication)", loopIters, fakeCompletionRounds)
	}
	if len(capture.plans) != fakeCompletionRounds {
		t.Fatalf("implementer saw %d plans, want %d", len(capture.plans), fakeCompletionRounds)
	}
	// 续跑提示词真的到了实现者手里：第 2、3 轮的 plan 首条 step 是它。
	for _, i := range []int{1, 2} {
		if got := capture.plans[i].Steps[0]; !strings.Contains(got, "PROVE completion") {
			t.Fatalf("iteration %d plan step 0 lacks the unproven-completion prompt: %q", i+1, got)
		}
	}
	if capture.plans[0].Steps[0] == capture.plans[1].Steps[0] {
		t.Fatalf("iteration 1 plan must not carry the directive, got %q", capture.plans[0].Steps[0])
	}
	var vetoes int
	for _, e := range events {
		if e.Phase == "Audit" && strings.Contains(e.Detail, "vetoed") {
			vetoes++
		}
	}
	if vetoes != 2 {
		t.Fatalf("saw %d veto events, want 2; events: %+v", vetoes, events)
	}
}

func TestAcceptsEvidenceBackedCompletion(t *testing.T) {
	decision, events, capture := runAuditLoop(t, Config{
		Planner: FakePlanner{Steps: []string{"step1"}},
		Evaluators: []Evaluator{FakeEvaluator{Verdict: EvalVerdict{
			Evaluator: "test", Pass: true, Evidence: "ok: 1/1 tests passed",
		}}},
		Judge:  AggregateJudge{},
		Budget: Budget{MaxIterations: 10},
		Audit:  NewCompletionAuditor(),
	})

	if !decision.Complete {
		t.Fatalf("evidence-backed completion was not accepted: %+v", decision)
	}
	if decision.StopReason != StopReasonComplete {
		t.Fatalf("stop reason = %q, want empty (complete)", decision.StopReason)
	}
	if len(capture.plans) != 1 {
		t.Fatalf("implementer saw %d plans, want 1 (no continuation rounds)", len(capture.plans))
	}
	for _, e := range events {
		if e.Phase == "Audit" && strings.Contains(e.Detail, "vetoed") {
			t.Fatalf("evidence-backed completion was vetoed: %+v", events)
		}
	}
}

func TestNoProgressDetectionInjectsCorrectivePrompt(t *testing.T) {
	// 同一失败集连跑三轮：第 1 轮定基线，第 2、3 轮无进展 → 纠偏提示词。
	decision, events, capture := runAuditLoop(t, Config{
		Planner: FakePlanner{Steps: []string{"step1"}},
		Evaluators: []Evaluator{FakeEvaluator{Verdict: EvalVerdict{
			Evaluator: "test", Pass: false, Evidence: "still failing", Gaps: []string{"g1"},
		}}},
		Judge:  AggregateJudge{},
		Budget: Budget{MaxIterations: 3},
		Audit:  NewCompletionAuditor(),
	})

	if decision.Complete || decision.StopReason != StopReasonEscalate {
		// 零值 Tier 让预算耗尽带升级提示（escalate）；关键是没完成、没认定。
		t.Fatalf("stalled run should end at max iterations, got %+v", decision)
	}
	if len(capture.plans) != 3 {
		t.Fatalf("implementer saw %d plans, want 3", len(capture.plans))
	}
	if got := capture.plans[0].Steps[0]; got != "step1" {
		t.Fatalf("baseline round must not carry a directive, got %q", got)
	}
	// 第 2 轮结束时才第一次判出无进展，所以它的 plan 不带指令；第 3 轮带。
	if got := capture.plans[1].Steps[0]; got != "step1" {
		t.Fatalf("first stale detection happens at round 2's end; its own plan must be clean, got %q", got)
	}
	if got := capture.plans[2].Steps[0]; !strings.Contains(got, "no measurable progress") {
		t.Fatalf("iteration 3 plan step 0 lacks the no-progress prompt: %q", got)
	}
	var stalls int
	for _, e := range events {
		if e.Phase == "Audit" && strings.Contains(e.Detail, "no progress") {
			stalls++
		}
	}
	if stalls != 2 {
		t.Fatalf("saw %d no-progress events, want 2; events: %+v", stalls, events)
	}
}

func TestAuditReasonsNameTheBrokenLink(t *testing.T) {
	t.Run("implementer error", func(t *testing.T) {
		impl := &FakeImplementer{Result: "done", FailFirst: 3}
		capture := &planCapturing{inner: impl}
		loop := New(Config{
			Planner:     FakePlanner{Steps: []string{"step1"}},
			Implementer: capture,
			Evaluators: []Evaluator{FakeEvaluator{Verdict: EvalVerdict{
				Evaluator: "test", Pass: true, Evidence: "ok",
			}}},
			Judge:  fakeCompleteJudge{},
			Budget: Budget{MaxIterations: 10},
			Audit:  NewCompletionAuditor(),
		})
		decision, err := loop.Run(context.Background(), Goal{Text: "g", Workdir: t.TempDir()}, nil)
		if err != nil {
			t.Fatalf("loop run: %v", err)
		}
		if decision.StopReason != StopReasonUnprovenCompletion {
			t.Fatalf("stop reason = %q, want %q", decision.StopReason, StopReasonUnprovenCompletion)
		}
	})

	t.Run("evaluator never ran", func(t *testing.T) {
		// 评估器返回 Go error：Run 把它包装成 Evaluator=="error" 的占位判决。
		// 宽松 judge 在含这种判决的轮里判完成，审计必须点名「缺考」。
		aud := NewCompletionAuditor()
		av := aud.AuditComplete(AuditRound{
			Iteration:   1,
			Plan:        Plan{Steps: []string{"s"}},
			Implemented: true,
			Verdicts: []EvalVerdict{
				{Evaluator: "test", Pass: true, Evidence: "ok"},
				{Evaluator: "error", Pass: false, Gaps: []string{"evaluator error: boom"}},
			},
		})
		if av.Accept || !strings.Contains(av.Reason, "evaluator failed to run") {
			t.Fatalf("error-verdict round not flagged: %+v", av)
		}
	})
}

func TestHonestIncompleteRoundResetsClaimStreak(t *testing.T) {
	// 交替：无凭据完成（veto）→ 诚实 incomplete。两次诚实轮打断「连续」，
	// 第 5 轮的声明只数到 1 —— 认定永不达成，循环走到 MaxIterations。
	flip := &flipEvaluator{onCall: func(n int) EvalVerdict {
		if n%2 == 1 { // 奇数轮：无凭据 pass → judge 判完成 → 审计 veto
			return EvalVerdict{Evaluator: "test", Pass: true}
		}
		return EvalVerdict{Evaluator: "test", Pass: false, Gaps: []string{"honest gap"}, Evidence: "not done"}
	}}
	decision, events, _ := runAuditLoop(t, Config{
		Planner:    FakePlanner{Steps: []string{"step1"}},
		Evaluators: []Evaluator{flip},
		Judge:      AggregateJudge{},
		Budget:     Budget{MaxIterations: 5},
		Audit:      NewCompletionAuditor(),
	})

	if decision.StopReason != StopReasonEscalate {
		// Tier 未设（零值 TierQuickFix）时耗尽预算的循环以 escalate 结束 ——
		// 关键断言是「不是 unproven_completion」：重置让认定永不达成。
		t.Fatalf("stop reason = %q, want %q (streak reset must prevent adjudication)",
			decision.StopReason, StopReasonEscalate)
	}
	if decision.Complete {
		t.Fatalf("streak-reset run must not report completion: %+v", decision)
	}
	vetoes := 0
	for _, e := range events {
		if e.Phase == "Audit" && strings.Contains(e.Detail, "vetoed") {
			vetoes++
		}
	}
	if vetoes != 3 {
		t.Fatalf("saw %d veto events, want 3 (odd rounds); events: %+v", vetoes, events)
	}
}

// flipEvaluator 第 n 次调用返回 onCall(n) 的判决。
type flipEvaluator struct {
	onCall func(n int) EvalVerdict
	calls  int
}

func (f *flipEvaluator) Evaluate(context.Context, Goal, Plan, string) (EvalVerdict, error) {
	f.calls++
	return f.onCall(f.calls), nil
}

func TestNilAuditKeepsLegacyBehavior(t *testing.T) {
	// Audit 为 nil：无凭据的 pass 照样按 judge 的判决完成 —— 未接线的调用方
	// 行为与引入前逐字节一致，nil 之外没有隐藏开关。
	decision, _, _ := runAuditLoop(t, Config{
		Planner: FakePlanner{Steps: []string{"step1"}},
		Evaluators: []Evaluator{FakeEvaluator{Verdict: EvalVerdict{
			Evaluator: "test", Pass: true,
		}}},
		Judge:  AggregateJudge{},
		Budget: Budget{MaxIterations: 10},
	})

	if !decision.Complete {
		t.Fatalf("nil audit must leave the pre-existing completion behavior intact: %+v", decision)
	}
}

func TestProgressSignatureIgnoresEvidenceNoise(t *testing.T) {
	// 签名只看 pass 数与失败 gap 集：同失败集下证据文本换了措辞不构成进展。
	a := progressSignature([]EvalVerdict{{Evaluator: "t", Pass: false, Evidence: "v1", Gaps: []string{"b", "a"}}})
	b := progressSignature([]EvalVerdict{{Evaluator: "t", Pass: false, Evidence: "totally different", Gaps: []string{"a", "b"}}})
	if a != b {
		t.Fatalf("signature treats evidence wording as progress: %q vs %q", a, b)
	}
	c := progressSignature([]EvalVerdict{{Evaluator: "t", Pass: false, Gaps: []string{"a", "b", "c"}}})
	if a == c {
		t.Fatalf("signature missed a new failing gap: %q vs %q", a, c)
	}
}

func iterationsOf(events []Event) int {
	max := 0
	for _, e := range events {
		if e.Iteration > max {
			max = e.Iteration
		}
	}
	return max
}
