package guard

import "strings"

// argvwrite.go answers the half of "where does this command write" that
// Segment.Redirects cannot.
//
// # The hole
//
// INF1 stopped refusing redirections and started JUDGING them: every `>` target
// goes through the FS dimension, which is what makes `echo k >
// ~/.ssh/authorized_keys` a prompt instead of a silent write. Sixteen spellings
// of that redirection are pinned in the corpus and all sixteen prompt.
//
// A redirection is not the only way to name the file you are writing. Measured,
// all reaching Allow while a real /bin/sh created the credential file:
//
//	tee -a ~/.ssh/authorized_keys              cp /dev/null ~/.ssh/authorized_keys
//	mv /tmp/k ~/.ssh/authorized_keys           ln -sf /etc/passwd ~/.ssh/authorized_keys
//	install -m 600 /dev/stdin ~/.ssh/…         sed -i s/x/y/ ~/.ssh/authorized_keys
//	dd of=/home/x/.ssh/authorized_keys
//
// The corpus recorded ONE of these — `tee` — as "a known design boundary of
// where shell writes are judged from". It was a boundary of at least ten
// programs, and on the PowerShell side it is not a corner at all: writing a file
// with a cmdlet (`Set-Content`, `Out-File`, `Add-Content`) is the ORDINARY way,
// and the redirection spelling is the unusual one. A defence that only reads
// redirections reads almost nothing of what PowerShell writes.
//
// # Membership rule, and the direction it fails in
//
// A program is here when writing to the named path is what the invocation IS —
// not when it merely might touch a file. `cp` writes its destination; `grep`
// does not write its operands. Adding a program produces one FS check on a path
// the command really does write, which under a permissive profile is a prompt on
// a credential path and nothing at all elsewhere. OMITTING one produces a silent
// write, which is what this file exists to stop, so the table errs toward
// inclusion.
//
// The paths are handed to checkFS RAW, exactly as checkRedirectTargets hands
// redirection targets over, so `~` and `$HOME` expansion and the built-in
// credential denylist all happen in the one place that already knows how.

// # The half of this file that is NOT a table
//
// argvWriters is a PROGRAM NAME TABLE, and opaque.go's header already says what
// happens to those: they are unbounded, so the default for an unrecognised
// spelling is a pass. The deletion dimension answered that with two readings
// that consult no name at all (classifyTrailingArgv reads every argv SUFFIX,
// classifyWordAsCommand reads every WORD). The write dimension had neither, and
// a close-out verification measured the consequence: checkSegmentWrites looked
// up the segment's FIRST program word and nothing else, so ONE prefix runner in
// front of a writer removed the entire FS write dimension.
//
//	tee -a ~/.ssh/authorized_keys              Prompt
//	sudo tee -a ~/.ssh/authorized_keys         Allow      ← and 16 more prefixes
//	zzrunner-nobody-knows tee -a ~/.ssh/…      Allow      ← an INVENTED name
//
// Under a profile that only permits writes inside the project tree the same
// prefix erased the profile's own answer: `sudo tee -a /etc/zz.conf` was Allow.
// Four of the spellings were witnessed by a real /bin/sh actually creating the
// credential file.
//
// segmentWriteTargets and outputFlagTargets below are the two name-independent
// readings, and they are deliberately the same two shapes the deletion side
// already uses:
//
//   - EVERY ARGV SUFFIX is read as a command in its own right, so a wrapper
//     table can be wrong, incomplete, or absent and the write is still seen.
//     This is classifyTrailingArgv's criterion, applied to the other dimension.
//   - A FLAG THAT MEANS "OUTPUT PATH" is read whatever program carries it, the
//     way codePayloadFlags reads "-c means the next word is code" for every
//     program. `curl -o`, `wget -O`, `gcc -o` and `sort -o` all name a file
//     they create, and none of them is (or should have to be) in the table.
//
// The failure directions differ and both are acceptable. A suffix or flag read
// WRONGLY yields one FS check on a path the command does not write, which is
// nothing under a permissive profile and one prompt under a narrow one. A suffix
// or flag NOT read yields a silent write, which is what this file exists to stop.

