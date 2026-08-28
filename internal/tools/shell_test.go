package tools

import (
	"context"
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/x6nux/yanshi/internal/guard"
	"github.com/x6nux/yanshi/internal/secproc"
	"github.com/x6nux/yanshi/internal/shell"
)

// spawnCtx is the context every shell_run test needs after W-B-02: the tool has
// exactly one launch path (secproc.Launch) and that path fails closed when no
// Factory is bound. Before W-B-02 these tests exercised the direct-pipe
// fallback instead, which is precisely the second spawn implementation the work
// package deleted — so binding the real unsandboxed factory here is not test
// scaffolding, it is the tests finally running the code production runs.
func spawnCtx(ctx context.Context) context.Context {
	return WithSecureProcessFactory(ctx, shell.UnsandboxedSecureFactory())
}

// TestShell_Run: basic happy path — `go version` returns go version string.
func TestShell_Run(t *testing.T) {
	sh := NewShellTools(".")
	ctx := WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"shell_*"}},
		Shell: guard.ShellPerm{Policy: "allowlist", Patterns: []string{"go *"}},
	})
	out, err := runTool(spawnCtx(ctx), sh.Run, `{"command":"go version"}`)
	require.NoError(t, err)
	assert.Contains(t, out, "go version go1")
}

// TestShell_StreamsProgress proves shell_run streams each stdout line via the
// Stream channel's Text chunks (TUI) while the full output also reaches the
// model via Result.
func TestShell_StreamsProgress(t *testing.T) {
	sh := NewShellTools(".")
	ctx := WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"shell_*"}},
		Shell: guard.ShellPerm{Policy: "allowlist", Patterns: []string{"echo *"}},
	})
	ch := sh.Run.Stream(spawnCtx(ctx), `{"command":"echo streaming-line"}`)
	var lines []string
	var result string
	for c := range ch {
		if c.Text != "" {
			lines = append(lines, strings.TrimRight(c.Text, "\n"))
		}
		result += c.Result
	}
	assert.Contains(t, result, "streaming-line", "full output reaches the model via Result")
	assert.Contains(t, lines, "streaming-line", "the line was streamed via Text chunks")
}

// TestShell_NoProgressWithoutCallback proves the legacy path (no callback bound)
// still works and returns the full output.
func TestShell_NoProgressWithoutCallback(t *testing.T) {
	sh := NewShellTools(".")
	ctx := WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"shell_*"}},
		Shell: guard.ShellPerm{Policy: "allowlist", Patterns: []string{"echo *"}},
	})
	out, err := runTool(spawnCtx(ctx), sh.Run, `{"command":"echo plain"}`)
	require.NoError(t, err)
	assert.Contains(t, out, "plain")
}

// TestShell_RunUnreadableStructureRejected: a command whose structure the
// segmenter cannot read is refused by the guard even when the allowlist is
// `*`, and the refusal reaches the model as a result rather than a Go error.
//
// This used to assert the same thing about `go version && echo pwned`. INF1
// (ADR-0004 supplement) made that command judged segment by segment — with
// patterns `["*"]` both halves match, so it is now allowed, and
// TestShell_RunChainIsJudgedPerSegment below covers that direction. The half
// of the old defence that did NOT move is what is asserted here.
func TestShell_RunUnreadableStructureRejected(t *testing.T) {
	sh := NewShellTools(".")
	ctx := WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"shell_*"}},
		Shell: guard.ShellPerm{Policy: "allowlist", Patterns: []string{"*"}},
	})
	out, err := runTool(spawnCtx(ctx), sh.Run, `{"command":"go version $(echo pwned)"}`)
	require.NoError(t, err, "permission denial must surface as a result, not a Go error")
	assert.Contains(t, out, "✗")
	assert.Contains(t, out, "permission denied")
}

