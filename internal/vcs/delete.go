package vcs

import (
	"fmt"
	"path/filepath"
	"strings"
)

// RecordDeleteMain records a deletion on the main scope: it upserts the path
// into vcs_uncommitted with op="deleted" so the next CommitMain removes it from
// the tree (commitScope deletes op="deleted" rows from the in-memory tree).
// agent is accepted for API symmetry with RecordEditMain; attribution is applied
// at Commit time. A path outside the repo root or ignored is silently skipped
// (mirroring recordEdit), so a delete outside the tracked area is a no-op.
//
// This is the autoVCS entry point for file deletions (used by apply_patch's
// delete/move operations); before this, only content writes (add/modify) could
// be auto-tracked, so a deleted file would have wrongly persisted in commits.
//
// The repo lock is acquired for consistency with RecordEditMain, ensuring
// getRepo and the uncommitted upsert are serialized against concurrent
// CommitMain / MergeToMain.
func (v *VCS) RecordDeleteMain(repoID, agent, absPath string) error {
	unlock := v.lockRepo(repoID)
	defer unlock()
	return v.recordDeleteMainLocked(repoID, agent, absPath)
}

func (v *VCS) recordDeleteMainLocked(repoID, agent, absPath string) error {
	r, err := v.getRepo(repoID)
	if err != nil {
		return err
	}
	return v.recordDelete("main", repoID, r.RootPath, absPath)
}

// RecordDeleteWorktree records a deletion on a worktree scope. absPath is
// resolved against the worktree's working dir (wt.Path), exactly like
// RecordEditWorktree; a path outside wt.Path is silently skipped.
func (v *VCS) RecordDeleteWorktree(wtID, agent, absPath string) error {
	wt, err := v.getWorktree(wtID)
	if err != nil {
		return err
	}
	unlock := v.lockRepo(wt.RepoID)
	defer unlock()
	return v.recordDeleteWorktreeLocked(wt.RepoID, wtID, agent, absPath)
}

func (v *VCS) recordDeleteWorktreeLocked(repoID, wtID, agent, absPath string) error {
	wt, err := v.getWorktree(wtID)
	if err != nil {
		return err
	}
	if wt.RepoID != repoID {
		return fmt.Errorf("vcs: worktree %s changed repository", wtID)
	}
	return v.recordDelete("worktree", wtID, wt.Path, absPath)
}

// recordDelete is the shared delete-track core: resolve absPath to a repo-relative
// path, skip silently if outside the repo root or ignored, and upsert the path
// into the scope's vcs_uncommitted with op="deleted" and an empty blob_hash (the
// blob is irrelevant — commitScope drops op="deleted" rows from the tree
// regardless of blob_hash). On conflict (path was added/modified then deleted in
// the same scope) op is overwritten to "deleted" so the net effect is a deletion.
func (v *VCS) recordDelete(scopeType, scopeID, repoRoot, absPath string) error {
	if repoRoot == "" {
		return nil
	}
	// Resolve symlinks on both repoRoot and absPath for the same reason
	// recordEdit does: the repoRoot may be canonical (main scope from
	// canonicalRepoRoot) or non-canonical (worktree scope with a manually-
	// inserted row); without resolution, filepath.Rel on macOS would see the
	// delete as outside the worktree and silently skip it. We do NOT case-fold
	// (resolveSymlinks, not canonicalPath) so the repo-relative key preserves
	// filename casing; Windows filepath.Rel is already case-insensitive.
	absPath = resolveSymlinks(absPath)
	repoRoot = resolveSymlinks(repoRoot)
	rel, err := filepath.Rel(repoRoot, absPath)
	if err != nil || strings.HasPrefix(filepath.ToSlash(rel), "..") {
		return nil // outside repo → skip silently
	}
	rel = filepath.ToSlash(rel)
	if v.isIgnored(rel) {
		return nil
	}
	_, err = v.store.DB.Exec(
		"INSERT INTO vcs_uncommitted (scope_type, scope_id, path, blob_hash, op) VALUES (?, ?, ?, '', 'deleted')\n"+
			"ON CONFLICT(scope_type, scope_id, path) DO UPDATE SET blob_hash='', op='deleted'",
		scopeType, scopeID, rel)
	return err
}
