package guard

import "strings"

// prefixrunner.go handles one of the ways a command's real shape can hide from
// the destructive gate's token-level inspection. ClassifyDestruction's header
// lists them all; this file owns
//
// COMMAND PREFIX RUNNERS: programs whose trailing argv IS another whole
// command, with no -c flag and no quoting to mark the boundary.
//
// It said "the third and last way" until a re-review measured four more, so
// the count is gone rather than restated — the authoritative list is the one
// on ClassifyDestruction, and a second copy of it here is a copy that stops
// being true.
//
//	sudo rm -rf /
//	timeout 5 rm -rf /
//	nohup rm -rf /
//	env rm -rf /
//	nice -n 19 rm -rf /
//	chroot / rm -rf /
//
// HOW THIS WAS FOUND, AND WHY IT MATTERS MORE THAN THE TWO ALREADY CLOSED.
// It was not reasoned out from the code; it was measured by feeding a table of
// real attack spellings through Guard.Check with a profile carrying
// `shell: {policy: allowlist, patterns: ["*"]}` — the common local relaxation
// storage.go's header already names — and reading the verdict back:
//
//	"rm -rf /"       verdict=HardDeny  (catastrophic)
//	"sudo rm -rf /"  verdict=Allow     ← no prompt, no record, no mode change
//
// lexShellLite returned program "sudo" with three harmless-looking operands.
// "sudo" is not in deletionPrograms, not in storageDestroyers, and not in
// shellWrappers, so every predicate in destructive.go declined and the command
// graded DestructionNone. The profile then allowed it outright.
//
// That is strictly worse than the ANSI-C hole this package already fixed. The
// hex spelling is something an attacker has to construct; `sudo` in front of a
// destructive command is what a model emits UNPROMPTED when a previous attempt
// failed on permissions, and it is the single most likely spelling of the exact
// command the catastrophic tier exists to refuse. It also inverts the tier: the
// MORE privileged form of `rm -rf /` was the one that got through.
//
// The guard's auto-approval prompt already names privilege escalation as a risk
// category and argues, correctly, that a model reading the raw command text
// sees the `sudo` that a tokenised program word hides. That argument is about
// the AUTO mode's judgement, and it does not reach here: the catastrophic tier
// is structural and runs before any mode is consulted, so nothing was reading
// that text. A defence written only into a prompt is not a defence — the same
// sentence netpolicy.CredentialPolicy opens with, arrived at independently.
//
// STRIPPING, NOT REFUSING — and never widening. The verdict of the stripped
// command is combined with the wrapper's own by taking the MORE SEVERE of the
// two (the same rule classifyLexed already applies to `bash -c` payloads), so
// removing a prefix can only ever reveal danger. `timeout 5 rm -rf ./build`
// still grades None, because the command that remains after the prefix is the
// one that would have graded None on its own.

// prefixRunnerSpec describes how to walk past one runner's own arguments to
// reach the command word behind it.
//
// A generic "skip anything starting with -" rule is not sufficient and the
// difference is exploitable in the safe direction only if we get it right:
// `nice -n 19 rm -rf /` puts a bare "19" where the command word would be, and a
// parser that stopped there would classify a program named "19" and return
// None. Each field below exists because some runner in the table needs it.
type prefixRunnerSpec struct {
	// valueFlags are the runner's flags that consume the FOLLOWING word.
	// Attached spellings (-n19, --adjustment=19) need no entry: they are
	// self-contained words that the generic flag test already skips.
	valueFlags map[string]bool

	// positionals is how many non-flag operands the runner consumes before the
	// command word. `timeout 5 CMD` and `chroot /newroot CMD` are the cases.
	positionals int

	// assignments reports whether VAR=value words may precede the command.
	// True for env and sudo, both of which accept them.
	assignments bool
}

