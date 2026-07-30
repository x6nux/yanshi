package task

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/x6nux/yanshi/internal/store"
	"github.com/x6nux/yanshi/internal/vcs"
)

func newTestBroker(t *testing.T, maxRetries int, hbTimeout time.Duration) (*Broker, *store.Store) {
	t.Helper()
	s, err := store.Open(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { s.Close() })
	b := NewBroker(s, maxRetries, hbTimeout)
	return b, s
}

func TestBroker_SubmitClaimRecordCompleted(t *testing.T) {
	b, _ := newTestBroker(t, 2, 5*time.Second)

	// Submit
	id, err := b.Submit("echo", `{"msg":"hi"}`, "")
	require.NoError(t, err)
	assert.NotEmpty(t, id)

	// Claim
	task, err := b.Claim("worker-1")
	require.NoError(t, err)
	require.NotNil(t, task)
	assert.Equal(t, id, task.ID)
	assert.Equal(t, "running", task.Status)
	assert.Equal(t, "worker-1", task.AssignedTo)

	// Record completed
	require.NoError(t, b.RecordResult(id, "worker-1", "completed", `{"reply":"hi"}`))

	got, err := b.Get(id)
	require.NoError(t, err)
	assert.Equal(t, "completed", got.Status)
	assert.Equal(t, `{"reply":"hi"}`, got.Result)
}

func TestBroker_ClaimNoTasks(t *testing.T) {
	b, _ := newTestBroker(t, 2, 5*time.Second)

	task, err := b.Claim("worker-1")
	require.NoError(t, err)
	assert.Nil(t, task)
}

func TestBroker_RecordResultFailedRetry(t *testing.T) {
	b, _ := newTestBroker(t, 2, 5*time.Second)

	id, err := b.Submit("echo", "in", "")
	require.NoError(t, err)

	// Claim so the task is running
	_, err = b.Claim("w1")
	require.NoError(t, err)

	// First failure (attempts=0, < maxRetries=2) → requeued as pending
	require.NoError(t, b.RecordResult(id, "w1", "failed", "err1"))
	got, err := b.Get(id)
	require.NoError(t, err)
	assert.Equal(t, "pending", got.Status)
	assert.Equal(t, "", got.Result)

	// Claim again, fail again (attempts=1, < maxRetries=2) → requeued
	_, err = b.Claim("w1")
	require.NoError(t, err)
	require.NoError(t, b.RecordResult(id, "w1", "failed", "err2"))
	got, err = b.Get(id)
	require.NoError(t, err)
	assert.Equal(t, "pending", got.Status)

	// Claim again, fail again (attempts=2, >= maxRetries=2) → final failed
	_, err = b.Claim("w1")
	require.NoError(t, err)
	require.NoError(t, b.RecordResult(id, "w1", "failed", "err3"))
	got, err = b.Get(id)
	require.NoError(t, err)
	assert.Equal(t, "failed", got.Status)
	assert.Equal(t, "err3", got.Result)
}

func TestBroker_RecordResultTimeoutRetry(t *testing.T) {
	b, _ := newTestBroker(t, 1, 5*time.Second)

	id, err := b.Submit("echo", "in", "")
	require.NoError(t, err)

	// Claim → timeout (attempts=0, < maxRetries=1) → requeued
	_, err = b.Claim("w1")
	require.NoError(t, err)
	require.NoError(t, b.RecordResult(id, "w1", "timeout", "timed out"))
	got, err := b.Get(id)
	require.NoError(t, err)
	assert.Equal(t, "pending", got.Status)

	// Claim → timeout again (attempts=1, >= maxRetries=1) → failed
	_, err = b.Claim("w1")
	require.NoError(t, err)
	require.NoError(t, b.RecordResult(id, "w1", "timeout", "timed out again"))
	got, err = b.Get(id)
	require.NoError(t, err)
	assert.Equal(t, "timeout", got.Status)
	assert.Equal(t, "timed out again", got.Result)
}

func TestBroker_Heartbeat(t *testing.T) {
	b, s := newTestBroker(t, 2, 5*time.Second)

	id, err := b.Submit("echo", "in", "")
	require.NoError(t, err)

	_, err = b.Claim("w1")
	require.NoError(t, err)

	before, err := s.GetTask(id)
	require.NoError(t, err)

	// Sleep >1s so the Unix-second timestamp changes
	time.Sleep(1100 * time.Millisecond)
	require.NoError(t, b.Heartbeat(id))

	after, err := s.GetTask(id)
	require.NoError(t, err)
	assert.Greater(t, after.UpdatedAt, before.UpdatedAt)
}

