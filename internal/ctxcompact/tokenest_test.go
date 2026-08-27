// internal/ctxcompact/tokenest_test.go
package ctxcompact

import (
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"
)

// corpusSample is one measured fixture: text, plus the token count a REAL BPE
// tokenizer produced for it.
//
// The `actual` numbers are not derivable from this repo — they were produced
// by encoding each string with tiktoken's cl100k_base and o200k_base and
// recording the LARGER count (the denser of the two encodings, so the safety
// assertion below is the strictest of the pair). They are pinned here because
// vendoring a tokenizer to recompute them is exactly the dependency C8 decided
// not to take, and an estimator with no external ground truth is a function
// asserting that it equals itself.
type corpusSample struct {
	name   string
	text   string
	actual int
}

// corpusSamples are the fixtures distilled from the C8 calibration corpus,
// chosen to span the shapes that make chars/4 wrong in each direction.
//
// The Chinese, Japanese and Korean entries are the reason C8 exists at all:
// chars/4 charges them a quarter of what they cost, so a history that is
// mostly CJK blows the window while the gate reads a third of it. The
// opaque entries (UUID, SHA, base64, go.sum line) are the second failure mode
// — dense values that a ReAct loop's tool output is full of. The prose, code
// and JSON entries are the common case, and are here so a weight tuned to fix
// the CJK case cannot quietly wreck them.
var corpusSamples = []corpusSample{
	{"english_prose", "The quick brown fox jumps over the lazy dog and then reads a file from disk. ", 18},
	{"go_code", "func estimateMessageTokens(m *schema.Message) int {\n\tif m == nil {\n\t\treturn 0\n\t}\n", 23},
	{"json_args", `{"command":"grep -rn 'x' internal","pattern":"^func .*\\(ctx"}`, 20},
	{"chinese", "这是一个用于测试中文分词密度的句子，包含标点符号以及数字123。", 29},
	{"japanese", "これはトークン密度のテストです。ファイルを読み込みました。", 25},
	{"korean", "이것은 토큰 밀도 테스트입니다. 파일을 읽었습니다.", 24},
	{"uuid", "9f1c2b7e-4a3d-11ee-be56-0242ac120002", 24},
	{"git_sha", "e359584a1b2c3d4e5f60718293a4b5c6d7e8f900", 29},
	{"base64", "Xl7Ap9dKaEs5kLoOQeQmPWevfnk/DM5qcLcYlA8ys6Y=", 35},
	{"gosum_line", "modernc.org/token v1.1.0 h1:Xl7Ap9dKaEs5kLoOQeQmPWevfnk/DM5qcLcYlA8ys6Y=", 47},
	{"file_path", "internal/ctxcompact/summarize.go", 9},
	{"emoji", "🎉✅🚀 done — «quoted» ünïcödé", 20},
	{"timestamps", "1754630400 1754630401 1754630402", 14},
	{"mixed_zh_en", "compaction 在 pre-turn 与 mid-turn 两条路径上都会触发，窗口按 provider.context_window 计算。", 31},
}

// TestEstimatorNeverUndercountsCorpus is the ONE property C8 promises: on real
// text, the estimate is never below the true token count.
//
// It is asserted as a direction and not as a band because the direction is
// what the system depends on. An undercount means the compaction gate opens
// after the window is already full, and the failure surfaces as a provider 400
// after a full round trip — the exact outcome this package exists to prevent.
// An overcount means one extra compaction: a wasted model call, invisible to
// the user, recoverable. So the test refuses undercounts absolutely and merely
// reports the overcount, rather than pinning a ratio that would turn every
// recalibration into a fixture-editing exercise.
//
// This is also the test that catches a weight edited toward a friendlier
// average. Every constant in tokenest.go can be lowered to make the estimate
// look tighter; each such edit walks the corpus back toward the undercounting
// regime, and this test is what says so.
func TestEstimatorNeverUndercountsCorpus(t *testing.T) {
	for _, s := range corpusSamples {
		t.Run(s.name, func(t *testing.T) {
			got := estimateTextTokens(s.text)
			if got < s.actual {
				t.Errorf("UNDERCOUNT: estimate %d < actual %d (%.0f%% of true) for %q\n"+
					"an undercount lets the history exceed the window before the gate fires, "+
					"which surfaces as a provider 400 rather than a compaction",
					got, s.actual, 100*float64(got)/float64(s.actual), truncateForMsg(s.text))
			}
			t.Logf("estimate %d, actual %d (%.2fx)", got, s.actual, float64(got)/float64(s.actual))
		})
	}
}

