package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoad_MemoryDefaults(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	os.WriteFile(p, []byte("server:\n  http_addr: 127.0.0.1:0\n"), 0o644)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Memory.Enabled {
		t.Errorf("默认应 Enabled=false")
	}
	if cfg.Memory.UserPath != "" {
		t.Errorf("默认 UserPath 应空(由 bootstrap 展开 ~)")
	}
	if cfg.Memory.MaxSize != 0 {
		t.Errorf("默认 MaxSize 应 0(用 memory.defaultMaxBytes)")
	}
}

func TestLoad_MemoryFromYAML(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "c.yaml")
	os.WriteFile(p, []byte(
		"memory:\n  enabled: true\n  user_path: ~/foo/m.md\n  max_size: 8192\n"), 0o644)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Memory.Enabled || cfg.Memory.UserPath != "~/foo/m.md" || cfg.Memory.MaxSize != 8192 {
		t.Errorf("memory 字段未解析: %+v", cfg.Memory)
	}
}
