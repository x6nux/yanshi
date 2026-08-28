package guard

import "strings"

// expansion.go gives the guard a second READING of a command: the one a POSIX
// shell produces after parameter expansion.
//
// # The hole it closes
//
// Neither reader expanded anything. Measured, all of these graded
// DestructionNone and reached Allow under a profile whose only possible refusal
// is a structural floor, while /bin/sh executed `rm -rf /` for every one:
//
//	rm${IFS}-rf${IFS}/        ${x:-rm} -rf /      rm -rf ${x:-/}
//	${IFS}rm -rf /            ${x-rm} -rf /       rm -rf "${x:-/}"
//	X=rm; $X -rf /            X=/; rm -rf $X      set -- rm -rf /; "$@"
//
// and these planted a key in ~/.ssh with no prompt:
//
//	echo k > ~/${x:-.ssh}/authorized_keys
//	X=.ssh; echo k > ~/$X/authorized_keys
//
// `rm -rf "${x:-/}"` is the one worth reading twice: it carries no `$(`, so
// hasControlOperator never fires, nothing is re-split and nothing is re-decoded.
// It is a straight-line path from a plain-looking string to Allow.
//
// # Why "only what the string defines", and not "blank everything"
//
// The obvious rule — treat every unresolved expansion as empty, which is what a
// shell does with an unset variable — is a REGRESSION, and a structural one.
// `rm -rf $BUILD_DIR` becomes `rm -rf`, a recursive delete with no target, which
// the catastrophic tier refuses in every mode including yolo. An ordinary
// command emitted by every model that has ever cleaned a build directory would
// become permanently unrunnable, and the deletion gate's own header says over-
// strictness in this direction is worse than one more prompt.
//
// So an expansion is resolved only when its VALUE IS PRESENT IN THE COMMAND
// STRING: ${IFS} (whose value is a space in every POSIX shell), a ${v:-default}
// whose default is written right there, an assignment earlier in the same
// command, or a `set --` earlier in the same command. Anything whose value comes
// from outside is copied through unchanged, exactly as before.
//
// That rule is also why this can never widen what runs: the result is graded as
// an ADDITIONAL reading and folded with moreSevere, so a resolution can reveal
// danger and never launder it — the same rule classifyLexed applies to wrapper
// payloads and unescapeWordLetters applies to escapes.
//
// # What is deliberately not resolved
//
// $1..$9, $$, $?, $! and $0 have values, but none of them is attacker-chosen
// from inside this string. Command substitution `$(…)` and `$'…'` are other
// readers' jobs (ParseCommandList refuses the first; lexShellLite decodes the
// second) and are copied through byte for byte, including their contents.

// expandKnownParameters rewrites cmd with every parameter expansion the string
// itself defines replaced by its value, and reports whether anything changed.
//
// Definitions are collected LEFT TO RIGHT at control-operator boundaries, so a
// value assigned after a use does not reach back to it: `rm -rf $X; X=/` keeps
// its `$X`, which is what the shell does.
func expandKnownParameters(cmd string) (string, bool) {
	// "$@" and "$*" are the one construct whose QUOTING changes the word count:
	// they expand to one word per positional parameter, not to one word holding
	// all of them. Dropping the quotes before the scan is how that word split is
	// modelled without teaching the emitter to reopen a quote mid-value.
	pre := strings.NewReplacer(`"$@"`, `$@`, `"$*"`, `$*`).Replace(cmd)

	e := &expander{vars: map[string]string{}}
	out, changed := e.run(pre)
	return out, changed || pre != cmd
}

// expander holds the left-to-right scan state: the values the command has
// defined so far, and the words of the segment currently being read.
type expander struct {
	vars       map[string]string
	positional []string
	words      []string
	word       strings.Builder
	inWord     bool
}

