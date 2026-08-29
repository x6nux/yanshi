package eino

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func fastEinoCfg() ResilientConfig {
	return ResilientConfig{MaxRetries: 3, BaseDelay: time.Millisecond, MaxDelay: 5 * time.Millisecond}
}

func TestResilientModel_RetriesThenSucceeds(t *testing.T) {
	// fake that fails twice (retryable) then succeeds
	var calls int32
	f := newScriptedModel([]bool{true, true, false}, &calls) // true=fail(retryable)
	r, err := NewResilientModel([]model.BaseChatModel{f}, fastEinoCfg())
	require.NoError(t, err)
	out, err := r.Generate(context.Background(), []*schema.Message{schema.UserMessage("x")})
	require.NoError(t, err)
	assert.Equal(t, "ok", out.Content)
	assert.Equal(t, int32(3), atomic.LoadInt32(&calls))
}

func TestResilientModel_FailoverToNext(t *testing.T) {
	var badCalls, goodCalls int32
	bad := newScriptedModel([]bool{true, true, true, true}, &badCalls)
	good := newScriptedModel([]bool{false}, &goodCalls)
	r, err := NewResilientModel([]model.BaseChatModel{bad, good}, fastEinoCfg())
	require.NoError(t, err)
	out, err := r.Generate(context.Background(), nil)
	require.NoError(t, err)
	assert.Equal(t, "ok", out.Content)
	assert.Equal(t, int32(4), atomic.LoadInt32(&badCalls)) // MaxRetries+1
	assert.Equal(t, int32(1), atomic.LoadInt32(&goodCalls))
}

func TestResilientModel_EmptyChain(t *testing.T) {
	_, err := NewResilientModel(nil, fastEinoCfg())
	require.Error(t, err)
}

func TestResilientModel_DefaultMaxRetries(t *testing.T) {
	// A zero-value config should default to MaxRetries=10, not 0.
	var calls int32
	fails := make([]bool, 11) // fail across all 1 initial + 10 retries
	for i := range fails {
		fails[i] = true
	}
	f := newScriptedModel(fails, &calls) // always fails retryable
	r, err := NewResilientModel([]model.BaseChatModel{f}, ResilientConfig{})
	require.NoError(t, err)
	assert.Equal(t, 10, r.cfg.MaxRetries)

	_, err = r.Generate(context.Background(), []*schema.Message{schema.UserMessage("x")})
	require.Error(t, err)
	// MaxRetries=10 means 11 attempts (1 initial + 10 retries).
	assert.Equal(t, int32(11), atomic.LoadInt32(&calls))
}

// ---------------------------------------------------------------------------
// test helper: scriptedModel
// ---------------------------------------------------------------------------

// scriptedModel is a model.BaseChatModel that increments *calls on each
// Generate and returns a RetryableModelError when fails[i] is true, else
// schema.AssistantMessage("ok", nil).
type scriptedModel struct {
	fails []bool
	calls *int32
}

func newScriptedModel(fails []bool, calls *int32) *scriptedModel {
	return &scriptedModel{fails: fails, calls: calls}
}

func (m *scriptedModel) Generate(_ context.Context, _ []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	i := int(atomic.AddInt32(m.calls, 1)) - 1
	if i < len(m.fails) && m.fails[i] {
		return nil, &RetryableModelError{Err: errors.New("transient")}
	}
	return schema.AssistantMessage("ok", nil), nil
}

func (m *scriptedModel) Stream(ctx context.Context, in []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	msg, err := m.Generate(ctx, in, opts...)
	if err != nil {
		return nil, err
	}
	return schema.StreamReaderFromArray[*schema.Message]([]*schema.Message{msg}), nil
}

// ---------------------------------------------------------------------------
// test helper: scriptedSeqModel (for empty-response retry tests)
// ---------------------------------------------------------------------------

// scriptedSeqModel returns a scripted sequence of messages on successive calls
// (Generate or Stream), incrementing *calls each time. Unlike scriptedModel it
// never errors — it returns empty assistant messages where the script says so,
// letting the empty-response retry path be exercised. Calls past the end of the
// script keep returning the last scripted message (so an "always empty" model
// can be expressed with a single-element []*schema.Message{empty}).
type scriptedSeqModel struct {
	msgs  []*schema.Message
	calls *int32
}

func (m *scriptedSeqModel) Generate(_ context.Context, _ []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	i := int(atomic.AddInt32(m.calls, 1)) - 1
	if i < len(m.msgs) {
		return m.msgs[i], nil
	}
	return m.msgs[len(m.msgs)-1], nil
}

func (m *scriptedSeqModel) Stream(ctx context.Context, in []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	msg, err := m.Generate(ctx, in, opts...)
	if err != nil {
		return nil, err
	}
	return schema.StreamReaderFromArray[*schema.Message]([]*schema.Message{msg}), nil
}

var _ model.BaseChatModel = (*scriptedSeqModel)(nil)

// ---------------------------------------------------------------------------
// isEmpty unit tests
// ---------------------------------------------------------------------------

func TestIsEmpty(t *testing.T) {
	empty := schema.AssistantMessage("", nil)
	nonEmpty := schema.AssistantMessage("x", nil)
	toolMsg := schema.ToolMessage("r", "c1")
	assert.True(t, isEmpty(empty), "assistant with no content/toolcalls is empty")
	assert.False(t, isEmpty(nonEmpty), "content present -> not empty")
	assert.False(t, isEmpty(toolMsg), "tool messages are never empty")
	assert.False(t, isEmpty(nil), "nil is absent, not empty")
	// Assistant with tool calls but no text is NOT empty (it's a tool-call turn).
	withCalls := schema.AssistantMessage("", []schema.ToolCall{{ID: "c1", Type: "function", Function: schema.FunctionCall{Name: "f"}}})
	assert.False(t, isEmpty(withCalls), "tool-call turn is not empty")
}

