package tui

// liverun_t5t11_test.go — a skill installed on disk, all the way to the
// command a user can type.
//
// T11 (dynamic slash commands) and T5 (declared dependencies) are both claims
// about a chain: SKILL.md on disk -> loader -> registry -> proto.SkillInfo on
// the wire -> the TUI's palette. The tests beside this file each check one
// link, mostly by constructing the intermediate value by hand — which is the
// right way to test a link and cannot detect a broken one. A frontmatter key
// the loader ignores, or a Missing list the wire drops, leaves every one of
// them green while the user sees a skill that is not there or a broken skill
// offered as usable.
//
// So this writes real SKILL.md files into a real directory, loads them with the
// real loader, converts through the real proto shape, and asks the real palette
// what a user typing `/skill run ` would be offered.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/x6nux/yanshi/internal/proto"
	"github.com/x6nux/yanshi/internal/skills"
)

// writeSkill creates <root>/<name>/SKILL.md with the given frontmatter body.
func writeSkill(t *testing.T, root, name, content string) {
	t.Helper()
	dir := filepath.Join(root, name)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(content), 0o644))
}

// loadPalette runs the production loader over root and returns the palette
// entries a user would see after typing `/skill run <prefix>`.
//
// It goes through proto.SkillInfo deliberately: that is the wire shape, and a
// field the registry fills but the frame drops is exactly the kind of break
// this test exists to catch.
func loadPalette(t *testing.T, root, prefix string) ([]command, []proto.SkillInfo) {
	t.Helper()
	reg, err := skills.NewLoader(skills.User(root)).Load()
	require.NoError(t, err)

	var infos []proto.SkillInfo
	for _, sk := range reg.List() {
		infos = append(infos, proto.SkillInfo{
			Name:        sk.Name,
			Description: sk.Description,
			Source:      sk.Source,
			Enabled:     sk.Enabled,
			Missing:     sk.Missing,
		})
	}
	return matchingSkillRunItems(skillCommandsFrom(infos), prefix), infos
}

// findCommand returns the palette entry with the given name.
func findCommand(cmds []command, name string) (command, bool) {
	for _, c := range cmds {
		if c.name == name {
			return c, true
		}
	}
	return command{}, false
}

// TestLiveRun_T11InstalledSkillAppearsInTheCommandPalette writes a skill to
// disk and asks the palette for it — the T11 claim end to end.
//
// The second skill is the control: a palette that returned everything
// regardless of prefix would satisfy the first assertion by accident.
func TestLiveRun_T11InstalledSkillAppearsInTheCommandPalette(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "release-notes", `---
name: release-notes
description: Draft release notes from the commit range.
---

# release-notes

Read the log, group by scope, write the notes.
`)
	writeSkill(t, root, "unrelated-thing", `---
name: unrelated-thing
description: Something else entirely.
---

# unrelated-thing
`)

	cmds, infos := loadPalette(t, root, "")
	names := make([]string, 0, len(cmds))
	for _, c := range cmds {
		names = append(names, c.name)
	}
	t.Logf("registry loaded %d skill(s); palette offers %v", len(infos), names)

	got, ok := findCommand(cmds, "release-notes")
	if !ok {
		t.Fatalf("a skill written to disk never reached the command palette; offered: %v", names)
	}
	if got.disabled {
		t.Errorf("a skill with no unmet requirements is offered as disabled: help=%q", got.help)
	}
	if !strings.Contains(got.help, "Draft release notes") {
		t.Errorf("the palette entry does not carry the skill's own description: %q", got.help)
	}
	if got.kind != cmdKindSkillRun {
		t.Errorf("the entry has kind %v, so completing it would not insert `/skill run …`", got.kind)
	}

	// Prefix filtering works, so the entry above was matched and not merely
	// included in a list of everything.
	filtered, _ := loadPalette(t, root, "release")
	if len(filtered) != 1 || filtered[0].name != "release-notes" {
		var fn []string
		for _, c := range filtered {
			fn = append(fn, c.name)
		}
		t.Errorf("prefix \"release\" offered %v, want just release-notes", fn)
	}
}

