package tools_test

import (
	"context"
	"encoding/json"
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

func TestAutomationCreateReadUpdatePauseResumeDeleteRun(t *testing.T) {
	set, m := setupAutomation(t)
	ctx := tools.WithProfile(context.Background(), allowAll(
		"automation_create", "automation_list", "automation_read",
		"automation_update", "automation_pause", "automation_resume",
		"automation_delete", "automation_run",
	))

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
