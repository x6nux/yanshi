package vcs

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestMaterializeMain_RestoresFileContents(t *testing.T) {
	v, repoID, root := setupSeamTestRepo(t)
	aPath := filepath.Join(root, "a.txt")
	// Advance: v0 -> v1 -> v2.
	for _, ver := range []string{"v1", "v2"} {
		if err := os.WriteFile(aPath, []byte(ver), 0o644); err != nil {
			t.Fatalf("WriteFile %s: %v", ver, err)
		}
		if err := v.RecordEditMain(repoID, "u", aPath, []byte(ver)); err != nil {
			t.Fatalf("RecordEditMain %s: %v", ver, err)
		}
		if _, err := v.CommitMain(repoID, "u", ver); err != nil {
			t.Fatalf("CommitMain %s: %v", ver, err)
		}
	}
	log, err := v.LogMain(repoID, 3)
	if err != nil {
		t.Fatalf("LogMain: %v", err)
	}
	if len(log) < 3 {
		t.Fatalf("expected >=3 commits, got %d", len(log))
	}
	v0ID := log[2].ID // newest-first: [v2, v1, v0]
	if err := v.MaterializeMain(repoID, v0ID); err != nil {
		t.Fatalf("MaterializeMain: %v", err)
	}
	got, err := os.ReadFile(aPath)
	if err != nil {
		t.Fatalf("read a.txt: %v", err)
	}
	if string(got) != "v0" {
		t.Errorf("after materialize v0: a.txt = %q, want %q", got, "v0")
	}
}

func TestMaterializeMain_DeletesFilesAbsentInTarget(t *testing.T) {
	v, repoID, root := setupSeamTestRepo(t)
	extra := filepath.Join(root, "extra.txt")
	if err := os.WriteFile(extra, []byte("present"), 0o644); err != nil {
		t.Fatalf("WriteFile extra: %v", err)
	}
	if err := v.RecordEditMain(repoID, "u", extra, []byte("present")); err != nil {
		t.Fatalf("RecordEditMain: %v", err)
	}
	if _, err := v.CommitMain(repoID, "u", "add extra"); err != nil {
		t.Fatalf("CommitMain: %v", err)
	}
	log, err := v.LogMain(repoID, 3)
	if err != nil {
		t.Fatalf("LogMain: %v", err)
	}
	priorID := log[1].ID // [with-extra, prior]
	if err := v.MaterializeMain(repoID, priorID); err != nil {
		t.Fatalf("MaterializeMain: %v", err)
	}
	if _, err := os.Stat(extra); !os.IsNotExist(err) {
		t.Errorf("extra.txt should be removed; stat err = %v", err)
	}
}

// TestMaterializeMain_RejectsWrongRepo — 必修项 G: commit 归属校验。
func TestMaterializeMain_RejectsWrongRepo(t *testing.T) {
	v, repoID, _ := setupSeamTestRepo(t)
	root2 := t.TempDir()
	repoID2, err := v.InitRepo(root2)
	if err != nil {
		t.Fatalf("InitRepo root2: %v", err)
	}
	p2 := filepath.Join(root2, "b.txt")
	_ = v.RecordEditMain(repoID2, "u", p2, []byte("B"))
	head2, err := v.CommitMain(repoID2, "u", "b")
	if err != nil {
		t.Fatalf("commit on repo2: %v", err)
	}
	err = v.MaterializeMain(repoID, head2)
	if err == nil {
		t.Fatal("MaterializeMain 应当拒绝 cross-repo commit")
	}
}

// TestMaterializeMain_RejectsWorktreeCommit — only main commits are valid
// rollback targets.
func TestMaterializeMain_RejectsWorktreeCommit(t *testing.T) {
	v, repoID, _ := setupSeamTestRepo(t)
	wt, err := v.AddWorktree(repoID, []string{"test"})
	if err != nil {
		t.Fatalf("AddWorktree: %v", err)
	}
	wtFile := filepath.Join(wt.Path, "wt.txt")
	if err := v.RecordEditWorktree(wt.ID, "u", wtFile, []byte("WT")); err != nil {
		t.Fatalf("RecordEditWorktree: %v", err)
	}
	wtHead, err := v.CommitWorktree(wt.ID, "u", "wt commit")
	if err != nil {
		t.Fatalf("CommitWorktree: %v", err)
	}
	err = v.MaterializeMain(repoID, wtHead)
	if err == nil {
		t.Fatal("MaterializeMain 应当拒绝 worktree commit")
	}
}

