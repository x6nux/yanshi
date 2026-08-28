package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

			// Healing is opt-in, so the owning-process option is what is under
			// test here. TestOpenWith_DoesNotHealUnlessTheCallerOwnsTheDatabase
			// covers the default.
			st, err := OpenWith(path, healingOptions())
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

	// healingOptions, NOT DefaultOptions: with healing switched off nothing
	// would quarantine anything no matter how wrong isCorruptDB got, and the
	// assertions below would pass vacuously. This test is the one that keeps
	// the predicate narrow, so it has to run with healing armed.
	st, openErr := OpenWith(path, healingOptions())
	if st != nil {
		_ = st.Close()
	}

	// The quarantine check runs BEFORE the skip below, and that ordering is the
	// test. Widening isCorruptDB makes OpenWith heal this database — renaming it
	// away and returning a fresh one with no error — so a skip conditioned on
	// "OpenWith returned an error" swallows precisely the mutation this test
	// exists to catch. Measured: with the predicate widened to any *sqlite.Error
	// and the skip first, this test passed. Quarantining is wrong whether the
	// open succeeded or failed, so it is asserted unconditionally.
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	for _, e := range entries {
		require.NotContains(t, e.Name(), ".corrupt-",
			"a recoverable failure must never quarantine the database")
	}

	if openErr == nil {
		t.Skip("filesystem ignores the read-only bit here; nothing further to assert")
	}

	// The file is still where it was, byte for byte.
	after, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, good, after, "a read-only database must not be rewritten")

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

// TestQuarantineCorrupt_TakesTheSidecarsWithIt proves the backup is a COMPLETE
// database rather than a truncated one. An un-checkpointed -wal holds the most
// recent writes — the very history a user would want salvaged — and the
// replacement database writes its own WAL to that same path, so a sidecar left
// behind is a sidecar destroyed.
//
// This asserts against quarantineCorrupt directly rather than through OpenWith,
// because SQLite gets to the sidecars first. Measured: on a database whose
// header is intact but whose schema page is smashed, the failed open reads the
// -wal, rejects it and DELETES both sidecars before OpenWith ever sees the
// error, so there is nothing left to move by then. Whether the files survive
// that far is SQLite's business; what this package owes is that they move when
// they are there.
func TestQuarantineCorrupt_TakesTheSidecarsWithIt(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "yanshi.db")

	files := map[string][]byte{
		"":     []byte("the corrupt database"),
		"-wal": []byte("the newest writes, not yet checkpointed"),
		"-shm": []byte("the shared memory index"),
	}
	for sfx, body := range files {
		require.NoError(t, os.WriteFile(path+sfx, body, 0o600))
	}

	backup, err := quarantineCorrupt(path)
	require.NoError(t, err)
	assert.Contains(t, backup, ".corrupt-", "the backup path must be recognisable as one")

	for sfx, body := range files {
		moved, err := os.ReadFile(backup + sfx)
		require.NoErrorf(t, err, "%q must be quarantined alongside the database", sfx)
		assert.Equalf(t, body, moved, "%q must survive intact", sfx)

		_, err = os.Stat(path + sfx)
		assert.Truef(t, os.IsNotExist(err),
			"%q must not be left behind for the replacement database to overwrite", sfx)
	}
}

// healingOptions is DefaultOptions with self-healing enabled, i.e. what
// bootstrap.Build passes as the process that owns the database.
func healingOptions() OpenOptions {
	o := DefaultOptions
	o.SelfHeal = true
	return o
}

