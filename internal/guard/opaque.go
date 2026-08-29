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
// `--rsh` is here because it means it in the most literal way there is: "the
// REMOTE SHELL to run". rsync's `-e` short form was already covered by the
// entry above it, and the long form was not.
var codePayloadFlags = map[string]bool{
	"-c": true, "-e": true,
	"--command": true, "--eval": true, "--execute": true, "--rsh": true,
}

// nonInterpreterPrograms are programs whose codePayloadFlags operand is DATA,
// not code: a regular expression, a filter script in the program's own
// language, a request body, a container name.
//
// This is the relief table the file header describes, and its failure direction
// is the safe one. A program missing from it produces one prompt on a command
// that did not need one; a program wrongly IN it produces a SILENT PASS.
//
// # The admission rule, rewritten after it let the same defect in twice
//
// The rule used to read "the flag's operand is documented as not being a
// program". Two entries satisfied that sentence and were still wrong:
//
//   - `rsync`, justified as "-e is the remote shell for the transfer" — and a
//     remote shell for the transfer is a PROGRAM rsync execs.
//     `rsync -e 'sh -c "rm -rf /"' a h:b` was Allow.
//   - `git`, justified with an EXAMPLE rather than a class: `git -c
//     core.pager="less -R" log`. `core.pager` is a program git hands to a
//     shell, and so are `alias.*`, `diff.external`, `core.editor` and
//     `sequence.editor`. `git -c core.pager='rm -rf /' log` was Allow.
//
// Both slipped through because the sentence was written about ONE value of the
// flag. The rule is therefore about the flag's VALUE SPACE, and it has a
// negative half that names the shape both failures had:
//
//	ADMITTED when the operand is consumed BY THE PROGRAM ITSELF as a value of
//	one fixed, non-executable type: a pattern, a script in the program's own
//	non-shell mini-language, a request body, a name.
//
//	REFUSED when the operand is a value in a GENERAL-PURPOSE KEY/VALUE CHANNEL
//	— a configuration key, an environment variable, a transport command.
//	Such a channel's value type is "whatever that key means", and the set of
//	keys meaning "a program to run" is unbounded. Operationally: IF THE FLAG
//	TAKES A `key=value` PAIR AT ALL, THE ENTRY IS REFUSED.
//
// That is what disqualifies rsync's `-e` and git's `-c`, and it also names the
// third instance BEFORE it arrives rather than after: `docker`/`podman`/
// `nerdctl` were here for `-e VAR=VALUE`, a container environment variable,
// which is the same channel one process boundary over. They are gone too —
// measured, their relief bought nothing, because `-e FOO=bar` carries no
// whitespace and looksLikeCode never fired on it.
//
// TestReliefTableMembershipIsPinned holds the membership and the operand class
// of every entry, so an addition is a reviewable edit with a class named on it
// rather than a one-word diff. It does not verify the claim — that is what the
// corpus rows and their real-shell witness are for.
var nonInterpreterPrograms = map[string]bool{
	// -e is a PATTERN.
	"grep": true, "egrep": true, "fgrep": true, "zgrep": true, "rg": true, "ag": true,
	// -e is a sed SCRIPT (its own tiny language, but its operands are not
	// commands and `sed -e 's/a b/c/'` is written thousands of times a day).
	"sed": true,
	// -e sets the exit status from the filter's result.
	"jq": true,
	// -d is a request BODY and -e is the referer header. Both routinely carry
	// JSON, which the operand scan below would otherwise read as code.
	"curl": true, "wget": true,
	// Their operands are text written to stdout. scriptEmitters says the same
	// thing for the trailing-argv scan; this is the membership rule's own
	// wording of it — the operand is documented as not being a program.
	"echo": true, "printf": true,
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

// looksLikeStatement is looksLikeCode's stricter sibling, for an operand NO
// FLAG marked as code.
//
// `awk 'BEGIN{system("rm -rf /")}'` puts its program in the FIRST POSITIONAL
// OPERAND, with no option in front of it at all, so the flag-driven loop below
// could never see it — measured Allow with a real /bin/sh running `rm -rf /`.
// `gawk`, `mawk`, `deno eval` and `php -r` are the same shape.
//
// Reusing looksLikeCode for positionals is not possible: a bare space would
// then make `ls "my file"`, `mkdir "a b"` and every commit message an opaque
// payload. A bare `$` is no better — `cd $HOME`, `ls ${HOME}` and
// `cp $SRC $DST` are ordinary and carry nothing else.
//
// So a positional has to look like a STATEMENT rather than merely like a
// string: whitespace AND structural punctuation together. That is what
// separates `BEGIN{system("rm -rf /")}` (a space and three brackets) from
// `ls ${HOME}` (brackets, no space) and `git commit -m "fix the thing"` (a
// space, no brackets).
//
// The boundary this leaves is written down rather than argued away: an operand
// that is a statement with NEITHER — `gdb -ex 'shell rm -rf /'` (spaces only),
// `deno eval "Deno.removeSync('/')"` (punctuation only) — is not seen. Both
// directions were measured; the rule is the widest one that does not prompt on
// ordinary work.
func looksLikeStatement(w string) bool {
	return strings.ContainsAny(w, " \t\n") && strings.ContainsAny(w, ";(){}|&<>`")
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
// marked reports whether a FLAG announced the operand as code (`-c`, `--eval`,
// `-EncodedCommand`). It is what separates the two tiers: a marked operand is a
// command by construction, so gradeUnreadPayload may put it on the floor when
// it reads as a disaster, while an unmarked one caps at a prompt for the same
// reason classifyTrailingArgv caps unknown programs — whether it is a command
// at all is precisely what is not known.
func opaquePayload(program string, args []string) (payload string, marked, ok bool) {
	if v, encoded := windowsEncodedPayload(program, args); encoded {
		return v, true, true
	}
	if nonInterpreterPrograms[program] {
		return "", false, false
	}
	for i, a := range args {
		if a == "--" {
			// Everything after `--` is an operand, so a code flag spelling
			// found there is a file name that happens to look like one.
			break
		}
		// `--eval=X` / `--rsh='sh -c …'` attach the operand to the flag, and a
		// scan that only looked at the following word saw a flag word and
		// skipped it. GNU-style long options take either spelling.
		if base, attached, found := strings.Cut(a, "="); found && codePayloadFlags[base] {
			if !isFlagWord(attached) && looksLikeCode(attached) {
				return attached, true, true
			}
			continue
		}
		if !codePayloadFlags[a] || i+1 >= len(args) {
			continue
		}
		v := args[i+1]
		if isFlagWord(v) || !looksLikeCode(v) {
			continue
		}
		return v, true, true
	}
	// An operand NO FLAG marked as code. awk's program, php's -r script and
	// deno's eval argument all sit here, and the loop above cannot reach any of
	// them. The discriminator is stricter than the one above because nothing
	// announced this operand as code — see looksLikeStatement.
	for _, a := range args {
		if isFlagWord(a) || a == "--" {
			continue
		}
		if looksLikeStatement(a) {
			return a, false, true
		}
	}
	return "", false, false
}

// gradeUnreadPayload decides WHICH TIER an unread payload lands in, and the
// answer is read off the payload rather than off the program holding it.
//
// # The inversion this closes
//
// DestructionOpaque is a Prompt, which yolo passes. That was decided for the
// shape it was named after — `python3 -c "…"`, a payload in a language this
// package does not read — and it was then applied to a shape it does not
// describe:
//
//	bash -c "rm -rf /"    structural HardDeny, refused in every mode
//	fish -c "rm -rf /"    Prompt, and yolo runs it
//
// `mksh`, `yash`, `elvish` and `nu` measured the same way. The two commands are
// the same command; the only difference is that one shell's name is in
// posixShellPrograms and the other's is not. "Use a shell the guard has not
// heard of" was a general-purpose way out of the floor, and the set of shells
// nobody has heard of is unbounded in exactly the way opaque.go's header says
// program-name tables always are.
//
// # The criterion
//
// If the payload CAN be read as a shell command and that reading is
// catastrophic, the verdict is the catastrophic one — whichever program was
// going to receive it. If it cannot be read that way, the tier stays Opaque.
// So `fish -c "rm -rf /"` is refused like `bash -c "rm -rf /"`, and
// `python3 -c "print(1)"` still only prompts: PROGRAM NAMES ARE UNBOUNDED, THE
// DANGER OF A PAYLOAD IS NOT.
//
// # What this costs, and why the cost was already being paid
//
// A payload that merely CONTAINS a destructive command at the head of a
// segment is refused unappealably even if the receiving program would have
// treated it as text — `zzsend -c "rm -rf / is dangerous"` is a refusal. That
// is not a new class of over-strictness: every program in posixShellPrograms
// has behaved exactly that way since the catastrophic tier existed, and the
// change here is that it stops depending on whether the shell's name is in a
// table. Amends the "must not be promoted to a structural HardDeny" constraint
// in ADR-0018, which was written about the tier as a whole; see ADR-0019.
//
// DestructionUnreadable is passed through rather than folded down to
// Catastrophic: it is structural too, and its reason ("nested deeper than the
// guard unwraps") describes what actually happened.
func gradeUnreadPayload(payload string, marked bool, workdir string, depth int) Destruction {
	if !marked {
		// Nothing announced this operand as a command, so "it is a command" is
		// the thing not known — the same position classifyTrailingArgv is in,
		// and the same cap. `mkdir "rm -rf /; x"` creates a directory.
		return DestructionOpaque
	}
	if depth > 0 {
		if d := classifyDestruction(payload, workdir, depth-1, false); d >= DestructionCatastrophic {
			return d
		}
	}
	return DestructionOpaque
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

// windowsEncodedPayload reports a PowerShell -EncodedCommand or
// -EncodedArguments operand.
//
// It is separate from the loop above because the operand is base64: it carries
// none of the punctuation looksLikeCode tests for, so the generic rule cannot
// see it. Decoding it would be a different change — it would move this shape
// from "cannot read" to "read", which is better and is not what this file is
// for.
//
// The spellings come from bindsPowerShellParam, which covers both halves of how
// the host's binder works: any unambiguous PREFIX of the parameter name, plus
// the hard-coded short ALIASES that are not prefixes of anything. The prefix
// half alone left `-ec` — the alias Microsoft's own documentation leads with,
// and the one red-team tooling emits — reaching Allow.
func windowsEncodedPayload(program string, args []string) (string, bool) {
	if !isPowerShellHost(program) {
		return "", false
	}
	for i, a := range args {
		if bindsPowerShellParam(a, powerShellEncodedParams, powerShellEncodedAliases) && i+1 < len(args) {
			return args[i+1], true
		}
	}
	return "", false
}
