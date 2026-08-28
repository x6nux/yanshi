package guard

import "strings"

// opaque.go is the deletion gate's answer to the question every previous round
// of this work answered by adding one more spelling to one more table: WHAT
// HAPPENS WHEN THE READER DOES NOT UNDERSTAND WHAT IT IS LOOKING AT.
//
// # The shape of the problem
//
// Five reviews of this package found 39 real bypasses between them. Every one
// was closed by teaching a reader one more construct, and every review after it
// found more. That is not a sign the reviews were unusually thorough; it is the
// shape of the problem. "Read a shell command correctly" is unbounded, and
// chasing it with a table of understood spellings makes the DEFAULT for an
// unknown spelling a PASS. An attacker only has to find one construct nobody
// has written down yet.
//
// # The inversion
//
// W-B-03 already built the piece that fixes this and then used it for exactly
// one case: running out of unwrap budget returns DestructionUnreadable, a
// refusal, rather than "grade whatever we could see". The principle it states —
// NOT HAVING READ THE PAYLOAD IS NOT THE SAME AS HAVING READ A SAFE ONE — was
// true of three other measured shapes at the same time, all of which passed:
//
//	python3 -c "import shutil;shutil.rmtree('/')"     Allow
//	perl -e "unlink '/etc/passwd'"                    Allow
//	powershell -EncodedCommand <base64 of rm -rf />   Allow
//
// The guard does not read Python, Perl or base64-encoded UTF-16, and it should
// not learn to. What it must not do is treat "I did not read it" as "there was
// nothing to read".
//
// # The tier, and why it is a prompt rather than a floor
//
// DestructionOpaque is a Prompt: it stops the command from passing silently,
// and it stops there. The catastrophic floor is for commands the guard has
// READ and understood to be disasters; a refusal nobody can appeal is only
// defensible when the reason for it can be stated. "There is a payload here I
// cannot read" is a reason to ask, not a reason to make `python3 -c` permanently
// unrunnable.
//
// The cost is real and is accepted rather than argued away: some ordinary
// commands begin prompting. That direction is observable — an operator sees the
// prompt and complains — while the direction it replaces is not: a silent pass
// leaves no trace anywhere. The relief table below exists to keep that cost
// down, and MISSING AN ENTRY IN IT COSTS ONE PROMPT, which is the only kind of
// mistake this file is allowed to make.
//
// # What this does not claim
//
// It does not make the reader complete. A program whose argv is code in another
// language can only be recognised by KNOWING THE PROGRAM — there is no property
// of the token `python3` that says "interpreter". What the rule below achieves
// is that the default for an UNRECOGNISED program carrying a code-shaped
// operand is a refusal, so the unbounded direction now fails closed. The
// bounded direction — a program that is recognised and read correctly — is
// still the tables in ansic.go, prefixrunner.go and storage.go.

// codePayloadFlags are the option spellings that, across the Unix world, mean
// "the next word is a program written in my language": python -c, perl -e,
// ruby -e, node --eval, awk -e, osascript -e, psql -c.
//
// The set is deliberately SHORT. `-E` is absent because grep's extended-regex
// flag is spelled that way and its operand is a pattern, and `-r` is absent
// because it is `--recursive` on nearly everything before it is php's eval.
// Both were measured producing prompts on ordinary commands with no compensating
// coverage: every interpreter that accepts them also accepts one of these.
var codePayloadFlags = map[string]bool{
	"-c": true, "-e": true,
	"--command": true, "--eval": true, "--execute": true,
}

// windowsEncodedCommandPrefix is PowerShell's -EncodedCommand, whose operand is
// base64-encoded UTF-16LE.
//
// PowerShell binds any unambiguous prefix of a parameter name, so `-e`, `-en`
// and `-enc` are the same flag as `-EncodedCommand` and all three were measured
// passing the same payload the spelled-out form does. Matching by prefix is
// what makes "add -EncodedCommand to the wrapper table" insufficient, and it is
// why this is a prefix test rather than three more map entries.
const windowsEncodedCommandPrefix = "-encodedcommand"

