package guard

import "strings"

// ansic.go teaches the destructive-deletion lexer to SEE THROUGH two encodings
// that hide a command's real shape from token-level inspection. The ANSI-C
// DECODER itself now lives in internal/execpolicy (execpolicy/ansic.go), shared
// with ParseCommandList — having two readers with two different sets of
// understood escapes is what produced the ~/.ssh bypass that moved it there.
// What stays here is the part that is specific to the deletion gate: which
// programs are wrappers, and how deep the descent goes.
//
//  1. ANSI-C quoting — $'\x72\x6d -rf /' is, to bash, exactly `rm -rf /`. The
//     guard's metacharacter table catches "$(" but has no entry for "$'", and
//     lexShellLite treats the whole thing as one opaque argument. The
//     destructive gate therefore saw a program named "bash" with one harmless
//     string operand and returned DestructionNone.
//  2. Shell wrappers — `bash -c "rm -rf /"` puts the real command inside a
//     single quoted argument, with the same result.
//
// SEEING THROUGH beats REFUSING. The obvious alternative is to add "$'" to
// checkShell's metacharacter table, but that table is a STRUCTURAL HardDeny:
// unappealable in every mode including yolo, with no interactive escape. Every
// legitimate `printf $'\n'` and `grep $'\t'` would become permanently
// unrunnable, and the payoff would be zero — the attacker just switches to
// `bash -c "$(printf ...)"`, or to plain `bash -c 'rm -rf /'`, which contains
// no ANSI-C quoting at all and was never blocked. Decoding, by contrast,
// routes the DECODED command through the destructive classifier that already
// knows what `rm -rf /` means, so the wrapper stops being a hiding place while
// staying a usable feature. QwenPaw reaches the same conclusion from the other
// side: its shell_evasion_guardian flags $'...' as a FINDING for review rather
// than a refusal, and its catastrophic-path regexes are explicitly written so
// that `bash -c "rm -rf /mnt/c"` still matches through the wrapper.

// shellWrappers are the programs whose "-c <string>" argument is another whole
// command. Normalized names (lowercased, base-named, .exe stripped) because
// lexShellLite hands the program word through normalizeProgramWord.
var shellWrappers = map[string]bool{
	"sh": true, "bash": true, "zsh": true, "dash": true, "ksh": true,
	"ash": true, "busybox": true, "env": true,
	// `script -c CMD file` records a session running CMD. It belongs here
	// rather than in prefixRunners because the command is a -c OPERAND, not the
	// trailing argv, and the spelling that was measured passing is `script -qc
	// 'rm -rf /' /dev/null` — a cluster, which is the shape the flag scan below
	// had to learn anyway.
	"script": true,
}

// shellShortValueFlags and shellLongValueFlags are the wrapper options whose
// NEXT WORD is a value rather than a command. They exist because the flag scan
// below used to give up on the first word that did not start with a dash, so
// `bash -o pipefail -c "rm -rf /"` ended at `pipefail` and graded as a script
// invocation — measured Allow, while the same payload behind a bare `-c` was a
// structural HardDeny.
var shellShortValueFlags = map[string]bool{"-o": true, "-O": true}

var shellLongValueFlags = map[string]bool{"--rcfile": true, "--init-file": true}

// windowsShellWrappers are the Windows-side wrappers, keyed by program and
// carrying the flags whose NEXT WORD is a whole command.
//
// They need their own table because unwrapShellCommand looks for a POSIX short-
// flag cluster ENDING IN 'c', which is what bash does and what none of these
// do: `-Command` ends in 'd' and `/c` does not start with a dash at all, so the
// POSIX scan bailed on the first word and every one of these graded
// DestructionNone.
//
// WHY THIS ARRIVED AS A REGRESSION. Before the PowerShell reader existed, the
// POSIX segmenter refused `… "…C:\"` outright with a trailing-escape error, so
// the whole family was a structural HardDeny for a reason that had nothing to
// do with what it ran. Teaching the segmenter to read a trailing backslash as a
// path separator was correct, and it made these commands READABLE — which is
// only an improvement if something then reads the PAYLOAD. Nothing did.
// Measured after that change and before this table:
//
//	Remove-Item -Recurse C:\                     structural HardDeny
//	powershell -Command "Remove-Item -Recurse C:\"   ALLOW
//
// the naive spelling refused and the wrapped one through: the same inversion —
// the more privileged form being the one that passes — that prefixrunner.go's
// header was written about.
//
// `-EncodedCommand` is DELIBERATELY ABSENT. Its operand is base64-encoded
// UTF-16, so listing it here would add a wrapper this code walks into and
// learns nothing from, and at the bottom of the unwrap budget it would turn a
// blob we cannot read into a refusal we cannot explain. Reading it means
// decoding it, and that is its own change with its own test.
var windowsShellWrappers = map[string]map[string]bool{
	"powershell": {"-command": true, "-c": true},
	"pwsh":       {"-command": true, "-c": true},
	"cmd":        {"/c": true, "/k": true, "/r": true},
}

