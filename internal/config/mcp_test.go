package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMCPConfig_Parse(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	data := []byte("mcp:\n  servers:\n    s:\n      enabled: true\n      transport: http\n      url: http://localhost/mcp\n      timeout: 30s\n")
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MCP.Servers["s"].URL != "http://localhost/mcp" {
		t.Fatalf("cfg=%+v", cfg.MCP)
	}
}
