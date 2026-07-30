package work

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// dbClosingDispatcher is a fakeDispatcher whose Submit/Cancel close the store's
// DB as a side effect. This lets us exercise the manager error-wrap branches
// that require AppendTimeline / AttachBrokerTask / Transition to fail *after*
// the durable Create has already committed.
type dbClosingDispatcher struct {
	closeDB      func()
	submitErr    error
	submitSucceeds bool
}

func (d *dbClosingDispatcher) Submit(_, _, _ string) (string, error) {
	if d.closeDB != nil {
		d.closeDB()
	}
	if d.submitErr != nil {
		return "", d.submitErr
	}
	return "bk-1", nil
}
func (d *dbClosingDispatcher) Cancel(string) error {
	if d.closeDB != nil {
		d.closeDB()
	}
	return nil
}

// openStore builds an in-memory store with the standard single-conn setup.
func openStore(t *testing.T) (*Store, *sql.DB) {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	require.NoError(t, err)
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	st, err := FromDB(db, nil)
	require.NoError(t, err)
	return st, db
}

// TestManagerCreateDispatchFailTimelineLogFail covers the branch where
// dispatcher.Submit fails AND the subsequent AppendTimeline also fails (DB
// closed by the dispatcher): Create must return an error wrapping both.
func TestManagerCreateDispatchFailTimelineLogFail(t *testing.T) {
	ctx := context.Background()
	st, db := openStore(t)
	disp := &dbClosingDispatcher{
		closeDB:   func() { _ = db.Close() },
		submitErr: errors.New("broker offline"),
	}
	mgr := NewManager(st, disp, ArtifactPolicy{})
	task, err := mgr.Create(ctx, CreateReq{Title: "x", Prompt: "p", Dispatch: true})
	require.Error(t, err)
	require.NotNil(t, task)
}

// TestManagerCreateAttachBrokerFail covers the AttachBrokerTask error branch:
// Submit succeeds but closes the DB, so AttachBrokerTask fails.
func TestManagerCreateAttachBrokerFail(t *testing.T) {
	ctx := context.Background()
	st, db := openStore(t)
	disp := &dbClosingDispatcher{
		closeDB:        func() { _ = db.Close() },
		submitSucceeds: true,
	}
	mgr := NewManager(st, disp, ArtifactPolicy{})
	task, err := mgr.Create(ctx, CreateReq{Title: "x", Prompt: "p", Dispatch: true})
	require.Error(t, err)
	require.NotNil(t, task)
}

// TestManagerCancelTransitionFail covers the Transition error branch of Cancel:
// broker cancel succeeds but closes the DB, so the status transition fails. We
// create the task WITHOUT dispatch (so Submit doesn't close the DB during
// Create), set the broker id manually, then Cancel.
func TestManagerCancelTransitionFail(t *testing.T) {
	ctx := context.Background()
	st, db := openStore(t)
	disp := &dbClosingDispatcher{closeDB: func() { _ = db.Close() }}
	mgr := NewManager(st, disp, ArtifactPolicy{})
	task, err := mgr.Create(ctx, CreateReq{Title: "x", Prompt: "p"})
	require.NoError(t, err)
	// Assign a broker id so Cancel takes the broker-cancel branch.
	require.NoError(t, st.AttachBrokerTask(ctx, task.ID, "bk-3"))
	require.NoError(t, mgr.Start(ctx, task.ID))
	_, err = mgr.Cancel(ctx, task.ID, "ops")
	require.Error(t, err)
}

// --- Get scan-error branches via type-mismatched child rows ---

// corruptChecklist inserts a checklist row whose item_id is non-numeric, so
// Store.Get's scan into an int fails.
func TestStoreGetChecklistScanError(t *testing.T) {
	ctx := context.Background()
	st, db := openStore(t)
	_ = db
	mgr := NewManager(st, nil, ArtifactPolicy{})
	task, err := mgr.Create(ctx, CreateReq{Title: "x", Prompt: "p"})
	require.NoError(t, err)
	_, err = st.db.ExecContext(ctx, `INSERT INTO task_work_checklist(task_id,item_id,content,status) VALUES(?, 'NaN', 'c', 'pending')`, task.ID)
	require.NoError(t, err)
	_, err = st.Get(ctx, task.ID)
	require.Error(t, err)
}

