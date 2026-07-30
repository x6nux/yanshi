package tools

import (
	"context"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/agent/registry"
)

type channelGatedRunner struct {
	started chan string
	release chan struct{}
	done    chan struct{}
	calls   atomic.Int32
}

func newChannelGatedRunner() *channelGatedRunner {
	return &channelGatedRunner{
		started: make(chan string, 20), release: make(chan struct{}), done: make(chan struct{}, 20),
	}
}

func (r *channelGatedRunner) Run(ctx context.Context, agentID, assignment string) (string, error) {
	defer func() { r.done <- struct{}{} }()
	r.calls.Add(1)
	r.started <- assignment
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case <-r.release:
		return "SUMMARY:\nCHANGES:\nEVIDENCE:\nfile.go:1\nRISKS:\nBLOCKERS:", nil
	}
}

func waitTerminal(t *testing.T, mgr *registry.Manager, id string, timeout time.Duration) registry.Record {
	t.Helper()
	rec, err := mgr.Wait(context.Background(), id, registry.WaitOpts{Timeout: timeout})
	require.NoError(t, err)
	return rec
}

func TestAcceptance_WorkflowUsesSharedLimitAndList(t *testing.T) {
	mgr := registry.NewManager(registry.NewManagerOpts{
		RootContext: context.Background(), Path: filepath.Join(t.TempDir(), "s.json"),
		SessionBootID: "boot", MaxConcurrent: 2,
	})
	t.Cleanup(mgr.Close)

	gate := newChannelGatedRunner()
	first, err := mgr.Spawn(context.Background(), registry.SpawnRequest{Role: "explore", Prompt: "A", Runner: gate})
	require.NoError(t, err)
	second, err := mgr.Spawn(context.Background(), registry.SpawnRequest{Role: "explore", Prompt: "B", Runner: gate})
	require.NoError(t, err)
	<-gate.started
	<-gate.started

	_, err = mgr.Spawn(context.Background(), registry.SpawnRequest{Role: "explore", Prompt: "C", Runner: gate})
	var capErr *registry.SpawnErrCap
	require.ErrorAs(t, err, &capErr)
	require.Equal(t, 2, capErr.Cap)
	list := mgr.List(false)
	require.Equal(t, 2, list.Running)
	require.Len(t, list.Agents, 2)

	close(gate.release)
	rec1 := waitTerminal(t, mgr, first, time.Second)
	require.Equal(t, registry.StatusCompleted, rec1.Status)
	rec2 := waitTerminal(t, mgr, second, time.Second)
	require.Equal(t, registry.StatusCompleted, rec2.Status)
}

func TestAcceptance_ParentCancelCascadesToChild(t *testing.T) {
	mgr := registry.NewManager(registry.NewManagerOpts{
		RootContext: context.Background(), Path: filepath.Join(t.TempDir(), "s.json"),
		SessionBootID: "boot", MaxConcurrent: 4,
	})
	t.Cleanup(mgr.Close)

	gate := newChannelGatedRunner()
	parent, err := mgr.Spawn(context.Background(), registry.SpawnRequest{Role: "general", Prompt: "parent", Runner: gate})
	require.NoError(t, err)
	<-gate.started
	child, err := mgr.Spawn(context.Background(), registry.SpawnRequest{ParentID: parent, Role: "explore", Prompt: "child", Runner: gate})
	require.NoError(t, err)
	<-gate.started

	require.NoError(t, mgr.Cancel(parent))
	<-gate.done
	<-gate.done
	recP := waitTerminal(t, mgr, parent, time.Second)
	require.Equal(t, registry.StatusCancelled, recP.Status)
	recC := waitTerminal(t, mgr, child, time.Second)
	require.Equal(t, registry.StatusCancelled, recC.Status)
}

func TestAcceptance_DepthAndUsageQueryable(t *testing.T) {
	mgr := registry.NewManager(registry.NewManagerOpts{
		RootContext: context.Background(), Path: filepath.Join(t.TempDir(), "s.json"),
		SessionBootID: "boot", MaxConcurrent: 4,
	})
	t.Cleanup(mgr.Close)

	gate := newChannelGatedRunner()
	parent, _ := mgr.Spawn(context.Background(), registry.SpawnRequest{Role: "general", Prompt: "parent", Runner: gate})
	<-gate.started
	child, _ := mgr.Spawn(context.Background(), registry.SpawnRequest{ParentID: parent, Role: "explore", Prompt: "child", Runner: gate})
	<-gate.started
	recParent, _ := mgr.Result(parent)
	recChild, _ := mgr.Result(child)
	require.Equal(t, 0, recParent.Depth)
	require.Equal(t, 1, recChild.Depth)

	require.NoError(t, mgr.AddUsage(child, registry.Usage{PromptTokens: 10, CompletionTokens: 3, TotalTokens: 13, ModelCalls: 1}))
	snap, ok := mgr.Result(child)
	require.True(t, ok)
	require.Equal(t, int64(13), snap.Usage.TotalTokens)

	close(gate.release)
}
