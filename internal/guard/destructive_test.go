package guard

import "testing"

// TestClassifyDestruction covers the destructive-deletion classifier across the
// three levels. The mode gate (yolo/auto) and the checkDestructive dimension are
// covered elsewhere; this pins the pure classification.
func TestClassifyDestruction(t *testing.T) {
	const w = "/home/me/proj" // the workdir / project root
	cases := []struct {
		name string
		cmd  string
		want Destruction
	}{
		// Non-deletion / in-scope.
		{"empty", "", DestructionNone},
		{"non-deletion program", "ls -la", DestructionNone},
		{"rm relative in workdir", "rm foo.txt", DestructionNone},
		{"rm recursive subdir", "rm -rf build", DestructionNone},
		{"rm recursive dotted subdir", "rm -rf ./dist", DestructionNone},
		{"rm absolute inside workdir", "rm -rf /home/me/proj/tmp", DestructionNone},
		{"rmdir in workdir", "rmdir old", DestructionNone},

		// Out-of-workdir deletion (block in yolo, AI in auto).
		{"rm absolute outside", "rm /etc/passwd", DestructionOutOfScope},
		{"rm recursive outside", "rm -rf /opt/other", DestructionOutOfScope},
		{"rm sibling of workdir", "rm -rf /home/me/other", DestructionOutOfScope},
		{"rm home file outside workdir", "rm ~/.ssh/config", DestructionOutOfScope},

		// Catastrophic mass / root deletion (block in ALL modes).
		{"rm -rf root", "rm -rf /", DestructionCatastrophic},
		{"rm -rf star", "rm -rf /*", DestructionCatastrophic},
		{"rm -rf tilde", "rm -rf ~", DestructionCatastrophic},
		{"rm -rf homevar", "rm -rf $HOME", DestructionCatastrophic},
		{"rm -rf bare star", "rm -rf *", DestructionCatastrophic},
		{"rm -rf dot", "rm -rf .", DestructionCatastrophic},
		{"rm -rf dotdot", "rm -rf ..", DestructionCatastrophic},
		{"rm -rf etc", "rm -rf /etc", DestructionCatastrophic},
		{"rm -rf usr", "rm -rf /usr", DestructionCatastrophic},
		{"rm -rf home", "rm -rf /home", DestructionCatastrophic},
		{"rm -rf bare", "rm -rf", DestructionCatastrophic},
		{"rm recursive root no force", "rm -r /", DestructionCatastrophic},
		{"rm -rf windows drive", "rm -rf C:\\", DestructionCatastrophic},
		{"rm -rf workdir itself", "rm -rf /home/me/proj", DestructionCatastrophic},
		{"rm -rf ancestor of workdir", "rm -rf /home/me", DestructionCatastrophic},

		// Chained commands are split and graded segment by segment (INF1);
		// they used to answer None and lean on the shell-metachar HardDeny.
		{"chained rm", "rm -rf / && echo hi", DestructionCatastrophic},
		{"chained rm, dangerous half last", "echo hi ; rm -rf /", DestructionCatastrophic},
		{"chained rm, piped", "echo hi | rm -rf /", DestructionCatastrophic},
		{"chained rm with a redirect", "rm -rf / > /dev/null", DestructionCatastrophic},
		{"chain of benign commands stays None", "ls && echo hi", DestructionNone},
		// A control operator INSIDE quotes is data. The splitter honours quotes
		// while the has-operator scan does not, so this is the shape whose
		// split-and-recurse never shrank its input (measured: stack overflow).
		{"quoted operator is data, not a boundary", `grep "a|b" file.txt`, DestructionNone},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ClassifyDestruction(c.cmd, w)
			if got != c.want {
				t.Fatalf("ClassifyDestruction(%q, %q) = %v, want %v", c.cmd, w, got, c.want)
			}
		})
	}
}

// TestClassifyDestruction_NoWorkdir_FailSafe proves that without a known workdir
// boundary, absolute deletion targets are treated as out-of-scope (fail-safe),
// while relative targets stay in-scope (cannot classify otherwise).
func TestClassifyDestruction_NoWorkdir_FailSafe(t *testing.T) {
	if got := ClassifyDestruction("rm /etc/passwd", ""); got != DestructionOutOfScope {
		t.Fatalf("absolute target with unknown workdir: want OutOfScope, got %v", got)
	}
	if got := ClassifyDestruction("rm foo.txt", ""); got != DestructionNone {
		t.Fatalf("relative target with unknown workdir: want None, got %v", got)
	}
	if got := ClassifyDestruction("rm -rf /", ""); got != DestructionCatastrophic {
		t.Fatalf("root deletion is catastrophic regardless of workdir: got %v", got)
	}
}

// TestCheckDestructiveDimension pins the verdict each classification maps to in
// Guard.Check: catastrophic => structural HardDeny (not Overridable, blocks all
// modes), out-of-scope => Promptable Prompt, none => Allow (profile decides).
func TestCheckDestructiveDimension(t *testing.T) {
	g := New()
	cases := []struct {
		name     string
		action   Action
		verdict  Verdict
		overrid  bool
		promptab bool
	}{
		{"catastrophic blocks all modes", Action{Shell: "rm -rf /", Workdir: "/p"}, HardDeny, false, false},
		{"out-of-scope is promptable", Action{Shell: "rm /etc/passwd", Workdir: "/p"}, Prompt, false, true},
		{"in-scope deletion defers to profile", Action{Shell: "rm foo", Workdir: "/p"}, Allow, false, false},
		{"non-shell is allow", Action{Tool: "fs_read"}, Allow, false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dec := g.checkDestructive(c.action)
			if dec.Verdict != c.verdict || dec.Overridable != c.overrid || dec.Promptable != c.promptab {
				t.Fatalf("%s: got %+v, want verdict=%v overridable=%v promptable=%v", c.name, dec, c.verdict, c.overrid, c.promptab)
			}
		})
	}
}

// TestCheck_CatastrophicBeatsPermissiveProfile proves the destructive dimension
// runs FIRST and short-circuits even a wildcard profile that would allow rm —
// the whole point of the gate (permissive profile + yolo must not run rm -rf /).
func TestCheck_CatastrophicBeatsPermissiveProfile(t *testing.T) {
	g := New()
	prof := PermissionProfile{
		Tools: ToolsPerm{Allow: []string{"shell_run"}},
		Shell: ShellPerm{Policy: "allowlist", Patterns: []string{"rm *"}}, // would allow rm
	}
	dec := g.Check(prof, Action{Tool: "shell_run", Shell: "rm -rf /", Workdir: "/proj"})
	if dec.Verdict != HardDeny || dec.Overridable {
		t.Fatalf("catastrophic deletion must HardDeny even under a permissive profile, got %+v", dec)
	}
}
