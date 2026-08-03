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
func Acquire(root string, lf Lockfile) (bool, error) {
	p, err := Path(root)
	if err != nil {
		return false, err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return false, err
	}
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
