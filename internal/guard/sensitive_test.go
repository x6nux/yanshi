package guard

import (
	"strings"
	"testing"
)

// permissiveProfile is the shipped config.example.yaml `coding` shape — the one
// whose `fs: { read: ["**"], write: ["**"] }` is the reason sensitive.go
// exists. Tests use it so the assertions are about the configuration operators
// actually run, not a strawman.
func permissiveProfile() PermissionProfile {
	return PermissionProfile{
		Tools: ToolsPerm{Allow: []string{"*"}},
		FS:    FSPerm{Read: []string{"**"}, Write: []string{"**"}},
	}
}

// TestIsSensitivePath is the table for the built-in credential denylist. The
// "not sensitive" rows are as load-bearing as the "sensitive" ones: a gate that
// prompts on ordinary project files is a gate operators switch off.
func TestIsSensitivePath(t *testing.T) {
	setHome(t, "/home/me")
	const wd = "/home/me/proj"
	cases := []struct {
		name  string
		path  string
		want  bool
		entry string
	}{
		// The shipped hole: every one of these was a silent Allow under "**".
		{"ssh private key", "/home/me/.ssh/id_rsa", true, "~/.ssh"},
		{"ssh dir itself", "/home/me/.ssh", true, "~/.ssh"},
		{"aws credentials", "/home/me/.aws/credentials", true, "~/.aws"},
		{"gnupg", "/home/me/.gnupg/secring.gpg", true, "~/.gnupg"},
		{"kube config", "/home/me/.kube/config", true, "~/.kube"},
		{"gcloud", "/home/me/.config/gcloud/credentials.db", true, "~/.config/gcloud"},
		{"docker auth", "/home/me/.docker/config.json", true, "~/.docker/config.json"},
		{"home dotenv", "/home/me/.env", true, "~/.env"},
		{"git credentials", "/home/me/.git-credentials", true, "~/.git-credentials"},
		{"npmrc", "/home/me/.npmrc", true, "~/.npmrc"},
		{"netrc", "/home/me/.netrc", true, "~/.netrc"},
		{"vault token", "/home/me/.vault-token", true, "~/.vault-token"},
		{"keychain", "/home/me/Library/Keychains/login.keychain-db", true, "~/Library/Keychains"},
		{"chrome login data", "/home/me/Library/Application Support/Google/Chrome/Default/Login Data", true, "~/Library/Application Support/Google/Chrome/Default/Login Data"},

		// Reached through a tilde, a $HOME, and a collapse — same verdict, which
		// is the point of sharing the normalizer with the destructive gate.
		{"via tilde", "~/.ssh/id_rsa", true, "~/.ssh"},
		{"via HOME var", "$HOME/.aws/credentials", true, "~/.aws"},
		{"via collapse", "/home/me/proj/../.ssh/id_ed25519", true, "~/.ssh"},
		{"via relative from workdir", "../.ssh/id_rsa", true, "~/.ssh"},

		// Absolute, non-home credential stores.
		{"etc shadow", "/etc/shadow", true, "/etc/shadow"},
		{"etc ssh host keys", "/etc/ssh/ssh_host_rsa_key", true, "/etc/ssh"},
		{"sudoers", "/etc/sudoers", true, "/etc/sudoers"},
		{"proc environ", "/proc/self/environ", true, "/proc/self/environ"},

		// Windows spellings.
		{"windows appdata gcloud", `C:\Users\me\AppData\Roaming\gcloud\x`, false, ""}, // home is /home/me here
		{"case-folded ssh", "/home/me/.SSH/id_rsa", true, "~/.ssh"},

		// NOT sensitive — the false-positive guard.
		{"project source", "/home/me/proj/main.go", false, ""},
		{"project dotenv is not the home one", "/home/me/proj/.env.example", false, ""},
		{"lookalike prefix", "/home/me/.sshconfig", false, ""},
		{"lookalike dir", "/home/me/.aws-notes/readme.md", false, ""},
		{"etc lookalike", "/etc/shadowing.conf", false, ""},
		{"etc passwd is not on the list", "/etc/passwd", false, ""},
		{"unresolvable relative", "relative/file", false, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			entry, got := IsSensitivePath(c.path, wd)
			if got != c.want {
				t.Fatalf("IsSensitivePath(%q) = %v (entry %q), want %v", c.path, got, entry, c.want)
			}
			if got && entry != c.entry {
				t.Fatalf("IsSensitivePath(%q) entry = %q, want %q", c.path, entry, c.entry)
			}
		})
	}
}

