// Package execpolicy parses a deliberately small shell-command subset for
// policy evaluation. It is NOT a shell: unsupported expansion/glob syntax is
// rejected fail-closed before any process is created.
//
// The lexer is byte-index based (not range-index based) because multi-byte
// operators like "&&", "||", ">>", "2>>", and "&>N" must be consumed exactly
// once in a single pass — a UTF-8 range loop would split some of these when
// non-ASCII characters appear earlier in the input (a regression source for
// the original implementation that confused ">" with ">>").
package execpolicy

import (
	"fmt"
	"strings"
	"unicode"
)

// TokenKind classifies a lexed token.
type TokenKind uint8

const (
	// Word is a literal argument / command word (post-quote-flattening).
	Word TokenKind = iota
	// Pipe is "|" (single pipe, not "||").
	Pipe
	// AndIf is "&&" — shell's conditional-and operator.
	AndIf
	// OrIf is "||" — shell's conditional-or operator.
	OrIf
	// Redirect is any redirection operator: ">", ">>", "<", "<<", "2>", "2>>",
	// "&>N", etc. The Text field carries the full literal form (with optional
	// leading fd digit and optional &-and-digit suffix) so the parser can
	// report it verbatim.
	Redirect
	// Semi is ";" — the sequencing operator. Lex never emits it: a ";" inside
	// the single command Lex describes is a syntax error, and passing it
	// through as an ordinary word character is what let `a;b` reach the rule
	// engine as a program literally named "a;b". Only ParseCommandList, which
	// segments a whole command LIST, produces it (as Segment.Operator).
	Semi
)

// NoOperator is Segment.Operator's zero value: it marks the LAST segment of a
// command list, the one no control operator follows. It is spelled as a named
// constant rather than left as a bare Word because "Operator == Word" reads
// like a claim about a token kind when it is really the absence of one.
const NoOperator = Word

// Token is a single lexeme produced by Lex.
type Token struct {
	Kind TokenKind
	Text string
}

// Lex tokenizes raw into a Token slice. It rejects:
//   - Unquoted shell expansions ($...), backticks, command substitution
//     $(...), glob metachars (* ? [), and Windows %VAR% syntax — the parser
//     layer does not honor these and silently accepting them would let a
//     model smuggle arbitrary text past a glob policy;
//   - Standalone "&" (job control) — only the "&&" form is recognized;
//   - Unterminated quotes and trailing backslashes.
//
// Quoted words (single or double) contribute their literal bytes to the
// in-flight Word token; the quote characters themselves are not preserved.
// Inside double quotes, "\x" becomes "x"; inside single quotes, no escape
// processing happens.
func Lex(raw string) ([]Token, error) {
	var tokens []Token
	var word strings.Builder
	quote := byte(0)
	flushWord := func() {
		if word.Len() != 0 {
			tokens = append(tokens, Token{Kind: Word, Text: word.String()})
			word.Reset()
		}
	}

	for i := 0; i < len(raw); {
		c := raw[i]
		if quote != 0 {
			if c == quote {
				quote = 0
				i++
				continue
			}
			if c == '\\' && quote == '"' {
				if i+1 >= len(raw) {
					return nil, fmt.Errorf("execpolicy: trailing escape")
				}
				word.WriteByte(raw[i+1])
				i += 2
				continue
			}
			if forbiddenExpansionAt(raw, i) {
				return nil, fmt.Errorf("execpolicy: expansion/glob syntax rejected at byte %d", i)
			}
			word.WriteByte(c)
			i++
			continue
		}

		if unicode.IsSpace(rune(c)) {
			flushWord()
			i++
			continue
		}
		switch c {
		case '\'', '"':
			quote = c
			i++
			continue
		case '\\':
			if i+1 >= len(raw) {
				return nil, fmt.Errorf("execpolicy: trailing escape")
			}
			word.WriteByte(raw[i+1])
			i += 2
			continue
		case '&':
			flushWord()
			if i+1 < len(raw) && raw[i+1] == '&' {
				tokens = append(tokens, Token{Kind: AndIf, Text: "&&"})
				i += 2
				continue
			}
			return nil, fmt.Errorf("execpolicy: standalone & rejected")
		case '|':
			flushWord()
			if i+1 < len(raw) && raw[i+1] == '|' {
				tokens = append(tokens, Token{Kind: OrIf, Text: "||"})
				i += 2
				continue
			}
			tokens = append(tokens, Token{Kind: Pipe, Text: "|"})
			i++
			continue
		case '>', '<':
			fd := ""
			if isDigits(word.String()) {
				fd = word.String()
				word.Reset()
			} else {
				flushWord()
			}
			op := string(c)
			i++
			if i < len(raw) && raw[i] == c {
				op += string(c)
				i++
			}
			if i < len(raw) && raw[i] == '&' {
				op += "&"
				i++
				start := i
				for i < len(raw) && raw[i] >= '0' && raw[i] <= '9' {
					i++
				}
				op += raw[start:i]
			}
			tokens = append(tokens, Token{Kind: Redirect, Text: fd + op})
			continue
		}
		if forbiddenExpansionAt(raw, i) {
			return nil, fmt.Errorf("execpolicy: expansion/glob syntax rejected at byte %d", i)
		}
		word.WriteByte(c)
		i++
	}
	if quote != 0 {
		return nil, fmt.Errorf("execpolicy: unterminated quote")
	}
	flushWord()
	if len(tokens) == 0 {
		return nil, fmt.Errorf("execpolicy: empty command")
	}
	return tokens, nil
}

// forbiddenExpansionAt reports whether raw[i] begins an expansion/glob construct
// the lexer refuses to pass through to the parser. Rationale per construct:
//   - '$'/'`' — POSIX shell expansion (variable, command substitution, etc.).
//     The parser would treat "$VAR" as a literal word and a glob policy
//     could not match the expanded value, so the model could smuggle text.
//   - '*'/'?'/'[' — glob metacharacters. Same reasoning: the policy layer
//     would have to honor or reject them; rejecting at lex time keeps the
//     parser simple and the failure mode loud.
//   - '%' with a later '%' — Windows %VAR% expansion. Requires at least two
//     '%' on the remaining input so a single literal "%" is still allowed.
func forbiddenExpansionAt(raw string, i int) bool {
	if i >= len(raw) {
		return false
	}
	switch raw[i] {
	case '$', '`', '*', '?', '[':
		return true
	case '%':
		return strings.IndexByte(raw[i+1:], '%') >= 0
	}
	return false
}

// isDigits reports whether s is a non-empty all-digits string — used to detect
// a leading fd ("2>>") so the lexer attaches it to the redirection operator
// rather than emitting it as a separate Word token.
func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}