func (e *expander) run(cmd string) (string, bool) {
	var out strings.Builder
	changed := false
	quote := byte(0)
	for i := 0; i < len(cmd); {
		c := cmd[i]
		if quote == '\'' {
			// Single quotes suppress expansion entirely, which is the whole
			// reason this scan tracks them: `echo '$HOME'` prints six bytes.
			if c == '\'' {
				quote = 0
			} else {
				e.put(c)
			}
			out.WriteByte(c)
			i++
			continue
		}
		switch {
		case c == '\\':
			out.WriteByte(c)
			if i+1 < len(cmd) {
				out.WriteByte(cmd[i+1])
				e.put(cmd[i+1])
				i += 2
				continue
			}
			i++
		case c == '\'' || c == '"':
			// The quote bytes themselves never join the logical word, so
			// endSegment sees `X=rm` for both `X=rm` and `X="rm"`.
			if quote == c {
				quote = 0
			} else if quote == 0 {
				quote = c
			}
			out.WriteByte(c)
			e.inWord = true
			i++
		case c == '$':
			lit, next, ok := e.expandAt(cmd, i)
			if !ok {
				out.WriteByte(c)
				e.put(c)
				i++
				continue
			}
			out.WriteString(lit)
			e.putString(lit)
			changed = true
			i = next
		case quote != 0:
			out.WriteByte(c)
			e.put(c)
			i++
		case c == ' ' || c == '\t':
			e.endWord()
			out.WriteByte(c)
			i++
		case c == ';' || c == '&' || c == '|' || c == '\n' || c == '\r':
			e.endSegment()
			out.WriteByte(c)
			i++
		default:
			out.WriteByte(c)
			e.put(c)
			i++
		}
	}
	e.endSegment()
	return out.String(), changed
}

func (e *expander) put(b byte) {
	e.word.WriteByte(b)
	e.inWord = true
}

func (e *expander) putString(s string) {
	e.word.WriteString(s)
	e.inWord = true
}

func (e *expander) endWord() {
	if !e.inWord {
		return
	}
	e.words = append(e.words, e.word.String())
	e.word.Reset()
	e.inWord = false
}

// endSegment closes the current command and records the definitions it made.
//
// Three shapes define something a later segment can use, and all three are what
// the shell itself does with them:
//
//	X=rm            an assignment word, whether alone or prefixing a command
//	export X=rm     the same, behind the builtin that also exports it
//	set -- a b c    the positional parameters $@ / $* / $1…
func (e *expander) endSegment() {
	e.endWord()
	words := e.words
	e.words = nil
	if len(words) == 0 {
		return
	}
	if words[0] == "export" || words[0] == "declare" || words[0] == "typeset" || words[0] == "local" {
		words = words[1:]
	}
	if len(words) > 0 && words[0] == "set" {
		for i := 1; i < len(words); i++ {
			if words[i] == "--" {
				e.positional = append([]string(nil), words[i+1:]...)
				return
			}
		}
		return
	}
	for _, w := range words {
		name, value, ok := splitAssignmentWord(w)
		if !ok {
			// Only a LEADING run of assignments belongs to the command; a
			// `KEY=value` further along is an operand (`grep FOO=1 file`).
			return
		}
		e.vars[name] = value
	}
}

// splitAssignmentWord splits `NAME=VALUE` into its halves. It reuses
// isAssignmentWord's notion of a valid name so the lexer and this scan cannot
// disagree about what an assignment is.
func splitAssignmentWord(w string) (name, value string, ok bool) {
	if !isAssignmentWord(w) {
		return "", "", false
	}
	eq := strings.IndexByte(w, '=')
	return w[:eq], w[eq+1:], true
}

// expandAt resolves the expansion beginning at cmd[i] (which is a '$') and
// returns its literal value plus the index just past it. ok=false means "not an
// expansion this reader resolves", and the caller then copies the '$' through
// unchanged.
func (e *expander) expandAt(cmd string, i int) (string, int, bool) {
	if i+1 >= len(cmd) {
		return "", 0, false
	}
	switch cmd[i+1] {
	case '(', '\'':
		// Command substitution and ANSI-C quoting belong to other readers.
		return "", 0, false
	case '@', '*':
		if len(e.positional) == 0 {
			return "", 0, false
		}
		return strings.Join(e.positional, " "), i + 2, true
	case '{':
		return e.expandBraced(cmd, i)
	}
	name, next := scanName(cmd, i+1)
	if name == "" {
		return "", 0, false
	}
	v, ok := e.lookup(name)
	if !ok {
		return "", 0, false
	}
	return v, next, true
}

