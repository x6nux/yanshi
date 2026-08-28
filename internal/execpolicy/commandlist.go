package execpolicy

import (
	"fmt"
	"strings"
)

// ParseCommandList splits a shell command string into the top-level segments a
// shell would run, so a policy layer can judge each one instead of refusing the
// whole string.
//
// # Why this is a SECOND front-end and not a widening of Lex
//
// Lex is deliberately strict: it rejects globs (`*`, `?`, `[`), `$VAR`
// expansion and Windows `%VAR%` because the execpolicy RULE engine matches on
// literal program+argument words, and a rule cannot honestly reason about a
// word whose value it cannot see. That strictness is correct for rules and
// wrong for splitting: `guard.Guard.checkShell` runs on EVERY profile,
// including the default glob one where `ls *.go` is an ordinary command that
// has always been allowed. Routing the split through Lex would turn the
// segmenter into a glob ban.
//
// So this scanner is lenient about WORD CONTENT and strict about STRUCTURE. It
// only needs to answer two questions — where does one command end and the next
// begin, and where does a redirection point — and it refuses, fail-closed, every
// construct that would make either answer a guess.
//
// # Refused forms (each becomes a structural HardDeny at the guard layer)
//
//   - command substitution `$(…)` and backticks — the text that actually runs
//     is not in this string at all;
//   - process substitution `<(…)` / `>(…)` — same, plus it is not a redirect
//     target;
//   - subshell grouping `(` / `)` — an unquoted paren makes the first word of
//     the group something other than the program, which is exactly how
//     `(rm -rf /)` used to walk past the deletion classifier's lexer;
//   - here-documents `<<` / `<<<` — the payload lives on lines this string does
//     not contain;
//   - background `&` — a detached child outlives the turn that authorized it;
//   - raw newline / carriage return — a second command line, not a segment;
//   - unterminated quotes and a trailing backslash — the string is truncated,
//     so any reading of it is a guess.
//
// A bare `$` (as in `$HOME`) is NOT refused: it carries no structure, the
// pre-existing metacharacter list never rejected it, and the destructive
// classifier resolves the home references it cares about itself
// (guard/pathnorm.go).
//
// # What each Segment carries
//
// Program/Args are the unquoted words (Program normalized the same way
// Parse normalizes it, so prefix rules match across `/usr/bin/go` and `go.exe`).
// Redirects carry the operator and its target word. Operator is the control
// operator that separates this segment from the NEXT one, NoOperator on the
// last. Text is the VERBATIM source slice for this segment — not a re-join of
// Program+Args, because re-joining loses quoting and `rm -rf "/my dir"` would
// come back as two targets.
func ParseCommandList(raw string) ([]Segment, error) {
	s := &listScanner{raw: raw, segFrom: -1}
	if err := s.run(); err != nil {
		return nil, err
	}
	if len(s.segs) == 0 {
		return nil, fmt.Errorf("execpolicy: empty command")
	}
	return s.segs, nil
}

// listScanner holds the byte-loop state for ParseCommandList. It is a struct
// rather than a pile of locals so the word/redirect/segment flushes can be
// three small methods instead of three copies of the same closure.
type listScanner struct {
	raw  string
	i    int
	segs []Segment

	word    strings.Builder
	inWord  bool
	words   []string
	redirs  []RedirectSpec
	segFrom int // byte offset of the current segment's first token, -1 = none

	// pendingRedirect indexes the RedirectSpec still waiting for its target
	// word, or -1. A redirection whose target never arrives is a parse error:
	// "cat >" names no file.
	pendingRedirect int
}