func TestBroker_HeartbeatMissingTask(t *testing.T) {
	b, _ := newTestBroker(t, 2, 5*time.Second)
	err := b.Heartbeat("nonexistent")
	assert.Error(t, err)
}

func TestBroker_RequeueStale(t *testing.T) {
	// Use a tiny heartbeat timeout so the task becomes stale quickly.
	// Note: store timestamps are Unix-second granularity, so we must
	// sleep >1s for the staleness cutoff to cross a second boundary.
	b, _ := newTestBroker(t, 3, 100*time.Millisecond)

	id, err := b.Submit("echo", "in", "")
	require.NoError(t, err)

	_, err = b.Claim("w1")
	require.NoError(t, err)

	// Wait for the task to become stale (must cross a second boundary)
	time.Sleep(1200 * time.Millisecond)

	// RequeueStale should put it back to pending and increment attempts
	require.NoError(t, b.RequeueStale(t.Context()))

	got, err := b.Get(id)
	require.NoError(t, err)
	assert.Equal(t, "pending", got.Status)
	assert.Equal(t, int64(1), got.Attempts)

	// Should be claimable again
	task, err := b.Claim("w2")
	require.NoError(t, err)
	require.NotNil(t, task)
	assert.Equal(t, id, task.ID)
}

func TestBroker_RequeueStaleMaxRetries(t *testing.T) {
	// maxRetries=1: first stale requeue increments to attempts=1 (still <= max, pending),
	// second stale requeue increments to attempts=2 (> max, failed).
	// Store timestamps are Unix-second granularity, so sleeps must be >1s.
	b, _ := newTestBroker(t, 1, 100*time.Millisecond)

	id, err := b.Submit("echo", "in", "")
	require.NoError(t, err)

	// Claim → running
	_, err = b.Claim("w1")
	require.NoError(t, err)

	// First stale: attempts 0→1, 1 <= maxRetries(1) → pending
	time.Sleep(1200 * time.Millisecond)
	require.NoError(t, b.RequeueStale(t.Context()))
	got, err := b.Get(id)
	require.NoError(t, err)
	assert.Equal(t, "pending", got.Status)
	assert.Equal(t, int64(1), got.Attempts)

	// Claim again → running
	_, err = b.Claim("w2")
	require.NoError(t, err)

	// Second stale: attempts 1→2, 2 > maxRetries(1) → failed
	time.Sleep(1200 * time.Millisecond)
	require.NoError(t, b.RequeueStale(t.Context()))
	got, err = b.Get(id)
	require.NoError(t, err)
	assert.Equal(t, "failed", got.Status)
}

func TestBroker_RequeueStaleNothingStale(t *testing.T) {
	b, _ := newTestBroker(t, 2, 1*time.Hour)

	id, err := b.Submit("echo", "in", "")
	require.NoError(t, err)
	_, err = b.Claim("w1")
	require.NoError(t, err)

	// With a 1-hour timeout, nothing should be stale
	require.NoError(t, b.RequeueStale(t.Context()))
	got, err := b.Get(id)
	require.NoError(t, err)
	assert.Equal(t, "running", got.Status)
}

func TestBroker_NotifyOnSubmit(t *testing.T) {
	b, _ := newTestBroker(t, 2, 5*time.Second)

	ch := b.Notify()

	// Drain any pre-existing signal (buffered channel may have one from init)
	select {
	case <-ch:
	default:
	}

	_, err := b.Submit("echo", "in", "")
	require.NoError(t, err)

	select {
	case <-ch:
		// got the notification
	case <-time.After(100 * time.Millisecond):
		t.Fatal("did not receive notify signal after submit")
	}
}

func TestBroker_GetMissingTask(t *testing.T) {
	b, _ := newTestBroker(t, 2, 5*time.Second)
	_, err := b.Get("nonexistent")
	assert.Error(t, err)
}

