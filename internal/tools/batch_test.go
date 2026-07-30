package tools_test

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
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

func TestAgentBatchCSVInputEndToEnd(t *testing.T) {
	set, _ := newBatchTools(t)
	echo := func(_ context.Context, prompt string, _ []string, _ string) (string, error) {
		return "ok-" + prompt, nil
	}
	ctx := tools.WithSubAgentRunner(
		tools.WithProfile(context.Background(), allowAllForBatch()),
		echo,
	)
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
	ctx := tools.WithSubAgentRunner(
		tools.WithProfile(context.Background(), allowAllForBatch()),
		echo,
	)
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
	ctx := tools.WithSubAgentRunner(
		tools.WithProfile(context.Background(), allowAllForBatch()),
		echo,
	)
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
	ctx := tools.WithSubAgentRunner(
		tools.WithProfile(context.Background(), allowAllForBatch()),
		echo,
	)
	payload := map[string]any{"prompt": "DO"}
	args := wrapInput(t, payload)
	result, err := set.AgentBatch.InvokableRun(ctx, args)
	require.NoError(t, err)
	assert.Contains(t, result, "exactly one of")
}

func TestAgentBatchRejectedWithoutSubAgentRunner(t *testing.T) {
	set, _ := newBatchTools(t)
	ctx := tools.WithProfile(context.Background(), allowAllForBatch())
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
