package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/x6nux/yanshi/internal/secproc"
	"github.com/x6nux/yanshi/internal/shell"
)

// tracingGitFactory is the factory bootstrap actually installs
// (shell.DefaultSecureFactory over shell.OSProcessFactory) with GIT_TRACE=1
// forced into the child environment. GIT_TRACE makes a perfectly successful
// git write several lines to stderr while exiting 0 and leaving stdout
// byte-for-byte correct — the exact shape of every real-world git warning
// ("warning: LF will be replaced by CRLF", "fatal: cannot exec <fsmonitor
// hook>", advice.* notices) that the porcelain parsers must never see.
//
// Nothing about this test double touches the transport under test: the spawn,
// the pipes and the stream routing are all production code.
type tracingGitFactory struct{}

func (tracingGitFactory) Start(ctx context.Context, spec secproc.SecureProcessSpec) (*secproc.StartedProcess, error) {
	spec.Env = append(append([]string(nil), spec.Env...), "GIT_TRACE=1")
	return shell.DefaultSecureFactory{OS: shell.OSProcessFactory{}}.Start(ctx, spec)
}

var _ secproc.Factory = tracingGitFactory{}

// TestGitStatusIsNotPollutedByGitStderr is the regression test for stderr
// being merged into stdout on the production spawn path.
//
// OSProcessFactory used to hand the Manager a single io.MultiReader(stdout,
// stderr) and DefaultSecureFactory used to report StartedProcess.Stderr as a
// discardReader, so commandResult.Stderr was ALWAYS empty in production and
// every byte git wrote to stderr was appended to commandResult.Stdout. The
// porcelain-v2 -z parser splits that buffer on NUL, so the trailing stderr
// blob became one extra "entry" whose XY and path are words sliced out of a
// diagnostic message — a file that does not exist, reported to the model as a
// modified path.
//
// Asserting on the entry COUNT (not just on the absence of a substring) is
// what makes this test fail for the right reason: a merged stream always
// produces at least one pseudo-entry here.
func TestGitStatusIsNotPollutedByGitStderr(t *testing.T) {
	root := initTempGitRepo(t)
	if err := os.WriteFile(filepath.Join(root, "only.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := WithWorkRoot(secureTestContext(t, tracingGitFactory{}), root)
	out, err := runTool(ctx, NewGitTools().Status, `{}`)
	if err != nil {
		t.Fatal(err)
	}
	var res struct {
		Entries []struct {
			Path string `json:"path"`
			XY   string `json:"xy"`
		} `json:"entries"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("unmarshal %q: %v", out, err)
	}
	if len(res.Entries) != 1 {
		t.Fatalf("git_status returned %d entries, want exactly 1 (the untracked only.txt); "+
			"extra entries are stderr text parsed as porcelain records: %s", len(res.Entries), out)
	}
	if res.Entries[0].Path != "only.txt" || res.Entries[0].XY != "??" {
		t.Fatalf("entry = %+v, want {Path:only.txt XY:??}", res.Entries[0])
	}
	if strings.Contains(out, "trace:") {
		t.Fatalf("git stderr leaked into the git_status result: %s", out)
	}
}

// TestSecureCaptureKeepsStderrOffStdout pins the transport contract the parser
// fix rests on: on the production factory, stdout and stderr arrive in their
// own commandResult fields. Without this, commandFailureTail's documented
// "stderr wins when present" rule is a dead branch in production — every
// failure message reaches it as stdout instead.
func TestSecureCaptureKeepsStderrOffStdout(t *testing.T) {
	root := initTempGitRepo(t)
	ctx := WithWorkRoot(secureTestContext(t, tracingGitFactory{}), root)
	res, err := secureCommandRunner(ctx, secproc.SecureProcessSpec{
		Tool: "git_status", Program: "git", Dir: root,
		Args: []string{"status", "--porcelain=v2", "-z"},
		Env:  gitEnvIsolation(root),
	}, 10*time.Second)
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	if !strings.Contains(res.Stderr, "trace:") {
		t.Fatalf("Stderr = %q, want git's trace output (production Stderr must not be a discard sink)", res.Stderr)
	}
	if strings.Contains(res.Stdout, "trace:") {
		t.Fatalf("Stdout = %q, want it free of stderr text", res.Stdout)
	}
}
