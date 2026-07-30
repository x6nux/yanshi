package work

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

// openTestStore 打开一个 in-memory SQLite Store 并完成 work schema 迁移。
// 每个测试用例独立拿到一个全新的内存库，避免相互污染。
func openTestStore(t *testing.T) *Store {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	require.NoError(t, err)
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	s, err := FromDB(db, nil)
	require.NoError(t, err)
	return s
}

func TestStoreCreatePersistsInitialTimeline(t *testing.T) {
	s := openTestStore(t)
	now := time.Now().Truncate(time.Second)
	w := &WorkTask{ID: "wt-1", Title: "x", Prompt: "p", Status: StatusPending, CreatedAt: now, UpdatedAt: now,
		Timeline: []TimelineEntry{{At: now, Kind: "created", Summary: "x"}}}
	require.NoError(t, s.Create(context.Background(), w))
	got, err := s.Get(context.Background(), w.ID)
	require.NoError(t, err)
	require.Len(t, got.Timeline, 1)
	assert.Equal(t, "created", got.Timeline[0].Kind)
}

func TestStoreListSingleConnectionDoesNotDeadlock(t *testing.T) {
	s := openTestStore(t)
	require.NoError(t, s.Create(context.Background(), &WorkTask{ID: "wt-1", Title: "x", Prompt: "p", Status: StatusPending, CreatedAt: time.Now(), UpdatedAt: time.Now()}))
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	got, err := s.List(ctx, 10, "")
	require.NoError(t, err)
	require.Len(t, got, 1)
}

func TestStoreTransitionUpdatesStatusAndAppendsTimeline(t *testing.T) {
	s := openTestStore(t)
	now := time.Now().Truncate(time.Second)
	require.NoError(t, s.Create(context.Background(), &WorkTask{ID: "wt-1", Title: "x", Prompt: "p", Status: StatusPending, CreatedAt: now, UpdatedAt: now}))
	require.NoError(t, s.Transition(context.Background(), "wt-1", StatusRunning, "start", "started"))
	got, err := s.Get(context.Background(), "wt-1")
	require.NoError(t, err)
	assert.Equal(t, StatusRunning, got.Status)
	require.Len(t, got.Timeline, 1)
	assert.Equal(t, "start", got.Timeline[0].Kind)

	// 转移到终态 completed
	require.NoError(t, s.Transition(context.Background(), "wt-1", StatusCompleted, "finish", "done"))
	// 终态拒绝继续转移
	require.Error(t, s.Transition(context.Background(), "wt-1", StatusRunning, "start", "again"))
}

func TestStorePatchChecklistItemNoReadModifyWrite(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	require.NoError(t, s.Create(ctx, &WorkTask{ID: "wt-1", Title: "x", Prompt: "p", Status: StatusPending, CreatedAt: time.Now(), UpdatedAt: time.Now(),
		Checklist: Checklist{Items: []ChecklistItem{
			{ID: 1, Content: "a", Status: ChecklistPending},
			{ID: 2, Content: "b", Status: ChecklistPending},
		}}}))
	var wg sync.WaitGroup
	wg.Add(2)
	errs := make(chan error, 2)
	go func() {
		defer wg.Done()
		errs <- s.PatchChecklistItem(ctx, "wt-1", 1, "", ChecklistDone)
	}()
	go func() {
		defer wg.Done()
		errs <- s.PatchChecklistItem(ctx, "wt-1", 2, "", ChecklistDone)
	}()
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	got, err := s.Get(ctx, "wt-1")
	require.NoError(t, err)
	require.Len(t, got.Checklist.Items, 2)
	statuses := map[int]ChecklistItemStatus{}
	for _, item := range got.Checklist.Items {
		statuses[item.ID] = item.Status
	}
	assert.Equal(t, ChecklistDone, statuses[1])
	assert.Equal(t, ChecklistDone, statuses[2])
}

