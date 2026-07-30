package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/x6nux/yanshi/internal/guard"
)

// TestRememberTool_AppendsToUserFile calls NewRememberTool and uses the existing
// runTool helper (internal/tools/helpers.go:71, accepts tool.InvokableTool)
// rather than rolling a parallel invokeSyncTool. BQ3 fix point.
func TestRememberTool_AppendsToUserFile(t *testing.T) {
	dir := t.TempDir()
	userPath := filepath.Join(dir, "u.md")
	projectPath := filepath.Join(dir, "p.md")
	tool := NewRememberTool(userPath, projectPath)
	ctx := WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"*"}},
	})
	out, err := runTool(ctx, tool, `{"content":"buy milk","scope":"user"}`)
	if err != nil {
		t.Fatalf("runTool: %v", err)
	}
	if !strings.Contains(out, "saved") || !strings.Contains(out, "restart") {
		t.Errorf("ack should include 'saved' and 'restart', got: %q", out)
	}
	body, _ := os.ReadFile(userPath)
	if !strings.Contains(string(body), "buy milk") {
		t.Errorf("user file should contain entry: %q", body)
	}
	if _, err := os.Stat(projectPath); !os.IsNotExist(err) {
		body2, _ := os.ReadFile(projectPath)
		t.Errorf("scope=user should not write project file, got: %q", body2)
	}
}

func TestRememberTool_AppendsToProjectFile(t *testing.T) {
	dir := t.TempDir()
	userPath := filepath.Join(dir, "u.md")
	projectPath := filepath.Join(dir, "p.md")
	tool := NewRememberTool(userPath, projectPath)
	ctx := WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"*"}},
	})
	if _, err := runTool(ctx, tool, `{"content":"use postgres","scope":"project"}`); err != nil {
		t.Fatalf("runTool: %v", err)
	}
	body, _ := os.ReadFile(projectPath)
	if !strings.Contains(string(body), "use postgres") {
		t.Errorf("project file should contain entry: %q", body)
	}
	if _, err := os.Stat(userPath); !os.IsNotExist(err) {
		t.Errorf("scope=project should not write user file")
	}
}

func TestRememberTool_RejectsEmptyContent(t *testing.T) {
	dir := t.TempDir()
	tool := NewRememberTool(filepath.Join(dir, "u.md"), filepath.Join(dir, "p.md"))
	ctx := WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"*"}},
	})
	out, err := runTool(ctx, tool, `{"content":""}`)
	if err != nil {
		t.Fatalf("GuardedTool operational failure should be a result, got Go error: %v", err)
	}
	if !strings.Contains(out, "content must be non-empty") {
		t.Fatalf("empty content should return a retryable tool result, got %q", out)
	}
}

func TestRememberTool_RejectsUnknownScope(t *testing.T) {
	dir := t.TempDir()
	tool := NewRememberTool(filepath.Join(dir, "u.md"), filepath.Join(dir, "p.md"))
	ctx := WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"*"}},
	})
	out, err := runTool(ctx, tool, `{"content":"x","scope":"elsewhere"}`)
	if err != nil {
		t.Fatalf("GuardedTool operational failure should be a result, got Go error: %v", err)
	}
	if !strings.Contains(out, "scope must be") {
		t.Fatalf("unknown scope should be rejected, got %q", out)
	}
}