// nonInterpreterPrograms are programs whose codePayloadFlags operand is DATA,
// not code: a regular expression, a config path, a transport command for
// another program to run.
//
// This is the relief table the file header describes, and its failure direction
// is the safe one. A program missing from it produces one prompt on a command
// that did not need one; a program wrongly IN it produces a silent pass, which
// is why membership is "the flag's operand is documented as not being a
// program" rather than "this program is trusted".
var nonInterpreterPrograms = map[string]bool{
	// -e is a PATTERN.
	"grep": true, "egrep": true, "fgrep": true, "zgrep": true, "rg": true, "ag": true,
	// -e is a sed SCRIPT (its own tiny language, but its operands are not
	// commands and `sed -e 's/a b/c/'` is written thousands of times a day).
	"sed": true,
	// -e is the remote shell for the transfer, -c is a cipher list.
	"rsync": true,
	// -e sets the exit status from the filter's result.
	"jq": true,
	// -c is a configuration override (`git -c core.pager="less -R" log`).
	"git": true,
	// -e is an environment variable for the container.
	"docker": true, "podman": true, "nerdctl": true,
	// -c names a container in a pod.
	"kubectl": true,
}

// looksLikeCode reports whether an operand has the shape of a program rather
// than of an option value.
//
// The discriminator is punctuation no ordinary option value carries: whitespace
// separating words, a statement separator, a bracket, an expansion, a pipe. It
// is what keeps `tail -c 100`, `cut -c 1-5`, `gcc -c foo.c`, `ssh -e none` and
// `kubectl logs -c web` out of the tier while `python3 -c
// "__import__('shutil').rmtree('/')"` — which contains no space at all — stays
// in it, via its parentheses.
//
// `=` is deliberately NOT in the set. Adding it drags in `git -c user.name=x`,
// `docker run -e FOO=bar` and `make -e CFLAGS=-O2`, and buys nothing: an
// interpreter payload short enough to contain an assignment and nothing else
// carries a `$` or a bracket anyway.
func looksLikeCode(w string) bool {
	return strings.ContainsAny(w, " \t\n;(){}$|&<>`")
}

// opaquePayload reports that program+args carries an operand this package does
// not read, and returns it (for the caller's diagnostics, not for grading — the
// whole point is that nobody here can grade it).
//
// It is consulted only when NO unwrapper and NO prefix stripper claimed the
// command, which is what makes it both the interpreter rule and the fail-closed
// backstop for the wrapper tables. `bash +o posix -c "rm -rf /"` uses a bash
// option spelling unwrapShellCommand does not walk past; the flag scan there
// returns "not a wrapper invocation" and this returns "there is a -c payload
// here that nobody read", so the shape is a prompt instead of a pass. That is
// the property the header calls the unbounded direction failing closed.
func opaquePayload(program string, args []string) (string, bool) {
	if v, ok := windowsEncodedPayload(program, args); ok {
		return v, true
	}
	if nonInterpreterPrograms[program] {
		return "", false
	}
	for i, a := range args {
		if a == "--" {
			// Everything after `--` is an operand, so a code flag spelling
			// found there is a file name that happens to look like one.
			return "", false
		}
		if !codePayloadFlags[a] || i+1 >= len(args) {
			continue
		}
		v := args[i+1]
		if isFlagWord(v) || !looksLikeCode(v) {
			continue
		}
		return v, true
	}
	return "", false
}