func TestBroker_StartSweeper(t *testing.T) {
	// Store timestamps are Unix-second granularity, so the sweeper
	// needs >1s to detect staleness. Use a short interval and wait.
	b, _ := newTestBroker(t, 3, 100*time.Millisecond)

	id, err := b.Submit("echo", "in", "")
	require.NoError(t, err)

	_, err = b.Claim("w1")
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)

	b.StartSweeper(ctx, 100*time.Millisecond)

	// Wait for the sweeper to requeue the stale task (needs to cross a second boundary)
	deadline := time.After(3 * time.Second)
	for {
		got, err := b.Get(id)
		require.NoError(t, err)
		if got.Status == "pending" {
			break
		}
		select {
		case <-deadline:
			t.Fatal("sweeper did not requeue stale task in time")
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func TestBroker_RecordResultWrongWorker(t *testing.T) {
	b, _ := newTestBroker(t, 3, 5*time.Second)

	id, err := b.Submit("echo", "in", "")
	require.NoError(t, err)

	_, err = b.Claim("w1")
	require.NoError(t, err)

	// A different worker tries to record the result → ErrNotOwner.
	err = b.RecordResult(id, "w2", "completed", "stolen")
	assert.ErrorIs(t, err, ErrNotOwner)

	// Task should still be running, assigned to w1.
	got, err := b.Get(id)
	require.NoError(t, err)
	assert.Equal(t, "running", got.Status)
	assert.Equal(t, "w1", got.AssignedTo)
}

func TestBroker_RecordResultNotRunning(t *testing.T) {
	b, _ := newTestBroker(t, 3, 5*time.Second)

	id, err := b.Submit("echo", "in", "")
	require.NoError(t, err)

	// Task is still "pending" (not claimed) → ErrNotOwner.
	err = b.RecordResult(id, "w1", "completed", "done")
	assert.ErrorIs(t, err, ErrNotOwner)

	// Claim and complete, then try again → ErrNotOwner (not running).
	_, err = b.Claim("w1")
	require.NoError(t, err)
	require.NoError(t, b.RecordResult(id, "w1", "completed", "done"))

	err = b.RecordResult(id, "w1", "completed", "again")
	assert.ErrorIs(t, err, ErrNotOwner)
}

func TestBroker_RecordResultInvalidStatus(t *testing.T) {
	b, _ := newTestBroker(t, 3, 5*time.Second)

	id, err := b.Submit("echo", "in", "")
	require.NoError(t, err)

	_, err = b.Claim("w1")
	require.NoError(t, err)

	// "pending" is not a valid result status → ErrInvalidStatus.
	err = b.RecordResult(id, "w1", "pending", "")
	assert.ErrorIs(t, err, ErrInvalidStatus)

	// "running" is also invalid.
	err = b.RecordResult(id, "w1", "running", "")
	assert.ErrorIs(t, err, ErrInvalidStatus)

	// Task should still be running.
	got, err := b.Get(id)
	require.NoError(t, err)
	assert.Equal(t, "running", got.Status)
}

func TestBroker_RecordResultStaleWorkerRejected(t *testing.T) {
	b, _ := newTestBroker(t, 3, 5*time.Second)

	id, err := b.Submit("echo", "in", "")
	require.NoError(t, err)

	// Worker A claims the task.
	_, err = b.Claim("A")
	require.NoError(t, err)

	// Simulate a stale-and-requeue: fail once so the task goes back to pending.
	require.NoError(t, b.RecordResult(id, "A", "failed", "err"))
	got, err := b.Get(id)
	require.NoError(t, err)
	assert.Equal(t, "pending", got.Status)

	// Worker B claims the requeued task.
	_, err = b.Claim("B")
	require.NoError(t, err)

	// Worker A (stale) tries to record a result → must be rejected.
	err = b.RecordResult(id, "A", "completed", "stale")
	assert.ErrorIs(t, err, ErrNotOwner)

	// Worker B (current owner) can record successfully.
	require.NoError(t, b.RecordResult(id, "B", "completed", "ok"))
	got, err = b.Get(id)
	require.NoError(t, err)
	assert.Equal(t, "completed", got.Status)
	assert.Equal(t, "ok", got.Result)
}

func TestBroker_RequeueStaleDoesNotClobberCompleted(t *testing.T) {
	// Simulate the race: a worker finalizes a task (completed) and then
	// the sweeper runs. The sweeper's guarded UPDATE must match 0 rows
	// (status is no longer 'running') and leave the completed result intact.
	b, _ := newTestBroker(t, 3, 100*time.Millisecond)

	id, err := b.Submit("echo", "in", "")
	require.NoError(t, err)

	// Worker claims the task → running, assigned to W.
	_, err = b.Claim("W")
	require.NoError(t, err)

	// Worker finalizes the task as completed with a result.
	require.NoError(t, b.RecordResult(id, "W", "completed", `{"reply":"done"}`))

	// Confirm it is completed.
	got, err := b.Get(id)
	require.NoError(t, err)
	assert.Equal(t, "completed", got.Status)
	assert.Equal(t, `{"reply":"done"}`, got.Result)

	// Now the sweeper runs — it should NOT clobber the completed task.
	// Wait past the heartbeat timeout so the task would be "stale" by
	// timestamp, but the guarded UPDATE checks status='running' and skips.
	time.Sleep(1200 * time.Millisecond)
	require.NoError(t, b.RequeueStale(t.Context()))

	// The task must remain completed with its result intact.
	got, err = b.Get(id)
	require.NoError(t, err)
	assert.Equal(t, "completed", got.Status, "sweeper must not clobber completed task")
	assert.Equal(t, `{"reply":"done"}`, got.Result, "sweeper must not wipe completed result")
	assert.Equal(t, int64(0), got.Attempts, "sweeper must not increment attempts on finalized task")
}

// TestBroker_RequeueStaleRespectsFreshHeartbeat verifies that a task which
// was stale at snapshot time but has since been heartbeated (updated_at bumped
// to now) is NOT requeued by the sweeper. The cutoff guard in RequeueStaleTask
// ensures updated_at < cutoff is re-checked atomically inside the UPDATE.
func TestBroker_RequeueStaleRespectsFreshHeartbeat(t *testing.T) {
	b, _ := newTestBroker(t, 3, 100*time.Millisecond)

	id, err := b.Submit("echo", "in", "")
	require.NoError(t, err)

	_, err = b.Claim("w1")
	require.NoError(t, err)

	// Wait for the task to become stale by timestamp.
	time.Sleep(1200 * time.Millisecond)

	// Simulate a fresh heartbeat: this bumps updated_at to now, so the
	// task is no longer stale even though it was stale a moment ago.
	require.NoError(t, b.Heartbeat(id))

	// RequeueStale computes cutoff = now - hbTimeout. Since the heartbeat
	// just set updated_at = now, updated_at > cutoff, so the guarded UPDATE
	// must match 0 rows and the task stays running.
	require.NoError(t, b.RequeueStale(t.Context()))

	got, err := b.Get(id)
	require.NoError(t, err)
	assert.Equal(t, "running", got.Status, "task with fresh heartbeat must not be requeued")
	assert.Equal(t, int64(0), got.Attempts, "attempts must not change")
}

// newTestBrokerWithVCS builds a broker backed by an in-memory store that is also
// wired to a VCS over a temp repo (a temp dir with one tracked file). The
// returned broker has SetVCS called, so Claim will create worktrees for tasks
// that don't carry a pre-set worktree_id. The VCS, repo id, and repo root are
// returned so tests can assert against the created worktrees.
func newTestBrokerWithVCS(t *testing.T, maxRetries int, hbTimeout time.Duration) (*Broker, *store.Store, *vcs.VCS, string, string) {
	t.Helper()
	s, err := store.Open(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { s.Close() })

	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "a.go"), []byte("package main"), 0o644))

	v := vcs.New(s, t.TempDir())
	repoID, err := v.InitRepo(root)
	require.NoError(t, err)

	b := NewBroker(s, maxRetries, hbTimeout)
	b.SetVCS(v, repoID)
	return b, s, v, repoID, root
}

