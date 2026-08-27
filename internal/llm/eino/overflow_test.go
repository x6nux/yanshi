package eino

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/x6nux/yanshi/internal/ctxcompact"
)

// stagedModel is a BaseChatModel that returns a scripted error or message per
// call and records what it was given.
//
// It is not FakeModel because these tests need per-attempt control: attempt 1
// must fail with a specific provider error and attempt 2 must succeed, and the
// assertion is about what the SECOND attempt received. FakeModel's single `err`
// field cannot express that.
type stagedModel struct {
	mu sync.Mutex
	// errs[i] is the error returned by call i (nil = success).
	errs []error
	// reply is returned on a successful call.
	reply *schema.Message
	// calls counts invocations.
	calls int
	// seen records the message slice of every call.
	seen [][]*schema.Message
	// seenOpts records the resolved common options of every call.
	seenOpts []*model.Options
}

func (m *stagedModel) next(msgs []*schema.Message, opts []model.Option) (*schema.Message, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	i := m.calls
	m.calls++
	cp := make([]*schema.Message, len(msgs))
	copy(cp, msgs)
	m.seen = append(m.seen, cp)
	m.seenOpts = append(m.seenOpts, model.GetCommonOptions(&model.Options{}, opts...))
	if i < len(m.errs) && m.errs[i] != nil {
		return nil, m.errs[i]
	}
	if m.reply != nil {
		return m.reply, nil
	}
	return schema.AssistantMessage("ok", nil), nil
}

func (m *stagedModel) Generate(_ context.Context, msgs []*schema.Message, opts ...model.Option) (
	*schema.Message, error) {
	return m.next(msgs, opts)
}

func (m *stagedModel) Stream(_ context.Context, msgs []*schema.Message, opts ...model.Option) (
	*schema.StreamReader[*schema.Message], error) {
	msg, err := m.next(msgs, opts)
	sr, sw := schema.Pipe[*schema.Message](1)
	go func() {
		defer sw.Close()
		if err != nil {
			sw.Send(nil, err)
			return
		}
		sw.Send(msg, nil)
	}()
	return sr, nil
}

func (m *stagedModel) callCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

func (m *stagedModel) call(i int) []*schema.Message {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.seen[i]
}

// longHistory builds a history big enough for a forced compaction to shrink.
func longHistory(n int) []*schema.Message {
	msgs := make([]*schema.Message, 0, n)
	msgs = append(msgs, schema.SystemMessage("you are a helpful assistant"))
	for i := 0; i < n; i++ {
		msgs = append(msgs, schema.UserMessage(strings.Repeat("question words here ", 40)))
		msgs = append(msgs, schema.AssistantMessage(strings.Repeat("answer words here ", 40), nil))
	}
	return msgs
}

// overflowErr is a provider rejection the classifier files as
// ClassContextOverflow.
var overflowErr = errors.New("error, status code: 400, message: maximum context length is 8192 tokens")

