package goalloop

// W-F-07（INF4 / A32）：防伪完成的停止审计。
//
// spec 的验收有三条：无进展判定生效；完成审计在 judge 之外独立成一道；
// 阻塞需连续三轮才认定。落点是 goalloop 的 Stop 时刻 —— judge 判 Complete
// 的那一瞬。codex 的 Stop hook 在这个时刻拦「模型谎报完成」；goalloop 的
// 对应物就是这道审计：它**不替换**评估器投票（judge 仍按各评估器的判决
// 聚合），而是在 judge 说「完成」之后独立检查这一轮的记录**配不配得上**
// 那句「完成」。配不上就不让停 —— 注入续跑提示词（防伪完成提示词本体）
// 强制下一轮拿出证据。
//
// ── 与 judge 的分工（为什么这不是第二个 judge）──
//
// judge 消费的是评估器的**判决**（pass/fail），审计消费的是**证据链**：
// 实现者这一轮到底跑没跑成、评估器到底执没执行、pass 判决后面有没有
// 实际证据。三个被点名的条件（见 AuditClaimReason）都是「判决说完成、
// 记录说不出来」的形状 —— 它们不评估目标是否达成（那是评估器的事），
// 只检查「完成」这个判决有没有记录支撑。judge 可以被换成任何实现
//（Judge 是接口），审计在同一把尺子上量所有 judge —— 这正是「独立成
// 一道」的含义。
//
// ── 阻塞需连续三轮（梯度语义，与 loopguard 的「先纠偏后停止」同构）──
//
// 第一、二次无凭据的完成声明只 veto：本轮不停，注入续跑提示词。连续
// 第三次才认定 —— 终态停止，StopReason 带专用值，操作员看到的是「完成
// 被声明了三轮、一次都没被证实」，而不是一个被无限 veto 的循环。计数
// 只在「连续」上累积：中间出现一轮（未被 veto 的）诚实的 incomplete
// 判决就清零 —— 模型至少诚实地报告过一次未完成，模式就断了。已认定
// 后不追加轮数：三次证据与三十次证据对操作员是同一个诊断。
//
// ── 无进展判定 ──
//
// 每轮算一个进度签名（pass 数 + 失败 gap 集合），与上一轮相同即无进展。
// 纠偏提示词从第一轮无进展起就注入（不等三轮）—— 它是纠偏不是惩罚，
// 晚一轮就白烧一轮预算；循环的兜底仍是 MaxIterations，这里不另设停止。
// 签名刻意不含实现者的结果串：同一个失败集配不同的自我描述是典型的
// 假进展。

import (
	"fmt"
	"sort"
	"strings"
)

// fakeCompletionRounds 是认定伪完成所需的**连续**无凭据完成声明轮数。
// spec 验收原文：「阻塞需连续三轮才认定」。
const fakeCompletionRounds = 3

// StopReasonUnprovenCompletion 常量住在 types.go 的 StopReason 块里 —— 那里
// 是全部停止原因的权威清单，这里只留判定语义。

// AuditRound 是审计看到的一轮记录。字段是 loop 已经算出来的值，审计
// 不回头重跑任何东西 —— 它量的是记录，不是世界。
type AuditRound struct {
	// Iteration 是本轮序号（从 1 起）。
	Iteration int
	// Plan 是本轮实际下发的计划（可能已带上一轮的续跑指令）。
	Plan Plan
	// Implemented 报告实现者本轮是否跑成（err == nil）。
	Implemented bool
	// Verdicts 是本轮全部评估器判决（评估器出错的轮里含 Evaluator=="error"
	// 的占位判决，见 Run 的错误包装）。
	Verdicts []EvalVerdict
}

// AuditVerdict 是完成审计对一次完成声明的判决。
type AuditVerdict struct {
	// Accept 为 true 表示证据链撑得起完成声明，循环照常以 Complete 停止。
	Accept bool
	// Final 为 true 表示这是连续第 fakeCompletionRounds 轮无凭据声明 ——
	// 已认定，循环应终态停止（StopReasonUnprovenCompletion）而不是再 veto。
	Final bool
	// Reason 点名证据链缺口的条件名（见 AuditClaimReason 的返回值）。
	Reason string
	// Directive 是 veto 轮注入下一轮的续跑提示词。Accept/Final 轮为空。
	Directive string
}

// CompletionAuditor 是 judge 之外的完成审计器。它跨轮持有两个计数
// （连续无凭据声明数、连续无进展轮数），因此**每次 Run 一个实例** ——
// 与 Loop 本身同生命周期。零值不可用，经 NewCompletionAuditor 构造。
//
// 无配置开关：审计接进 Config 即生效。它是防线不是策略 —— 「要不要防
// 模型谎报完成」不是操作员该做选择题的地方，接线即可关（不设 Audit 字段），
// 这已经比一个会被随手关掉的开关更诚实地表达了默认立场。
type CompletionAuditor struct {
	unprovenRounds int
	lastSig        string
	staleRounds    int
	started        bool
}

// NewCompletionAuditor 构造一个完成审计器。
func NewCompletionAuditor() *CompletionAuditor {
	return &CompletionAuditor{}
}

