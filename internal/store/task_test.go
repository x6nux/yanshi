package store

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTask_CreateClaimSetResultGet(t *testing.T) {
	s, err := Open(":memory:")
	require.NoError(t, err)
	defer s.Close()

	// Create
	id, err := s.CreateTask("echo", `{"msg":"hello"}`, "")
	require.NoError(t, err)
	assert.NotEmpty(t, id)

	// Get after create
	got, err := s.GetTask(id)
	require.NoError(t, err)
	assert.Equal(t, id, got.ID)
	assert.Equal(t, "echo", got.Type)
	assert.Equal(t, `{"msg":"hello"}`, got.Input)
	assert.Equal(t, "pending", got.Status)
	assert.Equal(t, "", got.AssignedTo)
	assert.Equal(t, "", got.Result)
	assert.Equal(t, "", got.ParentTask)
	assert.Equal(t, int64(0), got.Attempts)

	// Claim
	require.NoError(t, s.ClaimTask(id, "worker-1"))
	got, err = s.GetTask(id)
	require.NoError(t, err)
	assert.Equal(t, "running", got.Status)
	assert.Equal(t, "worker-1", got.AssignedTo)

	// Set result
	require.NoError(t, s.SetTaskResult(id, "completed", `{"reply":"hello"}`))
	got, err = s.GetTask(id)
	require.NoError(t, err)
	assert.Equal(t, "completed", got.Status)
	assert.Equal(t, `{"reply":"hello"}`, got.Result)
}

func TestTask_ListPending(t *testing.T) {
	s, err := Open(":memory:")
	require.NoError(t, err)
	defer s.Close()

	id1, err := s.CreateTask("echo", "in1", "")
	require.NoError(t, err)
	_, err = s.CreateTask("echo", "in2", "")
	require.NoError(t, err)
	_, err = s.CreateTask("build", "in3", "")
	require.NoError(t, err)

	// Claim one — it should no longer be pending
	require.NoError(t, s.ClaimTask(id1, "w1"))

	pending, err := s.ListPending(10)
	require.NoError(t, err)
	require.Len(t, pending, 2)
	for _, p := range pending {
		assert.Equal(t, "pending", p.Status)
		assert.NotEqual(t, id1, p.ID)
	}
}

func TestTask_ListPendingLimit(t *testing.T) {
	s, err := Open(":memory:")
	require.NoError(t, err)
	defer s.Close()

	for i := 0; i < 5; i++ {
		_, err := s.CreateTask("echo", "in", "")
		require.NoError(t, err)
	}

	pending, err := s.ListPending(2)
	require.NoError(t, err)
	assert.Len(t, pending, 2)
}

func TestTask_ClaimAlreadyClaimed(t *testing.T) {
	s, err := Open(":memory:")
	require.NoError(t, err)
	defer s.Close()

	id, err := s.CreateTask("echo", "in", "")
	require.NoError(t, err)

	require.NoError(t, s.ClaimTask(id, "w1"))
	// Second claim on a non-pending task should fail (rows affected = 0)
	err = s.ClaimTask(id, "w2")
	assert.Error(t, err)
}

func TestTask_GetMissing(t *testing.T) {
	s, err := Open(":memory:")
	require.NoError(t, err)
	defer s.Close()

	_, err = s.GetTask("nonexistent")
	assert.Error(t, err)
}

func TestTask_ParentTask(t *testing.T) {
	s, err := Open(":memory:")
	require.NoError(t, err)
	defer s.Close()

	parentID, err := s.CreateTask("orchestrator", "plan", "")
	require.NoError(t, err)

	childID, err := s.CreateTask("echo", "sub", parentID)
	require.NoError(t, err)

	got, err := s.GetTask(childID)
	require.NoError(t, err)
	assert.Equal(t, parentID, got.ParentTask)
}

// TestTask_TouchTask bumps updated_at on a claimed (running) task.
func TestTask_TouchTask(t *testing.T) {
	s, err := Open(":memory:")
	require.NoError(t, err)
	defer s.Close()

	id, err := s.CreateTask("t", "in", "")
	require.NoError(t, err)
	require.NoError(t, s.ClaimTask(id, "w1"))
	before, err := s.GetTask(id)
	require.NoError(t, err)

	require.NoError(t, s.TouchTask(id))
	after, err := s.GetTask(id)
	require.NoError(t, err)
	assert.Greater(t, after.UpdatedAt, before.UpdatedAt-1, // -1 tolerates same-second
		"TouchTask must advance updated_at")
}

