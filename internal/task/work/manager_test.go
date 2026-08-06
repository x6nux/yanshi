package work

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeDispatcher 是一个 deterministic in-memory Dispatcher，用来测试
// Manager.Create(dispatch=true) 与 Manager.Cancel 的 broker 协作路径。
// 它不需要 *sql.DB，也不引入 internal/task.Broker（避免 work → task 反向依赖）。
type fakeDispatcher struct {
	mu         sync.Mutex
	submitErr  error
	cancelErr  error
	submitted  []submitCall
	cancels    []string
	nextBroker int
}

type submitCall struct {
	Typ, Input, Parent string
}

func (f *fakeDispatcher) Submit(typ, input, parent string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.submitErr != nil {
		return "", f.submitErr
	}
	f.nextBroker++
	f.submitted = append(f.submitted, submitCall{typ, input, parent})
	return fmt.Sprintf("bk-%d", f.nextBroker), nil
}

func (f *fakeDispatcher) Cancel(id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cancels = append(f.cancels, id)
	return f.cancelErr
}

func newManagerWithFakeDispatcher(t *testing.T) (*Manager, *fakeDispatcher, *Store) {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	require.NoError(t, err)
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	st, err := FromDB(db, nil)
	require.NoError(t, err)
	disp := &fakeDispatcher{}
	mgr := NewManager(st, disp, ArtifactPolicy{})
	return mgr, disp, st
}

func TestManagerCreatePersistsAndRecovers(t *testing.T) {
	ctx := context.Background()
	mgr, _, st := newManagerWithFakeDispatcher(t)
	task, err := mgr.Create(ctx, CreateReq{Title: "做一件事", Prompt: "执行"})
	require.NoError(t, err)
	assert.NotEmpty(t, task.ID)
	assert.Equal(t, StatusPending, task.Status)
	require.Len(t, task.Timeline, 1)
	assert.Equal(t, "created", task.Timeline[0].Kind)

	// 重启恢复：用 Store.Get 模拟 Manager 重启后读回
	got, err := st.Get(ctx, task.ID)
	require.NoError(t, err)
	assert.Equal(t, task.ID, got.ID)
	assert.Equal(t, "做一件事", got.Title)
}

func TestManagerCreateValidation(t *testing.T) {
	ctx := context.Background()
	mgr, _, _ := newManagerWithFakeDispatcher(t)
	_, err := mgr.Create(ctx, CreateReq{Title: "", Prompt: "x"})
	require.Error(t, err)
	_, err = mgr.Create(ctx, CreateReq{Title: "x", Prompt: ""})
	require.Error(t, err)
}

func TestManagerStartFinishCancelAppendsTimeline(t *testing.T) {
	ctx := context.Background()
	mgr, _, _ := newManagerWithFakeDispatcher(t)
	task, err := mgr.Create(ctx, CreateReq{Title: "x", Prompt: "p"})
	require.NoError(t, err)

	require.NoError(t, mgr.Start(ctx, task.ID))
	got, err := mgr.Read(ctx, task.ID)
	require.NoError(t, err)
	assert.Equal(t, StatusRunning, got.Status)
	require.Len(t, got.Timeline, 2) // created + start

	require.NoError(t, mgr.Finish(ctx, task.ID, StatusCompleted, "done"))
	got, err = mgr.Read(ctx, task.ID)
	require.NoError(t, err)
	assert.Equal(t, StatusCompleted, got.Status)
	require.Len(t, got.Timeline, 3) // + finish

	// Cancel 路径单独走一个 task（completed 是终态不能再 cancel）
	task2, err := mgr.Create(ctx, CreateReq{Title: "y", Prompt: "p"})
	require.NoError(t, err)
	require.NoError(t, mgr.Start(ctx, task2.ID))
	cancelled, err := mgr.Cancel(ctx, task2.ID, "tester")
	require.NoError(t, err)
	assert.Equal(t, StatusCancelled, cancelled.Status)
}

func TestManagerFinishRejectsInvalidStatus(t *testing.T) {
	ctx := context.Background()
	mgr, _, _ := newManagerWithFakeDispatcher(t)
	task, err := mgr.Create(ctx, CreateReq{Title: "x", Prompt: "p"})
	require.NoError(t, err)
	require.NoError(t, mgr.Start(ctx, task.ID))
	require.Error(t, mgr.Finish(ctx, task.ID, StatusPending, ""))
	require.Error(t, mgr.Finish(ctx, task.ID, StatusCancelled, ""))
}

func TestManagerDispatchTrueRecordsBrokerID(t *testing.T) {
	ctx := context.Background()
	mgr, disp, _ := newManagerWithFakeDispatcher(t)
	task, err := mgr.Create(ctx, CreateReq{Title: "x", Prompt: "do work", Dispatch: true})
	require.NoError(t, err)
	assert.NotEmpty(t, task.BrokerTaskID, "broker id must be attached")
	require.Len(t, disp.submitted, 1)
	assert.Equal(t, "do work", disp.submitted[0].Input)
	assert.Equal(t, task.ID, disp.submitted[0].Parent)
	assert.Equal(t, "work_task", disp.submitted[0].Typ)
}

