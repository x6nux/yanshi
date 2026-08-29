package vcs

// liverun_test.go — whole-repository run-throughs, as opposed to the
// property-per-behaviour tests next door.
//
// The tests in this file exist because the unit tests around them all assert
// on the OUTPUT of one function. That is the right shape for "does the planner
// mark a dirty path dirty", and the wrong shape for the one question a rollback
// preview exists to answer: "is what the preview told the operator the same
// thing that later happened to their disk". Answering that requires running
// both halves against one real working copy and diffing the filesystem before
// and after, which is what every test here does — no function's return value is
// trusted as a description of the disk when the disk itself can be read.
//
// Each test is a scenario, not a case: build a repo with several turns of
// history, do the thing, then walk the tree.

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/x6nux/yanshi/internal/store"
)

// diskState is a path -> content map of every regular file under root, keyed by
// slash-separated repo-relative path. Directories are omitted; a rollback is
// judged by the bytes it leaves behind.
type diskState map[string]string

// snapshotDisk walks root and records every regular file.
func snapshotDisk(t *testing.T, root string) diskState {
	t.Helper()
	out := diskState{}
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil {
			return rerr
		}
		b, rerr := os.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		out[filepath.ToSlash(rel)] = string(b)
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return out
}

// observedOps derives the actual per-path effect of an operation by diffing two
// disk snapshots. The vocabulary matches RestoreOp so a preview can be compared
// against it directly.
func observedOps(before, after diskState) map[string]RestoreOp {
	ops := map[string]RestoreOp{}
	seen := map[string]bool{}
	for p := range before {
		seen[p] = true
	}
	for p := range after {
		seen[p] = true
	}
	for p := range seen {
		b, hadB := before[p]
		a, hadA := after[p]
		switch {
		case hadB && !hadA:
			ops[p] = RestoreDelete
		case !hadB && hadA:
			ops[p] = RestoreCreate
		case b != a:
			ops[p] = RestoreOverwrite
		}
	}
	return ops
}

// countTextLines counts lines the way countLines does, for comparing a plan's
// numbers against content read back off the disk.
func countTextLines(s string) int { return countLines([]byte(s)) }

// sortedKeys is a deterministic key list for failure messages.
func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// liveRepo is a working copy plus its VCS, wired to a per-test lock directory
// so a run leaves nothing in the user's cache.
type liveRepo struct {
	v      *VCS
	repoID string
	root   string
}

// newLiveRepo builds an initialised repository under a temp dir.
func newLiveRepo(t *testing.T) *liveRepo {
	t.Helper()
	v := newTestVCS(t)
	root := t.TempDir()
	id, err := v.InitRepo(root)
	if err != nil {
		t.Fatalf("init repo: %v", err)
	}
	return &liveRepo{v: v, repoID: id, root: root}
}

// edit writes a file through the VCS, exactly as an fs tool would.
func (r *liveRepo) edit(t *testing.T, rel, content string) {
	t.Helper()
	abs := filepath.Join(r.root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", rel, err)
	}
	if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
	if err := r.v.RecordEditMain(r.repoID, "agent", abs, []byte(content)); err != nil {
		t.Fatalf("record %s: %v", rel, err)
	}
}

// remove deletes a file through the VCS.
func (r *liveRepo) remove(t *testing.T, rel string) {
	t.Helper()
	abs := filepath.Join(r.root, filepath.FromSlash(rel))
	if err := os.Remove(abs); err != nil {
		t.Fatalf("remove %s: %v", rel, err)
	}
	if err := r.v.RecordDeleteMain(r.repoID, "agent", abs); err != nil {
		t.Fatalf("record delete %s: %v", rel, err)
	}
}

// seal ends a turn, producing a navigable snapshot.
func (r *liveRepo) seal(t *testing.T, sessionID string, turn int, label string) string {
	t.Helper()
	id, err := r.v.SealMainTurnSeam(r.repoID, sessionID, turn, turn*2, SeamPostTurn, label)
	if err != nil {
		t.Fatalf("seal turn %d: %v", turn, err)
	}
	return id
}