// argvWriteSpec describes where in a program's argv the path it WRITES is.
type argvWriteSpec struct {
	// valueFlags are options whose following word is a value, not an operand.
	valueFlags map[string]bool
	// requireFlags, when non-empty, means the program only writes its operands
	// when one of these flags is present: `sed` writes nothing without `-i`.
	requireFlags map[string]bool
	// lastOperandOnly selects the final operand instead of all of them, which
	// is the shape of every copy/move/link (`cp SRC… DEST`).
	lastOperandOnly bool
	// firstOperandOnly selects the leading operand, which is where every
	// PowerShell writing cmdlet binds its Path parameter positionally
	// (`Set-Content p v` writes p and stores v).
	firstOperandOnly bool
	// skipFirstOperand drops the leading operand, which for `sed` is the script
	// rather than a file. It is suppressed when the script was given with -e/-f.
	skipFirstOperand bool
	// prefixedValues are `name=value` operands whose value is the written path.
	// dd's `of=` is the only one.
	prefixedValues []string
	// pathFlags are the program's own options whose FOLLOWING word is the path
	// it writes, as opposed to valueFlags, which only says "skip the next word".
	// A spec listing a flag here must also list it in valueFlags, or the
	// operand would be counted twice.
	//
	// It is the generic form of what used to be a hard-coded powerShellPathFlags
	// lookup. Generalizing it is what lets `ssh-keygen -f ~/.ssh/authorized_keys`
	// be one row instead of a second mechanism.
	pathFlags map[string]bool
}

