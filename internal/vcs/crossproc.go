// internal/vcs/crossproc.go
//
// V8: cross-process serialization of the per-repo write lane.
//
// # Why SQLite alone does not cover this
//
// The first thing to rule out is that the store already handles it. It does
// not, and the reason is structural rather than a missing PRAGMA:
//
//   - busy_timeout and WAL (internal/store.buildDSN) make concurrent writers
//     WAIT instead of returning SQLITE_BUSY. That is about lock contention on
//     individual statements, not about atomicity of a composite operation.
//   - store.writeMu serializes writers within ONE process. Two yanshi
//     processes have two Stores and therefore two unrelated mutexes.
//   - Wrapping the writes in BEGIN IMMEDIATE would not help either, because the
//     critical sections are not purely database work. commitScope READS
//     main_head and vcs_uncommitted, computes a tree in Go, and only then opens
//     the write transaction. MergeToMain, RevertToSeam and MaterializeMain go
//     further and mutate the FILESYSTEM inside the same critical section. No
//     SQL transaction can span a working-copy write.
//
// Measured on the pre-fix code with two independent store.Open handles on one
// database file (which is exactly what two processes have) running 60
// record+commit cycles each: 57 of 120 files were missing from the final
// main_head tree. The losing writer read a head, built a tree from it, and then
// pointed main_head at a commit whose parent was already stale — orphaning the
// other writer's commit. It is a lost update, not a BUSY error, which is why
// the existing busy_timeout evidence did not cover it.
//
// # Why a file lock, and why flock specifically
//
// The package already decided that the unit of serialization is "the whole
// composite operation, per repo" — that is what lockRepo's sync.Mutex does at
// all 15 production call sites. V8 keeps that decision and only widens its
// scope from one process to the machine, so every call site is fixed at once
// rather than just the commit path.
//
// The lock is an flock(2) / LockFileEx advisory lock on a small file, NOT the
// atomic-create + PID-liveness scheme used by internal/lockfile. That scheme
// exists because a backend lockfile must survive its owner and record ADDRESS
// data, so a dead owner has to be detected and reclaimed. Here there is no
// payload and no reason to outlive the holder: the kernel drops an flock when
// the descriptor closes or the process dies, however it dies. Crash recovery is
// therefore automatic and needs no liveness heuristic — strictly less code and
// strictly fewer failure modes than reclaiming by PID, which cannot distinguish
// a crashed owner from a live one whose PID was recycled.
//
// internal/lockfile is also not importable here: internal/vcs is a port package
// and its GOV1 allowlist (archtest deps_test.go portAllowlists) admits only
// auth, execpolicy, guard, secrets and store.
//
// # Blocking, not timed out
//
// Acquisition blocks. A timeout would make the cross-process lane behave
// differently from the in-process lane it extends (sync.Mutex has no timeout
// either), so a long-but-legitimate materialize in one process would surface as
// a spurious failure in another. Deadlock is not a risk: the only nesting in
// this package is InitRepo's init-key -> repoID order, and no repoID holder
// ever takes an init-key, so there is no cycle to close.

package vcs

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// SetLockDir points the cross-process repo write lane at dir instead of the
// per-user cache directory. It exists for tests that need an isolated lock
// namespace, and must be called before any lane is acquired — lock files are
// opened once and retained, so a later change would leave already-open lanes
// pointing at the old directory.
//
// Two VCS instances only exclude each other when their lock dirs agree, so
// production must leave this unset and let defaultLockDir() decide.
func (v *VCS) SetLockDir(dir string) {
	v.repoLocksMu.Lock()
	defer v.repoLocksMu.Unlock()
	v.lockDir = dir
}

