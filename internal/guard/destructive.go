package guard

import (
	"os"
	"path"
	"strings"
)

// Destruction classifies a shell command's deletion intent for the interactive
// mode gate (yolo/auto). It is INDEPENDENT of the PermissionProfile: even when
// the profile would allow a command, catastrophic mass-deletion is blocked in
// every mode and out-of-workdir deletion is escalated so yolo can block it. See
// checkDestructive — the dimension wired first into Guard.Check.
type Destruction int

const (
	// DestructionNone: not a deletion, or a deletion scoped inside the working
	// directory. yolo allows; auto AI-judges.
	DestructionNone Destruction = iota
	// DestructionOutOfScope: a deletion whose target resolves OUTSIDE the working
	// directory (e.g. "rm /etc/passwd", "rm -rf /opt/other"). yolo blocks; auto
	// AI-judges; default/allow-edits prompt the user.
	DestructionOutOfScope
	// DestructionCatastrophic: recursive/forced mass deletion of a system root,
	// home, drive, wildcard root, the workdir itself, an ancestor of it, or a
	// bare "rm -rf". Structurally blocked in ALL modes — the immovable floor.
	DestructionCatastrophic
)

// deletionPrograms are the canonical (lowercased, base-named) programs that
// remove files or directories. execpolicy.Parse already normalizes the program
// word (strips path + .exe, lowercases), so these are plain lowercase names.
var deletionPrograms = map[string]bool{
	"rm": true, "rmdir": true, "unlink": true, "shred": true, "rimraf": true,
	"del": true, "erase": true, "rd": true,
}

// ClassifyDestruction inspects a shell command for destructive deletion. It
// returns DestructionNone for non-deletion or in-workdir deletion,
// DestructionOutOfScope for deletion outside workdir, and
// DestructionCatastrophic for mass/root/home/workdir deletion.
//
// workdir is the project root used as the in-scope boundary; "" means unknown,
// in which case absolute targets are treated as out-of-scope (fail-safe).
//
// It deliberately does NOT reuse execpolicy.Parse: the execpolicy lexer rejects
// glob/expansion/trailing-backslash tokens (*, $HOME, C:\) — exactly the
// catastrophic forms we must catch. Instead it uses a permissive tokenizer
// (lexShellLite). Commands containing a control operator (&&, ;, |, >, $(, …)
// defer to DestructionNone so the shell-metachar HardDeny in checkShell fires
// rather than being short-circuited by a Prompt here.
func ClassifyDestruction(cmd, workdir string) Destruction {
	if strings.TrimSpace(cmd) == "" {
		return DestructionNone
	}
	if hasControlOperator(cmd) {
		return DestructionNone
	}
	program, args, ok := lexShellLite(cmd)
	if !ok || !deletionPrograms[program] {
		return DestructionNone
	}
	recursive := deleteIsRecursive(program, args)
	targets := deleteTargets(args)
	w := cleanScope(workdir)

	// Catastrophic: recursive mass deletion of a broad/root target, the workdir
	// itself or an ancestor of it, or a bare recursive delete with no target.
	if recursive {
		if len(targets) == 0 {
			return DestructionCatastrophic
		}
		for _, t := range targets {
			if isCatastrophicTarget(t, w) {
				return DestructionCatastrophic
			}
		}
	}

	// Out-of-scope: any deletion whose target resolves outside the workdir.
	for _, t := range targets {
		if resolvesOutsideWorkdir(t, w) {
			return DestructionOutOfScope
		}
	}
	return DestructionNone
}

// hasControlOperator reports whether cmd contains a shell control operator or
// redirection that checkShell rejects structurally (its metacharacter HardDeny
// set). When true, ClassifyDestruction defers so that dimension fires instead of
// being short-circuited. Quote-unaware, matching checkShell's own behavior.
func hasControlOperator(cmd string) bool {
	for _, m := range []string{"&&", "&", "||", ";", "|", "`", "$(", "\n", "\r", ">", "<"} {
		if strings.Contains(cmd, m) {
			return true
		}
	}
	return false
}

