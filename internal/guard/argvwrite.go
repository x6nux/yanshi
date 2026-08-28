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

	// PowerShell. The cmdlet spelling is the ordinary one, so this is not a
	// corner of the language the way `tee` is a corner of sh.
	"set-content":   {valueFlags: powerShellPathFlags, firstOperandOnly: true},
	"add-content":   {valueFlags: powerShellPathFlags, firstOperandOnly: true},
	"out-file":      {valueFlags: powerShellPathFlags, firstOperandOnly: true},
	"export-csv":    {valueFlags: powerShellPathFlags, firstOperandOnly: true},
	"export-clixml": {valueFlags: powerShellPathFlags, firstOperandOnly: true},
	"tee-object":    {valueFlags: powerShellPathFlags, firstOperandOnly: true},
	"new-item":      {valueFlags: powerShellPathFlags, firstOperandOnly: true},
	"sc":            {valueFlags: powerShellPathFlags, firstOperandOnly: true}, // Set-Content's alias
	"ac":            {valueFlags: powerShellPathFlags, firstOperandOnly: true}, // Add-Content's alias
}

// powerShellPathFlags are the parameter names whose value is the file a cmdlet
// writes. They are also the ONLY value-consuming parameters these cmdlets have
// that matter here — every other one (-Value, -Encoding, -Force) either carries
// data or is a switch, and mis-skipping a switch would only drop a positional.
var powerShellPathFlags = map[string]bool{
	"-path": true, "-filepath": true, "-literalpath": true, "-pspath": true,
	"-outfile": true, "-destination": true,
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
	if len(spec.requireFlags) > 0 && !hasAnyFlag(args, spec.requireFlags) {
		return nil
	}
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
				if powerShellPathFlags[key] && i+1 < len(args) {
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
