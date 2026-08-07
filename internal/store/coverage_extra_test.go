// Package store 的额外覆盖测试，针对现有测试未覆盖的边界和错误路径。
package store

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Close / OpenWith / buildDSN 边界
// ---------------------------------------------------------------------------

// TestClose_WALCheckpoint 验证 Close 成功（WAL checkpoint 非致命错误）。
func TestClose_WALCheckpoint(t *testing.T) {
	s, err := Open(":memory:")
	require.NoError(t, err)
	// 先插入数据产生 WAL 记录
	_, err = s.CreateSession("x")
	require.NoError(t, err)
	// Close 不应返回错误
	require.NoError(t, s.Close())
}

// TestOpenWith_CustomOptions 验证 OpenWith 传入不同参数时的行为。
func TestOpenWith_CustomOptions(t *testing.T) {
	dir := t.TempDir()

	t.Run("zero options use defaults", func(t *testing.T) {
		s, err := OpenWith(filepath.Join(dir, "zero.db"), OpenOptions{})
		require.NoError(t, err)
		defer s.Close()
		assert.Equal(t, 4, s.DB.Stats().MaxOpenConnections)
	})

	t.Run("max open conns 4", func(t *testing.T) {
		s, err := OpenWith(filepath.Join(dir, "conn4.db"), OpenOptions{MaxOpenConns: 4, BusyTimeoutMs: 5000, WALAutoCheckpoint: 1000})
		require.NoError(t, err)
		defer s.Close()
		assert.Equal(t, 4, s.DB.Stats().MaxOpenConnections)
	})

	t.Run("max open conns 1 on memory", func(t *testing.T) {
		s, err := OpenWith(":memory:", OpenOptions{MaxOpenConns: 4})
		require.NoError(t, err)
		defer s.Close()
		assert.Equal(t, 1, s.DB.Stats().MaxOpenConnections)
	})

	t.Run("negative autocheckpoint disables", func(t *testing.T) {
		// 传入 -1 不会被默认值覆盖（ckpt != 0），且 DSN _pragma 接收负数。
		s, err := OpenWith(filepath.Join(dir, "negckpt.db"), OpenOptions{
			MaxOpenConns:      1,
			BusyTimeoutMs:     500,
			WALAutoCheckpoint: -1,
		})
		require.NoError(t, err)
		defer s.Close()
		// 验证 busy_timeout 生效
		var bt int
		require.NoError(t, s.DB.QueryRow("PRAGMA busy_timeout").Scan(&bt))
		assert.Equal(t, 500, bt)
	})
}

// TestBuildDSN 验证 buildDSN 生成正确的 DSN 字符串。
func TestBuildDSN(t *testing.T) {
	dsn := buildDSN("/path/db", 5000, 1000)
	assert.Contains(t, dsn, "/path/db")
	assert.Contains(t, dsn, "_pragma=busy_timeout(5000)")
	assert.Contains(t, dsn, "_pragma=synchronous(NORMAL)")
	assert.Contains(t, dsn, "_pragma=wal_autocheckpoint(1000)")

	// 不同的参数
	dsn2 := buildDSN("/other/db", 100, 50)
	assert.Contains(t, dsn2, "_pragma=busy_timeout(100)")
	assert.Contains(t, dsn2, "_pragma=wal_autocheckpoint(50)")
}

// ---------------------------------------------------------------------------
// UpdateSessionMeta BillingMeta 边界
// ---------------------------------------------------------------------------

// TestUpdateSessionMeta_CostKnownTrue 验证 CostKnown=true 时的持久化。
func TestUpdateSessionMeta_CostKnownTrue(t *testing.T) {
	s, err := Open(":memory:")
	require.NoError(t, err)
	defer s.Close()

	id, err := s.CreateSession("cost")
	require.NoError(t, err)

	// CostKnown=true
	err = s.UpdateSessionMeta(id, "", "", 0, 0, 1, 0, 0, BillingMeta{CostKnown: true})
	require.NoError(t, err)

	got, err := s.GetSession(id)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.True(t, got.CostKnown)

	// CostKnown=false
	err = s.UpdateSessionMeta(id, "", "", 0, 0, 1, 0, 0, BillingMeta{})
	require.NoError(t, err)

	got, err = s.GetSession(id)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.False(t, got.CostKnown)
}

