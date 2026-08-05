// SQLite-backed persistence for the work package.
//
// ⚠️ Writes do NOT currently route through the injected WriteTxer. The wt()
// helper exists and has zero callers: every write either begins its own
// transaction with s.db.BeginTx or runs a bare s.db.ExecContext, so none of
// them share the process-wide writeMu from store.Store. Concurrent goroutines
// can therefore still hit SQLITE_BUSY here.
//
// The predecessor of this comment asserted the opposite — "all write paths
// route through the injected WriteTxer" — which is the more dangerous kind of
// wrong: it tells a reader the concurrency problem is solved and stops them
// looking. Converting the twelve write sites is W3 Task 5 and is not done.
//
// Migration entry point is FromDB, called by bootstrap.Build after store.Open.
package work

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// SQLite-backed persistence for the work package.
//
// WriteTxer is the interface consumed by Store write methods. *store.Store
// structurally satisfies it, so bootstrap can inject the process-wide Store
// without work importing internal/store.
type WriteTxer interface {
	WriteTx(ctx context.Context, fn func(*sql.Tx) error) error
}

// unlockedWriteTxer is a WriteTxer whose transactions do NOT share the
// process-wide writeMu. Used by tests that construct Store from a bare *sql.DB
// (via FromDB(db, nil)) -- downstream tests that care about WAL concurrency
// should inject a real *store.Store instead.
type unlockedWriteTxer struct{ db *sql.DB }

func (u unlockedWriteTxer) WriteTx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := u.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// Store stores the work's SQLite persistence.
type Store struct {
	db      *sql.DB
	writeTx WriteTxer
}

// wt returns the active WriteTxer or a fallback unlocked one.
func (s *Store) wt() WriteTxer {
	if s.writeTx != nil {
		return s.writeTx
	}
	return unlockedWriteTxer{db: s.db}
}

// FromDB applies the work schema on db and returns a Store.
// writeTx is the process-wide WriteTxer (typically *store.Store).
// Pass nil for tests that do not need the shared writeMu.
func FromDB(db *sql.DB, writeTx WriteTxer) (*Store, error) {
	s := &Store{db: db, writeTx: writeTx}
	if err := s.migrate(); err != nil {
		return nil, err
	}
	return s, nil
}

const workSchema = `
CREATE TABLE IF NOT EXISTS task_work (
    id TEXT PRIMARY KEY,
    title TEXT NOT NULL,
    prompt TEXT NOT NULL,
    status TEXT NOT NULL,
    thread_id TEXT NOT NULL DEFAULT '',
    turn_id TEXT NOT NULL DEFAULT '',
    broker_task_id TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_task_work_thread_created ON task_work(thread_id, created_at DESC);

CREATE TABLE IF NOT EXISTS task_work_checklist (
    task_id TEXT NOT NULL,
    item_id INTEGER NOT NULL,
    content TEXT NOT NULL,
    status TEXT NOT NULL,
    PRIMARY KEY(task_id, item_id),
    FOREIGN KEY(task_id) REFERENCES task_work(id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS task_work_gates (
    task_id TEXT NOT NULL,
    gate TEXT NOT NULL,
    id TEXT NOT NULL,
    command TEXT NOT NULL,
    cwd TEXT NOT NULL,
    exit_code INTEGER NOT NULL,
    duration_ms INTEGER NOT NULL,
    classification TEXT NOT NULL,
    summary TEXT NOT NULL,
    log_artifact_id TEXT NOT NULL DEFAULT '',
    recorded_at INTEGER NOT NULL,
    PRIMARY KEY(task_id, gate),
    FOREIGN KEY(task_id) REFERENCES task_work(id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS task_work_artifacts (
    id TEXT PRIMARY KEY,
    task_id TEXT NOT NULL,
    label TEXT NOT NULL,
    summary TEXT NOT NULL,
    content_ref TEXT NOT NULL,
    size INTEGER NOT NULL,
    created_at INTEGER NOT NULL,
    FOREIGN KEY(task_id) REFERENCES task_work(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_task_work_artifacts_task_created ON task_work_artifacts(task_id, created_at);

CREATE TABLE IF NOT EXISTS task_work_timeline (
    seq INTEGER PRIMARY KEY AUTOINCREMENT,
    task_id TEXT NOT NULL,
    at INTEGER NOT NULL,
    kind TEXT NOT NULL,
    summary TEXT NOT NULL,
    detail_artifact_id TEXT NOT NULL DEFAULT '',
    FOREIGN KEY(task_id) REFERENCES task_work(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_task_work_timeline_task_seq ON task_work_timeline(task_id, seq);
`

func (s *Store) migrate() error {
	_, err := s.db.Exec(workSchema)
	return err
}

