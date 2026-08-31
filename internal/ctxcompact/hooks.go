package ctxcompact

// W-F-08（INF4 / B29）：压缩生命周期 hook 总线。
//
// 总线住在 ctxcompact（spec 的落点），而不是任何一条调用路径里 —— 这是
// 「三条压缩路径共用同一总线」的机器保证：pre-turn（MaybeCompact/
// ForceCompactWithOptions）、mid-turn（einollm.CompactingModel）、手动
// /compact（compactNow → ForceCompact）全部汇入 Run / OpenNewWindow，
// 事件在这里发一次，三条路径同时覆盖。在调用方各自发事件，漏掉一条的
// 方式就是「机制存在但没装在这条路上」—— 那正是本条要防的失效形状。
//
// ── Ruling：生命周期 hook 只观察，不判决；失败 fail-open ──
//
// 与 PreToolUse hook（fail-closed，ADR-0027）方向相反，这是刻意的：
//
//   - PreToolUse hook 判决的对象是**模型发起的调用**，fail-open 时「hook
//     拦 X」静默退化为「X 允许」—— 那是失败伪装成成功。压缩 hook 观察的
//     是一次**机械事件**（窗口到了、历史要被摘要替换），它没有「允许」
//     可给；它唯一能造成的差别是是否继续压缩。
//   - 压缩失败（closed）的方向是把对话历史**保持原样**送出去 —— 那是比
//     被摘要替换更大的上下文，最终变成操作员可见的 provider 长度错误。
//     让一个旁观 hook 的崩溃升级成这件事，等于把「观察者坏了」误报成
//     「上下文出问题了」。
//   - 压缩已有的唯一否决门是 durability-first（C1：历史没落盘就拒绝压缩）。
//     它保护的是数据不丢失；hook 观察门加不进去 —— hook 的输入是计数，
//     没有任何 hook 能看到「历史没落盘」之外的新事实。
//
// 所以：sink 内部的失败（hook 进程崩溃、超时、输出读不懂）一律记日志后
// 继续；事件消费方无权修改 Run 的返回值。
//
// ── 事件形状 ──
//
// 事件只携带计数与结局（TokensBefore/After、Overflow、Fallback、Failure），
// 不携带任何消息原文 —— hook 是操作员配置的外部程序，历史内容里可能有
// 凭据（压缩路径上还有 redactor，但那保护的是摘要模型，不是 hook）。

import (
	"context"
	"log/slog"
)

// 生命周期事件名与触发来源。写成常量集合让后续接新事件/新路径时有唯一
// 可对账的名字表；hook 程序看到的 event / trigger 字段就是这些字面值。
const (
	// LifecyclePreCompact 在摘要调用发起前发出。
	LifecyclePreCompact = "pre_compact"
	// LifecyclePostCompact 在压缩尝试出结局后发出（成功、fallback、纯折叠
	// 与失败都是「结局」，用 Failure/Fallback 字段区分）。
	LifecyclePostCompact = "post_compact"
)

// 触发来源（Trigger 字段）。四条路径各有名字；新路径必须来这里登记，
// 否则 hook 看到的是空串。
const (
	// TriggerPreTurn：user_message 回合开始前的阈值压缩（WS/SSE 传输层）。
	TriggerPreTurn = "pre_turn"
	// TriggerMidTurn：ADK ReAct 迭代之间的阈值压缩（CompactingModel）。
	TriggerMidTurn = "mid_turn"
	// TriggerManual：用户显式 /compact（force 路径）。
	TriggerManual = "manual"
	// TriggerNewWindow：模型经 context_new_window 工具主动要求的新窗口。
	TriggerNewWindow = "new_window"
)

// LifecycleEvent 是发给压缩生命周期 hook 的一次事件。
type LifecycleEvent struct {
	// Phase 是 LifecyclePreCompact / LifecyclePostCompact 之一。
	Phase string `json:"event"`
	// Trigger 是本次压缩的来源路径（四个 Trigger* 常量之一）。
	Trigger string `json:"trigger"`
	// TokensBefore 是压缩前历史的估算 token 数。
	TokensBefore int `json:"tokens_before"`
	// TokensAfter 是压缩后历史的估算 token 数。pre 事件为 0（还没算）。
	TokensAfter int `json:"tokens_after,omitempty"`
	// Overflow 报告压缩后的历史仍超窗（C9）。这是观察字段：溢出的处置权
	// 在发送方（CheckContextLimit），hook 改变不了它。
	Overflow bool `json:"overflow,omitempty"`
	// Fallback 报告走了 W-C-04 的 pins-only 降级（摘要模型没给出摘要）。
	Fallback bool `json:"fallback,omitempty"`
	// Failure 是非空时的失败原因（EMPTY/QUALITY 门或配置/接线失败）。
	// post 事件专有；此时调用方会保留原历史。
	Failure string `json:"failure,omitempty"`
}

// LifecycleSink 消费一条生命周期事件。它**不得**修改压缩的结局 —— 事件
// 是通知，不是询问。sink 自身的 panic/失败按 Ruling fail-open：总线在
// emit 处 recover 并记日志，一次 sink 崩溃不带走压缩。
type LifecycleSink func(ctx context.Context, ev LifecycleEvent)

// lifecycleSinkKey 是总线在 ctx 里的 key。
type lifecycleSinkKey struct{}

// WithLifecycleSink 把压缩生命周期 sink 绑进 ctx。nil sink 原样返回 ctx，
// Run 查不到总线就不发事件 —— 未接线的调用方行为与引入前一致。
func WithLifecycleSink(ctx context.Context, sink LifecycleSink) context.Context {
	if sink == nil {
		return ctx
	}
	return context.WithValue(ctx, lifecycleSinkKey{}, sink)
}

// lifecycleSinkFromContext 读回绑定的 sink；未绑定时 ok=false。
func lifecycleSinkFromContext(ctx context.Context) (LifecycleSink, bool) {
	sink, ok := ctx.Value(lifecycleSinkKey{}).(LifecycleSink)
	return sink, ok && sink != nil
}

// emitLifecycle 把一条事件交给 ctx 里的 sink。总线没绑、事件为空、sink
// panic 三种情况都在这里消化：压缩主体完全感知不到它们。
func emitLifecycle(ctx context.Context, ev LifecycleEvent) {
	if ev.Phase == "" {
		return
	}
	sink, ok := lifecycleSinkFromContext(ctx)
	if !ok {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			slog.Warn("ctxcompact: lifecycle sink panicked; compaction continues (fail-open)",
				"panic", r, "event", ev.Phase, "trigger", ev.Trigger)
		}
	}()
	sink(ctx, ev)
}
