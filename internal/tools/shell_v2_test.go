package tools

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/x6nux/yanshi/internal/guard"
	"github.com/x6nux/yanshi/internal/shell"
)

// L2 (v3): the factory MUST return non-nil Process/Console. Manager.Start
// (Task 17) immediately calls proc.PID()/console.PTY() and spawns the pump
// goroutine — a nil process panics there. These fakes mirror the shape of
// Task 17's fakeProcess/fakeConsole (kept here because this test lives in
// package tools, not shell, so it cannot reference the shell-internal fakes).
type fakeShellProcess struct{}

func (fakeShellProcess) Wait() error { return nil }
func (fakeShellProcess) PID() int    { return 1 }
func (fakeShellProcess) Kill() error { return nil }
func (fakeShellProcess) Capabilities() shell.ProcessCapabilities {
	return shell.ProcessCapabilities{CanKillTree: false}
}

type fakeShellConsole struct{}

func (fakeShellConsole) Read([]byte) (int, error)    { return 0, io.EOF }
func (fakeShellConsole) Write(p []byte) (int, error) { return len(p), nil }
func (fakeShellConsole) Close() error                { return nil }
func (fakeShellConsole) Resize(uint16, uint16) error { return nil }
func (fakeShellConsole) PTY() bool                   { return false }

type fakeShellFactory struct{}

func (fakeShellFactory) Start(context.Context, shell.LaunchSpec) (shell.Process, shell.Console, error) {
	return fakeShellProcess{}, fakeShellConsole{}, nil
}

// recordingShellFactory captures the LaunchSpec the manager hands to the
// launcher so a test can assert on the effective working directory without
// spawning a real process.
type recordingShellFactory struct{ specs []shell.LaunchSpec }

func (f *recordingShellFactory) Start(_ context.Context, spec shell.LaunchSpec) (shell.Process, shell.Console, error) {
	f.specs = append(f.specs, spec)
	return fakeShellProcess{}, fakeShellConsole{}, nil
}

// TestShellV2StartFeedsWorkdirToGuard pins the destructive-deletion baseline.
// guard.checkDestructive classifies "rm -rf <abs path>" against Action.Workdir;
// when Workdir is empty it fails safe and treats every absolute path as
// out-of-scope, so a v2 shell that forgets Workdir degrades into prompting on
// every absolute deletion AND loses the "deleting the work root itself"
// judgement entirely. The PermissionRequest is the existing observation seam:
// it mirrors Action.Workdir verbatim (permctx.go). NOTE both WithProfile and
// WithPermissionCallback are required — Authorize returns DenyErr before ever
// consulting the callback when no profile is bound.
func TestShellV2StartFeedsWorkdirToGuard(t *testing.T) {
	root := t.TempDir()
	manager := shell.NewManager(shell.Config{Root: root, MaxOutputBytes: 256, Factory: fakeShellFactory{}})
	defer func() { _ = manager.Close() }()
	var got PermissionRequest
	ctx := WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"shell_start"}},
		// Empty policy + empty patterns => guard.Check returns Prompt, which is
		// what routes the action through the interactive callback.
		Shell: guard.ShellPerm{},
	})
	ctx = WithPermissionCallback(ctx, func(req PermissionRequest) PermissionDecision {
		got = req
		return PermissionDeny
	})
	ctx = WithShellManager(ctx, manager)
	v := NewShellV2Tools(root)
	_, _ = runTool(ctx, v.Start, `{"command":"rm -rf /tmp/xyz"}`)
	if got.Tool != "shell_start" {
		t.Fatalf("permission callback was not consulted: %+v", got)
	}
	if got.Workdir != root {
		t.Fatalf("guard action Workdir = %q, want %q", got.Workdir, root)
	}
}

// TestShellV2StartDefaultsDirToRoot mirrors legacy shell_run: an omitted
// "workdir" arg must run the process at the work root, not at the server's
// process cwd (which is arbitrary and outside the sandboxed tree).
func TestShellV2StartDefaultsDirToRoot(t *testing.T) {
	root := t.TempDir()
	factory := &recordingShellFactory{}
	manager := shell.NewManager(shell.Config{Root: root, MaxOutputBytes: 256, Factory: factory})
	defer func() { _ = manager.Close() }()
	ctx := WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"shell_start", "task_shell_start"}},
		Shell: guard.ShellPerm{Policy: "allowlist", Patterns: []string{"echo *"}},
	})
	ctx = WithShellManager(ctx, manager)
	v := NewShellV2Tools(root)
	if _, err := runTool(ctx, v.Start, `{"command":"echo hi"}`); err != nil {
		t.Fatal(err)
	}
	if _, err := runTool(ctx, v.TaskStart, `{"command":"echo hi"}`); err != nil {
		t.Fatal(err)
	}
	if len(factory.specs) != 2 {
		t.Fatalf("factory saw %d launches, want 2", len(factory.specs))
	}
	for i, spec := range factory.specs {
		if spec.Dir != root {
			t.Fatalf("launch %d Dir = %q, want work root %q", i, spec.Dir, root)
		}
	}
}

func TestShellV2StartUsesRealToolName(t *testing.T) {
	manager := shell.NewManager(shell.Config{Root: t.TempDir(), MaxOutputBytes: 256, Factory: fakeShellFactory{}})
	ctx := WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"shell_start"}},
		Shell: guard.ShellPerm{Policy: "allowlist", Patterns: []string{"echo *"}},
	})
	ctx = WithShellManager(ctx, manager)
	v := NewShellV2Tools(t.TempDir())
	out, err := runTool(ctx, v.Start, `{"command":"echo hi"}`)
	if err != nil {
		t.Fatal(err)
	}
	// Session JSON uses "id":"session-..." — the canonical id field. We look
	// for the "session-" prefix so the test doesn't bind to the exact JSON key
	// spelling (which is "id", not "session_id").
	if !strings.Contains(out, "session-") {
		t.Fatalf("start result=%q", out)
	}
	// Clean up so the manager doesn't leak goroutines.
	_ = manager.Close()
}

func TestShellV2WriteAuthorizesAsWriteToolNotShellString(t *testing.T) {
	manager := shell.NewManager(shell.Config{Root: t.TempDir(), MaxOutputBytes: 256, Factory: fakeShellFactory{}})
	ctx := WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"shell_start"}}, // missing shell_write_stdin
		Shell: guard.ShellPerm{Policy: "allowlist", Patterns: []string{"*"}},
	})
	ctx = WithShellManager(ctx, manager)
	v := NewShellV2Tools(t.TempDir())
	out, _ := runTool(ctx, v.Write, `{"id":"missing","data":"x"}`)
	if !strings.Contains(out, "permission denied") {
		t.Fatalf("write must Authorize as shell_write_stdin, got %q", out)
	}
	_ = manager.Close()
}
