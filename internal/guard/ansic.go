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
}

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

// unwrapWindowsShellCommand extracts the payload of a windowsShellWrappers
// invocation. The flag match is case-insensitive because PowerShell's own
// parameter binding is: `-Command`, `-command` and `-COMMAND` are one flag.
func unwrapWindowsShellCommand(program string, args []string) (string, bool) {
	flags, ok := windowsShellWrappers[program]
	if !ok {
		return "", false
	}
	for i, a := range args {
		if flags[strings.ToLower(a)] && i+1 < len(args) {
			return args[i+1], true
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
// The flag scan accepts any short-flag cluster ENDING in 'c' (-c, -lc, -ec)
// because that is where bash itself looks; the command string is the argument
// immediately after. `env` is included in shellWrappers for `env FOO=1 bash -c
// …`, and the leading VAR=value assignments are skipped so the wrapper behind
// them is still found.
func unwrapShellCommand(program string, args []string) (string, bool) {
	if !shellWrappers[program] {
		return "", false
	}
	i := 0
	if program == "env" {
		// Skip `env`'s own options and VAR=value assignments to reach the real
		// program word, then require THAT to be a wrapper. Without this, `env`
		// alone would be treated as a wrapper and `env -i` would misparse.
		for i < len(args) && (strings.Contains(args[i], "=") || strings.HasPrefix(args[i], "-")) {
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
	for ; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") || a == "-" {
			return "", false // first non-flag word: this is a script path, not -c
		}
		if strings.HasPrefix(a, "--") {
			continue
		}
		if strings.HasSuffix(a, "c") && len(a) >= 2 {
			if i+1 < len(args) {
				return args[i+1], true
			}
			return "", false
		}
	}
	return "", false
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