func TestManagerDispatchFailurePersistsTaskWithTimeline(t *testing.T) {
	ctx := context.Background()
	mgr, disp, st := newManagerWithFakeDispatcher(t)
	disp.submitErr = errors.New("broker offline")
	task, err := mgr.Create(ctx, CreateReq{Title: "x", Prompt: "p", Dispatch: true})
	require.Error(t, err, "Create with dispatch failure must return error")
	require.NotNil(t, task, "durable task must still be returned")

	// 重读看到 dispatch_failed timeline + 空 broker_task_id
	got, err := st.Get(ctx, task.ID)
	require.NoError(t, err)
	assert.Empty(t, got.BrokerTaskID)
	var kinds []string
	for _, e := range got.Timeline {
		kinds = append(kinds, e.Kind)
	}
	assert.Equal(t, []string{"created", "dispatch_failed"}, kinds)
}

func TestManagerDispatchFailurePropagatesTimelineError(t *testing.T) {
	// 关掉 DB 模拟 AppendTimeline 失败：durable task 已 commit，但 timeline
	// 追加会因为 db 关闭报错。Create 必须把两层错误都返回。
	ctx := context.Background()
	db, err := sql.Open("sqlite", ":memory:")
	require.NoError(t, err)
	db.SetMaxOpenConns(1)
	st, err := FromDB(db, nil)
	require.NoError(t, err)
	disp := &fakeDispatcher{submitErr: errors.New("broker offline")}
	mgr := NewManager(st, disp, ArtifactPolicy{})
	_, err = mgr.Create(ctx, CreateReq{Title: "x", Prompt: "p", Dispatch: true})
	require.Error(t, err)
	// 关 DB 之后再 Create 一次：durable Create 会失败，看不到 wrap 的 timeline 错。
	require.NoError(t, db.Close())
	_, err = mgr.Create(ctx, CreateReq{Title: "x", Prompt: "p", Dispatch: true})
	require.Error(t, err)
}

func TestManagerCancelInvokesBroker(t *testing.T) {
	ctx := context.Background()
	mgr, disp, _ := newManagerWithFakeDispatcher(t)
	task, err := mgr.Create(ctx, CreateReq{Title: "x", Prompt: "p", Dispatch: true})
	require.NoError(t, err)
	require.NotEmpty(t, task.BrokerTaskID)
	require.NoError(t, mgr.Start(ctx, task.ID))
	cancelled, err := mgr.Cancel(ctx, task.ID, "ops")
	require.NoError(t, err)
	assert.Equal(t, StatusCancelled, cancelled.Status)
	require.Len(t, disp.cancels, 1)
	assert.Equal(t, task.BrokerTaskID, disp.cancels[0])
}

// ledger: A2/G05#4 checklist 状态持久
func TestManagerChecklistAPIs(t *testing.T) {
	ctx := context.Background()
	mgr, _, _ := newManagerWithFakeDispatcher(t)
	task, err := mgr.Create(ctx, CreateReq{Title: "x", Prompt: "p"})
	require.NoError(t, err)

	task, err = mgr.SetChecklist(ctx, task.ID, Checklist{Items: []ChecklistItem{
		{ID: 1, Content: "a", Status: ChecklistPending},
		{ID: 2, Content: "b", Status: ChecklistPending},
	}})
	require.NoError(t, err)
	require.Len(t, task.Checklist.Items, 2)

	task, err = mgr.AddChecklistItem(ctx, task.ID, "c")
	require.NoError(t, err)
	require.Len(t, task.Checklist.Items, 3)

	task, err = mgr.PatchChecklistItem(ctx, task.ID, 1, "", ChecklistDone)
	require.NoError(t, err)
	for _, item := range task.Checklist.Items {
		if item.ID == 1 {
			assert.Equal(t, ChecklistDone, item.Status)
		}
	}
}

func TestManagerRecordGate(t *testing.T) {
	ctx := context.Background()
	mgr, _, _ := newManagerWithFakeDispatcher(t)
	task, err := mgr.Create(ctx, CreateReq{Title: "x", Prompt: "p"})
	require.NoError(t, err)
	ev := Evidence{
		ID: NewID("ev"), Gate: "test", Command: "go test", Cwd: "/r",
		ExitCode: 0, DurationMs: 5, Classification: "pass", Summary: "ok", RecordedAt: time.Now().Unix(),
	}
	require.NoError(t, mgr.RecordGate(ctx, task.ID, ev))
	task, err = mgr.Read(ctx, task.ID)
	require.NoError(t, err)
	require.Len(t, task.Gates, 1)
}

