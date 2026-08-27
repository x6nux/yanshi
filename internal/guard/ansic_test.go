package guard

import "testing"

// TestDecodeANSIC is the table for the $'...' decoder. Every escape form bash
// honors is covered, plus the two behaviours that keep decoding honest:
// unrecognized escapes keep their backslash (bash does the same, so the decoder
// cannot manufacture a token the shell would not produce), and an unterminated
// span is left alone.
func TestDecodeANSIC(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    string
		decoded bool
	}{
		{"no ansi-c span", "rm -rf /tmp", "rm -rf /tmp", false},
		{"hex escapes spell rm -rf /", `$'\x72\x6d -rf /'`, "rm -rf /", true},
		{"single-digit hex", `$'\x7a'`, "z", true},
		{"octal escapes", `$'\162\155'`, "rm", true},
		{"short octal", `$'\11'`, "\t", true},
		{"simple escapes", `$'a\nb\tc\\d'`, "a\nb\tc\\d", true},
		{"escaped quote", `$'it\'s'`, "it's", true},
		{"bell and escape", `$'\a\e'`, "\a\x1b", true},
		{"control char", `$'\cA'`, "\x01", true},
		{"unicode short", `$'A'`, "A", true},
		{"unicode long", `$'\U00000042'`, "B", true},
		{"unknown escape keeps backslash", `$'\q'`, `\q`, true},
		{"bare x with no digits", `$'\x'`, `\x`, true},
		{"span in the middle of a command", `bash -c $'\x72\x6d' /tmp`, "bash -c rm /tmp", true},
		{"multiple spans", `$'\x61'$'\x62'`, "ab", true},
		{"unterminated span is left verbatim", `$'\x72\x6d`, `$'\x72\x6d`, false},
		{"empty span", `$''`, "", true},
		{"dollar without quote is untouched", "$HOME/x", "$HOME/x", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, decoded := decodeANSIC(c.in)
			if got != c.want || decoded != c.decoded {
				t.Fatalf("decodeANSIC(%q) = (%q, %v), want (%q, %v)", c.in, got, decoded, c.want, c.decoded)
			}
		})
	}
}

// TestUnwrapShellCommand covers the wrapper-detection table: which invocations
// carry an inner command, and which merely look like they do.
func TestUnwrapShellCommand(t *testing.T) {
	cases := []struct {
		name    string
		program string
		args    []string
		want    string
		ok      bool
	}{
		{"bash -c", "bash", []string{"-c", "rm -rf /"}, "rm -rf /", true},
		{"sh -c", "sh", []string{"-c", "ls"}, "ls", true},
		{"zsh -c", "zsh", []string{"-c", "ls"}, "ls", true},
		{"sh -lc cluster", "sh", []string{"-lc", "rm -rf /"}, "rm -rf /", true},
		{"bash -ec cluster", "bash", []string{"-ec", "ls"}, "ls", true},
		{"long flag then -c", "bash", []string{"--norc", "-c", "ls"}, "ls", true},
		{"env prefixed wrapper", "env", []string{"FOO=1", "bash", "-c", "rm -rf /"}, "rm -rf /", true},
		{"env with unset", "env", []string{"-u", "PATH", "sh", "-c", "ls"}, "ls", true},

		{"not a wrapper program", "go", []string{"-c", "x"}, "", false},
		{"script path not -c", "bash", []string{"script.sh"}, "", false},
		{"-c with no payload", "bash", []string{"-c"}, "", false},
		{"no args", "bash", nil, "", false},
		{"interactive flag only", "bash", []string{"-i"}, "", false},
		{"env with no program", "env", []string{"FOO=1"}, "", false},
		{"env to a non-wrapper", "env", []string{"FOO=1", "go", "test"}, "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := unwrapShellCommand(c.program, c.args)
			if got != c.want || ok != c.ok {
				t.Fatalf("unwrapShellCommand(%q, %v) = (%q, %v), want (%q, %v)", c.program, c.args, got, ok, c.want, c.ok)
			}
		})
	}
}

