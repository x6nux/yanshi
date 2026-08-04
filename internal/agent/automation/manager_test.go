package automation_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/agent/automation"
	"github.com/x6nux/yanshi/internal/store"
)

// fakeQueue 实现 automation.QueuePort，用于 manager 测试。
type fakeQueue struct {
	mu      sync.Mutex
	calls   int
	ids     map[string]string
	states  map[string]automation.RunStatus
	parents []string
}

func newFakeQueue() *fakeQueue {
	return &fakeQueue{
		ids:    map[string]string{},
		states: map[string]automation.RunStatus{},
	}
}

func (q *fakeQueue) SubmitRun(_ context.Context, p automation.RunPayload) (automation.RunReceipt, error) {
	if p.AutomationID == "" || p.Prompt == "" {
		return automation.RunReceipt{}, errors.New("automation_id and prompt required")
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	q.parents = append(q.parents, p.ParentTaskID)
	if id, ok := q.ids[p.IdempotencyKey]; ok {
		return automation.RunReceipt{WorkTaskID: id}, nil
	}
	q.calls++
	id := "wt-" + p.IdempotencyKey
	q.ids[p.IdempotencyKey] = id
	q.states[id] = automation.RunStatus{Status: automation.RunQueued, Error: ""}
	return automation.RunReceipt{WorkTaskID: id, BrokerTaskID: "broker-" + p.IdempotencyKey}, nil
}

func (q *fakeQueue) Lookup(_ context.Context, workTaskID string) (automation.RunStatus, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	s, ok := q.states[workTaskID]
	if !ok {
		return automation.RunStatus{}, errors.New("not found")
	}
	return s, nil
}

func newManagerWithFakeQueue(t *testing.T) (*automation.Manager, *fakeQueue) {
	t.Helper()
	s, err := store.Open(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	repo := automation.NewRepository(s)
	q := newFakeQueue()
	m, err := automation.NewManager(repo, q, time.Now)
	require.NoError(t, err)
	return m, q
}

// TestManagerCreateAssignsIDAndNextRun covers automation creation: Create
// assigns an id and schedules the first run.
func TestManagerCreateAssignsIDAndNextRun(t *testing.T) {
	m, _ := newManagerWithFakeQueue(t)
	item, err := m.Create(automation.CreateInput{
		Name:     "nightly",
		Prompt:   "do X",
		Schedule: automation.Schedule{Kind: "interval", IntervalSec: 60},
	})
	require.NoError(t, err)
	assert.NotEmpty(t, item.ID)
	assert.True(t, item.Active)
	require.NotNil(t, item.NextRunAt)
	assert.True(t, item.NextRunAt.After(time.Now().Add(-time.Second)))
}

func TestManagerCreateRejectsEmptyFields(t *testing.T) {
	m, _ := newManagerWithFakeQueue(t)
	_, err := m.Create(automation.CreateInput{Name: "", Prompt: "p"})
	require.Error(t, err)
	_, err = m.Create(automation.CreateInput{
		Name:     "x",
		Prompt:   "p",
		Schedule: automation.Schedule{Kind: "bad"},
	})
	require.Error(t, err)
}

func TestManagerPauseClearsNextRunResumeRecomputes(t *testing.T) {
	m, _ := newManagerWithFakeQueue(t)
	item, err := m.Create(automation.CreateInput{
		Name:     "x",
		Prompt:   "p",
		Schedule: automation.Schedule{Kind: "interval", IntervalSec: 60},
	})
	require.NoError(t, err)
	require.NoError(t, m.Pause(item.ID))
	read, _, err := m.Read(item.ID)
	require.NoError(t, err)
	assert.Nil(t, read.NextRunAt)
	assert.False(t, read.Active)

	require.NoError(t, m.Resume(item.ID))
	read, _, err = m.Read(item.ID)
	require.NoError(t, err)
	require.NotNil(t, read.NextRunAt)
	assert.True(t, read.Active)
}

func TestManagerRunNowEnqueuesAndFillsTaskID(t *testing.T) {
	m, q := newManagerWithFakeQueue(t)
	item, err := m.Create(automation.CreateInput{
		Name:     "x",
		Prompt:   "p",
		Schedule: automation.Schedule{Kind: "interval", IntervalSec: 60},
	})
	require.NoError(t, err)

	run, err := m.RunNow(context.Background(), item.ID)
	require.NoError(t, err)
	assert.Equal(t, automation.RunQueued, run.Status)
	assert.NotEmpty(t, run.TaskID)
	assert.Equal(t, 1, q.calls, "exactly one SubmitRun")
}

// TestManagerTickEnqueuesDueSlotOnce covers scheduled enqueue: a Tick at the
// due slot submits exactly one run, and a second Tick in the same slot does not
// double-enqueue.
//
// (Renamed 2026-08-04 from TestManagerRunNowIdempotentPerKey, which named a
// method this test never calls — the ledger cited it for "按时触发入队" and the
// name argued against the citation.)
func TestManagerTickEnqueuesDueSlotOnce(t *testing.T) {
	// 使用固定时钟，使 NextRunAt（= now+60s）在 slotA 之前。
	now := time.Date(2026, 7, 21, 11, 58, 0, 0, time.UTC)
	s, err := store.Open(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	repo := automation.NewRepository(s)
	q := newFakeQueue()
	m, err := automation.NewManager(repo, q, func() time.Time { return now })
	require.NoError(t, err)

	_, err = m.Create(automation.CreateInput{
		Name:     "x",
		Prompt:   "p",
		Schedule: automation.Schedule{Kind: "interval", IntervalSec: 60},
	})
	require.NoError(t, err)

	// 第一次 Tick 在 slotA 入队，第二次 Tick 在同 slot 不应重复入队。
	slotA := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	require.NoError(t, m.Tick(context.Background(), slotA))
	require.NoError(t, m.Tick(context.Background(), slotA))
	assert.Equal(t, 1, q.calls, "Tick must not double-enqueue same slot")
}

func TestManagerTickAdvancesNextSlot(t *testing.T) {
	m, _ := newManagerWithFakeQueue(t)
	item, err := m.Create(automation.CreateInput{
		Name:     "x",
		Prompt:   "p",
		Schedule: automation.Schedule{Kind: "interval", IntervalSec: 60},
	})
	require.NoError(t, err)

	slot := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	_, err = m.Update(automation.UpdateInput{
		ID:     item.ID,
		Active: boolPtr(false),
	})
	require.NoError(t, err)
	_, err = m.Update(automation.UpdateInput{
		ID:     item.ID,
		Active: boolPtr(true),
	})
	require.NoError(t, m.Tick(context.Background(), slot.Add(2*time.Minute)))
	read, _, err := m.Read(item.ID)
	require.NoError(t, err)
	require.NotNil(t, read.NextRunAt)
	assert.True(t, read.NextRunAt.After(slot.Add(2*time.Minute)), "NextRunAt must advance past tick time")
}

func TestManagerReconcileUpdatesRunStatus(t *testing.T) {
	m, q := newManagerWithFakeQueue(t)
	item, err := m.Create(automation.CreateInput{
		Name:     "x",
		Prompt:   "p",
		Schedule: automation.Schedule{Kind: "interval", IntervalSec: 60},
	})
	require.NoError(t, err)
	run, err := m.RunNow(context.Background(), item.ID)
	require.NoError(t, err)

	// fakeQueue 推进状态为 completed。
	q.mu.Lock()
	q.states[run.TaskID] = automation.RunStatus{Status: automation.RunCompleted}
	q.mu.Unlock()

	require.NoError(t, m.Reconcile(context.Background()))
	_, runs, err := m.Read(item.ID)
	require.NoError(t, err)
	require.Len(t, runs, 1)
	assert.Equal(t, automation.RunCompleted, runs[0].Status)
}

func TestManagerDeleteCascadesRuns(t *testing.T) {
	m, _ := newManagerWithFakeQueue(t)
	item, err := m.Create(automation.CreateInput{
		Name:     "x",
		Prompt:   "p",
		Schedule: automation.Schedule{Kind: "interval", IntervalSec: 60},
	})
	require.NoError(t, err)
	_, err = m.RunNow(context.Background(), item.ID)
	require.NoError(t, err)
	require.NoError(t, m.Delete(item.ID))
	_, _, err = m.Read(item.ID)
	require.Error(t, err)
}

func TestManagerDeleteMissingReturnsError(t *testing.T) {
	m, _ := newManagerWithFakeQueue(t)
	err := m.Delete("ghost")
	require.Error(t, err)
}

func TestManagerConstructorRejectsNilQueue(t *testing.T) {
	s, err := store.Open(":memory:")
	require.NoError(t, err)
	defer s.Close()
	_, err = automation.NewManager(automation.NewRepository(s), nil, time.Now)
	require.Error(t, err)
}

func TestManagerSubmitRunErrorMarksRunFailed(t *testing.T) {
	s, err := store.Open(":memory:")
	require.NoError(t, err)
	defer s.Close()
	repo := automation.NewRepository(s)
	failing := &failingQueue{}
	m, err := automation.NewManager(repo, failing, time.Now)
	require.NoError(t, err)
	item, err := m.Create(automation.CreateInput{
		Name:     "x",
		Prompt:   "p",
		Schedule: automation.Schedule{Kind: "interval", IntervalSec: 60},
	})
	require.NoError(t, err)
	_, err = m.RunNow(context.Background(), item.ID)
	require.Error(t, err)
	_, runs, err := m.Read(item.ID)
	require.NoError(t, err)
	require.Len(t, runs, 1)
	assert.Equal(t, automation.RunFailed, runs[0].Status)
}

// failingQueue 实现 QueuePort 并总是返回错误。
type failingQueue struct{}

func (failingQueue) SubmitRun(context.Context, automation.RunPayload) (automation.RunReceipt, error) {
	return automation.RunReceipt{}, errors.New("queue unavailable")
}
func (failingQueue) Lookup(context.Context, string) (automation.RunStatus, error) {
	return automation.RunStatus{}, errors.New("lookup unavailable")
}

func boolPtr(b bool) *bool { return &b }

func TestManagerConcurrentTickAndReadNoRace(t *testing.T) {
	m, _ := newManagerWithFakeQueue(t)
	item, err := m.Create(automation.CreateInput{
		Name:     "x",
		Prompt:   "p",
		Schedule: automation.Schedule{Kind: "interval", IntervalSec: 60},
	})
	require.NoError(t, err)
	var stop int32
	go func() {
		for atomic.LoadInt32(&stop) == 0 {
			_ = m.Tick(context.Background(), time.Now())
		}
	}()
	for i := 0; i < 50; i++ {
		_, _, _ = m.Read(item.ID)
	}
	atomic.StoreInt32(&stop, 1)
}

// TestQueuePortContractAcceptsNonEmptyParent — from Task 1.
func TestQueuePortContractAcceptsNonEmptyParent(t *testing.T) {
	q := newFakeQueue()
	receipt, err := q.SubmitRun(context.Background(), automation.RunPayload{
		AutomationID:   "auto-1",
		RunID:          "run-1",
		Prompt:         "p",
		ParentTaskID:   "non-empty-parent",
		IdempotencyKey: "k1",
	})
	require.NoError(t, err)
	require.NotEmpty(t, receipt.WorkTaskID)

	again, err := q.SubmitRun(context.Background(), automation.RunPayload{
		AutomationID:   "auto-1",
		RunID:          "run-1",
		Prompt:         "p",
		ParentTaskID:   "non-empty-parent",
		IdempotencyKey: "k1",
	})
	require.NoError(t, err)
	require.Equal(t, receipt.WorkTaskID, again.WorkTaskID)
	require.Equal(t, 1, q.calls)

	status, err := q.Lookup(context.Background(), receipt.WorkTaskID)
	require.NoError(t, err)
	require.Equal(t, automation.RunQueued, status.Status)
}