// TestMaterializeMain_FailFastOnMissingBlob injects a vcs_tree row whose blob
// is missing, asserts MaterializeMain errors WITHOUT touching the working copy.
func TestMaterializeMain_FailFastOnMissingBlob(t *testing.T) {
	v, repoID, root := setupSeamTestRepo(t)
	aPath := filepath.Join(root, "a.txt")
	_ = v.RecordEditMain(repoID, "u", aPath, []byte("v1"))
	head, _ := v.CommitMain(repoID, "u", "v1")
	tree := v.commitTree(head)
	for _, h := range tree {
		if _, err := v.store.DB.Exec("DELETE FROM vcs_blobs WHERE hash = ?", h); err != nil {
			t.Fatalf("delete blob: %v", err)
		}
	}
	before, _ := os.ReadFile(aPath)
	err := v.MaterializeMain(repoID, head)
	if err == nil {
		t.Fatal("MaterializeMain 应当对 missing blob 报错")
	}
	after, _ := os.ReadFile(aPath)
	if string(before) != string(after) {
		t.Errorf("working copy 被改动: before=%q after=%q", before, after)
	}
}

func TestMaterializeMain_RollsBackAllTouchedFilesOnInjectedFailure(t *testing.T) {
	v, repoID, root := setupSeamTestRepo(t)
	targetHead := mustMainHead(t, v, repoID) // seed tree: a.txt=v0, no extra.txt
	aPath := filepath.Join(root, "a.txt")
	extraPath := filepath.Join(root, "extra.txt")

	// Build and materialize the pre-operation state: a.txt=v2 + extra.txt=keep.
	if err := v.RecordEditMain(repoID, "u", aPath, []byte("v2")); err != nil {
		t.Fatalf("record a: %v", err)
	}
	if err := v.RecordEditMain(repoID, "u", extraPath, []byte("keep")); err != nil {
		t.Fatalf("record extra: %v", err)
	}
	currentHead, err := v.CommitMain(repoID, "u", "current")
	if err != nil {
		t.Fatalf("commit current: %v", err)
	}
	if err := os.WriteFile(aPath, []byte("v2"), 0o644); err != nil {
		t.Fatalf("write a: %v", err)
	}
	if err := os.WriteFile(extraPath, []byte("keep"), 0o644); err != nil {
		t.Fatalf("write extra: %v", err)
	}

	calls := 0
	injected := errors.New("injected second mutation failure")
	err = v.materializeMainLockedWithHook(repoID, targetHead,
		func(stage, path string) error {
			calls++
			if calls == 2 {
				return injected
			}
			return nil
		})
	if !errors.Is(err, injected) {
		t.Fatalf("MaterializeMain error = %v, want injected", err)
	}
	if got, readErr := os.ReadFile(aPath); readErr != nil || string(got) != "v2" {
		t.Fatalf("a.txt after compensation = %q, err=%v; want v2", got, readErr)
	}
	if got, readErr := os.ReadFile(extraPath); readErr != nil || string(got) != "keep" {
		t.Fatalf("extra.txt after compensation = %q, err=%v; want keep", got, readErr)
	}
	if got := mustMainHead(t, v, repoID); got != currentHead {
		t.Fatalf("head advanced on failed materialize: got %s want %s", got, currentHead)
	}
}

func TestReplaceFile_ReplacesExistingDestination(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "existing.txt")
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	root, err := openWorkRoot(dir)
	if err != nil {
		t.Fatalf("openWorkRoot: %v", err)
	}
	defer root.Close()
	if err := rootReplaceFile(root, "existing.txt", []byte("new"), 0o644); err != nil {
		t.Fatalf("rootReplaceFile: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != "new" {
		t.Fatalf("got %q, err=%v; want new", got, err)
	}
}

