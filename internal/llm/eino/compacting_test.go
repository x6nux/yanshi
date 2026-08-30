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

// callCount reads calls under the mutex. The cooldown tests assert on it
// repeatedly, and reading the field directly is a data race the -race job
// catches only when a test happens to run concurrently with a record().
func (r *recordingModel) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
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

// summarizableWindow is the smallest ContextWindow at which a compaction can
// actually produce a summary in this package's fixtures.
//
// A compaction is not free of framing: RunSummary appends the C4 structured
// instruction to whatever it summarizes, and that instruction costs ~130 tokens
// even in the terse form it falls back to on a small window. A window below
// that leaves a NEGATIVE chunk budget, so RunSummary refuses with
// ErrNoWindowRoom — correctly — and the caller reports "did not compact".
//
// That refusal is indistinguishable, from a test's point of view, from the
// compaction logic being broken: both show up as "no summarize call happened".
// Several fixtures here had windows of 100-220, tuned when the instruction was
// two sentences, and every one of them started reading as a compaction bug the
// moment the prompt grew. Naming the floor once, with the reason, is what stops
// the next prompt change from being diagnosed six times.
//
// 2000 is not a boundary value — it is comfortably clear of the floor, because
// none of these tests is about the floor. The ones that ARE about a boundary
// compute it from the window explicitly.
const summarizableWindow = 2000

// streamSummaryText is a summary long enough to clear the C10 quality gate.
//
// The gate demands min(MinChars, inputRunes/1000) runes and rejects text that
// is nothing but an acknowledgement. A short placeholder ("SUM", "ok") fails
// it, Run returns ErrSummaryRejected, and the caller keeps the original
// history — a CORRECT refusal that is indistinguishable, from a test, from
// compaction being broken. Fixtures that need a compaction to succeed use
// this; the ones testing the gate itself deliberately use a short string.
const streamSummaryText = "Reviewed the three prior assistant turns and folded " +
	"them into one continuation summary covering the task, its current state, " +
	"and the work still outstanding."

