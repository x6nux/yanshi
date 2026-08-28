package guard

import (
	"strings"
	"testing"
)

// TestPrefixRunnersArePenetratedToADepth is W-B-03's first two acceptance
// clauses: a nested wrapper is penetrated to the real program, and the
// penetration is bounded.
//
// The nesting is built rather than written out, so the test measures the actual
// budget instead of restating a constant. The boundary rows are the ones that
// matter: at exactly maxUnwrapDepth the real program is still reached, and one
// level further it is not.
func TestPrefixRunnersArePenetratedToADepth(t *testing.T) {
	// The spec's own example, spelled out.
	if got := ClassifyDestruction("sudo nohup timeout 5 rm -rf /", segTestWorkdir); got != DestructionCatastrophic {
		t.Errorf("ClassifyDestruction(%q) = %v, want Catastrophic — three prefix runners is "+
			"ordinary nesting, not an evasion", "sudo nohup timeout 5 rm -rf /", got)
	}
	// Mixed kinds draw on the same budget: prefix runner, su payload, eval and
	// a shell wrapper are all "the real command is one level further in".
	for _, cmd := range []string{
		`sudo nohup env FOO=1 bash -c "rm -rf /"`,
		`nohup eval rm -rf /`,
		`sudo su -c "eval rm -rf /"`,
		`timeout 5 nice -n 19 stdbuf -o0 rm -rf /`,
		// Six runners deep, written absolutely rather than relative to
		// maxUnwrapDepth: shrinking the budget back towards its old value of 3
		// is not a security regression (exhaustion refuses) but it would turn
		// ordinary over-wrapping into an unappealable denial, and nothing else
		// here would notice.
		`sudo nohup timeout 5 nice -n 19 stdbuf -o0 env FOO=1 rm -rf /`,
	} {
		if got := ClassifyDestruction(cmd, segTestWorkdir); got != DestructionCatastrophic {
			t.Errorf("ClassifyDestruction(%q) = %v, want Catastrophic", cmd, got)
		}
	}

	// Exactly at the budget: still reached. `nohup` is used because it consumes
	// no arguments of its own, so the nesting depth is the repetition count.
	atLimit := strings.Repeat("nohup ", maxUnwrapDepth) + "rm -rf /"
	if got := ClassifyDestruction(atLimit, segTestWorkdir); got != DestructionCatastrophic {
		t.Errorf("ClassifyDestruction(%d x nohup + rm -rf /) = %v, want Catastrophic — the last "+
			"level inside the budget must still be read", maxUnwrapDepth, got)
	}
}

// TestExhaustingTheUnwrapBudgetIsARefusal is W-B-03's third acceptance clause,
// and the one the audit that suggested a depth limit did not state.
//
// Running out of budget must be graded AS THE STRICTEST, not as "whatever we
// could see on the way down". Grade it as what was visible and the limit
// becomes the bypass: the outermost word of a deeply nested command is a
// wrapper, wrappers are in no deletion table, so the command grades None and a
// permissive profile allows it.
//
// Revert the fail-closed branch in classifyLexed (the `else if
// hasNestedCommand` arm) and every row here reports DestructionNone.
func TestExhaustingTheUnwrapBudgetIsARefusal(t *testing.T) {
	beyond := []string{
		strings.Repeat("nohup ", maxUnwrapDepth+1) + "rm -rf /",
		strings.Repeat("nohup ", maxUnwrapDepth+4) + "rm -rf /",
		// Something harmless at the bottom is refused too, and that is the
		// point: at this depth the guard does not know what is at the bottom.
		strings.Repeat("nohup ", maxUnwrapDepth+1) + "echo hi",
		strings.Repeat("sudo ", maxUnwrapDepth+1) + "ls",
		// Mixed kinds exhaust the same budget: the wrapper sits exactly one
		// level past it.
		strings.Repeat("nohup ", maxUnwrapDepth) + `bash -c "rm -rf /"`,
		strings.Repeat("nohup ", maxUnwrapDepth) + `su -c "rm -rf /"`,
		strings.Repeat("nohup ", maxUnwrapDepth) + `eval rm -rf /`,
	}
	for _, cmd := range beyond {
		if got := ClassifyDestruction(cmd, segTestWorkdir); got != DestructionUnreadable {
			t.Errorf("ClassifyDestruction(%q) = %v, want Unreadable — the budget ran out with "+
				"another command still hidden, and the strictest verdict is the only fail-closed one",
				cmd, got)
		}
	}

	// Check level: a structural HardDeny, and one whose reason says what
	// actually happened rather than borrowing the catastrophic wording.
	g := New()
	prof := PermissionProfile{
		Tools: ToolsPerm{Allow: []string{"*"}},
		FS:    FSPerm{Read: []string{"**"}, Write: []string{"**"}},
		Shell: ShellPerm{Policy: "denylist"},
		Net:   NetPerm{Allow: true},
	}
	d := g.Check(prof, segAction(beyond[0]))
	if d.Verdict != HardDeny || d.Overridable {
		t.Fatalf("Check(deeply nested) = {%v overridable=%v}, want a structural HardDeny",
			d.Verdict, d.Overridable)
	}
	if !strings.Contains(d.Reason, "nests command wrappers") {
		t.Errorf("Check(deeply nested).Reason = %q; a refusal that borrows the catastrophic "+
			"wording sends the reader looking for a deletion that is not there", d.Reason)
	}
}