// ---------------------------------------------------------------------------
// Generate: empty-response retry
// ---------------------------------------------------------------------------

// TestResilientModel_GenerateRetriesEmptyThenSucceeds proves a provider that
// returns N empty messages then a real one is retried up to MaxEmptyRetries and
// ultimately succeeds (call count = empty+1).
func TestResilientModel_GenerateRetriesEmptyThenSucceeds(t *testing.T) {
	var calls int32
	empty := schema.AssistantMessage("", nil)
	real := schema.AssistantMessage("ok", nil)
	f := &scriptedSeqModel{msgs: []*schema.Message{empty, empty, real}, calls: &calls}
	r, err := NewResilientModel([]model.BaseChatModel{f}, fastEinoCfg())
	require.NoError(t, err)

	out, err := r.Generate(context.Background(), []*schema.Message{schema.UserMessage("x")})
	require.NoError(t, err)
	assert.Equal(t, "ok", out.Content)
	assert.Equal(t, int32(3), atomic.LoadInt32(&calls), "2 empty + 1 real = 3 calls")
}

// TestResilientModel_GenerateAllEmptyErrors proves a provider that always
// returns empty exhausts MaxEmptyRetries (default 10) and yields a clear error.
// Total calls = 1 initial + 10 retries = 11.
func TestResilientModel_GenerateAllEmptyErrors(t *testing.T) {
	var calls int32
	empty := schema.AssistantMessage("", nil)
	f := &scriptedSeqModel{msgs: []*schema.Message{empty}, calls: &calls}
	r, err := NewResilientModel([]model.BaseChatModel{f}, fastEinoCfg())
	require.NoError(t, err)
	require.Equal(t, 10, r.cfg.MaxEmptyRetries, "MaxEmptyRetries defaults to 10")

	_, err = r.Generate(context.Background(), []*schema.Message{schema.UserMessage("x")})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty response after 10 retries")
	assert.Equal(t, int32(11), atomic.LoadInt32(&calls), "1 initial + 10 empty retries")
}

// TestResilientModel_GenerateRespectsMaxEmptyRetries proves an explicit
// MaxEmptyRetries is honored (not just the default).
func TestResilientModel_GenerateRespectsMaxEmptyRetries(t *testing.T) {
	var calls int32
	empty := schema.AssistantMessage("", nil)
	f := &scriptedSeqModel{msgs: []*schema.Message{empty}, calls: &calls}
	cfg := fastEinoCfg()
	cfg.MaxEmptyRetries = 2
	r, err := NewResilientModel([]model.BaseChatModel{f}, cfg)
	require.NoError(t, err)

	_, err = r.Generate(context.Background(), []*schema.Message{schema.UserMessage("x")})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty response after 2 retries")
	assert.Equal(t, int32(3), atomic.LoadInt32(&calls), "1 initial + 2 empty retries")
}

// ---------------------------------------------------------------------------
// Stream: empty-stream retry
// ---------------------------------------------------------------------------

// TestResilientModel_StreamRetriesEmptyThenSucceeds proves an empty stream is
// retried and a subsequent non-empty stream is forwarded intact (content +
// call count). The replayed reader must yield the real message.
func TestResilientModel_StreamRetriesEmptyThenSucceeds(t *testing.T) {
	var calls int32
	empty := schema.AssistantMessage("", nil)
	real := schema.AssistantMessage("ok", nil)
	f := &scriptedSeqModel{msgs: []*schema.Message{empty, empty, real}, calls: &calls}
	r, err := NewResilientModel([]model.BaseChatModel{f}, fastEinoCfg())
	require.NoError(t, err)

	sr, err := r.Stream(context.Background(), []*schema.Message{schema.UserMessage("x")})
	require.NoError(t, err)
	defer sr.Close()

	var got string
	for {
		msg, err := sr.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(t, err)
		got += msg.Content
	}
	assert.Equal(t, "ok", got)
	assert.Equal(t, int32(3), atomic.LoadInt32(&calls), "2 empty + 1 real = 3 Stream calls")
}

// TestResilientModel_StreamAllEmptyErrors proves a perpetually-empty stream
// exhausts MaxEmptyRetries and yields a clear error.
func TestResilientModel_StreamAllEmptyErrors(t *testing.T) {
	var calls int32
	empty := schema.AssistantMessage("", nil)
	f := &scriptedSeqModel{msgs: []*schema.Message{empty}, calls: &calls}
	r, err := NewResilientModel([]model.BaseChatModel{f}, fastEinoCfg())
	require.NoError(t, err)

	sr, err := r.Stream(context.Background(), []*schema.Message{schema.UserMessage("x")})
	require.NoError(t, err)
	defer sr.Close()

	// Stream always returns a reader; retries happen as the consumer drains it,
	// so the empty-exhaustion error surfaces on Recv, not from Stream itself.
	var recvErr error
	for {
		_, e := sr.Recv()
		if e != nil {
			recvErr = e
			break
		}
	}
	require.Error(t, recvErr)
	assert.Contains(t, recvErr.Error(), "empty stream after 10 retries")
	// 1 initial + 10 retries = 11 attempts; single provider → 11 Stream calls.
	assert.Equal(t, int32(11), atomic.LoadInt32(&calls))
}