// ---------------------------------------------------------------------------
// SetSessionArchived / DeleteSession 边界
// ---------------------------------------------------------------------------

// TestSetSessionArchived_TrueThenFalse 验证 archive/unarchive 完全不丢数据。
func TestSetSessionArchived_TrueThenFalse(t *testing.T) {
	s, err := Open(":memory:")
	require.NoError(t, err)
	defer s.Close()

	id, err := s.CreateSession("test")
	require.NoError(t, err)
	require.NoError(t, s.AppendMessage(id, 0, "user", "hi"))

	// archive → unarchive → 消息仍在
	require.NoError(t, s.SetSessionArchived(id, true))
	require.NoError(t, s.SetSessionArchived(id, false))

	msgs, err := s.Messages(id)
	require.NoError(t, err)
	require.Len(t, msgs, 1)
	assert.Equal(t, "hi", msgs[0].Content)
}

// TestDeleteSession_NonExistent 验证删除不存在的 session 不会报错（DELETE 影响 0 行）。
func TestDeleteSession_NonExistent(t *testing.T) {
	s, err := Open(":memory:")
	require.NoError(t, err)
	defer s.Close()

	err = s.DeleteSession("nonexistent")
	// 当前实现：WriteTx 内的 DELETE 影响 0 行但不报错
	require.NoError(t, err)
}

// TestDeleteSession_NoMessages 验证删除只有 session 行没有消息时的行为。
func TestDeleteSession_NoMessages(t *testing.T) {
	s, err := Open(":memory:")
	require.NoError(t, err)
	defer s.Close()

	id, err := s.CreateSession("lonely")
	require.NoError(t, err)
	require.NoError(t, s.DeleteSession(id))

	got, err := s.GetSession(id)
	require.NoError(t, err)
	assert.Nil(t, got)
}

// ---------------------------------------------------------------------------
// RestoreSessionAfterFailedRevert 额外路径
// ---------------------------------------------------------------------------

// TestRestoreSessionAfterFailedRevert_CostKnownTrue 验证 CostKnown=true 的 snapshot
// 能正确 restore。
func TestRestoreSessionAfterFailedRevert_CostKnownTrue(t *testing.T) {
	s, err := Open(":memory:")
	require.NoError(t, err)
	defer s.Close()

	id, err := s.CreateSession("cost-restore")
	require.NoError(t, err)

	// 创建 CostKnown=true 的 meta
	err = s.UpdateSessionMeta(id, "", "", 0, 0, 5, 0, 0, BillingMeta{CostKnown: true})
	require.NoError(t, err)

	snap, err := s.SnapshotSessionForRevert(id)
	require.NoError(t, err)
	assert.True(t, snap.Meta.CostKnown)

	// truncate 再 restore
	truncatedSnap, err := s.TruncateSessionForRevert(id, 0, 0)
	require.NoError(t, err)
	assert.True(t, truncatedSnap.Meta.CostKnown)

	require.NoError(t, s.RestoreSessionAfterFailedRevert(snap))

	got, err := s.GetSession(id)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.True(t, got.CostKnown)
}

// TestRestoreSessionAfterFailedRevert_CostKnownFalse
func TestRestoreSessionAfterFailedRevert_CostKnownFalse(t *testing.T) {
	s, err := Open(":memory:")
	require.NoError(t, err)
	defer s.Close()

	id, err := s.CreateSession("no-cost")
	require.NoError(t, err)

	snap := SessionRevertSnapshot{
		Meta: SessionSummary{ID: id, Turns: 3, CostKnown: false},
	}
	// 先写入 session 存在
	require.NoError(t, s.RestoreSessionAfterFailedRevert(snap))
	got, err := s.GetSession(id)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.False(t, got.CostKnown)
}