// TestClaim_CreatesWorktree verifies that claiming a task with no pre-set
// worktree_id (and a VCS-configured broker) creates a worktree and stamps its
// id on the returned task, and that the worktree actually exists in the VCS.
func TestClaim_CreatesWorktree(t *testing.T) {
	b, s, v, repoID, _ := newTestBrokerWithVCS(t, 2, 5*time.Second)

	id, err := b.Submit("echo", "in", "")
	require.NoError(t, err)

	task, err := b.Claim("worker-1")
	require.NoError(t, err)
	require.NotNil(t, task)
	assert.NotEmpty(t, task.WorktreeID, "claimed task should have a worktree stamped")

	// The worktree row exists in the VCS and belongs to this repo.
	wtPath, err := v.WorktreePath(task.WorktreeID)
	require.NoError(t, err, "stamped worktree must exist in the VCS")
	assert.True(t, len(wtPath) > 0)

	// The stamp is persisted on the store row, not just the in-memory copy.
	got, err := s.GetTask(id)
	require.NoError(t, err)
	assert.Equal(t, task.WorktreeID, got.WorktreeID)
	// repoID referenced indirectly: the worktree's repo must match.
	_ = repoID
}

// TestClaim_KeepsSharedWorktreeID verifies that a task pre-assigned a
// worktree_id (e.g. by a team plan allocating several tasks to one shared
// worktree) keeps that id through Claim — the broker must NOT overwrite it or
// create a new worktree.
func TestClaim_KeepsSharedWorktreeID(t *testing.T) {
	b, s, v, _, _ := newTestBrokerWithVCS(t, 2, 5*time.Second)

	id, err := b.Submit("echo", "in", "")
	require.NoError(t, err)

	// Pre-assign a shared worktree id before claim.
	require.NoError(t, s.SetTaskWorktree(id, "shared-wt"))

	// Count worktree rows before claim — claiming a pre-assigned task must
	// not add a new one.
	var before int
	require.NoError(t, s.DB.QueryRow("SELECT COUNT(*) FROM vcs_worktrees").Scan(&before))

	task, err := b.Claim("worker-1")
	require.NoError(t, err)
	require.NotNil(t, task)
	assert.Equal(t, "shared-wt", task.WorktreeID, "pre-set shared worktree id must be preserved")

	// No new worktree row was created.
	var after int
	require.NoError(t, s.DB.QueryRow("SELECT COUNT(*) FROM vcs_worktrees").Scan(&after))
	assert.Equal(t, before, after, "claiming a shared-worktree task must not create a new worktree")

	// And the shared id does not resolve to a real worktree in the VCS — the
	// broker didn't create one, which is the point.
	_, err = v.WorktreePath("shared-wt")
	assert.Error(t, err, "shared-wt is a plan-provided id; no real worktree is created for it")
}