// TestResilientModel_StreamPreservesMultiChunkContent proves the replayed
// reader forwards EVERY chunk of a multi-message stream (not just the peeked
// first one) — i.e. forwardStream correctly chains peeked + rest.
func TestResilientModel_StreamPreservesMultiChunkContent(t *testing.T) {
	var calls int32
	// A single Stream call yields a 3-chunk stream ("hel"+"lo"+"!"). The
	// resilient wrapper must forward all three.
	real := schema.AssistantMessage("ok", nil) // placeholder (not used by multi)
	_ = real
	// Build a custom single-shot model whose Stream returns 3 chunks.
	multi := &multiChunkModel{chunks: []*schema.Message{
		schema.AssistantMessage("hel", nil),
		schema.AssistantMessage("lo", nil),
		schema.AssistantMessage("!", nil),
	}, calls: &calls}
	r, err := NewResilientModel([]model.BaseChatModel{multi}, fastEinoCfg())
	require.NoError(t, err)

	sr, err := r.Stream(context.Background(), []*schema.Message{schema.UserMessage("x")})
	require.NoError(t, err)
	defer sr.Close()

	var got string
	for {
		msg, err := sr.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(t, err)
		got += msg.Content
	}
	assert.Equal(t, "hello!", got, "all chunks must be forwarded in order")
	assert.Equal(t, int32(1), atomic.LoadInt32(&calls), "single non-empty stream = 1 call")
}

// multiChunkModel is a single-shot model whose one Stream call yields the given
// chunks in order (then EOF on subsequent calls). Used to prove forwardStream
// replays peeked + rest across multiple chunks.
type multiChunkModel struct {
	chunks []*schema.Message
	calls  *int32
}

func (m *multiChunkModel) Generate(_ context.Context, _ []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	atomic.AddInt32(m.calls, 1)
	if len(m.chunks) > 0 {
		return m.chunks[0], nil
	}
	return schema.AssistantMessage("", nil), nil
}

func (m *multiChunkModel) Stream(_ context.Context, _ []*schema.Message, _ ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	atomic.AddInt32(m.calls, 1)
	return schema.StreamReaderFromArray[*schema.Message](m.chunks), nil
}

var _ model.BaseChatModel = (*multiChunkModel)(nil)

// unused import guard (io kept for potential future stream-error tests)
var _ = io.EOF

var _ model.BaseChatModel = (*scriptedModel)(nil)

// ---------------------------------------------------------------------------
// Stream: mid-stream-error retry (the "failed to receive stream chunk: EOF" case)
// ---------------------------------------------------------------------------

// streamReaderFrom builds a reader that yields msgs in order then either a clean
// EOF (sendErr == nil) or a stream error (sendErr != nil). Used to simulate a
// provider stream that drops mid-way.
func streamReaderFrom(msgs []*schema.Message, sendErr error) *schema.StreamReader[*schema.Message] {
	sr, sw := schema.Pipe[*schema.Message](1)
	go func() {
		defer sw.Close()
		for _, m := range msgs {
			if sw.Send(m, nil) {
				return // consumer closed
			}
		}
		if sendErr != nil {
			_ = sw.Send(nil, sendErr)
		}
	}()
	return sr
}

// flakyStreamModel yields a partial stream that errors mid-way on the first
// call, then a complete stream on the next. It emulates a gateway that drops
// the upstream connection ("unexpected EOF") after delivering some tokens.
type flakyStreamModel struct {
	calls *int32
}

func (m *flakyStreamModel) Generate(_ context.Context, _ []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	return schema.AssistantMessage("ok", nil), nil
}

func (m *flakyStreamModel) Stream(_ context.Context, _ []*schema.Message, _ ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	n := atomic.AddInt32(m.calls, 1)
	if n == 1 {
		// Partial content, then a mid-stream unexpected EOF.
		return streamReaderFrom([]*schema.Message{schema.AssistantMessage("hel", nil)}, io.ErrUnexpectedEOF), nil
	}
	// Retry regenerates the full response from the start.
	return streamReaderFrom([]*schema.Message{schema.AssistantMessage("hello world", nil)}, nil), nil
}

var _ model.BaseChatModel = (*flakyStreamModel)(nil)

// TestResilientModel_StreamRetriesMidStreamEOFThenSucceeds proves a mid-stream
// "unexpected EOF" (the acl's "failed to receive stream chunk" failure) is
// retried: attempt 1 delivers "hel" then EOFs; attempt 2 regenerates "hello
// world" and is re-fed IN FULL (overwrite semantics — the consumer discards the
// "hel" partial on the retry callback). The raw stream therefore carries the
// partial followed by the full content; this test asserts the full content is
// delivered (the consumer-facing overwrite is verified at the WS/TUI level).
func TestResilientModel_StreamRetriesMidStreamEOFThenSucceeds(t *testing.T) {
	var calls int32
	f := &flakyStreamModel{calls: &calls}
	r, err := NewResilientModel([]model.BaseChatModel{f}, fastEinoCfg())
	require.NoError(t, err)

	sr, err := r.Stream(context.Background(), []*schema.Message{schema.UserMessage("x")})
	require.NoError(t, err)
	defer sr.Close()

	var got string
	for {
		msg, e := sr.Recv()
		if errors.Is(e, io.EOF) {
			break
		}
		require.NoError(t, e)
		got += msg.Content
	}
	assert.Contains(t, got, "hello world", "the full regenerated content is re-fed after the retry")
	assert.Equal(t, int32(2), atomic.LoadInt32(&calls), "1 failed + 1 retry = 2 Stream calls")
}

