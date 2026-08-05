package registry

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
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

// TestSendInputInterruptEndsTheTurnNotTheAgent pins the interrupt semantics of
// SendInput: the in-flight turn is cancelled, the queued text becomes the next
// assignment, and the agent survives to run it.
//
// rt.turnCancel had no production assignment before this — only a white-box
// test ever wrote the field — so an interrupting SendInput queued its text and
// then waited out the very turn it was meant to cut short. The agent still
// finished, which is why nothing looked broken.
func TestSendInputInterruptEndsTheTurnNotTheAgent(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(NewManagerOpts{
		RootContext:   context.Background(),
		Path:          filepath.Join(dir, "s.json"),
		SessionBootID: "boot",
		MaxConcurrent: 2,
	})
	t.Cleanup(m.Close)

	firstTurn := make(chan struct{})
	var turns []string
	var mu sync.Mutex
	id, err := m.Spawn(context.Background(), SpawnRequest{
		AgentType: "subagent", Role: "explore", Prompt: "first",
		Runner: RunnerFunc(func(ctx context.Context, _ string, assignment string) (string, error) {
			mu.Lock()
			turns = append(turns, assignment)
			n := len(turns)
			mu.Unlock()
			if n == 1 {
				close(firstTurn)
				<-ctx.Done() // held until the interrupt arrives
				return "", ctx.Err()
			}
			return "SUMMARY\nsecond turn ran", nil
		}),
	})
	require.NoError(t, err)
	<-firstTurn

	require.NoError(t, m.SendInput(id, "second", true))

	rec, err := m.Wait(context.Background(), id, WaitOpts{Timeout: 3 * time.Second})
	require.NoError(t, err)

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, turns, 2, "the interrupt must start a second turn, not end the agent")
	assert.Equal(t, "second", turns[1], "the queued text becomes the next assignment")
	assert.Equal(t, StatusCompleted, rec.Status,
		"a cancelled TURN must not mark the AGENT failed or cancelled")
	assert.Contains(t, rec.Result, "second turn ran")
}

// TestParkFreesCapacityAndUnparkReclaimsIt pins the livelock fix.
//
// A parent that delegates blocks in Wait while holding its slot; its child
// retries Spawn until one frees. At cap N, N parents each waiting on a child
// means every slot is held by something that is only waiting, and the children
// they wait for can never start. Park exists to break that, so the two things
// worth pinning are that it actually frees capacity and that unparking takes
// it back — a Park that never reclaimed would silently uncap the manager.
func TestParkFreesCapacityAndUnparkReclaimsIt(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(NewManagerOpts{
		RootContext:   context.Background(),
		Path:          filepath.Join(dir, "s.json"),
		SessionBootID: "boot",
		MaxConcurrent: 1,
	})
	t.Cleanup(m.Close)

	started := make(chan struct{})
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	holder, err := m.Spawn(context.Background(), SpawnRequest{
		AgentType: "subagent", Role: "explore", Prompt: "holder",
		Runner: RunnerFunc(func(context.Context, string, string) (string, error) {
			close(started)
			<-release
			return "held", nil
		}),
	})
	require.NoError(t, err)
	<-started

	instant := RunnerFunc(func(context.Context, string, string) (string, error) {
		return "SUMMARY\nok", nil
	})

	// The single slot is taken, so a second spawn is refused.
	_, err = m.Spawn(context.Background(), SpawnRequest{
		AgentType: "subagent", Role: "explore", Prompt: "blocked", Runner: instant,
	})
	require.Error(t, err, "the cap must hold while the only agent is doing work")

	// Parking the holder frees its slot: it is waiting, not working.
	unpark := m.Park(holder)
	child, err := m.Spawn(context.Background(), SpawnRequest{
		AgentType: "subagent", Role: "explore", Prompt: "child", Runner: instant,
	})
	require.NoError(t, err, "a parked agent must not consume capacity")
	_, err = m.Wait(context.Background(), child, WaitOpts{Timeout: 2 * time.Second})
	require.NoError(t, err)

	// Unparking takes the capacity back.
	unpark()
	_, err = m.Spawn(context.Background(), SpawnRequest{
		AgentType: "subagent", Role: "explore", Prompt: "after", Runner: instant,
	})
	require.Error(t, err, "unpark must reclaim the slot, or Park silently uncaps the manager")
}