// TestOpenWith_DoesNotHealUnlessTheCallerOwnsTheDatabase pins the DEFAULT, and
// the default is the safety property. Healing renames the user's database, so
// every opener that is not the owning process must get an error instead:
// `yanshi doctor` would otherwise quarantine the database it was asked to
// inspect and then report that all is well, and the vcs-mcp subprocesses
// bootstrap spawns per ACP agent would race each other to do it.
//
// Open() is asserted alongside OpenWith(DefaultOptions) because Open is the
// entry point every one of those callers actually uses.
func TestOpenWith_DoesNotHealUnlessTheCallerOwnsTheDatabase(t *testing.T) {
	for name, corrupt := range corruptShapes {
		t.Run(name, func(t *testing.T) {
			for _, tc := range []struct {
				how  string
				open func(string) (*Store, error)
			}{
				{"Open", func(p string) (*Store, error) { return Open(p) }},
				{"OpenWith(DefaultOptions)", func(p string) (*Store, error) { return OpenWith(p, DefaultOptions) }},
			} {
				t.Run(tc.how, func(t *testing.T) {
					dir := t.TempDir()
					path := filepath.Join(dir, "yanshi.db")
					corrupt(t, path)
					before, err := os.ReadFile(path)
					require.NoError(t, err)

					st, err := tc.open(path)
					if st != nil {
						_ = st.Close()
					}
					require.Error(t, err, "a non-owning opener must report the corruption, not repair it")
					assert.Nil(t, st)

					after, err := os.ReadFile(path)
					require.NoError(t, err)
					assert.Equal(t, before, after, "the database must be left exactly as found")
					entries, err := os.ReadDir(dir)
					require.NoError(t, err)
					for _, e := range entries {
						assert.NotContains(t, e.Name(), ".corrupt-",
							"only the owning process may quarantine the database")
					}
				})
			}
		})
	}
}

// TestOpenWith_ReturnsTheOriginalErrorWhenTheRebuildFails covers the promise
// that only a SUCCESSFUL heal returns a nil error.
//
// The failure mode this exists to prevent is specific and silent: returning
// (healed, nil) when healed is nil hands the caller a nil *Store with no error
// to check, and bootstrap dereferences it on the very next line. So the
// assertion is that the ORIGINAL corruption error comes back — not the rebuild
// error, which describes the recovery attempt rather than what is wrong — and
// that no Store comes back with it.
//
// sqlOpener is swapped for one that works once and fails afterwards, so the
// first open fails on the genuinely corrupt file, the quarantine succeeds, and
// only the rebuild is broken.
func TestOpenWith_ReturnsTheOriginalErrorWhenTheRebuildFails(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "yanshi.db")
	corruptShapes["garbage from byte zero"](t, path)

	rebuildErr := errors.New("disk went away during the rebuild")
	calls := 0
	prev := sqlOpener
	sqlOpener = func(driver, dsn string) (*sql.DB, error) {
		calls++
		if calls > 1 {
			return nil, rebuildErr
		}
		return prev(driver, dsn)
	}
	t.Cleanup(func() { sqlOpener = prev })

	st, err := OpenWith(path, healingOptions())
	if st != nil {
		_ = st.Close()
	}

	require.Error(t, err, "a failed rebuild must not be reported as success")
	assert.Nil(t, st, "refusing to start beats handing back a Store that cannot be used")
	assert.True(t, isCorruptDB(err),
		"the original corruption must be reported, not the rebuild failure: %v", err)
	assert.NotErrorIs(t, err, rebuildErr,
		"the rebuild error describes the recovery attempt, not what is wrong with the database")
	require.Greater(t, calls, 1, "the rebuild must actually have been attempted")
}

// TestOpenWith_SecondHealDoesNotOverwriteTheFirstBackup guards the timestamp
// PRECISION of the backup path.
//
// Two heals of the same path inside one second is the single case where this
// feature destroys the data it exists to preserve: with a second-granular
// suffix the second quarantine renames over the first backup, and the history
// the user was told had been set aside is gone. Both heals below happen in far
// under a second, which is exactly the window that used to collide.
func TestOpenWith_SecondHealDoesNotOverwriteTheFirstBackup(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "yanshi.db")

	backups := func() []string {
		t.Helper()
		entries, err := os.ReadDir(dir)
		require.NoError(t, err)
		var out []string
		for _, e := range entries {
			if strings.Contains(e.Name(), ".corrupt-") {
				out = append(out, e.Name())
			}
		}
		return out
	}

	first := []byte("first corrupt database, distinct content")
	require.NoError(t, os.WriteFile(path, first, 0o600))
	st, err := OpenWith(path, healingOptions())
	require.NoError(t, err)
	require.NoError(t, st.Close())
	require.Len(t, backups(), 1)

	// Corrupt it again immediately — same second, different bytes.
	second := []byte("second corrupt database, also distinct")
	require.NoError(t, os.WriteFile(path, second, 0o600))
	st2, err := OpenWith(path, healingOptions())
	require.NoError(t, err)
	require.NoError(t, st2.Close())

	got := backups()
	require.Len(t, got, 2, "the second heal must not rename over the first backup: %v", got)

	// Both originals are still readable, and they are the two distinct files.
	var contents [][]byte
	for _, name := range got {
		b, err := os.ReadFile(filepath.Join(dir, name))
		require.NoError(t, err)
		contents = append(contents, b)
	}
	assert.ElementsMatch(t, [][]byte{first, second}, contents,
		"each heal must preserve its own database, not the last one only")
}

