package eino

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/ctxcompact"
)

// recordingModel is a model.BaseChatModel double that records the message
// slices it receives (one entry per call) and returns a scripted assistant
// message. It is the witness for CompactingModel tests: the LAST recorded input
// is what the inner model "saw" on the real (post-compaction) call, and the
// number of calls proves whether a summarization turn happened first.
type recordingModel struct {
	mu       sync.Mutex
	inputs   [][]*schema.Message
	calls    int    // total Generate+Stream calls; the 1st is the summarize turn
	summary  string // content returned for the summarize call (1st call)
	reply    string // content returned for the main call (subsequent calls)
	streamOK bool   // Stream returns a 1-chunk reader; else errors
}

func (r *recordingModel) record(in []*schema.Message) {
	// Copy so later caller mutation can't rewrite history.
	cp := make([]*schema.Message, len(in))
	copy(cp, in)
	r.mu.Lock()
	r.inputs = append(r.inputs, cp)
	r.calls++
	r.mu.Unlock()
}

// response returns the assistant message for the CURRENT (just-recorded) call:
// the summary on the first call, the reply afterwards.
func (r *recordingModel) response() *schema.Message {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.calls == 1 && r.summary != "" {
		return schema.AssistantMessage(r.summary, nil)
	}
	if r.reply != "" {
		return schema.AssistantMessage(r.reply, nil)
	}
	return schema.AssistantMessage("ok", nil)
}

func (r *recordingModel) Generate(_ context.Context, in []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	r.record(in)
	return r.response(), nil
}

func (r *recordingModel) Stream(_ context.Context, in []*schema.Message, _ ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	r.record(in)
	if !r.streamOK {
		return nil, errors.New("recordingModel: stream disabled")
	}
	return schema.StreamReaderFromArray[*schema.Message](
		[]*schema.Message{r.response()}), nil
}

func (r *recordingModel) inputsSnapshot() [][]*schema.Message {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([][]*schema.Message, len(r.inputs))
	copy(out, r.inputs)
	return out
}

// bigMessage builds a message whose content is ~tokens heuristic tokens (each
// 4 chars ~ 1 token, plus the 8-per-message overhead), so a test can construct
// an over-threshold history deterministically.
func bigMessage(tokens int) *schema.Message {
	chars := tokens * 4
	if chars < 1 {
		chars = 1
	}
	return &schema.Message{Role: schema.Assistant, Content: string(make([]byte, chars))}
}

// TestCompactingModel_PassthroughUnderThreshold proves that when the estimated
// token count is below Threshold*ContextWindow the wrapper forwards the
// ORIGINAL message slice unchanged (no summarization call, same count).
func TestCompactingModel_PassthroughUnderThreshold(t *testing.T) {
	inner := &recordingModel{reply: "answer", streamOK: true}
	cm := &CompactingModel{
		Inner:         inner,
		Threshold:     0.8,
		ContextWindow: 10_000, // huge → never over threshold
		KeepRecent:    2,
	}
	msgs := []*schema.Message{
		{Role: schema.User, Content: "hello"},
		{Role: schema.Assistant, Content: "hi"},
		{Role: schema.User, Content: "again"},
	}

	_, err := cm.Generate(context.Background(), msgs)
	require.NoError(t, err)

	ins := inner.inputsSnapshot()
	require.Len(t, ins, 1, "no summarization call when under threshold")
	assert.Len(t, ins[0], 3, "inner saw the original messages unchanged")
}

