package work

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	_ "modernc.org/sqlite"
)

// fileStore opens a work Store over a real file, so a second Open can observe
// what the first one wrote.
//
// Every other test in this package opens ":memory:" — all ten of them — which
// makes "persists across a restart" unprovable by construction: an in-memory
// database cannot outlive its connection, so the only round trip those tests
// can perform is within one instance.
func fileStore(t *testing.T, path string) *Store {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	st, err := FromDB(db, nil)
	require.NoError(t, err)
	return st
}

// TestStateMachineRejectsEveryEdgeItShould walks the whole matrix.
//
// The predecessor accepted any known status from any non-terminal one, so
// pending→completed was legal: a task could report success without ever having
// run. Only two of the twenty-five pairs were ever asserted, both of them
// terminal→something, which is the half that was already right.
//
// Walking KnownStatuses rather than a second hand-written list is what keeps
// this honest as the enum grows.
//
// ledger: A2/DT1#2 状态机正确
func TestStateMachineRejectsEveryEdgeItShould(t *testing.T) {
	legal := map[Status]map[Status]bool{
		StatusPending: {StatusRunning: true, StatusCancelled: true},
		StatusRunning: {
			StatusCompleted: true, StatusFailed: true,
			StatusCancelled: true, StatusPending: true,
		},
	}
	for _, from := range KnownStatuses() {
		for _, to := range KnownStatuses() {
			err := from.CanTransitionTo(to)
			if legal[from][to] {
				assert.NoErrorf(t, err, "%s -> %s must be legal", from, to)
				continue
			}
			assert.Errorf(t, err, "%s -> %s must be rejected", from, to)
			assert.ErrorIsf(t, err, ErrIllegalTransition,
				"%s -> %s was rejected but not as an illegal transition, so callers "+
					"cannot tell the state machine's 'no' from a database error", from, to)
		}
	}

	// The two edges worth naming, because they are the ones the loose version
	// admitted and the ones a reader is most likely to assume.
	require.Error(t, StatusPending.CanTransitionTo(StatusCompleted),
		"a task that was never dispatched must not be able to report success")
	require.NoError(t, StatusRunning.CanTransitionTo(StatusPending),
		"an interrupted task must be able to return to the queue")

	require.ErrorIs(t, StatusPending.CanTransitionTo(Status("nope")), ErrIllegalTransition)
}

// TestBrokerMirrorMovesTheDurableRow is the runtime half of the state machine.
//
// Manager.Start and Manager.Finish were correct and tested, and had zero
// production callers: the broker moved its own row through the lifecycle while
// the durable task_work row stayed at pending from creation onwards, so
// task_read reported "pending" for tasks that had long finished.
// running/completed/failed were unreachable at runtime.
//
// LifecycleMirror is the call site the broker uses; driving it directly is what
// the broker does, without needing a live queue.
//
// ledger: A2/DT1#2 状态机正确
func TestBrokerMirrorMovesTheDurableRow(t *testing.T) {
	ctx := context.Background()
	newTask := func(t *testing.T) (*Manager, string) {
		t.Helper()
		m := NewManager(fileStore(t, filepath.Join(t.TempDir(), "w.db")), nil, ArtifactPolicy{})
		task, err := m.Create(ctx, CreateReq{Title: "x", Prompt: "p"})
		require.NoError(t, err)
		require.Equal(t, StatusPending, task.Status)
		return m, task.ID
	}

	t.Run("completed", func(t *testing.T) {
		m, id := newTask(t)
		mir := NewLifecycleMirror(m)

		mir.OnRunning(id)
		got, err := m.Read(ctx, id)
		require.NoError(t, err)
		require.Equal(t, StatusRunning, got.Status,
			"the broker claimed the task but the durable row is still pending")

		mir.OnTerminal(id, "completed", "all good")
		got, err = m.Read(ctx, id)
		require.NoError(t, err)
		assert.Equal(t, StatusCompleted, got.Status)
	})

	t.Run("timeout counts as failed", func(t *testing.T) {
		m, id := newTask(t)
		mir := NewLifecycleMirror(m)
		mir.OnRunning(id)
		mir.OnTerminal(id, "timeout", "")
		got, err := m.Read(ctx, id)
		require.NoError(t, err)
		assert.Equal(t, StatusFailed, got.Status,
			"a task that ran out of time did not complete")
	})

	t.Run("requeue returns it to the queue", func(t *testing.T) {
		m, id := newTask(t)
		mir := NewLifecycleMirror(m)
		mir.OnRunning(id)
		mir.OnRequeued(id)
		got, err := m.Read(ctx, id)
		require.NoError(t, err)
		assert.Equal(t, StatusPending, got.Status,
			"a requeued attempt left the durable row at running, where nothing will pick it up")
	})

	t.Run("a rejected transition is not an error", func(t *testing.T) {
		m, id := newTask(t)
		mir := NewLifecycleMirror(m)
		var reported int
		mir.OnError = func(string, error) { reported++ }

		// Cancelled from the work side while the broker is still finishing:
		// the incoming terminal transition must be dropped quietly.
		_, err := m.Cancel(ctx, id, "user")
		require.NoError(t, err)
		mir.OnTerminal(id, "completed", "raced")

		got, err := m.Read(ctx, id)
		require.NoError(t, err)
		assert.Equal(t, StatusCancelled, got.Status, "the mirror overwrote a user cancellation")
		assert.Zero(t, reported,
			"losing a race with a cancel was reported as a fault; it is the expected outcome")
	})
}