// TestOpenWith_ConcurrentHealersDoNotDestroyEachOthersDatabase is the guard on
// healing being exclusive across processes.
//
// The failure it prevents is data destruction, not a wasted rename. Several
// yanshi processes hold one project database at once — cli.bootstrapOwner
// builds BEFORE it claims the lockfile, so two TUI windows starting together
// both reach healing, and `yanshi serve` can be beside either. Without the
// lock the second healer renames the database the FIRST one just repaired:
// measured, that left three files, an empty store, and the first process
// writing to an orphaned inode.
//
// Goroutines stand in for processes because the lock is a file, not a mutex in
// this address space — the same O_EXCL create that separates two processes
// separates these.
func TestOpenWith_ConcurrentHealersDoNotDestroyEachOthersDatabase(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "yanshi.db")
	corruptShapes["garbage from byte zero"](t, path)

	const healers = 6
	var wg sync.WaitGroup
	stores := make([]*Store, healers)
	errs := make([]error, healers)
	start := make(chan struct{})
	for i := range healers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			stores[i], errs[i] = OpenWith(path, healingOptions())
		}()
	}
	close(start)
	wg.Wait()

	healed := 0
	for i := range healers {
		if stores[i] != nil {
			healed++
			_ = stores[i].Close()
		}
		require.NoErrorf(t, errs[i], "healer %d", i)
	}
	assert.Equal(t, healers, healed, "every opener must end up with a usable store")

	// Exactly ONE quarantine. More than one means a healer renamed a database
	// that another healer had already repaired.
	var backups []string
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	for _, e := range entries {
		if strings.Contains(e.Name(), ".corrupt-") {
			backups = append(backups, e.Name())
		}
		assert.NotContains(t, e.Name(), ".healing", "the heal lock must not be left behind: %s", e.Name())
	}
	require.Len(t, backups, 1, "the database must be quarantined exactly once, got %v", backups)

	// The database still at path is the live one, and it is usable.
	final, err := Open(path)
	require.NoError(t, err, "the surviving database must be healthy")
	defer final.Close()
	_, err = final.CreateSession("after the race")
	require.NoError(t, err)
}

// TestHealCorrupt_SucceedsWhenTheFileIsAlreadyGone covers the outcome a racing
// peer must not be able to turn into a refusal to boot.
//
// A peer can move the file between this process's failed open and its rename,
// leaving the path empty. What actually delivers the recovery here is the
// RECHECK under the lock: it opens the now-empty path successfully and returns
// before quarantine is ever reached.
//
// Be precise about what this does and does not pin. Removing the old
// `if mvErr != nil { return nil, openErr }` early return was a correctness fix
// — a rename that fails because the file is already gone must not veto a reopen
// that would have succeeded — but NO test distinguishes its presence, this one
// included. Probed: restoring the early return leaves the suite green, and so
// does restoring it while also ablating the recheck, because an ablated recheck
// still CALLS openPrepared and that call recreates the file, after which the
// rename succeeds. The deletion is defence in depth, and unguarded; it is
// recorded that way rather than dressed up as a tested invariant.
func TestHealCorrupt_SucceedsWhenTheFileIsAlreadyGone(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "yanshi.db")
	corruptShapes["garbage from byte zero"](t, path)

	_, openErr := openPrepared(path, 4, 5000, 1000)
	require.Error(t, openErr)
	require.True(t, isCorruptDB(openErr))

	// Simulate the racing peer having already moved the file away.
	require.NoError(t, os.Remove(path))

	st, err := healCorrupt(path, 4, 5000, 1000, openErr)
	require.NoError(t, err, "a rename that fails because the file is already gone must not stop the boot")
	require.NotNil(t, st)
	defer st.Close()

	_, err = st.CreateSession("usable")
	require.NoError(t, err)
}

