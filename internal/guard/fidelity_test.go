package guard

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

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
		c := exec.Command("/bin/sh", "-c", cmd)
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
// NOT compute: the floor its verdict may not sink below.
//
// # Why the floor is written down instead of derived
//
// The program direction used to assert only `written >= reference`, where BOTH
// sides came from ClassifyDestruction. A grader that returns DestructionNone
// for everything satisfies `0 >= 0` for every row, so gutting classifyLexed
// left this test green while 70 others in the package went red. An ordering
// between two readings of the same broken grader is not a property about the
// shell.
//
// floor closes that. It is an ABSOLUTE expectation, and the shell is what keeps
// it honest in both directions:
//
//   - floor > DestructionNone asserts the row must grade at least that severe,
//     AND that /bin/sh really invoked one of the recorders. A row that claims
//     to be destructive but runs nothing is a mislabelled row, not a pass.
//   - floor == DestructionNone asserts the opposite witness: the shell must
//     have run NO recorder. That is what stops a benign row from being quietly
//     wrong about itself.
type fidelityRow struct {
	cmd   string
	floor Destruction
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
	{"rm -rf /", DestructionCatastrophic},
	{">out1 rm -rf /", DestructionCatastrophic},
	{"rm >out2 -rf /", DestructionCatastrophic},
	{"rm -rf / >out3", DestructionCatastrophic},
	{"2>out4 rm -rf /", DestructionCatastrophic},
	{">&out5 rm -rf /", DestructionCatastrophic},
	{"rm -rf / >& out6", DestructionCatastrophic},
	{"</dev/null rm -rf /", DestructionCatastrophic},
	{"rm -rf / 2>&1", DestructionCatastrophic},
	{"dd >out7 if=/dev/zero of=/dev/disk0", DestructionCatastrophic},
	{"shred >out8 -u /etc/shadow", DestructionOutOfScope},
	{"echo hi >& out9", DestructionNone},
	{"echo hi > out10", DestructionNone},
	{"echo hi >> out11", DestructionNone},
	{"echo hi &> out12", DestructionNone},
	{"echo hi 2>& out13", DestructionNone},
	{"echo hi 2>&1", DestructionNone},
	{"echo hi", DestructionNone},

	// Axis 2a — quoting and escaping inside the program word. /bin/sh strips
	// all of it before deciding which program to run.
	{`\rm -rf /`, DestructionCatastrophic},
	{`"rm" -rf /`, DestructionCatastrophic},
	{`'rm' -rf /`, DestructionCatastrophic},
	{`r''m -rf /`, DestructionCatastrophic},
	{`$'\x72\x6d' -rf /`, DestructionCatastrophic},
	{`r\m -rf /`, DestructionCatastrophic},
	{`d\d if=/dev/zero of=/dev/disk0`, DestructionCatastrophic},
	{`s\hred -u /etc/shadow`, DestructionOutOfScope},
	{`\r\m -rf /`, DestructionCatastrophic},

	// Axis 2b — assignment prefixes. The shell applies them and runs the word
	// after them; a reader that stops at the first word sees a program called
	// "foo=1".
	{"FOO=1 rm -rf /", DestructionCatastrophic},
	{"FOO=1 BAR=2 rm -rf /", DestructionCatastrophic},
	{"A= rm -rf /", DestructionCatastrophic},
	{"FOO=/tmp/x rm -rf /", DestructionCatastrophic},
	{"_x=1 rm -rf /", DestructionCatastrophic},

	// Axis 2c — compound commands and reserved words. The program word sits
	// after a keyword or a group opener, neither of which is a program.
	{"{ rm -rf /; }", DestructionCatastrophic},
	{"{ rm -rf /;}", DestructionCatastrophic},
	{"! rm -rf /", DestructionCatastrophic},
	{"if true; then rm -rf /; fi", DestructionCatastrophic},
	{"if false; then :; else rm -rf /; fi", DestructionCatastrophic},
	{"for x in 1; do rm -rf /; done", DestructionCatastrophic},
	{"while true; do rm -rf /; break; done", DestructionCatastrophic},

	// Axis 2d — eval, whose argv IS a command however it is quoted.
	{"eval rm -rf /", DestructionCatastrophic},
	{`eval "rm -rf /"`, DestructionCatastrophic},
	{`eval 'rm' '-rf' '/'`, DestructionCatastrophic},

	// Axis 2e — substitution and grouping. The word after the opener is a
	// program, so the opener has to be a boundary.
	{"$(rm -rf /)", DestructionCatastrophic},
	{"`rm -rf /`", DestructionCatastrophic},
	{"(rm -rf /)", DestructionCatastrophic},
	{"case x in x) rm -rf /;; esac", DestructionCatastrophic},
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

	totalRan, totalCreated, totalFloored := 0, 0, 0
	for _, row := range fidelityCorpus {
		cmd := row.cmd
		got := run(cmd)
		totalRan += len(got.ran)
		totalCreated += len(got.created)
		t.Logf("%-42q ran=%q created=%v", cmd, got.ran, got.created)

		written := ClassifyDestruction(cmd, work)

		// FLOOR + WITNESS. The two halves are asserted together because either
		// one alone is satisfiable by a degenerate implementation: the floor by
		// a corpus that lies about itself, the witness by a grader that grades
		// nothing.
		if row.floor > DestructionNone {
			totalFloored++
			if len(got.ran) == 0 {
				t.Errorf("corpus row %q declares floor %v but /bin/sh invoked no recorder at all; "+
					"the row is mislabelled and proves nothing", cmd, row.floor)
			}
			if written < row.floor {
				t.Errorf("ClassifyDestruction(%q) = %v, want at least %v — /bin/sh ran %q for it",
					cmd, written, row.floor, got.ran)
			}
		} else if len(got.ran) != 0 {
			t.Errorf("corpus row %q is declared benign but /bin/sh executed %q", cmd, got.ran)
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
