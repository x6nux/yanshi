package guard

import (
	"strings"
	"testing"
)

// programWordBypasses are the spellings a re-review measured walking past the
// destructive-deletion gate: every one of them was graded DestructionNone,
// reached Guard.Check as Allow under a profile whose only possible refusal is
// the structural floor, and was confirmed executing `rm -rf /` (or its dd /
// shred equivalent) by a real /bin/sh with recorder shims on PATH.
//
// The four independent readings that produced them:
//
//  1. a backslash escape inside the program word — lexShellLite keeps `\`
//     literal so Windows paths survive, then normalizeProgramWord treats it as
//     a path separator, so `r\m` became `m`;
//  2. a bare VAR=value assignment prefix — the program word was `foo=1`, which
//     is in no table this package consults, so every predicate declined;
//  3. a group opener or a shell reserved word in the program-word position —
//     `{`, `!`, `then`, `do`;
//  4. `eval`, whose argv is a command, and which was in neither the prefix
//     runner table nor the wrapper table.
//
// want is the grade the shell's own reading justifies: Catastrophic for a mass
// deletion of `/` and for overwriting a raw device, OutOfScope for shredding a
// file outside the workdir.
var programWordBypasses = []struct {
	cmd  string
	want Destruction
}{
	// 1 — backslash escapes.
	{`r\m -rf /`, DestructionCatastrophic},
	{`d\d if=/dev/zero of=/dev/disk0`, DestructionCatastrophic},
	{`s\hred -u /etc/shadow`, DestructionOutOfScope},
	{`\r\m -rf /`, DestructionCatastrophic},

	// 2 — assignment prefixes.
	{"FOO=1 rm -rf /", DestructionCatastrophic},
	{"FOO=1 BAR=2 rm -rf /", DestructionCatastrophic},
	{"A= rm -rf /", DestructionCatastrophic},
	{"FOO=/tmp/x rm -rf /", DestructionCatastrophic},
	{"_x=1 rm -rf /", DestructionCatastrophic},
	{"FOO=1 dd if=/dev/zero of=/dev/disk0", DestructionCatastrophic},

	// 3 — group openers and reserved words.
	{"{ rm -rf /; }", DestructionCatastrophic},
	{"{ rm -rf /;}", DestructionCatastrophic},
	{"! rm -rf /", DestructionCatastrophic},
	{"if true; then rm -rf /; fi", DestructionCatastrophic},
	{"if false; then :; else rm -rf /; fi", DestructionCatastrophic},
	{"for x in 1; do rm -rf /; done", DestructionCatastrophic},
	{"while true; do rm -rf /; break; done", DestructionCatastrophic},
	{"until false; do rm -rf /; done", DestructionCatastrophic},

	// 4 — eval, in both of its spellings.
	{"eval rm -rf /", DestructionCatastrophic},
	{`eval "rm -rf /"`, DestructionCatastrophic},
	{`eval 'rm' '-rf' '/'`, DestructionCatastrophic},

	// The same four readings inside a wrapper payload, which is where a model
	// that has already been refused once tends to put them.
	{`bash -c "FOO=1 rm -rf /"`, DestructionCatastrophic},
	{`bash -c 'r\m -rf /'`, DestructionCatastrophic},
	{`bash -c "{ rm -rf /; }"`, DestructionCatastrophic},
	{`bash -c "eval rm -rf /"`, DestructionCatastrophic},
	{`sudo FOO=1 rm -rf /`, DestructionCatastrophic},
	{`nohup eval rm -rf /`, DestructionCatastrophic},
}

// TestProgramWordIsFoundBehindEveryShellSpellingOfIt is the regression guard for
// the four readings above. Undo any one of them — take the backslash pass out of
// classifyDestruction, the assignment loop out of lexShellLite, the reserved
// words out of prefixRunners, or the eval branch out of classifyLexed — and the
// corresponding rows report DestructionNone here.
//
// It asserts the CLASSIFIER grade rather than only Check's verdict, because a
// Check-level assertion alone would not say which dimension refused: several of
// these rows also carry a construct execpolicy.ParseCommandList rejects, so the
// shell dimension would produce a structural HardDeny of its own and the test
// would stay green with the deletion gate blind. TestProgramWordBypassesReachAStructuralFloor
// covers the Check level separately and checks the reason text for the same
// reason.
func TestProgramWordIsFoundBehindEveryShellSpellingOfIt(t *testing.T) {
	for _, tc := range programWordBypasses {
		if got := ClassifyDestruction(tc.cmd, segTestWorkdir); got != tc.want {
			t.Errorf("ClassifyDestruction(%q) = %v, want %v — a real /bin/sh runs the "+
				"destructive program for this spelling", tc.cmd, got, tc.want)
		}
	}
}