// lockRepo locks the write lane for key and returns its unlock function.
// repoID is the normal key; InitRepo uses "init:"+canonicalRoot before an id
// exists. Lock entries intentionally live for the VCS lifetime (repo count is
// bounded by the process's opened projects; deleting them would create an ABA
// race with goroutines that already retained the mutex pointer).
//
// The lane has TWO layers, taken in this order and released in reverse:
//
//  1. the in-process sync.Mutex, excluding this process's own goroutines;
//  2. an advisory file lock, excluding OTHER yanshi processes (V8).
//
// Taking the process mutex first is deliberate: it means at most one goroutine
// per lane ever waits on the file lock, so the kernel-level queue stays one
// entry deep per process and the cheap lock absorbs the common contention.
//
// A failure to acquire the file lock is NOT fatal. Every call site treats
// lockRepo as infallible (it returns only an unlock func), and the alternative
// — failing the write — would turn an unwritable cache directory into a total
// VCS outage. Degrading to the in-process lane preserves exactly the guarantee
// this package had before V8, so the failure mode is "no worse than before"
// rather than "broken". It is reported on stderr because silently losing
// cross-process exclusion is precisely the class of defect V8 exists to fix,
// and a lost commit later would otherwise have no trace pointing here.
func (v *VCS) lockRepo(key string) func() {
	v.maybeSweep()
	v.repoLocksMu.Lock()
	if v.repoLocks == nil {
		v.repoLocks = make(map[string]*sync.Mutex)
	}
	mu := v.repoLocks[key]
	if mu == nil {
		mu = &sync.Mutex{}
		v.repoLocks[key] = mu
	}
	lockFile, fileErr := v.lockFileFor(key)
	v.repoLocksMu.Unlock()
	mu.Lock()

	if fileErr != nil {
		fmt.Fprintf(os.Stderr,
			"yanshi: vcs cross-process lock unavailable (%v); "+
				"writes are serialized within this process only\n", fileErr)
		return mu.Unlock
	}
	if err := lockFileExclusive(lockFile); err != nil {
		fmt.Fprintf(os.Stderr,
			"yanshi: vcs cross-process lock not acquired (%v); "+
				"writes are serialized within this process only\n", err)
		return mu.Unlock
	}
	// Prove this lane is alive so the stale sweep leaves it alone. Done while
	// the lock is HELD, so a sweeper can never observe a fresh mtime on a file
	// it is about to reclaim without also failing to lock it.
	touchLockFile(lockFile.Name())
	return func() {
		// Release the cross-process lock BEFORE the in-process one. The reverse
		// order would let a local goroutine enter the lane and start writing
		// while another process, already woken by the flock release, is doing
		// the same — reintroducing the interleaving this lane exists to prevent.
		if err := unlockFile(lockFile); err != nil {
			fmt.Fprintf(os.Stderr, "yanshi: vcs lock release failed: %v\n", err)
		}
		mu.Unlock()
	}
}

// vcsLockDirName is the per-user directory holding repo write-lane lock files.
const vcsLockDirName = "vcs-locks"

// defaultLockDir returns the per-user directory for repo lock files. It must
// resolve to the same location in every process on the machine, or two yanshi
// instances would take two different locks and serialize against nothing.
//
// os.UserCacheDir is the same base internal/lockfile uses for backend
// discovery. When it is unavailable (no HOME on a stripped-down container) the
// fallback is os.TempDir, which is still machine-shared and therefore still
// gives real mutual exclusion; only its persistence across reboots is weaker,
// and these files carry no state worth persisting.
func defaultLockDir() string {
	base, err := os.UserCacheDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "yanshi", vcsLockDirName)
	}
	return filepath.Join(base, "yanshi", vcsLockDirName)
}

// lockFilePath maps a lock lane key to its on-disk lock file.
//
// The key is either a repoID or "init:"+canonicalRoot. Both are agreed on by
// every process reading the same database (repo ids are stored rows; canonical
// roots are derived from the same path), so hashing is only about producing a
// safe filename — a raw key would carry path separators, drive colons and
// arbitrary length. SHA-256 keeps it fixed-width and collision-free in
// practice, and unlike a character-substitution scheme it cannot map two
// distinct repos onto one file (which would over-serialize) or one repo onto
// two (which would under-serialize).
func (v *VCS) lockFilePath(key string) string {
	dir := v.lockDir
	if dir == "" {
		dir = defaultLockDir()
	}
	sum := sha256.Sum256([]byte(key))
	return filepath.Join(dir, hex.EncodeToString(sum[:])+".lock")
}

// lockFileFor returns the retained descriptor for key's lock file, opening it
// on first use. Descriptors are kept for the VCS lifetime for the same reason
// lockRepo keeps its mutexes: closing one while another goroutine still holds
// the pointer would be an ABA race, and on POSIX closing ANY descriptor for a
// file can drop locks held on it.
//
// The lock file is never unlinked WHILE IN USE. Removing an advisory lock file
// out from under a live holder creates two lock domains — the deleted inode and
// the freshly created one are different locks, and both "holders" would proceed
// at once. Reclaiming abandoned files is therefore not done here but in
// sweepStaleLockFiles, which only unlinks a file it has itself locked; see
// there for why that is sufficient.
//
// Callers must hold repoLocksMu.
func (v *VCS) lockFileFor(key string) (*os.File, error) {
	if v.repoFiles == nil {
		v.repoFiles = make(map[string]*os.File)
	}
	if f := v.repoFiles[key]; f != nil {
		return f, nil
	}
	path := v.lockFilePath(key)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("vcs: create lock dir: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("vcs: open repo lock %s: %w", path, err)
	}
	v.repoFiles[key] = f
	return f, nil
}