// bigMessage builds a message that ctxcompact.EstimateTokens prices at
// approximately `tokens` tokens (message overhead included), so a test can
// construct an over- or under-threshold history deterministically.
//
// IT CALIBRATES AGAINST THE REAL ESTIMATOR RATHER THAN ASSUMING A RATE. The
// original form was `tokens * 4` characters of NUL, on the premise that four
// characters cost one token. C8 replaced that flat rate with a run-classifying
// estimator, and a run of NUL bytes is a non-word run charged at TWO characters
// per token — so every fixture in this file silently became worth double its
// name, and six tests that had tuned ContextWindow to sit just above or just
// below a boundary landed on the wrong side of it.
//
// The lesson is the helper's, not the estimator's: a fixture builder whose name
// states a token count must MEASURE that count, or it is an assumption about
// pricing dressed up as a constant. Growing the content until the estimate
// reaches the target costs a few loop iterations at test time and can never
// drift again — whatever the estimator decides a character is worth.
func bigMessage(tokens int) *schema.Message {
	if tokens < 1 {
		tokens = 1
	}
	// Binary search would be premature: the estimate is monotonic in length and
	// the fixtures are small, so stepping by the current shortfall converges in
	// two or three passes.
	chars := tokens
	for i := 0; i < 32; i++ {
		m := &schema.Message{Role: schema.Assistant, Content: strings.Repeat("x", chars)}
		got := ctxcompact.EstimateTokens([]*schema.Message{m})
		if got >= tokens {
			return m
		}
		// Grow by the shortfall, scaled by the observed characters-per-token,
		// with a floor so a zero-progress step cannot loop forever.
		step := (tokens - got) * chars / max(got, 1)
		if step < 1 {
			step = 1
		}
		chars += step
	}
	return &schema.Message{Role: schema.Assistant, Content: strings.Repeat("x", chars)}
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

// TestCompactingModel_SummarizerPreferredOverInnerForCompaction pins the
// W-C-10 field split: when Summarizer is set, maybeCompact's ctxcompact.Run
// call must use IT for the summarize call, not Inner — while Inner still
// performs the real (post-compaction) turn-answering call, unaffected. This
// is deliberately NOT the same model doing both: silently answering a turn
// with a different model would be a much bigger behavior change than retrying
// just the summarization call with a fallback, and W-C-10 does only the
// latter (see the field's doc comment and wrapCompaction in
// internal/agent/orchestrator).
func TestCompactingModel_SummarizerPreferredOverInnerForCompaction(t *testing.T) {
	inner := &recordingModel{reply: "answer", streamOK: true}
	summarizer := &recordingModel{summary: "SUMMARY", streamOK: true}
	cm := &CompactingModel{
		Inner:         inner,
		Summarizer:    summarizer,
		Threshold:     0.5,
		ContextWindow: 200, // 100-token threshold budget; ≥177 keeps RunSummary single-path
		KeepRecent:    2,
	}
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

	assert.Equal(t, 1, summarizer.callCount(), "Summarizer, not Inner, must perform the summarize call")
	assert.Equal(t, 1, inner.callCount(), "Inner still performs the real (post-compaction) call")

	// Prove Inner's call carries the COMPACTED set produced by Summarizer's
	// output, not the original 5 messages — i.e. the summary really flowed
	// from Summarizer into the history Inner then answered.
	ins := inner.inputsSnapshot()
	require.Len(t, ins, 1)
	last := ins[0]
	assert.Len(t, last, 3, "compacted = KeepRecent tail (2) + summary at tail")
	assert.Contains(t, last[2].Content, "SUMMARY")

	// Inner must never have been asked to summarize.
	summIns := summarizer.inputsSnapshot()
	require.Len(t, summIns, 1, "summarizer performed exactly the summarize call")
}

// TestCompactingModel_NilSummarizerUsesInner pins the default (nil Summarizer)
// behavior explicitly, as a companion to
// TestCompactingModel_SummarizerPreferredOverInnerForCompaction: with no
// Summarizer configured, Inner performs BOTH the summarize call and the real
// call — byte-identical to pre-W-C-10 CompactingModel, which had no
// Summarizer field at all.
func TestCompactingModel_NilSummarizerUsesInner(t *testing.T) {
	inner := &recordingModel{summary: "SUMMARY", reply: "answer", streamOK: true}
	cm := &CompactingModel{
		Inner:         inner,
		Threshold:     0.5,
		ContextWindow: 200,
		KeepRecent:    2,
	}
	msgs := []*schema.Message{
		bigMessage(20),
		bigMessage(20),
		bigMessage(20),
		bigMessage(20),
		bigMessage(20),
	}
	require.True(t, cm.shouldCompact(msgs))

	_, err := cm.Generate(context.Background(), msgs)
	require.NoError(t, err)

	assert.Equal(t, 2, inner.callCount(), "with no Summarizer, Inner alone performs both the summarize and real calls")
}

// TestCompactingModel_StreamCompacts proves Stream mirrors Generate's compaction
// (the ADK takes the Stream path under EnableStreaming).
//
// As with the Generate variant, ContextWindow + message size are tuned so
// ctxcompact.Run's single-path summary is taken (smaller windows would trip
// carry-style chunking and turn the recordingModel into a multi-call witness).
func TestCompactingModel_StreamCompacts(t *testing.T) {
	// The summary text has to clear the C10 quality gate's compression floor
	// (min(80, inputRunes/1000) runes). A 3-character placeholder like "SUM"
	// is rejected as too short and the compaction correctly does not happen —
	// which reads, from here, exactly like the Stream path failing to compact.
	inner := &recordingModel{summary: streamSummaryText, reply: "answer", streamOK: true}
	cm := &CompactingModel{
		Inner:         inner,
		Threshold:     0.5,
		ContextWindow: summarizableWindow,
		KeepRecent:    1,
	}
	// Over 0.5*2000 = 1000 tokens, and small enough that RunSummary stays on
	// its single cache-aligned call, so recordingModel is a 1-call witness.
	msgs := []*schema.Message{bigMessage(400), bigMessage(400), bigMessage(400)}

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
		ContextWindow: summarizableWindow,
		KeepRecent:    1,
	}
	msgs := []*schema.Message{bigMessage(400), bigMessage(400), bigMessage(400)}

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
//
// ledger: F2/CCL1#1 同 turn 不重复压缩
func TestCompactingModel_CooldownDefersReCompact(t *testing.T) {
	inner := &recordingModel{summary: streamSummaryText, reply: "done", streamOK: true}
	cm := &CompactingModel{
		Inner:     inner,
		Threshold: 0.5,
		// Every quantity below is a FRACTION of this window, so the whole
		// fixture scales together. It is summarizableWindow rather than the
		// original 400 because a compaction has to fit the C4 instruction
		// (~130 tokens) inside the window before it can summarize anything;
		// at 400 RunSummary refused with ErrNoWindowRoom and the test read
		// that correct refusal as "the cooldown logic is broken".
		ContextWindow:     summarizableWindow,
		KeepRecent:        2,
		CooldownTokens:    500,
		HardForceFraction: 0.95,
	}
	ctx := context.Background()

	// 6 × 190 ≈ 1140 tokens, over the 0.5 × 2000 = 1000 threshold.
	// KeepRecent=2 → 4 compressible messages fold into one short summary, so
	// TokensAfter is well below TokensBefore.
	msgs := []*schema.Message{
		bigMessage(190), bigMessage(190), bigMessage(190),
		bigMessage(190), bigMessage(190), bigMessage(190),
	}
	require.True(t, cm.shouldCompact(msgs), "first set is over threshold")
	out, didCompact := cm.maybeCompact(ctx, msgs)
	require.True(t, didCompact, "first call must compact (over threshold)")
	require.Equal(t, 1, inner.calls, "one summarize call must have happened")
	_ = out

	// Simulate post-compact: lastCompactTokens just under threshold=200.
	cm.lastCompactAt = time.Now()
	inner.calls = 0

	// Second call: the same 6 messages, so growth is ~0, far under
	// CooldownTokens=500 → the cooldown defers.
	msgs2 := []*schema.Message{
		bigMessage(190), bigMessage(190), bigMessage(190),
		bigMessage(190), bigMessage(190), bigMessage(190),
	}
	out2, didCompact2 := cm.maybeCompact(ctx, msgs2)
	require.False(t, didCompact2, "must NOT compact inside cooldown window")
	require.Equal(t, 0, inner.calls, "no additional summarize call")
	require.Equal(t, len(msgs2), len(out2), "cooldown returns original msgs")

	// Third call: still inside the cooldown, but now past the hard-force
	// fraction (0.95 × 2000 = 1900), so it must compact anyway.
	cm.lastCompactAt = time.Now()
	inner.calls = 0
	msgs3 := []*schema.Message{
		bigMessage(500), bigMessage(500), bigMessage(500), bigMessage(500),
	}
	_, didCompact3 := cm.maybeCompact(ctx, msgs3)
	require.True(t, didCompact3, "must compact at hard-force fraction regardless of cooldown")
}

// TestCompactingModel_HardForceOverridesCooldown was deleted in W4 review
// round 22. It never armed a cooldown -- didCompact was false and
// lastCompactAt zero, so inCooldown returned immediately -- which meant the
// compaction it observed happened because nothing was holding it back, not
// because hard force overrode anything. Deleting the hard-force branch
// entirely left it green (measured rounds 5 and 19).
//
// TestCompactingModel_HardForceBeatsCooldown covers the guarantee its name
// claimed, and asserts the cooldown is genuinely active first so it cannot
// pass for that same wrong reason.

// TestCompactingModel_FirstCompactNoCooldown tests that before any prior compact,
// the cooldown is a no-op (lastCompactTokens=0 → no cooldown).
//
// Two fixture details are load-bearing, and both were wrong until the C10
// quality gate exposed them by turning a silently-degraded path into a failure.
//
// streamOK MUST be true. The summarizer prefers Stream and falls back to
// Generate, so a fixture without it burns call #1 on the failing Stream --
// and recordingModel only returns `summary` on call #1. The fallback Generate
// is call #2, which returns `reply`. The summary therefore never reached the
// summarizer at all: this was the only fixture in the file that set `summary`
// without `streamOK`, and all 13 siblings set both.
//
// The summary text must be a plausible summary, not "s" or "ok". The
// compaction core applies ctxcompact.CheckQuality, which rejects a candidate
// too short to summarize its input or one that is a bare acknowledgement. A
// rejection surfaces here as didCompact=false, which this test would report as
// a cooldown bug -- naming the wrong subsystem. Summary CONTENT is incidental
// to what this test asserts.
func TestCompactingModel_FirstCompactNoCooldown(t *testing.T) {
	inner := &recordingModel{summary: "SUMMARY", reply: "ok", streamOK: true}
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

// TestCompactingModel_KeepRecentBridge was deleted in W4 review round 23. It
// asserted cm.KeepRecent/2 >= 2 on a struct the test itself had just built
// with KeepRecent: 4 -- both sides were literals it chose, so it restated
// 4/2 >= 2 and touched no production code at all.
//
// docs/superpowers/acceptance-breakdown.md listed it as the worked example of
// its 恒真空壳 category, quoting the test's own comment admitting it does not
// assert the bridge. It survived that diagnosis by roughly thirty review
// rounds.
//
// TestCompactingModel_KeepRecentBridgesMessagesToPairs pins the bridge for
// real: it sizes a history so that dropping the /2 pins the entire thing and
// compaction stops happening, and is verified red against exactly that.

// TestCompactingModel_MaybeCompact_RejectsAGrowingSummary pins the second half
// of maybeCompact's best-effort gate.
//
// That gate reads `err != nil || res.TokensAfter >= res.TokensBefore`, and its
// two halves fail differently. TestCompactingModel_MaybeCompact_BestEffort
// covers the error half by starving ctxcompact.Run of window. Nothing covered
// the size half: measured W4 review round 2, deleting it left the whole
// package green. A summary that came back larger than the history it replaced
// would then be forwarded to the model as if it were a compaction.
//
// ⚠️ THIS TEST USED TO ALSO ASSERT `didCompact == false` HERE, with the reason
// "a rejected compaction must not arm the cooldown, or the next turn skips a
// compaction it needs". That half was INVERTED — maybeCompact now arms on
// failure — and the reason is preserved rather than deleted, because THE DANGER
// IT NAMES IS REAL:
//
//	An over-large context that is refused a retry keeps growing, and eventually
//	the provider call does not fit.
//
// What makes arming on failure safe is not that the danger went away. It is
// that HardForceFraction blocks it: shouldCompact checks hard-force BEFORE the
// cooldown gate, so a cooldown can delay a retry but never prevent one once the
// history nears the window edge. Break that ordering, or let HardForceFraction
// reach a production path as 0, and the sentence above is true again.
//
// That coupling is load-bearing and therefore has its own test —
// TestHardForceBypassesCooldown_SoArmingOnFailureIsSafe. The cost the old rule
// ignored (one paid summary call per ReAct iteration, unbounded) is pinned by
// TestCompactingModel_AFailedCompactionArmsTheCooldown. What survives here is
// the part this test is actually about: the REJECTION.
func TestCompactingModel_MaybeCompact_RejectsAGrowingSummary(t *testing.T) {
	inner := &recordingModel{
		summary:  strings.Repeat("x", 40000), // dwarfs the history below
		reply:    "ok",
		streamOK: true,
	}
	cm := &CompactingModel{
		Inner:         inner,
		Threshold:     0.5,
		ContextWindow: 1000,
		KeepRecent:    2,
	}
	msgs := []*schema.Message{bigMessage(300), bigMessage(300), bigMessage(300)}

	got, did := cm.maybeCompact(context.Background(), msgs)

	assert.False(t, did, "a summary bigger than the history is not a compaction")
	assert.Equal(t, msgs, got, "the original history must be forwarded unchanged")
}

// TestCompactingModel_KeepRecentBridgesMessagesToPairs pins the /2 that
// converts this type's KeepRecent into ctxcompact's.
//
// The two fields share a name and count different things: CompactingModel
// counts MESSAGES, ctxcompact.PlanOpts counts PAIRS, and ctxcompact.Plan pins
// the last 2*KeepRecent messages. Dropping the /2 pins twice the intended
// tail, which here is the entire history, leaving nothing to summarize -- so
// compaction silently stops happening and the caller keeps growing a context
// it believes was compacted.
//
// The sizing is load-bearing, not arbitrary. The history must clear
// Threshold*ContextWindow so compaction triggers, and stay under the 0.9
// ChunkThreshold so ctxcompact.Run takes its single-call path: the chunked
// path does not preserve Plan's tail, and a first attempt at this test sized
// into it and stayed green under the mutation (W4 review round 3).
//
// â ï¸ Round 3 also claimed no existing test caught the missing /2. Round 19
// disproved that: TestCompactingModel_CompactsWhenOverThreshold reddens on the
// same mutation. The round-3 reading came from `grep '^--- FAIL'`, a pattern
// that silently matches nothing in this environment even when the very first
// line of output starts with it -- so an entire failing run read as clean.
// This test is kept for its explicit sizing rationale, but it did not close an
// open gap.
func TestCompactingModel_KeepRecentBridgesMessagesToPairs(t *testing.T) {
	inner := &recordingModel{summary: "SUMMARY", reply: "ok", streamOK: true}
	cm := &CompactingModel{
		Inner:         inner,
		Threshold:     0.5, // triggers above 500 tokens
		ContextWindow: 1000,
		KeepRecent:    4, // messages, so 2 pairs
	}
	// 8 x 80 = 640 tokens: over the 500 trigger, under the 900 chunk threshold.
	msgs := make([]*schema.Message, 8)
	for i := range msgs {
		msgs[i] = bigMessage(80)
	}

	got, did := cm.maybeCompact(context.Background(), msgs)

	assert.True(t, did, "4 of 8 messages sit outside the tail and must be summarized")
	assert.Less(t, len(got), len(msgs), "the compacted history must be shorter")
}

// TestCompactingModel_HardForceBeatsCooldown pins the branch that lets a
// near-full window compact even inside a cooldown period.
//
// Cooldown exists so an unchanged history is not compacted twice in a row.
// But it must not win when the context is about to overflow: without the
// hard-force branch the cooldown defers the compaction, the deferred history
// keeps growing, and the next inner call is handed a history that no longer
// fits the window. W4 review round 5 measured the gap -- replacing the branch
// with a constant false left the whole package green.
//
// ledger: F2/CCL1#2 逼近上限仍触发
func TestCompactingModel_HardForceBeatsCooldown(t *testing.T) {
	cm := &CompactingModel{
		Threshold:         0.5,
		ContextWindow:     1000,
		KeepRecent:        2,
		HardForceFraction: 0.9,
		CooldownTokens:    100000, // so large the token cooldown can never lapse
	}
	// Arm the cooldown as a just-completed compaction would.
	cm.didCompact = true
	cm.lastCompactTokens = 900
	cm.lastCompactAt = time.Now()

	// 950 tokens: inside the cooldown, but at 95% of the window.
	msgs := []*schema.Message{bigMessage(320), bigMessage(320), bigMessage(310)}

	assert.True(t, cm.inCooldown(ctxcompact.EstimateTokens(msgs)),
		"premise: the cooldown must be active, or this test proves nothing")
	assert.True(t, cm.shouldCompact(msgs),
		"a history at 95% of the window must compact despite the cooldown")
}

// TestCompactingModel_ShortHistoryIsNotCompacted pins the early return for a
// history no longer than the tail that would be pinned anyway.
//
// Such a history has nothing outside the tail to summarize, so compacting it
// can only spend an inner model call to produce something no smaller. The
// threshold gate does not catch this on its own: a handful of very large
// messages clears Threshold*ContextWindow while still being too few to have a
// summarizable middle. W4 review round 6 replaced the guard with a constant
// false and the whole package stayed green.
func TestCompactingModel_ShortHistoryIsNotCompacted(t *testing.T) {
	cm := &CompactingModel{
		Threshold:     0.5,
		ContextWindow: 1000,
		KeepRecent:    4,
	}
	// 3 messages, 900 tokens: far over the 500 threshold, but fewer messages
	// than KeepRecent, so the tail alone is the whole history.
	msgs := []*schema.Message{bigMessage(300), bigMessage(300), bigMessage(300)}

	assert.Greater(t, ctxcompact.EstimateTokens(msgs), int(cm.Threshold*float64(cm.ContextWindow)),
		"premise: the threshold gate must be cleared, or this test proves nothing")
	assert.False(t, cm.shouldCompact(msgs),
		"a history shorter than the pinned tail has nothing to summarize")
}

// TestCompactingModel_ConcurrentMaybeCompactIsRaceFree pins cmMu.
//
// One CompactingModel is shared by every turn of a session, and mid-turn
// compaction can be entered from more than one in-flight turn at a time.
// The cooldown fields it mutates are plain ints and a time.Time, so without
// the mutex this is a genuine data race, not a theoretical one -- W4 review
// round 7 removed the Lock/Unlock pairs and the -race suite still passed,
// because nothing had ever called maybeCompact concurrently.
func TestCompactingModel_ConcurrentMaybeCompactIsRaceFree(t *testing.T) {
	inner := &recordingModel{summary: "SUMMARY", reply: "ok", streamOK: true}
	cm := &CompactingModel{
		Inner:         inner,
		Threshold:     0.5,
		ContextWindow: 1000,
		KeepRecent:    4,
	}
	msgs := make([]*schema.Message, 8)
	for i := range msgs {
		msgs[i] = bigMessage(80)
	}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cm.maybeCompact(context.Background(), msgs)
		}()
	}
	wg.Wait()

	cm.cmMu.Lock()
	defer cm.cmMu.Unlock()
	assert.True(t, cm.didCompact, "at least one of the racing calls must have compacted")
}

