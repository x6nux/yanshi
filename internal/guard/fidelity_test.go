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
// gone. Those shapes are pinned deterministically instead, without a
// subprocess, by TestClassifyDestruction_ObfuscatedAndWrapped, whose table
// covers ANSI-C encoding, nested wrappers, chains and subshell grouping inside
// a payload.

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

// fidelityCorpus varies WHERE the redirection sits and HOW it is spelled, since
// that is the axis on which guard's reader and the shell's reader diverged.
// Every entry is a command whose meaning to /bin/sh is measured, not assumed.
var fidelityCorpus = []string{
	"rm -rf /",
	">out1 rm -rf /",
	"rm >out2 -rf /",
	"rm -rf / >out3",
	"2>out4 rm -rf /",
	">&out5 rm -rf /",
	"rm -rf / >& out6",
	"</dev/null rm -rf /",
	"rm -rf / 2>&1",
	"dd >out7 if=/dev/zero of=/dev/disk0",
	"shred >out8 -u /etc/shadow",
	"echo hi >& out9",
	"echo hi > out10",
	"echo hi >> out11",
	"echo hi &> out12",
	"echo hi 2>& out13",
	"echo hi 2>&1",
	"echo hi",
}

// TestGuardReadsAShellCommandTheWayTheShellDoes is the fidelity property.
//
// For every corpus command it asserts two things against a reference reading
// produced by /bin/sh itself:
//
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

	totalRan, totalCreated := 0, 0
	for _, cmd := range fidelityCorpus {
		got := run(cmd)
		totalRan += len(got.ran)
		totalCreated += len(got.created)
		t.Logf("%-38q ran=%q created=%v", cmd, got.ran, got.created)

		written := ClassifyDestruction(cmd, work)
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
	// anything, or whose redirections never landed, would satisfy every
	// assertion above by asserting about the empty set.
	if totalRan == 0 || totalCreated == 0 {
		t.Fatalf("the reference shell produced nothing to compare against (ran=%d created=%d); "+
			"the corpus is not exercising the harness", totalRan, totalCreated)
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
