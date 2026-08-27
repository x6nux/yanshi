// internal/vcs/revert.go
//
// revert.go 实现 B2-RB1 的回滚核心:MaterializeMain(把 main 工作副本物化到
// 指定 commit 的树)、ResetMainHead(更新 vcs_repos.main_head)、RevertToSeam
// (Task 5 加)。所有方法 strict、fail-fast,任何 blob/IO 失败都立即 return error,
// 不静默 continue(必修项 G)。
//
// Concurrency: public MaterializeMain/ResetMainHead acquire the per-repo lane.
// RevertToSeam already owns that lane and calls private locked cores.

package vcs

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/x6nux/yanshi/internal/store"
)

// validateRelPath enforces the path-safety rules a tree path must satisfy
// before MaterializeMain will touch the working copy (必修项 G):
//   - relative (no leading slash)
//   - no ".." segment (no parent-dir escape)
//   - no ".git" segment (autoVCS never tracks .git, but defense in depth)
//   - non-empty
func validateRelPath(rel string) error {
	rel = filepath.ToSlash(rel)
	if rel == "" || rel == "." {
		return fmt.Errorf("vcs: empty path")
	}
	if strings.HasPrefix(rel, "/") {
		return fmt.Errorf("vcs: absolute path %q", rel)
	}
	for _, seg := range strings.Split(rel, "/") {
		if seg == ".." {
			return fmt.Errorf("vcs: parent escape in %q", rel)
		}
		if seg == ".git" {
			return fmt.Errorf("vcs: .git segment in %q", rel)
		}
	}
	return nil
}

// MaterializeMain is the public serialized entry point (CB1).
func (v *VCS) MaterializeMain(repoID, commitID string) error {
	unlock := v.lockRepo(repoID)
	defer unlock()
	return v.materializeMainLocked(repoID, commitID)
}

func (v *VCS) materializeMainLocked(repoID, commitID string) error {
	return v.materializeMainLockedWithHook(repoID, commitID, nil)
}

type materializeHook func(stage, path string) error

type workingFileSnapshot struct {
	exists bool
	data   []byte
	mode   os.FileMode
}

// materializeMainLockedWithHook is the deterministic core. hook is nil in
// production; same-package tests inject an error between mutations to prove
// snapshot compensation restores every touched path.
func (v *VCS) materializeMainLockedWithHook(
	repoID, commitID string, hook materializeHook,
) error {
	c, err := v.getCommit(commitID)
	if err != nil {
		return fmt.Errorf("vcs: materialize: load commit %s: %w", commitID, err)
	}
	if c.RepoID != repoID {
		return fmt.Errorf("vcs: materialize: commit %s belongs to repo %s, not %s",
			commitID, c.RepoID, repoID)
	}
	if c.WorktreeID != "" {
		return fmt.Errorf("vcs: materialize: commit %s is worktree commit %s",
			commitID, c.WorktreeID)
	}
	r, err := v.getRepo(repoID)
	if err != nil {
		return fmt.Errorf("vcs: materialize: load repo: %w", err)
	}

	targetTree := v.commitTree(commitID)
	prevTree := v.commitTree(r.MainHead)
	paths := sortedTreeUnion(prevTree, targetTree)
	for _, path := range paths {
		if err := validateRelPath(path); err != nil {
			return err
		}
	}

	// Resolve every target blob before the first filesystem mutation.
	targetBytes := make(map[string][]byte, len(targetTree))
	for path, hash := range targetTree {
		content, err := v.getBlob(hash)
		if err != nil {
			return fmt.Errorf("vcs: materialize: blob %s for %s: %w", hash, path, err)
		}
		targetBytes[path] = content
	}

	// V6: every mutation below goes through a confined root handle instead of a
	// filepath.Join'd absolute path, so a working-copy directory swapped to a
	// symlink cannot redirect a materialize out of the repo.
	workRoot, err := openWorkRoot(r.RootPath)
	if err != nil {
		return err
	}
	defer workRoot.Close()

	// D1: snapshot all tracked paths that this operation may delete/replace.
	snapshot, err := snapshotWorkingFiles(r.RootPath, paths)
	if err != nil {
		return fmt.Errorf("vcs: materialize: snapshot: %w", err)
	}
	compensate := func(cause error) error {
		if rollbackErr := restoreWorkingFiles(r.RootPath, paths, snapshot); rollbackErr != nil {
			return errors.Join(cause,
				fmt.Errorf("vcs: materialize: compensation failed: %w", rollbackErr))
		}
		return cause
	}

	// Delete prev-only paths first, in deterministic order.
	for _, path := range paths {
		if _, wasPresent := prevTree[path]; !wasPresent {
			continue
		}
		if _, remains := targetTree[path]; remains {
			continue
		}
		if hook != nil {
			if err := hook("remove", path); err != nil {
				return compensate(err)
			}
		}
		if err := rootRemove(workRoot, path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return compensate(fmt.Errorf("vcs: materialize: remove %s: %w", path, err))
		}
	}

	// Replace target paths. sortedTreeUnion keeps order deterministic for tests.
	for _, path := range paths {
		content, ok := targetBytes[path]
		if !ok {
			continue
		}
		if hook != nil {
			if err := hook("replace", path); err != nil {
				return compensate(err)
			}
		}
		mode := os.FileMode(0o644)
		if prior := snapshot[path]; prior.exists {
			mode = prior.mode
		}
		if err := rootReplaceFile(workRoot, path, content, mode); err != nil {
			return compensate(fmt.Errorf("vcs: materialize: replace %s: %w", path, err))
		}
	}
	return nil
}