// TestUnwrapBudgetDoesNotRefuseOrdinaryCommands is the false-positive direction.
//
// hasNestedCommand fires only when there is genuinely another command behind
// the current one. A command that merely LOOKS like a runner — `sudo -l`,
// `timeout 5` with nothing after it, `env -i` — has no command behind it, and
// refusing those would break ordinary use at the bottom of any deep nesting.
func TestUnwrapBudgetDoesNotRefuseOrdinaryCommands(t *testing.T) {
	for _, cmd := range []string{
		"ls -la",
		"rm -rf ./build",
		"sudo -l",
		"timeout 5",
		"env -i",
		"eval",
		strings.Repeat("nohup ", maxUnwrapDepth) + "echo hi",
	} {
		if got := ClassifyDestruction(cmd, segTestWorkdir); got == DestructionUnreadable {
			t.Errorf("ClassifyDestruction(%q) = Unreadable; there is no command hidden behind "+
				"this one and the budget was never the reason to refuse it", cmd)
		}
	}
}

// TestHasNestedCommandCoversEveryUnwrappingClassifyLexedPerforms pins the
// pairing hasNestedCommand's doc claims.
//
// One representative of each of the four unwrappings must be recognised. A
// case that classifyLexed unwraps while it has budget but hasNestedCommand does
// not recognise is a hole at depth zero, which is the only depth an attacker
// gets to choose.
func TestHasNestedCommandCoversEveryUnwrappingClassifyLexedPerforms(t *testing.T) {
	for _, tc := range []struct {
		kind    string
		program string
		args    []string
	}{
		{"shell wrapper", "bash", []string{"-c", "rm -rf /"}},
		{"su payload", "su", []string{"root", "-c", "rm -rf /"}},
		{"eval argv", "eval", []string{"rm", "-rf", "/"}},
		{"prefix runner", "sudo", []string{"rm", "-rf", "/"}},
	} {
		if !hasNestedCommand(tc.program, tc.args) {
			t.Errorf("hasNestedCommand(%q, %q) = false for the %s case; classifyLexed unwraps "+
				"this shape while it has budget, so at depth zero it is a hole", tc.program, tc.args, tc.kind)
		}
		// And the same shape really is unwrapped, at depth: without this the
		// test would pass on a hasNestedCommand that says true for everything.
		if got := classifyLexed(tc.program, tc.args, segTestWorkdir, maxUnwrapDepth); got != DestructionCatastrophic {
			t.Errorf("classifyLexed(%q, %q) = %v at full depth, want Catastrophic — the %s case "+
				"is not actually unwrapped", tc.program, tc.args, got, tc.kind)
		}
	}
	for _, tc := range []struct {
		program string
		args    []string
	}{
		{"ls", []string{"-la"}},
		{"rm", []string{"-rf", "./build"}},
		{"sudo", nil},
		{"eval", nil},
	} {
		if hasNestedCommand(tc.program, tc.args) {
			t.Errorf("hasNestedCommand(%q, %q) = true; nothing is hidden behind this",
				tc.program, tc.args)
		}
	}
}