// TestShell_RunChainIsJudgedPerSegment is the other side of the same change,
// end to end through the tool rather than through guard.Check: a chain whose
// every segment is allowlisted now RUNS, and one with an off-list segment does
// not. Both halves matter — the first is the friction INF1 removed, the second
// is the reason removing it is not a blanket amnesty for chained commands.
func TestShell_RunChainIsJudgedPerSegment(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("echo semantics differ on windows")
	}
	sh := NewShellTools(".")
	ctx := WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"shell_*"}},
		Shell: guard.ShellPerm{Policy: "allowlist", Patterns: []string{"echo *"}},
	})
	out, err := runTool(spawnCtx(ctx), sh.Run, `{"command":"echo one && echo two"}`)
	require.NoError(t, err)
	assert.Contains(t, out, "one")
	assert.Contains(t, out, "two")

	out, err = runTool(spawnCtx(ctx), sh.Run, `{"command":"echo first && ls -la"}`)
	require.NoError(t, err, "permission denial must surface as a result, not a Go error")
	assert.Contains(t, out, "permission denied")
	assert.NotContains(t, out, "first",
		"the allowed segment must not run when the chain is refused — the chain is one spawn, "+
			"so a partial execution is not merely undesirable, it is impossible; asserting it "+
			"guards against a future 'run the allowed prefix' shortcut")
}

// TestShell_RunNotOnAllowlist: an allowed-by-metachar but not-on-allowlist
// command is rejected. Skipped on Windows due to platform differences.
func TestShell_RunNotOnAllowlist(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("echo semantics differ on windows")
	}
	sh := NewShellTools(".")
	ctx := WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"shell_*"}},
		Shell: guard.ShellPerm{Policy: "allowlist", Patterns: []string{"go *"}},
	})
	out, err := runTool(ctx, sh.Run, `{"command":"ls -la"}`)
	require.NoError(t, err, "permission denial must surface as a result, not a Go error")
	assert.Contains(t, out, "✗")
	assert.Contains(t, out, "permission denied")
}

// TestShell_RunTimeout: a command that exceeds the specified timeout is
// killed and returns an error, not a hang.
func TestShell_RunTimeout(t *testing.T) {
	sh := NewShellTools(".")
	var cmd, pattern, env string
	if runtime.GOOS == "windows" {
		// Use powershell directly so killing the shell also kills the command:
		// the default "auto" shell wraps the command in `cmd /c`, which spawns
		// the target as a child process. TerminateProcess on cmd.exe orphans
		// the child, which keeps the stdout pipe open long after the timeout
		// fires — so the ✗ error chunk never lands and the test sees only the
		// child's output. powershell.exe runs Start-Sleep in-process, so the
		// kill is immediate and the pipe EOFs at the timeout boundary.
		env = "powershell"
		cmd = "Start-Sleep -Seconds 10"
		pattern = "Start-Sleep*"
	} else {
		env = "auto"
		cmd = "sleep 5"
		pattern = "sleep*"
	}
	ctx := WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"shell_*"}},
		Shell: guard.ShellPerm{Policy: "allowlist", Patterns: []string{pattern}},
	})
	start := time.Now()
	out, err := runTool(spawnCtx(ctx), sh.Run, fmt.Sprintf(`{"command":%q,"timeout":1,"env":%q}`, cmd, env))
	elapsed := time.Since(start)
	require.NoError(t, err, "timeout must surface as a result, not a Go error")
	assert.Contains(t, out, "✗")
	// Must return well before the full sleep/ping duration.
	assert.Less(t, elapsed, 20*time.Second, "should not hang for full sleep/ping duration")
}

// TestShell_RunDeniedNoProfile: without a bound profile, the tool fails
// closed — surfaced as a permission-denied result (not a Go error).
func TestShell_RunDeniedNoProfile(t *testing.T) {
	sh := NewShellTools(".")
	out, err := runTool(context.Background(), sh.Run, `{"command":"go version"}`)
	require.NoError(t, err, "permission denial must surface as a result, not a Go error")
	assert.Contains(t, out, "✗")
	assert.Contains(t, out, "permission denied")
}

