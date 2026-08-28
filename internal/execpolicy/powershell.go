package execpolicy

import (
	"fmt"
	"strings"
)

// powershell.go is the second STRUCTURE reader, for commands that will be
// handed to powershell/pwsh instead of a POSIX shell.
//
// # Why a second reader and not a flag on the first one
//
// ADR-0016 draws the line: the WORD layer is shared, the STRUCTURE layer is
// not. PowerShell and sh disagree at the byte level about the one character
// that matters most on Windows —
//
//	sh:          `\` escapes, `` ` `` starts a command substitution
//	powershell:  `\` is a path separator, `` ` `` escapes
//
// — so running a PowerShell command through ParseCommandList reads the
// separators out of every path. Measured on the existing reader:
//
//	`Remove-Item -Recurse C:\temp`  ->  target `C:temp`
//	`echo x > $env:USERPROFILE\.ssh\authorized_keys`
//	                               ->  target `$env:USERPROFILE.ssh.authorized_keys`
//
// The second is the Windows spelling of the bypass the ANSI-C fix just closed:
// the guard's credential denylist matches on the `.ssh` path segment, and the
// segment was dissolved before it got there. A boolean on the existing scanner
// would mean two escape conventions inside one byte loop, which is two scanners
// wearing one name.
//
// # Same contract as ParseCommandList
//
// Lenient about WORD CONTENT (`$env:X`, `*`, `C:\…` are ordinary words the rule
// engine may not understand but the segmenter must not refuse), strict about
// STRUCTURE, and every construct that would make "where does this command end"
// or "where does this redirection point" a guess is a fail-closed error, which
// guard.checkShell turns into a structural HardDeny.
//
// # Refused forms
//
//   - subexpression `$(…)`, array subexpression `@(…)`, brace variable `${…}`
//     — the text that actually runs is not in this string;
//   - grouping `(…)` and script blocks `{…}` — the first word inside is not
//     the program, which is exactly how `(rm -rf /)` walked past a classifier
//     once already;
//   - here-strings `@"` / `@'` — the payload lives on lines this string does
//     not contain;
//   - `&` in either of its jobs (call operator, background) — `&&` is a
//     separator and a lone `&` is not readable enough to admit;
//   - `#` comments — everything after one is invisible to a reader that
//     stopped there;
//   - `<` — PowerShell reserves it and refuses to run it, so admitting it
//     would mean modelling a command that cannot exist;
//   - raw newline / carriage return — a second statement, not a segment;
//   - unterminated quote, and a trailing backtick (the string is truncated).
func ParsePowerShellCommandList(raw string) ([]Segment, error) {
	s := &psScanner{raw: raw, segFrom: -1, pendingRedirect: -1}
	if err := s.run(); err != nil {
		return nil, err
	}
	if len(s.segs) == 0 {
		return nil, fmt.Errorf("execpolicy: empty command")
	}
	return s.segs, nil
}

// psScanner is ParsePowerShellCommandList's byte-loop state. It mirrors
// listScanner's shape deliberately — same fields, same three flushes — so the
// two readers stay legible side by side; what differs is only the character
// rules, which is the whole point of there being two.
type psScanner struct {
	raw  string
	i    int
	segs []Segment

	word    strings.Builder
	inWord  bool
	words   []string
	redirs  []RedirectSpec
	segFrom int

	pendingRedirect int
}

