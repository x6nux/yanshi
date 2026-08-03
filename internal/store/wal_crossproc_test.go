package store

import (
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
func TestCrossProcessBusyTimeout_DualOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "yanshi.db")
	a, err := Open(path)
	require.NoError(t, err)
	defer a.Close()

	b, err := Open(path)
	require.NoError(t, err)
	defer b.Close()

	// Lock a's writeMu to simulate a long write in progress.
	a.writeMu.Lock()
	done := make(chan error, 1)
	go func() {
		done <- b.KVSet("cross_proc_key", "cross_proc_val")
	}()
	time.Sleep(50 * time.Millisecond) // let b start waiting on writeMu
	a.writeMu.Unlock()                // release so b can acquire it
	select {
	case err := <-done:
		assert.NoError(t, err, "second writer must succeed within busy_timeout")
	case <-time.After(6 * time.Second):
		t.Fatal("second writer timed out - busy_timeout not effective")
	}

	// Verify the value was actually written
	val, ok, err := b.KVGet("cross_proc_key")
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, "cross_proc_val", val)
}
