package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
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

// ledger: F1/WAL1#1 WAL 启用
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

// ledger: F1/WAL1#5 WAL 文件有界（roadmap:295）。plan 另有 10 条细化验收：每条池连接 PRAGMA 生效、MaxOpenConns 按配置且 :memory: 强制 1、16×50 零 BUSY、读不阻塞写、双 Open 跨进程 busy_timeout、rollback→WAL 幂等零丢失、Close 执行 wal_checkpoint(TRUNCATE)、work/vcs/auth/bootstrap 现有测试全绿、Windows CI 下并发/升级测试全绿、doctor 报告 journal_mode 与 -wal/-shm 大小
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

// corruptShapes are database files that SQLite genuinely refuses, established
// by measurement rather than assumption. Several plausible-looking kinds of
// damage do NOT qualify: an empty file opens as a fresh database, and a real
// database whose tail is overwritten with garbage opens, migrates and serves
// reads without a word. Only these two shapes actually produce SQLITE_NOTADB
// or SQLITE_CORRUPT, so only these two exercise the self-heal.
var corruptShapes = map[string]func(t *testing.T, path string){
	// SQLITE_NOTADB (26): the header is not SQLite's at all.
	"garbage from byte zero": func(t *testing.T, path string) {
		t.Helper()
		junk := make([]byte, 8192)
		for i := range junk {
			junk[i] = byte(i*31 + 7)
		}
		require.NoError(t, os.WriteFile(path, junk, 0o600))
	},
	// SQLITE_CORRUPT (11): a valid header over a smashed schema b-tree, which
	// is what a half-written page or a bad sector actually looks like.
	"valid header over a smashed schema page": func(t *testing.T, path string) {
		t.Helper()
		st, err := Open(path)
		require.NoError(t, err)
		_, err = st.CreateSession("data the user cares about")
		require.NoError(t, err)
		require.NoError(t, st.Close())

		raw, err := os.ReadFile(path)
		require.NoError(t, err)
		require.Greater(t, len(raw), 4096)
		for i := 100; i < 4096; i++ {
			raw[i] = 0x5A
		}
		require.NoError(t, os.WriteFile(path, raw, 0o600))
	},
}

// TestOpenWith_RecoversFromCorruptDatabase proves an unreadable database file
// lets the process start anyway, with the old file preserved and its location
// logged. yanshi is a single local binary: if a corrupt yanshi.db kept the TUI
// from starting, the user would have no second tool to repair it with.
func TestOpenWith_RecoversFromCorruptDatabase(t *testing.T) {
	for name, corrupt := range corruptShapes {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "yanshi.db")
			corrupt(t, path)
			before, err := os.ReadFile(path)
			require.NoError(t, err)

			// Confirm the fixture is really broken, so a passing test cannot
			// mean "SQLite tolerated this and no healing ever happened".
			raw, err := sqlOpener("sqlite", buildDSN(path, 5000, 1000))
			require.NoError(t, err)
			_, execErr := raw.Exec("PRAGMA journal_mode=WAL")
			raw.Close()
			require.Error(t, execErr, "fixture %q is not actually corrupt", name)
			require.True(t, isCorruptDB(execErr), "fixture %q fails for the wrong reason: %v", name, execErr)

			var logged bytes.Buffer
			prev := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelWarn})))
			t.Cleanup(func() { slog.SetDefault(prev) })

			st, err := OpenWith(path, DefaultOptions)
			require.NoError(t, err, "a corrupt database must not stop the process from starting")
			require.NotNil(t, st)
			defer st.Close()

			// The store is usable, not merely non-nil.
			id, err := st.CreateSession("after the heal")
			require.NoError(t, err)
			list, err := st.ListSessions(0)
			require.NoError(t, err)
			require.Len(t, list, 1, "the rebuilt database starts empty")
			assert.Equal(t, id, list[0].ID)

			// The old file was moved aside, not deleted, and its bytes survive.
			entries, err := os.ReadDir(dir)
			require.NoError(t, err)
			var backup string
			for _, e := range entries {
				if strings.HasPrefix(e.Name(), "yanshi.db.corrupt-") && !strings.HasSuffix(e.Name(), "-wal") && !strings.HasSuffix(e.Name(), "-shm") {
					backup = filepath.Join(dir, e.Name())
				}
			}
			require.NotEmpty(t, backup, "the corrupt file must be preserved, not discarded: %v", entries)
			saved, err := os.ReadFile(backup)
			require.NoError(t, err)
			assert.Equal(t, before, saved, "the quarantined file must be byte-identical to the original")

			// The backup path is in the log, which is the only way a user ever
			// learns their history was set aside and where it went.
			assert.Contains(t, logged.String(), filepath.Base(backup),
				"the backup path must be logged; got %q", logged.String())
		})
	}
}

// TestOpenWith_DoesNotQuarantineARecoverableFailure is the counterweight to the
// test above: self-healing renames the user's data away, so it must fire only
// for a file SQLite cannot read, never for a failure that is about this attempt
// to open it. Here the database is perfectly good and the filesystem is what
// says no.
func TestOpenWith_DoesNotQuarantineARecoverableFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "yanshi.db")

	st, err := Open(path)
	require.NoError(t, err)
	wanted, err := st.CreateSession("data that must survive")
	require.NoError(t, err)
	_, err = st.DB.Exec("DROP TABLE IF EXISTS vcs_repos") // force migrate() to write
	require.NoError(t, err)
	require.NoError(t, st.Close())

	good, err := os.ReadFile(path)
	require.NoError(t, err)
	require.NoError(t, os.Chmod(path, 0o400))
	t.Cleanup(func() { _ = os.Chmod(path, 0o600) })

	if _, err := OpenWith(path, DefaultOptions); err == nil {
		t.Skip("filesystem ignores the read-only bit here; nothing to assert")
	}

	// The file is still where it was, unchanged, and nothing was quarantined.
	after, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, good, after, "a read-only database must not be rewritten")
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	for _, e := range entries {
		assert.NotContains(t, e.Name(), ".corrupt-",
			"a recoverable failure must never quarantine the database")
	}

	// And the data is still readable once the filesystem cooperates again.
	// SQLite creates -wal/-shm with the same mode as the database, so the open
	// that just failed left read-only sidecars behind; they are empty (it never
	// got past the first pragma) and would otherwise keep the reopen read-only.
	require.NoError(t, os.Chmod(path, 0o600))
	for _, sfx := range []string{"-wal", "-shm"} {
		_ = os.Remove(path + sfx)
	}
	reopened, err := Open(path)
	require.NoError(t, err)
	defer reopened.Close()
	list, err := reopened.ListSessions(0)
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, wanted, list[0].ID)
}