// TestClassifyDestruction_ObfuscatedAndWrapped is the S10 regression table.
// Every "want Catastrophic" row here returned DestructionNone before the
// decoder and the wrapper unwrapping existed: lexShellLite saw a program named
// "bash" holding one opaque quoted operand, and the destructive gate has no
// opinion about `bash`.
func TestClassifyDestruction_ObfuscatedAndWrapped(t *testing.T) {
	setHome(t, "/home/me")
	const wd = "/home/me/proj"
	cases := []struct {
		name string
		cmd  string
		want Destruction
	}{
		// ANSI-C quoting hiding the whole command.
		{"hex-encoded rm -rf /", `bash -c $'\x72\x6d -rf /'`, DestructionCatastrophic},
		{"octal-encoded rm -rf /", `bash -c $'\162\155 -rf /'`, DestructionCatastrophic},
		{"ansi-c as the command itself", `$'\x72\x6d' -rf /`, DestructionCatastrophic},
		{"ansi-c hiding only the target", `rm -rf $'\x2f'`, DestructionCatastrophic},
		{"ansi-c hiding the recursive flag", `rm $'\x2d\x72\x66' /`, DestructionCatastrophic},

		// Plain wrappers — no encoding needed, and never caught before either.
		{"bash -c plain", `bash -c "rm -rf /"`, DestructionCatastrophic},
		{"sh -c single quoted", `sh -c 'rm -rf /'`, DestructionCatastrophic},
		{"zsh -lc", `zsh -lc "rm -rf /etc"`, DestructionCatastrophic},
		{"env wrapped", `env FOO=1 bash -c "rm -rf /"`, DestructionCatastrophic},
		{"nested wrappers", `bash -c "sh -c 'rm -rf /'"`, DestructionCatastrophic},
		{"wrapper hiding a collapse", `bash -c "rm -rf ~/.."`, DestructionCatastrophic},

		// Out-of-scope survives the wrapper too.
		{"wrapper hiding out-of-scope", `bash -c "rm /etc/passwd"`, DestructionOutOfScope},

		// The wrapper must not INVENT danger.
		{"wrapper with a benign payload", `bash -c "ls -la"`, DestructionNone},
		{"wrapper deleting inside workdir", `bash -c "rm -rf build"`, DestructionNone},
		{"ansi-c in a benign command", `printf $'\n'`, DestructionNone},
		{"grep with a tab escape", `grep $'\t' file.txt`, DestructionNone},

		// A chain whose operators are VISIBLE in the raw string — even inside a
		// wrapper's quotes — still defers to checkShell's structural
		// metacharacter HardDeny, which tests the raw string and is the
		// stronger refusal of the two.
		{"plain chain defers to metachar deny", `ls && rm -rf /`, DestructionNone},
		{"chain inside a wrapper is still visible to checkShell", `bash -c "ls && rm -rf /"`, DestructionNone},
		// When the operators are ANSI-C ENCODED there is no metacharacter in
		// the raw string, so checkShell will not fire and there is no stronger
		// gate to defer to. Those are classified here instead.
		{"encoded chain inside a wrapper", `bash -c $'ls \x26\x26 rm -rf /'`, DestructionCatastrophic},
		{"encoded chain at top level", `ls $'\x26\x26' rm -rf /`, DestructionCatastrophic},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ClassifyDestruction(c.cmd, wd); got != c.want {
				t.Fatalf("ClassifyDestruction(%q, %q) = %v, want %v", c.cmd, wd, got, c.want)
			}
		})
	}
}

// TestClassifyDestruction_WrapperRecursionIsBounded proves the descent
// terminates on adversarial nesting. An unbounded loop over attacker-controlled
// nesting would be a denial of service on the authorization path itself.
func TestClassifyDestruction_WrapperRecursionIsBounded(t *testing.T) {
	setHome(t, "/home/me")
	cmd := "rm -rf /"
	for i := 0; i < 50; i++ {
		cmd = `bash -c "` + cmd + `"`
	}
	// Deep nesting exceeds the budget, so the payload is no longer reached.
	// The requirement is only that this TERMINATES and returns a verdict; the
	// metacharacter and quoting layers still see the outer string.
	got := ClassifyDestruction(cmd, "/home/me/proj")
	if got != DestructionNone && got != DestructionCatastrophic {
		t.Fatalf("unexpected verdict %v", got)
	}
	// Within the budget the payload IS reached.
	if got := ClassifyDestruction(`bash -c "sh -c 'rm -rf /'"`, "/home/me/proj"); got != DestructionCatastrophic {
		t.Fatalf("two levels of nesting are within budget, want Catastrophic, got %v", got)
	}
}

// TestCheckDestructive_SeesThroughObfuscation is the end-to-end half: the
// decoded/unwrapped classification must actually reach Guard.Check and produce
// a structural, non-overridable HardDeny under a profile permissive enough to
// run the command. Without this, the classifier could be correct while the
// dimension that consumes it stayed wired to the old behaviour.
func TestCheckDestructive_SeesThroughObfuscation(t *testing.T) {
	setHome(t, "/home/me")
	g := New()
	prof := PermissionProfile{
		Tools: ToolsPerm{Allow: []string{"*"}},
		Shell: ShellPerm{Policy: "allowlist", Patterns: []string{"*"}},
	}
	for _, cmd := range []string{
		`bash -c $'\x72\x6d -rf /'`,
		`bash -c "rm -rf /"`,
		`sh -lc 'rm -rf ~'`,
	} {
		t.Run(cmd, func(t *testing.T) {
			d := g.Check(prof, Action{Tool: "shell_run", Shell: cmd, Workdir: "/home/me/proj"})
			if d.Verdict != HardDeny {
				t.Fatalf("verdict = %v, want HardDeny (reason %q)", d.Verdict, d.Reason)
			}
			if d.Overridable {
				t.Fatal("catastrophic deletion must stay structural; YOLO must not override it")
			}
		})
	}
}