// TestTask_ListStaleRunning proves a running task older than the cutoff is
// reported stale; a fresh one is not.
func TestTask_ListStaleRunning(t *testing.T) {
	s, err := Open(":memory:")
	require.NoError(t, err)
	defer s.Close()

	id, err := s.CreateTask("t", "in", "")
	require.NoError(t, err)
	require.NoError(t, s.ClaimTask(id, "w1"))
	got, err := s.GetTask(id)
	require.NoError(t, err)

	// cutoff in the future → this task IS stale.
	stale, err := s.ListStaleRunning(got.UpdatedAt + 100)
	require.NoError(t, err)
	require.Len(t, stale, 1)
	assert.Equal(t, id, stale[0].ID)

	// cutoff in the past → not stale.
	stale, err = s.ListStaleRunning(got.UpdatedAt - 100)
	require.NoError(t, err)
	assert.Empty(t, stale)
}

// TestTask_RequeueTask_OwnerGuard proves RequeueTask only succeeds for the
// owning worker; a different worker is rejected (rows affected = 0 → error).
func TestTask_RequeueTask_OwnerGuard(t *testing.T) {
	s, err := Open(":memory:")
	require.NoError(t, err)
	defer s.Close()

	id, err := s.CreateTask("t", "in", "")
	require.NoError(t, err)
	require.NoError(t, s.ClaimTask(id, "w1"))

	// Wrong owner → error, task stays running for w1.
	err = s.RequeueTask(id, "w2")
	require.Error(t, err)
	got, err := s.GetTask(id)
	require.NoError(t, err)
	assert.Equal(t, "running", got.Status)
	assert.Equal(t, "w1", got.AssignedTo)

	// Right owner → ok, back to pending, attempts++.
	require.NoError(t, s.RequeueTask(id, "w1"))
	got, err = s.GetTask(id)
	require.NoError(t, err)
	assert.Equal(t, "pending", got.Status)
	assert.Equal(t, int64(1), got.Attempts)
}

// TestTask_FinalizeTask_Failed proves FinalizeTask with status="failed"
// persists and is terminal (RequeueTask afterwards is rejected because the
// row is no longer running).
func TestTask_FinalizeTask_Failed(t *testing.T) {
	s, err := Open(":memory:")
	require.NoError(t, err)
	defer s.Close()

	id, err := s.CreateTask("t", "in", "")
	require.NoError(t, err)
	require.NoError(t, s.ClaimTask(id, "w1"))

	require.NoError(t, s.FinalizeTask(id, "w1", "failed", "boom"))
	got, err := s.GetTask(id)
	require.NoError(t, err)
	assert.Equal(t, "failed", got.Status)
	assert.Equal(t, "boom", got.Result)

	// Terminal → requeue rejected.
	err = s.RequeueTask(id, "w1")
	assert.Error(t, err)
}

// TestTask_IncrementAttempts proves the counter increments and does not depend
// on task status (it works on a fresh pending task).
func TestTask_IncrementAttempts(t *testing.T) {
	s, err := Open(":memory:")
	require.NoError(t, err)
	defer s.Close()

	id, err := s.CreateTask("t", "in", "")
	require.NoError(t, err)

	require.NoError(t, s.IncrementAttempts(id))
	require.NoError(t, s.IncrementAttempts(id))
	got, err := s.GetTask(id)
	require.NoError(t, err)
	assert.Equal(t, int64(2), got.Attempts)
}
// TestTask_WorktreeID_RoundTrip verifies that a freshly created task has an
// empty worktree_id (column default), that SetTaskWorktree persists a value
// readable back via GetTask, and that ListPending surfaces it too — so a
// planner pre-assigning shared worktree ids and a broker reading pending tasks
// both observe the same column.
func TestTask_WorktreeID_RoundTrip(t *testing.T) {
	s, err := Open(":memory:")
	require.NoError(t, err)
	defer s.Close()

	id, err := s.CreateTask("echo", "in", "")
	require.NoError(t, err)

	// Default: empty worktree_id.
	got, err := s.GetTask(id)
	require.NoError(t, err)
	assert.Equal(t, "", got.WorktreeID, "fresh task has no worktree assigned")

	// Stamp a shared worktree id.
	require.NoError(t, s.SetTaskWorktree(id, "shared-wt"))

	got, err = s.GetTask(id)
	require.NoError(t, err)
	assert.Equal(t, "shared-wt", got.WorktreeID)

	// ListPending surfaces the same id.
	pending, err := s.ListPending(10)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	assert.Equal(t, "shared-wt", pending[0].WorktreeID)

	// SetTaskWorktree on a missing task errors.
	err = s.SetTaskWorktree("nonexistent", "wt-x")
	assert.Error(t, err)
}

