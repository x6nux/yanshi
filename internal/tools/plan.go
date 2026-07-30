// Plan & checklist tools: update_plan + checklist_{write,add,update,list} + todo_* aliases.
//
// 这些工具共享 4 个 kernel：整组替换 / 增 / 改 / 读。同一 kernel 通过 SyncStream
// 包装后被注册成多个 *GuardedTool：update_plan / checklist_write / todo_write
// 都是"整组替换"的不同 alias 名，让模型可以选择自己习惯的命名。
//
// 强制约束：所有 emit 都是 EventPlanUpdate 或 EventChecklistUpdate（typed
// work.Event），由 orchestrator 翻译成 proto.ServerFrame。
package tools

import (
	"context"
	"time"

	"github.com/cloudwego/eino/schema"
	"github.com/x6nux/yanshi/internal/task/work"
)

// PlanTools 暴露 plan/checklist/todo 工具族。todo_* 是 checklist_* 的 alias。
type PlanTools struct {
	UpdatePlan *GuardedTool // update_plan：整组替换

	ChecklistWrite  *GuardedTool // checklist_write：alias for update_plan
	ChecklistAdd    *GuardedTool // checklist_add：追加一条 item
	ChecklistUpdate *GuardedTool // checklist_update：patch 一条 item
	ChecklistList   *GuardedTool // checklist_list：读取 task 的完整 checklist

	TodoWrite  *GuardedTool // todo_write：alias for checklist_write
	TodoAdd    *GuardedTool // todo_add：alias for checklist_add
	TodoUpdate *GuardedTool // todo_update：alias for checklist_update
	TodoList   *GuardedTool // todo_list：alias for checklist_list
}

// NewPlanTools 构造一组 plan 工具。所有 kernel 是 PlanTools 的方法，
// alias 工具通过 SyncStream(method) 共享同一 kernel。
func NewPlanTools() *PlanTools {
	pt := &PlanTools{}

	// 整组替换 kernel
	pt.UpdatePlan = NewGuardedTool(
		"update_plan", "Update Plan",
		"Replace the full checklist of a task.",
		30*time.Second,
		params(map[string]*schema.ParameterInfo{
			"task_id": {Type: schema.String, Desc: "task id", Required: true},
			"rows":    {Type: schema.Array, Desc: "new checklist rows (replaces existing)"},
		}),
		SyncStream(pt.updatePlan),
	)
	pt.ChecklistWrite = aliasGuardedTool("checklist_write", "Checklist Write",
		"Replace the full checklist (alias of update_plan).",
		params(map[string]*schema.ParameterInfo{
			"task_id": {Type: schema.String, Desc: "task id", Required: true},
			"rows":    {Type: schema.Array, Desc: "new checklist rows"},
		}), SyncStream(pt.updatePlan))
	pt.TodoWrite = aliasGuardedTool("todo_write", "Todo Write",
		"Replace the full todo list (alias of update_plan).",
		params(map[string]*schema.ParameterInfo{
			"task_id": {Type: schema.String, Desc: "task id", Required: true},
			"rows":    {Type: schema.Array, Desc: "new todo rows"},
		}), SyncStream(pt.updatePlan))

	// 追加 kernel
	pt.ChecklistAdd = NewGuardedTool(
		"checklist_add", "Checklist Add",
		"Append a new item to the checklist.",
		30*time.Second,
		params(map[string]*schema.ParameterInfo{
			"task_id": {Type: schema.String, Desc: "task id", Required: true},
			"content": {Type: schema.String, Desc: "item text", Required: true},
		}),
		SyncStream(pt.checklistAdd),
	)
	pt.TodoAdd = aliasGuardedTool("todo_add", "Todo Add",
		"Append a new todo item.",
		params(map[string]*schema.ParameterInfo{
			"task_id": {Type: schema.String, Desc: "task id", Required: true},
			"content": {Type: schema.String, Desc: "item text", Required: true},
		}), SyncStream(pt.checklistAdd))

	// 改 kernel
	pt.ChecklistUpdate = NewGuardedTool(
		"checklist_update", "Checklist Update",
		"Patch a single checklist item (content and/or status).",
		30*time.Second,
		params(map[string]*schema.ParameterInfo{
			"task_id": {Type: schema.String, Desc: "task id", Required: true},
			"item_id": {Type: schema.Integer, Desc: "item id", Required: true},
			"content": {Type: schema.String, Desc: "new content (omit to leave unchanged)"},
			"status":  {Type: schema.String, Desc: "new status: pending/in_progress/done"},
		}),
		SyncStream(pt.checklistUpdate),
	)
	pt.TodoUpdate = aliasGuardedTool("todo_update", "Todo Update",
		"Patch a single todo item.",
		params(map[string]*schema.ParameterInfo{
			"task_id": {Type: schema.String, Desc: "task id", Required: true},
			"item_id": {Type: schema.Integer, Desc: "item id", Required: true},
			"content": {Type: schema.String, Desc: "new content (omit to leave unchanged)"},
			"status":  {Type: schema.String, Desc: "new status: pending/in_progress/done"},
		}), SyncStream(pt.checklistUpdate))

	// 读 kernel
	pt.ChecklistList = NewGuardedTool(
		"checklist_list", "Checklist List",
		"Read a task and return its full checklist.",
		30*time.Second,
		params(map[string]*schema.ParameterInfo{
			"task_id": {Type: schema.String, Desc: "task id", Required: true},
		}),
		SyncStream(pt.checklistList),
	)
	pt.TodoList = aliasGuardedTool("todo_list", "Todo List",
		"Read a task and return its full todo list.",
		params(map[string]*schema.ParameterInfo{
			"task_id": {Type: schema.String, Desc: "task id", Required: true},
		}), SyncStream(pt.checklistList))

	return pt
}