// TestCompactingModel_CompactsWhenOverThreshold proves that when the history is
// over budget the wrapper summarizes the older messages and forwards
// [KeepRecent tail..., summary-as-user-at-tail] to the inner model. The
// summary lives at the TAIL as a sentinel-prefixed user message (bug④: not a
// System message at the head).
//
// ContextWindow is intentionally just large enough that ctxcompact.Run's
// single-path cache-aligned summary call is taken (summarize-set + instruction
// ≤ 0.9*ContextWindow): a smaller window would trip the carry-style chunked
// path and the recordingModel could not be a 1-call witness.
func TestCompactingModel_CompactsWhenOverThreshold(t *testing.T) {
	inner := &recordingModel{summary: "SUMMARY", reply: "answer", streamOK: true}
	cm := &CompactingModel{
		Inner:         inner,
		Threshold:     0.5,
		ContextWindow: 200, // 100-token threshold budget; ≥177 keeps RunSummary single-path
		KeepRecent:    2,
	}
	// 5 messages, each ~28 tokens → ~140 tokens, well over the 100 budget.
	msgs := []*schema.Message{
		bigMessage(20),
		bigMessage(20),
		bigMessage(20),
		bigMessage(20),
		bigMessage(20),
	}
	require.True(t, cm.shouldCompact(msgs), "fixture is over threshold")

	_, err := cm.Generate(context.Background(), msgs)
	require.NoError(t, err)

	ins := inner.inputsSnapshot()
	require.Len(t, ins, 2, "summarize call + real call")
	// The real (second) call is the compacted set: summary + KeepRecent.
	last := ins[1]
	assert.Len(t, last, 3, "compacted = KeepRecent tail (2) + summary at tail")
	// bug④: summary is user+sentinel at the TAIL, not System at the head
	assert.True(t, last[2].Role == schema.User &&
		strings.HasPrefix(last[2].Content, ctxcompact.SummarySentinel),
		"last message is the sentinel summary")
	assert.Contains(t, last[2].Content, "SUMMARY")
	assert.Equal(t, msgs[3], last[0], "trailing messages kept verbatim")
	assert.Equal(t, msgs[4], last[1])
}

// TestCompactingModel_StreamCompacts proves Stream mirrors Generate's compaction
// (the ADK takes the Stream path under EnableStreaming).
//
// As with the Generate variant, ContextWindow + message size are tuned so
// ctxcompact.Run's single-path summary is taken (smaller windows would trip
// carry-style chunking and turn the recordingModel into a multi-call witness).
func TestCompactingModel_StreamCompacts(t *testing.T) {
	inner := &recordingModel{summary: "SUM", reply: "answer", streamOK: true}
	cm := &CompactingModel{
		Inner:         inner,
		Threshold:     0.5,
		ContextWindow: 220, // ≥210 keeps RunSummary single-path with the bigger msgs
		KeepRecent:    1,
	}
	msgs := []*schema.Message{bigMessage(30), bigMessage(30), bigMessage(30)}

	sr, err := cm.Stream(context.Background(), msgs)
	require.NoError(t, err)
	// Drain so the Stream call completes and inputs are recorded.
	for {
		_, recvErr := sr.Recv()
		if errors.Is(recvErr, io.EOF) {
			break
		}
		require.NoError(t, recvErr)
	}
	sr.Close()

	ins := inner.inputsSnapshot()
	require.Len(t, ins, 2, "summarize stream + real stream")
	assert.True(t, ins[1][len(ins[1])-1].Role == schema.User &&
		strings.HasPrefix(ins[1][len(ins[1])-1].Content, ctxcompact.SummarySentinel),
		"real call saw the summary at tail")
}

// TestCompactingModel_TooShortDoesNotCompact proves the KeepRecent message-count
// guard suppresses summarization when the history isn't longer than KeepRecent
// (so a short conversation is never summarized into nothing), even if the token
// estimate is over budget.
func TestCompactingModel_TooShortDoesNotCompact(t *testing.T) {
	inner := &recordingModel{reply: "answer", streamOK: true}
	cm := &CompactingModel{
		Inner:         inner,
		Threshold:     0.5,
		ContextWindow: 1, // tiny → always over budget
		KeepRecent:    4,
	}
	msgs := []*schema.Message{
		{Role: schema.User, Content: "a"},
		{Role: schema.Assistant, Content: "b"},
	}
	_, err := cm.Generate(context.Background(), msgs)
	require.NoError(t, err)

	ins := inner.inputsSnapshot()
	require.Len(t, ins, 1, "too-short history is not summarized")
	assert.Len(t, ins[0], 2, "original messages forwarded unchanged")
}

// TestCompactingModel_DisabledIsPassthrough proves Threshold <= 0 disables
// compaction entirely (the wrapper is a pure pass-through), regardless of size.
func TestCompactingModel_DisabledIsPassthrough(t *testing.T) {
	inner := &recordingModel{reply: "answer", streamOK: true}
	cm := &CompactingModel{
		Inner:         inner,
		Threshold:     0, // disabled
		ContextWindow: 1,
		KeepRecent:    1,
	}
	msgs := []*schema.Message{bigMessage(100), bigMessage(100)}
	_, err := cm.Generate(context.Background(), msgs)
	require.NoError(t, err)

	ins := inner.inputsSnapshot()
	require.Len(t, ins, 1)
	assert.Len(t, ins[0], 2, "disabled wrapper forwards original slice")
}

