package vcs

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/x6nux/yanshi/internal/store"
)

// deadPID is a PID the tests declare dead through the injected probe. Using an
// injected probe rather than a real dead process is deliberate: spawning and
// killing a process to observe its corpse is slow, platform-specific and
// racy (PIDs are recycled), and the production probe already has its own tests
// in internal/lockfile.
const deadPID = 424242

// livePID is a PID the tests declare alive.
const livePID = 313131

// installTestProbe makes deadPID dead and everything else alive.
func installTestProbe(v *VCS) {
	v.SetProcessAlive(func(pid int) bool { return pid != deadPID })
}

// newWorktreeRepo builds a repo whose worktreeDir already EXISTS on disk.
//
// setupSeamTestRepo is deliberately not reused for the cleanup tests: it hands
// New a directory that has not been created yet, so canonicalPath cannot
// EvalSymlinks it. On macOS the stored worktree path then resolves through
// /var -> /private/var while v.worktreeDir does not, and removeWorktreeLocked's
// "is this under worktreeDir?" guard correctly declines to delete anything.
// That is the guard working, not a bug — but it means a cleanup test built on
// that helper would assert nothing.
func newWorktreeRepo(t *testing.T) (*VCS, string, string) {
	t.Helper()
	base := t.TempDir()
	root := filepath.Join(base, "repo")
	wtDir := filepath.Join(base, "worktrees")
	for _, d := range []string{root, wtDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	st, err := store.Open(filepath.Join(base, "test.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	v := New(st, wtDir)
	repoID, err := v.InitRepo(root)
	if err != nil {
		t.Fatalf("InitRepo: %v", err)
	}
	seed := filepath.Join(root, "a.txt")
	if err := os.WriteFile(seed, []byte("v0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := v.RecordEditMain(repoID, "test", seed, []byte("v0\n")); err != nil {
		t.Fatalf("seed record: %v", err)
	}
	if _, err := v.CommitMain(repoID, "test", "seed"); err != nil {
		t.Fatalf("seed commit: %v", err)
	}
	return v, repoID, root
}

// newWorktree creates a worktree with one commit on its branch.
func newWorktree(t *testing.T, v *VCS, repoID string) Worktree {
	t.Helper()
	wt, err := v.AddWorktree(repoID, []string{"agent"})
	if err != nil {
		t.Fatalf("AddWorktree: %v", err)
	}
	f := filepath.Join(wt.Path, "work.txt")
	if err := os.WriteFile(f, []byte("branch work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := v.RecordEditWorktree(wt.ID, "agent", f, []byte("branch work\n")); err != nil {
		t.Fatalf("RecordEditWorktree: %v", err)
	}
	if _, err := v.CommitWorktree(wt.ID, "agent", "branch commit"); err != nil {
		t.Fatalf("CommitWorktree: %v", err)
	}
	return wt
}

// TestWorktreeOrphaned_Definition is the decision table. It is a unit test on
// the predicate rather than an end-to-end one because the predicate is where a
// mistake costs a user their work: reporting a LIVE agent's branch as an orphan
// invites a cleanup that deletes running work.
func TestWorktreeOrphaned_Definition(t *testing.T) {
	tests := []struct {
		name  string
		state WorktreeState
		want  bool
	}{
		{
			name:  "dead owner, never merged",
			state: WorktreeState{Lifecycle: WorktreeActive, OwnerPID: deadPID},
			want:  true,
		},
		{
			name:  "live owner",
			state: WorktreeState{Lifecycle: WorktreeActive, OwnerPID: livePID},
		},
		{
			name:  "dead owner but already merged",
			state: WorktreeState{Lifecycle: WorktreeMerged, OwnerPID: deadPID},
		},
		{
			name:  "dead owner but deliberately abandoned",
			state: WorktreeState{Lifecycle: WorktreeAbandoned, OwnerPID: deadPID},
		},
		{
			// "Nobody ever claimed it" is indistinguishable from "the claim
			// predates V7". Deleting work on the strength of a MISSING record
			// is the one mistake this feature must not make.
			name:  "no owner recorded is not an orphan",
			state: WorktreeState{Lifecycle: WorktreeActive, OwnerPID: 0},
		},
		{
			name:  "negative pid is not an orphan",
			state: WorktreeState{Lifecycle: WorktreeActive, OwnerPID: -1},
		},
		{
			name:  "this very process is obviously alive",
			state: WorktreeState{Lifecycle: WorktreeActive, OwnerPID: os.Getpid()},
		},
		{
			name:  "already marked orphaned stays orphaned while the owner is dead",
			state: WorktreeState{Lifecycle: WorktreeOrphaned, OwnerPID: deadPID},
			want:  true,
		},
	}
	alive := func(pid int) bool { return pid != deadPID }
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.state.Orphaned(alive); got != tc.want {
				t.Errorf("Orphaned = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestScanOrphanWorktrees_FindsOnlyTheDeadAgentsBranch is the end-to-end scan.
func TestScanOrphanWorktrees_FindsOnlyTheDeadAgentsBranch(t *testing.T) {
	v, repoID, _ := setupSeamTestRepo(t)
	installTestProbe(v)

	crashed := newWorktree(t, v, repoID)
	running := newWorktree(t, v, repoID)
	finished := newWorktree(t, v, repoID)
	unclaimed := newWorktree(t, v, repoID)

	if err := v.ClaimWorktree(crashed.ID, deadPID); err != nil {
		t.Fatalf("claim crashed: %v", err)
	}
	if err := v.ClaimWorktree(running.ID, livePID); err != nil {
		t.Fatalf("claim running: %v", err)
	}
	if err := v.ClaimWorktree(finished.ID, deadPID); err != nil {
		t.Fatalf("claim finished: %v", err)
	}
	if err := v.MarkWorktreeMerged(finished.ID); err != nil {
		t.Fatalf("mark merged: %v", err)
	}

	orphans, err := v.ScanOrphanWorktrees(repoID)
	if err != nil {
		t.Fatalf("ScanOrphanWorktrees: %v", err)
	}
	if len(orphans) != 1 || orphans[0].ID != crashed.ID {
		var ids []string
		for _, o := range orphans {
			ids = append(ids, o.ID)
		}
		t.Fatalf("orphans = %v, want exactly [%s] "+
			"(running=%s finished=%s unclaimed=%s)",
			ids, crashed.ID, running.ID, finished.ID, unclaimed.ID)
	}
}

// TestScanOrphanWorktrees_UnwiredProbeReportsNothing pins the fail-safe
// default. An unwired build must report NO orphans rather than propose deleting
// every branch it cannot vouch for.
func TestScanOrphanWorktrees_UnwiredProbeReportsNothing(t *testing.T) {
	v, repoID, _ := setupSeamTestRepo(t)
	wt := newWorktree(t, v, repoID)
	if err := v.ClaimWorktree(wt.ID, deadPID); err != nil {
		t.Fatal(err)
	}
	// No SetProcessAlive call: this is the state of a build whose composition
	// root forgot to wire the probe.
	orphans, err := v.ScanOrphanWorktrees(repoID)
	if err != nil {
		t.Fatalf("ScanOrphanWorktrees: %v", err)
	}
	if len(orphans) != 0 {
		t.Fatalf("an unwired liveness probe reported %d orphans; "+
			"the fail-safe direction is zero", len(orphans))
	}
	// And once wired, the same worktree IS an orphan — otherwise the test above
	// would pass for a scan that never worked at all.
	installTestProbe(v)
	orphans, err = v.ScanOrphanWorktrees(repoID)
	if err != nil {
		t.Fatal(err)
	}
	if len(orphans) != 1 {
		t.Fatalf("with a wired probe: %d orphans, want 1", len(orphans))
	}
}

// TestMergeToMainRecordsTheMergedLifecycle pins the hook: the ledger is written
// by the operation that makes the fact true. Without it a merged branch reads
// "active" and gets reported as an orphan the moment its owner exits.
func TestMergeToMainRecordsTheMergedLifecycle(t *testing.T) {
	v, repoID, _ := setupSeamTestRepo(t)
	installTestProbe(v)
	wt := newWorktree(t, v, repoID)
	if err := v.ClaimWorktree(wt.ID, deadPID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := v.MergeToMain(wt.ID, "agent", false); err != nil {
		t.Fatalf("MergeToMain: %v", err)
	}
	states, err := v.ListWorktreeStates(repoID)
	if err != nil {
		t.Fatalf("ListWorktreeStates: %v", err)
	}
	var found bool
	for _, s := range states {
		if s.ID != wt.ID {
			continue
		}
		found = true
		if s.Lifecycle != WorktreeMerged {
			t.Errorf("lifecycle after merge = %q, want %q", s.Lifecycle, WorktreeMerged)
		}
	}
	if !found {
		t.Fatalf("worktree %s missing from the state listing", wt.ID)
	}
	orphans, err := v.ScanOrphanWorktrees(repoID)
	if err != nil {
		t.Fatal(err)
	}
	if len(orphans) != 0 {
		t.Errorf("a merged branch with a dead owner was reported as an orphan: %+v", orphans)
	}
}

// TestCleanupOrphanWorktree_RemovesTheDirButKeepsTheHistory pins the promise in
// CleanupOrphanWorktree's doc comment: the directory goes, the work stays
// recoverable.
func TestCleanupOrphanWorktree_RemovesTheDirButKeepsTheHistory(t *testing.T) {
	v, repoID, _ := newWorktreeRepo(t)
	installTestProbe(v)
	wt := newWorktree(t, v, repoID)
	if err := v.ClaimWorktree(wt.ID, deadPID); err != nil {
		t.Fatal(err)
	}
	tip := v.worktreeTip(wt.ID)
	if tip == "" {
		t.Fatal("precondition: the branch must have a commit")
	}

	if err := v.CleanupOrphanWorktree(repoID, wt.ID); err != nil {
		t.Fatalf("CleanupOrphanWorktree: %v", err)
	}
	if _, err := os.Stat(wt.Path); !os.IsNotExist(err) {
		t.Errorf("working dir %s survived cleanup (stat err = %v)", wt.Path, err)
	}
	if !commitExists(t, v, tip) {
		t.Fatal("cleanup deleted the branch's history; it must stay recoverable")
	}
	log, err := v.LogWorktree(wt.ID, 10)
	if err != nil || len(log) == 0 {
		t.Fatalf("branch history unreadable after cleanup: %d commits, err=%v", len(log), err)
	}
	// The recorded lifecycle is the audit trail for what happened.
	states, err := v.ListWorktreeStates(repoID)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range states {
		if s.ID == wt.ID && s.Lifecycle != WorktreeOrphaned {
			t.Errorf("lifecycle after cleanup = %q, want %q", s.Lifecycle, WorktreeOrphaned)
		}
	}
	// A GC must still spare it: the worktree row's tip is a reachability root.
	backdateAll(t, v, repoID)
	if _, err := v.RunGC(repoID, aggressiveGC); err != nil {
		t.Fatalf("RunGC: %v", err)
	}
	if !commitExists(t, v, tip) {
		t.Error("GC collected a cleaned-up orphan's history")
	}
}

// TestCleanupOrphanWorktree_RefusesALiveBranch is the guard: a branch the scan
// does not currently name must not be removable through this path.
func TestCleanupOrphanWorktree_RefusesALiveBranch(t *testing.T) {
	v, repoID, _ := newWorktreeRepo(t)
	installTestProbe(v)
	wt := newWorktree(t, v, repoID)
	if err := v.ClaimWorktree(wt.ID, livePID); err != nil {
		t.Fatal(err)
	}
	if err := v.CleanupOrphanWorktree(repoID, wt.ID); !errors.Is(err, ErrWorktreeNotOrphaned) {
		t.Fatalf("err = %v, want ErrWorktreeNotOrphaned", err)
	}
	if _, err := os.Stat(wt.Path); err != nil {
		t.Errorf("a live agent's working dir was disturbed: %v", err)
	}
}

// TestCleanupOrphanWorktree_RechecksRatherThanTrustingAnEarlierScan covers the
// TOCTOU the re-scan exists for: a worktree that WAS an orphan when the caller
// listed it, but whose PID has since been recycled by a live process.
func TestCleanupOrphanWorktree_RechecksRatherThanTrustingAnEarlierScan(t *testing.T) {
	v, repoID, _ := newWorktreeRepo(t)
	installTestProbe(v)
	wt := newWorktree(t, v, repoID)
	if err := v.ClaimWorktree(wt.ID, deadPID); err != nil {
		t.Fatal(err)
	}
	orphans, err := v.ScanOrphanWorktrees(repoID)
	if err != nil || len(orphans) != 1 {
		t.Fatalf("precondition: %d orphans, err=%v", len(orphans), err)
	}
	// The PID comes back to life between the caller's scan and its cleanup.
	v.SetProcessAlive(func(int) bool { return true })
	if err := v.CleanupOrphanWorktree(repoID, wt.ID); !errors.Is(err, ErrWorktreeNotOrphaned) {
		t.Fatalf("err = %v, want ErrWorktreeNotOrphaned", err)
	}
	if _, err := os.Stat(wt.Path); err != nil {
		t.Errorf("working dir removed despite the owner being alive again: %v", err)
	}
}

// TestHeartbeatWorktree pins the two behaviours the heartbeat has: it advances
// the stamp, it refuses an unclaimed worktree, and it does NOT resurrect a
// merged branch.
func TestHeartbeatWorktree(t *testing.T) {
	v, repoID, _ := setupSeamTestRepo(t)
	wt := newWorktree(t, v, repoID)

	if err := v.HeartbeatWorktree(wt.ID); err == nil {
		t.Error("a heartbeat on an unclaimed worktree must fail; " +
			"otherwise a typo'd id silently succeeds forever")
	}
	if err := v.ClaimWorktree(wt.ID, livePID); err != nil {
		t.Fatal(err)
	}
	first := stateOf(t, v, repoID, wt.ID)
	if first.HeartbeatAt == 0 {
		t.Error("ClaimWorktree must stamp a first heartbeat")
	}
	if first.OwnerPID != livePID {
		t.Errorf("owner = %d, want %d", first.OwnerPID, livePID)
	}

	// Unix-second granularity, so move the clock rather than hoping.
	if _, err := v.store.DB.Exec(
		"UPDATE vcs_worktree_state SET heartbeat_at=? WHERE worktree_id=?",
		time.Now().Add(-time.Hour).Unix(), wt.ID); err != nil {
		t.Fatal(err)
	}
	if err := v.HeartbeatWorktree(wt.ID); err != nil {
		t.Fatalf("HeartbeatWorktree: %v", err)
	}
	if after := stateOf(t, v, repoID, wt.ID); after.HeartbeatAt <= first.HeartbeatAt-3600 {
		t.Errorf("heartbeat did not advance: %d", after.HeartbeatAt)
	}

	if err := v.MarkWorktreeMerged(wt.ID); err != nil {
		t.Fatal(err)
	}
	if err := v.HeartbeatWorktree(wt.ID); err != nil {
		t.Fatalf("heartbeat on a merged worktree: %v", err)
	}
	if s := stateOf(t, v, repoID, wt.ID); s.Lifecycle != WorktreeMerged {
		t.Errorf("a heartbeat resurrected a merged branch to %q; "+
			"a late writer is not a resurrection", s.Lifecycle)
	}
}

// TestListWorktreeStates_DefaultsForRowsPredatingTheLedger pins the migration
// path: a worktree created before V7 has no state row and must read as
// active/unowned, which is exactly what it is.
func TestListWorktreeStates_DefaultsForRowsPredatingTheLedger(t *testing.T) {
	v, repoID, _ := setupSeamTestRepo(t)
	wt := newWorktree(t, v, repoID)
	s := stateOf(t, v, repoID, wt.ID)
	if s.Lifecycle != WorktreeActive {
		t.Errorf("lifecycle = %q, want %q", s.Lifecycle, WorktreeActive)
	}
	if s.OwnerPID != 0 || s.HeartbeatAt != 0 {
		t.Errorf("unclaimed worktree carries owner=%d heartbeat=%d, want 0/0",
			s.OwnerPID, s.HeartbeatAt)
	}
	if !s.Active {
		t.Error("a fresh worktree's directory is present, so Active must be true")
	}
	if s.BaseCommit == "" || s.Tip == "" {
		t.Error("the state view must carry the underlying worktree fields")
	}
}

// TestClaimWorktree_RejectsAnInvalidPID keeps a zero from being written as an
// owner, which would make the worktree permanently unorphanable.
func TestClaimWorktree_RejectsAnInvalidPID(t *testing.T) {
	v, repoID, _ := setupSeamTestRepo(t)
	wt := newWorktree(t, v, repoID)
	for _, pid := range []int{0, -1} {
		if err := v.ClaimWorktree(wt.ID, pid); err == nil {
			t.Errorf("ClaimWorktree(pid=%d) must fail", pid)
		}
	}
}

// stateOf returns one worktree's ledger entry.
func stateOf(t *testing.T, v *VCS, repoID, wtID string) WorktreeState {
	t.Helper()
	states, err := v.ListWorktreeStates(repoID)
	if err != nil {
		t.Fatalf("ListWorktreeStates: %v", err)
	}
	for _, s := range states {
		if s.ID == wtID {
			return s
		}
	}
	t.Fatalf("worktree %s not in state listing", wtID)
	return WorktreeState{}
}
