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
		why: "the payload is the PROGRAM NAME arriving on stdin; it is in no reading of this string, " +
			"and a reader that refused every pipe into sh would refuse the ordinary ones too"},

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
	{cmd: `truncate -s 0 /dev/disk0`, want: wantAllow,
		why: "`truncate` is in neither deletionPrograms nor storageDestroyers. A real gap, measured " +
			"and reported, and deliberately NOT closed in this batch: the fix is a storage.go table " +
			"entry with its own device-target predicate and it belongs with that table's work, not " +
			"smuggled into a corpus commit"},

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
	{cmd: `tee ~/.ssh/authorized_keys`, want: wantAllow,
		why: "the target is an ARGUMENT, not a redirection, and the FS dimension is fed from " +
			"Segment.Redirects. A known design boundary of where shell writes are judged from, " +
			"not a spelling that slipped past the reader — closing it means judging argv paths " +
			"per program, which is a different dimension"},

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
