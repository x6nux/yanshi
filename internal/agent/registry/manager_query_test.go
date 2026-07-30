package registry

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestWaitReturnsTerminalRecordAndResultIsSnapshot(t *testing.T) {
	m := NewManager(NewManagerOpts{
		RootContext: context.Background(), Path: filepath.Join(t.TempDir(), "s.json"),
		SessionBootID: "boot", MaxConcurrent: 4,
	})
	t.Cleanup(m.Close)

	done := make(chan struct{})
	id, err := m.Spawn(context.Background(), SpawnRequest{
		AgentType: "subagent", Role: "explore", Prompt: "scan",
		Runner: RunnerFunc(func(context.Context, string, string) (string, error) {
			<-done
			return "SUMMARY\n...\nEVIDENCE\nfile.go:10", nil
		}),
	})
	require.NoError(t, err)
	close(done)

	final, err := m.Wait(context.Background(), id, WaitOpts{Timeout: time.Second})
	require.NoError(t, err)
	require.Equal(t, StatusCompleted, final.Status)
	require.Contains(t, final.Result, "EVIDENCE")

	snap, ok := m.Result(id)
	require.True(t, ok)
	require.Equal(t, final, snap)

	list := m.List(false)
	require.Equal(t, 0, list.Running)
}

func TestResultMissingReturnsFalse(t *testing.T) {
	m := NewManager(NewManagerOpts{
		RootContext: context.Background(), Path: filepath.Join(t.TempDir(), "s.json"),
		SessionBootID: "boot", MaxConcurrent: 1,
	})
	_, ok := m.Result("does-not-exist")
	require.False(t, ok)
}

func TestWaitCanceledByContextReturnsLatestRecord(t *testing.T) {
	m := NewManager(NewManagerOpts{
		RootContext: context.Background(), Path: filepath.Join(t.TempDir(), "s.json"),
		SessionBootID: "boot", MaxConcurrent: 1,
	})
	t.Cleanup(m.Close)

	block := make(chan struct{})
	id, _ := m.Spawn(context.Background(), SpawnRequest{
		AgentType: "subagent", Role: "explore", Prompt: "block",
		Runner: RunnerFunc(func(ctx context.Context, _, _ string) (string, error) {
			<-block
			return "ok", nil
		}),
	})

	waitCtx, cancel := context.WithCancel(context.Background())
	cancel()
	latest, err := m.Wait(waitCtx, id, WaitOpts{})
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, StatusRunning, latest.Status) // 超时/取消：返回最新快照 + ctx.Err()

	close(block)
}