// TestCompactingModel_DegenerateConfigNeverCompacts pins the two terms of
// shouldCompact's config guard that its Threshold term hides.
//
// W4 review round 8 reduced the guard to its Threshold term alone and the
// whole package stayed green, so only the easiest of the three was covered.
// The other two are not decoration:
//
//   - KeepRecent == 0 makes ctxcompact pin no tail at all, so the summarizer
//     is handed the current turn's own messages and the model loses the thing
//     it was about to answer.
//   - ContextWindow == 0 puts every threshold at zero, so the gate fires on
//     any history at all and hands ctxcompact a zero window to plan against.
func TestCompactingModel_DegenerateConfigNeverCompacts(t *testing.T) {
	msgs := make([]*schema.Message, 8)
	for i := range msgs {
		msgs[i] = bigMessage(200)
	}

	t.Run("KeepRecent zero", func(t *testing.T) {
		cm := &CompactingModel{Threshold: 0.5, ContextWindow: 1000, KeepRecent: 0}
		assert.False(t, cm.shouldCompact(msgs),
			"a zero tail would let the summarizer eat the current turn")
	})

	t.Run("ContextWindow zero", func(t *testing.T) {
		cm := &CompactingModel{Threshold: 0.5, ContextWindow: 0, KeepRecent: 4}
		assert.False(t, cm.shouldCompact(msgs),
			"a zero window puts every threshold at zero and fires on anything")
	})

	// Round 8 added the two subtests above and left this one out, because
	// TestCompactingModel_DisabledIsPassthrough appeared to cover it. Round 24
	// measured that it does not: dropping the Threshold term from the guard
	// leaves that test -- and the whole package -- green, since its history is
	// short enough for downstream guards to decline the compaction anyway.
	t.Run("Threshold zero", func(t *testing.T) {
		cm := &CompactingModel{Threshold: 0, ContextWindow: 1000, KeepRecent: 4}
		assert.False(t, cm.shouldCompact(msgs),
			"threshold 0 means compaction is switched off, not that every history qualifies")
	})
}