// Tools returns all plan tools as a slice for convenience.
func (p *PlanTools) Tools() []*GuardedTool {
	return []*GuardedTool{
		p.UpdatePlan,
		p.ChecklistWrite, p.ChecklistAdd, p.ChecklistUpdate, p.ChecklistList,
		p.TodoWrite, p.TodoAdd, p.TodoUpdate, p.TodoList,
	}
}

// aliasGuardedTool 是 NewGuardedTool 的薄 wrapper，仅用于强调 alias 工具
// 在 display/desc 上不同、但 kernel 是同一个。当前与 NewGuardedTool 等价，
// 留作未来对 alias 行为分化的扩展点（如不同的 display 字符串前缀）。
func aliasGuardedTool(name, display, desc string, p *schema.ParamsOneOf, kernel StreamFunc) *GuardedTool {
	return NewGuardedTool(name, display, desc, 30*time.Second, p, kernel)
}

// --- kernels ---

type updatePlanArgs struct {
	TaskID string               `json:"task_id"`
	Rows   []work.ChecklistItem `json:"rows"`
}

// updatePlan 整组替换 checklist；空 rows 也合法（清空）。
// emit 的 EventChecklistUpdate 中 Checklist.Items 是 empty-slice（非 nil），
// 让 TUI / orchestrator 能区分"清空"与"未发事件"。
func (p *PlanTools) updatePlan(ctx context.Context, argsJSON string) (string, error) {
	var args updatePlanArgs
	if err := ParseArgs(argsJSON, &args); err != nil {
		return "", err
	}
	manager, err := requireTaskManager(ctx)
	if err != nil {
		return "", err
	}
	// 用 make([]T, 0) 而非 append(nil, ...) 以保证 empty-rows 时 Items 是非 nil 的
	// empty slice —— 区分"清空"与"未发事件"（已决策约束 §13）。
	items := make([]work.ChecklistItem, 0, len(args.Rows))
	items = append(items, args.Rows...)
	task, err := manager.SetChecklist(ctx, args.TaskID, work.Checklist{Items: items})
	if err != nil {
		return "", err
	}
	EmitWorkEvent(ctx, work.Event{
		Kind: work.EventPlanUpdate, TaskID: task.ID,
		Checklist: task.Checklist,
	})
	return toJSON(struct {
		TaskID    string         `json:"task_id"`
		Checklist work.Checklist `json:"checklist"`
	}{TaskID: task.ID, Checklist: task.Checklist}), nil
}

type checklistAddArgs struct {
	TaskID  string `json:"task_id"`
	Content string `json:"content"`
}

// checklistAdd 追加一条新 item；item id 由 manager 分配。
func (p *PlanTools) checklistAdd(ctx context.Context, argsJSON string) (string, error) {
	var args checklistAddArgs
	if err := ParseArgs(argsJSON, &args); err != nil {
		return "", err
	}
	manager, err := requireTaskManager(ctx)
	if err != nil {
		return "", err
	}
	task, err := manager.AddChecklistItem(ctx, args.TaskID, args.Content)
	if err != nil {
		return "", err
	}
	EmitWorkEvent(ctx, work.Event{
		Kind: work.EventChecklistUpdate, TaskID: task.ID,
		Checklist: task.Checklist,
	})
	return toJSON(struct {
		TaskID    string         `json:"task_id"`
		Checklist work.Checklist `json:"checklist"`
	}{TaskID: task.ID, Checklist: task.Checklist}), nil
}

type checklistUpdateArgs struct {
	TaskID  string `json:"task_id"`
	ItemID  int    `json:"item_id"`
	Content string `json:"content"`
	Status  string `json:"status"`
}

// checklistUpdate patch 一条 item；空 content/status 表示不改这一列。
func (p *PlanTools) checklistUpdate(ctx context.Context, argsJSON string) (string, error) {
	var args checklistUpdateArgs
	if err := ParseArgs(argsJSON, &args); err != nil {
		return "", err
	}
	manager, err := requireTaskManager(ctx)
	if err != nil {
		return "", err
	}
	task, err := manager.PatchChecklistItem(ctx, args.TaskID, args.ItemID, args.Content, work.ChecklistItemStatus(args.Status))
	if err != nil {
		return "", err
	}
	EmitWorkEvent(ctx, work.Event{
		Kind: work.EventChecklistUpdate, TaskID: task.ID,
		Checklist: task.Checklist,
	})
	return toJSON(struct {
		TaskID    string         `json:"task_id"`
		Checklist work.Checklist `json:"checklist"`
	}{TaskID: task.ID, Checklist: task.Checklist}), nil
}

type checklistListArgs struct {
	TaskID string `json:"task_id"`
}

// checklistList 读 task 并返回完整 checklist（也包含 gates/timeline，工具层不裁剪）。
func (p *PlanTools) checklistList(ctx context.Context, argsJSON string) (string, error) {
	var args checklistListArgs
	if err := ParseArgs(argsJSON, &args); err != nil {
		return "", err
	}
	manager, err := requireTaskManager(ctx)
	if err != nil {
		return "", err
	}
	task, err := manager.Read(ctx, args.TaskID)
	if err != nil {
		return "", err
	}
	// 注意：list 是读路径，不 emit EventChecklistUpdate（避免 list 触发 TUI 重复刷新）。
	return toJSON(struct {
		TaskID    string         `json:"task_id"`
		Checklist work.Checklist `json:"checklist"`
	}{TaskID: task.ID, Checklist: task.Checklist}), nil
}
