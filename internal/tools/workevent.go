// Typed work.Event callback context seam.
//
// 这是 durable task 工具（task_create / update_plan / task_gate_run / ...）
// 把领域事件回流到 orchestrator 的唯一通道。orchestrator 在每个 turn 起始处
// 注入一个 WorkEventCallback，工具调用 EmitWorkEvent 后，orchestrator 把
// work.Event 转成 proto.ServerFrame 经 WS/SSE 发出。
//
// 设计原则（与已决策约束 §1 一致）：callback 是 per-turn 状态，绝不挂在
// 共享 Orchestrator 上。
package tools

import (
	"context"

	"github.com/x6nux/yanshi/internal/task/work"
)

// workEventCallbackKey 是 WorkEventCallback 在 context 中的键。
type workEventCallbackKey struct{}

// WorkEventCallback 是 work.Event 的 sink 函数类型。callback 必须是
// 非阻塞的（orchestrator 内部 async dispatch 到 WS/SSE writer）。
type WorkEventCallback func(work.Event)

// WithWorkEventCallback 把 callback 注入 ctx。nil callback 是 no-op，
// 不修改 ctx —— 这让"EmitWorkEvent 没注册"等价于"丢弃事件"，工具侧不需要
// 额外的 if 已注册检查。
func WithWorkEventCallback(ctx context.Context, callback WorkEventCallback) context.Context {
	if callback == nil {
		return ctx
	}
	return context.WithValue(ctx, workEventCallbackKey{}, callback)
}

// WorkEventCallbackFromContext 读出绑定的 callback；不存在时返回 nil。
func WorkEventCallbackFromContext(ctx context.Context) WorkEventCallback {
	callback, _ := ctx.Value(workEventCallbackKey{}).(WorkEventCallback)
	return callback
}

// EmitWorkEvent 是工具层的便捷方法：没有 callback 时是 no-op。每次 durable
// task 变更（创建、状态转移、checklist 更新、gate 记录）都经此入口回流。
func EmitWorkEvent(ctx context.Context, event work.Event) {
	if callback := WorkEventCallbackFromContext(ctx); callback != nil {
		callback(event)
	}
}
