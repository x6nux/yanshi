package guard

import (
	"strings"
	"testing"
)

func psAction(cmd string) Action {
	return Action{Tool: "shell_run", Shell: cmd, Workdir: segTestWorkdir, Interpreter: "powershell"}
}

// TestPowerShellCommandsAreReadByThePowerShellReader is the Check-level half of
// W-B-05. The parser is pinned in internal/execpolicy; what this asserts is
// that guard actually SELECTS it, which is the half a parser test cannot see.
//
// The observable difference is the built-in credential denylist, which matches
// on a `.ssh` PATH SEGMENT. Writing to `~\.ssh\authorized_keys` has such a
// segment only if the backslashes survived the reader; the POSIX reader eats
// them as escapes and hands checkFS the single word `~.sshauthorized_keys`,
// which matches nothing.
//
// Both directions are asserted on the SAME command string, so the difference is
// attributable to the reader and to nothing else. That matters: the obvious
// version of this test compared two different commands under a path-glob
// profile, and the POSIX reading happened to be refused too — for an unrelated
// reason, which would have made the test pass with the reader never selected.
func TestPowerShellCommandsAreReadByThePowerShellReader(t *testing.T) {
	if homeDir() == "" {
		t.Skip("no HOME/USERPROFILE: the credential denylist is home-relative and cannot resolve")
	}
	g := New()
	prof := PermissionProfile{
		Tools: ToolsPerm{Allow: []string{"*"}},
		FS:    FSPerm{Read: []string{"**"}, Write: []string{"**"}},
		Shell: ShellPerm{Policy: "denylist"},
		Net:   NetPerm{Allow: true},
	}
	const cmd = `Write-Output ssh-rsa > ~\.ssh\authorized_keys`

	ps := g.Check(prof, psAction(cmd))
	if ps.Verdict != Prompt || !strings.Contains(ps.Reason, ".ssh") {
		t.Errorf("Check(%q, interpreter=powershell) = {%v %q}, want a Prompt naming .ssh — the "+
			"path segments only survive if the PowerShell reader was selected", cmd, ps.Verdict, ps.Reason)
	}

	posix := psAction(cmd)
	posix.Interpreter = ""
	if d := g.Check(prof, posix); d.Verdict == Prompt && strings.Contains(d.Reason, ".ssh") {
		t.Errorf("the POSIX reader also produces a .ssh Prompt for %q; the two readers agree here, "+
			"so the assertion above no longer demonstrates that a reader was chosen", cmd)
	}
}

// TestPowerShellStructuralRefusalIsAHardDeny is W-B-05's second acceptance
// clause. A PowerShell construct the reader cannot read has to arrive as a
// structural HardDeny — the same tier the POSIX reader's refusals land on —
// rather than as a Prompt someone can click through.
func TestPowerShellStructuralRefusalIsAHardDeny(t *testing.T) {
	g := New()
	prof := PermissionProfile{
		Tools: ToolsPerm{Allow: []string{"*"}},
		FS:    FSPerm{Read: []string{"**"}, Write: []string{"**"}},
		Shell: ShellPerm{Policy: "denylist"},
		Net:   NetPerm{Allow: true},
	}
	// None of these carries a deletion: checkDestructive runs FIRST and would
	// short-circuit with its own reason, and the assertion below would then be
	// about a dimension that is not the one under test. That is not
	// hypothetical — `$(Remove-Item -Recurse C:\)` was the first row written
	// here and it never reached the segmenter.
	for _, cmd := range []string{
		`Write-Output $(Get-Date)`,
		`& 'C:\tools\thing.exe'`,
		`Start-Process notepad &`,
		`Get-ChildItem | Where-Object { $_.Name -eq "x" }`,
		`Write-Output 'unterminated`,
		`Get-Content < in.txt`,
		"Get-Process\nGet-Service",
	} {
		d := g.Check(prof, psAction(cmd))
		if d.Verdict != HardDeny || d.Overridable {
			t.Errorf("Check(%q, interpreter=powershell) = {%v overridable=%v reason=%q}, want a "+
				"structural HardDeny", cmd, d.Verdict, d.Overridable, d.Reason)
			continue
		}
		if !strings.Contains(d.Reason, "shell command rejected") {
			t.Errorf("Check(%q).Reason = %q; want the segmenter's refusal, not another dimension's",
				cmd, d.Reason)
		}
	}
}

// TestPowerShellDeletionCmdletsAreGraded covers the destructive gate for the
// names PowerShell uses. Remove-Item and its aliases are what `rm` is called
// there, and none of them was in deletionPrograms.
//
// The gate itself needs no interpreter: lexShellLite grades the literal and the
// de-escaped reading and keeps the worse, so `C:\` is read as a drive root
// either way. What was missing was only the program name.
func TestPowerShellDeletionCmdletsAreGraded(t *testing.T) {
	for _, tc := range []struct {
		cmd  string
		want Destruction
	}{
		{`Remove-Item -Recurse -Force C:\`, DestructionCatastrophic},
		{`Remove-Item -Recurse -Force /`, DestructionCatastrophic},
		{`remove-item -r -force ~`, DestructionCatastrophic},
		{`ri -Recurse -Force C:\`, DestructionCatastrophic},
		{`Remove-Item C:\Windows\notepad.exe`, DestructionOutOfScope},
		// In-workdir deletion stays ordinary.
		{`Remove-Item -Recurse ` + segTestWorkdir + `/build`, DestructionNone},
	} {
		if got := ClassifyDestruction(tc.cmd, segTestWorkdir); got != tc.want {
			t.Errorf("ClassifyDestruction(%q) = %v, want %v", tc.cmd, got, tc.want)
		}
	}
}
