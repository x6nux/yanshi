package vcs

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// blobExists reports whether a blob row is still present.
func blobExists(t *testing.T, v *VCS, hash string) bool {
	t.Helper()
	var n int
	if err := v.store.DB.QueryRow(
		"SELECT COUNT(*) FROM vcs_blobs WHERE hash=?", hash).Scan(&n); err != nil {
		t.Fatalf("count blob %s: %v", hash, err)
	}
	return n > 0
}

// commitExists reports whether a commit row is still present.
func commitExists(t *testing.T, v *VCS, id string) bool {
	t.Helper()
	var n int
	if err := v.store.DB.QueryRow(
		"SELECT COUNT(*) FROM vcs_commits WHERE id=?", id).Scan(&n); err != nil {
		t.Fatalf("count commit %s: %v", id, err)
	}
	return n > 0
}

// aggressiveGC keeps one commit by position and one day by age. On its own
// that protects nothing useful, because every commit a test just wrote is
// younger than a day — which is exactly why every safety test below calls
// backdateAll first.
//
// Without the backdating these tests would be theatre: retention would keep
// everything, the collector would delete nothing, and the assertions
// ("the seam's blob is still there") would pass no matter how wrong the
// reachability analysis was.
var aggressiveGC = GCOptions{KeepRecent: 1, KeepDays: 1}

// backdateAll ages every commit of the repo past any retention window, so
// reachability becomes the ONLY thing standing between the collector and the
// data.
//
// It edits created_at directly rather than injecting a clock: created_at is
// exactly what retainedByPolicy reads, the rewrite is one statement, and a
// fake clock would have to be threaded through production code that has no
// other reason to know about time.
func backdateAll(t *testing.T, v *VCS, repoID string) {
	t.Helper()
	// `+ rowid` rather than a flat value: SQLite's implicit rowid follows
	// insertion order here (id is a TEXT primary key, so it is not an alias for
	// rowid), which keeps the commits strictly ordered in time. A flat value
	// would make every created_at equal and hand the "newest KeepRecent" choice
	// to the id tie-break — an arbitrary commit, so a retention assertion could
	// not name which one survives.
	old := time.Now().Add(-365 * 24 * time.Hour).Unix()
	if _, err := v.store.DB.Exec(
		"UPDATE vcs_commits SET created_at=?+rowid WHERE repo_id=?", old, repoID); err != nil {
		t.Fatalf("backdate commits: %v", err)
	}
}

// isFrozenErr reports whether err is the V5 refusal.
func isFrozenErr(err error) bool { return errors.Is(err, ErrWorkingCopyFrozen) }

