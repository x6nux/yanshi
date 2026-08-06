package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// isBusy reports whether err is SQLITE_BUSY / "database is locked".
//
// The driver returns these as opaque errors, so the text is the only handle.
// Matching on substrings is deliberate: a false negative here would make the
// zero-BUSY assertions below vacuous, and the two spellings are stable across
// modernc/sqlite versions.
func isBusy(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "busy") || strings.Contains(s, "database is locked")
}

// TestConcurrentWritesRecordZeroBusy counts SQLITE_BUSY explicitly.
//
// TestConcurrentAppend_NoBusy is named for BUSY and asserts on the message
// count: it would catch a BUSY that surfaced as a dropped write, but the clause
// is "并发写不报 locked" and nothing counted the errors by kind. A store that
// retried internally until it succeeded — or one whose writes failed for an
// entirely different reason — reads the same through a count.
//
// ledger: F1/WAL1#2 并发写不报 locked
func TestConcurrentWritesRecordZeroBusy(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "yanshi.db"))
	require.NoError(t, err)
	defer st.Close()
	sid, err := st.CreateSession("c")
	require.NoError(t, err)

	const N, M = 16, 50
	var busy, other atomic.Int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	for g := 0; g < N; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			<-start
			for i := 0; i < M; i++ {
				if err := st.AppendMessage(sid, g*M+i, "user", fmt.Sprintf("g%d-i%d", g, i)); err != nil {
					if isBusy(err) {
						busy.Add(1)
					} else {
						other.Add(1)
						t.Errorf("append: %v", err)
					}
				}
			}
		}(g)
	}
	close(start)
	wg.Wait()

	// busy_timeout is what makes this zero, not writeMu. Measured: removing
	// writeMu alone still yields zero; removing writeMu AND setting
	// busy_timeout(0) yields 717 of 800. So a regression that drops the DSN
	// pragma is what this catches, and the failure message says so.
	assert.Zero(t, busy.Load(),
		"%d of %d concurrent writes returned SQLITE_BUSY; busy_timeout is set per "+
			"connection via the DSN and is what makes contending writers wait instead "+
			"of failing",
		busy.Load(), N*M)
	assert.Zero(t, other.Load())

	count, err := st.SessionMessageCount(sid)
	require.NoError(t, err)
	assert.Equal(t, N*M, count, "no messages lost under concurrent write")
}

// TestWALIsBoundedAfterClose is the "WAL 文件有界" clause.
//
// TestClose_CheckpointsWAL wraps its only assertion in `if err == nil`, so a
// missing -wal file skips the check entirely — and a -wal that was never
// created because the writes did not land skips it too. It also never
// established that the file had grown, so "small at the end" could mean
// "nothing ever happened".
//
// Both halves are asserted here: the WAL grows while the store is open (so
// there is something to reclaim), and it is gone or small afterwards.
//
// What this does NOT isolate is who reclaims it. Measured: replacing the
// explicit wal_checkpoint(TRUNCATE) in Close with a no-op leaves this green,
// because SQLite checkpoints and removes the WAL itself when the last
// connection closes cleanly. The clause is "WAL 文件有界" — a property of the
// end state — so asserting the end state is right; attributing it to one line
// of our code would be wrong, and the comment on Close now says as much.
//
// ledger: F1/WAL1#5 WAL 文件有界（roadmap:295）。plan 另有 10 条细化验收：每条池连接 PRAGMA 生效、MaxOpenConns 按配置且 :memory: 强制 1、16×50 零 BUSY、读不阻塞写、双 Open 跨进程 busy_timeout、rollback→WAL 幂等零丢失、Close 执行 wal_checkpoint(TRUNCATE)、work/vcs/auth/bootstrap 现有测试全绿、Windows CI 下并发/升级测试全绿、doctor 报告 journal_mode 与 -wal/-shm 大小
func TestWALIsBoundedAfterClose(t *testing.T) {
	path := filepath.Join(t.TempDir(), "yanshi.db")
	st, err := Open(path)
	require.NoError(t, err)

	sid, err := st.CreateSession("t")
	require.NoError(t, err)
	// wal_autocheckpoint is set from config; write well past a page or two so
	// the file is non-trivial regardless of where that threshold sits.
	const payload = 4096
	for i := 0; i < 200; i++ {
		require.NoError(t, st.AppendMessage(sid, i, "user", strings.Repeat("x", payload)))
	}

	walPath := path + "-wal"
	fi, err := os.Stat(walPath)
	require.NoError(t, err,
		"no -wal file exists after 200 writes: the database is not in WAL mode, so the "+
			"truncation assertion below would be checking nothing")
	grown := fi.Size()
	require.Positive(t, grown, "the -wal file is empty after 200 writes")

	require.NoError(t, st.Close())

	after, err := os.Stat(walPath)
	if os.IsNotExist(err) {
		// Removing the file is a stronger form of the same guarantee.
		return
	}
	require.NoError(t, err)
	assert.Lessf(t, after.Size(), grown,
		"the -wal file is %d bytes after Close and was %d before: nothing reclaimed it, "+
			"so it carries forward and grows across restarts",
		after.Size(), grown)
	assert.Lessf(t, after.Size(), int64(1<<16),
		"the -wal file is still %d bytes after Close", after.Size())
}

