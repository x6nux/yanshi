package vcs

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/x6nux/yanshi/internal/store"
)

func newTestVCS(t *testing.T) *VCS {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return New(s, t.TempDir())
}

func TestNew_DefaultIgnoreIncludesNodeModules(t *testing.T) {
	v := newTestVCS(t)
	assert.True(t, v.isIgnored("node_modules/foo.js"))
	assert.True(t, v.isIgnored("vendor/x.go"))
	assert.True(t, v.isIgnored("app/dist/bundle.js"))
	assert.False(t, v.isIgnored("src/main.go"))
	// the SQLite store file is never tracked into a commit
	assert.True(t, v.isIgnored("yanshi.db"), "*.db / yanshi.db must be ignored by default")
	// compiled artifacts are never tracked (the 38MB yanshi.exe db-bloat root cause)
	for _, bin := range []string{"yanshi.exe", "build/app.dll", "libfoo.so", "libbar.dylib", "obj.o", "lib.a"} {
		assert.Truef(t, v.isIgnored(bin), "binary artifact %q must be ignored by default", bin)
	}
	assert.False(t, v.isIgnored("src/main.go"), "source files are still tracked")
}

func TestNew_ExtraIgnoreMerged(t *testing.T) {
	s, _ := store.Open(":memory:")
	defer s.Close()
	v := New(s, t.TempDir(), "coverage/*", ".cache")
	assert.True(t, v.isIgnored("coverage/lcov.info"))
	assert.True(t, v.isIgnored("node_modules/x"))
	assert.True(t, v.isIgnored("some/.cache/thing"))
	assert.False(t, v.isIgnored("src/main.go"))
}

func TestBlob_PutGet(t *testing.T) {
	v := newTestVCS(t)
	h := v.putBlob([]byte("hello"))
	// sha256("hello")
	assert.Equal(t, "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824", h)
	got, err := v.getBlob(h)
	require.NoError(t, err)
	assert.Equal(t, "hello", string(got))
	// dedup: same content → same hash, no duplicate
	h2 := v.putBlob([]byte("hello"))
	assert.Equal(t, h, h2)
}

func TestBlob_GetMissing(t *testing.T) {
	v := newTestVCS(t)
	_, err := v.getBlob("nonexistent")
	assert.Error(t, err)
}

func TestInitRepo_ScansAndIgnores(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "a.go"), []byte("package main"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "node_modules", "dep"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "node_modules", "dep", "x.js"), []byte("ignored"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, ".yanshiignore"), []byte("secret.txt\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "secret.txt"), []byte("topsecret"), 0o644))

	v := newTestVCS(t)
	repoID, err := v.InitRepo(root)
	require.NoError(t, err)
	require.NotEmpty(t, repoID)

	repo, err := v.getRepo(repoID)
	require.NoError(t, err)
	require.NotEmpty(t, repo.MainHead, "main_head must be set")

	tree := v.commitTree(repo.MainHead)
	assert.Contains(t, tree, "a.go")
	assert.NotContains(t, tree, "node_modules/dep/x.js")
	assert.NotContains(t, tree, "secret.txt", ".yanshiignore should exclude it")
	// a.go blob is the sha256 of its content
	assert.Equal(t, hashContent([]byte("package main")), tree["a.go"])
	// the .yanshiignore file itself is tracked (intentional — it's a real project file)
	assert.Contains(t, tree, ".yanshiignore")
}

func TestInitRepo_RelativePaths(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "src", "sub"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "src", "sub", "b.go"), []byte("x"), 0o644))
	v := newTestVCS(t)
	repoID, err := v.InitRepo(root)
	require.NoError(t, err)
	repo, _ := v.getRepo(repoID)
	tree := v.commitTree(repo.MainHead)
	assert.Contains(t, tree, "src/sub/b.go", "tree paths are repo-relative forward-slash")
}

