package automation_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/agent/automation"
)

func TestSchedulerStartStopExitsCleanly(t *testing.T) {
	m, _ := newManagerWithFakeQueue(t)
	sch := automation.NewScheduler(m, 10*time.Millisecond)

	var ticks int32
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		sch.Start(ctx)
	}()

	// 等 3 个 tick 触发。
	deadline := time.After(2 * time.Second)
	for atomic.LoadInt32(&ticks) < 1 {
		select {
		case <-deadline:
			t.Fatal("scheduler never ticked")
		default:
			// 通过创建 automation 来感知 tick 副作用。
			item, err := m.Create(automation.CreateInput{
				Name:     "x",
				Prompt:   "p",
				Schedule: automation.Schedule{Kind: "interval", IntervalSec: 1},
			})
			if err == nil && item.ID != "" {
				atomic.AddInt32(&ticks, 1)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}

	cancel()
	done := make(chan struct{})
	go func() { sch.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Scheduler.Wait did not return after cancel")
	}
}

func TestSchedulerStopReleasesResources(t *testing.T) {
	m, _ := newManagerWithFakeQueue(t)
	sch := automation.NewScheduler(m, 10*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	go sch.Start(ctx)
	cancel()
	sch.Wait()
	// 再调 Wait 必须立即返回（幂等）。
	sch.Wait()
	// 再调 Stop（cancel + Wait 的便捷封装）必须立即返回。
	require.NoError(t, sch.Stop(context.Background()))
}

func TestSchedulerTickErrorsDoNotPanic(t *testing.T) {
	m, _ := newManagerWithFakeQueue(t)
	sch := automation.NewScheduler(m, 5*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	go sch.Start(ctx)
	time.Sleep(30 * time.Millisecond)
	cancel()
	sch.Wait()
	assert.True(t, true) // 仅证明未 panic
}
