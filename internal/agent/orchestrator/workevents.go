// Work.Event → proto.ServerFrame 的映射。
//
// orchestrator.withTurnContext 把 EmitWorkFrame 回调包装成 WorkEventCallback，
// 工具层（task_create / update_plan / task_cancel / task_gate_run / ...）emit
// 的 work.Event 经过本文件的 workEventFrame 转成 proto.ServerFrame，再经
// WS/SSE 各自的共享 writer 发出（Task 12/13）。
//
// 这是唯一的 event→frame mapper（已决策约束 §12: work 包不 import proto）。
package orchestrator

import (
	"github.com/x6nux/yanshi/internal/proto"
	"github.com/x6nux/yanshi/internal/task/work"
)

// workEventFrame 把 work.Event 映射为一个 proto.ServerFrame。
// test 断言：EventPlanUpdate → ServerFrame.Checklist.Items 是 empty-slice 而非
// nil（让 TUI 能区分"清空 checklist"与"未发过 checklist 事件"）。
func workEventFrame(event work.Event) proto.ServerFrame {
	switch event.Kind {
	case work.EventPlanUpdate:
		return proto.NewPlanUpdate(event.TaskID, event.Checklist.Items)
	case work.EventChecklistUpdate:
		return proto.NewChecklistUpdate(event.TaskID, event.Checklist.Items)
	default:
		return proto.NewTaskUpdate(event.Task)
	}
}