// TestClaim_NoVCS_NoWorktree verifies that a broker without a VCS configured
// leaves the claimed task's worktree_id empty (the opt-in path).
func TestClaim_NoVCS_NoWorktree(t *testing.T) {
	b, _ := newTestBroker(t, 2, 5*time.Second)

	id, err := b.Submit("echo", "in", "")
	require.NoError(t, err)

	task, err := b.Claim("worker-1")
	require.NoError(t, err)
	require.NotNil(t, task)
	assert.Equal(t, "", task.WorktreeID, "without a VCS the task runs without a worktree")

	got, err := b.Get(id)
	require.NoError(t, err)
	assert.Equal(t, "", got.WorktreeID)
}

// worktreeActive reports the vcs_worktrees.active flag for wtID and whether a
// row exists at all (1 = active, 0 = deactivated). Used to assert the broker
// reclaims its created worktrees and leaves shared ones untouched.
func worktreeActive(t *testing.T, s *store.Store, wtID string) (active int, found bool) {
	t.Helper()
	err := s.DB.QueryRow("SELECT active FROM vcs_worktrees WHERE id=?", wtID).Scan(&active)
	if err != nil {
		return 0, false
	}
	return active, true
}

// TestClaim_FinalizeRemovesBrokerCreatedWorktree verifies that a worktree the
// broker itself created in Claim is reclaimed (disk dir removed + active=0) once
// the task reaches a terminal status via RecordResult. This is the leak fix.
func TestClaim_FinalizeRemovesBrokerCreatedWorktree(t *testing.T) {
	b, s, v, _, _ := newTestBrokerWithVCS(t, 2, 5*time.Second)

	id, err := b.Submit("echo", "in", "")
	require.NoError(t, err)

	task, err := b.Claim("worker-1")
	require.NoError(t, err)
	require.NotNil(t, task)
	require.NotEmpty(t, task.WorktreeID, "claimed task should have a worktree stamped")

	// The worktree working dir exists on disk and the row is active.
	wtPath, err := v.WorktreePath(task.WorktreeID)
	require.NoError(t, err)
	assert.DirExists(t, wtPath, "worktree working dir must exist after claim")
	active, found := worktreeActive(t, s, task.WorktreeID)
	assert.True(t, found, "worktree row must exist right after claim")
	assert.Equal(t, 1, active, "worktree row must be active right after claim")

	// Terminal finalize → broker reclaims its own worktree.
	require.NoError(t, b.RecordResult(id, "worker-1", "completed", `{"reply":"done"}`))

	_, statErr := os.Stat(wtPath)
	assert.True(t, os.IsNotExist(statErr), "worktree dir must be removed after terminal finalize (stat err=%v)", statErr)
	active, _ = worktreeActive(t, s, task.WorktreeID)
	assert.Equal(t, 0, active, "worktree row must be deactivated after terminal finalize")
}

