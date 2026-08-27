// internal/ctxcompact/quality_test.go
package ctxcompact

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// realSummary is a summary of the shape the instruction actually asks for:
// file paths, a command, an outcome, a next step. It is used as the ACCEPT
// fixture throughout so that every reject case is contrasted against something
// the gate must let through — a gate that rejects everything would otherwise
// pass every rejection test in this file.
const realSummary = "Edited internal/ctxcompact/summarize.go to add the output reserve; " +
	"ran go test ./internal/ctxcompact and 3 tests failed on the chunk budget; " +
	"next step is to stop subtracting the reserve twice."

// TestCheckQuality_Table is the main C10 table. Each case names the failure
// shape and the rule that must fire on it.
//
// inputRunes is stated per case because the length rule is RELATIVE: the same
// candidate is fine for a small history and a catastrophe for a large one, and
// a table that fixed the input size would only ever test one half of that.
func TestCheckQuality_Table(t *testing.T) {
	cases := []struct {
		name       string
		summary    string
		inputRunes int
		policy     QualityPolicy
		wantRules  []string // empty means "must be accepted"
		why        string
	}{
		{
			name:       "a real summary of a large history is accepted",
			summary:    realSummary,
			inputRunes: 200000,
			policy:     DefaultQualityPolicy,
			why:        "the gate must not reject the thing it exists to let through",
		},
		{
			name:       "a real summary of a small history is accepted",
			summary:    realSummary,
			inputRunes: 500,
			policy:     DefaultQualityPolicy,
			why:        "small inputs must not be held to the large-input floor",
		},
		{
			// THE motivating case: not blank, so the pre-existing empty check
			// passes it, and Assemble would then drop the entire middle of the
			// conversation and leave this sentence in its place.
			name:       "Chinese acknowledgement standing in for a whole session",
			summary:    "好的，我已经总结完毕。",
			inputRunes: 200000,
			policy:     DefaultQualityPolicy,
			wantRules:  []string{"too_short", "meta_only"},
			why:        "an acknowledgement is not a summary, however polite",
		},
		{
			name:       "English acknowledgement",
			summary:    "Sure, here is the summary.",
			inputRunes: 200000,
			policy:     DefaultQualityPolicy,
			wantRules:  []string{"too_short", "meta_only"},
		},
		{
			name:       "a refusal is not a summary",
			summary:    "I cannot summarize this conversation.",
			inputRunes: 200000,
			policy:     DefaultQualityPolicy,
			wantRules:  []string{"too_short", "meta_only"},
		},
		{
			// Long enough to clear every length rule, and still not a summary.
			// This is why isMetaOnly exists as a separate rule rather than
			// being folded into the length check.
			name:       "a padded acknowledgement clears the length rules",
			summary:    "好的。" + strings.Repeat("。", 400),
			inputRunes: 200000,
			policy:     DefaultQualityPolicy,
			wantRules:  []string{"meta_only"},
			why:        "punctuation padding must not buy a way past the gate",
		},
		{
			// The counter-case that keeps isMetaOnly from being over-broad: a
			// chatty preamble in front of a REAL summary is fine, and rejecting
			// it would throw away a good compaction over a stylistic tic.
			name:       "a chatty preamble in front of a real summary is accepted",
			summary:    "Sure, here is the summary: " + realSummary,
			inputRunes: 200000,
			policy:     DefaultQualityPolicy,
			why:        "the phrase match is prefix-anchored AND residue-bounded, not `contains`",
		},
		{
			name:       "Chinese preamble in front of a real summary is accepted",
			summary:    "以下是总结：修改了 internal/ctxcompact/budget.go，运行 go test 后有三个用例失败，下一步是修正 chunk 预算的重复扣减逻辑。",
			inputRunes: 200000,
			policy:     DefaultQualityPolicy,
			why:        "the Chinese half of the phrase list must be anchored the same way",
		},
		{
			name:       "a huge session collapsed to a fragment",
			summary:    "Worked on the code.",
			inputRunes: 200000,
			policy:     DefaultQualityPolicy,
			wantRules:  []string{"too_short"},
			why:        "not meta, just useless: the length rule is what catches this one",
		},
		{
			name:       "empty summary",
			summary:    "",
			inputRunes: 200000,
			policy:     DefaultQualityPolicy,
			wantRules:  []string{"too_short", "meta_only"},
		},
		{
			name:       "whitespace-only summary",
			summary:    "   \n\t  ",
			inputRunes: 200000,
			policy:     DefaultQualityPolicy,
			wantRules:  []string{"too_short", "meta_only"},
		},
		{
			name:       "a zero policy accepts anything",
			summary:    "好的",
			inputRunes: 200000,
			policy:     QualityPolicy{},
			why:        "the zero value must be the off switch, or the gate cannot be adopted incrementally",
		},
		{
			name:       "required sections are enforced when configured",
			summary:    realSummary,
			inputRunes: 200000,
			policy:     QualityPolicy{MinChars: 10, RequiredSections: []string{"## Open Work"}},
			wantRules:  []string{"missing_section"},
		},
		{
			name:    "required sections are satisfied case-insensitively",
			summary: realSummary + "\n\n## OPEN WORK\n- fix the budget",
			// A structured policy is only meaningful with a structured prompt;
			// the gate matches the heading however the model cased it.
			inputRunes: 200000,
			policy:     QualityPolicy{MinChars: 10, RequiredSections: []string{"## Open Work"}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckQuality(tc.summary, tc.inputRunes, tc.policy)

			if len(tc.wantRules) == 0 {
				assert.NoError(t, err, tc.why)
				return
			}
			require.Error(t, err, tc.why)
			assert.ErrorIs(t, err, ErrSummaryRejected,
				"every quality failure must match the sentinel so callers can classify it")

			var rejected *SummaryRejectedError
			require.ErrorAs(t, err, &rejected,
				"the concrete error must carry the issues, or the caller cannot log which rule fired")

			got := make([]string, 0, len(rejected.Issues))
			for _, iss := range rejected.Issues {
				got = append(got, iss.Rule)
			}
			assert.ElementsMatch(t, tc.wantRules, got, "wrong rules fired: %v", got)
		})
	}
}

