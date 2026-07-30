// internal/vcs/seam.go
//
// seam 是"逐轮回滚 UI"(B2-RB1)的核心抽象:一条命名指针指向 vcs_commits.id,
// 加上 turn_seq / history_len / kind / label 元数据。seam 行开销 = 1 行 SQLite,
// commit 本身已内容寻址 + 增量 delta 存储(writeCommitInTx),所以快照成本极低。
//
// 生命周期:
//   pre-turn   — user_message 进入主循环、user msg 已 append 到 cs.history、
//                模型尚未运行时打。CommitID = 当前 main_head(若有 pending
//                则先 fold 成新 commit)。HistoryLen = len(cs.history)(含 user msg)。
//   post-turn  — turn 结束(无论正常 / model error / tool error / cancel / disconnect)
//                时打。CommitID = turn 后 main_head。HistoryLen = len(cs.history)。
//   pre-revert — RevertToSeam 执行前打(UNDO seam),CommitID = previousHead。
//                返回给调用方用于"撤销本次回滚"。
//   post-revert — RevertToSeam 执行后打(AUDIT seam),CommitID = targetCommit。
//                仅作审计,不用于再次回滚。
//
// 排序:vcs_seams.seq 是单调自增主键(必修项 H),是 ListSeams 的 SOLE 排序键。
// created_at 仅作展示(秒级精度不足以稳定排序同秒插入)。
//
// 并发:SealMainTurnSeam 持有 per-repo 锁(必修项 B),串行化"flush pending → read
// head → insert seam"复合操作;FindSeam / ListSeams 是只读 DB 查询,不加 per-repo 锁。

package vcs

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// SeamKind 标识 seam 在 turn / revert 生命周期中的位置。
type SeamKind string

// Seam lifecycle positions within a turn/revert cycle.
const (
	SeamPreTurn    SeamKind = "pre-turn"
	SeamPostTurn   SeamKind = "post-turn"
	SeamPreRevert  SeamKind = "pre-revert"  // UNDO seam: 指向 previousHead
	SeamPostRevert SeamKind = "post-revert" // AUDIT seam: 指向 target commit
)

// Seam 是 vcs_seams 一行的轻量视图。
type Seam struct {
	ID          string
	RepoID      string
	SessionID   string
	CommitID    string
	TurnSeq     int
	HistoryLen  int
	// PrevTurnSeq / PrevHistoryLen are set only on SeamPreRevert (undo)
	// seams. They capture the conversation boundary BEFORE the revert, so
	// restoring the undo seam can restore the longer pre-revert history (D2).
	PrevTurnSeq    int
	PrevHistoryLen int
	// HistorySnapshot is an opaque JSON-encoded store.SessionRevertSnapshot.
	// It is non-empty only on SeamPreRevert and is never sent over proto.
	HistorySnapshot []byte
	Kind            SeamKind
	Label           string
	Seq             int64 // monotonic insertion order
	CreatedAt       int64
}

// SealMainTurnSeam atomically (under the per-repo lock): if main has pending edits, folds
// them into a new commit; then reads the resulting main_head; then inserts a
// seam row referencing that head with the given kind/label. Returns the new
// seam id.
//
// sessionID scopes the seam to a logical chat session (WS connection); turnSeq
// is cs.turns at seal time (pre-turn: before increment; post-turn: after);
// historyLen is len(cs.history) at seal time (revert uses it to truncate).
//
// Empty repoID is a silent no-op (returns "", nil) — the caller
// (Server.sealTurnBoundary) uses this to skip when VCS is unconfigured.
func (v *VCS) SealMainTurnSeam(repoID, sessionID string, turnSeq, historyLen int, kind SeamKind, label string) (string, error) {
	if repoID == "" {
		return "", nil
	}
	unlock := v.lockRepo(repoID)
	defer unlock()
	return v.sealMainTurnSeamLocked(repoID, sessionID, turnSeq, historyLen, kind, label)
}

