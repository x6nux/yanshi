package guard

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/x6nux/yanshi/internal/execpolicy"
)

// # Why a differential test and not another fold property
//
// TestSegmentedShellIsNeverMorePermissiveThanItsSegments is a FOLD property: the
// verdict for a chain must be at least as severe as the verdict for each of its
// segments. Both sides of that comparison are computed by the same reader, so a
// reader that misunderstands a command misunderstands it identically on both
// sides and the property holds vacuously. Measured: two shell forms reached a
// real process while that property was green — a redirection written BEFORE the
// command word (`>/dev/null rm -rf /`, which the deletion splitter read as a
// program called `null`) and `>&file` (which the segmenter read as a descriptor
// duplication with no path).
//
// The property below is a FIDELITY property instead: the reference reading is
// produced by a real shell, not by the code under test. It answers "does guard
// read this command the way /bin/sh does", which is the question the fold
// cannot ask.
//
// # Nothing destructive ever runs
//
// The corpus contains `rm -rf /` on purpose — that is the argv whose handling is
// being measured. Two independent things stop it from meaning anything:
//
//  1. PATH is set to the shim directory and NOTHING else, so the only `rm` the
//     child can resolve is the recorder. A missing shim is "command not found",
//     never the real rm. No corpus entry may name a program by absolute path.
//  2. shimsAreLive fails the test before the corpus runs if the recorder did not
//     capture a probe invocation, so a broken shim cannot degrade into "the test
//     quietly ran the real thing".
//
// # What this harness cannot reach
//
// Wrapper payloads. `bash -c "…"` is one quoted word to the outer shell, so
// measuring what the payload does would mean putting a real interpreter on the
// shim PATH — at which point the shim is no longer a recorder and rule 1 is
// gone. Prefix runners (`sudo`, `nohup`, `timeout`) are out of reach for the
// same reason: emulating them means putting an exec'ing program on the shim
// PATH. Both families are pinned deterministically instead, without a
// subprocess, by TestClassifyDestruction_ObfuscatedAndWrapped and
// TestPrefixRunnersArePenetratedToADepth.
//
// # Where this defence does not exist at all
//
// The whole file is skipped on windows: the reference reading comes from
// /bin/sh, and there is no /bin/sh to ask. CI runs the test matrix on
// [ubuntu, windows, macos], so the WINDOWS LEG HAS NO FIDELITY PROPERTY — it
// runs the deterministic tables only. That is inherent rather than an
// oversight (a windows reference reading would have to come from cmd.exe and
// would be measuring a different language), but it means a bug that only the
// differential property can see ships green on one third of the matrix.

// destructiveShims are the programs the corpus invokes. Each is replaced by a
// recorder that deletes nothing and appends its argv to the witness file.
var destructiveShims = []string{"rm", "dd", "shred"}

// shellReading is what a real /bin/sh did with a command: the programs it
// actually executed (with their real argv, as the program itself saw it) and
// the files that appeared in the working directory, which for this corpus can
// only be redirection targets.
type shellReading struct {
	ran     []string // "rm -rf /", reconstructed from the recorder's own argv
	created []string // file names that did not exist before the command ran
}

