package guard

import (
	"path"
	"strings"

	"github.com/x6nux/yanshi/internal/execpolicy"
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
	// DestructionOpaque: the command carries an operand this package does not
	// read — a payload in another language (`python3 -c …`, `perl -e …`), a
	// base64 blob (`powershell -EncodedCommand …`), or a `-c` payload behind an
	// option spelling no unwrapper here walks past. Nothing about it is known to
	// be dangerous; what is known is that NOBODY READ IT.
	//
	// It is a Prompt, not a floor, and opaque.go's header states why: a refusal
	// nobody can appeal is defensible only when the reason can be stated, and
	// "there is something here I cannot read" is a reason to ask. It ranks above
	// OutOfScope so that its reason — which names an unread payload rather than
	// a directory — is the one an operator is shown when a command is both.
	DestructionOpaque
	// DestructionCatastrophic: recursive/forced mass deletion of a system root,
	// home, drive, wildcard root, the workdir itself, an ancestor of it, or a
	// bare "rm -rf". Structurally blocked in ALL modes — the immovable floor.
	DestructionCatastrophic
	// DestructionUnreadable: the command nests shell wrappers, su/eval payloads
	// or command prefix runners deeper than maxUnwrapDepth, so the program that
	// would actually run was never reached. Structurally blocked in ALL modes,
	// for the opposite reason to Catastrophic: not "we know this is a disaster"
	// but "we ran out of budget before we could know".
	//
	// It ranks ABOVE Catastrophic so the same `d > worst` fold that combines
	// every other verdict keeps it, and so no partial verdict computed on the
	// way down can mask it. Exhausting the budget must never be the quiet
	// answer — the audit that suggested a depth limit did not say what to do at
	// the bottom of it, and "grade whatever we could see" is the answer that
	// makes the limit itself the bypass: eight `nohup`s in front of `rm -rf /`
	// would grade None.
	DestructionUnreadable
)

