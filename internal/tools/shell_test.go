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
)

// TestShell_Run: basic happy path — `go version` returns go version string.
func TestShell_Run(t *testing.T) {
	sh := NewShellTools(".")
	ctx := WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"shell_*"}},
		Shell: guard.ShellPerm{Policy: "allowlist", Patterns: []string{"go *"}},
	})
	out, err := runTool(ctx, sh.Run, `{"command":"go version"}`)
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
	ch := sh.Run.Stream(ctx, `{"command":"echo streaming-line"}`)
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
	out, err := runTool(ctx, sh.Run, `{"command":"echo plain"}`)
	require.NoError(t, err)
	assert.Contains(t, out, "plain")
}

// TestShell_RunMetacharRejected: commands with metacharacters (&&, |, etc.)
// are rejected by the guard even when the allowlist is permissive.
func TestShell_RunMetacharRejected(t *testing.T) {
	sh := NewShellTools(".")
	ctx := WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"shell_*"}},
		Shell: guard.ShellPerm{Policy: "allowlist", Patterns: []string{"*"}},
	})
	out, err := runTool(ctx, sh.Run, `{"command":"go version && echo pwned"}`)
	require.NoError(t, err, "permission denial must surface as a result, not a Go error")
	assert.Contains(t, out, "✗")
	assert.Contains(t, out, "permission denied")
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
	out, err := runTool(ctx, sh.Run, fmt.Sprintf(`{"command":%q,"timeout":1,"env":%q}`, cmd, env))
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
	out, err := runTool(ctx, sh.Run, `{"command":"go nonexistent"}`)
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
	out, _ := runTool(ctx, sh.Run, `{"command":"ls "}`)
	if !strings.Contains(out, "permission denied") {
		t.Fatalf("safe-list command must still go through Authorize; got %q", out)
	}
}

// TestShellRunLegacyUsesSecureProcessFactoryWhenBound proves that legacy
// shell_run still works but goes through the SecureProcessFactory when bound
// in context (Task 21). Previously it always used shellCommand + exec.Cmd
// directly.
func TestShellRunLegacyUsesSecureProcessFactoryWhenBound(t *testing.T) {
	sh := NewShellTools(t.TempDir())
	spy := &spySecureFactory{}
	ctx := WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"shell_run"}},
		Shell: guard.ShellPerm{Policy: "allowlist", Patterns: []string{"echo *"}},
	})
	ctx = WithSecureProcessFactory(ctx, spy)
	_, _ = runTool(ctx, sh.Run, `{"command":"echo hi"}`)
	if spy.calls != 1 {
		t.Fatalf("legacy shell_run must call SecureProcessFactory, got %d", spy.calls)
	}
}

type spySecureFactory struct{ calls int }

func (s *spySecureFactory) Start(_ context.Context, spec secproc.SecureProcessSpec) (*secproc.StartedProcess, error) {
	s.calls++
	return &secproc.StartedProcess{PID: 1234}, nil
}
