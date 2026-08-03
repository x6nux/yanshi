package bootstrap_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/agent/automation"
	"github.com/x6nux/yanshi/internal/bootstrap"
	"github.com/x6nux/yanshi/internal/task/work"
)

// fakeWorkManager 嵌入 A2 的 *work.FakeManager 以满足 work.ManagerLike 全部 12 个方法，
// 仅覆盖 Create + Read 以使用本测试自己的 tasks map。
type fakeWorkManager struct {
	*work.FakeManager
	mu    sync.Mutex
	tasks map[string]*work.WorkTask
}

func newFakeWorkManager() *fakeWorkManager {
	return &fakeWorkManager{
		FakeManager: work.NewFakeManager(),
		tasks:       map[string]*work.WorkTask{},
	}
}

func (m *fakeWorkManager) Create(ctx context.Context, req work.CreateReq) (*work.WorkTask, error) {
	if req.Title == "" || req.Prompt == "" {
		return nil, errors.New("title and prompt required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	w := &work.WorkTask{
		ID:       "wt-" + req.Title,
		Title:    req.Title,
		Prompt:   req.Prompt,
		Status:   work.StatusPending,
		ThreadID: req.ThreadID,
	}
	m.tasks[w.ID] = w
	return w, nil
}

func (m *fakeWorkManager) Read(ctx context.Context, id string) (*work.WorkTask, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	w, ok := m.tasks[id]
	if !ok {
		return nil, errors.New("not found")
	}
	return w, nil
}

// fakeBrokerSubmitter 仅用于 contract test；不需要真实 SQLite。
type fakeBrokerSubmitter struct {
	mu     sync.Mutex
	nextID int
	seen   map[string]string
}

func newFakeBrokerSubmitter() *fakeBrokerSubmitter {
	return &fakeBrokerSubmitter{seen: map[string]string{}}
}

func (b *fakeBrokerSubmitter) Submit(typ, input, parent string) (string, error) {
	if typ == "" || input == "" {
		return "", errors.New("typ and input required")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.nextID++
	id := "broker-" + json.Number(itoa(b.nextID)).String()
	b.seen[id] = typ + "|" + parent
	return id, nil
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [16]byte
	pos := len(buf)
	for n > 0 {
		pos--
		buf[pos] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[pos:])
}

// fakeKV 是 store.Store 的 KVGet/KVSet 最小子集，避免引入 SQLite。
type fakeKV struct {
	mu sync.Mutex
	m  map[string]string
}

func newFakeKV() *fakeKV { return &fakeKV{m: map[string]string{}} }

func (s *fakeKV) KVGet(key string) (string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.m[key]
	return v, ok, nil
}

func (s *fakeKV) KVSet(key, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[key] = value
	return nil
}

func TestA2AdapterImplementsQueuePort(t *testing.T) {
	var _ automation.QueuePort = (*bootstrap.A2Adapter)(nil)
}

func TestA2AdapterSubmitRunIdempotentAndAcceptsParent(t *testing.T) {
	kv := newFakeKV()
	adapter := bootstrap.NewA2Adapter(newFakeWorkManager(), newFakeBrokerSubmitter(), kv)

	p := automation.RunPayload{
		AutomationID:   "auto-1",
		RunID:          "run-1",
		Prompt:         "do thing",
		Cwds:           []string{"."},
		ParentTaskID:   "parent-xyz", // 非空 parent 是合法的（C1 review #10）
		IdempotencyKey: "automation/auto-1/slot-1",
		ThreadID:       "thread-1",
	}
	first, err := adapter.SubmitRun(context.Background(), p)
	require.NoError(t, err)
	require.NotEmpty(t, first.WorkTaskID)

	// 同 idempotency key 第二次提交必须返回同一 WorkTaskID，不重复创建。
	second, err := adapter.SubmitRun(context.Background(), p)
	require.NoError(t, err)
	require.Equal(t, first.WorkTaskID, second.WorkTaskID)
}

func TestA2AdapterLookupMapsWorkStatusToRunStatus(t *testing.T) {
	wm := newFakeWorkManager()
	kv := newFakeKV()
	adapter := bootstrap.NewA2Adapter(wm, newFakeBrokerSubmitter(), kv)

	p := automation.RunPayload{
		AutomationID:   "auto-2",
		RunID:          "run-2",
		Prompt:         "hi",
		IdempotencyKey: "automation/auto-2/slot-2",
	}
	receipt, err := adapter.SubmitRun(context.Background(), p)
	require.NoError(t, err)

	// 手动把 WorkTask 推进到 double-l "cancelled"，验证映射到 C1 单-l "canceled"。
	wm.mu.Lock()
	if wt, ok := wm.tasks[receipt.WorkTaskID]; ok {
		wt.Status = work.StatusCancelled // 注意：double-l
	}
	wm.mu.Unlock()

	status, err := adapter.Lookup(context.Background(), receipt.WorkTaskID)
	require.NoError(t, err)
	require.Equal(t, automation.RunCanceled, status.Status) // 单-l
}

func TestA2Adapter_SubmitRunNilGuard(t *testing.T) {
	// Passing nil to all NewA2Adapter args creates an adapter with nil fields
	// that triggers the nil-guard.
	_, err := bootstrap.NewA2Adapter(nil, nil, nil).SubmitRun(
		context.Background(),
		automation.RunPayload{
			AutomationID:   "a1",
			RunID:          "r1",
			Prompt:         "p",
			IdempotencyKey: "a1/s1",
		},
	)
	require.Error(t, err)
}

func TestA2Adapter_SubmitRunIdempotentHit(t *testing.T) {
	kv := newFakeKV()
	// Pre-populate the idempotency key.
	kv.m["automation:idem:auto-1/slot-1"] = "existing-wt-id"
	adapter := bootstrap.NewA2Adapter(newFakeWorkManager(), newFakeBrokerSubmitter(), kv)
	receipt, err := adapter.SubmitRun(
		context.Background(),
		automation.RunPayload{
			AutomationID:   "auto-1",
			RunID:          "run-1",
			Prompt:         "do thing",
			IdempotencyKey: "auto-1/slot-1",
		},
	)
	require.NoError(t, err)
	require.Equal(t, "existing-wt-id", receipt.WorkTaskID)
}

func TestA2Adapter_LookupNotFound(t *testing.T) {
	adapter := bootstrap.NewA2Adapter(newFakeWorkManager(), newFakeBrokerSubmitter(), newFakeKV())
	_, err := adapter.Lookup(context.Background(), "nonexistent")
	require.Error(t, err)
}

func TestA2Adapter_SubmitRunWorkCreateError(t *testing.T) {
	wm := newFakeWorkManager()
	kv := newFakeKV()
	adapter := bootstrap.NewA2Adapter(wm, newFakeBrokerSubmitter(), kv)
	// The fake WorkManager.Create rejects empty title, which should trigger
	// the work-create error path returning a partial receipt with BrokerTaskID.
	p := automation.RunPayload{
		AutomationID:   "",
		RunID:          "",
		Prompt:         "",
		IdempotencyKey: "auto-err",
	}
	_, err := adapter.SubmitRun(context.Background(), p)
	require.Error(t, err)
	// The error message must name "work create".
	require.Contains(t, err.Error(), "work create")
}

// errBroker returns an error on every Submit call.
type errBroker struct{}

func (errBroker) Submit(_, _, _ string) (string, error) {
	return "", errors.New("broker error")
}

func TestA2Adapter_SubmitRunBrokerError(t *testing.T) {
	adapter := bootstrap.NewA2Adapter(newFakeWorkManager(), errBroker{}, newFakeKV())
	_, err := adapter.SubmitRun(context.Background(), automation.RunPayload{
		AutomationID:   "a1",
		RunID:          "r1",
		Prompt:         "p",
		IdempotencyKey: "a1/s1",
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "broker submit")
}

// errKVSet returns an error only on KVSet for keys matching "automation:idem:".
type errKVSet struct {
	m map[string]string
}

func (kv *errKVSet) KVGet(key string) (string, bool, error) {
	v, ok := kv.m[key]
	return v, ok, nil
}

func (kv *errKVSet) KVSet(key, _ string) error {
	_ = key // suppress unused
	return errors.New("kv set error")
}

func TestA2Adapter_SubmitRunKVSetError(t *testing.T) {
	adapter := bootstrap.NewA2Adapter(newFakeWorkManager(), newFakeBrokerSubmitter(), &errKVSet{m: map[string]string{}})
	receipt, err := adapter.SubmitRun(context.Background(), automation.RunPayload{
		AutomationID:   "a1",
		RunID:          "r1",
		Prompt:         "p",
		IdempotencyKey: "a1/s1",
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "kv set idempotency")
	// Despite the KVSet error, the WorkTaskID should still be populated.
	require.NotEmpty(t, receipt.WorkTaskID, "WorkTaskID must be set even on KVSet error")
}

// TestA2Adapter_LookupUnknownStatus verifies that an unmapped task status
// results in RunFailed (fail-closed).
func TestA2Adapter_LookupUnknownStatus(t *testing.T) {
	wm := newFakeWorkManager()
	kv := newFakeKV()
	adapter := bootstrap.NewA2Adapter(wm, newFakeBrokerSubmitter(), kv)

	p := automation.RunPayload{
		AutomationID:   "auto-3",
		RunID:          "run-3",
		Prompt:         "hi",
		IdempotencyKey: "automation/auto-3/slot-3",
	}
	receipt, err := adapter.SubmitRun(context.Background(), p)
	require.NoError(t, err)

	// Set status to an unmapped value.
	wm.mu.Lock()
	if wt, ok := wm.tasks[receipt.WorkTaskID]; ok {
		wt.Status = work.Status("nonexistent_status")
	}
	wm.mu.Unlock()

	status, err := adapter.Lookup(context.Background(), receipt.WorkTaskID)
	require.NoError(t, err)
	require.Equal(t, automation.RunFailed, status.Status)
}
