// internal/vcs/worktreestate.go
//
// V7: a ledger for "did this branch finish safely?"
//
// # The gap
//
// AddWorktree creates a branch and a working dir; MergeToMain folds it back;
// RemoveWorktree flips active=0. Between those there was no record of INTENT.
// A sub-agent that crashed mid-run left a worktree row with active=1, a
// directory full of files under ~/.yanshi/worktrees/, and nothing anywhere
// saying whether its work had been merged, abandoned, or was still running.
// The only way to tell was for a human to look.
//
// This file adds the missing state: a lifecycle value, the owning process, and
// a heartbeat — enough to answer "is anyone still working on this?" mechanically.
//
// # Why a sidecar table instead of columns on vcs_worktrees
//
// vcs_worktrees is declared by internal/store's schema. Adding columns from
// here would mean this package issuing ALTER TABLE against a table another
// package owns, and ALTER TABLE ADD COLUMN is not idempotent — two yanshi
// processes opening the same database concurrently would race, one of them
// getting "duplicate column name". CREATE TABLE IF NOT EXISTS has no such
// problem: SQLite makes it atomic, so the sidecar needs no coordination at all.
//
// The join is a LEFT JOIN with defaults, which also solves migration: every
// worktree that predates this file reads as WorktreeActive with no owner,
// which is exactly what it is.
//
// # Why the PID check is injected rather than imported
//
// internal/lockfile already has the platform-specific liveness probe
// (alive_unix.go / alive_windows.go) and it should not be written twice. But
// internal/vcs is a port package and GOV1's allowlist (archtest deps_test.go
// portAllowlists) admits only auth, execpolicy, guard, secrets and store —
// importing lockfile here would fail the architecture gate. So the probe is a
// function value the composition root supplies (SetProcessAlive).
//
// The default is the FAIL-SAFE direction: with no probe installed, every owner
// is assumed alive and nothing is ever reported orphaned. An unwired build
// therefore reports no orphans rather than proposing to delete live work.

package vcs

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"sort"
	"time"
)

// WorktreeLifecycle is where a worktree stands in its life.
type WorktreeLifecycle string

// The four lifecycle states. They are stored verbatim, so the strings are part
// of the on-disk format and must not be renamed without a migration.
const (
	// WorktreeActive means an owner is (or was) working in it and it has not
	// been merged or given up on. It is the default for any worktree with no
	// state row.
	WorktreeActive WorktreeLifecycle = "active"
	// WorktreeMerged means its work reached main. Set by MarkWorktreeMerged,
	// which MergeToMain calls on success.
	WorktreeMerged WorktreeLifecycle = "merged"
	// WorktreeAbandoned means someone decided deliberately not to merge it.
	// Distinct from orphaned: abandonment is a choice with a record.
	WorktreeAbandoned WorktreeLifecycle = "abandoned"
	// WorktreeOrphaned means its owning process is gone and its work never
	// reached main. It is a CONCLUSION drawn by ScanOrphanWorktrees, not a
	// state anyone sets by hand.
	WorktreeOrphaned WorktreeLifecycle = "orphaned"
)

// ErrWorktreeNotOrphaned guards CleanupOrphanWorktree against removing a
// worktree that the orphan scan does not currently name.
var ErrWorktreeNotOrphaned = errors.New("vcs: worktree is not an orphan")

// WorktreeState is a worktree plus its lifecycle ledger entry.
type WorktreeState struct {
	Worktree
	Lifecycle WorktreeLifecycle
	// OwnerPID is the process that claimed this worktree, or 0 when unclaimed.
	OwnerPID int
	// HeartbeatAt is the last time the owner reported progress (unix seconds),
	// or 0 if it never did.
	HeartbeatAt int64
	// Active mirrors vcs_worktrees.active: whether the working dir still exists.
	Active bool
}

// Orphaned reports whether this state satisfies the orphan definition: the work
// never reached main AND the owning process is gone.
//
// A worktree with no owner PID is NOT an orphan by this definition. "Nobody
// ever claimed it" is indistinguishable from "the claim predates V7", and
// deleting work on the strength of a missing record is the one mistake this
// feature must not make.
func (s WorktreeState) Orphaned(alive func(int) bool) bool {
	if s.Lifecycle == WorktreeMerged || s.Lifecycle == WorktreeAbandoned {
		return false
	}
	if s.OwnerPID <= 0 {
		return false
	}
	if s.OwnerPID == os.Getpid() {
		// This process is the owner. Whatever else is true, it is running.
		return false
	}
	return !alive(s.OwnerPID)
}

