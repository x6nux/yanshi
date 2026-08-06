package skills

import (
	"os"
	"path/filepath"
	"testing"
)

func writeConflictSkill(t *testing.T, dir, name, desc string) {
	t.Helper()
	sub := filepath.Join(dir, name)
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "---\nname: " + name + "\ndescription: " + desc + "\n---\n\nbody\n"
	if err := os.WriteFile(filepath.Join(sub, "SKILL.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestLoadReportsShadowedSkills pins the half of E03 that was invisible.
//
// Load's duplicate branch was `continue // first-seen-wins`: the losing skill
// never entered the Registry, so /skills showed only the winner and a user
// whose project skill was shadowed by a user-level one of the same name saw no
// sign of it at all. The name resolved to something they did not write, and
// nothing in the product could say so.
//
// First-seen-wins is kept — changing the resolution order would silently swap
// which skill runs. What changes is that the loss is now RECORDED.
//
// ledger: C3/E03#3 重名可诊断
func TestLoadReportsShadowedSkills(t *testing.T) {
	winner := t.TempDir()
	loser := t.TempDir()
	writeConflictSkill(t, winner, "review", "the one that wins")
	writeConflictSkill(t, loser, "review", "the one that is hidden")
	writeConflictSkill(t, loser, "unique", "no conflict here")

	reg, err := NewLoader(
		Root{Dir: winner, Source: "project"},
		Root{Dir: loser, Source: "user"},
	).Load()
	if err != nil {
		t.Fatal(err)
	}

	got, ok := reg.Get("review")
	if !ok || got.Description != "the one that wins" {
		t.Fatalf("resolution order changed: %+v (ok=%v)", got, ok)
	}

	conflicts := reg.Conflicts()
	if len(conflicts) != 1 {
		t.Fatalf("want exactly one conflict, got %+v", conflicts)
	}
	c := conflicts[0]
	if c.Name != "review" {
		t.Errorf("conflict name = %q", c.Name)
	}
	if c.WinnerSource != "project" || c.ShadowedSource != "user" {
		t.Errorf("conflict does not name both sides: %+v", c)
	}
	if c.ShadowedDir == "" {
		t.Error("the shadowed directory is not reported, so the user cannot find " +
			"the file that is being ignored")
	}
	if c.WinnerDir == c.ShadowedDir {
		t.Errorf("winner and shadowed dir are the same: %+v", c)
	}
}

// TestValidateSkillRechecksAnInstalledSkill covers the other gap: the
// frontmatter and containment checks existed only INLINE in Install, so once a
// skill was on disk nothing could re-run them. A skill edited by hand after
// installation was unverifiable.
//
// ledger: C3/E03#2 恶意路径安全
func TestValidateSkillRechecksAnInstalledSkill(t *testing.T) {
	root := t.TempDir()
	writeConflictSkill(t, root, "good", "a valid description")

	if err := ValidateSkillDir(filepath.Join(root, "good")); err != nil {
		t.Fatalf("a valid skill failed validation: %v", err)
	}

	// An empty description is exactly what validDesc rejects at install time.
	bad := filepath.Join(root, "bad")
	if err := os.MkdirAll(bad, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bad, "SKILL.md"),
		[]byte("---\nname: bad\ndescription:\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ValidateSkillDir(bad); err == nil {
		t.Error("an invalid skill passed validation: the install-time rules must " +
			"still apply after installation, or a hand-edited skill is unverifiable")
	}

	if err := ValidateSkillDir(filepath.Join(root, "does-not-exist")); err == nil {
		t.Error("a missing directory must not validate")
	}
}