func TestStoreSetChecklistAndAddItem(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	require.NoError(t, s.Create(ctx, &WorkTask{ID: "wt-1", Title: "x", Prompt: "p", Status: StatusPending, CreatedAt: time.Now(), UpdatedAt: time.Now()}))
	require.NoError(t, s.SetChecklist(ctx, "wt-1", Checklist{Items: []ChecklistItem{
		{ID: 1, Content: "a", Status: ChecklistPending},
		{ID: 2, Content: "b", Status: ChecklistPending},
	}}))
	got, err := s.Get(ctx, "wt-1")
	require.NoError(t, err)
	require.Len(t, got.Checklist.Items, 2)

	newID, err := s.AddChecklistItem(ctx, "wt-1", "c")
	require.NoError(t, err)
	assert.Equal(t, 3, newID)
	got, err = s.Get(ctx, "wt-1")
	require.NoError(t, err)
	require.Len(t, got.Checklist.Items, 3)
	var ids []int
	for _, item := range got.Checklist.Items {
		ids = append(ids, item.ID)
	}
	assert.Equal(t, []int{1, 2, 3}, ids)
}

func TestStoreRecordGate(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	require.NoError(t, s.Create(ctx, &WorkTask{ID: "wt-1", Title: "x", Prompt: "p", Status: StatusPending, CreatedAt: time.Now(), UpdatedAt: time.Now()}))
	ev := Evidence{
		ID:             "ev-1",
		Gate:           "test",
		Command:        "go test ./...",
		Cwd:            "/repo",
		ExitCode:       0,
		DurationMs:     10,
		Classification: "pass",
		Summary:        "all good",
		RecordedAt:     time.Now().Unix(),
	}
	require.NoError(t, s.RecordGate(ctx, "wt-1", ev))
	got, err := s.Get(ctx, "wt-1")
	require.NoError(t, err)
	require.Len(t, got.Gates, 1)
	assert.Equal(t, "pass", got.Gates[0].Classification)
	require.Len(t, got.Timeline, 1)
	assert.Equal(t, "gate", got.Timeline[0].Kind)

	// INSERT OR REPLACE: 同 gate 替换
	ev.ExitCode = 1
	ev.Classification = "fail"
	ev.Summary = "broken"
	require.NoError(t, s.RecordGate(ctx, "wt-1", ev))
	got, err = s.Get(ctx, "wt-1")
	require.NoError(t, err)
	require.Len(t, got.Gates, 1)
	assert.Equal(t, "fail", got.Gates[0].Classification)
}

func TestStorePutGetArtifact(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	require.NoError(t, s.Create(ctx, &WorkTask{ID: "wt-1", Title: "x", Prompt: "p", Status: StatusPending, CreatedAt: time.Now(), UpdatedAt: time.Now()}))
	a := Artifact{ID: "art-1", TaskID: "wt-1", Label: "log", Summary: "s",
		ContentRef: ".yanshi/artifacts/wt-1/art-1.txt", Size: 11, CreatedAt: time.Now().Unix()}
	require.NoError(t, s.PutArtifact(ctx, a))
	got, err := s.GetArtifact(ctx, a.ID)
	require.NoError(t, err)
	assert.Equal(t, a.ID, got.ID)
	assert.Equal(t, a.Size, got.Size)
	assert.Equal(t, a.ContentRef, got.ContentRef)

	// ArtifactBytes 聚合 task 已用配额
	used, err := s.ArtifactBytes(ctx, "wt-1")
	require.NoError(t, err)
	assert.Equal(t, int64(11), used)

	// 第二个 artifact 累加
	b := Artifact{ID: "art-2", TaskID: "wt-1", Label: "log2", Summary: "s",
		ContentRef: ".yanshi/artifacts/wt-1/art-2.txt", Size: 5, CreatedAt: time.Now().Unix()}
	require.NoError(t, s.PutArtifact(ctx, b))
	used, err = s.ArtifactBytes(ctx, "wt-1")
	require.NoError(t, err)
	assert.Equal(t, int64(16), used)

	// GetArtifact 对不存在 ID 返回 ErrNoRows
	_, err = s.GetArtifact(ctx, "nope")
	require.Error(t, err)
}

