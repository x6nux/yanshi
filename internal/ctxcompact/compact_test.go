// internal/ctxcompact/compact_test.go
package ctxcompact_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	einollm "github.com/x6nux/yanshi/internal/llm/eino"
	"github.com/x6nux/yanshi/internal/ctxcompact"
)

// longMsgs builds n user messages of `chars` each — used for gate tests where
// pin policy doesn't matter (the gate fires before Plan runs).
func longMsgs(n, chars int) []*schema.Message {
	out := make([]*schema.Message, n)
	for i := 0; i < n; i++ {
		out[i] = &schema.Message{Role: schema.User, Content: strings.Repeat("a", chars)}
	}
	return out
}

func TestMaybeCompact_OverThreshold(t *testing.T) {
	fm := einollm.NewFakeModel([]string{"SUMMARY"}, nil)
	// user msgs pin verbatim (isUserOriginal); assistant noise gets summarized.
	msgs := []*schema.Message{
		{Role: schema.User, Content: "task"},
		{Role: schema.Assistant, Content: strings.Repeat("a", 800)},
		{Role: schema.Assistant, Content: strings.Repeat("a", 800)},
		{Role: schema.Assistant, Content: strings.Repeat("a", 800)},
		{Role: schema.User, Content: "recent"},
		{Role: schema.Assistant, Content: "reply"},
	}
	chunks := 0
	out, before, after, did := ctxcompact.MaybeCompact(context.Background(), msgs,
		0.8, 800, 1, fm, func(string) { chunks++ })
	require.True(t, did)
	assert.Less(t, len(out), len(msgs))
	assert.Greater(t, before, after)
	// bug④: summary is user+sentinel at TAIL (not system at head)
	assert.True(t, ctxcompact.IsSummaryMessage(out[len(out)-1]))
	assert.Contains(t, out[len(out)-1].Content, "SUMMARY")
	assert.GreaterOrEqual(t, chunks, 1)
}

func TestMaybeCompact_UnderThreshold(t *testing.T) {
	fm := einollm.NewFakeModel([]string{"SUMMARY"}, nil)
	msgs := longMsgs(3, 8)
	out, before, after, did := ctxcompact.MaybeCompact(context.Background(), msgs,
		0.8, 100000, 2, fm, func(string) {})
	assert.False(t, did)
	assert.Equal(t, len(msgs), len(out))
	assert.Equal(t, before, after)
}

func TestMaybeCompact_DisabledThreshold(t *testing.T) {
	fm := einollm.NewFakeModel([]string{"SUMMARY"}, nil)
	_, _, _, did := ctxcompact.MaybeCompact(context.Background(), longMsgs(10, 200),
		0, 100, 2, fm, func(string) {})
	assert.False(t, did)
}

func TestMaybeCompact_TooFewMessages(t *testing.T) {
	fm := einollm.NewFakeModel([]string{"SUMMARY"}, nil)
	out, _, _, did := ctxcompact.MaybeCompact(context.Background(), longMsgs(5, 200),
		0.8, 100, 4, fm, func(string) {})
	assert.False(t, did)
	assert.Equal(t, 5, len(out))
}

