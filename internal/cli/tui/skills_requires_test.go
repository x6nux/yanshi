package tui

import (
	"strings"
	"testing"

	"github.com/x6nux/yanshi/internal/proto"
)

// TestSkillsEntryReportsMissingRequirements is the user-visible end of T5. A
// skill whose declared programs are absent is withheld from the model's
// listing and refused by skill_use, so /skills is the ONE place a user can
// find out why a skill they installed is never being used. Rendering the state
// nowhere is the same silence the feature exists to break.
func TestSkillsEntryReportsMissingRequirements(t *testing.T) {
	e := skillsEntry{skills: []proto.SkillInfo{
		{Name: "codesearch", Source: "user", Enabled: true, Trusted: true,
			Missing: []string{"ast-grep", "rg"}},
		{Name: "plain", Source: "user", Enabled: true, Trusted: true},
	}}
	out := stripANSI(e.render(120, newSpinner()))

	if !strings.Contains(out, "ast-grep") || !strings.Contains(out, "rg") {
		t.Fatalf("the missing programs are not named:\n%s", out)
	}
	if !strings.Contains(out, "unavailable") {
		t.Errorf("the CONSEQUENCE is not stated; without it the row reads as a "+
			"cosmetic warning about a skill the user assumes still works:\n%s", out)
	}
	// The satisfied skill must not grow a spurious line.
	if strings.Count(out, "unavailable") != 1 {
		t.Errorf("unavailable line count = %d, want 1:\n%s",
			strings.Count(out, "unavailable"), out)
	}
}

// TestSkillsEntrySingularAndPluralPhrasing keeps the sentence readable in both
// shapes, since one missing binary is by far the common case.
func TestSkillsEntrySingularAndPluralPhrasing(t *testing.T) {
	one := stripANSI(skillsEntry{skills: []proto.SkillInfo{
		{Name: "a", Enabled: true, Missing: []string{"gh"}},
	}}.render(120, newSpinner()))
	if !strings.Contains(one, "install it") {
		t.Errorf("singular phrasing missing:\n%s", one)
	}

	two := stripANSI(skillsEntry{skills: []proto.SkillInfo{
		{Name: "a", Enabled: true, Missing: []string{"gh", "rg"}},
	}}.render(120, newSpinner()))
	if !strings.Contains(two, "install them") {
		t.Errorf("plural phrasing missing:\n%s", two)
	}
}

func TestPluralProgram(t *testing.T) {
	cases := map[int]string{0: "them", 1: "it", 2: "them", 9: "them"}
	for n, want := range cases {
		if got := pluralProgram(n); got != want {
			t.Errorf("pluralProgram(%d) = %q, want %q", n, got, want)
		}
	}
}
