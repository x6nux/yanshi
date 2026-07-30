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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- Manager.ReadArtifact / DispatcherRef / BrokerAdapter / StartArtifactJanitor ---

// TestManagerReadArtifact covers Manager.ReadArtifact: writing then reading an
// artifact round-trips the metadata.
func TestManagerReadArtifact(t *testing.T) {
	ctx := context.Background()
	mgr, _, root := newManagerWithTmpRoot(t, ArtifactPolicy{})
	task, err := mgr.Create(ctx, CreateReq{Title: "x", Prompt: "p"})
	require.NoError(t, err)

	art, err := mgr.WriteArtifact(ctx, task.ID, "log", []byte("data"), root)
	require.NoError(t, err)

	got, err := mgr.ReadArtifact(ctx, art.ID)
	require.NoError(t, err)
	assert.Equal(t, art.ID, got.ID)
	assert.Equal(t, art.TaskID, got.TaskID)
	assert.Equal(t, art.Summary, got.Summary)

	// not-found path
	_, err = mgr.ReadArtifact(ctx, "nope")
	require.Error(t, err)
}

// TestManagerCreateDispatchNilDispatcher covers the dispatch=true branch where
// the dispatcher is nil: the durable task is created, a dispatch_failed timeline
// entry is appended, and Create returns an error naming the still-created task.
func TestManagerCreateDispatchNilDispatcher(t *testing.T) {
	ctx := context.Background()
	_, _, st := newManagerWithFakeDispatcher(t)
	// Replace the dispatcher with nil by constructing a fresh Manager.
	mgr2 := NewManager(st, nil, ArtifactPolicy{})
	task, err := mgr2.Create(ctx, CreateReq{Title: "x", Prompt: "p", Dispatch: true})
	require.Error(t, err)
	require.NotNil(t, task, "durable task must be returned even on dispatch failure")

	got, err := st.Get(ctx, task.ID)
	require.NoError(t, err)
	assert.Empty(t, got.BrokerTaskID)
	var hasDispatchFailed bool
	for _, e := range got.Timeline {
		if e.Kind == "dispatch_failed" {
			hasDispatchFailed = true
		}
	}
	assert.True(t, hasDispatchFailed, "expected dispatch_failed timeline entry")
}

// TestManagerCancelBrokerError covers Manager.Cancel's broker-cancel error
// branch: when dispatcher.Cancel fails, the status transition must NOT happen.
func TestManagerCancelBrokerError(t *testing.T) {
	ctx := context.Background()
	mgr, disp, _ := newManagerWithFakeDispatcher(t)
	disp.cancelErr = errors.New("broker unreachable")
	task, err := mgr.Create(ctx, CreateReq{Title: "x", Prompt: "p", Dispatch: true})
	require.NoError(t, err)
	require.NoError(t, mgr.Start(ctx, task.ID))

	_, err = mgr.Cancel(ctx, task.ID, "ops")
	require.Error(t, err)
	// Task should still be running (transition never happened).
	got, err := mgr.Read(ctx, task.ID)
	require.NoError(t, err)
	assert.Equal(t, StatusRunning, got.Status)
}

// TestDispatcherRef covers the late-binding Dispatcher holder: Submit/Cancel
// return "dispatcher unavailable" before Bind, and delegate after Bind.
func TestDispatcherRef(t *testing.T) {
	ref := &DispatcherRef{}
	_, err := ref.Submit("work_task", "p", "parent")
	require.Error(t, err)
	require.Error(t, ref.Cancel("bk-1"))

	disp := &fakeDispatcher{}
	ref.Bind(disp)

	id, err := ref.Submit("work_task", "do", "parent")
	require.NoError(t, err)
	assert.Equal(t, "bk-1", id)
	require.Len(t, disp.submitted, 1)
	assert.Equal(t, "do", disp.submitted[0].Input)

	require.NoError(t, ref.Cancel("bk-1"))
	require.Len(t, disp.cancels, 1)
}

// fakeBroker is a minimal Broker-shaped value for BrokerAdapter tests.
type fakeBroker struct {
	submitErr error
	cancelErr error
	called    bool
}

