// internal/vcs/crossproc_test.go
//
// V8 regression tests.
//
// The central test (TestV8_ConcurrentWritersDoNotLoseCommits) is a REGRESSION
// test in the literal sense: it fails on the pre-V8 code. Measured there with
// two independent store handles on one database file, 57 of 120 files went
// missing from the final main_head tree. Two store.Open handles are the right
// model for two processes because the thing that does NOT cross the boundary —
// store.writeMu — is per-Store, and everything that does cross it (the SQLite
// file locks, the WAL, and now the flock) is shared by inode. The subprocess
// test below pins that equivalence rather than assuming it.

package vcs

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/store"
)

// twoWriterFixture builds one repo on disk plus two VCS instances backed by
// SEPARATE store handles on the same database file, sharing one lock dir.
// That is the in-test stand-in for two yanshi processes.
func twoWriterFixture(t *testing.T) (v1, v2 *VCS, repoID, root string) {
	t.Helper()
	base := t.TempDir()
	root = filepath.Join(base, "repo")
	require.NoError(t, os.MkdirAll(root, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "seed.txt"), []byte("seed"), 0o644))

	dbPath := filepath.Join(base, "yanshi.db")
	s1, err := store.Open(dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { s1.Close() })
	s2, err := store.Open(dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { s2.Close() })

	lockDir := filepath.Join(base, "locks")
	v1 = New(s1, filepath.Join(base, "wt1"))
	v1.SetLockDir(lockDir)
	v2 = New(s2, filepath.Join(base, "wt2"))
	v2.SetLockDir(lockDir)
	// lockRepo 打开的锁文件描述符是按 VCS 生命周期保留的（见 crossproc.go
	// lockFileFor），VCS.Close 正是为此设计的释放口；不关的话 windows 上
	// t.TempDir 的 RemoveAll 会撞上仍持有的 .lock 句柄（2026-09-01 CI）。
	//
	// Close 必须等测试派生的 lock goroutine 全部退场：那些 goroutine 里的
	// unlockFile 会读锁文件 FD，与 Close 的 unlinkHeldLockFile 关闭动作构成
	// data race（2026-09-01 race leg，TestV8_LaneIsMutuallyExclusive 等）。
	// 各测试通过 wgGo 注册自己的后台 goroutine，cleanup 先等再关。
	var wg sync.WaitGroup
	t.Cleanup(func() {
		wg.Wait()
		_ = v1.Close()
		_ = v2.Close()
	})
	wgGo = func(fn func()) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			fn()
		}()
	}

	repoID, err = v1.InitRepo(root)
	require.NoError(t, err)
	return v1, v2, repoID, root
}

// TestV8_ConcurrentWritersDoNotLoseCommits is the regression gate. Each writer
// records a uniquely-named file and commits; every file must be present in the
// final main_head tree.
//
// commitScope reads main_head and vcs_uncommitted, builds the new tree in Go,
// and only then opens the write transaction. Without a cross-process lane, the
// writer that read the older head commits a tree that never saw the other's
// file and then points main_head at it — the other commit is orphaned. No
// SQLite setting prevents this, because the read and the write are separate
// transactions with Go code in between.
func TestV8_ConcurrentWritersDoNotLoseCommits(t *testing.T) {
	const perWriter = 40
	v1, v2, repoID, root := twoWriterFixture(t)

	var wg sync.WaitGroup
	for writer, v := range []*VCS{v1, v2} {
		wg.Add(1)
		go func(writer int, v *VCS) {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				rel := fmt.Sprintf("w%d_%03d.txt", writer, i)
				abs := filepath.Join(root, rel)
				if err := os.WriteFile(abs, []byte(rel), 0o644); err != nil {
					t.Errorf("write %s: %v", rel, err)
					return
				}
				if err := v.RecordEditMain(repoID, "test", abs, []byte(rel)); err != nil {
					t.Errorf("record %s: %v", rel, err)
					return
				}
				// ErrNoChanges is legitimate: the other writer's commit may have
				// already folded this scope's pending edits (both write the
				// "main" scope of the same repo). It is not a lost update — the
				// file is in the tree either way, which is what we assert below.
				if _, err := v.CommitMain(repoID, "test", "c"); err != nil &&
					!strings.Contains(err.Error(), "no changes") {
					t.Errorf("commit %s: %v", rel, err)
					return
				}
			}
		}(writer, v)
	}
	wg.Wait()

	head, err := v1.RepoMainHead(repoID)
	require.NoError(t, err)
	// Drop the memo so the tree is reconstructed from SQLite, not from whatever
	// this instance happened to cache while writing.
	v1.treeCacheMu.Lock()
	v1.treeCache = map[string]map[string]string{}
	v1.treeCacheMu.Unlock()
	tree := v1.commitTree(head)

	var missing []string
	for writer := 0; writer < 2; writer++ {
		for i := 0; i < perWriter; i++ {
			rel := fmt.Sprintf("w%d_%03d.txt", writer, i)
			if _, ok := tree[rel]; !ok {
				missing = append(missing, rel)
			}
		}
	}
	assert.Emptyf(t, missing,
		"%d of %d committed files are absent from main_head: the write lane did "+
			"not serialize across store handles (lost update)",
		len(missing), 2*perWriter)
}

