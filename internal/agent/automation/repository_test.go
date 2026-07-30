package automation_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/agent/automation"
	"github.com/x6nux/yanshi/internal/store"
)

func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestRepositoryLoadEmptyReturnsDefault(t *testing.T) {
	repo := automation.NewRepository(newTestStore(t))
	state, err := repo.Load()
	require.NoError(t, err)
	assert.Equal(t, automation.StateSchemaVersion, state.SchemaVersion)
	assert.Empty(t, state.Automations)
	assert.Empty(t, state.Runs)
}

func TestRepositorySaveLoadRoundTrip(t *testing.T) {
	repo := automation.NewRepository(newTestStore(t))
	original := automation.State{
		SchemaVersion: automation.StateSchemaVersion,
		Automations: []automation.Automation{
			{ID: "auto-1", Name: "nightly", Prompt: "p", Active: true},
		},
		Runs: []automation.Run{
			{ID: "run-1", AutomationID: "auto-1", Status: automation.RunQueued},
		},
	}
	require.NoError(t, repo.Save(original))

	loaded, err := repo.Load()
	require.NoError(t, err)
	require.Len(t, loaded.Automations, 1)
	assert.Equal(t, "nightly", loaded.Automations[0].Name)
	require.Len(t, loaded.Runs, 1)
	assert.Equal(t, automation.RunQueued, loaded.Runs[0].Status)
}

func TestRepositoryRejectsUnknownSchemaVersion(t *testing.T) {
	s := newTestStore(t)
	// 手动写入一个未来版本。
	require.NoError(t, s.KVSet("automation:c1:state", `{"schema_version":99}`))
	repo := automation.NewRepository(s)
	_, err := repo.Load()
	require.Error(t, err)
}

func TestRepositoryRejectsCorruptJSON(t *testing.T) {
	s := newTestStore(t)
	require.NoError(t, s.KVSet("automation:c1:state", `not-json`))
	repo := automation.NewRepository(s)
	_, err := repo.Load()
	require.Error(t, err)
}