// TestRequiredRunes_ScalesWithInput pins the min(MinChars, input/N) shape
// directly, because it is the part of C10 most likely to be "simplified" back
// into a flat floor.
//
// A flat floor is wrong at the small end (it rejects the correct one-line
// summary of a six-message exchange) and a bare ratio is wrong at the large end
// (it demands nothing of a session that collapsed to a sentence). The table
// walks the input size across both regimes and asserts which term binds.
func TestRequiredRunes_ScalesWithInput(t *testing.T) {
	p := DefaultQualityPolicy // MinChars 80, denominator 1000
	cases := []struct {
		inputRunes int
		want       int
		binding    string
	}{
		{inputRunes: 0, want: 80, binding: "MinChars (no input measurement available)"},
		{inputRunes: 1000, want: 1, binding: "the ratio: a tiny history may summarize to a line"},
		{inputRunes: 40000, want: 40, binding: "the ratio, still under the cap"},
		{inputRunes: 80000, want: 80, binding: "the two meet here"},
		{inputRunes: 500000, want: 80, binding: "MinChars caps the demand"},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, p.requiredRunes(tc.inputRunes),
			"input=%d should be bound by %s", tc.inputRunes, tc.binding)
	}

	t.Run("the demand never exceeds MinChars", func(t *testing.T) {
		// Stated as a property rather than a row so it holds for inputs the
		// table does not list: an unbounded demand would reject every summary
		// of a long enough session.
		for _, in := range []int{0, 1, 999, 100000, 10000000} {
			assert.LessOrEqual(t, p.requiredRunes(in), p.MinChars,
				"input=%d produced a demand above the cap", in)
		}
	})

	t.Run("a zero denominator makes MinChars a flat floor", func(t *testing.T) {
		flat := QualityPolicy{MinChars: 50}
		assert.Equal(t, 50, flat.requiredRunes(10))
		assert.Equal(t, 50, flat.requiredRunes(100000))
	})
}

// TestIsMetaOnly_ResidueBound pins the two guards that keep the phrase list
// from eating real summaries. Without them "contains a polite opener" would be
// grounds for throwing away a correct compaction.
func TestIsMetaOnly_ResidueBound(t *testing.T) {
	cases := []struct {
		name string
		text string
		want bool
	}{
		{"bare acknowledgement", "好的", true},
		{"acknowledgement with punctuation only", "好的。—— 已完成 ✓", true},
		{"acknowledgement plus a real summary is not meta", "好的。" + realSummary, false},
		{"preamble plus a real summary is not meta", "Sure, here is the summary: " + realSummary, false},
		{"a long text is never meta whatever it opens with", "Okay. " + strings.Repeat("real content here. ", 200), false},
		{"a summary that merely mentions a phrase mid-text is not meta",
			"The user said okay to the plan; the assistant then edited main.go and ran the tests, which passed.", false},
		{"empty is meta", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, isMetaOnly(tc.text))
		})
	}
}