func (s *psScanner) run() error {
	for s.i < len(s.raw) {
		c := s.raw[s.i]
		switch {
		case c == '`':
			// PowerShell's escape character. It makes the NEXT byte literal,
			// including a space, a quote or an operator. A trailing backtick is
			// a line continuation onto a line this string does not have.
			if s.i+1 >= len(s.raw) {
				return fmt.Errorf("execpolicy: trailing PowerShell escape (`)")
			}
			s.startToken()
			s.word.WriteByte(s.raw[s.i+1])
			s.inWord = true
			s.i += 2
		case c == '\'':
			if err := s.scanSingleQuoted(); err != nil {
				return err
			}
		case c == '"':
			if err := s.scanDoubleQuoted(); err != nil {
				return err
			}
		case c == ' ' || c == '\t':
			s.flushWord()
			s.i++
		case c == '\n' || c == '\r':
			return fmt.Errorf("execpolicy: newline is a second PowerShell statement, not a segment")
		case c == '#':
			return fmt.Errorf("execpolicy: PowerShell comment (#) rejected")
		case c == '$' && s.peek(1) == '(':
			return fmt.Errorf("execpolicy: PowerShell subexpression $(…) rejected")
		case c == '$' && s.peek(1) == '{':
			return fmt.Errorf("execpolicy: PowerShell brace variable ${…} rejected")
		case c == '@' && (s.peek(1) == '(' || s.peek(1) == '"' || s.peek(1) == '\''):
			return fmt.Errorf("execpolicy: PowerShell array subexpression / here-string rejected")
		case c == '(' || c == ')':
			return fmt.Errorf("execpolicy: PowerShell grouping rejected")
		case c == '{' || c == '}':
			return fmt.Errorf("execpolicy: PowerShell script block rejected")
		case c == '<':
			return fmt.Errorf("execpolicy: PowerShell does not support input redirection (<)")
		case c == ';':
			if err := s.flushSegment(); err != nil {
				return err
			}
			s.i++
		case c == '|':
			width := 1
			if s.peek(1) == '|' {
				width = 2
			}
			if err := s.flushSegment(); err != nil {
				return err
			}
			s.i += width
		case c == '&':
			if s.peek(1) != '&' {
				// The call operator (`& 'C:\x.exe'`) and background (`cmd &`)
				// share this byte. Neither is readable enough to admit: the
				// first makes the program word a runtime value, the second
				// detaches a child that outlives the turn authorizing it.
				return fmt.Errorf("execpolicy: PowerShell call operator / background (&) rejected")
			}
			if err := s.flushSegment(); err != nil {
				return err
			}
			s.i += 2
		case c == '>':
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

func (s *psScanner) peek(n int) byte {
	if s.i+n >= len(s.raw) {
		return 0
	}
	return s.raw[s.i+n]
}

func (s *psScanner) startToken() {
	if s.segFrom < 0 {
		s.segFrom = s.i
	}
}

// scanSingleQuoted consumes a '…' run. PowerShell escapes nothing inside single
// quotes except a doubled quote, which stands for one literal quote.
func (s *psScanner) scanSingleQuoted() error {
	s.startToken()
	s.inWord = true
	s.i++
	for s.i < len(s.raw) {
		if s.raw[s.i] == '\'' {
			if s.peek(1) == '\'' {
				s.word.WriteByte('\'')
				s.i += 2
				continue
			}
			s.i++
			return nil
		}
		s.word.WriteByte(s.raw[s.i])
		s.i++
	}
	return fmt.Errorf("execpolicy: unterminated quote")
}

// scanDoubleQuoted consumes a "…" run. Inside it a backtick escapes the next
// byte and a doubled quote stands for one literal quote. Interpolation is NOT
// resolved — `"$env:HOME/x"` stays as written, the same leniency about word
// content ParseCommandList applies to `$HOME` — but an interpolated
// SUBEXPRESSION is refused, because that is code rather than a value.
func (s *psScanner) scanDoubleQuoted() error {
	s.startToken()
	s.inWord = true
	s.i++
	for s.i < len(s.raw) {
		c := s.raw[s.i]
		switch {
		case c == '`':
			if s.i+1 >= len(s.raw) {
				return fmt.Errorf("execpolicy: trailing PowerShell escape (`)")
			}
			s.word.WriteByte(s.raw[s.i+1])
			s.i += 2
		case c == '$' && s.peek(1) == '(':
			return fmt.Errorf("execpolicy: PowerShell subexpression $(…) rejected")
		case c == '"':
			if s.peek(1) == '"' {
				s.word.WriteByte('"')
				s.i += 2
				continue
			}
			s.i++
			return nil
		default:
			s.word.WriteByte(c)
			s.i++
		}
	}
	return fmt.Errorf("execpolicy: unterminated quote")
}

// scanRedirect consumes a PowerShell redirection and marks the next word as its
// target unless the operator folds a stream reference into itself (`2>&1`).
//
// PowerShell's stream prefix is a single digit 1-6 or `*` (all streams), not an
// arbitrary fd number, and it must be attached: `Get-Thing 2> err.txt`. The
// prefix is taken off the in-flight word the same way the POSIX reader takes
// off an fd, so `Get-Thing2 > x` still runs `Get-Thing2`.
func (s *psScanner) scanRedirect() error {
	stream := ""
	if s.inWord {
		if w := s.word.String(); isPowerShellStream(w) {
			stream = w
			s.word.Reset()
			s.inWord = false
		} else {
			s.flushWord()
		}
	}
	s.startToken()
	op := ">"
	s.i++
	if s.peek(0) == '>' {
		op = ">>"
		s.i++
	}
	if s.peek(0) == '&' {
		// `2>&1` and `*>&1` merge one stream into another and name no file.
		// Anything else after `&` is not a spelling PowerShell accepts, so it
		// is refused rather than guessed at.
		n := s.i + 1
		if n < len(s.raw) && s.raw[n] >= '1' && s.raw[n] <= '6' &&
			isRedirectWordBoundary(byteAtOrZero(s.raw, n+1)) {
			s.redirs = append(s.redirs, RedirectSpec{Operator: stream + op + "&" + string(s.raw[n])})
			s.i = n + 1
			return nil
		}
		return fmt.Errorf("execpolicy: unreadable PowerShell stream merge (%s&)", stream+op)
	}
	s.redirs = append(s.redirs, RedirectSpec{Operator: stream + op})
	s.pendingRedirect = len(s.redirs) - 1
	return nil
}

// isPowerShellStream reports whether w is a redirection stream selector: a
// single digit 1-6, or "*" for all streams.
func isPowerShellStream(w string) bool {
	if w == "*" {
		return true
	}
	return len(w) == 1 && w[0] >= '1' && w[0] <= '6'
}

func (s *psScanner) flushWord() {
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

func (s *psScanner) flushSegment() error {
	s.flushWord()
	if s.pendingRedirect >= 0 {
		return fmt.Errorf("execpolicy: redirect %q missing target", s.redirs[s.pendingRedirect].Operator)
	}
	if len(s.words) == 0 {
		return fmt.Errorf("execpolicy: operator without executable segment")
	}
	seg := Segment{
		Program:   normalizeProgram(s.words[0]),
		Redirects: s.redirs,
		Text:      strings.TrimSpace(s.raw[s.segFrom:s.i]),
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

func (s *psScanner) finish() error {
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
	return s.flushSegment()
}
