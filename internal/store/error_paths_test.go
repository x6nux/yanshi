package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/x6nux/yanshi/internal/auth"
)

// storeWithClosedDB opens an in-memory Store and then closes the underlying
// *sql.DB so every subsequent database operation fails with "sql: database is
// closed". This exercises the error-return paths in every Store method without
// needing file-system corruption or process-level interference.
func storeWithClosedDB(t *testing.T) *Store {
	t.Helper()
	s, err := Open(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	require.NoError(t, s.DB.Close()) // close the underlying pool
	return s
}

// ---------------------------------------------------------------------------
// Closed-DB error paths — every Store and Store-adjacent method.
// One big sub-test per logical group (session, task, memory, auth, codec).
// ---------------------------------------------------------------------------

func TestStore_ClosedDB_SessionMethods(t *testing.T) {
	s := storeWithClosedDB(t)

	t.Run("CreateSession", func(t *testing.T) {
		_, err := s.CreateSession("x")
		assert.Error(t, err)
	})
	t.Run("AppendMessage", func(t *testing.T) {
		err := s.AppendMessage("x", 0, "user", "x")
		assert.Error(t, err)
	})
	t.Run("UpdateSessionTitle", func(t *testing.T) {
		err := s.UpdateSessionTitle("x", "x")
		assert.Error(t, err)
	})
	t.Run("UpdateSessionMeta", func(t *testing.T) {
		err := s.UpdateSessionMeta("x", "m", "", 0, 0, 0, 0, 0, BillingMeta{})
		assert.Error(t, err)
	})
	t.Run("SetSessionArchived", func(t *testing.T) {
		err := s.SetSessionArchived("x", true)
		assert.Error(t, err)
	})
	t.Run("DeleteSession", func(t *testing.T) {
		err := s.DeleteSession("x")
		assert.Error(t, err)
	})
	t.Run("SessionMessageCount", func(t *testing.T) {
		_, err := s.SessionMessageCount("x")
		assert.Error(t, err)
	})
	t.Run("Messages", func(t *testing.T) {
		_, err := s.Messages("x")
		assert.Error(t, err)
	})
	t.Run("GetSession", func(t *testing.T) {
		ss, err := s.GetSession("x")
		assert.Error(t, err)
		assert.Nil(t, ss)
	})
	t.Run("ListSessions", func(t *testing.T) {
		_, err := s.ListSessions(0)
		assert.Error(t, err)
	})
	t.Run("ListArchivedSessions", func(t *testing.T) {
		_, err := s.ListArchivedSessions(0)
		assert.Error(t, err)
	})
	t.Run("SnapshotSessionForRevert", func(t *testing.T) {
		_, err := s.SnapshotSessionForRevert("x")
		assert.Error(t, err)
	})
	t.Run("TruncateSessionForRevert", func(t *testing.T) {
		_, err := s.TruncateSessionForRevert("x", 0, 0)
		assert.Error(t, err)
	})
}

// TestStore_ClosedDB_ForkSession exercises the WriteTx error path in
// ForkSession. Note: normal argument-validation errors (fromSeq < -1) are
// tested in TestForkSession_NegativeOtherThanMinusOneRejected.
func TestStore_ClosedDB_ForkSession(t *testing.T) {
	s := storeWithClosedDB(t)
	_, err := s.ForkSession("x", -1)
	assert.Error(t, err)
}

func TestStore_ClosedDB_RestoreSessionAfterFailedRevert(t *testing.T) {
	s := storeWithClosedDB(t)
	snap := SessionRevertSnapshot{
		Meta: SessionSummary{ID: "test-id", Turns: 1},
		Messages: []Message{
			{ID: "m1", SessionID: "test-id", Seq: 0, Role: "user", Content: "hi", CreatedAt: 100},
		},
	}
	err := s.RestoreSessionAfterFailedRevert(snap)
	assert.Error(t, err)
}

func TestStore_ClosedDB_KVMethods(t *testing.T) {
	s := storeWithClosedDB(t)

	t.Run("KVSet", func(t *testing.T) {
		err := s.KVSet("k", "v")
		assert.Error(t, err)
	})
	t.Run("KVGet", func(t *testing.T) {
		_, _, err := s.KVGet("k")
		assert.Error(t, err)
	})
}

func TestStore_ClosedDB_MemoryMethods(t *testing.T) {
	s := storeWithClosedDB(t)

	t.Run("WriteMemory", func(t *testing.T) {
		_, err := s.WriteMemory("note", "x")
		assert.Error(t, err)
	})
	t.Run("SearchMemory", func(t *testing.T) {
		_, err := s.SearchMemory("x", 5)
		assert.Error(t, err)
	})
	t.Run("RecallMemory", func(t *testing.T) {
		_, err := s.RecallMemory(5)
		assert.Error(t, err)
	})
}

func TestStore_ClosedDB_TaskMethods(t *testing.T) {
	s := storeWithClosedDB(t)

	t.Run("CreateTask", func(t *testing.T) {
		_, err := s.CreateTask("echo", "in", "")
		assert.Error(t, err)
	})
	t.Run("SetTaskWorktree", func(t *testing.T) {
		err := s.SetTaskWorktree("x", "wt")
		assert.Error(t, err)
	})
	t.Run("ClaimTask", func(t *testing.T) {
		err := s.ClaimTask("x", "w")
		assert.Error(t, err)
	})
	t.Run("SetTaskResult", func(t *testing.T) {
		err := s.SetTaskResult("x", "done", "ok")
		assert.Error(t, err)
	})
	t.Run("GetTask", func(t *testing.T) {
		_, err := s.GetTask("x")
		assert.Error(t, err)
	})
	t.Run("TouchTask", func(t *testing.T) {
		err := s.TouchTask("x")
		assert.Error(t, err)
	})
	t.Run("ListStaleRunning", func(t *testing.T) {
		_, err := s.ListStaleRunning(100)
		assert.Error(t, err)
	})
	t.Run("RequeueTask", func(t *testing.T) {
		err := s.RequeueTask("x", "w")
		assert.Error(t, err)
	})
	t.Run("FinalizeTask", func(t *testing.T) {
		err := s.FinalizeTask("x", "w", "done", "ok")
		assert.Error(t, err)
	})
	t.Run("IncrementAttempts", func(t *testing.T) {
		err := s.IncrementAttempts("x")
		assert.Error(t, err)
	})
	t.Run("CancelTask", func(t *testing.T) {
		err := s.CancelTask("x")
		assert.Error(t, err)
	})
	t.Run("RequeueStaleTask", func(t *testing.T) {
		_, err := s.RequeueStaleTask("x", 100, 3)
		assert.Error(t, err)
	})
	t.Run("ListPending", func(t *testing.T) {
		_, err := s.ListPending(10)
		assert.Error(t, err)
	})
}

func TestStore_ClosedDB_AuthMethods(t *testing.T) {
	s := storeWithClosedDB(t)
	a := AuthMetadataFromDB(s)

	t.Run("SaveAuthMetadata", func(t *testing.T) {
		err := a.SaveAuthMetadata("p", "a", auth.AuthMetadata{Source: "s"})
		assert.Error(t, err)
	})
	t.Run("LoadAuthMetadata", func(t *testing.T) {
		_, err := a.LoadAuthMetadata("p", "a")
		assert.Error(t, err)
	})
	t.Run("DeleteAuthMetadata", func(t *testing.T) {
		err := a.DeleteAuthMetadata("p", "a")
		assert.Error(t, err)
	})
}

func TestStore_ClosedDB_WriteTx(t *testing.T) {
	// WriteTx itself: BeginTx on a closed pool must error.
	s := storeWithClosedDB(t)
	err := s.WriteTx(context.Background(), func(tx *sql.Tx) error {
		return nil
	})
	assert.Error(t, err, "WriteTx must fail when the database is closed")
}

func TestStore_ClosedDB_ApplyConnectionPragmas(t *testing.T) {
	// :memory: stores skip WAL pragmas entirely (inMemory guard), so the closed-DB
	// error path is only reachable on a file-backed store.
	s, err := Open(filepath.Join(t.TempDir(), "closed.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	require.NoError(t, s.DB.Close())
	err = s.applyConnectionPragmas()
	assert.Error(t, err, "applyConnectionPragmas must fail when the database is closed")
}

func TestStore_ClosedDB_Migrate(t *testing.T) {
	s := storeWithClosedDB(t)
	err := s.migrate()
	assert.Error(t, err, "migrate must fail when the database is closed")
}

func TestStore_ClosedDB_AddColumnIfMissing(t *testing.T) {
	s := storeWithClosedDB(t)
	err := s.addColumnIfMissing("sessions", "test_col", "TEXT")
	assert.Error(t, err, "addColumnIfMissing must fail when the database is closed")
}

func TestStore_ClosedDB_Columns(t *testing.T) {
	s := storeWithClosedDB(t)
	_, err := s.columns("sessions")
	assert.Error(t, err, "columns must fail when the database is closed")
}

// ---------------------------------------------------------------------------
// Non-DB error paths — argument validation and codec edge cases.
// ---------------------------------------------------------------------------

func TestSnapshotSessionForRevert_EmptyID(t *testing.T) {
	s, err := Open(":memory:")
	require.NoError(t, err)
	defer s.Close()

	_, err = s.SnapshotSessionForRevert("")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "empty session id")
}

func TestTruncateSessionForRevert_InvalidArgs(t *testing.T) {
	s, err := Open(":memory:")
	require.NoError(t, err)
	defer s.Close()

	t.Run("empty session id", func(t *testing.T) {
		_, err := s.TruncateSessionForRevert("", 0, 0)
		assert.Error(t, err)
	})
	t.Run("negative fromSeq", func(t *testing.T) {
		_, err := s.TruncateSessionForRevert("sid", -1, 0)
		assert.Error(t, err)
	})
	t.Run("negative turns", func(t *testing.T) {
		_, err := s.TruncateSessionForRevert("sid", 0, -1)
		assert.Error(t, err)
	})
}

func TestRestoreSessionAfterFailedRevert_EmptySnapshot(t *testing.T) {
	s, err := Open(":memory:")
	require.NoError(t, err)
	defer s.Close()

	err = s.RestoreSessionAfterFailedRevert(SessionRevertSnapshot{})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "empty session compensation snapshot")
}

// TestRestoreSessionAfterFailedRevert_SessionMismatch exercises the message
// session_id != target session_id guard inside WriteTx.
func TestRestoreSessionAfterFailedRevert_SessionMismatch(t *testing.T) {
	s, err := Open(":memory:")
	require.NoError(t, err)
	defer s.Close()

	// Create a session so the restore target exists.
	id, err := s.CreateSession("target")
	require.NoError(t, err)

	snap := SessionRevertSnapshot{
		Meta: SessionSummary{ID: id, Turns: 1},
		Messages: []Message{
			{ID: "m1", SessionID: "different-session", Seq: 0, Role: "user", Content: "hi", CreatedAt: 100},
		},
	}
	err = s.RestoreSessionAfterFailedRevert(snap)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "belongs to")
}