// SetProcessAlive installs the PID-liveness probe. The composition root passes
// internal/lockfile's implementation; see the file header for why it cannot be
// imported directly. Passing nil restores the fail-safe default (everything
// alive, nothing orphaned).
func (v *VCS) SetProcessAlive(fn func(pid int) bool) {
	v.freezeMu.Lock()
	defer v.freezeMu.Unlock()
	v.processAlive = fn
}

// aliveProbe returns the installed probe or the fail-safe default.
func (v *VCS) aliveProbe() func(int) bool {
	v.freezeMu.Lock()
	defer v.freezeMu.Unlock()
	if v.processAlive == nil {
		return func(int) bool { return true }
	}
	return v.processAlive
}

// worktreeStateDDL creates the sidecar table. Run lazily on first use rather
// than in New, because New has no error return and a VCS is constructed in
// paths (config validation, tests) that never touch a worktree.
const worktreeStateDDL = `
CREATE TABLE IF NOT EXISTS vcs_worktree_state (
    worktree_id  TEXT PRIMARY KEY,
    lifecycle    TEXT NOT NULL DEFAULT 'active',
    owner_pid    INTEGER NOT NULL DEFAULT 0,
    heartbeat_at INTEGER NOT NULL DEFAULT 0,
    updated_at   INTEGER NOT NULL DEFAULT 0
);`

// ensureWorktreeState creates the sidecar table once per VCS.
func (v *VCS) ensureWorktreeState() error {
	var err error
	v.worktreeStateOnce.Do(func() {
		_, err = v.store.DB.Exec(worktreeStateDDL)
	})
	if err != nil {
		return fmt.Errorf("vcs: worktree state table: %w", err)
	}
	return nil
}

// ClaimWorktree records pid as the owner of wtID and stamps a first heartbeat.
// A sub-agent calls this as it starts working; without it the worktree can
// never be reported orphaned (see WorktreeState.Orphaned).
func (v *VCS) ClaimWorktree(wtID string, pid int) error {
	if pid <= 0 {
		return fmt.Errorf("vcs: claim worktree %s: invalid pid %d", wtID, pid)
	}
	return v.upsertWorktreeState(wtID, WorktreeActive, pid, time.Now().Unix())
}