// TestCompactingModel_ZeroCooldownTokensDisablesTheTokenDimension pins the
// CooldownTokens > 0 term in inCooldown.
//
// Zero means the operator turned the token dimension off. Without that term
// the comparison still runs, and tokens-lastT is negative whenever the history
// shrank -- which is exactly what a compaction does -- so a disabled cooldown
// would switch itself back on right after every compaction. W4 review round 9
// dropped the term and the whole package stayed green.
func TestCompactingModel_ZeroCooldownTokensDisablesTheTokenDimension(t *testing.T) {
	cm := &CompactingModel{
		CooldownTokens:   0, // disabled
		CooldownDuration: 0, // and no time dimension either
	}
	cm.didCompact = true
	cm.lastCompactTokens = 1000

	assert.False(t, cm.inCooldown(500),
		"a shrunken history must not revive a cooldown the operator disabled")
}

// TestCompactingModel_StreamCompactsToo pins the Stream entry point's call to
// maybeCompact.
//
// Generate and Stream each decide independently whether to compact, and the
// orchestrator runs with EnableStreaming: true -- so Stream is the production
// path, and a compaction wired into Generate alone would look correct in tests
// while never firing in a real session.
//
// ⚠️ Round 11 claimed severing Stream's call left the package green. That was
// a measurement failure, not a finding: the grep counting failures returned
// empty rather than zero and was read as zero. Re-measured in round 18 with
// the output captured to a file, TestCompactingModel_StreamCompacts already
// reddens on that mutation. This test is kept because it asserts a different
// thing -- what the inner model actually received, rather than the stream
// count -- but it did not close a gap that was open.
func TestCompactingModel_StreamCompactsToo(t *testing.T) {
	inner := &recordingModel{summary: "SUMMARY", reply: "ok", streamOK: true}
	cm := &CompactingModel{
		Inner:         inner,
		Threshold:     0.5,
		ContextWindow: 1000,
		KeepRecent:    4,
	}
	msgs := make([]*schema.Message, 8)
	for i := range msgs {
		msgs[i] = bigMessage(80)
	}

	_, err := cm.Stream(context.Background(), msgs)
	assert.NoError(t, err)

	inner.mu.Lock()
	defer inner.mu.Unlock()
	// inputs[0] is the summarize turn; the last input is what Stream forwarded.
	forwarded := inner.inputs[len(inner.inputs)-1]
	assert.Less(t, len(forwarded), len(msgs),
		"Stream must forward the compacted history, not the original")
}

// TestCompactingModel_MaybeCompactArmsTheTimeCooldown pins the write half of
// the time-based cooldown.
//
// TestCompactingModel_InCooldown_TimeBased covers the read: given a
// lastCompactAt, inCooldown respects CooldownDuration. It plants that field
// itself, so it says nothing about whether anything ever sets it. W4 review
// round 15 deleted the assignment in maybeCompact and the whole package
// stayed green -- in production lastCompactAt would stay zero forever, the
// time dimension would never fire, and CooldownDuration would be dead config.
func TestCompactingModel_MaybeCompactArmsTheTimeCooldown(t *testing.T) {
	inner := &recordingModel{summary: "SUMMARY", reply: "ok", streamOK: true}
	cm := &CompactingModel{
		Inner:         inner,
		Threshold:     0.5,
		ContextWindow: 1000,
		KeepRecent:    4,
	}
	msgs := make([]*schema.Message, 8)
	for i := range msgs {
		msgs[i] = bigMessage(80)
	}

	_, did := cm.maybeCompact(context.Background(), msgs)
	require.True(t, did, "premise: the compaction must happen, or nothing could be armed")

	cm.cmMu.Lock()
	defer cm.cmMu.Unlock()
	assert.False(t, cm.lastCompactAt.IsZero(),
		"a successful compaction must stamp lastCompactAt, or CooldownDuration is dead config")
}

