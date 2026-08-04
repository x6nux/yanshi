package tools

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os/exec"
	"sync"
	"time"

	"github.com/x6nux/yanshi/internal/secproc"
)

const secureCaptureLimit = 1 << 20 // 1 MiB

type commandResult struct {
	Stdout          string `json:"stdout"`
	Stderr          string `json:"stderr"`
	ExitCode        int    `json:"exit_code"`
	StdoutTruncated bool   `json:"stdout_truncated"`
	StderrTruncated bool   `json:"stderr_truncated"`
	StdoutBytes     int64  `json:"stdout_original_bytes"`
	StderrBytes     int64  `json:"stderr_original_bytes"`
}

type commandRunner func(context.Context, secproc.SecureProcessSpec, time.Duration) (commandResult, error)

var secureCommandRunner commandRunner = runSecureCapture

type boundedCapture struct {
	mu    sync.Mutex
	buf   bytes.Buffer
	limit int
	total int64
}

func (w *boundedCapture) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := len(p)
	w.total += int64(n)
	w.buf.Write(p)
	// Drain is complete (every Write accepted); only the retained window is
	// bounded. Drop from the front so the TAIL (recent errors/summaries) wins.
	if w.buf.Len() > w.limit {
		w.buf.Next(w.buf.Len() - w.limit)
	}
	return n, nil
}

func (w *boundedCapture) snapshot() (string, int64, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String(), w.total, w.total > int64(w.limit)
}

func drain(dst io.Writer, src io.Reader) <-chan error {
	done := make(chan error, 1)
	go func() { _, err := io.Copy(dst, src); done <- err }()
	return done
}

// exitCodeFromWaitErr translates the error returned by a process reaper into
// an exit code. A clean exit and a non-zero exit are both NORMAL outcomes the
// caller must report to the model; only a non-ExitError (pipe failure, context
// kill, a Factory-level error) is a real failure and comes back as err.
//
// Shared by every reaping call site (runSecureCapture, shell_run's factory
// path and shell_run's legacy pipe path) so "non-zero exit is not an error"
// is decided in exactly one place.
func exitCodeFromWaitErr(waitErr error) (int, error) {
	if waitErr == nil {
		return 0, nil
	}
	var exitErr *exec.ExitError
	if errors.As(waitErr, &exitErr) {
		return exitErr.ExitCode(), nil
	}
	return 0, waitErr
}

// waitExitCode reaps the process through wait and returns its exit code.
// Callers MUST invoke this (or Wait directly) exactly once per started
// process: skipping it leaves a zombie behind on every launch.
func waitExitCode(wait func() error) (int, error) {
	return exitCodeFromWaitErr(wait())
}

func runSecureCapture(ctx context.Context, spec secproc.SecureProcessSpec, timeout time.Duration) (commandResult, error) {
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	started, err := LaunchSecureProcess(runCtx, spec)
	if err != nil {
		return commandResult{}, err
	}
	// Fail closed on a Factory that forgot the reaper. Dereferencing it would
	// panic the whole agent process, and every tool on this path (git_status /
	// git_diff / run_tests / diagnostics / github_*) is model-reachable.
	if started.Wait == nil {
		return commandResult{}, errors.New("tools: Factory returned a process with no reaper (fail-closed)")
	}
	stdout := &boundedCapture{limit: secureCaptureLimit}
	stderr := &boundedCapture{limit: secureCaptureLimit}
	stdoutDone := drain(stdout, started.Stdout)
	stderrDone := drain(stderr, started.Stderr)
	// Drain stdout/stderr to EOF BEFORE calling Wait. StdoutPipe/StderrPipe
	// are closed by (*exec.Cmd).Wait once the process exits, so reading from
	// them after Wait returns "read |0: file already closed". On fast CI
	// runners (ubuntu) the git process exits — and Wait closes the pipe —
	// before io.Copy has drained it, silently dropping the output (the test
	// sees an empty result instead of the staged/unstaged/untracked files).
	// Drain-to-EOF first; the goroutines unblock when the process closes its
	// stdout/stderr (on exit or on context-cancel kill), then Wait reaps it.
	stdoutErr := <-stdoutDone
	stderrErr := <-stderrDone
	waitErr := started.Wait()
	if stdoutErr != nil && stdoutErr != io.EOF {
		return commandResult{}, stdoutErr
	}
	if stderrErr != nil && stderrErr != io.EOF {
		return commandResult{}, stderrErr
	}
	if err := runCtx.Err(); err != nil {
		return commandResult{}, err
	}
	exitCode, err := exitCodeFromWaitErr(waitErr)
	if err != nil {
		return commandResult{}, err
	}
	stdoutText, stdoutBytes, stdoutTruncated := stdout.snapshot()
	stderrText, stderrBytes, stderrTruncated := stderr.snapshot()
	return commandResult{
		Stdout: stdoutText, Stderr: stderrText, ExitCode: exitCode,
		StdoutTruncated: stdoutTruncated, StderrTruncated: stderrTruncated,
		StdoutBytes: stdoutBytes, StderrBytes: stderrBytes,
	}, nil
}
