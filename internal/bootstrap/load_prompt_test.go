package bootstrap

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadProjectPrompt_PrefersAGENTSmd(t *testing.T) {
	dir := t.TempDir()
	// All three present — AGENTS.md must win.
	os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("# AGENTS.md"), 0o644)
	os.WriteFile(filepath.Join(dir, "AGENT.md"), []byte("# AGENT.md"), 0o644)
	os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte("# CLAUDE.md"), 0o644)

	got := loadProjectPrompt(dir)
	if !strings.Contains(got, "# AGENTS.md") || strings.Contains(got, "# AGENT.md") {
		t.Errorf("loadProjectPrompt must prefer AGENTS.md over AGENT.md/CLAUDE.md; got %q", got)
	}
}

func TestLoadProjectPrompt_PrefersAGENTmd(t *testing.T) {
	dir := t.TempDir()

	// Write both files with different content.
	agentContent := "# AGENT.md instructions"
	claudeContent := "# CLAUDE.md instructions"
	os.WriteFile(filepath.Join(dir, "AGENT.md"), []byte(agentContent), 0o644)
	os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte(claudeContent), 0o644)

	got := loadProjectPrompt(dir)
	// loadProjectPrompt delegates to instruct.LoadHierarchical, which wraps the
	// content in a "## Instructions" header; assert by containment so the test
	// tracks the fallback-order contract (AGENT.md before CLAUDE.md) rather than
	// the exact wrapper bytes.
	if !strings.Contains(got, agentContent) {
		t.Errorf("loadProjectPrompt(dir) = %q; want to contain %q", got, agentContent)
	}
	if strings.Contains(got, claudeContent) {
		t.Errorf("loadProjectPrompt(dir) = %q; must not contain CLAUDE.md content when AGENT.md present", got)
	}
}

func TestLoadProjectPrompt_FallsBackToCLAUDEmd(t *testing.T) {
	dir := t.TempDir()

	claudeContent := "# CLAUDE.md instructions"
	os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte(claudeContent), 0o644)

	got := loadProjectPrompt(dir)
	if !strings.Contains(got, claudeContent) {
		t.Errorf("loadProjectPrompt(dir) = %q; want to contain %q", got, claudeContent)
	}
}

func TestLoadProjectPrompt_EmptyWhenNoFile(t *testing.T) {
	dir := t.TempDir()
	// No AGENT.md, no CLAUDE.md.

	got := loadProjectPrompt(dir)
	if got != "" {
		t.Errorf("loadProjectPrompt(dir) = %q; want empty", got)
	}
}

func TestLoadProjectPrompt_IgnoresSubdirs(t *testing.T) {
	dir := t.TempDir()

	// File in a subdirectory, not at the project root — must NOT be picked up.
	subdir := filepath.Join(dir, "sub")
	os.MkdirAll(subdir, 0o755)
	os.WriteFile(filepath.Join(subdir, "AGENT.md"), []byte("sub content"), 0o644)

	got := loadProjectPrompt(dir)
	if got != "" {
		t.Errorf("loadProjectPrompt(dir) = %q; want empty (AGENT.md is in subdir)", got)
	}
}

func TestLoadProjectPrompt_EmptyDir(t *testing.T) {
	got := loadProjectPrompt("")
	if got != "" {
		t.Errorf("loadProjectPrompt(\"\") = %q; want empty", got)
	}
}
