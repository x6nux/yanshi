// Package lockfile manages the yanshi serve lockfile: a per-project file in
// the OS cache dir recording the PID + address of a running backend, so a CLI
// can discover (or bootstrap) the backend for the current project.
package lockfile

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Lockfile is the on-disk record of a running backend for one project root.
type Lockfile struct {
	PID       int       `json:"pid"`
	Addr      string    `json:"addr"` // e.g. "127.0.0.1:54321"
	Auth      string    `json:"auth"` // "none" (loopback) or "token"
	Root      string    `json:"root"` // absolute project root, for verification
	StartedAt time.Time `json:"started_at"`
	Version   int       `json:"version"`
}

// currentVersion is bumped whenever the lockfile schema changes incompatibly.
const currentVersion = 1

// Dir returns the lockfile directory: os.UserCacheDir()/yanshi/run.
func Dir() (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "yanshi", "run"), nil
}

// Path returns the lockfile path for the given absolute project root.
func Path(root string) (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, sanitize(root)+".lock"), nil
}

// sanitize turns an absolute path into a filename-safe key: every character
// that is not [A-Za-z0-9.-] becomes "_". This collapses path separators and
// the Windows drive colon into a stable, readable key (e.g.
// "D:\code\yanshi" -> "D__code_yanshi").
func sanitize(absPath string) string {
	var b strings.Builder
	b.Grow(len(absPath))
	for _, r := range absPath {
		switch {
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// ErrNotFound is returned by Read when no lockfile exists for the root.
var ErrNotFound = errors.New("lockfile: not found")

// Write writes the lockfile for root, creating the directory if needed. It
// stamps StartedAt (when zero) and Version (when zero) before writing.
func Write(root string, lf Lockfile) error {
	p, err := Path(root)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	if lf.StartedAt.IsZero() {
		lf.StartedAt = time.Now()
	}
	if lf.Version == 0 {
		lf.Version = currentVersion
	}
	data, err := json.MarshalIndent(lf, "", "  ")
	if err != nil {
		return err
	}
	// O_TRUNC|O_CREATE so a re-write by the same owner replaces cleanly.
	return os.WriteFile(p, data, 0o644)
}

// Read reads the lockfile for root. Returns ErrNotFound when absent.
func Read(root string) (Lockfile, error) {
	p, err := Path(root)
	if err != nil {
		return Lockfile{}, err
	}
	data, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return Lockfile{}, ErrNotFound
	}
	if err != nil {
		return Lockfile{}, err
	}
	var lf Lockfile
	if err := json.Unmarshal(data, &lf); err != nil {
		return Lockfile{}, err
	}
	return lf, nil
}

// Remove removes the lockfile for root. It is not an error if the file is
// already gone (idempotent), so callers can always defer it.
func Remove(root string) error {
	p, err := Path(root)
	if err != nil {
		return err
	}
	err = os.Remove(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// Acquire attempts to claim the lockfile for root atomically. It writes the
// lockfile with O_CREATE|O_EXCL so that, among concurrent callers, exactly one
// succeeds. Returns (true, nil) when this caller won; (false, nil) when another
// live owner already holds it; (false, err) on an unexpected error.
//
// If a lockfile exists but its PID is dead (stale), Acquire reclaims it by
// overwriting — this is how owner election recovers after a crashed owner.
//
// Acquire also sweeps the directory for lockfiles belonging to OTHER roots
// whose owners are long gone (see SweepStale). Per-root reclaim cannot reach
// those: a lockfile is only ever revisited by a process opening that same
// project, and a project that no longer exists is never opened again. Measured
// on a developer machine, 1,475 such files had accumulated, every one of them
// naming a temp directory that had been deleted months earlier. Sweeping here
// costs one ReadDir on a path already being written and needs no new caller.
func Acquire(root string, lf Lockfile) (bool, error) {
	p, err := Path(root)
	if err != nil {
		return false, err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return false, err
	}
	defer sweepOnce()
	if lf.StartedAt.IsZero() {
		lf.StartedAt = time.Now()
	}
	if lf.Version == 0 {
		lf.Version = currentVersion
	}
	data, err := json.Marshal(lf)
	if err != nil {
		return false, err
	}
	// Atomic claim: fail if the file already exists.
	f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err == nil {
		_, werr := f.Write(data)
		cerr := f.Close()
		if werr != nil {
			return false, werr
		}
		return true, cerr
	}
	if !errors.Is(err, os.ErrExist) {
		return false, err
	}
	// File exists: claim it only if the recorded owner is dead (stale reclaim).
	existing, rerr := Read(root)
	if rerr != nil {
		return false, rerr
	}
	if existing.Alive() {
		return false, nil // someone else owns it
	}
	// Stale: overwrite (not exclusive — we intentionally replace a dead owner).
	if werr := os.WriteFile(p, data, 0o644); werr != nil {
		return false, werr
	}
	return true, nil
}

// StaleMaxAge is how long a lockfile must be untouched before SweepStale will
// consider reclaiming it, on top of its owner being dead.
//
// The age floor is not redundant with the liveness check. PIDs are recycled,
// and a fresh lockfile whose recorded PID happens to be dead for a
// millisecond — between the owner's fork and its first write, say — must not be
// deleted by a passer-by. A day is far longer than that window and far shorter
// than the "never" the previous behaviour amounted to.
const StaleMaxAge = 24 * time.Hour

// SweepStale removes lockfiles in the run directory whose owner process is gone
// AND whose project root no longer exists, reporting how many it removed.
//
// Both conditions are required, and the second is the conservative one. A dead
// PID alone is the normal state of a machine that has been rebooted: the
// project is still there and its lockfile will be reclaimed, in place and with
// the right contents, the next time somebody opens it. Deleting it early gains
// nothing. The files that actually accumulate are the ones whose ROOT has been
// deleted — temp directories from test runs, checkouts that were removed — and
// nothing will ever revisit those. Requiring a vanished root therefore targets
// exactly the unreclaimable population and leaves every recoverable file alone.
//
// Unreadable and malformed entries are skipped rather than deleted: this
// directory is shared with whatever else lands in a user cache, and a sweep
// that removed things it could not parse would be a sweep that removes other
// programs' data.
func SweepStale(maxAge time.Duration) int {
	dir, err := Dir()
	if err != nil {
		return 0
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	cutoff := time.Now().Add(-maxAge)
	removed := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".lock") {
			continue
		}
		info, err := e.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		p := filepath.Join(dir, e.Name())
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var lf Lockfile
		if err := json.Unmarshal(data, &lf); err != nil {
			continue // not ours, or corrupt: leave it alone
		}
		if lf.Alive() {
			continue
		}
		if lf.Root == "" {
			continue // cannot judge recoverability; keep it
		}
		if _, err := os.Stat(lf.Root); err == nil {
			continue // the project still exists; this file is still useful
		}
		if err := os.Remove(p); err == nil {
			removed++
		}
	}
	return removed
}

// sweepOnceGuard bounds the automatic sweep to one pass per process.
var sweepOnceGuard sync.Once

// sweepOnce runs SweepStale at most once per process.
//
// It runs SYNCHRONOUSLY, which was not the first design and is worth the note.
// A background goroutine looked strictly better — ReadDir over a directory of
// thousands of entries should not sit in front of a backend starting up — but a
// real run showed it never happened at all: `yanshi exec` claims the lockfile,
// does its turn and exits, and the process tore down before the goroutine was
// scheduled. Measured, the run directory was unchanged after a real exec even
// though calling SweepStale directly removed 890 files from it. That is the
// house failure mode exactly: the code was written, reachable and provably
// correct in isolation, and had no effect in production.
//
// The synchronous cost is bounded and small — 249 ms measured on the 1,476-file
// directory that motivated this, once per process, off the interactive path
// (owner election happens while a backend is being assembled anyway) — and it
// shrinks every time it runs, because the population it walks is the one it
// just deleted.
func sweepOnce() {
	sweepOnceGuard.Do(func() { SweepStale(StaleMaxAge) })
}