// TestRestoreSessionAfterFailedRevert_InsertMessageFailure 测试在 restore 期间
// INSERT messages 失败的路径（通过触发器模拟）。
func TestRestoreSessionAfterFailedRevert_InsertMessageFailure(t *testing.T) {
	s, err := Open(":memory:")
	require.NoError(t, err)
	defer s.Close()

	id, err := s.CreateSession("fail-insert")
	require.NoError(t, err)
	require.NoError(t, s.AppendMessage(id, 0, "user", "keep-me"))

	snap, err := s.SnapshotSessionForRevert(id)
	require.NoError(t, err)

	// 安装无条件失败的触发器（modernc/sqlite 不支持 trigger 中的参数化变量）
	_, err = s.DB.Exec(`CREATE TRIGGER fail_restore_insert
		BEFORE INSERT ON messages
		BEGIN SELECT RAISE(ABORT, 'injected restore insert failure'); END;`)
	require.NoError(t, err)

	err = s.RestoreSessionAfterFailedRevert(snap)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "injected restore insert failure")
}

// TestRestoreSessionAfterFailedRevert_DeleteMessagesFailure 测试 restore 开始时
// DELETE FROM messages 失败的路径。
func TestRestoreSessionAfterFailedRevert_DeleteMessagesFailure(t *testing.T) {
	s, err := Open(":memory:")
	require.NoError(t, err)
	defer s.Close()

	id, err := s.CreateSession("fail-del")
	require.NoError(t, err)
	require.NoError(t, s.AppendMessage(id, 0, "user", "hi"))

	snap, err := s.SnapshotSessionForRevert(id)
	require.NoError(t, err)

	// 安装无条件失败的触发器
	_, err = s.DB.Exec(`CREATE TRIGGER fail_restore_delete
		BEFORE DELETE ON messages
		BEGIN SELECT RAISE(ABORT, 'injected restore delete failure'); END;`)
	require.NoError(t, err)

	err = s.RestoreSessionAfterFailedRevert(snap)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "injected restore delete failure")
}

// TestRestoreSessionAfterFailedRevert_SessionsTableUnwritable 验证 restore 中
// UPDATE sessions 自身 Exec 失败时的错误路径（session.go::RestoreSessionAfterFailedRevert
// 里包 "restore session meta" 的那一段）。
//
// 制造失败的手段是一个 BEFORE UPDATE 触发器，而不是原先的 DROP TABLE sessions。
// messages.session_id 被强制执行之后，父表一旦消失，restore 的**第一条语句**
// （DELETE FROM messages）就要去查父表，于是先报 "no such table: main.sessions"，
// UPDATE 那条路径根本走不到 —— 测试仍会红/绿，但描述的已经是另一条语句。
// 触发器把失败精确地钉在 UPDATE 上，且不动 messages 这一侧。
func TestRestoreSessionAfterFailedRevert_SessionsTableUnwritable(t *testing.T) {
	s, err := Open(":memory:")
	require.NoError(t, err)
	defer s.Close()

	id, err := s.CreateSession("test-del-sess")
	require.NoError(t, err)

	// 获取有效 snapshot
	snap, err := s.SnapshotSessionForRevert(id)
	require.NoError(t, err)
	assert.NotEmpty(t, snap.Meta.ID)

	_, err = s.DB.Exec(`CREATE TRIGGER block_session_update BEFORE UPDATE ON sessions
	  BEGIN SELECT RAISE(ABORT, 'sessions is unwritable'); END`)
	require.NoError(t, err)

	// Restore 应该失败于 UPDATE sessions
	err = s.RestoreSessionAfterFailedRevert(snap)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "restore session meta")
	assert.Contains(t, err.Error(), "sessions is unwritable",
		"必须是 UPDATE 自己失败，而不是前面某条语句先报错")
}

// ---------------------------------------------------------------------------
// TruncateSessionForRevert 额外路径
// ---------------------------------------------------------------------------

