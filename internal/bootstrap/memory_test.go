package bootstrap

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestBuild_MemorySuffixWired proves that with cfg.Memory.Enabled=true and the
// memory file present, App.Orch's memorySuffix is populated correctly.
func TestBuild_MemorySuffixWired(t *testing.T) {
	projectDir := t.TempDir()
	memFile := filepath.Join(projectDir, "mem.md")
	os.WriteFile(memFile, []byte("prefer concise answers\n"), 0o644)
	cfgPath := filepath.Join(projectDir, "config.yaml")
	os.WriteFile(cfgPath, []byte(
		"server:\n  http_addr: 127.0.0.1:0\n"+
			"storage:\n  sqlite_path: \":memory:\"\n"+
			"memory:\n"+
			"  enabled: true\n"+
			"  user_path: "+memFile+"\n"+
			"  max_size: 4096\n"), 0o644)

	app, err := Build(Options{ConfigPath: cfgPath, FakeModel: true})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	// CB7: must use context.Background(); nil panics on <-ctx.Done() in Shutdown.
	defer app.Shutdown(context.Background())

	got := app.Orch.MemorySuffix()
	if !strings.Contains(got, "prefer concise answers") {
		t.Errorf("memorySuffix should include file content, got: %q", got)
	}
	if !strings.Contains(got, "<user_memory") {
		t.Errorf("memorySuffix should include <user_memory> XML block, got: %q", got)
	}
}

// TestBuild_MemorySuffixDisabled proves Enabled=false leaves memorySuffix empty.
// SC2 consistency: Enabled=false empties memorySuffix; Step 3a's same if also
// gates MemoryPath and remember registration. This test only observes suffix
// directly — the other two are guaranteed by the single gate in Step 3a.
func TestBuild_MemorySuffixDisabled(t *testing.T) {
	projectDir := t.TempDir()
	cfgPath := filepath.Join(projectDir, "config.yaml")
	os.WriteFile(cfgPath, []byte(
		"server:\n  http_addr: 127.0.0.1:0\n"+
			"storage:\n  sqlite_path: \":memory:\"\n"), 0o644)
	app, err := Build(Options{ConfigPath: cfgPath, FakeModel: true})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	defer app.Shutdown(context.Background())
	if got := app.Orch.MemorySuffix(); got != "" {
		t.Errorf("Enabled=false, memorySuffix should be empty, got: %q", got)
	}
}

// TestBuild_MemoryExpandsUserPath proves ~ is expanded.
func TestBuild_MemoryExpandsUserPath(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("os.UserHomeDir failed")
	}
	memFile := filepath.Join(home, ".yanshi-test-mem.md")
	os.WriteFile(memFile, []byte("HOME_MEMORY_CONTENT\n"), 0o644)
	defer os.Remove(memFile)

	projectDir := t.TempDir()
	cfgPath := filepath.Join(projectDir, "config.yaml")
	os.WriteFile(cfgPath, []byte(
		"server:\n  http_addr: 127.0.0.1:0\n"+
			"storage:\n  sqlite_path: \":memory:\"\n"+
			"memory:\n"+
			"  enabled: true\n"+
			"  user_path: ~/.yanshi-test-mem.md\n"), 0o644)
	app, err := Build(Options{ConfigPath: cfgPath, FakeModel: true})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	defer app.Shutdown(context.Background())
	if got := app.Orch.MemorySuffix(); !strings.Contains(got, "HOME_MEMORY_CONTENT") {
		t.Errorf("~ should be expanded and content read, got: %q", got)
	}
}