// TestResilientModel_StreamRetriesClientTimeout proves a mid-stream
// "net/http: request canceled (Client.Timeout …)" is retried — even though the
// error wraps context.Canceled (as Go's transport does for a client timeout),
// the TURN context is still alive, so the new ctx.Err()-based check treats it as
// transient. The old errors.Is(err, context.Canceled) check would have
// suppressed this retry.
func TestResilientModel_StreamRetriesClientTimeout(t *testing.T) {
	// Simulate the real error shape: the acl wraps the transport error, which
	// itself wraps context.Canceled (from the request's derived context).
	timeoutErr := fmt.Errorf("failed to receive stream chunk: %w",
		fmt.Errorf("net/http: request canceled (Client.Timeout or context cancellation while reading body): %w",
			context.Canceled))

	var calls int32
	mdl := &errorThenOKModel{calls: &calls, err: timeoutErr}
	r, err := NewResilientModel([]model.BaseChatModel{mdl}, fastEinoCfg())
	require.NoError(t, err)

	sr, err := r.Stream(context.Background(), []*schema.Message{schema.UserMessage("x")})
	require.NoError(t, err)
	defer sr.Close()

	var got string
	for {
		msg, e := sr.Recv()
		if errors.Is(e, io.EOF) {
			break
		}
		require.NoError(t, e, "the client-timeout error must be retried, not surfaced")
		got += msg.Content
	}
	assert.Contains(t, got, "hello world")
	assert.Equal(t, int32(2), atomic.LoadInt32(&calls), "1 failed + 1 retry = 2 Stream calls")
}

// TestResilientModel_StreamNoRetryOnTurnCancel proves a REAL turn-context
// cancellation (user hit Ctrl-C, or a turn deadline fired) is NOT retried: the
// error surfaces immediately and Stream is called exactly once.
func TestResilientModel_StreamNoRetryOnTurnCancel(t *testing.T) {
	var calls int32
	mdl := &errorThenOKModel{calls: &calls, err: io.ErrUnexpectedEOF}
	r, err := NewResilientModel([]model.BaseChatModel{mdl}, fastEinoCfg())
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel BEFORE streaming → the turn is done

	sr, err := r.Stream(ctx, []*schema.Message{schema.UserMessage("x")})
	require.NoError(t, err)
	defer sr.Close()

	_, e := sr.Recv()
	assert.Error(t, e, "a canceled turn must surface the error, not retry")
	assert.NotEqual(t, int32(2), atomic.LoadInt32(&calls), "must not retry a canceled turn")
}

// errorThenOKModel yields a partial stream that errors mid-way with err on the
// first call, then a complete stream on the next. Used to exercise the
// retry/no-retry split for specific error shapes.
type errorThenOKModel struct {
	calls *int32
	err   error
}

func (m *errorThenOKModel) Generate(_ context.Context, _ []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	return schema.AssistantMessage("ok", nil), nil
}

func (m *errorThenOKModel) Stream(_ context.Context, _ []*schema.Message, _ ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	n := atomic.AddInt32(m.calls, 1)
	if n == 1 {
		return streamReaderFrom([]*schema.Message{schema.AssistantMessage("hel", nil)}, m.err), nil
	}
	return streamReaderFrom([]*schema.Message{schema.AssistantMessage("hello world", nil)}, nil), nil
}

var _ model.BaseChatModel = (*errorThenOKModel)(nil)

// TestResilientModel_StreamRetryProgressCallback proves the WithRetryCallback
// fires before each backoff sleep with the attempt index, the cap, the
// triggering error, and a positive delay.
func TestResilientModel_StreamRetryProgressCallback(t *testing.T) {
	var calls int32
	f := &flakyStreamModel{calls: &calls}
	r, err := NewResilientModel([]model.BaseChatModel{f}, fastEinoCfg())
	require.NoError(t, err)

	var snap struct {
		attempt, max int
		err          error
		delay        time.Duration
		fired        bool
	}
	ctx := WithRetryCallback(context.Background(), func(attempt, maxAttempts int, err error, delay time.Duration) {
		snap.attempt, snap.max, snap.err, snap.delay, snap.fired = attempt, maxAttempts, err, delay, true
	})
	sr, err := r.Stream(ctx, []*schema.Message{schema.UserMessage("x")})
	require.NoError(t, err)
	defer sr.Close()
	for {
		_, e := sr.Recv()
		if e != nil {
			break
		}
	}
	assert.True(t, snap.fired, "retry callback must fire")
	assert.Equal(t, 1, snap.attempt)
	assert.Equal(t, 3, snap.max, "max is MaxRetries (fastEinoCfg=3)")
	assert.NotNil(t, snap.err)
	assert.Contains(t, snap.err.Error(), "unexpected EOF")
	assert.Greater(t, snap.delay, time.Duration(0))
}

// toolThenDropModel delivers a tool call, then errors mid-stream. Because a
// tool call has been delivered, the mid-stream error must NOT be retried
// (retrying would duplicate the tool call) — it should propagate instead.
type toolThenDropModel struct {
	calls *int32
}

func (m *toolThenDropModel) Generate(_ context.Context, _ []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	return schema.AssistantMessage("ok", nil), nil
}

func (m *toolThenDropModel) Stream(_ context.Context, _ []*schema.Message, _ ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	atomic.AddInt32(m.calls, 1)
	withTool := schema.AssistantMessage("", []schema.ToolCall{
		{ID: "c1", Type: "function", Function: schema.FunctionCall{Name: "fs_read"}},
	})
	return streamReaderFrom([]*schema.Message{withTool}, io.ErrUnexpectedEOF), nil
}

var _ model.BaseChatModel = (*toolThenDropModel)(nil)

