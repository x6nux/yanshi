package ctxcompact

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestIsTransient_NilError covers nil error returning false.
func TestIsTransient_NilError(t *testing.T) {
	assert.False(t, isTransient(nil))
}

// TestIsTransient_ContextCancel covers context.Canceled not being transient.
func TestIsTransient_ContextCancel(t *testing.T) {
	assert.False(t, isTransient(context.Canceled))
	assert.False(t, isTransient(context.DeadlineExceeded))
}

// TestIsTransient_NonTransient covers errors that are not retryable.
func TestIsTransient_NonTransient(t *testing.T) {
	assert.False(t, isTransient(assert.AnError))
}

// TestIsTransient_TimeoutKeyword covers timeout being transient.
func TestIsTransient_TimeoutKeyword(t *testing.T) {
	assert.True(t, isTransient(errors.New("request timeout")))
	assert.True(t, isTransient(errors.New("connection reset")))
	assert.True(t, isTransient(errors.New("received 429")))
}

// TestIsTransient_EOF covers EOF being transient.
func TestIsTransient_EOF(t *testing.T) {
	assert.True(t, isTransient(errors.New("eof")))
	assert.True(t, isTransient(errors.New("broken pipe")))

}

// TestSplitIsSafe_OutOfRange covers i <= 0 and i >= len(msgs).
func TestSplitIsSafe_OutOfRange(t *testing.T) {
	msgs := []*schema.Message{
		{Role: schema.User, Content: "a"},
		{Role: schema.Assistant, Content: "b"},
	}
	assert.True(t, splitIsSafe(msgs, 0), "i=0 is <= 0 → safe")
	assert.True(t, splitIsSafe(msgs, 2), "i=2 is >= len(msgs) → safe")
	assert.True(t, splitIsSafe(msgs, 1), "i=1 with no tool pairs → safe")
}

// TestSplitIsSafe_WithOrphan checks behavior when left has a tool_call
// whose result is on the right — must return false.
func TestSplitIsSafe_WithOrphanPair(t *testing.T) {
	msgs := []*schema.Message{
		{Role: schema.User, Content: "a"},
		{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{ID: "c1", Function: schema.FunctionCall{Name: "read"}}}},
		{Role: schema.Tool, ToolCallID: "c1", Content: "result"},
	}
	// Split at i=2: left has c1 whose result is at i=2 → unsafe
	assert.False(t, splitIsSafe(msgs, 2), "split after call before result → unsafe")
	// Split at i=1: left (user) has no tool calls → safe
	assert.True(t, splitIsSafe(msgs, 1), "split before tool_call → safe")
}

// TestSplitIsSafe_ResultCallOnRight checks when right is a tool result
// whose call is on the left (but NOT at i-1, so only the right check catches it).
func TestSplitIsSafe_ResultCallOnRight(t *testing.T) {
	msgs := []*schema.Message{
		{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{ID: "c1", Function: schema.FunctionCall{Name: "read"}}}}, // 0 ← call
		{Role: schema.User, Content: "in between"},               // 1 ← no ToolCalls (left at split)
		{Role: schema.Tool, ToolCallID: "c1", Content: "result"}, // 2 ← result (right at split)
	}
	// Split at i=2: left=msgs[1] has no ToolCalls (left check passes).
	// Right=msgs[2] has ToolCallID="c1" → scans left for matching call.
	// msgs[0] has ToolCall ID="c1" → match → return false.
	assert.False(t, splitIsSafe(msgs, 2), "right check catches call on left → unsafe")
}

// TestTakeChunk_ZeroBudget returns a single-msg chunk when budget is 0.
func TestTakeChunk_ZeroBudget(t *testing.T) {
	msgs := []*schema.Message{
		{Role: schema.User, Content: "a"},
		{Role: schema.Assistant, Content: "b"},
	}
	chunk, rest := takeChunk(msgs, 0)
	// With budget=0, the first split at i=1 that's safe returns a single-item
	// chunk (the first message fits, adding the second exceeds budget 0).
	assert.Equal(t, 1, len(chunk))
	assert.NotNil(t, rest)
	assert.Equal(t, 1, len(rest))
}