// TestInitRepo_IdempotentPerRoot pins the restart-history-persistence fix.
// Bootstrap calls InitRepo(workRoot) on every Build/serve boot over the SAME
// persistent store. A naive InitRepo mints a fresh repo id + init commit each
// time, orphaning all prior history (each restart resets main_head). The fix
// reuses an existing vcs_repos row for the same absolute root (returning its id,
// preserving main_head) and only scans + inserts when none exists. Different
// roots must get distinct repo ids.
func TestInitRepo_IdempotentPerRoot(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "a.go"), []byte("package main"), 0o644))

	// S1 simulates the persisted store that survives a process restart.
	s1, err := store.Open(":memory:")
	require.NoError(t, err)
	defer s1.Close()

	// First boot: scan + create the repo.
	v1 := New(s1, t.TempDir())
	id1, err := v1.InitRepo(root)
	require.NoError(t, err)
	require.NotEmpty(t, id1)

	// Advance main_head with a real edit commit — history that must survive a
	// restart. c1 is the commit main_head must still point at afterwards.
	require.NoError(t, v1.RecordEditMain(id1, "orchestrator", filepath.Join(root, "a.go"), []byte("package main // edited")))
	c1, err := v1.CommitMain(id1, "orchestrator", "edit a.go")
	require.NoError(t, err)
	require.NotEmpty(t, c1)

	// Restart: a NEW VCS over the SAME store S1 (the persisted db). InitRepo must
	// reuse the existing repo — NOT mint a new id, NOT re-scan, NOT reset main_head.
	v2 := New(s1, t.TempDir())
	id2, err := v2.InitRepo(root)
	require.NoError(t, err)

	assert.Equal(t, id1, id2, "same root → same repo id (reused, not a new repo)")

	// main_head must still point at the edit commit c1, not a fresh init commit.
	repo2, err := v2.getRepo(id2)
	require.NoError(t, err)
	assert.Equal(t, c1, repo2.MainHead, "main_head preserved across restart (history not reset)")

	// LogMain via v2 walks the full chain: the edit commit + the init commit.
	log, err := v2.LogMain(id2, 10)
	require.NoError(t, err)
	require.Len(t, log, 2, "history = init + edit commit")
	assert.Equal(t, "edit a.go", log[0].Message, "newest commit first")
	assert.Equal(t, "vcs init", log[1].Message)

	// A DIFFERENT root must get a distinct repo id (not confused with root's).
	other := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(other, "b.go"), []byte("x"), 0o644))
	id3, err := v2.InitRepo(other)
	require.NoError(t, err)
	assert.NotEqual(t, id1, id3, "different root → different repo id")
}

func TestRecordEdit_MainTracksToUncommitted(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "a.go"), []byte("package main"), 0o644))
	v := newTestVCS(t)
	repoID, err := v.InitRepo(root)
	require.NoError(t, err)

	err = v.RecordEditMain(repoID, "orchestrator", filepath.Join(root, "a.go"), []byte("package main // edited"))
	require.NoError(t, err)

	pending := v.Uncommitted("main", repoID)
	assert.Contains(t, pending, "a.go")
	assert.Equal(t, hashContent([]byte("package main // edited")), pending["a.go"])
}

func TestRecordEdit_NewFileAdded(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "a.go"), []byte("x"), 0o644))
	v := newTestVCS(t)
	repoID, _ := v.InitRepo(root)

	err := v.RecordEditMain(repoID, "orchestrator", filepath.Join(root, "new.go"), []byte("new"))
	require.NoError(t, err)
	pending := v.Uncommitted("main", repoID)
	assert.Contains(t, pending, "new.go")
}

func TestRecordEdit_IgnoredSkipped(t *testing.T) {
	root := t.TempDir()
	v := newTestVCS(t)
	repoID, _ := v.InitRepo(root)
	err := v.RecordEditMain(repoID, "orchestrator", filepath.Join(root, "node_modules", "x.js"), []byte("x"))
	require.NoError(t, err) // no error, just skipped
	assert.Empty(t, v.Uncommitted("main", repoID))
}

func TestRecordEdit_OutsideRepoSkipped(t *testing.T) {
	root := t.TempDir()
	other := t.TempDir()
	v := newTestVCS(t)
	repoID, _ := v.InitRepo(root)
	err := v.RecordEditMain(repoID, "orchestrator", filepath.Join(other, "external.go"), []byte("x"))
	require.NoError(t, err)
	assert.Empty(t, v.Uncommitted("main", repoID))
}

func TestRecordEdit_WorktreeScope(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "a.go"), []byte("x"), 0o644))
	v := newTestVCS(t)
	repoID, _ := v.InitRepo(root)
	repo, _ := v.getRepo(repoID)
	// insert a worktree row manually (AddWorktree is V9)
	wtID := "wt-test"
	_, err := v.store.DB.Exec(
		"INSERT INTO vcs_worktrees (id, repo_id, path, base_commit, created_at, active) VALUES (?, ?, ?, ?, ?, 1)",
		wtID, repoID, root, repo.MainHead, time.Now().Unix())
	require.NoError(t, err)

	err = v.RecordEditWorktree(wtID, "worker-1", filepath.Join(root, "a.go"), []byte("edited"))
	require.NoError(t, err)
	assert.Contains(t, v.Uncommitted("worktree", wtID), "a.go")
	// main scope untouched
	assert.Empty(t, v.Uncommitted("main", repoID))
}