// Close releases every lock-file descriptor this VCS opened and removes the
// files it can prove nobody else is using.
//
// # Why this exists
//
// Each distinct repo id (and each "init:"+root key) creates one lock file that
// used to live forever. That is invisible on a workstation with three projects
// and ruinous for anything that opens many short-lived repositories: measured
// on this repository's own test suite, ONE `go test ./internal/vcs` run left
// 313 zero-byte files in the user's cache directory, and the accumulated total
// there was 27,968. Nothing ever removed them, so the count only went up. A
// user running goal loops over generated worktrees hits the same growth, more
// slowly.
//
// # Why it is safe to unlink here and not in lockFileFor
//
// The danger with unlinking an advisory lock file is two lock domains: if a
// live holder keeps the old inode open while a newcomer creates a fresh file at
// the same path, the two lock different inodes and both proceed. sweep avoids
// that by only removing a file whose lock it currently HOLDS and which is still
// the same inode as the path it is about to unlink. A holder in another process
// would fail that first condition, so the file stays.
//
// Close is idempotent and never fails the caller: a lock directory that cannot
// be tidied is a hygiene problem, not a correctness one, and returning an error
// would push callers toward ignoring it. Errors are aggregated into the
// returned value for callers that do care, and nil is returned when the sweep
// found nothing to complain about.
func (v *VCS) Close() error {
	v.repoLocksMu.Lock()
	files := v.repoFiles
	v.repoFiles = nil
	v.repoLocksMu.Unlock()

	var firstErr error
	for _, f := range files {
		name := f.Name()
		// Reclaim only if free: a descriptor still held by a goroutine in this
		// process, or by another process, must keep its file.
		if got, err := tryLockFileExclusive(f); err == nil && got {
			if err := removeIfSameInode(f, name); err != nil && firstErr == nil {
				firstErr = err
			}
			_ = unlockFile(f)
		}
		if err := f.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// removeIfSameInode unlinks name only when it still refers to the very file the
// caller holds open and locked.
//
// The comparison is what makes the unlink safe against the two-domain problem
// described on Close: between this process opening the file and deciding to
// remove it, another process may have removed the original and created a new
// file at the same path. That new file is a different inode which this process
// does not hold a lock on, and unlinking it would delete a lock some other
// process is actively relying on.
func removeIfSameInode(f *os.File, name string) error {
	openInfo, err := f.Stat()
	if err != nil {
		return nil // cannot compare; leaving the file is always safe
	}
	pathInfo, err := os.Stat(name)
	if err != nil {
		return nil // already gone, or unreadable: nothing to do
	}
	if !os.SameFile(openInfo, pathInfo) {
		return nil
	}
	if err := os.Remove(name); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("vcs: remove stale repo lock %s: %w", name, err)
	}
	return nil
}

// sweepStaleLockFiles removes lock files in dir that no process currently
// holds and that have not been touched for at least maxAge.
//
// It runs opportunistically from lockRepo (once per VCS, in the background) so
// files abandoned by processes that crashed — or by any build of yanshi that
// predates Close — are eventually reclaimed rather than accumulating forever.
// Close alone cannot do that: it only knows about the files THIS instance
// opened, and the 27,968 files already sitting in a user's cache belong to
// processes that exited long ago.
//
// The age floor matters. A lock file that is free right now may belong to a
// process that is between two operations and will lock it again in a
// microsecond; unlinking it then would split the lane into two domains for that
// process's next acquisition. Requiring the file to be untouched for maxAge
// makes that window astronomically unlikely without needing any coordination,
// and the mtime is refreshed by touchLockFile on every acquisition precisely so
// that an actively used lane keeps proving it is alive.
func sweepStaleLockFiles(dir string, maxAge time.Duration) (removed int) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	cutoff := time.Now().Add(-maxAge)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".lock") {
			continue
		}
		info, err := e.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		path := filepath.Join(dir, e.Name())
		f, err := os.OpenFile(path, os.O_RDWR, 0o600)
		if err != nil {
			continue
		}
		if got, lerr := tryLockFileExclusive(f); lerr == nil && got {
			if removeIfSameInode(f, path) == nil {
				removed++
			}
			_ = unlockFile(f)
		}
		_ = f.Close()
	}
	return removed
}

// staleLockMaxAge is how long a lock file must sit untouched before the sweep
// will reclaim it. A day is far longer than any single VCS operation and far
// shorter than "never", which is what the previous behaviour amounted to.
const staleLockMaxAge = 24 * time.Hour

// maybeSweep runs the stale-lock sweep at most once per VCS instance.
//
// Once per instance rather than on a timer: the accumulation this fixes is
// slow, so a single pass per process is enough to keep the directory bounded.
//
// It runs SYNCHRONOUSLY. A background goroutine is the obvious choice and is
// wrong for the same measured reason it was wrong in internal/lockfile: a
// short-lived invocation (`yanshi exec`, `yanshi vcs-mcp`) can exit before the
// goroutine is ever scheduled, so the sweep that looks correct in review simply
// never happens. The first caller is InitRepo during app assembly, not an
// interactive turn, and the cost falls off a cliff after one pass — 1.9 s
// measured on the 28,721-file directory that motivated this, and microseconds
// on the bounded directory it leaves behind.
func (v *VCS) maybeSweep() {
	v.sweepOnce.Do(func() {
		v.repoLocksMu.Lock()
		dir := v.lockDir
		v.repoLocksMu.Unlock()
		if dir == "" {
			dir = defaultLockDir()
		}
		sweepStaleLockFiles(dir, staleLockMaxAge)
	})
}

// touchLockFile refreshes a lock file's mtime so the stale sweep can tell an
// actively used lane from an abandoned one. Failures are ignored: a lane whose
// mtime cannot be updated is at worst swept a day later and recreated on the
// next acquisition.
func touchLockFile(path string) {
	now := time.Now()
	_ = os.Chtimes(path, now, now)
}