func TestMaybeCompact_PreservesToolCalls(t *testing.T) {
	// bug①/② integration: pinned tool_call/tool_result must survive compaction
	// VERBATIM (not flattened into the summary). 6 msgs + kr=2 so the too-few
	// gate (len <= kr*2+1 = 5) does NOT fire and the tool pair at {3,4} lands in
	// the pinned tail. cw=1000 lets the single summary path fit while the
	// assistant noise at index 1 (3000 chars ~ 750 tok) pushes before-tokens
	// over the 0.8*1000 budget so compaction actually runs.
	fm := einollm.NewFakeModel([]string{"summary"}, nil)
	msgs := []*schema.Message{
		{Role: schema.User, Content: "edit internal/ctxcompact/compact.go"}, // 0 ws (pin)
		{Role: schema.Assistant, Content: strings.Repeat("x", 3000)},       // 1 summarize
		{Role: schema.Assistant, Content: "noise"},                         // 2 tail
		{Role: schema.Assistant, ToolCalls: []schema.ToolCall{              // 3 tail tool_call
			{ID: "c1", Function: schema.FunctionCall{Name: "edit_file", Arguments: `{"path":"internal/ctxcompact/compact.go"}`}},
		}},
		{Role: schema.Tool, ToolCallID: "c1", Content: "edited"}, // 4 tail tool_result
		{Role: schema.User, Content: "recent"},                   // 5 tail
	}
	out, _, _, did := ctxcompact.MaybeCompact(context.Background(), msgs, 0.8, 1000, 2, fm, func(string) {})
	require.True(t, did, "compaction must run (over budget, enough msgs)")
	var sawCall, sawResult bool
	for _, m := range out {
		if len(m.ToolCalls) > 0 {
			sawCall = true
		}
		if m.ToolCallID == "c1" {
			sawResult = true
		}
	}
	assert.True(t, sawCall, "tool_call preserved verbatim through compaction")
	assert.True(t, sawResult, "tool_result preserved verbatim through compaction")
}

// TestForceCompact_SkipsThresholdGate verifies that ForceCompact fires even
// when the history is well under what MaybeCompact's threshold gate would
// require (here: 6 messages at ~3 tokens each, vs. a 100000-token window —
// MaybeCompact would skip this; ForceCompact must still run). The
// too-few-messages guard and the did-means-actual-shrink check stay in force,
// so a tiny history no-ops (TestForceCompact_TooFewMessages) and a no-shrink
// summary no-ops (TestForceCompact_NoShrink).
func TestForceCompact_SkipsThresholdGate(t *testing.T) {
	fm := einollm.NewFakeModel([]string{"SUMMARY"}, nil)
	msgs := []*schema.Message{
		{Role: schema.User, Content: "task"},
		{Role: schema.Assistant, Content: strings.Repeat("a", 800)},
		{Role: schema.Assistant, Content: strings.Repeat("a", 800)},
		{Role: schema.Assistant, Content: strings.Repeat("a", 800)},
		{Role: schema.User, Content: "recent"},
		{Role: schema.Assistant, Content: "reply"},
	}
	// Same args as MaybeCompact except NO threshold: keepRecent=1 pins the last
	// pair, summarize the rest. The history is far below 0.8*100000 — MaybeCompact
	// would noop; ForceCompact must compact.
	out, before, after, did := ctxcompact.ForceCompact(context.Background(), msgs,
		100000, 1, fm, func(string) {})
	require.True(t, did, "ForceCompact fires regardless of the threshold gate")
	assert.Less(t, len(out), len(msgs), "summary replaces the older turns")
	assert.Greater(t, before, after, "compaction must actually shrink the estimate")
	assert.True(t, ctxcompact.IsSummaryMessage(out[len(out)-1]),
		"summary is a sentinel-tagged user-role message at the tail")
}

// TestForceCompact_TooFewMessages locks the guard that ForceCompact KEEPS:
// a sub-keepRecent*2+1 history short-circuits even on a forced compact, so a
// fresh session's /compact does not summarize into nothing.
func TestForceCompact_TooFewMessages(t *testing.T) {
	fm := einollm.NewFakeModel([]string{"SUMMARY"}, nil)
	msgs := longMsgs(3, 200) // 3 messages, keepRecent=4 → guard at 9, fires
	out, _, _, did := ctxcompact.ForceCompact(context.Background(), msgs,
		100000, 4, fm, func(string) {})
	assert.False(t, did, "too-few-messages guard must short-circuit ForceCompact")
	assert.Equal(t, len(msgs), len(out), "input returned unchanged")
	assert.Equal(t, 0, fm.StreamCalls, "no model call wasted on a no-op compact")
}