// TestRecordEdit_WorktreeResolvesAgainstWorktreePath is the regression test for
// the realistic agent flow: a worktree created via AddWorktree has its working
// dir (wt.Path) OUTSIDE the repo root, under worktreeDir. RecordEditWorktree
// must resolve the edit's absPath relative to wt.Path (not the repo root), so
// an edit to wt.Path/a.go is tracked under the repo-relative key "a.go", while
// an edit to a file outside wt.Path (e.g. in the repo root) is silently skipped.
func TestRecordEdit_WorktreeResolvesAgainstWorktreePath(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "a.go"), []byte("x"), 0o644))
	v := newTestVCS(t)
	repoID, err := v.InitRepo(root)
	require.NoError(t, err)

	wt, err := v.AddWorktree(repoID, nil)
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(wt.Path, v.worktreeDir),
		"worktree working dir must live under worktreeDir, outside the repo root")
	require.NotEqual(t, root, wt.Path, "wt.Path must differ from repo root for this test to be meaningful")

	// Edit a.go UNDER the worktree's working dir — the realistic agent flow.
	err = v.RecordEditWorktree(wt.ID, "w1", filepath.Join(wt.Path, "a.go"), []byte("edited"))
	require.NoError(t, err)
	pending := v.Uncommitted("worktree", wt.ID)
	assert.Equal(t, hashContent([]byte("edited")), pending["a.go"],
		"edit under wt.Path must be tracked under its repo-relative key")

	// An edit to a file OUTSIDE wt.Path (the repo root) must be skipped:
	// wt.Path is the base for relative resolution, so root/a.go is outside.
	err = v.RecordEditWorktree(wt.ID, "w1", filepath.Join(root, "a.go"), []byte("from-root"))
	require.NoError(t, err)
	pending = v.Uncommitted("worktree", wt.ID)
	assert.Len(t, pending, 1, "edit outside wt.Path must not add a second entry")
	assert.Equal(t, hashContent([]byte("edited")), pending["a.go"],
		"the outside-wt.Path edit must not overwrite the tracked entry")
}

func TestCommitMain_CreatesCommitAdvancesHead(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "a.go"), []byte("package main"), 0o644))
	v := newTestVCS(t)
	repoID, _ := v.InitRepo(root)
	before, _ := v.getRepo(repoID)

	require.NoError(t, v.RecordEditMain(repoID, "orchestrator", filepath.Join(root, "a.go"), []byte("package main // v2")))
	cid, err := v.CommitMain(repoID, "orchestrator", "edit a.go")
	require.NoError(t, err)
	require.NotEmpty(t, cid)

	after, _ := v.getRepo(repoID)
	assert.Equal(t, cid, after.MainHead)
	assert.NotEqual(t, before.MainHead, after.MainHead)
	assert.Empty(t, v.Uncommitted("main", repoID)) // changeset cleared
	tree := v.commitTree(cid)
	assert.Equal(t, hashContent([]byte("package main // v2")), tree["a.go"])
}

func TestCommitMain_NewFileIncluded(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "a.go"), []byte("x"), 0o644))
	v := newTestVCS(t)
	repoID, _ := v.InitRepo(root)
	require.NoError(t, v.RecordEditMain(repoID, "o", filepath.Join(root, "b.go"), []byte("new")))
	cid, err := v.CommitMain(repoID, "o", "add b.go")
	require.NoError(t, err)
	tree := v.commitTree(cid)
	assert.Contains(t, tree, "a.go") // parent content preserved
	assert.Contains(t, tree, "b.go")
}

func TestCommitMain_NoChangesErrors(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "a.go"), []byte("x"), 0o644))
	v := newTestVCS(t)
	repoID, _ := v.InitRepo(root)
	_, err := v.CommitMain(repoID, "o", "nothing")
	assert.ErrorIs(t, err, ErrNoChanges)
}

func TestCommitWorktree_BranchCommit(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "a.go"), []byte("x"), 0o644))
	v := newTestVCS(t)
	repoID, _ := v.InitRepo(root)
	repo, _ := v.getRepo(repoID)
	wtID := "wt-c"
	_, _ = v.store.DB.Exec(
		"INSERT INTO vcs_worktrees (id, repo_id, path, base_commit, created_at, active) VALUES (?, ?, ?, ?, ?, 1)",
		wtID, repoID, root, repo.MainHead, time.Now().Unix())
	require.NoError(t, v.RecordEditWorktree(wtID, "w1", filepath.Join(root, "a.go"), []byte("edited")))
	cid, err := v.CommitWorktree(wtID, "w1", "wt edit")
	require.NoError(t, err)
	assert.Equal(t, cid, v.worktreeTip(wtID)) // tip advanced to the new commit
	assert.Empty(t, v.Uncommitted("worktree", wtID))
	// main untouched
	assert.Equal(t, repo.MainHead, v.getRepoMust(repoID).MainHead)
}

