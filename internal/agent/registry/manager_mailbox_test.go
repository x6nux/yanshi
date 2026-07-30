package registry

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestSendInputQueuesAndAssignPersists(t *testing.T) {
	m := NewManager(NewManagerOpts{
		RootContext: context.Background(), Path: filepath.Join(t.TempDir(), "s.json"),
		SessionBootID: "boot", MaxConcurrent: 2,
	})
	t.Cleanup(m.Close)

	firstDone := make(chan struct{})
	resumeCh := make(chan struct{})
	id, err := m.Spawn(context.Background(), SpawnRequest{
		AgentType: "subagent", Role: "general", Prompt: "first",
		Runner: RunnerFunc(func(ctx context.Context, agentID, assignment string) (string, error) {
			if assignment == "first" {
				close(firstDone)
				<-resumeCh
				return "first done", nil
			}
			return "second done", nil
		}),
	})
	require.NoError(t, err)

	<-firstDone
	require.NoError(t, m.SendInput(id, "follow up", false))
	close(resumeCh)

	final, err := m.Wait(context.Background(), id, WaitOpts{Timeout: time.Second})
	require.NoError(t, err)
	require.Equal(t, StatusCompleted, final.Status)
	require.Equal(t, "second done", final.Result)
}

func TestSendInputRejectsNotRunning(t *testing.T) {
	m := NewManager(NewManagerOpts{
		RootContext: context.Background(), Path: filepath.Join(t.TempDir(), "s.json"),
		SessionBootID: "boot", MaxConcurrent: 1,
	})
	t.Cleanup(m.Close)
	require.ErrorIs(t, m.SendInput("ghost", "x", false), ErrNotRunning)
}

func TestAssignPersistsBeforeEnqueueAndRollsBackOnFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "subagents.v1.json")
	m := NewManager(NewManagerOpts{
		RootContext: context.Background(), Path: path, SessionBootID: "boot", MaxConcurrent: 1,
	})
	t.Cleanup(m.Close)

	block := make(chan struct{})
	id, err := m.Spawn(context.Background(), SpawnRequest{
		AgentType: "subagent", Role: "general", Prompt: "p",
		Runner: RunnerFunc(func(ctx context.Context, _, _ string) (string, error) {
			<-block
			return "ok", nil
		}),
	})
	require.NoError(t, err)

	require.NoError(t, m.Assign(id, "audit module auth"))
	snap, ok := m.Result(id)
	require.True(t, ok)
	require.Equal(t, "audit module auth", snap.Assignment)

	// Persist failure: rename dir, assign must roll back.
	require.NoError(t, os.Rename(dir, dir+".gone"))
	err = m.Assign(id, "second assignment must not stick")
	require.Error(t, err)
	require.NoError(t, os.Rename(dir+".gone", dir))
	snap, _ = m.Result(id)
	require.Equal(t, "audit module auth", snap.Assignment)

	close(block)
}

func TestCancelAgent(t *testing.T) {
	m := NewManager(NewManagerOpts{
		RootContext: context.Background(), Path: filepath.Join(t.TempDir(), "s.json"),
		SessionBootID: "boot", MaxConcurrent: 1,
	})
	t.Cleanup(m.Close)

	block := make(chan struct{})
	id, err := m.Spawn(context.Background(), SpawnRequest{
		AgentType: "subagent", Role: "general", Prompt: "p",
		Runner: RunnerFunc(func(ctx context.Context, _, _ string) (string, error) {
			<-block
			return "ok", nil
		}),
	})
	require.NoError(t, err)

	require.NoError(t, m.Cancel(id))
	close(block)

	final, err := m.Wait(context.Background(), id, WaitOpts{Timeout: time.Second})
	require.NoError(t, err)
	require.Equal(t, StatusCancelled, final.Status)
}

func TestCancelNotFound(t *testing.T) {
	m := NewManager(NewManagerOpts{
		RootContext: context.Background(), Path: filepath.Join(t.TempDir(), "s.json"),
		SessionBootID: "boot", MaxConcurrent: 1,
	})
	require.ErrorIs(t, m.Cancel("ghost"), ErrNotFound)
}