// wgGo is rebound by twoWriterFixture to run fn tracked on the fixture's
// WaitGroup, so cleanup waits for spawned lock goroutines before closing VCS
// handles (avoids the FD close-vs-unlock data race seen on the race leg).
var wgGo func(fn func())

// TestV8_LaneIsMutuallyExclusive proves the lane itself excludes, independently
// of what commitScope does with it. Without this, a change that kept the tree
// intact by some other means would let the lock rot undetected.
func TestV8_LaneIsMutuallyExclusive(t *testing.T) {
	v1, v2, repoID, _ := twoWriterFixture(t)

	unlock1 := v1.lockRepo(repoID)
	entered := make(chan struct{})
	wgGo(func() {
		unlock2 := v2.lockRepo(repoID)
		close(entered)
		unlock2()
	})

	select {
	case <-entered:
		unlock1()
		t.Fatal("the second holder entered the lane while the first still held it")
	case <-time.After(150 * time.Millisecond):
	}

	unlock1()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the second holder never acquired the lane after release")
	}
}

// TestV8_DistinctReposDoNotSerialize pins that the lock is PER REPO. A single
// global lock would also pass the lost-update test while needlessly serializing
// unrelated projects, so this is what keeps the fix from over-reaching.
func TestV8_DistinctReposDoNotSerialize(t *testing.T) {
	v1, v2, repoID, _ := twoWriterFixture(t)

	unlock1 := v1.lockRepo(repoID)
	defer unlock1()

	acquired := make(chan struct{})
	wgGo(func() {
		unlock := v2.lockRepo("some-other-repo-id")
		close(acquired)
		unlock()
	})

	select {
	case <-acquired:
	case <-time.After(5 * time.Second):
		t.Fatal("a lane for a DIFFERENT repo blocked on an unrelated repo's lock")
	}
}

// TestV8_LockIsReleasedWhenHolderProcessDies is the crash-recovery case.
//
// A helper process takes the lane and then exits WITHOUT unlocking (os.Exit
// skips every defer, which is what a crash looks like). The lock must still
// become available, because the kernel drops flock/LockFileEx when the holding
// descriptor is closed by process teardown. This is the property that let V8
// skip the PID-liveness reclaim that internal/lockfile needs.
func TestV8_LockIsReleasedWhenHolderProcessDies(t *testing.T) {
	if os.Getenv("YANSHI_V8_HOLD_LOCK") != "" {
		holdLockAndDie()
		return
	}

	base := t.TempDir()
	lockDir := filepath.Join(base, "locks")
	lockKey := "crash-recovery-repo"

	ready := filepath.Join(base, "ready")
	cmd := exec.Command(os.Args[0], "-test.run", "TestV8_LockIsReleasedWhenHolderProcessDies")
	cmd.Env = append(os.Environ(),
		"YANSHI_V8_HOLD_LOCK=1",
		"YANSHI_V8_LOCK_DIR="+lockDir,
		"YANSHI_V8_LOCK_KEY="+lockKey,
		"YANSHI_V8_READY_FILE="+ready,
	)
	out, err := cmd.CombinedOutput()
	// The helper kills itself with a non-zero status on purpose.
	require.Error(t, err, "helper should have exited non-zero; output: %s", out)
	require.FileExists(t, ready,
		"helper never reported that it held the lock; output: %s", out)

	// The holder is gone without ever unlocking. The lane must be free.
	v := New(nil, filepath.Join(base, "wt"))
	v.SetLockDir(lockDir)
	gone := make(chan struct{})
	acquired := make(chan struct{})
	wgGo(func() {
		unlock := v.lockRepo(lockKey)
		close(acquired)
		unlock()
		close(gone)
	})
	select {
	case <-acquired:
	case <-time.After(10 * time.Second):
		t.Fatal("the lock survived its holder's death: a crashed process would " +
			"wedge the repo write lane permanently")
	}
	// Close 不能与 unlockFile 并发（FD close-vs-read race）：等 goroutine
	// 完成 unlock 退出后再关。
	<-gone
	_ = v.Close()
}

