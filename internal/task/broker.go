// Package task provides the task broker that wraps the store with lifecycle
// management: submission, claiming, result recording, retry logic, and
// heartbeat-timeout requeue via a background sweeper.
package task

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/x6nux/yanshi/internal/store"
	"github.com/x6nux/yanshi/internal/vcs"
)

// ErrInvalidStatus is returned by RecordResult when the status is not one
// of the allowed values: completed, failed, timeout.
var ErrInvalidStatus = errors.New("task: invalid status")

// ErrNotOwner is returned by RecordResult when the task is not running or
// not assigned to the calling worker.
var ErrNotOwner = errors.New("task: not running or not owned by caller")

// Broker wraps a Store with task lifecycle management including retry
// logic and heartbeat-timeout detection.
type Broker struct {
	store            *store.Store
	maxRetries       int
	heartbeatTimeout time.Duration
	notify           chan struct{}
	// VCS, when set with RepoID, enables per-task worktree assignment on Claim.
	// A task with a pre-set worktree_id keeps it (shared worktree); a task with
	// none gets a freshly created worktree stamped on it. Both fields are
	// optional so existing NewBroker callers are unaffected.
	VCS    *vcs.VCS
	RepoID string
// createdWT tracks worktrees the broker itself created in Claim (taskID →
// worktreeID) so terminal paths can reclaim them. Lifecycle:
//   ╔═══════════╗   Claim   ╔════════════════╗   terminal (finalize /   ╔═══════════════╗
//   ║  no VCS / ║ ──► ───── ─║ createdWT[id] ║ ──► cancel / requeue-  ──║ reclaimWorktree║
//   ║ pre-set wt║  skip map  ║  = wtID       ║    failed)              ║ delete + 移除  ║
//   ╚═══════════╝            ╚════════════════╝                         ╚═══════════════╝
//   non-terminal requeue → KEEP worktree (reusable by re-claim)
	createdWT   map[string]string
	createdWTMu sync.Mutex
}

// NewBroker creates a Broker backed by the given store.
// maxRetries is the maximum number of retry attempts before a failed task
// is marked as permanently failed.
// hbTimeout is the duration after which a running task without heartbeats
// is considered stale and requeued.
func NewBroker(s *store.Store, maxRetries int, hbTimeout time.Duration) *Broker {
	return &Broker{
		store:            s,
		maxRetries:       maxRetries,
		heartbeatTimeout: hbTimeout,
		notify:           make(chan struct{}, 1),
		createdWT:        make(map[string]string),
	}
}

// SetVCS enables per-task worktree assignment on Claim. When a worker claims a
// task that has no worktree_id, the broker creates a worktree in v for RepoID
// and stamps its id on the task. Tasks pre-assigned a worktree_id keep it.
// Passing a nil v disables the feature.
func (b *Broker) SetVCS(v *vcs.VCS, repoID string) {
	b.VCS = v
	b.RepoID = repoID
}

// Submit creates a new pending task and signals that a task is available.
func (b *Broker) Submit(typ, input, parent string) (string, error) {
	id, err := b.store.CreateTask(typ, input, parent)
	if err != nil {
		return "", err
	}
	// Non-blocking send to the notify channel
	select {
	case b.notify <- struct{}{}:
	default:
	}
	return id, nil
}

// Claim grabs the next pending task for a worker. Returns (nil, nil) if no
// pending tasks are available. When a VCS is configured (SetVCS) and the task
// has no pre-assigned worktree_id, a new worktree is created and stamped on the
// task (best-effort: on failure the task still runs without one). A task that
// already carries a worktree_id (e.g. from a team plan) keeps it unchanged,
// enabling shared worktrees (multiple tasks → one worktree).
func (b *Broker) Claim(worker string) (*store.Task, error) {
	pending, err := b.store.ListPending(1)
	if err != nil {
		return nil, err
	}
	if len(pending) == 0 {
		return nil, nil
	}
	t := pending[0]
	if err := b.store.ClaimTask(t.ID, worker); err != nil {
		return nil, err
	}
	got, err := b.store.GetTask(t.ID)
	if err != nil {
		return nil, err
	}
	// Best-effort worktree assignment. A pre-set id (shared worktree) is
	// preserved; only an empty worktree_id triggers creation. Any failure
	// (worktree creation or the stamp UPDATE) is swallowed so the task still
	// runs without a worktree.
	if b.VCS != nil && b.RepoID != "" && got.WorktreeID == "" {
		if wt, werr := b.VCS.AddWorktree(b.RepoID, []string{worker}); werr == nil {
			if serr := b.store.SetTaskWorktree(got.ID, wt.ID); serr == nil {
				got.WorktreeID = wt.ID
				// Record that the broker created this worktree so
				// RecordResult can reclaim it on terminal finalize. Shared
				// (pre-set) worktrees are never recorded and are preserved.
				b.createdWTMu.Lock()
				b.createdWT[got.ID] = wt.ID
				b.createdWTMu.Unlock()
			}
		}
	}
	return &got, nil
}