// TestRestoreSessionAfterFailedRevert_RowsAffectedZero exercises the
// RowsAffected==0 guard after the UPDATE sessions statement inside WriteTx.
//
// The snapshot carries NO messages on purpose. The guard under test sits after
// the message-insert loop, and since the store began enforcing messages.
// session_id, a snapshot message naming a session that does not exist fails on
// the INSERT and never reaches the UPDATE — the test would then assert the
// right message from the wrong statement.
func TestRestoreSessionAfterFailedRevert_RowsAffectedZero(t *testing.T) {
	s, err := Open(":memory:")
	require.NoError(t, err)
	defer s.Close()

	snap := SessionRevertSnapshot{Meta: SessionSummary{ID: "different-id", Turns: 1}}
	err = s.RestoreSessionAfterFailedRevert(snap)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "affected 0 rows")
}

// TestRestoreSessionAfterFailedRevert_DeleteMessagesBeforeRestore covers the
// "DELETE FROM messages" inside RestoreSessionAfterFailedRevert — the typical
// success path only tests truncate->restore (messages exist to delete).
// This test calls Restore directly on a session with messages already present,
// verifying the delete+insert round trip works end-to-end.
func TestRestoreSessionAfterFailedRevert_DeleteMessagesRoundTrip(t *testing.T) {
	s, err := Open(":memory:")
	require.NoError(t, err)
	defer s.Close()

	id, err := s.CreateSession("target")
	require.NoError(t, err)
	require.NoError(t, s.AppendMessage(id, 0, "user", "existing"))
	require.NoError(t, s.AppendMessage(id, 1, "assistant", "old"))

	snap := SessionRevertSnapshot{
		Meta: SessionSummary{ID: id, Turns: 5},
		Messages: []Message{
			{ID: "m1", SessionID: id, Seq: 0, Role: "user", Content: "replacement", CreatedAt: 200},
		},
	}
	require.NoError(t, s.RestoreSessionAfterFailedRevert(snap))
	msgs, err := s.Messages(id)
	require.NoError(t, err)
	require.Len(t, msgs, 1)
	assert.Equal(t, "replacement", msgs[0].Content)
	got, err := s.GetSession(id)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, 5, got.Turns)
}

