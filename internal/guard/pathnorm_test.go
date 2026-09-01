package guard

import (
	"path"
	"runtime"
	"testing"
)

// setHome points HOME/USERPROFILE at a deterministic directory for the duration
// of one test. Every path gate in this package resolves "~" through the
// environment, so a test that relies on the real home directory would assert
// something different on every machine.
func setHome(t *testing.T, dir string) {
	t.Helper()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
}

// TestNormalizePath is the table for the shared normalizer both path gates use.
// The collapse cases are the point: expansion MUST happen before cleaning, or
// "~/.." becomes "." and the caller compares against the workdir instead of the
// parent of the home directory.
func TestNormalizePath(t *testing.T) {
	setHome(t, "/home/me")
	const wd = "/home/me/proj"
	cases := []struct {
		name    string
		in      string
		workdir string
		want    string
		ok      bool
	}{
		{"absolute unchanged", "/etc/passwd", wd, "/etc/passwd", true},
		{"relative joined", "build", wd, "/home/me/proj/build", true},
		{"relative with no workdir is unresolvable", "build", "", "", false},
		{"empty is unresolvable", "", wd, "", false},

		// Collapse: the S11 bypass. Each of these must resolve ABOVE the home.
		{"tilde parent collapse", "~/foo/../..", wd, "/home", true},
		{"tilde dotdot", "~/..", wd, "/home", true},
		{"home var collapse", "$HOME/a/../..", wd, "/home", true},
		{"braced home var", "${HOME}/x", wd, "/home/me/x", true},
		{"userprofile percent", "%USERPROFILE%/x", wd, "/home/me/x", true},
		{"workdir relative escape", "../../..", wd, "/", true},

		// Trailing and repeated separators must not change identity.
		{"trailing slash", "/etc/", wd, "/etc", true},
		{"multiple slashes", "/etc///passwd", wd, "/etc/passwd", true},
		{"dot segments", "/etc/./passwd", wd, "/etc/passwd", true},
		{"trailing dot", "/etc/.", wd, "/etc", true},

		// Windows spellings fold to forward slashes; the drive stays a root.
		{"windows drive", `C:\Users\me`, wd, "C:/Users/me", true},
		{"windows drive root", `C:\`, wd, "C:", true},
		{"windows collapse", `C:\Users\me\..\..`, wd, "C:", true},

		// UNC must not lose its double-slash marker, or //server/share would be
		// compared as the local path /server/share.
		{"unc share", `\\server\share\dir`, wd, "//server/share/dir", true},
		{"unc collapse", `\\server\share\a\..`, wd, "//server/share", true},

		// A "~" that cannot be resolved is unknown, not safe.
		{"tilde other user is left alone", "~otheruser/x", wd, "/home/me/proj/~otheruser/x", true},
		{"quoted path is unquoted", `"/etc/passwd"`, wd, "/etc/passwd", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := normalizePath(c.in, c.workdir)
			if ok != c.ok {
				t.Fatalf("normalizePath(%q, %q) ok = %v, want %v", c.in, c.workdir, ok, c.ok)
			}
			if ok && got != c.want {
				t.Fatalf("normalizePath(%q, %q) = %q, want %q", c.in, c.workdir, got, c.want)
			}
		})
	}
}

// TestNormalizePath_NoHome proves the fail-safe: with neither HOME nor
// USERPROFILE set, a "~" target is UNRESOLVABLE rather than silently becoming
// the literal string "~". Returning the literal would make "~/.ssh" compare
// unequal to every denylist entry and pass the gate.
func TestNormalizePath_NoHome(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv("USERPROFILE", "")
	if _, ok := normalizePath("~/.ssh/id_rsa", "/proj"); ok {
		t.Fatal("with no HOME, a ~ path must be unresolvable, not silently literal")
	}
	if _, ok := normalizePath("$HOME/x", "/proj"); ok {
		t.Fatal("with no HOME, a $HOME path must be unresolvable")
	}
}

// TestNormalizePath_HomeUnsetProfileSet pins the native-Windows shape of the
// home lookup: HOME unset, USERPROFILE set. The three home spellings name ONE
// directory, so `$HOME`/`${HOME}` must resolve where `~` does. With a
// getenv("HOME")-only lookup they came back unresolvable there, which graded
// `rm -rf $HOME` Allow while `rm -rf ~` stayed Catastrophic and let
// `> $HOME/.ssh/authorized_keys` past the credential denylist — measured on a
// Windows CI runner; the decision itself is recorded on homeReferences.
func TestNormalizePath_HomeUnsetProfileSet(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv("USERPROFILE", "/home/me")

	for _, ref := range []string{"~", "$HOME", "${HOME}"} {
		got, ok := normalizePath(ref+"/x", "/proj")
		if !ok {
			t.Fatalf("normalizePath(%q/…) unresolvable with HOME unset: the spelling now grades apart from ~", ref)
		}
		if got != "/home/me/x" {
			t.Fatalf("normalizePath(%q/…) = %q, want /home/me/x", ref, got)
		}
	}
	if got := ClassifyDestruction("rm -rf ${HOME}", "/proj"); got != DestructionCatastrophic {
		t.Fatalf("ClassifyDestruction(\"rm -rf ${HOME}\") = %v, want Catastrophic: an unresolvable spelling must not grade below ~", got)
	}
	entry, ok := IsSensitivePath("$HOME/.ssh/authorized_keys", "/proj")
	if !ok {
		t.Fatal("a credential write spelled with $HOME missed the denylist entirely")
	}
	if entry != "~/.ssh" {
		t.Fatalf("denylist matched %q, want ~/.ssh", entry)
	}
}

// TestExpandHomeReferences_OnlyAtStartAndOnBoundary pins the two restrictions
// that keep expansion from mangling ordinary paths: the reference must be at
// the start of the token, and it must be followed by a separator or the end.
func TestExpandHomeReferences_OnlyAtStartAndOnBoundary(t *testing.T) {
	setHome(t, "/home/me")
	cases := []struct {
		in   string
		want string
	}{
		{"~", "/home/me"},
		{"~/x", "/home/me/x"},
		{"$HOME", "/home/me"},
		{"$HOMEBREW_PREFIX/bin", "$HOMEBREW_PREFIX/bin"}, // not a boundary
		{"~user/x", "~user/x"},                           // another user's home: unresolvable, left alone
		{"/opt/$HOME", "/opt/$HOME"},                     // not at the start
		{"./~", "./~"},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			got, ok := expandHomeReferences(c.in)
			if !ok {
				t.Fatalf("expandHomeReferences(%q) reported unresolvable", c.in)
			}
			if got != c.want {
				t.Fatalf("expandHomeReferences(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// TestScopeComparisonsRespectBoundaries pins that containment is tested at a
// path-segment boundary. A bare strings.HasPrefix would read "/srv/dat" as an
// ancestor of "/srv/data" and let a sibling directory count as in-scope.
func TestScopeComparisonsRespectBoundaries(t *testing.T) {
	cases := []struct {
		name       string
		parent     string
		child      string
		isAncestor bool
	}{
		{"true child", "/srv/data", "/srv/data/x", true},
		{"same path is not an ancestor", "/srv/data", "/srv/data", false},
		{"prefix but not segment", "/srv/dat", "/srv/data", false},
		{"sibling", "/srv/other", "/srv/data", false},
		{"root is an ancestor of everything", "/", "/srv", true},
		{"deep child", "/a", "/a/b/c/d", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isAncestorOf(c.parent, c.child); got != c.isAncestor {
				t.Fatalf("isAncestorOf(%q, %q) = %v, want %v", c.parent, c.child, got, c.isAncestor)
			}
			wantWithin := c.isAncestor || c.parent == c.child
			if got := isWithin(c.child, c.parent); got != wantWithin {
				t.Fatalf("isWithin(%q, %q) = %v, want %v", c.child, c.parent, got, wantWithin)
			}
		})
	}
}

// TestFoldForScope_MatchesHostFilesystem pins the deliberate asymmetry between
// the two folding helpers. Scope folding must follow the host: folding on Linux
// would widen the in-scope set (making an out-of-scope deletion look allowed),
// while NOT folding on macOS/Windows would treat two names for the same
// directory as different places.
func TestFoldForScope_MatchesHostFilesystem(t *testing.T) {
	same := samePath("/srv/Data", "/srv/data")
	wantSame := runtime.GOOS == "darwin" || runtime.GOOS == "windows"
	if same != wantSame {
		t.Fatalf("samePath case-folding = %v on %s, want %v", same, runtime.GOOS, wantSame)
	}
	// Deny folding is unconditional on every platform: it can only add prompts.
	if foldForDeny("/Home/Me/.SSH") != "/home/me/.ssh" {
		t.Fatal("foldForDeny must lowercase on every platform")
	}
}

// TestUNCHelpers pins UNC recognition and volume splitting. //server and
// //server/share are whole shares (deleting either is the network analogue of
// wiping a drive), which splitVolume reports by leaving an empty remainder;
// anything deeper keeps a remainder and is an ordinary path.
func TestUNCHelpers(t *testing.T) {
	uncCases := []struct {
		in   string
		want bool
	}{
		{`\\server\share`, true},
		{`//server/share`, true},
		{`\\`, false},
		{`///x`, false},
		{`/single`, false},
		{`C:\x`, false},
	}
	for _, c := range uncCases {
		if got := isUNCPath(c.in); got != c.want {
			t.Errorf("isUNCPath(%q) = %v, want %v", c.in, got, c.want)
		}
	}
	rootCases := []struct {
		in         string
		wantPrefix string
		wantRest   string
	}{
		{"//server", "//server", ""},
		{"//server/share", "//server/share", ""},
		{"//server/share/dir", "//server/share", "dir"},
		{"C:/", "C:", "/"},
		{"C:/Users", "C:", "/Users"},
		{"/local/path", "", "/local/path"},
	}
	for _, c := range rootCases {
		prefix, rest := splitVolume(c.in)
		if prefix != c.wantPrefix || rest != c.wantRest {
			t.Errorf("splitVolume(%q) = (%q, %q), want (%q, %q)", c.in, prefix, rest, c.wantPrefix, c.wantRest)
		}
	}
}

// TestClassifyDestruction_CollapseBypass is the S11 regression table. Before
// normalization was shared and ordered (expand, then clean), every one of these
// resolved to a harmless-looking token and returned DestructionNone while the
// shell would have deleted the home directory or a system root.
func TestClassifyDestruction_CollapseBypass(t *testing.T) {
	setHome(t, "/home/me")
	const wd = "/home/me/proj"
	cases := []struct {
		name string
		cmd  string
		want Destruction
	}{
		{"tilde collapse to /home", "rm -rf ~/foo/../..", DestructionCatastrophic},
		{"tilde dotdot", "rm -rf ~/..", DestructionCatastrophic},
		{"home var collapse", "rm -rf $HOME/a/../..", DestructionCatastrophic},
		{"braced home collapse", "rm -rf ${HOME}/../..", DestructionCatastrophic},
		{"relative escape to root", "rm -rf ../../../..", DestructionCatastrophic},
		{"relative escape to home", "rm -rf ../..", DestructionCatastrophic},
		{"collapse back into workdir stays none", "rm -rf build/../dist", DestructionNone},
		{"collapse to sibling is out of scope", "rm -rf ../sibling", DestructionOutOfScope},

		// Trailing / repeated separators must not change the verdict.
		{"root with trailing slash", "rm -rf //", DestructionCatastrophic},
		{"etc with trailing slash", "rm -rf /etc/", DestructionCatastrophic},
		{"etc with doubled slash", "rm -rf //etc", DestructionCatastrophic},
		{"workdir with trailing slash", "rm -rf /home/me/proj/", DestructionCatastrophic},
		{"dot segments to workdir", "rm -rf /home/me/proj/./", DestructionCatastrophic},

		// Windows and UNC.
		{"drive root backslash", `rm -rf C:\`, DestructionCatastrophic},
		{"drive root collapse", `rm -rf C:\Users\..\..`, DestructionCatastrophic},
		{"unc share root", `rm -rf \\server\share`, DestructionCatastrophic},
		{"unc deep path is out of scope", `rm -rf \\server\share\proj\build`, DestructionOutOfScope},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := classifyDestruction(c.cmd, wd, maxUnwrapDepth, true); got != c.want {
				t.Fatalf("ClassifyDestruction(%q, %q) = %v, want %v", c.cmd, wd, got, c.want)
			}
		})
	}
}

// TestClassifyDestruction_UnknownWorkdirFailSafe pins the fail-safe direction
// when the boundary is unknown: an absolute target is OUT of scope (we cannot
// prove it is inside), while catastrophic roots stay catastrophic because they
// need no boundary to be recognized.
func TestClassifyDestruction_UnknownWorkdirFailSafe(t *testing.T) {
	setHome(t, "/home/me")
	cases := []struct {
		cmd  string
		want Destruction
	}{
		{"rm /etc/passwd", DestructionOutOfScope},
		{"rm -rf /var/log", DestructionOutOfScope},
		{"rm -rf /", DestructionCatastrophic},
		{"rm -rf ~", DestructionCatastrophic},
		{"rm -rf ~/..", DestructionCatastrophic},
		{"rm -rf /etc", DestructionCatastrophic},
		{"rm foo.txt", DestructionNone}, // relative with no boundary: unclassifiable
	}
	for _, c := range cases {
		t.Run(c.cmd, func(t *testing.T) {
			if got := ClassifyDestruction(c.cmd, ""); got != c.want {
				t.Fatalf("ClassifyDestruction(%q, \"\") = %v, want %v", c.cmd, got, c.want)
			}
		})
	}
}

// TestCleanScopeMatchesTargetNormalization is the anti-drift assertion for the
// shared normalizer: the workdir and a target naming the same directory must
// normalize to the SAME string. If they ever diverge, every scope comparison
// silently reports "outside" and the destructive gate degrades into a prompt
// generator.
func TestCleanScopeMatchesTargetNormalization(t *testing.T) {
	setHome(t, "/home/me")
	for _, wd := range []string{
		"/home/me/proj",
		"/home/me/proj/",
		"/home/me/./proj",
		`C:\Users\me\proj`,
		"/home/me/other/../proj",
	} {
		t.Run(wd, func(t *testing.T) {
			scope := cleanScope(wd)
			target, ok := normalizePath(wd, "")
			if !ok {
				t.Fatalf("normalizePath(%q) unresolvable", wd)
			}
			if scope != target {
				t.Fatalf("cleanScope(%q) = %q but normalizePath = %q; the two gates would disagree", wd, scope, target)
			}
			// And the round trip: the workdir is inside itself.
			if !isWithin(target, scope) {
				t.Fatalf("workdir %q is not within its own scope %q", target, scope)
			}
		})
	}
}

// TestCleanScope_UnresolvableWorkdirStillNormalizes covers the fallback branch:
// a workdir containing an unresolvable home reference (no HOME set) must still
// produce a usable, cleaned scope rather than an empty string, because an empty
// scope would make every absolute target look out-of-bounds.
func TestCleanScope_UnresolvableWorkdirStillNormalizes(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv("USERPROFILE", "")
	got := cleanScope("~/proj/./sub")
	want := path.Clean("~/proj/./sub")
	if got != want {
		t.Fatalf("cleanScope fallback = %q, want %q", got, want)
	}
	if cleanScope("") != "" {
		t.Fatal("cleanScope(\"\") must stay empty so callers can detect an unknown boundary")
	}
}