// TestCompactingModel_ThresholdBoundaries pins both gates at the exact token
// count where they flip.
//
// Low severity on its own -- EstimateTokens rarely lands precisely on a
// boundary -- but W4 review round 16 measured that both comparisons could be
// loosened by one (< to <=, >= to >) with the whole package still green, so
// the boundary was documented only by the source. It matters most when a
// future change reads the comment rather than the operator: the threshold
// gate compacts AT the threshold, not merely above it.
func TestCompactingModel_ThresholdBoundaries(t *testing.T) {
	// 8 messages so the short-history guard cannot interfere.
	at := func(tokensEach int) []*schema.Message {
		msgs := make([]*schema.Message, 8)
		for i := range msgs {
			msgs[i] = bigMessage(tokensEach)
		}
		return msgs
	}

	t.Run("threshold gate fires exactly at the threshold", func(t *testing.T) {
		cm := &CompactingModel{Threshold: 0.5, ContextWindow: 1000, KeepRecent: 4}
		msgs := at(80)
		tokens := ctxcompact.EstimateTokens(msgs)
		cm.Threshold = float64(tokens) / float64(cm.ContextWindow) // threshold == tokens
		assert.True(t, cm.shouldCompact(msgs),
			"at the threshold the gate must fire; loosening < to <= would defer it")
	})

	t.Run("hard force fires exactly at its fraction", func(t *testing.T) {
		cm := &CompactingModel{Threshold: 0.99, ContextWindow: 1000, KeepRecent: 4}
		msgs := at(80)
		tokens := ctxcompact.EstimateTokens(msgs)
		cm.HardForceFraction = float64(tokens) / float64(cm.ContextWindow)
		assert.True(t, cm.shouldCompact(msgs),
			"at the hard-force fraction it must fire even below the threshold; "+
				"tightening >= to > would let the window overflow by one token")
	})
}

// TestCompactingModel_DoesNotMutateTheCallersHistory pins the premise
// ADR-0013's whole rule rests on: compaction is not sticky.
//
// The ADR says mid-turn accounting must use the UNCOMPACTED dimension
// "because the result never goes back into ADK state -- the next iteration
// hands it the same history it would have seen anyway". That premise is only
// true while maybeCompact leaves the caller's slice alone. Rewrite it in
// place and the next iteration receives the compacted form instead, at which
// point storing TokensBefore is the wrong choice and the cooldown starts
// measuring against a history that no longer exists.
//
// Nothing pinned this before: the ADR's load-bearing sentence was an argued
// property, which is the exact failure mode this package's review kept
// finding in the code it reviews.
//
// Scope: the snapshot is a struct value copy, so it catches replacing a
// message, rewriting its text, or clearing a field. It does NOT catch
// mutation THROUGH a shared slice header -- msgs[i].ToolCalls[0].ID = "x"
// shares its backing array with the snapshot. That route is UNPINNED; a deep
// copy would cover it, at the cost of hand-maintaining one per schema change.
func TestCompactingModel_DoesNotMutateTheCallersHistory(t *testing.T) {
	inner := &recordingModel{summary: "SUMMARY", reply: "ok", streamOK: true}
	cm := &CompactingModel{
		Inner:         inner,
		Threshold:     0.5,
		ContextWindow: 1000,
		KeepRecent:    4,
	}
	msgs := make([]*schema.Message, 8)
	for i := range msgs {
		msgs[i] = bigMessage(80)
	}
	// One message must carry a tool call, or the ToolCalls half of the
	// comparison below is asserted only against nil and a mutation clearing
	// the field is a no-op on the fixture -- which is how round 4's first
	// attempt at this measured green.
	msgs[1].ToolCalls = []schema.ToolCall{{ID: "c1", Function: schema.FunctionCall{Name: "f"}}}
	msgs[2] = &schema.Message{Role: schema.Tool, ToolCallID: "c1", Content: strings.Repeat("r", 320)}

	// Snapshot identity and the whole struct value before the call. Content
	// alone is not enough: clearing ToolCalls in place leaves the pointer and
	// the text untouched and still breaks the premise, and an earlier version
	// of this test missed exactly that (measured W4 review round 4).
	before := make([]*schema.Message, len(msgs))
	copy(before, msgs)
	values := make([]schema.Message, len(msgs))
	for i, m := range msgs {
		values[i] = *m
	}

	out, did := cm.maybeCompact(context.Background(), msgs)
	require.True(t, did, "premise: the history must actually be compacted")
	require.Less(t, len(out), len(msgs), "premise: the returned history is shorter")

	require.Len(t, msgs, len(before), "the caller's slice changed length")
	for i := range msgs {
		assert.Same(t, before[i], msgs[i],
			"message %d was replaced: the next ADK iteration would receive a different history", i)
		assert.Equal(t, values[i], *msgs[i],
			"message %d was rewritten in place, which is the same breakage by another route", i)
	}
}

// TestCompactingModel_UnderThresholdWithACompressibleHistory pins the
// threshold gate itself.
//
// TestCompactingModel_PassthroughUnderThreshold shares the name but not the
// guarantee: its history is short enough that the len(msgs) <= KeepRecent
// guard and the best-effort size check would refuse the compaction anyway, so
// it stays green with the threshold comparison removed entirely (measured W4
// review round 21). It proves passthrough happens, not that the threshold is
// what caused it.
//
// This one hands over a history that is long, compressible and comfortably
// under the threshold, so the threshold gate is the only thing that can
// decline it.
func TestCompactingModel_UnderThresholdWithACompressibleHistory(t *testing.T) {
	inner := &recordingModel{summary: "SUMMARY", reply: "ok", streamOK: true}
	cm := &CompactingModel{
		Inner:         inner,
		Threshold:     0.9, // fires at 900 tokens
		ContextWindow: 1000,
		KeepRecent:    4,
	}
	// 8 x 40 = 320 tokens: far under the 900 trigger, but long enough and
	// large enough that every downstream guard would happily compact it.
	msgs := make([]*schema.Message, 8)
	for i := range msgs {
		msgs[i] = bigMessage(40)
	}
	require.Less(t, ctxcompact.EstimateTokens(msgs), int(cm.Threshold*float64(cm.ContextWindow)),
		"premise: the history must be under the threshold")
	require.Greater(t, len(msgs), cm.KeepRecent,
		"premise: the short-history guard must not be what declines this")

	out, did := cm.maybeCompact(context.Background(), msgs)
	assert.False(t, did, "a history under the threshold must not be compacted")
	assert.Equal(t, msgs, out, "the original history is forwarded unchanged")

	inner.mu.Lock()
	defer inner.mu.Unlock()
	assert.Zero(t, inner.calls, "no summariser call may be spent below the threshold")
}