// TestForceCompact_NoShrink verifies the did-means-actual-shrink check: when
// Plan pins everything verbatim (so TokensAfter == TokensBefore), ForceCompact
// reports did=false so the caller does not emit a misleading "compacted"
// status. Construct the no-shrink case with a single-call history that Plan
// pins entirely (len <= keepRecent*2+1 trips the too-few guard first, so use a
// history just over the guard with everything in the pinned tail).
func TestForceCompact_NoShrink(t *testing.T) {
	// keepRecent=4 pins the last 8 messages; a 9-message history is just over
	// the guard (len > 9? no — len <= 9 trips it). Use 11 so Plan runs but with
	// KeepRecent=5 it pins the last 10, leaving only index 0 to summarize —
	// which still shrinks. Instead, force no-shrink by making every message a
	// user message (Plan pins user originals verbatim), so SummarizeIndices is
	// empty and Run returns TokensAfter == TokensBefore.
	fm := einollm.NewFakeModel([]string{"SUMMARY"}, nil)
	msgs := longMsgs(11, 200) // all user — Plan pins all as user originals
	out, before, after, did := ctxcompact.ForceCompact(context.Background(), msgs,
		100000, 1, fm, func(string) {})
	assert.False(t, did, "no-shrink result must report did=false")
	assert.Equal(t, before, after, "tokens unchanged when nothing was actually summarized")
	assert.Equal(t, len(msgs), len(out), "input returned verbatim")
}

// TestMaybeCompact_ModelError covers the error path where Run fails and
// MaybeCompact returns did=false with original messages unchanged.
func TestMaybeCompact_ModelError(t *testing.T) {
	errModel := einollm.NewFakeModel(nil, assert.AnError)
	msgs := []*schema.Message{
		{Role: schema.User, Content: "task"},
		{Role: schema.Assistant, Content: strings.Repeat("a", 2000)},
		{Role: schema.User, Content: "recent"},
		{Role: schema.Assistant, Content: "reply"},
	}
	// cw=250 means 0.8*250=200 threshold — the ~500-token history exceeds it,
	// so MaybeCompact enters Run, which calls the error model, hitting err!=nil.
	out, before, after, did := ctxcompact.MaybeCompact(context.Background(), msgs,
		0.8, 250, 1, errModel, func(string) {})
	assert.False(t, did, "model error -> no compaction")
	assert.Equal(t, len(msgs), len(out), "original messages returned unchanged")
	assert.Equal(t, before, after)
}

// TestMaybeCompact_NoShrink covers the TokensAfter>=TokensBefore noop path
// when all messages are user-original (Plan pins everything).
func TestMaybeCompact_NoShrink(t *testing.T) {
	fm := einollm.NewFakeModel([]string{"summary"}, nil)
	msgs := longMsgs(11, 200) // all user -> Plan pins all via isUserOriginal
	out, before, after, did := ctxcompact.MaybeCompact(context.Background(), msgs,
		0.8, 100000, 1, fm, func(string) {})
	assert.False(t, did, "no shrink -> did=false")
	assert.Equal(t, len(msgs), len(out), "original messages intact")
	assert.Equal(t, before, after)
}

// TestForceCompact_ErrorPath covers the error path in ForceCompact.
func TestForceCompact_ErrorPath(t *testing.T) {
	errModel := einollm.NewFakeModel(nil, assert.AnError)
	msgs := []*schema.Message{
		{Role: schema.User, Content: "task"},
		{Role: schema.Assistant, Content: strings.Repeat("a", 800)},
		{Role: schema.User, Content: "recent"},
		{Role: schema.Assistant, Content: "reply"},
	}
	out, before, after, did := ctxcompact.ForceCompact(context.Background(), msgs,
		100000, 1, errModel, func(string) {})
	assert.False(t, did, "model error -> did=false")
	assert.Equal(t, len(msgs), len(out), "original messages intact")
	assert.Equal(t, before, after)
}

// TestMaybeCompact_ZeroContextWindow covers the contextWindow <= 0 gate.
func TestMaybeCompact_ZeroContextWindow(t *testing.T) {
	fm := einollm.NewFakeModel([]string{"SUMMARY"}, nil)
	_, _, _, did := ctxcompact.MaybeCompact(context.Background(), longMsgs(10, 200),
		0.8, 0, 2, fm, func(string) {})
	assert.False(t, did, "zero contextWindow -> no compaction")
}

