package guard

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// bypasscorpus_test.go is the SPELLING CATALOGUE: every command shape a review
// has measured against this package, with the verdict Guard.Check must return
// for it.
//
// # Why it exists
//
// Three consecutive reviews of the same change line found 26 real bypasses, and
// each round found them by INVENTING A NEW LIST OF SPELLINGS. The lists were
// thrown away with the review documents, so every round paid the invention cost
// again and every round's fixes were re-broken by shapes the previous round had
// already tried. The corpus below is that list made permanent: 120 spellings,
// INCLUDING the ones that were never bypasses, because a spelling that is
// correctly refused today is the regression sample for tomorrow.
//
// # Two assertions, and neither one is a restatement of the other
//
// TestEveryMeasuredSpellingKeepsItsVerdict pins what guard SAYS. On its own that
// is a table of the author's beliefs — the same defect fidelity_test.go's header
// describes, one layer up.
//
// TestNoSpellingTheShellExecutesIsAllowed is what keeps the table honest: it
// runs every POSIX row through a real /bin/sh with recorder shims and refuses to
// let any row whose shell run DESTROYED something or PLANTED A CREDENTIAL be
// pinned as Allow. So a future edit cannot make a red row green by relaxing its
// `want` — the shell has to agree.
//
// # What a row's `why` is for
//
// A row pinned to Allow must carry one. There are legitimately-Allow rows here
// (a payload that lives on stdin, a write the profile really does permit), and
// there are rows that are Allow because of a boundary this package has decided
// not to cross. Both are fine; being Allow silently is not. The test refuses a
// justification-free Allow, so downgrading a row is a reviewable edit rather
// than a one-word diff.

// wantVerdict is the observable class of a guard Decision — the four outcomes a
// caller can distinguish, with the two HardDeny tiers kept apart because only
// one of them is a floor yolo cannot buy past.
type wantVerdict string

const (
	wantAllow  wantVerdict = "Allow"
	wantPrompt wantVerdict = "Prompt"
	// wantFloor is HardDeny with Overridable=false: the structural floor.
	wantFloor wantVerdict = "HardDeny(structural)"
	// wantPolicy is HardDeny with Overridable=true: a profile opinion.
	wantPolicy wantVerdict = "HardDeny(profile policy)"
)

func classOf(d Decision) wantVerdict {
	switch d.Verdict {
	case Allow:
		return wantAllow
	case Prompt:
		return wantPrompt
	default:
		if d.Overridable {
			return wantPolicy
		}
		return wantFloor
	}
}

// bypassRow is one measured spelling.
//
// interp is guard.Action.Interpreter: "" means the command goes to a POSIX
// shell (which is what every caller that does not set the field gets), and
// "powershell" selects the PowerShell reader.
//
// why is mandatory for a row pinned to Allow and ignored otherwise.
type bypassRow struct {
	cmd    string
	want   wantVerdict
	interp string
	why    string
}

// probeProfile is the profile under which the whole corpus is graded: it says
// yes to everything a profile can say yes to, so the ONLY refusal any row can
// produce is one of the dimensions that does not consult the profile. A row
// graded under a restrictive profile would prove nothing — the refusal could
// come from the allowlist rather than from the defence being measured.
func probeProfile() PermissionProfile {
	return PermissionProfile{
		Tools: ToolsPerm{Allow: []string{"*"}},
		FS:    FSPerm{Read: []string{"**"}, Write: []string{"**"}},
		Shell: ShellPerm{Policy: "denylist"},
		Net:   NetPerm{Allow: true},
	}
}

