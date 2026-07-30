package registry

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAddUsageAccumulatesAndPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.json")
	m := NewManager(NewManagerOpts{
		RootContext: context.Background(), Path: path, SessionBootID: "boot", MaxConcurrent: 2,
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

	require.NoError(t, m.AddUsage(id, Usage{PromptTokens: 100, CompletionTokens: 20, TotalTokens: 120, ModelCalls: 1}))
	require.NoError(t, m.AddUsage(id, Usage{PromptTokens: 50, CompletionTokens: 5, TotalTokens: 55, ModelCalls: 1}))

	snap, ok := m.Result(id)
	require.True(t, ok)
	require.Equal(t, int64(150), snap.Usage.PromptTokens)
	require.Equal(t, int64(25), snap.Usage.CompletionTokens)
	require.Equal(t, int64(175), snap.Usage.TotalTokens)
	require.Equal(t, int64(2), snap.Usage.ModelCalls)

	close(block)
}