func sortedTreeUnion(a, b map[string]string) []string {
	set := make(map[string]struct{}, len(a)+len(b))
	for path := range a {
		set[path] = struct{}{}
	}
	for path := range b {
		set[path] = struct{}{}
	}
	paths := make([]string, 0, len(set))
	for path := range set {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths
}

// snapshotWorkingFiles captures the pre-operation bytes/existence/mode of every
// listed tracked path, for the compensation in materializeMainLockedWithHook and
// RevertToSeam.
//
// V6: reads go through a confined root handle and use Lstat, not Stat. With
// Stat a symlink standing where a tracked file belongs would resolve to its
// target — the snapshot would capture an OUTSIDE file's bytes, and the
// compensating restore would then write them back through the link. Lstat sees
// the link itself, which is not a regular file, so such a path is rejected here
// rather than laundered into the rollback set.
func snapshotWorkingFiles(root string, paths []string) (map[string]workingFileSnapshot, error) {
	workRoot, err := openWorkRoot(root)
	if err != nil {
		return nil, err
	}
	defer workRoot.Close()

	out := make(map[string]workingFileSnapshot, len(paths))
	for _, path := range paths {
		info, err := rootLstat(workRoot, path)
		if errors.Is(err, os.ErrNotExist) {
			out[path] = workingFileSnapshot{}
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("stat %s: %w", path, err)
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("vcs: materialize: %s is not a regular file", path)
		}
		data, err := rootReadFile(workRoot, path)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", path, err)
		}
		out[path] = workingFileSnapshot{exists: true, data: data, mode: info.Mode().Perm()}
	}
	return out, nil
}

// restoreWorkingFiles puts every listed path back to its snapshotted state.
// Confined for the same reason as the forward direction: compensation is a
// write, and a rollback that follows a symlink is as damaging as the mutation
// it is undoing.
func restoreWorkingFiles(
	root string, paths []string, snapshot map[string]workingFileSnapshot,
) error {
	workRoot, err := openWorkRoot(root)
	if err != nil {
		return err
	}
	defer workRoot.Close()

	var errs []error
	for _, path := range paths {
		prior := snapshot[path]
		if prior.exists {
			if err := rootReplaceFile(workRoot, path, prior.data, prior.mode); err != nil {
				errs = append(errs, fmt.Errorf("restore %s: %w", path, err))
			}
			continue
		}
		if err := rootRemove(workRoot, path); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, fmt.Errorf("remove new %s: %w", path, err))
		}
	}
	return errors.Join(errs...)
}

// replaceFile was the unconfined same-directory temp+rename writer. V6 replaced
// every call site with rootReplaceFile (internal/vcs/rootwrite.go), which does
// the same temp+rename through an os.Root handle so no component of the path
// can be a symlink out of the working copy. It is deleted rather than kept
// "just in case": a working-copy writer that does not go through the root
// handle is exactly the shape of the bug V6 fixed, and leaving one in the
// package invites the next call site to use it.

// ResetMainHead is also a public repo-serialized write path (CB1).
func (v *VCS) ResetMainHead(repoID, commitID string) error {
	unlock := v.lockRepo(repoID)
	defer unlock()
	return v.resetMainHeadLocked(repoID, commitID)
}