// TestRequeueStaleTask_RespectsCutoff verifies that RequeueStaleTask only
// requeues a running task when its updated_at is still older than the cutoff
// timestamp — closing the TOCTOU window where a heartbeat bumps updated_at
// after the stale snapshot was taken.
func TestRequeueStaleTask_RespectsCutoff(t *testing.T) {
	s, err := Open(":memory:")
	require.NoError(t, err)
	defer s.Close()

	id, err := s.CreateTask("echo", "in", "")
	require.NoError(t, err)
	require.NoError(t, s.ClaimTask(id, "w1"))

	got, err := s.GetTask(id)
	require.NoError(t, err)
	now := got.UpdatedAt // task is running with updated_at = now

	// cutoff BEFORE updated_at (task is NOT stale per cutoff) → changed=false
	changed, err := s.RequeueStaleTask(id, now-1, 3)
	require.NoError(t, err)
	assert.False(t, changed, "task should not be requeued when updated_at >= cutoff")

	got, err = s.GetTask(id)
	require.NoError(t, err)
	assert.Equal(t, "running", got.Status, "task must remain running")
	assert.Equal(t, int64(0), got.Attempts, "attempts must not change")

	// cutoff AFTER updated_at (task IS stale per cutoff) → changed=true
	changed, err = s.RequeueStaleTask(id, now+100, 3)
	require.NoError(t, err)
	assert.True(t, changed, "task should be requeued when updated_at < cutoff")

	got, err = s.GetTask(id)
	require.NoError(t, err)
	assert.Equal(t, "pending", got.Status, "task should be pending after requeue")
	assert.Equal(t, int64(1), got.Attempts, "attempts should be incremented")
}

// TestStoreCancelTask_GuardedUpdate verifies CancelTask succeeds for pending
// and running rows (RowsAffected==1) and fails for terminal/missing rows
// (RowsAffected==0 → error). The guarded WHERE clause is what allows
// broker.Cancel to be a thin delegate without re-checking task status.
func TestStoreCancelTask_GuardedUpdate(t *testing.T) {
	s, err := Open(":memory:")
	require.NoError(t, err)
	defer s.Close()

	// pending → cancellable
	pid, err := s.CreateTask("t", "in", "")
	require.NoError(t, err)
	require.NoError(t, s.CancelTask(pid))
	got, err := s.GetTask(pid)
	require.NoError(t, err)
	assert.Equal(t, "cancelled", got.Status)

	// running → cancellable
	rid, err := s.CreateTask("t", "in", "")
	require.NoError(t, err)
	require.NoError(t, s.ClaimTask(rid, "w1"))
	require.NoError(t, s.CancelTask(rid))
	got, err = s.GetTask(rid)
	require.NoError(t, err)
	assert.Equal(t, "cancelled", got.Status)

	// already-cancelled → error
	require.Error(t, s.CancelTask(pid))

	// completed → error
	cid, err := s.CreateTask("t", "in", "")
	require.NoError(t, err)
	require.NoError(t, s.ClaimTask(cid, "w1"))
	require.NoError(t, s.FinalizeTask(cid, "w1", "completed", ""))
	require.Error(t, s.CancelTask(cid))

	// missing → error
	require.Error(t, s.CancelTask("nope-not-an-id"))
}
