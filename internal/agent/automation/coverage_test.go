// Package automation 的额外覆盖测试（package 级，非 _test，以访问未导出函数）。
package automation

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/store"
)

// ---------------------------------------------------------------------------
// Helper types
// ---------------------------------------------------------------------------

// fakeQueue 实现 QueuePort，供 package 级测试复用。
type packageFakeQueue struct {
	submitCh chan struct{}
	err      error
	runID    int64
}

func newPackageFakeQueue() *packageFakeQueue {
	return &packageFakeQueue{submitCh: make(chan struct{}, 100)}
}

func (q *packageFakeQueue) SubmitRun(_ context.Context, _ RunPayload) (RunReceipt, error) {
	select {
	case <-q.submitCh:
	default:
	}
	if q.err != nil {
		return RunReceipt{}, q.err
	}
	id := atomic.AddInt64(&q.runID, 1)
	return RunReceipt{WorkTaskID: "wt-" + itoa(id), BrokerTaskID: "broker-" + itoa(id)}, nil
}

func (q *packageFakeQueue) Lookup(_ context.Context, workTaskID string) (RunStatus, error) {
	return RunStatus{Status: RunCompleted}, nil
}

func (q *packageFakeQueue) setErr(err error) { q.err = err }

func itoa(i int64) string {
	if i == 1 {
		return "1"
	}
	if i == 2 {
		return "2"
	}
	return "x"
}

type packageFailingQueue struct{}

func (packageFailingQueue) SubmitRun(context.Context, RunPayload) (RunReceipt, error) {
	return RunReceipt{}, errors.New("queue unavailable")
}
func (packageFailingQueue) Lookup(context.Context, string) (RunStatus, error) {
	return RunStatus{}, errors.New("lookup unavailable")
}

// newManagerWithFakeQueue 创建带 fakeQueue 的 Manager（package 内部）。
func newManagerWithFakeQueue(t *testing.T) (*Manager, *packageFakeQueue, *store.Store) {
	t.Helper()
	s, err := store.Open(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	repo := NewRepository(s)
	q := newPackageFakeQueue()
	m, err := NewManager(repo, q, time.Now)
	require.NoError(t, err)
	return m, q, s
}

// createItem 快捷创建 interval 自动化。
func createItem(t *testing.T, m *Manager, name string) Automation {
	t.Helper()
	item, err := m.Create(CreateInput{
		Name:     name,
		Prompt:   "do " + name,
		Schedule: Schedule{Kind: "interval", IntervalSec: 60},
	})
	require.NoError(t, err)
	return item
}

// ---------------------------------------------------------------------------
// TestNewManager edge cases
// ---------------------------------------------------------------------------

func TestNewManager_NilRepo(t *testing.T) {
	_, err := NewManager(nil, newPackageFakeQueue(), time.Now)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "repository is nil")
}

func TestNewManager_NilQueue(t *testing.T) {
	s, err := store.Open(":memory:")
	require.NoError(t, err)
	defer s.Close()
	repo := NewRepository(s)
	_, err = NewManager(repo, nil, time.Now)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "QueuePort")
}

func TestNewManager_NilNow(t *testing.T) {
	s, err := store.Open(":memory:")
	require.NoError(t, err)
	defer s.Close()
	repo := NewRepository(s)
	q := newPackageFakeQueue()
	m, err := NewManager(repo, q, nil)
	require.NoError(t, err)
	require.NotNil(t, m)
}

// ---------------------------------------------------------------------------
// Test List (0% coverage)
// ---------------------------------------------------------------------------

func TestList_Empty(t *testing.T) {
	m, _, _ := newManagerWithFakeQueue(t)
	list, err := m.List()
	require.NoError(t, err)
	assert.Empty(t, list)
}

func TestList_WithItems(t *testing.T) {
	m, _, _ := newManagerWithFakeQueue(t)
	createItem(t, m, "a")
	createItem(t, m, "b")

	list, err := m.List()
	require.NoError(t, err)
	require.Len(t, list, 2)
	assert.Equal(t, "a", list[0].Name)
	assert.Equal(t, "b", list[1].Name)
}

func TestList_RepoLoadError(t *testing.T) {
	m, _, s := newManagerWithFakeQueue(t)
	// 关闭 store 使 Load 失败
	require.NoError(t, s.DB.Close())

	_, err := m.List()
	require.Error(t, err)
}

// ---------------------------------------------------------------------------
// Test Create edge cases
// ---------------------------------------------------------------------------