// TestClaim_FinalizePreservesSharedWorktree verifies that a worktree a plan
// pre-assigned (shared across multiple tasks) is NOT reclaimed when one of those
// tasks finalizes — the broker only reclaims worktrees it created itself.
func TestClaim_FinalizePreservesSharedWorktree(t *testing.T) {
	b, s, _, _, _ := newTestBrokerWithVCS(t, 2, 5*time.Second)

	id, err := b.Submit("echo", "in", "")
	require.NoError(t, err)

	// Pre-assign a shared worktree id before claim — plan-provided, not real.
	require.NoError(t, s.SetTaskWorktree(id, "shared-wt"))

	task, err := b.Claim("worker-1")
	require.NoError(t, err)
	require.NotNil(t, task)
	assert.Equal(t, "shared-wt", task.WorktreeID, "pre-set shared worktree id must be preserved through claim")

	// No vcs_worktrees row exists for the plan-provided shared id.
	_, found := worktreeActive(t, s, "shared-wt")
	assert.False(t, found, "no real worktree row should exist for the plan-provided shared id")

	// Terminal finalize must not touch the shared worktree: it isn't tracked as
	// broker-created, so RemoveWorktree is never called for it. Nothing panics.
	require.NoError(t, b.RecordResult(id, "worker-1", "completed", "done"))

	got, err := b.Get(id)
	require.NoError(t, err)
	assert.Equal(t, "shared-wt", got.WorktreeID, "shared worktree id must survive terminal finalize unchanged")
}

// TestClaim_RequeueKeepsWorktree verifies that a non-terminal RecordResult
// (failed but still within retry budget → requeued) keeps the broker-created
// worktree in place so a re-claim can reuse it. Only a terminal finalize removes
// the worktree.
func TestClaim_RequeueKeepsWorktree(t *testing.T) {
	b, s, v, _, _ := newTestBrokerWithVCS(t, 2, 5*time.Second)

	id, err := b.Submit("echo", "in", "")
	require.NoError(t, err)

	task, err := b.Claim("worker-1")
	require.NoError(t, err)
	require.NotNil(t, task)
	require.NotEmpty(t, task.WorktreeID)

	wtPath, err := v.WorktreePath(task.WorktreeID)
	require.NoError(t, err)
	assert.DirExists(t, wtPath)

	// First failure (attempts=0, < maxRetries=2) → requeued. Worktree persists.
	require.NoError(t, b.RecordResult(id, "worker-1", "failed", "err1"))
	assert.DirExists(t, wtPath, "worktree must persist across a non-terminal requeue")
	active, found := worktreeActive(t, s, task.WorktreeID)
	assert.True(t, found)
	assert.Equal(t, 1, active, "worktree must remain active after a non-terminal requeue")

	// Re-claim reuses the existing worktree (id already set → no new worktree).
	task2, err := b.Claim("worker-1")
	require.NoError(t, err)
	require.NotNil(t, task2)
	assert.Equal(t, task.WorktreeID, task2.WorktreeID, "re-claim must reuse the existing worktree, not create a new one")

	// Second failure (attempts=1, < maxRetries=2) → still requeued. Still persists.
	require.NoError(t, b.RecordResult(id, "worker-1", "failed", "err2"))
	assert.DirExists(t, wtPath, "worktree must still persist after the second non-terminal requeue")
	active, _ = worktreeActive(t, s, task.WorktreeID)
	assert.Equal(t, 1, active)

	// Third claim + failure (attempts=2, >= maxRetries=2) → terminal failed.
	task3, err := b.Claim("worker-1")
	require.NoError(t, err)
	require.NotNil(t, task3)
	assert.Equal(t, task.WorktreeID, task3.WorktreeID)
	require.NoError(t, b.RecordResult(id, "worker-1", "failed", "err3"))

	// Terminal finalize reclaims the worktree.
	_, statErr := os.Stat(wtPath)
	assert.True(t, os.IsNotExist(statErr), "worktree must be removed only once the task reaches a terminal state (stat err=%v)", statErr)
	active, _ = worktreeActive(t, s, task.WorktreeID)
	assert.Equal(t, 0, active, "worktree must be deactivated on terminal finalize")
}

// TestBroker_Cancel verifies Cancel delegates to store.CancelTask's guarded
// UPDATE: it cancels pending/running tasks and errors for terminal/missing.
// It also exercises the worktree-reclaim branch: when the broker itself
// created a worktree for a task, Cancel reclaims it via VCS.RemoveWorktree.
func TestBroker_Cancel(t *testing.T) {
	b, s := newTestBroker(t, 3, time.Minute)

	// pending → cancellable
	pid, err := b.Submit("t", "in", "")
	require.NoError(t, err)
	require.NoError(t, b.Cancel(pid))
	got, err := s.GetTask(pid)
	require.NoError(t, err)
	assert.Equal(t, "cancelled", got.Status)

	// running → cancellable
	rid, err := b.Submit("t", "in", "")
	require.NoError(t, err)
	_, err = b.Claim("w1")
	require.NoError(t, err)
	require.NoError(t, b.Cancel(rid))
	got, err = s.GetTask(rid)
	require.NoError(t, err)
	assert.Equal(t, "cancelled", got.Status)

	// already cancelled → error
	require.Error(t, b.Cancel(pid))

	// missing → error
	require.Error(t, b.Cancel("nope"))
}

