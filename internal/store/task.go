package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Task is a unit of work tracked in the task broker.
type Task struct {
	ID         string
	Type       string
	Input      string
	Status     string
	AssignedTo string
	Result     string
	ParentTask string
	CreatedAt  int64
	UpdatedAt  int64
	Deadline   int64
	Attempts   int64
	// WorktreeID is the VCS worktree the task runs in. Empty means unassigned;
	// the broker fills it on Claim when a VCS is configured. A pre-set id
	// (e.g. from a team plan) is preserved so multiple tasks can share a worktree.
	WorktreeID string
}

// CreateTask inserts a new task with status "pending" and returns its id.
func (s *Store) CreateTask(typ, input, parent string) (string, error) {
	id := newID()
	now := time.Now().Unix()
	err := s.WriteTx(context.Background(), func(tx *sql.Tx) error {
		_, e := tx.Exec(
			`INSERT INTO tasks (id, type, input, status, assigned_to, result, parent_task, created_at, updated_at, deadline, attempts)
			 VALUES (?, ?, ?, 'pending', '', '', ?, ?, ?, 0, 0)`,
			id, typ, input, parent, now, now,
		)
		return e
	})
	if err != nil {
		return "", err
	}
	return id, nil
}

// SetTaskWorktree stamps worktreeID on a task. Used by the broker on Claim to
// attach a freshly created worktree, or by a planner to pre-assign a shared
// worktree id to multiple tasks before they are claimed. Returns an error if
// the task does not exist.
func (s *Store) SetTaskWorktree(id, worktreeID string) error {
	return s.WriteTx(context.Background(), func(tx *sql.Tx) error {
		res, err := tx.Exec(
			"UPDATE tasks SET worktree_id = ? WHERE id = ?",
			worktreeID, id,
		)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return errors.New("task not found")
		}
		return nil
	})
}

// ClaimTask atomically transitions a task from "pending" to "running"
// and sets assigned_to to the given worker. Returns an error if the
// task is not in the pending state (already claimed or missing).
func (s *Store) ClaimTask(id, worker string) error {
	return s.WriteTx(context.Background(), func(tx *sql.Tx) error {
		now := time.Now().Unix()
		res, err := tx.Exec(
			"UPDATE tasks SET status = 'running', assigned_to = ?, updated_at = ? WHERE id = ? AND status = 'pending'",
			worker, now, id,
		)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return errors.New("task not pending or not found")
		}
		return nil
	})
}

// SetTaskResult sets the status and result of a task, updating updated_at.
func (s *Store) SetTaskResult(id, status, result string) error {
	return s.WriteTx(context.Background(), func(tx *sql.Tx) error {
		now := time.Now().Unix()
		res, err := tx.Exec(
			"UPDATE tasks SET status = ?, result = ?, updated_at = ? WHERE id = ?",
			status, result, now, id,
		)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return errors.New("task not found")
		}
		return nil
	})
}

// GetTask retrieves a task by id. Returns an error if not found.
func (s *Store) GetTask(id string) (Task, error) {
	var t Task
	err := s.DB.QueryRow(
		`SELECT id, type, input, status, assigned_to, result, parent_task, created_at, updated_at, deadline, attempts, worktree_id
		 FROM tasks WHERE id = ?`,
		id,
	).Scan(&t.ID, &t.Type, &t.Input, &t.Status, &t.AssignedTo, &t.Result, &t.ParentTask, &t.CreatedAt, &t.UpdatedAt, &t.Deadline, &t.Attempts, &t.WorktreeID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Task{}, errors.New("task not found")
		}
		return Task{}, err
	}
	return t, nil
}

// TouchTask updates the updated_at timestamp of a task without changing
// its status. Used by the broker's Heartbeat method.
func (s *Store) TouchTask(id string) error {
	return s.WriteTx(context.Background(), func(tx *sql.Tx) error {
		now := time.Now().Unix()
		res, err := tx.Exec(
			"UPDATE tasks SET updated_at = ? WHERE id = ?",
			now, id,
		)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return errors.New("task not found")
		}
		return nil
	})
}

