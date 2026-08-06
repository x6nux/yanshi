package automation_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/agent/automation"
	"github.com/x6nux/yanshi/internal/store"
)

// TestSchedulerEnqueuesWithoutAnyoneCallingTick is the "on time" half of the
// clause.
//
// TestManagerTickEnqueuesDueSlotOnce proves the enqueue, but it calls Tick by
// hand — so it says nothing about anything ever calling Tick on a schedule.
// Measured: replacing `go scheduler.Start(parent)` in
// internal/bootstrap::BuildAutomation with a no-op leaves that test green,
// which means the entire periodic loop could be absent and the ledger would
// still read done. "按时" and "入队" are two claims and only one was covered.
//
// The test therefore calls nothing: it starts the Scheduler and waits for the
// queue to move on its own.
//
// ledger: C1/AU1#2 按时触发入队
func TestSchedulerEnqueuesWithoutAnyoneCallingTick(t *testing.T) {
	s, err := store.Open(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	// A clock that is already past the due slot, so the first tick the
	// Scheduler fires has work to do. Fixing it also keeps the test off
	// wall-clock timing for the DUE decision — only the tick cadence is real.
	due := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	q := newFakeQueue()
	m, err := automation.NewManager(automation.NewRepository(s), q,
		func() time.Time { return due.Add(-time.Minute) })
	require.NoError(t, err)

	_, err = m.Create(automation.CreateInput{
		Name: "x", Prompt: "p",
		Schedule: automation.Schedule{Kind: "interval", IntervalSec: 60},
	})
	require.NoError(t, err)
	require.Equal(t, 0, q.enqueued(), "nothing may be enqueued before the loop runs")

	sched := automation.NewScheduler(m, 10*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	go sched.Start(ctx)
	t.Cleanup(func() { cancel(); sched.Wait() })

	// The Scheduler passes time.Now() to Tick, which is well past the fixed
	// due slot, so the first tick enqueues. Nothing in this test calls Tick.
	deadline := time.After(5 * time.Second)
	for q.enqueued() == 0 {
		select {
		case <-deadline:
			t.Fatal("no run was enqueued: the Scheduler never ticked, so an automation " +
				"only fires when something else calls Tick by hand")
		case <-time.After(5 * time.Millisecond):
		}
	}
	assert.Positive(t, q.enqueued())

	// Exiting on cancel is the other half of "periodic": a loop that runs
	// forever is a leak, and Wait in the cleanup would hang.
	cancel()
	done := make(chan struct{})
	go func() { sched.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the Scheduler did not exit after its context was cancelled")
	}
}
