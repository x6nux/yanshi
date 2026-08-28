package store

import (
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/secrets"
)

// TestMessageQueue_EnqueueToOfflineSession is acceptance clause 1: a message
// can be queued for a session nobody is connected to. Nothing here opens a
// connection or starts a server — the queue is durable state, not a channel.
func TestMessageQueue_EnqueueToOfflineSession(t *testing.T) {
	s := openTestStore(t)
	sid, err := s.CreateSession("offline")
	require.NoError(t, err)

	n, err := s.EnqueueMessage(sid, "run the release script")
	require.NoError(t, err)
	require.Equal(t, 1, n)
	n, err = s.EnqueueMessage(sid, "then open a PR")
	require.NoError(t, err)
	require.Equal(t, 2, n)

	pending, err := s.PendingQueuedMessages(sid)
	require.NoError(t, err)
	require.Equal(t, []string{"run the release script", "then open a PR"}, pending)
}

// TestMessageQueue_VisibleAcrossProcesses is acceptance clause 2.
//
// TWO Store HANDLES ON ONE FILE. Using one handle twice would test the sql.DB
// connection pool, not the database — the whole claim is that a message typed
// by `yanshi enqueue` in one process reaches a session running in another.
func TestMessageQueue_VisibleAcrossProcesses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "yanshi.db")
	producer, err := Open(path)
	require.NoError(t, err)
	defer producer.Close()
	consumer, err := Open(path)
	require.NoError(t, err)
	defer consumer.Close()

	sid, err := producer.CreateSession("shared")
	require.NoError(t, err)
	_, err = producer.EnqueueMessage(sid, "queued from another process")
	require.NoError(t, err)

	got, err := consumer.ConsumeQueuedMessages(sid)
	require.NoError(t, err)
	require.Equal(t, []string{"queued from another process"}, got)
}

// TestMessageQueue_ConsumedOnSessionResume is acceptance clause 3 at the store
// boundary: consuming returns the messages once and leaves the queue empty.
func TestMessageQueue_ConsumedOnSessionResume(t *testing.T) {
	s := openTestStore(t)
	sid, err := s.CreateSession("resumed")
	require.NoError(t, err)
	_, err = s.EnqueueMessage(sid, "first")
	require.NoError(t, err)
	_, err = s.EnqueueMessage(sid, "second")
	require.NoError(t, err)

	got, err := s.ConsumeQueuedMessages(sid)
	require.NoError(t, err)
	require.Equal(t, []string{"first", "second"}, got)

	again, err := s.ConsumeQueuedMessages(sid)
	require.NoError(t, err)
	require.Empty(t, again, "a consumed message must not be delivered twice")
	pending, err := s.PendingQueuedMessages(sid)
	require.NoError(t, err)
	require.Empty(t, pending)
}

// TestMessageQueue_ConcurrentConsumersEachMessageOnce: two windows resuming the
// same session must not both receive the queue. The read and the mark are one
// transaction precisely so this cannot happen.
func TestMessageQueue_ConcurrentConsumersEachMessageOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "yanshi.db")
	a, err := Open(path)
	require.NoError(t, err)
	defer a.Close()
	b, err := Open(path)
	require.NoError(t, err)
	defer b.Close()

	sid, err := a.CreateSession("contended")
	require.NoError(t, err)
	for i := range 10 {
		_, err := a.EnqueueMessage(sid, string(rune('a'+i)))
		require.NoError(t, err)
	}

	var mu sync.Mutex
	var all []string
	var wg sync.WaitGroup
	start := make(chan struct{})
	for _, s := range []*Store{a, b} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			got, err := s.ConsumeQueuedMessages(sid)
			if err == nil {
				mu.Lock()
				all = append(all, got...)
				mu.Unlock()
			}
		}()
	}
	close(start)
	wg.Wait()

	require.Len(t, all, 10, "every message must be delivered exactly once")
	seen := map[string]bool{}
	for _, m := range all {
		require.False(t, seen[m], "message %q delivered twice", m)
		seen[m] = true
	}
}

// TestMessageQueue_RejectsUnknownSession: a mistyped id must fail here rather
// than parking a message in a queue nothing will ever drain.
func TestMessageQueue_RejectsUnknownSession(t *testing.T) {
	s := openTestStore(t)
	_, err := s.EnqueueMessage("no-such-session", "hello")
	require.Error(t, err)
	require.Contains(t, err.Error(), "no session")

	_, err = s.EnqueueMessage("", "hello")
	require.Error(t, err)
	sid, err := s.CreateSession("x")
	require.NoError(t, err)
	_, err = s.EnqueueMessage(sid, "")
	require.Error(t, err)
	_, err = s.ConsumeQueuedMessages("")
	require.Error(t, err)
}

// TestMessageQueue_RedactsContent: a queued message is user text on its way to
// SQLite, so it goes through the same redactor as every other user text. A
// queue that stored secrets verbatim would be a way around S10.
func TestMessageQueue_RedactsContent(t *testing.T) {
	s := openTestStore(t)
	r := secrets.NewRedactor()
	r.Register("sk-live-secret")
	s.SetRedactor(r)
	sid, err := s.CreateSession("secrets")
	require.NoError(t, err)

	_, err = s.EnqueueMessage(sid, "use key sk-live-secret please")
	require.NoError(t, err)
	got, err := s.PendingQueuedMessages(sid)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.NotContains(t, got[0], "sk-live-secret")
}
