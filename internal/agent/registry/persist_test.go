package registry

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLoadStateMissingFile(t *testing.T) {
	st, err := loadState(filepath.Join(t.TempDir(), "nonexistent.json"))
	require.NoError(t, err)
	require.Equal(t, persistenceSchemaVersion, st.SchemaVersion)
	require.Empty(t, st.Agents)
}

func TestLoadStateAcceptsUnknownFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.json")
	raw := `{"schema_version":1,"unknown_field":"ignored","agents":[{"id":"ag-1"}]}`
	require.NoError(t, os.WriteFile(path, []byte(raw), 0o600))
	st, err := loadState(path)
	require.NoError(t, err)
	require.Len(t, st.Agents, 1)
	require.Equal(t, "ag-1", st.Agents[0].ID)
}

func TestWriteAndLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.json")
	st := persistedState{
		SchemaVersion: persistenceSchemaVersion,
		SessionBootID: "boot-1",
		Agents: []Record{{
			ID: "ag-1", Status: StatusRunning, Prompt: "test",
		}},
	}
	raw, err := json.Marshal(st)
	require.NoError(t, err)
	require.NoError(t, writeAtomic(path, raw))

	loaded, err := loadState(path)
	require.NoError(t, err)
	require.Equal(t, st.SchemaVersion, loaded.SchemaVersion)
	require.Equal(t, st.SessionBootID, loaded.SessionBootID)
	require.Len(t, loaded.Agents, 1)
	require.Equal(t, "ag-1", loaded.Agents[0].ID)
	require.Equal(t, StatusRunning, loaded.Agents[0].Status)
}

func TestLoadStateHandlesCorruptFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "corrupt.json")
	require.NoError(t, os.WriteFile(path, []byte("{bad json}"), 0o600))
	_, err := loadState(path)
	require.Error(t, err)
}

func TestWriteAtomicCreatesFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.json")
	require.NoError(t, writeAtomic(path, []byte(`{"key":"value"}`)))
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.JSONEq(t, `{"key":"value"}`, string(data))
}

func TestWriteAtomicOverwritesExisting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "overwrite.json")
	require.NoError(t, os.WriteFile(path, []byte(`"old"`), 0o600))
	require.NoError(t, writeAtomic(path, []byte(`"new"`)))
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, `"new"`, string(data))
}
