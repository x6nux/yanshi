// internal/vcs/gc.go
//
// V2: bounded history. Retention plus real space reclamation.
//
// # Why this exists
//
// Every turn seals a seam, and a seam folds pending edits into a commit. A goal
// loop that runs for a few hundred iterations therefore writes a few hundred
// commits, each carrying a delta of blob rows, and nothing in this package has
// ever deleted any of it. The SQLite file only grows. That is fine for a chat
// session and untenable for the self-driving loop this repo is built around.
//
// # The safety argument comes first, because a wrong GC is unrecoverable
//
// Reachability here is deliberately CONSERVATIVE and is computed from roots
// that are enumerated, not inferred:
//
//   - vcs_repos.main_head — where every main working copy stands;
//   - every vcs_worktrees.tip and .base_commit, for EVERY worktree row
//     including inactive ones. Inactive means "the working dir was removed",
//     not "the history may be destroyed": RemoveWorktree keeps the row
//     precisely so the branch stays auditable, and a merge commit still points
//     at it via merged_from;
//   - every vcs_seams.commit_id, without exception and regardless of age.
//
// From those roots the parent chain is walked to the root commit, and a commit
// is deletable only if it is in NO chain. A blob is deletable only if no
// surviving commit's delta and no pending vcs_uncommitted row references it.
//
// The asymmetry is intentional: a retained commit costs a row, while a deleted
// reachable commit costs the user their history with no recovery path. So
// retention (keep N / keep D days) NARROWS the candidate set and reachability
// VETOES it — retention can never promote an object to deletable, it can only
// decline to delete one reachability already cleared. `TestGC_SeamOnlyBlob…`
// pins the case that motivated the ordering: a blob whose only surviving
// reference is an old seam's commit.
//
// # Why VACUUM, and why it is optional
//
// Deleting rows in SQLite returns pages to the file's freelist; the file does
// not shrink. For the goal-loop case the whole point is that the file shrinks,
// so RunGC issues a VACUUM. It is gated behind GCOptions.Vacuum because VACUUM
// rewrites the entire database, cannot run inside a transaction, and takes a
// lock for its duration — a caller doing routine pruning mid-session wants the
// rows gone without a multi-second stall.

package vcs

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"time"
)

// GCOptions configures a garbage-collection pass.
type GCOptions struct {
	// KeepRecent retains at least this many of the newest main-line commits
	// regardless of age. <= 0 uses DefaultGCKeepRecent. It is a floor, not a
	// cap: reachable commits beyond it are still retained.
	KeepRecent int
	// KeepDays retains every commit created within this many days regardless of
	// position. <= 0 uses DefaultGCKeepDays; use KeepDaysNone to disable the
	// age floor entirely.
	KeepDays int
	// DryRun computes and reports the pass without deleting anything. Like the
	// restore preview, it runs the SAME code as the real pass; only the final
	// mutation is skipped.
	DryRun bool
	// Vacuum rewrites the database after a successful non-dry-run pass so the
	// file actually shrinks. Off by default; see the file header.
	Vacuum bool
}

// DefaultGCKeepRecent is the retention floor in commits.
const DefaultGCKeepRecent = 100

// DefaultGCKeepDays is the retention floor in days.
const DefaultGCKeepDays = 14

// KeepDaysNone disables the age floor, leaving KeepRecent as the only retention
// rule.
//
// It exists because the age floor otherwise has no off switch, and its absence
// made this whole collector inert for the case the file header opens with. A
// goal loop that writes several hundred commits does so in MINUTES, so every
// commit it produces is younger than any positive KeepDays and is retained by
// age no matter how unreachable it is. KeepDays <= 0 could not express "no age
// floor" because that range was already spelled "use the default", so the only
// way to collect same-session garbage was to wait fourteen days — by which time
// the loop has been over for a fortnight.
//
// This was invisible to the unit tests because every one of them backdates
// created_at by a year before collecting; the collector was only ever measured
// on histories no live process can produce. A live run of a few hundred
// commits followed by a reset collected nothing at all.
//
// The value is negative and specific rather than simply 0: 0 is what an
// unset struct field and an omitted JSON number both produce, and turning
// "the caller said nothing" into "delete everything reachability permits" is
// the wrong default for a destructive operation.
const KeepDaysNone = -1