// argvWriters is the table. Entries are normalized program words (base-named,
// lowercased, .exe stripped), which is what lexShellLite produces.
var argvWriters = map[string]argvWriteSpec{
	// Every operand is written.
	"tee":      {},
	"truncate": {valueFlags: map[string]bool{"-s": true, "--size": true, "-r": true, "--reference": true}},
	// The compressors REPLACE their operand with a compressed copy, which is a
	// write of the original path's directory entry and a deletion of the file.
	"gzip": {}, "gunzip": {}, "bzip2": {}, "bunzip2": {}, "xz": {}, "unxz": {}, "zstd": {},

	// The destination is the last operand.
	"cp": {lastOperandOnly: true, valueFlags: map[string]bool{"-t": true, "--target-directory": true, "-S": true, "--suffix": true}},
	"mv": {lastOperandOnly: true, valueFlags: map[string]bool{"-t": true, "--target-directory": true, "-S": true, "--suffix": true}},
	"ln": {lastOperandOnly: true, valueFlags: map[string]bool{"-t": true, "--target-directory": true, "-S": true, "--suffix": true}},
	"install": {lastOperandOnly: true, valueFlags: map[string]bool{
		"-m": true, "--mode": true, "-o": true, "--owner": true,
		"-g": true, "--group": true, "-t": true, "--target-directory": true,
		"-S": true, "--suffix": true,
	}},

	// In-place editors write every file operand, and only with the in-place flag.
	"sed": {
		requireFlags:     map[string]bool{"-i": true, "--in-place": true},
		valueFlags:       map[string]bool{"-e": true, "--expression": true, "-f": true, "--file": true},
		skipFirstOperand: true,
	},
	"perl": {requireFlags: map[string]bool{"-i": true}, valueFlags: map[string]bool{"-e": true, "-E": true}},

	// dd names its destination with a prefixed value.
	"dd": {prefixedValues: []string{"of="}},

	// LANDING SOMEBODY ELSE'S BYTES ON A PATH YOU CHOSE. The generic
	// outputFlagTargets reading covers the `-o`/`-O` half of this family
	// (`curl -o`, `wget -O`, `base64 -o`); these three name their destination
	// positionally instead, so they need the row. All three satisfy the
	// membership rule above without argument: creating the named path is what
	// `touch` IS, and a copy's destination is a copy's destination whether the
	// source is local (cp, already here), remote (scp) or synced (rsync).
	"touch": {valueFlags: map[string]bool{"-d": true, "--date": true, "-r": true, "--reference": true, "-t": true}},
	"rsync": {lastOperandOnly: true, valueFlags: map[string]bool{
		"-e": true, "--rsh": true, "-T": true, "--temp-dir": true,
		"--exclude": true, "--include": true, "--filter": true, "-f": true,
		"--files-from": true, "--compare-dest": true, "--link-dest": true,
	}},
	"scp": {lastOperandOnly: true, valueFlags: map[string]bool{
		"-i": true, "-l": true, "-o": true, "-P": true, "-c": true, "-F": true, "-S": true, "-J": true,
	}},
	// `-f` names the key file ssh-keygen CREATES. It is a per-program row and
	// not an outputPathFlags member on purpose: `-f` is "the file to read" in
	// most programs that spell it.
	"ssh-keygen": {
		requireFlags: map[string]bool{"-f": true},
		valueFlags:   map[string]bool{"-f": true, "-N": true, "-C": true, "-t": true, "-b": true, "-P": true, "-m": true},
		pathFlags:    map[string]bool{"-f": true},
	},

	// DELIBERATELY ABSENT, so the next reader does not have to re-derive it:
	// `tar -C DIR`, `unzip -d DIR` and `git clone URL DIR` all write into a
	// DIRECTORY named by a flag or a trailing operand whose spelling is shared
	// with the read direction — `tar -C` is also how you extract FROM a
	// directory, `make -C` and `git -C` change directory and write nothing
	// there. Reading them would prompt on ordinary work, and what they place in
	// the directory comes out of an archive or a remote repository, which is the
	// boundary this package already records for payloads that are not in the
	// command string (see ADR-0020's Consequences).

	// PowerShell. The cmdlet spelling is the ordinary one, so this is not a
	// corner of the language the way `tee` is a corner of sh.
	"set-content":   {valueFlags: powerShellPathFlags, pathFlags: powerShellPathFlags, firstOperandOnly: true},
	"add-content":   {valueFlags: powerShellPathFlags, pathFlags: powerShellPathFlags, firstOperandOnly: true},
	"out-file":      {valueFlags: powerShellPathFlags, pathFlags: powerShellPathFlags, firstOperandOnly: true},
	"export-csv":    {valueFlags: powerShellPathFlags, pathFlags: powerShellPathFlags, firstOperandOnly: true},
	"export-clixml": {valueFlags: powerShellPathFlags, pathFlags: powerShellPathFlags, firstOperandOnly: true},
	"tee-object":    {valueFlags: powerShellPathFlags, pathFlags: powerShellPathFlags, firstOperandOnly: true},
	"new-item":      {valueFlags: powerShellPathFlags, pathFlags: powerShellPathFlags, firstOperandOnly: true},
	"sc":            {valueFlags: powerShellPathFlags, pathFlags: powerShellPathFlags, firstOperandOnly: true}, // Set-Content's alias
	"ac":            {valueFlags: powerShellPathFlags, pathFlags: powerShellPathFlags, firstOperandOnly: true}, // Add-Content's alias
}

// powerShellPathFlags are the parameter names whose value is the file a cmdlet
// writes. They are also the ONLY value-consuming parameters these cmdlets have
// that matter here — every other one (-Value, -Encoding, -Force) either carries
// data or is a switch, and mis-skipping a switch would only drop a positional.
var powerShellPathFlags = map[string]bool{
	"-path": true, "-filepath": true, "-literalpath": true, "-pspath": true,
	"-outfile": true, "-destination": true,
}