// Create 在单事务内插入 task 行、checklist 与初始 timeline。
// 调用方在 Manager.Create 处预置一条 "created" timeline 一起写入。
func (s *Store) Create(ctx context.Context, w *WorkTask) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `INSERT INTO task_work
		(id,title,prompt,status,thread_id,turn_id,broker_task_id,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?)`, w.ID, w.Title, w.Prompt, string(w.Status), w.ThreadID, w.TurnID,
		w.BrokerTaskID, w.CreatedAt.Unix(), w.UpdatedAt.Unix()); err != nil {
		return fmt.Errorf("work: insert task: %w", err)
	}
	for _, item := range w.Checklist.Items {
		if _, err = tx.ExecContext(ctx, `INSERT INTO task_work_checklist(task_id,item_id,content,status) VALUES(?,?,?,?)`,
			w.ID, item.ID, item.Content, string(item.Status)); err != nil {
			return err
		}
	}
	for _, entry := range w.Timeline {
		if _, err = tx.ExecContext(ctx, `INSERT INTO task_work_timeline(task_id,at,kind,summary,detail_artifact_id) VALUES(?,?,?,?,?)`,
			w.ID, entry.At.Unix(), entry.Kind, entry.Summary, entry.DetailArtifactID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Get 读取单个 task 并装配 checklist/gates/artifacts/timeline。
// 不存在时返回 sql.ErrNoRows（与标准库语义一致）。
func (s *Store) Get(ctx context.Context, id string) (*WorkTask, error) {
	var (
		w          WorkTask
		status     string
		threadID   string
		turnID     string
		brokerTask string
		createdAt  int64
		updatedAt  int64
	)
	row := s.db.QueryRowContext(ctx, `SELECT id,title,prompt,status,thread_id,turn_id,broker_task_id,created_at,updated_at
		FROM task_work WHERE id=?`, id)
	if err := row.Scan(&w.ID, &w.Title, &w.Prompt, &status, &threadID, &turnID, &brokerTask, &createdAt, &updatedAt); err != nil {
		return nil, err
	}
	w.Status = Status(status)
	w.ThreadID = threadID
	w.TurnID = turnID
	w.BrokerTaskID = brokerTask
	w.CreatedAt = time.Unix(createdAt, 0)
	w.UpdatedAt = time.Unix(updatedAt, 0)

	rows, err := s.db.QueryContext(ctx, `SELECT item_id,content,status FROM task_work_checklist WHERE task_id=? ORDER BY item_id`, id)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var item ChecklistItem
		var st string
		if err := rows.Scan(&item.ID, &item.Content, &st); err != nil {
			rows.Close()
			return nil, err
		}
		item.Status = ChecklistItemStatus(st)
		w.Checklist.Items = append(w.Checklist.Items, item)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	rows, err = s.db.QueryContext(ctx, `SELECT id,gate,command,cwd,exit_code,duration_ms,classification,summary,log_artifact_id,recorded_at
		FROM task_work_gates WHERE task_id=? ORDER BY recorded_at`, id)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var ev Evidence
		if err := rows.Scan(&ev.ID, &ev.Gate, &ev.Command, &ev.Cwd, &ev.ExitCode, &ev.DurationMs,
			&ev.Classification, &ev.Summary, &ev.LogArtifactID, &ev.RecordedAt); err != nil {
			rows.Close()
			return nil, err
		}
		w.Gates = append(w.Gates, ev)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	rows, err = s.db.QueryContext(ctx, `SELECT id,label,summary,content_ref,size,created_at FROM task_work_artifacts WHERE task_id=? ORDER BY created_at`, id)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var art Artifact
		if err := rows.Scan(&art.ID, &art.Label, &art.Summary, &art.ContentRef, &art.Size, &art.CreatedAt); err != nil {
			rows.Close()
			return nil, err
		}
		art.TaskID = id
		w.Artifacts = append(w.Artifacts, art)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	rows, err = s.db.QueryContext(ctx, `SELECT at,kind,summary,detail_artifact_id FROM task_work_timeline WHERE task_id=? ORDER BY seq`, id)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var (
			entry TimelineEntry
			at    int64
		)
		if err := rows.Scan(&at, &entry.Kind, &entry.Summary, &entry.DetailArtifactID); err != nil {
			rows.Close()
			return nil, err
		}
		entry.At = time.Unix(at, 0)
		w.Timeline = append(w.Timeline, entry)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return &w, nil
}

// List 用单条聚合查询返回 Summary 列表。
// 单条 SQL 避免 N+1；SetMaxOpenConns(1) 下同时只允许一条查询挂起，杜绝死锁。
func (s *Store) List(ctx context.Context, limit int, threadID string) ([]Summary, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	query := `SELECT w.id,w.title,w.status,w.thread_id,w.turn_id,w.created_at,w.updated_at,
		CASE WHEN COALESCE(c.total,0)=0 THEN 0 ELSE c.done*100/c.total END,
		COALESCE(g.gate_count,0)
	FROM task_work w
	LEFT JOIN (
		SELECT task_id,COUNT(*) AS total,SUM(CASE WHEN status='done' THEN 1 ELSE 0 END) AS done
		FROM task_work_checklist GROUP BY task_id
	) c ON c.task_id=w.id
	LEFT JOIN (
		SELECT task_id,COUNT(*) AS gate_count FROM task_work_gates GROUP BY task_id
	) g ON g.task_id=w.id
	WHERE (?='' OR w.thread_id=?)
	ORDER BY w.created_at DESC,w.id DESC LIMIT ?`
	rows, err := s.db.QueryContext(ctx, query, threadID, threadID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Summary, 0)
	for rows.Next() {
		var item Summary
		var status string
		var createdAt, updatedAt int64
		if err := rows.Scan(&item.ID, &item.Title, &status, &item.ThreadID, &item.TurnID,
			&createdAt, &updatedAt, &item.Pct, &item.GateCount); err != nil {
			return nil, err
		}
		item.Status = Status(status)
		item.CreatedAt = time.Unix(createdAt, 0)
		item.UpdatedAt = time.Unix(updatedAt, 0)
		out = append(out, item)
	}
	return out, rows.Err()
}

// Transition 在单事务内做 guarded UPDATE + timeline 追加。
// WHERE id=? AND status=current 的 guarded UPDATE 保证并发转移只能成功一条；
// 失败者拿到 "work: status changed concurrently"。
func (s *Store) Transition(ctx context.Context, id string, next Status, kind, summary string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var current string
	if err := tx.QueryRowContext(ctx, `SELECT status FROM task_work WHERE id=?`, id).Scan(&current); err != nil {
		return err
	}
	if err := Status(current).CanTransitionTo(next); err != nil {
		return err
	}
	now := time.Now().Unix()
	result, err := tx.ExecContext(ctx, `UPDATE task_work SET status=?,updated_at=? WHERE id=? AND status=?`,
		string(next), now, id, current)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows != 1 {
		return errors.New("work: status changed concurrently")
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO task_work_timeline(task_id,at,kind,summary) VALUES(?,?,?,?)`,
		id, now, kind, summary); err != nil {
		return err
	}
	return tx.Commit()
}

// AppendTimeline 追加单条 timeline 并 bump updated_at（dispatch_failed 与后续
// broker-result 回写 hook 都走它）。不与 Create/Transition 共用事务——这两条
// 都是 Manager 在 Store 调用之后、针对已存在 task 的补记。
func (s *Store) AppendTimeline(ctx context.Context, taskID string, entry TimelineEntry) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO task_work_timeline(task_id,at,kind,summary,detail_artifact_id) VALUES(?,?,?,?,?)`,
		taskID, entry.At.Unix(), entry.Kind, entry.Summary, entry.DetailArtifactID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE task_work SET updated_at=? WHERE id=?`,
		entry.At.Unix(), taskID); err != nil {
		return err
	}
	return tx.Commit()
}

// AttachBrokerTask 把 broker task id 盖到 durable task 上；guarded 到恰好一行。
func (s *Store) AttachBrokerTask(ctx context.Context, taskID, brokerTaskID string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE task_work SET broker_task_id=?,updated_at=? WHERE id=?`,
		brokerTaskID, time.Now().Unix(), taskID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return errors.New("work: task not found")
	}
	return nil
}

// SetChecklist 在单事务内 DELETE + 批量 INSERT，整组替换清单。
func (s *Store) SetChecklist(ctx context.Context, taskID string, c Checklist) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM task_work_checklist WHERE task_id=?`, taskID); err != nil {
		return err
	}
	for _, item := range c.Items {
		if _, err := tx.ExecContext(ctx, `INSERT INTO task_work_checklist(task_id,item_id,content,status) VALUES(?,?,?,?)`,
			taskID, item.ID, item.Content, string(item.Status)); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE task_work SET updated_at=? WHERE id=?`, time.Now().Unix(), taskID); err != nil {
		return err
	}
	return tx.Commit()
}

