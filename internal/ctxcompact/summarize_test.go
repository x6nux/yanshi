// internal/ctxcompact/summarize_test.go
package ctxcompact_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/ctxcompact"
	einollm "github.com/x6nux/yanshi/internal/llm/eino"
)

// Single cache-aligned: summarize set ≤ 0.9 window, one call, prefix = msgs verbatim.
func TestRunSummary_SingleCacheAligned(t *testing.T) {
	// Echo model returns concatenated input content, proving the original msgs
	// reached the model verbatim (cache-aligned prefix).
	fm := einollm.NewFakeModel(nil, nil)
	fm.Echo = true
	msgs := []*schema.Message{
		{Role: schema.User, Content: "alpha"},
		{Role: schema.Assistant, Content: "beta"},
	}
	summary, err := ctxcompact.RunSummary(context.Background(), msgs, ctxcompact.RunOpts{ModelWindow: 10000, ChunkThreshold: 0.9, SummaryWordLimit: 200}, fm, nil)
	require.NoError(t, err)
	assert.Contains(t, summary, "alpha")
	assert.Contains(t, summary, "beta")
}

// Carry-style chunking: summarize set > 0.9 window -> split, each chunk input ≤ budget.
func TestRunSummary_CarryChunkedStaysInBudget(t *testing.T) {
	rm := &recordingModel{reply: "summary-chunk"}
	big := []*schema.Message{}
	for i := 0; i < 40; i++ {
		big = append(big, &schema.Message{Role: schema.User, Content: strings.Repeat("x", 200)}) // ~58 tok each
	}
	_, err := ctxcompact.RunSummary(context.Background(), big, ctxcompact.RunOpts{ModelWindow: 300, ChunkThreshold: 0.9, SummaryWordLimit: 100}, rm, nil)
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(rm.inputs), 2, "must split into multiple chunks")
	window := 300 // opts.ModelWindow in this test
	for i, in := range rm.inputs {
		tok := ctxcompact.EstimateTokens(in)
		assert.LessOrEqual(t, tok, window, "chunk %d input must fit MODEL WINDOW (carry+chunk+ack+instruction ≤ window)", i)
	}
}

// Failure must NOT produce an empty summary: retries exhausted -> error.
func TestRunSummary_FailureReturnsError(t *testing.T) {
	fm := einollm.NewFakeModel(nil, errors.New("boom"))
	_, err := ctxcompact.RunSummary(context.Background(), []*schema.Message{{Role: schema.User, Content: "x"}},
		ctxcompact.RunOpts{ModelWindow: 10000, ChunkThreshold: 0.9}, fm, nil)
	assert.Error(t, err, "failure must surface, not produce empty summary (bug⑥)")
}

// I-3: dynamic budget must SHRINK as carry grows — a fixed-overhead
// implementation would keep chunk size constant. Large carry reply makes the
// shrinkage observable.
//
// THE WINDOW IS 600, NOT 300, and the difference is not cosmetic. The carry
// this fake returns is 500 characters; under the C8 estimator that plus the
// ack and the 500-word instruction frames to ~277 tokens, so a 300-token
// window leaves chunkBudgetFor returning a NEGATIVE budget on the very first
// carry and RunSummary correctly refuses with ErrNoWindowRoom before a second
// chunk exists. That refusal is right — a window that cannot hold its own
// framing cannot make progress, and TestRunSummary_CarryOverflowReturnsError
// covers it deliberately — but it means the shrinkage this test is here to
// observe never gets a chance to happen. 600 leaves ~290 tokens of chunk
// budget on the first pass, which is several messages' worth and shrinks
// visibly as the carry grows.
func TestRunSummary_ChunkBudgetShrinksAsCarryGrows(t *testing.T) {
	rm := &recordingModel{reply: strings.Repeat("y", 500)}
	big := []*schema.Message{}
	for i := 0; i < 40; i++ {
		big = append(big, &schema.Message{Role: schema.User, Content: strings.Repeat("x", 200)})
	}
	_, err := ctxcompact.RunSummary(context.Background(), big, ctxcompact.RunOpts{ModelWindow: 600, ChunkThreshold: 0.9, SummaryWordLimit: 500}, rm, nil)
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(rm.inputs), 3, "multiple chunks")
	// Strip framing (carry+ack prefix on later chunks, instruction suffix on all)
	// to measure JUST the original-message chunk each call summarized. A fixed-
	// overhead implementation would keep this constant; dynamic budget shrinks it
	// as carry grows. (Total input stays near the window in both schemes, so
	// measuring the whole input would not distinguish them.)
	chunkOnly := func(in []*schema.Message) []*schema.Message {
		start := 0
		if len(in) > 0 && in[0] != nil && strings.HasPrefix(in[0].Content, ctxcompact.SummarySentinel) {
			start = 2 // skip [carry, ack]
		}
		return in[start : len(in)-1] // drop trailing instruction
	}
	tok1Chunk := ctxcompact.EstimateTokens(chunkOnly(rm.inputs[0]))
	tokLastChunk := ctxcompact.EstimateTokens(chunkOnly(rm.inputs[len(rm.inputs)-1]))
	assert.Less(t, tokLastChunk, tok1Chunk, "chunk portion shrinks as carry grows (dynamic budget)")
}

