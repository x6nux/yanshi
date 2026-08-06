package registry

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestASecondManagerOverTheSameFileSeesAndResumesWhatTheFirstWrote is the
// restart clause, measured across two Manager instances.
//
// TestResumeRestoresSavedConstraintsAndEmitsEvent already proves the harder
// half — a new boot ID reads Running as Interrupted and restores every field —
// but the state.json it reads is hand-written in the test as a persistedState
// literal. That proves the LOADER against a fixture the test itself controls;
// it says nothing about whether a live Manager ever writes a file of that
// shape. A Manager that persisted nothing, or persisted a schema the loader
// rejects, passes it unchanged.
//
// So the subject here is the round trip: Manager A does the writing, and
// nothing but the file on disk crosses between the two.
//
// ledger: B1/M04b#1 重启后可 list/resume
func TestASecondManagerOverTheSameFileSeesAndResumesWhatTheFirstWrote(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")

	a := NewManager(NewManagerOpts{
		RootContext: context.Background(), Path: path,
		SessionBootID: "boot-a", MaxConcurrent: 2,
	})
	id, err := a.Spawn(context.Background(), SpawnRequest{
		Prompt:       "audit the parser",
		Role:         "general",
		AllowedTools: []string{"fs_read", "grep_search"},
		Instruction:  "stay read only",
		Runner: RunnerFunc(func(context.Context, string, string) (string, error) {
			return "first pass", nil
		}),
	})
	require.NoError(t, err)

	rec, err := a.Wait(context.Background(), id, WaitOpts{Timeout: 5 * time.Second})
	require.NoError(t, err)
	require.Equal(t, StatusCompleted, rec.Status)
	a.Close()

	// Everything above this line is gone. The only channel left is the file.
	raw, err := os.ReadFile(path)
	require.NoError(t, err, "the first Manager wrote no state file at all")
	require.NotEmpty(t, raw)

	b := NewManager(NewManagerOpts{
		RootContext: context.Background(), Path: path,
		SessionBootID: "boot-b", MaxConcurrent: 2,
	})
	t.Cleanup(b.Close)

	listed := b.List(true)
	require.Len(t, listed.Agents, 1,
		"the second Manager does not see the agent the first one ran: nothing survived the restart")
	got := listed.Agents[0]
	assert.Equal(t, id, got.ID)
	assert.Equal(t, StatusCompleted, got.Status)
	assert.Equal(t, "first pass", got.Result)
	// The fields Resume needs in order to rebuild the same agent. A snapshot
	// that kept only the ID and status would still satisfy List.
	assert.Equal(t, "audit the parser", got.Prompt)
	assert.Equal(t, []string{"fs_read", "grep_search"}, got.AllowedTools)
	assert.Equal(t, "stay read only", got.Instruction)

	assignment := make(chan string, 1)
	resumedID, err := b.Resume(context.Background(), id, ResumeRequest{
		Runner: RunnerFunc(func(_ context.Context, _, a string) (string, error) {
			assignment <- a
			return "second pass", nil
		}),
	})
	require.NoError(t, err, "the agent is listed but cannot be resumed")
	require.Equal(t, id, resumedID)

	select {
	case a := <-assignment:
		// Resume falls back to the persisted prompt, so the resumed turn works
		// on the same task rather than an empty one.
		assert.Equal(t, "audit the parser", a)
	case <-time.After(5 * time.Second):
		t.Fatal("the resumed runner never ran")
	}

	final, err := b.Wait(context.Background(), id, WaitOpts{Timeout: 5 * time.Second})
	require.NoError(t, err)
	assert.Equal(t, StatusCompleted, final.Status)
	assert.Equal(t, "second pass", final.Result)
}

// TestTerminalAgentReleasesItsConcurrencySlot is the release direction of the
// concurrency cap.
//
// Both existing cap tests (TestSpawnRespectsCapAndReturnsSpawnErrCap and
// internal/tools::TestAcceptance_WorkflowUsesSharedLimitAndList) assert the
// upper bound only: with the cap full, the next Spawn is refused. That
// assertion holds just as well for a counter that never decrements — which is
// exactly what this Manager had, and the reason the cap behaved as a lifetime
// spawn budget rather than a concurrency limit. A batch of more rows than the
// cap could therefore never finish, no matter how fast each row completed.
//
// So the assertion that carries the clause is the opposite one: after an agent
// reaches a terminal status, the slot must be usable again.
//
// ledger: B1/M04b#2 并发上限生效
//
// ledger: F2/LEAK2#1 并发上限生效
//
// ledger: B1/M04#3 取消不泄漏
func TestTerminalAgentReleasesItsConcurrencySlot(t *testing.T) {
	m := NewManager(NewManagerOpts{
		RootContext: context.Background(), Path: filepath.Join(t.TempDir(), "s.json"),
		SessionBootID: "boot", MaxConcurrent: 1,
	})
	t.Cleanup(m.Close)

	first, err := m.Spawn(context.Background(), SpawnRequest{
		Prompt: "one",
		Runner: RunnerFunc(func(context.Context, string, string) (string, error) {
			return "done", nil
		}),
	})
	require.NoError(t, err)
	rec, err := m.Wait(context.Background(), first, WaitOpts{Timeout: 5 * time.Second})
	require.NoError(t, err)
	require.True(t, rec.Status.Terminal())

	// The runtime map is the sole authority on who holds a slot
	// (runningLocked), so an entry left behind is the leak itself rather than
	// a symptom of it — worth asserting directly, because a cap raised for an
	// unrelated reason would hide the leak from the Spawn assertion below.
	m.mtx.RLock()
	live := len(m.runtime)
	m.mtx.RUnlock()
	assert.Equal(t, 0, live,
		"the finished agent still holds a runtime entry, so its concurrency slot was never released")

	second, err := m.Spawn(context.Background(), SpawnRequest{
		Prompt: "two",
		Runner: RunnerFunc(func(context.Context, string, string) (string, error) {
			return "done", nil
		}),
	})
	require.NoError(t, err,
		"the cap rejected a spawn while nothing was running: it is a lifetime budget, not a concurrency limit")
	require.NotEqual(t, first, second)

	rec2, err := m.Wait(context.Background(), second, WaitOpts{Timeout: 5 * time.Second})
	require.NoError(t, err)
	require.Equal(t, StatusCompleted, rec2.Status)

	// The cap must still bite. A release path that simply stopped counting
	// would pass everything above and make MaxConcurrent meaningless.
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	blocking, err := m.Spawn(context.Background(), SpawnRequest{
		Prompt: "three",
		Runner: RunnerFunc(func(ctx context.Context, _, _ string) (string, error) {
			select {
			case <-release:
			case <-ctx.Done():
			}
			return "done", nil
		}),
	})
	require.NoError(t, err)
	require.NotEmpty(t, blocking)

	_, err = m.Spawn(context.Background(), SpawnRequest{
		Prompt: "four",
		Runner: RunnerFunc(func(context.Context, string, string) (string, error) {
			return "done", nil
		}),
	})
	var capErr *SpawnErrCap
	require.ErrorAs(t, err, &capErr,
		"a fourth spawn was accepted while the only slot is occupied: the cap no longer limits anything")
	assert.Equal(t, 1, capErr.Cap)
}