// AddChecklistItem 在事务内以 COALESCE(MAX(item_id),0)+1 分配新 id 并插入。
// 返回新分配的 item_id（从 1 开始）。
func (s *Store) AddChecklistItem(ctx context.Context, taskID, content string) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var nextID int
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(item_id),0)+1 FROM task_work_checklist WHERE task_id=?`, taskID).Scan(&nextID); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO task_work_checklist(task_id,item_id,content,status) VALUES(?,?,?,?)`,
		taskID, nextID, content, string(ChecklistPending)); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE task_work SET updated_at=? WHERE id=?`, time.Now().Unix(), taskID); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return nextID, nil
}

// PatchChecklistItem 用单条 guarded UPDATE（CASE 区分空 content/空 status）；
// 不做 read-modify-write 以避免两 goroutine 互相覆盖。
func (s *Store) PatchChecklistItem(ctx context.Context, taskID string, itemID int, content string, status ChecklistItemStatus) error {
	now := time.Now().Unix()
	result, err := s.db.ExecContext(ctx, `UPDATE task_work_checklist
		SET content=CASE WHEN ?='' THEN content ELSE ? END,
		    status=CASE WHEN ?='' THEN status ELSE ? END
		WHERE task_id=? AND item_id=?`, content, content, string(status), string(status), taskID, itemID)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows != 1 {
		return errors.New("work: checklist item not found")
	}
	_, err = s.db.ExecContext(ctx, `UPDATE task_work SET updated_at=? WHERE id=?`, now, taskID)
	return err
}

// RecordGate 用 INSERT OR REPLACE 替换同 gate 的证据，并追加 timeline。
// timeline summary 形如 "test: pass"，便于用户在 TUI 上扫读。
func (s *Store) RecordGate(ctx context.Context, taskID string, evidence Evidence) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `INSERT OR REPLACE INTO task_work_gates
		(task_id,gate,id,command,cwd,exit_code,duration_ms,classification,summary,log_artifact_id,recorded_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?)`, taskID, evidence.Gate, evidence.ID, evidence.Command, evidence.Cwd,
		evidence.ExitCode, evidence.DurationMs, evidence.Classification, evidence.Summary,
		evidence.LogArtifactID, evidence.RecordedAt); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO task_work_timeline(task_id,at,kind,summary) VALUES(?,?,?,?)`,
		taskID, evidence.RecordedAt, "gate", evidence.Gate+": "+evidence.Classification); err != nil {
		return err
	}
	return tx.Commit()
}