// ListStaleRunning returns tasks with status "running" whose updated_at is
// older than the given Unix timestamp. Used by the broker's RequeueStale
// to find tasks whose worker has stopped sending heartbeats.
func (s *Store) ListStaleRunning(before int64) ([]Task, error) {
	rows, err := s.DB.Query(
		`SELECT id, type, input, status, assigned_to, result, parent_task, created_at, updated_at, deadline, attempts, worktree_id
		 FROM tasks WHERE status = 'running' AND updated_at < ? ORDER BY created_at ASC`,
		before,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Task
	for rows.Next() {
		var t Task
		if err := rows.Scan(&t.ID, &t.Type, &t.Input, &t.Status, &t.AssignedTo, &t.Result, &t.ParentTask, &t.CreatedAt, &t.UpdatedAt, &t.Deadline, &t.Attempts, &t.WorktreeID); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ErrNotRunningOrOwned is returned by guarded update methods when the task
// is not in the running state or not owned by the specified worker. This
// allows callers to distinguish ownership conflicts from genuine DB errors.
var ErrNotRunningOrOwned = errors.New("task not running or not owned by worker")

// RequeueTask atomically increments attempts and sets the task back to
// pending (clearing assigned_to and result), guarded by the condition
// that the task is still running and assigned to the given worker.
// Returns ErrNotRunningOrOwned if the guard fails.
func (s *Store) RequeueTask(id, worker string) error {
	return s.WriteTx(context.Background(), func(tx *sql.Tx) error {
		now := time.Now().Unix()
		res, err := tx.Exec(
			"UPDATE tasks SET status = 'pending', result = '', assigned_to = '', attempts = attempts + 1, updated_at = ? WHERE id = ? AND status = 'running' AND assigned_to = ?",
			now, id, worker,
		)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return ErrNotRunningOrOwned
		}
		return nil
	})
}

// FinalizeTask sets a final status and result on a task, guarded by the
// condition that the task is still running and assigned to the given worker.
// Returns ErrNotRunningOrOwned if the guard fails.
func (s *Store) FinalizeTask(id, worker, status, result string) error {
	return s.WriteTx(context.Background(), func(tx *sql.Tx) error {
		now := time.Now().Unix()
		res, err := tx.Exec(
			"UPDATE tasks SET status = ?, result = ?, updated_at = ? WHERE id = ? AND status = 'running' AND assigned_to = ?",
			status, result, now, id, worker,
		)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return ErrNotRunningOrOwned
		}
		return nil
	})
}

// IncrementAttempts increments the attempts counter of a task.
func (s *Store) IncrementAttempts(id string) error {
	return s.WriteTx(context.Background(), func(tx *sql.Tx) error {
		res, err := tx.Exec(
			"UPDATE tasks SET attempts = attempts + 1 WHERE id = ?",
			id,
		)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return errors.New("task not found")
		}
		return nil
	})
}

// CancelTask 把一条 pending 或 running task 标记为 cancelled；其它状态
// （completed/failed/cancelled/不存在）都返回 error。SQL guarded 到状态
// 白名单，让并发 RecordResult 与 Cancel 互相竞争时只一条成功。
//
// 由 internal/task.Broker.Cancel 唯一调用 —— work.Manager 不直接 UPDATE
// tasks 表（依赖方向：work → Dispatcher → broker → store）。
func (s *Store) CancelTask(id string) error {
	return s.WriteTx(context.Background(), func(tx *sql.Tx) error {
		result, err := tx.Exec(
			`UPDATE tasks SET status='cancelled',updated_at=? WHERE id=? AND status IN ('pending','running')`,
			time.Now().Unix(), id,
		)
		if err != nil {
			return err
		}
		rows, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if rows != 1 {
			return errors.New("task not cancellable or not found")
		}
		return nil
	})
}

// RequeueStaleTask atomically requeues a stale running task in a single
// guarded UPDATE. The guard ensures the task is still in 'running' status AND
// its updated_at is still older than the cutoff timestamp — if a worker
// already finalized it (completed/failed), re-claimed it, or sent a fresh
// heartbeat (bumping updated_at past the cutoff), the UPDATE matches 0 rows
// and changed=false, so the sweeper skips it. This closes the TOCTOU window
// between ListStaleRunning (snapshot) and this UPDATE: a task that was stale
// at snapshot time but has since been heartbeated will not be clobbered.
// When changed, attempts is incremented; if the new attempt count exceeds
// maxRetries the task is set to 'failed' (preserving any existing result),
// otherwise it is set to 'pending' with a cleared result for re-claim.
func (s *Store) RequeueStaleTask(id string, cutoff int64, maxRetries int) (changed bool, err error) {
	err = s.WriteTx(context.Background(), func(tx *sql.Tx) error {
		now := time.Now().Unix()
		res, e := tx.Exec(
			`UPDATE tasks
			 SET attempts = attempts + 1,
			     status = CASE WHEN attempts + 1 > ? THEN 'failed' ELSE 'pending' END,
			     assigned_to = '',
			     result = CASE WHEN attempts + 1 > ? THEN result ELSE '' END,
			     updated_at = ?
			 WHERE id = ? AND status = 'running' AND updated_at < ?`,
			maxRetries, maxRetries, now, id, cutoff,
		)
		if e != nil {
			return e
		}
		n, e := res.RowsAffected()
		if e != nil {
			return e
		}
		changed = n > 0
		return nil
	})
	return
}

// ListPending returns up to limit tasks with status "pending", ordered by creation time.
func (s *Store) ListPending(limit int) ([]Task, error) {
	rows, err := s.DB.Query(
		`SELECT id, type, input, status, assigned_to, result, parent_task, created_at, updated_at, deadline, attempts, worktree_id
		 FROM tasks WHERE status = 'pending' ORDER BY created_at ASC LIMIT ?`,
		limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Task
	for rows.Next() {
		var t Task
		if err := rows.Scan(&t.ID, &t.Type, &t.Input, &t.Status, &t.AssignedTo, &t.Result, &t.ParentTask, &t.CreatedAt, &t.UpdatedAt, &t.Deadline, &t.Attempts, &t.WorktreeID); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}