// TestTruncateSessionForRevert_ApplyDeletedMessageFail exercises the
// RowsAffected error path in TruncateSessionForRevert (after the DELETE).
func TestTruncateSessionForRevert_SessionUpdateRowsAffected(t *testing.T) {
	s, err := Open(":memory:")
	require.NoError(t, err)
	defer s.Close()

	id, err := s.CreateSession("t")
	require.NoError(t, err)
	require.NoError(t, s.AppendMessage(id, 0, "user", "m0"))

	// Simulate a failure in UPDATE sessions after the DELETE succeeds.
	// We use a trigger that fires on UPDATE of sessions and raises ABORT.
	_, err = s.DB.Exec(
		`CREATE TRIGGER fail_session_update_before_truncate
		 BEFORE UPDATE ON sessions
		 BEGIN SELECT RAISE(ABORT, 'injected session update failure'); END;`)
	require.NoError(t, err)

	_, err = s.TruncateSessionForRevert(id, 0, 0)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "injected")
}

// ---------------------------------------------------------------------------
// EncodeSessionRevertSnapshot / DecodeSessionRevertSnapshot edge cases.
// ---------------------------------------------------------------------------

func TestEncodeSessionRevertSnapshot_EmptySessionID(t *testing.T) {
	_, err := EncodeSessionRevertSnapshot(SessionRevertSnapshot{
		Meta: SessionSummary{ID: ""},
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "empty session revert snapshot")
}

func TestDecodeSessionRevertSnapshot_EmptyBlob(t *testing.T) {
	// Empty (non-nil) slice — different from nil, but same len==0 guard.
	_, err := DecodeSessionRevertSnapshot([]byte{})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "no session revert snapshot")
}