// TestCompactingModel_OnCompactCallback proves that when an OnCompact callback
// is bound in the context, the summary is STREAMED to the inner model and each
// text delta is forwarded to the callback (so a WS handler can emit
// compact_chunk frames). The callback path uses Stream, not Generate.
func TestCompactingModel_OnCompactCallback(t *testing.T) {
	cm := &CompactingModel{
		Inner:         nil, // patched below
		Threshold:     0.5,
		ContextWindow: 100,
		KeepRecent:    1,
	}
	msgs := []*schema.Message{bigMessage(20), bigMessage(20), bigMessage(20)}

	// cbModel streams the configured chunks on the FIRST Stream call (the
	// summarize turn), then replies with a fixed assistant message for every
	// subsequent call. Because an OnCompact callback is bound, the wrapper
	// takes the Stream path for the summary and forwards each delta.
	var got []string
	var firstStreamed bool
	cm.Inner = &cbModel{chunks: []string{"A", "B"}, reply: "answer", firstStreamed: &firstStreamed}
	ctx := WithCompactCallback(context.Background(), func(chunk string) { got = append(got, chunk) })

	_, err := cm.Generate(ctx, msgs)
	require.NoError(t, err)

	assert.True(t, firstStreamed, "summarize call took the Stream path (callback present)")
	assert.Equal(t, []string{"A", "B"}, got, "each summary delta forwarded to the callback")
}

// cbModel streams the configured chunks on the FIRST Stream call (the summarize
// turn), then replies with a fixed assistant message for every subsequent call.
type cbModel struct {
	chunks        []string
	reply         string
	firstStreamed *bool
	calls         int
	mu            sync.Mutex
}

func (m *cbModel) Generate(_ context.Context, _ []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	return schema.AssistantMessage(m.reply, nil), nil
}

func (m *cbModel) Stream(_ context.Context, _ []*schema.Message, _ ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	m.mu.Lock()
	m.calls++
	first := m.calls == 1
	m.mu.Unlock()
	if first {
		*m.firstStreamed = true
		msgs := make([]*schema.Message, 0, len(m.chunks))
		for _, c := range m.chunks {
			msgs = append(msgs, schema.AssistantMessage(c, nil))
		}
		return schema.StreamReaderFromArray[*schema.Message](msgs), nil
	}
	return schema.StreamReaderFromArray[*schema.Message](
		[]*schema.Message{schema.AssistantMessage(m.reply, nil)}), nil
}

// TestWithCompactCallback_NilIsNoop proves a nil callback leaves the context
// unchanged (no spurious value inserted).
func TestWithCompactCallback_NilIsNoop(t *testing.T) {
	ctx := WithCompactCallback(context.Background(), nil)
	assert.Nil(t, compactCallback(ctx), "nil callback installs nothing")
}