func TestResetMainHead_UpdatesRepo(t *testing.T) {
	v, repoID, _ := setupSeamTestRepo(t)
	log, _ := v.LogMain(repoID, 5)
	if len(log) < 1 {
		t.Fatal("need >=1 commit")
	}
	newHead := log[0].ParentID
	if newHead == "" {
		t.Skip("root commit has no parent")
	}
	if err := v.ResetMainHead(repoID, newHead); err != nil {
		t.Fatalf("ResetMainHead: %v", err)
	}
	got, _ := v.RepoMainHead(repoID)
	if got != newHead {
		t.Errorf("after reset: RepoMainHead = %s, want %s", got, newHead)
	}
}

func TestValidateRelPath_DotGitRejected(t *testing.T) {
	bad := []string{".git/HEAD", "foo/.git/config", "../escape", "/abs/path", "sub/../.."}
	for _, p := range bad {
		if err := validateRelPath(p); err == nil {
			t.Errorf("validateRelPath(%q) 应当报错", p)
		}
	}
	good := []string{"a.txt", "sub/dir/f.txt", "foo/bar.go"}
	for _, p := range good {
		if err := validateRelPath(p); err != nil {
			t.Errorf("validateRelPath(%q) 不应报错: %v", p, err)
		}
	}
}

// TestValidateRelPath_EmptyAndDot covers the rel=="" || rel=="." branch at
// revert.go:34-35 that the dot-git suite doesn't exercise.
func TestValidateRelPath_EmptyAndDot(t *testing.T) {
	for _, p := range []string{"", "."} {
		if err := validateRelPath(p); err == nil {
			t.Errorf("validateRelPath(%q) 应当报错", p)
		}
	}
}

// TestRevertToSeam_RevertsFilesAndHead verifies the basic revert: after two
// turns, reverting to the pre-turn-1 seam puts a.txt back to v0 AND advances
// main_head to the v0 commit.
func TestRevertToSeam_RevertsFilesAndHead(t *testing.T) {
	v, repoID, root := setupSeamTestRepo(t)
	aPath := filepath.Join(root, "a.txt")

	// Seal a pre-turn-1 seam BEFORE advancing. The seam captures the v0 head.
	pre1ID, err := v.SealMainTurnSeam(repoID, "s1", 0, 0, SeamPreTurn, "pre-turn:1")
	if err != nil {
		t.Fatalf("SealMainTurnSeam pre1: %v", err)
	}
	pre1, _ := v.FindSeam(pre1ID)
	v0Commit := pre1.CommitID

	// Two turns advance to v2.
	for _, ver := range []string{"v1", "v2"} {
		if err := os.WriteFile(aPath, []byte(ver), 0o644); err != nil {
			t.Fatalf("WriteFile %s: %v", ver, err)
		}
		if err := v.RecordEditMain(repoID, "u", aPath, []byte(ver)); err != nil {
			t.Fatalf("RecordEditMain %s: %v", ver, err)
		}
		if _, err := v.CommitMain(repoID, "u", ver); err != nil {
			t.Fatalf("CommitMain %s: %v", ver, err)
		}
	}
	headV2 := mustMainHead(t, v, repoID)

	// Revert to pre1.
	undoID, err := v.RevertToSeam(repoID, pre1ID, "revert to pre1", 0, 0, nil)
	if err != nil {
		t.Fatalf("RevertToSeam: %v", err)
	}
	if undoID == "" {
		t.Fatal("undo seam id 空串")
	}
	got, _ := os.ReadFile(aPath)
	if string(got) != "v0" {
		t.Errorf("after revert: a.txt = %q, want %q", got, "v0")
	}
	headAfter := mustMainHead(t, v, repoID)
	if headAfter != v0Commit {
		t.Errorf("after revert: head = %s, want %s(v0Commit)", headAfter, v0Commit)
	}

	// The undo seam MUST point at the pre-revert head (v2) — that is what makes
	// the revert reversible. Reverting the undo seam puts a.txt back to v2.
	undoSeam, _ := v.FindSeam(undoID)
	if undoSeam.CommitID != headV2 {
		t.Errorf("undo seam.CommitID = %s, want %s(previousHead v2)", undoSeam.CommitID, headV2)
	}
	if undoSeam.Kind != SeamPreRevert {
		t.Errorf("undo seam.Kind = %q, want %q", undoSeam.Kind, SeamPreRevert)
	}
}