// TestRecoverInterruptedReturnsRunningTasksToTheQueue is the restart clause.
//
// TestManagerCreatePersistsAndRecovers, which the name suggests covers this,
// re-Gets from the SAME Store instance over ":memory:" — its own comment says
// it "simulates" a restart. Nothing crossed a process boundary, and there was
// no recovery logic to cross it: registry.NewManager rewrites Running to
// Interrupted on a new boot, and the work store had no counterpart, so a task
// left running by a killed process stayed running forever — invisible to
// ListPending and never re-claimed.
//
// Here the first Manager is closed and a second one opens the same file.
//
// ledger: A2/DT1#4 重启后持久恢复
func TestRecoverInterruptedReturnsRunningTasksToTheQueue(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "w.db")

	first := NewManager(fileStore(t, path), nil, ArtifactPolicy{})
	running, err := first.Create(ctx, CreateReq{Title: "interrupted", Prompt: "p"})
	require.NoError(t, err)
	require.NoError(t, first.Start(ctx, running.ID))

	pending, err := first.Create(ctx, CreateReq{Title: "queued", Prompt: "p"})
	require.NoError(t, err)

	finished, err := first.Create(ctx, CreateReq{Title: "finished", Prompt: "p"})
	require.NoError(t, err)
	require.NoError(t, first.Start(ctx, finished.ID))
	require.NoError(t, first.Finish(ctx, finished.ID, StatusCompleted, "ok"))

	// The process dies here. Everything below sees only the file.
	second := NewManager(fileStore(t, path), nil, ArtifactPolicy{})

	before, err := second.Read(ctx, running.ID)
	require.NoError(t, err, "the task did not survive to a second Manager")
	require.Equal(t, StatusRunning, before.Status,
		"the row is not running, so this test would prove nothing about recovery")
	require.Equal(t, "interrupted", before.Title, "fields did not survive the restart")

	n, err := second.RecoverInterrupted(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, n, "recovered %d tasks, expected only the running one", n)

	got, err := second.Read(ctx, running.ID)
	require.NoError(t, err)
	assert.Equal(t, StatusPending, got.Status,
		"a task left running by a dead process is still marked running: no worker holds it "+
			"and nothing will ever pick it up again")

	// The interruption has to be visible, otherwise the task looks like it was
	// never started.
	var kinds []string
	for _, e := range got.Timeline {
		kinds = append(kinds, e.Kind)
	}
	assert.Contains(t, kinds, "recovered", "the timeline does not record the interruption")

	// The other two are untouched: recovery must not disturb work that was not
	// interrupted.
	stillPending, err := second.Read(ctx, pending.ID)
	require.NoError(t, err)
	assert.Equal(t, StatusPending, stillPending.Status)
	stillDone, err := second.Read(ctx, finished.ID)
	require.NoError(t, err)
	assert.Equal(t, StatusCompleted, stillDone.Status,
		"recovery moved a task that had already completed")

	// Idempotent: running it twice must not resurrect anything.
	n, err = second.RecoverInterrupted(ctx)
	require.NoError(t, err)
	assert.Zero(t, n, "a second recovery pass moved %d tasks; nothing was running", n)
}