// RecordResult records the outcome of a task. The worker must be the
// currently assigned owner of a running task. status must be one of
// "completed", "failed", or "timeout". If the status is "failed" or
// "timeout" and the task hasn't exceeded maxRetries, it is requeued as
// pending for another attempt. Otherwise the status is set as final.
// Returns ErrInvalidStatus for unknown statuses, ErrNotOwner if the task
// is not running or not assigned to worker.
func (b *Broker) RecordResult(id, worker, status, result string) error {
	switch status {
	case "completed", "failed", "timeout":
	default:
		return ErrInvalidStatus
	}

	t, err := b.store.GetTask(id)
	if err != nil {
		return err
	}
	if t.Status != "running" || t.AssignedTo != worker {
		return ErrNotOwner
	}

	if (status == "failed" || status == "timeout") && t.Attempts < int64(b.maxRetries) {
		if err := b.store.RequeueTask(id, worker); err != nil {
			if errors.Is(err, store.ErrNotRunningOrOwned) {
				return ErrNotOwner
			}
			return err
		}
		return nil
	}
	if err := b.store.FinalizeTask(id, worker, status, result); err != nil {
		if errors.Is(err, store.ErrNotRunningOrOwned) {
			return ErrNotOwner
		}
		return err
	}
	// Terminal transition succeeded: reclaim any worktree the broker itself
	// created for this task.
	b.reclaimWorktree(id)
	return nil
}

// reclaimWorktree removes the broker-created worktree (if any) for task id from
// both the createdWT bookkeeping map and the VCS. Shared (pre-set) worktrees
// are never in createdWT and are left untouched. Operation is under createdWTMu
// so it is mutually exclusive with the same path in Cancel / RecordResult and
// the future RequeueStale terminal path. Callers MUST have confirmed the task
// reached a terminal status before calling. Removal is best-effort: delete is
// idempotent and RemoveWorktree errors are swallowed (no logger in this pkg).
func (b *Broker) reclaimWorktree(id string) {
	b.createdWTMu.Lock()
	wtID, created := b.createdWT[id]
	if created {
		delete(b.createdWT, id)
	}
	b.createdWTMu.Unlock()
	if created && b.VCS != nil {
		_ = b.VCS.RemoveWorktree(wtID)
	}
}

// Heartbeat touches the updated_at timestamp of a task, indicating the
// worker is still active.
func (b *Broker) Heartbeat(id string) error {
	return b.store.TouchTask(id)
}

// Get retrieves a task by id.
func (b *Broker) Get(id string) (*store.Task, error) {
	t, err := b.store.GetTask(id)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// Cancel 把一条 pending 或 running task 标记为 cancelled，并回收 broker 自己
// 在 Claim 时创建的 worktree（共享 worktree 不回收）。
//
// broker 不做任何额外的 UPDATE tasks —— CancelTask 是唯一变更点，
// 让 work.BrokerAdapter.Cancel 能无副作用委托到这里。
func (b *Broker) Cancel(id string) error {
	if err := b.store.CancelTask(id); err != nil {
		return err
	}
	// Reclaim any worktree the broker created for this task.
	b.reclaimWorktree(id)
	return nil
}

// RequeueStale finds running tasks whose updated_at is older than the
// heartbeat timeout and requeues them. For each stale task:
//   - attempts is incremented atomically
//   - if attempts > maxRetries → status set to "failed"
//   - otherwise → status set to "pending" (available for re-claim)
//
// The requeue is guarded by WHERE status='running' so that a task a worker
// has already finalized (completed/failed) is never clobbered back to pending.
func (b *Broker) RequeueStale(ctx context.Context) error {
	cutoff := time.Now().Add(-b.heartbeatTimeout).Unix()
	stale, err := b.store.ListStaleRunning(cutoff)
	if err != nil {
		return err
	}
	for _, t := range stale {
		changed, err := b.store.RequeueStaleTask(t.ID, cutoff, b.maxRetries)
		if err != nil {
			return err
		}
		if !changed {
			// Task was already finalized or re-claimed — skip.
			continue
		}
		// RequeueStaleTask atomically set the status to either 'failed'
		// (exceeded maxRetries) or 'pending' (within retry budget). Only
		// terminal statuses reclaim the broker-created worktree; pending
		// keeps it for the retry.
		t2, err := b.store.GetTask(t.ID)
		if err != nil {
			return err
		}
		if t2.Status == "failed" {
			b.reclaimWorktree(t.ID)
		}
		// Signal that a task is available for claiming
		select {
		case b.notify <- struct{}{}:
		default:
		}
	}
	return nil
}

// StartSweeper launches a background goroutine that periodically calls
// RequeueStale. It runs until ctx is cancelled.
func (b *Broker) StartSweeper(ctx context.Context, interval time.Duration) {
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				_ = b.RequeueStale(ctx)
			}
		}
	}()
}

// Notify returns a channel that receives a signal when a task is submitted
// or requeued. The channel is buffered (size 1) and non-blocking on the
// sender side.
func (b *Broker) Notify() <-chan struct{} {
	return b.notify
}