// TestResilientModel_StreamNoRetryAfterToolCall proves that once a tool call has
// been delivered, a subsequent mid-stream error is NOT retried (it would
// duplicate the tool call); the error propagates to the consumer instead.
func TestResilientModel_StreamNoRetryAfterToolCall(t *testing.T) {
	var calls int32
	f := &toolThenDropModel{calls: &calls}
	r, err := NewResilientModel([]model.BaseChatModel{f}, fastEinoCfg())
	require.NoError(t, err)

	sr, err := r.Stream(context.Background(), []*schema.Message{schema.UserMessage("x")})
	require.NoError(t, err)
	defer sr.Close()

	var sawTool, sawErr bool
	for {
		msg, e := sr.Recv()
		if e != nil {
			require.Error(t, e)
			require.False(t, errors.Is(e, io.EOF), "the mid-stream error must propagate, not a clean EOF")
			sawErr = true
			break
		}
		if msg != nil && len(msg.ToolCalls) > 0 {
			sawTool = true
		}
	}
	assert.True(t, sawTool, "the tool call was delivered before the drop")
	assert.True(t, sawErr, "the mid-stream error propagated (not retried)")
	assert.Equal(t, int32(1), atomic.LoadInt32(&calls), "no retry after a delivered tool call")
}

// ---------------------------------------------------------------------------
// Stream: reasoning-only response is treated as incomplete and retried
// ---------------------------------------------------------------------------

// reasoningMsg is an assistant message carrying only reasoning (no content,
// no tool calls) — the "thought but produced nothing" shape.
func reasoningMsg(s string) *schema.Message {
	return &schema.Message{Role: schema.Assistant, ReasoningContent: s}
}

// reasoningOnlyThenFullModel yields a reasoning-only stream on the first call
// (no content → incomplete), then the same reasoning plus real content on the
// retry. Emulates a model that thinks but forgets to answer, then recovers.
type reasoningOnlyThenFullModel struct {
	calls *int32
}

func (m *reasoningOnlyThenFullModel) Generate(_ context.Context, _ []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	return schema.AssistantMessage("ok", nil), nil
}

func (m *reasoningOnlyThenFullModel) Stream(_ context.Context, _ []*schema.Message, _ ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	n := atomic.AddInt32(m.calls, 1)
	if n == 1 {
		return streamReaderFrom([]*schema.Message{reasoningMsg("thinking...")}, nil), nil
	}
	return streamReaderFrom([]*schema.Message{
		reasoningMsg("thinking..."),
		schema.AssistantMessage("answer", nil),
	}, nil), nil
}

var _ model.BaseChatModel = (*reasoningOnlyThenFullModel)(nil)

// TestResilientModel_StreamRetriesReasoningOnlyThenSucceeds proves a response
// that is reasoning-only (no content, no tool calls) is retried as incomplete:
// attempt 1 shows "thinking…" then ends with no answer; attempt 2 reproduces
// "thinking…" and adds "answer". With overwrite semantics the regenerated
// stream is re-fed in full (reasoning appears again in the raw stream); this
// test asserts the answer content is produced. The consumer discards the
// first reasoning partial on the retry callback (verified at the TUI level).
func TestResilientModel_StreamRetriesReasoningOnlyThenSucceeds(t *testing.T) {
	var calls int32
	f := &reasoningOnlyThenFullModel{calls: &calls}
	r, err := NewResilientModel([]model.BaseChatModel{f}, fastEinoCfg())
	require.NoError(t, err)

	sr, err := r.Stream(context.Background(), []*schema.Message{schema.UserMessage("x")})
	require.NoError(t, err)
	defer sr.Close()

	var content, reasoning string
	for {
		msg, e := sr.Recv()
		if errors.Is(e, io.EOF) {
			break
		}
		require.NoError(t, e)
		content += msg.Content
		reasoning += msg.ReasoningContent
	}
	assert.Equal(t, "answer", content, "the retried response produced real content")
	assert.Contains(t, reasoning, "thinking...", "the regenerated reasoning is present")
	assert.Equal(t, int32(2), atomic.LoadInt32(&calls), "1 incomplete + 1 retry = 2 Stream calls")
}

// alwaysReasoningModel never produces content — only reasoning — so every
// attempt is incomplete and retries exhaust MaxEmptyRetries.
type alwaysReasoningModel struct {
	calls *int32
}

func (m *alwaysReasoningModel) Generate(_ context.Context, _ []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	return schema.AssistantMessage("ok", nil), nil
}

func (m *alwaysReasoningModel) Stream(_ context.Context, _ []*schema.Message, _ ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	atomic.AddInt32(m.calls, 1)
	return streamReaderFrom([]*schema.Message{reasoningMsg("just thinking, no answer")}, nil), nil
}

var _ model.BaseChatModel = (*alwaysReasoningModel)(nil)

// TestResilientModel_StreamAllReasoningErrors proves a perpetually
// reasoning-only stream exhausts retries and yields a clear error (it is
// treated like an empty stream, not a successful one).
func TestResilientModel_StreamAllReasoningErrors(t *testing.T) {
	var calls int32
	f := &alwaysReasoningModel{calls: &calls}
	r, err := NewResilientModel([]model.BaseChatModel{f}, fastEinoCfg())
	require.NoError(t, err)

	sr, err := r.Stream(context.Background(), []*schema.Message{schema.UserMessage("x")})
	require.NoError(t, err)
	defer sr.Close()

	var recvErr error
	for {
		_, e := sr.Recv()
		if e != nil {
			recvErr = e
			break
		}
	}
	require.Error(t, recvErr, "reasoning-only-forever must error, not succeed")
	assert.Contains(t, recvErr.Error(), "empty stream after 10 retries")
	assert.Equal(t, int32(11), atomic.LoadInt32(&calls))
}

