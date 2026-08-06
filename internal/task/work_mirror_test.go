package task

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recordingWork captures what the broker reports, in order.
type recordingWork struct {
	mu     sync.Mutex
	events []string
}

func (r *recordingWork) add(s string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, s)
}

func (r *recordingWork) OnRunning(id string)             { r.add("running:" + id) }
func (r *recordingWork) OnTerminal(id, status, _ string) { r.add(status + ":" + id) }
func (r *recordingWork) OnRequeued(id string)            { r.add("requeued:" + id) }
func (r *recordingWork) seen() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.events...)
}

// TestBrokerReportsDurableTaskLifecycle is the broker end of the mirror.
//
// work.Manager.Start and work.Manager.Finish had zero production call sites:
// nothing connected the broker's transitions to the durable row, so a task_work
// row stayed pending while its broker row ran to completion. The work-side test
// drives LifecycleMirror directly; this one pins that the broker actually calls
// it, which is the hop between them.
//
// ledger: A2/DT1#2 状态机正确
func TestBrokerReportsDurableTaskLifecycle(t *testing.T) {
	t.Run("claim then finish", func(t *testing.T) {
		b, _ := newTestBroker(t, 2, 5*time.Second)
		rec := &recordingWork{}
		b.Work = rec

		_, err := b.Submit(DurableTaskType, "prompt", "wt-1")
		require.NoError(t, err)

		task, err := b.Claim("w1")
		require.NoError(t, err)
		require.NotNil(t, task)
		require.NoError(t, b.RecordResult(task.ID, "w1", "completed", "done"))

		assert.Equal(t, []string{"running:wt-1", "completed:wt-1"}, rec.seen(),
			"the durable row was never told the task started or finished")
	})

	t.Run("a failed attempt within budget is a requeue, not a failure", func(t *testing.T) {
		b, _ := newTestBroker(t, 2, 5*time.Second)
		rec := &recordingWork{}
		b.Work = rec

		_, err := b.Submit(DurableTaskType, "prompt", "wt-2")
		require.NoError(t, err)
		task, err := b.Claim("w1")
		require.NoError(t, err)
		require.NoError(t, b.RecordResult(task.ID, "w1", "failed", "boom"))

		assert.Equal(t, []string{"running:wt-2", "requeued:wt-2"}, rec.seen(),
			"a retryable failure was reported as terminal, so the durable row would show "+
				"failed while the broker still had attempts left")
	})

	t.Run("non-durable tasks are not reported", func(t *testing.T) {
		b, _ := newTestBroker(t, 2, 5*time.Second)
		rec := &recordingWork{}
		b.Work = rec

		// Wrong type, and no parent: neither has a durable row behind it.
		_, err := b.Submit("echo", "hi", "wt-3")
		require.NoError(t, err)
		task, err := b.Claim("w1")
		require.NoError(t, err)
		require.NoError(t, b.RecordResult(task.ID, "w1", "completed", "done"))

		_, err = b.Submit(DurableTaskType, "hi", "")
		require.NoError(t, err)
		task, err = b.Claim("w1")
		require.NoError(t, err)
		require.NoError(t, b.RecordResult(task.ID, "w1", "completed", "done"))

		assert.Empty(t, rec.seen(),
			"the broker reported a task with no durable row; the work layer would look up "+
				"an id that does not exist on every plain task the broker runs")
	})

	t.Run("a nil sink is not a crash", func(t *testing.T) {
		b, _ := newTestBroker(t, 2, 5*time.Second)
		_, err := b.Submit(DurableTaskType, "prompt", "wt-4")
		require.NoError(t, err)
		task, err := b.Claim("w1")
		require.NoError(t, err)
		require.NoError(t, b.RecordResult(task.ID, "w1", "completed", "done"))
	})
}

// TestRequeueStaleReportsToTheWorkLayer covers the path a dead worker takes.
//
// A worker that stops heartbeating never calls RecordResult, so the sweeper is
// the only thing that moves its task — and without this report the durable row
// would sit at running until someone restarted the process. That is the same
// stuck state RecoverInterrupted exists to clean up, arrived at while the
// process is still alive, where no restart is coming.
//
// ledger: A2/DT1#2 状态机正确
func TestRequeueStaleReportsToTheWorkLayer(t *testing.T) {
	// A negative timeout puts the staleness cutoff in the future, so the task
	// is stale the instant it is claimed. hbTimeout 0 is not enough: cutoff
	// lands on the same second as updated_at and the strict comparison misses.
	b, _ := newTestBroker(t, 2, -time.Second)
	rec := &recordingWork{}
	b.Work = rec

	_, err := b.Submit(DurableTaskType, "prompt", "wt-stale")
	require.NoError(t, err)
	_, err = b.Claim("w1")
	require.NoError(t, err)

	require.NoError(t, b.RequeueStale(t.Context()))
	assert.Equal(t, []string{"running:wt-stale", "requeued:wt-stale"}, rec.seen(),
		"the sweeper reclaimed a stale task without telling the durable row, which stays "+
			"at running with no worker holding it")
}
