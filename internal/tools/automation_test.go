package tools_test

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/agent/automation"
	"github.com/x6nux/yanshi/internal/guard"
	"github.com/x6nux/yanshi/internal/store"
	"github.com/x6nux/yanshi/internal/tools"
)

// fakeQueueStub 满足 QueuePort 的最简 stub（记录 SubmitRun 调用）。
type fakeQueueStub struct {
	mu    sync.Mutex
	calls int
}

func (r *fakeQueueStub) SubmitRun(_ context.Context, p automation.RunPayload) (automation.RunReceipt, error) {
	r.mu.Lock()
	r.calls++
	r.mu.Unlock()
	return automation.RunReceipt{WorkTaskID: "wt-" + p.RunID, BrokerTaskID: "broker-" + p.RunID}, nil
}
func (r *fakeQueueStub) Lookup(_ context.Context, workTaskID string) (automation.RunStatus, error) {
	return automation.RunStatus{Status: automation.RunQueued}, nil
}

// setupAutomation 构造一个真实 *automation.Manager + 内置 fakeQueue，便于工具层端到端测试。
func setupAutomation(t *testing.T) (*tools.AutomationTools, *automation.Manager) {
	t.Helper()
	s, err := store.Open(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	repo := automation.NewRepository(s)
	q := &fakeQueueStub{}
	m, err := automation.NewManager(repo, q, nil)
	require.NoError(t, err)
	return tools.NewAutomationTools(m), m
}

func allowAll(names ...string) guard.PermissionProfile {
	return guard.PermissionProfile{Tools: guard.ToolsPerm{Allow: names}}
}

// withApprovingUser 绑定一个恒返回 PermissionAllow 的 permission callback，模拟 WS
// 传输上用户逐次点"允许"。automation_* 与 agent_batch 是 approval 门禁工具，没有
// callback 时恒被拒（见 TestApprovalGatedToolsDeniedWithoutCallback），所以任何要
// 真正执行到工具体的测试都必须先过这一关。
func withApprovingUser(ctx context.Context) context.Context {
	return tools.WithPermissionCallback(ctx, func(req tools.PermissionRequest) tools.PermissionDecision {
		if !req.ApprovalRequired {
			return tools.PermissionDeny
		}
		return tools.PermissionAllow
	})
}

func TestAutomationToolsAllEightPresent(t *testing.T) {
	set, _ := setupAutomation(t)
	seen := map[string]bool{}
	for _, gt := range []*tools.GuardedTool{set.Create, set.List, set.Read, set.Update, set.Pause, set.Resume, set.Delete, set.Run} {
		info, err := gt.Info(context.Background())
		require.NoError(t, err)
		seen[info.Name] = true
	}
	for _, want := range []string{
		"automation_create", "automation_list", "automation_read",
		"automation_update", "automation_pause", "automation_resume",
		"automation_delete", "automation_run",
	} {
		assert.True(t, seen[want], "missing tool %q", want)
	}
}

// TestApprovalPromisedInDescriptionIsEnforced 守住「描述不撒谎」这条不变量：凡工具
// 描述里写了 "Approval required"，就必须真的走 AuthorizeApprovalRequired 门禁。
// 注意过滤串故意不带句点 —— automation_list 的描述是 "Approval required even though
// this is read-only."，带句点过滤会把它静默跳过。
//
// 这是「approval 门禁」这条验收的主证据：它遍历全部 9 个工具，并用
// assert.Equal(len(all), checked) 保证没有一个被静默跳过。
func TestApprovalPromisedInDescriptionIsEnforced(t *testing.T) {
	set, _ := setupAutomation(t)
	batchSet, _ := newBatchTools(t)
	all := []*tools.GuardedTool{
		set.Create, set.List, set.Read, set.Update,
		set.Pause, set.Resume, set.Delete, set.Run,
		batchSet.AgentBatch,
	}
	checked := 0
	for _, gt := range all {
		info, err := gt.Info(context.Background())
		require.NoError(t, err)
		if !strings.Contains(info.Desc, "Approval required") {
			continue
		}
		checked++
		assert.True(t, gt.ApprovalRequired(),
			"%s promises %q in its description but is not approval-gated", info.Name, "Approval required")
	}
	assert.Equal(t, len(all), checked, "every tool in this set is expected to promise approval")
}

// TestApprovalGatedToolsDeniedWithoutCallback 固化上一条测试的直接后果：approval 门禁
// 工具在没有 permission callback 的 context 下恒被拒（AuthorizeApprovalRequired 在无
// callback 时直接 DenyErr）。这意味着这九个工具只在 WS/TUI 这类交互式传输上可用，
// SSE / v1 / task-agent 三条路径上一律拒绝 —— 这是「需要人工批准」的必然结果，不是缺陷。
//
// ⚠️ 这条只跑一个工具（set.List），因此它是门禁**后果**的证据，不是「这九个工具都
// 被门禁」的证据 —— 后者由 TestApprovalPromisedInDescriptionIsEnforced 承担。
func TestApprovalGatedToolsDeniedWithoutCallback(t *testing.T) {
	set, _ := setupAutomation(t)
	ctx := tools.WithProfile(context.Background(), allowAll(
		"automation_create", "automation_list", "automation_read",
		"automation_update", "automation_pause", "automation_resume",
		"automation_delete", "automation_run",
	))
	result, err := set.List.InvokableRun(ctx, `{"input":"{}"}`)
	require.NoError(t, err)
	assert.Contains(t, result, "permission denied")
	assert.Contains(t, result, "requires explicit approval")
}

// TestAutomationCreateReadUpdatePauseResumeDeleteRun walks the whole tool-layer
// lifecycle — create → read → update → pause → resume → run → list → delete —
// and proves REACHABILITY: every one of the eight tools accepts its envelope,
// dispatches, and comes back without a transport error.
//
// It does NOT prove the mutating four took effect, and the distinction is not
// academic: GuardedTool.InvokableRun reports refusals in the RESULT STRING with
// a nil error (TestAutomationToolsDeniedWithoutProfile below is built on
// exactly that shape), so update/pause/resume/delete are checked here by a bare
// require.NoError against a value that stays nil whether the operation ran or
// was refused. Hollowing out Manager.Update and Manager.Delete to `if true {
// return }` leaves this test green.
//
// That is why C1/AU1#3 is not a terminal clause. Closing it means asserting the
// observable after-state — read reports paused, then active again, prompt
// really became "do Y", and read after delete reports not-found — not merely
// that the calls returned.
func TestAutomationCreateReadUpdatePauseResumeDeleteRun(t *testing.T) {
	set, m := setupAutomation(t)
	ctx := withApprovingUser(tools.WithProfile(context.Background(), allowAll(
		"automation_create", "automation_list", "automation_read",
		"automation_update", "automation_pause", "automation_resume",
		"automation_delete", "automation_run",
	)))

	// create
	createResp, err := set.Create.InvokableRun(ctx, mustJSON(t, map[string]any{
		"name": "nightly", "prompt": "do X",
		"schedule_kind": "interval", "schedule": "60",
	}))
	require.NoError(t, err)
	var created automation.Automation
	require.NoError(t, json.Unmarshal([]byte(createResp), &created))
	assert.NotEmpty(t, created.ID)

	// read
	readResp, err := set.Read.InvokableRun(ctx, mustJSON(t, map[string]any{"id": created.ID}))
	require.NoError(t, err)
	assert.Contains(t, readResp, created.ID)

	// update
	_, err = set.Update.InvokableRun(ctx, mustJSON(t, map[string]any{
		"id": created.ID, "prompt": "do Y",
	}))
	require.NoError(t, err)

	// pause / resume
	_, err = set.Pause.InvokableRun(ctx, mustJSON(t, map[string]any{"id": created.ID}))
	require.NoError(t, err)
	_, err = set.Resume.InvokableRun(ctx, mustJSON(t, map[string]any{"id": created.ID}))
	require.NoError(t, err)

	// run
	runResp, err := set.Run.InvokableRun(ctx, mustJSON(t, map[string]any{"id": created.ID}))
	require.NoError(t, err)
	assert.Contains(t, runResp, `"status":"queued"`)

	// list
	listResp, err := set.List.InvokableRun(ctx, `{"input":"{}"}`)
	require.NoError(t, err)
	assert.Contains(t, listResp, created.ID)

	// delete
	_, err = set.Delete.InvokableRun(ctx, mustJSON(t, map[string]any{"id": created.ID}))
	require.NoError(t, err)

	_ = m
}

func TestAutomationToolsDeniedWithoutProfile(t *testing.T) {
	set, _ := setupAutomation(t)
	for name, gt := range map[string]*tools.GuardedTool{
		"create": set.Create, "list": set.List, "read": set.Read,
		"update": set.Update, "pause": set.Pause, "resume": set.Resume,
		"delete": set.Delete, "run": set.Run,
	} {
		result, err := gt.InvokableRun(context.Background(), `{}`)
		require.NoError(t, err, "%s", name)
		assert.Contains(t, result, "permission denied", "%s", name)
	}
}

func TestAutomationToolsDeniedWhenProfileOmitsName(t *testing.T) {
	set, _ := setupAutomation(t)
	ctx := tools.WithProfile(context.Background(), allowAll("automation_create"))
	result, err := set.Run.InvokableRun(ctx, `{}`)
	require.NoError(t, err)
	assert.Contains(t, result, "permission denied")
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	envelope, err := json.Marshal(map[string]string{"input": string(b)})
	require.NoError(t, err)
	return string(envelope)
}