// TestIsSensitivePath_WindowsHome runs the Windows half of the table with a
// Windows-shaped home, which is the only way those entries can ever match.
// Without this the Windows rows in sensitivePathSuffixes would be untested
// decoration.
func TestIsSensitivePath_WindowsHome(t *testing.T) {
	setHome(t, `C:\Users\me`)
	cases := []struct {
		path string
		want bool
	}{
		{`C:\Users\me\AppData\Roaming\gcloud\credentials.db`, true},
		{`C:\Users\me\AppData\Local\Microsoft\Credentials\x`, true},
		{`C:\Users\me\AppData\Roaming\Microsoft\Protect\key`, true},
		{`C:\Users\me\AppData\Local\Google\Chrome\User Data\Default\Login Data`, true},
		{`C:\Users\me\.ssh\id_rsa`, true},
		{`%USERPROFILE%\.aws\credentials`, true},
		{`C:\Users\me\proj\main.go`, false},
		{`C:\Users\me\AppData\Local\Temp\build`, false},
	}
	for _, c := range cases {
		t.Run(c.path, func(t *testing.T) {
			entry, got := IsSensitivePath(c.path, `C:\Users\me\proj`)
			if got != c.want {
				t.Fatalf("IsSensitivePath(%q) = %v (entry %q), want %v", c.path, got, entry, c.want)
			}
		})
	}
}

// TestCheckFS_SensitivePathsPromptUnderWildcardProfile is the S2 regression:
// under the SHIPPED example profile, reading a credential path must no longer
// return Allow. This is the assertion that fails if the denylist is removed or
// moved after the profile globs.
func TestCheckFS_SensitivePathsPromptUnderWildcardProfile(t *testing.T) {
	setHome(t, "/home/me")
	g := New()
	prof := permissiveProfile()
	for _, path := range []string{
		"/home/me/.ssh/id_rsa",
		"~/.aws/credentials",
		"/home/me/.git-credentials",
		"/etc/shadow",
	} {
		t.Run(path, func(t *testing.T) {
			d := g.Check(prof, Action{
				Tool:    "fs_read",
				FS:      FSWant{Op: "read", Paths: []string{path}},
				Workdir: "/home/me/proj",
			})
			if d.Verdict != Prompt {
				t.Fatalf("verdict = %v, want Prompt (reason %q)", d.Verdict, d.Reason)
			}
			if !d.Promptable {
				t.Fatal("the denial must be promptable, or the user has no way to grant a path they own")
			}
			if !strings.Contains(d.Reason, "sensitive") {
				t.Errorf("reason %q should name the gate so the denial is attributable", d.Reason)
			}
		})
	}
}

// TestCheckFS_OrdinaryPathsStillAllowed is the discriminating half. If the
// denylist over-matched, this test — not the one above — is what would notice.
func TestCheckFS_OrdinaryPathsStillAllowed(t *testing.T) {
	setHome(t, "/home/me")
	g := New()
	prof := permissiveProfile()
	for _, path := range []string{
		"/home/me/proj/main.go",
		"/home/me/proj/.env.example",
		"/home/me/proj/internal/guard/guard.go",
		"/tmp/scratch.txt",
	} {
		t.Run(path, func(t *testing.T) {
			d := g.Check(prof, Action{
				Tool:    "fs_read",
				FS:      FSWant{Op: "read", Paths: []string{path}},
				Workdir: "/home/me/proj",
			})
			if d.Verdict != Allow {
				t.Fatalf("verdict = %v for an ordinary file, want Allow (reason %q)", d.Verdict, d.Reason)
			}
		})
	}
}

