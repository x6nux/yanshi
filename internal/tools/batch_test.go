package tools_test

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/agent/registry"
	"github.com/x6nux/yanshi/internal/guard"
	"github.com/x6nux/yanshi/internal/tools"
)

func newBatchTools(t *testing.T) (*tools.BatchTools, *registry.Manager) {
	t.Helper()
	m := registry.NewManager(registry.NewManagerOpts{
		RootContext:   context.Background(),
		MaxConcurrent: 4,
		Path:          filepath.Join(t.TempDir(), "state.json"),
	})
	t.Cleanup(m.Close)
	return tools.NewBatchTools(m), m
}

func TestAgentBatchMetadataMentionsB1AndApproval(t *testing.T) {
	set, _ := newBatchTools(t)
	info, err := set.AgentBatch.Info(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "agent_batch", info.Name)
	for _, phrase := range []string{"CSV", "structured", "B1", "concurrency", "Approval"} {
		assert.Contains(t, info.Desc, phrase, "Desc missing %q", phrase)
	}
}

// ledger: C1/M07#1 可提交批量任务
func TestAgentBatchCSVInputEndToEnd(t *testing.T) {
	set, _ := newBatchTools(t)
	echo := func(_ context.Context, prompt string, _ []string, _ string) (string, error) {
		return "ok-" + prompt, nil
	}
	ctx := withApprovingUser(tools.WithSubAgentRunner(
		tools.WithProfile(context.Background(), allowAllForBatch()),
		echo,
	))
	payload := map[string]any{
		"prompt": "DO",
		"csv":    "name,city\nAlice,NYC\nBob,SF\n",
	}
	args := wrapInput(t, payload)
	result, err := set.AgentBatch.InvokableRun(ctx, args)
	require.NoError(t, err)
	assert.Contains(t, result, `"success":2`)
	assert.Contains(t, result, `"index":0`)
	assert.Contains(t, result, `"index":1`)
}

func TestAgentBatchStructuredInputEndToEnd(t *testing.T) {
	set, _ := newBatchTools(t)
	echo := func(_ context.Context, prompt string, _ []string, _ string) (string, error) {
		return "ok", nil
	}
	ctx := withApprovingUser(tools.WithSubAgentRunner(
		tools.WithProfile(context.Background(), allowAllForBatch()),
		echo,
	))
	payload := map[string]any{
		"prompt": "DO",
		"rows":   []map[string]string{{"q": "a"}, {"q": "b"}},
	}
	args := wrapInput(t, payload)
	result, err := set.AgentBatch.InvokableRun(ctx, args)
	require.NoError(t, err)
	assert.Contains(t, result, `"success":2`)
}

func TestAgentBatchRejectsBothCSVAndRows(t *testing.T) {
	set, _ := newBatchTools(t)
	echo := func(_ context.Context, _ string, _ []string, _ string) (string, error) { return "", nil }
	ctx := withApprovingUser(tools.WithSubAgentRunner(
		tools.WithProfile(context.Background(), allowAllForBatch()),
		echo,
	))
	payload := map[string]any{
		"prompt": "DO",
		"csv":    "a\n1\n",
		"rows":   []map[string]string{{"q": "a"}},
	}
	args := wrapInput(t, payload)
	result, err := set.AgentBatch.InvokableRun(ctx, args)
	require.NoError(t, err)
	assert.Contains(t, result, "exactly one of")
}

func TestAgentBatchRejectsNeitherCSVNorRows(t *testing.T) {
	set, _ := newBatchTools(t)
	echo := func(_ context.Context, _ string, _ []string, _ string) (string, error) { return "", nil }
	ctx := withApprovingUser(tools.WithSubAgentRunner(
		tools.WithProfile(context.Background(), allowAllForBatch()),
		echo,
	))
	payload := map[string]any{"prompt": "DO"}
	args := wrapInput(t, payload)
	result, err := set.AgentBatch.InvokableRun(ctx, args)
	require.NoError(t, err)
	assert.Contains(t, result, "exactly one of")
}

func TestAgentBatchRejectedWithoutSubAgentRunner(t *testing.T) {
	set, _ := newBatchTools(t)
	ctx := withApprovingUser(tools.WithProfile(context.Background(), allowAllForBatch()))
	payload := map[string]any{"prompt": "DO", "csv": "a\n1\n"}
	args := wrapInput(t, payload)
	result, err := set.AgentBatch.InvokableRun(ctx, args)
	require.NoError(t, err)
	assert.Contains(t, result, "sub-agent runner is not bound")
}

func TestAgentBatchDeniedWithoutProfile(t *testing.T) {
	set, _ := newBatchTools(t)
	echo := func(_ context.Context, _ string, _ []string, _ string) (string, error) { return "", nil }
	ctx := tools.WithSubAgentRunner(context.Background(), echo)
	result, err := set.AgentBatch.InvokableRun(ctx, wrapInput(t, map[string]any{
		"prompt": "DO", "csv": "a\n1\n",
	}))
	require.NoError(t, err)
	assert.Contains(t, result, "permission denied")
}

// TestAgentBatchDispatchAppliesSubAgentIsolation is behavioural coverage for
// batch.go's isolatedSpawn wrapper (the WithSubAgentIsolation call site at
// batch.go:122). GOV6 only requires that SOME production caller of
// WithSubAgentIsolation exists; agent_dag.go's call site alone would satisfy
// it, so GOV6 stays green even if this exact wrapper is deleted. This test
// drives a real registry.Manager through the actual agent_batch tool
// (set.AgentBatch.InvokableRun -> runAgentBatch -> batch.Runner.Run ->
// Manager.Spawn -> rowRunner.Run -> isolatedSpawn) and asserts the marker is
// visible on the ctx the row spawn function actually receives — not a
// hand-built ctx that bypasses the dispatch path.
//
// ledger: none — new coverage, not tied to an existing acceptance clause.
func TestAgentBatchDispatchAppliesSubAgentIsolation(t *testing.T) {
	set, _ := newBatchTools(t)

	var mu sync.Mutex
	var isolated []bool
	spawn := func(ctx context.Context, _ string, _ []string, _ string) (string, error) {
		mu.Lock()
		isolated = append(isolated, tools.SubAgentIsolationRequested(ctx))
		mu.Unlock()
		return "ok", nil
	}
	ctx := withApprovingUser(tools.WithSubAgentRunner(
		tools.WithProfile(context.Background(), allowAllForBatch()),
		spawn,
	))
	payload := map[string]any{
		"prompt": "DO",
		"rows":   []map[string]string{{"q": "a"}, {"q": "b"}},
	}
	result, err := set.AgentBatch.InvokableRun(ctx, wrapInput(t, payload))
	require.NoError(t, err)
	assert.Contains(t, result, `"success":2`)

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, isolated, 2, "expected both rows to reach the spawn function")
	for i, v := range isolated {
		assert.True(t, v, "row %d: isolation marker did not reach the row's spawn ctx", i)
	}
}

func allowAllForBatch() guard.PermissionProfile {
	return guard.PermissionProfile{Tools: guard.ToolsPerm{Allow: []string{"agent_batch"}}}
}

func wrapInput(t *testing.T, payload map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(payload)
	require.NoError(t, err)
	envelope, err := json.Marshal(map[string]string{"input": string(raw)})
	require.NoError(t, err)
	return string(envelope)
}

// Ensure fmt stays imported if other helpers are removed later.
var _ = fmt.Sprintf