// TestShell_RunNonZeroExit: a command that exits non-zero returns the
// exit code in the JSON output, NOT as a Go error.
func TestShell_RunNonZeroExit(t *testing.T) {
	sh := NewShellTools(".")
	ctx := WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"shell_*"}},
		Shell: guard.ShellPerm{Policy: "allowlist", Patterns: []string{"go *"}},
	})
	out, err := runTool(spawnCtx(ctx), sh.Run, `{"command":"go nonexistent"}`)
	require.NoError(t, err)
	assert.Contains(t, out, "exit 2")
	assert.Contains(t, out, "unknown command")
}

// TestShellRun_SafeListNoLongerBypassesAuthorize (Task 3): "ls" is in
// safeShellCommands. With a profile that DENIES shell entirely, the tool MUST
// still deny — proving the safe-list no longer bypasses Authorize.
func TestShellRun_SafeListNoLongerBypassesAuthorize(t *testing.T) {
	sh := NewShellTools(t.TempDir())
	ctx := WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"shell_run"}},
		Shell: guard.ShellPerm{Policy: "deny"},
	})
	out, _ := runTool(spawnCtx(ctx), sh.Run, `{"command":"ls "}`)
	if !strings.Contains(out, "permission denied") {
		t.Fatalf("safe-list command must still go through Authorize; got %q", out)
	}
}

// TestShellRunAlwaysGoesThroughTheSecureLauncher is W-B-02's acceptance for
// shell_run, and it needs both halves to mean anything.
//
// The first half (a bound factory is used) held before W-B-02 too. The second
// half is the new one: with NO factory bound, the tool must fail closed rather
// than spawn the command itself. That fallback existed, it ran a raw
// exec.Cmd pipe with no credential scrub and no sandbox seam, and deleting it
// is the whole work item — so a regression that restored it would keep the
// first half green and only fail here.
func TestShellRunAlwaysGoesThroughTheSecureLauncher(t *testing.T) {
	base := WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"shell_run"}},
		Shell: guard.ShellPerm{Policy: "allowlist", Patterns: []string{"echo *"}},
	})

	t.Run("bound factory is the launcher", func(t *testing.T) {
		sh := NewShellTools(t.TempDir())
		spy := &spySecureFactory{}
		_, _ = runTool(WithSecureProcessFactory(base, spy), sh.Run, `{"command":"echo hi"}`)
		if spy.calls != 1 {
			t.Fatalf("shell_run must launch through the bound SecureProcessFactory, got %d calls", spy.calls)
		}
	})

	t.Run("no factory fails closed", func(t *testing.T) {
		sh := NewShellTools(t.TempDir())
		out, err := runTool(base, sh.Run, `{"command":"echo hi"}`)
		if err != nil {
			t.Fatalf("the refusal must reach the model as a result: %v", err)
		}
		if !strings.Contains(out, "no Factory in context") {
			t.Fatalf("shell_run spawned without a launcher; result = %q", out)
		}
		if strings.Contains(out, "hi") {
			t.Fatalf("the command actually ran without a launcher; result = %q", out)
		}
	})
}

