package tools

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/x6nux/yanshi/internal/secproc"
	"github.com/x6nux/yanshi/internal/shell"
)

// prodFactory mirrors the factory bootstrap.Build actually installs
// (bootstrap.go: shell.DefaultSecureFactory{OS: shell.OSProcessFactory{}} →
// orchConfig.SecureFactory → orchestrator → tools.WithSecureProcessFactory).
// Policy/Sandbox stay nil because their absence is not what this test pins.
func prodFactory() secproc.Factory {
	return shell.DefaultSecureFactory{OS: shell.OSProcessFactory{}}
}

// helperSpec re-execs the test binary into TestSecureCaptureHelperProcess so
// the spawn is portable across windows/darwin/linux (no /bin/sh, no echo.exe).
func helperSpec(t *testing.T, stdout string, exitCode int) secproc.SecureProcessSpec {
	t.Helper()
	outPath := t.TempDir() + string(os.PathSeparator) + "out"
	if err := os.WriteFile(outPath, []byte(stdout), 0o644); err != nil {
		t.Fatal(err)
	}
	return secproc.SecureProcessSpec{
		Tool:    "run_tests",
		Program: os.Args[0],
		Args:    []string{"-test.run=TestSecureCaptureHelperProcess", "--"},
		Env: append(os.Environ(),
			"YANSHI_CAPTURE_HELPER=1",
			"YANSHI_CAPTURE_STDOUT_FILE="+outPath,
			"YANSHI_CAPTURE_EXIT="+strconv.Itoa(exitCode),
			"YANSHI_CAPTURE_BLOCK=false",
		),
	}
}

// TestRunSecureCaptureWithProductionFactory pins the reaper contract on the
// factory bootstrap actually ships. Every other Factory in this package is a
// test double that supplies its own reaper, so a production factory that
// returned an unreapable StartedProcess stayed invisible to the whole suite
// while git_status / git_diff / run_tests / diagnostics / github_* — all eight
// of them registered in bootstrap and model-visible — crashed on first call.
//
// The non-zero subtest is the load-bearing half: shell.Process.Wait documents
// that it MUST surface *exec.ExitError so runSecureCapture can read ExitCode
// off it. A reaper that swallowed the exit status would report every failing
// command as a success.
func TestRunSecureCaptureWithProductionFactory(t *testing.T) {
	for _, tc := range []struct {
		name     string
		stdout   string
		exitCode int
	}{
		{"clean exit", "prod-ok", 0},
		{"non-zero exit", "prod-fail", 7},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := secureTestContext(t, prodFactory())
			got, err := runSecureCapture(ctx, helperSpec(t, tc.stdout, tc.exitCode), 30*time.Second)
			if err != nil {
				t.Fatalf("runSecureCapture: %v", err)
			}
			if got.ExitCode != tc.exitCode {
				t.Fatalf("ExitCode = %d, want %d", got.ExitCode, tc.exitCode)
			}
			// The helper writes the payload to stdout, and the production
			// factory keeps the two streams apart, so it lands on Stdout —
			// not "wherever the merge happened to put it" — for both exit
			// codes.
			if !strings.Contains(got.Stdout, tc.stdout) {
				t.Fatalf("Stdout = %q, want it to contain %q", got.Stdout, tc.stdout)
			}
		})
	}
}

// TestRunSecureCaptureFailsClosedWithoutReaper proves the missing-reaper case
// degrades to an error instead of a nil-deref panic. A panic here takes down
// the whole agent process, so this path must stay a plain error even if some
// future Factory forgets to populate it.
func TestRunSecureCaptureFailsClosedWithoutReaper(t *testing.T) {
	ctx := secureTestContext(t, reaperlessFactory{})
	_, err := runSecureCapture(ctx, secproc.SecureProcessSpec{Tool: "git_status", Program: "git"}, time.Second)
	if err == nil || !strings.Contains(err.Error(), "reaper") {
		t.Fatalf("err = %v, want a fail-closed reaper error", err)
	}
}

// reaperlessFactory returns a StartedProcess with no Wait — the exact shape
// DefaultSecureFactory used to return.
type reaperlessFactory struct{}

func (reaperlessFactory) Start(_ context.Context, _ secproc.SecureProcessSpec) (*secproc.StartedProcess, error) {
	return &secproc.StartedProcess{PID: 4242, Stdout: strings.NewReader(""), Stderr: strings.NewReader("")}, nil
}

var _ secproc.Factory = reaperlessFactory{}