// deletionPrograms are the canonical (lowercased, base-named) programs that
// remove files or directories. execpolicy.Parse already normalizes the program
// word (strips path + .exe, lowercases), so these are plain lowercase names.
// The PowerShell names are here for the same reason `del` and `rd` are: they
// are what deletion is CALLED in the language a shell_run with
// `env: "powershell"` is written in, and the gate matches on the program word.
// `remove-item` is unambiguous. `ri` is the alias people actually type, and it
// collides with Ruby's documentation browser — accepted deliberately, because
// the collision only produces a verdict when a `ri` invocation also carries a
// recursive flag and a root-like operand, which `ri Array` does not.
var deletionPrograms = map[string]bool{
	"rm": true, "rmdir": true, "unlink": true, "shred": true, "rimraf": true,
	"del": true, "erase": true, "rd": true,
	"remove-item": true, "ri": true,
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
// (lexShellLite). Commands containing a control operator (&&, ;, |, $(, …) are
// SPLIT and every segment graded, the most severe verdict winning. A REDIRECTION
// is not a boundary — it and its target word are removed from the segment they
// sit in, wherever in the command they were written (see skipRedirect).
//
// That split used to happen only inside a wrapper payload: at the top level a
// chained command returned DestructionNone so checkShell's whole-string
// metacharacter HardDeny would refuse it instead. INF1 (ADR-0004 supplement)
// took that receiver away — checkShell now judges `ls && rm -rf /` segment by
// segment and finds two individually-plausible commands — so the deferral
// became a hole and the split moved to the top level too. This is the only
// reason INF1 is a refinement of the metacharacter defence rather than a
// removal of it.
//
// Several obfuscations are seen THROUGH rather than refused, because the
// visible text of a command is not always what runs (see ansic.go for the
// rationale). Every one of them was a measured Allow before it was handled:
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
//   - Backslash escapes: `r\m` is `rm` to a POSIX shell and `r/m` to a Windows
//     one. Both readings are graded and the worse wins (unescapeWordLetters).
//   - Assignment prefixes: `FOO=1 rm -rf /` runs rm, not a program called
//     `foo=1`; lexShellLite walks past them to the real command word.
//   - Reserved words and group openers: `{`, `!`, `then`, `do` occupy the
//     program-word position without being programs, so they are prefix runners
//     (prefixrunner.go).
//   - `eval`, whose argv IS a command whichever way it is quoted
//     (classifyLexed).
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
// topLevel no longer decides whether a chain is split — both levels split now
// (see ClassifyDestruction). What it still decides is whether an ANSI-C-encoded
// chain is decoded and re-split: a command whose operators are spelled in hex
// carries no literal operator, so the split above does not fire and the decoded
// form has to be re-examined. Inside a wrapper payload lexShellLite already
// decodes ANSI-C per token, so repeating it there would only re-walk ground the
// wrapper descent has covered.
func classifyDestruction(cmd, workdir string, depth int, topLevel bool) Destruction {
	if strings.TrimSpace(cmd) == "" {
		return DestructionNone
	}
	// worst accumulates across every READING of this command string. There are
	// four (expansion, control-operator split, ANSI-C decode, backslash escapes)
	// plus the two lexings at the bottom, and each one can only ever add a
	// verdict: a reading that reveals danger raises it and a reading that
	// reveals nothing leaves it alone.
	worst := DestructionNone
	// THE EXPANSION READING APPLIES INSIDE A PAYLOAD TOO. Guard.Check runs
	// expandKnownParameters on the command it was handed, but a wrapper payload
	// never reaches Guard.Check — classifyLexed re-enters HERE — so the second
	// reading stopped at the wrapper. Measured: `X=rm; $X -rf /` was a
	// structural HardDeny and `bash -c 'X=rm; $X -rf /'` was Allow, which is the
	// inversion (the more wrapped form being the one that passes) that
	// prefixrunner.go's header was written about.
	//
	// It runs BEFORE the control-operator split, and that position is
	// load-bearing: the definition and the use of a variable are in DIFFERENT
	// segments (`X=rm; $X -rf /`), so a split that happened first would hand
	// each half to a reader that cannot see the other.
	//
	// The recursion terminates on STRING EQUALITY rather than on the `changed`
	// flag — a value that expands to itself (`X=$X; $X`) sets changed without
	// shortening anything — and the budget decrement is the second guard.
	if depth > 0 {
		if expanded, _ := expandKnownParameters(cmd); expanded != cmd {
			worst = maxDestruction(worst, classifyDestruction(expanded, workdir, depth-1, topLevel))
		}
	}
	// A PIPELINE'S PAYLOAD IS IN THE STRING TOO. The split below grades each
	// stage on its own, which reads `printf 'rm -rf /'` as a harmless print and
	// `sh` as a shell with no arguments; the connection between them is the
	// pipe, and only a reader that keeps the stages adjacent can see it. See
	// classifyScriptOnStdin.
	worst = maxDestruction(worst, classifyScriptOnStdin(cmd, workdir, depth))
	if hasControlOperator(cmd) {
		if segs, ok := splitIntoStrictlySmallerSegments(cmd); ok {
			for _, seg := range segs {
				worst = maxDestruction(worst, classifyDestruction(seg, workdir, depth, false))
			}
			return worst
		}
		// The splitter found no boundary it agrees with (the operator was
		// inside quotes). Fall through and classify the command as written.
	}
	// A chain whose operators are ANSI-C encoded reaches here with a raw string
	// that has no literal operator in it, so the split above did not fire and
	// checkShell's segmenter sees one innocent-looking command. The hex
	// spelling of an && chain is exactly that shape. Decode and re-classify:
	// this is the only place the encoded form is expanded to a chain, since
	// lexShellLite decodes per token and a decoded token never word-splits.
	if topLevel {
		if decoded, wasEncoded := execpolicy.DecodeANSIC(cmd); wasEncoded && hasControlOperator(decoded) {
			return maxDestruction(worst, classifyDestruction(decoded, workdir, depth, false))
		}
	}
	// A backslash escape hides a letter of the program word from lexShellLite,
	// which keeps backslashes literal so a Windows path (`C:\Users\me`) survives
	// as one. /bin/sh reads `r\m` as `rm`; the literal reading normalizes it to
	// `r/m` and takes the base name, so the deletion gate saw a program called
	// `m`. Measured: `r\m -rf /`, `d\d if=/dev/zero of=/dev/disk0` and
	// `s\hred -u /etc/shadow` all graded DestructionNone and reached Allow.
	//
	// BOTH readings are graded and the more severe wins, rather than the literal
	// one being replaced. Replacing it would trade this bypass for a Windows
	// regression: `rm -rf C:\Users\me` de-escapes to `C:Usersme`, a relative
	// path that resolves inside the workdir. Only letters and digits are
	// unescaped, so this pass can never manufacture a control operator out of
	// `ls \&\& rm -rf /` — where the escaped `&&` is an operand to ls, not a
	// chain — and turn an ordinary command into a structural refusal.
	if unescaped, hadEscape := unescapeWordLetters(cmd); hadEscape {
		worst = maxDestruction(worst, classifyDestruction(unescaped, workdir, depth, topLevel))
	}
	// A backslash before a double quote is an ESCAPE inside double quotes, and a
	// backslash before a BLANK is an escape outside them — neither is modelled
	// by the permissive lexer, which keeps every backslash literal so that `C:\`
	// survives. Both readings are graded and the worse wins, for the same reason
	// the escape pass above grades both: each one is right about a shape the
	// other gets wrong. See lexShellLitePOSIXEscapes.
	if strings.Contains(cmd, `\"`) || strings.Contains(cmd, "\\ ") || strings.Contains(cmd, "\\\t") {
		if program, args, ok := lexShellLitePOSIXEscapes(cmd); ok {
			worst = maxDestruction(worst, classifyLexed(program, args, workdir, depth))
		}
	}
	program, args, ok := lexShellLite(cmd)
	if !ok {
		return worst
	}
	return maxDestruction(worst, classifyLexed(program, args, workdir, depth))
}

// maxDestruction folds two readings of the same command into one verdict. It is
// the `d > worst` idiom this file applies at every reading boundary, named once
// so the direction — a reading can add danger, never remove it — is stated in a
// single place rather than restated at each of the nine call sites.
func maxDestruction(a, b Destruction) Destruction {
	if b > a {
		return b
	}
	return a
}

// unescapeWordLetters removes the backslash from every `\<letter>` and
// `\<digit>` pair, which is how a POSIX shell reads a backslash inside an
// unquoted word.
//
// TWO RESTRICTIONS, each one measured rather than reasoned:
//
//   - Only letters and digits are unescaped. Any other escaped byte keeps its
//     backslash, so the result can never contain a control operator the input
//     did not already carry — that is what lets classifyDestruction re-split
//     the result without widening what runs. `ls \&\& rm -rf /` passes through
//     untouched.
//   - A word that is recognizably a WINDOWS PATH is skipped whole: one starting
//     with `\\` (a UNC share) or containing `:` or `/`. On Windows the
//     backslash is a separator, not an escape, and de-escaping collapses the
//     separators. Measured: `rm -rf \\server\share\proj\build` became
//     `\\servershareprojbuild`, which normalizes to a whole UNC share and
//     graded Catastrophic — an unappealable refusal in place of the correct
//     out-of-workdir Prompt. That is the direction where an extra reading costs
//     something, so the words where it can happen do not get one.
//
// The reported bool is "the visible text is not what the shell would run". It
// is false on a second application (the words it changed no longer contain an
// escaped letter, and the words it skipped are skipped again), which is what
// terminates the caller's recursion.
func unescapeWordLetters(cmd string) (string, bool) {
	if !strings.Contains(cmd, `\`) {
		return cmd, false
	}
	var out strings.Builder
	changed := false
	for i := 0; i < len(cmd); {
		start := i
		for i < len(cmd) && (cmd[i] == ' ' || cmd[i] == '\t') {
			i++
		}
		out.WriteString(cmd[start:i])
		start = i
		for i < len(cmd) && cmd[i] != ' ' && cmd[i] != '\t' {
			i++
		}
		word, wordChanged := unescapeOneWord(cmd[start:i])
		out.WriteString(word)
		changed = changed || wordChanged
	}
	return out.String(), changed
}

// unescapeOneWord is unescapeWordLetters applied to a single whitespace-
// delimited word, with the Windows-path exclusion its header describes.
func unescapeOneWord(w string) (string, bool) {
	if !strings.Contains(w, `\`) || strings.HasPrefix(w, `\\`) || strings.ContainsAny(w, ":/") {
		return w, false
	}
	var out strings.Builder
	changed := false
	for i := 0; i < len(w); i++ {
		if w[i] != '\\' || i+1 >= len(w) {
			out.WriteByte(w[i])
			continue
		}
		n := w[i+1]
		if isASCIILetter(n) || (n >= '0' && n <= '9') {
			out.WriteByte(n)
			changed = true
		} else {
			out.WriteByte('\\')
			out.WriteByte(n)
		}
		i++
	}
	return out.String(), changed
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
	return classifyLexedArgv(program, args, workdir, depth, true)
}

// classifyLexedArgv is classifyLexed with the trailing-argv scan made
// switchable. The scan re-enters this function once per suffix, and letting
// those re-entries scan their own suffixes would make the work exponential in
// the length of an attacker-supplied argv; scanTail=false is how the recursion
// is kept to one level. See classifyTrailingArgv.
func classifyLexedArgv(program string, args []string, workdir string, depth int, scanTail bool) Destruction {
	worst := DestructionNone
	// read records whether ANY reader claimed this command — an unwrapper that
	// found a payload, or a prefix stripper that found a command behind a
	// runner. It is what separates "there was nothing nested here" from "there
	// was something and no table matched it"; opaquePayload is consulted only in
	// the second case. See opaque.go.
	read := false
	if depth > 0 {
		// Every way one command carries another AS A STRING — a -c payload on
		// either side of the platform divide, an su -c payload, an eval argv, a
		// trap handler — is one entry in nestedCommandUnwrappers, walked here by
		// the same loop hasNestedCommand and nestedPayloads walk. The payload is
		// classified with the SAME workdir (a wrapper does not change which
		// directory the deletion lands in) and the MORE SEVERE verdict wins, so
		// unwrapping can only ever reveal danger, never launder it.
		for _, unwrap := range nestedCommandUnwrappers {
			inner, ok := unwrap(program, args)
			if !ok {
				continue
			}
			read = true
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
		for _, strip := range commandPrefixStrippers {
			inner, innerArgs, isPrefix := strip(program, args)
			if !isPrefix {
				continue
			}
			read = true
			if d := classifyLexedArgv(inner, innerArgs, workdir, depth-1, scanTail); d > worst {
				worst = d
			}
			if worst == DestructionCatastrophic {
				return worst
			}
		}
	} else if hasNestedCommand(program, args) {
		// FAIL-CLOSED AT THE BOTTOM OF THE BUDGET. The recursion above stopped,
		// and this command still hides another one behind it, so nothing below
		// has been read. Returning the verdict computed so far would make the
		// depth limit into the bypass it exists to bound: `nohup nohup nohup
		// nohup nohup nohup nohup nohup nohup rm -rf /` would grade None
		// because `nohup` is in no deletion table.
		//
		// The limit is deliberately generous (maxUnwrapDepth) so that reaching
		// it means the command is contrived, not merely wrapped.
		return DestructionUnreadable
	}
	// THE TRAILING ARGV MIGHT BE A COMMAND. This runs whatever `read` says,
	// which is the difference between it and the opaque check below: being
	// claimed by a reader is not evidence that the reader UNDERSTOOD what it
	// claimed. `taskset -c 0 rm -rf /` was claimed by a prefix stripper whose
	// flag walk consumed `rm` as the CPU mask, so `read` was set, the backstop
	// stood down, and the command reached Allow. See classifyTrailingArgv.
	if scanTail {
		worst = maxDestruction(worst, classifyTrailingArgv(program, args, workdir, depth))
	}
	// NOBODY READ THE PAYLOAD. Every unwrapper and every prefix stripper
	// declined, and the command still hands a code-shaped operand to something.
	// Graded rather than returned, so a command that is BOTH opaque and
	// recognisably catastrophic (`python3 -c "…" ` behind `sudo rm -rf /` in the
	// same segment) still reports the worse of the two. See opaque.go.
	if !read {
		if payload, marked, opaque := opaquePayload(program, args); opaque {
			worst = maxDestruction(worst, gradeUnreadPayload(payload, marked, workdir, depth))
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

// splitIntoStrictlySmallerSegments is splitControlSegments plus the termination
// proof its recursive caller needs.
//
// hasControlOperator is a bare strings.Contains scan while splitControlSegments
// honours quotes, so the two disagree on `grep "a|b" x`: the first says "there
// is a pipe", the second hands back the identical string because that pipe is
// data. classifyDestruction then recurses on an input that never shrinks.
// Measured, before this guard existed: `classifyDestruction("grep \"a|b\" x",
// wd, maxUnwrapDepth, false)` overflows the goroutine stack. It was unreachable
// only because the top level used to bail out on any control operator, and
// INF1 needed exactly that bail-out removed — the deletion gate has to see
// every segment of `ls && rm -rf /` now that checkShell no longer refuses the
// whole chain.
//
// ok=false means "no boundary this splitter agrees with"; the caller must then
// classify the string as written rather than recurse.
func splitIntoStrictlySmallerSegments(cmd string) ([]string, bool) {
	segs := splitControlSegments(cmd)
	if len(segs) == 1 && strings.TrimSpace(segs[0]) == strings.TrimSpace(cmd) {
		return nil, false
	}
	return segs, true
}

// splitControlSegments breaks a command on its control operators and
// command-substitution delimiters so each executable piece can be classified on
// its own. It is quote-aware: an operator character inside quotes is data, not a
// separator.
//
// Splitting is deliberately generous — a fragment that is not a command simply
// fails to match a deletion program and contributes DestructionNone — because an
// extra fragment costs nothing while a missed segment is a laundered
// `rm -rf /`. That generosity used to be contained: the splitter ran ONLY on
// wrapper payloads, and anything at the top level carrying a control operator
// was refused wholesale by checkShell. INF1 removed that outer refusal and the
// splitter is now the top-level reader too, so a shape it reads differently
// from /bin/sh is no longer a harmless extra fragment.
//
// A REDIRECTION IS NOT A COMMAND BOUNDARY. `>` and `<` used to be in the
// separator set above, which is true of no shell: `>/dev/null rm -rf /` is one
// command (POSIX allows a redirection anywhere in a simple command, including
// before the command word), and splitting there produced the single fragment
// `/dev/null rm -rf /`, whose program word normalizes to `null`. Measured: that
// graded DestructionNone and the argv reached a real process under a
// `patterns: ["*"]` profile. skipRedirect consumes the operator together with
// its target word instead, so the command word keeps its position and the
// target — which is a FILE, never a program — is left to checkRedirectTargets
// and the FS dimension, where INF1 put it.
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
		case ';', '|', '&', '\n', '\r', '`', '(', ')':
			// `(` opens a subshell or a process substitution, and the word
			// after it is a PROGRAM, not an operand. Only `)` was a boundary
			// before, so `bash -c "(rm -rf /)"` reached lexShellLite whole and
			// produced the program word `(rm` — measured Allow under a
			// permissive profile. ParseCommandList refuses a bare paren, but it
			// never sees one inside a quoted wrapper payload, so this splitter
			// is the only reader that gets the chance.
			flush()
		case '>', '<':
			// A standalone all-digit token in front of the operator is the file
			// descriptor and belongs to the redirection, not to the command:
			// `2>/dev/null rm -rf /` runs rm, not a program called "2".
			trimmed := trimTrailingFD(cur.String())
			cur.Reset()
			cur.WriteString(trimmed)
			i = skipRedirect(cmd, i) - 1 // -1: the loop's own i++ steps past it
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

// skipRedirect consumes the redirection beginning at cmd[i] — the operator and,
// unless the operator folds a descriptor into itself, the target word that
// follows it — and returns the index of the first byte after it. It always
// advances by at least one byte, which is what keeps splitControlSegments'
// loop monotone.
//
// The target word is DROPPED rather than kept or emitted as its own fragment.
// It names a file, so classifying it as a command was never right: `echo > rm
// -rf /` writes a file called `rm` and runs `echo -rf /`, and reading the tail
// as a deletion was a false positive the old boundary-based split produced by
// accident. Where the target does need judging — is this path writable, is it
// in the credential denylist — is checkRedirectTargets, which gets it from
// execpolicy.ParseCommandList with its quoting intact.
//
// A process substitution `>(…)` has no target word; it is left for the caller's
// `(`/`)` handling, which is where it was already going.
func skipRedirect(cmd string, i int) int {
	c := cmd[i]
	i++
	if i < len(cmd) && cmd[i] == c { // `>>` append, `<<` here-document
		i++
	}
	if i < len(cmd) && cmd[i] == '(' {
		return i
	}
	if i < len(cmd) && cmd[i] == '&' {
		i++
		digits := i
		for digits < len(cmd) && cmd[digits] >= '0' && cmd[digits] <= '9' {
			digits++
		}
		// `2>&1` duplicates a descriptor and `>&-` closes one; neither names a
		// file. Every other spelling of `>&word` writes to a file called word —
		// see execpolicy.scanRedirect, which measured bash, sh and zsh.
		if digits > i && isRedirectWordBoundary(byteAtOrZero(cmd, digits)) {
			return digits
		}
		if byteAtOrZero(cmd, i) == '-' {
			return i + 1
		}
	}
	for i < len(cmd) && (cmd[i] == ' ' || cmd[i] == '\t') {
		i++
	}
	quote := byte(0)
	for i < len(cmd) {
		ch := cmd[i]
		if quote != 0 {
			if ch == quote {
				quote = 0
			}
			i++
			continue
		}
		switch {
		case ch == '\'' || ch == '"':
			quote = ch
		case isRedirectWordBoundary(ch):
			return i
		}
		i++
	}
	return i
}

// trimTrailingFD removes a trailing file-descriptor token from the segment text
// accumulated so far. The digits must form a whole word (`ls 2>x` yields "ls "),
// not the tail of one: in `x2>f y` the shell runs `x2 y`, so "x2" stays.
func trimTrailingFD(s string) string {
	j := len(s)
	for j > 0 && s[j-1] >= '0' && s[j-1] <= '9' {
		j--
	}
	if j == len(s) {
		return s
	}
	if j == 0 || s[j-1] == ' ' || s[j-1] == '\t' {
		return s[:j]
	}
	return s
}

// isRedirectWordBoundary reports whether b ends a redirection target word. Zero
// stands for end-of-string.
func isRedirectWordBoundary(b byte) bool {
	switch b {
	case 0, ' ', '\t', '\n', '\r', ';', '|', '&', '<', '>', '`', '(', ')':
		return true
	}
	return false
}

func byteAtOrZero(s string, i int) byte {
	if i >= len(s) {
		return 0
	}
	return s[i]
}

// hasControlOperator reports whether cmd carries anything that makes it more
// than one plain command word plus operands: a control operator, a redirection,
// or a command substitution. When true, classifyDestruction splits the command
// and grades every piece, so this is the gate in front of that split rather
// than a handoff to another dimension.
//
// It said the opposite until INF1: checkShell used to reject this whole
// character set structurally, so ClassifyDestruction deferred to it and
// returned DestructionNone. checkShell now reads chains instead of refusing
// them (ADR-0004's supplement), which is exactly why the deferral became a hole
// and the split took its place.
//
// It stays quote-unaware, and that is what keeps the ANSI-C decoder from ever
// WIDENING what runs. Being over-eager here is free — splitControlSegments is
// quote-aware and simply hands back the original string when the operator turns
// out to be data, which splitIntoStrictlySmallerSegments reports as "no
// boundary" so the command is graded as written. The direction that would cost
// something is missing an operator, and a bare Contains scan cannot.
//
// A `&&` written as $'\x26\x26' carries no literal operator, so this returns
// false for it and the split does not fire. That form is caught one level down
// instead, by classifyDestruction's top-level execpolicy.DecodeANSIC branch, which expands
// it and re-classifies the decoded chain — `internal/guard`'s
// TestClassifyDestruction_ObfuscatedAndWrapped is what fails if that branch
// goes away.
func hasControlOperator(cmd string) bool {
	for _, m := range []string{"&&", "&", "||", ";", "|", "`", "$(", "\n", "\r", ">", "<", "(", ")"} {
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
	return lexShellLiteMode(cmd, false)
}

// lexShellLitePOSIXEscapes is lexShellLite with the two POSIX escape rules the
// permissive lexer drops: inside double quotes a backslash escapes `"`, `\`,
// `$` and a backtick, and OUTSIDE quotes a backslash escapes a space, which
// joins what looks like two words into one.
//
// The second rule arrived with `bash -c 'rm'\ '-rf'\ '/'`, measured Allow while
// /bin/sh ran `rm -rf /`. The three quoted fragments and the two escaped spaces
// are ONE word to the shell — `rm -rf /` — and the permissive lexer, which
// treats every backslash as a literal, cut it into three. So the payload the
// wrapper carries was in no reading of the string, which is the same failure
// the double-quote rule above was added for, one escape convention over.
//
// It is a SECOND READING rather than a replacement, folded by
// classifyDestruction with the more-severe rule, because the two readings
// disagree on a shape each one gets right and the other does not:
//
//	bash -c "bash -c \"bash -c 'rm -rf /'\""   only the escaping reading
//	powershell -Command "Remove-Item C:\"      only the literal reading
//
// The first is three levels of ordinary shell nesting: without `\"` handling the
// lexer cut the tokens at the wrong bytes and produced the argv
// ["-c", "bash -c \\bash", "-c", "rm -rf /\\"], so the real payload was in no
// reading of the string and the wrapper descent had nothing to descend into —
// measured Allow, at a nesting depth of three against a budget of eight, which
// is why DestructionUnreadable never fired. The second is a Windows path whose
// trailing separator sits against the closing quote; reading `\"` as an escape
// there leaves the quote unterminated and the whole command unlexable, which
// would turn a structural HardDeny into a pass.
func lexShellLitePOSIXEscapes(cmd string) (program string, args []string, ok bool) {
	return lexShellLiteMode(cmd, true)
}

// isDoubleQuoteEscapable reports whether a backslash before b is an escape
// inside double quotes. POSIX lists exactly these four (plus a newline, which
// is a line continuation rather than a character); before anything else the
// backslash is itself a literal.
func isDoubleQuoteEscapable(b byte) bool {
	return b == '"' || b == '\\' || b == '$' || b == '`'
}

func lexShellLiteMode(cmd string, dqEscapes bool) (program string, args []string, ok bool) {
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
			if dqEscapes && quote == '"' && c == '\\' && i+1 < len(cmd) && isDoubleQuoteEscapable(cmd[i+1]) {
				cur.WriteByte(cmd[i+1])
				inTok = true
				i++
				continue
			}
			if c == quote {
				quote = 0
				continue
			}
			cur.WriteByte(c)
			inTok = true
			continue
		}
		switch {
		case dqEscapes && c == '\\' && i+1 < len(cmd) && (cmd[i+1] == ' ' || cmd[i+1] == '\t'):
			// An escaped blank outside quotes is part of the word, not a
			// separator. `'rm'\ '-rf'\ '/'` is one word to the shell.
			cur.WriteByte(cmd[i+1])
			inTok = true
			i++
		case c == '$' && i+1 < len(cmd) && cmd[i+1] == '\'':
			lit, next, spanOK := execpolicy.DecodeANSICSpan(cmd, i+2)
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
	// Leading VAR=value words are ASSIGNMENTS the shell applies to the
	// environment of the command that follows; the program is the first word
	// that is not one. Measured before this loop existed: `FOO=1 rm -rf /` and
	// `A= rm -rf /` produced the program word `foo=1`, which is in no table
	// this file consults, so every predicate declined and the command graded
	// DestructionNone under a permissive profile.
	//
	// This belongs in the lexer rather than in stripCommandPrefix because
	// normalizeProgramWord splits on path separators: by the time a caller sees
	// the program word, `FOO=/tmp/x rm -rf /` has already become `x`. The
	// existing `env FOO=1 rm -rf /` spelling was caught only because `env` is a
	// prefix runner with assignments enabled; the bare prefix is the same shape
	// with the `env` left off, which is what a shell accepts and a model writes.
	//
	// The last token is never consumed: `FOO=1` on its own runs nothing, and
	// reporting an empty program would lose the fact that there was a command
	// word at all.
	first := 0
	for first < len(tokens)-1 && isAssignmentWord(tokens[first]) {
		first++
	}
	return normalizeProgramWord(tokens[first]), tokens[first+1:], true
}

// isAssignmentWord reports whether w has the shape of a shell variable
// assignment: a name made of letters, digits and underscores that does not
// start with a digit, followed by "=". The value may be empty (`A= cmd` is a
// legal assignment of the empty string).
func isAssignmentWord(w string) bool {
	eq := strings.IndexByte(w, '=')
	if eq <= 0 {
		return false
	}
	for i := 0; i < eq; i++ {
		c := w[i]
		if isASCIILetter(c) || c == '_' {
			continue
		}
		if i > 0 && c >= '0' && c <= '9' {
			continue
		}
		return false
	}
	return true
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