// TestTruncateSessionForRevert_UpdateSessionFailure 测试 truncate 中
// UPDATE sessions 失败的路径（通过触发器模拟）。
func TestTruncateSessionForRevert_UpdateSessionFailure(t *testing.T) {
	s, err := Open(":memory:")
	require.NoError(t, err)
	defer s.Close()

	id, err := s.CreateSession("trunc-fail")
	require.NoError(t, err)
	require.NoError(t, s.AppendMessage(id, 0, "user", "m0"))

	// 安装触发器，在 UPDATE sessions 时失败
	_, err = s.DB.Exec(`CREATE TRIGGER fail_trunc_update
		BEFORE UPDATE ON sessions
		BEGIN SELECT RAISE(ABORT, 'injected trunc update failure'); END;`)
	require.NoError(t, err)

	_, err = s.TruncateSessionForRevert(id, 0, 0)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "injected trunc update failure")
}

// ---------------------------------------------------------------------------
// ForkSession 额外路径
// ---------------------------------------------------------------------------

// TestForkSession_MessagesQueryError 验证 ForkSession 中 tx.Query 查询 messages
// 失败时的错误路径（session_fork.go line 57-59）。
// 通过 DROP TABLE messages 使查询失败，覆盖 tx.Query → err != nil 分支。
func TestForkSession_MessagesQueryError(t *testing.T) {
	s, err := Open(":memory:")
	require.NoError(t, err)
	defer s.Close()

	sid, err := s.CreateSession("src")
	require.NoError(t, err)
	require.NoError(t, s.AppendMessage(sid, 0, "user", "hi"))

	// 删除 messages 表使后续的消息查询失败
	_, err = s.DB.Exec("DROP TABLE messages")
	require.NoError(t, err)

	_, err = s.ForkSession(sid, -1)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ForkSession: load messages")
}

// TestForkSession_EmptyTitleSource 验证 source session 标题为空时 fork 使用 "fork"。

// TestForkSession_EmptyTitleSource 验证 source session 标题为空时 fork 使用 "fork"。
func TestForkSession_EmptyTitleSource(t *testing.T) {
	s, err := Open(":memory:")
	require.NoError(t, err)
	defer s.Close()

	// 创建空标题 session
	sid, err := s.CreateSession("")
	require.NoError(t, err)
	// CreateSession 使用 redact，空字符串不会被 redact 改变
	require.NoError(t, s.AppendMessage(sid, 0, "user", "hi"))

	forkID, err := s.ForkSession(sid, -1)
	require.NoError(t, err)

	forked, err := s.GetSession(forkID)
	require.NoError(t, err)
	require.NotNil(t, forked)
	assert.Equal(t, "fork", forked.Title)
}

// TestForkSession_InsertSessionFailure 验证 ForkSession 中 INSERT sessions 失败。
func TestForkSession_InsertSessionFailure(t *testing.T) {
	s, err := Open(":memory:")
	require.NoError(t, err)
	defer s.Close()

	sid, err := s.CreateSession("src")
	require.NoError(t, err)
	require.NoError(t, s.AppendMessage(sid, 0, "user", "hi"))

	_, err = s.DB.Exec(`CREATE TRIGGER fail_fork_session
		BEFORE INSERT ON sessions
		BEGIN SELECT RAISE(ABORT, 'injected fork session failure'); END;`)
	require.NoError(t, err)

	_, err = s.ForkSession(sid, -1)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "injected fork session failure")
}

// TestForkSession_InsertMessageFailure 验证 ForkSession 中 INSERT messages 失败。
func TestForkSession_InsertMessageFailure(t *testing.T) {
	s, err := Open(":memory:")
	require.NoError(t, err)
	defer s.Close()

	sid, err := s.CreateSession("src")
	require.NoError(t, err)
	require.NoError(t, s.AppendMessage(sid, 0, "user", "hi"))

	_, err = s.DB.Exec(`CREATE TRIGGER fail_fork_msg
		BEFORE INSERT ON messages
		BEGIN SELECT RAISE(ABORT, 'injected fork message failure'); END;`)
	require.NoError(t, err)

	_, err = s.ForkSession(sid, -1)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "injected fork message failure")
}