// TestCompactingModel_AFailedCompactionArmsTheCooldown is the retry-storm
// guard, and it is the reason the sibling assertion in
// TestCompactingModel_MaybeCompact_RejectsAGrowingSummary was inverted rather
// than kept.
//
// maybeCompact used to arm the cooldown ONLY on success. That is defensible in
// isolation — a rejected compaction is one the context still needs — but this
// is the MID-TURN path, and the ADK ReAct loop calls Generate on every
// iteration with the full history. An unarmed gate therefore means the same
// doomed compaction is attempted once per iteration, and each attempt is a paid
// summary model call against a history that has not changed. Nothing about the
// second attempt can succeed where the first failed.
//
// The old rationale ("the next turn skips a compaction it needs") is answered
// by HardForceFraction, which is checked BEFORE the cooldown gate: once the
// history reaches the hard-force fraction the cooldown is overridden and
// compaction retries regardless. So arming on failure does not disable
// compaction, it rate-limits the retry to once per CooldownTokens of growth —
// bounded above by hard-force, which TestCompactingModel_HardForceBeatsCooldown
// already pins.
//
// THE ASSERTION IS THE CALL COUNT. The broken version returned no error, logged
// nothing, and produced a correct final answer; the only observable was how
// many times the provider was billed.
func TestCompactingModel_AFailedCompactionArmsTheCooldown(t *testing.T) {
	inner := &recordingModel{
		summary:  strings.Repeat("x", 40000), // rejected: bigger than the history
		reply:    "ok",
		streamOK: true,
	}
	cm := &CompactingModel{
		Inner:          inner,
		Threshold:      0.5,
		ContextWindow:  1000,
		KeepRecent:     2,
		CooldownTokens: 100,
		// HardForceFraction stays 0 so the cooldown is the only gate under test;
		// its override is covered by TestCompactingModel_HardForceBeatsCooldown.
	}
	msgs := []*schema.Message{bigMessage(300), bigMessage(300), bigMessage(300)}

	_, did := cm.maybeCompact(context.Background(), msgs)
	require.False(t, did, "a summary bigger than the history is not a compaction")
	require.Equal(t, 1, inner.callCount(), "the first attempt costs exactly one summary call")

	// Same history, next ReAct iteration. Nothing has changed, so nothing can
	// succeed — and nothing may be billed.
	_, did2 := cm.maybeCompact(context.Background(), msgs)
	assert.False(t, did2)
	assert.Equal(t, 1, inner.callCount(),
		"the failed attempt must not be repeated on every iteration of the turn")
}

// TestCompactingModel_ModelFailureFallsBackToPinsOnly is the mid-turn half of
// W-C-04's ladder symmetry (CLAUDE.md's mid-turn/pre-turn requirement): it
// exercises maybeCompact's OWN copy of the
// `err != nil || TokensAfter >= TokensBefore` success gate (line 206 above)
// against a summary model that fails OUTRIGHT — never returns a reply at
// all — which is a DIFFERENT failure mode from
// TestCompactingModel_AFailedCompactionArmsTheCooldown just above: that
// test's model call SUCCEEDS with an oversized summary, rejected by the C10
// quality gate one layer down inside ctxcompact.Run, and Run still returns an
// error on THAT path (see run.go's doc comment: EMPTY/QUALITY stay gates).
// Before W-C-04 an outright model-call failure also propagated as a Run
// error and maybeCompact returned (msgs, false); now ctxcompact.Run absorbs
// it into a Fallback=true Result, so did must come back true here — the
// fixture (NewFakeModel(nil, err) + this msgs slice) is independent from
// both ctxcompact/run_test.go's fallback fixtures and
// ctxcompact/compact_test.go's pre-turn fixture, so a regression in any ONE
// of the three literal gate copies reddens only that layer's test.
func TestCompactingModel_ModelFailureFallsBackToPinsOnly(t *testing.T) {
	inner := NewFakeModel(nil, errors.New("model down")) // non-transient -> immediate, no retry cost
	cm := &CompactingModel{
		Inner:         inner,
		Threshold:     0.5,
		ContextWindow: 200, // 100-token threshold budget, same fixture as TestCompactingModel_CompactsWhenOverThreshold
		KeepRecent:    2,
	}
	// 5 messages, each ~28 tokens → ~140 tokens, well over the 100 budget —
	// same shape as TestCompactingModel_CompactsWhenOverThreshold, so this
	// test isolates the model-failure branch rather than a threshold-gate
	// difference.
	msgs := []*schema.Message{
		bigMessage(20),
		bigMessage(20),
		bigMessage(20),
		bigMessage(20),
		bigMessage(20),
	}
	require.True(t, cm.shouldCompact(msgs), "fixture is over threshold")

	out, did := cm.maybeCompact(context.Background(), msgs)
	require.True(t, did, "model failure is a fallback, not a no-op (W-C-04)")
	assert.Less(t, len(out), len(msgs), "the summarize-only messages are dropped")
}