// --- Custom model helpers for callWithRetry and Generate fallback tests ---

// streamFailModel returns an error from Stream but succeeds on Generate.
// This exercises the streamOnce Generate fallback path.
type streamFailModel struct {
	reply string
}

func (m *streamFailModel) Stream(_ context.Context, _ []*schema.Message, _ ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	return nil, assert.AnError
}
func (m *streamFailModel) Generate(_ context.Context, _ []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	return &schema.Message{Role: schema.Assistant, Content: m.reply}, nil
}

var _ ctxcompact.ModelSummarizer = (*streamFailModel)(nil)

// TestRunSummary_GenerateFallback covers streamOnce falling back to Generate
// when Stream returns an error. This exercises the Generate fallback branch.
func TestRunSummary_GenerateFallback(t *testing.T) {
	m := &streamFailModel{reply: "fallback-summary"}
	msgs := []*schema.Message{
		{Role: schema.User, Content: "task"},
		{Role: schema.Assistant, Content: "noise"},
	}
	summary, err := ctxcompact.RunSummary(context.Background(), msgs,
		ctxcompact.RunOpts{ModelWindow: 10000, ChunkThreshold: 0.9, SummaryWordLimit: 200}, m, nil)
	require.NoError(t, err, "Generate fallback should succeed")
	assert.Equal(t, "fallback-summary", summary)
}

// TestRunSummary_GenerateFallbackTrigger covers the triggered fallback path
// where Stream fails and Generate succeeds. Also tests onChunk in fallback.
func TestRunSummary_GenerateFallbackTrigger(t *testing.T) {
	m := &streamFailModel{reply: "chunked-fallback"}
	var chunk string
	msgs := []*schema.Message{
		{Role: schema.User, Content: "task"},
		{Role: schema.Assistant, Content: "response"},
	}
	summary, err := ctxcompact.RunSummary(context.Background(), msgs,
		ctxcompact.RunOpts{ModelWindow: 10000, ChunkThreshold: 0.9, SummaryWordLimit: 200}, m, func(s string) { chunk += s })
	require.NoError(t, err)
	assert.Equal(t, "chunked-fallback", summary)
	assert.Equal(t, "chunked-fallback", chunk, "onChunk receives Generate output")
}

// TestRunSummary_TransientRetry exercises the callWithRetry retry path.
// The model fails on BOTH Stream and Generate for the first errCalls
// attempt-pairs (each attempt = Stream fallthrough + Generate fallback),
// then succeeds. This forces callWithRetry into its retry loop.
func TestRunSummary_TransientRetry(t *testing.T) {
	m := &timeoutThenOkModel{errCalls: 2, reply: "retried-summary"}
	msgs := []*schema.Message{
		{Role: schema.User, Content: strings.Repeat("x", 200)},
		{Role: schema.Assistant, Content: strings.Repeat("y", 200)},
	}
	summary, err := ctxcompact.RunSummary(context.Background(), msgs,
		ctxcompact.RunOpts{ModelWindow: 10000, ChunkThreshold: 0.9, SummaryWordLimit: 200}, m, nil)
	require.NoError(t, err, "transient error should be retried")
	assert.Equal(t, "retried-summary", summary)
}

// TestRunSummary_TransientExhausted exercises the retry-exhaustion path
// where the model fails on ALL attempts (both Stream and Generate).
func TestRunSummary_TransientExhausted(t *testing.T) {
	m := &timeoutThenOkModel{errCalls: 99, reply: "never"}
	msgs := []*schema.Message{
		{Role: schema.User, Content: "task"},
		{Role: schema.Assistant, Content: "response"},
	}
	_, err := ctxcompact.RunSummary(context.Background(), msgs,
		ctxcompact.RunOpts{ModelWindow: 10000, ChunkThreshold: 0.9, SummaryWordLimit: 200}, m, nil)
	require.Error(t, err, "should exhaust retries")
}