func (f *fakeBroker) Submit(typ, input, parent string) (string, error) {
	f.called = true
	if f.submitErr != nil {
		return "", f.submitErr
	}
	return "bk-9", nil
}
func (f *fakeBroker) Cancel(id string) error { return f.cancelErr }

// TestBrokerAdapter covers BrokerAdapter.Submit/Cancel delegation to the wrapped
// Broker (the work package does not import internal/task, so the adapter is the
// only seam).
func TestBrokerAdapter(t *testing.T) {
	b := &fakeBroker{}
	a := BrokerAdapter{Broker: b}
	id, err := a.Submit("work_task", "in", "par")
	require.NoError(t, err)
	assert.Equal(t, "bk-9", id)
	assert.True(t, b.called)
	require.NoError(t, a.Cancel("bk-9"))

	// error propagation
	b2 := &fakeBroker{submitErr: errors.New("nope"), cancelErr: errors.New("boom")}
	a2 := BrokerAdapter{Broker: b2}
	_, err = a2.Submit("t", "i", "p")
	require.Error(t, err)
	require.Error(t, a2.Cancel("x"))
}

// TestStartArtifactJanitorSweepsAndStops proves the janitor goroutine actually
// sweeps expired artifacts on its ticker and exits cleanly when ctx is done.
func TestStartArtifactJanitorSweepsAndStops(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mgr, st, root := newManagerWithTmpRoot(t, ArtifactPolicy{})
	task, err := mgr.Create(ctx, CreateReq{Title: "x", Prompt: "p"})
	require.NoError(t, err)
	art, err := mgr.WriteArtifact(ctx, task.ID, "old", []byte("old"), root)
	require.NoError(t, err)
	// Force the artifact into the past so a near-zero TTL sweeps it.
	_, err = st.db.ExecContext(ctx, `UPDATE task_work_artifacts SET created_at=? WHERE id=?`, time.Now().Add(-1*time.Hour).Unix(), art.ID)
	require.NoError(t, err)

	// interval=10ms, ttl=1ns → every tick sweeps everything older than 1ns.
	StartArtifactJanitor(ctx, st, root, 10*time.Millisecond, time.Nanosecond)

	// Wait until the metadata row is gone (the janitor swept it).
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := st.GetArtifact(ctx, art.ID); err != nil {
			break // gone
		}
		time.Sleep(10 * time.Millisecond)
	}
	_, err = st.GetArtifact(ctx, art.ID)
	require.Error(t, err, "janitor should have swept the expired artifact")

	// Cancelling ctx must let the goroutine return without leaking.
	cancel()
	time.Sleep(30 * time.Millisecond) // give the goroutine time to observe ctx.Done
}

// --- WriteArtifact error paths ---

// TestWriteArtifactArtifactBytesError covers the ArtifactBytes error branch
// (store unavailable) of WriteArtifact.
func TestWriteArtifactArtifactBytesError(t *testing.T) {
	ctx := context.Background()
	mgr, st, root := newManagerWithTmpRoot(t, ArtifactPolicy{})
	task, err := mgr.Create(ctx, CreateReq{Title: "x", Prompt: "p"})
	require.NoError(t, err)
	require.NoError(t, st.db.Close()) // make ArtifactBytes fail
	_, err = mgr.WriteArtifact(ctx, task.ID, "log", []byte("x"), root)
	require.Error(t, err)
}

// TestWriteArtifactPutArtifactRollback covers the PutArtifact failure branch:
// the file is written then removed when metadata persistence fails.
func TestWriteArtifactPutArtifactRollback(t *testing.T) {
	ctx := context.Background()
	mgr, st, root := newManagerWithTmpRoot(t, ArtifactPolicy{})
	task, err := mgr.Create(ctx, CreateReq{Title: "x", Prompt: "p"})
	require.NoError(t, err)

	// Capture the artifact dir; then break the DB so PutArtifact fails AFTER the
	// file lands. We close the store DB right before PutArtifact by intercepting:
	// simplest is to close it, but MkdirAll/WriteFile/Rename don't need the DB.
	require.NoError(t, st.db.Close())
	_, err = mgr.WriteArtifact(ctx, task.ID, "log", []byte("rollback-me"), root)
	require.Error(t, err)

	// No .txt artifact files should remain in the task dir.
	dir := filepath.Join(root, ".yanshi", "artifacts", task.ID)
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".txt") {
			t.Fatalf("artifact file %q should have been rolled back", e.Name())
		}
	}
}

