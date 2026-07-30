// per-turn context seams for the tools package.
//
// 这些 context value 端口是工具层（task_create / update_plan / task_gate_run /
// artifact_read / ...）读取"本 turn 的 thread / plan / task manager / event sink"
// 的唯一通道。它们都是 **per-turn**：orchestrator 在每个 ReAct 迭代前注入一份，
// 不存在共享 Orchestrator 字段 —— 这与已决策约束 §1 一致。
//
// 与 permctx.go 里的 PermissionCallback/Profile 是同一类模式：横切状态经 ctx 流动，
// 不进工具参数。
package tools

import (
	"context"

	"github.com/x6nux/yanshi/internal/guard"
	"github.com/x6nux/yanshi/internal/task/work"
)

// taskManagerKey 是 *work.ManagerLike 在 context 中的键。
type taskManagerKey struct{}

// planModeKey 是 bool Plan mode 标志在 context 中的键。
type planModeKey struct{}

// threadLinkKey 是 ThreadLink 在 context 中的键。
type threadLinkKey struct{}

// ThreadLink 把 thread/turn 标识捆绑为单个 value，避免两个独立 key 在并发
// context 派生时出现 "thread 来自 turn A，turn ID 来自 turn B" 的撕裂。
type ThreadLink struct {
	ThreadID string
	TurnID   string
}

// TaskManagerCtx 是 work.ManagerLike 的类型别名，保留旧调用方"TaskManagerCtx"
// 的命名习惯。新代码请直接使用 work.ManagerLike。
type TaskManagerCtx = work.ManagerLike

// WithTaskManager 把 manager 注入 ctx。nil manager 是 no-op：
// 子工具看到"没有 manager"（TaskManagerFromContext 返回 false），
// 不会误把空接口塞进 ctx 让后续调用方拿不到 nil 检查机会。
func WithTaskManager(ctx context.Context, manager work.ManagerLike) context.Context {
	if manager == nil {
		return ctx
	}
	return context.WithValue(ctx, taskManagerKey{}, manager)
}

// TaskManagerFromContext 读取绑定的 manager；不存在或为 nil 时 ok=false。
func TaskManagerFromContext(ctx context.Context) (TaskManagerCtx, bool) {
	manager, ok := ctx.Value(taskManagerKey{}).(work.ManagerLike)
	return manager, ok && manager != nil
}

// WithPlanMode 把 plan mode active 标志注入 ctx。Plan mode 是只读模式，
// orchestrator 在工具 dispatch 前据此选择 plan-safe 工具子集。
func WithPlanMode(ctx context.Context, active bool) context.Context {
	return context.WithValue(ctx, planModeKey{}, active)
}

// PlanModeActive 读 ctx 中的 plan 标志；缺失视为 false（默认 runtime mode）。
func PlanModeActive(ctx context.Context) bool {
	active, _ := ctx.Value(planModeKey{}).(bool)
	return active
}

// WithThreadLink 把 thread/turn 标识捆绑注入 ctx。空字符串也是合法值
// （比如进程内 FakeBackend 没有真实 thread）。
func WithThreadLink(ctx context.Context, threadID, turnID string) context.Context {
	return context.WithValue(ctx, threadLinkKey{}, ThreadLink{ThreadID: threadID, TurnID: turnID})
}

// ThreadLinkFromContext 读出 thread link；不存在时 ok=false。
func ThreadLinkFromContext(ctx context.Context) (ThreadLink, bool) {
	link, ok := ctx.Value(threadLinkKey{}).(ThreadLink)
	return link, ok
}

// PlanToolAllowed 是 guard.PlanToolAllowed 的跨包 re-export。
// orchestrator 与 ws.go 已经 import tools，再 import guard 只为一个函数会
// 散落各处；统一从 tools 取它，与 WithProfile / PlanModeActive / ProfileFromContext
// 同居一个命名空间。canonical 真值仍是 guard.planAllowedTools。
func PlanToolAllowed(name string) bool { return guard.PlanToolAllowed(name) }