// TestCallWithRetry_TransientErrorSuccess tests callWithRetry with a transient
// error that eventually succeeds after retries.
func TestCallWithRetry_TransientErrorSuccess(t *testing.T) {
	m := &timeoutThenOkModel{errCalls: 1, reply: "success-after-retry"}
	msgs := []*schema.Message{
		{Role: schema.User, Content: "task"},
		{Role: schema.Assistant, Content: "response"},
	}
	summary, err := ctxcompact.RunSummary(context.Background(), msgs,
		ctxcompact.RunOpts{ModelWindow: 10000, ChunkThreshold: 0.9, SummaryWordLimit: 200}, m, nil)
	require.NoError(t, err, "transient error should be retried")
	assert.Equal(t, "success-after-retry", summary)
}

// timeoutThenOkModel fails on BOTH Stream AND Generate for the first
// errCalls attempt-pairs. An attempt-pair means Stream then Generate
// as fallback — callWithRetry only enters its retry loop when streamOnce
// (which tries both) returns an error. Each retry attempt by callWithRetry
// calls streamOnce again, which calls Stream, then Generate on failure.
// We count the attempt number via pairCount: each pair is Stream+Generate
// and both fail until pairStr > errCalls.
type timeoutThenOkModel struct {
	errCalls int
	reply    string
	pairNum  int // incremented per Stream call (start of each attempt-pair)
}

func (m *timeoutThenOkModel) Stream(_ context.Context, _ []*schema.Message, _ ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	m.pairNum++
	if m.pairNum <= m.errCalls {
		return nil, errTimeout
	}
	return schema.StreamReaderFromArray([]*schema.Message{{Role: schema.Assistant, Content: m.reply}}), nil
}
func (m *timeoutThenOkModel) Generate(_ context.Context, _ []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	if m.pairNum <= m.errCalls {
		return nil, errTimeout
	}
	return &schema.Message{Role: schema.Assistant, Content: m.reply}, nil
}

var _ ctxcompact.ModelSummarizer = (*timeoutThenOkModel)(nil)

// TestRunSummary_StreamRecvError covers the recvErr path in streamOnce — when
// Stream returns a valid reader but Recv returns an error.
func TestRunSummary_StreamRecvError(t *testing.T) {
	sr, sw := schema.Pipe[*schema.Message](1)
	sw.Send(nil, errors.New("stream recv error"))
	sw.Close()

	m := &pipeStreamModel{reader: sr}
	_, err := ctxcompact.RunSummary(context.Background(),
		[]*schema.Message{{Role: schema.User, Content: "x"}},
		ctxcompact.RunOpts{ModelWindow: 10000, ChunkThreshold: 0.9}, m, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "stream recv error")
}

// pipeStreamModel returns a pre-built stream reader from Pipe, used to inject
// errors into the Recv path of streamOnce.
type pipeStreamModel struct {
	reader *schema.StreamReader[*schema.Message]
}

func (m *pipeStreamModel) Stream(_ context.Context, _ []*schema.Message, _ ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	return m.reader, nil
}
func (m *pipeStreamModel) Generate(_ context.Context, _ []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	return nil, errors.New("generate fails")
}

var _ ctxcompact.ModelSummarizer = (*pipeStreamModel)(nil)

// TestRunSummary_ContextCancelledDuringRetry covers the ctx.Done path in
// callWithRetry's retry backoff select. The model returns transient errors
// on every call; the context is cancelled during the backoff window.
func TestRunSummary_ContextCancelledDuringRetry(t *testing.T) {
	m := &timeoutThenOkModel{errCalls: 99, reply: "never"}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Cancel after a short delay so that the first call fails with a
	// transient timeout, then the context is cancelled during the
	// 1-second retry backoff, hitting the ctx.Done() branch.
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	_, err := ctxcompact.RunSummary(ctx,
		[]*schema.Message{{Role: schema.User, Content: "x"}},
		ctxcompact.RunOpts{ModelWindow: 10000, ChunkThreshold: 0.9}, m, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "context canceled")
}

// errTimeout is a transient error containing the "timeout" keyword.
var errTimeout = &timeoutErr{}

type timeoutErr struct{}

func (e *timeoutErr) Error() string { return "request timeout" }
func (e *timeoutErr) Unwrap() error { return nil }