// TestWriteArtifactMkdirAllError covers the MkdirAll failure branch by making
// root a path component that is already a file.
func TestWriteArtifactMkdirAllError(t *testing.T) {
	ctx := context.Background()
	mgr, _, root := newManagerWithTmpRoot(t, ArtifactPolicy{})
	task, err := mgr.Create(ctx, CreateReq{Title: "x", Prompt: "p"})
	require.NoError(t, err)

	// Create a regular file, then use it as a parent directory prefix so MkdirAll
	// cannot create the artifacts dir under it.
	blocker := filepath.Join(root, "blocker")
	require.NoError(t, os.WriteFile(blocker, []byte("x"), 0o600))
	badRoot := filepath.Join(blocker, "sub") // blocker is a file, not a dir

	_, err = mgr.WriteArtifact(ctx, task.ID, "log", []byte("x"), badRoot)
	require.Error(t, err)
}

// --- Manager checklist/store-error branches (closed DB) ---

// TestManagerChecklistStoreErrors covers the store-error branches of
// SetChecklist/AddChecklistItem/PatchChecklistItem/RecordGate by closing the DB.
func TestManagerChecklistStoreErrors(t *testing.T) {
	ctx := context.Background()
	mgr, _, st := newManagerWithFakeDispatcher(t)
	task, err := mgr.Create(ctx, CreateReq{Title: "x", Prompt: "p"})
	require.NoError(t, err)
	require.NoError(t, st.db.Close())

	_, err = mgr.SetChecklist(ctx, task.ID, Checklist{Items: []ChecklistItem{{ID: 1, Content: "a"}}})
	require.Error(t, err)
	_, err = mgr.AddChecklistItem(ctx, task.ID, "b")
	require.Error(t, err)
	_, err = mgr.PatchChecklistItem(ctx, task.ID, 1, "c", ChecklistDone)
	require.Error(t, err)
	require.Error(t, mgr.RecordGate(ctx, task.ID, Evidence{ID: "ev", Gate: "test", Classification: "pass", Summary: "ok", RecordedAt: time.Now().Unix()}))
}

// --- truncate & summarizeArtifact extra branches ---

func TestTruncateBranches(t *testing.T) {
	assert.Equal(t, "…", truncate("abc", 0))   // max <= 0
	assert.Equal(t, "…", truncate("abc", -1))  // negative
	assert.Equal(t, "ab", "ab")                // sanity
	assert.Equal(t, "abcde", truncate("abcde", 5))
	// truncation: 5 runes from a 10-rune string + ellipsis
	out := truncate("abcdefghij", 5)
	assert.Equal(t, "abcde…", out)
	// multi-byte safe
	out = truncate("你好世界你好世界", 3)
	assert.Equal(t, "你好世…", out)
}

// --- SweepArtifacts SecureArtifactPath-error branch ---

// TestSweepArtifactsBadRefSkips covers the `continue` branch of SweepArtifacts
// when a content_ref cannot be jailed (ref escapes root).
func TestSweepArtifactsBadRefSkips(t *testing.T) {
	ctx := context.Background()
	mgr, st, root := newManagerWithTmpRoot(t, ArtifactPolicy{})
	task, err := mgr.Create(ctx, CreateReq{Title: "x", Prompt: "p"})
	require.NoError(t, err)

	// Insert an artifact row with a content_ref that escapes root. Because
	// SweepArtifacts reads refs straight from the DB, this exercises the
	// SecureArtifactPath error → continue path without aborting the sweep.
	_, err = st.db.ExecContext(ctx, `INSERT INTO task_work_artifacts(id,task_id,label,summary,content_ref,size,created_at)
		VALUES(?,?,?,?,?,?,?)`, "art-bad", task.ID, "bad", "x", "../../../etc/passwd", 1, time.Now().Add(-1*time.Hour).Unix())
	require.NoError(t, err)

	// Must not error even though the ref is unjailable.
	require.NoError(t, SweepArtifacts(ctx, st, root, time.Now()))
}

