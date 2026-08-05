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

// TestTerminalAgentsReleaseTheirSlots pins the leak that made the concurrency
// cap permanently tighten within a process.
//
// Measured before detachRuntime existed: spawn two agents at MaxConcurrent=2,
// wait for both to reach a terminal status, and List reports Running=0 while a
// third Spawn is still rejected for being at the cap. runningLocked took
// max(runtime entries, StatusRunning records) and nothing ever removed a
// runtime entry, so the count only ever grew.
func TestTerminalAgentsReleaseTheirSlots(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(NewManagerOpts{
		RootContext:   context.Background(),
		Path:          filepath.Join(dir, "s.json"),
		SessionBootID: "boot",
		MaxConcurrent: 2,
	})
	t.Cleanup(m.Close)

	instant := RunnerFunc(func(context.Context, string, string) (string, error) {
		return "SUMMARY\ndone", nil
	})
	for i := range 2 {
		id, err := m.Spawn(context.Background(), SpawnRequest{
			AgentType: "subagent", Role: "explore", Prompt: "p", Runner: instant,
		})
		require.NoError(t, err, "spawn %d", i)
		_, err = m.Wait(context.Background(), id, WaitOpts{Timeout: 2 * time.Second})
		require.NoError(t, err, "wait %d", i)
	}

	// Both are finished, so both slots are free.
	_, err := m.Spawn(context.Background(), SpawnRequest{
		AgentType: "subagent", Role: "explore", Prompt: "third", Runner: instant,
	})
	require.NoError(t, err, "a third spawn must fit once the first two are terminal")
}
