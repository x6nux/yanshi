package execpolicy

import "strings"

// ansic.go is the ANSI-C quoting decoder, shared by the two readers this
// repository points at a shell command.
//
// It used to live in internal/guard, private to the destructive-deletion lexer.
// That is how the two readers came to have DIFFERENT blind spots: guard's
// lexShellLite decoded $'\x2e\x73\x73\x68' and ParseCommandList did not, so
//
//	echo ssh-rsa AAAA > ~/$'\x2e\x73\x73\x68'/authorized_keys
//
// produced the literal redirect target `~/$\x2e\x73\x73\x68/authorized_keys`,
// which the credential denylist matches by the `~/.ssh` directory prefix and
// therefore did not match at all. Measured: verdict Allow, and both /bin/bash
// and /bin/sh planted the key in ~/.ssh/authorized_keys (with HOME pointed at a
// temporary directory). The plain spelling of the same command prompts.
//
// The decoder is the WORD layer of a shell reading: what a token's bytes are
// once quoting has been resolved. Both readers need exactly that and nothing
// more, which is why this is the piece that is shared rather than the parsers
// themselves — those answer different questions (see ADR-0016).
//
// Decoding a $'...' span is always safe for a caller that appends the result to
// the CURRENT TOKEN, because $'...' is a quoting construct: its content never
// word-splits and never becomes a control operator. A caller that instead
// decoded the raw string before scanning it would let $'\x26\x26'
// manufacture a chain, which is why neither reader does that.

// DecodeANSIC expands every $'...' span in a command into the literal bytes
// bash would produce, leaving the rest of the string untouched. Escapes that
// bash does not recognize are passed through verbatim (bash keeps the
// backslash), so decoding can only ever reveal characters, never invent them.
//
// The returned bool reports whether any span was decoded; callers use it to
// tell "nothing to see" from "the visible text is not what runs".
func DecodeANSIC(cmd string) (string, bool) {
	if !strings.Contains(cmd, "$'") {
		return cmd, false
	}
	var out strings.Builder
	decoded := false
	for i := 0; i < len(cmd); {
		if cmd[i] == '$' && i+1 < len(cmd) && cmd[i+1] == '\'' {
			lit, next, ok := DecodeANSICSpan(cmd, i+2)
			if ok {
				out.WriteString(lit)
				i = next
				decoded = true
				continue
			}
		}
		out.WriteByte(cmd[i])
		i++
	}
	return out.String(), decoded
}

// DecodeANSICSpan decodes one $'...' body starting at index start (just past
// the opening quote) and returns the literal text plus the index just past the
// closing quote. ok=false for an unterminated span, in which case the caller
// leaves the text alone rather than guessing where it ended — an unterminated
// quote is already ok=false from lexShellLite, so nothing is lost.
func DecodeANSICSpan(cmd string, start int) (lit string, next int, ok bool) {
	var b strings.Builder
	for i := start; i < len(cmd); {
		c := cmd[i]
		if c == '\'' {
			return b.String(), i + 1, true
		}
		if c != '\\' || i+1 >= len(cmd) {
			b.WriteByte(c)
			i++
			continue
		}
		text, consumed := decodeANSICEscape(cmd[i:])
		b.WriteString(text)
		i += consumed
	}
	return "", 0, false
}

// ansicSimpleEscapes are the single-character escapes bash's $'...' honors.
// \e and \E are the ESC byte; \c<x> (control char) is handled separately
// because it consumes a following character.
var ansicSimpleEscapes = map[byte]byte{
	'a': 0x07, 'b': 0x08, 'e': 0x1b, 'E': 0x1b, 'f': 0x0c,
	'n': '\n', 'r': '\r', 't': '\t', 'v': 0x0b,
	'\\': '\\', '\'': '\'', '"': '"', '?': '?',
}

// decodeANSICEscape decodes a single backslash escape at the head of s (s[0]
// is the backslash) and reports how many bytes it consumed. An escape bash
// does not recognize is returned verbatim INCLUDING its backslash, matching
// bash's own behavior — silently dropping the backslash would let this
// function manufacture a token that the shell would never produce.
func decodeANSICEscape(s string) (string, int) {
	if len(s) < 2 {
		return s, len(s)
	}
	c := s[1]
	if b, ok := ansicSimpleEscapes[c]; ok {
		return string([]byte{b}), 2
	}
	switch c {
	case 'x': // \xHH — one or two hex digits
		n := 0
		v := 0
		for n < 2 && 2+n < len(s) && isHexDigit(s[2+n]) {
			v = v*16 + hexValue(s[2+n])
			n++
		}
		if n == 0 {
			return s[:2], 2
		}
		return string([]byte{byte(v)}), 2 + n
	case 'u', 'U': // \uHHHH / \UHHHHHHHH — Unicode code point
		width := 4
		if c == 'U' {
			width = 8
		}
		n := 0
		v := 0
		for n < width && 2+n < len(s) && isHexDigit(s[2+n]) {
			v = v*16 + hexValue(s[2+n])
			n++
		}
		if n == 0 {
			return s[:2], 2
		}
		return string(rune(v)), 2 + n
	case 'c': // \cX — control character
		if len(s) < 3 {
			return s[:2], 2
		}
		return string([]byte{s[2] & 0x1f}), 3
	}
	if c >= '0' && c <= '7' { // \NNN — up to three octal digits
		n := 0
		v := 0
		for n < 3 && 1+n < len(s) && s[1+n] >= '0' && s[1+n] <= '7' {
			v = v*8 + int(s[1+n]-'0')
			n++
		}
		return string([]byte{byte(v)}), 1 + n
	}
	return s[:2], 2 // unrecognized: bash keeps the backslash
}

func isHexDigit(b byte) bool {
	return (b >= '0' && b <= '9') || (b >= 'a' && b <= 'f') || (b >= 'A' && b <= 'F')
}

func hexValue(b byte) int {
	switch {
	case b >= '0' && b <= '9':
		return int(b - '0')
	case b >= 'a' && b <= 'f':
		return int(b-'a') + 10
	default:
		return int(b-'A') + 10
	}
}