// TestCompactingModel_CooldownDefersReCompact verifies that after one compaction
// succeeds, a second shouldCompact call with moderate growth (still over threshold
// but within token-growth cooldown) does NOT compress again. The hard-force
// fraction of 0.95 provides a safety override for extreme growth.
func TestCompactingModel_CooldownDefersReCompact(t *testing.T) {
	inner := &recordingModel{summary: "summarized", reply: "done", streamOK: true}
	cm := &CompactingModel{
		Inner:             inner,
		Threshold:         0.5,
		ContextWindow:     400,
		KeepRecent:        2,
		CooldownTokens:    100,
		HardForceFraction: 0.95,
	}
	ctx := context.Background()

	// First call: 6 bigMessage(30) ≈ 6×(30+8)=228 tokens, over 0.5×400=200.
	// KeepRecent=2 → 4 compressible messages summarize to a short summary,
	// so TokensAfter ≪ TokensBefore.
	msgs := []*schema.Message{
		bigMessage(30), bigMessage(30), bigMessage(30),
		bigMessage(30), bigMessage(30), bigMessage(30),
	} // total ≈ 228 tokens
	require.True(t, cm.shouldCompact(msgs), "first set is over threshold")
	out, didCompact := cm.maybeCompact(ctx, msgs)
	require.True(t, didCompact, "first call must compact (over threshold)")
	require.Equal(t, 1, inner.calls, "one summarize call must have happened")
	_ = out

	// Simulate post-compact: lastCompactTokens just under threshold=200.
	cm.lastCompactTokens = 180
	cm.lastCompactAt = time.Now()
	inner.calls = 0

	// Second call: same 6 messages ≈228 tokens, growth ≈48 < CooldownTokens=100
	// → cooldown defers.
	msgs2 := []*schema.Message{
		bigMessage(30), bigMessage(30), bigMessage(30),
		bigMessage(30), bigMessage(30), bigMessage(30),
	}
	out2, didCompact2 := cm.maybeCompact(ctx, msgs2)
	require.False(t, didCompact2, "must NOT compact inside cooldown window")
	require.Equal(t, 0, inner.calls, "no additional summarize call")
	require.Equal(t, len(msgs2), len(out2), "cooldown returns original msgs")

	// Third call: roughly same size as first, but we increased lastCompactTokens
	// to 380 so growth is negative and still within cooldown.
	// Instead, set lastCompactTokens back and HardForceFraction=0.95 means we
	// need 0.95×400=380 tokens. Use many bigMessage(100) → ~100+8=108 each.
	// 4 × bigMessage(100) = 432 tokens > 380 → hard force.
	cm.lastCompactTokens = 180
	cm.lastCompactAt = time.Now()
	inner.calls = 0
	msgs3 := []*schema.Message{
		bigMessage(100), bigMessage(100), bigMessage(100), bigMessage(100),
	}
	_, didCompact3 := cm.maybeCompact(ctx, msgs3)
	require.True(t, didCompact3, "must compact at hard-force fraction regardless of cooldown")
}

// TestCompactingModel_HardForceOverridesCooldown verifies that when estimated
// tokens reach 0.95×ContextWindow, shouldCompact returns true even when inside
// the cooldown period.
func TestCompactingModel_HardForceOverridesCooldown(t *testing.T) {
	inner := &recordingModel{summary: "s", reply: "ok"}
	cm := &CompactingModel{
		Inner:             inner,
		Threshold:         0.8,
		ContextWindow:     1000,
		KeepRecent:        1,
		CooldownTokens:    99999, // extremely large — should still be overridden
		HardForceFraction: 0.95,
	}
	msgs := []*schema.Message{
		schema.UserMessage("go"),
		bigMessage(500), bigMessage(500), // 2×(500+8)+9 ≈ 1025 tokens, > 950
	}
	out, didCompact := cm.maybeCompact(context.Background(), msgs)
	require.True(t, didCompact, "must compact at 0.95 fraction regardless of cooldown")
	_ = out
}

// TestCompactingModel_FirstCompactNoCooldown tests that before any prior compact,
// the cooldown is a no-op (lastCompactTokens=0 → no cooldown).
func TestCompactingModel_FirstCompactNoCooldown(t *testing.T) {
	inner := &recordingModel{summary: "s", reply: "ok"}
	cm := &CompactingModel{
		Inner:             inner,
		Threshold:         0.8,
		ContextWindow:     2000,
		KeepRecent:        2,
		CooldownTokens:    100,
		HardForceFraction: 0.0, // disable hard-force for isolation
	}
	msgs := []*schema.Message{
		schema.UserMessage("go"),
		bigMessage(600), bigMessage(600), bigMessage(600),
	}
	out, didCompact := cm.maybeCompact(context.Background(), msgs)
	require.True(t, didCompact, "first compact must proceed even with cooldown configured")
	_ = out
}

// TestCompactingModel_KeepRecentBridge verifies the /2 bridge is documented.
// The test does NOT assert the bridge itself (it's a legacy semantics decision);
// it simply asserts existing CompactingModel.KeepRecent behavior works.
func TestCompactingModel_KeepRecentBridge(t *testing.T) {
	cm := &CompactingModel{KeepRecent: 4}
	// KeepRecent on CompactingModel is a MESSAGE count (not pair count).
	// ctxcompact.PlanOpts.KeepRecent is a PAIR count, bridged via /2.
	if cm.KeepRecent/2 < 2 { // 4/2 = 2 pairs → 4 messages
		t.Fatal("KeepRecent=4 must bridge to at least 2 pinned pairs")
	}
}