// ---------------------------------------------------------------------------
// AppendMessage INSERT 失败路径
// ---------------------------------------------------------------------------

// TestAppendMessage_InsertFails 验证 AppendMessage 中 INSERT messages 失败时的行为。
func TestAppendMessage_InsertFails(t *testing.T) {
	s, err := Open(":memory:")
	require.NoError(t, err)
	defer s.Close()

	sid, err := s.CreateSession("t")
	require.NoError(t, err)

	// 安装触发器让 INSERT messages 失败
	_, err = s.DB.Exec(`CREATE TRIGGER fail_msg_insert
		BEFORE INSERT ON messages
		BEGIN SELECT RAISE(ABORT, 'injected msg insert failure'); END;`)
	require.NoError(t, err)

	err = s.AppendMessage(sid, 0, "user", "hi")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "injected msg insert failure")
}

// ---------------------------------------------------------------------------
// DeleteSession 写入失败路径
// ---------------------------------------------------------------------------

// TestDeleteSession_SessionDeleteFails 验证 DeleteSession 中 DELETE sessions 失败。
func TestDeleteSession_SessionDeleteFails(t *testing.T) {
	s, err := Open(":memory:")
	require.NoError(t, err)
	defer s.Close()

	sid, err := s.CreateSession("t")
	require.NoError(t, err)
	require.NoError(t, s.AppendMessage(sid, 0, "user", "hi"))

	// messages 删除成功，但 sessions 删除失败
	_, err = s.DB.Exec(`CREATE TRIGGER fail_session_delete
		BEFORE DELETE ON sessions
		BEGIN SELECT RAISE(ABORT, 'injected session delete failure'); END;`)
	require.NoError(t, err)

	err = s.DeleteSession(sid)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "injected session delete failure")
}

// ---------------------------------------------------------------------------
// KV（SQL 查询失败路径 — 已由 closed-DB 覆盖；仅补充边界）
// ---------------------------------------------------------------------------

// TestKVGet_ScanError 验证 KVGet 在 sql.ErrNoRows 时返回 ok=false。
func TestKVGet_ScanError(t *testing.T) {
	s, err := Open(":memory:")
	require.NoError(t, err)
	defer s.Close()

	_, ok, err := s.KVGet("nonexistent")
	require.NoError(t, err)
	assert.False(t, ok)
}

// ---------------------------------------------------------------------------
// 带触发器的 TASK 写入失败路径
// ---------------------------------------------------------------------------

// TestTask_SetTaskResult_UpdateFails 验证 SetTaskResult 在 UPDATE tasks 失败时。
func TestTask_SetTaskResult_UpdateFails(t *testing.T) {
	s, err := Open(":memory:")
	require.NoError(t, err)
	defer s.Close()

	id, err := s.CreateTask("t", "in", "")
	require.NoError(t, err)

	_, err = s.DB.Exec(`CREATE TRIGGER fail_task_result
		BEFORE UPDATE ON tasks
		BEGIN SELECT RAISE(ABORT, 'injected task update failure'); END;`)
	require.NoError(t, err)

	err = s.SetTaskResult(id, "done", "ok")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "injected task update failure")
}

// TestTask_ClaimTask_UpdateFails 验证 ClaimTask 的 Exec 失败路径。
func TestTask_ClaimTask_UpdateFails(t *testing.T) {
	s, err := Open(":memory:")
	require.NoError(t, err)
	defer s.Close()

	id, err := s.CreateTask("t", "in", "")
	require.NoError(t, err)

	_, err = s.DB.Exec(`CREATE TRIGGER fail_task_claim
		BEFORE UPDATE ON tasks
		BEGIN SELECT RAISE(ABORT, 'injected claim failure'); END;`)
	require.NoError(t, err)

	err = s.ClaimTask(id, "w1")
	require.Error(t, err)
}

