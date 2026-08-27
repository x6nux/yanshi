// internal/ctxcompact/tokenest.go
package ctxcompact

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// TokenCountMode names how EstimateTokens arrived at a number. It is exported
// so a caller can put the mode in a log line next to the count: the two
// compaction gates are sized off these numbers, and an operator debugging "why
// did compaction fire at 40% of the window" needs to know whether they are
// reading a tokenizer's answer or a heuristic's.
//
// There is currently exactly one mode. It is a named type rather than a bool
// because the axis has more than two values the moment a real BPE tokenizer is
// wired in (exact-cl100k, exact-o200k, …), and a bool named IsExact would have
// to be replaced rather than extended.
type TokenCountMode string

// TokenCountHeuristic is the mode of the built-in structural estimator: no BPE
// vocabulary, no network, no model-specific state. See EstimateTokens for the
// measured error band and TokenCountingMode for why this is the only mode.
const TokenCountHeuristic TokenCountMode = "heuristic"

// TokenCountingMode reports how this build counts tokens.
//
// WHY THERE IS NO EXACT MODE. A true count needs the provider's BPE merge
// table. The Go options (tiktoken-go and friends) fetch that table over the
// network on first use, which makes the FIRST compaction of an air-gapped or
// proxied deployment fail — precisely the deployment that cannot afford a
// failed compaction — and the offline alternative is to vendor several
// megabytes of vocabulary per encoding. Neither buys anything the gates need:
// compaction thresholds are fractions of a window, so a bounded, ALWAYS-HIGH
// estimate is operationally equivalent to an exact count and strictly safer.
// See estimateTextTokens for the measurement that establishes the bound.
func TokenCountingMode() TokenCountMode { return TokenCountHeuristic }

// Token cost weights. Every one is a MEASURED value, not a guess; the
// measurement procedure and the resulting error band are recorded on
// estimateTextTokens. They are deliberately biased so the estimate lands ABOVE
// the true count on every sample in the corpus — see that function for why the
// bias is one-directional.
const (
	// wordCharsPerToken is the divisor for an ordinary identifier or word run
	// ([A-Za-z0-9_]+ that is not opaque). Measured densities on real BPE
	// vocabularies: an English word is ~1 token however long it is,
	// "estimateMessageTokens" (21 chars) is 3 tokens, "NewApprovalGuardedTool
	// FactoryBuilder" (36 chars) is 7. 3.5 sits below all of those, so a word
	// run is charged more tokens than it costs.
	wordCharsPerToken = 3.5

	// opaqueCharsPerToken is the divisor for an opaque run — a mixed
	// letters+digits identifier at least opaqueRunMinLen long (a UUID field, a
	// git SHA, a go.sum hash, a base64 chunk). Measured over the whole
	// population of such runs in go.sum, CLAUDE.md and UUID/base64 fixtures:
	// 1.48-1.58 characters per token, an order of magnitude denser than prose.
	// Charging them at the word rate is how the old chars/4 estimate
	// undercounted go.sum-shaped and UUID-heavy tool output by 2-4x.
	opaqueCharsPerToken = 1.2

	// digitCharsPerToken is the divisor for an all-digit run. Every BPE in use
	// caps its numeric vocabulary at three digits, so a long number is chopped
	// into fixed-size groups: measured 2.0-3.0 characters per token
	// ("4096" = 2, "12345678" = 3, "1754630400" = 4).
	//
	// Digits get their own rate because they are BETWEEN the two neighbours
	// and would be misfiled by either. At the word rate a timestamp is
	// undercounted; at the opaque rate it is charged like a hash, which is 2x
	// too much — and an all-digit run passes the hex test trivially, so
	// without this branch every timestamp, port, byte count and line number in
	// a tool result would be priced as a SHA. That was the first thing the
	// estimator got wrong.
	digitCharsPerToken = 2.0

	// nonWordCharsPerToken is the divisor for a run of ASCII punctuation and
	// whitespace. Measured: runs of 1-2 characters are 1 token, and longer
	// runs (JSON structure, indentation) approach 2 characters per token.
	nonWordCharsPerToken = 2.0

	// cjkTokensPerRune is the per-rune cost of a CJK ideograph, kana or
	// hangul syllable. Measured on both cl100k and o200k: Chinese runs
	// 1.0-1.3 tokens per character and Japanese/Korean are denser still. This
	// single constant is the whole reason the old estimate was off by up to
	// 4x on this repo's own Chinese documentation — chars/4 charges a CJK
	// paragraph a QUARTER of what it costs.
	cjkTokensPerRune = 1.1

	// otherBytesPerToken is the divisor for any other non-ASCII rune (emoji,
	// accented Latin, symbols), charged by UTF-8 BYTE because byte-level BPE
	// splits exactly these into their bytes. Measured ~2.5 bytes per token
	// across the emoji/accented sample.
	otherBytesPerToken = 2.5

	// opaqueRunMinLen is the length at which a mixed letters+digits run is
	// treated as opaque rather than as a word.
	//
	// It is 8 rather than something shorter because short mixed runs are
	// overwhelmingly ordinary identifiers ("sha256", "utf8", "v1", "arg2"),
	// which tokenize at word density; classifying those as opaque inflates
	// every Go file's estimate for no safety gain. At 8 and above the
	// population flips to real opaque values: short SHA prefixes, UUID
	// fields, base64 chunks, log ids.
	opaqueRunMinLen = 8
)