// TestSingleBudget_ModelWindowZero covers ModelWindow <= 0 returning 0.
func TestSingleBudget_ModelWindowZero(t *testing.T) {
	assert.Equal(t, 0, singleBudget(RunOpts{ModelWindow: 0, ChunkThreshold: 0.9}))
	assert.Equal(t, 0, singleBudget(RunOpts{ModelWindow: -1, ChunkThreshold: 0.9}))
}

// TestSingleBudget_ChunkThresholdZero covers ChunkThreshold <= 0 returning ModelWindow.
func TestSingleBudget_ChunkThresholdZero(t *testing.T) {
	assert.Equal(t, 1000, singleBudget(RunOpts{ModelWindow: 1000, ChunkThreshold: 0}))
	assert.Equal(t, 1000, singleBudget(RunOpts{ModelWindow: 1000, ChunkThreshold: -1}))
}

// TestChunkBudgetFor_ModelWindowZero covers ModelWindow <= 0 returning 0.
func TestChunkBudgetFor_ModelWindowZero(t *testing.T) {
	assert.Equal(t, 0, chunkBudgetFor(RunOpts{ModelWindow: 0}, "", 10))
	assert.Equal(t, 0, chunkBudgetFor(RunOpts{ModelWindow: -1}, "", 10))
}

// TestBuildCarryRequest_NoCarry covers the no-carry (first chunk) path.
func TestBuildCarryRequest_NoCarry(t *testing.T) {
	chunk := []*schema.Message{{Role: schema.User, Content: "hello"}}
	req := buildCarryRequest("", chunk, 100, false, SeqRef{}, 0)
	assert.Equal(t, 2, len(req), "no carry: chunk + instruction")
	assert.Equal(t, "hello", req[0].Content)
}

// TestBuildCarryRequest_WithCarry covers the with-carry path.
func TestBuildCarryRequest_WithCarry(t *testing.T) {
	chunk := []*schema.Message{{Role: schema.User, Content: "next chunk"}}
	req := buildCarryRequest("prior summary", chunk, 100, true, SeqRef{}, 0)
	assert.Equal(t, 4, len(req), "with carry: carry + ack + chunk + instruction")
	assert.Contains(t, req[0].Content, SummarySentinel)
	assert.Equal(t, "next chunk", req[2].Content)
}

// TestTakeChunk_SplitAtToolPairBoundary covers the case where splitIsSafe
// returns false, so the toolpair is kept intact (over budget).
func TestTakeChunk_SplitAtToolPairBoundary(t *testing.T) {
	// First, confirm the normal split works when no pair is crossed.
	msgs := []*schema.Message{
		{Role: schema.User, Content: "start"},       // 0: ~9 tok
		{Role: schema.Assistant, Content: "middle"}, // 1: ~9 tok
		{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{ID: "c1", Function: schema.FunctionCall{Name: "read"}}}}, // 2: ~25 tok
		{Role: schema.Tool, ToolCallID: "c1", Content: "result"},                                                        // 3: ~9 tok
	}
	chunk, rest := takeChunk(msgs, 20)
	// After 2 messages (9+9=18), adding the third (25) exceeds budget 20.
	// Split at i=2: no tool pair crosses the boundary → safe split.
	assert.Equal(t, 2, len(chunk), "split before the tool_call")
	assert.Equal(t, 2, len(rest), "tool_call and result in rest")

	// Now test pair protection: the tool_call at index 0 and its result at
	// index 1 form a pair. A split at i=1 would separate the call (left) from
	// its result (right) — splitIsSafe must block it, keeping both in the chunk.
	msgs2 := []*schema.Message{
		{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{ID: "c1", Function: schema.FunctionCall{Name: "read"}}}}, // 0: ~25 tok
		{Role: schema.Tool, ToolCallID: "c1", Content: "result"},                                                        // 1: ~9 tok
	}
	chunk2, rest2 := takeChunk(msgs2, 20)
	// After msg0 (25) exceeds budget at i=1, but splitIsSafe blocks because
	// left (msg0) has ToolCall c1 whose result is at i=1 (right).
	// So the loop continues, tok=25+9=34, and returns all msgs as one chunk.
	assert.Equal(t, 2, len(chunk2), "pair kept intact even over budget")
	assert.Nil(t, rest2, "no remainder when pair integrity prevents any split")
}