// TestTask_TouchTask_UpdateFails 验证 TouchTask 的 Exec 失败路径。
func TestTask_TouchTask_UpdateFails(t *testing.T) {
	s, err := Open(":memory:")
	require.NoError(t, err)
	defer s.Close()

	id, err := s.CreateTask("t", "in", "")
	require.NoError(t, err)

	_, err = s.DB.Exec(`CREATE TRIGGER fail_task_touch
		BEFORE UPDATE ON tasks
		BEGIN SELECT RAISE(ABORT, 'injected touch failure'); END;`)
	require.NoError(t, err)

	err = s.TouchTask(id)
	require.Error(t, err)
}

// TestTask_IncrementAttempts_UpdateFails 验证 IncrementAttempts 的 Exec 失败路径。
func TestTask_IncrementAttempts_UpdateFails(t *testing.T) {
	s, err := Open(":memory:")
	require.NoError(t, err)
	defer s.Close()

	id, err := s.CreateTask("t", "in", "")
	require.NoError(t, err)

	_, err = s.DB.Exec(`CREATE TRIGGER fail_task_inc
		BEFORE UPDATE ON tasks
		BEGIN SELECT RAISE(ABORT, 'injected inc failure'); END;`)
	require.NoError(t, err)

	err = s.IncrementAttempts(id)
	require.Error(t, err)
}

// TestTask_RequeueTask_UpdateFails 验证 RequeueTask 的 Exec 失败路径。
func TestTask_RequeueTask_UpdateFails(t *testing.T) {
	s, err := Open(":memory:")
	require.NoError(t, err)
	defer s.Close()

	id, err := s.CreateTask("t", "in", "")
	require.NoError(t, err)
	require.NoError(t, s.ClaimTask(id, "w1"))

	_, err = s.DB.Exec(`CREATE TRIGGER fail_task_requeue
		BEFORE UPDATE ON tasks
		BEGIN SELECT RAISE(ABORT, 'injected requeue failure'); END;`)
	require.NoError(t, err)

	err = s.RequeueTask(id, "w1")
	require.Error(t, err)
}

// TestTask_FinalizeTask_UpdateFails 验证 FinalizeTask 的 Exec 失败路径。
func TestTask_FinalizeTask_UpdateFails(t *testing.T) {
	s, err := Open(":memory:")
	require.NoError(t, err)
	defer s.Close()

	id, err := s.CreateTask("t", "in", "")
	require.NoError(t, err)
	require.NoError(t, s.ClaimTask(id, "w1"))

	_, err = s.DB.Exec(`CREATE TRIGGER fail_task_finalize
		BEFORE UPDATE ON tasks
		BEGIN SELECT RAISE(ABORT, 'injected finalize failure'); END;`)
	require.NoError(t, err)

	err = s.FinalizeTask(id, "w1", "completed", "ok")
	require.Error(t, err)
}

// TestTask_CancelTask_UpdateFails 验证 CancelTask 的 Exec 失败路径。
func TestTask_CancelTask_UpdateFails(t *testing.T) {
	s, err := Open(":memory:")
	require.NoError(t, err)
	defer s.Close()

	id, err := s.CreateTask("t", "in", "")
	require.NoError(t, err)

	_, err = s.DB.Exec(`CREATE TRIGGER fail_task_cancel
		BEFORE UPDATE ON tasks
		BEGIN SELECT RAISE(ABORT, 'injected cancel failure'); END;`)
	require.NoError(t, err)

	err = s.CancelTask(id)
	require.Error(t, err)
}

// TestTask_SetTaskWorktree_UpdateFails 验证 SetTaskWorktree 的 Exec 失败路径。
func TestTask_SetTaskWorktree_UpdateFails(t *testing.T) {
	s, err := Open(":memory:")
	require.NoError(t, err)
	defer s.Close()

	id, err := s.CreateTask("t", "in", "")
	require.NoError(t, err)

	_, err = s.DB.Exec(`CREATE TRIGGER fail_task_wt
		BEFORE UPDATE ON tasks
		BEGIN SELECT RAISE(ABORT, 'injected wt failure'); END;`)
	require.NoError(t, err)

	err = s.SetTaskWorktree(id, "wt-1")
	require.Error(t, err)
}