// run drives the scan and flushes the trailing segment.
func (s *listScanner) run() error {
	s.pendingRedirect = -1
	for s.i < len(s.raw) {
		c := s.raw[s.i]
		switch {
		case c == '\'' || c == '"':
			if err := s.scanQuoted(c); err != nil {
				return err
			}
		case c == '\\':
			if s.i+1 >= len(s.raw) {
				return fmt.Errorf("execpolicy: trailing escape")
			}
			s.startToken()
			s.word.WriteByte(s.raw[s.i+1])
			s.inWord = true
			s.i += 2
		case c == ' ' || c == '\t':
			s.flushWord()
			s.i++
		case c == '\n' || c == '\r':
			return fmt.Errorf("execpolicy: newline is a second command line, not a segment")
		case c == '`':
			return fmt.Errorf("execpolicy: backtick command substitution rejected")
		case c == '$' && s.peek(1) == '\'':
			// ANSI-C quoting. Decoding it here — into the CURRENT WORD, past
			// the scanner — is what makes this reader see the same word the
			// shell does. Measured before it did: `echo ssh-rsa AAAA >
			// ~/$'\x2e\x73\x73\x68'/authorized_keys` produced the literal
			// target `~/$\x2e\x73\x73\x68/authorized_keys`, the credential
			// denylist matches on the `~/.ssh` directory prefix and so did not
			// fire, and the key landed on disk with no prompt. Hiding the
			// FILENAME instead was already caught, because the prefix survived.
			//
			// It cannot widen the split: the decoded bytes join the word
			// directly and are never re-scanned, so $'\x26\x26' stays one word
			// rather than becoming an AND-IF. The same invariant guard's
			// lexShellLite relies on, now enforced in both readers by the same
			// decoder (execpolicy/ansic.go).
			lit, next, ok := DecodeANSICSpan(s.raw, s.i+2)
			if !ok {
				return fmt.Errorf("execpolicy: unterminated ANSI-C quote")
			}
			s.startToken()
			s.word.WriteString(lit)
			s.inWord = true
			s.i = next
		case c == '$' && s.peek(1) == '(':
			return fmt.Errorf("execpolicy: command substitution $(…) rejected")
		case c == '(' || c == ')':
			return fmt.Errorf("execpolicy: subshell grouping rejected")
		case c == ';':
			if err := s.flushSegment(Semi); err != nil {
				return err
			}
			s.i++
		case c == '|':
			op := Pipe
			width := 1
			if s.peek(1) == '|' {
				op, width = OrIf, 2
			}
			if err := s.flushSegment(op); err != nil {
				return err
			}
			s.i += width
		case c == '&':
			if err := s.scanAmpersand(); err != nil {
				return err
			}
		case c == '>' || c == '<':
			if err := s.scanRedirect(); err != nil {
				return err
			}
		default:
			s.startToken()
			s.word.WriteByte(c)
			s.inWord = true
			s.i++
		}
	}
	return s.finish()
}

// peek returns the byte n positions ahead, or 0 past the end.
func (s *listScanner) peek(n int) byte {
	if s.i+n >= len(s.raw) {
		return 0
	}
	return s.raw[s.i+n]
}

// startToken records where the current segment begins. Called before the first
// byte of every token so Segment.Text can be sliced out of raw verbatim.
func (s *listScanner) startToken() {
	if s.segFrom < 0 {
		s.segFrom = s.i
	}
}

// scanQuoted consumes a single- or double-quoted run, appending its literal
// bytes to the in-flight word. Inside double quotes `\x` yields `x`, matching
// Lex; inside single quotes nothing is escaped. Control operators inside quotes
// are data, which is the whole reason quotes have to be tracked here at all.
func (s *listScanner) scanQuoted(quote byte) error {
	s.startToken()
	s.inWord = true
	s.i++ // opening quote
	for s.i < len(s.raw) {
		c := s.raw[s.i]
		if c == quote {
			s.i++
			return nil
		}
		if c == '\\' && quote == '"' {
			if s.i+1 >= len(s.raw) {
				return fmt.Errorf("execpolicy: trailing escape")
			}
			s.word.WriteByte(s.raw[s.i+1])
			s.i += 2
			continue
		}
		s.word.WriteByte(c)
		s.i++
	}
	return fmt.Errorf("execpolicy: unterminated quote")
}

// scanAmpersand disambiguates the three things a bare `&` can start: the AND-IF
// operator, the merge-both-streams redirection `&>`, and job control. Only the
// last is refused — a detached process outlives the turn that authorized it, so
// there is nothing left to report a denial to.
func (s *listScanner) scanAmpersand() error {
	if s.peek(1) == '&' {
		if err := s.flushSegment(AndIf); err != nil {
			return err
		}
		s.i += 2
		return nil
	}
	if s.peek(1) == '>' {
		s.startToken()
		op := "&>"
		s.i += 2
		if s.peek(0) == '>' {
			op += ">"
			s.i++
		}
		s.pushRedirect(op)
		return nil
	}
	return fmt.Errorf("execpolicy: background job control (&) rejected")
}

