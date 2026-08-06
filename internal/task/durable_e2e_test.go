package task_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/store"
	"github.com/x6nux/yanshi/internal/task"
	"github.com/x6nux/yanshi/internal/task/work"

	_ "modernc.org/sqlite"
)

// TestDurableTaskRunsEndToEnd joins the two halves that were each tested alone.
//
// internal/task::TestBrokerReportsDurableTaskLifecycle proves the broker calls
// the sink; internal/task/work::TestBrokerMirrorMovesTheDurableRow proves the
// mirror moves the row. Neither runs the other's code, so a mismatch between
// them — a task type the broker does not recognise as durable, a parent id it
// passes in a different field, an adapter that submits with the wrong type
// string — passes both and leaves task_read reporting "pending" for a finished
// task, which is exactly the defect this work package set out to fix.
//
// Everything here is real: one SQLite file, a real Broker, a real work.Manager
// dispatching through BrokerAdapter, and a worker doing what the agent-worker
// CLI does — Claim, then RecordResult.
//
// ledger: A2/DT1#2 状态机正确
func TestDurableTaskRunsEndToEnd(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	st, err := store.Open(filepath.Join(dir, "yanshi.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	db, err := sql.Open("sqlite", filepath.Join(dir, "work.db"))
	require.NoError(t, err)
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	ws, err := work.FromDB(db, nil)
	require.NoError(t, err)

	broker := task.NewBroker(st, 2, 0)
	mgr := work.NewManager(ws, work.BrokerAdapter{Broker: broker}, work.ArtifactPolicy{})
	broker.Work = work.NewLifecycleMirror(mgr)

	// Dispatch=true is the path that puts a durable task on the queue.
	wt, err := mgr.Create(ctx, work.CreateReq{Title: "build", Prompt: "run the build", Dispatch: true})
	require.NoError(t, err)
	require.NotEmpty(t, wt.BrokerTaskID, "the task was not dispatched, so no worker will ever see it")
	require.Equal(t, work.StatusPending, wt.Status)

	// A worker picks it up.
	claimed, err := broker.Claim("worker-1")
	require.NoError(t, err)
	require.NotNil(t, claimed, "the queue had nothing to claim")
	require.Equal(t, wt.BrokerTaskID, claimed.ID)
	require.Equal(t, task.DurableTaskType, claimed.Type,
		"the work layer submits a type the broker does not treat as durable")
	require.Equal(t, wt.ID, claimed.ParentTask,
		"the durable id is not on the broker row, so the mirror has nothing to address")

	running, err := mgr.Read(ctx, wt.ID)
	require.NoError(t, err)
	assert.Equal(t, work.StatusRunning, running.Status,
		"a worker claimed the task and the durable row still reads pending")

	// …and finishes it.
	require.NoError(t, broker.RecordResult(claimed.ID, "worker-1", "completed", "build ok"))

	done, err := mgr.Read(ctx, wt.ID)
	require.NoError(t, err)
	assert.Equal(t, work.StatusCompleted, done.Status,
		"the worker finished and the durable row did not follow; task_read would report "+
			"this task as unfinished forever")

	// The timeline is what a user reads to see what happened, so the
	// transitions have to be on it and not only in the status column.
	var kinds []string
	for _, e := range done.Timeline {
		kinds = append(kinds, e.Kind)
	}
	assert.Contains(t, kinds, "created")
	assert.Contains(t, kinds, "started", "the run is invisible on the timeline")
	assert.Contains(t, kinds, "finished")
}

// TestDurableTaskSurvivesAWorkerThatDies is the same pipeline with the failure
// that has no RecordResult behind it.
//
// A worker that stops heartbeating never reports anything. The sweeper is the
// only thing that moves its task, and a durable row that does not follow the
// sweeper sits at running with nothing holding it — the same stuck state
// RecoverInterrupted cleans up at startup, reached while the process is still
// alive, where no restart is coming to help.
//
// ledger: A2/DT1#2 状态机正确
func TestDurableTaskSurvivesAWorkerThatDies(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	st, err := store.Open(filepath.Join(dir, "yanshi.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	db, err := sql.Open("sqlite", filepath.Join(dir, "work.db"))
	require.NoError(t, err)
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	ws, err := work.FromDB(db, nil)
	require.NoError(t, err)

	// A negative heartbeat timeout puts the staleness cutoff in the future, so
	// the task is stale the moment it is claimed. It has to be at least a
	// second: the cutoff and updated_at are both second-granularity Unix
	// timestamps, so a sub-second offset rounds to the same value and the
	// strict comparison in ListStaleRunning finds nothing.
	broker := task.NewBroker(st, 2, -time.Second)
	mgr := work.NewManager(ws, work.BrokerAdapter{Broker: broker}, work.ArtifactPolicy{})
	broker.Work = work.NewLifecycleMirror(mgr)

	wt, err := mgr.Create(ctx, work.CreateReq{Title: "flaky", Prompt: "p", Dispatch: true})
	require.NoError(t, err)
	_, err = broker.Claim("doomed-worker")
	require.NoError(t, err)

	running, err := mgr.Read(ctx, wt.ID)
	require.NoError(t, err)
	require.Equal(t, work.StatusRunning, running.Status)

	// The worker is gone; the sweeper reclaims it.
	require.NoError(t, broker.RequeueStale(ctx))

	got, err := mgr.Read(ctx, wt.ID)
	require.NoError(t, err)
	assert.Equal(t, work.StatusPending, got.Status,
		"the durable row is still running after its worker was reclaimed as stale: it is "+
			"back in the queue on the broker side and unavailable on the work side")
}
