package task

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/x6nux/yanshi/internal/store"
)

func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(":memory:")
	assert.NoError(t, err)
	t.Cleanup(func() { s.Close() })
	return s
}

func TestBroker_ConcurrentClaimRecordCancel(t *testing.T) {
	s := newTestStore(t)
	b := NewBroker(s, 2, 5*time.Second)

	const n = 10
	ids := make([]string, n)
	for i := range n {
		id, err := b.Submit("race-test", "input", "")
		assert.NoError(t, err)
		ids[i] = id
	}

	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			task, err := b.Claim("worker-1")
			if err != nil || task == nil {
				return
			}
			if idx%2 == 0 {
				_ = b.RecordResult(task.ID, "worker-1", "completed", `{"ok":true}`)
			} else {
				_ = b.Cancel(task.ID)
			}
		}(i)
	}
	wg.Wait()
}

func TestBroker_ConcurrentHeartbeat(t *testing.T) {
	s := newTestStore(t)
	b := NewBroker(s, 2, 5*time.Second)

	id, err := b.Submit("hb-test", "input", "")
	assert.NoError(t, err)
	_, err = b.Claim("worker-1")
	assert.NoError(t, err)

	var wg sync.WaitGroup
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = b.Heartbeat(id)
		}()
	}
	wg.Wait()
}

func TestBroker_ConcurrentRequeueStale(t *testing.T) {
	s := newTestStore(t)
	b := NewBroker(s, 2, time.Hour)

	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id, err := b.Submit("req-test", "in", "")
			if err == nil {
				_, _ = b.Claim("worker-1")
				_ = id
			}
		}()
	}
	wg.Wait()

	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = b.RequeueStale(context.Background())
		}()
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = b.Submit("req-test-2", "in", "")
		}()
	}
	wg.Wait()
}

func TestBroker_LeakProbeCreatedWT(t *testing.T) {
	s := newTestStore(t)
	b := NewBroker(s, 1, 10*time.Millisecond)

	b.createdWTMu.Lock()
	baseline := len(b.createdWT)
	b.createdWT["leak-test-1"] = "wt-1"
	b.createdWT["leak-test-2"] = "wt-2"
	b.createdWTMu.Unlock()

	_ = b.RequeueStale(context.Background())

	b.createdWTMu.Lock()
	t.Logf("createdWT entries after RequeueStale: %d (baseline=%d)", len(b.createdWT), baseline)
	assert.GreaterOrEqual(t, len(b.createdWT), baseline+2,
		"LEAK PROBE: RequeueStale does NOT clean createdWT (F2/LEAK1 fix expected)")
	b.createdWTMu.Unlock()
}