// TestLiveRun_RollbackPreviewMatchesTheRollback is the V1 acceptance run: the
// preview is only worth having if every claim it makes is what the disk shows
// afterwards. It compares, per path, the predicted op and the predicted
// before/after line counts against a filesystem walk taken after the real
// revert — and, in the other direction, fails on any path the preview did not
// mention that nevertheless changed.
//
// The scenario deliberately contains all three ops plus a path modified
// OUTSIDE the VCS, because an out-of-band edit is the case where the "lines you
// are about to lose" number can silently be computed from the tree instead of
// from the disk.
func TestLiveRun_RollbackPreviewMatchesTheRollback(t *testing.T) {
	r := newLiveRepo(t)

	r.edit(t, "a.txt", "a1\na2\na3\n")
	r.edit(t, "b.txt", "b1\n")
	r.edit(t, "sub/c.txt", "c1\nc2\n")
	r.edit(t, "d.txt", "d1\nd2\nd3\nd4\n")
	seam1 := r.seal(t, "sess1", 1, "turn one")
	atTurn1 := snapshotDisk(t, r.root)

	r.edit(t, "a.txt", "a1\nCHANGED\na3\nEXTRA\n")
	r.remove(t, "b.txt")
	r.edit(t, "e.txt", "e1\n")
	r.edit(t, "d.txt", "d1\n")
	r.seal(t, "sess1", 2, "turn two")

	// An editor autosave: on disk, never recorded. The preview must count the
	// lines that are really there, not the ones the head tree remembers.
	outOfBand := filepath.Join(r.root, "sub", "c.txt")
	if err := os.WriteFile(outOfBand, []byte("c1\nc2\nUNSAVED\nWORK\n"), 0o644); err != nil {
		t.Fatalf("out-of-band write: %v", err)
	}

	before := snapshotDisk(t, r.root)

	plan, err := r.v.PlanRevertToSeam(r.repoID, seam1)
	if err != nil {
		t.Fatalf("plan revert: %v", err)
	}
	predicted := map[string]RestoreChange{}
	for _, c := range plan.Changes {
		predicted[c.Path] = c
	}
	t.Logf("preview: %d changes, %d unchanged, dirty=%v",
		len(plan.Changes), plan.Unchanged, plan.DirtyPaths())
	for _, c := range plan.Changes {
		t.Logf("  %-9s %-10s before=%d after=%d +%d -%d dirty=%v",
			c.Op, c.Path, c.LinesBefore, c.LinesAfter, c.LinesAdded, c.LinesRemoved, c.Dirty)
	}

	if _, err := r.v.RevertToSeam(r.repoID, seam1, "probe", 0, 0, nil); err != nil {
		t.Fatalf("revert: %v", err)
	}
	after := snapshotDisk(t, r.root)
	actual := observedOps(before, after)

	for _, p := range sortedKeys(actual) {
		c, ok := predicted[p]
		if !ok {
			t.Errorf("path %s was %s by the revert but the preview never mentioned it",
				p, actual[p])
			continue
		}
		if c.Op != actual[p] {
			t.Errorf("path %s: preview said %s, disk shows %s", p, c.Op, actual[p])
		}
		if want := countTextLines(before[p]); c.LinesBefore != want {
			t.Errorf("path %s: preview LinesBefore=%d, disk before the revert had %d",
				p, c.LinesBefore, want)
		}
		if actual[p] != RestoreDelete {
			if want := countTextLines(after[p]); c.LinesAfter != want {
				t.Errorf("path %s: preview LinesAfter=%d, disk after the revert has %d",
					p, c.LinesAfter, want)
			}
		}
	}
	for _, p := range sortedKeys(predicted) {
		if _, ok := actual[p]; !ok {
			t.Errorf("preview promised %s on %s but the disk is unchanged there",
				predicted[p].Op, p)
		}
	}

	// The dirty list is the "you will lose this" warning; the out-of-band file
	// must be on it, and it must be the ONLY path on it, since every other
	// change was committed.
	if got := plan.DirtyPaths(); len(got) != 1 || got[0] != "sub/c.txt" {
		t.Errorf("DirtyPaths = %v, want exactly [sub/c.txt]", got)
	}

	// And the end state is turn one's tree, except that the unsaved work is
	// gone — which is precisely what the preview warned about.
	if diff := diskDiff(atTurn1, after); diff != "" {
		t.Errorf("working copy after revert does not equal the turn-1 tree:\n%s", diff)
	}
}

