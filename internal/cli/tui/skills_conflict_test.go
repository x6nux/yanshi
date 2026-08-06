package tui

import (
	"strings"
	"testing"

	"github.com/x6nux/yanshi/internal/proto"
)

// TestSkillsEntryShowsShadowedCopies is the user-visible end of E03's conflict
// diagnostics. A conflict recorded in the registry and never rendered is the
// same silence the feature exists to break.
//
// ledger: C3/E03#3 重名可诊断
func TestSkillsEntryShowsShadowedCopies(t *testing.T) {
	e := skillsEntry{skills: []proto.SkillInfo{
		{Name: "review", Source: "project", Enabled: true, Trusted: true,
			Shadowed: []proto.ShadowedSkill{{Source: "user", Dir: "/home/u/.yanshi/skills/review"}}},
		{Name: "solo", Source: "user", Enabled: true, Trusted: true},
	}}
	out := stripANSI(e.render(120, newSpinner()))

	if !strings.Contains(out, "shadowed") {
		t.Fatalf("a shadowed copy is not reported:\n%s", out)
	}
	if !strings.Contains(out, "/home/u/.yanshi/skills/review") {
		t.Error("the ignored DIRECTORY is not shown; a source label alone does not " +
			"tell the user which file is being ignored")
	}
	// The unconflicted skill must not grow a spurious line.
	if strings.Count(out, "shadowed") != 1 {
		t.Errorf("shadow line count = %d, want 1:\n%s", strings.Count(out, "shadowed"), out)
	}
}

// TestSkillValidateIsRoutedToTheServer pins the subcommand doctor-style
// re-validation needs. cmdSkill's switch had six verbs and a default that
// reported "unknown /skill subcommand", so the install-time checks — which
// live only inside Install — could never be re-run.
//
// ledger: C3/E03#1 可安装/列出/启停/校验
func TestSkillValidateIsRoutedToTheServer(t *testing.T) {
	rec := &recordingSession{}
	m := newModel(rec, "/proj")

	mm, _ := m.runCommand("/skill validate")
	m = mm.(model)
	if len(rec.frames) == 0 {
		t.Fatal("/skill validate sent no frame: the subcommand is not routed")
	}
	if got := rec.frames[len(rec.frames)-1].Type; got != "validate_skill" {
		t.Fatalf("frame type = %q, want validate_skill", got)
	}
	// An empty name means "all installed skills" — the useful default after
	// editing one by hand, and the reason this verb does not go through
	// skillNamed, which requires an argument.
	if got := rec.frames[len(rec.frames)-1].Name; got != "" {
		t.Errorf("bare /skill validate carried a name %q", got)
	}

	mm, _ = m.runCommand("/skill validate review")
	m = mm.(model)
	if got := rec.frames[len(rec.frames)-1].Name; got != "review" {
		t.Errorf("named validate carried %q", got)
	}
}