func (v *VCS) resetMainHeadLocked(repoID, commitID string) error {
	c, err := v.getCommit(commitID)
	if err != nil {
		return fmt.Errorf("vcs: reset head: load commit: %w", err)
	}
	if c.RepoID != repoID || c.WorktreeID != "" {
		return fmt.Errorf("vcs: reset head: commit %s is not main commit of repo %s",
			commitID, repoID)
	}
	res, err := v.store.DB.Exec(
		"UPDATE vcs_repos SET main_head=? WHERE id=?", commitID, repoID)
	if err != nil {
		return fmt.Errorf("vcs: reset head: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("vcs: reset head rows: %w", err)
	}
	if n != 1 {
		return fmt.Errorf("vcs: reset head: repo %s not found", repoID)
	}
	return nil
}

// (RevertToSeam is added in Task 5.)

// RevertToSeam reverts repoID's main scope to the state captured by seamID.
// Returns the undo seam id (pointing at the PRE-revert main_head) — call
// RevertToSeam again with that id to UNDO this revert. The undo + audit seams
// and the main_head reset land in ONE SQLite transaction (必修项 G). Before
// materialization this method snapshots every touched working-copy path; if
// Begin/Exec/Commit fails after disk mutation, a deferred compensation restores
// the exact pre-operation bytes/existence/modes (D1).
//
// prevHistoryLen / prevTurnSeq record the caller's conversation boundary
// (len(history), turns) AT REVERT TIME. historySnap is the exact durable state
// at that same boundary. All three are stored on the undo seam in the same tx
// as main_head, so reverting the undo seam can restore deleted messages after a
// reconnect, not merely recover counters (D2). The WS handler passes real values
// and a non-nil snapshot; the agent revert_turn tool passes 0,0,nil because it
// does not own WS conversation history.
//
// Must be called WITHOUT the repo lock held; the method acquires it for the full
// materialize + tx sequence so a concurrent CommitMain cannot race the head.
func (v *VCS) RevertToSeam(
	repoID, seamID, label string,
	prevHistoryLen, prevTurnSeq int,
	historySnap *store.SessionRevertSnapshot,
) (undoSeamID string, err error) {
	// V5: raise the working-copy freeze BEFORE taking the lane. Cooperative VCS
	// writers now fail fast instead of queueing behind the revert and later
	// committing content they read from a working copy that no longer exists.
	// Non-VCS writers (a shell_run compile, a background worker) are NOT
	// stopped by this and cannot portably be — the fingerprint comparison
	// inside applyRestorePlanLocked is what catches those. See
	// internal/vcs/freeze.go.
	thaw, err := v.freezeWorkingCopy(repoID)
	if err != nil {
		return "", err
	}
	defer thaw()

	unlock := v.lockRepo(repoID)
	defer unlock()

	// (1) Load + validate seam.
	seam, err := v.FindSeam(seamID)
	if err != nil {
		return "", fmt.Errorf("vcs: revert: load seam: %w", err)
	}
	if seam.RepoID != repoID {
		return "", fmt.Errorf("vcs: revert: seam %s belongs to repo %s, not %s",
			seamID, seam.RepoID, repoID)
	}

	// D2: the WS caller supplies the exact pre-revert durable session snapshot.
	// Encode before touching disk. Agent VCS-only reverts pass nil and store an
	// empty blob. They are assigned session_id="" below so their undo/audit seams
	// never enter a WS session list or imply conversation-history undo (D4).
	historyJSON := []byte{}
	if historySnap != nil {
		if historySnap.Meta.ID != seam.SessionID {
			return "", fmt.Errorf("vcs: revert: history snapshot session %s does not match seam session %s",
				historySnap.Meta.ID, seam.SessionID)
		}
		historyJSON, err = store.EncodeSessionRevertSnapshot(*historySnap)
		if err != nil {
			return "", fmt.Errorf("vcs: revert: encode history snapshot: %w", err)
		}
	}

	// (2) Capture previousHead BEFORE any mutation.
	r, err := v.getRepo(repoID)
	if err != nil {
		return "", fmt.Errorf("vcs: revert: load repo: %w", err)
	}
	previousHead := r.MainHead

	// (3) Validate target commit (seam.CommitID) belongs to repo + is main.
	targetCommit := seam.CommitID
	c, err := v.getCommit(targetCommit)
	if err != nil {
		return "", fmt.Errorf("vcs: revert: target commit: %w", err)
	}
	if c.RepoID != repoID {
		return "", fmt.Errorf("vcs: revert: commit %s not in repo %s", targetCommit, repoID)
	}
	if c.WorktreeID != "" {
		return "", fmt.Errorf("vcs: revert: commit %s is worktree, not main", targetCommit)
	}

	// (4) Snapshot exact pre-op working-copy state for every path either tree may
	// touch. Validate before Join/read so a malicious tree path cannot escape root.
	rollbackPaths := sortedTreeUnion(v.commitTree(previousHead), v.commitTree(targetCommit))
	for _, path := range rollbackPaths {
		if err := validateRelPath(path); err != nil {
			return "", err
		}
	}
	diskBefore, err := snapshotWorkingFiles(r.RootPath, rollbackPaths)
	if err != nil {
		return "", fmt.Errorf("vcs: revert: snapshot: %w", err)
	}

	// (5) Materialize through the SAME plan the preview renders (V1). This
	// used to call materializeMainLocked, which walked the trees itself; the
	// preview would then have been a second walker, free to drift from what the
	// revert actually did. planRestoreLocked + applyRestorePlanLocked is one
	// computation with two consumers — see internal/vcs/preview.go.
	//
	// track=false: RevertToSeam moves main_head to targetCommit below, so the
	// working copy already matches the new head. Recording the same paths as
	// pending edits would make the next commit a no-op delta against itself.
	plan, err := v.planRestoreLocked(repoID, targetCommit, nil)
	if err != nil {
		return "", fmt.Errorf("vcs: revert: plan: %w", err)
	}
	if err := v.applyRestorePlanLocked(plan, false); err != nil {
		return "", fmt.Errorf("vcs: revert: materialize: %w", err)
	}
	keepMaterialized := false
	defer func() {
		if keepMaterialized {
			return
		}
		if rollbackErr := restoreWorkingFiles(r.RootPath, rollbackPaths, diskBefore); rollbackErr != nil {
			err = errors.Join(err,
				fmt.Errorf("vcs: revert: restore pre-operation files: %w", rollbackErr))
		}
	}()

	// (6) Atomic tx: reset head + insert undo + audit seams.
	tx, err := v.store.DB.Begin()
	if err != nil {
		return "", fmt.Errorf("vcs: revert: begin tx: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.Exec(
		"UPDATE vcs_repos SET main_head = ? WHERE id = ?", targetCommit, repoID,
	); err != nil {
		return "", fmt.Errorf("vcs: revert: reset head: %w", err)
	}

	undoID := newVCSID()
	auditID := newVCSID()
	now := time.Now().Unix()
	sessionID := seam.SessionID
	if historySnap == nil {
		// D4: an agent/tool revert is VCS-only. Keep its undo/audit seams outside
		// every WS session namespace; the returned exact ID still supports a later
		// VCS-only undo through the tool.
		sessionID = ""
	}
	turnSeq := seam.TurnSeq

	// UNDO seam: points at PREVIOUS head. Returning this id is how the caller
	// can undo this revert (re-revert to previousHead). turn_seq/history_len copy
	// the TARGET seam's boundary (the state being reverted TO); prev_turn_seq /
	// prev_history_len record the PRE-revert boundary (D2) so reverting THIS undo
	// seam restores the longer history.
	if _, err := tx.Exec(
		`INSERT INTO vcs_seams (id, repo_id, session_id, commit_id, turn_seq, history_len, prev_turn_seq, prev_history_len, history_snapshot, kind, label, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		undoID, repoID, sessionID, previousHead, turnSeq, seam.HistoryLen,
		prevTurnSeq, prevHistoryLen, historyJSON,
		string(SeamPreRevert), label, now,
	); err != nil {
		return "", fmt.Errorf("vcs: revert: insert undo seam: %w", err)
	}

	// AUDIT seam: points at TARGET commit. For audit trail only; never the
	// return value of RevertToSeam (reverting it would be a no-op). prev_* are 0
	// (audit seams are never reverted to).
	if _, err := tx.Exec(
		`INSERT INTO vcs_seams (id, repo_id, session_id, commit_id, turn_seq, history_len, prev_turn_seq, prev_history_len, history_snapshot, kind, label, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		auditID, repoID, sessionID, targetCommit, turnSeq, seam.HistoryLen,
		0, 0, []byte{},
		string(SeamPostRevert), label, now,
	); err != nil {
		return "", fmt.Errorf("vcs: revert: insert audit seam: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("vcs: revert: commit tx: %w", err)
	}
	keepMaterialized = true // DB head + seams now match disk; disable compensation.
	return undoID, nil
}