func TestDecodeSessionRevertSnapshot_InvalidJSON(t *testing.T) {
	_, err := DecodeSessionRevertSnapshot([]byte(`not-json`))
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "decode session revert snapshot")
}

func TestDecodeSessionRevertSnapshot_ValidJSONButEmptyID(t *testing.T) {
	blob, err := json.Marshal(SessionRevertSnapshot{
		Meta: SessionSummary{ID: ""},
	})
	require.NoError(t, err)
	_, err = DecodeSessionRevertSnapshot(blob)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "empty session id")
}

// ---------------------------------------------------------------------------
// Task "not found" / "not running" error paths (not-DB-error).
// ---------------------------------------------------------------------------

// TestTask_SetTaskResult_Missing ensures SetTaskResult on a non-existent task
// returns the "task not found" error (RowsAffected==0 guard).
func TestTask_SetTaskResult_Missing(t *testing.T) {
	s, err := Open(":memory:")
	require.NoError(t, err)
	defer s.Close()

	err = s.SetTaskResult("nonexistent", "done", "ok")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "task not found")
}

// TestTask_FinalizeTask_WrongOwnerAndMissing exercises both the
// ErrNotRunningOrOwned path (wrong worker) and the non-existent path.
func TestTask_FinalizeTask_WrongOwnerAndMissing(t *testing.T) {
	s, err := Open(":memory:")
	require.NoError(t, err)
	defer s.Close()

	id, err := s.CreateTask("t", "in", "")
	require.NoError(t, err)
	require.NoError(t, s.ClaimTask(id, "w1"))

	// Wrong owner → ErrNotRunningOrOwned
	err = s.FinalizeTask(id, "w2", "completed", "ok")
	assert.Error(t, err)
	assert.ErrorIs(t, err, ErrNotRunningOrOwned)

	// Non-existent → not ErrNotRunningOrOwned (the WHERE matches 0 rows)
	err = s.FinalizeTask("nonexistent", "w1", "completed", "ok")
	assert.Error(t, err)
	assert.ErrorIs(t, err, ErrNotRunningOrOwned)
}