// TestRevertToSeam_RoundTripIsReversible — 必修项 A 核心证据。RevertToSeam(r1)
// must restore the pre-revert state. The test fails if RevertToSeam returns a
// post-revert seam (pointing at the target) instead of an undo seam (pointing
// at previousHead): r1's commit would be the target, so reverting r1 would be
// a no-op, leaving a.txt at v0 instead of going back to v2.
func TestRevertToSeam_RoundTripIsReversible(t *testing.T) {
	v, repoID, root := setupSeamTestRepo(t)
	aPath := filepath.Join(root, "a.txt")
	pre1ID, _ := v.SealMainTurnSeam(repoID, "s1", 0, 0, SeamPreTurn, "pre-turn:1")
	for _, ver := range []string{"v1", "v2"} {
		if err := os.WriteFile(aPath, []byte(ver), 0o644); err != nil {
			t.Fatalf("WriteFile %s: %v", ver, err)
		}
		if err := v.RecordEditMain(repoID, "u", aPath, []byte(ver)); err != nil {
			t.Fatalf("RecordEditMain %s: %v", ver, err)
		}
		if _, err := v.CommitMain(repoID, "u", ver); err != nil {
			t.Fatalf("CommitMain %s: %v", ver, err)
		}
	}

	// First revert: v2 -> v0.
	r1, err := v.RevertToSeam(repoID, pre1ID, "first revert", 0, 0, nil)
	if err != nil {
		t.Fatalf("first RevertToSeam: %v", err)
	}
	if got, _ := os.ReadFile(aPath); string(got) != "v0" {
		t.Fatalf("after first revert: a.txt = %q, want v0", got)
	}

	// Undo the revert by reverting r1 (the undo seam pointing at v2 head).
	_, err = v.RevertToSeam(repoID, r1, "undo first revert", 0, 0, nil)
	if err != nil {
		t.Fatalf("undo RevertToSeam(r1): %v", err)
	}
	got, _ := os.ReadFile(aPath)
	if string(got) != "v2" {
		t.Errorf("after RevertToSeam(r1): a.txt = %q, want %q(undo)", got, "v2")
	}
}

// TestRevertToSeam_RejectsWrongRepo — 必修项 G.
func TestRevertToSeam_RejectsWrongRepo(t *testing.T) {
	v, repoID, _ := setupSeamTestRepo(t)
	preID, _ := v.SealMainTurnSeam(repoID, "s1", 0, 0, SeamPreTurn, "pre")
	root2 := t.TempDir()
	repoID2, _ := v.InitRepo(root2)
	_, err := v.RevertToSeam(repoID2, preID, "cross-repo", 0, 0, nil)
	if err == nil {
		t.Fatal("RevertToSeam 应当拒绝 cross-repo seam")
	}
}

// TestRevertToSeam_FailFastOnMissingBlob — 必修项 G: materialize failure
// MUST NOT advance main_head.
func TestRevertToSeam_FailFastOnMissingBlob(t *testing.T) {
	v, repoID, root := setupSeamTestRepo(t)
	aPath := filepath.Join(root, "a.txt")
	preID, _ := v.SealMainTurnSeam(repoID, "s1", 0, 0, SeamPreTurn, "pre")
	_ = v.RecordEditMain(repoID, "u", aPath, []byte("v1"))
	_, _ = v.CommitMain(repoID, "u", "v1")
	pre, _ := v.FindSeam(preID)
	// Corrupt: delete blobs referenced by pre.CommitID.
	for _, h := range v.commitTree(pre.CommitID) {
		_, _ = v.store.DB.Exec("DELETE FROM vcs_blobs WHERE hash = ?", h)
	}
	headBefore := mustMainHead(t, v, repoID)
	_, err := v.RevertToSeam(repoID, preID, "should fail", 0, 0, nil)
	if err == nil {
		t.Fatal("RevertToSeam 应当对 missing blob 报错")
	}
	headAfter := mustMainHead(t, v, repoID)
	if headAfter != headBefore {
		t.Errorf("失败后 head 被切换: before=%s after=%s", headBefore, headAfter)
	}
}