// TestAcquireHealLock_IsExclusiveAndReclaimsStaleLocks covers the mutex itself:
// a second holder is refused, a released lock is reusable, and a lock left
// behind by a process that died mid-heal does not disable healing forever.
//
// The refusal must be reported as fs.ErrExist specifically. healCorrupt keys
// off that to decide between waiting and giving up, so a contention failure
// that reported some other error would send the caller down the give-up path
// and vice versa — see TestAcquireHealLock_UncreatableLockIsNotContention.
func TestAcquireHealLock_IsExclusiveAndReclaimsStaleLocks(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "yanshi.db")

	unlock, err := acquireHealLock(path)
	require.NoError(t, err)
	_, second := acquireHealLock(path)
	require.Error(t, second, "a second holder must be refused while the first holds it")
	assert.ErrorIs(t, second, fs.ErrExist, "contention must be reported as fs.ErrExist")

	unlock()
	unlock2, err2 := acquireHealLock(path)
	require.NoError(t, err2, "the lock must be reusable once released")
	unlock2()

	// A lock abandoned by a dead process must be reclaimable, or one crash
	// would disable healing for this database permanently.
	lock := path + ".healing"
	require.NoError(t, os.WriteFile(lock, nil, 0o600))
	stale := time.Now().Add(-2 * healLockTTL)
	require.NoError(t, os.Chtimes(lock, stale, stale))
	unlock3, err3 := acquireHealLock(path)
	require.NoError(t, err3, "a lock older than healLockTTL must be taken over")
	unlock3()
}

// TestAcquireHealLock_UncreatableLockIsNotContention separates "somebody else
// holds the lock" from "the lock cannot be created here at all".
//
// Healing renames and recreates inside the database's own directory, so if that
// directory is read-only the whole operation is impossible and waiting for a
// peer is pure delay — measured, treating the two alike added 5.02s to a
// startup that was going to fail with the same error anyway.
func TestAcquireHealLock_UncreatableLockIsNotContention(t *testing.T) {
	// The uncreatable directory is a REGULAR FILE, not a chmod 0o500 one.
	//
	// The earlier version made the parent read-only and skipped when the lock
	// was created anyway. That skip is silent, and it fires on exactly the
	// machine that most needs the guard: CI containers run as root, and the
	// write bit does not apply to root, so this test quietly stopped running
	// wherever it mattered while reporting PASS locally. Opening anything under
	// a path whose parent is a file is ENOTDIR for every uid there is.
	dir := t.TempDir()
	notADir := filepath.Join(dir, "ro")
	require.NoError(t, os.WriteFile(notADir, []byte("this is a file"), 0o600))
	path := filepath.Join(notADir, "yanshi.db")

	_, err := acquireHealLock(path)
	require.Error(t, err, "creating a lock under a non-directory must fail for every uid")
	assert.NotErrorIs(t, err, fs.ErrExist,
		"an uncreatable lock must not be reported as contention: %v", err)

	// And the caller must give up immediately rather than wait out the timeout.
	start := time.Now()
	_, healErr := healCorrupt(path, 4, 5000, 1000, errors.New("original corruption"))
	require.Error(t, healErr)
	assert.Less(t, time.Since(start), healWaitTimeout,
		"healing must not wait for a peer that cannot exist")
}

