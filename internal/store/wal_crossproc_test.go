package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCrossProcessBusyTimeout_DualOpen simulates two independent processes
// (two store.Opens, each with its own writeMu) contending on the same database
// file. The busy_timeout DSN setting should let the second writer wait for the
// first's lock to release rather than failing with SQLITE_BUSY.
// TestCrossProcessBusyTimeout_DualOpen pins that a second connection waits out
// a writer holding the database file lock instead of failing immediately.
//
// The predecessor did not test that at all: it locked a.writeMu — a mutex
// private to Store instance a — and then called b.KVSet, where b is a separate
// Store with its OWN writeMu. b never waited on anything, and the 50ms sleep
// was long enough for it to finish before the "lock" was released. What it
// actually proved is that two Store instances can both write the same file,
// which is true with busy_timeout set to zero.
//
// The lock that matters is SQLite's, taken by a real write transaction. It is
// file-level, so two sql.DB handles in one process contend exactly the way two
// processes do — no subprocess needed to exercise the semantics.
//
// ledger: F1/WAL1#2 并发写不报 locked
func TestCrossProcessBusyTimeout_DualOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "yanshi.db")
	a, err := Open(path)
	require.NoError(t, err)
	defer a.Close()

	b, err := Open(path)
	require.NoError(t, err)
	defer b.Close()

	// a takes the write lock for real and holds it.
	tx, err := a.DB.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	_, err = tx.Exec("INSERT INTO kv(key,value) VALUES('holder','1')")
	require.NoError(t, err, "the transaction did not acquire the write lock, so b has nothing to wait for")

	done := make(chan error, 1)
	go func() { done <- b.KVSet("cross_proc_key", "cross_proc_val") }()

	// b must still be blocked. Returning here would mean the write lock is not
	// being contended and the rest of the test proves nothing.
	select {
	case err := <-done:
		_ = tx.Rollback()
		t.Fatalf("the second writer completed (%v) while the first held the write lock", err)
	case <-time.After(200 * time.Millisecond):
	}

	require.NoError(t, tx.Commit())

	select {
	case err := <-done:
		assert.NoError(t, err,
			"the second writer failed instead of waiting: busy_timeout is not in effect on "+
				"this connection, so any concurrent process loses its write")
	case <-time.After(6 * time.Second):
		t.Fatal("second writer timed out - busy_timeout not effective")
	}

	val, ok, err := b.KVGet("cross_proc_key")
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, "cross_proc_val", val)
}
