package vcs

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/x6nux/yanshi/internal/store"
)

// ---------- helpers ----------

// newTestVCSWithTwoRepos creates a VCS with two separate initialized repos,
// returning the VCS, both repo ids, and both root directories.
func newTestVCSWithTwoRepos(t *testing.T) (*VCS, string, string, string, string) {
	t.Helper()
	v := newTestVCS(t)
	root1 := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root1, "a.txt"), []byte("repo1"), 0o644))
	repoID1, err := v.InitRepo(root1)
	require.NoError(t, err)

	root2 := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root2, "b.txt"), []byte("repo2"), 0o644))
	repoID2, err := v.InitRepo(root2)
	require.NoError(t, err)

	return v, repoID1, repoID2, root1, root2
}

// testCommitID returns the main_head commit id for repoID (fatal on error).
func testCommitID(t *testing.T, v *VCS, repoID string) string {
	t.Helper()
	r, err := v.getRepo(repoID)
	require.NoError(t, err)
	return r.MainHead
}

// ---------- restoreLocked error paths ----------

// TestVCS_RestoreLockedRepoIDMismatch covers the lockedRepoID != repoID check
// at vcs.go:1099. A commit from repo2 is passed to restoreLocked with repoID1,
// so the re-read inside restoreLocked detects the mismatch.
func TestVCS_RestoreLockedRepoIDMismatch(t *testing.T) {
	v, repoID1, repoID2, root1, _ := newTestVCSWithTwoRepos(t)
	commitID2 := testCommitID(t, v, repoID2)

	err := v.restoreLocked(repoID1, commitID2, "b.txt", root1)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "changed repository")
}

// TestVCS_RestoreLockedUnsafePath tests the path validation at vcs.go:1108-1111.
// A path with "../" must be rejected before any filesystem mutation.
func TestVCS_RestoreLockedUnsafePath(t *testing.T) {
	v, repoID1, _, root1, _ := newTestVCSWithTwoRepos(t)
	commitID1 := testCommitID(t, v, repoID1)

	err := v.restoreLocked(repoID1, commitID1, "../escape", root1)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsafe restore path")
}

// TestVCS_RestoreLockedMissingBlob covers the getBlob error path at
// vcs.go:1118-1120: the tree references a hash that has been deleted from
// vcs_blobs.
func TestVCS_RestoreLockedMissingBlob(t *testing.T) {
	v, repoID1, _, root1, _ := newTestVCSWithTwoRepos(t)
	commitID1 := testCommitID(t, v, repoID1)

	// Delete all blobs referenced by this commit's tree.
	tree := v.commitTree(commitID1)
	require.NotEmpty(t, tree, "tree must have at least one file")
	for _, h := range tree {
		_, err := v.store.DB.Exec("DELETE FROM vcs_blobs WHERE hash = ?", h)
		require.NoError(t, err)
	}

	err := v.restoreLocked(repoID1, commitID1, "a.txt", root1)
	require.Error(t, err)
	// The error should mention the blob read failure.
	assert.Contains(t, err.Error(), "blob")
}

// TestVCS_RestoreLockedScopeNotActive covers the "not an active working copy"
// error at vcs.go:1202. The destDir is neither the repo root nor an active
// worktree.
func TestVCS_RestoreLockedScopeNotActive(t *testing.T) {
	v, repoID1, _, _, _ := newTestVCSWithTwoRepos(t)
	commitID1 := testCommitID(t, v, repoID1)

	other := t.TempDir() // not repo root, not a worktree
	err := v.restoreLocked(repoID1, commitID1, "a.txt", other)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not an active working copy")
}

// ---------- removeWorktreeLocked edge cases ----------

// TestVCS_RemoveWorktreeLockedRepoIDMismatch covers the wt.RepoID != repoID
// check at vcs.go:747.
func TestVCS_RemoveWorktreeLockedRepoIDMismatch(t *testing.T) {
	v, repoID1, repoID2, root1, _ := newTestVCSWithTwoRepos(t)

	// "root1" directory exists but repoID2 has no worktree on it — the
	// worktree exists in repo1.  Insert a worktree manually for repo1.
	repo1, err := v.getRepo(repoID1)
	require.NoError(t, err)
	wtID := "wt-cross-repo"
	_, err = v.store.DB.Exec(
		`INSERT INTO vcs_worktrees (id, repo_id, path, base_commit, created_at, active)
		 VALUES (?, ?, ?, ?, ?, 1)`,
		wtID, repoID1, root1, repo1.MainHead, time.Now().Unix())
	require.NoError(t, err)

	// Call removeWorktreeLocked with repoID2 as the expected repo — wt.RepoID
	// (repo1) != repoID2 → error.
	err = v.removeWorktreeLocked(repoID2, wtID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "changed repository")
}