// TestStoreGetGatesScanError inserts a gate with a non-integer exit_code.
func TestStoreGetGatesScanError(t *testing.T) {
	ctx := context.Background()
	st, _ := openStore(t)
	mgr := NewManager(st, nil, ArtifactPolicy{})
	task, err := mgr.Create(ctx, CreateReq{Title: "x", Prompt: "p"})
	require.NoError(t, err)
	_, err = st.db.ExecContext(ctx, `INSERT INTO task_work_gates
		(task_id,gate,id,command,cwd,exit_code,duration_ms,classification,summary,log_artifact_id,recorded_at)
		VALUES(?, 'g', 'id', 'cmd', '/r', 'NaN', 1, 'pass', 's', '', ?)`, task.ID, time.Now().Unix())
	require.NoError(t, err)
	_, err = st.Get(ctx, task.ID)
	require.Error(t, err)
}

// TestStoreGetArtifactsScanError inserts an artifact with a non-integer size.
func TestStoreGetArtifactsScanError(t *testing.T) {
	ctx := context.Background()
	st, _ := openStore(t)
	mgr := NewManager(st, nil, ArtifactPolicy{})
	task, err := mgr.Create(ctx, CreateReq{Title: "x", Prompt: "p"})
	require.NoError(t, err)
	_, err = st.db.ExecContext(ctx, `INSERT INTO task_work_artifacts(id,task_id,label,summary,content_ref,size,created_at)
		VALUES('a', ?, 'l', 's', 'r', 'NaN', ?)`, task.ID, time.Now().Unix())
	require.NoError(t, err)
	_, err = st.Get(ctx, task.ID)
	require.Error(t, err)
}

// TestStoreGetTimelineScanError inserts a timeline row with a non-integer at.
func TestStoreGetTimelineScanError(t *testing.T) {
	ctx := context.Background()
	st, _ := openStore(t)
	mgr := NewManager(st, nil, ArtifactPolicy{})
	task, err := mgr.Create(ctx, CreateReq{Title: "x", Prompt: "p"})
	require.NoError(t, err)
	_, err = st.db.ExecContext(ctx, `INSERT INTO task_work_timeline(task_id,at,kind,summary,detail_artifact_id)
		VALUES(?, 'NaN', 'k', 's', '')`, task.ID)
	require.NoError(t, err)
	_, err = st.Get(ctx, task.ID)
	require.Error(t, err)
}

// TestStoreGetArtifactScanError inserts an artifact with a non-integer size and
// reads it directly via GetArtifact (covers GetArtifact's scan-error branch).
func TestStoreGetArtifactScanError(t *testing.T) {
	ctx := context.Background()
	st, _ := openStore(t)
	_, err := st.db.ExecContext(ctx, `INSERT INTO task_work_artifacts(id,task_id,label,summary,content_ref,size,created_at)
		VALUES('a1', 't', 'l', 's', 'r', 'NaN', 0)`)
	require.NoError(t, err)
	_, err = st.GetArtifact(ctx, "a1")
	require.Error(t, err)
}

// TestStoreArtifactBytesScanError corrupts the SUM result so the NullInt64 scan
// ... ArtifactBytes uses NullInt64 which never fails on text, so instead we
// verify the not-found path returns 0 cleanly (the existing happy path).
func TestStoreArtifactBytes(t *testing.T) {
	ctx := context.Background()
	st, _ := openStore(t)
	n, err := st.ArtifactBytes(ctx, "ghost")
	require.NoError(t, err)
	require.Zero(t, n)
}

// TestStoreDeleteArtifactsBeforeNone covers the empty-refs early return.
func TestStoreDeleteArtifactsBeforeNone(t *testing.T) {
	ctx := context.Background()
	st, _ := openStore(t)
	refs, err := st.DeleteArtifactsBefore(ctx, time.Now().Unix())
	require.NoError(t, err)
	require.Empty(t, refs)
}

// TestStoreDeleteArtifactsBeforeScanError inserts a row that makes the content_ref
// scan fail is impossible (TEXT column always scans); instead exercise the delete
// path with a real row to cover the DELETE branch.
func TestStoreDeleteArtifactsBeforeWithRows(t *testing.T) {
	ctx := context.Background()
	st, _ := openStore(t)
	_, err := st.db.ExecContext(ctx, `INSERT INTO task_work_artifacts(id,task_id,label,summary,content_ref,size,created_at)
		VALUES('a2','t','l','s','ref', 1, ?)`, time.Now().Add(-time.Hour).Unix())
	require.NoError(t, err)
	refs, err := st.DeleteArtifactsBefore(ctx, time.Now().Unix())
	require.NoError(t, err)
	require.Len(t, refs, 1)
}