// sealMainTurnSeamLocked is the repo-lock-held core. Callers that ALREADY hold
// the repo lock call this to avoid re-locking. The mutex protects: (a) pending-changeset
// query, (b) commitScope's tx, (c) main_head read, (d) seam INSERT — a concurrent
// CommitMain between any of these would produce a seam referencing a stale head.
func (v *VCS) sealMainTurnSeamLocked(repoID, sessionID string, turnSeq, historyLen int, kind SeamKind, label string) (string, error) {
	r, err := v.getRepo(repoID)
	if err != nil {
		return "", err
	}
	head := r.MainHead

	// Fold pending edits so the seam references a STABLE head. ErrNoChanges is
	// benign — nothing to fold. commitScope is the same path CommitMain takes;
	// safe to call here because we hold the repo lock and commitScope does NOT re-lock.
	if len(v.Uncommitted("main", repoID)) > 0 {
		newHead, cErr := v.commitScope("main", repoID, repoID, "", "orchestrator", label)
		if cErr == nil {
			head = newHead
		} else if !errors.Is(cErr, ErrNoChanges) {
			return "", fmt.Errorf("vcs: seal %s: commit: %w", kind, cErr)
		}
	}

	seamID := newVCSID()
	now := time.Now().Unix()
	if _, err := v.store.DB.Exec(
		`INSERT INTO vcs_seams (id, repo_id, session_id, commit_id, turn_seq, history_len, prev_turn_seq, prev_history_len, history_snapshot, kind, label, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		seamID, repoID, sessionID, head, turnSeq, historyLen, 0, 0, []byte{}, string(kind), label, now,
	); err != nil {
		return "", fmt.Errorf("vcs: seal %s: insert: %w", kind, err)
	}
	return seamID, nil
}

// FindSeam loads a single seam by id. Returns (Seam{}, sql.ErrNoRows) when absent.
// Read-only — does NOT take the per-repo lock (DB reads are serialized by SetMaxOpenConns(1)).
func (v *VCS) FindSeam(seamID string) (Seam, error) {
	var s Seam
	var kind string
	err := v.store.DB.QueryRow(
		`SELECT id, repo_id, session_id, commit_id, turn_seq, history_len,
		        prev_turn_seq, prev_history_len, history_snapshot, kind, label, seq, created_at
		 FROM vcs_seams WHERE id = ?`,
		seamID,
	).Scan(&s.ID, &s.RepoID, &s.SessionID, &s.CommitID, &s.TurnSeq, &s.HistoryLen,
		&s.PrevTurnSeq, &s.PrevHistoryLen, &s.HistorySnapshot, &kind, &s.Label, &s.Seq, &s.CreatedAt)
	if err != nil {
		return Seam{}, err
	}
	s.Kind = SeamKind(kind)
	return s, nil
}

// ListSeams returns seams for repoID newest-first (ORDER BY seq DESC). When
// sessionID != "" the result is filtered to that session (必修项 J). limit <= 0
// means a default cap of 50. Read-only — does NOT take the per-repo lock.
func (v *VCS) ListSeams(repoID, sessionID string, limit int) ([]Seam, error) {
	if limit <= 0 {
		limit = 50
	}
	var (
		rows *sql.Rows
		err  error
	)
	if sessionID != "" {
		rows, err = v.store.DB.Query(
			`SELECT id, repo_id, session_id, commit_id, turn_seq, history_len,
			        prev_turn_seq, prev_history_len, history_snapshot, kind, label, seq, created_at
			 FROM vcs_seams WHERE repo_id = ? AND session_id = ?
			 ORDER BY seq DESC LIMIT ?`,
			repoID, sessionID, limit,
		)
	} else {
		rows, err = v.store.DB.Query(
			`SELECT id, repo_id, session_id, commit_id, turn_seq, history_len,
			        prev_turn_seq, prev_history_len, history_snapshot, kind, label, seq, created_at
			 FROM vcs_seams WHERE repo_id = ?
			 ORDER BY seq DESC LIMIT ?`,
			repoID, limit,
		)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Seam
	for rows.Next() {
		var s Seam
		var kind string
		if err := rows.Scan(&s.ID, &s.RepoID, &s.SessionID, &s.CommitID, &s.TurnSeq, &s.HistoryLen,
			&s.PrevTurnSeq, &s.PrevHistoryLen, &s.HistorySnapshot, &kind, &s.Label, &s.Seq, &s.CreatedAt); err != nil {
			return nil, err
		}
		s.Kind = SeamKind(kind)
		out = append(out, s)
	}
	return out, rows.Err()
}
