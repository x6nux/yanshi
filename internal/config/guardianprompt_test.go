package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/x6nux/yanshi/internal/guard"
)

// writeGuardianFixture writes a guardian prompt file covering every required
// risk category, minus the ones named in drop.
func writeGuardianFixture(t *testing.T, dir string, drop ...string) string {
	t.Helper()
	dropped := map[string]bool{}
	for _, d := range drop {
		dropped[d] = true
	}
	var b strings.Builder
	b.WriteString("Operator policy for this deployment.\n")
	for _, c := range guard.RequiredRiskCategories() {
		if dropped[c.Name] {
			continue
		}
		b.WriteString(c.Name + ": " + strings.Join(c.Markers, " ") + "\n")
	}
	path := filepath.Join(dir, "guardian.txt")
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestGuardianPromptFileIsLoadedAndValidated pins the three outcomes of
// security.guardian_prompt_file (W-B-14).
//
// The two failure cases both REFUSE THE START rather than falling back. That
// is the point of validating here at all: guard.AutoApprovalPromptWith already
// falls back safely at use, so a load path that also fell back would leave the
// operator with a policy file that has no effect and no error — the exact
// "written but nobody reads it" shape this repository keeps rediscovering.
func TestGuardianPromptFileIsLoadedAndValidated(t *testing.T) {
	dir := t.TempDir()

	t.Run("valid file is loaded", func(t *testing.T) {
		c := &Config{}
		c.Security.GuardianPromptFile = writeGuardianFixture(t, dir)
		if err := c.validate(); err != nil {
			t.Fatalf("valid guardian prompt rejected: %v", err)
		}
		if !strings.Contains(c.Security.GuardianPrompt, "Operator policy") {
			t.Fatalf("prompt not loaded into the config: %q", c.Security.GuardianPrompt)
		}
	})

	t.Run("missing file refuses the start", func(t *testing.T) {
		c := &Config{}
		c.Security.GuardianPromptFile = filepath.Join(dir, "does-not-exist.txt")
		err := c.validate()
		if err == nil || !strings.Contains(err.Error(), "guardian_prompt_file") {
			t.Fatalf("a missing policy file must name itself in the error, got %v", err)
		}
	})

	t.Run("hollowed file refuses the start", func(t *testing.T) {
		missing := guard.RequiredRiskCategories()[0]
		c := &Config{}
		c.Security.GuardianPromptFile = writeGuardianFixture(t, t.TempDir(), missing.Name)
		err := c.validate()
		if err == nil || !strings.Contains(err.Error(), missing.Name) {
			t.Fatalf("a policy file missing %q must be rejected by name, got %v", missing.Name, err)
		}
		if c.Security.GuardianPrompt != "" {
			t.Fatal("a rejected policy file was still installed")
		}
	})

	t.Run("unset stays empty", func(t *testing.T) {
		c := &Config{}
		if err := c.validate(); err != nil {
			t.Fatalf("no guardian file configured must not error: %v", err)
		}
		if c.Security.GuardianPrompt != "" {
			t.Fatalf("unset file produced a prompt: %q", c.Security.GuardianPrompt)
		}
	})
}
