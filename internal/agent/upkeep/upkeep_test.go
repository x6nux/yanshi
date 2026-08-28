package upkeep

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/store"
)

// idleSession creates a session with n messages whose last activity is `age`
// ago, and returns its id.
func idleSession(t *testing.T, s *store.Store, n int, age time.Duration) string {
	t.Helper()
	sid, err := s.CreateSession("idle")
	require.NoError(t, err)
	msgs := make([]store.Message, 0, n)
	for i := range n {
		msgs = append(msgs, store.Message{
			Role:    store.RoleAssistant,
			Content: "turn body number " + string(rune('a'+i%26)),
		})
	}
	_, _, err = s.AppendMessages(sid, msgs)
	require.NoError(t, err)
	// AppendMessages touches updated_at, so backdate it afterwards.
	require.NoError(t, s.WriteTx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.Exec("UPDATE sessions SET updated_at = ? WHERE id = ?",
			time.Now().Add(-age).Unix(), sid)
		return err
	}))
	return sid
}

func openStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "yanshi.db"))
	require.NoError(t, err)
	t.Cleanup(func() { s.Close() })
	return s
}

// TestWorker_RetentionDaysDrivesCompression is the flag→behaviour guard for
// storage.retention_days: the same fixture must be left alone at 0 and packed
// at a real value, and a session younger than the threshold must survive.
//
// Blanking the RetentionDays field in Config, or the cutoff computation in
// compressCold, makes this red.
func TestWorker_RetentionDaysDrivesCompression(t *testing.T) {
	s := openStore(t)
	old := idleSession(t, s, 5, 30*24*time.Hour)
	fresh := idleSession(t, s, 5, time.Hour)

	packedRows := func(sid string) int {
		t.Helper()
		var n int
		require.NoError(t, s.DB.QueryRow(
			"SELECT COUNT(*) FROM messages WHERE session_id = ?", sid).Scan(&n))
		return n
	}

	New(s, Config{RetentionDays: 0}).RunOnce(context.Background())
	require.Equal(t, 5, packedRows(old), "retention 0 must compress nothing")

	New(s, Config{RetentionDays: 7}).RunOnce(context.Background())
	require.Zero(t, packedRows(old), "a 30-day-idle session must be compressed at retention 7")
	require.Equal(t, 5, packedRows(fresh), "a one-hour-idle session must be left alone")

	// The conversation is still readable, which is the property that makes
	// compression acceptable at all.
	msgs, err := s.Messages(old)
	require.NoError(t, err)
	require.Len(t, msgs, 5)
}

// TestWorker_StartStopsOnCancel: the loop must exit on cancellation and Wait
// must observe it. bootstrap.App.Shutdown closes the store right after Wait
// returns, so a Wait that returns early is a use-after-close.
func TestWorker_StartStopsOnCancel(t *testing.T) {
	s := openStore(t)
	w := New(s, Config{Interval: time.Millisecond, RetentionDays: 7})
	ctx, cancel := context.WithCancel(context.Background())
	go w.Start(ctx)
	time.Sleep(10 * time.Millisecond)
	cancel()

	done := make(chan struct{})
	go func() { w.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Wait did not return after cancel")
	}
}

// TestWorker_NilStoreIsANoOp: a boot whose store failed still calls Start and
// Wait. Both must be safe on the nil Worker New returns, or the soft-degrade
// path panics instead of degrading.
func TestWorker_NilStoreIsANoOp(t *testing.T) {
	w := New(nil, Config{RetentionDays: 7})
	require.Nil(t, w)
	w.Start(context.Background())
	w.Wait()
	w.RunOnce(context.Background())
}

// TestWorker_CancelledContextSkipsTheSweep: RunOnce on a dead context must not
// start work. Without the guard a shutdown that races the ticker begins a
// compression the store is about to be closed underneath.
func TestWorker_CancelledContextSkipsTheSweep(t *testing.T) {
	s := openStore(t)
	sid := idleSession(t, s, 4, 30*24*time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	New(s, Config{RetentionDays: 1}).RunOnce(ctx)

	var n int
	require.NoError(t, s.DB.QueryRow(
		"SELECT COUNT(*) FROM messages WHERE session_id = ?", sid).Scan(&n))
	require.Equal(t, 4, n)
}

// storePath returns a path two Store handles can share, standing in for two
// processes.
func storePath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "yanshi.db")
}

// openStoreAt opens an additional handle on an existing path.
func openStoreAt(t *testing.T, path string) *store.Store {
	t.Helper()
	s, err := store.Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { s.Close() })
	return s
}