// prefixRunners is the table of programs that run another command given in
// their trailing argv.
//
// Membership rule: the program's ordinary purpose is to execute the argv that
// follows it, unchanged. That is why `xargs` is here (it appends stdin items to
// the given command, so `xargs rm -rf /` still runs `rm -rf /`) and why
// something like `git` is not — `git rm` is git's own subcommand, not a nested
// program, and treating it as one would classify a scoped `git rm -r .` against
// the wrong table.
//
// Entries are the normalized program words lexShellLite produces (base-named,
// lowercased, .exe stripped).
var prefixRunners = map[string]prefixRunnerSpec{
	// Privilege elevation. The reason this file exists: these are what a model
	// reaches for after a permission failure.
	"sudo": {valueFlags: map[string]bool{
		"-u": true, "--user": true, "-g": true, "--group": true,
		"-p": true, "--prompt": true, "-C": true, "--close-from": true,
		"-h": true, "--host": true, "-r": true, "--role": true,
		"-t": true, "--type": true, "-U": true, "--other-user": true,
		"-D": true, "--chdir": true, "-R": true, "--chroot": true,
	}, assignments: true},
	"doas":   {valueFlags: map[string]bool{"-u": true, "-C": true}},
	"please": {valueFlags: map[string]bool{"-u": true}},

	// su/runuser appear here AS WELL AS in suLikeRunners, because they accept
	// the command in two incompatible shapes and each helper sees only one:
	// `su -c "rm -rf /"` puts it in a quoted -c argument (suLikeRunners), while
	// util-linux's `runuser -u root -- rm -rf /` puts it in the trailing argv
	// with no -c at all. classifyLexed runs both helpers and takes the more
	// severe verdict, so covering the two shapes separately costs nothing and
	// missing either one is a live bypass — the `--` spelling is the one the
	// runuser man page leads with, and it was caught by this file's own
	// regression table before the entry existed.
	"runuser": {valueFlags: map[string]bool{"-u": true, "--user": true, "-g": true, "--group": true, "-G": true, "--supp-group": true, "-s": true, "--shell": true}},
	"su":      {valueFlags: map[string]bool{"-s": true, "--shell": true, "-g": true, "--group": true, "-G": true}},

	// Detachment / session wrappers.
	"nohup":  {},
	"setsid": {},
	"daemon": {},

	// Scheduling and resource wrappers.
	"nice":   {valueFlags: map[string]bool{"-n": true, "--adjustment": true}},
	"ionice": {valueFlags: map[string]bool{"-c": true, "--class": true, "-n": true, "--classdata": true, "-p": true, "--pid": true}},
	"taskset": {valueFlags: map[string]bool{"-p": true, "--pid": true, "-c": true, "--cpu-list": true},
		positionals: 1}, // the CPU mask

	// `timeout DURATION CMD` — the duration is a bare positional.
	"timeout": {valueFlags: map[string]bool{"-s": true, "--signal": true, "-k": true, "--kill-after": true}, positionals: 1},

	// Buffering / tty wrappers.
	"stdbuf":   {valueFlags: map[string]bool{"-i": true, "-o": true, "-e": true, "--input": true, "--output": true, "--error": true}},
	"unbuffer": {},

	// Measurement. `time rm -rf /` is both a shell keyword and /usr/bin/time.
	"time": {valueFlags: map[string]bool{"-o": true, "--output": true, "-f": true, "--format": true}},

	// Shell builtins that execute their operand as a command.
	"command": {},
	"exec":    {valueFlags: map[string]bool{"-a": true}},
	"builtin": {},

	// Root/namespace changes. `chroot /newroot CMD` consumes the new root.
	"chroot":   {valueFlags: map[string]bool{"--userspec": true, "--groups": true, "--skip-chdir": false}, positionals: 1},
	"fakeroot": {},
	"proot":    {valueFlags: map[string]bool{"-r": true, "-b": true, "-w": true}},

	// `xargs CMD` runs CMD with stdin items appended — the CMD still runs.
	"xargs": {valueFlags: map[string]bool{
		"-I": true, "-i": true, "--replace": true,
		"-L": true, "--max-lines": true, "-n": true, "--max-args": true,
		"-P": true, "--max-procs": true, "-s": true, "--max-chars": true,
		"-d": true, "--delimiter": true, "-E": true, "-e": true,
		"-a": true, "--arg-file": true,
	}},

	// `env` also appears in shellWrappers for the `env FOO=1 bash -c …` shape.
	// It needs an entry here too for the shape with no wrapper behind it:
	// `env rm -rf /`, which unwrapShellCommand declines because the word it
	// reaches ("rm") is not itself a wrapper.
	"env": {valueFlags: map[string]bool{"-u": true, "--unset": true, "-C": true, "--chdir": true, "-S": true}, assignments: true},

	// Windows. `start /wait CMD` and `runas /user:x CMD` are the same shape;
	// their flags use the slash spelling, which the flag test below accepts.
	"start": {},
	"runas": {},

	// SHELL RESERVED WORDS AND THE GROUP OPENER. These are not programs at
	// all, but the position they occupy is the one lexShellLite reports as the
	// program word, and the word after them IS a command. splitControlSegments
	// cuts `{ rm -rf /; }` at the semicolon, so the first fragment arrives here
	// as the program `{` with `rm -rf /` in its argv; `if true; then rm -rf /;
	// fi` arrives as three fragments, the middle one starting with `then`.
	// Measured: every one of those graded DestructionNone.
	//
	// Only the words a command may FOLLOW are listed. `fi`, `done`, `esac` and
	// `}` close a construct and have nothing behind them, so an entry for them
	// would never strip anything. `for` and `case` are followed by a variable
	// name or a word to match, not a command, and listing them would grade that
	// operand as a program.
	//
	// They are safe to add for the same reason every other entry is: stripping
	// combines with the MORE SEVERE rule, so `{ rm -rf ./build; }` still grades
	// None. A program genuinely named `then` would only be reclassified when
	// what follows it is itself destructive.
	"{":     {},
	"!":     {},
	"then":  {},
	"else":  {},
	"elif":  {},
	"do":    {},
	"if":    {},
	"while": {},
	"until": {},
}