// TestCommitWorktree_TipTracksNewest proves the tip column (not a created_at/id
// tie-break) identifies the newest worktree commit. Two CommitWorktree calls in
// the same second (no sleep) must leave worktreeTip pointing at the SECOND
// commit: created_at is second-granularity and the commit id is a content hash
// (not recency), so a previous ORDER BY created_at DESC, id DESC query could
// pick the older commit and orphan the newer one. The tip column is advanced
// inside commitScope's transaction, so it is always the newest commit and the
// update is atomic with the commit row (a crash cannot leave a stale tip).
func TestCommitWorktree_TipTracksNewest(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "a.go"), []byte("x"), 0o644))
	v := newTestVCS(t)
	repoID, _ := v.InitRepo(root)
	repo, _ := v.getRepo(repoID)
	wtID := "wt-tip"
	_, _ = v.store.DB.Exec(
		"INSERT INTO vcs_worktrees (id, repo_id, path, base_commit, created_at, active) VALUES (?, ?, ?, ?, ?, 1)",
		wtID, repoID, root, repo.MainHead, time.Now().Unix())

	// Two commits in the same second (no sleep between them).
	require.NoError(t, v.RecordEditWorktree(wtID, "w1", filepath.Join(root, "a.go"), []byte("edit1")))
	cid1, err := v.CommitWorktree(wtID, "w1", "first")
	require.NoError(t, err)
	// After the first commit the tip is immediately visible (tip UPDATE landed
	// in the same tx as the commit row — atomicity holds).
	assert.Equal(t, cid1, v.worktreeTip(wtID), "tip must advance after the first commit")

	require.NoError(t, v.RecordEditWorktree(wtID, "w1", filepath.Join(root, "a.go"), []byte("edit2")))
	cid2, err := v.CommitWorktree(wtID, "w1", "second")
	require.NoError(t, err)

	require.NotEqual(t, cid1, cid2, "the two commits must have distinct ids")
	assert.Equal(t, cid2, v.worktreeTip(wtID), "tip must track the SECOND (newest) commit, not the first")

	// LogWorktree chains newest-first from the tip: second -> first -> init.
	log, err := v.LogWorktree(wtID, 10)
	require.NoError(t, err)
	require.Len(t, log, 3, "chain = init + 2 worktree commits")
	assert.Equal(t, cid2, log[0].ID, "newest commit first")
	assert.Equal(t, cid1, log[1].ID)
	assert.Equal(t, "vcs init", log[2].Message)

	// Sanity: confirm the tip column itself (not just worktreeTip's return) holds
	// the second commit — the worktree row was updated inside commitScope's tx.
	var tip string
	require.NoError(t, v.store.DB.QueryRow("SELECT tip FROM vcs_worktrees WHERE id=?", wtID).Scan(&tip))
	assert.Equal(t, cid2, tip, "the tip column must store the newest commit id")
}

func TestLogMain_ChainOrder(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "a.go"), []byte("x"), 0o644))
	v := newTestVCS(t)
	repoID, _ := v.InitRepo(root)
	require.NoError(t, v.RecordEditMain(repoID, "o", filepath.Join(root, "a.go"), []byte("v2")))
	_, _ = v.CommitMain(repoID, "alice", "second")
	require.NoError(t, v.RecordEditMain(repoID, "o", filepath.Join(root, "a.go"), []byte("v3")))
	_, _ = v.CommitMain(repoID, "bob", "third")

	log, err := v.LogMain(repoID, 10)
	require.NoError(t, err)
	require.Len(t, log, 3)
	assert.Equal(t, "third", log[0].Message) // newest first
	assert.Equal(t, "bob", log[0].Author)
	assert.Equal(t, "second", log[1].Message)
	assert.Equal(t, "vcs init", log[2].Message)
	// parent links form a chain
	assert.Equal(t, log[1].ID, log[0].ParentID)
	assert.Equal(t, log[2].ID, log[1].ParentID)
	// FilesChanged is the delta vs parent, not the total file count
	assert.Equal(t, 1, log[0].FilesChanged, "third commit changed exactly one file")
	assert.Equal(t, 1, log[1].FilesChanged, "second commit changed exactly one file")
	assert.Equal(t, 1, log[2].FilesChanged, "root commit = all files (a.go)")
}

func TestLogMain_Limit(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "a.go"), []byte("x"), 0o644))
	v := newTestVCS(t)
	repoID, _ := v.InitRepo(root)
	for i := 0; i < 3; i++ {
		require.NoError(t, v.RecordEditMain(repoID, "o", filepath.Join(root, "a.go"), []byte(fmt.Sprintf("v%d", i))))
		_, _ = v.CommitMain(repoID, "o", fmt.Sprintf("c%d", i))
	}
	log, _ := v.LogMain(repoID, 2)
	assert.Len(t, log, 2)
	assert.Equal(t, "c2", log[0].Message) // only the 2 newest
}