// TestParkOnUnknownAgentIsANoOp keeps callers from having to special-case an
// agent that reached a terminal status between lookup and park.
func TestParkOnUnknownAgentIsANoOp(t *testing.T) {
	m := NewManager(NewManagerOpts{
		RootContext:   context.Background(),
		Path:          filepath.Join(t.TempDir(), "s.json"),
		SessionBootID: "boot",
		MaxConcurrent: 1,
	})
	t.Cleanup(m.Close)
	unpark := m.Park("ag-does-not-exist")
	require.NotNil(t, unpark)
	unpark()
	unpark() // idempotent
}

// TestRunnerContextIsClosedAfterTerminal pins that nothing the runner started
// is left running against a live context once the agent has finished.
//
// ⚠️ It does NOT pin detachRuntime's cancel, despite being written for that.
// The context a runner receives is the TURN context, which its own cleanup
// cancels when the pass returns — so this stays green with detachRuntime's
// cancel neutered. Measured, W3 review round 1.
//
// The agent-level cancel is therefore an UNPINNED design choice: it is what
// stops work bound to the agent rather than to a turn, and no test observes
// it. Reaching it needs a handle on the agent context that outlives the run,
// which the Manager does not currently expose. Do not read this test's name
// as evidence for that guarantee, and do not read the absence of a red test
// as evidence the cancel is unnecessary.
func TestRunnerContextIsClosedAfterTerminal(t *testing.T) {
	m := NewManager(NewManagerOpts{
		RootContext:   context.Background(),
		Path:          filepath.Join(t.TempDir(), "s.json"),
		SessionBootID: "boot",
		MaxConcurrent: 2,
	})
	t.Cleanup(m.Close)

	captured := make(chan context.Context, 1)
	id, err := m.Spawn(context.Background(), SpawnRequest{
		AgentType: "subagent", Role: "explore", Prompt: "p",
		Runner: RunnerFunc(func(ctx context.Context, _, _ string) (string, error) {
			captured <- ctx
			return "SUMMARY\ndone", nil
		}),
	})
	require.NoError(t, err)
	_, err = m.Wait(context.Background(), id, WaitOpts{Timeout: 2 * time.Second})
	require.NoError(t, err)

	turnCtx := <-captured
	select {
	case <-turnCtx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("the agent reached a terminal status but the runner's context is " +
			"still live: anything it started will never be told to stop")
	}
}

// TestSendInputRefusesAgentsWithNoRuntime pins the existence half of
// SendInput's precondition: an unknown agent, and one that has finished, are
// both refused rather than having their follow-up queued into a mailbox no
// runner will read again. Silently accepting would tell the caller their input
// landed when nothing will act on it.
//
// ⚠️ It pins the existence check ONLY. SendInput also requires
// rec.Status == StatusRunning and rt.accepting, and neither is covered:
// measured W3 review round 14, reducing the guard to `!ok || !recOK` reddens
// nothing in the whole package, including this test. The finished-agent case
// cannot reach those conditions because finishTerminal detaches the runtime
// entry first, so !ok fires before them.
//
// Those two remain UNPINNED. Constructing a live runtime entry whose record is
// not Running, or whose mailbox is closed, needs white-box access the Manager
// does not offer. Do not read this test as evidence for them.
func TestSendInputRefusesAgentsWithNoRuntime(t *testing.T) {
	m := NewManager(NewManagerOpts{
		RootContext:   context.Background(),
		Path:          filepath.Join(t.TempDir(), "s.json"),
		SessionBootID: "boot",
		MaxConcurrent: 2,
	})
	t.Cleanup(m.Close)

	require.ErrorIs(t, m.SendInput("ag-never-existed", "hi", false), ErrNotRunning,
		"an unknown agent cannot take input")

	id, err := m.Spawn(context.Background(), SpawnRequest{
		AgentType: "subagent", Role: "explore", Prompt: "p",
		Runner: RunnerFunc(func(context.Context, string, string) (string, error) {
			return "SUMMARY\ndone", nil
		}),
	})
	require.NoError(t, err)
	_, err = m.Wait(context.Background(), id, WaitOpts{Timeout: 2 * time.Second})
	require.NoError(t, err)

	require.ErrorIs(t, m.SendInput(id, "too late", false), ErrNotRunning,
		"a finished agent must refuse input rather than queue it for a runner that is gone")
	// Reached via the existence check, not the status check — see the note above.
}