// ---------------------------------------------------------------------------
// 4xx client errors: must NOT retry (retry masks config/auth bugs)
// ---------------------------------------------------------------------------

// errModel always returns the same error on Generate/Stream, recording how many
// times it was called. Used to assert that an error wrapped as RetryableModelError
// (so it WOULD normally retry) is short-circuited when its text marks a real 4xx
// client error.
type errModel struct {
	err   error
	calls int
}

func (m *errModel) Generate(context.Context, []*schema.Message, ...model.Option) (*schema.Message, error) {
	m.calls++
	return nil, m.err
}

func (m *errModel) Stream(context.Context, []*schema.Message, ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	m.calls++
	return nil, m.err
}

var _ model.BaseChatModel = (*errModel)(nil)

// TestNonRetryableClientError proves a RetryableModelError whose text marks a
// real 4xx client error (invalid_api_key, 401, 404, …) is NOT retried — even
// though RetryableModelError would otherwise trigger retries. Without the
// isNonRetryableClientErr short-circuit, each marker below would retry and calls
// would exceed 1.
func TestNonRetryableClientError(t *testing.T) {
	for _, marker := range []string{"invalid_api_key", "model_not_found", "invalid_request_error", "401", "403", "404", "422"} {
		m := &errModel{err: &RetryableModelError{Err: fmt.Errorf("status: %s", marker)}}
		r, err := NewResilientModel([]model.BaseChatModel{m}, ResilientConfig{MaxRetries: 5, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond})
		if err != nil {
			t.Fatalf("NewResilientModel: %v", err)
		}
		_, _ = r.Generate(context.Background(), []*schema.Message{schema.UserMessage("hi")})
		if m.calls != 1 {
			t.Fatalf("marker %q 不应重试，实际 calls=%d", marker, m.calls)
		}
	}
}

// ---------------------------------------------------------------------------
// userCancelCtx: decoupling user-cancel from network-cancel
// ---------------------------------------------------------------------------

// cancelCtxModel returns a RetryableModelError on the first Generate so the
// retry path is entered, then succeeds on the second call. It is the minimal
// fixture for proving retry decisions watch the userCancelCtx, not the callCtx:
// with callCtx pre-canceled but userCancelCtx alive, the resilient layer must
// still retry (calls >= 2). Stream always errors so the same scenario can be
// extended to the stream path if needed.
type cancelCtxModel struct{ calls int }

func (m *cancelCtxModel) Generate(ctx context.Context, _ []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	m.calls++
	if m.calls == 1 {
		return nil, &RetryableModelError{Err: fmt.Errorf("unexpected EOF")}
	}
	return schema.AssistantMessage("ok", nil), nil
}

func (m *cancelCtxModel) Stream(ctx context.Context, _ []*schema.Message, _ ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	return nil, &RetryableModelError{Err: fmt.Errorf("unexpected EOF")}
}

