package lockfile

// sweep_test.go — the stale-lockfile reclaim, and the isolation that keeps a
// test run from being the thing that fills the directory it is testing.
//
// Every other test in this package writes into the REAL user cache directory,
// because Dir() reads os.UserCacheDir() and nothing overrode it. That is how
// 1,475 lockfiles accumulated there, all naming temp directories deleted months
// earlier. These tests redirect the cache directory per test, so they exercise
// the sweep without adding to the pile.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// isolateCacheDir points os.UserCacheDir at a temp directory for the duration
// of the test, and returns the run directory the package will use.
//
// The environment variable differs per platform because os.UserCacheDir does:
// XDG_CACHE_HOME on Unix-likes other than macOS, HOME on macOS,
// LocalAppData on Windows.
func isolateCacheDir(t *testing.T) string {
	t.Helper()
	base := t.TempDir()
	switch runtime.GOOS {
	case "windows":
		t.Setenv("LocalAppData", base)
	case "darwin":
		t.Setenv("HOME", base)
	default:
		t.Setenv("XDG_CACHE_HOME", base)
	}
	dir, err := Dir()
	require.NoError(t, err)
	require.True(t, filepath.IsAbs(dir))
	require.NoError(t, os.MkdirAll(dir, 0o755))
	return dir
}

// writeAgedLockfile drops a lockfile naming pid/root and backdates its mtime.
func writeAgedLockfile(t *testing.T, dir, name string, pid int, root string, age time.Duration) string {
	t.Helper()
	p := filepath.Join(dir, name+".lock")
	data, err := json.Marshal(Lockfile{
		PID: pid, Addr: "127.0.0.1:1", Auth: "none", Root: root,
		StartedAt: time.Now().Add(-age), Version: currentVersion,
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(p, data, 0o644))
	when := time.Now().Add(-age)
	require.NoError(t, os.Chtimes(p, when, when))
	return p
}

// countLockfiles counts .lock entries in dir.
func countLockfiles(t *testing.T, dir string) int {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	n := 0
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".lock" {
			n++
		}
	}
	return n
}

// deadPID is a pid no process can hold. Signal 0 against it always fails.
const deadPID = 1<<31 - 1

// TestSweepStale_ReclaimsOnlyUnrecoverableLockfiles pins all four decisions the
// sweep makes, in one directory, so a change that widens it to "any dead PID"
// (which would delete a live project's recoverable lockfile) fails here.
func TestSweepStale_ReclaimsOnlyUnrecoverableLockfiles(t *testing.T) {
	dir := isolateCacheDir(t)
	liveRoot := t.TempDir() // a project that still exists

	// (1) Dead owner, vanished root, old: the accumulating population. Two of
	// them, one naming a directory that genuinely existed and was then removed
	// (the temp-dir case that produced the real 1,475) and one that never did.
	vanishedRoot := filepath.Join(t.TempDir(), "deleted-long-ago")
	require.NoError(t, os.MkdirAll(vanishedRoot, 0o755))
	writeAgedLockfile(t, dir, "vanished", deadPID, vanishedRoot, 72*time.Hour)
	require.NoError(t, os.RemoveAll(vanishedRoot))
	doomed := writeAgedLockfile(t, dir, "doomed", deadPID, "/nonexistent/project/xyz", 72*time.Hour)

	// (2) Dead owner but the project still exists: recoverable in place on the
	// next open, so deleting it gains nothing.
	recoverable := writeAgedLockfile(t, dir, "recoverable", deadPID, liveRoot, 72*time.Hour)

	// (3) Live owner: never touched, whatever its age.
	livePID := writeAgedLockfile(t, dir, "liveowner", os.Getpid(), "/nonexistent/other", 72*time.Hour)

	// (4) Young: inside the age floor, so a PID that is momentarily
	// unobservable cannot cost somebody their lockfile.
	young := writeAgedLockfile(t, dir, "young", deadPID, "/nonexistent/young", time.Minute)

	// (5) Not ours: unparseable content must survive, since this directory is
	// shared with whatever else lands in a user cache.
	stranger := filepath.Join(dir, "stranger.lock")
	require.NoError(t, os.WriteFile(stranger, []byte("garbage"), 0o644))
	old := time.Now().Add(-72 * time.Hour)
	require.NoError(t, os.Chtimes(stranger, old, old))

	removed := SweepStale(StaleMaxAge)
	t.Logf("sweep removed %d of %d lockfiles", removed, countLockfiles(t, dir)+removed)
	require.Equal(t, 2, removed, "only the dead-owner + vanished-root files may be reclaimed")

	for _, p := range []string{recoverable, livePID, young, stranger} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("sweep deleted a lockfile it must keep: %s (%v)", filepath.Base(p), err)
		}
	}
	if _, err := os.Stat(doomed); !os.IsNotExist(err) {
		t.Errorf("sweep kept an unrecoverable lockfile: %s (stat err %v)", filepath.Base(doomed), err)
	}
}

// TestAcquire_SweepsSynchronouslySoAShortLivedProcessActuallyReclaims is the
// regression guard for the defect this fix itself shipped first.
//
// The sweep was originally launched as a goroutine from Acquire. It was correct
// and it never ran: `yanshi exec` claims the lockfile, runs its turn and exits,
// and the process was gone before the goroutine was scheduled. A real exec left
// the directory completely unchanged while calling SweepStale directly removed
// 890 files from it.
//
// This test asserts the reclaim has happened by the time Acquire RETURNS, which
// is the only formulation a short-lived process can rely on. Under a
// backgrounded sweep it fails without needing a sleep to make it flaky.
func TestAcquire_SweepsSynchronouslySoAShortLivedProcessActuallyReclaims(t *testing.T) {
	dir := isolateCacheDir(t)
	// This process may already have swept (another test, or an earlier
	// Acquire), and the guard is deliberately once-per-process. Reset it so the
	// assertion is about Acquire's behaviour rather than about test ordering.
	sweepOnceGuard = sync.Once{}
	t.Cleanup(func() { sweepOnceGuard = sync.Once{} })

	for i := 0; i < 5; i++ {
		writeAgedLockfile(t, dir, "abandoned"+string(rune('a'+i)), deadPID,
			"/nonexistent/gone/"+string(rune('a'+i)), 72*time.Hour)
	}
	require.Equal(t, 5, countLockfiles(t, dir))

	mine := t.TempDir()
	won, err := Acquire(mine, Lockfile{PID: os.Getpid(), Addr: "127.0.0.1:2", Root: mine})
	require.NoError(t, err)
	require.True(t, won)

	// Only this caller's own lockfile may remain — checked with no sleep.
	left := countLockfiles(t, dir)
	t.Logf("after Acquire returned, %d lockfiles remain (want 1: our own)", left)
	if left != 1 {
		t.Errorf("Acquire returned with %d lockfiles still present; the sweep did not "+
			"complete before the call returned, so a short-lived process reclaims nothing", left)
	}
	if _, err := os.Stat(filepath.Join(dir, sanitize(mine)+".lock")); err != nil {
		t.Errorf("Acquire's own lockfile is missing: %v", err)
	}
}