// GCResult reports what a pass did (or, for a dry run, would do).
type GCResult struct {
	// DeletedCommits and DeletedBlobs are sorted ids, so a result is stable and
	// comparable across runs.
	DeletedCommits []string
	DeletedBlobs   []string
	// KeptCommits counts commits that survived, whether by reachability or by
	// retention.
	KeptCommits int
	// ProtectedByReachability counts commits that retention selected as
	// candidates but reachability vetoed. A persistently high value means the
	// retention settings are asking for something history will not give.
	ProtectedByReachability int
	// FreedBytes is the summed size of deleted blobs. It is the row payload,
	// not the on-disk delta: without Vacuum the pages stay in the freelist.
	FreedBytes int64
	DryRun     bool
	Vacuumed   bool
}

// RunGC prunes unreachable history for repoID under the given retention policy.
//
// It takes the repo lane for the whole pass so a concurrent commit cannot add a
// reference between the reachability computation and the delete, and refuses to
// run while a restore holds the working copy (V5) — a GC that deleted the blob
// a half-finished rollback was about to write would turn a recoverable failure
// into an unrecoverable one.
func (v *VCS) RunGC(repoID string, opts GCOptions) (GCResult, error) {
	unlock, err := v.lockRepoUnlessFrozen(repoID)
	if err != nil {
		return GCResult{}, err
	}
	defer unlock()
	return v.runGCLocked(repoID, opts)
}

func (v *VCS) runGCLocked(repoID string, opts GCOptions) (GCResult, error) {
	if opts.KeepRecent <= 0 {
		opts.KeepRecent = DefaultGCKeepRecent
	}
	if opts.KeepDays == 0 {
		opts.KeepDays = DefaultGCKeepDays
	}
	res := GCResult{DryRun: opts.DryRun}

	reachable, err := v.reachableCommits(repoID)
	if err != nil {
		return res, err
	}
	all, err := v.repoCommits(repoID)
	if err != nil {
		return res, err
	}
	protectedByAge := v.retainedByPolicy(all, opts)

	var doomed []string
	for _, c := range all {
		if reachable[c.id] {
			res.KeptCommits++
			if protectedByAge[c.id] {
				continue
			}
			// Retention offered this one up; reachability kept it. Counting the
			// overlap is how an operator learns their KeepRecent is not the
			// thing bounding the database.
			res.ProtectedByReachability++
			continue
		}
		if protectedByAge[c.id] {
			res.KeptCommits++
			continue
		}
		doomed = append(doomed, c.id)
	}
	sort.Strings(doomed)
	res.DeletedCommits = doomed

	liveBlobs, err := v.liveBlobHashes(doomed)
	if err != nil {
		return res, err
	}
	orphans, freed, err := v.orphanBlobs(liveBlobs)
	if err != nil {
		return res, err
	}
	res.DeletedBlobs = orphans
	res.FreedBytes = freed

	if opts.DryRun || (len(doomed) == 0 && len(orphans) == 0) {
		return res, nil
	}
	if err := v.deleteGCTargets(doomed, orphans); err != nil {
		return res, err
	}
	// The tree cache memoizes reconstructed trees by commit id; entries for
	// deleted commits are now claims about rows that no longer exist. Dropping
	// the whole cache rather than the deleted keys is deliberate — a cached
	// tree for a SURVIVING commit was reconstructed by walking a chain that may
	// have included a deleted ancestor, so selective eviction would keep
	// exactly the entries most likely to be stale.
	v.treeCacheMu.Lock()
	v.treeCache = map[string]map[string]string{}
	v.treeCacheMu.Unlock()

	if opts.Vacuum {
		// VACUUM cannot run inside a transaction, hence the bare Exec rather
		// than WriteTx.
		if _, err := v.store.DB.Exec("VACUUM"); err != nil {
			return res, fmt.Errorf("vcs: gc: vacuum: %w", err)
		}
		// The checkpoint is NOT optional decoration. The store runs in WAL mode
		// (internal/store applyConnectionPragmas), and in WAL mode a VACUUM
		// writes the ENTIRE rebuilt database into the -wal sidecar rather than
		// into the main file. Stopping after VACUUM therefore leaves the
		// combined on-disk footprint LARGER than before — the exact opposite of
		// what the caller asked for — until some later automatic checkpoint
		// happens to run. TRUNCATE both applies the WAL to the main file and
		// resets the sidecar to zero length, so the space is actually back when
		// this returns.
		//
		// A checkpoint failure is not fatal: the rows are gone, the database is
		// consistent, and the pages will be reclaimed by the next automatic
		// checkpoint. Reporting Vacuumed=false is the honest signal.
		if _, err := v.store.DB.Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
			return res, fmt.Errorf("vcs: gc: checkpoint after vacuum: %w", err)
		}
		res.Vacuumed = true
	}
	return res, nil
}