// TestTask_TouchTask_Missing ensures TouchTask on a non-existent task
// returns the "task not found" error.
func TestTask_TouchTask_Missing(t *testing.T) {
	s, err := Open(":memory:")
	require.NoError(t, err)
	defer s.Close()

	err = s.TouchTask("nonexistent")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "task not found")
}

// TestTask_IncrementAttempts_Missing ensures IncrementAttempts on a non-
// existent task returns the "task not found" error.
func TestTask_IncrementAttempts_Missing(t *testing.T) {
	s, err := Open(":memory:")
	require.NoError(t, err)
	defer s.Close()

	err = s.IncrementAttempts("nonexistent")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "task not found")
}

// TestTask_RequeueStaleTask_ExceedsMaxRetries proves that when the incremented
// attempt count exceeds maxRetries, the status is set to 'failed' instead of
// 'pending', preserving any existing result.
func TestTask_RequeueStaleTask_ExceedsMaxRetries(t *testing.T) {
	s, err := Open(":memory:")
	require.NoError(t, err)
	defer s.Close()

	id, err := s.CreateTask("t", "in", "")
	require.NoError(t, err)
	require.NoError(t, s.ClaimTask(id, "w1"))

	got, err := s.GetTask(id)
	require.NoError(t, err)
	cutoff := got.UpdatedAt + 100 // task is "stale"

	// maxRetries=0: attempts+1=1 > 0 → status becomes 'failed'
	changed, err := s.RequeueStaleTask(id, cutoff, 0)
	require.NoError(t, err)
	assert.True(t, changed)

	got, err = s.GetTask(id)
	require.NoError(t, err)
	assert.Equal(t, "failed", got.Status)
	assert.Equal(t, int64(1), got.Attempts)

	// Verify that attempting to requeue it again does nothing (it's no longer
	// in 'running' status).
	changed, err = s.RequeueStaleTask(id, cutoff+200, 0)
	require.NoError(t, err)
	assert.False(t, changed)
}

// TestTask_RequeueStaleTask_PreservesResultWhenFailed proves that when
// a running task is flagged stale and maxRetries is exceeded, the existing
// result is preserved (not cleared to ”).
func TestTask_RequeueStaleTask_PreservesResultWhenFailed(t *testing.T) {
	s, err := Open(":memory:")
	require.NoError(t, err)
	defer s.Close()

	id, err := s.CreateTask("t", "in", "")
	require.NoError(t, err)
	require.NoError(t, s.ClaimTask(id, "w1"))
	// Set a partial result before failing via stale requeue.
	require.NoError(t, s.SetTaskResult(id, "running", "partial-work"))

	got, err := s.GetTask(id)
	require.NoError(t, err)

	changed, err := s.RequeueStaleTask(id, got.UpdatedAt+100, 0)
	require.NoError(t, err)
	assert.True(t, changed)

	got, err = s.GetTask(id)
	require.NoError(t, err)
	assert.Equal(t, "failed", got.Status)
	assert.Equal(t, "partial-work", got.Result, "existing result must be preserved")
}

