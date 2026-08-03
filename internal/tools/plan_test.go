package tools

import (
	"context"
	"testing"

	"github.com/cloudwego/eino/components/tool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/x6nux/yanshi/internal/guard"
	"github.com/x6nux/yanshi/internal/task/work"
)

// TestUpdatePlan_ReplaceAll: update_plan 整组替换 checklist。
func TestUpdatePlan_ReplaceAll(t *testing.T) {
	manager := work.NewFakeManager()
	task, err := manager.Create(context.Background(), work.CreateReq{Title: "x", Prompt: "p"})
	require.NoError(t, err)

	ctx := WithProfile(WithTaskManager(context.Background(), manager), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"*"}},
	})
	pt := NewPlanTools()
	out, err := runTool(ctx, pt.UpdatePlan, `{"task_id":"`+task.ID+`","rows":[{"id":1,"content":"a","status":"pending"},{"id":2,"content":"b","status":"done"}]}`)
	require.NoError(t, err)
	assert.Contains(t, out, task.ID)
	assert.Contains(t, out, `"content":"a"`)
	assert.Contains(t, out, `"status":"done"`)
}

// TestChecklist_AddUpdateList: add 一条新 item，再 update 状态，再 list。
func TestChecklist_AddUpdateList(t *testing.T) {
	manager := work.NewFakeManager()
	task, err := manager.Create(context.Background(), work.CreateReq{Title: "x", Prompt: "p"})
	require.NoError(t, err)
	require.NoError(t, manager.Start(context.Background(), task.ID))
	// 需要把 task 从 pending → running 之后才能 add；或者直接 add 也行
	// （SetChecklist 不要求 task 在某个状态）。
	require.NoError(t, manager.Finish(context.Background(), task.ID, work.StatusCompleted, "done"))

	// 重新创建一个保持 pending 的 task 来测 add
	task2, err := manager.Create(context.Background(), work.CreateReq{Title: "y", Prompt: "p"})
	require.NoError(t, err)

	ctx := WithProfile(WithTaskManager(context.Background(), manager), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"*"}},
	})
	pt := NewPlanTools()

	// add
	out, err := runTool(ctx, pt.ChecklistAdd, `{"task_id":"`+task2.ID+`","content":"first"}`)
	require.NoError(t, err)
	assert.Contains(t, out, `"content":"first"`)

	// update the first item to done
	out, err = runTool(ctx, pt.ChecklistUpdate, `{"task_id":"`+task2.ID+`","item_id":1,"status":"done"}`)
	require.NoError(t, err)
	assert.Contains(t, out, `"status":"done"`)

	// list
	out, err = runTool(ctx, pt.ChecklistList, `{"task_id":"`+task2.ID+`"}`)
	require.NoError(t, err)
	assert.Contains(t, out, task2.ID)
}

// TestUpdatePlan_EmptyRowsEmitsNonNilEvent: 空 rows 仍 emit 一个
// EventChecklistUpdate（checklist 字段非 nil 但 Items 为空数组）。
func TestUpdatePlan_EmptyRowsEmitsNonNilEvent(t *testing.T) {
	manager := work.NewFakeManager()
	task, err := manager.Create(context.Background(), work.CreateReq{Title: "x", Prompt: "p"})
	require.NoError(t, err)

	var events []work.Event
	cb := WorkEventCallback(func(e work.Event) {
		events = append(events, e)
	})
	ctx := WithWorkEventCallback(WithProfile(WithTaskManager(context.Background(), manager),
		guard.PermissionProfile{Tools: guard.ToolsPerm{Allow: []string{"*"}}}), cb)
	pt := NewPlanTools()
	_, err = runTool(ctx, pt.UpdatePlan, `{"task_id":"`+task.ID+`","rows":[]}`)
	require.NoError(t, err)
	require.Len(t, events, 1)
	assert.Equal(t, work.EventPlanUpdate, events[0].Kind)
	// Checklist 字段是非 nil 的 zero-value（items slice 为空，但 struct 本身存在）
	assert.NotNil(t, events[0].Checklist.Items)
	assert.Empty(t, events[0].Checklist.Items)
}

// TestTodoAliases_NameAndBehavior: todo_write/add/update/list 的 Info().Name
// 是 alias 名，但行为委托同一组 kernel。
func TestTodoAliases_NameAndBehavior(t *testing.T) {
	pt := NewPlanTools()
	ctx := context.Background()

	// 名字是 alias
	todoNames := map[string]tool.InvokableTool{
		"todo_write":  pt.TodoWrite,
		"todo_add":    pt.TodoAdd,
		"todo_update": pt.TodoUpdate,
		"todo_list":   pt.TodoList,
	}
	for expectedName, tt := range todoNames {
		info, err := tt.Info(ctx)
		require.NoError(t, err)
		assert.Equal(t, expectedName, info.Name)
	}

	// 行为委托同一 kernel：todo_write 等同于 update_plan（empty rows 也 OK）
	manager := work.NewFakeManager()
	task, err := manager.Create(context.Background(), work.CreateReq{Title: "x", Prompt: "p"})
	require.NoError(t, err)
	runCtx := WithProfile(WithTaskManager(ctx, manager), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"*"}},
	})
	out, err := runTool(runCtx, pt.TodoWrite, `{"task_id":"`+task.ID+`","rows":[]}`)
	require.NoError(t, err)
	assert.Contains(t, out, task.ID)
}

// TestPlanTools_NoManager: 缺 manager 时所有 kernel 都返回 error。
func TestPlanTools_NoManager(t *testing.T) {
	ctx := WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"*"}},
	})
	pt := NewPlanTools()
	_, err := runKernel(ctx, pt.updatePlan, `{"task_id":"x","rows":[]}`)
	assert.Error(t, err)
}