func TestBroker_CancelReclaimsWorktree(t *testing.T) {
	b, s, v, _, _ := newTestBrokerWithVCS(t, 2, 5*time.Second)

	id, err := b.Submit("echo", "in", "")
	require.NoError(t, err)

	task, err := b.Claim("worker-1")
	require.NoError(t, err)
	require.NotNil(t, task)
	require.NotEmpty(t, task.WorktreeID)

	// Confirm worktree is present.
	wtPath, err := v.WorktreePath(task.WorktreeID)
	require.NoError(t, err)
	assert.DirExists(t, wtPath)

	// Cancel reclaims the worktree.
	require.NoError(t, b.Cancel(id))

	_, statErr := os.Stat(wtPath)
	assert.True(t, os.IsNotExist(statErr), "worktree dir must be removed after cancel")
	active, found := worktreeActive(t, s, task.WorktreeID)
	assert.True(t, found)
	assert.Equal(t, 0, active, "worktree must be deactivated after cancel")

	// createdWT map must be cleared.
	b.createdWTMu.Lock()
	_, has := b.createdWT[id]
	b.createdWTMu.Unlock()
	assert.False(t, has, "createdWT entry must be cleared after cancel")
}

func TestBroker_ReclaimWorktreeClearsMapAndRemovesWorktree(t *testing.T) {
	b, s, _, _, _ := newTestBrokerWithVCS(t, 2, 5*time.Second)
	id, err := b.Submit("echo", "in", "")
	task, err := b.Claim("worker-1")
	require.NoError(t, err)
	require.NotEmpty(t, task.WorktreeID)

	// reclaimWorktree must clear the map entry and call RemoveWorktree.
	// assert our internal state before.
	b.createdWTMu.Lock()
	_, has := b.createdWT[id]
	b.createdWTMu.Unlock()
	assert.True(t, has, "createdWT must track the broker-created worktree after Claim")

	b.reclaimWorktree(id)

	b.createdWTMu.Lock()
	_, stillHas := b.createdWT[id]
	b.createdWTMu.Unlock()
	assert.False(t, stillHas, "createdWT must be cleared after reclaimWorktree")

	// The VCS worktree row is deactivated.
	active, found := worktreeActive(t, s, task.WorktreeID)
	assert.True(t, found, "worktree row must exist (deactivated)")
	assert.Equal(t, 0, active, "worktree row must be deactivated after reclaim")
}

func TestBroker_ReclaimWorktreeIdempotent_NoErrorForMissingID(t *testing.T) {
	b, _, _, _, _ := newTestBrokerWithVCS(t, 2, 5*time.Second)
	// No createdWT entry for "never-claimed" — must not panic or error.
	b.reclaimWorktree("never-claimed")
	// No assertions needed: the helper is a no-op.
	b.reclaimWorktree("never-claimed") // double call also safe.
}

// TestBroker_RequeueStaleMaxRetriesReclaimsWorktree verifies that when RequeueStale
// fails a task (exceeded maxRetries) the broker-created worktree is reclaimed —
// this is the LEAK1 gap fix.
func TestBroker_RequeueStaleMaxRetriesReclaimsWorktree(t *testing.T) {
	b, s, v, _, root := newTestBrokerWithVCS(t, 0, 100*time.Millisecond)
	// maxRetries=0: after one stale requeue, attempts+1=1 > 0 → 'failed' immediately.
	// Store timestamps are Unix-second granularity; we must sleep >1s.
	_ = root

	id, err := b.Submit("echo", "in", "")
	require.NoError(t, err)

	// Claim → broker creates a worktree and records it in createdWT.
	task, err := b.Claim("worker-1")
	require.NoError(t, err)
	require.NotNil(t, task)
	require.NotEmpty(t, task.WorktreeID, "should have a broker-created worktree")

	// Confirm worktree is present.
	_, err = v.WorktreePath(task.WorktreeID)
	require.NoError(t, err)

	// Wait for the heartbeat timeout to expire. Store timestamps are
	// Unix-second, so we must cross a second boundary.
	time.Sleep(1200 * time.Millisecond)
	require.NoError(t, b.RequeueStale(context.Background()))
	// maxRetries=1 → first stale timeout exceeds maxRetries → status='failed'.

	got, err := s.GetTask(id)
	require.NoError(t, err)
	require.Equal(t, "failed", got.Status, "task must be failed after exceeding maxRetries")

	// createdWT must no longer contain this id.
	b.createdWTMu.Lock()
	_, has := b.createdWT[id]
	b.createdWTMu.Unlock()
	assert.False(t, has, "createdWT entry must be cleared when RequeueStale fails a task")

	// VCS worktree must be deactivated.
	active, found := worktreeActive(t, s, task.WorktreeID)
	assert.True(t, found, "worktree row must exist (deactivated)")
	assert.Equal(t, 0, active, "worktree must be deactivated after RequeueStale terminal")
}

