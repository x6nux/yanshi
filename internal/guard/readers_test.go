package guard

import "testing"

// readers_test.go holds the invariant ADR-0016 puts in place of "be careful".
//
// guard points TWO readers at a shell command — lexShellLite (permissive about
// word CONTENT, because `*` and `$HOME` and `C:\` are the catastrophic forms it
// exists to catch) and execpolicy.ParseCommandList (strict about STRUCTURE,
// because "where does this command end" must never be a guess). Keeping both is
// the decision; they answer different questions and a single parser can only
// satisfy one of the two strictness requirements.
//
// What is NOT allowed is the thing that produced two Blocking-grade bypasses in
// two consecutive reviews: the two readers resolving the same WORD to different
// bytes. One understood ANSI-C and not backslashes (`r\m` read as `m`), the
// other understood backslashes and not ANSI-C (`~/$'\x2e\x73\x73\x68'` read
// literally, so the credential denylist's `~/.ssh` prefix never matched).
//
// The invariant below is the checkable form of "they share a word layer":
// SPELLING DOES NOT CHANGE THE VERDICT. It is directional in both ways on
// purpose — an obfuscated spelling must not be milder than the plain one, and a
// plain command must not become a refusal because someone wrote it with extra
// spaces.

// spellingClasses group commands that mean the SAME THING to a shell. The
// canonical member is written plainly; every variant is a spelling of it that
// /bin/sh resolves identically, several of them measured doing so by
// TestGuardReadsAShellCommandTheWayTheShellDoes.
var spellingClasses = []struct {
	name      string
	canonical string
	variants  []string
}{
	{
		name:      "mass deletion of the root",
		canonical: "rm -rf /",
		variants: []string{
			"rm  -rf   /",
			`"rm" -rf /`,
			`'rm' -rf /`,
			`\rm -rf /`,
			`r\m -rf /`,
			`$'\x72\x6d' -rf /`,
			"FOO=1 rm -rf /",
			"A= rm -rf /",
			"{ rm -rf /; }",
			"! rm -rf /",
			"eval rm -rf /",
			`eval "rm -rf /"`,
			"if true; then rm -rf /; fi",
			"sudo rm -rf /",
			"nohup rm -rf /",
			`bash -c "rm -rf /"`,
			`bash -c 'r\m -rf /'`,
		},
	},
	{
		name:      "shredding a file outside the workdir",
		canonical: "shred -u /etc/shadow",
		variants: []string{
			`s\hred -u /etc/shadow`,
			"FOO=1 shred -u /etc/shadow",
			`$'\x73\x68\x72\x65\x64' -u /etc/shadow`,
		},
	},
	{
		name:      "planting a key in the credential denylist",
		canonical: "echo ssh-rsa AAAA > ~/.ssh/authorized_keys",
		variants: []string{
			"echo ssh-rsa AAAA >> ~/.ssh/authorized_keys",
			"echo ssh-rsa AAAA >& ~/.ssh/authorized_keys",
			`echo ssh-rsa AAAA > ~/$'\x2e\x73\x73\x68'/authorized_keys`,
			`echo ssh-rsa AAAA > ~/.ssh/authorized_$'\x6b'eys`,
			`echo ssh-rsa AAAA > ~/.s\sh/authorized_keys`,
		},
	},
	{
		// The over-refusal direction. A reader that got stricter in the wrong
		// place turns an ordinary write into a structural HardDeny that no mode
		// can override, which is a worse outcome than a prompt.
		name:      "an ordinary redirected write",
		canonical: "echo hi > out.txt",
		variants: []string{
			"echo   hi   >   out.txt",
			"echo hi>out.txt",
			`echo "hi" > out.txt`,
			`echo hi > "out.txt"`,
			`echo hi > $'\x6f'ut.txt`,
			`e\cho hi > out.txt`,
		},
	},
}

// TestSpellingDoesNotChangeTheVerdict is ADR-0016's invariant.
//
// Add a word-layer capability to one reader and not the other and a variant in
// one of these classes stops matching its canonical form. That is the failure
// this test exists to make loud: neither reader is wrong on its own terms, and
// only a comparison BETWEEN them can see it.
//
// Verdict and Overridable are both compared, because the difference between a
// structural refusal and a profile-policy one is the difference between "no
// mode may run this" and "yolo may".
func TestSpellingDoesNotChangeTheVerdict(t *testing.T) {
	if homeDir() == "" {
		t.Skip("no HOME/USERPROFILE: the credential class is home-relative and cannot resolve")
	}
	g := New()
	// The only refusal this profile can produce on its own is the structural
	// floor and the built-in credential denylist, so a difference between two
	// spellings is a difference in how they were READ, never in policy.
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
	for _, class := range spellingClasses {
		want := g.Check(prof, segAction(class.canonical))
		t.Logf("%-42s %-46q -> verdict=%v overridable=%v", class.name, class.canonical,
			want.Verdict, want.Overridable)
		for _, variant := range class.variants {
			got := g.Check(prof, segAction(variant))
			if got.Verdict != want.Verdict || got.Overridable != want.Overridable {
				t.Errorf("%s: Check(%q) = {%v overridable=%v reason=%q}, but the same command "+
					"written %q = {%v overridable=%v} — the two readers resolved this word "+
					"differently", class.name, variant, got.Verdict, got.Overridable, got.Reason,
					class.canonical, want.Verdict, want.Overridable)
			}
		}
	}
	// Self-proof: a class list whose canonical forms all landed on the same
	// verdict would make the comparison above vacuous — every variant would
	// match every canonical. The four classes deliberately span Allow, Prompt
	// and a structural HardDeny.
	seen := map[Verdict]bool{}
	for _, class := range spellingClasses {
		seen[g.Check(prof, segAction(class.canonical)).Verdict] = true
	}
	if len(seen) < 3 {
		t.Fatalf("the canonical commands cover only %d distinct verdicts (%v); a spelling that "+
			"collapsed every class into one would satisfy this test by accident", len(seen), seen)
	}
}
