package registry

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// blockingRunner returns a Runner that parks until release is closed. Spawn
// reads a parent's depth out of m.runtime, so a parent that finishes before its
// child spawns is invisible and the child comes out at depth 0 — every test
// that builds a chain needs its parents to still be running.
func blockingRunner(release <-chan struct{}) Runner {
	return RunnerFunc(func(ctx context.Context, _, _ string) (string, error) {
		select {
		case <-release:
		case <-ctx.Done():
		}
		return "done", nil
	})
}

// TestCapLimitsConcurrencyNotLifetimeSpawns is the concurrency clause at a cap
// above one.
//
// Both pre-existing cap tests spawn into a FULL manager and assert the refusal.
// That assertion is equally true of a counter that only ever grows, which is
// what runningLocked() returned before W3: finishTerminal never removed the
// runtime entry, so the "limit" was a lifetime spawn budget. Downstream that
// was not merely a wrong number — internal/tools::spawnWithRetry retries
// SpawnErrCap with backoff and no attempt ceiling, so once the budget was spent
// every ManagedSubAgentRun hung until the turn timed out.
//
// Running the cap twice over is what separates the two readings.
//
// ledger: F2/LEAK2#1 并发上限生效
func TestCapLimitsConcurrencyNotLifetimeSpawns(t *testing.T) {
	const cap = 3
	m := NewManager(NewManagerOpts{
		RootContext: context.Background(), Path: filepath.Join(t.TempDir(), "s.json"),
		SessionBootID: "boot", MaxConcurrent: cap,
	})
	t.Cleanup(m.Close)

	done := RunnerFunc(func(context.Context, string, string) (string, error) {
		return "ok", nil
	})

	// Two full waves. The second one is the assertion: under a lifetime budget
	// its first Spawn already fails.
	for wave := 0; wave < 2; wave++ {
		var ids []string
		for i := 0; i < cap; i++ {
			id, err := m.Spawn(context.Background(), SpawnRequest{Prompt: "p", Runner: done})
			require.NoErrorf(t, err,
				"wave %d spawn %d was refused although every earlier agent had finished: "+
					"the cap is counting lifetime spawns, not concurrency", wave, i)
			ids = append(ids, id)
		}
		for _, id := range ids {
			rec, err := m.Wait(context.Background(), id, WaitOpts{Timeout: 5 * time.Second})
			require.NoError(t, err)
			require.True(t, rec.Status.Terminal())
		}
		m.mtx.RLock()
		live := len(m.runtime)
		m.mtx.RUnlock()
		assert.Equalf(t, 0, live,
			"wave %d left %d runtime entries behind after every agent reached a terminal status",
			wave, live)
		assert.Equalf(t, 0, m.List(false).Running,
			"wave %d: List reports agents still running after all of them finished", wave)
	}
}

// TestListRunningAgreesWithTheSpawnGate pins the two counts to each other.
//
// List().Running counts records; the Spawn gate counts runtime entries. While
// those could diverge, the observable symptom was a manager that reported zero
// running agents and refused the next Spawn with SpawnErrCap in the same
// breath — an operator reading /agents had no way to see why. Neither count
// alone catches that; only the comparison does.
//
// ledger: F2/LEAK2#3 计数准确
func TestListRunningAgreesWithTheSpawnGate(t *testing.T) {
	const cap = 2
	m := NewManager(NewManagerOpts{
		RootContext: context.Background(), Path: filepath.Join(t.TempDir(), "s.json"),
		SessionBootID: "boot", MaxConcurrent: cap,
	})
	t.Cleanup(m.Close)

	release := make(chan struct{})
	closed := false
	t.Cleanup(func() {
		if !closed {
			close(release)
		}
	})

	for i := 0; i < cap; i++ {
		_, err := m.Spawn(context.Background(), SpawnRequest{
			Prompt: "p", Runner: blockingRunner(release),
		})
		require.NoError(t, err)
		assert.Equal(t, i+1, m.List(false).Running,
			"List does not see the agent that just took a slot")
	}

	// Full: both views agree that it is full.
	_, err := m.Spawn(context.Background(), SpawnRequest{Prompt: "p", Runner: blockingRunner(release)})
	var capErr *SpawnErrCap
	require.ErrorAs(t, err, &capErr)
	require.Equal(t, cap, m.List(false).Running,
		"the gate refused a spawn but List does not show the manager as full")

	close(release)
	closed = true

	// Empty: both views agree that it is empty. The old defect lived exactly
	// here — Running fell to 0 while the gate stayed at the cap.
	deadline := time.After(5 * time.Second)
	for m.List(false).Running != 0 {
		select {
		case <-deadline:
			t.Fatal("agents never left the running count after their runners returned")
		case <-time.After(5 * time.Millisecond):
		}
	}
	m.mtx.RLock()
	live := len(m.runtime)
	m.mtx.RUnlock()
	assert.Equal(t, 0, live,
		"List reports 0 running but the spawn gate still counts %d: the two views of "+
			"'how many are running' disagree, so a manager can look idle and refuse to spawn", live)

	_, err = m.Spawn(context.Background(), SpawnRequest{
		Prompt: "p",
		Runner: RunnerFunc(func(context.Context, string, string) (string, error) { return "ok", nil }),
	})
	assert.NoError(t, err, "List says 0 running, yet the gate refused the next spawn")
}

// TestDepthWinsWhenBothLimitsAreExceeded pins the priority the Spawn comment
// documents.
//
// Spawn says depth is checked first and that ErrTooDeep wins when both limits
// are exceeded — "a deeper agent will never starve a shallower slot". That
// sentence had no test: TestSpawnRejectsTooDeep runs at MaxConcurrent=10, so
// concurrency is nowhere near its limit and the ordering is never exercised.
// Swapping the two gates leaves it green.
//
// ledger: F2/LEAK2#4 与深度上限交互文档化
func TestDepthWinsWhenBothLimitsAreExceeded(t *testing.T) {
	// The chain itself has to fit: MaxDepth levels of parent occupy MaxDepth
	// slots, so the cap is set to exactly that. The spawn under test is then
	// over BOTH limits at once.
	m := NewManager(NewManagerOpts{
		RootContext: context.Background(), Path: filepath.Join(t.TempDir(), "s.json"),
		SessionBootID: "boot", MaxConcurrent: MaxDepth,
	})
	t.Cleanup(m.Close)

	release := make(chan struct{})
	t.Cleanup(func() { close(release) })

	ctx := context.Background()
	for i := 0; i < MaxDepth; i++ {
		id, err := m.Spawn(ctx, SpawnRequest{Prompt: "p", Runner: blockingRunner(release)})
		require.NoErrorf(t, err, "building the chain failed at level %d", i)
		ctx = WithCurrentAgentID(context.Background(), id)
	}

	// Both limits are now saturated: the chain is MaxDepth deep and holds every
	// slot.
	require.Equal(t, MaxDepth, m.List(false).Running, "the chain does not hold every slot")

	_, err := m.Spawn(ctx, SpawnRequest{
		Prompt: "p",
		Runner: RunnerFunc(func(context.Context, string, string) (string, error) { return "ok", nil }),
	})
	require.Error(t, err)
	require.ErrorIs(t, err, ErrTooDeep,
		"both limits were exceeded and the concurrency gate answered first; Spawn documents "+
			"the opposite, and the difference is load-bearing: SpawnErrCap is retried with "+
			"backoff by internal/tools::spawnWithRetry, so a too-deep agent reported as "+
			"too-busy retries forever instead of failing")

	var capErr *SpawnErrCap
	assert.NotErrorAs(t, err, &capErr, "the error is a cap error, not a depth error")
}