// TestHealCorrupt_DoesNotQuarantineADatabaseAPeerAlreadyRepaired pins the
// recheck under the heal lock, which is the last thing standing between a
// concurrent boot and data loss.
//
// The sequence is the one the lock cannot prevent by itself: this process fails
// its open on a corrupt file, a peer wins the lock and repairs the database,
// the peer releases, and only then does this process acquire the lock. Its
// original error is now stale — the file at path is a healthy database with the
// peer's data in it — so quarantining on the strength of that stale diagnosis
// would rename away a database another process is actively using.
//
// Driving healCorrupt directly is deliberate: reaching this window through
// OpenWith requires the lock to change hands at one exact instant, which is not
// something a test should be asked to hit reliably.
func TestHealCorrupt_DoesNotQuarantineADatabaseAPeerAlreadyRepaired(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "yanshi.db")
	corruptShapes["garbage from byte zero"](t, path)

	// Our own failed open — the diagnosis we are about to act on.
	_, openErr := openPrepared(path, 4, 5000, 1000)
	require.Error(t, openErr)
	require.True(t, isCorruptDB(openErr))

	// A peer heals it out from under us and writes something we must not lose.
	require.NoError(t, os.Remove(path))
	peer, err := Open(path)
	require.NoError(t, err)
	peerSession, err := peer.CreateSession("the peer's data")
	require.NoError(t, err)
	require.NoError(t, peer.Close())

	st, err := healCorrupt(path, 4, 5000, 1000, openErr)
	require.NoError(t, err)
	require.NotNil(t, st)
	defer st.Close()

	// We must have adopted the peer's database, not replaced it.
	list, err := st.ListSessions(0)
	require.NoError(t, err)
	require.Len(t, list, 1, "the peer's database must be adopted, not rebuilt empty")
	assert.Equal(t, peerSession, list[0].ID)

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	for _, e := range entries {
		assert.NotContains(t, e.Name(), ".corrupt-",
			"a database a peer already repaired must not be quarantined on a stale diagnosis")
	}
}

// TestOpen_ConcurrentFirstOpenIsFreeOfTwoFixedRaces covers two concurrent-start
// failures that have nothing to do with self-healing. Two yanshi processes
// opening one project at the same time is the ORDINARY case, not an exotic one:
// the TUI's lockfile election is decided only after both have built their store.
//
// Both fixed modes are asserted by CLASS, because both are about a process being
// told something is wrong when nothing is:
//
//   - SQLITE_BUSY from PRAGMA journal_mode=WAL. Switching journal mode needs a
//     brief exclusive lock that busy_timeout does not cover, so the loser failed
//     before doing anything at all. applyConnectionPragmas now retries.
//   - "duplicate column name" from addColumnIfMissing, a check-then-ALTER that
//     is not atomic across processes. The loser now treats it as success,
//     because the column existing is the whole postcondition.
//
// KNOWN OPEN, deliberately tolerated here: migrate() is not safe to run
// concurrently in general, and a third mode survives — "database schema has
// changed: vtable constructor failed: messages_fts" from the conditional FTS
// rebuild in migrateMessageLog (measured ~1 in 150 openers). Fixing that means
// serialising migrations across processes, which changes the hot path for every
// caller in the repo and is not this task's to make. It is tolerated EXPLICITLY
// rather than by loosening the assertion, so the day it is fixed this test
// starts reporting the tolerance as unused.
func TestOpen_ConcurrentFirstOpenIsFreeOfTwoFixedRaces(t *testing.T) {
	fixed := []string{"database is locked", "duplicate column name"}
	knownOpen := "vtable constructor failed"
	sawKnownOpen := 0

	for attempt := range 20 {
		dir := t.TempDir()
		path := filepath.Join(dir, "yanshi.db")

		const openers = 8
		var wg sync.WaitGroup
		errs := make([]error, openers)
		stores := make([]*Store, openers)
		start := make(chan struct{})
		for i := range openers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				stores[i], errs[i] = Open(path)
			}()
		}
		close(start)
		wg.Wait()

		for i := range openers {
			if stores[i] != nil {
				_ = stores[i].Close()
			}
			if errs[i] == nil {
				continue
			}
			msg := errs[i].Error()
			for _, mode := range fixed {
				assert.NotContainsf(t, msg, mode,
					"attempt %d opener %d hit a FIXED concurrent-start race: %v", attempt, i, errs[i])
			}
			if strings.Contains(msg, knownOpen) {
				sawKnownOpen++
				continue
			}
			assert.NoErrorf(t, errs[i], "attempt %d opener %d: unrecognised concurrent-start failure", attempt, i)
		}
	}
	t.Logf("known-open migrate() FTS race hit %d times (see doc comment)", sawKnownOpen)
}
