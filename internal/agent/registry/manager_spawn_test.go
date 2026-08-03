package registry

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestNewManagerDefaults(t *testing.T) {
	m := NewManager(NewManagerOpts{
		RootContext: context.Background(), Path: filepath.Join(t.TempDir(), "s.json"),
		SessionBootID: "boot", MaxConcurrent: 0,
	})
	t.Cleanup(m.Close)
	require.Equal(t, 10, m.limit)
	require.NotNil(t, m.records)
	require.NotNil(t, m.runtime)
}

func TestSpawnReturnsIDAndPersistsRunning(t *testing.T) {
	m, _ := newManager(t)
	block := make(chan struct{})
	id, err := m.Spawn(context.Background(), SpawnRequest{
		AgentType: "subagent", Role: "explore", Prompt: "scan",
		Runner: RunnerFunc(func(ctx context.Context, _, _ string) (string, error) {
			<-block
			return "summary done", nil
		}),
	})
	require.NoError(t, err)
	require.NotEmpty(t, id)

	rec, ok := m.Result(id)
	require.True(t, ok)
	require.Equal(t, StatusRunning, rec.Status)

	list := m.List(false)
	require.Equal(t, 1, list.Running)
	close(block)
}

func TestSpawnRespectsCapAndReturnsSpawnErrCap(t *testing.T) {
	m, _ := newManager(t)
	block := make(chan struct{})
	_, err := m.Spawn(context.Background(), SpawnRequest{
		Role: "explore", Prompt: "a",
		Runner: RunnerFunc(func(context.Context, string, string) (string, error) {
			<-block
			return "a", nil
		}),
	})
	require.NoError(t, err)

	_, err = m.Spawn(context.Background(), SpawnRequest{
		Role: "explore", Prompt: "b",
		Runner: RunnerFunc(func(context.Context, string, string) (string, error) {
			<-block
			return "b", nil
		}),
	})
	var capErr *SpawnErrCap
	require.ErrorAs(t, err, &capErr)
	require.Equal(t, 1, capErr.Cap)
	close(block)
}

func TestSpawnRejectsClosedManager(t *testing.T) {
	m, _ := newManager(t)
	m.Close()
	_, err := m.Spawn(context.Background(), SpawnRequest{
		Role: "explore", Prompt: "x",
		Runner: RunnerFunc(func(context.Context, string, string) (string, error) {
			return "x", nil
		}),
	})
	require.ErrorIs(t, err, ErrClosed)
}

// TestRunAgentLoopBindsRoleIntoRunnerContext pins the missing half of the role
// pipeline. The consumer side has always been complete — the orchestrator reads
// registry.RoleFromContext, looks the role up in the tools.AgentRoles catalog,
// and applies both the PromptPrefix and the per-role tools.RolePolicy — but
// nothing ever *bound* the role, so every one of the seven role definitions in
// internal/tools/agentroles.go was inert for every registry-spawned agent: the
// lookup always ran against the empty string. Binding it in runAgentLoop (the
// single place every sub-agent run funnels through) is what makes the catalog
// take effect.
func TestRunAgentLoopBindsRoleIntoRunnerContext(t *testing.T) {
	m, _ := newManager(t)
	seen := make(chan string, 1)
	id, err := m.Spawn(context.Background(), SpawnRequest{
		AgentType: "subagent", Role: "explore", Prompt: "scan",
		Runner: RunnerFunc(func(ctx context.Context, _, _ string) (string, error) {
			seen <- RoleFromContext(ctx)
			return "done", nil
		}),
	})
	require.NoError(t, err)

	_, err = m.Wait(context.Background(), id, WaitOpts{Timeout: 2 * time.Second})
	require.NoError(t, err)

	select {
	case role := <-seen:
		require.Equal(t, "explore", role,
			"runner context must carry the spawn role, else agentroles.go is inert")
	case <-time.After(time.Second):
		t.Fatal("runner never observed the context")
	}
}

// TestRunAgentLoopLeavesEmptyRoleUnbound guards the other direction: a spawn
// with no role must not bind an empty role, which would shadow a role bound
// further out (e.g. by the orchestrator) with a value that matches no catalog
// entry.
func TestRunAgentLoopLeavesEmptyRoleUnbound(t *testing.T) {
	// The child context descends from the manager's root context, so binding
	// the outer role there is what an ambient (non-registry) binding looks
	// like from runAgentLoop's point of view.
	m := NewManager(NewManagerOpts{
		RootContext:   WithRole(context.Background(), "outer"),
		Path:          filepath.Join(t.TempDir(), "subagents.v1.json"),
		SessionBootID: "boot", MaxConcurrent: 1,
	})
	t.Cleanup(m.Close)
	seen := make(chan string, 1)
	id, err := m.Spawn(context.Background(), SpawnRequest{
		AgentType: "subagent", Prompt: "scan",
		Runner: RunnerFunc(func(ctx context.Context, _, _ string) (string, error) {
			seen <- RoleFromContext(ctx)
			return "done", nil
		}),
	})
	require.NoError(t, err)

	_, err = m.Wait(context.Background(), id, WaitOpts{Timeout: 2 * time.Second})
	require.NoError(t, err)

	select {
	case role := <-seen:
		require.Equal(t, "outer", role, "an empty role must not overwrite an outer binding")
	case <-time.After(time.Second):
		t.Fatal("runner never observed the context")
	}
}

func newManager(t *testing.T) (*Manager, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "subagents.v1.json")
	m := NewManager(NewManagerOpts{
		RootContext: context.Background(), Path: path,
		SessionBootID: "boot", MaxConcurrent: 1,
	})
	t.Cleanup(m.Close)
	return m, path
}