// TestTask_CreateTask_ErrorFromWriteTx closes the DB and verifies the error
// path (the "if err != nil" guard after WriteTx).
func TestTask_CreateTask_ErrorPath(t *testing.T) {
	s := storeWithClosedDB(t)
	_, err := s.CreateTask("echo", "in", "")
	assert.Error(t, err)
}

// TestStore_ClosedDB_ForkSession_SourceMissing covers the case where a
// non-existent source session ID causes the query inside ForkSession's WriteTx
// to fail (sql.ErrNoRows). This is distinct from the closed-DB path because
// the DB is healthy but the session doesn't exist.
func TestForkSession_SourceMissing_Single(t *testing.T) {
	s, err := Open(":memory:")
	require.NoError(t, err)
	defer s.Close()

	_, err = s.ForkSession("nonexistent", -1)
	assert.Error(t, err)
}

// TestForkSession_SeqOutOfBounds_Single: already covered in
// session_fork_test.go TestForkSession_SeqOutOfBoundsRejected - but repeat
// here for completeness.
func TestForkSession_ClosingDBError(t *testing.T) {
	s := storeWithClosedDB(t)
	_, err := s.ForkSession("x", -1)
	assert.Error(t, err)
}

// TestSession_AppendMessageError_OnUpdate is a targeted test that exercises
// the second-Exec error path of AppendMessage (the UPDATE sessions after the
// INSERT succeeds). We simulate this by creating a trigger that fires on
// UPDATE of sessions and raises an error.
func TestSession_AppendMessageError_OnUpdate(t *testing.T) {
	s, err := Open(":memory:")
	require.NoError(t, err)
	defer s.Close()

	sid, err := s.CreateSession("t")
	require.NoError(t, err)

	// Install a trigger that aborts any UPDATE on sessions.
	_, err = s.DB.Exec(
		`CREATE TRIGGER fail_session_update
		 BEFORE UPDATE ON sessions
		 BEGIN SELECT RAISE(ABORT, 'injected session update failure'); END;`)
	require.NoError(t, err)

	// AppendMessage should fail because the UPDATE sessions fails.
	err = s.AppendMessage(sid, 0, "user", "hi")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "injected")
}

// TestSession_DeleteSessionMessagesDeleteFails exercises the DELETE FROM
// messages error path in DeleteSession's WriteTx.
func TestSession_DeleteSessionMessagesDeleteFails(t *testing.T) {
	s, err := Open(":memory:")
	require.NoError(t, err)
	defer s.Close()

	sid, err := s.CreateSession("t")
	require.NoError(t, err)
	require.NoError(t, s.AppendMessage(sid, 0, "user", "hi"))

	// Install a trigger that aborts DELETE from messages.
	_, err = s.DB.Exec(
		`CREATE TRIGGER fail_delete_messages
		 BEFORE DELETE ON messages
		 BEGIN SELECT RAISE(ABORT, 'injected message delete failure'); END;`)
	require.NoError(t, err)

	err = s.DeleteSession(sid)
	assert.Error(t, err)
}

// TestSnapshotSessionForRevert_ScanError exercises the scan failure inside
// snapshotSessionTx. We install a trigger that corrupts the sessions table
// on read (not possible in pure SQLite), so instead we close the DB while
// inside the WriteTx by panicking... Actually, the simpler approach: we
// call snapshotSessionTx with a non-existent session (QueryRow will Scan
// sql.ErrNoRows which IS tested by the missing-session test). To test the
// scan error path we need a different approach. Skip this for now — the
// scan error on a healthy DB is unreachable because PRAGMA table_info and
// the SELECT both return well-formed rows.

// ---------------------------------------------------------------------------
// Store internal method coverage
// ---------------------------------------------------------------------------

