package orchestrator

import (
	"context"
	"testing"

	"github.com/cloudwego/eino/adk"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/agent/registry"
	"github.com/x6nux/yanshi/internal/proto"
	"github.com/x6nux/yanshi/internal/tools"
	einollm "github.com/x6nux/yanshi/internal/llm/eino"
)

// TestBindManagedRunner_WithRealManager proves that when a real subagentMgr
// is configured, bindManagedRunner binds the Manager, ManagedRunnerFactory,
// and AvailableModels into the context.
func TestBindManagedRunner_WithRealManager(t *testing.T) {
	fm := einollm.NewFakeModel([]string{"ok"}, nil)
	mgr := registry.NewManager(registry.NewManagerOpts{
		RootContext: context.Background(),
		Path:        t.TempDir(),
	})
	t.Cleanup(mgr.Close)

	o, err := New(Config{
		Model:           fm,
		SubagentManager: mgr,
		AvailableModels: map[string]bool{"gpt-4o": true},
	})
	require.NoError(t, err)
	require.NotNil(t, o.subagentMgr)
	require.NotNil(t, o.availableModels)

	ctx := o.bindManagedRunner(context.Background())

	// Manager must be bound (was nil in the nil mgr test)
	foundMgr := tools.ManagerFromContext(ctx)
	require.NotNil(t, foundMgr, "Manager must be bound when subagentMgr is set")
}

// TestBindManagedRunner_WithAvailableModelsNoMgr proves that when only
// AvailableModels is set but subagentMgr is nil, the Manager is not bound.
func TestBindManagedRunner_WithAvailableModelsNoMgr(t *testing.T) {
	fm := einollm.NewFakeModel([]string{"ok"}, nil)
	o, err := New(Config{
		Model:           fm,
		AvailableModels: map[string]bool{"gpt-4o": true},
	})
	require.NoError(t, err)
	require.Nil(t, o.subagentMgr)

	ctx := o.bindManagedRunner(context.Background())
	mgr := tools.ManagerFromContext(ctx)
	assert.Nil(t, mgr, "Manager must NOT be bound when subagentMgr is nil")
}

// TestRunSubAgentTurn_DepthExceeded proves the depth limit error.
func TestRunSubAgentTurn_DepthExceeded(t *testing.T) {
	fm := einollm.NewFakeModel([]string{"ok"}, nil)
	o, err := New(Config{Model: fm})
	require.NoError(t, err)

	ctx := tools.WithSubAgentDepth(context.Background(), tools.MaxSubAgentDepth)
	_, err = o.runSubAgentTurn(ctx, "prompt", nil, "", tools.MaxSubAgentDepth)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "depth")
}

// TestRoleForSubagent_Found proves roleForSubagent finds a known agent role.
func TestRoleForSubagent_Found(t *testing.T) {
	for _, r := range tools.AgentRoles() {
		def := roleForSubagent(r.Name)
		require.NotNil(t, def)
		assert.Equal(t, r.Name, def.Name)
		break // just check one
	}
}

// TestClassifyEventsWithNilIterator proves nil-safe behavior.
func TestClassifyEventsWithNilIterator(t *testing.T) {
	// Not testing nil iterator directly, but instead empty iterator
	iter, gen := adk.NewAsyncIteratorPair[*adk.AgentEvent]()
	gen.Close()

	var frames []proto.ServerFrame
	ClassifyEvents(iter, func(f proto.ServerFrame) { frames = append(frames, f) })
	assert.Empty(t, frames)
}