// TestGC_SeamOnlyBlobSurvivesAndRevertStillWorks is the attack test the feature
// exists to pass.
//
// It builds a blob whose ONLY surviving reference is an old seam's commit: the
// path is later overwritten and then deleted, so nothing in main's current tree
// mentions it, and retention is set to keep nothing. If reachability considered
// only main_head, the blob and its commit would both be collected — and the
// seam would still be listed in the UI, still be offered as a rollback target,
// and fail at the moment the operator pressed the button.
func TestGC_SeamOnlyBlobSurvivesAndRevertStillWorks(t *testing.T) {
	v, repoID, root := setupSeamTestRepo(t)
	// The head BEFORE the moment worth keeping. main is reset back to it below.
	beforeSeam := mustMainHead(t, v, repoID)
	const secret = "ONLY THIS SEAM REMEMBERS ME\n"
	commitWith(t, v, repoID, root, "the moment worth keeping", map[string]string{
		"important.txt": secret,
	})
	seamID, err := v.SealMainTurnSeam(repoID, "s1", 0, 0, SeamPreTurn, "keepable")
	if err != nil {
		t.Fatalf("SealMainTurnSeam: %v", err)
	}
	seam, err := v.FindSeam(seamID)
	if err != nil {
		t.Fatalf("FindSeam: %v", err)
	}
	secretHash := hashContent([]byte(secret))
	if !blobExists(t, v, secretHash) {
		t.Fatal("precondition: the seam's blob must exist before GC")
	}

	// Bury it. Burying by moving FORWARD is not enough — the seam's commit
	// would remain an ancestor of main_head, so main_head alone would keep it
	// reachable and the test would pass against a collector that reads no other
	// root. That is exactly the mutant this test has to catch, so main is moved
	// BACKWARDS instead: an earlier commit becomes the head, and everything
	// after it — including the seam's commit — leaves main's ancestry entirely.
	//
	// This is not a contrived shape. It is what every rollback produces: the
	// abandoned future is off-chain, and its seams are precisely the handles an
	// operator uses to change their mind again.
	commitWith(t, v, repoID, root, "overwrite", map[string]string{
		"important.txt": "replaced\n",
	})
	deleteAndCommit(t, v, repoID, root, "delete it", "important.txt")
	for i := 0; i < 5; i++ {
		commitWith(t, v, repoID, root, "noise", map[string]string{
			"noise.txt": string(rune('a'+i)) + "\n",
		})
	}
	if err := v.ResetMainHead(repoID, beforeSeam); err != nil {
		t.Fatalf("ResetMainHead: %v", err)
	}
	if seam.CommitID == beforeSeam {
		t.Fatal("precondition: the seam's commit must be OFF main's ancestry, " +
			"or main_head alone would keep it and this test could not fail")
	}

	backdateAll(t, v, repoID)
	res, err := v.RunGC(repoID, aggressiveGC)
	if err != nil {
		t.Fatalf("RunGC: %v", err)
	}

	if !blobExists(t, v, secretHash) {
		t.Fatalf("GC deleted a blob whose only reference is a seam "+
			"(deleted %d commits / %d blobs)", len(res.DeletedCommits), len(res.DeletedBlobs))
	}
	if !commitExists(t, v, seam.CommitID) {
		t.Fatalf("GC deleted the commit a seam points at: %s", seam.CommitID)
	}

	// The real acceptance criterion: the seam is not merely PRESENT, it still
	// works. A GC that kept the rows but broke the parent chain would pass the
	// two checks above and fail here.
	if _, err := v.RevertToSeam(repoID, seamID, "post-gc", 0, 0, nil); err != nil {
		t.Fatalf("revert to a seam that survived GC: %v", err)
	}
	mustFile(t, filepath.Join(root, "important.txt"), secret)
}

// TestGC_EverySeamStillRevertsAfterCollection widens the previous test from one
// seam to all of them: after an aggressive pass, every seam in the repo must
// still revert successfully and produce the tree it named.
func TestGC_EverySeamStillRevertsAfterCollection(t *testing.T) {
	v, repoID, root := setupSeamTestRepo(t)
	type checkpoint struct {
		seamID string
		want   map[string]string
	}
	var checkpoints []checkpoint
	for i := 0; i < 6; i++ {
		content := "generation " + string(rune('0'+i)) + "\n"
		commitWith(t, v, repoID, root, "gen", map[string]string{
			"rolling.txt": content,
			"per-gen.txt": "unique " + content,
		})
		seamID, err := v.SealMainTurnSeam(repoID, "s1", i, i, SeamPostTurn, "gen")
		if err != nil {
			t.Fatalf("seal %d: %v", i, err)
		}
		checkpoints = append(checkpoints, checkpoint{
			seamID: seamID,
			want:   map[string]string{"rolling.txt": content, "per-gen.txt": "unique " + content},
		})
	}
	backdateAll(t, v, repoID)
	if _, err := v.RunGC(repoID, aggressiveGC); err != nil {
		t.Fatalf("RunGC: %v", err)
	}
	// Walk the checkpoints backwards, which is how an operator actually uses
	// them, and make each one produce its own tree.
	for i := len(checkpoints) - 1; i >= 0; i-- {
		cp := checkpoints[i]
		if _, err := v.RevertToSeam(repoID, cp.seamID, "post-gc", 0, 0, nil); err != nil {
			t.Fatalf("checkpoint %d: revert after GC: %v", i, err)
		}
		for rel, want := range cp.want {
			mustFile(t, filepath.Join(root, rel), want)
		}
	}
}