// I-3: when the window is too small for instruction+carry, chunkBudget <= 0
// and RunSummary returns an error instead of silently over-running.
func TestRunSummary_CarryOverflowReturnsError(t *testing.T) {
	rm := &recordingModel{reply: "s"}
	big := []*schema.Message{}
	for i := 0; i < 10; i++ {
		big = append(big, &schema.Message{Role: schema.User, Content: strings.Repeat("x", 200)})
	}
	_, err := ctxcompact.RunSummary(context.Background(), big, ctxcompact.RunOpts{ModelWindow: 50, ChunkThreshold: 0.9}, rm, nil)
	assert.Error(t, err, "window too small for instruction+carry -> error, not silent overrun")
}

// TestRunSummary_EmptyMessages covers the empty-msgs early return.
func TestRunSummary_EmptyMessages(t *testing.T) {
	fm := einollm.NewFakeModel([]string{"unused"}, nil)
	s, err := ctxcompact.RunSummary(context.Background(), nil,
		ctxcompact.RunOpts{ModelWindow: 10000, ChunkThreshold: 0.9}, fm, nil)
	require.NoError(t, err)
	assert.Empty(t, s, "empty messages produce empty summary")
}

// TestRunSummary_ModelWindowZero covers the singleBudget=0 path (carry path).
// With ModelWindow=0 and ChunkThreshold=0.9, singleBudget returns 0, so
// the carry path is taken. chunkBudgetFor also returns 0 -> error.
func TestRunSummary_ModelWindowZero(t *testing.T) {
	fm := einollm.NewFakeModel([]string{"summary"}, nil)
	_, err := ctxcompact.RunSummary(context.Background(), []*schema.Message{{Role: schema.User, Content: "x"}},
		ctxcompact.RunOpts{ModelWindow: 0, ChunkThreshold: 0.9}, fm, nil)
	assert.Error(t, err, "ModelWindow=0 should produce an error from the carry path")
}

// TestRunSummary_ChunkThresholdZero covers the single path where ChunkThreshold
// <= 0 means use the full ModelWindow as single budget.
func TestRunSummary_ChunkThresholdZero(t *testing.T) {
	fm := einollm.NewFakeModel(nil, nil)
	fm.Echo = true
	msgs := []*schema.Message{
		{Role: schema.User, Content: "hello"},
		{Role: schema.Assistant, Content: "world"},
	}
	summary, err := ctxcompact.RunSummary(context.Background(), msgs,
		ctxcompact.RunOpts{ModelWindow: 10000, ChunkThreshold: 0, SummaryWordLimit: 200}, fm, nil)
	require.NoError(t, err)
	assert.Contains(t, summary, "hello")
	assert.Contains(t, summary, "world")
}

// TestSingleSummary_WithWordLimit tests the single summary path with word limit.
func TestSingleSummary_WithWordLimit(t *testing.T) {
	fm := einollm.NewFakeModel(nil, nil)
	fm.Echo = true
	msgs := []*schema.Message{
		{Role: schema.User, Content: "task"},
		{Role: schema.Assistant, Content: strings.Repeat("a", 200)},
	}
	summary, err := ctxcompact.RunSummary(context.Background(), msgs,
		ctxcompact.RunOpts{ModelWindow: 10000, ChunkThreshold: 0.9, SummaryWordLimit: 100}, fm, nil)
	require.NoError(t, err)
	assert.Contains(t, summary, "task")
}

// TestRunSummary_OnChunkCallback verifies the onChunk callback fires during Stream.
func TestRunSummary_OnChunkCallback(t *testing.T) {
	fm := einollm.NewFakeModel([]string{"the summary content"}, nil)
	got := ""
	msgs := []*schema.Message{
		{Role: schema.User, Content: "task"},
		{Role: schema.Assistant, Content: strings.Repeat("x", 200)},
		{Role: schema.User, Content: "recent"},
	}
	summary, err := ctxcompact.RunSummary(context.Background(), msgs,
		ctxcompact.RunOpts{ModelWindow: 10000, ChunkThreshold: 0.9, SummaryWordLimit: 200}, fm, func(s string) { got += s })
	require.NoError(t, err)
	assert.Equal(t, "the summary content", got, "onChunk receives model output")
	assert.Equal(t, "the summary content", summary)
}

// recordingModel records each call's input messages and returns a fixed reply.
var _ ctxcompact.ModelSummarizer = (*recordingModel)(nil)

type recordingModel struct {
	inputs [][]*schema.Message
	reply  string
}

func (r *recordingModel) Generate(_ context.Context, in []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	cp := make([]*schema.Message, len(in))
	copy(cp, in)
	r.inputs = append(r.inputs, cp)
	return &schema.Message{Role: schema.Assistant, Content: r.reply}, nil
}

func (r *recordingModel) Stream(ctx context.Context, in []*schema.Message, _ ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	r.Generate(ctx, in)
	return schema.StreamReaderFromArray[*schema.Message]([]*schema.Message{{Role: schema.Assistant, Content: r.reply}}), nil
}