// TestStoreCreateWithChecklistAndTimeline covers the inner insert loops of
// Store.Create (checklist items + timeline entries) with non-empty slices.
func TestStoreCreateWithChecklistAndTimeline(t *testing.T) {
	ctx := context.Background()
	st, _ := openStore(t)
	now := time.Now()
	w := &WorkTask{
		ID: "wt-c", Title: "t", Prompt: "p", Status: StatusPending, CreatedAt: now, UpdatedAt: now,
		Checklist: Checklist{Items: []ChecklistItem{
			{ID: 1, Content: "a", Status: ChecklistPending},
			{ID: 2, Content: "b", Status: ChecklistDone},
		}},
		Timeline: []TimelineEntry{
			{At: now, Kind: "created", Summary: "s"},
			{At: now, Kind: "note", Summary: "n", DetailArtifactID: "art-x"},
		},
	}
	require.NoError(t, st.Create(ctx, w))
	got, err := st.Get(ctx, w.ID)
	require.NoError(t, err)
	require.Len(t, got.Checklist.Items, 2)
	require.Len(t, got.Timeline, 2)
}

// TestStoreGetMissing covers Store.Get on a nonexistent id (sql.ErrNoRows).
func TestStoreGetMissing(t *testing.T) {
	ctx := context.Background()
	st, _ := openStore(t)
	_, err := st.Get(ctx, "ghost")
	require.Error(t, err)
	require.ErrorIs(t, err, sql.ErrNoRows)
}

// TestStoreListScanError corrupts a column so Store.List's Scan fails.
func TestStoreListScanError(t *testing.T) {
	ctx := context.Background()
	st, _ := openStore(t)
	// Insert a task row with a non-integer created_at so the List Scan fails.
	_, err := st.db.ExecContext(ctx, `INSERT INTO task_work(id,title,prompt,status,thread_id,turn_id,broker_task_id,created_at,updated_at)
		VALUES('wt-le','t','p','pending','','','','NaN','NaN')`)
	require.NoError(t, err)
	_, err = st.List(ctx, 10, "")
	require.Error(t, err)
}

// TestStoreAttachBrokerTaskRowsAffectedError is hard to trigger directly; the
// not-found case (n==0) is covered by TestStoreAttachBrokerTaskMissing. Here we
// exercise the success path to keep AttachBrokerTask's happy branch warm.
func TestStoreAttachBrokerTaskSuccess(t *testing.T) {
	ctx := context.Background()
	st, _ := openStore(t)
	mgr := NewManager(st, nil, ArtifactPolicy{})
	task, err := mgr.Create(ctx, CreateReq{Title: "x", Prompt: "p"})
	require.NoError(t, err)
	require.NoError(t, st.AttachBrokerTask(ctx, task.ID, "bk-7"))
}

// TestStoreRecordGateExisting covers RecordGate's INSERT OR REPLACE replacing an
// existing gate (same task + gate name).
func TestStoreRecordGateExisting(t *testing.T) {
	ctx := context.Background()
	st, _ := openStore(t)
	mgr := NewManager(st, nil, ArtifactPolicy{})
	task, err := mgr.Create(ctx, CreateReq{Title: "x", Prompt: "p"})
	require.NoError(t, err)
	ev := Evidence{ID: "ev1", Gate: "test", Command: "c", Cwd: "/r", ExitCode: 0, DurationMs: 1, Classification: "pass", Summary: "ok", RecordedAt: time.Now().Unix()}
	require.NoError(t, st.RecordGate(ctx, task.ID, ev))
	ev2 := ev
	ev2.ID = "ev2"
	ev2.Summary = "ok2"
	require.NoError(t, st.RecordGate(ctx, task.ID, ev2))
	got, err := st.Get(ctx, task.ID)
	require.NoError(t, err)
	require.Len(t, got.Gates, 1)
}

// --- inner statement-error branches via DROP TABLE ---
//
// Dropping a table after a task exists makes BeginTx succeed but the next
// statement against the missing table fail — the cleanest way to exercise the
// per-statement error returns inside Store's transactional methods without a
// custom driver.

// dropTable drops a work schema table on the given store.
func dropTable(t *testing.T, st *Store, table string) {
	t.Helper()
	_, err := st.db.Exec("DROP TABLE " + table)
	require.NoError(t, err)
}