// TestGC_WorktreeReferencesAreRoots pins the second and third root classes: a
// worktree's tip and its base commit are never collectible, and the rule holds
// for an INACTIVE worktree too — RemoveWorktree deletes the directory, not the
// branch's history.
func TestGC_WorktreeReferencesAreRoots(t *testing.T) {
	tests := []struct {
		name       string
		deactivate bool
	}{
		{name: "active worktree"},
		{name: "removed worktree keeps its history", deactivate: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v, repoID, root := setupSeamTestRepo(t)
			commitWith(t, v, repoID, root, "base", map[string]string{"base.txt": "b\n"})
			wt, err := v.AddWorktree(repoID, []string{"agent"})
			if err != nil {
				t.Fatalf("AddWorktree: %v", err)
			}
			const branchOnly = "ONLY ON THE BRANCH\n"
			branchFile := filepath.Join(wt.Path, "branch.txt")
			if err := os.WriteFile(branchFile, []byte(branchOnly), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := v.RecordEditWorktree(wt.ID, "agent", branchFile, []byte(branchOnly)); err != nil {
				t.Fatalf("RecordEditWorktree: %v", err)
			}
			branchCommit, err := v.CommitWorktree(wt.ID, "agent", "branch work")
			if err != nil {
				t.Fatalf("CommitWorktree: %v", err)
			}
			// Move main far ahead so nothing on the branch is near the retention
			// window.
			for i := 0; i < 5; i++ {
				commitWith(t, v, repoID, root, "main moves on",
					map[string]string{"main.txt": string(rune('a'+i)) + "\n"})
			}
			if tc.deactivate {
				if err := v.RemoveWorktree(wt.ID); err != nil {
					t.Fatalf("RemoveWorktree: %v", err)
				}
			}

			backdateAll(t, v, repoID)
			if _, err := v.RunGC(repoID, aggressiveGC); err != nil {
				t.Fatalf("RunGC: %v", err)
			}
			if !commitExists(t, v, branchCommit) {
				t.Error("GC deleted a worktree tip commit")
			}
			if !commitExists(t, v, wt.BaseCommit) {
				t.Error("GC deleted a worktree base commit")
			}
			if !blobExists(t, v, hashContent([]byte(branchOnly))) {
				t.Error("GC deleted a blob reachable only through a worktree branch")
			}
			log, err := v.LogWorktree(wt.ID, 10)
			if err != nil || len(log) == 0 {
				t.Errorf("worktree history unreadable after GC: %d commits, err=%v", len(log), err)
			}
		})
	}
}