// TestRun_QualityFailureKeepsHistory is the one-way-failure requirement stated
// end to end, and it is the assertion that matters most in this file.
//
// "Reject" must mean the history is NOT replaced. A gate that noticed the bad
// summary, logged it, and used it anyway would satisfy every unit test above
// while still destroying the conversation. Run returning (nil, err) is what
// makes MaybeCompact and CompactingModel fall back to the original messages.
func TestRun_QualityFailureKeepsHistory(t *testing.T) {
	msgs := []*schema.Message{
		{Role: schema.User, Content: "do the work"},
		{Role: schema.Assistant, Content: strings.Repeat("important detail ", 200)},
		{Role: schema.Assistant, Content: strings.Repeat("more detail ", 200)},
		{Role: schema.Assistant, Content: strings.Repeat("further detail ", 200)},
		{Role: schema.User, Content: "status?"},
		{Role: schema.Assistant, Content: "ok"},
	}
	opts := RunOpts{ModelWindow: 100000, ChunkThreshold: 0.9}

	t.Run("an acknowledgement is refused", func(t *testing.T) {
		rs := &recordingSummarizer{Return: "好的，我已经总结完毕。"}
		res, err := Run(context.Background(), msgs, PlanOpts{KeepRecent: 1}, opts, rs, nil)

		require.Error(t, err, "an acknowledgement must not be allowed to replace the history")
		assert.Nil(t, res, "a rejected compaction must not hand back a partial result")
		assert.ErrorIs(t, err, ErrSummaryRejected,
			"the caller must be able to tell a rejected summary from a provider failure")

		var rejected *SummaryRejectedError
		require.ErrorAs(t, err, &rejected)
		assert.NotEmpty(t, rejected.Issues, "the reasons must ride out on the error for logging")
	})

	t.Run("a real summary is accepted", func(t *testing.T) {
		// The contrast case. Without it, every assertion above is satisfied by
		// a Run that rejects unconditionally — which would disable compaction
		// entirely while looking, in this file, like a working gate.
		rs := &recordingSummarizer{Return: realSummary}
		res, err := Run(context.Background(), msgs, PlanOpts{KeepRecent: 1}, opts, rs, nil)

		require.NoError(t, err)
		require.NotNil(t, res)
		assert.Less(t, res.TokensAfter, res.TokensBefore, "a good summary must still compact")
		assert.True(t, IsSummaryMessage(res.Messages[len(res.Messages)-1]))
	})

	t.Run("the gate can be switched off", func(t *testing.T) {
		off := opts
		off.DisableQualityGate = true
		rs := &recordingSummarizer{Return: "好的"}
		res, err := Run(context.Background(), msgs, PlanOpts{KeepRecent: 1}, off, rs, nil)

		require.NoError(t, err, "DisableQualityGate must actually disable the gate")
		require.NotNil(t, res)
	})
}

// TestRunOpts_QualityPolicyDefaults pins the three-way resolution, including
// the distinction the DisableQualityGate field exists to make: a caller who
// left Quality zero gets the defaults, and only an explicit opt-out gets none.
func TestRunOpts_QualityPolicyDefaults(t *testing.T) {
	t.Run("unset means default", func(t *testing.T) {
		assert.Equal(t, DefaultQualityPolicy, RunOpts{}.qualityPolicy(),
			"a caller who forgot to configure a policy must still get the floors")
	})
	t.Run("explicit policy wins", func(t *testing.T) {
		custom := QualityPolicy{MinChars: 5}
		assert.Equal(t, custom, RunOpts{Quality: custom}.qualityPolicy())
	})
	t.Run("disable beats an explicit policy too", func(t *testing.T) {
		got := RunOpts{Quality: QualityPolicy{MinChars: 5}, DisableQualityGate: true}.qualityPolicy()
		assert.False(t, got.enabled(), "the opt-out must win over a configured policy")
	})
}

// TestSummaryRejectedError_Message asserts the error text carries the
// measurements. An operator seeing this in a log needs to know whether the
// summary model is too weak or the history is pathological, and "compaction
// failed" answers neither.
func TestSummaryRejectedError_Message(t *testing.T) {
	err := CheckQuality("好的", 200000, DefaultQualityPolicy)
	require.Error(t, err)
	msg := err.Error()

	assert.Contains(t, msg, "200000", "the input size must appear")
	assert.Contains(t, msg, "meta_only", "the rule that fired must be named")
	assert.True(t, errors.Is(err, ErrSummaryRejected))
}