// TestSplitIsSafe_NilLeftMessage covers the left == nil branch.
func TestSplitIsSafe_NilLeftMessage(t *testing.T) {
	msgs := []*schema.Message{
		nil,                               // 0: nil — left at i=1
		{Role: schema.User, Content: "b"}, // 1: right at i=1
	}
	// Split at i=1: left=msgs[0]=nil → left check skipped (left==nil).
	// Right=msgs[1] has no ToolCallID → right check skipped → safe.
	assert.True(t, splitIsSafe(msgs, 1), "nil left message → left check skipped")
}

// TestSplitIsSafe_EmptyToolCallInLeft covers the tc.ID=="" continue in left-side check.
func TestSplitIsSafe_EmptyToolCallInLeft(t *testing.T) {
	msgs := []*schema.Message{
		{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{ID: "", Function: schema.FunctionCall{Name: "read"}}}},
		{Role: schema.User, Content: "right"},
	}
	// Split at i=1: left has a ToolCall with empty ID → skipped (continue).
	// Right has no ToolCallID → right check skipped → safe.
	assert.True(t, splitIsSafe(msgs, 1), "ToolCall with empty ID in left → continue → safe")
}

// TestTakeChunk_NilMessageInList handles nil messages within the chunked list.
func TestTakeChunk_NilMessageInList(t *testing.T) {
	msgs := []*schema.Message{
		{Role: schema.User, Content: "a"},
		nil,
		{Role: schema.User, Content: "b"},
	}
	chunk, rest := takeChunk(msgs, 100)
	assert.Equal(t, 3, len(chunk), "nil messages stay in chunk")
	assert.Nil(t, rest)
}

// TestTakeChunk_OvershootShapesAreMeasured is the reproduction command behind
// the overshoot discussion in docs/compaction.md. That section used to state
// four measured ratios with no way to re-derive them: unlike the neighbouring
// 24.15x figure, which names the `go test -run` that prints it, the table was
// four bare numbers nobody could check and nothing could keep honest.
//
// Run it with -v to see the numbers:
//
//	go test ./internal/ctxcompact -run TestTakeChunk_OvershootShapesAreMeasured -v
//
// The ASSERTIONS are deliberately about shape rather than digits, because the
// digits are fixture-dependent and would rot exactly the way the table did.
// What the doc claims, and what is checked here, is that both overshoot
// mechanisms are real and that only one of them is pairing:
//
//   - the parallel-result ratio grows with the parallel tool count, unbounded
//     by any multiple of the window;
//   - a single oversized message overshoots on its own, with no tool pair
//     anywhere in the input.
func TestTakeChunk_OvershootShapesAreMeasured(t *testing.T) {
	const budget = 1000
	// One result is already about the size of the whole budget, which is the
	// regime docs/compaction.md describes: even n=1 cannot be split off.
	result := strings.Repeat("r", 4000)

	ratios := map[int]float64{}
	for _, n := range []int{1, 5, 20} {
		var calls []schema.ToolCall
		msgs := []*schema.Message{nil} // placeholder for the call message
		for i := 0; i < n; i++ {
			id := fmt.Sprintf("id%d", i)
			calls = append(calls, schema.ToolCall{
				ID: id, Function: schema.FunctionCall{Name: "fs_read", Arguments: `{"path":"a.go"}`}})
			msgs = append(msgs, &schema.Message{Role: schema.Tool, ToolCallID: id, Content: result})
		}
		msgs[0] = &schema.Message{Role: schema.Assistant, ToolCalls: calls}

		chunk, _ := takeChunk(msgs, budget)
		tok := EstimateTokens(chunk)
		ratios[n] = float64(tok) / budget
		t.Logf("n=%d parallel results: chunk=%d tok, %.2fx budget=%d (whole group is one indivisible run)",
			n, tok, ratios[n], budget)
		assert.Len(t, chunk, len(msgs),
			"every interior cut point in a parallel group is unsafe, so the whole group ships as one chunk")
	}
	assert.Greater(t, ratios[1], 1.0, "one pair of this size already exceeds the budget")
	assert.Greater(t, ratios[5], ratios[1])
	assert.Greater(t, ratios[20], ratios[5],
		"the overshoot tracks the parallel tool count, which is why no multiple of the window bounds it")

	// Mechanism 2: index 0 is never budget-checked, so one huge message
	// overshoots with no pairing involved.
	huge := []*schema.Message{
		{Role: schema.User, Content: strings.Repeat("x", 40000)},
		{Role: schema.User, Content: "short"},
	}
	chunk, rest := takeChunk(huge, budget)
	tok := EstimateTokens(chunk)
	t.Logf("single oversized message (no tool pair): chunk=%d tok, %.2fx budget=%d",
		tok, float64(tok)/budget, budget)
	assert.Len(t, chunk, 1, "the oversized message ships alone; the next one is cut normally")
	assert.Len(t, rest, 1)
	assert.Greater(t, tok, budget,
		"index 0 is never budget-checked, so this overshoot owes nothing to tool pairing")
}