// TestGC_PendingEditBlobsAreNeverCollected covers the uncommitted changeset:
// a blob recorded but not yet committed has no commit referencing it at all, so
// a collector that scanned only vcs_tree would delete the content of an edit
// the agent is about to commit.
func TestGC_PendingEditBlobsAreNeverCollected(t *testing.T) {
	v, repoID, root := setupSeamTestRepo(t)
	commitWith(t, v, repoID, root, "history", map[string]string{"f.txt": "committed\n"})
	const pending = "RECORDED BUT NOT YET COMMITTED\n"
	abs := filepath.Join(root, "pending.txt")
	if err := os.WriteFile(abs, []byte(pending), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := v.RecordEditMain(repoID, "test", abs, []byte(pending)); err != nil {
		t.Fatalf("RecordEditMain: %v", err)
	}
	backdateAll(t, v, repoID)
	if _, err := v.RunGC(repoID, aggressiveGC); err != nil {
		t.Fatalf("RunGC: %v", err)
	}
	if !blobExists(t, v, hashContent([]byte(pending))) {
		t.Fatal("GC deleted the blob of a pending, uncommitted edit")
	}
	if _, err := v.CommitMain(repoID, "test", "commit the pending edit"); err != nil {
		t.Fatalf("commit after GC: %v", err)
	}
	tree := v.commitTree(mustMainHead(t, v, repoID))
	if tree["pending.txt"] == "" {
		t.Error("the pending edit did not survive GC into the commit")
	}
}

// TestGC_DryRunChangesNothing pins the preview half: a dry run must report the
// same numbers a real pass would produce and delete nothing.
func TestGC_DryRunChangesNothing(t *testing.T) {
	v, repoID, root := setupSeamTestRepo(t)
	// Unreferenced history: reset the head back so the newer commits become
	// unreachable without any seam pointing at them.
	firstHead := mustMainHead(t, v, repoID)
	for i := 0; i < 4; i++ {
		commitWith(t, v, repoID, root, "doomed",
			map[string]string{"tmp.txt": string(rune('a'+i)) + "\n"})
	}
	if err := v.ResetMainHead(repoID, firstHead); err != nil {
		t.Fatalf("ResetMainHead: %v", err)
	}
	backdateAll(t, v, repoID)

	dry, err := v.RunGC(repoID, GCOptions{KeepRecent: 1, KeepDays: 1, DryRun: true})
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if !dry.DryRun {
		t.Error("result must report DryRun")
	}
	if len(dry.DeletedCommits) == 0 {
		t.Fatal("precondition: the dry run should have found unreachable commits")
	}
	for _, id := range dry.DeletedCommits {
		if !commitExists(t, v, id) {
			t.Fatalf("dry run deleted commit %s", id)
		}
	}
	for _, h := range dry.DeletedBlobs {
		if !blobExists(t, v, h) {
			t.Fatalf("dry run deleted blob %s", h)
		}
	}

	wet, err := v.RunGC(repoID, GCOptions{KeepRecent: 1, KeepDays: 1})
	if err != nil {
		t.Fatalf("real pass: %v", err)
	}
	if len(wet.DeletedCommits) != len(dry.DeletedCommits) ||
		len(wet.DeletedBlobs) != len(dry.DeletedBlobs) {
		t.Fatalf("dry run promised %d commits / %d blobs, real pass did %d / %d",
			len(dry.DeletedCommits), len(dry.DeletedBlobs),
			len(wet.DeletedCommits), len(wet.DeletedBlobs))
	}
	for _, id := range wet.DeletedCommits {
		if commitExists(t, v, id) {
			t.Errorf("commit %s survived a real pass that claimed to delete it", id)
		}
	}
	for _, h := range wet.DeletedBlobs {
		if blobExists(t, v, h) {
			t.Errorf("blob %s survived a real pass that claimed to delete it", h)
		}
	}
}

// TestGC_RetentionKeepsRecentAndYoungCommits pins the retention half: an
// unreachable commit is still kept when either retention rule covers it.
func TestGC_RetentionKeepsRecentAndYoungCommits(t *testing.T) {
	v, repoID, root := setupSeamTestRepo(t)
	firstHead := mustMainHead(t, v, repoID)
	var doomed []string
	for i := 0; i < 4; i++ {
		doomed = append(doomed, commitWith(t, v, repoID, root, "unreachable soon",
			map[string]string{"tmp.txt": string(rune('a'+i)) + "\n"}))
	}
	if err := v.ResetMainHead(repoID, firstHead); err != nil {
		t.Fatalf("ResetMainHead: %v", err)
	}
	backdateAll(t, v, repoID)

	tests := []struct {
		name     string
		opts     GCOptions
		wantKept bool
	}{
		{
			name:     "KeepDays covers the whole (backdated) history",
			opts:     GCOptions{KeepRecent: 1, KeepDays: 400, DryRun: true},
			wantKept: true,
		},
		{
			name:     "KeepRecent covers the whole history",
			opts:     GCOptions{KeepRecent: 100, KeepDays: 1, DryRun: true},
			wantKept: true,
		},
		{
			name: "neither rule covers them",
			opts: GCOptions{KeepRecent: 1, KeepDays: 1, DryRun: true},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res, err := v.RunGC(repoID, tc.opts)
			if err != nil {
				t.Fatalf("RunGC: %v", err)
			}
			deleted := map[string]bool{}
			for _, id := range res.DeletedCommits {
				deleted[id] = true
			}
			// The newest unreachable commit is excluded from the assertion: with
			// KeepRecent=1 it is precisely the one position-retention keeps, in
			// every case including the "neither rule" one. Asserting on it would
			// be asserting that KeepRecent does not work.
			for _, id := range doomed[:len(doomed)-1] {
				if deleted[id] == tc.wantKept {
					t.Errorf("commit %s: deleted=%v, wantKept=%v", id, deleted[id], tc.wantKept)
				}
			}
			if deleted[doomed[len(doomed)-1]] {
				t.Errorf("KeepRecent=1 must retain the newest commit %s",
					doomed[len(doomed)-1])
			}
		})
	}
}

