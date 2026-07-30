package task

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestBroker_Claim_StoreListPendingError covers the error return at
// broker.go:93-95 when store.ListPending fails.
func TestBroker_Claim_StoreListPendingError(t *testing.T) {
	b, s := newTestBroker(t, 2, 5*time.Second)

	// Close the underlying database so ListPending fails.
	require.NoError(t, s.DB.Close())

	_, err := b.Claim("w1")
	assert.Error(t, err)
}

// TestBroker_Submit_StoreCreateError covers the error return at
// broker.go:73-76 when store.CreateTask fails.
func TestBroker_Submit_StoreCreateError(t *testing.T) {
	b, s := newTestBroker(t, 2, 5*time.Second)

	require.NoError(t, s.DB.Close())

	_, err := b.Submit("echo", "in", "")
	assert.Error(t, err)
}

// TestBroker_RequeueStale_ListError covers the error return at
// broker.go:230-231 when store.ListStaleRunning fails.
func TestBroker_RequeueStale_ListError(t *testing.T) {
	b, s := newTestBroker(t, 2, 5*time.Second)

	require.NoError(t, s.DB.Close())

	err := b.RequeueStale(context.Background())
	assert.Error(t, err)
}

// TestBroker_ReclaimWorktree_NilVCS covers the nil-VCS branch at
// broker.go:185. When b.VCS is nil, reclaimWorktree must be a safe no-op.
func TestBroker_ReclaimWorktree_NilVCS(t *testing.T) {
	b, _ := newTestBroker(t, 2, 5*time.Second)

	// b.VCS is nil — reclaimWorktree must not panic or error.
	b.reclaimWorktree("any-id")     // no createdWT entry → no-op
	b.reclaimWorktree("another-id") // idempotent
}

// TestBroker_ReclaimWorktree_VCSNoCreatedEntry verifies that reclaimWorktree
// is a safe no-op when VCS is set but the id is not in createdWT.
func TestBroker_ReclaimWorktree_VCSNoCreatedEntry(t *testing.T) {
	b, _, _, _, _ := newTestBrokerWithVCS(t, 2, 5*time.Second)

	// With VCS set but no createdWT entry, RemoveWorktree is never called.
	b.reclaimWorktree("never-claimed")
	b.reclaimWorktree("also-never")
}

// TestBroker_RecordResult_AlreadyTerminal covers the ErrNotOwner path
// when GetTask succeeds but status is not "running". This exercises the
// same code path as TestBroker_RecordResultNotRunning but verifies that
// a second call after completion (status=completed, not running) also
// returns ErrNotOwner.
func TestBroker_RecordResult_AlreadyTerminal(t *testing.T) {
	b, _ := newTestBroker(t, 2, 5*time.Second)

	id, err := b.Submit("echo", "in", "")
	require.NoError(t, err)

	_, err = b.Claim("w1")
	require.NoError(t, err)

	// Complete the task.
	require.NoError(t, b.RecordResult(id, "w1", "completed", "ok"))

	// Try again — now status is "completed" not "running".
	err = b.RecordResult(id, "w1", "failed", "too late")
	assert.ErrorIs(t, err, ErrNotOwner)
}