// perMessageOverhead approximates the role/structural framing every chat
// message carries on the wire (role token, separators, name field).
const perMessageOverhead = 8

// perToolCallOverhead approximates the structural framing of one tool call on
// top of its name, arguments and id text.
const perToolCallOverhead = 16

// estimateTextTokens returns an upper-biased token estimate for s.
//
// HOW IT WORKS. The text is walked once and split into kinds of run, each
// charged at its own measured density:
//
//	word run      [A-Za-z0-9_]+, ordinary            len/wordCharsPerToken
//	digit run     all digits                         len/digitCharsPerToken
//	opaque run    letters+digits, >= 8               len/opaqueCharsPerToken
//	non-word run  ASCII punctuation and whitespace   len/nonWordCharsPerToken
//	CJK rune      ideograph / kana / hangul          cjkTokensPerRune each
//	other rune    emoji, accented Latin, symbols     bytes/otherBytesPerToken
//
// Every run costs AT LEAST one token, which is the single most important rule
// in the function: byte-level BPE never emits a fractional token, and the old
// chars/4 form charged a 3-character run 0.75 of one.
//
// WHY THE BIAS IS DELIBERATE AND ONE-DIRECTIONAL. These numbers gate WHEN to
// compact and where to cut chunks. Underestimating means the gate opens too
// late and the provider answers 400 — the exact failure compaction exists to
// prevent, and one that costs a full round trip and a dead turn.
// Overestimating means one extra compaction: a wasted model call, recoverable,
// invisible to the user. So the weights are chosen to keep the estimate at or
// above the true count rather than to minimize average error.
//
// MEASURED ERROR BAND. Method: every .go file under internal/, every .md under
// docs/, plus CLAUDE.md, config.example.yaml, go.mod, go.sum, and synthetic
// worst cases (pure Chinese / Japanese / Korean, emoji, base64, UUID runs,
// English prose), each encoded with BOTH cl100k_base and o200k_base — 1169
// documents, 2338 measurements. Reported as estimate/actual:
//
//	                min     p05     median  max     samples under 1.0
//	chars/4 (old)   0.250   0.481   0.918   1.320   1983 of 2338
//	this estimator  1.004   1.237   1.521   2.157   0 of 2338
//
// Read that table as: the old estimate undercounted on 85% of the corpus and
// by up to 4x on Chinese text, which is how a 128K window could be blown
// through while the gate believed it was at 32K. This one never undercounts on
// the corpus and pays for it with a median 1.5x overcount, which spends one
// early compaction. The worst overcount (2.16x) is a pure-hex fixture; real
// documents top out near 1.8x.
//
// The numbers are NOT re-derivable by reading this function — they come from
// running it against a real tokenizer. TestEstimatorNeverUndercountsCorpus
// pins the direction (never below 1.0) on fixtures distilled from that corpus,
// so a weight edited to a friendlier-looking value reddens rather than
// silently reopening the 400.
func estimateTextTokens(s string) int {
	if s == "" {
		return 0
	}
	total := 0.0
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c < utf8.RuneSelf && isWordByte(c):
			j := i
			for j < len(s) && s[j] < utf8.RuneSelf && isWordByte(s[j]) {
				j++
			}
			run := s[i:j]
			total += atLeastOne(float64(len(run)) / wordRunDivisor(run))
			i = j
		case c < utf8.RuneSelf:
			j := i
			for j < len(s) && s[j] < utf8.RuneSelf && !isWordByte(s[j]) {
				j++
			}
			total += atLeastOne(float64(j-i) / nonWordCharsPerToken)
			i = j
		default:
			r, size := utf8.DecodeRuneInString(s[i:])
			if isCJK(r) {
				total += cjkTokensPerRune
			} else {
				total += float64(size) / otherBytesPerToken
			}
			i += size
		}
	}
	// Round UP: a fractional tail is still text the provider will tokenize,
	// and truncation is the one rounding direction that can undercount.
	n := int(total)
	if total > float64(n) {
		n++
	}
	return n
}

// atLeastOne floors a run's cost at one token. No byte-level BPE emits less
// than one token for a non-empty run, so any value below 1 is an undercount by
// construction rather than by calibration.
func atLeastOne(v float64) float64 {
	if v < 1 {
		return 1
	}
	return v
}

