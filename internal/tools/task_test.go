package tools

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/x6nux/yanshi/internal/guard"
	"github.com/x6nux/yanshi/internal/task/work"
)

// runKernel 直接驱动一个 SyncStream 的 kernel 函数（不经 GuardedTool 的 Authorize
// / Stream / chunk 收集），用于 task/plan/gate 工具单测：在真实 context 上调用
// 纯业务逻辑。需要测 Authorize 行为时改用 runTool + *GuardedTool。
func runKernel(ctx context.Context, kernel func(context.Context, string) (string, error), argsJSON string) (string, error) {
	return kernel(ctx, argsJSON)
}

// wildcardProfile 是一个 Tools.Allow=["*"] 的 profile，用于测试 force-prompt
// 必须穿过 wildcard 直达 callback。
func wildcardProfile() guard.PermissionProfile {
	return guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"*"}},
	}
}

func TestTaskCreateReadListCancel(t *testing.T) {
	manager := work.NewFakeManager()
	ctx := WithProfile(WithTaskManager(context.Background(), manager), wildcardProfile())
	tt := NewTaskTools()

	// create
	out, err := runTool(ctx, tt.Create, `{"title":"my task","prompt":"do thing"}`)
	require.NoError(t, err)
	var createPayload struct {
		Task *work.WorkTask `json:"task"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &createPayload))
	require.NotNil(t, createPayload.Task)
	assert.NotEmpty(t, createPayload.Task.ID)
	assert.Equal(t, "my task", createPayload.Task.Title)

	// read
	out, err = runTool(ctx, tt.Read, `{"id":"`+createPayload.Task.ID+`"}`)
	require.NoError(t, err)
	var readPayload struct {
		Task *work.WorkTask `json:"task"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &readPayload))
	assert.Equal(t, createPayload.Task.ID, readPayload.Task.ID)

	// list
	out, err = runTool(ctx, tt.List, `{"limit":5}`)
	require.NoError(t, err)
	var listPayload struct {
		Tasks []work.Summary `json:"tasks"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &listPayload))
	require.Len(t, listPayload.Tasks, 1)
	assert.Equal(t, createPayload.Task.ID, listPayload.Tasks[0].ID)

	// cancel 走 force-prompt 分支，必须先注册一个 callback
	cancelCtx := WithPermissionCallback(ctx, func(req PermissionRequest) PermissionDecision {
		require.True(t, req.ForcePrompt, "task_cancel must set ForcePrompt=true")
		return PermissionAllow
	})
	out, err = runTool(cancelCtx, tt.Cancel, `{"id":"`+createPayload.Task.ID+`"}`)
	require.NoError(t, err)
	var cancelPayload struct {
		Task *work.WorkTask `json:"task"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &cancelPayload))
	assert.Equal(t, work.StatusCancelled, cancelPayload.Task.Status)
}