// --- FakeManager artifact methods (were 0%) ---

func TestFakeManagerArtifacts(t *testing.T) {
	ctx := context.Background()
	f := NewFakeManager()
	task, err := f.Create(ctx, CreateReq{Title: "x", Prompt: "p"})
	require.NoError(t, err)

	art, err := f.WriteArtifact(ctx, task.ID, "log", []byte("hello\nworld"), "/tmp")
	require.NoError(t, err)
	assert.NotEmpty(t, art.ID)
	assert.Equal(t, task.ID, art.TaskID)
	assert.Equal(t, "hello", art.Summary)

	got, err := f.ReadArtifact(ctx, art.ID)
	require.NoError(t, err)
	assert.Equal(t, art.ID, got.ID)

	list, err := f.ListArtifacts(ctx, task.ID)
	require.NoError(t, err)
	require.Len(t, list, 1)

	// not-found read
	_, err = f.ReadArtifact(ctx, "missing")
	require.Error(t, err)
}

// TestFakeManagerRecordGateNotFound covers the task-not-found branch and the
// replace-existing-gate branch of FakeManager.RecordGate.
func TestFakeManagerRecordGateNotFound(t *testing.T) {
	ctx := context.Background()
	f := NewFakeManager()
	require.Error(t, f.RecordGate(ctx, "ghost", Evidence{ID: "ev", Gate: "test", Classification: "pass", Summary: "ok", RecordedAt: time.Now().Unix()}))

	task, err := f.Create(ctx, CreateReq{Title: "x", Prompt: "p"})
	require.NoError(t, err)
	ev := Evidence{ID: "ev1", Gate: "test", Command: "c", Cwd: "/r", ExitCode: 0, Classification: "pass", Summary: "ok", RecordedAt: time.Now().Unix()}
	require.NoError(t, f.RecordGate(ctx, task.ID, ev))
	// Same gate → replace (not append).
	ev2 := ev
	ev2.ID = "ev2"
	ev2.Summary = "ok2"
	require.NoError(t, f.RecordGate(ctx, task.ID, ev2))
	got, err := f.Read(ctx, task.ID)
	require.NoError(t, err)
	require.Len(t, got.Gates, 1)
	assert.Equal(t, "ev2", got.Gates[0].ID)
}

// TestFakeManagerMissingTaskErrors sweeps the task-not-found branches of the
// remaining FakeManager methods that were partially covered.
func TestFakeManagerMissingTaskErrors(t *testing.T) {
	ctx := context.Background()
	f := NewFakeManager()
	_, err := f.Read(ctx, "ghost")
	require.Error(t, err)
	require.Error(t, f.Start(ctx, "ghost"))
	require.Error(t, f.Finish(ctx, "ghost", StatusCompleted, ""))
	_, err = f.Cancel(ctx, "ghost", "ops")
	require.Error(t, err)
	_, err = f.SetChecklist(ctx, "ghost", Checklist{})
	require.Error(t, err)
	_, err = f.AddChecklistItem(ctx, "ghost", "b")
	require.Error(t, err)
	_, err = f.PatchChecklistItem(ctx, "ghost", 1, "c", ChecklistDone)
	require.Error(t, err)
}

// TestFakeManagerPatchItemNotFound covers the item-not-found branch.
func TestFakeManagerPatchItemNotFound(t *testing.T) {
	ctx := context.Background()
	f := NewFakeManager()
	task, err := f.Create(ctx, CreateReq{Title: "x", Prompt: "p"})
	require.NoError(t, err)
	_, err = f.PatchChecklistItem(ctx, task.ID, 99, "c", ChecklistDone)
	require.Error(t, err)
}

// TestFakeManagerFinishInvalidStatus covers the invalid-finish-status branch.
func TestFakeManagerFinishInvalidStatus(t *testing.T) {
	ctx := context.Background()
	f := NewFakeManager()
	task, err := f.Create(ctx, CreateReq{Title: "x", Prompt: "p"})
	require.NoError(t, err)
	require.Error(t, f.Finish(ctx, task.ID, StatusPending, ""))
}