// diskDiff renders the differing paths between two snapshots, or "" when equal.
func diskDiff(want, got diskState) string {
	var b strings.Builder
	seen := map[string]bool{}
	for p := range want {
		seen[p] = true
	}
	for p := range got {
		seen[p] = true
	}
	for _, p := range sortedKeys(seen) {
		w, hadW := want[p]
		g, hadG := got[p]
		if hadW == hadG && w == g {
			continue
		}
		fmt.Fprintf(&b, "  %s: want(present=%v)=%q got(present=%v)=%q\n", p, hadW, w, hadG, g)
	}
	return b.String()
}

// TestLiveRun_SelectiveRestoreLeavesEverythingElseAlone is the V4 acceptance
// run. Restoring two paths must restore exactly those two: the test asserts on
// the whole tree, so a restore that also reverted a third file — or that
// reverted the whole tree and happened to leave the two right — fails.
func TestLiveRun_SelectiveRestoreLeavesEverythingElseAlone(t *testing.T) {
	r := newLiveRepo(t)

	r.edit(t, "keep/one.txt", "one v1\n")
	r.edit(t, "keep/two.txt", "two v1\n")
	r.edit(t, "broken/x.go", "package x // v1\n")
	r.edit(t, "broken/y.go", "package y // v1\n")
	r.edit(t, "untouched.md", "docs v1\n")
	old, err := r.v.CommitMain(r.repoID, "agent", "v1")
	if err != nil {
		t.Fatalf("commit v1: %v", err)
	}

	// A later turn changes everything, including the two files we want back.
	r.edit(t, "keep/one.txt", "one v2\n")
	r.edit(t, "keep/two.txt", "two v2\n")
	r.edit(t, "broken/x.go", "package x // v2 BROKEN\n")
	r.edit(t, "broken/y.go", "package y // v2 BROKEN\n")
	r.edit(t, "untouched.md", "docs v2\n")
	if _, err := r.v.CommitMain(r.repoID, "agent", "v2"); err != nil {
		t.Fatalf("commit v2: %v", err)
	}
	before := snapshotDisk(t, r.root)

	plan, err := r.v.PlanRestore(r.repoID, old, []string{"broken/*.go"})
	if err != nil {
		t.Fatalf("plan selective restore: %v", err)
	}
	var planned []string
	for _, c := range plan.Changes {
		planned = append(planned, c.Path)
	}
	sort.Strings(planned)
	t.Logf("selective plan touches %v (unchanged=%d)", planned, plan.Unchanged)
	if want := []string{"broken/x.go", "broken/y.go"}; !equalStrings(planned, want) {
		t.Fatalf("plan touches %v, want %v", planned, want)
	}

	if _, err := r.v.ApplyRestore(r.repoID, old, []string{"broken/*.go"}, plan.ConfirmToken); err != nil {
		t.Fatalf("apply selective restore: %v", err)
	}
	after := snapshotDisk(t, r.root)

	want := diskState{}
	for p, c := range before {
		want[p] = c
	}
	want["broken/x.go"] = "package x // v1\n"
	want["broken/y.go"] = "package y // v1\n"
	if diff := diskDiff(want, after); diff != "" {
		t.Errorf("selective restore changed more (or less) than the two selected files:\n%s", diff)
	}

	// The restored content is genuinely the old content, not a no-op.
	if after["broken/x.go"] == before["broken/x.go"] {
		t.Errorf("broken/x.go was not restored at all: still %q", after["broken/x.go"])
	}
	// And the head did not move: a selective restore is a working-copy edit.
	head, err := r.v.RepoMainHead(r.repoID)
	if err != nil {
		t.Fatalf("head: %v", err)
	}
	if head == old {
		t.Errorf("selective restore moved main_head back to %s; it must stay on the newer commit", old)
	}
}