// lexShellLite splits a shell command into a normalized program word and its
// argument tokens. It is deliberately permissive: it honors single/double
// quotes (stripping them) but does NOT reject globs, expansions, or Windows
// drive/backslash paths — those are the destructive forms we must inspect. A
// backslash is treated as a literal character (so "C:\" stays intact). Returns
// ok=false for an unterminated quote or empty command.
func lexShellLite(cmd string) (program string, args []string, ok bool) {
	var tokens []string
	var cur strings.Builder
	quote := byte(0)
	inTok := false
	flush := func() {
		if inTok {
			tokens = append(tokens, cur.String())
			cur.Reset()
			inTok = false
		}
	}
	for i := 0; i < len(cmd); i++ {
		c := cmd[i]
		if quote != 0 {
			if c == quote {
				quote = 0
				continue
			}
			cur.WriteByte(c)
			inTok = true
			continue
		}
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			flush()
		case c == '\'' || c == '"':
			quote = c
			inTok = true
		default:
			cur.WriteByte(c)
			inTok = true
		}
	}
	if quote != 0 {
		return "", nil, false
	}
	flush()
	if len(tokens) == 0 {
		return "", nil, false
	}
	return normalizeProgramWord(tokens[0]), tokens[1:], true
}

// normalizeProgramWord canonicalizes the program word the way execpolicy does:
// split on path separators (/ and \), take the base name, lowercase, drop a
// trailing .exe. So "/bin/rm", "RM.EXE", "rm" all map to "rm".
func normalizeProgramWord(w string) string {
	w = strings.ReplaceAll(w, "\\", "/")
	if idx := strings.LastIndex(w, "/"); idx >= 0 {
		w = w[idx+1:]
	}
	w = strings.ToLower(w)
	return strings.TrimSuffix(w, ".exe")
}

// deleteIsRecursive reports whether the args turn the deletion recursive. For
// Unix programs (rm/rmdir/unlink/shred/rimraf) that means -r/-R/--recursive;
// for Windows del/erase/rd it means a /s (or /S) flag.
func deleteIsRecursive(program string, args []string) bool {
	windows := program == "del" || program == "erase" || program == "rd"
	for _, a := range args {
		if a == "--" {
			break
		}
		if windows {
			if len(a) >= 2 && a[0] == '/' && strings.ContainsAny(strings.ToLower(a), "s") {
				return true
			}
			continue
		}
		if strings.HasPrefix(a, "--") {
			if a == "--recursive" || a == "--dir" {
				return true
			}
			continue
		}
		if len(a) >= 2 && a[0] == '-' {
			for _, ch := range a[1:] {
				if ch == 'r' || ch == 'R' {
					return true
				}
			}
		}
	}
	return false
}

// deleteTargets returns the path operands of a deletion command, honoring the
// "--" end-of-options separator. Flag-like tokens (-x, --long, and Windows /s)
// are skipped. A bare "-" is treated as a target (stdin convention is not a
// deletion target, but leaving it out is harmless).
func deleteTargets(args []string) []string {
	var targets []string
	noOpts := false
	for _, a := range args {
		if noOpts {
			targets = append(targets, a)
			continue
		}
		if a == "--" {
			noOpts = true
			continue
		}
		if strings.HasPrefix(a, "--") {
			continue
		}
		if len(a) >= 2 && a[0] == '-' {
			continue // short-flag cluster like -rf
		}
		if isWindowsFlag(a) {
			continue // /s /q /f style
		}
		targets = append(targets, a)
	}
	return targets
}

// isWindowsFlag recognizes single-letter Windows option tokens like "/s", "/Q".
// A path such as "/etc" or "/home/u" (Unix absolute) is NOT a Windows flag.
func isWindowsFlag(a string) bool {
	if len(a) < 2 || a[0] != '/' {
		return false
	}
	rest := a[1:]
	if rest == "" {
		return false
	}
	for _, ch := range rest {
		if !isASCIILetter(byte(ch)) {
			return false
		}
	}
	// "/etc", "/home" are multi-letter paths, not flags; "/s", "/q" are flags.
	return len(rest) == 1
}