func TestDiff_AddedModifiedDeleted(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "a.go"), []byte("a1"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "b.go"), []byte("b1"), 0o644))
	v := newTestVCS(t)
	repoID, _ := v.InitRepo(root)
	initCommit := v.getRepoMust(repoID).MainHead

	// edit a.go, add c.go, delete b.go (record a delete for b)
	require.NoError(t, v.RecordEditMain(repoID, "o", filepath.Join(root, "a.go"), []byte("a2")))
	require.NoError(t, v.RecordEditMain(repoID, "o", filepath.Join(root, "c.go"), []byte("c1")))
	// simulate a delete of b.go by writing op='deleted' directly
	_, _ = v.store.DB.Exec("INSERT INTO vcs_uncommitted (scope_type, scope_id, path, blob_hash, op) VALUES ('main', ?, 'b.go', '', 'deleted')", repoID)
	c2, err := v.CommitMain(repoID, "o", "changes")
	require.NoError(t, err)

	diffs, err := v.Diff(repoID, initCommit, c2)
	require.NoError(t, err)
	byPath := map[string]FileDiff{}
	for _, d := range diffs {
		byPath[d.Path] = d
	}
	assert.Equal(t, "modified", byPath["a.go"].Op)
	assert.Equal(t, "added", byPath["c.go"].Op)
	assert.Equal(t, "deleted", byPath["b.go"].Op)
}

func TestDiff_NoChanges(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "a.go"), []byte("x"), 0o644))
	v := newTestVCS(t)
	repoID, _ := v.InitRepo(root)
	head := v.getRepoMust(repoID).MainHead
	diffs, err := v.Diff(repoID, head, head)
	require.NoError(t, err)
	assert.Empty(t, diffs)
}

func TestRestore_WritesBlobToDisk(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "a.go"), []byte("original"), 0o644))
	v := newTestVCS(t)
	repoID, _ := v.InitRepo(root)
	head := v.getRepoMust(repoID).MainHead

	// B2-RB1: Restore now requires destDir to be the repo root or an active
	// worktree (CB1: write + pending tracking in the same repo lane). Restoring
	// into the repo root records the restored blob as a pending main edit.
	require.NoError(t, v.Restore(head, "a.go", root))
	got, err := os.ReadFile(filepath.Join(root, "a.go"))
	require.NoError(t, err)
	assert.Equal(t, "original", string(got))
}

func TestRestore_MissingPathErrors(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "a.go"), []byte("x"), 0o644))
	v := newTestVCS(t)
	repoID, _ := v.InitRepo(root)
	head := v.getRepoMust(repoID).MainHead
	err := v.Restore(head, "nonexistent.go", root)
	assert.Error(t, err)
}

func TestAddWorktree_BranchesFromMain(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "a.go"), []byte("package main"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "src"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "src", "b.go"), []byte("b"), 0o644))
	v := newTestVCS(t) // worktreeDir = a temp dir
	repoID, _ := v.InitRepo(root)
	repo, _ := v.getRepo(repoID)

	wt, err := v.AddWorktree(repoID, []string{"worker-1"})
	require.NoError(t, err)
	assert.Equal(t, repo.MainHead, wt.BaseCommit) // branched from main_head
	assert.True(t, strings.HasPrefix(wt.Path, v.worktreeDir), "worktree lives under worktreeDir")

	// working dir contains the main tree (a.go + src/b.go), repo-relative layout
	data, err := os.ReadFile(filepath.Join(wt.Path, "a.go"))
	require.NoError(t, err)
	assert.Equal(t, "package main", string(data))
	data2, err := os.ReadFile(filepath.Join(wt.Path, "src", "b.go"))
	require.NoError(t, err)
	assert.Equal(t, "b", string(data2))

	// vcs_worktrees row recorded
	got, err := v.getWorktree(wt.ID)
	require.NoError(t, err)
	assert.Equal(t, repo.MainHead, got.BaseCommit)
	assert.Equal(t, repoID, got.RepoID)
}

func TestAddWorktree_IndependentOfMainEdits(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "a.go"), []byte("v1"), 0o644))
	v := newTestVCS(t)
	repoID, _ := v.InitRepo(root)
	wt, _ := v.AddWorktree(repoID, nil)

	// edit main after branching
	require.NoError(t, v.RecordEditMain(repoID, "o", filepath.Join(root, "a.go"), []byte("v2")))
	_, _ = v.CommitMain(repoID, "o", "main edit")

	// worktree's a.go is still v1 (branched snapshot)
	data, _ := os.ReadFile(filepath.Join(wt.Path, "a.go"))
	assert.Equal(t, "v1", string(data))
}