// suLikeRunners take the command as the argument of a -c flag, but unlike the
// entries in shellWrappers they may carry a bare username positional first
// (`su root -c "…"`). unwrapShellCommand bails on the first non-flag word, so
// they need their own tolerance rather than a shellWrappers entry.
var suLikeRunners = map[string]bool{"su": true, "runuser": true}

// isFlagWord reports whether w is an option rather than an operand, accepting
// both the POSIX "-x" and the Windows "/x" spellings. A bare "-" is an operand
// by convention (stdin), and "/etc" must not be read as a flag, so the slash
// form requires a short single-letter body or an explicit "/name:value".
func isFlagWord(w string) bool {
	if w == "" || w == "-" {
		return false
	}
	if w[0] == '-' {
		return true
	}
	if w[0] != '/' {
		return false
	}
	rest := w[1:]
	if i := strings.IndexByte(rest, ':'); i >= 0 {
		rest = rest[:i]
		return rest != "" // "/user:admin"
	}
	return len(rest) == 1 && isASCIILetter(rest[0]) // "/s", "/q" but not "/etc"
}

// stripCommandPrefix walks past a prefix runner's own arguments and returns the
// command hiding behind it.
//
// ok=false means program is not a prefix runner, or the runner had no command
// word after its arguments (`sudo -l`, `env -i`, a truncated `timeout 5`). In
// both cases the caller keeps whatever verdict it already had — this function
// can add a classification but never remove one.
func stripCommandPrefix(program string, args []string) (string, []string, bool) {
	spec, ok := prefixRunners[program]
	if !ok {
		return "", nil, false
	}
	i := 0
	for i < len(args) {
		w := args[i]
		if w == "--" {
			i++
			break
		}
		if spec.assignments && !isFlagWord(w) && strings.Contains(w, "=") {
			i++
			continue
		}
		if !isFlagWord(w) {
			break
		}
		i++
		if spec.valueFlags[w] {
			i++ // this flag eats the next word
		}
	}
	for n := 0; n < spec.positionals && i < len(args); n++ {
		if isFlagWord(args[i]) {
			break
		}
		i++
	}
	if i >= len(args) {
		return "", nil, false
	}
	return normalizeProgramWord(args[i]), args[i+1:], true
}

// unwrapSuCommand extracts the payload of `su -c "…"` / `su - root -c "…"`.
//
// It is separate from unwrapShellCommand because su's argument order is
// different in exactly one way that matters: a bare username may sit between
// the flags and the -c, and unwrapShellCommand treats the first non-flag word
// as a script path and stops. Folding that tolerance into the shared helper
// would loosen it for `bash`, where a non-flag word really is a script path and
// anything after it is the script's own argv, not a command.
func unwrapSuCommand(program string, args []string) (string, bool) {
	if !suLikeRunners[program] {
		return "", false
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			continue
		}
		if !isFlagWord(a) {
			continue // the username operand, or "-" for a login shell
		}
		if strings.HasPrefix(a, "--") {
			if a == "--command" && i+1 < len(args) {
				return args[i+1], true
			}
			if v, found := strings.CutPrefix(a, "--command="); found {
				return v, true
			}
			continue
		}
		if strings.HasSuffix(a, "c") && i+1 < len(args) {
			return args[i+1], true
		}
	}
	return "", false
}