// TestTask_RequeueStaleTask_UpdateFails 验证 RequeueStaleTask 的 Exec 失败路径。
func TestTask_RequeueStaleTask_UpdateFails(t *testing.T) {
	s, err := Open(":memory:")
	require.NoError(t, err)
	defer s.Close()

	id, err := s.CreateTask("t", "in", "")
	require.NoError(t, err)
	require.NoError(t, s.ClaimTask(id, "w1"))

	// updated_at 是 Unix 时间戳（秒），当前约 1.7 亿，所以 cutoff 必须大于此值
	// 让 WHERE updated_at < cutoff 为真
	_, err = s.DB.Exec(`CREATE TRIGGER fail_task_stale
		BEFORE UPDATE ON tasks
		BEGIN SELECT RAISE(ABORT, 'injected stale failure'); END;`)
	require.NoError(t, err)

	// 使用极大的 cutoff 值确保 updated_at < cutoff
	_, err = s.RequeueStaleTask(id, 9999999999, 3)
	require.Error(t, err)
}

// ---------------------------------------------------------------------------
// ListSessions 边界（limit <= 0）
// ---------------------------------------------------------------------------

// TestListSessions_LimitZeroReturnsAll 验证 limit=0 返回所有活跃 session。
func TestListSessions_LimitZeroReturnsAll(t *testing.T) {
	s, err := Open(":memory:")
	require.NoError(t, err)
	defer s.Close()

	for i := 0; i < 3; i++ {
		_, err := s.CreateSession(fmt.Sprintf("s-%d", i))
		require.NoError(t, err)
	}

	all, err := s.ListSessions(0)
	require.NoError(t, err)
	assert.Len(t, all, 3)

	limited, err := s.ListSessions(2)
	require.NoError(t, err)
	assert.Len(t, limited, 2)
}

// TestListArchivedSessions_LimitZeroReturnsAll 验证 limit=0 返回所有归档 session。
func TestListArchivedSessions_LimitZeroReturnsAll(t *testing.T) {
	s, err := Open(":memory:")
	require.NoError(t, err)
	defer s.Close()

	for i := 0; i < 3; i++ {
		id, err := s.CreateSession(fmt.Sprintf("a-%d", i))
		require.NoError(t, err)
		require.NoError(t, s.SetSessionArchived(id, true))
	}

	all, err := s.ListArchivedSessions(0)
	require.NoError(t, err)
	assert.Len(t, all, 3)

	limited, err := s.ListArchivedSessions(2)
	require.NoError(t, err)
	assert.Len(t, limited, 2)
}

// ---------------------------------------------------------------------------
// SnapshotSessionForRevert 扫描失败路径（通过触发器模拟无法直接 scan 失败，
// 所以测试 invalid session 的情况，验证它返回正确的 "not found" 错误）
// ---------------------------------------------------------------------------

// TestSnapshotSessionForRevert_MissingSession 确保 snapshot 不存在的 session 报错。
func TestSnapshotSessionForRevert_MissingSession(t *testing.T) {
	s, err := Open(":memory:")
	require.NoError(t, err)
	defer s.Close()

	_, err = s.SnapshotSessionForRevert("definitely-nonexistent")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "snapshot session")
}

// TestSnapshotSessionForRevert_MessagesQueryError 验证 snapshotSessionTx 中
// 查询 messages 失败时的错误路径（session.go line 237-239）。
// 通过 DROP TABLE messages 使 tx.Query 失败，覆盖 tx.Query → err != nil 分支。
func TestSnapshotSessionForRevert_MessagesQueryError(t *testing.T) {
	s, err := Open(":memory:")
	require.NoError(t, err)
	defer s.Close()

	sid, err := s.CreateSession("snap-msg-q")
	require.NoError(t, err)
	require.NoError(t, s.AppendMessage(sid, 0, "user", "hi"))

	// 删除 messages 表，使 snapshotSessionTx 中的消息查询失败
	_, err = s.DB.Exec("DROP TABLE messages")
	require.NoError(t, err)

	_, err = s.SnapshotSessionForRevert(sid)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "store: snapshot messages")
}