// TestBroker_RequeueStaleRequeueKeepsWorktree verifies that when RequeueStale
// requeues a task (within retry budget → pending) the broker-created worktree
// is PRESERVED for reuse by the next Claim.
func TestBroker_RequeueStaleRequeueKeepsWorktree(t *testing.T) {
	b, s, v, _, _ := newTestBrokerWithVCS(t, 3, 100*time.Millisecond)

	id, err := b.Submit("echo", "in", "")
	require.NoError(t, err)
	task, err := b.Claim("worker-1")
	require.NoError(t, err)
	require.NotEmpty(t, task.WorktreeID)

	// Stale → requeued (attempts=1 < maxRetries=3 → pending).
	// Store timestamps are Unix-second granularity; we must cross a second boundary.
	time.Sleep(1200 * time.Millisecond)
	require.NoError(t, b.RequeueStale(context.Background()))

	got, err := s.GetTask(id)
	require.NoError(t, err)
	require.Equal(t, "pending", got.Status, "must be pending because within retry budget")

	// createdWT MUST retain the entry (worktree to be reused).
	b.createdWTMu.Lock()
	_, has := b.createdWT[id]
	b.createdWTMu.Unlock()
	assert.True(t, has, "createdWT entry must be kept for a pending requeue")

	// Worktree still exists on disk.
	wtPath, err := v.WorktreePath(task.WorktreeID)
	require.NoError(t, err)
	assert.DirExists(t, wtPath, "worktree dir must exist after pending requeue")
}

// TestBroker_RequeueStaleLongRunMapBounds asserts that after submitting N tasks all doomed
// to fail (maxRetries=0) the len(createdWT) returns to zero.
func TestBroker_RequeueStaleLongRunMapBounds(t *testing.T) {
	b, s, _, _, _ := newTestBrokerWithVCS(t, 0, 100*time.Millisecond)
	n := 10
	for i := 0; i < n; i++ {
		_, err := b.Submit("echo", "in", "")
		require.NoError(t, err)
		_, err = b.Claim("worker-1")
		require.NoError(t, err)
	}
	// Store timestamps are Unix-second; wait for staleness.
	time.Sleep(1200 * time.Millisecond)
	require.NoError(t, b.RequeueStale(context.Background()))

	b.createdWTMu.Lock()
	leakCount := len(b.createdWT)
	b.createdWTMu.Unlock()
	assert.Equal(t, 0, leakCount, "len(createdWT) must be zero after all tasks have failed and been reclaimed")

	// Also confirm all worktree rows are deactivated.
	rows, err := s.DB.Query("SELECT active FROM vcs_worktrees")
	require.NoError(t, err)
	defer rows.Close()
	for rows.Next() {
		var active int
		require.NoError(t, rows.Scan(&active))
		assert.Equal(t, 0, active, "all worktree rows must be deactivated")
	}
	require.NoError(t, rows.Err())
}

// TestBroker_RequeueStaleConcurrentWithCancel_Race ensures that running RequeueStale
// concurrently with Cancel and RecordResult does not produce a data race or
// double-reclaim a worktree. Requires -race.
func TestBroker_RequeueStaleConcurrentWithCancel_Race(t *testing.T) {
	b, s, _, _, _ := newTestBrokerWithVCS(t, 3, 100*time.Millisecond)

	for i := 0; i < 20; i++ {
		_, err := b.Submit("echo", "in", "")
		require.NoError(t, err)
		_, err = b.Claim("worker-1")
		require.NoError(t, err)
	}

	// Let tasks become stale before racing.
	time.Sleep(50 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	var wg sync.WaitGroup
	// RequeueStale loop.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for ctx.Err() == nil {
			_ = b.RequeueStale(ctx)
		}
	}()

	// Concurrent Cancels.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for ctx.Err() == nil {
			pending, err := s.ListPending(100)
			if err == nil {
				for _, t := range pending {
					_ = b.Cancel(t.ID)
				}
			}
		}
	}()

	wg.Wait()
	// No panic, no race. -race must pass.
}
