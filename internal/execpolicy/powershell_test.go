package execpolicy

import "testing"

// TestParsePowerShellReadsPathsTheWayPowerShellDoes is W-B-05's reason to
// exist. The POSIX reader treats `\` as an escape, so it dissolves every path
// separator in a Windows path; PowerShell treats it as an ordinary character
// and uses the backtick to escape.
//
// The second row is the Windows spelling of the ANSI-C credential bypass: the
// guard's denylist matches on a `.ssh` path segment, and the POSIX reading has
// no path segments left to match.
func TestParsePowerShellReadsPathsTheWayPowerShellDoes(t *testing.T) {
	for _, c := range []struct {
		raw        string
		wantProg   string
		wantArgs   []string
		wantTarget string
		posixReads string // what ParseCommandList makes of the same string
	}{
		{`Remove-Item -Recurse C:\temp`, "remove-item", []string{"-Recurse", `C:\temp`}, "", "C:temp"},
		{`echo x > $env:USERPROFILE\.ssh\authorized_keys`, "echo", []string{"x"},
			`$env:USERPROFILE\.ssh\authorized_keys`, "$env:USERPROFILE.sshauthorized_keys"},
		{`Get-Content C:\a\b.txt`, "get-content", []string{`C:\a\b.txt`}, "", "C:ab.txt"},
	} {
		segs, err := ParsePowerShellCommandList(c.raw)
		if err != nil {
			t.Errorf("ParsePowerShellCommandList(%q) = error %v", c.raw, err)
			continue
		}
		if len(segs) != 1 {
			t.Errorf("ParsePowerShellCommandList(%q) produced %d segments, want 1", c.raw, len(segs))
			continue
		}
		if segs[0].Program != c.wantProg {
			t.Errorf("ParsePowerShellCommandList(%q).Program = %q, want %q", c.raw, segs[0].Program, c.wantProg)
		}
		if got := joinWords(segs[0].Args); got != joinWords(c.wantArgs) {
			t.Errorf("ParsePowerShellCommandList(%q).Args = %q, want %q", c.raw, segs[0].Args, c.wantArgs)
		}
		target := ""
		for _, r := range segs[0].Redirects {
			if r.Target != "" {
				target = r.Target
			}
		}
		if target != c.wantTarget {
			t.Errorf("ParsePowerShellCommandList(%q) redirect target = %q, want %q", c.raw, target, c.wantTarget)
		}
		// And the difference is real: the POSIX reader on the same string
		// produces the collapsed spelling. If this stops holding, either the
		// POSIX reader changed or this test is comparing two identical readers
		// and proves nothing.
		posix, err := ParseCommandList(c.raw)
		if err == nil && len(posix) == 1 {
			all := append([]string{posix[0].Program}, posix[0].Args...)
			for _, r := range posix[0].Redirects {
				all = append(all, r.Target)
			}
			found := false
			for _, w := range all {
				if w == c.posixReads {
					found = true
				}
			}
			if !found {
				t.Errorf("the POSIX reader no longer produces %q for %q (it produced %q); the two "+
					"readers may have converged, in which case this test compares nothing",
					c.posixReads, c.raw, all)
			}
		}
	}
}

// TestParsePowerShellEscapeAndQuoting covers the word layer PowerShell has and
// sh does not: the backtick escape, doubled quotes inside both quote kinds, and
// the fact that interpolation is left as written (leniency about word CONTENT,
// the same contract ParseCommandList has for `$HOME`).
func TestParsePowerShellEscapeAndQuoting(t *testing.T) {
	for _, c := range []struct {
		raw  string
		prog string
		args []string
	}{
		{"Write-Output a`;b", "write-output", []string{"a;b"}},
		{"Write-Output a` b", "write-output", []string{"a b"}},
		{"Write-Output 'it''s'", "write-output", []string{"it's"}},
		{`Write-Output "say ""hi"""`, "write-output", []string{`say "hi"`}},
		{"Write-Output \"a`\"b\"", "write-output", []string{`a"b`}},
		{`Write-Output "$env:HOME/x"`, "write-output", []string{"$env:HOME/x"}},
		{`Write-Output $env:HOME\x`, "write-output", []string{`$env:HOME\x`}},
		{`Get-ChildItem *.go`, "get-childitem", []string{"*.go"}},
	} {
		segs, err := ParsePowerShellCommandList(c.raw)
		if err != nil {
			t.Errorf("ParsePowerShellCommandList(%q) = error %v", c.raw, err)
			continue
		}
		if segs[0].Program != c.prog || joinWords(segs[0].Args) != joinWords(c.args) {
			t.Errorf("ParsePowerShellCommandList(%q) = %q %q, want %q %q",
				c.raw, segs[0].Program, segs[0].Args, c.prog, c.args)
		}
	}
}

