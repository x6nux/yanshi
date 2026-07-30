package tools

import (
	"context"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/agent/registry"
)

func TestManagedRunnerReservesSlotAndPersists(t *testing.T) {
	mgr, _ := newTestManager(t)
	var calls int32
	runner := registry.RunnerFunc(func(ctx context.Context, agentID, assignment string) (string, error) {
		atomic.AddInt32(&calls, 1)
		return "SUMMARY:\nCHANGES:\nEVIDENCE:\nfile.go:1\nRISKS:\nBLOCKERS:", nil
	})

	spec := ManagedSubAgentSpec{
		Role: "explore", Prompt: "scan", AllowedTools: []string{"fs_read"}, Runner: runner,
	}
	res, err := ManagedSubAgentRun(WithManager(context.Background(), mgr), spec)
	require.NoError(t, err)
	require.Contains(t, res.Text, "EVIDENCE")
	require.Equal(t, int32(1), atomic.LoadInt32(&calls))

	list := mgr.List(false)
	require.Equal(t, 0, list.Running)
	require.Len(t, list.Agents, 1)
}

func TestManagedRunnerBlocksUntilSlotFrees(t *testing.T) {
	mgr, _ := newTestManager(t)
	block := make(chan struct{})
	first := registry.RunnerFunc(func(ctx context.Context, _, _ string) (string, error) {
		<-block
		return "first", nil
	})
	second := registry.RunnerFunc(func(context.Context, string, string) (string, error) {
		return "second", nil
	})

	done := make(chan error, 2)
	go func() {
		_, err := ManagedSubAgentRun(WithManager(context.Background(), mgr),
			ManagedSubAgentSpec{Role: "explore", Prompt: "a", Runner: first})
		done <- err
	}()
	go func() {
		_, err := ManagedSubAgentRun(WithManager(context.Background(), mgr),
			ManagedSubAgentSpec{Role: "explore", Prompt: "b", Runner: second})
		done <- err
	}()
	close(block)
	require.NoError(t, <-done)
	require.NoError(t, <-done)
	require.Len(t, mgr.List(false).Agents, 2)
}

func newTestManager(t *testing.T) (*registry.Manager, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "s.json")
	m := registry.NewManager(registry.NewManagerOpts{
		RootContext: context.Background(), Path: path, SessionBootID: "boot", MaxConcurrent: 4,
	})
	t.Cleanup(m.Close)
	return m, path
}
