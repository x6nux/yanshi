package tools

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/x6nux/yanshi/internal/guard"
	"github.com/x6nux/yanshi/internal/sandbox"
	"github.com/x6nux/yanshi/internal/secproc"
)

func allowAllProfile() guard.PermissionProfile {
	return guard.PermissionProfile{
		FS:    guard.FSPerm{Read: []string{"**"}, Write: []string{"**"}},
		Tools: guard.ToolsPerm{Allow: []string{"*"}},
		Shell: guard.ShellPerm{Policy: "allowlist", Patterns: []string{"*"}},
		Net:   guard.NetPerm{Allow: true, Hosts: []string{"*"}},
	}
}

// cannedResult is startCannedHelper's input: exact stdout/stderr text,
// exit code, and optionally block forever (cancellation tests).
type cannedResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
	Block    bool
}

// scriptedFactory starts the helper subprocess with canned output written to
// temp files (avoids Windows env size limits). resultFn maps a spec to the
// output. Thread-safe; lastSpec() records the most-recent Start call. This is
// the ONE shared fake Factory for Task 2/3/4/6 — do not redefine it.
type scriptedFactory struct {
	t        *testing.T
	dir      string
	seq      atomic.Int64
	resultFn func(secproc.SecureProcessSpec) cannedResult
	mu       sync.Mutex
	last     secproc.SecureProcessSpec
}

func newScriptedFactory(t *testing.T, resultFn func(secproc.SecureProcessSpec) cannedResult) *scriptedFactory {
	return &scriptedFactory{t: t, dir: t.TempDir(), resultFn: resultFn}
}

func (f *scriptedFactory) Start(ctx context.Context, spec secproc.SecureProcessSpec) (*secproc.StartedProcess, error) {
	f.mu.Lock()
	f.last = spec
	res := f.resultFn(spec)
	f.mu.Unlock()
	id := f.seq.Add(1)
	outPath := filepath.Join(f.dir, fmt.Sprintf("out-%d", id))
	errPath := filepath.Join(f.dir, fmt.Sprintf("err-%d", id))
	if err := os.WriteFile(outPath, []byte(res.Stdout), 0o644); err != nil {
		return nil, err
	}
	if err := os.WriteFile(errPath, []byte(res.Stderr), 0o644); err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=TestSecureCaptureHelperProcess", "--")
	cmd.Env = append(os.Environ(),
		"YANSHI_CAPTURE_HELPER=1",
		"YANSHI_CAPTURE_STDOUT_FILE="+outPath,
		"YANSHI_CAPTURE_STDERR_FILE="+errPath,
		"YANSHI_CAPTURE_EXIT="+strconv.Itoa(res.ExitCode),
		"YANSHI_CAPTURE_BLOCK="+strconv.FormatBool(res.Block),
	)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &secproc.StartedProcess{Wait: cmd.Wait, PID: cmd.Process.Pid, Stdout: stdout, Stderr: stderr}, nil
}

func (f *scriptedFactory) lastSpec() secproc.SecureProcessSpec {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.last
}

// startCannedHelper returns a StartedProcess that emits one fixed cannedResult.
// Convenience wrapper for tests that need only a single output.
func startCannedHelper(ctx context.Context, t *testing.T, res cannedResult) (*secproc.StartedProcess, error) {
	return newScriptedFactory(t, func(secproc.SecureProcessSpec) cannedResult { return res }).Start(ctx, secproc.SecureProcessSpec{})
}

// TestSecureCaptureHelperProcess is the re-exec'd helper: it copies the temp
// files referenced by env to stdout/stderr, then exits. When Block is set it
// parks until the ctx-cancelled CommandContext kills it.
func TestSecureCaptureHelperProcess(t *testing.T) {
	if os.Getenv("YANSHI_CAPTURE_HELPER") != "1" {
		return
	}
	if os.Getenv("YANSHI_CAPTURE_BLOCK") == "true" {
		select {}
	}
	if p := os.Getenv("YANSHI_CAPTURE_STDOUT_FILE"); p != "" {
		if b, err := os.ReadFile(p); err == nil {
			_, _ = os.Stdout.Write(b)
		}
	}
	if p := os.Getenv("YANSHI_CAPTURE_STDERR_FILE"); p != "" {
		if b, err := os.ReadFile(p); err == nil {
			_, _ = os.Stderr.Write(b)
		}
	}
	code, _ := strconv.Atoi(os.Getenv("YANSHI_CAPTURE_EXIT"))
	os.Exit(code)
}

func secureTestContext(t *testing.T, factory secproc.Factory) context.Context {
	return WithSecureProcessFactory(WithProfile(context.Background(), allowAllProfile()), factory)
}

func TestRunSecureCapturePreservesQualifiedSpecAndExit(t *testing.T) {
	factory := newScriptedFactory(t, func(secproc.SecureProcessSpec) cannedResult {
		return cannedResult{Stdout: "oo", Stderr: "eee", ExitCode: 7}
	})
	spec := secproc.SecureProcessSpec{
		Tool: "run_tests", Program: "cargo", Args: []string{"test", "name with space"},
		Dir: t.TempDir(), Env: []string{"LANG=C"}, UseSandboxTier: sandbox.WorkspaceWrite,
	}
	got, err := runSecureCapture(secureTestContext(t, factory), spec, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if got.Stdout != "oo" || got.Stderr != "eee" || got.ExitCode != 7 {
		t.Fatalf("got=%+v", got)
	}
	last := factory.lastSpec()
	if last.Program != "cargo" || last.Args[1] != "name with space" || last.Env[0] != "LANG=C" {
		t.Fatalf("spec=%+v", last)
	}
}

func TestRunSecureCaptureCancellationStopsWait(t *testing.T) {
	factory := newScriptedFactory(t, func(secproc.SecureProcessSpec) cannedResult { return cannedResult{Block: true} })
	ctx, cancel := context.WithCancel(secureTestContext(t, factory))
	cancel()
	_, err := runSecureCapture(ctx, secproc.SecureProcessSpec{Tool: "run_tests", Program: "go"}, time.Second)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v", err)
	}
}

func TestRunSecureCaptureFailsClosedWithoutFactory(t *testing.T) {
	ctx := WithProfile(context.Background(), allowAllProfile())
	_, err := runSecureCapture(ctx, secproc.SecureProcessSpec{Tool: "git_status", Program: "git"}, time.Second)
	if err == nil || !strings.Contains(err.Error(), "Factory") {
		t.Fatalf("err=%v", err)
	}
}

func TestRunSecureCaptureDrainsAndReportsTruncation(t *testing.T) {
	factory := newScriptedFactory(t, func(secproc.SecureProcessSpec) cannedResult {
		return cannedResult{Stdout: strings.Repeat("o", secureCaptureLimit+123), Stderr: strings.Repeat("e", secureCaptureLimit+7)}
	})
	got, err := runSecureCapture(secureTestContext(t, factory), secproc.SecureProcessSpec{Tool: "run_tests", Program: "go"}, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !got.StdoutTruncated || !got.StderrTruncated {
		t.Fatalf("got=%+v", got)
	}
	if got.StdoutBytes != int64(secureCaptureLimit+123) || got.StderrBytes != int64(secureCaptureLimit+7) {
		t.Fatalf("bytes=%d/%d", got.StdoutBytes, got.StderrBytes)
	}
	if len(got.Stdout) != secureCaptureLimit || len(got.Stderr) != secureCaptureLimit {
		t.Fatalf("kept=%d/%d", len(got.Stdout), len(got.Stderr))
	}
}

var _ secproc.Factory = (*scriptedFactory)(nil)
