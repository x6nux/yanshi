package guard

import (
	"os"
	"path"
	"runtime"
	"strings"
)

// pathnorm.go is the single normalization used by BOTH path-comparing gates in
// this package: the destructive-deletion classifier (destructive.go) and the
// built-in sensitive-credential denylist (sensitive.go). They must agree
// character for character — a target that one of them collapses to "/etc" and
// the other leaves as "~/foo/../../../etc" is a gate that disagrees with itself,
// and the looser half is the one an attacker gets to pick.
//
// The order is load-bearing: EXPAND the home/env references FIRST, clean
// SECOND. Cleaning first is the classic bypass — path.Clean("~/..") collapses to
// "." (a relative path that then joins harmlessly against the workdir) instead
// of resolving to the parent of the home directory, which is what the shell
// would actually delete.

// homeReferences maps the textual home/profile references a shell user or a
// model can type to the environment variable that resolves them. Every form is
// listed explicitly rather than running a generic ${VAR} expander: an
// unrestricted expander would resolve attacker-influenced variable names, and
// the whole point of this file is that the set of things that can turn into an
// absolute path is closed and reviewable.
//
// The "$HOME" family resolves through homeDir() (env: ""), the same lookup "~"
// uses, because all three spellings name ONE directory and must not split into
// two verdicts. A native Windows host usually has HOME unset and USERPROFILE
// set, so a getenv("HOME")-only lookup made `rm -rf ~` catastrophic while
// `rm -rf $HOME` — the same deletion, and one the interpreter that actually
// runs it there expands (PowerShell resolves $HOME to the profile directory) —
// fell through to Allow; `> $HOME/.ssh/authorized_keys` missed the credential
// denylist the same way. Measured by the bypass corpus, which pins all three
// spellings to one verdict per target. What this over-approximates is the
// cmd.exe reading, where ${HOME} stays a literal relative directory; prompting
// on a write to a directory literally named "$HOME" is the accepted cost, and
// it is the safe direction for a gate whose tiers are Prompt and HardDeny.
//
// %VAR% spellings are kept on every platform, not just Windows. A profile or a
// command authored on Windows can be evaluated by a Linux server (the goal loop
// hands commands between machines), and an unexpanded "%USERPROFILE%" that
// silently stays literal is a denylist entry that quietly matches nothing.
var homeReferences = []struct {
	prefix string // the literal token, without a trailing separator
	env    string // environment variable that resolves it ("" = home lookup)
}{
	{"~", ""},
	{"${HOME}", ""},
	{"$HOME", ""},
	{"${USERPROFILE}", "USERPROFILE"},
	{"$USERPROFILE", "USERPROFILE"},
	{"%USERPROFILE%", "USERPROFILE"},
	{"%HOMEPATH%", "USERPROFILE"},
	{"%APPDATA%", "APPDATA"},
	{"%LOCALAPPDATA%", "LOCALAPPDATA"},
	{"%PROGRAMDATA%", "PROGRAMDATA"},
}

// homeDir resolves the current user's home directory, preferring HOME
// (Unix/macOS, and Git Bash / MSYS on Windows) and falling back to USERPROFILE
// (native Windows). Returns "" when neither is set, in which case callers must
// treat "~" as unresolvable rather than guessing — guessing would produce a
// confident comparison against a path that does not exist on this machine.
func homeDir() string {
	if h := os.Getenv("HOME"); h != "" {
		return h
	}
	return os.Getenv("USERPROFILE")
}

// expandHomeReferences rewrites a leading home/profile reference into its
// absolute form. It only ever matches at the START of the token: "$HOME" in the
// middle of a path is not a shell expansion the destructive classifier needs to
// model, and matching it anywhere would let a literal directory named "$HOME"
// masquerade as the real one.
//
// The reference must be followed by a separator or end-of-string, so
// "~foo" (another user's home in shell syntax, which we cannot resolve without
// a passwd lookup) and "$HOMEBREW_PREFIX" are left alone instead of being
// mangled into "<home>foo" / "<home>BREW_PREFIX".
func expandHomeReferences(t string) (string, bool) {
	for _, ref := range homeReferences {
		if !strings.HasPrefix(t, ref.prefix) {
			continue
		}
		rest := t[len(ref.prefix):]
		if rest != "" && rest[0] != '/' && rest[0] != '\\' {
			continue
		}
		var base string
		if ref.env == "" {
			base = homeDir()
		} else {
			base = os.Getenv(ref.env)
		}
		if base == "" {
			return "", false
		}
		return base + rest, true
	}
	return t, true
}

