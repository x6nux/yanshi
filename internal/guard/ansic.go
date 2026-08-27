package guard

import (
	"strings"
)

// ansic.go teaches the destructive-deletion lexer to SEE THROUGH two encodings
// that hide a command's real shape from token-level inspection:
//
//  1. ANSI-C quoting — $'\x72\x6d -rf /' is, to bash, exactly `rm -rf /`. The
//     guard's metacharacter table catches "$(" but has no entry for "$'", and
//     lexShellLite treats the whole thing as one opaque argument. The
//     destructive gate therefore saw a program named "bash" with one harmless
//     string operand and returned DestructionNone.
//  2. Shell wrappers — `bash -c "rm -rf /"` puts the real command inside a
//     single quoted argument, with the same result.
//
// SEEING THROUGH beats REFUSING. The obvious alternative is to add "$'" to
// checkShell's metacharacter table, but that table is a STRUCTURAL HardDeny:
// unappealable in every mode including yolo, with no interactive escape. Every
// legitimate `printf $'\n'` and `grep $'\t'` would become permanently
// unrunnable, and the payoff would be zero — the attacker just switches to
// `bash -c "$(printf ...)"`, or to plain `bash -c 'rm -rf /'`, which contains
// no ANSI-C quoting at all and was never blocked. Decoding, by contrast,
// routes the DECODED command through the destructive classifier that already
// knows what `rm -rf /` means, so the wrapper stops being a hiding place while
// staying a usable feature. QwenPaw reaches the same conclusion from the other
// side: its shell_evasion_guardian flags $'...' as a FINDING for review rather
// than a refusal, and its catastrophic-path regexes are explicitly written so
// that `bash -c "rm -rf /mnt/c"` still matches through the wrapper.

// decodeANSIC expands every $'...' span in a command into the literal bytes
// bash would produce, leaving the rest of the string untouched. Escapes that
// bash does not recognize are passed through verbatim (bash keeps the
// backslash), so decoding can only ever reveal characters, never invent them.
//
// The returned bool reports whether any span was decoded; callers use it to
// tell "nothing to see" from "the visible text is not what runs".
func decodeANSIC(cmd string) (string, bool) {
	if !strings.Contains(cmd, "$'") {
		return cmd, false
	}
	var out strings.Builder
	decoded := false
	for i := 0; i < len(cmd); {
		if cmd[i] == '$' && i+1 < len(cmd) && cmd[i+1] == '\'' {
			lit, next, ok := decodeANSICSpan(cmd, i+2)
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

// decodeANSICSpan decodes one $'...' body starting at index start (just past
// the opening quote) and returns the literal text plus the index just past the
// closing quote. ok=false for an unterminated span, in which case the caller
// leaves the text alone rather than guessing where it ended — an unterminated
// quote is already ok=false from lexShellLite, so nothing is lost.
func decodeANSICSpan(cmd string, start int) (lit string, next int, ok bool) {
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

// shellWrappers are the programs whose "-c <string>" argument is another whole
// command. Normalized names (lowercased, base-named, .exe stripped) because
// lexShellLite hands the program word through normalizeProgramWord.
var shellWrappers = map[string]bool{
	"sh": true, "bash": true, "zsh": true, "dash": true, "ksh": true,
	"ash": true, "busybox": true, "env": true,
}

// unwrapShellCommand extracts the inner command from a shell-wrapper
// invocation — `bash -c "…"`, `sh -lc "…"`, `zsh -o pipefail -c "…"`.
//
// It returns ok=false when the command is not a wrapper invocation, so the
// caller keeps its original classification. The recursion in the caller is
// bounded (maxUnwrapDepth) because `bash -c "bash -c \"bash -c …\""` is a legal
// construction and an unbounded loop over attacker-controlled nesting is a
// denial-of-service on the authorization path.
//
// The flag scan accepts any short-flag cluster ENDING in 'c' (-c, -lc, -ec)
// because that is where bash itself looks; the command string is the argument
// immediately after. `env` is included in shellWrappers for `env FOO=1 bash -c
// …`, and the leading VAR=value assignments are skipped so the wrapper behind
// them is still found.
func unwrapShellCommand(program string, args []string) (string, bool) {
	if !shellWrappers[program] {
		return "", false
	}
	i := 0
	if program == "env" {
		// Skip `env`'s own options and VAR=value assignments to reach the real
		// program word, then require THAT to be a wrapper. Without this, `env`
		// alone would be treated as a wrapper and `env -i` would misparse.
		for i < len(args) && (strings.Contains(args[i], "=") || strings.HasPrefix(args[i], "-")) {
			if args[i] == "-u" || args[i] == "--unset" {
				i++ // consumes a variable name
			}
			i++
		}
		if i >= len(args) {
			return "", false
		}
		return unwrapShellCommand(normalizeProgramWord(args[i]), args[i+1:])
	}
	for ; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") || a == "-" {
			return "", false // first non-flag word: this is a script path, not -c
		}
		if strings.HasPrefix(a, "--") {
			continue
		}
		if strings.HasSuffix(a, "c") && len(a) >= 2 {
			if i+1 < len(args) {
				return args[i+1], true
			}
			return "", false
		}
	}
	return "", false
}

// maxUnwrapDepth bounds shell-wrapper recursion. Three levels covers every
// wrapper nesting seen in practice (`sh -c "bash -c '…'"` is already contrived)
// while keeping the authorization path's work bounded regardless of what the
// model emits.
const maxUnwrapDepth = 3