// segmentWriteTargets is the write dimension's name-INDEPENDENT reading of one
// segment: the paths written by the program in front, by every argv suffix
// behind it, and by any generic output flag anywhere in the argv.
//
// The suffix walk is classifyTrailingArgv's, deliberately: SUFFIXES rather than
// "the first non-flag word", because `chroot / tee -a ~/.ssh/authorized_keys`
// and `bwrap --dev-bind / / tee …` put bare operands where a generic flag walk
// expects the command, and a walk that stopped at the first one would look up a
// program called `/`.
//
// It costs nothing on the ordinary case: a suffix only produces a target when
// its own head word is a writer or carries an output flag, so `ls -la` and
// `git status` walk their argv and return nil.
//
// scriptEmitters are exempt exactly as they are in classifyTrailingArgv:
// `echo tee /etc/passwd` writes six words to stdout. The relief is that existing
// table rather than a new one.
//
// # The suffix reading is a WEAKER reading, and is scoped to say so
//
// The head word IS the program; a suffix word only MIGHT be, and program names
// collide with subcommand words. `apt-get install vim` puts coreutils' `install`
// — a real argvWriters entry, whose destination is its last operand — in a
// position where the last operand is a package name. Measured: it turned every
// `<tool> install <thing>` into a write of `<thing>` and, under a profile with
// no fs.write list at all, into a refusal.
//
// So a SUFFIX-derived target is taken only when its spelling LEAVES THE WORKING
// TREE (leavesWorkingTree). That is the range where the reading can change any
// answer: under every profile that permits writing in the project at all, a
// relative target is permitted anyway, so keeping it buys nothing and costs a
// false refusal on ordinary work. Head-derived and output-flag-derived targets
// are NOT scoped this way — `go build -o yanshi` really does write `yanshi`.
//
// The boundary that leaves behind, written down rather than argued away: under
// a profile whose fs.write is EMPTY, `sudo tee -a build/out.txt` is Allow where
// the unprefixed spelling is a refusal. Every measured member of the family this
// function exists for names an absolute or home path.
func segmentWriteTargets(program string, args []string) []string {
	if scriptEmitters[program] {
		return nil
	}
	out := append(argvWriteTargets(program, args), outputFlagTargets(args)...)
	for i, a := range args {
		if a == "--" || isFlagWord(a) {
			continue
		}
		w := normalizeProgramWord(a)
		if w == "" || scriptEmitters[w] {
			continue
		}
		// outputFlagTargets is NOT re-run per suffix: a suffix's flags are a
		// subset of the full argv's, so the single scan above already saw them.
		for _, t := range argvWriteTargets(w, args[i+1:]) {
			if leavesWorkingTree(t) {
				out = append(out, t)
			}
		}
	}
	return out
}