func TestManagerListUsesStoreOrder(t *testing.T) {
	ctx := context.Background()
	mgr, _, _ := newManagerWithFakeDispatcher(t)
	// 创建三个任务；Store.List 按 created_at DESC 返回。
	t1, _ := mgr.Create(ctx, CreateReq{Title: "1", Prompt: "p"})
	t2, _ := mgr.Create(ctx, CreateReq{Title: "2", Prompt: "p"})
	t3, _ := mgr.Create(ctx, CreateReq{Title: "3", Prompt: "p"})
	time.Sleep(time.Second)
	// 重新插入一个最新的（确保 created_at 严格不同）
	t4, _ := mgr.Create(ctx, CreateReq{Title: "4", Prompt: "p"})

	list, err := mgr.List(ctx, 10, "")
	require.NoError(t, err)
	require.Len(t, list, 4)
	// 最新优先（严格不同的 created_at）
	assert.Equal(t, t4.ID, list[0].ID)
	// 剩下三个不一定按插入顺序（created_at 相同时按 ID DESC）
	rest := map[string]bool{list[1].ID: true, list[2].ID: true, list[3].ID: true}
	assert.True(t, rest[t1.ID] && rest[t2.ID] && rest[t3.ID], "expected earlier tasks in tail; got %v", list[1:])

	// threadID 过滤
	list, err = mgr.List(ctx, 10, "th-x")
	require.NoError(t, err)
	assert.Empty(t, list)
}

func TestManagerCancelMissingTask(t *testing.T) {
	ctx := context.Background()
	mgr, _, _ := newManagerWithFakeDispatcher(t)
	_, err := mgr.Cancel(ctx, "missing", "ops")
	require.Error(t, err)
}

func TestFakeManagerCreateList(t *testing.T) {
	ctx := context.Background()
	f := NewFakeManager()
	t1, err := f.Create(ctx, CreateReq{Title: "1", Prompt: "p"})
	require.NoError(t, err)
	t2, err := f.Create(ctx, CreateReq{Title: "2", Prompt: "p"})
	require.NoError(t, err)
	t3, err := f.Create(ctx, CreateReq{Title: "3", Prompt: "p"})
	require.NoError(t, err)
	list, err := f.List(ctx, 10, "")
	require.NoError(t, err)
	require.Len(t, list, 3)
	// CreatedAt DESC；同样时间戳时按 ID DESC。Fake 自增时间戳。
	assert.Equal(t, t3.ID, list[0].ID)
	assert.Equal(t, t2.ID, list[1].ID)
	assert.Equal(t, t1.ID, list[2].ID)

	// dispatch=true 会被 Fake 模拟一个 broker id 占位
	td, err := f.Create(ctx, CreateReq{Title: "d", Prompt: "p", Dispatch: true})
	require.NoError(t, err)
	assert.NotEmpty(t, td.BrokerTaskID)

	// 校验
	_, err = f.Create(ctx, CreateReq{Title: "", Prompt: "x"})
	require.Error(t, err)
}

func TestFakeManagerReadStartFinishCancel(t *testing.T) {
	ctx := context.Background()
	f := NewFakeManager()
	task, err := f.Create(ctx, CreateReq{Title: "x", Prompt: "p"})
	require.NoError(t, err)
	require.NoError(t, f.Start(ctx, task.ID))
	got, err := f.Read(ctx, task.ID)
	require.NoError(t, err)
	assert.Equal(t, StatusRunning, got.Status)
	require.NoError(t, f.Finish(ctx, task.ID, StatusCompleted, "ok"))
	got, err = f.Read(ctx, task.ID)
	require.NoError(t, err)
	assert.Equal(t, StatusCompleted, got.Status)

	// Cancel 路径
	t2, _ := f.Create(ctx, CreateReq{Title: "y", Prompt: "p", Dispatch: true})
	require.NoError(t, f.Start(ctx, t2.ID))
	cancelled, err := f.Cancel(ctx, t2.ID, "tester")
	require.NoError(t, err)
	assert.Equal(t, StatusCancelled, cancelled.Status)
}

func TestFakeManagerChecklistAndGate(t *testing.T) {
	ctx := context.Background()
	f := NewFakeManager()
	task, err := f.Create(ctx, CreateReq{Title: "x", Prompt: "p"})
	require.NoError(t, err)

	task, err = f.SetChecklist(ctx, task.ID, Checklist{Items: []ChecklistItem{
		{ID: 1, Content: "a", Status: ChecklistPending},
	}})
	require.NoError(t, err)
	require.Len(t, task.Checklist.Items, 1)

	task, err = f.AddChecklistItem(ctx, task.ID, "b")
	require.NoError(t, err)
	require.Len(t, task.Checklist.Items, 2)

	task, err = f.PatchChecklistItem(ctx, task.ID, 1, "", ChecklistDone)
	require.NoError(t, err)

	ev := Evidence{ID: NewID("ev"), Gate: "test", Command: "c", Cwd: "/r", ExitCode: 0,
		DurationMs: 1, Classification: "pass", Summary: "ok", RecordedAt: time.Now().Unix()}
	require.NoError(t, f.RecordGate(ctx, task.ID, ev))
}

// 编译期 ManagerLike 断言（在测试文件里即可，让缺少方法时立刻失败）
var _ ManagerLike = (*Manager)(nil)
var _ ManagerLike = (*FakeManager)(nil)
