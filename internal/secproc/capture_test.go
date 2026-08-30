package secproc_test

import (
	"bytes"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/x6nux/yanshi/internal/secproc"
)

// TestWaitDrained_NilWaitFailsClosed proves the W-C-12 review's m-2(a)
// finding is fixed: a Factory that hands back a StartedProcess with no
// reaper (Wait == nil) must not be dereferenced — that would panic the
// whole process on model-controlled input (an auth.command spawn resolved
// from provider config) — it must fail closed with ErrNoReaper instead.
// Before this test nothing in the tree drove this path at all: the shared
// nil-check landed in WaitDrained as part of the B-1 fix, but no caller's
// test — internal/tools/secproc_capture_test.go included — ever constructed
// a StartedProcess with a nil Wait to exercise it.
func TestWaitDrained_NilWaitFailsClosed(t *testing.T) {
	started := &secproc.StartedProcess{
		Stdout: strings.NewReader(""),
		Stderr: strings.NewReader(""),
	}
	waitErr, drainErr := secproc.WaitDrained(started, io.Discard, io.Discard)
	if !errors.Is(drainErr, secproc.ErrNoReaper) {
		t.Fatalf("drainErr = %v, want ErrNoReaper", drainErr)
	}
	if waitErr != nil {
		t.Fatalf("waitErr = %v, want nil — Wait was never callable", waitErr)
	}
}

// TestWaitDrained_DrainsBeforeWait proves the ordering documented on
// WaitDrained's own doc comment is load-bearing, not aspirational: draining
// must start BEFORE started.Wait() is called. A real *exec.Cmd closes its
// StdoutPipe/StderrPipe once Wait reaps the process — that is the stdlib's
// own documented behavior ("Wait will close the pipe after seeing the
// command exit... it is thus incorrect to call Wait before all reads from
// the pipe have completed"). This test's Wait stub reproduces exactly that
// close-on-reap behavior against a real os.Pipe, so it captures the full
// payload under the current drain-then-wait implementation. It would go red
// under a wait-then-drain reordering: the pipe's read end would already be
// closed by the time drain() started reading, losing the payload entirely
// and surfacing a non-nil drainErr instead of the bytes asserted here.
// Before this test, only caller packages (internal/llm/eino's cmdauth,
// internal/tools' secproc_capture) exercised this ordering indirectly
// through their own fakes — internal/secproc's own suite never proved its
// one load-bearing function actually holds the order its comment promises.
func TestWaitDrained_DrainsBeforeWait(t *testing.T) {
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	const payload = "the child already wrote this before anyone called Wait"
	childExited := make(chan struct{})
	go func() {
		io.WriteString(pw, payload)
		pw.Close()
		close(childExited)
	}()

	started := &secproc.StartedProcess{
		Stdout: pr,
		Stderr: strings.NewReader(""),
		Wait: func() error {
			<-childExited
			// Reproduces (*exec.Cmd).Wait's documented behavior: it closes
			// the pipe once the process is reaped. Calling this before the
			// pipe has been drained is the exact bug this ordering test
			// guards against — a wait-then-drain WaitDrained would call this
			// closure, and thus pr.Close(), before drain() ever reads a byte.
			pr.Close()
			return nil
		},
	}

	var stdout bytes.Buffer
	waitErr, drainErr := secproc.WaitDrained(started, &stdout, io.Discard)
	if drainErr != nil {
		t.Fatalf("drainErr = %v, want nil — draining must finish before Wait closes the pipe", drainErr)
	}
	if waitErr != nil {
		t.Fatalf("waitErr = %v, want nil", waitErr)
	}
	if stdout.String() != payload {
		t.Fatalf("stdout = %q, want %q — drain-then-wait must capture everything the child wrote before Wait tore the pipe down", stdout.String(), payload)
	}
}

// TestBoundedCapture_TruncatesButReportsTrueTotal proves the W-C-12 review's
// m-2(b) finding: a capture sink used to read an untrusted child's
// stdout/stderr (an auth.command helper is arbitrary operator-configured
// argv) must not grow without bound in memory. BoundedCapture keeps only the
// tail up to its limit while Snapshot's total/truncated results still
// reflect the real size, so a caller can tell "here is the whole output" from
// "here is the last N bytes of a much longer one".
func TestBoundedCapture_TruncatesButReportsTrueTotal(t *testing.T) {
	const limit = 16
	bc := secproc.NewBoundedCapture(limit)
	if _, err := bc.Write([]byte("0123456789")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if _, err := bc.Write([]byte("abcdefghij")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	text, total, truncated := bc.Snapshot()
	const wantText = "456789abcdefghij" // last 16 bytes of "0123456789abcdefghij"
	if text != wantText {
		t.Fatalf("Snapshot text = %q, want %q", text, wantText)
	}
	if total != 20 {
		t.Fatalf("Snapshot total = %d, want 20 (the true byte count, not the retained one)", total)
	}
	if !truncated {
		t.Fatalf("Snapshot truncated = false, want true — 20 written bytes exceed the 16-byte limit")
	}
}