// AuditComplete 审计一次完成声明。仅当 judge 判 Complete 时由 loop 调用；
// 判决基于本轮记录（AuditRound），三个证据条件见 AuditClaimReason。
//
// 接受 → Accept。首次/第二次拒绝 → veto（Directive 带续跑提示词，循环
// 继续）。连续第三次拒绝 → Final（循环应终态停止）。
func (a *CompletionAuditor) AuditComplete(r AuditRound) AuditVerdict {
	reason, proven := auditClaimReason(r)
	if proven {
		a.unprovenRounds = 0
		return AuditVerdict{Accept: true}
	}
	a.unprovenRounds++
	if a.unprovenRounds >= fakeCompletionRounds {
		return AuditVerdict{Final: true, Reason: reason}
	}
	return AuditVerdict{
		Reason:    reason,
		Directive: UnprovenCompletionPrompt(a.unprovenRounds, reason),
	}
}

// ResetClaims 清空连续无凭据声明的计数。loop 在「judge 判 incomplete 且
// 本轮不是 veto 造成的」时调用：模型至少诚实地报告过一次未完成，「连续」
// 的模式就断了。
func (a *CompletionAuditor) ResetClaims() { a.unprovenRounds = 0 }

// ObserveProgress 记录一轮的进度签名并返回无进展纠偏提示词。签名与上一轮
// 相同即无进展 —— 从第一轮无进展起返回非空 Directive（纠偏不等梯度），
// 有进展则清零计数并返回空串。每轮（终态轮除外）都要调，否则签名基线断档。
func (a *CompletionAuditor) ObserveProgress(r AuditRound) (directive string, staleRounds int) {
	sig := progressSignature(r.Verdicts)
	if a.started && sig == a.lastSig {
		a.staleRounds++
		return NoProgressPrompt(a.staleRounds, sig), a.staleRounds
	}
	a.started = true
	a.lastSig = sig
	a.staleRounds = 0
	return "", 0
}

// auditClaimReason 检查一轮记录能否支撑「完成」声明。proven=true 表示
// 三条证据条件全部通过。条件按「先实现、再评估、后证据」排列，返回
// 第一个命中的名字 —— reason 是给操作员和续跑提示词看的诊断，不是布尔。
func auditClaimReason(r AuditRound) (reason string, proven bool) {
	if !r.Implemented {
		return "implementer reported an error this round", false
	}
	for _, v := range r.Verdicts {
		if v.Evaluator == "error" {
			// Run 把评估器的 Go error 包装成这个占位判决：它没跑成，
			// 它的任何「判决」都不存在。judge 若在含这种判决的轮里判
			// Complete，审计拦下的正是「在缺考的科目上宣布毕业」。
			return "an evaluator failed to run this round", false
		}
	}
	for _, v := range r.Verdicts {
		if v.Pass && strings.TrimSpace(v.Evidence) != "" {
			return "", true
		}
	}
	return "no evaluator recorded evidence for its pass verdict", false
}

// progressSignature 计算一轮的进度签名：pass 数 + 排序后的失败 gap 集。
// 刻意不含证据文本与实现者结果串 —— 证据逐轮会有噪声，而失败集合不变
// 的「新证据」是假进展最常见的形态。
func progressSignature(verdicts []EvalVerdict) string {
	pass := 0
	gaps := []string{}
	for _, v := range verdicts {
		if v.Pass {
			pass++
			continue
		}
		gaps = append(gaps, v.Gaps...)
	}
	sort.Strings(gaps)
	return fmt.Sprintf("pass=%d;gaps=%s", pass, strings.Join(gaps, "|"))
}

// joinDirectives 合并同轮可能同时产生的两条注入指令（veto 与无进展纠偏）。
// 两者可以同时命中：一个无凭据的完成声明通常也伴随签名不变。合并不去重 ——
// 两段话指向两个不同的失职（声称没证据 + 方法没变化），各说各的。
func joinDirectives(a, b string) string {
	switch {
	case a == "":
		return b
	case b == "":
		return a
	}
	return a + "\n" + b
}

// UnprovenCompletionPrompt 是 W-F-07 的防伪完成提示词本体：veto 轮注入
// 下一轮的第一条实现指令。它只做一件事 —— 把「声称完成」换成「证明完成」：
// 亲跑验收测试、给出工件、坦白未竟部分。措辞刻意不描述「该怎么实现」，
// 那是计划的事；它描述的只是**什么样的回话不算完成**。
func UnprovenCompletionPrompt(rounds int, reason string) string {
	return fmt.Sprintf(
		"Your previous turn claimed completion, but the loop could not verify it "+
			"(round %d without verifiable evidence; reason: %s). Do not restate the claim. "+
			"This round you must PROVE completion: (1) run the plan's acceptance tests "+
			"yourself and include their output; (2) name the concrete artifacts you "+
			"changed or produced; (3) if any part is unfinished, say so explicitly — "+
			"an honest incomplete report ends this loop's suspicion, a repeated claim deepens it.",
		rounds, reason)
}

// NoProgressPrompt 是无进展轮注入的纠偏提示词。它点名签名里仍然失败的
// gap，要求换方法而不是重复旧方法 —— 无进展的循环里模型最常见的行为
// 是把同一个方法再跑一遍并换个措辞描述它。
func NoProgressPrompt(staleRounds int, sig string) string {
	return fmt.Sprintf(
		"The last %d iterations produced no measurable progress (unchanged verdict "+
			"signature: %s). Do not repeat the previous approach. Identify why the "+
			"failing checks still fail, change the method, and address those specific gaps.",
		staleRounds, sig)
}
