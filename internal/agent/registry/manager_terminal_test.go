package registry

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestFinishTerminalEmitsPersistenceFailedThenTerminal(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "subagents.v1.json")
	m := NewManager(NewManagerOpts{
		RootContext: context.Background(), Path: path, SessionBootID: "boot", MaxConcurrent: 2,
	})
	t.Cleanup(m.Close)

	var mu sync.Mutex
	var events []Event
	sink := EventSink(func(ev Event) {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, ev)
	})

	release := make(chan struct{})
	runnerDone := make(chan struct{})
	id, err := m.Spawn(context.Background(), SpawnRequest{
		AgentType: "subagent", Role: "explore", Prompt: "p", Emit: sink,
		Runner: RunnerFunc(func(context.Context, string, string) (string, error) {
			defer close(runnerDone)
			<-release
			return "SUMMARY\n...\nEVIDENCE\nfile.go", nil
		}),
	})
	require.NoError(t, err)

	// Render the directory unwritable.
	require.NoError(t, os.Rename(dir, dir+".gone"))
	close(release)

	select {
	case <-runnerDone:
	case <-time.After(time.Second):
		t.Fatal("runner did not finish")
	}

	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(events) >= 2
	}, time.Second, 10*time.Millisecond)

	mu.Lock()
	require.GreaterOrEqual(t, len(events), 2)
	last := events[len(events)-1]
	prev := events[len(events)-2]
	require.Equal(t, EventPersistenceFailed, prev.Type)
	require.Contains(t, []EventType{EventCompleted, EventFailed, EventCancelled}, last.Type)
	mu.Unlock()

	snap, ok := m.Result(id)
	require.True(t, ok)
	require.True(t, snap.Status.Terminal())

	// Restore for cleanup.
	_ = os.Rename(dir+".gone", dir)
}