func TestRemoveWorktree_Deactivates(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "a.go"), []byte("x"), 0o644))
	v := newTestVCS(t)
	repoID, _ := v.InitRepo(root)
	wt, _ := v.AddWorktree(repoID, nil)
	// The materialized working dir exists under the configured worktreeDir.
	require.DirExists(t, wt.Path)
	require.NoError(t, v.RemoveWorktree(wt.ID))
	var active int
	require.NoError(t, v.store.DB.QueryRow("SELECT active FROM vcs_worktrees WHERE id=?", wt.ID).Scan(&active))
	assert.Equal(t, 0, active)
	// The working dir must be removed from disk to avoid leaks (it lives under
	// worktreeDir, so the guard permits the RemoveAll).
	assert.NoDirExists(t, wt.Path, "RemoveWorktree must delete the materialized working dir")
}

// TestRemoveWorktree_GuardDoesNotDeleteOutsideWorktreeDir proves the cleanup
// guard never removes a working dir that is NOT under the configured
// worktreeDir — a worktree whose path points elsewhere (e.g. a test that
// reuses the repo root) is left on disk; only the active flag is flipped.
func TestRemoveWorktree_GuardDoesNotDeleteOutsideWorktreeDir(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "a.go"), []byte("x"), 0o644))
	v := newTestVCS(t)
	repoID, _ := v.InitRepo(root)
	repo, _ := v.getRepo(repoID)
	// Insert a worktree whose path is the REPO root (outside worktreeDir) and a
	// sentinel file so we can prove the dir is NOT touched.
	wtID := "wt-outside"
	_, _ = v.store.DB.Exec(
		"INSERT INTO vcs_worktrees (id, repo_id, path, base_commit, created_at, active) VALUES (?, ?, ?, ?, ?, 1)",
		wtID, repoID, root, repo.MainHead, time.Now().Unix())
	sentinel := filepath.Join(root, "sentinel.txt")
	require.NoError(t, os.WriteFile(sentinel, []byte("keep"), 0o644))

	require.NoError(t, v.RemoveWorktree(wtID))
	assert.DirExists(t, root, "a guarded path must NOT be deleted")
	_, err := os.ReadFile(sentinel)
	require.NoError(t, err, "files under a guarded path must be untouched")
}

// addWorktreeWithEdit creates a worktree, records an edit to relpath, commits on the worktree.
// The edit targets the file under the worktree's working dir (wt.Path), which is
// the realistic agent flow — AddWorktree materializes main's tree there with
// repo-relative layout, and RecordEditWorktree resolves against wt.Path.
func addWorktreeWithEdit(t *testing.T, v *VCS, repoID, root, relpath, content, author, msg string) (Worktree, string) {
	t.Helper()
	wt, err := v.AddWorktree(repoID, nil)
	require.NoError(t, err)
	require.NoError(t, v.RecordEditWorktree(wt.ID, author, filepath.Join(wt.Path, relpath), []byte(content)))
	cid, err := v.CommitWorktree(wt.ID, author, msg)
	require.NoError(t, err)
	return wt, cid
}

// mustReadBlob reads a path's blob content out of commitID's tree or fatals.
func mustReadBlob(t *testing.T, v *VCS, commitID, path string) string {
	t.Helper()
	tree := v.commitTree(commitID)
	b, err := v.getBlob(tree[path])
	require.NoError(t, err)
	return string(b)
}

func TestMerge_FastForward(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "a.go"), []byte("a1"), 0o644))
	v := newTestVCS(t)
	repoID, _ := v.InitRepo(root)
	beforeMain := v.getRepoMust(repoID).MainHead

	wt, _ := addWorktreeWithEdit(t, v, repoID, root, "a.go", "a2", "w1", "wt edit a")
	cid, conflicts, err := v.MergeToMain(wt.ID, "merger", false)
	require.NoError(t, err)
	assert.Empty(t, conflicts)
	after := v.getRepoMust(repoID)
	assert.Equal(t, cid, after.MainHead) // main advanced
	assert.NotEqual(t, beforeMain, after.MainHead)
	// the merged tree has a2
	assert.Equal(t, hashContent([]byte("a2")), v.commitTree(cid)["a.go"])
	// merged_from recorded
	mc, _ := v.getCommit(cid)
	assert.Equal(t, wt.ID, mc.MergedFrom)
}

func TestMerge_ThreeWay_OneSide(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "a.go"), []byte("a1"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "b.go"), []byte("b1"), 0o644))
	v := newTestVCS(t)
	repoID, _ := v.InitRepo(root)

	wt, _ := addWorktreeWithEdit(t, v, repoID, root, "a.go", "a2", "w1", "wt edits a")
	// meanwhile, main edits b.go (different file → no conflict)
	require.NoError(t, v.RecordEditMain(repoID, "o", filepath.Join(root, "b.go"), []byte("b2")))
	_, _ = v.CommitMain(repoID, "o", "main edits b")

	cid, conflicts, err := v.MergeToMain(wt.ID, "merger", false)
	require.NoError(t, err)
	assert.Empty(t, conflicts)
	tree := v.commitTree(cid)
	assert.Equal(t, hashContent([]byte("a2")), tree["a.go"]) // from worktree (theirs)
	assert.Equal(t, hashContent([]byte("b2")), tree["b.go"]) // from main (ours)
}