// bypassCorpus is the catalogue. Grouped by the DEFENCE each group probes, not
// by the review that found it, so a new spelling has an obvious home.
var bypassCorpus = []bypassRow{
	// ---- Command substitution, quoted and unquoted --------------------------
	//
	// The unquoted forms were always refused. The double-quoted ones were not:
	// scanQuoted copied `$(` and a backtick into the word byte for byte, so the
	// reader that refuses command substitution never saw one — while /bin/sh
	// performs the substitution inside double quotes exactly as it does outside.
	// Three documents asserted this defence existed before it did.
	{cmd: `$(rm -rf /)`, want: wantFloor},
	{cmd: "`rm -rf /`", want: wantFloor},
	{cmd: `rm -rf "$(echo /)"`, want: wantFloor},
	{cmd: "rm -rf \"`echo /`\"", want: wantFloor},
	{cmd: `eval "$(echo rm) -rf /"`, want: wantFloor},
	{cmd: "eval \"`echo rm` -rf /\"", want: wantFloor},
	{cmd: `eval "$(echo cm0= | base64 -d) -rf /"`, want: wantFloor},
	{cmd: `eval "rm -rf \"$(echo /)\""`, want: wantFloor},
	{cmd: `sh -c "$(echo rm) -rf /"`, want: wantFloor},
	{cmd: `bash -c "$(echo rm) -rf /"`, want: wantFloor},
	{cmd: `echo k > "$(echo ~/.ssh/authorized_keys)"`, want: wantFloor},
	// Single quotes really are data: `$(` inside them is a literal, and a
	// reader that refused it would refuse `echo '$(1+1)'` forever.
	{cmd: `echo '$(rm -rf /)'`, want: wantAllow,
		why: "single quotes suppress substitution in every POSIX shell; the text is data, and /bin/sh runs nothing"},

	// ---- eval, builtins and command prefix runners --------------------------
	{cmd: `eval rm -rf /`, want: wantFloor},
	{cmd: `eval "rm -rf /"`, want: wantFloor},
	{cmd: `command rm -rf /`, want: wantFloor},
	{cmd: `builtin rm -rf /`, want: wantFloor},
	{cmd: `exec rm -rf /`, want: wantFloor},
	{cmd: `xargs rm -rf /`, want: wantFloor},
	{cmd: `sudo rm -rf /`, want: wantFloor},
	{cmd: `nohup rm -rf /`, want: wantFloor},
	{cmd: `timeout 5 rm -rf /`, want: wantFloor},
	{cmd: `env rm -rf /`, want: wantFloor},
	{cmd: `printf rm | sh -s -- -rf /`, want: wantAllow,
		why: "the payload is the PROGRAM NAME arriving on stdin, and `rm` on its own is a deletion " +
			"with no target, which grades None. The justification used to read `it is in no reading " +
			"of this string`, which was true here and FALSE for the two rows below — where the whole " +
			"command, operands included, is a quoted word sitting in the text"},
	{cmd: `printf 'rm -rf /' | sh`, want: wantFloor},
	{cmd: `echo 'rm -rf /' | sh -s`, want: wantFloor},
	{cmd: `echo 'rm -rf /' || sh`, want: wantAllow,
		why: "`||` is not a pipe: /bin/sh runs the echo, sees a zero exit status and never starts sh. " +
			"The negative control for splitPipeStages, which would grade this catastrophic — an " +
			"unappealable refusal of a harmless command — if it folded `||` in with `|`"},
	{cmd: `echo rm | xargs -I{} {} -rf /`, want: wantAllow,
		why: "same boundary as the first row of this group: with -I the program word itself comes " +
			"from stdin, so the string names no command"},
	{cmd: `flock /tmp/lk rm -rf /`, want: wantFloor},
	{cmd: `unshare rm -rf /`, want: wantFloor},
	{cmd: `parallel rm -rf ::: /`, want: wantFloor},
	{cmd: `watch rm -rf /`, want: wantFloor},
	{cmd: `script -qc 'rm -rf /' /dev/null`, want: wantFloor},
	{cmd: `env -S 'rm -rf /'`, want: wantFloor},
	{cmd: `env -S'rm -rf /'`, want: wantFloor},

	// ---- ssh: a command that runs on another machine ------------------------
	//
	// Graded, and the decision is recorded on remoteShellRunners: the
	// catastrophic tier names nothing local, `ssh` is what a model reaches for
	// when a local path is refused, and reading only the token form would have
	// covered the bare spelling while missing the quoted one — the
	// wrapped-form-passes inversion this package keeps closing.
	{cmd: `ssh localhost rm -rf /`, want: wantFloor},
	{cmd: `ssh localhost "rm -rf /"`, want: wantFloor},
	{cmd: `ssh -p 22 localhost uptime`, want: wantAllow,
		why: "an ordinary remote command; the negative sample that stops the two rows above from " +
			"being satisfied by refusing every ssh"},
	{cmd: `ssh localhost`, want: wantAllow,
		why: "no command operand at all — an interactive login, which unwrapRemoteCommand declines"},

	// ---- find -exec and the multi-call binary -------------------------------
	//
	// `find` was half covered: findDeleteOnCatastrophicTarget catches -delete,
	// and nothing read -exec, whose operand words up to `;`/`+` ARE a command.
	// `busybox` was in shellWrappers for its `-c` spelling only, so
	// `busybox rm -rf /` — the spelling the applet table is FOR — walked past.
	{cmd: `find . -exec rm -rf {} +`, want: wantFloor},
	{cmd: `find . -exec rm -rf {} \;`, want: wantFloor},
	{cmd: `find . -execdir rm -rf {} +`, want: wantFloor},
	{cmd: `find . -ok rm -rf {} \;`, want: wantFloor},
	{cmd: `find / -delete`, want: wantFloor},
	{cmd: `find / -name x -delete`, want: wantFloor},
	{cmd: `busybox rm -rf /`, want: wantFloor},
	{cmd: `busybox sh -c "rm -rf /"`, want: wantFloor},
	{cmd: `find . -name '*.tmp'`, want: wantAllow,
		why: "an ordinary find with no -exec and no -delete; the corpus needs the negative sample " +
			"or the -exec entries could be satisfied by refusing find outright"},

	// ---- Reserved words and compound commands -------------------------------
	//
	// The program word sits after a keyword, a group opener or a function
	// definition, none of which is a program.
	{cmd: `i=0; until [ $i = 1 ]; do rm -rf /; i=1; done`, want: wantFloor},
	{cmd: `while true; do rm -rf /; break; done`, want: wantFloor},
	{cmd: `select x in a; do rm -rf /; done`, want: wantFloor},
	{cmd: `case x in x) rm -rf /;; esac`, want: wantFloor},
	{cmd: `f() { rm -rf /; }; f`, want: wantFloor},
	{cmd: `time rm -rf /`, want: wantFloor},
	{cmd: `coproc rm -rf /`, want: wantFloor},
	{cmd: `{ rm -rf /; }`, want: wantFloor},
	{cmd: `! rm -rf /`, want: wantFloor},
	{cmd: `if true; then rm -rf /; fi`, want: wantFloor},

	// ---- trap: the payload is the FIRST operand -----------------------------
	//
	// Exactly the shape of eval, and it was missing for the same reason eval
	// was. It is the stealthier of the two: the payload runs when the shell
	// EXITS, so an operator watching the transcript never sees the moment.
	{cmd: `trap 'rm -rf /' EXIT`, want: wantFloor},
	{cmd: `trap "rm -rf /" EXIT`, want: wantFloor},
	{cmd: `trap 'rm -rf /' 0`, want: wantFloor},
	{cmd: `trap 'rm -rf /' INT EXIT`, want: wantFloor},
	{cmd: `trap -- 'rm -rf /' EXIT`, want: wantFloor},
	{cmd: `trap 'echo k > ~/.ssh/authorized_keys' EXIT`, want: wantPrompt},
	{cmd: `trap - EXIT`, want: wantAllow,
		why: "resetting a handler runs nothing; the negative sample that stops the entries above " +
			"from being satisfied by refusing every trap"},
	{cmd: `trap 'echo done' EXIT`, want: wantAllow,
		why: "an ordinary handler; its payload grades None on its own, which is the point of " +
			"classifying the payload instead of refusing the word"},

	// ---- Parameter expansion ------------------------------------------------
	//
	// Neither reader expanded anything, and the deletion gate's own header said
	// `$HOME` and `~` were resolved — which is what made the middle state the
	// dangerous one: two special cases handled, everything else taken as a
	// literal, and the file reading as though expansion were covered.
	//
	// The rule now is narrow on purpose: an expansion is resolved only when its
	// VALUE IS PRESENT IN THE COMMAND STRING (${IFS}, a ${v:-default}, an
	// assignment or a `set --` earlier in the same command). An expansion whose
	// value comes from outside is left alone rather than blanked, because
	// blanking it turns `rm -rf $BUILD_DIR` into a bare `rm -rf` and refuses an
	// ordinary command structurally.
	{cmd: `rm${IFS}-rf${IFS}/`, want: wantFloor},
	{cmd: `${IFS}rm -rf /`, want: wantFloor},
	{cmd: `${x:-rm} -rf /`, want: wantFloor},
	{cmd: `${x-rm} -rf /`, want: wantFloor},
	{cmd: `rm -rf ${x:-/}`, want: wantFloor},
	{cmd: `rm -rf "${x:-/}"`, want: wantFloor},
	{cmd: `X=rm; $X -rf /`, want: wantFloor},
	{cmd: `X=/; rm -rf $X`, want: wantFloor},
	{cmd: `X=rm
$X -rf /`, want: wantFloor},
	{cmd: `export X=rm; $X -rf /`, want: wantFloor},
	{cmd: `set -- rm -rf /; "$@"`, want: wantFloor},
	{cmd: `set -- rm -rf /; $@`, want: wantFloor},
	{cmd: `rm -rf $HOME`, want: wantFloor},
	{cmd: `rm -rf ${HOME}`, want: wantFloor},
	{cmd: `rm -rf ~`, want: wantFloor},
	{cmd: `rm -rf $BUILD_DIR`, want: wantAllow,
		why: "the value is not in the string, so a POSIX shell expands it to a path this reader " +
			"cannot see. It is the row that fails if unresolved expansions are blanked: blanking " +
			"leaves a bare `rm -rf`, which the catastrophic tier refuses unappealably"},
	{cmd: `$UNSET_PROGRAM -rf /`, want: wantAllow,
		why: "same direction: an unset variable expands to nothing and /bin/sh runs no command at all"},
	{cmd: `BUILD_DIR=build; rm -rf $BUILD_DIR`, want: wantAllow,
		why: "the value IS in the string, so the expansion resolves — to a path inside the work " +
			"directory. The positive control for the two rows above: without it they could be " +
			"satisfied by resolving no expansion at all"},

	// ---- Lexical boundaries around the program word and the target ----------
	{cmd: `rm -rf \/`, want: wantFloor},
	{cmd: `rm -rf /\.`, want: wantFloor},
	{cmd: `rm -rf \/\.`, want: wantFloor},
	{cmd: `rm -rf "\/"`, want: wantFloor},
	{cmd: `rm -rf ""/`, want: wantFloor},
	{cmd: `rm -rf /''`, want: wantFloor},
	{cmd: `rm -rf /..`, want: wantFloor},
	{cmd: `rm -rf //`, want: wantFloor},
	{cmd: `rm -rf ///`, want: wantFloor},
	{cmd: `rm -r -f /`, want: wantFloor},
	{cmd: `rm --recursive --force \/`, want: wantFloor},
	{cmd: `r\m -rf /`, want: wantFloor},
	{cmd: `$'\x72\x6d' -rf /`, want: wantFloor},
	{cmd: `rm -rf \~`, want: wantPrompt},
	{cmd: `unlink /etc/passwd`, want: wantPrompt},

	// ---- The shell wrapper's own flag scan -----------------------------------
	//
	// Three standard spellings of `-c` were unreadable, each for a different
	// reason, and the function's doc named one of them as an example of what it
	// COULD read. A cluster is a set of flags with no meaning to the order
	// inside it, `-o` takes a value, and `--` ends option processing.
	{cmd: `bash -cx "rm -rf /"`, want: wantFloor},
	{cmd: `bash -o pipefail -c "rm -rf /"`, want: wantFloor},
	{cmd: `zsh -o pipefail -c "rm -rf /"`, want: wantFloor},
	{cmd: `sh -c -- "rm -rf /"`, want: wantFloor},
	{cmd: `bash --rcfile /dev/null -c "rm -rf /"`, want: wantFloor},
	{cmd: `bash -c "bash -c \"bash -c 'rm -rf /'\""`, want: wantFloor},
	{cmd: `bash script.sh`, want: wantAllow,
		why: "a script path, not a -c payload. The negative sample for the rows above: without it " +
			"they could be satisfied by treating every bash operand as a command"},
	{cmd: `bash -x script.sh`, want: wantAllow, why: "same, with a flag in front of the script path"},

	// ---- A payload nobody read ----------------------------------------------
	//
	// W-B-03 decided that running out of unwrap budget is a refusal rather than
	// a pass, and then applied that decision to the budget alone. These are the
	// same situation reached by a different route: there is a payload, and this
	// package does not read the language it is written in. They are Prompts, not
	// floors — see opaque.go for why a refusal nobody can appeal needs a reason
	// that can be stated, and `python3 -c` is not it.
	{cmd: `python3 -c "import shutil;shutil.rmtree('/')"`, want: wantPrompt},
	{cmd: `perl -e "unlink '/etc/passwd'"`, want: wantPrompt},
	{cmd: `node -e "require('fs')"`, want: wantPrompt},
	{cmd: `python3 -c "print(1)"`, want: wantPrompt},
	{cmd: `bash +o posix -c "rm -rf /"`, want: wantPrompt},
	{cmd: `python3 script.py`, want: wantAllow,
		why: "the payload is in a FILE, not in this string — the same boundary `cat script.sh | sh` " +
			"has. It is the negative sample that stops the interpreter rows above from being " +
			"satisfied by refusing every python invocation"},
	{cmd: `awk '{print $1}' out10`, want: wantAllow,
		why: "awk's program is a positional operand here, not a -c/-e payload, and an operand this " +
			"reader does not treat as code is left alone"},
	{cmd: `tail -c 100 out10`, want: wantAllow,
		why: "`-c` here is a byte count. The looksLikeCode discriminator is what separates it from " +
			"`python3 -c`, and without this row that discriminator could be deleted unnoticed"},
	{cmd: `cut -c 1-5 out10`, want: wantAllow, why: "same: a character range, not a program"},
	{cmd: `gcc -c out10`, want: wantAllow, why: "same: compile-only, and the operand is a file name"},
	{cmd: `grep -e "foo bar" out10`, want: wantAllow,
		why: "`-e` here is a PATTERN and the operand does contain a space, so it is the " +
			"nonInterpreterPrograms relief table — not looksLikeCode — that keeps it out. Missing " +
			"an entry there costs one prompt; this row is what proves the entry is live"},
	{cmd: `git -c user.name=x commit -m "hi there"`, want: wantAllow, why: "same relief table"},

	// ---- Storage destroyers reached through their own operands --------------
	{cmd: `truncate -s 0 /dev/disk0`, want: wantFloor},
	{cmd: `tar -cf /dev/disk0 .`, want: wantFloor},
	{cmd: `tar -xf /dev/disk0 -C .`, want: wantAllow,
		why: "EXTRACTING from a device reads it. tar is in storageDestroyers under a narrower rule " +
			"than the rest of that table (see isStorageDestruction), and this is the row that fails " +
			"if the write-mode test is dropped and the whole program is refused"},
	{cmd: `tar -cf backup.tar .`, want: wantAllow,
		why: "creating an ordinary archive; the operand is a regular file, not a device"},
	{cmd: `chattr -i /etc/passwd`, want: wantAllow,
		why: "clearing the immutable attribute destroys nothing — it is a PRECURSOR to a deletion, " +
			"and the deletion that follows is what the gate grades. Measured and recorded rather " +
			"than closed, because a table of `programs that make a later command possible` has no " +
			"boundary this package could state"},

	// ---- Redirection corners ------------------------------------------------
	{cmd: `echo x <> out_rw`, want: wantAllow, why: "an ordinary read-write open under FS write `**`"},
	{cmd: `exec 3> out_fd3`, want: wantAllow, why: "an ordinary write under FS write `**`"},
	{cmd: `echo x {fd}> out_named`, want: wantAllow, why: "an ordinary write under FS write `**`"},
	{cmd: `echo x > a1 > a2`, want: wantAllow, why: "both targets reach the FS dimension and both are permitted by `**`"},
	{cmd: `echo x &>> outB`, want: wantAllow, why: "an ordinary append under FS write `**`"},
	{cmd: `echo x >| outA`, want: wantFloor,
		why: "OVER-STRICT, recorded rather than fixed: `>|` is the POSIX noclobber override and " +
			"ParseCommandList reads no target for it, so the whole command is refused structurally. " +
			"The direction is safe and the fix is a redirection-operator change; it is written down " +
			"here so the over-strictness cannot be mistaken for a defence"},
	{cmd: `echo hi > out10`, want: wantAllow, why: "the plain spelling, and the control sample for every row above it"},

	// ---- Credential targets -------------------------------------------------
	//
	// The denylist matches on the `~/.ssh` DIRECTORY PREFIX, so anything that
	// perturbs the prefix walks past it. The twelve plain spellings were already
	// caught; the expansion spellings were not.
	{cmd: `echo k > ~/.ssh/authorized_keys`, want: wantPrompt},
	{cmd: `echo k > ~/".ssh"/authorized_keys`, want: wantPrompt},
	{cmd: `echo k > ~/.s\sh/authorized_keys`, want: wantPrompt},
	{cmd: `echo k > ~/.ssh/../.ssh/authorized_keys`, want: wantPrompt},
	{cmd: `echo k > $HOME/.ssh/authorized_keys`, want: wantPrompt},
	{cmd: `echo k > ${HOME}/.ssh/authorized_keys`, want: wantPrompt},
	{cmd: `echo k > ~/.ssh//authorized_keys`, want: wantPrompt},
	{cmd: `echo k > ~/./.ssh/authorized_keys`, want: wantPrompt},
	{cmd: `echo k > ~/.ssh/authorized_keys > /dev/null`, want: wantPrompt},
	{cmd: `exec 3> ~/.ssh/authorized_keys`, want: wantPrompt},
	{cmd: `echo k >~/.ssh/authorized_keys`, want: wantPrompt},
	{cmd: `echo k > ~/$'\x2e\x73\x73\x68'/authorized_keys`, want: wantPrompt},
	{cmd: `echo k > ~/.s${x}sh/authorized_keys`, want: wantPrompt},
	{cmd: `echo k > ~/${x:-.ssh}/authorized_keys`, want: wantPrompt},
	{cmd: `echo k > ~/.ssh${x}/authorized_keys`, want: wantPrompt},
	{cmd: `X=.ssh; echo k > ~/$X/authorized_keys`, want: wantPrompt},
	// The rest of the expansion reading, which stopped at four constructs it
	// happened to have been written for. Every one of these ran `rm -rf /` under
	// a real /bin/sh while grading DestructionNone.
	{cmd: `IFS=,; X=rm,-rf,/; $X`, want: wantFloor},
	{cmd: `IFS=:; X=rm:-rf:/; $X`, want: wantFloor},
	{cmd: `X=rm,-rf,/; IFS=,; $X`, want: wantFloor},
	{cmd: `set -- /; rm -rf $1`, want: wantFloor},
	{cmd: `set -- /; rm -rf "$1"`, want: wantFloor},
	{cmd: `set -- rm; $1 -rf /`, want: wantFloor},
	{cmd: `readonly X=rm; $X -rf /`, want: wantFloor},
	{cmd: `bash -c 'X=rm; $X -rf /'`, want: wantFloor},
	{cmd: `X='; rm -rf /'; echo $X`, want: wantAllow,
		why: "OVER-STRICTNESS THAT WAS FIXED, kept as its regression sample. A POSIX shell does not " +
			"re-scan an expansion's result for control operators, so `echo` prints a semicolon and " +
			"/bin/sh runs nothing else. Pasting the value back as bare text made this a STRUCTURAL " +
			"HardDeny — unappealable in every mode including yolo — which is the exact cost ADR-0017 " +
			"rejected two other designs for"},
	{cmd: `X=' && rm -rf /'; echo $X`, want: wantAllow, why: "same, with the other operator"},
	{cmd: `MSG='rm -rf /'; echo "$MSG"`, want: wantAllow,
		why: "the quoted spelling of the same thing, which was already correct; it is here so the " +
			"two rows above cannot be satisfied by ceasing to resolve assignments at all"},
	{cmd: `echo k >| ~/.ssh/authorized_keys`, want: wantFloor,
		why: "the `>|` over-strictness above, on a credential target; refused for the wrong reason " +
			"but refused"},
	{cmd: `echo k 1>| ~/.ssh/authorized_keys`, want: wantFloor, why: "same"},
	// A wrapper payload redirects somewhere too, and to the outer reader the
	// whole payload is one quoted word. Not on the review's list — found while
	// closing `trap`, which is the same shape one table over — and measured
	// planting a key with no prompt while the identical redirection written at
	// the top level was refused.
	{cmd: `bash -c "echo k > ~/.ssh/authorized_keys"`, want: wantPrompt},
	{cmd: `sh -c 'echo k > ~/.ssh/authorized_keys'`, want: wantPrompt},
	{cmd: `eval "echo k > ~/.ssh/authorized_keys"`, want: wantPrompt},
	{cmd: `su -c "echo k > ~/.ssh/authorized_keys"`, want: wantPrompt},
	{cmd: `bash -c "echo hi > out.txt"`, want: wantAllow,
		why: "the negative sample for the four rows above: an ordinary payload write, permitted by " +
			"FS write `**`. Without it those four could be satisfied by refusing every -c payload"},
	// The target is an ARGUMENT rather than a redirection. This was recorded as
	// a one-program boundary (`tee`) and measured as a family of at least ten,
	// every one of which planted the key under a real /bin/sh. See argvwrite.go.
	{cmd: `tee ~/.ssh/authorized_keys`, want: wantPrompt},
	{cmd: `tee -a ~/.ssh/authorized_keys`, want: wantPrompt},
	{cmd: `cp /dev/null ~/.ssh/authorized_keys`, want: wantPrompt},
	{cmd: `mv /tmp/k ~/.ssh/authorized_keys`, want: wantPrompt},
	{cmd: `ln -sf /etc/passwd ~/.ssh/authorized_keys`, want: wantPrompt},
	{cmd: `install -m 600 /dev/stdin ~/.ssh/authorized_keys`, want: wantPrompt},
	{cmd: `sed -i s/x/y/ ~/.ssh/authorized_keys`, want: wantPrompt},
	{cmd: `sed -i -e s/x/y/ ~/.ssh/authorized_keys`, want: wantPrompt},
	{cmd: `dd of=/dev/null if=out10`, want: wantAllow,
		why: "dd's of= IS read as a write target now, and /dev/null is an ordinary one. The " +
			"negative sample for the dd entry in argvWriters"},
	{cmd: `cp out10 out11`, want: wantAllow,
		why: "an ordinary copy inside the work directory, permitted by FS write `**`. Without it " +
			"the cp/mv/ln rows above could be satisfied by refusing every copy"},
	{cmd: `sed -e 's/a b/c/' out10`, want: wantAllow,
		why: "no -i, so sed writes nothing; the requireFlags half of its entry is what this pins"},
	{cmd: `gzip -f /etc/shadow`, want: wantPrompt},
	{cmd: `gzip -f out10`, want: wantAllow,
		why: "gzip replaces its operand, which is a write — of a path inside the work directory " +
			"that FS write `**` permits"},

	// ---- PowerShell ---------------------------------------------------------
	//
	// This front-end is the newest attack surface in the package, and its first
	// version came with a regression attached: teaching the segmenter to read a
	// trailing `\` as a path separator made a family of commands READABLE that
	// the POSIX reader had been refusing outright for its trailing-escape error.
	// Readable is only an improvement if something then reads the payload, and
	// nothing knew `Invoke-Expression` (PowerShell's eval) or the `-Command` /
	// `/c` wrappers. The naive spelling was refused and the wrapped one passed —
	// the same inversion the prefix-runner table was written to end.
	{cmd: `Remove-Item -Recurse C:\`, want: wantFloor, interp: "powershell"},
	{cmd: `ri -Recurse C:\`, want: wantFloor, interp: "powershell"},
	{cmd: `rd /s C:\`, want: wantFloor, interp: "powershell"},
	{cmd: `Remove-Item -Recurse "C:\"`, want: wantFloor, interp: "powershell"},
	{cmd: `sudo Remove-Item -Recurse C:\`, want: wantFloor, interp: "powershell"},
	{cmd: "Remove-Item -Recurse `C:\\", want: wantFloor, interp: "powershell"},
	{cmd: `Remove-Item -Recurse --% C:\`, want: wantFloor, interp: "powershell"},
	{cmd: `Get-ChildItem C:\ | Remove-Item -Recurse`, want: wantFloor, interp: "powershell"},
	{cmd: `Invoke-Expression "Remove-Item -Recurse C:\"`, want: wantFloor, interp: "powershell"},
	{cmd: `Invoke-Expression 'Remove-Item -Recurse C:\'`, want: wantFloor, interp: "powershell"},
	{cmd: `iex "Remove-Item -Recurse C:\"`, want: wantFloor, interp: "powershell"},
	{cmd: `powershell -Command "Remove-Item -Recurse C:\"`, want: wantFloor, interp: "powershell"},
	{cmd: `pwsh -c "Remove-Item -Recurse C:\"`, want: wantFloor, interp: "powershell"},
	{cmd: `cmd /c "rd /s /q C:\"`, want: wantFloor, interp: "powershell"},
	{cmd: `del C:\ -Recurse`, want: wantPrompt, interp: "powershell"},
	{cmd: `Remove-Item -Recurse C:\temp\`, want: wantPrompt, interp: "powershell"},
	{cmd: `Remove-Item -Recurse $env:SystemDrive`, want: wantAllow, interp: "powershell",
		why: "the value of $env:SystemDrive is not in the string; same rule as $BUILD_DIR above"},
	{cmd: `Start-Process powershell -ArgumentList 'Remove-Item -Recurse C:\'`, want: wantAllow, interp: "powershell",
		why: "the payload is inside -ArgumentList, which is a PARAMETER of a launcher rather than a " +
			"wrapper's command operand. Reading it means modelling PowerShell's parameter binder; " +
			"measured and left open rather than half-covered"},
	{cmd: `Get-ChildItem C:\`, want: wantAllow, interp: "powershell",
		why: "an ordinary listing. It is here because the POSIX reader refuses it (trailing escape) " +
			"and the whole point of the PowerShell reader is that it must not"},
	{cmd: `Get-ChildItem C:\Users\me\`, want: wantAllow, interp: "powershell", why: "same"},
	{cmd: `Write-Output ssh-rsa > ~\.ssh\authorized_keys`, want: wantPrompt, interp: "powershell"},
	{cmd: `Write-Output $(Get-Date)`, want: wantFloor, interp: "powershell"},
	{cmd: `Start-Process notepad &`, want: wantFloor, interp: "powershell"},
	// The wrapper payload UNQUOTED, which is what an operator types. The quoted
	// spellings above were pinned and the unquoted ones were Allow, because the
	// unwrapper took ONE WORD after the flag rather than the rest of the line.
	{cmd: `powershell -Command Remove-Item -Recurse C:\`, want: wantFloor, interp: "powershell"},
	{cmd: `cmd /c rd /s /q C:\`, want: wantFloor, interp: "powershell"},
	// -EncodedCommand, and the prefixes PowerShell's binder accepts for it. The
	// previous round's report claimed each of its recorded boundaries was pinned
	// as a corpus row; these two were not, so the only record of them was a doc
	// comment nobody executes. The operand is base64 UTF-16, which this package
	// does not decode — that is exactly what DestructionOpaque is for.
	{cmd: `powershell -EncodedCommand cgBtACAALQByAGYAIAAvAA==`, want: wantPrompt, interp: "powershell"},
	{cmd: `powershell -enc cgBtACAALQByAGYAIAAvAA==`, want: wantPrompt, interp: "powershell"},
	{cmd: `powershell -e cgBtACAALQByAGYAIAAvAA==`, want: wantPrompt, interp: "powershell"},
	// Writing a file with a cmdlet is the ORDINARY spelling in this language, so
	// a reader that only judged redirections judged almost nothing PowerShell
	// writes.
	{cmd: `Set-Content ~\.ssh\authorized_keys 'k'`, want: wantPrompt, interp: "powershell"},
	{cmd: `Set-Content -Path ~\.ssh\authorized_keys -Value k`, want: wantPrompt, interp: "powershell"},
	{cmd: `Out-File -FilePath ~\.ssh\authorized_keys`, want: wantPrompt, interp: "powershell"},
	{cmd: `Add-Content -Path $HOME\.ssh\authorized_keys -Value k`, want: wantPrompt, interp: "powershell"},
	{cmd: `Set-Content out.txt 'k'`, want: wantAllow, interp: "powershell",
		why: "an ordinary write inside the work directory. The negative sample for the four rows " +
			"above, and the one that fails if the cmdlet's VALUE operand is graded as a path too"},

	// ---- cmd.exe: the third escape convention, with no reader ---------------
	//
	// docs/user-guide/guard.md records this boundary for operators and nothing
	// executed it. `^` is cmd's escape character; the POSIX reader leaves it in
	// the word, so the program word is not the program that runs.
	{cmd: `r^m -rf /`, want: wantAllow, interp: "cmd",
		why: "cmd.exe reads `r^m` as `rm`; this package has no cmd reader and reads the command " +
			"with the POSIX one, which leaves the caret in the program word. A stated boundary " +
			"(guard.md) that had no row until now — closing it means a third reader, not a table"},
	{cmd: `del /s /q C:\`, want: wantFloor, interp: "cmd",
		why: "unreachable why (the row is not Allow); kept adjacent as the control that shows the " +
			"UNESCAPED spelling of the row above is refused"},
}

// TestEveryMeasuredSpellingKeepsItsVerdict pins the verdict of every catalogued
// spelling. See the file header for why the table is not on its own sufficient.
func TestEveryMeasuredSpellingKeepsItsVerdict(t *testing.T) {
	if homeDir() == "" {
		t.Skip("no HOME/USERPROFILE: the credential rows are home-relative and cannot resolve")
	}
	g := New()
	prof := probeProfile()
	for _, row := range bypassCorpus {
		got := classOf(g.Check(prof, Action{
			Tool: "shell_run", Shell: row.cmd, Workdir: segTestWorkdir, Interpreter: row.interp,
		}))
		if got != row.want {
			t.Errorf("Check(%q, interpreter=%q) = %s, want %s", row.cmd, row.interp, got, row.want)
		}
		if row.want == wantAllow && row.why == "" {
			t.Errorf("corpus row %q is pinned to Allow with no justification; an Allow in this "+
				"catalogue has to say why it is one", row.cmd)
		}
	}
}

// TestNoSpellingTheShellExecutesIsAllowed is the honesty half of the catalogue.
//
// Every POSIX row is executed by a real /bin/sh with recorder shims on an
// otherwise empty PATH and HOME pointing at a temporary directory. A row whose
// run DESTROYED something or PLANTED A CREDENTIAL may not be pinned to Allow.
//
// Nothing destructive can happen: the shims delete nothing and the only `rm`
// resolvable is one of them (fidelity_test.go's header states the two
// independent reasons in full, and shimsAreLive below refuses to run the corpus
// if a shim is not intercepting).
//
// The PowerShell rows are skipped here rather than faked, because a reference
// reading has to come from the language being read. They are covered by the
// verdict table alone, which the file header records as the weaker of the two.
func TestNoSpellingTheShellExecutesIsAllowed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the reference reading comes from /bin/sh")
	}
	_, run := newShellHarness(t)
	shimsAreLive(t, run)
	home := os.Getenv("HOME")
	if home == "" {
		t.Fatal("newShellHarness did not set HOME; the credential rows cannot be witnessed")
	}
	work := filepath.Join(home, "work")
	keyPath := filepath.Join(home, ".ssh", "authorized_keys")
	if err := os.MkdirAll(filepath.Dir(keyPath), 0o700); err != nil {
		t.Fatal(err)
	}

	destroyed, planted := 0, 0
	for _, row := range bypassCorpus {
		if row.interp != "" {
			continue
		}
		if err := os.Remove(keyPath); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		got := run(row.cmd)
		_, keyErr := os.Stat(keyPath)
		harmed := shellRanAHarmfulArgv(work, got.ran)
		if keyErr == nil {
			planted++
		}
		if harmed {
			destroyed++
		}
		if !harmed && keyErr != nil {
			continue
		}
		if row.want == wantAllow {
			t.Errorf("corpus row %q is pinned to Allow, but /bin/sh ran %q and %s — a spelling the "+
				"shell acts on may not be pinned to Allow, whatever the justification says",
				row.cmd, got.ran, map[bool]string{true: "planted the credential file", false: "that argv is destructive on its own"}[keyErr == nil])
		}
	}
	// Self-proof: a harness whose shell executed nothing and planted nothing
	// would satisfy the loop above by asserting about the empty set.
	if destroyed == 0 || planted == 0 {
		t.Fatalf("the reference shell produced nothing to compare against (destructive=%d planted=%d); "+
			"the corpus is not exercising the harness", destroyed, planted)
	}
	if err := os.Remove(keyPath); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
}

// shellRanAHarmfulArgv reports whether any argv the recorders captured would, on
// its own, have destroyed something.
//
// It is ClassifyDestruction applied to the SHELL's reading rather than to the
// written command — the same reference the fidelity property uses — with one
// exemption. A deletion program invoked with NO path operand deletes nothing:
// `rm -rf` exits with a usage error. ClassifyDestruction grades that
// Catastrophic on purpose, because the operand may be an expansion the guard
// could not see, but as a witness of HARM it is a false positive. The corpus row
// `rm -rf $BUILD_DIR` produces exactly that argv on a shell where the variable
// is unset, and that row exists to prove such commands are NOT refused.
func shellRanAHarmfulArgv(work string, ran []string) bool {
	for _, actual := range ran {
		if ClassifyDestruction(actual, work) < DestructionOutOfScope {
			continue
		}
		program, args, ok := lexShellLite(actual)
		if ok && deletionPrograms[program] && len(deleteTargets(args)) == 0 {
			continue
		}
		return true
	}
	return false
}