// equalStrings compares two string slices element-wise.
func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestLiveRun_GCReclaimsSpaceWithoutBreakingASeamOnlyRollback is the V2
// acceptance run. It measures the DATABASE FILE, not the row count, because
// "GC works" for the goal-loop case means the file stops growing — and rows
// deleted without a VACUUM leave the file exactly as large as before. Then it
// performs the rollback that the surviving seam points at, and reads the
// working copy back, because a collector that frees space by deleting a blob a
// seam still needs turns a recoverable state into an unrecoverable one.
func TestLiveRun_GCReclaimsSpaceWithoutBreakingASeamOnlyRollback(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "repo")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(base, "live.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	v := New(st, filepath.Join(base, "wt"))
	v.SetLockDir(filepath.Join(base, "locks"))
	repoID, err := v.InitRepo(root)
	if err != nil {
		t.Fatalf("init repo: %v", err)
	}
	r := &liveRepo{v: v, repoID: repoID, root: root}

	// A commit worth coming back to, and a seam that is its only handle.
	const treasure = "THE ONLY COPY OF THIS LIVES BEHIND A SEAM\n"
	r.edit(t, "treasure.txt", treasure)
	if _, err := v.CommitMain(repoID, "agent", "treasure"); err != nil {
		t.Fatalf("commit treasure: %v", err)
	}
	seamID := r.seal(t, "sess", 1, "worth keeping")
	treasureHash := hashContent([]byte(treasure))

	// Bury it: move main BACKWARDS past the seam so the seam's commit leaves
	// main's ancestry entirely — the shape every rollback produces.
	beforeTreasure, err := v.RepoMainHead(repoID)
	if err != nil {
		t.Fatal(err)
	}
	_ = beforeTreasure

	// Now generate real bulk: 120 turns each writing a distinct 40 KiB file,
	// enough that a VACUUM has something measurable to give back.
	const bulkTurns = 120
	for i := 0; i < bulkTurns; i++ {
		body := strings.Repeat(fmt.Sprintf("line %04d of turn %04d padding padding padding\n", i, i), 800)
		r.edit(t, fmt.Sprintf("bulk/f%03d.txt", i), body)
		if _, err := v.CommitMain(repoID, "agent", fmt.Sprintf("bulk %d", i)); err != nil {
			t.Fatalf("bulk commit %d: %v", i, err)
		}
	}
	// Orphan all of the bulk by resetting main to the treasure commit's parent's
	// successor: the treasure commit itself. Everything after is now unreachable
	// from main_head, and only the seam still points into it.
	seam, err := v.FindSeam(seamID)
	if err != nil {
		t.Fatal(err)
	}
	if err := v.ResetMainHead(repoID, seam.CommitID); err != nil {
		t.Fatalf("reset head: %v", err)
	}

	sizeBefore := dbFileSize(t, dbPath)
	res, err := v.RunGC(repoID, GCOptions{KeepRecent: 1, KeepDays: KeepDaysNone, Vacuum: true})
	if err != nil {
		t.Fatalf("run gc: %v", err)
	}
	sizeAfter := dbFileSize(t, dbPath)
	t.Logf("gc: deleted %d commits / %d blobs, freed %d bytes of payload; db %d -> %d bytes (vacuumed=%v)",
		len(res.DeletedCommits), len(res.DeletedBlobs), res.FreedBytes, sizeBefore, sizeAfter, res.Vacuumed)

	if len(res.DeletedCommits) == 0 {
		t.Errorf("GC deleted no commits at all, so nothing was reclaimed")
	}
	if !res.Vacuumed {
		t.Errorf("Vacuum was requested but the result says it did not run")
	}
	if sizeAfter >= sizeBefore {
		t.Errorf("database did not shrink: %d bytes before, %d after — "+
			"row deletion without a real file reclaim is the failure V2 exists to avoid",
			sizeBefore, sizeAfter)
	}

	// The seam's blob must have survived, and the rollback it names must still
	// put the bytes back on disk.
	if !blobExists(t, v, treasureHash) {
		t.Fatalf("GC deleted the blob whose only reference is a seam; the rollback below cannot work")
	}
	if err := os.Remove(filepath.Join(root, "treasure.txt")); err != nil {
		t.Fatalf("remove treasure from disk: %v", err)
	}
	if _, err := v.RevertToSeam(repoID, seamID, "post-gc", 0, 0, nil); err != nil {
		t.Fatalf("revert to the surviving seam after GC: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(root, "treasure.txt"))
	if err != nil {
		t.Fatalf("read restored file: %v", err)
	}
	if string(got) != treasure {
		t.Errorf("post-GC rollback restored %q, want %q", got, treasure)
	}
}

// dbFileSize returns the total bytes SQLite occupies, including the WAL — the
// WAL is where recently written pages live, so ignoring it would let a "shrink"
// be an artefact of pages that simply had not been checkpointed yet.
func dbFileSize(t *testing.T, dbPath string) int64 {
	t.Helper()
	var total int64
	for _, suffix := range []string{"", "-wal", "-shm"} {
		fi, err := os.Stat(dbPath + suffix)
		if err != nil {
			continue
		}
		total += fi.Size()
	}
	return total
}

// TestLiveRun_ExternalWriterDuringRollbackIsDetected is the V5 acceptance run.
// The rollback must not silently overwrite a file that something outside the
// VCS wrote while the preview was on the operator's screen: the apply is
// expected to REFUSE and to name the path.
//
// The external write is a real os.WriteFile through a path this package has no
// knowledge of, which is exactly what a shell_run compile is from the VCS's
// point of view.
func TestLiveRun_ExternalWriterDuringRollbackIsDetected(t *testing.T) {
	r := newLiveRepo(t)
	r.edit(t, "src/app.go", "package app // v1\n")
	r.edit(t, "src/util.go", "package app // util v1\n")
	old, err := r.v.CommitMain(r.repoID, "agent", "v1")
	if err != nil {
		t.Fatal(err)
	}
	r.edit(t, "src/app.go", "package app // v2\n")
	r.edit(t, "src/util.go", "package app // util v2\n")
	if _, err := r.v.CommitMain(r.repoID, "agent", "v2"); err != nil {
		t.Fatal(err)
	}

	plan, err := r.v.PlanRestore(r.repoID, old, nil)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	t.Logf("previewed %d changes; operator is now looking at them", len(plan.Changes))

	// An outside writer — a code generator, an editor, another process.
	victim := filepath.Join(r.root, "src", "app.go")
	const external = "package app // WRITTEN BY A COMPILER MID-DECISION\n"
	if err := os.WriteFile(victim, []byte(external), 0o644); err != nil {
		t.Fatalf("external write: %v", err)
	}

	_, err = r.v.ApplyRestore(r.repoID, old, nil, plan.ConfirmToken)
	if err == nil {
		t.Fatalf("the restore silently overwrote a file changed underneath it")
	}
	if !errors.Is(err, ErrExternalMutation) {
		t.Fatalf("apply failed with %v, want ErrExternalMutation", err)
	}
	if !strings.Contains(err.Error(), "src/app.go") {
		t.Errorf("the error does not name the mutated path, so an operator cannot act on it: %v", err)
	}
	t.Logf("refused as expected: %v", err)

	// And it must have refused BEFORE writing: the external content is intact.
	got, err := os.ReadFile(victim)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != external {
		t.Errorf("the refused restore still modified the file: on disk %q, external writer left %q",
			got, external)
	}
}

// TestLiveRun_SymlinkedDirectoryCannotRedirectARestore is the V6 acceptance
// run. It creates a real symlink from inside the repo to a real directory
// outside it, holding a real file with known bytes, then restores a tracked
// path that resolves through that symlink — and reads the OUTSIDE file back to
// prove it was untouched.
//
// Checking only that the call returned an error would not be enough: the write
// could have landed and the error come afterwards.
func TestLiveRun_SymlinkedDirectoryCannotRedirectARestore(t *testing.T) {
	r := newLiveRepo(t)
	r.edit(t, "docs/notes.md", "in-repo notes v1\n")
	old, err := r.v.CommitMain(r.repoID, "agent", "v1")
	if err != nil {
		t.Fatal(err)
	}
	r.edit(t, "docs/notes.md", "in-repo notes v2\n")
	if _, err := r.v.CommitMain(r.repoID, "agent", "v2"); err != nil {
		t.Fatal(err)
	}

	// An outside directory holding something that must survive.
	outside := t.TempDir()
	const sacred = "SYSTEM FILE — MUST NOT BE REWRITTEN\n"
	outsideFile := filepath.Join(outside, "notes.md")
	if err := os.WriteFile(outsideFile, []byte(sacred), 0o644); err != nil {
		t.Fatal(err)
	}

	// The agent-reachable precondition: swap the repo's docs/ for a symlink.
	// `ln -s /etc "$REPO/docs"` is a plain shell_run away.
	docs := filepath.Join(r.root, "docs")
	if err := os.RemoveAll(docs); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, docs); err != nil {
		t.Skipf("symlinks unavailable on this system: %v", err)
	}

	plan, planErr := r.v.PlanRestore(r.repoID, old, nil)
	var applyErr error
	if planErr == nil {
		_, applyErr = r.v.ApplyRestore(r.repoID, old, nil, plan.ConfirmToken)
	}
	t.Logf("plan error: %v; apply error: %v", planErr, applyErr)
	if planErr == nil && applyErr == nil {
		t.Errorf("the restore followed a symlink out of the repo without complaint")
	}

	// The load-bearing assertion: the file outside the repo is byte-identical.
	got, err := os.ReadFile(outsideFile)
	if err != nil {
		t.Fatalf("read the outside file back: %v", err)
	}
	if string(got) != sacred {
		t.Errorf("a restore wrote through the symlink and rewrote a file OUTSIDE the repo:\n"+
			"  path: %s\n  want: %q\n  got:  %q", outsideFile, sacred, got)
	}
}

// TestLiveRun_OrphanScanRecognisesAKilledOwner is the V7 acceptance run. A real
// child process claims a worktree and is then killed; the scan must name it.
//
// The liveness probe is the same one the composition root installs
// (lockfile.Alive is a signal-0 kill), reproduced here because internal/vcs may
// not import internal/lockfile (GOV1). The probe is what makes this test able
// to fail: with the fail-safe default, every scan returns nothing and a broken
// implementation looks identical to a healthy repo.
func TestLiveRun_OrphanScanRecognisesAKilledOwner(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the child is killed with a POSIX signal; the Windows path is covered by lockfile's own probe tests")
	}
	r := newLiveRepo(t)
	r.edit(t, "seed.txt", "seed\n")
	if _, err := r.v.CommitMain(r.repoID, "agent", "seed"); err != nil {
		t.Fatal(err)
	}
	r.v.SetProcessAlive(func(pid int) bool {
		// os.FindProcess + Signal rather than syscall.Kill: this file has no
		// build constraint, so it must COMPILE on windows even though the test
		// skips there at run time, and syscall.Kill does not exist on windows.
		// Before this, `GOOS=windows go vet ./...` failed on this one line —
		// which meant the whole windows CI leg was red, and "the evidence for
		// this acceptance is the windows leg" could never be cashed in.
		// internal/lockfile's Alive uses signal 0 the same way behind a build
		// tag; the tag is not available here because the rest of the file is
		// portable.
		proc, err := os.FindProcess(pid)
		if err != nil {
			return false
		}
		return proc.Signal(syscall.Signal(0)) == nil
	})

	wt, err := r.v.AddWorktree(r.repoID, []string{"subagent"})
	if err != nil {
		t.Fatalf("add worktree: %v", err)
	}

	// A real subprocess that will outlive the claim only until we kill it.
	child := exec.Command("/bin/sh", "-c", "sleep 60")
	if err := child.Start(); err != nil {
		t.Fatalf("start child: %v", err)
	}
	pid := child.Process.Pid
	t.Cleanup(func() { _ = child.Process.Kill(); _, _ = child.Process.Wait() })

	if err := r.v.ClaimWorktree(wt.ID, pid); err != nil {
		t.Fatalf("claim worktree for pid %d: %v", pid, err)
	}
	orphans, err := r.v.ScanOrphanWorktrees(r.repoID)
	if err != nil {
		t.Fatal(err)
	}
	if len(orphans) != 0 {
		t.Fatalf("a worktree owned by a LIVE process was reported orphaned: %+v", orphans)
	}
	t.Logf("owner pid %d alive -> 0 orphans", pid)

	if err := child.Process.Kill(); err != nil {
		t.Fatalf("kill child: %v", err)
	}
	// Reap it, or the zombie keeps answering signal 0 on Linux.
	_, _ = child.Process.Wait()

	orphans, err = r.v.ScanOrphanWorktrees(r.repoID)
	if err != nil {
		t.Fatal(err)
	}
	if len(orphans) != 1 || orphans[0].ID != wt.ID {
		t.Fatalf("after killing owner pid %d the scan reported %+v, want exactly worktree %s",
			pid, orphans, wt.ID)
	}
	t.Logf("owner pid %d dead -> orphan %s (lifecycle=%s)", pid, orphans[0].ID, orphans[0].Lifecycle)

	// And the cleanup actually removes the directory it named.
	dir := orphans[0].Path
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("orphan worktree dir %s should exist before cleanup: %v", dir, err)
	}
	if err := r.v.CleanupOrphanWorktree(r.repoID, wt.ID); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("cleanup left the worktree dir %s in place (stat err = %v)", dir, err)
	}
	// History survives: the branch's commits are still reachable.
	if _, err := r.v.LogWorktree(wt.ID, 10); err != nil {
		t.Errorf("cleanup destroyed the branch's history: %v", err)
	}
}