func isASCIILetter(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

// catastrophicRoots is the set of literal targets whose deletion is catastrophic
// regardless of location. Stored normalized: lowercased, forward-slashed, no
// trailing slash.
var catastrophicRoots = map[string]bool{
	"/": true, "/*": true,
	"~": true, "~/*": true, "$home": true, "$home/*": true,
	"*": true, ".": true, "./": true, "..": true,
	"/etc": true, "/etc/*": true,
	"/usr": true, "/usr/*": true,
	"/var": true, "/var/*": true,
	"/bin": true, "/sbin": true,
	"/boot": true, "/lib": true, "/lib64": true,
	"/opt": true, "/opt/*": true,
	"/home": true, "/home/*": true,
	"/users": true, "/users/*": true,
	"/root": true, "/root/*": true,
	"/system": true, "/system/*": true,
	"/private": true, "/private/*": true,
	"c:/": true, "c:/*": true,
	"%userprofile%": true, "%userprofile%/*": true,
	"%programdata%": true, "%programdata%/*": true,
}

// isCatastrophicTarget reports whether deleting t would be catastrophic: a
// known system root/home/drive/wildcard, the workdir itself, or an ancestor of
// the workdir (which would delete the whole project).
func isCatastrophicTarget(t, workdir string) bool {
	raw := normLiteral(t)
	if catastrophicRoots[raw] {
		return true
	}
	// any drive root: x:, x:/, x:\ (whole-drive deletion)
	if (len(raw) == 2 || len(raw) == 3) && raw[1] == ':' && (len(raw) == 2 || raw[2] == '/') {
		return true
	}
	resolved, ok := resolveTarget(t, workdir)
	if !ok || workdir == "" {
		return false
	}
	w := cleanScope(workdir)
	if resolved == w {
		return true // deleting the workdir root itself
	}
	if strings.HasPrefix(w, resolved+"/") {
		return true // deleting an ancestor of the workdir
	}
	return false
}

// resolvesOutsideWorkdir reports whether t resolves to a path outside the
// working directory. Absolute targets with an unknown boundary (workdir=="")
// are treated as outside (fail-safe).
func resolvesOutsideWorkdir(t, workdir string) bool {
	resolved, ok := resolveTarget(t, workdir)
	if !ok {
		return false
	}
	if workdir == "" {
		return true
	}
	w := cleanScope(workdir)
	if resolved == w {
		return false
	}
	return !strings.HasPrefix(resolved, w+"/")
}

// resolveTarget resolves a deletion target to a normalized absolute-ish forward
// slash path for scope comparison. It expands ~/$HOME/%USERPROFILE%, converts
// backslashes to forward slashes, joins relative targets against workdir, and
// lexically cleans . and .. It does NOT touch the filesystem (the target need
// not exist; we classify intent). Returns ok=false for a relative target with
// no workdir.
func resolveTarget(t, workdir string) (string, bool) {
	t = strings.Trim(t, `"'`)
	if t == "" {
		return "", false
	}
	// homeDir tries HOME first (Unix/macOS), then USERPROFILE (Windows).
	homeDir := os.Getenv("HOME")
	if homeDir == "" {
		homeDir = os.Getenv("USERPROFILE")
	}
	switch {
	case t == "~" || strings.HasPrefix(t, "~/"):
		if homeDir == "" {
			return "", false
		}
		t = homeDir + t[1:]
	case t == "$HOME" || strings.HasPrefix(t, "$HOME/"):
		if homeDir == "" {
			return "", false
		}
		t = homeDir + strings.TrimPrefix(t, "$HOME")
	}
	t = strings.ReplaceAll(t, "\\", "/")
	if !(strings.HasPrefix(t, "/") || isDrivePath(t)) {
		if workdir == "" {
			return "", false
		}
		t = strings.ReplaceAll(workdir, "\\", "/") + "/" + t
	}
	return path.Clean(t), true
}

// cleanScope normalizes the workdir the same way resolveTarget normalizes
// targets, so prefix comparisons are consistent.
func cleanScope(workdir string) string {
	if workdir == "" {
		return ""
	}
	return path.Clean(strings.ReplaceAll(workdir, "\\", "/"))
}

func normLiteral(t string) string {
	t = strings.Trim(t, `"'`)
	t = strings.ReplaceAll(t, "\\", "/")
	t = strings.ToLower(t)
	// Collapse trailing slashes, but leave a drive root like "c:/" intact so the
	// drive-root check below still recognizes it.
	for len(t) > 1 && strings.HasSuffix(t, "/") && !(len(t) == 3 && t[1] == ':') {
		t = t[:len(t)-1]
	}
	if t == "" {
		t = "/"
	}
	return t
}

func isDrivePath(t string) bool {
	return len(t) >= 3 && t[1] == ':' && (t[2] == '/' || t[2] == '\\')
}
