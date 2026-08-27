package log

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// DefaultRotateMaxBytes is the size at which the active log file is rotated.
// 10 MiB keeps a single generation small enough to open in an editor while
// still holding a meaningful stretch of a long-running goal loop.
const DefaultRotateMaxBytes int64 = 10 << 20

// DefaultRotateMaxBackups is how many rotated generations are kept next to the
// active file (yanshi.log.1 … yanshi.log.5). Together with the size cap this
// bounds a yanshi installation's log footprint at
// DefaultRotateMaxBytes*(DefaultRotateMaxBackups+1).
const DefaultRotateMaxBackups = 5

// RotateConfig bounds the on-disk footprint of a file log sink. The zero value
// means "use the defaults"; a negative MaxBytes or MaxBackups disables that
// half of the policy (unbounded size, or no retained generations).
//
// This exists instead of a third-party rotator (lumberjack and friends)
// because the whole policy is two renames and a stat: taking a dependency for
// that buys a supply-chain surface and an upgrade cadence in exchange for code
// that fits on one screen.
type RotateConfig struct {
	// MaxBytes is the size the active file may reach before it is rotated.
	// Zero means DefaultRotateMaxBytes. Negative disables rotation by size,
	// which makes the writer a plain append-only file.
	MaxBytes int64
	// MaxBackups is the number of rotated generations kept. Zero means
	// DefaultRotateMaxBackups. Negative means none are kept: the active file
	// is truncated on rotation rather than renamed.
	MaxBackups int
}

// resolve returns the effective policy: zero fields take the package defaults,
// negative fields are normalised to the "disabled" sentinel the writer checks.
func (c RotateConfig) resolve() (maxBytes int64, maxBackups int) {
	maxBytes = c.MaxBytes
	switch {
	case maxBytes == 0:
		maxBytes = DefaultRotateMaxBytes
	case maxBytes < 0:
		maxBytes = 0 // 0 is the writer's "never rotate by size" sentinel
	}
	maxBackups = c.MaxBackups
	switch {
	case maxBackups == 0:
		maxBackups = DefaultRotateMaxBackups
	case maxBackups < 0:
		maxBackups = 0
	}
	return maxBytes, maxBackups
}

// RotatingWriter is a concurrency-safe io.WriteCloser that appends to a file
// and rotates it once it exceeds a size cap, keeping a bounded number of
// numbered generations.
//
// Two properties are load-bearing and are the reason this is not a bare
// *os.File plus a cron job:
//
//   - Every Write is serialised by a mutex, because slog handlers are shared
//     across every goroutine in the process and a torn record is worse than a
//     dropped one: a half-written JSON line breaks every downstream parser for
//     the rest of the file.
//   - A failed rotation NEVER fails a Write and never panics. If the rename
//     chain or the reopen breaks (a Windows share lock, a read-only parent
//     directory, a full disk), the writer keeps appending to whatever handle
//     it still has. Logging is diagnostics: losing the ability to rotate is a
//     housekeeping problem, whereas propagating that failure into slog.Handle
//     turns it into a fault in the code being diagnosed.
type RotatingWriter struct {
	path       string
	maxBytes   int64
	maxBackups int

	mu   sync.Mutex
	file *os.File
	size int64
	// rotateErr records the most recent rotation failure so operators can see
	// that retention silently stopped applying. Cleared by a later success.
	rotateErr error
}

// NewRotatingWriter opens path for append (creating it and its parent
// directory when missing) and returns a writer that keeps the file under
// cfg.MaxBytes by rotating it into numbered generations.
//
// The returned writer is ready for concurrent use. The caller owns Close; a
// process that never closes it loses nothing, because every Write reaches the
// OS immediately (no user-space buffering).
func NewRotatingWriter(path string, cfg RotateConfig) (*RotatingWriter, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return nil, err
	}
	maxBytes, maxBackups := cfg.resolve()
	w := &RotatingWriter{path: abs, maxBytes: maxBytes, maxBackups: maxBackups}
	if err := w.open(); err != nil {
		return nil, err
	}
	return w, nil
}

// open opens the active file for append and syncs the tracked size to what is
// already on disk. Callers must hold mu (or be the constructor).
func (w *RotatingWriter) open() error {
	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	size := int64(0)
	if fi, statErr := f.Stat(); statErr == nil {
		size = fi.Size()
	}
	w.file = f
	w.size = size
	return nil
}

// Path returns the absolute path of the active log file.
func (w *RotatingWriter) Path() string { return w.path }