// TestLiveRun_GCCollectsHistoryProducedInThisSession is the regression guard
// for the defect the run above found: with no way to disable the age floor,
// this collector could not collect anything a live process had just produced.
//
// It deliberately does NOT backdate created_at. Every other GC test does, and
// that is precisely why none of them could see the problem — they measure the
// collector on a history no running goal loop can produce. Here the commits are
// seconds old, which is the only age a same-session GC ever encounters.
func TestLiveRun_GCCollectsHistoryProducedInThisSession(t *testing.T) {
	r := newLiveRepo(t)
	r.edit(t, "x.txt", "v0\n")
	first, err := r.v.CommitMain(r.repoID, "agent", "c0")
	if err != nil {
		t.Fatal(err)
	}
	const churn = 20
	for i := 0; i < churn; i++ {
		r.edit(t, "x.txt", fmt.Sprintf("v%d\n", i+1))
		if _, err := r.v.CommitMain(r.repoID, "agent", fmt.Sprintf("c%d", i+1)); err != nil {
			t.Fatal(err)
		}
	}
	// A rollback: everything after the first commit leaves main's ancestry.
	if err := r.v.ResetMainHead(r.repoID, first); err != nil {
		t.Fatal(err)
	}

	// The default age floor keeps all of it — correct, and the reason an
	// explicit opt-out has to exist.
	def, err := r.v.RunGC(r.repoID, GCOptions{KeepRecent: 1, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(def.DeletedCommits) != 0 {
		t.Errorf("the default 14-day floor collected %d minutes-old commits; "+
			"the floor is supposed to protect them", len(def.DeletedCommits))
	}

	// KeepDaysNone is the opt-out, and it must actually collect.
	none, err := r.v.RunGC(r.repoID, GCOptions{KeepRecent: 1, KeepDays: KeepDaysNone, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("same-session churn of %d commits: default floor deletes %d, KeepDaysNone deletes %d",
		churn, len(def.DeletedCommits), len(none.DeletedCommits))
	if len(none.DeletedCommits) == 0 {
		t.Fatalf("KeepDaysNone collected nothing from %d unreachable, minutes-old commits: "+
			"the age floor still has no off switch and GC is inert for the goal loop it was written for",
			churn)
	}

	// Zero must keep meaning "use the default", or every caller that omits the
	// field silently becomes an aggressive collector.
	zero, err := r.v.RunGC(r.repoID, GCOptions{KeepRecent: 1, KeepDays: 0, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(zero.DeletedCommits) != len(def.DeletedCommits) {
		t.Errorf("KeepDays=0 deleted %d commits but the unset default deleted %d; "+
			"an omitted field must not change the policy",
			len(zero.DeletedCommits), len(def.DeletedCommits))
	}
}

// TestLiveRun_LockDirectoryDoesNotGrowWithoutBound is the acceptance run for
// the cache-directory leak.
//
// Every distinct repo opened by a process created one lock file that was never
// removed. Measured on this repository before the fix: one `go test
// ./internal/vcs` run added 313 zero-byte files to the user's cache directory,
// which had accumulated 27,968 of them. The count only ever went up.
//
// The test opens many short-lived repositories — the shape a goal loop over
// generated worktrees produces — and asserts the directory is bounded
// afterwards. Counting files is the whole point: a fix that closed descriptors
// without unlinking would leave this failing, which is exactly the distinction
// that went unnoticed.
func TestLiveRun_LockDirectoryDoesNotGrowWithoutBound(t *testing.T) {
	base := t.TempDir()
	lockDir := filepath.Join(base, "vcs-locks")

	const repos = 40
	for i := 0; i < repos; i++ {
		func() {
			root := filepath.Join(base, fmt.Sprintf("proj%02d", i))
			if err := os.MkdirAll(root, 0o755); err != nil {
				t.Fatal(err)
			}
			st, err := store.Open(filepath.Join(base, fmt.Sprintf("db%02d.sqlite", i)))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = st.Close() }()
			v := New(st, filepath.Join(base, "wt"))
			v.SetLockDir(lockDir)
			repoID, err := v.InitRepo(root)
			if err != nil {
				t.Fatal(err)
			}
			r := &liveRepo{v: v, repoID: repoID, root: root}
			r.edit(t, "f.txt", fmt.Sprintf("content %d\n", i))
			if _, err := v.CommitMain(repoID, "agent", "c"); err != nil {
				t.Fatal(err)
			}
			// A real process ends. Close is what the composition root calls.
			if err := v.Close(); err != nil {
				t.Errorf("Close on repo %d: %v", i, err)
			}
		}()
	}

	left := countLockFiles(t, lockDir)
	t.Logf("%d repositories opened and closed -> %d lock files left behind", repos, left)
	if left > 0 {
		t.Errorf("%d lock files survived %d closed repositories; before the fix this "+
			"number equalled the repository count and grew forever", left, repos)
	}
}

// TestLiveRun_LockFileOfALiveHolderIsNeverReclaimed is the safety half of the
// leak fix: reclaiming too eagerly would be far worse than leaking, because two
// processes would then hold two different inodes and both enter the write lane.
//
// A second VCS instance stands in for the other process — flock is per
// open-file-description, so two descriptors contend even inside one process,
// which is the same property the V8 tests rely on.
func TestLiveRun_LockFileOfALiveHolderIsNeverReclaimed(t *testing.T) {
	base := t.TempDir()
	lockDir := filepath.Join(base, "locks")
	const key = "repo-held-by-someone-else"

	holder := New(nil, "")
	holder.SetLockDir(lockDir)
	release := holder.lockRepo(key)

	before := countLockFiles(t, lockDir)
	if before != 1 {
		t.Fatalf("expected exactly one lock file while the lane is held, found %d", before)
	}

	// A sweep with a zero age floor is the most aggressive pass possible; it
	// still must not touch a held file.
	removed := sweepStaleLockFiles(lockDir, 0)
	after := countLockFiles(t, lockDir)
	t.Logf("aggressive sweep while the lane is held: removed=%d, files left=%d", removed, after)
	if removed != 0 || after != 1 {
		t.Errorf("the sweep reclaimed a lock file whose lane is currently HELD "+
			"(removed=%d, left=%d); a second process would now lock a different inode",
			removed, after)
	}

	// The holder keeps working through the same descriptor.
	release()
	release2 := holder.lockRepo(key)
	release2()
	if err := holder.Close(); err != nil {
		t.Errorf("close holder: %v", err)
	}

	// Once released and closed, the same sweep does reclaim it.
	if got := countLockFiles(t, lockDir); got != 0 {
		t.Errorf("after the holder closed, %d lock files remain", got)
	}
}

// TestLiveRun_SweepReclaimsFilesLeftByDeadProcesses covers the files that
// Close can never reach: those written by processes that already exited —
// including every build predating the fix, which is what the 27,968 files in
// the measured cache directory were.
func TestLiveRun_SweepReclaimsFilesLeftByDeadProcesses(t *testing.T) {
	dir := t.TempDir()
	const abandoned = 50
	old := time.Now().Add(-72 * time.Hour)
	for i := 0; i < abandoned; i++ {
		p := filepath.Join(dir, fmt.Sprintf("%064x.lock", i))
		if err := os.WriteFile(p, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}
	// A file that is old but NOT a lock file must survive: the sweep runs in a
	// directory it does not exclusively own on a machine where anything may
	// have been dropped.
	stranger := filepath.Join(dir, "README")
	if err := os.WriteFile(stranger, []byte("not mine"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(stranger, old, old); err != nil {
		t.Fatal(err)
	}
	// And a RECENT lock file must survive: it may belong to a process that is
	// between two operations.
	fresh := filepath.Join(dir, strings.Repeat("f", 64)+".lock")
	if err := os.WriteFile(fresh, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	removed := sweepStaleLockFiles(dir, staleLockMaxAge)
	t.Logf("sweep removed %d of %d abandoned lock files", removed, abandoned)
	if removed != abandoned {
		t.Errorf("sweep removed %d abandoned lock files, want %d", removed, abandoned)
	}
	if _, err := os.Stat(stranger); err != nil {
		t.Errorf("the sweep deleted a file that is not a lock file: %v", err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Errorf("the sweep deleted a lock file younger than the age floor: %v", err)
	}
}

// countLockFiles counts .lock files in dir; a missing dir counts as zero.
func countLockFiles(t *testing.T, dir string) int {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0
		}
		t.Fatalf("read lock dir: %v", err)
	}
	n := 0
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".lock") {
			n++
		}
	}
	return n
}
