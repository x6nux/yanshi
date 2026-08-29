package secproc

import (
	"bytes"
	"errors"
	"io"
	"sync"
)

// CaptureLimit is the byte ceiling every bounded stdout/stderr capture keeps
// in memory for a spawned process's output. Shared by internal/tools (the
// run_tests/git_status/git_diff/diagnostics/github_* capture path) and
// internal/llm/eino (the auth.command token/error text) so "how much of a
// wayward subprocess's output do we retain" is answered in exactly one
// place, instead of two packages picking their own number.
const CaptureLimit = 1 << 20 // 1 MiB

// BoundedCapture is an io.Writer that retains only the most recent Limit
// bytes written to it — the TAIL, not the head, because the informative part
// of a failing command's output is usually at the end — while still
// accepting and counting every byte, so Snapshot's total/truncated results
// reflect the true output size even when the retained text is shorter.
type BoundedCapture struct {
	mu    sync.Mutex
	buf   bytes.Buffer
	limit int
	total int64
}

// NewBoundedCapture returns a BoundedCapture retaining at most limit bytes.
func NewBoundedCapture(limit int) *BoundedCapture {
	return &BoundedCapture{limit: limit}
}

// Write implements io.Writer. It never fails: a capture sink has no downstream
// to propagate an I/O error to, so Write only ever returns (len(p), nil).
func (w *BoundedCapture) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := len(p)
	w.total += int64(n)
	w.buf.Write(p)
	// Drain is complete (every Write accepted); only the retained window is
	// bounded. Drop from the front so the TAIL wins.
	if w.buf.Len() > w.limit {
		w.buf.Next(w.buf.Len() - w.limit)
	}
	return n, nil
}

// Snapshot returns the retained text, the true total byte count written, and
// whether the total exceeded Limit (i.e. the retained text is a truncated
// tail rather than the whole stream).
func (w *BoundedCapture) Snapshot() (text string, total int64, truncated bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String(), w.total, w.total > int64(w.limit)
}

// drain copies src into dst on its own goroutine and reports the copy's
// terminal error (io.EOF on a clean stream close) on the returned channel.
func drain(dst io.Writer, src io.Reader) <-chan error {
	done := make(chan error, 1)
	go func() { _, err := io.Copy(dst, src); done <- err }()
	return done
}

// ErrNoReaper is WaitDrained's fail-closed answer when started.Wait is nil —
// a Factory that forgot to hand back a reaper. Dereferencing a nil Wait would
// panic the whole process, and every caller on this path is reachable from
// model-controlled input (a tool call, an auth.command spawn resolved from
// provider config), so this fails closed with an error instead of a crash.
var ErrNoReaper = errors.New("secproc: Factory returned a process with no reaper (fail-closed)")

// WaitDrained reaps started the one correct way: it drains BOTH stdout and
// stderr to EOF concurrently, in goroutines, BEFORE calling started.Wait().
//
// This ordering is load-bearing, not stylistic. (*exec.Cmd)'s StdoutPipe /
// StderrPipe are closed by Wait once the process is reaped, so reading from
// them AFTER Wait races the reaper closing the pipe out from under the
// reader — on a fast exit this can lose output entirely ("read |0: file
// already closed") rather than merely error loudly. Worse, a caller that
// drains one stream to EOF and only THEN starts draining the other (rather
// than both concurrently) can deadlock outright: the OS pipe buffer for the
// unread stream (~64KiB on macOS) fills, the child blocks trying to write to
// it, and the child therefore never reaches the exit that would let the
// first stream's read finish either. Both mistakes have shipped in this
// codebase under different names; this is the one place drain-then-wait is
// implemented, so every caller (internal/tools' capture path and
// internal/llm/eino's auth.command runner) calls this instead of
// re-implementing it a third time.
//
// stdout/stderr receive the process's two streams as they arrive — pass a
// *BoundedCapture (or any io.Writer) sized for the caller's needs.
//
// Returns waitErr, the reaped process error: nil on a clean exit, an
// *exec.ExitError on a non-zero exit — BOTH normal outcomes the caller
// decides how to report, not failures of WaitDrained itself. drainErr is
// non-nil only for a genuine stream I/O failure (or ErrNoReaper) distinct
// from a non-zero exit.
func WaitDrained(started *StartedProcess, stdout, stderr io.Writer) (waitErr, drainErr error) {
	if started.Wait == nil {
		return nil, ErrNoReaper
	}
	stdoutDone := drain(stdout, started.Stdout)
	stderrDone := drain(stderr, started.Stderr)
	stdoutErr := <-stdoutDone
	stderrErr := <-stderrDone
	waitErr = started.Wait()
	if stdoutErr != nil && stdoutErr != io.EOF {
		return waitErr, stdoutErr
	}
	if stderrErr != nil && stderrErr != io.EOF {
		return waitErr, stderrErr
	}
	return waitErr, nil
}