// TestGC_VacuumReclaimsSpace proves the space actually comes back. Deleting
// rows alone returns pages to SQLite's freelist and leaves the file the same
// size, which is precisely the outcome V2 exists to avoid.
func TestGC_VacuumReclaimsSpace(t *testing.T) {
	v, repoID, root := setupSeamTestRepo(t)
	firstHead := mustMainHead(t, v, repoID)
	// Write enough distinct content that page reuse cannot hide the difference.
	big := make([]byte, 64*1024)
	for i := range big {
		big[i] = byte('a' + i%26)
	}
	for i := 0; i < 12; i++ {
		content := string(rune('A'+i)) + string(big)
		commitWith(t, v, repoID, root, "bulk", map[string]string{"bulk.txt": content})
	}
	if err := v.ResetMainHead(repoID, firstHead); err != nil {
		t.Fatalf("ResetMainHead: %v", err)
	}

	backdateAll(t, v, repoID)
	// setupSeamTestRepo puts the store next to the repo dir, not inside it.
	dbPath := filepath.Join(filepath.Dir(root), "test.db")
	sizeBefore := dbFootprint(t, dbPath)

	res, err := v.RunGC(repoID, GCOptions{KeepRecent: 1, KeepDays: 1, Vacuum: true})
	if err != nil {
		t.Fatalf("RunGC: %v", err)
	}
	if !res.Vacuumed {
		t.Fatal("Vacuum was requested but not reported")
	}
	if res.FreedBytes <= 0 {
		t.Fatalf("FreedBytes = %d, want > 0", res.FreedBytes)
	}
	sizeAfter := dbFootprint(t, dbPath)
	if sizeAfter >= sizeBefore {
		t.Errorf("database did not shrink: %d -> %d bytes (freed %d bytes of blobs)",
			sizeBefore, sizeAfter, res.FreedBytes)
	}
}

// TestGC_RefusesWhileWorkingCopyIsFrozen pins the V5 interaction: collecting a
// blob a half-finished rollback is about to write would turn a recoverable
// failure into an unrecoverable one.
func TestGC_RefusesWhileWorkingCopyIsFrozen(t *testing.T) {
	v, repoID, root := setupSeamTestRepo(t)
	commitWith(t, v, repoID, root, "history", map[string]string{"f.txt": "v1\n"})
	thaw, err := v.freezeWorkingCopy(repoID)
	if err != nil {
		t.Fatalf("freeze: %v", err)
	}
	defer thaw()
	if _, err := v.RunGC(repoID, aggressiveGC); !isFrozenErr(err) {
		t.Fatalf("RunGC while frozen: err = %v, want ErrWorkingCopyFrozen", err)
	}
}

// dbFootprint sums the main database file and its write-ahead log.
//
// The main file alone is the wrong measure: the store runs in WAL mode
// (internal/store applyConnectionPragmas), so freshly written pages live in
// the -wal sidecar until a checkpoint moves them. Before a VACUUM the main
// file can still be one page while megabytes sit in the WAL, which would make
// a real reclamation read as "no change at all".
func dbFootprint(t *testing.T, path string) int64 {
	t.Helper()
	var total int64
	for _, p := range []string{path, path + "-wal"} {
		info, err := os.Stat(p)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			t.Fatalf("stat %s: %v", p, err)
		}
		total += info.Size()
	}
	if total == 0 {
		t.Fatalf("no database file found at %s", path)
	}
	return total
}