// TestWALConcurrencyDoesNotSerialiseWorseThanSequential is the "性能不退化"
// clause in the only form that can be asserted.
//
// There is no benchmark and no baseline anywhere in internal/store, and a
// number with nothing to compare it to is a measurement, not an assertion — so
// this does not attempt an absolute figure, which would fail on a loaded CI box
// and prove nothing on a fast one. What "not degraded" has to mean for a
// concurrent store is structural: N goroutines writing must not cost more than
// N times one goroutine writing the same amount. A regression that reintroduced
// per-write lock contention, or dropped WAL back to rollback journaling, shows
// up as a superlinear ratio; ordinary machine noise does not.
//
// The generous factor is the point. This catches a collapse, not a slowdown.
//
// ledger: F1/WAL1#3 性能不退化
func TestWALConcurrencyDoesNotSerialiseWorseThanSequential(t *testing.T) {
	const total = 400

	write := func(t *testing.T, st *Store, sid string, from, count int) {
		for i := from; i < from+count; i++ {
			require.NoError(t, st.AppendMessage(sid, i, "user", strings.Repeat("y", 512)))
		}
	}

	// Sequential baseline.
	seqStore, err := Open(filepath.Join(t.TempDir(), "seq.db"))
	require.NoError(t, err)
	defer seqStore.Close()
	seqSID, err := seqStore.CreateSession("s")
	require.NoError(t, err)
	seqStart := time.Now()
	write(t, seqStore, seqSID, 0, total)
	seqElapsed := time.Since(seqStart)

	// Same total work, spread over 8 writers.
	parStore, err := Open(filepath.Join(t.TempDir(), "par.db"))
	require.NoError(t, err)
	defer parStore.Close()
	parSID, err := parStore.CreateSession("p")
	require.NoError(t, err)
	const writers = 8
	perWriter := total / writers
	parStart := time.Now()
	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			write(t, parStore, parSID, w*perWriter, perWriter)
		}(w)
	}
	wg.Wait()
	parElapsed := time.Since(parStart)

	t.Logf("sequential %d writes: %v — %d concurrent writers: %v",
		total, seqElapsed, writers, parElapsed)

	require.Positive(t, seqElapsed, "the sequential baseline took no measurable time")
	assert.Lessf(t, parElapsed, 8*seqElapsed,
		"the same %d writes took %v spread over %d writers and %v sequentially — more than "+
			"8x. Concurrency is not merely unhelpful here (writes are serialised by design), "+
			"it is actively degrading, which is what a lock-contention or journal-mode "+
			"regression looks like",
		total, parElapsed, writers, seqElapsed)

	seqCount, err := seqStore.SessionMessageCount(seqSID)
	require.NoError(t, err)
	parCount, err := parStore.SessionMessageCount(parSID)
	require.NoError(t, err)
	require.Equal(t, total, seqCount)
	require.Equal(t, total, parCount, "the concurrent run was faster because it wrote less")
}

// TestEveryPoolConnectionGetsItsPragmas closes the gap the WAL clause's own
// note flags.
//
// journal_mode is a database-level property, so reading it through any one
// connection is right. synchronous, busy_timeout and wal_autocheckpoint are
// PER CONNECTION and come from the DSN — a pool connection opened later, or one
// opened while others are checked out, would silently miss them.
// TestOpen_PragmasOnEveryPoolConn checks busy_timeout on four connections but
// closes each before opening the next, so it never has more than one live at a
// time and cannot see a per-connection divergence.
//
// ledger: F1/WAL1#1 WAL 启用
func TestEveryPoolConnectionGetsItsPragmas(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "yanshi.db"))
	require.NoError(t, err)
	defer st.Close()

	max := st.DB.Stats().MaxOpenConnections
	require.Positive(t, max)

	// Hold them all at once: this is what forces the pool to open fresh
	// connections rather than hand back the one already configured.
	conns := make([]*sql.Conn, 0, max)
	defer func() {
		for _, c := range conns {
			_ = c.Close()
		}
	}()
	ctx := context.Background()
	for i := 0; i < max; i++ {
		c, err := st.DB.Conn(ctx)
		require.NoErrorf(t, err, "could not open pool connection %d", i)
		conns = append(conns, c)
	}

	for i, c := range conns {
		var busy int
		require.NoError(t, c.QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&busy))
		assert.Positivef(t, busy,
			"pool connection %d has busy_timeout=0: a cross-process lock fails immediately "+
				"on this connection instead of waiting", i)

		var sync int
		require.NoError(t, c.QueryRowContext(ctx, "PRAGMA synchronous").Scan(&sync))
		assert.Equalf(t, 1, sync,
			"pool connection %d has synchronous=%d, not NORMAL(1)", i, sync)

		var mode string
		require.NoError(t, c.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&mode))
		assert.Equalf(t, "wal", mode, "pool connection %d reports journal_mode=%s", i, mode)
	}
}