func TestCreate_Paused(t *testing.T) {
	m, _, _ := newManagerWithFakeQueue(t)
	item, err := m.Create(CreateInput{
		Name:     "nightly",
		Prompt:   "do X",
		Schedule: Schedule{Kind: "interval", IntervalSec: 60},
		Paused:   true,
	})
	require.NoError(t, err)
	assert.False(t, item.Active)
	assert.Nil(t, item.NextRunAt, "paused automation must not compute NextRunAt")
}

func TestCreate_RepoLoadError(t *testing.T) {
	m, _, s := newManagerWithFakeQueue(t)
	s.DB.Close()

	_, err := m.Create(CreateInput{
		Name:     "x",
		Prompt:   "p",
		Schedule: Schedule{Kind: "interval", IntervalSec: 60},
	})
	require.Error(t, err)
}

func TestCreate_RepoSaveError(t *testing.T) {
	m, _, s := newManagerWithFakeQueue(t)
	createItem(t, m, "seed") // 写入初始 state

	s.DB.Close()

	_, err := m.Create(CreateInput{
		Name:     "x",
		Prompt:   "p",
		Schedule: Schedule{Kind: "interval", IntervalSec: 60},
	})
	require.Error(t, err)
}

// ---------------------------------------------------------------------------
// Test Read edge cases
// ---------------------------------------------------------------------------

