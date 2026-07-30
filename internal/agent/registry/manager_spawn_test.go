package registry

import (
	"context"
	"path/filepath"
	"testing"

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
