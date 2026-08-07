package work

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// This file holds the two halves of a durable task's lifecycle that had no
// production path before: the broker→work mirror, and restart recovery.
//
// Manager.Start and Manager.Finish existed and were tested, but nothing called
// them. A durable task therefore sat at "pending" from creation to eternity
// while the broker row behind it went pending → running → completed, and
// task_read reported "pending" for a task that had already finished. Both the
// state machine and the "restart recovers" claim were true of code nobody ran.

// ---------------------------------------------------------------------------
// broker → work mirror
// ---------------------------------------------------------------------------

// LifecycleMirror implements internal/task.WorkLifecycle on top of a Manager.
//
// It converts the broker's string vocabulary into work.Status and swallows
// errors by design: the broker calls it AFTER a transition it has already
// committed, so returning an error here could not undo anything, and blocking
// the broker on a durable-row write would let a slow work store stall task
// dispatch. Failures go to OnError instead, where the composition root can log
// them.
//
// A transition the state machine rejects is NOT an error worth surfacing: the
// broker retries and a task can legitimately be cancelled from the work side
// while its broker row is still finishing, so "already terminal" is a race the
// caller loses on purpose.
type LifecycleMirror struct {
	mgr *Manager
	// OnError, when set, receives write failures. Optional.
	OnError func(workTaskID string, err error)
	// OnTransition, when set, is called after a transition this mirror
	// SUCCESSFULLY wrote. Optional.
	//
	// It exists because the durable row was the only place a transition
	// landed: a user watching the TUI saw a task go to "pending" and stay
	// there until they ran task_read again, no matter what the worker did.
	// The tool-layer event path cannot carry these — it needs a callback bound
	// into a turn context, and by the time a worker finishes there is no turn.
	//
	// Called synchronously on the broker's worker goroutine, so an
	// implementation that blocks stalls task dispatch. The composition root
	// wires it to a non-blocking broadcast.
	OnTransition func(workTaskID string)
}

// NewLifecycleMirror wraps mgr so the broker can report transitions into it.
func NewLifecycleMirror(mgr *Manager) *LifecycleMirror {
	return &LifecycleMirror{mgr: mgr}
}

func (l *LifecycleMirror) report(id string, err error) {
	if err == nil {
		if l.OnTransition != nil {
			l.OnTransition(id)
		}
		return
	}
	if l.OnError == nil {
		return
	}
	// An illegal transition means the durable row moved on without us (most
	// often a cancel from the work side). Not a fault.
	if errors.Is(err, ErrIllegalTransition) {
		return
	}
	l.OnError(id, err)
}

// OnRunning moves the durable task to running.
func (l *LifecycleMirror) OnRunning(workTaskID string) {
	l.report(workTaskID, l.mgr.Start(context.Background(), workTaskID))
}

// OnTerminal moves the durable task to its final status. The broker's
// "timeout" maps to failed — from the durable row's point of view a task that
// ran out of time did not complete.
func (l *LifecycleMirror) OnTerminal(workTaskID, status, note string) {
	final := StatusFailed
	if status == "completed" {
		final = StatusCompleted
	}
	if status == "timeout" && note == "" {
		note = "worker timed out"
	}
	l.report(workTaskID, l.mgr.Finish(context.Background(), workTaskID, final, note))
}

// OnRequeued returns the durable task to pending so it does not sit at
// "running" while it waits in the queue for another attempt.
func (l *LifecycleMirror) OnRequeued(workTaskID string) {
	l.report(workTaskID, l.mgr.store.Transition(context.Background(), workTaskID,
		StatusPending, "requeued", "attempt failed; returned to the queue"))
}

// ---------------------------------------------------------------------------
// restart recovery
// ---------------------------------------------------------------------------

// RecoverInterrupted returns every task left at "running" by a previous
// process to "pending", and reports how many it moved.
//
// A task is "running" only while a worker holds it. If the process died, no
// worker holds anything, so a row still marked running after a restart is
// describing a worker that no longer exists — it would stay that way forever,
// invisible to ListPending and never re-claimed. registry.NewManager has done
// the equivalent for sub-agents since it was written (it rewrites Running to
// Interrupted on a new boot); the work store had no counterpart at all, which
// is what made "重启后持久恢复" a claim about the schema rather than about
// any behaviour.
//
// pending rather than a distinct "interrupted" status: the durable queue's
// contract is that pending work gets picked up, and a status no consumer
// filters on would just be a second way to be stuck. The timeline entry is
// what preserves the fact that an interruption happened.
//
// Callers run this once at startup, before the broker's sweeper starts.
func (m *Manager) RecoverInterrupted(ctx context.Context) (int, error) {
	ids, err := m.store.runningIDs(ctx)
	if err != nil {
		return 0, err
	}
	moved := 0
	for _, id := range ids {
		if err := m.store.Transition(ctx, id, StatusPending, "recovered",
			"process restarted while this task was running"); err != nil {
			// A task that reached a terminal status between the query and the
			// transition is not a failure — it simply no longer needs
			// recovering.
			if errors.Is(err, sql.ErrNoRows) || errors.Is(err, ErrIllegalTransition) {
				continue
			}
			return moved, fmt.Errorf("work: recover %s: %w", id, err)
		}
		moved++
	}
	return moved, nil
}

// runningIDs lists the ids of every task currently marked running.
func (s *Store) runningIDs(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id FROM task_work WHERE status=? ORDER BY created_at`, string(StatusRunning))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