// TestRetryWaitsOnUserCancelCtxNotCallCtx proves retry decisions and the
// backoff sleep watch the userCancelCtx (bound via WithUserCancelCtx), NOT the
// per-call ctx. The callCtx is pre-canceled (emulating a network/transport
// cancel of the request's own context), but userCancelCtx is still alive — so
// the resilient layer must retry the transient RetryableModelError instead of
// bailing out as if the user had pressed Ctrl-C.
//
// Pre-fix: sleepRetry's `<-ctx.Done()` and isRetryableStreamErr's
// `ctx.Err() != nil` both saw the canceled callCtx and returned immediately,
// yielding calls=1 (the bug: network cancel was misread as user cancel).
// Post-fix: both watch userCancelCtxFrom(ctx), which is alive, so the retry
// proceeds and calls>=2.
func TestRetryWaitsOnUserCancelCtxNotCallCtx(t *testing.T) {
	m := &cancelCtxModel{}
	r, err := NewResilientModel([]model.BaseChatModel{m}, ResilientConfig{MaxRetries: 3, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	userCancelCtx := context.Background() // never canceled in this test
	ctx := WithUserCancelCtx(context.Background(), userCancelCtx)
	cancelledCallCtx, cancel := context.WithCancel(ctx)
	cancel() // emulate a network cancel of callCtx while userCancelCtx is alive

	_, _ = r.Generate(cancelledCallCtx, []*schema.Message{schema.UserMessage("hi")})
	if m.calls < 2 {
		t.Fatalf("期望重试（callCtx 取消不应停止重试），实际 calls=%d", m.calls)
	}
}

func TestWithRetryCallback_NilIsNoOp(t *testing.T) {
	ctx := context.Background()
	got := WithRetryCallback(ctx, nil)
	if got != ctx {
		t.Fatal("nil callback must return ctx unchanged")
	}
}

func TestWithUserCancelCtx_NilIsNoOp(t *testing.T) {
	ctx := context.Background()
	got := WithUserCancelCtx(ctx, nil)
	if got != ctx {
		t.Fatal("nil userCancelCtx must return ctx unchanged")
	}
}

func TestIsBlank_NilIsBlank(t *testing.T) {
	if !isBlank(nil) {
		t.Fatal("nil message must be blank")
	}
}

func TestIsBlank_NoUsageMetaIsBlank(t *testing.T) {
	msg := &schema.Message{
		Role:         schema.Assistant,
		ResponseMeta: &schema.ResponseMeta{},
	}
	if !isBlank(msg) {
		t.Fatal("message with empty ResponseMeta must be blank")
	}
}

func TestIsRetryableStreamErr_NilReturnsFalse(t *testing.T) {
	if isRetryableStreamErr(context.Background(), nil) {
		t.Fatal("nil err must not be retryable")
	}
}

func TestIsRetryableStreamErr_UserCancelReturnsFalse(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if isRetryableStreamErr(ctx, io.ErrUnexpectedEOF) {
		t.Fatal("user-canceled context must not retry")
	}
}

func TestIsRetryableStreamErr_NonRetryableClientErr(t *testing.T) {
	if isRetryableStreamErr(context.Background(), errors.New("401 Unauthorized")) {
		t.Fatal("4xx client errors must not be retryable in stream")
	}
}

func TestOpenStreamChain_AllFail(t *testing.T) {
	r, err := NewResilientModel([]model.BaseChatModel{&errModel{err: errors.New("fail1")}}, fastEinoCfg())
	if err != nil {
		t.Fatal(err)
	}
	sr, err := r.Stream(context.Background(), []*schema.Message{schema.UserMessage("x")})
	if err != nil {
		t.Fatal(err)
	}
	defer sr.Close()
	_, e := sr.Recv()
	if e == nil {
		t.Fatal("all providers failed stream setup must propagate an error")
	}
}

func TestUnwrapRetryableError(t *testing.T) {
	inner := errors.New("inner")
	re := &RetryableModelError{Err: inner}
	if !errors.Is(re, inner) {
		t.Fatal("errors.Is must traverse Unwrap")
	}
}

// ---------------------------------------------------------------------------
// hangingStreamModel: proves runStream wires a per-attempt cancellable
// context all the way into the provider chain, not just into the standalone
// watchdogReader unit exercised by streamwatchdog_test.go.
// ---------------------------------------------------------------------------

// hangingStreamModel mimics a gateway that accepts the connection and then
// sends nothing: Stream returns immediately and its goroutine writes nothing
// until its own ctx is cancelled — the same shape AnthropicModel,
// openaiResponsesModel (both built on http.NewRequestWithContext), and
// eino-ext's openai ACL client (ctx threaded into
// CreateChatCompletionStream) all share. unblocked closes the moment this
// model FIRST observes a ctx.Done(), which is the only signal a wiring bug
// in runStream (e.g. passing the turn ctx instead of the per-attempt one to
// openStreamChain) could break. A retrying caller invokes Stream once per
// attempt, each with its own per-attempt ctx, so the close is guarded by
// sync.Once rather than assuming exactly one call.
type hangingStreamModel struct {
	unblocked chan struct{}
	once      sync.Once
}

func newHangingStreamModel() *hangingStreamModel {
	return &hangingStreamModel{unblocked: make(chan struct{})}
}

func (m *hangingStreamModel) Generate(_ context.Context, _ []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	return schema.AssistantMessage("unused", nil), nil
}

func (m *hangingStreamModel) Stream(ctx context.Context, _ []*schema.Message, _ ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	sr, sw := schema.Pipe[*schema.Message](1)
	go func() {
		defer sw.Close()
		<-ctx.Done()
		m.once.Do(func() { close(m.unblocked) })
	}()
	return sr, nil
}

var _ model.BaseChatModel = (*hangingStreamModel)(nil)

// Not itself ledger evidence (A2/W-A-06#1's evidence citation stays on
// TestWatchdogFirstChunkTimeout) — this is the wiring half of the same fix:
// streamwatchdog_test.go pins that watchdogReader.Recv calls the cancel func
// it is GIVEN; this pins that runStream actually GIVES it a cancel tied to
// the context handed to the provider — the gap a stray plain `ctx` left in
// openStreamChain's call would slip through undetected by the unit tests
// alone.
func TestResilientModel_StreamIdleTimeoutCancelsProviderContext(t *testing.T) {
	m := newHangingStreamModel()
	cfg := fastEinoCfg()
	cfg.MaxRetries = 1 // NewResilientModel defaults MaxRetries<=0 to 10; keep this small but positive so the test stays fast even across two attempts
	cfg.FirstChunkTimeout = 20 * time.Millisecond
	r, err := NewResilientModel([]model.BaseChatModel{m}, cfg)
	require.NoError(t, err)

	sr, err := r.Stream(context.Background(), []*schema.Message{schema.UserMessage("x")})
	require.NoError(t, err)
	defer sr.Close()
	_, recvErr := sr.Recv()
	require.ErrorIs(t, recvErr, ErrStreamIdle)

	select {
	case <-m.unblocked:
	case <-time.After(time.Second):
		t.Fatal("provider's Stream ctx was never cancelled — runStream is not threading the watchdog's cancel into the chain")
	}
}

// ---------------------------------------------------------------------------
// W-C-07: per-provider MaxRetries
// ---------------------------------------------------------------------------

// TestMaxRetriesFor pins maxRetriesFor's contract directly: the -1 sentinel
// (not present, out of range, or explicitly -1) falls back to cfg.MaxRetries;
// any other value — including the legitimate "never retry" 0 — is used as-is.
func TestMaxRetriesFor(t *testing.T) {
	cases := []struct {
		name   string
		per    []int
		idx    int
		global int
		want   int
	}{
		{"nil slice falls back", nil, 0, 5, 5},
		{"empty slice falls back", []int{}, 0, 5, 5},
		{"sentinel -1 falls back", []int{-1, 2}, 0, 5, 5},
		{"explicit zero is honoured, not treated as unset", []int{0}, 0, 5, 0},
		{"explicit override is honoured", []int{7}, 0, 5, 7},
		{"index out of range falls back", []int{1}, 3, 5, 5},
		{"negative index (total-open-failure state) falls back", []int{1}, -1, 5, 5},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := newScriptedModel([]bool{false}, new(int32))
			r, err := NewResilientModel([]model.BaseChatModel{m}, ResilientConfig{
				MaxRetries:            tc.global,
				PerProviderMaxRetries: tc.per,
				BaseDelay:             time.Millisecond,
				MaxDelay:              time.Millisecond,
			})
			require.NoError(t, err)
			assert.Equal(t, tc.want, r.maxRetriesFor(tc.idx))
		})
	}
}