// powerShellCommandParams are the host parameters whose operand is a whole
// PowerShell command, spelled out in full. The BINDER accepts any unambiguous
// prefix of a parameter name, so matching these by prefix is what covers
// `-Comm`, `-Comman` and the rest — an exact-spelling table listed `-Command`
// and `-c` and let every abbreviation between them through.
//
// `-File` is deliberately absent. Its operand is a SCRIPT PATH, so the payload
// is in a file rather than in this string — the same boundary `bash script.sh`
// and `cat script.sh | sh` have, and reading it would mean reading the file.
var powerShellCommandParams = []string{"-command", "-commandwithargs"}

// powerShellCommandAliases are the host's hard-coded short names, which are NOT
// prefixes of the parameter they stand for and therefore cannot come out of a
// prefix test. `-cwa` is `-CommandWithArgs` (PowerShell 7.4), whose operand is a
// command exactly as `-Command`'s is.
var powerShellCommandAliases = map[string]bool{"-c": true, "-cwa": true}

// powerShellEncodedParams are the host parameters whose operand is a base64
// blob: a command in -EncodedCommand's case, its arguments in the other. Both
// are unreadable here, which is the whole point — see opaque.go.
var powerShellEncodedParams = []string{"-encodedcommand", "-encodedarguments"}

// powerShellEncodedAliases is the same problem as powerShellCommandAliases, and
// it cost a live bypass: Microsoft documents the parameter as
// `-EncodedCommand | -e | -ec`, and `-ec` is the spelling that turns up in the
// wild. `-e` and `-en` and `-enc` ARE prefixes and were covered; `-ec` is not a
// prefix of anything and was Allow.
var powerShellEncodedAliases = map[string]bool{"-ec": true}

// isPowerShellHost reports whether program is a PowerShell host, whose
// parameter binder accepts abbreviations. cmd.exe's `/c` is an exact spelling
// and gets no prefix treatment.
func isPowerShellHost(program string) bool {
	return program == "powershell" || program == "pwsh"
}

// bindsPowerShellParam reports whether word abbreviates one of the given
// parameter names the way PowerShell's binder would, or is one of the host's
// hard-coded aliases.
//
// A single dash is not enough to bind anything (`-` is an operand), so a
// two-character minimum stands in for the binder's ambiguity rule: every name
// here starts with a different second letter than the host's other parameters
// that matter, and a prefix short enough to be genuinely ambiguous is refused
// by PowerShell itself.
func bindsPowerShellParam(word string, params []string, aliases map[string]bool) bool {
	l := strings.ToLower(word)
	if aliases[l] {
		return true
	}
	if len(l) < 2 {
		return false
	}
	for _, p := range params {
		if strings.HasPrefix(p, l) {
			return true
		}
	}
	return false
}

// windowsCommandFlag reports whether word is a flag of program whose operand is
// a whole command.
func windowsCommandFlag(program, word string) bool {
	flags, ok := windowsShellWrappers[program]
	if !ok {
		return false
	}
	if flags[strings.ToLower(word)] {
		return true
	}
	return isPowerShellHost(program) &&
		bindsPowerShellParam(word, powerShellCommandParams, powerShellCommandAliases)
}

// unwrapWindowsShellCommand extracts the payload of a windowsShellWrappers
// invocation. The flag match is case-insensitive because PowerShell's own
// parameter binding is: `-Command`, `-command` and `-COMMAND` are one flag.
//
// THE PAYLOAD IS EVERYTHING AFTER THE FLAG, not the next word. Both shells
// treat the rest of the command line as the command — `powershell -Command
// Remove-Item -Recurse C:\` and `cmd /c rd /s /q C:\` are ordinary spellings
// with no quotes anywhere — and taking one word graded them as `Remove-Item`
// and `rd` with no operands at all. Measured: the quoted spelling of each was a
// structural HardDeny while the unquoted one, which is what an operator
// actually types, was Allow.
//
// Re-joining loses the boundary between two quoted operands, so `-Command
// Remove-Item "C:\my dir"` is read as two targets rather than one. That is the
// over-strict direction (an extra target can only add a verdict, and a path
// with a space is the rarer spelling), and the alternative measured worse: the
// payload was not read at all.
func unwrapWindowsShellCommand(program string, args []string) (string, bool) {
	if _, ok := windowsShellWrappers[program]; !ok {
		return "", false
	}
	for i, a := range args {
		if windowsCommandFlag(program, a) && i+1 < len(args) {
			return strings.Join(args[i+1:], " "), true
		}
	}
	return "", false
}