func TestStoreDeleteArtifactsBefore(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	require.NoError(t, s.Create(ctx, &WorkTask{ID: "wt-1", Title: "x", Prompt: "p", Status: StatusPending, CreatedAt: time.Now(), UpdatedAt: time.Now()}))
	old := time.Now().Add(-2 * time.Hour).Unix()
	require.NoError(t, s.PutArtifact(ctx, Artifact{ID: "art-1", TaskID: "wt-1", Label: "old", Summary: "s",
		ContentRef: "old-ref", Size: 3, CreatedAt: old}))
	require.NoError(t, s.PutArtifact(ctx, Artifact{ID: "art-2", TaskID: "wt-1", Label: "new", Summary: "s",
		ContentRef: "new-ref", Size: 3, CreatedAt: time.Now().Unix()}))

	refs, err := s.DeleteArtifactsBefore(ctx, time.Now().Add(-time.Hour).Unix())
	require.NoError(t, err)
	assert.Equal(t, []string{"old-ref"}, refs)
	// art-1 已删除
	_, err = s.GetArtifact(ctx, "art-1")
	require.Error(t, err)
	// art-2 仍在
	_, err = s.GetArtifact(ctx, "art-2")
	require.NoError(t, err)
}

func TestStoreAppendTimelineAndAttachBroker(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Now().Truncate(time.Second)
	require.NoError(t, s.Create(ctx, &WorkTask{ID: "wt-1", Title: "x", Prompt: "p", Status: StatusPending, CreatedAt: now, UpdatedAt: now}))

	later := now.Add(time.Minute)
	require.NoError(t, s.AppendTimeline(ctx, "wt-1", TimelineEntry{At: later, Kind: "dispatch_failed", Summary: "broker unreachable"}))
	got, err := s.Get(ctx, "wt-1")
	require.NoError(t, err)
	require.Len(t, got.Timeline, 1)
	assert.Equal(t, "dispatch_failed", got.Timeline[0].Kind)
	assert.True(t, got.UpdatedAt.After(now), "updated_at must move forward; got=%v prev=%v", got.UpdatedAt, now)

	// AttachBrokerTask 对不存在 ID 报错
	require.Error(t, s.AttachBrokerTask(ctx, "missing", "br-1"))

	// 对已存在 ID 写入并重读
	require.NoError(t, s.AttachBrokerTask(ctx, "wt-1", "br-42"))
	got, err = s.Get(ctx, "wt-1")
	require.NoError(t, err)
	assert.Equal(t, "br-42", got.BrokerTaskID)
}

func TestStoreGetNotFound(t *testing.T) {
	s := openTestStore(t)
	_, err := s.Get(context.Background(), "nope")
	require.Error(t, err)
	assert.True(t, errors.Is(err, sql.ErrNoRows), "want ErrNoRows got %v", err)
}

func TestStoreListThreadsFilter(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	require.NoError(t, s.Create(ctx, &WorkTask{ID: "wt-1", Title: "a", Prompt: "p", Status: StatusPending, ThreadID: "th-1", CreatedAt: time.Now(), UpdatedAt: time.Now()}))
	require.NoError(t, s.Create(ctx, &WorkTask{ID: "wt-2", Title: "b", Prompt: "p", Status: StatusPending, ThreadID: "th-2", CreatedAt: time.Now(), UpdatedAt: time.Now()}))
	got, err := s.List(ctx, 10, "th-1")
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "wt-1", got[0].ID)
}

func TestStorePutArtifactRejectsDuplicate(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	require.NoError(t, s.Create(ctx, &WorkTask{ID: "wt-1", Title: "x", Prompt: "p", Status: StatusPending, CreatedAt: time.Now(), UpdatedAt: time.Now()}))
	a := Artifact{ID: "art-1", TaskID: "wt-1", Label: "l", Summary: "s", ContentRef: "r", Size: 1, CreatedAt: time.Now().Unix()}
	require.NoError(t, s.PutArtifact(ctx, a))
	// duplicate id rejected
	require.Error(t, s.PutArtifact(ctx, a))
}