// TestSensitiveGateRunsBeforeProfileGlobs pins the ORDER, which is the whole
// mechanism. A denylist consulted after a matching allow-glob never runs, so
// this asserts the denial arrives even though "**" would have allowed it — and
// it fails if someone moves the call below the glob loop.
func TestSensitiveGateRunsBeforeProfileGlobs(t *testing.T) {
	setHome(t, "/home/me")
	g := New()
	// A profile whose glob matches the credential path explicitly-as-a-wildcard.
	prof := PermissionProfile{
		Tools: ToolsPerm{Allow: []string{"*"}},
		FS:    FSPerm{Read: []string{"/home/me/.ssh/**", "**"}},
	}
	d := g.Check(prof, Action{
		Tool: "fs_read",
		FS:   FSWant{Op: "read", Paths: []string{"/home/me/.ssh/id_rsa"}},
	})
	if d.Verdict != Prompt {
		t.Fatalf("a wildcard glob over a key directory must not silently grant it; verdict = %v", d.Verdict)
	}
}

// TestProfileGrantsExplicitly is the escape-hatch table. The distinction under
// test is between a pattern that MATCHES the path and a pattern that NAMES it:
// only the second is an operator decision about a credential store.
func TestProfileGrantsExplicitly(t *testing.T) {
	setHome(t, "/home/me")
	const wd = "/home/me/proj"
	cases := []struct {
		name     string
		patterns []string
		path     string
		want     bool
	}{
		{"exact literal file", []string{"/home/me/.aws/credentials"}, "/home/me/.aws/credentials", true},
		{"literal via tilde", []string{"~/.aws/credentials"}, "/home/me/.aws/credentials", true},
		{"literal dir covers contents", []string{"~/.aws"}, "/home/me/.aws/credentials", true},
		{"literal among wildcards", []string{"**", "~/.ssh/id_rsa"}, "/home/me/.ssh/id_rsa", true},

		{"bare double star does not grant", []string{"**"}, "/home/me/.ssh/id_rsa", false},
		{"glob over the key dir does not grant", []string{"~/.ssh/*"}, "/home/me/.ssh/id_rsa", false},
		{"recursive glob over the key dir does not grant", []string{"~/.ssh/**"}, "/home/me/.ssh/id_rsa", false},
		{"question mark is a wildcard", []string{"~/.ssh/id_rs?"}, "/home/me/.ssh/id_rsa", false},
		{"bracket class is a wildcard", []string{"~/.ssh/id_[re]sa"}, "/home/me/.ssh/id_rsa", false},
		{"literal for a different file", []string{"~/.aws/config"}, "/home/me/.aws/credentials", false},
		{"empty patterns", nil, "/home/me/.ssh/id_rsa", false},
		{"literal that is a lookalike prefix", []string{"~/.ssh-backup"}, "/home/me/.ssh/id_rsa", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := profileGrantsExplicitly(c.patterns, c.path, wd); got != c.want {
				t.Fatalf("profileGrantsExplicitly(%v, %q) = %v, want %v", c.patterns, c.path, got, c.want)
			}
		})
	}
}