// holdLockAndDie is the helper-process body for the crash test. It takes the
// lane, signals readiness by creating a file, then terminates via os.Exit so no
// deferred unlock ever runs.
func holdLockAndDie() {
	v := New(nil, "")
	v.SetLockDir(os.Getenv("YANSHI_V8_LOCK_DIR"))
	_ = v.lockRepo(os.Getenv("YANSHI_V8_LOCK_KEY"))
	if err := os.WriteFile(os.Getenv("YANSHI_V8_READY_FILE"), []byte("held"), 0o644); err != nil {
		os.Exit(3)
	}
	// Give the parent a moment to observe the held lock before dying.
	time.Sleep(200 * time.Millisecond)
	os.Exit(7)
}

// TestV8_RealSubprocessesDoNotLoseCommits closes the modelling gap: every other
// test uses two store handles in ONE process as a stand-in for two processes.
// That stand-in is sound only if nothing process-global is doing the
// serialization, so this case runs genuine child processes against one database
// and asserts the same invariant.
func TestV8_RealSubprocessesDoNotLoseCommits(t *testing.T) {
	if os.Getenv("YANSHI_V8_CHILD_WRITER") != "" {
		runChildWriter()
		return
	}

	base := t.TempDir()
	root := filepath.Join(base, "repo")
	require.NoError(t, os.MkdirAll(root, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "seed.txt"), []byte("seed"), 0o644))
	dbPath := filepath.Join(base, "yanshi.db")
	lockDir := filepath.Join(base, "locks")

	parent, err := store.Open(dbPath)
	require.NoError(t, err)
	v := New(parent, filepath.Join(base, "wt"))
	v.SetLockDir(lockDir)
	t.Cleanup(func() { _ = v.Close() })
	repoID, err := v.InitRepo(root)
	require.NoError(t, err)
	// Close the parent handle so the children contend only with each other.
	require.NoError(t, parent.Close())

	const children, perChild = 3, 15
	var wg sync.WaitGroup
	errs := make(chan error, children)
	for c := 0; c < children; c++ {
		wg.Add(1)
		go func(c int) {
			defer wg.Done()
			cmd := exec.Command(os.Args[0], "-test.run", "TestV8_RealSubprocessesDoNotLoseCommits")
			cmd.Env = append(os.Environ(),
				"YANSHI_V8_CHILD_WRITER=1",
				"YANSHI_V8_DB="+dbPath,
				"YANSHI_V8_ROOT="+root,
				"YANSHI_V8_LOCK_DIR="+lockDir,
				"YANSHI_V8_REPO_ID="+repoID,
				fmt.Sprintf("YANSHI_V8_WRITER=%d", c),
				fmt.Sprintf("YANSHI_V8_COUNT=%d", perChild),
			)
			if out, err := cmd.CombinedOutput(); err != nil {
				errs <- fmt.Errorf("child %d: %w: %s", c, err, out)
			}
		}(c)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}

	verify, err := store.Open(dbPath)
	require.NoError(t, err)
	defer verify.Close()
	vv := New(verify, filepath.Join(base, "wt-verify"))
	head, err := vv.RepoMainHead(repoID)
	require.NoError(t, err)
	tree := vv.commitTree(head)

	var missing []string
	for c := 0; c < children; c++ {
		for i := 0; i < perChild; i++ {
			rel := fmt.Sprintf("c%d_%03d.txt", c, i)
			if _, ok := tree[rel]; !ok {
				missing = append(missing, rel)
			}
		}
	}
	assert.Emptyf(t, missing,
		"%d of %d files committed by real subprocesses are missing from "+
			"main_head — cross-process exclusion is not working",
		len(missing), children*perChild)
}