// gcCommit is the minimal commit view the GC needs.
type gcCommit struct {
	id        string
	parent    string
	createdAt int64
}

// repoCommits lists every commit of repoID, newest first.
func (v *VCS) repoCommits(repoID string) ([]gcCommit, error) {
	rows, err := v.store.DB.Query(
		"SELECT id, parent_id, created_at FROM vcs_commits WHERE repo_id=? ORDER BY created_at DESC, id DESC",
		repoID)
	if err != nil {
		return nil, fmt.Errorf("vcs: gc: list commits: %w", err)
	}
	defer rows.Close()
	var out []gcCommit
	for rows.Next() {
		var c gcCommit
		if err := rows.Scan(&c.id, &c.parent, &c.createdAt); err != nil {
			return nil, fmt.Errorf("vcs: gc: scan commit: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// reachableCommits computes the closure described in the file header: every
// commit on the parent chain of any root.
//
// merged_from is followed as a second parent edge. A merge commit's tree
// already contains the merged content, so the source branch's own commits are
// not needed to RECONSTRUCT it — but they are needed to explain it, and
// LogWorktree still walks them. Treating merges as a single-parent chain would
// delete the history of every merged worktree, which is a data loss an operator
// would discover only when they went looking for it.
func (v *VCS) reachableCommits(repoID string) (map[string]bool, error) {
	roots, err := v.gcRoots(repoID)
	if err != nil {
		return nil, err
	}
	parents, err := v.parentEdges(repoID)
	if err != nil {
		return nil, err
	}
	reachable := make(map[string]bool, len(parents))
	stack := append([]string(nil), roots...)
	for len(stack) > 0 {
		id := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if id == "" || reachable[id] {
			continue
		}
		reachable[id] = true
		stack = append(stack, parents[id]...)
	}
	return reachable, nil
}

// gcRoots enumerates every commit id something still points at. Each query is
// separate and none of them is optional; see the file header for why worktree
// rows are read regardless of their active flag.
func (v *VCS) gcRoots(repoID string) ([]string, error) {
	var roots []string
	collect := func(query string, args ...any) error {
		rows, err := v.store.DB.Query(query, args...)
		if err != nil {
			return fmt.Errorf("vcs: gc: roots: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var id sql.NullString
			if err := rows.Scan(&id); err != nil {
				return fmt.Errorf("vcs: gc: scan root: %w", err)
			}
			if id.Valid && id.String != "" {
				roots = append(roots, id.String)
			}
		}
		return rows.Err()
	}
	if err := collect("SELECT main_head FROM vcs_repos WHERE id=?", repoID); err != nil {
		return nil, err
	}
	if err := collect("SELECT tip FROM vcs_worktrees WHERE repo_id=?", repoID); err != nil {
		return nil, err
	}
	if err := collect("SELECT base_commit FROM vcs_worktrees WHERE repo_id=?", repoID); err != nil {
		return nil, err
	}
	if err := collect("SELECT commit_id FROM vcs_seams WHERE repo_id=?", repoID); err != nil {
		return nil, err
	}
	return roots, nil
}

// parentEdges maps each commit to its parent ids (parent_id plus merged_from's
// worktree tip, resolved through the worktree row).
func (v *VCS) parentEdges(repoID string) (map[string][]string, error) {
	rows, err := v.store.DB.Query(
		"SELECT id, parent_id, merged_from FROM vcs_commits WHERE repo_id=?", repoID)
	if err != nil {
		return nil, fmt.Errorf("vcs: gc: parent edges: %w", err)
	}
	defer rows.Close()
	edges := map[string][]string{}
	var merges []struct{ id, wtID string }
	for rows.Next() {
		var id, parent, mergedFrom string
		if err := rows.Scan(&id, &parent, &mergedFrom); err != nil {
			return nil, fmt.Errorf("vcs: gc: scan edge: %w", err)
		}
		if parent != "" {
			edges[id] = append(edges[id], parent)
		}
		if mergedFrom != "" {
			merges = append(merges, struct{ id, wtID string }{id, mergedFrom})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// merged_from stores a WORKTREE id, not a commit id, so the second parent
	// edge is the worktree's tip at merge time — approximated by its current
	// tip, which is the newest commit that branch ever had.
	for _, m := range merges {
		if tip := v.worktreeTip(m.wtID); tip != "" {
			edges[m.id] = append(edges[m.id], tip)
		}
	}
	return edges, nil
}

// retainedByPolicy returns the commits retention alone would keep: the newest
// KeepRecent, plus everything created within KeepDays.
//
// KeepDaysNone (and any negative value, which cannot describe a window) drops
// the age term entirely, leaving KeepRecent as the sole rule. Computing a
// cutoff from a negative duration would instead put it in the FUTURE and retain
// every commit unconditionally — which is what made a same-session GC a no-op.
func (v *VCS) retainedByPolicy(all []gcCommit, opts GCOptions) map[string]bool {
	keep := make(map[string]bool, len(all))
	ageFloor := opts.KeepDays > 0
	cutoff := time.Now().Add(-time.Duration(opts.KeepDays) * 24 * time.Hour).Unix()
	for i, c := range all {
		if i < opts.KeepRecent || (ageFloor && c.createdAt >= cutoff) {
			keep[c.id] = true
		}
	}
	return keep
}

// liveBlobHashes returns every blob hash still referenced after the doomed
// commits are removed: the deltas of all SURVIVING commits (across every repo,
// since blobs are content-addressed and shared globally) plus every pending
// vcs_uncommitted row.
//
// Scanning all repos is not over-caution: two repos that contain the same file
// share one blob row, so scoping this query to repoID would delete a blob the
// other repo's history depends on.
func (v *VCS) liveBlobHashes(doomed []string) (map[string]bool, error) {
	dead := make(map[string]bool, len(doomed))
	for _, id := range doomed {
		dead[id] = true
	}
	live := map[string]bool{}
	rows, err := v.store.DB.Query("SELECT commit_id, blob_hash FROM vcs_tree")
	if err != nil {
		return nil, fmt.Errorf("vcs: gc: scan trees: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var commitID, hash string
		if err := rows.Scan(&commitID, &hash); err != nil {
			return nil, fmt.Errorf("vcs: gc: scan tree row: %w", err)
		}
		if hash != "" && !dead[commitID] {
			live[hash] = true
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	pending, err := v.store.DB.Query("SELECT blob_hash FROM vcs_uncommitted")
	if err != nil {
		return nil, fmt.Errorf("vcs: gc: scan pending: %w", err)
	}
	defer pending.Close()
	for pending.Next() {
		var hash string
		if err := pending.Scan(&hash); err != nil {
			return nil, fmt.Errorf("vcs: gc: scan pending row: %w", err)
		}
		if hash != "" {
			live[hash] = true
		}
	}
	return live, pending.Err()
}

// orphanBlobs returns the sorted hashes of blobs no live reference names, plus
// their total stored size.
func (v *VCS) orphanBlobs(live map[string]bool) ([]string, int64, error) {
	rows, err := v.store.DB.Query("SELECT hash, size FROM vcs_blobs")
	if err != nil {
		return nil, 0, fmt.Errorf("vcs: gc: scan blobs: %w", err)
	}
	defer rows.Close()
	var out []string
	var freed int64
	for rows.Next() {
		var hash string
		var size int64
		if err := rows.Scan(&hash, &size); err != nil {
			return nil, 0, fmt.Errorf("vcs: gc: scan blob row: %w", err)
		}
		if !live[hash] {
			out = append(out, hash)
			freed += size
		}
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	sort.Strings(out)
	return out, freed, nil
}

// deleteGCTargets removes the doomed commits, their delta rows and the orphaned
// blobs in ONE transaction. All-or-nothing matters here more than usual: a
// partial delete could leave a surviving commit whose parent chain has a hole,
// and reconstructTree would then silently return a tree missing every path the
// hole contributed.
//
// Delta rows go first so no vcs_tree row ever references an absent commit.
func (v *VCS) deleteGCTargets(commits, blobs []string) error {
	return v.store.WriteTx(context.Background(), func(tx *sql.Tx) error {
		for _, id := range commits {
			if _, err := tx.Exec("DELETE FROM vcs_tree WHERE commit_id=?", id); err != nil {
				return fmt.Errorf("vcs: gc: delete tree of %s: %w", id, err)
			}
		}
		for _, id := range commits {
			if _, err := tx.Exec("DELETE FROM vcs_commits WHERE id=?", id); err != nil {
				return fmt.Errorf("vcs: gc: delete commit %s: %w", id, err)
			}
		}
		for _, h := range blobs {
			if _, err := tx.Exec("DELETE FROM vcs_blobs WHERE hash=?", h); err != nil {
				return fmt.Errorf("vcs: gc: delete blob %s: %w", h, err)
			}
		}
		return nil
	})
}