// PutArtifact 写入一个 artifact 的元数据（不含文件内容 —— 文件由 Manager 落盘）。
// artifact.ID 必须由调用方预先生成；重复 ID 触发主键冲突返回 error。
func (s *Store) PutArtifact(ctx context.Context, a Artifact) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO task_work_artifacts(id,task_id,label,summary,content_ref,size,created_at)
		VALUES(?,?,?,?,?,?,?)`, a.ID, a.TaskID, a.Label, a.Summary, a.ContentRef, a.Size, a.CreatedAt)
	return err
}

// GetArtifact 只返回元数据；不存在返回 sql.ErrNoRows。
func (s *Store) GetArtifact(ctx context.Context, id string) (Artifact, error) {
	var (
		a         Artifact
		createdAt int64
	)
	err := s.db.QueryRowContext(ctx, `SELECT id,task_id,label,summary,content_ref,size,created_at FROM task_work_artifacts WHERE id=?`, id).
		Scan(&a.ID, &a.TaskID, &a.Label, &a.Summary, &a.ContentRef, &a.Size, &createdAt)
	if err != nil {
		return Artifact{}, err
	}
	a.CreatedAt = createdAt
	return a, nil
}

// ArtifactBytes 返回指定 task 当前所有 artifact 的累计字节数，用于 quota 校验。
func (s *Store) ArtifactBytes(ctx context.Context, taskID string) (int64, error) {
	var sum sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(size),0) FROM task_work_artifacts WHERE task_id=?`, taskID).Scan(&sum)
	if err != nil {
		return 0, err
	}
	return sum.Int64, nil
}

// DeleteArtifactsBefore 删除 created_at < beforeUnix 的 artifact 元数据，
// 返回被删行的 content_ref 列表，供 janitor 同步清理外部文件。
//
// 实现先完整读取并关闭 rows，再执行 DELETE —— 不能在 SetMaxOpenConns(1)
// 下边查边删，否则会在同一连接上嵌套占用导致死锁。
func (s *Store) DeleteArtifactsBefore(ctx context.Context, beforeUnix int64) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT content_ref FROM task_work_artifacts WHERE created_at<?`, beforeUnix)
	if err != nil {
		return nil, err
	}
	refs := make([]string, 0)
	for rows.Next() {
		var ref string
		if err := rows.Scan(&ref); err != nil {
			rows.Close()
			return nil, err
		}
		refs = append(refs, ref)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(refs) == 0 {
		return refs, nil
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM task_work_artifacts WHERE created_at<?`, beforeUnix); err != nil {
		return nil, err
	}
	return refs, nil
}