// TestProgramWordBypassesReachAStructuralFloor is the Check-level half: the
// classifier is only the first of the two things standing between the model and
// a spawned process.
//
// The profile is the one the re-review used — its only possible refusal is the
// structural floor, so an Allow here is an Allow all the way to exec. The
// reason text is asserted so the pass is ATTRIBUTED to checkDestructive: `{ rm
// -rf /; }` would also be refused by the shell dimension if the deletion gate
// went blind again, and a bare "is it HardDeny" assertion cannot tell the two
// apart.
func TestProgramWordBypassesReachAStructuralFloor(t *testing.T) {
	g := New()
	prof := PermissionProfile{
		Tools: ToolsPerm{Allow: []string{"*"}},
		FS:    FSPerm{Read: []string{"**"}, Write: []string{"**"}},
		Shell: ShellPerm{Policy: "denylist"},
		Net:   NetPerm{Allow: true},
	}
	if d := g.Check(prof, segAction("ls")); d.Verdict != Allow {
		t.Fatalf("precondition: an ordinary command must be allowed under this profile, got %v (%s)",
			d.Verdict, d.Reason)
	}
	for _, tc := range programWordBypasses {
		d := g.Check(prof, segAction(tc.cmd))
		switch tc.want {
		case DestructionCatastrophic:
			if d.Verdict != HardDeny || d.Overridable {
				t.Errorf("Check(%q) = {%v overridable=%v reason=%q}, want a structural HardDeny",
					tc.cmd, d.Verdict, d.Overridable, d.Reason)
				continue
			}
			if !strings.Contains(d.Reason, "catastrophic destruction blocked") {
				t.Errorf("Check(%q) was refused by %q, not by the deletion gate — the gate is "+
					"blind again and another dimension happens to cover for it", tc.cmd, d.Reason)
			}
		case DestructionOutOfScope:
			if d.Verdict == Allow {
				t.Errorf("Check(%q) = Allow, want at least a Prompt", tc.cmd)
			}
		}
	}
}

// TestUnescapeWordLettersLeavesOperatorsEscaped pins the direction the
// backslash pass must NOT go.
//
// classifyDestruction grades the de-escaped spelling as well as the literal
// one, and re-splits the result. That is only bounded because
// unescapeWordLetters touches letters and digits and nothing else: unescaping
// `\&` would let this pass CREATE a control operator out of a string that
// carries none, which is precisely the invariant lexShellLite's header protects
// on the ANSI-C side ("decoding can reveal a target but can never manufacture a
// chain").
//
// The assertion is on the function rather than on a verdict, because a verdict
// cannot isolate it: hasControlOperator is a deliberately quote- and
// escape-unaware substring scan, so `ls \&\& rm -rf /` was already split at the
// ampersand and graded Catastrophic before this pass existed. That over-refusal
// is pre-existing and documented on hasControlOperator; what is new here is only
// that it must not GROW.
func TestUnescapeWordLettersLeavesOperatorsEscaped(t *testing.T) {
	for _, tc := range []struct {
		in, want string
		changed  bool
	}{
		{`r\m -rf /`, `rm -rf /`, true},
		{`\r\m`, `rm`, true},
		{`d\d if=/dev/zero`, `dd if=/dev/zero`, true},
		{`s\1`, `s1`, true},
		{`rm -rf /`, `rm -rf /`, false},
		// Operators keep their backslash: unescaping any of these would turn a
		// string with no control operator into one that has several.
		{`ls \&\& rm -rf /`, `ls \&\& rm -rf /`, false},
		{`echo \; rm`, `echo \; rm`, false},
		{`echo \| rm`, `echo \| rm`, false},
		{`echo \> rm`, `echo \> rm`, false},
		{`echo \$\(rm\)`, `echo \$\(rm\)`, false},
		{"echo \\`rm\\`", "echo \\`rm\\`", false},
		// An escaped space joins two words into one; unescaping it would split
		// `rm\ -rf` (a program nothing can resolve) into a deletion command.
		{`rm\ -rf /`, `rm\ -rf /`, false},
		// A trailing backslash has nothing to escape.
		{`rm \`, `rm \`, false},
		// Windows paths keep their separators. De-escaping a UNC share
		// collapses it into a whole-share deletion that no shell would perform.
		{`rm -rf \\server\share\proj\build`, `rm -rf \\server\share\proj\build`, false},
		{`rm -rf C:\Users\me`, `rm -rf C:\Users\me`, false},
		{`rm -rf proj\build`, `rm -rf projbuild`, true},
	} {
		got, changed := unescapeWordLetters(tc.in)
		if got != tc.want || changed != tc.changed {
			t.Errorf("unescapeWordLetters(%q) = (%q, %v), want (%q, %v)",
				tc.in, got, changed, tc.want, tc.changed)
		}
	}
}

// TestBackslashPassKeepsTheWindowsPathReading is the other half of "grade both
// spellings, keep the worse".
//
// The de-escaped reading of `C:\Users\me` is `C:Usersme`, a relative path that
// resolves INSIDE the workdir. If the literal reading were replaced rather than
// joined, these would silently drop from OutOfScope to None on the platform
// where backslash really is a path separator.
func TestBackslashPassKeepsTheWindowsPathReading(t *testing.T) {
	for _, tc := range []struct {
		cmd  string
		want Destruction
	}{
		{`rm -rf C:\Users\me`, DestructionOutOfScope},
		{`del /s C:\`, DestructionCatastrophic},
		{`rd /s /q C:\Windows`, DestructionOutOfScope},
	} {
		if got := ClassifyDestruction(tc.cmd, segTestWorkdir); got != tc.want {
			t.Errorf("ClassifyDestruction(%q) = %v, want %v — the literal backslash reading is "+
				"the Windows one and must survive the de-escaping pass", tc.cmd, got, tc.want)
		}
	}
}

// TestAssignmentWordShape pins isAssignmentWord against the two mistakes that
// would matter: reading a flag or an operand as an assignment (which would make
// the lexer skip a real program word) and failing to read a legal one.
func TestAssignmentWordShape(t *testing.T) {
	for _, w := range []string{"A=", "A=1", "_x=1", "FOO_BAR2=/tmp/x", "a=b=c"} {
		if !isAssignmentWord(w) {
			t.Errorf("isAssignmentWord(%q) = false, want true", w)
		}
	}
	for _, w := range []string{"", "=", "=1", "rm", "-rf", "--flag=1", "2=x", "a-b=c", "./x=y", "a b=c"} {
		if isAssignmentWord(w) {
			t.Errorf("isAssignmentWord(%q) = true, want false — skipping this word would hide a "+
				"real program word from the deletion gate", w)
		}
	}
}