// TestResilientModel_GeneratePerProviderMaxRetriesOverridesGlobal proves a
// provider with a lower PerProviderMaxRetries than the global MaxRetries
// exhausts its OWN budget and fails over sooner than the global value would
// allow. Without maxRetriesFor's per-index dispatch, bad would be retried
// against the global cap (5) instead of its override (1), so it would take 6
// calls (not 2) before failover — this is the assertion that would go red if
// Generate's retry loop reverted to r.cfg.MaxRetries for every provider.
func TestResilientModel_GeneratePerProviderMaxRetriesOverridesGlobal(t *testing.T) {
	var badCalls, goodCalls int32
	bad := newScriptedModel([]bool{true, true, true, true, true, true}, &badCalls)
	good := newScriptedModel([]bool{false}, &goodCalls)
	r, err := NewResilientModel([]model.BaseChatModel{bad, good}, ResilientConfig{
		MaxRetries:            5,
		PerProviderMaxRetries: []int{1, -1},
		BaseDelay:             time.Millisecond,
		MaxDelay:              time.Millisecond,
	})
	require.NoError(t, err)
	out, err := r.Generate(context.Background(), []*schema.Message{schema.UserMessage("x")})
	require.NoError(t, err)
	assert.Equal(t, "ok", out.Content)
	assert.Equal(t, int32(2), atomic.LoadInt32(&badCalls), "override (1) means 1 initial + 1 retry, then failover")
	assert.Equal(t, int32(1), atomic.LoadInt32(&goodCalls))
}

// TestResilientModel_GeneratePerProviderMaxRetriesFallsBackToGlobal proves the
// -1 sentinel, present as an explicit array element (not merely absent),
// still falls back to the global MaxRetries — the "未设置时回退全局值" half
// of the acceptance criterion, exercised through the same array shape
// bootstrap.go actually builds (one element per configured provider, -1 for
// every provider without an explicit override).
func TestResilientModel_GeneratePerProviderMaxRetriesFallsBackToGlobal(t *testing.T) {
	var calls int32
	f := newScriptedModel([]bool{true, true, true}, &calls) // fails 3x, global cap is 2 retries
	r, err := NewResilientModel([]model.BaseChatModel{f}, ResilientConfig{
		MaxRetries:            2,
		PerProviderMaxRetries: []int{-1},
		BaseDelay:             time.Millisecond,
		MaxDelay:              time.Millisecond,
	})
	require.NoError(t, err)
	_, err = r.Generate(context.Background(), []*schema.Message{schema.UserMessage("x")})
	require.Error(t, err)
	assert.Equal(t, int32(3), atomic.LoadInt32(&calls), "1 initial + 2 retries = global MaxRetries, not the sentinel")
}

// alwaysFlakyStreamModel opens successfully on every call (so openStreamChain
// never fails over — see its doc comment: failover only happens on a p.Stream
// open error) but errors mid-stream every time, forever. It is the fixture
// for proving the STREAM path's mid-stream retry cap (runStream's streamErr
// case) reads maxRetriesFor(curIdx), not the global cfg.MaxRetries.
type alwaysFlakyStreamModel struct {
	calls *int32
}

func (m *alwaysFlakyStreamModel) Generate(_ context.Context, _ []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	return schema.AssistantMessage("ok", nil), nil
}

func (m *alwaysFlakyStreamModel) Stream(_ context.Context, _ []*schema.Message, _ ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	atomic.AddInt32(m.calls, 1)
	return streamReaderFrom([]*schema.Message{schema.AssistantMessage("partial", nil)}, io.ErrUnexpectedEOF), nil
}

var _ model.BaseChatModel = (*alwaysFlakyStreamModel)(nil)

// TestResilientModel_StreamPerProviderMaxRetriesCapsMidStreamRetries proves
// the override caps mid-stream retries below what the global MaxRetries (5)
// would otherwise allow (6 calls). Without maxRetriesFor(curIdx) in the
// streamErr branch, this would take 6 Stream() calls instead of 2.
func TestResilientModel_StreamPerProviderMaxRetriesCapsMidStreamRetries(t *testing.T) {
	var calls int32
	f := &alwaysFlakyStreamModel{calls: &calls}
	r, err := NewResilientModel([]model.BaseChatModel{f}, ResilientConfig{
		MaxRetries:            5,
		PerProviderMaxRetries: []int{1},
		BaseDelay:             time.Millisecond,
		MaxDelay:              time.Millisecond,
	})
	require.NoError(t, err)

	sr, err := r.Stream(context.Background(), []*schema.Message{schema.UserMessage("x")})
	require.NoError(t, err)
	defer sr.Close()

	var recvErr error
	for {
		_, e := sr.Recv()
		if e != nil {
			recvErr = e
			break
		}
	}
	require.Error(t, recvErr, "budget exhausted, the mid-stream error must surface")
	assert.Equal(t, int32(2), atomic.LoadInt32(&calls), "override (1) means 1 initial open + 1 retried open, then give up")
}
