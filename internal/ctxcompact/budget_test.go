// internal/ctxcompact/budget_test.go
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

// TestEffectiveReserve_Table pins the reserve policy across the window sizes
// yanshi actually runs against.
//
// The proportional shape is the point. A flat 4096-token reserve is a 1.6%
// haircut on a 256K window and the ENTIRE budget on a 4K one — so the small
// fast summary models that compaction.model exists to enable would be unable to
// compact anything. Each row therefore names which term is binding, so a change
// that flattens the curve fails on the reason and not just on a digit.
func TestEffectiveReserve_Table(t *testing.T) {
	cases := []struct {
		name       string
		window     int
		configured int
		want       int
		binding    string
	}{
		{name: "unbudgeted window reserves nothing", window: 0, want: 0,
			binding: "no window means no budgeting anywhere in this package"},
		{name: "negative window reserves nothing", window: -1, want: 0},
		{name: "large window is capped", window: 256000, want: DefaultOutputReserve,
			binding: "the cap: a proportional reserve would be 64000, far past any completion"},
		{name: "at the cap boundary", window: 16384, want: 4096,
			binding: "window/4 meets the cap exactly here"},
		{name: "small window scales down", window: 8000, want: 2000,
			binding: "window/4: a flat 4096 would take half of this"},
		{name: "tiny window stays workable", window: 300, want: 75,
			binding: "window/4: the carry-loop tests run here and must keep a usable budget"},
		{name: "explicit reserve is honoured", window: 100000, configured: 8192, want: 8192,
			binding: "the caller knows its own max_tokens"},
		{name: "explicit reserve is clamped to half the window", window: 1000, configured: 900, want: 500,
			binding: "the reply may not outweigh the context it replies to"},
		{name: "negative configured reserve falls back to the default", window: 8000, configured: -5, want: 2000,
			binding: "a miscomputed reserve must not produce a budget LARGER than the window"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, effectiveReserve(tc.window, tc.configured), tc.binding)
		})
	}
}

// TestEffectiveReserve_NeverExceedsHalfTheWindow states the clamp as a property
// over the whole range rather than at the two points the table happens to list.
//
// The invariant that matters is that a budget is always left: if the reserve
// could reach the window, budgetFor would return zero or less and every history
// would overflow, turning C9 from a safety net into a total outage.
func TestEffectiveReserve_NeverExceedsHalfTheWindow(t *testing.T) {
	for _, window := range []int{1, 2, 7, 100, 300, 4096, 8000, 128000, 1000000} {
		for _, configured := range []int{0, -1, 1, 500, 100000, 1 << 30} {
			got := effectiveReserve(window, configured)
			assert.LessOrEqual(t, got, window/maxOutputReserveDenominator,
				"window=%d configured=%d reserved more than half the window", window, configured)
			assert.GreaterOrEqual(t, got, 0,
				"window=%d configured=%d produced a negative reserve", window, configured)
		}
	}
}

// TestBudgetFor_SubtractsTheReserve is the C9 headline: the input budget is the
// window LESS the reserve, so a history compacted to fit still leaves room for
// the reply. Before C9 this was the raw window, which silently assumed the
// reply costs nothing.
func TestBudgetFor_SubtractsTheReserve(t *testing.T) {
	t.Run("the reserve comes off the window", func(t *testing.T) {
		opts := RunOpts{ModelWindow: 100000, OutputReserve: 8192}
		assert.Equal(t, 100000-8192, budgetFor(opts))
		assert.Less(t, budgetFor(opts), opts.ModelWindow,
			"a budget equal to the window is the bug C9 fixes")
	})

	t.Run("an unconfigured reserve still applies", func(t *testing.T) {
		// The likely regression: someone adds OutputReserve, wires nothing, and
		// the default path keeps using the raw window.
		opts := RunOpts{ModelWindow: 100000}
		assert.Equal(t, 100000-DefaultOutputReserve, budgetFor(opts))
	})

	t.Run("an unbudgeted window has no budget", func(t *testing.T) {
		assert.Equal(t, 0, budgetFor(RunOpts{ModelWindow: 0}))
		assert.Equal(t, 0, budgetFor(RunOpts{ModelWindow: -5}))
	})
}

