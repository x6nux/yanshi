package bootstrap

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/x6nux/yanshi/internal/mcp"
)

func buildFakeApp(t *testing.T, extraYAML string) (*App, func()) {
	t.Helper()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	dbPath := filepath.Join(dir, "test.db")
	// Use toYAMLPath to escape backslashes in Windows paths
	cfg := "server:\n  http_addr: \"127.0.0.1:0\"\nstorage:\n  sqlite_path: \"" + toYAMLPath(dbPath) + "\"\ntoken: \"test-token\"\n" + extraYAML
	if err := os.WriteFile(cfgPath, []byte(cfg), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	app, err := Build(Options{ConfigPath: cfgPath, FakeModel: true})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	return app, func() { _ = app.Shutdown(context.Background()) }
}

func toYAMLPath(p string) string {
	// Escape backslashes for YAML
	return filepath.ToSlash(p)
}

// TestBuild_MCP_EmptyConfig ensures App.MCP is always non-nil (soft-degrade):
// an empty mcp.servers map yields a disabled manager whose Enabled()==false.
func TestBuild_MCP_EmptyConfig(t *testing.T) {
	app, cleanup := buildFakeApp(t, "")
	defer cleanup()
	if app.MCP == nil {
		t.Fatal("App.MCP must be non-nil even with no mcp servers")
	}
	if app.MCP.Enabled() {
		t.Fatal("App.MCP should be disabled when the servers map is empty")
	}
	if snaps := app.MCP.Snapshot(context.Background()); len(snaps) != 0 {
		t.Fatalf("expected zero servers in snapshot, got %+v", snaps)
	}
}

// TestBuild_MCP_FakeHTTPServer wires a real mcp.NewFakeHTTPServer through
// Build's config.yaml and asserts StartAll marked the server ready.
func TestBuild_MCP_FakeHTTPServer(t *testing.T) {
	ts, _ := mcp.NewFakeHTTPServer([]mcp.ToolDescriptor{{ToolName: "echo"}})
	defer ts.Close()
	extra := "\nmcp:\n  servers:\n    s:\n      enabled: true\n      transport: http\n      url: \"" + ts.URL + "\"\n"
	app, cleanup := buildFakeApp(t, extra)
	defer cleanup()
	if app.MCP == nil || !app.MCP.Enabled() {
		t.Fatal("App.MCP must be non-nil and enabled with a configured server")
	}
	snaps := app.MCP.Snapshot(context.Background())
	if len(snaps) != 1 || snaps[0].Status != mcp.StatusReady {
		t.Fatalf("expected server ready, got %+v", snaps)
	}
}