// TestSummaryRetryBackoffSequence pins the retry schedule that docs/compaction.md
// quotes to operators. The old doc line said "重试 3 次指数退避（1s/2s/4s）",
// which is wrong in both halves: summaryRetryMax counts ATTEMPTS (3), not
// retries (2), and the 4s step is unreachable because the final attempt is not
// followed by a sleep. An operator budgeting worst-case latency from that line
// doubles it (7s instead of 3s).
//
// Asserting the derived slice rather than re-deriving the shift keeps this from
// being an F4 wish: change summaryRetryMax or summaryRetryBaseMs and this test
// tells you the new sequence to publish.
func TestSummaryRetryBackoffSequence(t *testing.T) {
	got := summaryRetryBackoffs()
	assert.Equal(t, []time.Duration{1 * time.Second, 2 * time.Second}, got)

	// The structural claims behind the numbers, stated separately so a future
	// constant change fails on the reason, not just on the literal.
	assert.Len(t, got, summaryRetryMax-1,
		"one backoff per retry: the last of the %d attempts is not followed by a sleep", summaryRetryMax)
	var total time.Duration
	for _, d := range got {
		total += d
		assert.NotEqual(t, 4*time.Second, d, "no 4s step is reachable")
	}
	assert.Equal(t, 3*time.Second, total, "worst-case time spent sleeping across a full exhaustion")
}

// alwaysTransientModel fails every Stream/Generate with a retryable error, so
// callWithRetry walks its whole backoff schedule and then gives up.
type alwaysTransientModel struct{ calls int }

func (m *alwaysTransientModel) Stream(context.Context, []*schema.Message, ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	m.calls++
	return nil, errors.New("connection reset by peer")
}

func (m *alwaysTransientModel) Generate(context.Context, []*schema.Message, ...model.Option) (*schema.Message, error) {
	return nil, errors.New("connection reset by peer")
}

// TestCallWithRetrySleepsTheDerivedBackoffs is the behavioural half of
// TestSummaryRetryBackoffSequence, which only proves the function computes
// 1s/2s — not that anything sleeps them. Inlining the doubling back into
// callWithRetry (and deleting `backoffs :=` so the compiler stays quiet) would
// orphan summaryRetryBackoffs without failing a single test, leaving
// docs/compaction.md pointing at a value that no longer drives behaviour.
//
// It substitutes the timer seam instead of sleeping, so the assertion is on the
// exact requested durations rather than on wall-clock slop, and the test costs
// no real time.
func TestCallWithRetrySleepsTheDerivedBackoffs(t *testing.T) {
	var slept []time.Duration
	orig := summaryAfter
	summaryAfter = func(d time.Duration) <-chan time.Time {
		slept = append(slept, d)
		fired := make(chan time.Time, 1)
		fired <- time.Now()
		return fired
	}
	t.Cleanup(func() { summaryAfter = orig })

	m := &alwaysTransientModel{}
	_, err := callWithRetry(context.Background(), m,
		[]*schema.Message{{Role: schema.User, Content: "x"}}, nil)
	require.Error(t, err, "an always-transient model must exhaust the retries")

	assert.Equal(t, summaryRetryMax, m.calls,
		"the loop must make summaryRetryMax attempts, not summaryRetryMax retries")
	assert.Equal(t, summaryRetryBackoffs(), slept,
		"callWithRetry did not sleep the sequence summaryRetryBackoffs derives — "+
			"either the loop stopped using it (making the function an orphan the "+
			"docs still quote) or the two have diverged")
}