// TestEstimatorBeatsCharsOverFourOnCJK is the regression that names the bug C8
// fixed, on the shape that made it worst.
//
// It asserts TWO things, and separating them is the point. chars/4 is not
// merely inaccurate on CJK, it is inaccurate in the DANGEROUS direction: it
// charges a Chinese paragraph a quarter of what it costs, so the gate reads a
// third of the window while the window is full. The replacement is allowed to
// be just as far from the truth in absolute terms — on mixed_zh_en it is
// exactly as far, 13 tokens either way — as long as the error is on the side
// that costs a wasted model call rather than a provider 400. So:
//
//  1. DIRECTION (the load-bearing half): where chars/4 undercounts, the
//     estimator must not.
//  2. MAGNITUDE (the anti-cheat half): the estimator must not be FARTHER from
//     the truth than chars/4 was. Without this, "multiply by 1000" would
//     satisfy claim 1 and pass.
//
// Comparing against the old rule computed inline, rather than a pinned number,
// is what keeps the claim checkable after the new constants move.
func TestEstimatorBeatsCharsOverFourOnCJK(t *testing.T) {
	for _, s := range corpusSamples {
		if countCJKRunes(s.text) == 0 {
			continue
		}
		t.Run(s.name, func(t *testing.T) {
			old := len([]rune(s.text)) / 4
			got := estimateTextTokens(s.text)
			if old >= s.actual {
				t.Skipf("chars/4 did not undercount this sample (%d vs %d); nothing to improve on", old, s.actual)
			}
			if got < s.actual {
				t.Errorf("still undercounts CJK: estimator %d < actual %d (chars/4 was %d). "+
					"an undercount here is the original bug, whatever its magnitude",
					got, s.actual, old)
			}
			oldErr, newErr := s.actual-old, abs(got-s.actual)
			if newErr > oldErr {
				t.Errorf("farther from the truth than chars/4: off by %d (%d vs %d) where chars/4 was off by %d (%d). "+
					"overcounting is the safe direction but it is not free",
					newErr, got, s.actual, oldErr, old)
			}
			t.Logf("chars/4=%d (under by %d) estimator=%d (over by %d) actual=%d",
				old, oldErr, got, got-s.actual, s.actual)
		})
	}
}

// TestEstimateTextTokens_RunClassification pins each run class to the rate it
// was calibrated for, on the shortest input that distinguishes it.
//
// The interesting rows are the last three. A pure-letter run must NOT be
// priced as opaque — an earlier draft accepted any all-hex run, and a long run
// of 'a' is all-hex, so a padding blob was charged 6.75x its real cost and
// pushed histories over the gate that were nowhere near it. A pure-DIGIT run
// must not be opaque either, for the same reason with timestamps and byte
// counts, which appear in essentially every tool result.
func TestEstimateTextTokens_RunClassification(t *testing.T) {
	tests := []struct {
		name       string
		text       string
		wantOpaque bool
		wantDigits bool
	}{
		{"short_mixed_is_a_word", "sha256", false, false},
		{"long_mixed_is_opaque", "0242ac120002ff", true, false},
		{"long_camel_is_a_word", "NewApprovalGuardedToolFactory", false, false},
		{"repeated_letter_is_not_opaque", strings.Repeat("a", 200), false, false},
		{"all_digits_are_digits", "1754630400", false, true},
		{"short_digits_are_digits", "42", false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isOpaqueRun(tt.text); got != tt.wantOpaque {
				t.Errorf("isOpaqueRun(%q) = %v, want %v", truncateForMsg(tt.text), got, tt.wantOpaque)
			}
			if got := isAllDigits(tt.text); got != tt.wantDigits {
				t.Errorf("isAllDigits(%q) = %v, want %v", truncateForMsg(tt.text), got, tt.wantDigits)
			}
			// The divisor actually applied must match the classification: the
			// predicates above are only meaningful if wordRunDivisor consults
			// them, and an earlier draft had a branch order that made one of
			// them unreachable.
			want := wordCharsPerToken
			switch {
			case tt.wantDigits:
				want = digitCharsPerToken
			case tt.wantOpaque:
				want = opaqueCharsPerToken
			}
			if got := wordRunDivisor(tt.text); got != want {
				t.Errorf("wordRunDivisor(%q) = %v, want %v", truncateForMsg(tt.text), got, want)
			}
		})
	}
}