// TestStoreGetQueryErrors covers the four child-query error branches of Get by
// dropping each child table in turn.
func TestStoreGetQueryErrors(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		drop string
	}{
		{"checklist", "task_work_checklist"},
		{"gates", "task_work_gates"},
		{"artifacts", "task_work_artifacts"},
		{"timeline", "task_work_timeline"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, _ := openStore(t)
			mgr := NewManager(st, nil, ArtifactPolicy{})
			task, err := mgr.Create(ctx, CreateReq{Title: "x", Prompt: "p"})
			require.NoError(t, err)
			dropTable(t, st, tc.drop)
			_, err = st.Get(ctx, task.ID)
			require.Error(t, err)
		})
	}
}

// TestStoreCreateInsertErrors covers Create's task/checklist/timeline insert
// error branches by dropping the relevant table and calling Store.Create
// directly with a populated WorkTask.
func TestStoreCreateInsertErrors(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	t.Run("task insert", func(t *testing.T) {
		st, _ := openStore(t)
		dropTable(t, st, "task_work")
		err := st.Create(ctx, &WorkTask{ID: "x", Title: "t", Prompt: "p", Status: StatusPending, CreatedAt: now, UpdatedAt: now})
		require.Error(t, err)
	})
	t.Run("checklist insert", func(t *testing.T) {
		st, _ := openStore(t)
		dropTable(t, st, "task_work_checklist")
		err := st.Create(ctx, &WorkTask{
			ID: "x", Title: "t", Prompt: "p", Status: StatusPending, CreatedAt: now, UpdatedAt: now,
			Checklist: Checklist{Items: []ChecklistItem{{ID: 1, Content: "a", Status: ChecklistPending}}},
		})
		require.Error(t, err)
	})
	t.Run("timeline insert", func(t *testing.T) {
		st, _ := openStore(t)
		dropTable(t, st, "task_work_timeline")
		err := st.Create(ctx, &WorkTask{
			ID: "x", Title: "t", Prompt: "p", Status: StatusPending, CreatedAt: now, UpdatedAt: now,
			Timeline: []TimelineEntry{{At: now, Kind: "created", Summary: "s"}},
		})
		require.Error(t, err)
	})
}

// TestStoreTransitionSelectError covers the SELECT-status error branch of
// Transition (task_work dropped mid-call).
func TestStoreTransitionSelectError(t *testing.T) {
	ctx := context.Background()
	st, _ := openStore(t)
	mgr := NewManager(st, nil, ArtifactPolicy{})
	task, err := mgr.Create(ctx, CreateReq{Title: "x", Prompt: "p"})
	require.NoError(t, err)
	dropTable(t, st, "task_work")
	require.Error(t, st.Transition(ctx, task.ID, StatusRunning, "started", "s"))
}

// TestStoreAppendTimelineErrors covers AppendTimeline's insert-error and
// update-error branches.
func TestStoreAppendTimelineErrors(t *testing.T) {
	ctx := context.Background()
	t.Run("timeline insert", func(t *testing.T) {
		st, _ := openStore(t)
		mgr := NewManager(st, nil, ArtifactPolicy{})
		task, err := mgr.Create(ctx, CreateReq{Title: "x", Prompt: "p"})
		require.NoError(t, err)
		dropTable(t, st, "task_work_timeline")
		require.Error(t, st.AppendTimeline(ctx, task.ID, TimelineEntry{Kind: "k", Summary: "s", At: time.Now()}))
	})
	t.Run("task update", func(t *testing.T) {
		st, _ := openStore(t)
		mgr := NewManager(st, nil, ArtifactPolicy{})
		task, err := mgr.Create(ctx, CreateReq{Title: "x", Prompt: "p"})
		require.NoError(t, err)
		dropTable(t, st, "task_work")
		require.Error(t, st.AppendTimeline(ctx, task.ID, TimelineEntry{Kind: "k", Summary: "s", At: time.Now()}))
	})
}