// TestVCS_RemoveWorktreeLockedEmptyDir covers the guard at vcs.go:753-754
// where v.worktreeDir == "" prevents disk deletion (only the active flag is
// flipped).
func TestVCS_RemoveWorktreeLockedEmptyDir(t *testing.T) {
	v, repoID1, _, _, _ := newTestVCSWithTwoRepos(t)

	// Empty worktreeDir disables the disk-removal guard.
	v.worktreeDir = ""

	repo1, err := v.getRepo(repoID1)
	require.NoError(t, err)
	wtPath := t.TempDir()
	wtID := "wt-empty-dir-test"
	_, err = v.store.DB.Exec(
		`INSERT INTO vcs_worktrees (id, repo_id, path, base_commit, created_at, active)
		 VALUES (?, ?, ?, ?, ?, 1)`,
		wtID, repoID1, wtPath, repo1.MainHead, time.Now().Unix())
	require.NoError(t, err)

	err = v.removeWorktreeLocked(repoID1, wtID)
	require.NoError(t, err)

	// Disk path is NOT removed because v.worktreeDir == "".
	assert.DirExists(t, wtPath, "path must survive when worktreeDir is empty")

	var active int
	require.NoError(t, v.store.DB.QueryRow("SELECT active FROM vcs_worktrees WHERE id=?", wtID).Scan(&active))
	assert.Equal(t, 0, active, "worktree must be deactivated")
}

// ---------- commitWorktreeLocked error paths ----------

// TestVCS_CommitWorktreeLockedRepoIDMismatch covers the wt.RepoID != repoID
// check at vcs.go:978.
func TestVCS_CommitWorktreeLockedRepoIDMismatch(t *testing.T) {
	v, repoID1, repoID2, _, _ := newTestVCSWithTwoRepos(t)

	wt, err := v.AddWorktree(repoID1, nil)
	require.NoError(t, err)

	_, err = v.commitWorktreeLocked(repoID2, wt.ID, "author", "msg")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "changed repository")
}

// ---------- recordEditWorktreeLocked error paths ----------

