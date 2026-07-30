package tools

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/agent/registry"
)

// ---------------------------------------------------------------------------
// streamAgentResume — test the full flow with a real manager
// ---------------------------------------------------------------------------

func TestStreamAgentResume_NoManager(t *testing.T) {
	tools := NewAgentTools(nil)
	ch := tools.streamAgentResume(context.Background(), `{"agent_id":"x"}`)
	result := readAutomationChunkT(t, ch)
	assert.Contains(t, result, "manager not configured")
}

func TestStreamAgentResume_NotFound(t *testing.T) {
	mgr, _ := newTestManager(t)
	ctx := WithManager(context.Background(), mgr)
	tools := NewAgentTools(nil)
	ch := tools.streamAgentResume(ctx, `{"agent_id":"nonexistent"}`)
	result := readAutomationChunkT(t, ch)
	assert.Contains(t, result, "not found")
}

func TestStreamAgentResume_NoProfile(t *testing.T) {
	mgr, _ := newTestManager(t)
	ctx := WithManager(context.Background(), mgr)
	// First, spawn an agent so we have one to resume
	spawnID := spawnTestAgent(t, mgr)
	tools := NewAgentTools(nil)
	ch := tools.streamAgentResume(ctx, `{"agent_id":"`+spawnID+`"}`)
	result := readAutomationChunkT(t, ch)
	assert.Contains(t, result, "profile")
}

func TestStreamAgentResume_NoFactory(t *testing.T) {
	mgr, _ := newTestManager(t)
	ctx := WithManager(context.Background(), mgr)
	ctx = WithProfile(ctx, defaultTestProfile())
	spawnID := spawnTestAgent(t, mgr)
	tools := NewAgentTools(nil)
	ch := tools.streamAgentResume(ctx, `{"agent_id":"`+spawnID+`"}`)
	result := readAutomationChunkT(t, ch)
	assert.Contains(t, result, "factory")
}

func TestStreamAgentResume_BadArgs(t *testing.T) {
	mgr, _ := newTestManager(t)
	ctx := WithManager(context.Background(), mgr)
	tools := NewAgentTools(nil)
	ch := tools.streamAgentResume(ctx, `not-json`)
	result := readAutomationChunkT(t, ch)
	assert.NotEmpty(t, result)
}

func TestStreamAgentResume_WithFactory(t *testing.T) {
	mgr, _ := newTestManager(t)
	factory := ManagedRunnerFactory(func(allowed []string, instr string) registry.Runner {
		return registry.RunnerFunc(func(ctx context.Context, _, _ string) (string, error) {
			return "resumed", nil
		})
	})
	ctx := context.Background()
	ctx = WithManager(ctx, mgr)
	ctx = WithProfile(ctx, defaultTestProfile())
	ctx = WithManagedRunnerFactory(ctx, factory)
	ctx = WithAvailableModels(ctx, map[string]bool{})

	spawnID := spawnTestAgent(t, mgr)
	tools := NewAgentTools(nil)
	ch := tools.streamAgentResume(ctx, `{"agent_id":"`+spawnID+`"}`)
	var result string
	for c := range ch {
		if c.Result != "" {
			result += c.Result
		}
	}
	// Should succeed or return a resume-related response
	assert.NotEmpty(t, result)
}

// spawnTestAgent creates a simple agent in the manager and returns its ID.
func spawnTestAgent(t *testing.T, mgr *registry.Manager) string {
	t.Helper()
	runner := registry.RunnerFunc(func(ctx context.Context, _, _ string) (string, error) {
		return "done", nil
	})
	id, err := spawnWithRetry(context.Background(), mgr, registry.SpawnRequest{
		AgentType: "subagent",
		Role:      "general",
		Runner:    runner,
	})
	require.NoError(t, err)
	return id
}