// TestHardForceBypassesCooldown_SoArmingOnFailureIsSafe pins the COUPLING that
// makes maybeCompact's arm-on-failure safe. Read this before changing either
// the cooldown or the hard-force branch.
//
// # The danger this guards is real, not hypothetical
//
// Arming the cooldown when a compaction FAILS is a deliberate inversion of an
// earlier rule ("a rejected compaction must not arm the cooldown, or the next
// turn skips a compaction it needs"). That rule was protecting against
// something true: an over-large context that is refused a retry keeps growing
// and eventually the provider call does not fit. The inversion is safe for
// exactly one reason — shouldCompact checks HardForceFraction BEFORE the
// cooldown gate, so the cooldown can delay a retry but can never prevent one
// once the history nears the window edge.
//
// IF THAT ORDERING BREAKS, OR HardForceFraction REACHES A PRODUCTION PATH AS 0,
// THE OLD DANGER COMES BACK. Nothing else in the package would notice: the
// symptom is a context that quietly stops being compacted, and every test that
// arms the cooldown by hand would still pass. So the coupling gets a test
// naming what it protects.
//
// TestCompactingModel_HardForceBeatsCooldown covers the same override at the
// shouldCompact level with a HAND-ARMED cooldown. This one goes through
// maybeCompact end to end with the cooldown armed BY AN ACTUAL FAILED
// COMPACTION, which is the state the inversion actually creates.
//
// # It also covers the transient-failure cost
//
// Rate-limiting a retry also rate-limits a retry after a RECOVERABLE failure —
// a provider blip costs CooldownTokens of growth before the next attempt, where
// it used to cost one iteration. Step 3 is that scenario end to end: the first
// attempt is rejected, the session keeps growing, and the compaction does
// eventually happen and succeed. The session is delayed, never stranded.
//
// CooldownTokens is set absurdly high on purpose. Token growth can then never
// lapse the cooldown by itself, so if step 3 compacts, hard-force is the only
// thing that could have let it through.
func TestHardForceBypassesCooldown_SoArmingOnFailureIsSafe(t *testing.T) {
	// recordingModel returns `summary` on call 1 and `reply` afterwards, which
	// is a transient failure for free: attempt 1 gets a summary bigger than the
	// history (rejected), attempt 2 gets a usable one.
	inner := &recordingModel{
		summary:  strings.Repeat("x", 40000), // rejected: dwarfs the history
		reply:    streamSummaryText,          // long enough to clear the C10 gate
		streamOK: true,
	}
	cm := &CompactingModel{
		Inner: inner,
		// summarizableWindow, not a hand-picked number: step 3 requires a
		// compaction that SUCCEEDS, and this file documents that a window below
		// the summary instruction's framing cost makes RunSummary refuse — a
		// correct refusal that reads exactly like compaction being broken.
		ContextWindow:     summarizableWindow, // 2000
		Threshold:         0.5,                // 1000 tokens
		HardForceFraction: 0.9,                // 1800 tokens
		KeepRecent:        2,
		CooldownTokens:    100000, // growth alone can never lapse this
	}
	ctx := context.Background()

	// STEP 1 — over threshold, under hard-force. The attempt is made and fails.
	small := []*schema.Message{bigMessage(400), bigMessage(400), bigMessage(400)} // ~1200
	_, did := cm.maybeCompact(ctx, small)
	require.False(t, did, "premise: the first attempt must fail")
	require.Equal(t, 1, inner.callCount(), "and must have cost exactly one call")
	cm.cmMu.Lock()
	armed := cm.didCompact
	cm.cmMu.Unlock()
	require.True(t, armed, "premise: the failure armed the cooldown")

	// STEP 2 — unchanged history. The cooldown holds, so nothing is billed.
	_, did = cm.maybeCompact(ctx, small)
	require.False(t, did)
	require.Equal(t, 1, inner.callCount(),
		"premise: the cooldown really is suppressing retries — otherwise step 3 proves nothing")

	// STEP 3 — the session kept growing and is now at the window edge. The
	// cooldown is still nowhere near lapsing, so ONLY hard-force can let this
	// through. It must, and the retry must succeed.
	big := []*schema.Message{ // ~2000, past the 1800 hard-force line
		bigMessage(400), bigMessage(400), bigMessage(400), bigMessage(400), bigMessage(400),
	}
	require.True(t, cm.inCooldown(ctxcompact.EstimateTokens(big)),
		"premise: still inside the cooldown, so hard-force is the only way through")

	out, did := cm.maybeCompact(ctx, big)
	assert.True(t, did,
		"a history at the window edge must compact despite an armed cooldown — "+
			"this is the entire reason arming on failure is safe")
	assert.Equal(t, 2, inner.callCount(), "the retry happened, exactly once")
	assert.Less(t, len(out), len(big), "and it really compacted")
}

// TestRequestNewWindow_NoSignalBound proves the write side is a safe no-op
// (not a panic, not a silent context.WithValue on a key nobody reads) when
// the turn never bound a signal — e.g. a sub-agent context, or any call site
// that predates W-C-14's orchestrator wiring. This is what
// internal/tools/contextwindow.go's run() relies on to report an error
// instead of claiming a request landed that nothing will ever read.
func TestRequestNewWindow_NoSignalBound(t *testing.T) {
	ok := RequestNewWindow(context.Background(), "done exploring")
	assert.False(t, ok, "no signal bound on this context")
}

// TestRequestNewWindow_ConsumedOnce pins the one-shot contract directly, at
// the signal layer, independent of CompactingModel: a request survives
// exactly one read. A second read on the same turn context — e.g. a second
// maybeCompact call within the same ReAct loop after the model already got
// its fresh window — must not re-trigger.
func TestRequestNewWindow_ConsumedOnce(t *testing.T) {
	ctx := WithNewWindowSignal(context.Background())

	ok := RequestNewWindow(ctx, "finished reading the big file")
	require.True(t, ok, "signal is bound, so the write must succeed")

	reason, got := consumeNewWindowRequest(ctx)
	assert.True(t, got)
	assert.Equal(t, "finished reading the big file", reason)

	_, got = consumeNewWindowRequest(ctx)
	assert.False(t, got, "a second read on the same request must find nothing — one-shot")
}

// TestCompactingModel_NewWindowRequestBypassesThreshold is W-C-14's core
// behavioral pin: a pending request wins even on a history shouldCompact
// would otherwise leave alone (huge ContextWindow, small history), AND it
// never calls the inner model at all — the tool's whole point is skipping
// summarization, not merely rushing it forward.
func TestCompactingModel_NewWindowRequestBypassesThreshold(t *testing.T) {
	inner := &recordingModel{reply: "answer", streamOK: true}
	cm := &CompactingModel{
		Inner:         inner,
		Threshold:     0.8,
		ContextWindow: 1_000_000, // shouldCompact would say no on this history
		KeepRecent:    2,
	}
	msgs := []*schema.Message{
		{Role: schema.User, Content: "task"},
		bigMessage(200),
		bigMessage(200),
		{Role: schema.User, Content: "recent"},
	}
	require.False(t, cm.shouldCompact(msgs), "premise: the threshold gate alone would say no")

	ctx := WithNewWindowSignal(context.Background())
	require.True(t, RequestNewWindow(ctx, "finished the exploratory read"))

	out, did := cm.maybeCompact(ctx, msgs)
	assert.True(t, did, "a pending request bypasses the threshold gate")
	assert.Less(t, len(out), len(msgs), "history actually shrank")
	assert.Equal(t, 0, inner.callCount(), "no summary call — that is the entire point of this path")
}

// TestCompactingModel_NewWindowRequestIsOneShot proves the bypass in
// TestCompactingModel_NewWindowRequestBypassesThreshold does not linger: the
// NEXT maybeCompact call on the same turn context, with the same
// under-threshold history and no new request, must fall through to the
// ordinary threshold gate (which still says no) rather than re-triggering.
func TestCompactingModel_NewWindowRequestIsOneShot(t *testing.T) {
	inner := &recordingModel{reply: "answer", streamOK: true}
	cm := &CompactingModel{
		Inner:         inner,
		Threshold:     0.8,
		ContextWindow: 1_000_000,
		KeepRecent:    2,
	}
	msgs := []*schema.Message{
		{Role: schema.User, Content: "task"},
		bigMessage(200),
		bigMessage(200),
		{Role: schema.User, Content: "recent"},
	}
	ctx := WithNewWindowSignal(context.Background())
	require.True(t, RequestNewWindow(ctx, "reason"))

	_, did := cm.maybeCompact(ctx, msgs)
	require.True(t, did, "premise: the first call consumes the request")

	out, did := cm.maybeCompact(ctx, msgs)
	assert.False(t, did, "the request was already consumed — this call must fall through to shouldCompact")
	assert.Len(t, out, len(msgs), "and shouldCompact still says no on this history")
}