func TestRead_NotFound(t *testing.T) {
	m, _, _ := newManagerWithFakeQueue(t)
	_, _, err := m.Read("nonexistent")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestRead_RepoLoadError(t *testing.T) {
	m, _, s := newManagerWithFakeQueue(t)
	s.DB.Close()

	_, _, err := m.Read("x")
	require.Error(t, err)
}

// ---------------------------------------------------------------------------
// Test Update edge cases
// ---------------------------------------------------------------------------

func TestUpdate_AllFields(t *testing.T) {
	m, _, _ := newManagerWithFakeQueue(t)
	item := createItem(t, m, "original")

	updated, err := m.Update(UpdateInput{
		ID:       item.ID,
		Name:     strPtr("new-name"),
		Prompt:   strPtr("new-prompt"),
		Schedule: schedulePtr(Schedule{Kind: "interval", IntervalSec: 120}),
		Cwds:     cwdsPtr([]string{"/tmp"}),
		Active:   boolPtr(false),
	})
	require.NoError(t, err)
	assert.Equal(t, "new-name", updated.Name)
	assert.Equal(t, "new-prompt", updated.Prompt)
	assert.Equal(t, int64(120), updated.Schedule.IntervalSec)
	assert.Equal(t, []string{"/tmp"}, updated.Cwds)
	assert.False(t, updated.Active)
}

func TestUpdate_NameOnly(t *testing.T) {
	m, _, _ := newManagerWithFakeQueue(t)
	item := createItem(t, m, "orig")

	updated, err := m.Update(UpdateInput{
		ID:   item.ID,
		Name: strPtr("renamed"),
	})
	require.NoError(t, err)
	assert.Equal(t, "renamed", updated.Name)
	assert.Equal(t, "do orig", updated.Prompt) // unchanged
	assert.True(t, updated.Active)             // unchanged
}

func TestUpdate_ScheduleOnly(t *testing.T) {
	m, _, _ := newManagerWithFakeQueue(t)
	item := createItem(t, m, "sched")

	updated, err := m.Update(UpdateInput{
		ID:       item.ID,
		Schedule: schedulePtr(Schedule{Kind: "interval", IntervalSec: 300}),
	})
	require.NoError(t, err)
	assert.Equal(t, int64(300), updated.Schedule.IntervalSec)
	require.NotNil(t, updated.NextRunAt)
}

func TestUpdate_CwdsOnly(t *testing.T) {
	m, _, _ := newManagerWithFakeQueue(t)
	item := createItem(t, m, "cd")

	updated, err := m.Update(UpdateInput{
		ID:   item.ID,
		Cwds: cwdsPtr([]string{"/a", "/b"}),
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"/a", "/b"}, updated.Cwds)
}

func TestUpdate_PauseViaActiveFalse(t *testing.T) {
	m, _, _ := newManagerWithFakeQueue(t)
	item := createItem(t, m, "pause-me")

	updated, err := m.Update(UpdateInput{
		ID:     item.ID,
		Active: boolPtr(false),
	})
	require.NoError(t, err)
	assert.False(t, updated.Active)
	assert.Nil(t, updated.NextRunAt)
}

func TestUpdate_ScheduleValidationError(t *testing.T) {
	m, _, _ := newManagerWithFakeQueue(t)
	item := createItem(t, m, "bad")

	_, err := m.Update(UpdateInput{
		ID:       item.ID,
		Schedule: schedulePtr(Schedule{Kind: "cron", Cron: "invalid"}),
	})
	require.Error(t, err)
}

func TestUpdate_NotFound(t *testing.T) {
	m, _, _ := newManagerWithFakeQueue(t)
	_, err := m.Update(UpdateInput{ID: "nonexistent"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestUpdate_RepoLoadError(t *testing.T) {
	m, _, s := newManagerWithFakeQueue(t)
	s.DB.Close()

	_, err := m.Update(UpdateInput{ID: "x"})
	require.Error(t, err)
}

func TestUpdate_RepoSaveError(t *testing.T) {
	m, _, s := newManagerWithFakeQueue(t)
	item := createItem(t, m, "save-fail")
	s.DB.Close()

	_, err := m.Update(UpdateInput{
		ID:   item.ID,
		Name: strPtr("new"),
	})
	require.Error(t, err)
}

// ---------------------------------------------------------------------------
// Test Pause / Resume
// ---------------------------------------------------------------------------

func TestPause_NotFound(t *testing.T) {
	m, _, _ := newManagerWithFakeQueue(t)
	err := m.Pause("nonexistent")
	require.Error(t, err)
}

func TestResume_NotFound(t *testing.T) {
	m, _, _ := newManagerWithFakeQueue(t)
	err := m.Resume("nonexistent")
	require.Error(t, err)
}

// ---------------------------------------------------------------------------
// Test Delete edge cases
// ---------------------------------------------------------------------------

func TestDelete_RepoLoadError(t *testing.T) {
	m, _, s := newManagerWithFakeQueue(t)
	s.DB.Close()

	err := m.Delete("x")
	require.Error(t, err)
}

// ---------------------------------------------------------------------------
// Test Tick error paths
// ---------------------------------------------------------------------------

// TestTick_ScheduleNextError 验证 Tick 在 Schedule.Next 失败的路径。
// 需要构造一个能通过 Create 验证但在 Tick 时 Next 失败的 Schedule。
func TestTick_ScheduleNextError(t *testing.T) {
	s, err := store.Open(":memory:")
	require.NoError(t, err)
	defer s.Close()

	repo := NewRepository(s)
	q := newPackageFakeQueue()
	m, err := NewManager(repo, q, time.Now)
	require.NoError(t, err)

	// 创建一个自动化
	item := createItem(t, m, "tick-test")

	// 直接修改持久化的 state，把 schedule 设成 Next 会失败的版本
	// 同时保证 NextRunAt 在过去
	state, err := repo.Load()
	require.NoError(t, err)
	past := time.Now().Add(-time.Hour)
	for i := range state.Automations {
		if state.Automations[i].ID == item.ID {
			state.Automations[i].Schedule = Schedule{Kind: "cron", Cron: "invalid-cron"}
			state.Automations[i].NextRunAt = &past
			break
		}
	}
	require.NoError(t, repo.Save(state))

	// Tick 现在会读到 NextRunAt 在过去，并尝试调用 Schedule.Next（会失败）
	now := time.Now()
	err = m.Tick(context.Background(), now)
	require.Error(t, err)
}

// TestTick_RepoSaveError 验证 Tick 在 repo.Save 失败的路径。
func TestTick_RepoSaveError(t *testing.T) {
	m, _, s := newManagerWithFakeQueue(t)
	createItem(t, m, "tick-save")

	// 创建一个即将到期的自动化
	// 设置 NextRunAt 在过去
	state, err := NewRepository(s).Load()
	require.NoError(t, err)
	for i := range state.Automations {
		state.Automations[i].NextRunAt = timePtr(time.Now().Add(-time.Hour))
	}
	require.NoError(t, NewRepository(s).Save(state))

	s.DB.Close()

	err = m.Tick(context.Background(), time.Now())
	require.Error(t, err)
}

// TestTick_EnqueueUnlockedError 验证 Tick 在 enqueueUnlocked 失败的路径。
func TestTick_EnqueueUnlockedError(t *testing.T) {
	m, q, _ := newManagerWithFakeQueue(t)
	createItem(t, m, "tick-enq")

	// 设置自动化在到期队列中
	state := loadState(t, m)
	for i := range state.Automations {
		state.Automations[i].NextRunAt = timePtr(time.Now().Add(-time.Hour))
	}
	saveState(t, m, state)

	// 让 queue 失败
	q.setErr(errors.New("enqueue failed"))

	err := m.Tick(context.Background(), time.Now())
	require.Error(t, err)
}

// ---------------------------------------------------------------------------
// Test RunNow / enqueue edge cases
// ---------------------------------------------------------------------------

func TestRunNow_NotFound(t *testing.T) {
	m, _, _ := newManagerWithFakeQueue(t)
	_, err := m.RunNow(context.Background(), "nonexistent")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestRunNow_RepoLoadError(t *testing.T) {
	m, _, s := newManagerWithFakeQueue(t)
	s.DB.Close()

	_, err := m.RunNow(context.Background(), "x")
	require.Error(t, err)
}

func TestRunNow_SubmitRunErrorMarksFailed(t *testing.T) {
	s, err := store.Open(":memory:")
	require.NoError(t, err)
	defer s.Close()

	repo := NewRepository(s)
	m, err := NewManager(repo, packageFailingQueue{}, time.Now)
	require.NoError(t, err)

	item := createItem(t, m, "fail-run")
	run, err := m.RunNow(context.Background(), item.ID)
	require.Error(t, err)
	assert.Equal(t, RunFailed, run.Status)
	assert.NotEmpty(t, run.Error)
}

func TestRunNow_RepoSaveAfterSubmitError(t *testing.T) {
	m, _, s := newManagerWithFakeQueue(t)
	item := createItem(t, m, "save-after-enq")

	// 第一次调用 RunNow 成功写入 run
	run, err := m.RunNow(context.Background(), item.ID)
	require.NoError(t, err)
	assert.NotEmpty(t, run.ID)

	// 关闭 store，使第二次 RunNow 的 repo.Save 失败
	s.DB.Close()

	// 第二次调用时，SubmitRun 成功（fakeQueue），但 repo.Save 失败
	_, err = m.RunNow(context.Background(), item.ID)
	require.Error(t, err)
}

// ---------------------------------------------------------------------------
// Test Reconcile edge cases
// ---------------------------------------------------------------------------

func TestReconcile_RepoLoadError(t *testing.T) {
	m, _, s := newManagerWithFakeQueue(t)
	s.DB.Close()

	err := m.Reconcile(context.Background())
	require.Error(t, err)
}

func TestReconcile_LookupError(t *testing.T) {
	s, err := store.Open(":memory:")
	require.NoError(t, err)
	defer s.Close()

	repo := NewRepository(s)
	m, err := NewManager(repo, packageFailingQueue{}, time.Now)
	require.NoError(t, err)

	item := createItem(t, m, "reconcile-fail")
	_, err = m.RunNow(context.Background(), item.ID)
	require.Error(t, err) // SubmitRun fails, so run gets RunFailed

	// Reconcile 应该跳过已失败的 run（不会调 Lookup）
	require.NoError(t, m.Reconcile(context.Background()))
}

func TestReconcile_SkipsCompletedFailedCanceled(t *testing.T) {
	m, _, _ := newManagerWithFakeQueue(t)
	item := createItem(t, m, "reconcile-skip")

	// 手动创建 run 直接写入 state
	run := Run{
		ID:             "run-skip",
		AutomationID:   item.ID,
		Status:         RunCompleted,
		TaskID:         "wt-done",
		IdempotencyKey: "k-skip",
	}
	state := loadState(t, m)
	state.Runs = append(state.Runs, run)
	saveState(t, m, state)

	// Reconcile 不应报错，即使 Lookup 可能失败（因为跳过已完成的 run）
	require.NoError(t, m.Reconcile(context.Background()))
}

// ---------------------------------------------------------------------------
// Test enqueueUnlocked
// ---------------------------------------------------------------------------

func TestEnqueueUnlocked_SubmitRunError(t *testing.T) {
	s, err := store.Open(":memory:")
	require.NoError(t, err)
	defer s.Close()

	repo := NewRepository(s)
	m, err := NewManager(repo, packageFailingQueue{}, time.Now)
	require.NoError(t, err)

	createItem(t, m, "enq-fail")

	d := dueItem{id: createItem(t, m, "test").ID, slot: time.Now(), prompt: "p"}
	// 直接用未导出的 enqueueUnlocked
	run, err := m.enqueueUnlocked(context.Background(), d, "manual")
	require.Error(t, err)
	assert.Equal(t, RunFailed, run.Status)
}

func TestEnqueueUnlocked_RepoSaveError(t *testing.T) {
	m, _, s := newManagerWithFakeQueue(t)
	item := createItem(t, m, "enq-save")

	s.DB.Close()

	d := dueItem{id: item.ID, slot: time.Now(), prompt: "p"}
	_, err := m.enqueueUnlocked(context.Background(), d, "manual")
	require.Error(t, err)
}

// ---------------------------------------------------------------------------
// Test Schedule.Next
// ---------------------------------------------------------------------------

func TestScheduleNext_CronError(t *testing.T) {
	s := Schedule{Kind: "cron", Cron: "bad-cron"}
	_, err := s.Next(time.Now())
	require.Error(t, err)
}

func TestScheduleNext_IntervalError(t *testing.T) {
	s := Schedule{Kind: "interval", IntervalSec: 0}
	_, err := s.Next(time.Now())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "positive")
}

func TestScheduleNext_UnknownKind(t *testing.T) {
	s := Schedule{Kind: "unknown"}
	_, err := s.Next(time.Now())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown schedule kind")
}

// ---------------------------------------------------------------------------
// Test Repository error paths
// ---------------------------------------------------------------------------

func TestRepositoryLoad_NilStore(t *testing.T) {
	r := &Repository{store: nil}
	_, err := r.Load()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not configured")
}

func TestRepositorySave_NilStore(t *testing.T) {
	r := &Repository{store: nil}
	err := r.Save(State{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not configured")
}

func TestRepositorySave_FromClosedDB(t *testing.T) {
	s, err := store.Open(":memory:")
	require.NoError(t, err)
	require.NoError(t, s.DB.Close())
	r := NewRepository(s)
	err = r.Save(State{SchemaVersion: StateSchemaVersion})
	require.Error(t, err)
}

// ---------------------------------------------------------------------------
// Test newID fallback
// ---------------------------------------------------------------------------

func TestNewID_NotEmpty(t *testing.T) {
	id := newID("auto")
	assert.NotEmpty(t, id)
	assert.Contains(t, id, "auto-")
}

// ---------------------------------------------------------------------------
// Test buildIdempotencyKey
// ---------------------------------------------------------------------------

func TestBuildIdempotencyKey_Manual(t *testing.T) {
	key := buildIdempotencyKey("auto-1", time.Now(), "manual")
	assert.Contains(t, key, "auto-1/manual/")
}

func TestBuildIdempotencyKey_Schedule(t *testing.T) {
	slot := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	key := buildIdempotencyKey("auto-1", slot, "schedule")
	assert.Equal(t, "automation/auto-1/slot/2026-07-21T12:00:00Z", key)
}

// ---------------------------------------------------------------------------
// Test ThreadID
// ---------------------------------------------------------------------------

func TestThreadID_Empty(t *testing.T) {
	a := Automation{}
	assert.Equal(t, "", a.ThreadID())
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func strPtr(s string) *string        { return &s }
func boolPtr(b bool) *bool           { return &b }
func timePtr(t time.Time) *time.Time { return &t }

func cwdsPtr(c []string) *[]string { return &c }

func schedulePtr(s Schedule) *Schedule { return &s }

func loadState(t *testing.T, m *Manager) State {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	state, err := m.repo.Load()
	require.NoError(t, err)
	return state
}

func saveState(t *testing.T, m *Manager, state State) {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	require.NoError(t, m.repo.Save(state))
}

// TestDelete_RunFromDifferentAutomationKept 验证 Delete 在删除 automation 时，
// 属于另一 automation 的 run 不会被误删（filtered = append(filtered, run) 路径）。
func TestDelete_RunFromDifferentAutomationKept(t *testing.T) {
	m, _, _ := newManagerWithFakeQueue(t)
	keepItem := createItem(t, m, "keep-me")
	deleteItem := createItem(t, m, "delete-me")

	// 为要删除的 automation 创建 run
	_, err := m.RunNow(context.Background(), deleteItem.ID)
	require.NoError(t, err)

	// 为要保留的 automation 创建 run（此 run 应在 Delete 后存活）
	_, err = m.RunNow(context.Background(), keepItem.ID)
	require.NoError(t, err)

	// 删除 deleteItem — 此时 for 循环需遍历 keepItem 的 run 并 append 到 filtered
	err = m.Delete(deleteItem.ID)
	require.NoError(t, err)

	// 验证 keepItem 的 run 仍存在
	_, runs, err := m.Read(keepItem.ID)
	require.NoError(t, err)
	require.Len(t, runs, 1)
	assert.Equal(t, keepItem.ID, runs[0].AutomationID)

	// 验证 deleteItem 已被删除
	_, _, err = m.Read(deleteItem.ID)
	require.Error(t, err)
}

// TestMapTaskStatus_FullCoverage 验证 MapTaskStatus 映射表完整（补充测试：遍历所有已知状态）。
func TestMapTaskStatus_FullCoverage(t *testing.T) {
	expectedKeys := []string{"pending", "running", "completed", "failed", "cancelled", "timeout"}
	for _, k := range expectedKeys {
		_, ok := MapTaskStatus[k]
		assert.True(t, ok, "MapTaskStatus missing key %q", k)
	}
}