// wordWriteTargets is the write dimension's reading of a command written INTO
// ONE WORD, and it is the exact counterpart of what classifyAssignmentPrefix and
// classifyWordAsCommand do for the deletion dimension (ADR-0020).
//
// # The gap it closes
//
// segmentWriteTargets above reads the segment's PROGRAM WORD and its argv
// SUFFIXES. Neither reading can see a command that is a single word, and the
// most ordinary way to write one is POSIX assignment syntax: lexShellLite WALKS
// PAST an assignment prefix (assignmentPrefixLen) to reach the program word, so
// the VALUE was in no reading the write dimension had. Measured, all reaching
// Allow under BOTH `fs.write: ["**"]` and an fs.write narrowed to the project
// tree, while the unprefixed spelling of the same write is a Prompt:
//
//	GIT_SSH_COMMAND='tee ~/.ssh/authorized_keys' git fetch
//	EDITOR='dd of=~/.ssh/authorized_keys' crontab -e
//	env GIT_SSH_COMMAND='curl -o ~/.ssh/authorized_keys http://h/k' git fetch
//	SUDO_ASKPASS='tee /etc/sudoers.d/zz' sudo -A id
//
// The third spelling is why this reads EVERY token rather than only the
// assignment prefix: with a runner in front, the assignment is an ordinary argv
// word and assignmentPrefixLen is 0. That is the same reason classifyTrailingArgv
// runs classifyWordAsCommand over every argv word instead of only over the
// prefix classifyAssignmentPrefix already covers.
//
// # The criterion is the value, not a list of variable names
//
// No name is consulted, on either side: a made-up variable in front of a made-up
// program (`ZQ_NOBODY_READS_THIS='tee ~/.ssh/authorized_keys' zq-nobody-runs-this`)
// is read exactly like GIT_SSH_COMMAND in front of git. The bypasscorpus rows
// carrying `zq-` names are what fail if this becomes a table.
//
// # Scoped to targets that LEAVE THE WORKING TREE, for the reason the suffix
// # walk is
//
// A word only MIGHT be a command — `git commit -m "cp a b"` is a message, not a
// copy — which is the same uncertainty that caps classifyWordAsCommand at
// DestructionOpaque. So a word-derived target is taken only when its spelling
// leaves the working tree (leavesWorkingTree), the range where the reading can
// change an answer at all. Without that scope, `MAKEFLAGS='-j 8 -O out.log' make`
// and `ZZ='cp src.txt dst.txt' make` become refusals under a project-scoped
// profile; with it, the ordinary-command sample measured 0 new prompts.
//
// The boundary this leaves is the same one segmentWriteTargets records for its
// suffix walk: under a profile whose fs.write is EMPTY, a relative target inside
// an assignment value is Allow where the unprefixed spelling is a refusal. Every
// measured member of the family this exists for names an absolute or home path.
//
// # What it does not read
//
// One level. A value that is itself a wrapper carrying a payload
// (`EDITOR='sh -c "echo k > ~/.ssh/authorized_keys"' crontab -e`) is not
// descended into here — that shape is already a Prompt from the deletion
// dimension's opaque tier, which is where an unread payload belongs. A value
// naming a PATH rather than a command (`BASH_ENV=./payload.sh`) is the file
// boundary ADR-0020 records and stops at.
func wordWriteTargets(cmd string) []string {
	tokens, ok := lexShellLiteTokens(cmd, false)
	if !ok {
		return nil
	}
	var out []string
	for _, tok := range tokens {
		for _, reading := range commandReadingsOfWord(tok) {
			// A LEADING PIPE is not POSIX syntax — execpolicy.ParseCommandList
			// refuses it as "operator without executable segment" — so where one
			// appears at the head of a value it is the "run this as a filter"
			// convention (less's LESSOPEN, mailcap), which is precisely the case
			// where the value IS handed to a shell. Without the trim,
			// `LESSOPEN='|tee ~/.ssh/authorized_keys %s' less f` lexes to the
			// program word `|tee`, which is in no table, and stays Allow while
			// its unpiped spelling is a Prompt. The rule reads the VALUE's
			// spelling and no variable name, same as everything else here.
			program, args, lexed := lexShellLite(strings.TrimPrefix(reading, "|"))
			if !lexed {
				continue
			}
			for _, t := range segmentWriteTargets(program, args) {
				if leavesWorkingTree(t) {
					out = append(out, t)
				}
			}
		}
	}
	return out
}

