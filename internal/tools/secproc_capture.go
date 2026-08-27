package tools

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/x6nux/yanshi/internal/sandbox"
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

// commandFailureTail extracts the most informative slice of a failed
// subprocess's output. Capped so a huge log cannot flood the model's context —
// run_tests already spills the full output to an artifact.
//
// It reports BOTH streams when both carry text, stderr first. "stderr wins,
// stdout as fallback" is the intuitive rule and it is wrong often enough to
// matter, because which stream holds the reason is per-tool and sometimes
// per-invocation:
//
//   - git and gh do put the reason on stderr ("fatal: not a git repository",
//     "gh: not authenticated"), and stdout is empty or irrelevant.
//   - `go test -json` puts even COMPILE ERRORS on stdout, as build-output
//     events; on Go 1.26 stderr is 0 bytes for a package that fails to build.
//     What does reach stderr is progress chatter ("go: downloading …"). Under
//     a strict stderr-wins rule a build failure was reported to the model as
//     "go: downloading github.com/google/uuid v1.6.0" — a true statement that
//     answers nothing, with the actual "undefined: foo" sitting unused in
//     stdout.
//
// Emitting both costs at most one extra line and removes the whole class of
// "the tool told the model the wrong reason". Each stream keeps half the
// budget so neither can crowd the other out.
//
// Shared by every tool that turns a non-zero exit into a reported failure
// (run_tests, github_*, git_status/git_diff) so the answer to "what did the
// command actually complain about" is assembled in exactly one place.
func commandFailureTail(res commandResult) string {
	const maxTail = 1024
	errText := strings.TrimSpace(res.Stderr)
	outText := strings.TrimSpace(res.Stdout)
	switch {
	case errText == "" && outText == "":
		return "(no output)"
	case outText == "":
		return tailOf(errText, maxTail)
	case errText == "":
		return tailOf(outText, maxTail)
	}
	return "stderr: " + tailOf(errText, maxTail/2) + "\nstdout: " + tailOf(outText, maxTail/2)
}

// tailOf returns the last max bytes of s, marked with a leading ellipsis when
// it truncated. The tail (not the head) is kept because runners print the
// summary of what went wrong last.
func tailOf(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return "…" + s[len(s)-max:]
}

// runSecureCapture is the capture path every non-streaming tool spawns
// through (git_status, git_diff, run_tests, ast_search, diagnostics,
// github_*), and since S12 it is also the single wiring point for the
// sandbox-violation escalation loop.
//
// Putting the loop HERE rather than in each tool is what makes it real for all
// of them at once: a tool that forgot to opt in would report a sandbox refusal
// to the model as a bare non-zero exit, which is precisely the state S12
// exists to end. The one-shot spawn is also the only shape where a retry is
// clean — nothing has been emitted to the model or the TUI yet, so re-running
// at a higher tier produces one result rather than two interleaved ones.
//
// When the escalation retries, the returned commandResult is the RETRY's. When
// it does not, the original result is returned with the explanation appended
// to Stderr, so a caller that renders failures (commandFailureTail) tells the
// model why the command was refused without every call site having to learn
// about escalations.
func runSecureCapture(ctx context.Context, spec secproc.SecureProcessSpec, timeout time.Duration) (commandResult, error) {
	var res commandResult
	command := spec.Shell
	if command == "" {
		command = strings.TrimSpace(spec.Program + " " + strings.Join(spec.Args, " "))
	}
	_, esc, err := EscalateOnSandboxViolation(ctx, spec.Tool, command, spec.UseSandboxTier,
		func(runCtx context.Context, tier sandbox.AccessTier) (SandboxAttempt, error) {
			attemptSpec := spec
			attemptSpec.UseSandboxTier = tier
			out, err := runSecureCaptureOnce(runCtx, attemptSpec, timeout)
			if err != nil {
				return SandboxAttempt{}, err
			}
			res = out
			return SandboxAttempt{ExitCode: out.ExitCode, Stdout: out.Stdout, Stderr: out.Stderr}, nil
		})
	if err != nil {
		return commandResult{}, err
	}
	if esc.Explanation != "" {
		res.Stderr = appendExplanation(res.Stderr, esc.Explanation)
	}
	return res, nil
}

// appendExplanation puts the escalation's model-facing text on the end of a
// stream. Separate function because two paths need identical framing and a
// silently different separator would make the two transports' transcripts
// diverge for no reason.
func appendExplanation(stream, explanation string) string {
	if strings.TrimSpace(stream) == "" {
		return explanation
	}
	return stream + "\n" + explanation
}

// runSecureCaptureOnce is one spawn: launch, drain both streams to EOF, reap,
// report. It holds no policy of its own — everything about tiers, refusals and
// retries lives in runSecureCapture above.
func runSecureCaptureOnce(ctx context.Context, spec secproc.SecureProcessSpec, timeout time.Duration) (commandResult, error) {
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