func TestMerge_Conflict_RefusedThenForced(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "a.go"), []byte("a1"), 0o644))
	v := newTestVCS(t)
	repoID, _ := v.InitRepo(root)

	wt, _ := addWorktreeWithEdit(t, v, repoID, root, "a.go", "from-worktree", "w1", "wt edits a")
	// main ALSO edits a.go → both-side conflict
	require.NoError(t, v.RecordEditMain(repoID, "o", filepath.Join(root, "a.go"), []byte("from-main")))
	_, _ = v.CommitMain(repoID, "o", "main edits a")

	// without force → refused
	_, conflicts, err := v.MergeToMain(wt.ID, "merger", false)
	assert.ErrorIs(t, err, ErrConflicts)
	assert.Contains(t, conflicts, "a.go")
	// main unchanged
	assert.NotEqual(t, "from-worktree", string(mustReadBlob(t, v, v.getRepoMust(repoID).MainHead, "a.go")))

	// with force → theirs (worktree) wins
	cid, conflicts2, err := v.MergeToMain(wt.ID, "merger", true)
	require.NoError(t, err)
	assert.Contains(t, conflicts2, "a.go") // still reported, but merged
	assert.Equal(t, hashContent([]byte("from-worktree")), v.commitTree(cid)["a.go"])
}

// TestCommitMain_ClearsChangesetAtomically asserts the atomicity intent of
// CommitMain: on success BOTH side-effects are visible together — the scope's
// vcs_uncommitted changeset is empty AND main_head == the returned commit id.
// Neither alone is sufficient; a crash between them would leave an orphaned
// commit with a stale head or an uncleared changeset.
func TestCommitMain_ClearsChangesetAtomically(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "a.go"), []byte("x"), 0o644))
	v := newTestVCS(t)
	repoID, _ := v.InitRepo(root)

	require.NoError(t, v.RecordEditMain(repoID, "o", filepath.Join(root, "a.go"), []byte("v2")))
	cid, err := v.CommitMain(repoID, "o", "edit a.go")
	require.NoError(t, err)
	require.NotEmpty(t, cid)

	// Both-or-neither: the changeset is cleared AND main_head points at the commit.
	assert.Empty(t, v.Uncommitted("main", repoID), "changeset must be cleared on success")
	repo, err := v.getRepo(repoID)
	require.NoError(t, err)
	assert.Equal(t, cid, repo.MainHead, "main_head must equal the returned commit id")
}

// TestMerge_ConflictNoWrite asserts that a refused merge (ErrConflicts) creates
// NO new commit row — the total vcs_commits count is unchanged and no merge
// commit references the worktree (merged_from stays empty for this wtID).
func TestMerge_ConflictNoWrite(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "a.go"), []byte("a1"), 0o644))
	v := newTestVCS(t)
	repoID, _ := v.InitRepo(root)

	wt, _ := addWorktreeWithEdit(t, v, repoID, root, "a.go", "from-worktree", "w1", "wt edits a")
	// main ALSO edits a.go → both-side conflict
	require.NoError(t, v.RecordEditMain(repoID, "o", filepath.Join(root, "a.go"), []byte("from-main")))
	_, _ = v.CommitMain(repoID, "o", "main edits a")

	var before int
	require.NoError(t, v.store.DB.QueryRow("SELECT COUNT(*) FROM vcs_commits").Scan(&before))

	// refused merge → ErrConflicts, no commit created
	_, _, err := v.MergeToMain(wt.ID, "merger", false)
	assert.ErrorIs(t, err, ErrConflicts)

	var after int
	require.NoError(t, v.store.DB.QueryRow("SELECT COUNT(*) FROM vcs_commits").Scan(&after))
	assert.Equal(t, before, after, "no new commit row on refused merge")

	// and specifically: no merge commit referencing this worktree
	var mergeCommits int
	require.NoError(t, v.store.DB.QueryRow("SELECT COUNT(*) FROM vcs_commits WHERE merged_from=?", wt.ID).Scan(&mergeCommits))
	assert.Equal(t, 0, mergeCommits, "no merge commit for this worktree on refused merge")
}

// getRepoMust is a test helper that fatals if the repo row can't be loaded.
func (v *VCS) getRepoMust(id string) repoRow {
	r, err := v.getRepo(id)
	if err != nil {
		panic(err)
	}
	return r
}