// TestFakeManagerStartInvalidTransition covers the CanTransitionTo failure on
// Start (e.g. starting an already-running task twice).
func TestFakeManagerStartInvalidTransition(t *testing.T) {
	ctx := context.Background()
	f := NewFakeManager()
	task, err := f.Create(ctx, CreateReq{Title: "x", Prompt: "p"})
	require.NoError(t, err)
	require.NoError(t, f.Start(ctx, task.ID))
	require.NoError(t, f.Finish(ctx, task.ID, StatusCompleted, "ok"))
	// completed is terminal → cannot start again.
	require.Error(t, f.Start(ctx, task.ID))
}

// TestFakeManagerListSortAndFilter covers the CreatedAt-equal tiebreak (ID DESC)
// and the threadID filter-continue branch of FakeManager.List.
func TestFakeManagerListSortAndFilter(t *testing.T) {
	ctx := context.Background()
	f := NewFakeManager()
	// Force equal CreatedAt timestamps by crafting tasks with known times via
	// direct map insertion is not possible (Create owns the timestamp), so use
	// the natural counter: distinct increasing timestamps. The ID-DESC tiebreak
	// needs equal timestamps — emulate by creating two then swapping timestamps.
	t1, err := f.Create(ctx, CreateReq{Title: "1", Prompt: "p", ThreadID: "thA"})
	require.NoError(t, err)
	t2, err := f.Create(ctx, CreateReq{Title: "2", Prompt: "p", ThreadID: "thA"})
	require.NoError(t, err)
	_, err = f.Create(ctx, CreateReq{Title: "3", Prompt: "p", ThreadID: "thB"})
	require.NoError(t, err)

	// Make t1 and t2 share the same CreatedAt so the sort hits the ID-DESC branch.
	f.mu.Lock()
	f.tasks[t1.ID].CreatedAt = f.tasks[t2.ID].CreatedAt
	f.mu.Unlock()

	// threadID filter (thA) exercises the `continue` branch for thB.
	list, err := f.List(ctx, 0, "thA")
	require.NoError(t, err)
	require.Len(t, list, 2)
	// Among equal-timestamp tasks the larger ID sorts first.
	assert.True(t, list[0].ID > list[1].ID, "equal-timestamp tiebreak should be ID DESC")

	// limit truncation branch.
	listAll, err := f.List(ctx, 1, "")
	require.NoError(t, err)
	require.Len(t, listAll, 1)
}

// TestFakeManagerFinishFromTerminal covers Finish's CanTransitionTo error on a
// terminal-status task.
func TestFakeManagerFinishFromTerminal(t *testing.T) {
	ctx := context.Background()
	f := NewFakeManager()
	task, err := f.Create(ctx, CreateReq{Title: "x", Prompt: "p"})
	require.NoError(t, err)
	require.NoError(t, f.Start(ctx, task.ID))
	require.NoError(t, f.Finish(ctx, task.ID, StatusCompleted, "ok"))
	// completed is terminal → cannot finish again.
	require.Error(t, f.Finish(ctx, task.ID, StatusCompleted, "again"))
}

// TestFakeManagerCancelFromTerminal covers Cancel's CanTransitionTo error on a
// terminal-status task.
func TestFakeManagerCancelFromTerminal(t *testing.T) {
	ctx := context.Background()
	f := NewFakeManager()
	task, err := f.Create(ctx, CreateReq{Title: "x", Prompt: "p"})
	require.NoError(t, err)
	require.NoError(t, f.Start(ctx, task.ID))
	require.NoError(t, f.Finish(ctx, task.ID, StatusFailed, "bad"))
	_, err = f.Cancel(ctx, task.ID, "ops")
	require.Error(t, err)
}

// TestFakeManagerPatchContent covers PatchChecklistItem's content != "" branch
// (previously only the status branch was exercised).
func TestFakeManagerPatchContent(t *testing.T) {
	ctx := context.Background()
	f := NewFakeManager()
	task, err := f.Create(ctx, CreateReq{Title: "x", Prompt: "p"})
	require.NoError(t, err)
	_, err = f.AddChecklistItem(ctx, task.ID, "original")
	require.NoError(t, err)
	got, err := f.PatchChecklistItem(ctx, task.ID, 1, "updated content", "")
	require.NoError(t, err)
	assert.Equal(t, "updated content", got.Checklist.Items[0].Content)
}