// HeartbeatWorktree refreshes wtID's liveness stamp. A long-running agent calls
// it periodically. It deliberately does NOT change the lifecycle: a heartbeat
// on a worktree already marked merged is a stale writer, not a resurrection.
func (v *VCS) HeartbeatWorktree(wtID string) error {
	if err := v.ensureWorktreeState(); err != nil {
		return err
	}
	now := time.Now().Unix()
	return v.store.WriteTx(context.Background(), func(tx *sql.Tx) error {
		res, err := tx.Exec(
			"UPDATE vcs_worktree_state SET heartbeat_at=?, updated_at=? WHERE worktree_id=?",
			now, now, wtID)
		if err != nil {
			return fmt.Errorf("vcs: heartbeat %s: %w", wtID, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("vcs: heartbeat rows: %w", err)
		}
		if n == 0 {
			return fmt.Errorf("vcs: heartbeat %s: worktree has no owner claim", wtID)
		}
		return nil
	})
}

// MarkWorktreeMerged records that wtID's work reached main. Called by
// MergeToMain on success, so the ledger is updated by the operation that makes
// it true rather than by a caller who might forget.
func (v *VCS) MarkWorktreeMerged(wtID string) error {
	return v.setWorktreeLifecycle(wtID, WorktreeMerged)
}

// MarkWorktreeAbandoned records a deliberate decision not to merge wtID.
func (v *VCS) MarkWorktreeAbandoned(wtID string) error {
	return v.setWorktreeLifecycle(wtID, WorktreeAbandoned)
}

// setWorktreeLifecycle writes a lifecycle value, preserving the existing owner
// and heartbeat.
func (v *VCS) setWorktreeLifecycle(wtID string, lc WorktreeLifecycle) error {
	if err := v.ensureWorktreeState(); err != nil {
		return err
	}
	now := time.Now().Unix()
	return v.store.WriteTx(context.Background(), func(tx *sql.Tx) error {
		if _, err := tx.Exec(
			`INSERT INTO vcs_worktree_state (worktree_id, lifecycle, owner_pid, heartbeat_at, updated_at)
			 VALUES (?, ?, 0, 0, ?)
			 ON CONFLICT(worktree_id) DO UPDATE SET lifecycle=excluded.lifecycle, updated_at=excluded.updated_at`,
			wtID, string(lc), now,
		); err != nil {
			return fmt.Errorf("vcs: set lifecycle %s: %w", wtID, err)
		}
		return nil
	})
}

// upsertWorktreeState writes a full state row.
func (v *VCS) upsertWorktreeState(
	wtID string, lc WorktreeLifecycle, pid int, heartbeat int64,
) error {
	if err := v.ensureWorktreeState(); err != nil {
		return err
	}
	now := time.Now().Unix()
	return v.store.WriteTx(context.Background(), func(tx *sql.Tx) error {
		if _, err := tx.Exec(
			`INSERT INTO vcs_worktree_state (worktree_id, lifecycle, owner_pid, heartbeat_at, updated_at)
			 VALUES (?, ?, ?, ?, ?)
			 ON CONFLICT(worktree_id) DO UPDATE SET
			   lifecycle=excluded.lifecycle, owner_pid=excluded.owner_pid,
			   heartbeat_at=excluded.heartbeat_at, updated_at=excluded.updated_at`,
			wtID, string(lc), pid, heartbeat, now,
		); err != nil {
			return fmt.Errorf("vcs: claim %s: %w", wtID, err)
		}
		return nil
	})
}

// ListWorktreeStates returns every worktree of repoID with its ledger entry,
// oldest first. Worktrees with no ledger row read as active/unowned.
func (v *VCS) ListWorktreeStates(repoID string) ([]WorktreeState, error) {
	if err := v.ensureWorktreeState(); err != nil {
		return nil, err
	}
	rows, err := v.store.DB.Query(
		`SELECT w.id, w.repo_id, w.path, w.base_commit, w.created_at, w.tip, w.active,
		        COALESCE(s.lifecycle, 'active'), COALESCE(s.owner_pid, 0), COALESCE(s.heartbeat_at, 0)
		 FROM vcs_worktrees w
		 LEFT JOIN vcs_worktree_state s ON s.worktree_id = w.id
		 WHERE w.repo_id = ?
		 ORDER BY w.created_at ASC, w.id ASC`, repoID)
	if err != nil {
		return nil, fmt.Errorf("vcs: list worktree states: %w", err)
	}
	defer rows.Close()
	var out []WorktreeState
	for rows.Next() {
		var s WorktreeState
		var lc string
		var active int
		if err := rows.Scan(&s.ID, &s.RepoID, &s.Path, &s.BaseCommit, &s.CreatedAt,
			&s.Tip, &active, &lc, &s.OwnerPID, &s.HeartbeatAt); err != nil {
			return nil, fmt.Errorf("vcs: scan worktree state: %w", err)
		}
		s.Lifecycle = WorktreeLifecycle(lc)
		s.Active = active != 0
		out = append(out, s)
	}
	return out, rows.Err()
}

// ScanOrphanWorktrees returns the worktrees whose owning process is gone and
// whose work never reached main, sorted by id.
//
// It only REPORTS. Removing a worktree destroys the only copy of whatever the
// dead agent produced, so the decision belongs to the caller — and, with the
// default (unwired) liveness probe, the answer is always "none", which is the
// safe direction to be wrong in.
func (v *VCS) ScanOrphanWorktrees(repoID string) ([]WorktreeState, error) {
	states, err := v.ListWorktreeStates(repoID)
	if err != nil {
		return nil, err
	}
	alive := v.aliveProbe()
	var out []WorktreeState
	for _, s := range states {
		if s.Orphaned(alive) {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// CleanupOrphanWorktree marks wtID orphaned and removes its working dir.
//
// It re-runs the scan and refuses (ErrWorktreeNotOrphaned) unless wtID is still
// named by it. Re-checking rather than trusting the caller's earlier scan is
// the point: between a scan and a cleanup the owner may have been a recycled
// PID, or a new agent may have claimed the worktree.
//
// HISTORY IS NOT DELETED. This removes the materialized directory and flips
// active=0 (via removeWorktreeLocked, which already guards against deleting
// anything outside worktreeDir); the branch's commits stay reachable through
// the worktree row's tip and remain excluded from GC. An orphaned branch's work
// is recoverable afterwards through LogWorktree and Restore.
func (v *VCS) CleanupOrphanWorktree(repoID, wtID string) error {
	unlock, err := v.lockRepoUnlessFrozen(repoID)
	if err != nil {
		return err
	}
	defer unlock()

	orphans, err := v.ScanOrphanWorktrees(repoID)
	if err != nil {
		return err
	}
	found := false
	for _, o := range orphans {
		if o.ID == wtID {
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("%w: %s", ErrWorktreeNotOrphaned, wtID)
	}
	if err := v.setWorktreeLifecycle(wtID, WorktreeOrphaned); err != nil {
		return err
	}
	return v.removeWorktreeLocked(repoID, wtID)
}
