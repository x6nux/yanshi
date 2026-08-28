package bootstrap_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/bootstrap"
	"github.com/x6nux/yanshi/internal/config"
	"github.com/x6nux/yanshi/internal/store"
)

// TestUpkeep_RetentionReachesTheAssembledWorker asserts on the CONSUMPTION of
// storage.retention_days, not on the fact that BuildUpkeep is called.
//
// The distinction is the one this work package keeps re-learning: a test that
// reads the wiring (an AST scan, or "App.Upkeep is not nil") stays green when
// the value stops travelling — delete `RetentionDays: cfg.Storage.RetentionDays`
// from BuildUpkeep and every such assertion still passes while the feature is
// dead. So this drives the REAL assembled worker against the REAL store and
// checks that a stale session actually left the messages table.
func TestUpkeep_RetentionReachesTheAssembledWorker(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "yanshi.db")

	// Seed a stale session through a separate handle, closed before Build, so
	// the App opens the same file on disk rather than sharing a connection.
	sid := seedStaleSession(t, dbPath)

	cfg := &config.Config{
		Server: config.ServerConfig{HTTPAddr: "127.0.0.1:0"},
		Storage: config.StorageConfig{
			SQLitePath:    dbPath,
			RetentionDays: 7,
		},
		Secrets: config.SecretsConfig{Backend: "none"},
	}
	app, err := bootstrap.Build(bootstrap.Options{Cfg: cfg, FakeModel: true})
	require.NoError(t, err)
	t.Cleanup(func() { _ = app.Shutdown(context.Background()) })
	require.NotNil(t, app.Upkeep)

	app.Upkeep.RunOnce(context.Background())

	var rows int
	require.NoError(t, app.Store.DB.QueryRow(
		"SELECT COUNT(*) FROM messages WHERE session_id = ?", sid).Scan(&rows))
	require.Zero(t, rows, "the configured retention must reach the running worker")

	msgs, err := app.Store.Messages(sid)
	require.NoError(t, err)
	require.Len(t, msgs, 4, "and the transcript must still read back in full")
}

// TestUpkeep_ZeroRetentionLeavesTheStoreAlone is the other direction: the same
// assembled path with no retention configured must not touch a single row.
// Without it, a BuildUpkeep that hardcoded a retention would pass the test
// above.
func TestUpkeep_ZeroRetentionLeavesTheStoreAlone(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "yanshi.db")
	sid := seedStaleSession(t, dbPath)

	cfg := &config.Config{
		Server:  config.ServerConfig{HTTPAddr: "127.0.0.1:0"},
		Storage: config.StorageConfig{SQLitePath: dbPath},
		Secrets: config.SecretsConfig{Backend: "none"},
	}
	app, err := bootstrap.Build(bootstrap.Options{Cfg: cfg, FakeModel: true})
	require.NoError(t, err)
	t.Cleanup(func() { _ = app.Shutdown(context.Background()) })

	app.Upkeep.RunOnce(context.Background())

	var rows int
	require.NoError(t, app.Store.DB.QueryRow(
		"SELECT COUNT(*) FROM messages WHERE session_id = ?", sid).Scan(&rows))
	require.Equal(t, 4, rows)
}

// seedStaleSession writes a four-message session backdated 30 days into the
// database at path, then closes its handle.
func seedStaleSession(t *testing.T, path string) string {
	t.Helper()
	s, err := store.Open(path)
	require.NoError(t, err)
	defer func() { require.NoError(t, s.Close()) }()

	sid, err := s.CreateSession("stale")
	require.NoError(t, err)
	_, _, err = s.AppendMessages(sid, []store.Message{
		{Role: store.RoleUser, Content: "first"},
		{Role: store.RoleAssistant, Content: "second"},
		{Role: store.RoleUser, Content: "third"},
		{Role: store.RoleAssistant, Content: "fourth"},
	})
	require.NoError(t, err)
	require.NoError(t, s.WriteTx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.Exec("UPDATE sessions SET updated_at = ? WHERE id = ?",
			time.Now().Add(-30*24*time.Hour).Unix(), sid)
		return err
	}))
	return sid
}

// TestUpkeep_MemoryQuotaReachesTheAssembledWorker guards the second
// config→worker line the same way the first one is guarded: through the real
// assembled App and an observable effect, not through the wiring's shape.
func TestUpkeep_MemoryQuotaReachesTheAssembledWorker(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "yanshi.db")
	seedMemories(t, dbPath, 20)

	cfg := &config.Config{
		Server:  config.ServerConfig{HTTPAddr: "127.0.0.1:0"},
		Storage: config.StorageConfig{SQLitePath: dbPath, MemoryQuota: 5},
		Secrets: config.SecretsConfig{Backend: "none"},
	}
	app, err := bootstrap.Build(bootstrap.Options{Cfg: cfg, FakeModel: true})
	require.NoError(t, err)
	t.Cleanup(func() { _ = app.Shutdown(context.Background()) })

	app.Upkeep.RunOnce(context.Background())

	var n int
	require.NoError(t, app.Store.DB.QueryRow("SELECT COUNT(*) FROM memories").Scan(&n))
	require.Equal(t, 5, n, "the configured quota must reach the running worker")
}

// TestUpkeep_MemoryAutoExtractGatesTheModel is the flag→behaviour guard for
// storage.memory_auto_extract.
//
// It asserts on the LEASE rather than on produced memories, because the fake
// model's output contains no NOTE lines and would yield zero rows either way —
// a memory-count assertion would pass with the flag ignored. Claiming a lease
// happens if and only if the extraction job ran at all, which is exactly the
// thing the flag controls. Deleting the `if cfg.Storage.MemoryAutoExtract`
// branch in BuildUpkeep makes the first half red; hardcoding the model past it
// makes the second half red.
func TestUpkeep_MemoryAutoExtractGatesTheModel(t *testing.T) {
	leases := func(app *bootstrap.App) int {
		t.Helper()
		var n int
		require.NoError(t, app.Store.DB.QueryRow(
			"SELECT COUNT(*) FROM kv WHERE key LIKE 'lease:memextract:%'").Scan(&n))
		return n
	}

	for _, tc := range []struct {
		name    string
		enabled bool
		want    int
	}{
		{"off by default", false, 0},
		{"on when configured", true, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dbPath := filepath.Join(t.TempDir(), "yanshi.db")
			seedStaleSession(t, dbPath)
			cfg := &config.Config{
				Server: config.ServerConfig{HTTPAddr: "127.0.0.1:0"},
				Storage: config.StorageConfig{
					SQLitePath:        dbPath,
					MemoryAutoExtract: tc.enabled,
				},
				Secrets: config.SecretsConfig{Backend: "none"},
			}
			app, err := bootstrap.Build(bootstrap.Options{Cfg: cfg, FakeModel: true})
			require.NoError(t, err)
			t.Cleanup(func() { _ = app.Shutdown(context.Background()) })

			app.Upkeep.RunOnce(context.Background())
			require.Equal(t, tc.want, leases(app))
		})
	}
}

// seedMemories writes n memories into the database at path and closes it.
func seedMemories(t *testing.T, path string, n int) {
	t.Helper()
	s, err := store.Open(path)
	require.NoError(t, err)
	defer func() { require.NoError(t, s.Close()) }()
	for i := range n {
		_, err := s.WriteMemory("note", "seeded fact "+strconv.Itoa(i))
		require.NoError(t, err)
	}
}
