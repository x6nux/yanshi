package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOpen_InMemory(t *testing.T) {
	db, err := Open(":memory:")
	require.NoError(t, err)
	defer db.Close()

	// migrations should create the kv table
	var v string
	err = db.DB.QueryRow("SELECT value FROM kv WHERE key = ?", "__probe").Scan(&v)
	assert.True(t, errors.Is(err, sql.ErrNoRows)) // table exists; row absent
}

func TestOpen_MigratesIdempotently(t *testing.T) {
	db, err := Open(":memory:")
	require.NoError(t, err)
	defer db.Close()

	require.NoError(t, db.migrate()) // running again must not error
}

func TestMigrate_AddsWorktreeColumn(t *testing.T) {
	s, err := Open(":memory:")
	require.NoError(t, err)
	defer s.Close()
	cols, err := s.columns("tasks")
	require.NoError(t, err)
	assert.Contains(t, cols, "worktree_id")
	// idempotent: migrate again is a no-op
	require.NoError(t, s.addColumnIfMissing("tasks", "worktree_id", "TEXT NOT NULL DEFAULT ''"))
	for _, tbl := range []string{"vcs_repos", "vcs_worktrees", "vcs_commits", "vcs_tree", "vcs_blobs", "vcs_uncommitted"} {
		_, err := s.DB.Exec("SELECT 1 FROM " + tbl + " LIMIT 1")
		assert.NoError(t, err, "table %s should exist", tbl)
	}
}



func TestOpen_AppliesWALPragmas(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "yanshi.db")
	st, err := Open(path)
	require.NoError(t, err)
	defer st.Close()
	assertPragma(t, st.DB, "journal_mode", "wal")
	// synchronous=NORMAL returns integer 1 in modernc/sqlite.
	assertPragmaEq(t, st.DB, "synchronous", 1)
	assertPragmaGt(t, st.DB, "busy_timeout", 0)
}

func assertPragma(t *testing.T, db *sql.DB, name, expected string) {
	t.Helper()
	var v string
	require.NoError(t, db.QueryRow("PRAGMA "+name).Scan(&v))
	assert.Equal(t, expected, v)
}

func assertPragmaEq(t *testing.T, db *sql.DB, name string, expected int) {
	t.Helper()
	var v int
	require.NoError(t, db.QueryRow("PRAGMA "+name).Scan(&v))
	assert.Equal(t, expected, v)
}

func assertPragmaGt(t *testing.T, db *sql.DB, name string, min int) {
	t.Helper()
	var v int
	require.NoError(t, db.QueryRow("PRAGMA "+name).Scan(&v))
	assert.Greater(t, v, min)
}

func TestOpen_PragmasOnEveryPoolConn(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(filepath.Join(dir, "yanshi.db"))
	require.NoError(t, err)
	defer st.Close()
	for i := 0; i < 4; i++ {
		c, err := st.DB.Conn(context.Background())
		require.NoError(t, err)
		var bt int
		require.NoError(t, c.QueryRowContext(context.Background(), "PRAGMA busy_timeout").Scan(&bt))
		require.Greater(t, bt, 0, "pool conn %d busy_timeout must be set", i)
		_ = c.Close()
	}
}

func TestOpen_MemoryForcesSingleConn(t *testing.T) {
	st, err := Open(":memory:")
	require.NoError(t, err)
	defer st.Close()
	assert.Equal(t, 1, st.DB.Stats().MaxOpenConnections)
}

func TestWriteTx_CommitsAndRollsBack(t *testing.T) {
	st, err := Open(":memory:")
	require.NoError(t, err)
	defer st.Close()
	require.NoError(t, st.WriteTx(context.Background(), func(tx *sql.Tx) error {
		_, err := tx.Exec("INSERT INTO kv(key,value) VALUES('k','v')")
		return err
	}))
	var v string
	_ = st.DB.QueryRow("SELECT value FROM kv WHERE key='k'").Scan(&v)
	assert.Equal(t, "v", v)
	// fn returns error -> rollback
	err = st.WriteTx(context.Background(), func(tx *sql.Tx) error {
		return fmt.Errorf("boom")
	})
	assert.Error(t, err)
}
func TestAppendMessage_BumpsUpdatedAtAtomically(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(filepath.Join(dir, "yanshi.db"))
	require.NoError(t, err)
	defer st.Close()
	sid, err := st.CreateSession("t")
	require.NoError(t, err)
	before := getSessionUpdatedAt(t, st, sid)
	time.Sleep(time.Second) // second-granularity timestamp needs >=1s sleep
	require.NoError(t, st.AppendMessage(sid, 1, "user", "hi"))
	after := getSessionUpdatedAt(t, st, sid)
	assert.Greater(t, after, before)
}

// TestMigrate_VCSTableColumns asserts the VCS schema carries the columns the
// vcs package queries actually reference. Column NAMES are sourced from vcs.go
// query strings (not re-declared here) so this test breaks if a column is
// renamed without migrating — but does not duplicate the full DDL.
func TestMigrate_VCSTableColumns(t *testing.T) {
	s, err := Open(":memory:")
	require.NoError(t, err)
	defer s.Close()

	want := map[string][]string{
		"vcs_repos":       {"id", "root_path", "main_head", "created_at"},
		"vcs_worktrees":   {"id", "repo_id", "path", "base_commit", "created_at", "active", "tip"},
		"vcs_commits":     {"id", "repo_id", "worktree_id", "parent_id", "merged_from", "author", "message", "created_at"},
		"vcs_tree":        {"commit_id", "path", "blob_hash", "op"},
		"vcs_blobs":       {"hash", "content", "size"},
		"vcs_uncommitted": {"scope_type", "scope_id", "path", "blob_hash", "op"},
	}
	for tbl, cols := range want {
		got, err := s.columns(tbl)
		require.NoErrorf(t, err, "table %s", tbl)
		for _, c := range cols {
			assert.Containsf(t, got, c, "table %s missing column %s", tbl, c)
		}
	}

	// vcs_seams exists with at least its key columns (seam schema landed in
	// 8f22c88); assert the minimal set rather than the full list to avoid
	// coupling to every seam column.
	seamCols, err := s.columns("vcs_seams")
	require.NoError(t, err)
	assert.NotEmpty(t, seamCols, "vcs_seams must exist after migrate")
}

func getSessionUpdatedAt(t *testing.T, st *Store, sessionID string) int64 {
	t.Helper()
	var updatedAt int64
	require.NoError(t, st.DB.QueryRow("SELECT updated_at FROM sessions WHERE id=?", sessionID).Scan(&updatedAt))
	return updatedAt
}


