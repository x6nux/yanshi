// internal/vcs/restore.go
//
// Single-file restore: pull one historical blob back into an ACTIVE working
// copy (the repo root or a materialized worktree dir) and record the same
// bytes in that scope's pending changeset, so the restore is itself tracked.
//
// This block used to live in vcs.go. It was moved out under the repo's
// 1000-pure-code-line rule (GOV2) when V1/V4 landed alongside it; nothing
// about the logic changed in the move. Restore and its scope resolution are a
// self-contained responsibility — "put ONE path back" — as opposed to the
// path-set restore in preview.go, which computes a whole plan first.

package vcs

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Restore writes the historical blob into an ACTIVE working copy and records
// the same bytes in that scope's pending changeset before releasing the repo
// lane (CB1). The public signature is unchanged; commit ownership identifies
// repoID and destDir must exactly match that repo's root or an active worktree.
//
// V5: refused with ErrWorkingCopyFrozen while a whole-tree restore holds the
// repo — a single-file restore that queued behind one would land on top of the
// rollback's output and silently reintroduce one file from the discarded state.
func (v *VCS) Restore(commitID, path, destDir string) error {
	repoID, err := v.commitRepoID(commitID)
	if err != nil {
		return err
	}
	unlock, err := v.lockRepoUnlessFrozen(repoID)
	if err != nil {
		return err
	}
	defer unlock()
	return v.restoreLocked(repoID, commitID, path, destDir)
}

func (v *VCS) commitRepoID(commitID string) (string, error) {
	var repoID string
	if err := v.store.DB.QueryRow(
		"SELECT repo_id FROM vcs_commits WHERE id=?", commitID,
	).Scan(&repoID); err != nil {
		return "", fmt.Errorf("vcs: resolve commit %s: %w", commitID, err)
	}
	return repoID, nil
}

func (v *VCS) restoreLocked(repoID, commitID, path, destDir string) error {
	// Re-read ownership while holding the repo lane: closes lookup→lock TOCTOU.
	lockedRepoID, err := v.commitRepoID(commitID)
	if err != nil {
		return err
	}
	if lockedRepoID != repoID {
		return fmt.Errorf("vcs: commit %s changed repository", commitID)
	}
	scopeType, scopeID, scopeRoot, err := v.restoreScopeLocked(repoID, destDir)
	if err != nil {
		return err
	}

	cleanRel := filepath.Clean(filepath.FromSlash(path))
	relSlash := filepath.ToSlash(cleanRel)
	if cleanRel == "." || filepath.IsAbs(cleanRel) || relSlash == ".." ||
		strings.HasPrefix(relSlash, "../") {
		return fmt.Errorf("vcs: unsafe restore path %q", path)
	}
	tree := v.commitTree(commitID)
	h, ok := tree[relSlash]
	if !ok {
		return fmt.Errorf("vcs: %s not in commit %s", relSlash, commitID)
	}
	content, err := v.getBlob(h)
	if err != nil {
		return fmt.Errorf("vcs: read restore blob %s: %w", h, err)
	}
	// V6: write through a confined root handle on the working copy rather than a
	// Join'd absolute path. cleanRel passed the lexical checks above, but those
	// say nothing about what scopeRoot's subdirectories POINT at on disk — an
	// agent that can run `ln -s /etc "$repo/docs"` would otherwise get this
	// restore to write outside the repo.
	workRoot, err := openWorkRoot(scopeRoot)
	if err != nil {
		return err
	}
	defer workRoot.Close()

	dest := filepath.Join(scopeRoot, cleanRel)

	// Snapshot the destination so a recordEdit failure cannot leave an untracked
	// filesystem mutation. Blob insertion on a failed upsert is harmless dedup data.
	old, readErr := rootReadFile(workRoot, relSlash)
	existed := readErr == nil
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		return readErr
	}
	if err := rootWriteFile(workRoot, relSlash, content, 0o644); err != nil {
		return err
	}
	if err := v.recordEdit(scopeType, scopeID, scopeRoot, dest, content); err != nil {
		var rollbackErr error
		if existed {
			rollbackErr = rootWriteFile(workRoot, relSlash, old, 0o644)
		} else {
			rollbackErr = rootRemove(workRoot, relSlash)
			if errors.Is(rollbackErr, os.ErrNotExist) {
				rollbackErr = nil
			}
		}
		return errors.Join(fmt.Errorf("vcs: track restored file: %w", err), rollbackErr)
	}
	return nil
}

func (v *VCS) restoreScopeLocked(repoID, destDir string) (string, string, string, error) {
	// Canonicalize the destination so it matches the EvalSymlinks-resolved
	// root_path / worktree paths on all platforms, not just Windows. macOS
	// temp dirs sit behind /var → /private/var; without resolution the dest
	// never matches the repo root and every Restore falls through to the
	// "not an active working copy" error.
	destAbs := canonicalPath(destDir)
	r, err := v.getRepo(repoID)
	if err != nil {
		return "", "", "", err
	}
	if destAbs == r.RootPath {
		return "main", repoID, r.RootPath, nil
	}
	rows, err := v.store.DB.Query(
		"SELECT id, path FROM vcs_worktrees WHERE repo_id=? AND active=1", repoID)
	if err != nil {
		return "", "", "", err
	}
	defer rows.Close()
	for rows.Next() {
		var wtID, wtPath string
		if err := rows.Scan(&wtID, &wtPath); err != nil {
			return "", "", "", err
		}
		if destAbs == canonicalPath(wtPath) {
			return "worktree", wtID, wtPath, nil
		}
	}
	if err := rows.Err(); err != nil {
		return "", "", "", err
	}
	return "", "", "", fmt.Errorf(
		"vcs: restore destination %s is not an active working copy for repo %s",
		destAbs, repoID)
}