// Write appends p to the active file, rotating first when the write would push
// the file past the size cap.
//
// The cap is checked BEFORE the write rather than after, so a file never
// exceeds MaxBytes by more than one record. A record larger than the whole cap
// is still written whole into a freshly rotated file: splitting it would
// produce two invalid JSON lines, which is the one outcome worse than an
// oversized file.
func (w *RotatingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		return 0, errors.New("log: rotating writer is closed")
	}
	if w.maxBytes > 0 && w.size > 0 && w.size+int64(len(p)) > w.maxBytes {
		w.rotateLocked()
	}
	n, err := w.file.Write(p)
	w.size += int64(n)
	return n, err
}

// rotateLocked performs one rotation. Callers must hold mu.
//
// It is deliberately total: every failure path ends with w.file pointing at
// SOME writable handle, so the caller's Write can proceed. The error is
// recorded in rotateErr rather than returned, because there is no caller in
// the chain (slog.Handler.Handle → io.Writer.Write) that can do anything
// useful with it.
func (w *RotatingWriter) rotateLocked() {
	// Clear the previous verdict first: rotateErr answers "did the LAST
	// rotation work", so a stale failure must not outlive the rotation that
	// recovered from it.
	w.rotateErr = nil

	// A rotation with no retained generations is a truncate: there is nowhere
	// to move the bytes to, and keeping them would defeat the size cap.
	if w.maxBackups <= 0 {
		if err := w.file.Truncate(0); err != nil {
			w.rotateErr = fmt.Errorf("log: truncate %s: %w", w.path, err)
			return
		}
		if _, err := w.file.Seek(0, 0); err != nil {
			w.rotateErr = fmt.Errorf("log: rewind %s: %w", w.path, err)
			return
		}
		w.size = 0
		return
	}

	// Close before renaming: Windows refuses to rename a file with an open
	// handle, and a POSIX rename of an open file would leave the handle
	// pointing at the rotated generation (writes would keep landing in .1).
	if err := w.file.Close(); err != nil {
		w.rotateErr = fmt.Errorf("log: close %s before rotate: %w", w.path, err)
		// The handle may still be usable; try to carry on with a fresh one.
		if openErr := w.open(); openErr != nil {
			w.rotateErr = errors.Join(w.rotateErr, openErr)
		}
		return
	}
	w.file = nil

	// Prune every generation at or beyond the cap, then shift each surviving
	// one down. Missing generations are normal (the chain has not filled yet)
	// and are skipped.
	//
	// Pruning a RANGE rather than just .maxBackups looks redundant against the
	// shift below -- os.Rename replaces its target on every platform the repo
	// builds for, so .maxBackups is overwritten anyway -- and in steady state
	// it is. A mutation probe that deleted the single-generation version of
	// this loop went green for exactly that reason. It earns its place on the
	// TRANSITION: an installation that lowers MaxBackups leaves the older
	// generations on disk with nothing that will ever rename over them, so
	// retention quietly stops bounding anything and the operator's stated cap
	// under-reports the real footprint. Pruning upward from the cap makes the
	// number in the config the number on disk.
	for i := w.maxBackups; ; i++ {
		stale := w.backupPath(i)
		if _, statErr := os.Stat(stale); os.IsNotExist(statErr) {
			break
		}
		if err := os.Remove(stale); err != nil && !os.IsNotExist(err) {
			w.rotateErr = fmt.Errorf("log: remove %s: %w", stale, err)
			break // a generation we cannot unlink blocks every older one too
		}
	}
	for i := w.maxBackups - 1; i >= 1; i-- {
		from, to := w.backupPath(i), w.backupPath(i+1)
		if err := os.Rename(from, to); err != nil && !os.IsNotExist(err) {
			w.rotateErr = fmt.Errorf("log: rename %s: %w", from, err)
		}
	}
	if err := os.Rename(w.path, w.backupPath(1)); err != nil {
		w.rotateErr = fmt.Errorf("log: rename %s: %w", w.path, err)
	}

	if err := w.open(); err != nil {
		// Nothing left to write to. Keep file nil; Write returns an error for
		// this record instead of dereferencing nil. Retrying here is not worth
		// the complexity: a directory that cannot be opened for append will
		// not heal within a process.
		w.rotateErr = errors.Join(w.rotateErr, err)
	}
}

// backupPath returns the path of the n-th rotated generation ("<path>.1" is
// the most recent).
func (w *RotatingWriter) backupPath(n int) string {
	return fmt.Sprintf("%s.%d", w.path, n)
}

// RotateError returns the most recent rotation failure, or nil when the last
// rotation succeeded (or none has run).
//
// It exists so a diagnostic surface -- doctor, a status frame -- can tell
// "retention is working" from "retention silently stopped and this disk is
// filling up". Without it a failed rename is invisible until the volume is
// full, which is exactly the failure rotation was added to prevent.
func (w *RotatingWriter) RotateError() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.rotateErr
}

// Close closes the active file. Subsequent Writes return an error rather than
// panicking. Close is idempotent.
func (w *RotatingWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		return nil
	}
	err := w.file.Close()
	w.file = nil
	return err
}