// leavesWorkingTree reports whether a path's SPELLING names somewhere other than
// a location under the directory the command runs in: an absolute path, a home
// reference, an unresolved expansion (which could be either, so it counts), a
// Windows drive-qualified path, or a `..` escape.
//
// It is deliberately lexical. checkFS owns normalization — pre-normalizing here
// would rewrite a relative target into an absolute one and stop a profile
// written as `write: ["src/**"]` from matching, which is the tightening-by-
// accident checkRedirectTargets' header refuses.
func leavesWorkingTree(p string) bool {
	if p == "" {
		return false
	}
	switch p[0] {
	case '/', '\\', '~', '$':
		return true
	}
	if len(p) >= 2 && p[1] == ':' { // C:\Users\…, and UNC's \\ is covered above
		return true
	}
	return p == ".." || strings.HasPrefix(p, "../") || strings.HasPrefix(p, `..\`) ||
		strings.Contains(p, "/../") || strings.Contains(p, `\..\`)
}

// outputPathFlags are the option spellings that mean "write your output to this
// path", in every program that has the notion.
//
// This is the write dimension's codePayloadFlags, and it is short for the same
// reason that set is: the value is that the reading consults NO PROGRAM NAME, so
// `curl -o ~/.ssh/authorized_keys url` — the standard way to land remote content
// on a chosen path, and the shape the guard's own risk prompt calls
// "download and run" — is judged without curl being in any table.
//
// Entries earn a place when the flag's operand is a path the invocation CREATES
// in essentially every program that spells the flag that way. `-C` is absent
// although `tar -C` and `unzip -d` really do write there: it means "change
// directory" for `make`, `git` and `find` too, so reading it would prompt on
// ordinary work whose write lands nowhere near the named directory. `-f` is
// absent because it is the archive tar READS as often as the one it writes.
func outputPathFlags(f string) bool {
	switch f {
	case "-o", "-O", "--output", "--output-document", "--output-file":
		return true
	}
	return false
}

// outputFlagTargets returns the paths an argv names with a generic output flag.
//
// Three operand shapes are skipped, and each one is a measured false positive
// rather than a precaution:
//
//   - a URL. `curl -O https://x/k` spells a SWITCH the same way wget spells its
//     output flag, and the operand after it is the URL.
//   - a `key=value` word. `ssh -o StrictHostKeyChecking=no` and `mount -o` put
//     settings there; opaque.go's admission rule names the same shape as the
//     general-purpose key/value channel it refuses to read as one type.
//   - a bare `-`, which is stdout by convention.
func outputFlagTargets(args []string) []string {
	var out []string
	keep := func(v string) {
		if v == "" || v == "-" || isFlagWord(v) ||
			strings.Contains(v, "=") || strings.Contains(v, "://") {
			return
		}
		out = append(out, v)
	}
	for i, a := range args {
		if a == "--" {
			break
		}
		if base, attached, found := strings.Cut(a, "="); found && outputPathFlags(base) {
			keep(attached)
			continue
		}
		if outputPathFlags(a) && i+1 < len(args) {
			keep(args[i+1])
		}
	}
	return out
}

// argvWriteTargets returns the paths a command writes that are named as
// OPERANDS rather than as redirection targets.
//
// PowerShell parameter names are matched case-insensitively, because the
// language's own binder is: `-Path`, `-path` and `-PATH` are one parameter.
func argvWriteTargets(program string, args []string) []string {
	spec, ok := argvWriters[program]
	if !ok {
		return nil
	}
	// A path named inside the program's own little language rather than in its
	// argv. sed is the only one here; see sedScriptWriteTargets.
	out := sedScriptWriteTargets(program, args)
	if len(spec.requireFlags) > 0 && !hasAnyFlag(args, spec.requireFlags) {
		return out
	}
	return append(out, argvWriteOperands(spec, args)...)
}

// sedScriptWriteTargets returns the files a sed SCRIPT writes.
//
// argvWriters gives sed a requireFlags of `-i`, on the reading that sed writes
// nothing without it. That is true of its OPERANDS and false of its script:
// `w FILE` and the `w` flag of `s///` create or truncate the named file, so
// `sed -e 'w /etc/shadow' f` wrote a path the FS dimension never saw.
//
// The scan is deliberately shallow — sed's language is not parsed, only the two
// spellings that name a file are looked for, and a `w` is only believed where
// sed can put one: at the start of a command, after an address, or after the
// closing delimiter of an s-command. `sed 's/low /high/'` contains the letters
// but in none of those positions.
func sedScriptWriteTargets(program string, args []string) []string {
	if program != "sed" {
		return nil
	}
	var out []string
	sawScriptFlag := false
	var operands []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			operands = append(operands, args[i+1:]...)
			break
		}
		if !isFlagWord(a) {
			operands = append(operands, a)
			continue
		}
		switch {
		case a == "-e" || a == "--expression":
			sawScriptFlag = true
			if i+1 < len(args) {
				out = append(out, sedWriteFiles(args[i+1])...)
				i++
			}
		case strings.HasPrefix(a, "--expression="):
			sawScriptFlag = true
			out = append(out, sedWriteFiles(strings.TrimPrefix(a, "--expression="))...)
		case a == "-f" || a == "--file":
			// The script is in a file, which is the same boundary
			// `bash script.sh` has.
			sawScriptFlag = true
			i++
		}
	}
	if !sawScriptFlag && len(operands) > 0 {
		out = append(out, sedWriteFiles(operands[0])...)
	}
	return out
}