// TestTaskCreate_ReadsThreadLinkFromContext: when args omit thread/turn,
// the kernel reads them from the context's ThreadLink.
func TestTaskCreate_ReadsThreadLinkFromContext(t *testing.T) {
	manager := work.NewFakeManager()
	ctx := WithProfile(WithTaskManager(context.Background(), manager), wildcardProfile())
	ctx = WithThreadLink(ctx, "th-123", "tn-456")
	tt := NewTaskTools()
	out, err := runKernel(ctx, tt.runCreate, `{"title":"t","prompt":"p"}`)
	require.NoError(t, err)
	var payload struct {
		Task *work.WorkTask `json:"task"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &payload))
	assert.Equal(t, "th-123", payload.Task.ThreadID)
	assert.Equal(t, "tn-456", payload.Task.TurnID)
}

// TestTaskCancel_NoCallbackFails: 没绑 callback 时 task_cancel 必须 fail-closed，
// 即使 wildcard profile 也救不了。DenyErr 经 InvokableRun 表面化为 result string
// （非 Go error，让模型可重试），所以测试检查 result 文本而非 err。
func TestTaskCancel_NoCallbackFails(t *testing.T) {
	manager := work.NewFakeManager()
	task, err := manager.Create(context.Background(), work.CreateReq{Title: "x", Prompt: "p"})
	require.NoError(t, err)
	ctx := WithProfile(WithTaskManager(context.Background(), manager), wildcardProfile())
	tt := NewTaskTools()
	out, err := runTool(ctx, tt.Cancel, `{"id":"`+task.ID+`"}`)
	// InvokableRun 把 DenyErr 当作结果文本回喂模型；err 为 nil 是设计如此
	require.NoError(t, err)
	require.Contains(t, out, "permission denied", "want denial text in result; got %q", out)
	// task 在 manager 中仍非 cancelled（Cancel kernel 未执行）
	stored, _ := manager.Read(context.Background(), task.ID)
	assert.NotEqual(t, work.StatusCancelled, stored.Status, "task must not be cancelled when callback is absent")
}

// TestTaskCancel_AlwaysAllowDoesNotRecord: 连续两次用 PermissionAlwaysAllow 应答，
// 第二次仍触发 callback（force-prompt 永不进 allowlist）。
func TestTaskCancel_AlwaysAllowDoesNotRecord(t *testing.T) {
	manager := work.NewFakeManager()
	t1, _ := manager.Create(context.Background(), work.CreateReq{Title: "1", Prompt: "p"})
	require.NoError(t, manager.Start(context.Background(), t1.ID))
	t2, _ := manager.Create(context.Background(), work.CreateReq{Title: "2", Prompt: "p"})
	require.NoError(t, manager.Start(context.Background(), t2.ID))

	var prompts int
	ctx := WithProfile(WithTaskManager(context.Background(), manager), wildcardProfile())
	ctx = WithPermissionCallback(ctx, func(req PermissionRequest) PermissionDecision {
		require.True(t, req.ForcePrompt)
		prompts++
		return PermissionAlwaysAllow
	})
	tt := NewTaskTools()
	_, err := runTool(ctx, tt.Cancel, `{"id":"`+t1.ID+`"}`)
	require.NoError(t, err)
	_, err = runTool(ctx, tt.Cancel, `{"id":"`+t2.ID+`"}`)
	require.NoError(t, err)
	assert.Equal(t, 2, prompts, "force-prompt must prompt every time, even after PermissionAlwaysAllow")
}

// TestTaskTools_NoManager: 没有 task manager 绑定时所有工具应回错误。
func TestTaskTools_NoManager(t *testing.T) {
	ctx := WithProfile(context.Background(), wildcardProfile())
	tt := NewTaskTools()
	_, err := runKernel(ctx, tt.runCreate, `{"title":"x","prompt":"p"}`)
	require.Error(t, err)
}

// TestAuthorize_ForcePrompt_BeforeAllow: wildcard profile 下 task_cancel 仍触发 callback；
// 与非 force-prompt 工具对比（它们会直接被 guard.Check Allow 短路）。
func TestAuthorize_ForcePrompt_BeforeAllow(t *testing.T) {
	// task_cancel: 即使 wildcard profile 也必须走 callback
	var asked int
	ctx := WithProfile(context.Background(), wildcardProfile())
	ctx = WithPermissionCallback(ctx, func(req PermissionRequest) PermissionDecision {
		asked++
		return PermissionAllow
	})
	err := Authorize(ctx, guard.Action{Tool: "task_cancel"}, "{}")
	require.NoError(t, err)
	assert.Equal(t, 1, asked)

	// 非 force-prompt 工具：wildcard profile 直接 Allow，callback 不被调用
	asked = 0
	err = Authorize(ctx, guard.Action{Tool: "task_read"}, "{}")
	require.NoError(t, err)
	assert.Equal(t, 0, asked)
}

// TestAuthorize_ForcePrompt_NoCallbackRejects: 没 callback 时 force-prompt 工具必须 fail-closed。
func TestAuthorize_ForcePrompt_NoCallbackRejects(t *testing.T) {
	ctx := WithProfile(context.Background(), wildcardProfile())
	err := Authorize(ctx, guard.Action{Tool: "task_cancel"}, "{}")
	require.Error(t, err)
	require.True(t, errors.Is(err, &DenyErr{}) || IsDenyErr(err))
}

// TestAuthorize_PlanMode_DeniesWriteTools: Plan mode 下 fs_write/shell_run/task_gate_run
// 等写工具被 PlanToolAllowed 直接拒绝（不进 force-prompt / approval.Manager 分支）。
//
// ledger: A2/G05#1 plan 模式禁编辑类工具
func TestAuthorize_PlanMode_DeniesWriteTools(t *testing.T) {
	ctx := WithPlanMode(WithProfile(context.Background(), wildcardProfile()), true)
	for _, tool := range []string{"fs_write", "fs_edit", "shell_run", "task_gate_run", "task_cancel", "vcs_commit"} {
		err := Authorize(ctx, guard.Action{Tool: tool}, "{}")
		require.Error(t, err, "Plan mode must deny %q", tool)
		require.True(t, IsDenyErr(err), "want DenyErr for %q in plan mode", tool)
	}
}

// TestAuthorize_PlanMode_AllowsReadOnlyTools: Plan mode 下只读工具仍能进 guard.Check
// （这里 wildcard profile 直接 Allow）。task_create 也在白名单内，可正常通过。
func TestAuthorize_PlanMode_AllowsReadOnlyTools(t *testing.T) {
	ctx := WithPlanMode(WithProfile(context.Background(), wildcardProfile()), true)
	for _, tool := range []string{"fs_read", "task_create", "task_list", "task_read", "update_plan", "artifact_read"} {
		err := Authorize(ctx, guard.Action{Tool: tool}, "{}")
		require.NoError(t, err, "Plan mode should not deny plan-safe tool %q at the firewall; got %v", tool, err)
	}
}

// TestPlanToolAllowed_ReExport: tools.PlanToolAllowed 与 guard.PlanToolAllowed 一致。
func TestPlanToolAllowed_ReExport(t *testing.T) {
	assert.True(t, PlanToolAllowed("fs_read"))
	assert.True(t, PlanToolAllowed("task_create"))
	assert.False(t, PlanToolAllowed("shell_run"))
	assert.False(t, PlanToolAllowed("task_cancel"))
}