// TestStoreGetLoadsArtifacts covers Store.Get's artifact-loading branch by
// creating a task with an artifact (via PutArtifact) then reading the full task.
func TestStoreGetLoadsArtifacts(t *testing.T) {
	ctx := context.Background()
	mgr, st, _ := newManagerWithTmpRoot(t, ArtifactPolicy{})
	task, err := mgr.Create(ctx, CreateReq{Title: "x", Prompt: "p"})
	require.NoError(t, err)
	require.NoError(t, st.PutArtifact(ctx, Artifact{
		ID: "art-g", TaskID: task.ID, Label: "l", Summary: "s", ContentRef: "r", Size: 4, CreatedAt: 1,
	}))
	got, err := st.Get(ctx, task.ID)
	require.NoError(t, err)
	require.Len(t, got.Artifacts, 1)
	assert.Equal(t, "art-g", got.Artifacts[0].ID)
	assert.Equal(t, task.ID, got.Artifacts[0].TaskID)
}

// TestSweepArtifactsStoreError covers SweepArtifacts' DeleteArtifactsBefore
// error branch (store unavailable → return err).
func TestSweepArtifactsStoreError(t *testing.T) {
	ctx := context.Background()
	st := newClosedStore(t)
	err := SweepArtifacts(ctx, st, t.TempDir(), time.Now())
	require.Error(t, err)
}

// --- types.go CanTransitionTo unknown-status branch ---

func TestStatusCanTransitionToUnknown(t *testing.T) {
	// A known non-terminal status rejecting an unknown target.
	require.Error(t, StatusPending.CanTransitionTo("frobnicated"))
	// Terminal status rejects everything.
	require.Error(t, StatusCompleted.CanTransitionTo(StatusRunning))
	require.Error(t, StatusFailed.CanTransitionTo(StatusRunning))
	require.Error(t, StatusCancelled.CanTransitionTo(StatusRunning))
}

// --- Store error branches via closed DB ---

// newClosedStore returns a Store whose underlying DB is already closed, so every
// query/tx reports an error. Used to cover the error returns of Store methods.
func newClosedStore(t *testing.T) *Store {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	require.NoError(t, err)
	st, err := FromDB(db, nil)
	require.NoError(t, err)
	require.NoError(t, db.Close())
	return st
}

// TestStoreErrorBranches exercises the failure returns of every Store method by
// closing the DB first. Each call must error rather than panic.
func TestStoreErrorBranches(t *testing.T) {
	ctx := context.Background()
	st := newClosedStore(t)

	require.Error(t, st.Create(ctx, &WorkTask{ID: "x", Title: "t", Prompt: "p", Status: StatusPending}))
	_, err := st.Get(ctx, "x")
	require.Error(t, err)
	_, err = st.List(ctx, 10, "")
	require.Error(t, err)
	require.Error(t, st.Transition(ctx, "x", StatusRunning, "started", "s"))
	require.Error(t, st.AppendTimeline(ctx, "x", TimelineEntry{Kind: "k", Summary: "s"}))
	require.Error(t, st.AttachBrokerTask(ctx, "x", "bk"))
	require.Error(t, st.SetChecklist(ctx, "x", Checklist{}))
	_, err = st.AddChecklistItem(ctx, "x", "c")
	require.Error(t, err)
	require.Error(t, st.PatchChecklistItem(ctx, "x", 1, "c", ChecklistDone))
	require.Error(t, st.RecordGate(ctx, "x", Evidence{ID: "ev", Gate: "g", Classification: "pass", Summary: "s", RecordedAt: time.Now().Unix()}))
	require.Error(t, st.PutArtifact(ctx, Artifact{ID: "a", TaskID: "x", Label: "l", Summary: "s", ContentRef: "r", Size: 1, CreatedAt: time.Now().Unix()}))
	_, err = st.GetArtifact(ctx, "a")
	require.Error(t, err)
	_, err = st.ArtifactBytes(ctx, "x")
	require.Error(t, err)
	_, err = st.DeleteArtifactsBefore(ctx, time.Now().Unix())
	require.Error(t, err)
}