// scanRedirect consumes a redirection operator and, unless the operator folds a
// fd reference into itself (`2>&1`), marks the next word as its target.
//
// A leading all-digit word is the fd, exactly as in Lex: `2>err.log` redirects
// stderr, it does not run a program called "2".
func (s *listScanner) scanRedirect() error {
	c := s.raw[s.i]
	fd := ""
	if s.inWord && isDigits(s.word.String()) {
		fd = s.word.String()
		s.word.Reset()
		s.inWord = false
	} else {
		s.flushWord()
	}
	s.startToken()
	op := string(c)
	s.i++
	if s.peek(0) == c {
		// `<<` is a here-document and `>>` is append. The here-document's
		// payload is on lines this string does not contain, so it cannot be
		// judged; append is an ordinary write.
		if c == '<' {
			return fmt.Errorf("execpolicy: here-document (<<) rejected")
		}
		op += string(c)
		s.i++
	}
	if s.peek(0) == '(' {
		return fmt.Errorf("execpolicy: process substitution rejected")
	}
	if s.peek(0) == '&' {
		// `>&word` is a descriptor duplication ONLY when word is a plain fd
		// number (`2>&1`, `>&2`) or the close token (`>&-`). Measured on bash,
		// sh and zsh: every other spelling WRITES THE FILE named word — `echo x
		// >& ./t` leaves "x" in ./t on all three. Reading it as a duplication
		// left RedirectSpec.Target empty, checkRedirectTargets skips empty
		// targets, and `echo ssh-rsa … >& ~/.ssh/authorized_keys` was measured
		// planting a key on disk with no prompt while the `>` spelling of the
		// same command was refused by the credential denylist.
		//
		// The digit run is looked at before it is consumed, because `>&1x` is a
		// file called "1x" and the digits have to stay available as the first
		// bytes of the target word.
		digits := s.i + 1
		for digits < len(s.raw) && s.raw[digits] >= '0' && s.raw[digits] <= '9' {
			digits++
		}
		if digits > s.i+1 && isRedirectWordBoundary(byteAtOrZero(s.raw, digits)) {
			s.redirs = append(s.redirs, RedirectSpec{Operator: fd + op + "&" + s.raw[s.i+1:digits]})
			s.i = digits
			return nil
		}
		if byteAtOrZero(s.raw, s.i+1) == '-' {
			s.redirs = append(s.redirs, RedirectSpec{Operator: fd + op + "&-"})
			s.i += 2
			return nil
		}
		s.i++
		s.pushRedirect(fd + op + "&")
		return nil
	}
	s.pushRedirect(fd + op)
	return nil
}

// isRedirectWordBoundary reports whether b ends a shell word. Zero stands for
// end-of-string. It is what tells `2>&1` (a descriptor number, then nothing)
// from `>&1x` (a file called "1x").
func isRedirectWordBoundary(b byte) bool {
	switch b {
	case 0, ' ', '\t', '\n', '\r', ';', '|', '&', '<', '>', '`', '(', ')':
		return true
	}
	return false
}

func byteAtOrZero(s string, i int) byte {
	if i >= len(s) {
		return 0
	}
	return s[i]
}

// pushRedirect records an operator whose target is the next word.
func (s *listScanner) pushRedirect(op string) {
	s.redirs = append(s.redirs, RedirectSpec{Operator: op})
	s.pendingRedirect = len(s.redirs) - 1
}

// flushWord ends the in-flight word, routing it either to a redirection
// awaiting its target or to the segment's word list.
func (s *listScanner) flushWord() {
	if !s.inWord {
		return
	}
	w := s.word.String()
	s.word.Reset()
	s.inWord = false
	if s.pendingRedirect >= 0 {
		s.redirs[s.pendingRedirect].Target = w
		s.pendingRedirect = -1
		return
	}
	s.words = append(s.words, w)
}

// flushSegment closes the current segment, tags it with the operator that
// separates it from the next one, and resets the accumulators.
//
// An operator with no segment in front of it is a parse error rather than an
// empty segment: `; ls` and `a ;; b` are shell syntax errors, and inventing an
// empty command for them would let a malformed string reach the policy layer as
// something the policy layer thinks it understood.
func (s *listScanner) flushSegment(op TokenKind) error {
	s.flushWord()
	if s.pendingRedirect >= 0 {
		return fmt.Errorf("execpolicy: redirect %q missing target", s.redirs[s.pendingRedirect].Operator)
	}
	if len(s.words) == 0 {
		return fmt.Errorf("execpolicy: operator without executable segment")
	}
	text := strings.TrimSpace(s.raw[s.segFrom:s.i])
	seg := Segment{
		Program:   normalizeProgram(s.words[0]),
		Redirects: s.redirs,
		Operator:  op,
		Text:      text,
	}
	if len(s.words) > 1 {
		seg.Args = append([]string(nil), s.words[1:]...)
	}
	s.segs = append(s.segs, seg)
	s.words = nil
	s.redirs = nil
	s.segFrom = -1
	return nil
}

// finish closes the last segment. A trailing separator (`ls;`) is legal and
// leaves nothing to flush; anything else that leaves words or a dangling
// redirect behind is not.
func (s *listScanner) finish() error {
	s.flushWord()
	if s.pendingRedirect >= 0 {
		return fmt.Errorf("execpolicy: redirect %q missing target", s.redirs[s.pendingRedirect].Operator)
	}
	if len(s.words) == 0 {
		if len(s.redirs) > 0 {
			return fmt.Errorf("execpolicy: redirect before executable")
		}
		return nil
	}
	return s.flushSegment(NoOperator)
}