// isWordByte reports whether an ASCII byte belongs to an identifier/word run.
// '_' is included because it is glued into identifiers by every tokenizer's
// pre-tokenization pattern and by every language in this repo's tool output.
func isWordByte(c byte) bool {
	return c == '_' ||
		(c >= '0' && c <= '9') ||
		(c >= 'a' && c <= 'z') ||
		(c >= 'A' && c <= 'Z')
}

// wordRunDivisor picks the characters-per-token rate for one [A-Za-z0-9_] run.
//
// Three rates, in increasing density:
//
//   - WORD (wordCharsPerToken) — ordinary identifiers and words, the default.
//   - DIGITS (digitCharsPerToken) — an all-digit run. Checked BEFORE opaque:
//     an all-digit run is trivially all-hex too, so the opaque test alone
//     would price every timestamp and byte count like a SHA.
//   - OPAQUE (opaqueCharsPerToken) — hex blobs and letters+digits ids at
//     opaqueRunMinLen or longer. See isOpaqueRun.
func wordRunDivisor(run string) float64 {
	if isAllDigits(run) {
		return digitCharsPerToken
	}
	if isOpaqueRun(run) {
		return opaqueCharsPerToken
	}
	return wordCharsPerToken
}

// isAllDigits reports whether run consists only of ASCII digits.
func isAllDigits(run string) bool {
	if run == "" {
		return false
	}
	for i := 0; i < len(run); i++ {
		if run[i] < '0' || run[i] > '9' {
			return false
		}
	}
	return true
}

// isOpaqueRun reports whether a word run is an opaque value — a SHA / UUID
// field / base64 chunk / log id — rather than a word or identifier.
//
// The test is: at least opaqueRunMinLen long, AND mixing letters with digits.
//
// BOTH HALVES OF THAT CONJUNCTION ARE LOAD-BEARING, and the letters-and-digits
// half was learned the hard way. An earlier form also accepted an ALL-HEX run,
// which is the same rule stated more permissively — and "all hex" is trivially
// true of a long run of the single letter 'a'. A file of repeated 'a's (a
// fixture shape, but also what a truncated binary or a padding blob looks like)
// was therefore priced at hash density, 6.75x its real cost, which pushed
// histories over the compaction gate that were nowhere near it. Requiring a
// digit alongside the letters keeps every real hash — they all carry digits —
// while excluding degenerate letter runs.
//
// Pure-letter runs never qualify however long, because a long lowerCamel
// identifier tokenizes at word density (measured 5-7 chars/token). Pure-digit
// runs are routed to digitCharsPerToken before this is consulted; see
// wordRunDivisor.
func isOpaqueRun(run string) bool {
	if len(run) < opaqueRunMinLen {
		return false
	}
	letter, digit := false, false
	for i := 0; i < len(run); i++ {
		switch c := run[i]; {
		case c >= '0' && c <= '9':
			digit = true
		case (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z'):
			letter = true
		}
	}
	return letter && digit
}

// isCJK reports whether r is a CJK ideograph, kana, hangul syllable, or a
// CJK-width form/punctuation. These are the ranges where a byte-level BPE
// spends roughly one token per CHARACTER instead of per word, and they are the
// single largest source of undercounting in a chars/4 estimate for this repo,
// whose own documentation and operator conversations are in Chinese.
func isCJK(r rune) bool {
	switch {
	case r >= 0x3000 && r <= 0x303F: // CJK symbols and punctuation
		return true
	case r >= 0x3040 && r <= 0x30FF: // hiragana + katakana
		return true
	case r >= 0x3400 && r <= 0x4DBF: // CJK ext A
		return true
	case r >= 0x4E00 && r <= 0x9FFF: // CJK unified ideographs
		return true
	case r >= 0xAC00 && r <= 0xD7AF: // hangul syllables
		return true
	case r >= 0xF900 && r <= 0xFAFF: // CJK compatibility ideographs
		return true
	case r >= 0xFF00 && r <= 0xFFEF: // halfwidth and fullwidth forms
		return true
	case r >= 0x20000 && r <= 0x2FA1F: // CJK ext B..F + compat supplement
		return true
	}
	return false
}

// countCJKRunes reports how many runes of s fall in the CJK ranges. It exists
// for the estimator's own tests and for callers that want to explain a large
// estimate ("this history is 60% Chinese") rather than just report it.
func countCJKRunes(s string) int {
	n := 0
	for _, r := range s {
		if isCJK(r) {
			n++
		}
	}
	return n
}

// approxWords counts whitespace-separated fields, used by the structured
// summary's length guidance. It lives here because it shares the estimator's
// "cheap structural measurement, no tokenizer" premise.
func approxWords(s string) int {
	return len(strings.FieldsFunc(s, unicode.IsSpace))
}
