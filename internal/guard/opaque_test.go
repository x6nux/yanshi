package guard

import "testing"

// unmodelledInterpreter is a program word that appears in NO table in this
// package. TestNoTableModelsTheProbeProgram proves that, so the rows below
// really do measure "a construct the reader does not model" rather than "a
// construct we forgot to add".
const unmodelledInterpreter = "zq-language-nobody-here-reads"

// TestNoTableModelsTheProbeProgram is the premise of the test below it.
//
// Without it, TestAnUnreadPayloadIsRefusedRatherThanAllowed could silently stop
// measuring anything the day somebody adds `zq-language-nobody-here-reads` to a
// table — the verdict would still be a refusal and the test would still pass,
// while the property it claims to prove (an UNKNOWN program's payload is
// refused) went untested.
func TestNoTableModelsTheProbeProgram(t *testing.T) {
	p := unmodelledInterpreter
	if shellWrappers[p] || deletionPrograms[p] || nonInterpreterPrograms[p] ||
		firstOperandCommands[p] || suLikeRunners[p] || posixShellPrograms[p] ||
		scriptEmitters[p] || isStorageDestroyer(p) {
		t.Fatalf("%q is in a table; it can no longer stand in for an unmodelled program", p)
	}
	if _, ok := prefixRunners[p]; ok {
		t.Fatalf("%q is a prefix runner", p)
	}
	if _, ok := windowsShellWrappers[p]; ok {
		t.Fatalf("%q is a windows shell wrapper", p)
	}
	if _, ok := argvWriters[p]; ok {
		t.Fatalf("%q is an argv writer", p)
	}
	if _, ok := remoteShellRunners[p]; ok {
		t.Fatalf("%q is a remote shell runner", p)
	}
	if hasNestedCommand(p, []string{"-c", "anything at all"}) {
		t.Fatalf("%q is unwrapped by some reader after all", p)
	}
}

// TestAnUnreadPayloadIsRefusedRatherThanAllowed is the assertion the whole
// DestructionOpaque tier exists for: HAND THE READER A CONSTRUCT IT DOES NOT
// MODEL AND THE VERDICT IS A REFUSAL, NOT A PASS.
//
// This is the property no amount of adding spellings to tables can produce.
// Five reviews closed 39 measured bypasses by teaching one more construct each
// time, and each review after found more, because the DEFAULT for an unknown
// construct was Allow. Here the default is a prompt, so the unbounded direction
// — every spelling nobody has thought of yet — fails closed.
//
// The rows are chosen to separate the tier from the tables: the first two use a
// program word no table contains (TestNoTableModelsTheProbeProgram), and the
// third is a shell option spelling unwrapShellCommand deliberately does not
// walk past, so it exercises the backstop rather than the interpreter rule.
func TestAnUnreadPayloadIsRefusedRatherThanAllowed(t *testing.T) {
	g := New()
	prof := probeProfile()
	for _, tc := range []struct {
		cmd  string
		want wantVerdict
		note string
	}{
		{cmd: unmodelledInterpreter + ` -c "delete everything; now"`, want: wantPrompt,
			note: "a program in no table, carrying a code-shaped -c payload"},
		{cmd: unmodelledInterpreter + ` -e "wipe($HOME)"`, want: wantPrompt,
			note: "the same, with the other code flag"},
		{cmd: `bash +o posix -c "rm -rf /"`, want: wantPrompt,
			note: "THE BACKSTOP. `+o` is a legitimate shell option spelling that unwrapShellCommand " +
				"does not walk past, so no reader here reaches the payload at all — and the verdict " +
				"is still a refusal. Teaching the flag scan `+` would make this a floor and is " +
				"deliberately NOT done: it is the one-more-spelling move this tier replaces, and " +
				"doing it would leave the backstop with nothing demonstrating it"},

		// THE OTHER DIRECTION. Without these the rows above are satisfied by a
		// rule that refuses everything, which would be a far worse defect than
		// the one being fixed.
		{cmd: unmodelledInterpreter + ` --version`, want: wantAllow,
			note: "no code flag: an unknown program is not refused for being unknown"},
		{cmd: unmodelledInterpreter + ` -c 100`, want: wantAllow,
			note: "a code flag whose operand is an option VALUE, not a program"},
		{cmd: unmodelledInterpreter + ` -c -v out10`, want: wantAllow,
			note: "a code flag followed by another flag"},
	} {
		got := classOf(g.Check(prof, Action{
			Tool: "shell_run", Shell: tc.cmd, Workdir: segTestWorkdir,
		}))
		if got != tc.want {
			t.Errorf("Check(%q) = %s, want %s — %s", tc.cmd, got, tc.want, tc.note)
		}
	}
}

// TestOpaqueRanksBetweenOutOfScopeAndCatastrophic pins the one thing the fold
// in classifyDestruction depends on.
//
// Every reading in that function is combined with maxDestruction, so the
// ORDER of these constants is what decides which reason an operator is shown
// and, for the two HardDeny tiers, whether yolo can buy past it. Moving
// DestructionOpaque above DestructionCatastrophic would silently downgrade
// `python3 -c "…" && rm -rf /` from the structural floor to a prompt.
func TestOpaqueRanksBetweenOutOfScopeAndCatastrophic(t *testing.T) {
	if !(DestructionNone < DestructionOutOfScope &&
		DestructionOutOfScope < DestructionOpaque &&
		DestructionOpaque < DestructionCatastrophic &&
		DestructionCatastrophic < DestructionUnreadable) {
		t.Fatalf("the Destruction order is not None < OutOfScope < Opaque < Catastrophic < Unreadable")
	}
	g := New()
	prof := probeProfile()
	both := `python3 -c "print(1)" && rm -rf /`
	if got := classOf(g.Check(prof, Action{Tool: "shell_run", Shell: both, Workdir: segTestWorkdir})); got != wantFloor {
		t.Errorf("Check(%q) = %s, want %s — an opaque payload beside a catastrophic command must "+
			"not soften it", both, got, wantFloor)
	}
}

// TestOpaqueIsNotTheStructuralFloor pins the tier's OTHER half: it refuses, and
// it refuses appealably.
//
// A Prompt is what lets `python3 -c` stay usable — the operator answers and the
// command runs. Promoting it to hardDeny() would make every interpreter
// invocation permanently unrunnable in default mode and unappealable in yolo,
// which is the cost opaque.go's header refuses to pay and which no verdict-class
// assertion elsewhere would catch (both are "not Allow").
func TestOpaqueIsNotTheStructuralFloor(t *testing.T) {
	g := New()
	d := g.checkDestructive(Action{Tool: "shell_run", Shell: `python3 -c "print(1)"`, Workdir: segTestWorkdir})
	if d.Verdict != Prompt || !d.Promptable {
		t.Fatalf("checkDestructive(python3 -c …) = %+v, want a promptable Prompt", d)
	}
	if d.Reason == "" {
		t.Fatal("the opaque refusal has no reason; an operator cannot act on it")
	}
}
