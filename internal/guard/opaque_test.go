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

// TestATrailingArgvNobodyClaimedIsRefusedRatherThanAllowed is the second half of
// the same property, for the family the first one could not see.
//
// TestAnUnreadPayloadIsRefusedRatherThanAllowed only ever fires on an operand a
// FLAG marked as code. A re-review measured the flagless spelling passing on
// eleven real programs and on two it invented, every one running `rm -rf /`
// under a real /bin/sh. The invented rows are the ones that matter: they are
// what separates "the wrapper table is missing some entries" — a complaint that
// can always be answered with one more row — from "this family had no default".
//
// The first row uses unmodelledInterpreter, which
// TestNoTableModelsTheProbeProgram proves is in no table in this package. If
// the criterion were a program name, that row could not pass.
func TestATrailingArgvNobodyClaimedIsRefusedRatherThanAllowed(t *testing.T) {
	g := New()
	prof := probeProfile()
	for _, tc := range []struct {
		cmd  string
		want wantVerdict
		note string
	}{
		{cmd: unmodelledInterpreter + ` rm -rf /`, want: wantPrompt,
			note: "THE STRUCTURAL CLAIM. This program word is in no table here; the verdict comes " +
				"from the argv reading as a destructive command, not from recognising the wrapper"},
		{cmd: `pkexec rm -rf /`, want: wantPrompt,
			note: "the polkit spelling of `doas`, which IS in prefixRunners — one act, two " +
				"distribution spellings, and before this the table decided which one passed"},
		{cmd: `bwrap --dev-bind / / rm -rf /`, want: wantPrompt,
			note: "two bare operands sit where a generic flag walk expects the command word, which " +
				"is why the scan reads every SUFFIX rather than the first non-flag word"},
		{cmd: unmodelledInterpreter + ` bash -c "rm -rf /"`, want: wantPrompt,
			note: "the suffix is itself a wrapper invocation, so one level of unwrapping has to " +
				"happen inside the scan"},

		// THE OTHER DIRECTION. Without these the rows above are satisfied by a
		// rule that prompts on every argv, which would refuse ordinary work.
		{cmd: `echo rm -rf /`, want: wantAllow,
			note: "scriptEmitters is the relief: echo writes its operands to stdout and executes " +
				"nothing. This is why the tier is capped at a prompt for unknown programs and why " +
				"the one program documented here as NOT executing its argv is exempt outright"},
		{cmd: unmodelledInterpreter + ` rm -rf ./build`, want: wantAllow,
			note: "the suffix reads as an in-workdir deletion, which is not destructive on its own"},
		{cmd: unmodelledInterpreter + ` --flag out10 out11`, want: wantAllow,
			note: "an unknown program is not refused for being unknown"},
		{cmd: `git rm -r ./pkg`, want: wantAllow,
			note: "`rm` as another program's subcommand, scoped inside the workdir"},
	} {
		got := classOf(g.Check(prof, Action{
			Tool: "shell_run", Shell: tc.cmd, Workdir: segTestWorkdir,
		}))
		if got != tc.want {
			t.Errorf("Check(%q) = %s, want %s — %s", tc.cmd, got, tc.want, tc.note)
		}
	}
}

// TestAMisreadRunnerDoesNotStandTheBackstopDown is the finding underneath the
// one above, and it is the more general of the two.
//
// `taskset -c 0 rm -rf /` was Allow because the table entry counted `-c` as a
// value flag AND a CPU-mask positional, so the walk consumed `rm` and handed
// the classifier a program called `-rf`. The verdict was wrong for an ordinary
// reason; what made it a HOLE is that being claimed by a reader set
// classifyLexed's `read` flag, and the fail-closed backstop only ran when
// nothing had claimed the command. "I misread it" produced a weaker answer than
// "I could not read it".
//
// Both halves are pinned: the table entry no longer counts a positional the -c
// spelling does not have, AND the trailing-argv scan runs whatever `read` says.
func TestAMisreadRunnerDoesNotStandTheBackstopDown(t *testing.T) {
	g := New()
	prof := probeProfile()
	for _, cmd := range []string{
		`taskset -c 0 rm -rf /`,
		`taskset -c 0-3 rm -rf /`,
		`taskset --cpu-list 0 rm -rf /`,
		`taskset 0x1 rm -rf /`, // the control: the spelling that was already read correctly
		`taskset -c 0 bash -c "rm -rf /"`,
	} {
		got := classOf(g.Check(prof, Action{Tool: "shell_run", Shell: cmd, Workdir: segTestWorkdir}))
		if got != wantFloor {
			t.Errorf("Check(%q) = %s, want %s — taskset is a runner this package models, so a "+
				"destructive argv behind it gets the full verdict", cmd, got, wantFloor)
		}
	}
	// The reverse direction: the flag walk must not have been "fixed" by making
	// it consume nothing, which would grade the mask itself as a command.
	if got := ClassifyDestruction(`taskset 0x1 ls`, segTestWorkdir); got != DestructionNone {
		t.Errorf("ClassifyDestruction(taskset 0x1 ls) = %v, want None", got)
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
