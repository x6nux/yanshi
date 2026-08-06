package tools

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/task/work"

	_ "modernc.org/sqlite"
)

// fileBackedManager builds a work.Manager over a real SQLite file.
//
// Every existing task-tool test runs against work.NewFakeManager(), an
// in-memory double. That proves the tool code runs; it cannot prove anything
// reaches a database, which is what "durable" means.
func fileBackedManager(t *testing.T, path string) *work.Manager {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	st, err := work.FromDB(db, nil)
	require.NoError(t, err)
	return work.NewManager(st, nil, work.ArtifactPolicy{})
}

func taskPayload(t *testing.T, out string) *work.WorkTask {
	t.Helper()
	var p struct {
		Task *work.WorkTask `json:"task"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &p), "tool output: %s", out)
	require.NotNil(t, p.Task)
	return p.Task
}

// TestTaskToolsRoundTripThroughARealDatabase is the create/list/read/cancel
// clause against something durable.
//
// The pre-existing round trip runs on a FakeManager, so it holds for a tool
// that writes to a map and never touches SQL. Reopening the file between the
// write and the read is what separates the two.
//
// ledger: A2/DT1#1 可创建/列出/读取/取消
func TestTaskToolsRoundTripThroughARealDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "work.db")
	mgr := fileBackedManager(t, path)
	ctx := WithProfile(WithTaskManager(context.Background(), mgr), wildcardProfile())
	tt := NewTaskTools()

	out, err := runTool(ctx, tt.Create, `{"title":"ship it","prompt":"do the thing"}`)
	require.NoError(t, err)
	created := taskPayload(t, out)
	require.NotEmpty(t, created.ID)

	out, err = runTool(ctx, tt.List, `{"limit":5}`)
	require.NoError(t, err)
	var listed struct {
		Tasks []work.Summary `json:"tasks"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &listed))
	require.Len(t, listed.Tasks, 1)
	assert.Equal(t, created.ID, listed.Tasks[0].ID)

	out, err = runTool(ctx, tt.Read, `{"id":"`+created.ID+`"}`)
	require.NoError(t, err)
	assert.Equal(t, "ship it", taskPayload(t, out).Title)

	cancelCtx := WithPermissionCallback(ctx, func(req PermissionRequest) PermissionDecision {
		require.True(t, req.ForcePrompt, "task_cancel must set ForcePrompt=true")
		return PermissionAllow
	})
	out, err = runTool(cancelCtx, tt.Cancel, `{"id":"`+created.ID+`"}`)
	require.NoError(t, err)
	require.Equal(t, work.StatusCancelled, taskPayload(t, out).Status)

	// A second Manager over the same file. Nothing carries over in memory, so
	// every field below came off disk.
	reopened := fileBackedManager(t, path)
	ctx2 := WithProfile(WithTaskManager(context.Background(), reopened), wildcardProfile())
	out, err = runTool(ctx2, tt.Read, `{"id":"`+created.ID+`"}`)
	require.NoError(t, err)
	after := taskPayload(t, out)
	assert.Equal(t, "ship it", after.Title, "the title did not reach the database")
	assert.Equal(t, "do the thing", after.Prompt)
	assert.Equal(t, work.StatusCancelled, after.Status,
		"the cancellation was only in memory: a restart would show the task as still pending")
}

// TestTaskListIsolatesThreads is the thread-link clause seen from the tool.
//
// The injection chain (WithThreadLink → runCreate → thread_id column) has
// assertions, but nothing checked that task_list actually FILTERS on the
// column it writes. A list that ignored the filter would leak every other
// conversation's tasks into this one's context window, and the write-side
// tests would all still pass.
//
// ledger: A2/DT1#3 thread/turn 关联准确
func TestTaskListIsolatesThreads(t *testing.T) {
	mgr := fileBackedManager(t, filepath.Join(t.TempDir(), "work.db"))
	tt := NewTaskTools()

	mk := func(thread, turn, title string) string {
		ctx := WithProfile(
			WithThreadLink(WithTaskManager(context.Background(), mgr), thread, turn),
			wildcardProfile())
		out, err := runTool(ctx, tt.Create, `{"title":"`+title+`","prompt":"p"}`)
		require.NoError(t, err)
		task := taskPayload(t, out)
		require.Equal(t, thread, task.ThreadID, "the thread link did not reach the row")
		require.Equal(t, turn, task.TurnID, "the turn link did not reach the row")
		return task.ID
	}

	aID := mk("thread-a", "turn-a1", "task in A")
	// Same thread, later turn: the thread groups, the turn distinguishes.
	a2ID := mk("thread-a", "turn-a2", "second task in A")
	bID := mk("thread-b", "turn-b1", "task in B")

	listIn := func(thread string) []string {
		ctx := WithProfile(
			WithThreadLink(WithTaskManager(context.Background(), mgr), thread, "whatever"),
			wildcardProfile())
		out, err := runTool(ctx, tt.List, `{"limit":50}`)
		require.NoError(t, err)
		var listed struct {
			Tasks []work.Summary `json:"tasks"`
		}
		require.NoError(t, json.Unmarshal([]byte(out), &listed))
		ids := make([]string, 0, len(listed.Tasks))
		for _, s := range listed.Tasks {
			ids = append(ids, s.ID)
		}
		return ids
	}

	inA := listIn("thread-a")
	assert.ElementsMatch(t, []string{aID, a2ID}, inA,
		"thread A sees %v; a list that ignores the thread filter puts another "+
			"conversation's tasks into this one's context", inA)
	assert.ElementsMatch(t, []string{bID}, listIn("thread-b"))
}
