package tools

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/guard"
	"github.com/x6nux/yanshi/internal/task/work"
)

// realTaskManager builds a work.Manager backed by a real SQLite file, so a
// second Manager over the same file can observe what the first wrote.
func realTaskManager(t *testing.T, path string) *work.Manager {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	st, err := work.FromDB(db, nil)
	require.NoError(t, err)
	return work.NewManager(st, nil, work.ArtifactPolicy{})
}

// TestUpdatePlanReachesTheDatabase joins the two halves the clause needs.
//
// The store/manager layer has SQLite round-trip tests, and the TOOL layer has
// tests — but every one of the tool tests runs against work.NewFakeManager(),
// an in-memory double. So "the checklist persists" was proved of the store, and
// "the tool writes a checklist" was proved of a fake: nothing joined them, and
// a tool that wrote to the manager's cache without reaching the database would
// pass both sets.
//
// The second Manager over the same file is what makes this a persistence test
// rather than a longer version of the fake one.
//
// ledger: A2/G05#4 checklist 状态持久
func TestUpdatePlanReachesTheDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "work.db")
	mgr := realTaskManager(t, path)

	ctx := context.Background()
	task, err := mgr.Create(ctx, work.CreateReq{Title: "x", Prompt: "p"})
	require.NoError(t, err)

	toolCtx := WithProfile(WithTaskManager(ctx, mgr), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"*"}},
	})
	out, err := runTool(toolCtx, NewPlanTools().UpdatePlan,
		`{"task_id":"`+task.ID+`","rows":[`+
			`{"id":1,"content":"read the spec","status":"done"},`+
			`{"id":2,"content":"write the parser","status":"in_progress"}]}`)
	require.NoError(t, err)
	require.NotContains(t, out, "permission denied", out)

	// A fresh Manager over the same file: nothing carries over in memory.
	reopened := realTaskManager(t, path)
	got, err := reopened.Read(ctx, task.ID)
	require.NoError(t, err, "the task did not survive to a second manager")
	require.NotNil(t, got.Checklist, "the checklist the tool wrote is not in the database")
	require.Len(t, got.Checklist.Items, 2)

	assert.Equal(t, "read the spec", got.Checklist.Items[0].Content)
	assert.Equal(t, work.ChecklistDone, got.Checklist.Items[0].Status)
	assert.Equal(t, "write the parser", got.Checklist.Items[1].Content)
	// The STATUS is the part worth checking separately: a store that persisted
	// the text and dropped the status would leave a plan that always reads as
	// not started.
	assert.Equal(t, work.ChecklistInProgress, got.Checklist.Items[1].Status)
}
