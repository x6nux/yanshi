package tools

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/x6nux/yanshi/internal/guard"
	"github.com/x6nux/yanshi/internal/shell"
)

func TestShellV2Read(t *testing.T) {
	manager := shell.NewManager(shell.Config{Root: t.TempDir(), MaxOutputBytes: 4096, Factory: fakeShellFactory{}})
	defer func() { _ = manager.Close() }()
	ctx := contextWithShellPerms(t, manager, []string{"shell_start", "shell_read"})

	v := NewShellV2Tools(t.TempDir())
	// Start a session first.
	startOut, err := runTool(ctx, v.Start, `{"command":"echo hi"}`)
	if err != nil {
		t.Fatal(err)
	}
	sessionID := extractSessionID(t, startOut)
	// Read from the session — the fake console returns io.EOF so output is empty.
	out, err := runTool(ctx, v.Read, `{"id":"`+sessionID+`","max_bytes":1024}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "output") {
		t.Fatalf("read result=%q", out)
	}
}

func TestShellV2Write(t *testing.T) {
	manager := shell.NewManager(shell.Config{Root: t.TempDir(), MaxOutputBytes: 4096, Factory: fakeShellFactory{}})
	defer func() { _ = manager.Close() }()
	ctx := contextWithShellPerms(t, manager, []string{"shell_start", "shell_write_stdin"})

	v := NewShellV2Tools(t.TempDir())
	startOut, err := runTool(ctx, v.Start, `{"command":"echo hi"}`)
	if err != nil {
		t.Fatal(err)
	}
	sessionID := extractSessionID(t, startOut)
	out, err := runTool(ctx, v.Write, `{"id":"`+sessionID+`","data":"some input"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "written") {
		t.Fatalf("write result=%q", out)
	}
}

func TestShellV2Wait(t *testing.T) {
	manager := shell.NewManager(shell.Config{Root: t.TempDir(), MaxOutputBytes: 4096, Factory: fakeShellFactory{}})
	defer func() { _ = manager.Close() }()
	ctx := contextWithShellPerms(t, manager, []string{"shell_start", "shell_wait"})

	v := NewShellV2Tools(t.TempDir())
	startOut, err := runTool(ctx, v.Start, `{"command":"echo hi"}`)
	if err != nil {
		t.Fatal(err)
	}
	sessionID := extractSessionID(t, startOut)
	ctxTimeout, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := runTool(ctxTimeout, v.Wait, `{"id":"`+sessionID+`"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "exit_code") {
		t.Fatalf("wait result=%q", out)
	}
}

func TestShellV2Cancel(t *testing.T) {
	manager := shell.NewManager(shell.Config{Root: t.TempDir(), MaxOutputBytes: 4096, Factory: fakeShellFactory{}})
	defer func() { _ = manager.Close() }()
	ctx := contextWithShellPerms(t, manager, []string{"shell_start", "shell_cancel"})

	v := NewShellV2Tools(t.TempDir())
	startOut, err := runTool(ctx, v.Start, `{"command":"echo hi"}`)
	if err != nil {
		t.Fatal(err)
	}
	sessionID := extractSessionID(t, startOut)
	out, err := runTool(ctx, v.Cancel, `{"id":"`+sessionID+`"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "canceled") {
		t.Fatalf("cancel result=%q", out)
	}
}

func TestShellV2TaskStart(t *testing.T) {
	manager := shell.NewManager(shell.Config{Root: t.TempDir(), MaxOutputBytes: 4096, Factory: fakeShellFactory{}})
	defer func() { _ = manager.Close() }()
	ctx := contextWithShellPerms(t, manager, []string{"task_shell_start"})

	v := NewShellV2Tools(t.TempDir())
	out, err := runTool(ctx, v.TaskStart, `{"command":"echo hi"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "id") {
		t.Fatalf("taskStart result=%q", out)
	}
}

func TestShellV2TaskWait(t *testing.T) {
	manager := shell.NewManager(shell.Config{Root: t.TempDir(), MaxOutputBytes: 4096, Factory: fakeShellFactory{}})
	defer func() { _ = manager.Close() }()
	ctx := contextWithShellPerms(t, manager, []string{"task_shell_start", "task_shell_wait"})

	v := NewShellV2Tools(t.TempDir())
	startOut, err := runTool(ctx, v.TaskStart, `{"command":"echo hi"}`)
	if err != nil {
		t.Fatal(err)
	}
	jobID := extractSessionID(t, startOut)
	out, err := runTool(ctx, v.TaskWait, `{"id":"`+jobID+`","max_bytes":1024}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "output") {
		t.Fatalf("taskWait result=%q", out)
	}
}

func TestShellV2TaskWrite(t *testing.T) {
	manager := shell.NewManager(shell.Config{Root: t.TempDir(), MaxOutputBytes: 4096, Factory: fakeShellFactory{}})
	defer func() { _ = manager.Close() }()
	ctx := contextWithShellPerms(t, manager, []string{"task_shell_start", "task_shell_stdin"})

	v := NewShellV2Tools(t.TempDir())
	startOut, err := runTool(ctx, v.TaskStart, `{"command":"echo hi"}`)
	if err != nil {
		t.Fatal(err)
	}
	sesID := extractSessionIDFromJob(t, startOut)
	out, err := runTool(ctx, v.TaskWrite, `{"id":"`+sesID+`","data":"input"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "written") {
		t.Fatalf("taskWrite result=%q", out)
	}
}

func TestShellV2TaskCancel(t *testing.T) {
	manager := shell.NewManager(shell.Config{Root: t.TempDir(), MaxOutputBytes: 4096, Factory: fakeShellFactory{}})
	defer func() { _ = manager.Close() }()
	ctx := contextWithShellPerms(t, manager, []string{"task_shell_start", "task_shell_cancel"})

	v := NewShellV2Tools(t.TempDir())
	startOut, err := runTool(ctx, v.TaskStart, `{"command":"echo hi"}`)
	if err != nil {
		t.Fatal(err)
	}
	sesID := extractSessionIDFromJob(t, startOut)
	out, err := runTool(ctx, v.TaskCancel, `{"id":"`+sesID+`"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "canceled") {
		t.Fatalf("taskCancel result=%q", out)
	}
}

func TestShellV2AllDeniedWithoutProfile(t *testing.T) {
	v := NewShellV2Tools(t.TempDir())
	ctx := context.Background()
	// Without profile, all tools should return permission denied.
	tt := []struct {
		name string
		tool *GuardedTool
		args string
	}{
		{"start", v.Start, `{"command":"echo hi"}`},
		{"read", v.Read, `{"id":"x","max_bytes":1}`},
		{"write", v.Write, `{"id":"x","data":"x"}`},
		{"wait", v.Wait, `{"id":"x"}`},
		{"cancel", v.Cancel, `{"id":"x"}`},
		{"taskStart", v.TaskStart, `{"command":"echo hi"}`},
		{"taskWait", v.TaskWait, `{"id":"x","max_bytes":1}`},
		{"taskWrite", v.TaskWrite, `{"id":"x","data":"x"}`},
		{"taskCancel", v.TaskCancel, `{"id":"x"}`},
	}
	for _, tc := range tt {
		t.Run(tc.name, func(t *testing.T) {
			out, _ := runTool(ctx, tc.tool, tc.args)
			if !strings.Contains(out, "permission denied") {
				t.Fatalf("expected permission denied, got %q", out)
			}
		})
	}
}

// contextWithShellPerms returns a context with profile, permission callback,
// and shell manager.
func contextWithShellPerms(t *testing.T, manager *shell.Manager, allowedTools []string) context.Context {
	t.Helper()
	profile := guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: allowedTools},
		Shell: guard.ShellPerm{Policy: "allowlist", Patterns: []string{"*"}},
	}
	ctx := WithProfile(context.Background(), profile)
	ctx = WithPermissionCallback(ctx, func(req PermissionRequest) PermissionDecision {
		return PermissionAllow
	})
	ctx = WithShellManager(ctx, manager)
	return ctx
}

// extractSessionIDFromJob pulls the session_id from a job JSON result.
func extractSessionIDFromJob(t *testing.T, jsonResult string) string {
	t.Helper()
	idx := strings.Index(jsonResult, `"session_id":"`)
	if idx < 0 {
		t.Fatalf("no session_id field in %q", jsonResult)
	}
	start := idx + len(`"session_id":"`)
	end := strings.Index(jsonResult[start:], `"`)
	if end < 0 {
		t.Fatalf("malformed session_id in %q", jsonResult)
	}
	return jsonResult[start : start+end]
}

// extractSessionID pulls the first alphanumeric id value from a JSON result.
func extractSessionID(t *testing.T, jsonResult string) string {
	t.Helper()
	// Look for "id":"session-..." or "id":"job-..."
	idx := strings.Index(jsonResult, `"id":"`)
	if idx < 0 {
		t.Fatalf("no id field in %q", jsonResult)
	}
	start := idx + len(`"id":"`)
	end := strings.Index(jsonResult[start:], `"`)
	if end < 0 {
		t.Fatalf("malformed id in %q", jsonResult)
	}
	return jsonResult[start : start+end]
}