// TestEstimateTextTokens_EveryRunCostsAtLeastOne pins the floor. A byte-level
// BPE cannot emit a fractional token, so any single non-empty run must cost at
// least one — the rule chars/4 broke for every run shorter than four
// characters, which in punctuation-heavy JSON is most of them.
func TestEstimateTextTokens_EveryRunCostsAtLeastOne(t *testing.T) {
	for _, s := range []string{"a", "ab", "abc", ".", "{", "  ", "1", "_"} {
		if got := estimateTextTokens(s); got < 1 {
			t.Errorf("estimateTextTokens(%q) = %d, want >= 1", s, got)
		}
	}
	if got := estimateTextTokens(""); got != 0 {
		t.Errorf("estimateTextTokens(\"\") = %d, want 0", got)
	}
}

// TestEstimateTextTokens_Monotonic pins that appending text never lowers the
// estimate. Budget arithmetic all over this package (takeChunk's running sum,
// chunkBudgetFor's shrinking carry) assumes it, and a per-run rule with a
// max(1, …) floor is not obviously monotonic — merging two runs into one can
// lose a floor.
func TestEstimateTextTokens_Monotonic(t *testing.T) {
	base := "the quick brown fox "
	prev := 0
	s := ""
	for i := 0; i < 40; i++ {
		s += base
		got := estimateTextTokens(s)
		if got < prev {
			t.Fatalf("estimate dropped from %d to %d when text grew to %d chars", prev, got, len(s))
		}
		prev = got
	}
}

// TestEstimateMessageTokens_ToolCallsPricedSeparately pins that a tool call's
// name, arguments and id are each estimated as their own run.
//
// The old form summed the three lengths and divided once, which applies a
// single blended rate to a function name (word density), a JSON blob
// (punctuation density) and a call id (opaque density) — three rates that
// differ by more than 2x. This is the difference that made tool-heavy ReAct
// histories the worst case for the old estimate.
func TestEstimateMessageTokens_ToolCallsPricedSeparately(t *testing.T) {
	args := `{"path":"internal/ctxcompact/summarize.go","offset":0,"end":200}`
	msg := &schema.Message{
		Role: schema.Assistant,
		ToolCalls: []schema.ToolCall{{
			ID:       "call_9f1c2b7e4a3d11ee",
			Function: schema.FunctionCall{Name: "fs_read", Arguments: args},
		}},
	}
	got := estimateMessageTokens(msg)
	want := perMessageOverhead + perToolCallOverhead +
		estimateTextTokens("fs_read") + estimateTextTokens(args) + estimateTextTokens("call_9f1c2b7e4a3d11ee")
	if got != want {
		t.Errorf("estimateMessageTokens = %d, want %d (sum of separately-estimated parts)", got, want)
	}
	// And the blended form it replaced must be measurably different, or the
	// separation is a no-op dressed as a fix.
	blended := estimateTextTokens("fs_read" + args + "call_9f1c2b7e4a3d11ee")
	parts := estimateTextTokens("fs_read") + estimateTextTokens(args) + estimateTextTokens("call_9f1c2b7e4a3d11ee")
	if blended == parts {
		t.Errorf("separate estimation makes no difference (%d either way); the split is not doing anything", parts)
	}
}

// TestTokenCountingModeIsReported pins that the mode is exposed. The gates are
// sized off these numbers and an operator reading a compaction log needs to
// know whether they are looking at a tokenizer's answer or a heuristic's.
func TestTokenCountingModeIsReported(t *testing.T) {
	if got := TokenCountingMode(); got != TokenCountHeuristic {
		t.Errorf("TokenCountingMode() = %q, want %q", got, TokenCountHeuristic)
	}
	if TokenCountHeuristic == "" {
		t.Error("the mode constant is empty, so a log line would report nothing")
	}
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// truncateForMsg keeps a failure message readable when the offending input is
// a 200-character run.
func truncateForMsg(s string) string {
	const max = 48
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}
