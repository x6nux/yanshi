package tui

import (
	"strings"
	"testing"

	"github.com/x6nux/yanshi/internal/cli"
	"github.com/x6nux/yanshi/internal/proto"
)

// TestSkillCommandsFromClassifiesAvailability pins the projection from the wire
// frame onto the runnable set. Each unavailable reason has a different remedy,
// and collapsing them into one "unavailable" would send the user to fix the
// wrong thing.
func TestSkillCommandsFromClassifiesAvailability(t *testing.T) {
	got := skillCommandsFrom([]proto.SkillInfo{
		{Name: "zeta", Description: "last alphabetically", Enabled: true},
		{Name: "alpha", Description: "fine", Enabled: true},
		{Name: "off", Description: "turned off", Enabled: false},
		{Name: "needs", Description: "wants docker", Enabled: true, Missing: []string{"docker", "kubectl"}},
	})
	if len(got) != 4 {
		t.Fatalf("got %d entries, want 4 — an unusable skill must be LISTED, not dropped", len(got))
	}
	// Sorted, so the palette order does not depend on map iteration.
	wantOrder := []string{"alpha", "needs", "off", "zeta"}
	for i, want := range wantOrder {
		if got[i].Name != want {
			t.Fatalf("entry %d = %q, want %q (sorted)", i, got[i].Name, want)
		}
	}
	byName := map[string]skillCommand{}
	for _, sk := range got {
		byName[sk.Name] = sk
	}
	if byName["alpha"].Unavailable != "" {
		t.Errorf("alpha is usable but marked %q", byName["alpha"].Unavailable)
	}
	if byName["off"].Unavailable != "disabled" {
		t.Errorf("off.Unavailable = %q, want disabled", byName["off"].Unavailable)
	}
	if !strings.Contains(byName["needs"].Unavailable, "docker") ||
		!strings.Contains(byName["needs"].Unavailable, "kubectl") {
		t.Errorf("needs.Unavailable = %q; it must name every missing program",
			byName["needs"].Unavailable)
	}
}

// TestSkillUnavailableReasonOrdering: a disabled skill stays disabled no matter
// what PATH holds, so reporting the missing program first would send the user
// to install something they do not need yet.
func TestSkillUnavailableReasonOrdering(t *testing.T) {
	got := skillUnavailableReason(proto.SkillInfo{
		Name: "x", Enabled: false, Missing: []string{"docker"},
	})
	if got != "disabled" {
		t.Fatalf("reason = %q, want disabled to take precedence over the missing program", got)
	}
}

// TestUpdateSkillRunPaletteOnlyTakesOverItsOwnPrefix. If it claimed inputs it
// does not handle, every other slash command would lose its palette.
func TestUpdateSkillRunPaletteOnlyTakesOverItsOwnPrefix(t *testing.T) {
	cases := []struct {
		input string
		taken bool
	}{
		{"/model", false},
		{"/skill", false},
		{"/skill ", false},
		{"/skill install foo", false},
		// `/skill run` with no trailing space is still the SUBCOMMAND being
		// typed; completing a skill name there would replace the half-typed
		// word.
		{"/skill run", false},
		{"/skill run ", true},
		{"/skill run al", true},
		{"/skill run alpha extra words", true},
		{"not a command", false},
	}
	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			m := &model{skillCommands: skillCommandsFrom([]proto.SkillInfo{
				{Name: "alpha", Enabled: true},
			})}
			if got := m.updateSkillRunPalette(tc.input); got != tc.taken {
				t.Fatalf("updateSkillRunPalette(%q) = %v, want %v", tc.input, got, tc.taken)
			}
		})
	}
}