// unwrapShellCommand extracts the inner command from a shell-wrapper
// invocation — `bash -c "…"`, `sh -lc "…"`, `zsh -o pipefail -c "…"`.
//
// It returns ok=false when the command is not a wrapper invocation, so the
// caller keeps its original classification. The recursion in the caller is
// bounded (maxUnwrapDepth) because `bash -c "bash -c \"bash -c …\""` is a legal
// construction and an unbounded loop over attacker-controlled nesting is a
// denial-of-service on the authorization path.
//
// The flag scan accepts any short-flag cluster CONTAINING 'c' (-c, -lc, -cx,
// -qc) because that is where bash itself looks — a cluster is a set of flags
// and the order inside it carries no meaning. It said "ending in 'c'" and did
// exactly that, so `bash -cx "rm -rf /"` graded as a script invocation and
// reached Allow, and the doc's own third example (`zsh -o pipefail -c "…"`)
// was not readable by the code the doc was attached to.
//
// Three shapes are handled that the ending-in-'c' scan was not:
//
//   - a cluster with c anywhere in it (-cx, -qc)
//   - an option that consumes the following word (shellShortValueFlags,
//     shellLongValueFlags), so `-o pipefail` no longer looks like a script path
//   - `--` between the -c and its operand, which is where POSIX says option
//     processing ends and the command string begins
//
// `env` is included in shellWrappers for `env FOO=1 bash -c …`, and the leading
// VAR=value assignments are skipped so the wrapper behind them is still found.
func unwrapShellCommand(program string, args []string) (string, bool) {
	if !shellWrappers[program] {
		return "", false
	}
	if program == "env" {
		return unwrapEnvCommand(args)
	}
	sawC := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			if sawC {
				continue // `sh -c -- "rm -rf /"`: the operand is still to come
			}
			return "", false // `bash -- script.sh`
		}
		if !strings.HasPrefix(a, "-") || a == "-" {
			if sawC {
				return a, true
			}
			return "", false // first non-flag word: this is a script path, not -c
		}
		if strings.HasPrefix(a, "--") {
			if shellLongValueFlags[a] {
				i++
			}
			continue
		}
		if strings.ContainsRune(a[1:], 'c') {
			sawC = true
			continue
		}
		if shellShortValueFlags[a] {
			i++
		}
	}
	return "", false
}

// unwrapEnvCommand is unwrapShellCommand's `env` branch.
//
// It skips env's own options and VAR=value assignments to reach the real
// program word, then requires THAT to be a wrapper. Without the second half,
// `env` alone would be treated as a wrapper and `env -i` would misparse.
//
// `-S` is the exception, and it is a whole command rather than a program word:
// GNU and BSD env both take `env -S 'rm -rf /'` and SPLIT THE STRING into a
// command line. Measured Allow — the loop below reached the single word
// `rm -rf /`, handed it to normalizeProgramWord, and got back the empty string
// because the word's last path separator is its final byte. The attached
// spelling `env -S'rm -rf /'` is the same flag with no space and is accepted
// here for the same reason the flag scan accepts clusters.
func unwrapEnvCommand(args []string) (string, bool) {
	i := 0
	for i < len(args) && (strings.Contains(args[i], "=") || strings.HasPrefix(args[i], "-")) {
		if args[i] == "-S" || args[i] == "--split-string" {
			if i+1 < len(args) {
				return args[i+1], true
			}
			return "", false
		}
		if v, found := strings.CutPrefix(args[i], "-S"); found && v != "" {
			return v, true
		}
		if v, found := strings.CutPrefix(args[i], "--split-string="); found {
			return v, true
		}
		if args[i] == "-u" || args[i] == "--unset" {
			i++ // consumes a variable name
		}
		i++
	}
	if i >= len(args) {
		return "", false
	}
	return unwrapShellCommand(normalizeProgramWord(args[i]), args[i+1:])
}

// maxUnwrapDepth bounds the nesting classifyLexed will walk through: shell
// wrappers, su/eval payloads and command prefix runners all draw on the same
// budget, because they are all "the real command is one level further in".
//
// Eight is the figure the capability audit reports codex using, and it is
// generous on purpose. The budget is not a safety limit — running out of it is
// a structural refusal (DestructionUnreadable), not a pass — it is a bound on
// the work an attacker-controlled string can make the authorization path do.
// Setting it low would only turn ordinary nesting into refusals:
// `sudo nohup timeout 5 nice -n 19 rm -rf /` already spends four.
const maxUnwrapDepth = 8