// sedWriteFiles finds the `w FILE` targets in one sed script.
func sedWriteFiles(script string) []string {
	var out []string
	for _, piece := range strings.FieldsFunc(script, func(r rune) bool { return r == ';' || r == '\n' }) {
		piece = strings.TrimSpace(piece)
		i := strings.Index(piece, "w ")
		if i < 0 {
			continue
		}
		if i > 0 {
			// The byte before a real `w` is an s-command's closing delimiter,
			// the end of a numeric or `$` address, or another flag letter of the
			// same s-command (`s/x/y/gw FILE`).
			if p := piece[i-1]; p != '/' && p != '$' && !(p >= '0' && p <= '9') && !isASCIILetter(p) {
				continue
			}
		}
		if f := strings.TrimSpace(piece[i+2:]); f != "" {
			out = append(out, f)
		}
	}
	return out
}

// argvWriteOperands is argvWriteTargets' argv walk, split out so a program that
// also names paths inside its own script language can have both.
func argvWriteOperands(spec argvWriteSpec, args []string) []string {
	var named []string
	var operands []string
	sawValueFlag := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			operands = append(operands, args[i+1:]...)
			break
		}
		for _, p := range spec.prefixedValues {
			if v, found := strings.CutPrefix(a, p); found {
				named = append(named, v)
			}
		}
		if isFlagWord(a) {
			key := strings.ToLower(a)
			if spec.valueFlags[a] || spec.valueFlags[key] {
				sawValueFlag = true
				if (spec.pathFlags[a] || spec.pathFlags[key]) && i+1 < len(args) {
					named = append(named, args[i+1])
				}
				i++
			}
			continue
		}
		if len(spec.prefixedValues) > 0 && strings.Contains(a, "=") {
			continue // dd's other operands (if=, bs=, count=) are not paths
		}
		operands = append(operands, a)
	}
	if spec.skipFirstOperand && !sawValueFlag && len(operands) > 0 {
		operands = operands[1:]
	}
	if spec.lastOperandOnly && len(operands) > 0 {
		operands = operands[len(operands)-1:]
	}
	if spec.firstOperandOnly && len(operands) > 0 {
		operands = operands[:1]
	}
	if len(spec.prefixedValues) > 0 {
		// dd writes only what `of=` names; its bare operands are not paths.
		return named
	}
	if len(named) > 0 {
		// A cmdlet that named its path explicitly does not also write its
		// positional operands — `Set-Content -Path f -Value 'x'` writes f.
		return named
	}
	return operands
}

// hasAnyFlag reports whether args carries one of the given option spellings,
// accepting the clustered and `=value` forms (`sed -i.bak`, `--in-place=.bak`).
func hasAnyFlag(args []string, want map[string]bool) bool {
	for _, a := range args {
		if want[a] {
			return true
		}
		if !strings.HasPrefix(a, "-") {
			continue
		}
		if base, _, found := strings.Cut(a, "="); found && want[base] {
			return true
		}
		// `-i.bak` is `-i` with an attached suffix argument.
		for w := range want {
			if len(w) == 2 && strings.HasPrefix(a, w) && !strings.HasPrefix(a, "--") {
				return true
			}
		}
	}
	return false
}