// normalizePath turns a raw path token into the canonical form both gates
// compare against: home/env references expanded, backslashes folded to forward
// slashes, relative tokens joined against workdir, and the result lexically
// cleaned.
//
// It deliberately does NOT touch the filesystem. Both callers classify INTENT —
// the target of an `rm -rf` need not exist yet, and a filesystem round-trip
// would make an authorization decision depend on whether an attacker had
// already created a symlink.
//
// ok=false means "this token cannot be resolved to a comparable path": a
// relative token with no workdir, or a home reference with no HOME. Callers
// must not treat that as "safe" — it means "unknown", and each caller has its
// own fail-safe (the destructive classifier declines to widen, checkFS falls
// through to the profile globs).
func normalizePath(raw, workdir string) (string, bool) {
	t := strings.Trim(raw, `"'`)
	if t == "" {
		return "", false
	}
	t, ok := expandHomeReferences(t)
	if !ok {
		return "", false
	}
	unc := isUNCPath(t)
	t = strings.ReplaceAll(t, `\`, "/")
	vol, _ := splitVolume(t)
	if !unc && vol == "" && !strings.HasPrefix(t, "/") {
		if workdir == "" {
			return "", false
		}
		t = strings.ReplaceAll(workdir, `\`, "/") + "/" + t
	}
	// Split off any volume prefix before cleaning. path.Clean has no notion of
	// volumes: it treats "C:" as an ordinary segment, so "C:/Users/../.."
	// cleans to ".." — a RELATIVE path, which every subsequent comparison then
	// reads as something inside the workdir. That is the whole-drive wipe
	// arriving disguised as a local one. Cleaning only the remainder, with the
	// remainder rooted so ".." cannot walk off the front, keeps the volume
	// anchored where the filesystem anchors it.
	prefix, rest := splitVolume(t)
	if prefix == "" {
		return path.Clean(t), true
	}
	cleaned := path.Clean("/" + strings.TrimPrefix(rest, "/"))
	if cleaned == "/" {
		return prefix, true
	}
	return prefix + cleaned, true
}

// splitVolume separates a Windows volume prefix from the rest of a
// forward-slashed path. It returns ("", p) for ordinary POSIX paths.
//
// The prefix keeps its canonical, comparable spelling: "C:" for a drive and
// "//server/share" for a UNC share, so a drive root and a UNC share root each
// have exactly one representation.
func splitVolume(p string) (prefix, rest string) {
	if isUNCPath(p) {
		body := strings.TrimPrefix(p, "//")
		segs := strings.SplitN(body, "/", 3)
		switch len(segs) {
		case 1:
			return "//" + segs[0], ""
		case 2:
			return "//" + segs[0] + "/" + segs[1], ""
		default:
			return "//" + segs[0] + "/" + segs[1], segs[2]
		}
	}
	if len(p) >= 2 && p[1] == ':' && isASCIILetter(p[0]) {
		return p[:2], p[2:]
	}
	return "", p
}

// isUNCPath reports whether t is a Windows UNC path (\\server\share\... or the
// forward-slash spelling //server/share). A bare "//" or "\\\\" with nothing
// after it is not a share reference.
func isUNCPath(t string) bool {
	if len(t) < 3 {
		return false
	}
	if (t[0] != '/' && t[0] != '\\') || (t[1] != '/' && t[1] != '\\') {
		return false
	}
	return t[2] != '/' && t[2] != '\\'
}

// caseInsensitiveFS reports whether the host's filesystem treats paths
// case-insensitively. macOS (APFS/HFS+ default) and Windows do; Linux does not.
//
// This is consulted ONLY for scope comparisons (is this path inside the
// workdir?), where folding case in the wrong direction would widen the
// in-scope set and make the gate weaker. Denylist comparisons fold
// unconditionally — see foldForDeny.
func caseInsensitiveFS() bool {
	return runtime.GOOS == "darwin" || runtime.GOOS == "windows"
}

// foldForDeny lowercases a normalized path for comparison against a DENY set.
//
// It folds on every platform, including Linux, and that asymmetry with
// foldForScope is deliberate. Folding moves a deny comparison in the safe
// direction: at worst a Linux user with a genuinely distinct "~/.SSH" directory
// gets one extra prompt. Not folding moves it in the unsafe direction on the
// two platforms where "~/.Ssh/id_rsa" and "~/.ssh/id_rsa" are the same file,
// and those are the platforms most of this project's users are on.
func foldForDeny(p string) string { return strings.ToLower(p) }

// foldForScope normalizes a path for an IN-SCOPE comparison (equality with, or
// containment in, the working directory). Unlike foldForDeny it respects the
// host filesystem: folding on Linux would let "/srv/Data" count as inside
// workdir "/srv/data", turning an out-of-scope deletion into an allowed one.
func foldForScope(p string) string {
	if caseInsensitiveFS() {
		return strings.ToLower(p)
	}
	return p
}

// samePath reports whether two already-normalized paths denote the same
// location under scope-comparison rules.
func samePath(a, b string) bool { return foldForScope(a) == foldForScope(b) }

// isAncestorOf reports whether the normalized path a strictly contains b.
// The trailing separator is required so "/srv/dat" is not read as an ancestor
// of "/srv/data".
func isAncestorOf(a, b string) bool {
	fa, fb := foldForScope(a), foldForScope(b)
	if fa == fb {
		return false
	}
	if fa == "/" {
		return strings.HasPrefix(fb, "/")
	}
	return strings.HasPrefix(fb, fa+"/")
}

// isWithin reports whether the normalized path child is the scope root itself
// or lives underneath it.
func isWithin(child, root string) bool {
	return samePath(child, root) || isAncestorOf(root, child)
}