// expandBraced resolves `${NAME}` and the four default-value forms
// `${NAME:-W}`, `${NAME-W}`, `${NAME:=W}`, `${NAME=W}`.
//
// The `:+` / `+` forms are NOT resolved: their word is used only when the
// variable IS set, and a variable this reader cannot see is one it must assume
// nothing about. Resolving them to W would be inventing a value rather than
// reading one.
func (e *expander) expandBraced(cmd string, i int) (string, int, bool) {
	rel := strings.IndexByte(cmd[i:], '}')
	if rel < 0 {
		return "", 0, false
	}
	end := i + rel + 1
	body := cmd[i+2 : end-1]
	if body == "@" || body == "*" {
		if len(e.positional) == 0 {
			return "", 0, false
		}
		return strings.Join(e.positional, " "), end, true
	}
	name, rest := body, ""
	if k := strings.IndexAny(body, ":-=+?#%"); k > 0 {
		name, rest = body[:k], body[k:]
	}
	if name == "" || !isAssignmentWord(name+"=") {
		return "", 0, false
	}
	if v, ok := e.lookup(name); ok {
		return v, end, true
	}
	for _, op := range []string{":-", ":=", "-", "="} {
		if def, found := strings.CutPrefix(rest, op); found {
			return def, end, true
		}
	}
	return "", 0, false
}

// lookup resolves a variable name to the value this command string gives it.
//
// IFS is the one name with a value the string does not have to state: its POSIX
// default is space-tab-newline, and `rm${IFS}-rf${IFS}/` is the spelling that
// makes that matter. A space is the reading that reproduces what the shell does
// with the default value; an explicit assignment in the command wins over it.
func (e *expander) lookup(name string) (string, bool) {
	if v, ok := e.vars[name]; ok {
		return v, true
	}
	if name == "IFS" {
		return " ", true
	}
	return "", false
}

// scanName reads a bare `$NAME` identifier starting at i and returns it with the
// index just past it. An empty name means the byte at i does not start one.
func scanName(cmd string, i int) (string, int) {
	j := i
	for j < len(cmd) {
		c := cmd[j]
		if isASCIILetter(c) || c == '_' || (j > i && c >= '0' && c <= '9') {
			j++
			continue
		}
		break
	}
	return cmd[i:j], j
}

// elideExpansions removes every parameter expansion from a path, so a path whose
// SEGMENTS are broken up by an expansion can still be matched against a table of
// literal segments.
//
// It is the credential denylist's half of the same problem expandKnownParameters
// solves for the deletion gate, and it takes the opposite decision about unknown
// values ON PURPOSE. The denylist matches on a `~/.ssh` directory prefix, and
// `~/.s${x}sh/authorized_keys` is that prefix with an empty expansion spliced
// into it — measured planting a key with no prompt. Blanking is exactly right
// here and exactly wrong there, because the two failure directions are not the
// same: an extra reading of a PATH can only produce one more prompt on a
// credential-shaped path, while an extra reading of a DELETION can produce an
// unappealable refusal of an ordinary command.
func elideExpansions(p string) (string, bool) {
	if !strings.Contains(p, "$") {
		return p, false
	}
	var out strings.Builder
	changed := false
	for i := 0; i < len(p); {
		if p[i] != '$' || i+1 >= len(p) {
			out.WriteByte(p[i])
			i++
			continue
		}
		if p[i+1] == '{' {
			if rel := strings.IndexByte(p[i:], '}'); rel >= 0 {
				i += rel + 1
				changed = true
				continue
			}
		}
		if name, next := scanName(p, i+1); name != "" {
			i = next
			changed = true
			continue
		}
		out.WriteByte(p[i])
		i++
	}
	return out.String(), changed
}
