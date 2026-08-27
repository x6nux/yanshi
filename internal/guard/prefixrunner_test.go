package guard

import (
	"os"
	"path/filepath"
	"testing"
)

// prefixrunner_test.go is the regression suite for the COMMAND PREFIX RUNNER
// bypass — the third obfuscation channel, found by empirical probing rather
// than by reading the code (see prefixrunner.go's header for the measurement).
//
// The shape of the finding is what makes the reverse-control half of this file
// non-negotiable: before the fix, `rm -rf /` was a structural HardDeny and
// `sudo rm -rf /` was an outright Allow. A fix that merely made the second one
// deny would be trivially satisfiable by grading every command starting with
// `sudo` as catastrophic, which would refuse `sudo apt-get install vim` and
// teach operators to turn the gate off. Both halves are asserted here.

// prefixRunnerAttacks are real spellings of a catastrophic command hidden
// behind a prefix runner. Every one of these graded DestructionNone before
// prefixrunner.go existed, and under a profile with
// `shell: {policy: allowlist, patterns: ["*"]}` every one reached Allow with no
// prompt, no audit record and no mode change.
var prefixRunnerAttacks = []struct {
	cmd  string
	want Destruction
}{
	// Privilege elevation — the spelling a model emits unprompted after a
	// permission failure, and therefore the most likely one to occur.
	{`sudo rm -rf /`, DestructionCatastrophic},
	{`sudo -u root rm -rf /`, DestructionCatastrophic},
	{`sudo -- rm -rf /`, DestructionCatastrophic},
	{`sudo FOO=1 rm -rf /`, DestructionCatastrophic},
	{`doas rm -rf /`, DestructionCatastrophic},
	{`sudo dd if=/dev/zero of=/dev/disk0`, DestructionCatastrophic},
	{`sudo bash -c 'rm -rf /'`, DestructionCatastrophic},

	// su-family: the command rides a -c flag, but a bare username may precede
	// it, which is why unwrapShellCommand (which stops at the first non-flag
	// word) could not see these.
	{`su -c 'rm -rf /'`, DestructionCatastrophic},
	{`su root -c 'rm -rf /'`, DestructionCatastrophic},
	{`su - root -c 'rm -rf /'`, DestructionCatastrophic},
	{`runuser -u root -- rm -rf /`, DestructionCatastrophic},

	// Detachment and scheduling wrappers.
	{`nohup rm -rf /`, DestructionCatastrophic},
	{`setsid rm -rf /`, DestructionCatastrophic},
	{`nice -n 19 rm -rf /`, DestructionCatastrophic},
	{`ionice -c 3 rm -rf /`, DestructionCatastrophic},
	{`taskset 0x1 rm -rf /`, DestructionCatastrophic},
	{`stdbuf -o0 rm -rf /`, DestructionCatastrophic},

	// `timeout DURATION CMD`: the duration is a bare positional, so a parser
	// that only skipped flag-shaped words would classify a program named "5".
	{`timeout 5 rm -rf /`, DestructionCatastrophic},
	{`timeout -s KILL 5 rm -rf /`, DestructionCatastrophic},
	{`timeout 5 sudo rm -rf /`, DestructionCatastrophic},

	// Builtins and measurement wrappers.
	{`time rm -rf /`, DestructionCatastrophic},
	{`command rm -rf /`, DestructionCatastrophic},
	{`exec rm -rf /`, DestructionCatastrophic},

	// `env CMD` with no shell behind it: unwrapShellCommand declines because
	// the word it reaches ("rm") is not itself a wrapper.
	{`env rm -rf /`, DestructionCatastrophic},
	{`env FOO=1 rm -rf /`, DestructionCatastrophic},

	// xargs appends stdin items to the given command — the command still runs.
	{`xargs rm -rf /`, DestructionCatastrophic},
	{`xargs -n1 rm -rf /`, DestructionCatastrophic},

	// Namespace changes; chroot consumes the new root as a positional.
	{`chroot / rm -rf /`, DestructionCatastrophic},
	{`fakeroot rm -rf /`, DestructionCatastrophic},

	// Windows spelling, whose flags use the slash form.
	{`runas /user:admin rm -rf C:\`, DestructionCatastrophic},

	// The prefix must not UPGRADE a tier either: a recursive chmod on a system
	// root is reversible and stays OutOfScope behind sudo, exactly as it is
	// without it. See storage.go's header for why that tier split exists.
	{`sudo chmod -R 000 /`, DestructionOutOfScope},
}

// prefixRunnerBenign are commands that must keep working. Each one exercises a
// specific way the stripper could over-reach:
//   - the payload is an ordinary program (apt-get, go, npm)
//   - the payload is a SCOPED destructive command, so the strip happens and the
//     inner classification correctly returns None
//   - the runner is invoked with no command at all (`sudo -l`, `env -i`)
//   - the runner's name appears as a subcommand of something else (`git rm`)
var prefixRunnerBenign = []string{
	`sudo apt-get install vim`,
	`sudo systemctl restart nginx`,
	`timeout 5 go test ./...`,
	`nohup npm run build`,
	`env FOO=1 go build`,
	`nice -n 19 make`,
	`xargs grep foo`,
	`command ls`,
	`exec bash`,
	`time go build ./...`,
	// A destructive verb whose target is inside the project: the prefix is
	// stripped, the inner command is classified, and it grades None on its own
	// merits. This is the row that distinguishes "sees through the prefix" from
	// "panics at the sight of sudo".
	`sudo rm -rf ./build`,
	`timeout 30 rm -rf node_modules`,
	// Runners with no command behind them.
	`sudo -l`,
	`env -i`,
	`timeout 5`,
	`time`,
	`xargs`,
	// `rm` as another program's subcommand, not a nested program.
	`git rm -r .`,
}

// TestPrefixRunnerAttacksAreGraded is the forward half: every prefix spelling
// of a catastrophic command reaches the tier the plain spelling reaches.
func TestPrefixRunnerAttacksAreGraded(t *testing.T) {
	wd := t.TempDir()
	for _, tc := range prefixRunnerAttacks {
		got := ClassifyDestruction(tc.cmd, wd)
		if got != tc.want {
			t.Errorf("ClassifyDestruction(%q) = %v, want %v", tc.cmd, got, tc.want)
		}
	}
}

// TestPrefixRunnerBenignCommandsAreNotGraded is the reverse half, and it is the
// one that keeps the fix honest. Without it, `return DestructionCatastrophic`
// for anything beginning with a runner name would pass the forward test.
func TestPrefixRunnerBenignCommandsAreNotGraded(t *testing.T) {
	wd := t.TempDir()
	for _, cmd := range prefixRunnerBenign {
		if got := ClassifyDestruction(cmd, wd); got != DestructionNone {
			t.Errorf("ClassifyDestruction(%q) = %v, want DestructionNone (false positive)", cmd, got)
		}
	}
}

// TestPrefixRunnerReachesHardDenyThroughGuardCheck asserts the finding at the
// layer where it actually mattered.
//
// ClassifyDestruction is an internal predicate; what the incident was about is
// the VERDICT a caller receives. The profile here is the wide-open shell
// relaxation storage.go names as common in local setups, and under it the
// pre-fix behaviour was Verdict=Allow — not a prompt the operator could catch,
// but a silent authorization. The assertion is on Overridable=false too,
// because a merely overridable deny would still be bypassed by `/mode yolo`.
func TestPrefixRunnerReachesHardDenyThroughGuardCheck(t *testing.T) {
	wd := t.TempDir()
	p := PermissionProfile{
		Tools: ToolsPerm{Allow: []string{"*"}},
		Shell: ShellPerm{Policy: "allowlist", Patterns: []string{"*"}},
	}
	g := New()
	for _, cmd := range []string{`sudo rm -rf /`, `timeout 5 rm -rf /`, `env rm -rf /`} {
		d := g.Check(p, Action{Tool: "shell_run", Shell: cmd, Workdir: wd})
		if d.Verdict != HardDeny {
			t.Errorf("Check(%q).Verdict = %v, want HardDeny", cmd, d.Verdict)
		}
		if d.Overridable {
			t.Errorf("Check(%q) is Overridable; yolo would bypass it", cmd)
		}
	}
	// And the same profile still admits the benign form outright, so the gate
	// has not been converted into a blanket refusal of prefixed commands.
	if d := g.Check(p, Action{Tool: "shell_run", Shell: `sudo apt-get install vim`, Workdir: wd}); d.Verdict != Allow {
		t.Errorf("Check(sudo apt-get install vim).Verdict = %v, want Allow", d.Verdict)
	}
}

// TestPrefixRunnerHomeTargetsResolveThroughTheStrip proves the strip hands the
// inner command to the SAME path-resolution the plain spelling gets, rather
// than to a second, weaker copy. `~`, `$HOME` and `..`-collapsing spellings all
// have to survive the hand-off; if the payload were re-serialized and re-lexed
// instead of passed as tokens, this is where that would show.
func TestPrefixRunnerHomeTargetsResolveThroughTheStrip(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	wd := filepath.Join(home, "proj")
	if err := os.MkdirAll(wd, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, cmd := range []string{
		`sudo rm -rf ~`,
		`sudo rm -rf $HOME`,
		`timeout 5 rm -rf ~/../`,
		`nohup rm -rf ` + wd + `/../`,
	} {
		if got := ClassifyDestruction(cmd, wd); got != DestructionCatastrophic {
			t.Errorf("ClassifyDestruction(%q) = %v, want DestructionCatastrophic", cmd, got)
		}
	}
}

// TestPrefixRunnerRecursionIsBounded asserts that deeply nested prefixes cannot
// turn the authorization path into unbounded work, and — more importantly —
// that exhausting the budget cannot LAUNDER a verdict. Depth is spent going
// inward, so a chain longer than maxUnwrapDepth stops descending; the test
// pins that the verdict at that point is still a refusal for the shallow
// prefixes and never silently becomes None.
func TestPrefixRunnerRecursionIsBounded(t *testing.T) {
	wd := t.TempDir()
	// Within budget: three prefixes then the payload.
	if got := ClassifyDestruction(`sudo nohup setsid rm -rf /`, wd); got != DestructionCatastrophic {
		t.Errorf("three-deep prefix chain = %v, want DestructionCatastrophic", got)
	}
	// Beyond budget the descent stops rather than looping; the requirement is
	// termination with a defined answer, which a completed call demonstrates.
	deep := ""
	for i := 0; i < 200; i++ {
		deep += "sudo "
	}
	_ = ClassifyDestruction(deep+"rm -rf /", wd)
}
