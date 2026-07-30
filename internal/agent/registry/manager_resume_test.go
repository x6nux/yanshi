package registry

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestResumeRestoresSavedConstraintsAndEmitsEvent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.json")
	rec := Record{
		ID: "ag-old", SessionBootID: "boot", Role: "custom", Status: StatusRunning,
		Prompt: "audit auth", StartedAt: time.Now().UTC(),
		Custom:          &CustomRole{Name: "audit", PromptPrefix: "audit", AllowedTools: []string{"fs_read"}, ReadOnlyShell: true},
		AllowedTools:    []string{"fs_read"},
		Instruction:     "stay read only",
		ModelOverride:   "gpt-4o-mini",
		ReasoningEffort: "low",
	}
	raw, err := json.Marshal(persistedState{
		SchemaVersion: persistenceSchemaVersion, SessionBootID: "boot", Agents: []Record{rec},
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, raw, 0o600))

	// New boot loads disk Running as Interrupted.
	m := NewManager(NewManagerOpts{
		RootContext: context.Background(), Path: path, SessionBootID: "boot2", MaxConcurrent: 2,
	})
	t.Cleanup(m.Close)
	archived := m.List(true)
	require.Len(t, archived.Agents, 1)
	require.Equal(t, StatusInterrupted, archived.Agents[0].Status)

	seen := make(chan string, 1)
	id, err := m.Resume(context.Background(), rec.ID, ResumeRequest{
		Runner: RunnerFunc(func(ctx context.Context, agentID, assignment string) (string, error) {
			seen <- assignment
			return "second pass", nil
		}),
	})
	require.NoError(t, err)
	require.Equal(t, rec.ID, id)

	got, ok := m.Result(id)
	require.True(t, ok)
	require.Equal(t, StatusRunning, got.Status)
	require.Equal(t, []string{"fs_read"}, got.AllowedTools)
	require.Equal(t, "stay read only", got.Instruction)
	require.Equal(t, "gpt-4o-mini", got.ModelOverride)
	require.Equal(t, "low", got.ReasoningEffort)
	require.NotNil(t, got.Custom)
	require.Equal(t, "audit", got.Custom.Name)
	require.Equal(t, "audit auth", <-seen) // prompt fallback to persisted Prompt
}