// TestIsContextOverflow pins that BOTH sides of the boundary are recognised as
// the same condition: the local C9 pre-send gate and the remote provider 400.
func TestIsContextOverflow(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{name: "provider 400", err: overflowErr, want: true},
		{
			name: "local C9 sentinel",
			err:  &ctxcompact.ContextOverflowError{Tokens: 9000, Limit: 8000, Window: 8192, Reserve: 192},
			want: true,
		},
		{name: "wrapped local sentinel", err: ctxcompact.ErrContextOverflow, want: true},
		{name: "an ordinary 400", err: errors.New("status code: 400, message: invalid_request_error"), want: false},
		{name: "a 429", err: errors.New("status code: 429, message: rate limit"), want: false},
		{name: "nil", err: nil, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsContextOverflow(tc.err); got != tc.want {
				t.Errorf("IsContextOverflow = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestForceCompactOnlyReportsRealShrinks is rule 1 of the C6 policy: the bool
// must be false whenever the input did not actually get smaller, because
// resending an unchanged request is a guaranteed second 400 and a second
// charge.
func TestForceCompactOnlyReportsRealShrinks(t *testing.T) {
	summarizer := NewFakeModel([]string{"a short summary of the earlier turns"}, nil)
	summarizer.Repeat = true

	t.Run("a long history shrinks", func(t *testing.T) {
		msgs := longHistory(12)
		out, shrank := forceCompact(context.Background(), msgs, nil,
			OverflowRecoveryConfig{ContextWindow: 4096, Summarizer: summarizer})
		if !shrank {
			t.Fatal("shrank = false for a history that should compact")
		}
		if ctxcompact.EstimateTokens(out) >= ctxcompact.EstimateTokens(msgs) {
			t.Error("shrank = true but the token count did not fall")
		}
	})

	t.Run("a zero window disables recovery entirely", func(t *testing.T) {
		msgs := longHistory(12)
		out, shrank := forceCompact(context.Background(), msgs, nil,
			OverflowRecoveryConfig{Summarizer: summarizer})
		if shrank {
			t.Error("shrank = true with no window; there is no budget to compact toward")
		}
		if len(out) != len(msgs) {
			t.Error("the original history was not returned")
		}
	})

	t.Run("a history shorter than the pinned tail cannot shrink", func(t *testing.T) {
		msgs := []*schema.Message{schema.UserMessage("hi"), schema.AssistantMessage("hello", nil)}
		_, shrank := forceCompact(context.Background(), msgs, nil,
			OverflowRecoveryConfig{ContextWindow: 4096, Summarizer: summarizer})
		if shrank {
			t.Error("shrank = true for a history at or below KeepRecent")
		}
	})

	t.Run("no summarizer at all", func(t *testing.T) {
		msgs := longHistory(12)
		_, shrank := forceCompact(context.Background(), msgs, nil,
			OverflowRecoveryConfig{ContextWindow: 4096})
		if shrank {
			t.Error("shrank = true with no summarizer and no inner model")
		}
	})

	t.Run("a failing summarizer does not claim a shrink", func(t *testing.T) {
		broken := NewFakeModel(nil, errors.New("summary model is down"))
		msgs := longHistory(12)
		_, shrank := forceCompact(context.Background(), msgs, nil,
			OverflowRecoveryConfig{ContextWindow: 4096, Summarizer: broken})
		if shrank {
			t.Error("shrank = true after the summarizer failed")
		}
	})

	t.Run("a summary bigger than what it replaces is not a shrink", func(t *testing.T) {
		// The discriminating case for the `after >= before` guard, and the one
		// a mutation probe exposed as missing: every other row here is caught
		// by an EARLIER guard (no window, tail too short, summarizer error), so
		// removing the size comparison left the whole table green. Here Run
		// succeeds and returns a valid history that happens to be LARGER,
		// because the summariser was verbose — which is exactly the state in
		// which a resend is a guaranteed second 400 and a second charge.
		verbose := NewFakeModel([]string{strings.Repeat("verbose summary text ", 2000)}, nil)
		verbose.Repeat = true
		msgs := longHistory(6)
		out, shrank := forceCompact(context.Background(), msgs, nil,
			OverflowRecoveryConfig{ContextWindow: 1 << 20, Summarizer: verbose})
		if shrank {
			t.Errorf("shrank = true though the result is %d tokens against the original %d",
				ctxcompact.EstimateTokens(out), ctxcompact.EstimateTokens(msgs))
		}
		if len(out) != len(msgs) {
			t.Error("the original history was not returned")
		}
	})
}

// TestAdaptiveDoesNotResendAnUnshrunkHistory is the wrapper-level twin of the
// verbose-summary case: compaction ran, produced something valid, and did not
// help. No second request may be made.
func TestAdaptiveDoesNotResendAnUnshrunkHistory(t *testing.T) {
	verbose := NewFakeModel([]string{strings.Repeat("verbose summary text ", 2000)}, nil)
	verbose.Repeat = true
	inner := &stagedModel{errs: []error{overflowErr}}
	a := NewAdaptiveModel(inner, AdaptiveConfig{
		ModelID:  "m",
		Overflow: OverflowRecoveryConfig{ContextWindow: 1 << 20, Summarizer: verbose},
	})
	if _, err := a.Generate(context.Background(), longHistory(6)); err == nil {
		t.Fatal("Generate succeeded; want the provider error")
	}
	if inner.callCount() != 1 {
		t.Errorf("inner called %d times, want 1 — the compaction did not shrink anything",
			inner.callCount())
	}
}

// TestOverflowKeepRecentDefault pins the tail size, which is the difference
// between a recovered turn that still knows what it was asked and one that
// answers a question nobody posed.
func TestOverflowKeepRecentDefault(t *testing.T) {
	if got := (OverflowRecoveryConfig{}).keepRecent(); got != DefaultOverflowKeepRecent {
		t.Errorf("keepRecent = %d, want %d", got, DefaultOverflowKeepRecent)
	}
	if got := (OverflowRecoveryConfig{KeepRecent: 8}).keepRecent(); got != 8 {
		t.Errorf("keepRecent = %d, want the configured 8", got)
	}
}

// TestAdaptiveGenerateOverflowRetriesOnce is the C6 end-to-end assertion: one
// forced compaction, one resend, and the second request is genuinely smaller.
func TestAdaptiveGenerateOverflowRetriesOnce(t *testing.T) {
	inner := &stagedModel{errs: []error{overflowErr}}
	summarizer := NewFakeModel([]string{"summary"}, nil)
	summarizer.Repeat = true
	a := NewAdaptiveModel(inner, AdaptiveConfig{
		ModelID:  "m",
		Overflow: OverflowRecoveryConfig{ContextWindow: 4096, Summarizer: summarizer},
	})

	msgs := longHistory(12)
	out, err := a.Generate(context.Background(), msgs)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if out == nil {
		t.Fatal("Generate returned no message")
	}
	if inner.callCount() != 2 {
		t.Fatalf("inner called %d times, want exactly 2 (one retry)", inner.callCount())
	}
	first, second := inner.call(0), inner.call(1)
	if len(second) >= len(first) {
		t.Errorf("the retry sent %d messages, not fewer than the first attempt's %d",
			len(second), len(first))
	}
}

// TestAdaptiveGenerateOverflowDoesNotRetryWhenNothingShrank is rule 1 at the
// wrapper level: no shrink, no second request.
func TestAdaptiveGenerateOverflowDoesNotRetryWhenNothingShrank(t *testing.T) {
	inner := &stagedModel{errs: []error{overflowErr}}
	a := NewAdaptiveModel(inner, AdaptiveConfig{
		ModelID: "m",
		// No window → forceCompact refuses → no retry.
		Overflow: OverflowRecoveryConfig{},
	})
	_, err := a.Generate(context.Background(), longHistory(12))
	if err == nil {
		t.Fatal("Generate succeeded; want the provider error")
	}
	if !errors.Is(err, overflowErr) {
		t.Errorf("err = %v, want the provider's own error", err)
	}
	if inner.callCount() != 1 {
		t.Errorf("inner called %d times, want 1 (an unshrunk retry is a wasted charge)", inner.callCount())
	}
}

// TestAdaptiveGenerateOverflowRetriesExactlyOnce is rule 2: a second overflow
// after a real shrink ends the turn rather than compacting again.
func TestAdaptiveGenerateOverflowRetriesExactlyOnce(t *testing.T) {
	inner := &stagedModel{errs: []error{overflowErr, overflowErr, overflowErr}}
	summarizer := NewFakeModel([]string{"summary"}, nil)
	summarizer.Repeat = true
	a := NewAdaptiveModel(inner, AdaptiveConfig{
		ModelID:  "m",
		Overflow: OverflowRecoveryConfig{ContextWindow: 4096, Summarizer: summarizer},
	})
	_, err := a.Generate(context.Background(), longHistory(12))
	if err == nil {
		t.Fatal("Generate succeeded; want the second failure")
	}
	if inner.callCount() != 2 {
		t.Fatalf("inner called %d times, want exactly 2 — repeated compaction is a metered loop",
			inner.callCount())
	}
	// The wrapped error must carry both sizes, or an operator cannot tell
	// "compaction did nothing" from "still too large after a real shrink".
	var wrapped *overflowRetryError
	if !errors.As(err, &wrapped) {
		t.Fatalf("err = %T, want *overflowRetryError carrying the token counts", err)
	}
	if wrapped.Before <= wrapped.After {
		t.Errorf("before=%d after=%d; the wrap claims no shrink happened",
			wrapped.Before, wrapped.After)
	}
	if !errors.Is(err, overflowErr) {
		t.Error("the provider's error is no longer reachable through the wrapper")
	}
}

// TestAdaptiveStreamOverflowRetriesOnce covers the streaming half. It exists
// separately because a provider rejection surfaces on the first Recv, not from
// Stream, so the retry needs the peek machinery and could easily work for
// Generate while doing nothing here.
func TestAdaptiveStreamOverflowRetriesOnce(t *testing.T) {
	inner := &stagedModel{errs: []error{overflowErr}}
	summarizer := NewFakeModel([]string{"summary"}, nil)
	summarizer.Repeat = true
	a := NewAdaptiveModel(inner, AdaptiveConfig{
		ModelID:  "m",
		Overflow: OverflowRecoveryConfig{ContextWindow: 4096, Summarizer: summarizer},
	})

	sr, err := a.Stream(context.Background(), longHistory(12))
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	got := drain(t, sr)
	if len(got) == 0 {
		t.Fatal("the recovered stream delivered nothing")
	}
	if inner.callCount() != 2 {
		t.Errorf("inner called %d times, want exactly 2", inner.callCount())
	}
}

// drain reads a stream to EOF, failing on any error.
func drain(t *testing.T, sr *schema.StreamReader[*schema.Message]) []*schema.Message {
	t.Helper()
	defer sr.Close()
	var out []*schema.Message
	for {
		msg, err := sr.Recv()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatalf("stream error: %v", err)
		}
		out = append(out, msg)
	}
}

// TestOpenAndPeekReplaysTheFirstItem pins the property that makes the streaming
// retry invisible to a caller that does not retry: the peeked item is put back.
func TestOpenAndPeekReplaysTheFirstItem(t *testing.T) {
	want := []string{"a", "b", "c"}
	open := func(context.Context) (*schema.StreamReader[*schema.Message], error) {
		sr, sw := schema.Pipe[*schema.Message](1)
		go func() {
			defer sw.Close()
			for _, s := range want {
				sw.Send(schema.AssistantMessage(s, nil), nil)
			}
		}()
		return sr, nil
	}
	sr, err := openAndPeek(context.Background(), open)
	if err != nil {
		t.Fatalf("openAndPeek: %v", err)
	}
	got := drain(t, sr)
	if len(got) != len(want) {
		t.Fatalf("got %d messages, want %d — the peeked item was swallowed", len(got), len(want))
	}
	for i := range want {
		if got[i].Content != want[i] {
			t.Errorf("message %d = %q, want %q", i, got[i].Content, want[i])
		}
	}
}

// TestOpenAndPeekSurfacesTheFirstError pins the other half: an error that would
// otherwise appear only on the caller's first Recv comes back as a value.
func TestOpenAndPeekSurfacesTheFirstError(t *testing.T) {
	boom := errors.New("provider said no")
	open := func(context.Context) (*schema.StreamReader[*schema.Message], error) {
		sr, sw := schema.Pipe[*schema.Message](1)
		go func() {
			defer sw.Close()
			sw.Send(nil, boom)
		}()
		return sr, nil
	}
	if _, err := openAndPeek(context.Background(), open); !errors.Is(err, boom) {
		t.Errorf("err = %v, want the provider error", err)
	}
	// A setup error propagates too.
	setupErr := errors.New("could not open")
	_, err := openAndPeek(context.Background(), func(context.Context) (
		*schema.StreamReader[*schema.Message], error) {
		return nil, setupErr
	})
	if !errors.Is(err, setupErr) {
		t.Errorf("err = %v, want the setup error", err)
	}
}

// TestOpenAndPeekEmptyStreamIsNotAnError pins that an immediate EOF is handed
// back as an empty reader: ResilientChatModel owns the empty-response retry
// budget and duplicating it here would give one empty response two ladders.
func TestOpenAndPeekEmptyStreamIsNotAnError(t *testing.T) {
	open := func(context.Context) (*schema.StreamReader[*schema.Message], error) {
		sr, sw := schema.Pipe[*schema.Message](1)
		sw.Close()
		return sr, nil
	}
	sr, err := openAndPeek(context.Background(), open)
	if err != nil {
		t.Fatalf("openAndPeek on an empty stream: %v", err)
	}
	if got := drain(t, sr); len(got) != 0 {
		t.Errorf("got %d messages from an empty stream", len(got))
	}
}