// ---------------------------------------------------------------------------
// Upgrade/addColumnIfMissing 扩展测试
// ---------------------------------------------------------------------------

// TestMigrate_AddColumnIfMissingTwice 验证 addColumnIfMissing 幂等。
func TestMigrate_AddColumnIfMissingTwice(t *testing.T) {
	s, err := Open(":memory:")
	require.NoError(t, err)
	defer s.Close()

	// 添加测试列
	require.NoError(t, s.addColumnIfMissing("sessions", "_test_dummy", "TEXT"))

	// 再添加（幂等）
	require.NoError(t, s.addColumnIfMissing("sessions", "_test_dummy", "TEXT"))

	// 验证列存在
	cols, err := s.columns("sessions")
	require.NoError(t, err)
	assert.Contains(t, cols, "_test_dummy")

	// 清理
	_, err = s.DB.Exec("ALTER TABLE sessions DROP COLUMN _test_dummy")
	require.NoError(t, err)
}

// TestMigrate_AddColumnError 验证 addColumnIfMissing 在 columns 查询失败时。
func TestMigrate_AddColumnIfMissingOnBadTable(t *testing.T) {
	s, err := Open(":memory:")
	require.NoError(t, err)
	defer s.Close()

	err = s.addColumnIfMissing("nonexistent_table", "col", "TEXT")
	require.Error(t, err)
}

// ---------------------------------------------------------------------------
// Redact 边界
// ---------------------------------------------------------------------------

// TestRedact_NilRedactor 验证 redactor 为 nil 时返回原文本。
func TestRedact_NilRedactor(t *testing.T) {
	s, err := Open(":memory:")
	require.NoError(t, err)
	defer s.Close()

	// redactor 为 nil（默认）
	result := s.redact("sensitive data")
	assert.Equal(t, "sensitive data", result)
}

// ---------------------------------------------------------------------------
// Encode/Decode SessionRevertSnapshot 额外路径
// ---------------------------------------------------------------------------

// TestEncodeSessionRevertSnapshot_RoundTrip 验证完整的 encode→decode 过程。
func TestEncodeSessionRevertSnapshot_RoundTrip(t *testing.T) {
	snap := SessionRevertSnapshot{
		Meta: SessionSummary{
			ID:        "test-session",
			Title:     "test",
			CreatedAt: 100,
			UpdatedAt: 200,
			Turns:     3,
			Archived:  true,
			CostKnown: true,
		},
		Messages: []Message{
			{ID: "m1", SessionID: "test-session", Seq: 0, Role: "user", Content: "hi", CreatedAt: 100},
		},
	}

	blob, err := EncodeSessionRevertSnapshot(snap)
	require.NoError(t, err)
	assert.True(t, json.Valid(blob))

	decoded, err := DecodeSessionRevertSnapshot(blob)
	require.NoError(t, err)
	assert.Equal(t, "test-session", decoded.Meta.ID)
	assert.Equal(t, 3, decoded.Meta.Turns)
	assert.True(t, decoded.Meta.Archived)
}

// TestDecodeSessionRevertSnapshot_NilBlob 验证 nil blob 被拒绝。
func TestDecodeSessionRevertSnapshot_NilBlob(t *testing.T) {
	_, err := DecodeSessionRevertSnapshot(nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no session revert snapshot")
}

// ---------------------------------------------------------------------------
// 工具函数——测试用 trigger 辅助
// ---------------------------------------------------------------------------

// TestHelper_TableExists 验证迁移后关键表存在。
func TestHelper_TableExists(t *testing.T) {
	s, err := Open(":memory:")
	require.NoError(t, err)
	defer s.Close()

	for _, tbl := range []string{"kv", "sessions", "messages", "memories", "tasks", "vcs_repos"} {
		var n int
		err := s.DB.QueryRow("SELECT COUNT(*) FROM " + tbl).Scan(&n)
		assert.NoError(t, err, "table %s should exist", tbl)
	}
}