// newShellHarness installs the recorders and returns the work directory plus a
// runner. The runner executes cmd through a real /bin/sh and reports what the
// shell — not the guard — made of it.
func newShellHarness(t *testing.T) (workdir string, run func(string) shellReading) {
	t.Helper()
	root := t.TempDir()
	if r, err := filepath.EvalSymlinks(root); err == nil {
		root = r
	}
	shimDir := filepath.Join(root, "bin")
	work := filepath.Join(root, "work")
	for _, d := range []string{shimDir, work} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	witness := filepath.Join(root, "witness.txt")
	for _, name := range destructiveShims {
		script := "#!/bin/sh\nprintf '" + name + " %s\\n' \"$*\" >> " + witness + "\nexit 0\n"
		if err := os.WriteFile(filepath.Join(shimDir, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", root)
	t.Setenv("USERPROFILE", root)

	run = func(cmd string) shellReading {
		t.Helper()
		if err := os.Remove(witness); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		before := dirEntries(t, work)
		// A corpus row is attacker-shaped text run through a real shell, and
		// some of the shapes worth measuring are loops. `until false; do rm -rf
		// /; done` is a legitimate row (the reserved word is the axis) and it
		// never returns — measured: it hung the whole package until this
		// deadline existed, calling the recorder forever. A row that outlives
		// the deadline is killed and reported as having run nothing, which the
		// witness assertion then flags as a mislabelled row rather than passing
		// silently.
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		c := exec.CommandContext(ctx, "/bin/sh", "-c", cmd)
		c.Dir = work
		// PATH carries the shim directory alone: an unshimmed program cannot be
		// resolved at all, so a bug in this harness fails closed.
		c.Env = []string{"PATH=" + shimDir, "HOME=" + root}
		out, err := c.CombinedOutput()
		if err != nil {
			t.Logf("  (%q exited with %v: %s)", cmd, err, strings.TrimSpace(string(out)))
		}
		var r shellReading
		if b, err := os.ReadFile(witness); err == nil {
			for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
				if line = strings.TrimSpace(line); line != "" {
					r.ran = append(r.ran, line)
				}
			}
		}
		for name := range dirEntries(t, work) {
			if !before[name] {
				r.created = append(r.created, name)
			}
		}
		sort.Strings(r.created)
		return r
	}
	return work, run
}

func dirEntries(t *testing.T, dir string) map[string]bool {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]bool{}
	for _, e := range ents {
		m[e.Name()] = true
	}
	return m
}

// fidelityRow is one corpus command plus the one fact about it that guard does
// NOT compute: the verdict its reading must produce.
//
// # Why the expectation is written down instead of derived
//
// The program direction used to assert only `written >= reference`, where BOTH
// sides came from ClassifyDestruction. A grader that returns DestructionNone
// for everything satisfies `0 >= 0` for every row, so gutting classifyLexed
// left this test green while 70 others in the package went red. An ordering
// between two readings of the same broken grader is not a property about the
// shell.
//
// want closes that. It is an ABSOLUTE expectation, and the shell is what keeps
// it honest in both directions:
//
//   - want > DestructionNone asserts the row must grade at least that severe,
//     AND that /bin/sh really invoked one of the recorders. A row that claims
//     to be destructive but runs nothing is a mislabelled row, not a pass.
//   - want == DestructionNone asserts the opposite witness: the shell must
//     have run NO recorder. That is what stops a benign row from being quietly
//     wrong about itself.
//
// # The upper bound, and why it was missing
//
// want was a FLOOR until a re-review pointed the obvious mutation at it: with
// `return DestructionCatastrophic` inserted at the top of classifyLexed — a
// grader that calls `echo hi` an unappealable catastrophe — this whole file
// reported ok. Every assertion was `written >= something`, so over-strictness
// was invisible, and over-strictness is the direction readers_test.go's own
// header calls worse than one more prompt: it produces refusals no mode can
// appeal, on commands that do nothing.
//
// The comparison is now EQUALITY. A row that guard deliberately grades more
// severely than the shell's behaviour warrants sets overStrict to the reason,
// which trades the upper bound for a written justification — and the row must
// then really be over-strict, so a note left behind after the over-strictness
// is fixed fails just as loudly as the over-strictness did.
type fidelityRow struct {
	cmd  string
	want Destruction
	// overStrict, when set, is why this row is graded HARDER than /bin/sh's
	// behaviour warrants. It replaces the equality check with "must be strictly
	// more severe than want", so it cannot be used to pin an ordinary row.
	overStrict string
}

// fidelityCorpus varies the two things guard's reader and the shell's reader
// have been measured to disagree about: WHERE a redirection sits and how it is
// spelled, and HOW THE PROGRAM WORD IS SPELLED. The second axis was absent
// until the re-review added eight rows to the first one's corpus and watched
// seven of them fail — the test's name claims "a shell command" while its
// corpus only ever said "a redirection".
//
// Every entry is a command whose meaning to /bin/sh is measured, not assumed.
var fidelityCorpus = []fidelityRow{
	// Axis 1 — where the redirection sits and how it is spelled.
	{"rm -rf /", DestructionCatastrophic, ""},
	{">out1 rm -rf /", DestructionCatastrophic, ""},
	{"rm >out2 -rf /", DestructionCatastrophic, ""},
	{"rm -rf / >out3", DestructionCatastrophic, ""},
	{"2>out4 rm -rf /", DestructionCatastrophic, ""},
	{">&out5 rm -rf /", DestructionCatastrophic, ""},
	{"rm -rf / >& out6", DestructionCatastrophic, ""},
	{"</dev/null rm -rf /", DestructionCatastrophic, ""},
	{"rm -rf / 2>&1", DestructionCatastrophic, ""},
	{"dd >out7 if=/dev/zero of=/dev/disk0", DestructionCatastrophic, ""},
	{"shred >out8 -u /etc/shadow", DestructionOutOfScope, ""},
	{"echo hi >& out9", DestructionNone, ""},
	{"echo hi > out10", DestructionNone, ""},
	{"echo hi >> out11", DestructionNone, ""},
	{"echo hi &> out12", DestructionNone, ""},
	{"echo hi 2>& out13", DestructionNone, ""},
	{"echo hi 2>&1", DestructionNone, ""},
	{"echo hi", DestructionNone, ""},

	// Axis 2a — quoting and escaping inside the program word. /bin/sh strips
	// all of it before deciding which program to run.
	{`\rm -rf /`, DestructionCatastrophic, ""},
	{`"rm" -rf /`, DestructionCatastrophic, ""},
	{`'rm' -rf /`, DestructionCatastrophic, ""},
	{`r''m -rf /`, DestructionCatastrophic, ""},
	{`$'\x72\x6d' -rf /`, DestructionCatastrophic, ""},
	{`r\m -rf /`, DestructionCatastrophic, ""},
	{`d\d if=/dev/zero of=/dev/disk0`, DestructionCatastrophic, ""},
	{`s\hred -u /etc/shadow`, DestructionOutOfScope, ""},
	{`\r\m -rf /`, DestructionCatastrophic, ""},

	// Axis 2b — assignment prefixes. The shell applies them and runs the word
	// after them; a reader that stops at the first word sees a program called
	// "foo=1".
	{"FOO=1 rm -rf /", DestructionCatastrophic, ""},
	{"FOO=1 BAR=2 rm -rf /", DestructionCatastrophic, ""},
	{"A= rm -rf /", DestructionCatastrophic, ""},
	{"FOO=/tmp/x rm -rf /", DestructionCatastrophic, ""},
	{"_x=1 rm -rf /", DestructionCatastrophic, ""},

	// Axis 2c — compound commands and reserved words. The program word sits
	// after a keyword or a group opener, neither of which is a program.
	{"{ rm -rf /; }", DestructionCatastrophic, ""},
	{"{ rm -rf /;}", DestructionCatastrophic, ""},
	{"! rm -rf /", DestructionCatastrophic, ""},
	{"if true; then rm -rf /; fi", DestructionCatastrophic, ""},
	{"if false; then :; else rm -rf /; fi", DestructionCatastrophic, ""},
	{"for x in 1; do rm -rf /; done", DestructionCatastrophic, ""},
	{"while true; do rm -rf /; break; done", DestructionCatastrophic, ""},

	// Axis 2d — eval, whose argv IS a command however it is quoted.
	{"eval rm -rf /", DestructionCatastrophic, ""},
	{`eval "rm -rf /"`, DestructionCatastrophic, ""},
	{`eval 'rm' '-rf' '/'`, DestructionCatastrophic, ""},

	// Axis 2e — substitution and grouping. The word after the opener is a
	// program, so the opener has to be a boundary.
	{"$(rm -rf /)", DestructionCatastrophic, ""},
	{"`rm -rf /`", DestructionCatastrophic, ""},
	{"(rm -rf /)", DestructionCatastrophic, ""},
	{"case x in x) rm -rf /;; esac", DestructionCatastrophic, ""},

	// Axis 3 — ANSI-C spelled redirection TARGETS. The deletion lexer decoded
	// $'…' and the segment reader did not, so a target could be hidden from the
	// FS dimension while staying perfectly visible to the shell.
	{`echo hi > $'\x6f'ut20`, DestructionNone, ""},
	{`echo hi > ou$'\x74'21`, DestructionNone, ""},
	{`echo hi > $'\x6f\x75\x7422'`, DestructionNone, ""},
	{`echo hi >> $'\x6f'ut23`, DestructionNone, ""},
	{`echo hi > "ou"$'\x74'24`, DestructionNone, ""},

	// Axis 4 — commands /bin/sh reads as harmless and guard reads as worse.
	// This axis exists only because the property gained an upper bound; before
	// that it could not have a row, since every assertion was a floor.
	{`ls \&\& rm -rf /`, DestructionNone,
		"hasControlOperator is a quote-and-escape-unaware Contains scan, so the ESCAPED `\\&\\&` — " +
			"which /bin/sh passes to ls as two operands, running nothing — is split into segments and " +
			"the tail grades Catastrophic. The scan is deliberately over-eager (missing an operator " +
			"is the expensive direction and splitControlSegments hands the string back unchanged when " +
			"the operator turns out to be data), and the escape case is the one shape where over-" +
			"eagerness survives that second opinion. Recorded rather than fixed: the fix is in the " +
			"splitter's escape handling, not in the gate this file measures"},
}

// TestGuardReadsAShellCommandTheWayTheShellDoes is the fidelity property.
//
// For every corpus command it asserts four things, three of them against a
// reference reading produced by /bin/sh itself:
//
//   - FLOOR: a row declared destructive must grade at least as severe as its
//     declared floor. This is the one assertion whose expected value is
//     written down rather than computed, and it is the reason the test cannot
//     pass with a grader that returns DestructionNone for everything.
//   - WITNESS: the shell's own behaviour has to agree with that declaration —
//     a destructive row must have invoked a recorder, a benign row must not.
//     Without this the floor would only be a table of the author's beliefs.
//   - PROGRAMS: whatever the shell actually executed is re-presented to
//     ClassifyDestruction in its plain, unadorned spelling. The command AS
//     WRITTEN must not grade milder than that. This is what catches a splitter
//     that loses the command word to a leading redirection.
//   - REDIRECTION TARGETS: every file the shell created has to appear as a
//     redirect target in ParseCommandList's reading, because that reading is
//     the only thing routing writes into the FS dimension. This is what catches
//     a spelling whose target is dropped on the floor.
//
// The reference side is never hand-written, which is the whole point: a
// hand-written expectation records what the author believed the shell does.
func TestGuardReadsAShellCommandTheWayTheShellDoes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the reference reading comes from /bin/sh")
	}
	work, run := newShellHarness(t)
	shimsAreLive(t, run)

	totalRan, totalCreated, totalFloored, totalOverStrict := 0, 0, 0, 0
	for _, row := range fidelityCorpus {
		cmd := row.cmd
		got := run(cmd)
		totalRan += len(got.ran)
		totalCreated += len(got.created)
		t.Logf("%-42q ran=%q created=%v", cmd, got.ran, got.created)

		written := ClassifyDestruction(cmd, work)

		// VERDICT + WITNESS. The two halves are asserted together because either
		// one alone is satisfiable by a degenerate implementation: the verdict by
		// a corpus that lies about itself, the witness by a grader that grades
		// nothing.
		if row.want > DestructionNone {
			totalFloored++
			if len(got.ran) == 0 {
				t.Errorf("corpus row %q wants %v but /bin/sh invoked no recorder at all; "+
					"the row is mislabelled and proves nothing", cmd, row.want)
			}
			if written < row.want {
				t.Errorf("ClassifyDestruction(%q) = %v, want at least %v — /bin/sh ran %q for it",
					cmd, written, row.want, got.ran)
			}
		} else if len(got.ran) != 0 {
			t.Errorf("corpus row %q is declared benign but /bin/sh executed %q", cmd, got.ran)
		}

		// UPPER BOUND. Without it every assertion in this file is `written >=
		// something`, and a grader that returns DestructionCatastrophic for
		// everything — `echo hi` included — reports ok. Over-strictness produces
		// unappealable refusals of commands that do nothing, which readers_test.go
		// calls the worse of the two directions.
		switch {
		case row.overStrict == "":
			if written != row.want {
				t.Errorf("ClassifyDestruction(%q) = %v, want exactly %v — /bin/sh ran %q for it. "+
					"If guard is deliberately stricter than the shell here, say so in the row's "+
					"overStrict field rather than raising want",
					cmd, written, row.want, got.ran)
			}
		case written <= row.want:
			// Dead-entry detection, the same discipline the repository applies to
			// its debt tables: a justification for over-strictness that is no
			// longer over-strict is a pre-authorization for the next one.
			t.Errorf("corpus row %q carries an overStrict note but grades %v, which is not stricter "+
				"than its declared %v — delete the note and let the equality check stand.\nnote: %s",
				cmd, written, row.want, row.overStrict)
		default:
			totalOverStrict++
		}

		for _, actual := range got.ran {
			reference := ClassifyDestruction(actual, work)
			if written < reference {
				t.Errorf("ClassifyDestruction(%q) = %v, but /bin/sh actually executed %q, "+
					"which grades %v on its own — guard is reading a different command than the shell runs",
					cmd, written, actual, reference)
			}
		}

		seen := map[string]bool{}
		if segs, err := execpolicy.ParseCommandList(cmd); err == nil {
			for _, seg := range segs {
				for _, r := range seg.Redirects {
					if r.Target != "" {
						seen[filepath.Base(r.Target)] = true
					}
				}
			}
		}
		for _, name := range got.created {
			if !seen[name] {
				t.Errorf("/bin/sh created %q while running %q, but ParseCommandList reports no such "+
					"redirect target — the write never reaches the FS dimension", name, cmd)
			}
		}
	}
	// Self-proof (review-checklist.md C-bis): a harness whose shell never ran
	// anything, whose redirections never landed, or whose corpus declared
	// nothing destructive would satisfy every assertion above by asserting
	// about the empty set.
	if totalRan == 0 || totalCreated == 0 || totalFloored == 0 {
		t.Fatalf("the reference shell produced nothing to compare against (ran=%d created=%d floored=%d); "+
			"the corpus is not exercising the harness", totalRan, totalCreated, totalFloored)
	}
	// The upper bound needs its own self-proof for the same reason: a corpus in
	// which every row were excused would satisfy the equality check by never
	// running it.
	if totalOverStrict >= len(fidelityCorpus) {
		t.Fatalf("every corpus row (%d) is excused from the upper bound; the equality check is "+
			"asserting about the empty set", totalOverStrict)
	}
}

// shimsAreLive refuses to continue unless the recorder really intercepted a
// destructive program name. Without it, a chmod that did not stick or a shell
// that ignores PATH would turn every corpus row into "nothing ran", which reads
// exactly like "nothing was supposed to run".
func shimsAreLive(t *testing.T, run func(string) shellReading) {
	t.Helper()
	for _, name := range destructiveShims {
		got := run(name + " --probe")
		if len(got.ran) != 1 || !strings.HasPrefix(got.ran[0], name+" --probe") {
			t.Fatalf("the %q recorder did not intercept the probe (got %q); refusing to run a "+
				"corpus containing destructive argv", name, got.ran)
		}
	}
}
