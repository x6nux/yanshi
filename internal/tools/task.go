// durable task tools: task_create / task_list / task_read / task_cancel.
//
// 这些是 work.ManagerLike 在工具层的薄壳：从 context 取 manager、调用对应方法、
// 经 EmitWorkEvent 把领域事件回流到 orchestrator、把结果序列化成模型可读 JSON。
//
// task_cancel 是 forcePromptTools 的一员（已决策约束 §5）：在 Authorize 中
// 永远走 callback，无法被 allowlist/always_allow 短路。其它三个工具按 profile
// 正常 dispatch。
package tools

import (
	"context"
	"errors"
	"time"

	"github.com/cloudwego/eino/schema"
	"github.com/x6nux/yanshi/internal/task/work"
)

// TaskTools 暴露 durable task 工具族：Create/List/Read/Cancel。
// 每个 *GuardedTool 字段可直接注册进 ADK ToolsNode。
type TaskTools struct {
	Create *GuardedTool
	List   *GuardedTool
	Read   *GuardedTool
	Cancel *GuardedTool
}

// NewTaskTools 构造一组 durable task 工具。timeout 是每个工具的默认执行超时
// （durable task 操作都是同步 SQLite，30 秒非常宽松）。
func NewTaskTools() *TaskTools {
	tt := &TaskTools{}
	tt.Create = NewGuardedTool(
		"task_create", "Task Create",
		"Create a durable task. Use dispatch=true to also submit to the broker.",
		30*time.Second,
		params(map[string]*schema.ParameterInfo{
			"title":    {Type: schema.String, Desc: "short title", Required: true},
			"prompt":   {Type: schema.String, Desc: "full prompt for the task", Required: true},
			"dispatch": {Type: schema.Boolean, Desc: "if true, also submit to the broker"},
		}),
		SyncStream(tt.runCreate),
	)
	tt.List = NewGuardedTool(
		"task_list", "Task List",
		"List durable tasks. Pass all=true to ignore the current thread filter.",
		30*time.Second,
		params(map[string]*schema.ParameterInfo{
			"limit": {Type: schema.Integer, Desc: "max results (default 20)"},
			"all":   {Type: schema.Boolean, Desc: "if true, do not filter by current thread"},
		}),
		SyncStream(tt.runList),
	)
	tt.Read = NewGuardedTool(
		"task_read", "Task Read",
		"Read a single durable task by id, including checklist, gates, and timeline.",
		30*time.Second,
		params(map[string]*schema.ParameterInfo{
			"id": {Type: schema.String, Desc: "task id", Required: true},
		}),
		SyncStream(tt.runRead),
	)
	tt.Cancel = NewGuardedTool(
		"task_cancel", "Task Cancel",
		"Cancel a durable task. ALWAYS requires explicit user approval (force-prompt).",
		30*time.Second,
		params(map[string]*schema.ParameterInfo{
			"id": {Type: schema.String, Desc: "task id", Required: true},
		}),
		SyncStream(tt.runCancel),
	)
	return tt
}

// Tools returns all task tools as a slice for convenience.
func (t *TaskTools) Tools() []*GuardedTool {
	return []*GuardedTool{t.Create, t.List, t.Read, t.Cancel}
}

// requireTaskManager 读出绑定的 manager；缺失返回 error。是所有 task 工具的入口检查。
func requireTaskManager(ctx context.Context) (work.ManagerLike, error) {
	manager, ok := TaskManagerFromContext(ctx)
	if !ok {
		return nil, errors.New("tools: task manager unavailable")
	}
	return manager, nil
}

type taskCreateArgs struct {
	Title    string `json:"title"`
	Prompt   string `json:"prompt"`
	Dispatch bool   `json:"dispatch"`
}

func (t *TaskTools) runCreate(ctx context.Context, argsJSON string) (string, error) {
	var args taskCreateArgs
	if err := ParseArgs(argsJSON, &args); err != nil {
		return "", err
	}
	manager, err := requireTaskManager(ctx)
	if err != nil {
		return "", err
	}
	link, _ := ThreadLinkFromContext(ctx)
	task, err := manager.Create(ctx, work.CreateReq{
		Title:    args.Title,
		Prompt:   args.Prompt,
		ThreadID: link.ThreadID,
		TurnID:   link.TurnID,
		Dispatch: args.Dispatch,
	})
	if err != nil {
		return "", err
	}
	EmitWorkEvent(ctx, work.Event{Kind: work.EventTaskUpdate, Task: task, TaskID: task.ID})
	return toJSON(struct {
		Task *work.WorkTask `json:"task"`
	}{Task: task}), nil
}

type taskListArgs struct {
	Limit int  `json:"limit"`
	All   bool `json:"all"`
}

func (t *TaskTools) runList(ctx context.Context, argsJSON string) (string, error) {
	var args taskListArgs
	if err := ParseArgs(argsJSON, &args); err != nil {
		return "", err
	}
	manager, err := requireTaskManager(ctx)
	if err != nil {
		return "", err
	}
	limit := args.Limit
	if limit <= 0 {
		limit = 20
	}
	threadID := ""
	if !args.All {
		// all=false 时按当前 thread filter；没有 thread link 仍传空（等同 all=true）
		if link, ok := ThreadLinkFromContext(ctx); ok {
			threadID = link.ThreadID
		}
	}
	summaries, err := manager.List(ctx, limit, threadID)
	if err != nil {
		return "", err
	}
	return toJSON(struct {
		Tasks []work.Summary `json:"tasks"`
	}{Tasks: summaries}), nil
}

type taskReadArgs struct {
	ID string `json:"id"`
}

func (t *TaskTools) runRead(ctx context.Context, argsJSON string) (string, error) {
	var args taskReadArgs
	if err := ParseArgs(argsJSON, &args); err != nil {
		return "", err
	}
	manager, err := requireTaskManager(ctx)
	if err != nil {
		return "", err
	}
	task, err := manager.Read(ctx, args.ID)
	if err != nil {
		return "", err
	}
	return toJSON(struct {
		Task *work.WorkTask `json:"task"`
	}{Task: task}), nil
}

type taskCancelArgs struct {
	ID string `json:"id"`
}

func (t *TaskTools) runCancel(ctx context.Context, argsJSON string) (string, error) {
	var args taskCancelArgs
	if err := ParseArgs(argsJSON, &args); err != nil {
		return "", err
	}
	manager, err := requireTaskManager(ctx)
	if err != nil {
		return "", err
	}
	task, err := manager.Cancel(ctx, args.ID, "agent")
	if err != nil {
		return "", err
	}
	EmitWorkEvent(ctx, work.Event{Kind: work.EventTaskUpdate, Task: task, TaskID: task.ID})
	return toJSON(struct {
		Task *work.WorkTask `json:"task"`
	}{Task: task}), nil
}