// TestVCS_ConcurrentCommitTreeNoPanic pins the treeCache RWMutex fix. A VCS is
// shared across HTTP handler goroutines (orchestrator + broker), and net/http
// dispatches per-request goroutines, so two concurrent commitTree calls (e.g. a
// chat CommitMain racing a broker AddWorktree) hit the shared treeCache map.
// Without the RWMutex this is a fatal "concurrent map read and map write" panic
// (not caught by the sequential test suite). Here N goroutines call
// commitTree(head) simultaneously on a COLD cache (so they exercise the
// reconstruct + cache-fill write path, not just the RLock read fast path) and we
// assert no panic + every result equals the single correct tree.
func TestVCS_ConcurrentCommitTreeNoPanic(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "a.go"), []byte("x"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "b.go"), []byte("y"), 0o644))
	v := newTestVCS(t)
	repoID, err := v.InitRepo(root)
	require.NoError(t, err)

	// Populate a multi-commit history so commitTree does real chain-walk work on
	// a cold cache (not a trivial root-only tree).
	for i := 0; i < 5; i++ {
		require.NoError(t, v.RecordEditMain(repoID, "o", filepath.Join(root, "a.go"), []byte(fmt.Sprintf("v%d", i))))
		_, err := v.CommitMain(repoID, "o", fmt.Sprintf("edit %d", i))
		require.NoError(t, err)
	}
	head := v.getRepoMust(repoID).MainHead

	// Baseline computed now (this also warms the cache), then COLD the cache so
	// the goroutines below race through reconstruct + fill rather than all
	// hitting the read fast path.
	want := v.commitTree(head)
	v.treeCacheMu.Lock()
	v.treeCache = map[string]map[string]string{}
	v.treeCacheMu.Unlock()

	const n = 20
	var wg sync.WaitGroup
	results := make([]map[string]string, n)
	panics := make([]any, n)
	start := make(chan struct{})
	wg.Add(n)
	for i := 0; i < n; i++ {
		i := i
		go func() {
			defer wg.Done()
			defer func() { panics[i] = recover() }()
			<-start // barrier: all goroutines fire together to maximize contention
			results[i] = v.commitTree(head)
		}()
	}
	close(start)
	wg.Wait()

	for i := 0; i < n; i++ {
		require.Nil(t, panics[i], "goroutine %d panicked (concurrent map access not guarded)", i)
		assert.Equal(t, want, results[i], "goroutine %d returned a divergent tree", i)
	}
}

// TestCommitTree_DeleteThenReaddReconstructs covers the subtle delta-reconstruction
// path the review traced but no test exercised: a file deleted then re-added must
// resolve to the re-add's hash, not a stale value left over from before the delete.
// Delta storage records an 'add', then a 'mod', then a 'del', then another 'add'
// for path X; reconstruction walks root→head applying each. If the 'del' failed
// to remove X, or the final 'add' failed to re-insert it, the head tree would
// hold H1/H2 or be missing X entirely. The cache is colded before the final read
// so the reconstruction path (not a pre-warmed cache entry) is what's asserted.
func TestCommitTree_DeleteThenReaddReconstructs(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "X"), []byte("H1"), 0o644))
	v := newTestVCS(t)
	repoID, err := v.InitRepo(root)
	require.NoError(t, err)
	// root commit: X = hashContent("H1")

	// mod X = H2
	require.NoError(t, v.RecordEditMain(repoID, "o", filepath.Join(root, "X"), []byte("H2")))
	_, err = v.CommitMain(repoID, "o", "mod X=H2")
	require.NoError(t, err)

	// del X (op='deleted' is recorded by the caller, not by deriveOp)
	_, err = v.store.DB.Exec(
		"INSERT INTO vcs_uncommitted (scope_type, scope_id, path, blob_hash, op) VALUES ('main', ?, 'X', '', 'deleted')",
		repoID)
	require.NoError(t, err)
	_, err = v.CommitMain(repoID, "o", "del X")
	require.NoError(t, err)

	// add X = H3 (re-add after the delete)
	require.NoError(t, v.RecordEditMain(repoID, "o", filepath.Join(root, "X"), []byte("H3")))
	head, err := v.CommitMain(repoID, "o", "add X=H3")
	require.NoError(t, err)

	// Cold the cache so commitTree must reconstruct from the delta chain rather
	// than read back the tree writeCommitInTx pre-warmed.
	v.treeCacheMu.Lock()
	v.treeCache = map[string]map[string]string{}
	v.treeCacheMu.Unlock()

	h3 := hashContent([]byte("H3"))
	tree := v.commitTree(head)
	assert.Equal(t, h3, tree["X"], "re-add after delete must win (H3), not stale H1/H2")
	assert.Len(t, tree, 1, "only X should be present at head")
}