// TestShellRunPassesTheToolRootAsTheDestructiveBoundary pins the one field the
// spec's `Workdir` plumbing exists for. The model controls shell_run's
// "workdir" argument; if that value reached guard.Action.Workdir, a call could
// declare `/` as its own project root and every deletion would grade as
// in-scope. The boundary must be the root the tool was CONSTRUCTED with.
func TestShellRunPassesTheToolRootAsTheDestructiveBoundary(t *testing.T) {
	root := t.TempDir()
	sh := NewShellTools(root)
	var seen secproc.SecureProcessSpec
	factory := factoryFunc(func(_ context.Context, spec secproc.SecureProcessSpec) (*secproc.StartedProcess, error) {
		seen = spec
		return &secproc.StartedProcess{PID: 1, Wait: func() error { return nil }}, nil
	})
	ctx := WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"shell_run"}},
		Shell: guard.ShellPerm{Policy: "allowlist", Patterns: []string{"echo *"}},
	})
	_, _ = runTool(WithSecureProcessFactory(ctx, factory), sh.Run, `{"command":"echo hi","workdir":"/"}`)
	if seen.Workdir != root {
		t.Errorf("spec.Workdir = %q, want the tool root %q — a model-supplied workdir must not "+
			"move the destructive dimension's boundary", seen.Workdir, root)
	}
	if seen.Dir != "/" {
		t.Errorf("spec.Dir = %q, want the caller-supplied \"/\" — the two fields are separate on purpose", seen.Dir)
	}
	if seen.ArgsJSON == "" {
		t.Error("spec.ArgsJSON is empty; the approval dialog would show the operator nothing")
	}
}

// factoryFunc adapts a func to secproc.Factory for tests that only need to
// capture the spec.
type factoryFunc func(context.Context, secproc.SecureProcessSpec) (*secproc.StartedProcess, error)

func (f factoryFunc) Start(ctx context.Context, spec secproc.SecureProcessSpec) (*secproc.StartedProcess, error) {
	return f(ctx, spec)
}

type spySecureFactory struct{ calls int }

func (s *spySecureFactory) Start(_ context.Context, spec secproc.SecureProcessSpec) (*secproc.StartedProcess, error) {
	s.calls++
	return &secproc.StartedProcess{PID: 1234}, nil
}

// TestChainedCommandCannotBeInteractivelyApproved pins the second, quieter half
// of INF1's blast radius — and it is the half that keeps the first half small.
//
// ADR-0004's supplement names the cost of per-segment judging: a chain whose
// worst segment is a Prompt is no longer a structural HardDeny, so in principle
// YOLO could run it. In practice it cannot, because Authorize's escalation path
// begins by building an approval Scope and scopeFromAction refuses a command
// with more than one executable segment (a single approval rule cannot honestly
// cover two programs). Every escalating chain therefore dies there, before the
// permission callback is consulted at all.
//
// That is worth a test rather than a comment because it is load-bearing in the
// direction nobody would notice breaking: teaching scopeFromAction to summarise
// a chain would silently open the interactive path to exactly the commands the
// metacharacter HardDeny used to refuse.
func TestChainedCommandCannotBeInteractivelyApproved(t *testing.T) {
	profile := guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"shell_run"}},
		Shell: guard.ShellPerm{Policy: "allowlist", Patterns: []string{"echo *"}},
	}
	asked := 0
	ctx := WithPermissionCallback(WithProfile(context.Background(), profile),
		func(PermissionRequest) PermissionDecision {
			asked++
			return PermissionAllow
		})

	// Control: an UNCHAINED off-list command does reach the callback and is
	// approved. Without this the assertion below could pass because the whole
	// callback wiring is dead.
	if err := Authorize(ctx, guard.Action{Tool: "shell_run", Shell: "ls -la"}, "{}"); err != nil {
		t.Fatalf("precondition: a single off-list command must be approvable: %v", err)
	}
	if asked != 1 {
		t.Fatalf("precondition: callback consulted %d times, want 1", asked)
	}

	asked = 0
	err := Authorize(ctx, guard.Action{Tool: "shell_run", Shell: "echo hi && ls -la"}, "{}")
	if err == nil {
		t.Fatal("a chained command reached an approval; one rule cannot cover two programs")
	}
	if !strings.Contains(err.Error(), "one executable segment") {
		t.Errorf("denial = %v; want the approval-scope refusal", err)
	}
	if asked != 0 {
		t.Errorf("the callback was consulted %d times for a chained command; the escalation "+
			"path must close before the operator is asked", asked)
	}
}