// classifyTrailingArgv grades the possibility that a program EXECUTES THE ARGV
// WRITTEN AFTER IT, for programs this package has no reader for.
//
// # The half of the backstop that was missing
//
// opaquePayload above is the fail-closed default for "an operand this package
// cannot read", and it only ever fires on an operand a FLAG marked as code. A
// re-review measured the other half of the same family passing:
//
//	pkexec rm -rf /        firejail rm -rf /      strace -f rm -rf /
//	bwrap --dev-bind / /   systemd-run --scope    toolbox run rm -rf /
//	retry rm -rf /         zzrunner-nobody-knows rm -rf /
//
// every one Allow, every one running `rm -rf /` under a real /bin/sh. `doas` is
// in prefixRunners and refused; `pkexec` is not and passed — the same act, two
// distribution spellings, and the table decided. The last two names above are
// INVENTED, which is what makes this a structural hole rather than a list of
// missing rows: no table of program names can be finished.
//
// # The criterion is the argv, not the program name
//
// Every suffix of the argv is read as a command in its own right. If one of them
// grades destructive, this reports it — whatever the program in front is called.
// A wrapper table can then be wrong, incomplete, or absent and the shape is
// still seen.
//
// SUFFIXES, not "the first non-flag word": `bwrap --dev-bind / / rm -rf /` puts
// two bare operands where a generic flag walk expects the command, so a walk
// that stopped at the first one would classify a program called `/`.
//
// # Two tiers, and why the unknown one is capped
//
// A program IN prefixRunners or remoteShellRunners is DEFINED as running its
// trailing argv, so a destructive suffix gets its full verdict. That is what
// closes the second half of the re-review's finding: `taskset -c 0 rm -rf /` was
// Allow because the table entry consumed `-c 0` as a value flag AND `rm` as the
// CPU mask positional, leaving `-rf /`. The flag walk being wrong is now a
// precision loss rather than a hole — being CLAIMED by a reader is no longer a
// certificate that the reader understood what it claimed.
//
// A program in NO table is capped at DestructionOpaque, because "this program
// probably executes its argv" is exactly the thing that is not known. `echo rm
// -rf /` prints six words and deletes nothing, so a floor here would be an
// unappealable refusal of an ordinary command — the direction ADR-0017 and
// ADR-0018 both refuse. Prompt says what is actually known: something that looks
// like a destructive command is sitting where an unknown program's arguments go.
//
// scriptEmitters are exempt entirely: `echo` and `printf` write their operands
// to stdout verbatim, which is the one thing in this package already documented
// as NOT executing them. The relief is that existing table, not a new one.
//
// The suffix is graded with a budget of one and with its own tail scan
// suppressed, which keeps the fan-out linear in the length of the argv. One
// level is enough for the shapes this exists for: `pkexec bash -c "rm -rf /"`
// unwraps the payload, `pkexec sudo rm -rf /` strips the prefix, and a deeper
// nest is reached by the suffix that starts further along.
func classifyTrailingArgv(program string, args []string, workdir string, depth int) Destruction {
	if depth <= 0 || len(args) == 0 || scriptEmitters[program] {
		return DestructionNone
	}
	_, runsItsArgv := prefixRunners[program]
	if !runsItsArgv {
		_, runsItsArgv = remoteShellRunners[program]
	}
	worst := DestructionNone
	for i, a := range args {
		if a == "--" || isFlagWord(a) {
			continue
		}
		d := classifyLexedArgv(normalizeProgramWord(a), args[i+1:], workdir, 1, false)
		if d > DestructionCatastrophic {
			// Running out of the one-level budget is an artifact of THIS scan,
			// not a fact about the command, and DestructionUnreadable carries a
			// reason ("nested deeper than the guard unwraps") that would
			// misdescribe what was refused.
			d = DestructionCatastrophic
		}
		if !runsItsArgv && d > DestructionOpaque {
			d = DestructionOpaque
		}
		worst = maxDestruction(worst, d)
		if worst == DestructionCatastrophic {
			return worst
		}
	}
	return worst
}

// windowsEncodedPayload reports a PowerShell -EncodedCommand operand.
//
// It is separate from the loop above because the operand is base64: it carries
// none of the punctuation looksLikeCode tests for, so the generic rule cannot
// see it. Decoding it would be a different change — it would move this shape
// from "cannot read" to "read", which is better and is not what this file is
// for.
func windowsEncodedPayload(program string, args []string) (string, bool) {
	if _, isWindowsShell := windowsShellWrappers[program]; !isWindowsShell {
		return "", false
	}
	for i, a := range args {
		l := strings.ToLower(a)
		if len(l) >= 2 && strings.HasPrefix(windowsEncodedCommandPrefix, l) && i+1 < len(args) {
			return args[i+1], true
		}
	}
	return "", false
}