// TestCompactingModel_NewWindowRequestFiresNoticeOnCallback proves the
// client-visible half of "开窗被记录": a bound OnCompact callback receives
// exactly ctxcompact.NewWindowNotice — not ctxcompact.FallbackNotice, not
// silence — so internal/cli/tui/model.go's compact_chunk switch can render
// the model-requested wording rather than the model-failure wording.
func TestCompactingModel_NewWindowRequestFiresNoticeOnCallback(t *testing.T) {
	inner := &recordingModel{reply: "answer", streamOK: true}
	cm := &CompactingModel{
		Inner:         inner,
		Threshold:     0.8,
		ContextWindow: 1_000_000,
		KeepRecent:    1,
	}
	msgs := []*schema.Message{
		{Role: schema.User, Content: "task"},
		bigMessage(200),
		{Role: schema.User, Content: "recent"},
	}
	ctx := WithNewWindowSignal(context.Background())
	require.True(t, RequestNewWindow(ctx, "reason"))
	var got []string
	ctx = WithCompactCallback(ctx, func(chunk string) { got = append(got, chunk) })

	_, did := cm.maybeCompact(ctx, msgs)
	require.True(t, did)
	assert.Equal(t, []string{ctxcompact.NewWindowNotice}, got)
}

// TestContextBudgetFromContext_NoSignalBound mirrors
// TestRequestNewWindow_NoSignalBound for the W-C-11 read side: a context that
// never called WithContextBudgetSignal (a sub-agent turn, most tests, any
// call site predating this wiring) must report "not available" rather than a
// zero-value snapshot that looks like a real "nothing left" answer.
func TestContextBudgetFromContext_NoSignalBound(t *testing.T) {
	_, ok := ContextBudgetFromContext(context.Background())
	assert.False(t, ok, "no signal bound on this context")
}

// TestRecordContextBudget_AgreesWithDirectCtxcompactCall is W-C-11's core
// pin: recordContextBudget must publish EXACTLY what calling
// ctxcompact.EstimateTokens/RemainingBudget directly on the same msgs/window
// would produce, across a table of windows and history sizes. A future
// change that re-derives these numbers with different arithmetic — even
// arithmetic that looks equivalent — would very likely disagree with the
// direct call on at least one row, which is what this test exists to catch.
func TestRecordContextBudget_AgreesWithDirectCtxcompactCall(t *testing.T) {
	cases := []struct {
		name   string
		window int
		n      int
	}{
		{"comfortably under budget", 10_000, 5},
		{"comfortably over budget", 1_000, 100},
		{"empty history", 1_000, 0},
		{"large window many messages", 200_000, 40},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msgs := make([]*schema.Message, tc.n)
			for i := range msgs {
				msgs[i] = bigMessage(50)
			}

			ctx := WithContextBudgetSignal(context.Background())
			recordContextBudget(ctx, msgs, tc.window)

			got, ok := ContextBudgetFromContext(ctx)
			require.True(t, ok, "recordContextBudget must publish a snapshot when a signal is bound")

			wantUsed := ctxcompact.EstimateTokens(msgs)
			wantRemaining := ctxcompact.RemainingBudget(msgs, ctxcompact.RunOpts{ModelWindow: tc.window})
			assert.Equal(t, tc.window, got.Window)
			assert.Equal(t, wantUsed, got.Used, "Used must be exactly ctxcompact.EstimateTokens, not a re-derived count")
			assert.Equal(t, wantRemaining, got.Remaining, "Remaining must be exactly ctxcompact.RemainingBudget, not a re-derived number")
		})
	}
}

// TestRecordContextBudget_NoSignalBoundIsNoop proves the write side is
// equally nil-safe: calling recordContextBudget on a context with no bound
// signal (the overwhelming majority of call sites — sub-agent turns, unit
// tests that construct a CompactingModel directly) must not panic and must
// leave nothing behind to read.
func TestRecordContextBudget_NoSignalBoundIsNoop(t *testing.T) {
	ctx := context.Background()
	require.NotPanics(t, func() {
		recordContextBudget(ctx, []*schema.Message{bigMessage(10)}, 1000)
	})
	_, ok := ContextBudgetFromContext(ctx)
	assert.False(t, ok)
}

// TestRecordContextBudget_NonPositiveWindowIsNoop mirrors
// ctxcompact.RemainingBudget's own "0 means unbudgeted" convention: a turn
// whose model has no configured context window publishes nothing, rather
// than a snapshot claiming a window of 0.
func TestRecordContextBudget_NonPositiveWindowIsNoop(t *testing.T) {
	ctx := WithContextBudgetSignal(context.Background())
	recordContextBudget(ctx, []*schema.Message{bigMessage(10)}, 0)
	_, ok := ContextBudgetFromContext(ctx)
	assert.False(t, ok, "a non-positive window must not publish a snapshot")
}

// TestCompactingModel_MaybeCompactPublishesContextBudgetEveryIteration proves
// the wiring inside maybeCompact itself: the snapshot is published even on an
// iteration where shouldCompact says no (small history, huge window) — the
// context_budget tool must be able to answer "how much room is left" on every
// turn, not only the turns that happened to trigger a summarization.
func TestCompactingModel_MaybeCompactPublishesContextBudgetEveryIteration(t *testing.T) {
	inner := &recordingModel{reply: "answer", streamOK: true}
	cm := &CompactingModel{
		Inner:         inner,
		Threshold:     0.8,
		ContextWindow: 1_000_000,
		KeepRecent:    2,
	}
	msgs := []*schema.Message{
		{Role: schema.User, Content: "task"},
		bigMessage(200),
		{Role: schema.User, Content: "recent"},
	}
	require.False(t, cm.shouldCompact(msgs), "premise: this history does not trigger compaction")

	ctx := WithContextBudgetSignal(context.Background())
	_, did := cm.maybeCompact(ctx, msgs)
	require.False(t, did, "compaction did not fire")

	got, ok := ContextBudgetFromContext(ctx)
	require.True(t, ok, "a snapshot must be published even when compaction does not fire")
	wantRemaining := ctxcompact.RemainingBudget(msgs, ctxcompact.RunOpts{ModelWindow: cm.ContextWindow})
	assert.Equal(t, wantRemaining, got.Remaining)
	assert.Equal(t, cm.ContextWindow, got.Window)
}