// TestSummaryCallBudgetsIgnoreTheOutputReserve pins the boundary between the
// two budgets, which is the single most confusable part of C9.
//
// There are two different budgets with two different owners. budgetFor is the
// TURN's: how much history may be carried so the ASSISTANT can reply.
// singleBudget/chunkBudgetFor size the SUMMARY CALL, a separate request whose
// own output headroom is ChunkThreshold. Charging the summary call the turn's
// reserve compounds the two — measured during development: it pushed histories
// that fit the single cache-aligned path onto the chunked path, losing the
// prefix-cache hit that path exists for and changing behaviour for every
// existing caller.
func TestSummaryCallBudgetsIgnoreTheOutputReserve(t *testing.T) {
	base := RunOpts{ModelWindow: 10000, ChunkThreshold: 0.9}
	reserved := base
	reserved.OutputReserve = 4000

	assert.Equal(t, singleBudget(base), singleBudget(reserved),
		"singleBudget sizes the SUMMARY call and must not be charged the turn's output reserve")
	assert.Equal(t, chunkBudgetFor(base, "", 50), chunkBudgetFor(reserved, "", 50),
		"chunkBudgetFor sizes the SUMMARY call and must not be charged the turn's output reserve")

	// The contrast: the turn budget DOES move. Without this the assertions
	// above would also hold for an OutputReserve that was wired nowhere at all.
	assert.NotEqual(t, budgetFor(base), budgetFor(reserved),
		"the turn budget must respond to the reserve, or C9 is not wired to anything")
}

// TestCheckContextLimit_Table covers the exported pre-send gate: the thing a
// transport calls immediately before dispatching.
func TestCheckContextLimit_Table(t *testing.T) {
	// ~33 tokens each (chars/4 + 8 overhead).
	msg := func(n int) []*schema.Message {
		out := make([]*schema.Message, n)
		for i := range out {
			out[i] = &schema.Message{Role: schema.User, Content: strings.Repeat("x", 100)}
		}
		return out
	}

	t.Run("a history within budget passes", func(t *testing.T) {
		assert.NoError(t, CheckContextLimit(msg(5), RunOpts{ModelWindow: 10000}))
	})

	t.Run("a history over budget is refused with the numbers", func(t *testing.T) {
		msgs := msg(100) // ~3300 tokens
		opts := RunOpts{ModelWindow: 1000}
		err := CheckContextLimit(msgs, opts)

		require.Error(t, err, "an over-budget history must be refused locally, not by a provider 400")
		assert.ErrorIs(t, err, ErrContextOverflow,
			"recovery code matches the sentinel; an untyped error cannot be recovered from")

		var overflow *ContextOverflowError
		require.ErrorAs(t, err, &overflow,
			"the measurements must be reachable: how far over decides whether a retry can help")
		assert.Equal(t, EstimateTokens(msgs), overflow.Tokens)
		assert.Equal(t, budgetFor(opts), overflow.Limit)
		assert.Equal(t, 1000, overflow.Window)
		assert.Equal(t, effectiveReserve(1000, 0), overflow.Reserve)
		assert.Greater(t, overflow.Tokens, overflow.Limit)
	})

	t.Run("the reserve is what makes the difference", func(t *testing.T) {
		// A history that fits the raw window but NOT the window less the
		// reserve. This is precisely the request that used to be forwarded and
		// come back as a 400 with no room to answer, so it is the case C9
		// exists for; if the reserve were ever dropped this is the only test
		// here that would notice.
		opts := RunOpts{ModelWindow: 1000, OutputReserve: 400}
		msgs := msg(24) // ~792 tokens: under 1000, over 600
		require.Less(t, EstimateTokens(msgs), opts.ModelWindow,
			"fixture must fit the RAW window, or it proves nothing about the reserve")
		require.Greater(t, EstimateTokens(msgs), budgetFor(opts),
			"fixture must exceed the RESERVED budget")

		assert.Error(t, CheckContextLimit(msgs, opts),
			"a history that fits the window but leaves no room to reply must be refused")
	})

	t.Run("an unbudgeted window can never overflow", func(t *testing.T) {
		assert.NoError(t, CheckContextLimit(msg(1000), RunOpts{ModelWindow: 0}),
			"with no window configured there is no limit to be over")
	})

	t.Run("an empty history never overflows", func(t *testing.T) {
		assert.NoError(t, CheckContextLimit(nil, RunOpts{ModelWindow: 1000}))
	})
}

// TestContextOverflowError_Message asserts the rendered text carries every
// number an operator needs. "Context overflow" alone cannot distinguish a 5%
// overshoot (compact harder) from a 5x one (the window is wrong).
func TestContextOverflowError_Message(t *testing.T) {
	err := &ContextOverflowError{Tokens: 9000, Limit: 6000, Window: 8000, Reserve: 2000}
	msg := err.Error()

	for _, want := range []string{"9000", "6000", "8000", "2000"} {
		assert.Contains(t, msg, want, "every measurement must appear in the message")
	}
	assert.True(t, errors.Is(err, ErrContextOverflow))
}