// TestLiveRun_T5SkillWithAMissingProgramIsFlaggedNotSilentlyOffered is the T5
// claim: a skill that declares a program this machine does not have must be
// visibly unusable BEFORE the model follows step one and shell_run reports
// "command not found".
//
// The requirement names a binary that cannot exist, so the test does not depend
// on what happens to be installed on the runner.
func TestLiveRun_T5SkillWithAMissingProgramIsFlaggedNotSilentlyOffered(t *testing.T) {
	root := t.TempDir()
	const absent = "yanshi-definitely-not-a-real-program-xyz"
	writeSkill(t, root, "needs-tooling", `---
name: needs-tooling
description: Refactor with a structural search tool.
requires:
  - bin: `+absent+`
---

# needs-tooling

Step 1: run `+absent+` over the tree.
`)
	// A skill requiring a program that certainly exists, as the control: if
	// probing marked EVERYTHING missing, the assertion below would pass for the
	// wrong reason.
	writeSkill(t, root, "needs-nothing-special", `---
name: needs-nothing-special
description: Plain skill with no external programs.
---

# needs-nothing-special
`)

	cmds, infos := loadPalette(t, root, "")
	for _, i := range infos {
		t.Logf("registry: %s enabled=%v missing=%v", i.Name, i.Enabled, i.Missing)
	}

	broken, ok := findCommand(cmds, "needs-tooling")
	require.True(t, ok, "the skill must still be LISTED; hiding it would leave the user unable to see why")
	t.Logf("palette entry: name=%s disabled=%v help=%q", broken.name, broken.disabled, broken.help)

	if !broken.disabled {
		t.Errorf("a skill declaring a program that is not installed is offered as runnable; "+
			"the user finds out when shell_run says \"command not found\" mid-turn (help=%q)",
			broken.help)
	}
	if !strings.Contains(broken.help, absent) {
		t.Errorf("the palette entry does not name the missing program, so the user cannot "+
			"fix it: %q", broken.help)
	}

	fine, ok := findCommand(cmds, "needs-nothing-special")
	require.True(t, ok)
	if fine.disabled {
		t.Errorf("a skill with no requirements was also marked unavailable (%q); the probe "+
			"is failing everything rather than detecting anything", fine.help)
	}

	// And the registry itself must carry the reason, not just the UI: the probe
	// is of the BACKEND's PATH, so a client-side recomputation would answer a
	// different question in remote mode.
	var flagged *proto.SkillInfo
	for i := range infos {
		if infos[i].Name == "needs-tooling" {
			flagged = &infos[i]
		}
	}
	require.NotNil(t, flagged)
	if len(flagged.Missing) == 0 {
		t.Errorf("SkillInfo.Missing is empty for a skill with an unmet requirement; " +
			"a remote TUI would show it as perfectly usable")
	} else if flagged.Missing[0] != absent {
		t.Errorf("Missing = %v, want the declared program %q", flagged.Missing, absent)
	}
}

// TestLiveRun_T5UnknownRequirementKindIsRefusedAtInstallNotAtLoad records where
// the refusal actually lives, because the first version of this test looked in
// the wrong place and reported a defect that is not one.
//
// requires.go says an unknown requirement kind is "REJECTED rather than
// ignored". The rejection is at INSTALL time (ValidateSkillDir, which both the
// git and HTTP install paths and the WS validate verb run). Load deliberately
// does NOT reject: its contract is "one bad skill never fails the whole load",
// and a dependency declaration is metadata rather than the skill — dropping a
// pack over a frontmatter typo would be a silent uninstall of something already
// on disk.
//
// So the honest assertion is a pair: the gate refuses the pack, and a pack that
// nevertheless reaches the disk still loads. Asserting only the second (as the
// first draft did) reads as "the requirement is silently meaningless", which is
// wrong, and asserting only the first would miss that the load path has to stay
// forgiving.
func TestLiveRun_T5UnknownRequirementKindIsRefusedAtInstallNotAtLoad(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "future-pack", `---
name: future-pack
description: Declares a requirement kind that does not exist yet.
requires:
  - python-package: pandas
---

# future-pack
`)
	dir := filepath.Join(root, "future-pack")

	// The gate every install path runs must refuse it, and say why.
	err := skills.ValidateSkillDir(dir)
	t.Logf("ValidateSkillDir -> %v", err)
	if err == nil {
		t.Fatalf("the install-time gate accepted an unrecognised requirement kind; " +
			"a pack can ship a requirement that means nothing")
	}
	if !strings.Contains(err.Error(), "requires") {
		t.Errorf("the refusal does not point at the requires block: %v", err)
	}

	// And a pack already on disk still loads, because one bad frontmatter key
	// must not remove a skill the user installed earlier.
	reg, lerr := skills.NewLoader(skills.User(root)).Load()
	require.NoError(t, lerr, "one malformed skill must not fail the whole load")
	sk, ok := reg.Get("future-pack")
	require.True(t, ok, "the pack must still load; dropping it is a silent uninstall")
	t.Logf("loaded anyway: enabled=%v requires=%v missing=%v", sk.Enabled, sk.Requires, sk.Missing)

	// The unparseable entry must have been normalised away rather than probed:
	// an empty requirement resolves as missing on EVERY machine, which would
	// suppress the skill everywhere.
	for _, r := range sk.Requires {
		if r.Bin == "" {
			t.Errorf("an unrecognised requirement survived into Requires as %v; "+
				"probing it would mark the skill unusable on every machine", r)
		}
	}
}