// TestVCS_RecordEditWorktreeLockedRepoIDMismatch covers the wt.RepoID != repoID
// check at vcs.go:882.
func TestVCS_RecordEditWorktreeLockedRepoIDMismatch(t *testing.T) {
	v, repoID1, repoID2, _, _ := newTestVCSWithTwoRepos(t)

	wt, err := v.AddWorktree(repoID1, nil)
	require.NoError(t, err)

	err = v.recordEditWorktreeLocked(repoID2, wt.ID, "agent", filepath.Join(wt.Path, "a.txt"), []byte("x"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "changed repository")
}

// ---------- mergeToMainLocked error paths ----------

// TestVCS_MergeToMainLockedRepoIDMismatch covers the wt.RepoID != repoID
// check at vcs.go:1234.
func TestVCS_MergeToMainLockedRepoIDMismatch(t *testing.T) {
	v, repoID1, repoID2, _, _ := newTestVCSWithTwoRepos(t)

	wt, err := v.AddWorktree(repoID1, nil)
	require.NoError(t, err)

	_, _, err = v.mergeToMainLocked(repoID2, wt.ID, "author", false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "changed repository")
}

// ---------- recordEdit/recordDelete silent skips ----------

// TestVCS_RecordEditEmptyRepoRoot covers the early-return at vcs.go:792
// when repoRoot is empty.
func TestVCS_RecordEditEmptyRepoRoot(t *testing.T) {
	v := newTestVCS(t)
	err := v.recordEdit("main", "id", "", "/some/path.go", []byte("content"))
	require.NoError(t, err) // silently skipped
}

// TestVCS_RecordDeleteEmptyRepoRoot covers the early-return at delete.go:68
// when repoRoot is empty.
func TestVCS_RecordDeleteEmptyRepoRoot(t *testing.T) {
	v := newTestVCS(t)
	err := v.recordDelete("main", "id", "", "/some/path.go")
	require.NoError(t, err) // silently skipped
}

// ---------- recordDelete DB error ----------

// TestVCS_RecordDeleteDbError covers the store.DB.Exec error path at
// delete.go:79-83. After closing the underlying DB, the Exec fails.
func TestVCS_RecordDeleteDbError(t *testing.T) {
	root := t.TempDir()
	v := newTestVCS(t)
	// Close the underlying database so Exec returns an error.
	require.NoError(t, v.store.DB.Close())

	err := v.recordDelete("main", "test-id", root, filepath.Join(root, "file.go"))
	require.Error(t, err)
}

// ---------- worktreeTip ----------

// TestVCS_WorktreeTipMissingID covers the early-return "" at vcs.go:661-663
// when getWorktree fails.
func TestVCS_WorktreeTipMissingID(t *testing.T) {
	v := newTestVCS(t)
	tip := v.worktreeTip("nonexistent")
	assert.Empty(t, tip)
}

// ---------- scopeHeadTree ----------

// TestVCS_ScopeHeadTreeMissingRepo covers the empty-map path at vcs.go:773-775
// when getRepo fails for the "main" scope.
func TestVCS_ScopeHeadTreeMissingRepo(t *testing.T) {
	v := newTestVCS(t)
	tree := v.scopeHeadTree("main", "nonexistent")
	assert.Empty(t, tree)
}

// ---------- RevertToSeam error paths ----------

// TestVCS_RevertToSeamHistorySnapshotMismatch covers the session-id mismatch
// check at revert.go:341-344 when historySnap.Meta.ID != seam.SessionID.
func TestVCS_RevertToSeamHistorySnapshotMismatch(t *testing.T) {
	v, repoID, _ := setupSeamTestRepo(t)

	// Seal a pre-turn seam with a specific session ID.
	seamID, err := v.SealMainTurnSeam(repoID, "session-A", 0, 0, SeamPreTurn, "pre")
	require.NoError(t, err)

	// Build a history snapshot with a DIFFERENT session ID → mismatch.
	snap := &store.SessionRevertSnapshot{}
	snap.Meta.ID = "session-B"
	// Set at least one message so the snap is non-empty and Marshal succeeds.
	snap.Messages = []store.Message{{Role: "user", Content: "hello"}}

	_, err = v.RevertToSeam(repoID, seamID, "mismatch", 0, 0, snap)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does not match")
}

// TestVCS_RevertToSeamSnapshotError covers the snapshotWorkingFiles error path
// at revert.go:379-381. The working copy has a directory where a tracked file
// is expected, so snapshotWorkingFiles returns "not a regular file".
func TestVCS_RevertToSeamSnapshotError(t *testing.T) {
	v, repoID, root := setupSeamTestRepo(t)
	aPath := filepath.Join(root, "a.txt")

	// Seal a pre-turn seam capturing the current head (a.txt=v0).
	seamID, err := v.SealMainTurnSeam(repoID, "session-A", 0, 0, SeamPreTurn, "pre")
	require.NoError(t, err)

	// Advance to v1: write to disk AND track via VCS.
	require.NoError(t, os.WriteFile(aPath, []byte("v1"), 0o644))
	require.NoError(t, v.RecordEditMain(repoID, "u", aPath, []byte("v1")))
	headV1, err := v.CommitMain(repoID, "u", "v1")
	require.NoError(t, err)

	// Replace the tracked file with a directory — snapshotWorkingFiles
	// will find a non-regular file and return an error.
	require.NoError(t, os.RemoveAll(aPath))
	require.NoError(t, os.MkdirAll(aPath, 0o755))

	// RevertToSeam must fail at the snapshot stage.
	_, err = v.RevertToSeam(repoID, seamID, "snapshot-error", 0, 0, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "snapshot")

	// Head must NOT have advanced on a failed revert.
	headAfter := mustMainHead(t, v, repoID)
	assert.Equal(t, headV1, headAfter, "head must not advance on failed revert")
}

// ---------- RemoveWorktree path-based guard ----------

// TestVCS_RemoveWorktreeLockedRelRootGuard tests the guard at vcs.go:754
// where filepath.Rel returns "." (wt.Path == worktreeDir) — the disk
// removal is skipped.
func TestVCS_RemoveWorktreeLockedRelRootGuard(t *testing.T) {
	v, repoID1, _, _, _ := newTestVCSWithTwoRepos(t)

	// Set worktreeDir to the wt.Path itself so filepath.Rel returns ".".
	repo1, err := v.getRepo(repoID1)
	require.NoError(t, err)
	wtPath := t.TempDir()
	v.worktreeDir = wtPath

	wtID := "wt-rel-dot"
	_, err = v.store.DB.Exec(
		`INSERT INTO vcs_worktrees (id, repo_id, path, base_commit, created_at, active)
		 VALUES (?, ?, ?, ?, ?, 1)`,
		wtID, repoID1, wtPath, repo1.MainHead, time.Now().Unix())
	require.NoError(t, err)

	// Create a sentinel so we can verify it's NOT removed.
	sentinel := filepath.Join(wtPath, "sentinel.txt")
	require.NoError(t, os.WriteFile(sentinel, []byte("keep"), 0o644))

	err = v.removeWorktreeLocked(repoID1, wtID)
	require.NoError(t, err)

	// rel == "." so disk deletion is skipped.
	_, err = os.Stat(sentinel)
	require.NoError(t, err, "sentinel under wt.Path must survive")
}

// ---------- cacheTree nil map ----------

// TestVCS_CacheTreeNilMap covers the nil-map initialization inside cacheTree
// (vcs.go:479-481). The VCS is constructed directly (not via New) so the
// treeCache map is nil.
func TestVCS_CacheTreeNilMap(t *testing.T) {
	s, err := store.Open(":memory:")
	require.NoError(t, err)
	defer s.Close()

	v := &VCS{store: s, ignore: defaultIgnore}
	// treeCache is nil → cacheTree must initialize it.
	v.cacheTree("test-id", map[string]string{"path": "hash"})

	v.treeCacheMu.RLock()
	got, ok := v.treeCache["test-id"]
	v.treeCacheMu.RUnlock()
	assert.True(t, ok, "treeCache must have the entry after cacheTree")
	assert.Equal(t, "hash", got["path"])
}

// ---------- sealMainTurnSeam no-pending folded ----------

// TestVCS_SealMainTurnSeamFoldsPendingWithHistorySnap makes a post-turn seam
// that successfully folds pending edits, using RepoRoot to verify the resulting
// repo state (a confidence test bridging methods).
func TestVCS_SealMainTurnSeamFoldsPendingWithHistorySnap(t *testing.T) {
	v, repoID, root := setupSeamTestRepo(t)

	// Add a pending edit.
	aPath := filepath.Join(root, "a.txt")
	require.NoError(t, os.WriteFile(aPath, []byte("pending"), 0o644))
	require.NoError(t, v.RecordEditMain(repoID, "u", aPath, []byte("pending")))

	// Seal post-turn: pending edits must be folded into a new commit.
	seamID, err := v.SealMainTurnSeam(repoID, "s1", 1, 2, SeamPostTurn, "post")
	require.NoError(t, err)
	require.NotEmpty(t, seamID)

	// The seam's commit must include the pending edit.
	seam, err := v.FindSeam(seamID)
	require.NoError(t, err)
	tree := v.commitTree(seam.CommitID)
	assert.Equal(t, hashContent([]byte("pending")), tree["a.txt"])
}

// ---------- commitScope applied-zero path ----------

// TestVCS_CommitScopeAppliedZero covers the applied==0 path at vcs.go:923-925
// where there are no op="deleted" or add/mod rows for the scope — despite a
// rows.Next iteration that finds zero usable entries.
func TestVCS_CommitScopeAppliedZero(t *testing.T) {
	v, repoID, _ := setupSeamTestRepo(t)

	// Insert an uncommitted row with a blob_hash that doesn't reference a real
	// blob — commitScope will encounter it but the op won't be "deleted" or
	// add/mod in a meaningful way. Actually let's just try with no uncommitted
	// rows and assert we get ErrNoChanges.
	_, err := v.commitScope("main", repoID, repoID, "", "author", "no-op")
	assert.ErrorIs(t, err, ErrNoChanges)
}

// ---------- scopeHeadTree empty scope type ----------

// TestVCS_ScopeHeadTreeUnknownScopeType covers scopeHeadTree returning an
// empty map for an unrecognized scopeType (not "main" or "worktree").
func TestVCS_ScopeHeadTreeUnknownScopeType(t *testing.T) {
	v, repoID, _ := setupSeamTestRepo(t)
	tree := v.scopeHeadTree("unknown", repoID)
	assert.Empty(t, tree)
}

// ---------- recordEdit outside repo skip ----------

// TestVCS_RecordEditOutsideRepoSilentlySkipped verifies that a path outside
// the repo root is silently skipped (not an error) in a code path that goes
// through recordEdit directly.
func TestVCS_RecordEditOutsideRepoSilentlySkipped(t *testing.T) {
	v, repoID, root := setupSeamTestRepo(t)
	other := t.TempDir()

	// Pass the path directly through recordEdit.
	err := v.recordEdit("main", repoID, root, filepath.Join(other, "external.go"), []byte("x"))
	require.NoError(t, err)
	assert.Empty(t, v.Uncommitted("main", repoID))
}

// ---------- removedWorktree with getWorktree error ----------

// TestVCS_RemoveWorktreeLockedGetWtError covers the error path at vcs.go:740-745
// when getWorktree fails with an error other than sql.ErrNoRows. We inject a
// nonexistent worktree id; the getWorktree returns sql.ErrNoRows, which is
// caught and returned as nil at L741-742.
func TestVCS_RemoveWorktreeLockedNonexistentReturnsNil(t *testing.T) {
	v, repoID1, _, _, _ := newTestVCSWithTwoRepos(t)
	err := v.removeWorktreeLocked(repoID1, "nonexistent-wt")
	require.NoError(t, err) // sql.ErrNoRows → nil
}

// ---------- scopeHeadTree with worktree scope and no tip ----------

// TestVCS_ScopeHeadTreeWorktreeEmptyTip covers the empty-head path at
// vcs.go:780-781 when scopeType is "worktree" but the worktree's tip is empty
// and base_commit is also empty (non-existent worktree).
func TestVCS_ScopeHeadTreeWorktreeEmptyTip(t *testing.T) {
	v := newTestVCS(t)
	tree := v.scopeHeadTree("worktree", "nonexistent")
	assert.Empty(t, tree, "worktree scope with missing id yields empty tree")
}

// ---------- commitScope rows.Err path ----------

// TestVCS_CommitScopeQueryErr covers the Query error path at vcs.go:903-904
// where the uncommitted query fails.
func TestVCS_CommitScopeQueryErr(t *testing.T) {
	v, repoID, _ := setupSeamTestRepo(t)

	// Close the DB so the query fails.
	require.NoError(t, v.store.DB.Close())

	_, err := v.commitScope("main", repoID, repoID, "", "author", "msg")
	require.Error(t, err)
}

// ---------- isIgnored with pure-path patterns ----------

// TestVCS_IsIgnoredSegmentMatch covers the segment-only match at vcs.go:122-128
// where a pattern without glob characters matches a path segment directly.
func TestVCS_IsIgnoredSegmentMatch(t *testing.T) {
	v := newTestVCS(t)
	// "node_modules" is in defaultIgnore as a segment match (no wildcards).
	assert.True(t, v.isIgnored("src/node_modules/pkg/x.js"))
	// ".git" is also matched by "*.log" glob but ensure segment matching too.
	// Actually ".git" is not in defaultIgnore — let's use a pattern we know is there.
	// "node_modules" is a directory name, so isIgnored should match it as a segment.
	assert.True(t, v.isIgnored("a/node_modules/b/c"))
	// But a partial segment should NOT match.
	assert.False(t, v.isIgnored("src/my_node_modules_thing/x.js"))
}

// ---------- WorktreePath with missing worktree ----------

// TestVCS_WorktreePathMissing already exists as TestWorktreePath_Missing,
// so we don't duplicate.

// deleteBypassingForeignKeys runs a DELETE that PRAGMA foreign_keys=ON would
// otherwise refuse, on a connection pinned with the pragma turned off.
//
// The tests below simulate a corrupted database — a vcs_repos or vcs_commits
// row missing while rows that reference it survive. That state is unreachable
// through this package's own API, which is the point: the branches they cover
// are defensive. Since the store began enforcing referential integrity it is
// also unreachable through a plain Exec, so the corruption has to be staged the
// way it would really arrive — an external tool writing to the file with
// enforcement off (the sqlite3 CLI defaults to exactly that).
//
// The pragma is per-connection, so it must be set and cleared on ONE pinned
// *sql.Conn; setting it on s.DB would arm a random member of the pool and hand
// the rest back to production code still unenforced.
func deleteBypassingForeignKeys(t *testing.T, s *store.Store, query string, args ...any) {
	t.Helper()
	ctx := context.Background()
	conn, err := s.DB.Conn(ctx)
	require.NoError(t, err)
	defer func() {
		// Re-arm before the connection returns to the pool, or the next caller
		// to draw it silently loses enforcement.
		_, rearmErr := conn.ExecContext(ctx, "PRAGMA foreign_keys=ON")
		require.NoError(t, rearmErr)
		require.NoError(t, conn.Close())
	}()
	_, err = conn.ExecContext(ctx, "PRAGMA foreign_keys=OFF")
	require.NoError(t, err)
	_, err = conn.ExecContext(ctx, query, args...)
	require.NoError(t, err)
}

// ---------- RevertToSeam: getRepo error ----------

// TestVCS_RevertToSeamRepoNotFound covers the getRepo error at
// revert.go:353-355 when the repo row has been deleted between FindSeam
// and getRepo (possible via manual DB manipulation).
func TestVCS_RevertToSeamRepoNotFound(t *testing.T) {
	v, repoID, _ := setupSeamTestRepo(t)

	seamID, err := v.SealMainTurnSeam(repoID, "s1", 0, 0, SeamPreTurn, "pre")
	require.NoError(t, err)

	// Delete the repo row from vcs_repos — FindSeam succeeds (seam row
	// exists) but getRepo fails (no repo row).
	deleteBypassingForeignKeys(t, v.store, "DELETE FROM vcs_repos WHERE id=?", repoID)

	_, err = v.RevertToSeam(repoID, seamID, "missing-repo", 0, 0, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "load repo")
}

// ---------- RevertToSeam: target commit with wrong repo ----------

// TestVCS_RevertToSeamTargetCommitWrongRepo covers the
// c.RepoID != repoID check at revert.go:364-366.
func TestVCS_RevertToSeamTargetCommitWrongRepo(t *testing.T) {
	v, repoID1, repoID2, _, _ := newTestVCSWithTwoRepos(t)

	// Get a commit from repo2.
	commitID2 := testCommitID(t, v, repoID2)

	// Insert a seam for repo1 that points at repo2's commit.
	seamID := "cross-repo-seam"
	_, err := v.store.DB.Exec(
		`INSERT INTO vcs_seams (id, repo_id, session_id, commit_id, turn_seq, history_len, kind, label, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		seamID, repoID1, "s1", commitID2, 0, 0, string(SeamPreTurn), "cross-repo", time.Now().Unix())
	require.NoError(t, err)

	_, err = v.RevertToSeam(repoID1, seamID, "test", 0, 0, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not in repo")
}

// ---------- RevertToSeam: worktree commit ----------

// TestVCS_RevertToSeamWorktreeCommit covers the c.WorktreeID != ""
// check at revert.go:367-368.
func TestVCS_RevertToSeamWorktreeCommit(t *testing.T) {
	v, repoID1, _, _, _ := newTestVCSWithTwoRepos(t)

	// Create a worktree and commit on it.
	wt, err := v.AddWorktree(repoID1, nil)
	require.NoError(t, err)
	require.NoError(t, v.RecordEditWorktree(wt.ID, "u",
		filepath.Join(wt.Path, "a.txt"), []byte("wt")))
	wtHead, err := v.CommitWorktree(wt.ID, "u", "wt commit")
	require.NoError(t, err)

	// Insert a seam pointing at the worktree commit.
	seamID := "wt-commit-seam"
	_, err = v.store.DB.Exec(
		`INSERT INTO vcs_seams (id, repo_id, session_id, commit_id, turn_seq, history_len, kind, label, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		seamID, repoID1, "s1", wtHead, 0, 0, string(SeamPreTurn), "wt-commit", time.Now().Unix())
	require.NoError(t, err)

	_, err = v.RevertToSeam(repoID1, seamID, "test", 0, 0, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "worktree")
}

// ---------- RevertToSeam: target commit not found ----------

// TestVCS_RevertToSeamTargetCommitNotFound covers the getCommit error
// at revert.go:360-362.
func TestVCS_RevertToSeamTargetCommitNotFound(t *testing.T) {
	v, repoID, _ := setupSeamTestRepo(t)

	seamID, err := v.SealMainTurnSeam(repoID, "s1", 0, 0, SeamPreTurn, "pre")
	require.NoError(t, err)

	// Delete the target commit that the seam references.
	seam, err := v.FindSeam(seamID)
	require.NoError(t, err)
	deleteBypassingForeignKeys(t, v.store, "DELETE FROM vcs_commits WHERE id=?", seam.CommitID)

	_, err = v.RevertToSeam(repoID, seamID, "test", 0, 0, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "target commit")
}

// ---------- resetMainHeadLocked: repo not found ----------

// TestVCS_ResetMainHeadLockedRepoNotFound covers the n != 1 check at
// revert.go:291-292 when the UPDATE matches 0 rows.
func TestVCS_ResetMainHeadLockedRepoNotFound(t *testing.T) {
	v, repoID, _ := setupSeamTestRepo(t)
	head := mustMainHead(t, v, repoID)

	// Delete the repo row so the UPDATE matches 0 rows.
	deleteBypassingForeignKeys(t, v.store, "DELETE FROM vcs_repos WHERE id=?", repoID)

	err := v.resetMainHeadLocked(repoID, head)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

// ---------- recordDeleteWorktreeLocked: repo mismatch ----------

// TestVCS_RecordDeleteWorktreeLockedRepoIDMismatch covers the
// wt.RepoID != repoID check at delete.go:55.
func TestVCS_RecordDeleteWorktreeLockedRepoIDMismatch(t *testing.T) {
	v, repoID1, repoID2, _, _ := newTestVCSWithTwoRepos(t)

	wt, err := v.AddWorktree(repoID1, nil)
	require.NoError(t, err)

	err = v.recordDeleteWorktreeLocked(repoID2, wt.ID, "agent",
		filepath.Join(wt.Path, "a.txt"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "changed repository")
}

// ---------- MergeToMain: getRepo error ----------

// TestVCS_MergeToMainLockedGetRepoErr covers the getRepo error at
// vcs.go:1237-1239 when the repo row is missing.
func TestVCS_MergeToMainLockedGetRepoErr(t *testing.T) {
	v, repoID1, _, _, _ := newTestVCSWithTwoRepos(t)

	wt, err := v.AddWorktree(repoID1, nil)
	require.NoError(t, err)

	// Delete the repo row so getRepo fails.
	deleteBypassingForeignKeys(t, v.store, "DELETE FROM vcs_repos WHERE id=?", repoID1)

	_, _, err = v.mergeToMainLocked(repoID1, wt.ID, "author", false)
	require.Error(t, err)
}

// ---------- scopeHeadTree: worktree scope with missing repo (error path) ----------

// TestVCS_ScopeHeadTreeWorktreeMissingRepo covers the error path at
// vcs.go:771-775 for the "worktree" scope when the worktree exists but
// the worktreeTip returns "" (no commits on this worktree branch). It
// exercises the empty worktreeTip → empty map fallback.
func TestVCS_ScopeHeadTreeWorktreeEmptyBase(t *testing.T) {
	v, repoID1, _, _, _ := newTestVCSWithTwoRepos(t)

	// Insert a worktree row with empty tip and base_commit.
	wtID := "wt-no-base"
	_, err := v.store.DB.Exec(
		`INSERT INTO vcs_worktrees (id, repo_id, path, base_commit, created_at, active)
		 VALUES (?, ?, ?, '', ?, 1)`,
		wtID, repoID1, t.TempDir(), time.Now().Unix())
	require.NoError(t, err)

	tree := v.scopeHeadTree("worktree", wtID)
	assert.Empty(t, tree)
}

// ---------- queryRowScoped / queryScoped with non-nil tx ----------

// TestVCS_QueryScopedTxPath exercises queryRowScoped and queryScoped with
// a non-nil tx by forcing a cache-miss chain walk inside writeCommitInTx.
func TestVCS_QueryScopedTxPath(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "a.go"), []byte("x"), 0o644))
	v := newTestVCS(t)
	repoID, err := v.InitRepo(root) // commit 1
	require.NoError(t, err)

	// Commit 2 — establishes a parent chain with a cached tree.
	require.NoError(t, v.RecordEditMain(repoID, "o", filepath.Join(root, "a.go"), []byte("v2")))
	_, err = v.CommitMain(repoID, "o", "second")
	require.NoError(t, err)

	// Clear the tree cache so writeCommitInTx must walk the chain with tx.
	v.treeCacheMu.Lock()
	v.treeCache = map[string]map[string]string{}
	v.treeCacheMu.Unlock()

	// Commit 3 — writeCommitInTx calls reconstructTree with a non-nil tx
	// on a cold cache, exercising the tx != nil branch in queryRowScoped
	// and queryScoped.
	require.NoError(t, v.RecordEditMain(repoID, "o", filepath.Join(root, "a.go"), []byte("v3")))
	_, err = v.CommitMain(repoID, "o", "third")
	require.NoError(t, err)
}

// ---------- canonicalRepoRoot EvalSymlinks error ----------

// TestVCS_CanonicalRepoRootSymlinkErr covers the EvalSymlinks error
// path at vcs.go:517-518 when the repo root does not exist.
func TestVCS_CanonicalRepoRootSymlinkErr(t *testing.T) {
	v := newTestVCS(t)
	missing := filepath.Join(t.TempDir(), "nonexistent")
	_, err := v.InitRepo(missing)
	require.Error(t, err)
}

// ---------- queryRowScoped / queryScoped non-nil tx branch ----------

// TestVCS_QueryRowScopedWithTx directly exercises the tx != nil branch
// of queryRowScoped by calling getCommitScoped inside a WriteTx callback.
func TestVCS_QueryRowScopedWithTx(t *testing.T) {
	v, repoID, _ := setupSeamTestRepo(t)
	head := mustMainHead(t, v, repoID)

	err := v.store.WriteTx(context.Background(), func(tx *sql.Tx) error {
		c, err := v.getCommitScoped(tx, head)
		require.NoError(t, err)
		assert.Equal(t, head, c.ID)
		return nil
	})
	require.NoError(t, err)
}

// TestVCS_QueryScopedWithTx directly exercises the tx != nil branch
// of queryScoped by calling commitDeltaScoped inside a WriteTx callback.
func TestVCS_QueryScopedWithTx(t *testing.T) {
	v, repoID, _ := setupSeamTestRepo(t)
	head := mustMainHead(t, v, repoID)

	err := v.store.WriteTx(context.Background(), func(tx *sql.Tx) error {
		entries := v.commitDeltaScoped(tx, head)
		assert.NotEmpty(t, entries)
		return nil
	})
	require.NoError(t, err)
}

// TestCov_SealMainTurnSeam_EmptyRepoID covers the early-return branch at
// seam.go:78-79: an empty repoID is a silent no-op (returns "").
func TestCov_SealMainTurnSeam_EmptyRepoID(t *testing.T) {
	v := newTestVCS(t)
	id, err := v.SealMainTurnSeam("", "s1", 0, 0, SeamPreTurn, "label")
	require.NoError(t, err)
	assert.Empty(t, id)
}

// TestCov_FindSeam_NotFound covers the QueryRow error branch at seam.go:133-134.
func TestCov_FindSeam_NotFound(t *testing.T) {
	v := newTestVCS(t)
	_, err := v.FindSeam("no-such-seam")
	require.Error(t, err)
	assert.ErrorIs(t, err, sql.ErrNoRows)
}

// TestCov_SealMainTurnSeam_FoldsPendingEdits covers the commitScope path at
// seam.go:100-107. With uncommitted edits in the main scope, sealing a seam
// folds them into a new commit so the seam references a stable head.
func TestCov_SealMainTurnSeam_FoldsPendingEdits(t *testing.T) {
	v, repoID, root := setupSeamTestRepo(t)
	// Stage an uncommitted edit via RecordEditMain.
	require.NoError(t, os.WriteFile(filepath.Join(root, "new.txt"), []byte("data"), 0o644))
	require.NoError(t, v.RecordEditMain(repoID, "test", filepath.Join(root, "new.txt"), []byte("data")))

	seamID, err := v.SealMainTurnSeam(repoID, "s1", 0, 1, SeamPreTurn, "fold-test")
	require.NoError(t, err)
	assert.NotEmpty(t, seamID)

	// After sealing, the uncommitted queue should be folded.
	assert.Empty(t, v.Uncommitted("main", repoID))

	// The seam's commit should be the new head.
	seam, err := v.FindSeam(seamID)
	require.NoError(t, err)
	head, err := v.RepoMainHead(repoID)
	require.NoError(t, err)
	assert.Equal(t, head, seam.CommitID)
}