// TestRun_ReportsOverflowWithoutDiscardingTheCompaction is the C9 decision that
// is easiest to get backwards, so it is asserted from both sides.
//
// Overflow is reported on Result.Overflow and the compacted history is STILL
// returned. Raising it as an error instead would make every caller fall back to
// the ORIGINAL history — which is strictly larger than the one that just failed
// to fit — turning "too big" into "even bigger". The refusal to send belongs to
// whoever sends.
func TestRun_ReportsOverflowWithoutDiscardingTheCompaction(t *testing.T) {
	msgs := []*schema.Message{
		{Role: schema.User, Content: strings.Repeat("a", 4000)},
		{Role: schema.Assistant, Content: strings.Repeat("b", 4000)},
		{Role: schema.Assistant, Content: strings.Repeat("c", 4000)},
		{Role: schema.Assistant, Content: strings.Repeat("d", 4000)},
		{Role: schema.User, Content: strings.Repeat("e", 4000)},
		{Role: schema.Assistant, Content: "tail"},
	}
	// A window far too small for the PINNED set alone, so no summary of the
	// rest can rescue it — the shape where overflow is unavoidable.
	opts := RunOpts{ModelWindow: 600, ChunkThreshold: 0.9}
	rs := &recordingSummarizer{Return: realSummary}

	res, err := Run(context.Background(), msgs, PlanOpts{KeepRecent: 1}, opts, rs, nil)

	require.NoError(t, err,
		"overflow must NOT be an error: callers treat errors as `keep the original`, "+
			"and the original is bigger than what we just built")
	require.NotNil(t, res)
	require.Error(t, res.Overflow, "the overflow must still be reported to the caller")
	assert.ErrorIs(t, res.Overflow, ErrContextOverflow)
	assert.NotEmpty(t, res.Messages, "the compacted history is still the best thing available")
	assert.Less(t, res.TokensAfter, res.TokensBefore, "and it is still smaller than the input")
}

// TestRun_NoOverflowOnAComfortableWindow is the contrast case: Result.Overflow
// must be nil when the result fits. Without it, a checkOverflow hard-wired to
// always report would satisfy the test above.
func TestRun_NoOverflowOnAComfortableWindow(t *testing.T) {
	msgs := []*schema.Message{
		{Role: schema.User, Content: "task"},
		{Role: schema.Assistant, Content: strings.Repeat("x", 2000)},
		{Role: schema.Assistant, Content: strings.Repeat("y", 2000)},
		{Role: schema.Assistant, Content: strings.Repeat("z", 2000)},
		{Role: schema.User, Content: "status"},
		{Role: schema.Assistant, Content: "done"},
	}
	rs := &recordingSummarizer{Return: realSummary}

	res, err := Run(context.Background(), msgs, PlanOpts{KeepRecent: 1},
		RunOpts{ModelWindow: 100000, ChunkThreshold: 0.9}, rs, nil)

	require.NoError(t, err)
	require.NotNil(t, res)
	assert.NoError(t, res.Overflow, "a comfortably-fitting result must not be flagged")
}

// TestRun_ReportsOverflowWhenNothingWasSummarized covers the branch most likely
// to overflow and easiest to forget: Plan pinned everything, so Run returns
// early with the input unchanged. "Compaction had nothing to do" and "the result
// fits" are independent claims, and a caller that forwards this result needs the
// second one answered too.
func TestRun_ReportsOverflowWhenNothingWasSummarized(t *testing.T) {
	// All-user history: Plan pins every message via isUserOriginal, so
	// SummarizeIndices is empty and Run takes the early return.
	msgs := make([]*schema.Message, 20)
	for i := range msgs {
		msgs[i] = &schema.Message{Role: schema.User, Content: strings.Repeat("q", 400)}
	}
	rs := &recordingSummarizer{Return: realSummary}

	res, err := Run(context.Background(), msgs, PlanOpts{KeepRecent: 1},
		RunOpts{ModelWindow: 500, ChunkThreshold: 0.9}, rs, nil)

	require.NoError(t, err)
	require.NotNil(t, res)
	assert.Equal(t, 0, len(rs.GenerateCalls)+len(rs.StreamCalls),
		"nothing was summarizable, so no model call should have been spent")
	require.Error(t, res.Overflow,
		"the early return must still answer whether the history fits")
	assert.ErrorIs(t, res.Overflow, ErrContextOverflow)
}