// TestRevertToSeam_AtomicallyInsertsUndoAndAuditSeam — 必修项 G: the undo +
// audit seams + head reset MUST land in one tx.
func TestRevertToSeam_AtomicallyInsertsUndoAndAuditSeam(t *testing.T) {
	v, repoID, _ := setupSeamTestRepo(t)
	preID, _ := v.SealMainTurnSeam(repoID, "s1", 0, 0, SeamPreTurn, "pre")
	undoID, err := v.RevertToSeam(repoID, preID, "audit check", 0, 0, nil)
	if err != nil {
		t.Fatalf("RevertToSeam: %v", err)
	}
	seams, _ := v.ListSeams(repoID, "", 0) // VCS-only call stores undo/audit outside WS sessions.
	// Expected: preID, undoID, plus one SeamPostRevert audit seam.
	kinds := map[string]int{}
	for _, s := range seams {
		kinds[string(s.Kind)]++
		if s.ID == undoID && s.Kind != SeamPreRevert {
			t.Errorf("undo seam kind = %q, want %q", s.Kind, SeamPreRevert)
		}
	}
	if kinds["pre-revert"] == 0 {
		t.Errorf("未找到 pre-revert(undo)seam;kinds=%v", kinds)
	}
	if kinds["post-revert"] == 0 {
		t.Errorf("未找到 post-revert(audit)seam;kinds=%v", kinds)
	}
}

// TestRevertToSeam_TxFailureRestoresPreOperationFiles proves that a DB failure
// after successful materialization does not leave disk at target with old head.
func TestRevertToSeam_TxFailureRestoresPreOperationFiles(t *testing.T) {
	v, repoID, root := setupSeamTestRepo(t)
	aPath := filepath.Join(root, "a.txt")
	preID, err := v.SealMainTurnSeam(repoID, "s1", 0, 0,
		SeamPreTurn, "pre-turn:1")
	if err != nil {
		t.Fatalf("seal target: %v", err)
	}
	if err := v.RecordEditMain(repoID, "u", aPath, []byte("v1")); err != nil {
		t.Fatalf("record v1: %v", err)
	}
	if err := os.WriteFile(aPath, []byte("v1"), 0o644); err != nil {
		t.Fatalf("write v1: %v", err)
	}
	currentHead, err := v.CommitMain(repoID, "u", "v1")
	if err != nil {
		t.Fatalf("commit v1: %v", err)
	}
	if _, err := v.store.DB.Exec(`
		CREATE TRIGGER fail_pre_revert_insert
		BEFORE INSERT ON vcs_seams
		WHEN NEW.kind = 'pre-revert'
		BEGIN SELECT RAISE(ABORT, 'injected revert tx failure'); END;
	`); err != nil {
		t.Fatalf("create trigger: %v", err)
	}

	if _, err := v.RevertToSeam(repoID, preID, "inject", 0, 0, nil); err == nil {
		t.Fatal("RevertToSeam should fail when undo seam INSERT is aborted")
	}
	got, readErr := os.ReadFile(aPath)
	if readErr != nil || string(got) != "v1" {
		t.Fatalf("disk after tx failure = %q, err=%v; want pre-op v1", got, readErr)
	}
	if gotHead := mustMainHead(t, v, repoID); gotHead != currentHead {
		t.Fatalf("head after tx failure = %s, want %s", gotHead, currentHead)
	}
}