// runChildWriter is the helper-process body for the subprocess test: one real
// process recording and committing its own uniquely-named files.
func runChildWriter() {
	st, err := store.Open(os.Getenv("YANSHI_V8_DB"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "child store.Open:", err)
		os.Exit(1)
	}
	defer st.Close()

	root := os.Getenv("YANSHI_V8_ROOT")
	repoID := os.Getenv("YANSHI_V8_REPO_ID")
	v := New(st, filepath.Join(root, "..", "wt-child"))
	v.SetLockDir(os.Getenv("YANSHI_V8_LOCK_DIR"))

	var writer, count int
	fmt.Sscanf(os.Getenv("YANSHI_V8_WRITER"), "%d", &writer)
	fmt.Sscanf(os.Getenv("YANSHI_V8_COUNT"), "%d", &count)

	for i := 0; i < count; i++ {
		rel := fmt.Sprintf("c%d_%03d.txt", writer, i)
		abs := filepath.Join(root, rel)
		if err := os.WriteFile(abs, []byte(rel), 0o644); err != nil {
			fmt.Fprintln(os.Stderr, "child write:", err)
			os.Exit(1)
		}
		if err := v.RecordEditMain(repoID, "child", abs, []byte(rel)); err != nil {
			fmt.Fprintln(os.Stderr, "child record:", err)
			os.Exit(1)
		}
		if _, err := v.CommitMain(repoID, "child", "c"); err != nil &&
			!strings.Contains(err.Error(), "no changes") {
			fmt.Fprintln(os.Stderr, "child commit:", err)
			os.Exit(1)
		}
	}
	os.Exit(0)
}

// TestV8_LockFilePathIsStableAndDistinct pins the mapping from lane key to lock
// file. Stability across instances is what makes two processes pick the SAME
// file; distinctness is what keeps unrelated repos off one lock.
func TestV8_LockFilePathIsStableAndDistinct(t *testing.T) {
	dir := t.TempDir()
	a := New(nil, "")
	a.SetLockDir(dir)
	b := New(nil, "")
	b.SetLockDir(dir)

	cases := []struct {
		name string
		key  string
	}{
		{"repo id", "abc123"},
		{"init key", initRepoLockKey("/some/canonical/root")},
		{"windows style root", initRepoLockKey(`C:\code\yanshi`)},
		{"key with separators", "repo/with:odd\\chars"},
	}
	seen := map[string]string{}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pathA := a.lockFilePath(tc.key)
			assert.Equal(t, pathA, b.lockFilePath(tc.key),
				"two instances derived different lock files for one key: they "+
					"would take different locks and exclude nothing")
			assert.Equal(t, dir, filepath.Dir(pathA), "lock file left the configured dir")
			if prev, dup := seen[pathA]; dup {
				t.Fatalf("keys %q and %q collide on lock file %s", prev, tc.key, pathA)
			}
			seen[pathA] = tc.key
		})
	}
}

// TestV8_DefaultLockDirIsMachineWide pins that production (which never calls
// SetLockDir) still lands on one absolute, shared location. A relative path
// would silently make the lock per-working-directory.
func TestV8_DefaultLockDirIsMachineWide(t *testing.T) {
	dir := defaultLockDir()
	assert.True(t, filepath.IsAbs(dir),
		"the default lock dir is relative, so processes started from different "+
			"working directories would not share a lock")
	assert.Contains(t, dir, vcsLockDirName)

	v := New(nil, "")
	assert.Equal(t, dir, filepath.Dir(v.lockFilePath("some-repo")),
		"an instance with no SetLockDir must use the machine-wide default")
}

// TestV8_LaneIsReentrantAcrossSequentialAcquisitions pins that the retained
// descriptor is reusable: the same lane taken, released and taken again must
// not leak a lock or fail. A naive implementation that closed the file on
// release would break the second acquisition on POSIX.
func TestV8_LaneIsReentrantAcrossSequentialAcquisitions(t *testing.T) {
	v := New(nil, "")
	v.SetLockDir(t.TempDir())
	t.Cleanup(func() { _ = v.Close() })
	for i := 0; i < 5; i++ {
		unlock := v.lockRepo("repeat-key")
		unlock()
	}
	done := make(chan struct{})
	wgGo(func() {
		unlock := v.lockRepo("repeat-key")
		unlock()
		close(done)
	})
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the lane was not released by a previous acquisition")
	}
}