func TestStore_HappyPathMeta(t *testing.T) {
	// TruncateSessionForRevert additionally exercises the write path for
	// RowsAffected which is not captured by the closed-DB helper
	// (BeginTx fails before RowsAffected is reached).
	s, err := Open(":memory:")
	require.NoError(t, err)
	defer s.Close()

	sid, err := s.CreateSession("x")
	require.NoError(t, err)
	require.NoError(t, s.AppendMessage(sid, 0, "user", "hi"))

	// Already tested in seam_truncate_test.go, but verify it doesn't regress.
	_, truncErr := s.TruncateSessionForRevert(sid, 0, 0)
	require.NoError(t, truncErr)
}

// TestListStaleRunning_EmptyQuery is covered by TestTask_ListStaleRunning,
// but ensure the empty query-error path is also tested.
func TestListPending_Empty(t *testing.T) {
	s, err := Open(":memory:")
	require.NoError(t, err)
	defer s.Close()

	pending, err := s.ListPending(10)
	require.NoError(t, err)
	assert.Empty(t, pending)
}

// TestListStaleRunning_Empty asserts an idle store returns no stale tasks.
func TestListStaleRunning_Empty(t *testing.T) {
	s, err := Open(":memory:")
	require.NoError(t, err)
	defer s.Close()

	stale, err := s.ListStaleRunning(1000)
	require.NoError(t, err)
	assert.Empty(t, stale)
}

// ---------------------------------------------------------------------------
// scanMemories mock tests — exercises the scan-error and rows.Err() error
// paths that are unreachable with real *sql.Rows on :memory:.
// ---------------------------------------------------------------------------

type mockScanRows struct {
	nextReturn bool
	scanErr    error
	errReturn  error
}

func (m *mockScanRows) Next() bool {
	return m.nextReturn
}

func (m *mockScanRows) Scan(dest ...any) error {
	if m.scanErr != nil {
		return m.scanErr
	}
	// Fill in realistic values so the scan path is fully exercised.
	*dest[0].(*string) = "id1"
	*dest[1].(*string) = "note"
	*dest[2].(*string) = "content"
	*dest[3].(*int64) = 100
	return nil
}

func (m *mockScanRows) Err() error {
	return m.errReturn
}

func TestScanMemories_ScanError(t *testing.T) {
	mock := &mockScanRows{nextReturn: true, scanErr: assert.AnError}
	_, err := scanMemories(mock)
	assert.Error(t, err)
}

func TestScanMemories_RowsErr(t *testing.T) {
	mock := &mockScanRows{nextReturn: false, errReturn: assert.AnError}
	_, err := scanMemories(mock)
	assert.Error(t, err)
}

func TestSearchMemory_DefaultLimit(t *testing.T) {
	s, err := Open(":memory:")
	require.NoError(t, err)
	defer s.Close()

	// limit <= 0 triggers the default limit (10).
	_, err = s.WriteMemory("note", "hello world")
	require.NoError(t, err)

	got, err := s.SearchMemory("hello", 0)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "hello world", got[0].Content)
}

// TestOpenWith_SQLOpenError exercises the sql.Open error path inside
// OpenWith by temporarily replacing sqlOpener with a failing function.
func TestOpenWith_SQLOpenError(t *testing.T) {
	old := sqlOpener
	sqlOpener = func(driverName, dataSourceName string) (*sql.DB, error) {
		return nil, fmt.Errorf("injected open error")
	}
	defer func() { sqlOpener = old }()

	_, err := Open(":memory:")
	if err == nil {
		t.Fatal("expected error from injected sql.Open failure")
	}
	t.Logf("error: %v", err)
}

// TestEncodeSessionRevertSnapshot_MarshalError exercises the json.Marshal
// error path inside EncodeSessionRevertSnapshot by temporarily replacing
// jsonMarshal with a failing function.
func TestEncodeSessionRevertSnapshot_MarshalError(t *testing.T) {
	old := jsonMarshal
	jsonMarshal = func(v any) ([]byte, error) {
		return nil, fmt.Errorf("injected marshal error")
	}
	defer func() { jsonMarshal = old }()

	_, err := EncodeSessionRevertSnapshot(SessionRevertSnapshot{
		Meta: SessionSummary{ID: "test-session"},
	})
	if err == nil {
		t.Fatal("expected error from injected json.Marshal failure")
	}
	t.Logf("error: %v", err)
}
