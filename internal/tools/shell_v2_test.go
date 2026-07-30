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

func (fakeShellProcess) Wait() error                                  { return nil }
func (fakeShellProcess) PID() int                                     { return 1 }
func (fakeShellProcess) Kill() error                                  { return nil }
func (fakeShellProcess) Capabilities() shell.ProcessCapabilities {
	return shell.ProcessCapabilities{CanKillTree: false}
}

type fakeShellConsole struct{}

func (fakeShellConsole) Read([]byte) (int, error)     { return 0, io.EOF }
func (fakeShellConsole) Write(p []byte) (int, error)  { return len(p), nil }
func (fakeShellConsole) Close() error                 { return nil }
func (fakeShellConsole) Resize(uint16, uint16) error  { return nil }
func (fakeShellConsole) PTY() bool                    { return false }

type fakeShellFactory struct{}

func (fakeShellFactory) Start(context.Context, shell.LaunchSpec) (shell.Process, shell.Console, error) {
	return fakeShellProcess{}, fakeShellConsole{}, nil
}

func TestShellV2StartUsesRealToolName(t *testing.T) {
	manager := shell.NewManager(shell.Config{Root: t.TempDir(), MaxOutputBytes: 256, Factory: fakeShellFactory{}})
	ctx := WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"shell_start"}},
		Shell: guard.ShellPerm{Policy: "allowlist", Patterns: []string{"echo *"}},
	})
	ctx = WithShellManager(ctx, manager)
	v := NewShellV2Tools()
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
	v := NewShellV2Tools()
	out, _ := runTool(ctx, v.Write, `{"id":"missing","data":"x"}`)
	if !strings.Contains(out, "permission denied") {
		t.Fatalf("write must Authorize as shell_write_stdin, got %q", out)
	}
	_ = manager.Close()
}
