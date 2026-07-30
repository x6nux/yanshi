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

func runSecureCapture(ctx context.Context, spec secproc.SecureProcessSpec, timeout time.Duration) (commandResult, error) {
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	started, err := LaunchSecureProcess(runCtx, spec)
	if err != nil {
		return commandResult{}, err
	}
	stdout := &boundedCapture{limit: secureCaptureLimit}
	stderr := &boundedCapture{limit: secureCaptureLimit}
	stdoutDone := drain(stdout, started.Stdout)
	stderrDone := drain(stderr, started.Stderr)
	waitErr := started.Cmd.Wait()
	stdoutErr, stderrErr := <-stdoutDone, <-stderrDone
	if stdoutErr != nil {
		return commandResult{}, stdoutErr
	}
	if stderrErr != nil {
		return commandResult{}, stderrErr
	}
	if err := runCtx.Err(); err != nil {
		return commandResult{}, err
	}
	exitCode := 0
	if waitErr != nil {
		var exitErr *exec.ExitError
		if !errors.As(waitErr, &exitErr) {
			return commandResult{}, waitErr
		}
		exitCode = exitErr.ExitCode()
	}
	stdoutText, stdoutBytes, stdoutTruncated := stdout.snapshot()
	stderrText, stderrBytes, stderrTruncated := stderr.snapshot()
	return commandResult{
		Stdout: stdoutText, Stderr: stderrText, ExitCode: exitCode,
		StdoutTruncated: stdoutTruncated, StderrTruncated: stderrTruncated,
		StdoutBytes: stdoutBytes, StderrBytes: stderrBytes,
	}, nil
}