// TestSkillRunPaletteFiltersAndMarks: the palette must offer matching names and
// must MARK the unusable ones rather than hide them, for the same reason the
// listing does.
func TestSkillRunPaletteFiltersAndMarks(t *testing.T) {
	skills := skillCommandsFrom([]proto.SkillInfo{
		{Name: "deploy", Description: "ship it", Enabled: true},
		{Name: "debug", Description: "poke it", Enabled: true, Missing: []string{"dlv"}},
		{Name: "unrelated", Enabled: true},
	})
	m := &model{skillCommands: skills}

	if !m.updateSkillRunPalette("/skill run de") {
		t.Fatal("the palette did not take over")
	}
	if len(m.paletteItems) != 2 {
		t.Fatalf("got %d items for prefix 'de', want 2: %+v", len(m.paletteItems), m.paletteItems)
	}
	for _, it := range m.paletteItems {
		if it.kind != cmdKindSkillRun {
			t.Errorf("item %q has kind %v; completion would insert a bare name that "+
				"runCommand rejects as an unknown command", it.name, it.kind)
		}
	}
	byName := map[string]command{}
	for _, it := range m.paletteItems {
		byName[it.name] = it
	}
	if byName["debug"].disabled != true {
		t.Error("a skill with a missing requirement must be marked disabled in the palette")
	}
	if byName["deploy"].disabled != false {
		t.Error("a usable skill must not be marked disabled")
	}
	if !strings.Contains(byName["debug"].help, "dlv") {
		t.Errorf("the palette row %q does not say what is missing", byName["debug"].help)
	}

	// A prefix nobody matches yields an empty palette rather than everything.
	if !m.updateSkillRunPalette("/skill run zzz") {
		t.Fatal("the palette did not take over")
	}
	if len(m.paletteItems) != 0 {
		t.Fatalf("a non-matching prefix offered %d items", len(m.paletteItems))
	}

	// Past the name, there is nothing left to complete.
	if !m.updateSkillRunPalette("/skill run deploy the thing") {
		t.Fatal("the palette did not take over")
	}
	if len(m.paletteItems) != 0 {
		t.Fatalf("free-text arguments still offered completions: %+v", m.paletteItems)
	}
}

// TestSkillInvocationCarriesTheArguments pins the text sent as a user turn. It
// names the skill (so skill_use can find it) and preserves the user's own
// words, which are the whole point of typing arguments.
func TestSkillInvocationCarriesTheArguments(t *testing.T) {
	if got := skillInvocation("deploy", nil); !strings.Contains(got, "deploy") {
		t.Errorf("invocation %q does not name the skill", got)
	}
	got := skillInvocation("deploy", []string{"to", "staging"})
	if !strings.Contains(got, "deploy") || !strings.Contains(got, "to staging") {
		t.Errorf("invocation %q lost the arguments", got)
	}
	if strings.Contains(skillInvocation("deploy", []string{"  "}), "  .") {
		t.Error("blank arguments produced a dangling clause")
	}
}

// TestLookupSkillCommandIsExact: prefix matching here would run a different
// skill than the one named, silently.
func TestLookupSkillCommandIsExact(t *testing.T) {
	m := model{skillCommands: skillCommandsFrom([]proto.SkillInfo{
		{Name: "deploy", Enabled: true},
		{Name: "deployment", Enabled: true},
	})}
	sk, ok := m.lookupSkillCommand("deploy")
	if !ok || sk.Name != "deploy" {
		t.Fatalf("lookup(deploy) = %+v %v", sk, ok)
	}
	if _, ok := m.lookupSkillCommand("dep"); ok {
		t.Fatal("a prefix matched; a typo would silently run a different skill")
	}
	if _, ok := m.lookupSkillCommand("Deploy"); ok {
		t.Fatal("matching is case-insensitive; skill names are case-sensitive on disk")
	}
}

// TestSkillsListFrameFeedsTheRunPalette is the wiring assertion: without it the
// projection is a function nobody calls, and `/skill run ` would complete
// nothing forever while the code that computes the completions passes its own
// unit tests.
func TestSkillsListFrameFeedsTheRunPalette(t *testing.T) {
	m := newTestModel(t)
	if m.skillsLoaded {
		t.Fatal("a fresh model must not claim the skill list has arrived")
	}
	m = m.applyEvent(cli.StreamEvent{Kind: "skills_list", Skills: []proto.SkillInfo{
		{Name: "deploy", Description: "ship it", Enabled: true},
	}})
	if !m.skillsLoaded {
		t.Fatal("skillsLoaded stayed false after a skills_list frame")
	}
	if len(m.skillCommands) != 1 || m.skillCommands[0].Name != "deploy" {
		t.Fatalf("skillCommands = %+v; the frame did not reach the run palette", m.skillCommands)
	}
}