// TestStoreSetChecklistErrors covers SetChecklist's delete-error and update-error
// branches.
func TestStoreSetChecklistErrors(t *testing.T) {
	ctx := context.Background()
	t.Run("checklist delete", func(t *testing.T) {
		st, _ := openStore(t)
		mgr := NewManager(st, nil, ArtifactPolicy{})
		task, err := mgr.Create(ctx, CreateReq{Title: "x", Prompt: "p"})
		require.NoError(t, err)
		dropTable(t, st, "task_work_checklist")
		require.Error(t, st.SetChecklist(ctx, task.ID, Checklist{Items: []ChecklistItem{{ID: 1, Content: "a"}}}))
	})
	t.Run("task update", func(t *testing.T) {
		st, _ := openStore(t)
		mgr := NewManager(st, nil, ArtifactPolicy{})
		task, err := mgr.Create(ctx, CreateReq{Title: "x", Prompt: "p"})
		require.NoError(t, err)
		dropTable(t, st, "task_work")
		require.Error(t, st.SetChecklist(ctx, task.ID, Checklist{}))
	})
}

// TestStoreAddChecklistItemErrors covers AddChecklistItem's select-error and
// task-update-error branches.
func TestStoreAddChecklistItemErrors(t *testing.T) {
	ctx := context.Background()
	t.Run("checklist select", func(t *testing.T) {
		st, _ := openStore(t)
		mgr := NewManager(st, nil, ArtifactPolicy{})
		task, err := mgr.Create(ctx, CreateReq{Title: "x", Prompt: "p"})
		require.NoError(t, err)
		dropTable(t, st, "task_work_checklist")
		_, err = st.AddChecklistItem(ctx, task.ID, "c")
		require.Error(t, err)
	})
	t.Run("task update", func(t *testing.T) {
		st, _ := openStore(t)
		mgr := NewManager(st, nil, ArtifactPolicy{})
		task, err := mgr.Create(ctx, CreateReq{Title: "x", Prompt: "p"})
		require.NoError(t, err)
		dropTable(t, st, "task_work")
		_, err = st.AddChecklistItem(ctx, task.ID, "c")
		require.Error(t, err)
	})
}

// TestWriteArtifactPutArtifactRollbackViaConstraint covers WriteArtifact's
// PutArtifact failure + file-rollback branch (manager.go:280). We keep
// ArtifactBytes working (it SELECTs) but make INSERTs fail by recreating the
// artifacts table with an always-false CHECK constraint — so the file lands and
// is then removed when metadata persistence fails.
func TestWriteArtifactPutArtifactRollbackViaConstraint(t *testing.T) {
	ctx := context.Background()
	mgr, st, root := newManagerWithTmpRoot(t, ArtifactPolicy{})
	task, err := mgr.Create(ctx, CreateReq{Title: "x", Prompt: "p"})
	require.NoError(t, err)

	_, err = st.db.ExecContext(ctx, `DROP TABLE task_work_artifacts`)
	require.NoError(t, err)
	_, err = st.db.ExecContext(ctx, `CREATE TABLE task_work_artifacts(
		id TEXT PRIMARY KEY, task_id TEXT, label TEXT, summary TEXT, content_ref TEXT, size INTEGER, created_at INTEGER, CHECK(0))`)
	require.NoError(t, err)

	_, err = mgr.WriteArtifact(ctx, task.ID, "log", []byte("rollback-me"), root)
	require.Error(t, err)

	// The file must have been rolled back (no .txt in the task dir).
	entries, _ := os.ReadDir(filepath.Join(root, ".yanshi", "artifacts", task.ID))
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".txt") {
			t.Fatalf("artifact file %q should have been rolled back", e.Name())
		}
	}
}

// TestStoreRecordGateErrors covers RecordGate's gate-insert and timeline-insert
// error branches.
func TestStoreRecordGateErrors(t *testing.T) {
	ctx := context.Background()
	ev := Evidence{ID: "ev", Gate: "g", Command: "c", Cwd: "/r", ExitCode: 0, DurationMs: 1, Classification: "pass", Summary: "s", RecordedAt: time.Now().Unix()}
	t.Run("gate insert", func(t *testing.T) {
		st, _ := openStore(t)
		mgr := NewManager(st, nil, ArtifactPolicy{})
		task, err := mgr.Create(ctx, CreateReq{Title: "x", Prompt: "p"})
		require.NoError(t, err)
		dropTable(t, st, "task_work_gates")
		require.Error(t, st.RecordGate(ctx, task.ID, ev))
	})
	t.Run("timeline insert", func(t *testing.T) {
		st, _ := openStore(t)
		mgr := NewManager(st, nil, ArtifactPolicy{})
		task, err := mgr.Create(ctx, CreateReq{Title: "x", Prompt: "p"})
		require.NoError(t, err)
		dropTable(t, st, "task_work_timeline")
		require.Error(t, st.RecordGate(ctx, task.ID, ev))
	})
}
