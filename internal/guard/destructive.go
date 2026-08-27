package guard

import (
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
//
// Two obfuscations are seen THROUGH rather than refused, because the visible
// text of a command is not always what runs (see ansic.go for the rationale):
//
//   - ANSI-C quoting: $'\x72\x6d -rf /' is decoded by lexShellLite as it
//     tokenizes, so the hex spelling reaches the same verdict as the plain one.
//     Decoding happens per TOKEN, not on the raw string, because $'...' is a
//     QUOTING construct: its content never word-splits and never becomes a
//     control operator, so decoding can reveal a target but can never
//     manufacture a chain.
//   - Shell wrappers: the inner string of `bash -c "..."` / `sh -lc "..."` is
//     re-lexed and re-classified recursively (bounded by maxUnwrapDepth). The
//     MOST SEVERE verdict across the wrapper and its payload wins - a wrapper
//     can only ever add danger, never launder it away.
func ClassifyDestruction(cmd, workdir string) Destruction {
	return classifyDestruction(cmd, workdir, maxUnwrapDepth, true)
}

// classifyDestruction is ClassifyDestruction with two pieces of state made
// explicit.
//
// depth is the wrapper-recursion budget; reaching zero stops the descent, and
// the verdict computed so far still stands, so exhausting the budget cannot
// turn a Catastrophic into a None.
//
// topLevel decides what a control operator MEANS, and the distinction is
// load-bearing. At the top level a chained command is handed to checkShell's
// structural metacharacter HardDeny by returning DestructionNone - classifying
// it here would short-circuit that stronger refusal with a mere Prompt. Inside
// a wrapper payload no such handoff exists: checkShell only ever sees the OUTER
// string, and `bash -c "ls && rm -rf /"` presents it with a metacharacter-free
// command. So an inner chain is split and every segment is classified. Without
// that split, chaining inside a wrapper would launder a command past both gates
// at once.
func classifyDestruction(cmd, workdir string, depth int, topLevel bool) Destruction {
	if strings.TrimSpace(cmd) == "" {
		return DestructionNone
	}
	if hasControlOperator(cmd) {
		if topLevel {
			return DestructionNone
		}
		worst := DestructionNone
		for _, seg := range splitControlSegments(cmd) {
			if d := classifyDestruction(seg, workdir, depth, false); d > worst {
				worst = d
			}
		}
		return worst
	}
	// A chain whose operators are ANSI-C encoded reaches here with a raw string
	// that has no metacharacter in it — so checkShell's structural HardDeny will
	// NOT fire, and the "defer to that dimension" branch above would be
	// deferring to nobody. `ls $'\x26\x26' rm -rf /` is exactly that shape.
	// Since there is no stronger gate downstream to hand it to, classify the
	// decoded segments here instead, the same way a wrapper payload is handled.
	if topLevel {
		if decoded, wasEncoded := decodeANSIC(cmd); wasEncoded && hasControlOperator(decoded) {
			return classifyDestruction(decoded, workdir, depth, false)
		}
	}
	program, args, ok := lexShellLite(cmd)
	if !ok {
		return DestructionNone
	}
	return classifyLexed(program, args, workdir, depth)
}

// classifyLexed grades an ALREADY-TOKENIZED command. It is split out from
// classifyDestruction so that a prefix runner's payload can be classified
// without being re-serialized: stripCommandPrefix hands back the inner
// program plus its remaining argv, and re-joining those into a string to feed
// back through the lexer would require re-quoting every operand. Any bug in
// that re-quoting would land on the security side — `rm -rf "/my dir"` rejoined
// as `rm -rf /my dir` becomes two targets, and `rm -rf $'\x2f'` cannot be
// re-emitted at all once decoded. Passing the tokens straight through has no
// such step.
func classifyLexed(program string, args []string, workdir string, depth int) Destruction {
	worst := DestructionNone
	if depth > 0 {
		if inner, isWrapper := unwrapShellCommand(program, args); isWrapper {
			// The payload of `bash -c "..."` is a whole command in its own
			// right. Classify it with the same workdir: the wrapper does not
			// change which directory the deletion lands in.
			worst = classifyDestruction(inner, workdir, depth-1, false)
			if worst == DestructionCatastrophic {
				return worst
			}
		}
		if inner, isSu := unwrapSuCommand(program, args); isSu {
			// `su -c "rm -rf /"` / `su root -c "…"`: same shape as a shell
			// wrapper but with a username positional bash never allows. See
			// unwrapSuCommand for why it is not folded into shellWrappers.
			if d := classifyDestruction(inner, workdir, depth-1, false); d > worst {
				worst = d
			}
			if worst == DestructionCatastrophic {
				return worst
			}
		}
		// COMMAND PREFIX RUNNERS: `sudo rm -rf /`, `timeout 5 rm -rf /`,
		// `nohup rm -rf /`. The trailing argv IS another command, with no -c
		// flag to mark it, so unwrapShellCommand cannot see it and every
		// predicate below declined on the program word "sudo". Measured: that
		// made `sudo rm -rf /` grade None and reach Allow under a
		// `patterns: ["*"]` profile while the plain spelling was refused. The
		// stripped command is classified and the MORE SEVERE verdict wins, so
		// stripping can only reveal danger, never launder it. See
		// prefixrunner.go.
		if inner, innerArgs, isPrefix := stripCommandPrefix(program, args); isPrefix {
			if d := classifyLexed(inner, innerArgs, workdir, depth-1); d > worst {
				worst = d
			}
			if worst == DestructionCatastrophic {
				return worst
			}
		}
	}
	// Storage destruction (dd onto a device, mkfs, wipefs, ...) is graded
	// BEFORE the deletionPrograms filter, because none of those programs is a
	// deletion program and every one of them destroys more than `rm -rf /`
	// does. See storage.go for how the gap was measured. Placed inside the
	// same recursion so wrapper and ANSI-C unwrapping already applies.
	w := cleanScope(workdir)
	if isStorageDestruction(program, args) {
		return DestructionCatastrophic
	}
	if findDeleteOnCatastrophicTarget(program, args, w) {
		return DestructionCatastrophic
	}
	// Recursive chmod/chown on a system root is reversible, so it prompts
	// rather than being refused outright. See storage.go's header.
	if isRecursiveOwnershipOnCatastrophicTarget(program, args, w) {
		return DestructionOutOfScope
	}

	if !deletionPrograms[program] {
		return worst
	}
	recursive := deleteIsRecursive(program, args)
	targets := deleteTargets(args)

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
	return worst
}

// splitControlSegments breaks a command on its control operators, redirections
// and command-substitution delimiters so each executable piece can be
// classified on its own. It is quote-aware: an operator character inside quotes
// is data, not a separator.
//
// It is used ONLY for wrapper payloads (see classifyDestruction's topLevel
// parameter). Splitting is deliberately generous - a fragment that is not a
// command simply fails to match a deletion program and contributes
// DestructionNone - because an extra fragment costs nothing while a missed
// segment is a laundered `rm -rf /`.
func splitControlSegments(cmd string) []string {
	var segs []string
	var cur strings.Builder
	quote := byte(0)
	flush := func() {
		if strings.TrimSpace(cur.String()) != "" {
			segs = append(segs, cur.String())
		}
		cur.Reset()
	}
	for i := 0; i < len(cmd); i++ {
		c := cmd[i]
		if quote != 0 {
			if c == quote {
				quote = 0
			}
			cur.WriteByte(c)
			continue
		}
		switch c {
		case '\'', '"':
			quote = c
			cur.WriteByte(c)
		case ';', '|', '&', '\n', '\r', '>', '<', '`', ')':
			flush()
		case '$':
			if i+1 < len(cmd) && cmd[i+1] == '(' {
				flush()
				i++
				continue
			}
			cur.WriteByte(c)
		default:
			cur.WriteByte(c)
		}
	}
	flush()
	return segs
}

// hasControlOperator reports whether cmd contains a shell control operator or
// redirection that checkShell rejects structurally (its metacharacter HardDeny
// set). When true, ClassifyDestruction defers so that dimension fires instead of
// being short-circuited. Quote-unaware, matching checkShell's own behavior.
//
// The quote-unawareness is what keeps the ANSI-C decoder from ever WIDENING
// what runs. checkShell tests the raw string with the same literal set, so any
// command whose raw text contains a metacharacter is structurally denied
// regardless of what the decoder would have made of it. A `&&` written as
// $'\x26\x26' therefore never reaches an allow: the decoded form is not
// consulted here, and the raw form still carries "$'" through to a lexer that
// treats it as one token.
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
//
// It also DECODES ANSI-C quoting ($'...') into the literal bytes bash would
// produce, as part of the same pass that handles ordinary quotes. That is the
// correct layer for it: $'...' is a quoting construct, so its content joins the
// current token exactly as '...' content does, never word-splits, and never
// becomes a control operator. Decoding earlier — over the whole raw string —
// would break that invariant and let $'\x26\x26' materialize a chain that the
// caller's control-operator check had already passed. See ansic.go.
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
		case c == '$' && i+1 < len(cmd) && cmd[i+1] == '\'':
			lit, next, spanOK := decodeANSICSpan(cmd, i+2)
			if !spanOK {
				return "", nil, false // unterminated $'...' — same as any unterminated quote
			}
			cur.WriteString(lit)
			inTok = true
			i = next - 1
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
//
// The literal table is consulted on the RAW token and the resolved comparison
// on the normalized one, and both halves are needed. The table catches forms
// that have no filesystem resolution at all ("*", "~/*", "%PROGRAMDATA%" on a
// host that does not set it); the resolved half catches the forms that only
// become catastrophic after expansion and collapse — `rm -rf ~/foo/../..`,
// which no literal table can enumerate.
func isCatastrophicTarget(t, workdir string) bool {
	raw := normLiteral(t)
	if catastrophicRoots[raw] {
		return true
	}
	// any drive root: x:, x:/, x:\ (whole-drive deletion)
	if (len(raw) == 2 || len(raw) == 3) && raw[1] == ':' && (len(raw) == 2 || raw[2] == '/') {
		return true
	}
	resolved, ok := normalizePath(t, workdir)
	if !ok {
		return false
	}
	// A resolved path that lands on a system root or a home root is
	// catastrophic no matter how it was spelled. This is the half that closes
	// the collapse bypass: "~/foo/../.." normalizes to the parent of the home
	// directory, which the literal table never sees.
	if resolvedIsCatastrophicRoot(resolved) {
		return true
	}
	if workdir == "" {
		return false
	}
	w := cleanScope(workdir)
	if samePath(resolved, w) {
		return true // deleting the workdir root itself
	}
	return isAncestorOf(resolved, w) // deleting an ancestor of the workdir
}

// resolvedCatastrophicRoots are absolute paths whose deletion is catastrophic
// once a target has been fully expanded and lexically cleaned. It overlaps
// catastrophicRoots on purpose: that table matches the raw SPELLING (including
// unresolvable forms like "*"), this one matches the RESOLVED location, and a
// collapse attack is only visible to the second.
var resolvedCatastrophicRoots = map[string]bool{
	"/":    true,
	"/etc": true, "/usr": true, "/var": true, "/bin": true, "/sbin": true,
	"/boot": true, "/lib": true, "/lib64": true, "/opt": true, "/dev": true,
	"/proc": true, "/sys": true, "/home": true, "/users": true, "/root": true,
	"/system": true, "/private": true, "/library": true, "/applications": true,
	"/volumes": true, "/mnt": true, "/media": true, "/srv": true, "/run": true,
}

// resolvedIsCatastrophicRoot reports whether a fully normalized path names a
// system root, a drive root, a whole UNC share, or the user's home directory.
func resolvedIsCatastrophicRoot(resolved string) bool {
	folded := foldForDeny(resolved)
	if resolvedCatastrophicRoots[folded] {
		return true
	}
	// A volume prefix with nothing (or only a root slash) after it is a whole
	// drive or a whole UNC share. normalizePath emits exactly one spelling for
	// each, so this is an equality test rather than a family of patterns.
	if vol, rest := splitVolume(resolved); vol != "" && strings.Trim(rest, "/") == "" {
		return true
	}
	if home := homeDir(); home != "" {
		if h, ok := normalizePath(home, ""); ok && samePath(resolved, h) {
			return true // wiping the entire home directory
		}
	}
	return false
}

// resolvesOutsideWorkdir reports whether t resolves to a path outside the
// working directory. Absolute targets with an unknown boundary (workdir=="")
// are treated as outside (fail-safe).
func resolvesOutsideWorkdir(t, workdir string) bool {
	resolved, ok := normalizePath(t, workdir)
	if !ok {
		return false
	}
	if workdir == "" {
		return true
	}
	return !isWithin(resolved, cleanScope(workdir))
}

// cleanScope normalizes the workdir the same way normalizePath normalizes
// targets, so prefix comparisons are consistent. The workdir is supplied by the
// shell tool rather than by the model, so it needs no relative-path fallback.
func cleanScope(workdir string) string {
	if workdir == "" {
		return ""
	}
	normalized, ok := normalizePath(workdir, "")
	if !ok {
		return path.Clean(strings.ReplaceAll(workdir, `\`, "/"))
	}
	return normalized
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