// TestParsePowerShellSplitsAndRedirects pins the structure layer: where the
// segment boundaries fall, and which stream a redirection names.
func TestParsePowerShellSplitsAndRedirects(t *testing.T) {
	for _, c := range []struct {
		raw   string
		texts []string
	}{
		{"Get-Process; Get-Service", []string{"Get-Process", "Get-Service"}},
		{"Get-Process | Select-Object Name", []string{"Get-Process", "Select-Object Name"}},
		{"Get-Process && Get-Service", []string{"Get-Process", "Get-Service"}},
		{"Get-Process || Get-Service", []string{"Get-Process", "Get-Service"}},
		{"Get-Process", []string{"Get-Process"}},
	} {
		segs, err := ParsePowerShellCommandList(c.raw)
		if err != nil {
			t.Errorf("ParsePowerShellCommandList(%q) = error %v", c.raw, err)
			continue
		}
		if len(segs) != len(c.texts) {
			t.Errorf("ParsePowerShellCommandList(%q) produced %d segments, want %d",
				c.raw, len(segs), len(c.texts))
			continue
		}
		for i, seg := range segs {
			if seg.Text != c.texts[i] {
				t.Errorf("ParsePowerShellCommandList(%q) segment %d Text = %q, want %q",
					c.raw, i, seg.Text, c.texts[i])
			}
		}
	}
	for _, c := range []struct {
		raw    string
		op     string
		target string
	}{
		{"Get-Process > out.txt", ">", "out.txt"},
		{"Get-Process >> out.txt", ">>", "out.txt"},
		{"Get-Process 2> err.txt", "2>", "err.txt"},
		{"Get-Process 2>> err.txt", "2>>", "err.txt"},
		{"Get-Process *> all.txt", "*>", "all.txt"},
		{"Get-Process 2>&1", "2>&1", ""},
		{"Get-Process *>&1", "*>&1", ""},
		// A digit that is part of the program word is not a stream selector.
		{"Get-Thing2 > out.txt", ">", "out.txt"},
	} {
		segs, err := ParsePowerShellCommandList(c.raw)
		if err != nil {
			t.Errorf("ParsePowerShellCommandList(%q) = error %v", c.raw, err)
			continue
		}
		if len(segs[0].Redirects) != 1 {
			t.Errorf("ParsePowerShellCommandList(%q) has %d redirects, want 1", c.raw, len(segs[0].Redirects))
			continue
		}
		r := segs[0].Redirects[0]
		if r.Operator != c.op || r.Target != c.target {
			t.Errorf("ParsePowerShellCommandList(%q) redirect = %q -> %q, want %q -> %q",
				c.raw, r.Operator, r.Target, c.op, c.target)
		}
	}
	if segs, err := ParsePowerShellCommandList("Get-Thing2 > out.txt"); err == nil {
		if segs[0].Program != "get-thing2" {
			t.Errorf(`ParsePowerShellCommandList("Get-Thing2 > out.txt").Program = %q, want "get-thing2" `+
				`— the trailing digit belongs to the program word, not to the redirection`, segs[0].Program)
		}
	}
}

// TestParsePowerShellRefusesUnreadableStructure is the acceptance clause "解析
// 失败走 HardDeny": every construct that makes "where does this command end" or
// "where does this redirection point" a guess has to come back as an error, and
// guard.checkShell turns an error here into a structural HardDeny.
func TestParsePowerShellRefusesUnreadableStructure(t *testing.T) {
	for _, raw := range []string{
		`Write-Output $(Remove-Item -Recurse C:\)`,
		`Write-Output "$(Remove-Item -Recurse C:\)"`,
		`Write-Output ${x}`,
		`Write-Output @(1,2)`,
		"Write-Output @\"\nhi\n\"@",
		`(Remove-Item -Recurse C:\)`,
		`Get-ChildItem | Where-Object { $_.Name -eq "x" }`,
		`& 'C:\evil.exe'`,
		`Start-Process notepad &`,
		`Get-Process # comment`,
		"Get-Process\nGet-Service",
		`Get-Content < in.txt`,
		`Write-Output 'unterminated`,
		`Write-Output "unterminated`,
		"Write-Output a`",
		`Get-Process >`,
		`Get-Process 2>&9`,
		`; Get-Process`,
		`Get-Process ;; Get-Service`,
		``,
	} {
		if _, err := ParsePowerShellCommandList(raw); err == nil {
			t.Errorf("ParsePowerShellCommandList(%q) = nil error; an unreadable structure must "+
				"fail closed, because guard.checkShell turns exactly this error into the "+
				"structural HardDeny that stands in for it", raw)
		}
	}
}

func joinWords(w []string) string {
	out := ""
	for _, s := range w {
		out += "\x00" + s
	}
	return out
}