// TestCheckFS_ExplicitGrantAllowsSensitivePath is the escape hatch end to end:
// an operator who names the path literally gets Allow, with no prompt. Without
// this the gate would be a capability nobody can grant, which is how gates end
// up deleted.
func TestCheckFS_ExplicitGrantAllowsSensitivePath(t *testing.T) {
	setHome(t, "/home/me")
	g := New()
	prof := PermissionProfile{
		Tools: ToolsPerm{Allow: []string{"*"}},
		FS:    FSPerm{Read: []string{"**", "~/.aws/config"}},
	}
	d := g.Check(prof, Action{
		Tool: "fs_read",
		FS:   FSWant{Op: "read", Paths: []string{"/home/me/.aws/config"}},
	})
	if d.Verdict != Allow {
		t.Fatalf("a literally-granted credential path must be allowed; verdict = %v (reason %q)", d.Verdict, d.Reason)
	}
	// The literal grant is scoped: a SIBLING credential file it does not name
	// still prompts.
	d = g.Check(prof, Action{
		Tool: "fs_read",
		FS:   FSWant{Op: "read", Paths: []string{"/home/me/.aws/credentials"}},
	})
	if d.Verdict != Prompt {
		t.Fatalf("a literal grant must not extend to files it does not name; verdict = %v", d.Verdict)
	}
}

// TestCheckSensitiveFS_ReadAndWriteUseTheirOwnPatterns pins that the escape
// hatch consults the pattern list for the OPERATION being attempted. A read
// grant must not authorize a write — overwriting ~/.ssh/authorized_keys is a
// persistence mechanism, not a read.
func TestCheckSensitiveFS_ReadAndWriteUseTheirOwnPatterns(t *testing.T) {
	setHome(t, "/home/me")
	g := New()
	prof := PermissionProfile{
		Tools: ToolsPerm{Allow: []string{"*"}},
		FS:    FSPerm{Read: []string{"**", "~/.ssh/authorized_keys"}, Write: []string{"**"}},
	}
	read := g.Check(prof, Action{
		Tool: "fs_read",
		FS:   FSWant{Op: "read", Paths: []string{"/home/me/.ssh/authorized_keys"}},
	})
	if read.Verdict != Allow {
		t.Fatalf("read is literally granted; verdict = %v", read.Verdict)
	}
	write := g.Check(prof, Action{
		Tool: "fs_write",
		FS:   FSWant{Op: "write", Paths: []string{"/home/me/.ssh/authorized_keys"}},
	})
	if write.Verdict != Prompt {
		t.Fatalf("a READ grant must not authorize a WRITE to the same key file; verdict = %v", write.Verdict)
	}
}

// TestCheckSensitiveFS_NoHomeDoesNotCrashOrOverMatch covers the degenerate
// environment: with no HOME, home-relative entries cannot be resolved, so they
// simply do not match. The absolute entries must keep working — otherwise the
// gate would go entirely inert in a container that does not set HOME.
func TestCheckSensitiveFS_NoHomeDoesNotCrashOrOverMatch(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv("USERPROFILE", "")
	if _, hit := IsSensitivePath("/some/where/.ssh/id_rsa", "/proj"); hit {
		t.Fatal("without a home directory, a home-relative entry must not match an arbitrary path")
	}
	if entry, hit := IsSensitivePath("/etc/shadow", "/proj"); !hit || entry != "/etc/shadow" {
		t.Fatalf("absolute entries must keep matching with no HOME; got (%q, %v)", entry, hit)
	}
}

// TestCheckSensitiveFS_MultiPathActionDeniesOnAnyHit pins that a batch
// operation is judged by its worst member. Tools pass several paths in one
// Action, and allowing the batch because most of it is innocuous would let a
// credential ride along with a directory listing.
func TestCheckSensitiveFS_MultiPathActionDeniesOnAnyHit(t *testing.T) {
	setHome(t, "/home/me")
	g := New()
	d := g.Check(permissiveProfile(), Action{
		Tool: "fs_read",
		FS: FSWant{Op: "read", Paths: []string{
			"/home/me/proj/a.go",
			"/home/me/proj/b.go",
			"/home/me/.ssh/id_rsa",
		}},
	})
	if d.Verdict != Prompt {
		t.Fatalf("a batch containing one credential path must not be allowed wholesale; verdict = %v", d.Verdict)
	}
}
