package store

import (
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestClaimLease_IsExclusiveAcrossProcesses is the acceptance clause "the same
// session processed by concurrent processes is extracted only once".
//
// TWO Store HANDLES ON ONE FILE, not one handle used twice: a single handle
// serialises every write behind writeMu, so a same-handle test would prove the
// Go mutex works and say nothing about the SQL. Two handles is the shape
// production actually has — a TUI that bootstrapped its own backend, a `serve`,
// and a spawned vcs-mcp all hold the same yanshi.db.
//
// The contenders race in real goroutines. A version that claimed twice in
// sequence, or slept to order them, would pass against a read-then-write
// implementation that is atomic in one process and broken across two.
func TestClaimLease_IsExclusiveAcrossProcesses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "yanshi.db")
	a, err := Open(path)
	require.NoError(t, err)
	defer a.Close()
	b, err := Open(path)
	require.NoError(t, err)
	defer b.Close()

	const contenders = 8
	var wg sync.WaitGroup
	start := make(chan struct{})
	won := make(chan bool, contenders)
	for i := range contenders {
		s := a
		if i%2 == 1 {
			s = b
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			ok, err := s.ClaimLease("session-x", time.Minute)
			if err == nil {
				won <- ok
			}
		}()
	}
	close(start)
	wg.Wait()
	close(won)

	var winners int
	for ok := range won {
		if ok {
			winners++
		}
	}
	require.Equal(t, 1, winners,
		"exactly one contender may hold the lease; got %d", winners)
}

// TestClaimLease_ExpiryReleasesIt: a holder that dies must not wedge the work
// forever. A lease is never explicitly released, so expiry is the only path
// back.
func TestClaimLease_ExpiryReleasesIt(t *testing.T) {
	s := openTestStore(t)
	won, err := s.ClaimLease("job", time.Second)
	require.NoError(t, err)
	require.True(t, won)

	won, err = s.ClaimLease("job", time.Second)
	require.NoError(t, err)
	require.False(t, won, "a live lease must not be re-claimable")

	// Backdate the stored expiry rather than sleeping: the claim compares
	// against wall-clock seconds, and a test that slept would be slow and
	// flaky at exactly one second of resolution.
	require.NoError(t, s.KVSet(leasePrefix+"job", "1"))
	won, err = s.ClaimLease("job", time.Second)
	require.NoError(t, err)
	require.True(t, won, "an expired lease must be re-claimable")
}

// TestRetireLease_IsPermanent: retiring means "this work is done, never repeat
// it". Without it the sweep re-extracts every session on every tick.
func TestRetireLease_IsPermanent(t *testing.T) {
	s := openTestStore(t)
	require.NoError(t, s.RetireLease("job"))

	won, err := s.ClaimLease("job", time.Hour)
	require.NoError(t, err)
	require.False(t, won)

	until, ok, err := s.LeaseHeldUntil("job")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, LeaseRetired, until)
}

// TestLeaseHeldUntil_UnknownAndUnparseable: an unknown lease reports absent (so
// the caller claims it), and a corrupted scalar reports "held until 0" (so the
// caller claims it too). Neither may be an error — refusing to work because one
// kv value got mangled would wedge the job permanently.
func TestLeaseHeldUntil_UnknownAndUnparseable(t *testing.T) {
	s := openTestStore(t)
	_, ok, err := s.LeaseHeldUntil("never-claimed")
	require.NoError(t, err)
	require.False(t, ok)

	require.NoError(t, s.KVSet(leasePrefix+"mangled", "not-a-number"))
	until, ok, err := s.LeaseHeldUntil("mangled")
	require.NoError(t, err)
	require.True(t, ok)
	require.Zero(t, until)

	won, err := s.ClaimLease("mangled", time.Minute)
	require.NoError(t, err)
	require.True(t, won, "a mangled expiry must not lock the work out")
}

// TestClaimLease_RejectsBadInput keeps a caller from silently claiming a lease
// named "" (which every caller would then share) or one that expires in the
// past (which is not a claim at all).
func TestClaimLease_RejectsBadInput(t *testing.T) {
	s := openTestStore(t)
	_, err := s.ClaimLease("", time.Minute)
	require.Error(t, err)
	_, err = s.ClaimLease("job", 0)
	require.Error(t, err)
	require.Error(t, s.RetireLease(""))
}