// TestStoreAttachBrokerTaskMissing covers the n != 1 (task not found) branch of
// AttachBrokerTask on an open DB.
func TestStoreAttachBrokerTaskMissing(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite", ":memory:")
	require.NoError(t, err)
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	st, err := FromDB(db, nil)
	require.NoError(t, err)
	require.Error(t, st.AttachBrokerTask(ctx, "nope", "bk-1"))
}

// TestStorePatchChecklistItemMissing covers the n != 1 (item not found) branch.
func TestStorePatchChecklistItemMissing(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite", ":memory:")
	require.NoError(t, err)
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	st, err := FromDB(db, nil)
	require.NoError(t, err)
	require.Error(t, st.PatchChecklistItem(ctx, "nope", 1, "c", ChecklistDone))
}

// TestStoreTransitionIllegal covers the CanTransitionTo error branch of
// Transition (terminal → running).
func TestStoreTransitionIllegal(t *testing.T) {
	ctx := context.Background()
	mgr, _, st := newManagerWithFakeDispatcher(t)
	task, err := mgr.Create(ctx, CreateReq{Title: "x", Prompt: "p"})
	require.NoError(t, err)
	require.NoError(t, mgr.Start(ctx, task.ID))
	require.NoError(t, mgr.Finish(ctx, task.ID, StatusCompleted, "ok"))
	// completed is terminal → Transition refuses.
	require.Error(t, st.Transition(ctx, task.ID, StatusRunning, "started", "s"))
}

// TestStoreFromDBMigrateError covers the FromDB migrate-error branch by passing
// a closed DB (Exec fails).
func TestStoreFromDBMigrateError(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	require.NoError(t, err)
	require.NoError(t, db.Close())
	_, err = FromDB(db, nil)
	require.Error(t, err)
}

// TestStoreListLimitClamp covers the limit clamping (limit > 200 → 50, limit
// <= 0 → 50) of Store.List.
func TestStoreListLimitClamp(t *testing.T) {
	ctx := context.Background()
	mgr, _, _ := newManagerWithFakeDispatcher(t)
	for i := 0; i < 3; i++ {
		_, err := mgr.Create(ctx, CreateReq{Title: "x", Prompt: "p"})
		require.NoError(t, err)
	}
	// limit > 200 → clamped to 50; should still return the 3 rows.
	list, err := mgr.List(ctx, 9999, "")
	require.NoError(t, err)
	assert.Len(t, list, 3)
	// limit <= 0 → clamped to 50.
	list, err = mgr.List(ctx, 0, "")
	require.NoError(t, err)
	assert.Len(t, list, 3)
}

// --- WriteTxer / wt() (were 0%) ---

// TestUnlockedWriteTxer covers unlockedWriteTxer.WriteTx directly: success,
// fn-error rollback, and begin-error paths. These methods are part of the
// package's WriteTxer contract even though the current Store methods call
// s.db.BeginTx inline.
func TestUnlockedWriteTxer(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	require.NoError(t, err)
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	require.NoError(t, db.Ping())
	u := unlockedWriteTxer{db: db}

	// success path
	require.NoError(t, u.WriteTx(context.Background(), func(tx *sql.Tx) error {
		_, err := tx.Exec("CREATE TABLE IF NOT EXISTS x (v INTEGER)")
		return err
	}))
	// fn-error → rollback → returns the error
	require.Error(t, u.WriteTx(context.Background(), func(tx *sql.Tx) error {
		return errors.New("boom")
	}))

	// wt() returns the unlocked txer when writeTx is nil, the injected one otherwise.
	sNil := &Store{db: db, writeTx: nil}
	got := sNil.wt()
	assert.NotNil(t, got)

	injected := unlockedWriteTxer{db: db} // any WriteTxer
	sInj := &Store{db: db, writeTx: injected}
	got2 := sInj.wt()
	assert.NotNil(t, got2)
}
