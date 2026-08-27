package eino

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"
	"github.com/stretchr/testify/require"
)

// pipeStream returns a reader plus a writer the test drives by hand, so a
// "gateway that connects and then says nothing" is expressible.
func pipeStream() (*schema.StreamReader[*schema.Message], *schema.StreamWriter[*schema.Message]) {
	return schema.Pipe[*schema.Message](8)
}

// ledger: A2/W-A-06#1 首块超时后流被终止并返回可重试错误
//
// This also pins the fix for a previously-false doc comment on Recv: timing
// out must cancel the per-attempt context, because sr.Close() alone (the old,
// wrong claim) cannot unblock a goroutine parked in sr.Recv — only the
// WRITER side closing can (schema/stream.go: closeSend closes items, which
// recv() blocks on; closeRecv, what sr.Close() calls, closes an unrelated
// channel recv() never selects on). The producer goroutine below stands in
// for a real provider's read goroutine: it is not schema.Pipe's job to watch
// ctx (it has none), so — exactly like AnthropicModel.readStream and
// openaiResponsesModel.readStream (both built on
// http.NewRequestWithContext) and eino-ext's openai ACL client (ctx threaded
// into CreateChatCompletionStream) — it is the PRODUCER that must notice
// ctx.Done() and close its writer. What this proves: the watchdog's cancel
// is actually invoked, and a ctx-aware producer actually unblocks and exits
// because of it. What it does NOT prove: that every third-party SSE client's
// internal Recv() truly selects on ctx under the hood — that half is
// established by reading eino-ext's source (it does, via the underlying
// HTTP transport aborting the request), not by this test.
func TestWatchdogFirstChunkTimeout(t *testing.T) {
	sr, sw := pipeStream()
	ctx, cancel := context.WithCancel(context.Background())
	producerExited := make(chan struct{})
	go func() {
		defer close(producerExited)
		defer sw.Close()
		<-ctx.Done() // mimics a provider goroutine blocked on the HTTP response
	}()

	w := newWatchdogReader(sr, 50*time.Millisecond, time.Hour, cancel)
	_, err := w.Recv()

	require.ErrorIs(t, err, ErrStreamIdle)
	require.True(t, IsRetryableModelErr(err),
		"a stalled gateway is transient; a non-retryable verdict would burn the whole failover chain")
	require.ErrorIs(t, ctx.Err(), context.Canceled,
		"Recv must cancel the per-attempt context on timeout, not just leave the goroutine parked")

	select {
	case <-producerExited:
	case <-time.After(time.Second):
		t.Fatal("producer goroutine did not exit after the watchdog's cancel — cancellation did not propagate")
	}
}

// Recv has two return sites for a fired budget: the timer.C branch above
// (deadline elapses while waiting) and the "already past deadline" fast path
// taken on ENTRY when a prior call's deadline has already elapsed (e.g. a
// caller that keeps retrying after ErrStreamIdle). Both must cancel — this
// pins the fast path specifically, since it is a separate call to w.cancel()
// in the source and a revert-probe showed the first test does not exercise
// it (only the timer.C branch fires on a stream's first, and only, call).
func TestWatchdogAlreadyExpiredDeadlineCancelsAttemptContext(t *testing.T) {
	sr, _ := pipeStream() // no writer: the leaked internal Recv goroutine never receives, which is fine — it lives no longer than the test binary.
	ctx, cancel := context.WithCancel(context.Background())

	w := newWatchdogReader(sr, 20*time.Millisecond, time.Hour, cancel)
	_, err := w.Recv() // times out via the timer.C branch, arming w.deadline in the past
	require.ErrorIs(t, err, ErrStreamIdle)
	require.ErrorIs(t, ctx.Err(), context.Canceled)

	// Second call: w.begun is still false (note() never ran on an error), so
	// budget() still returns "first", and w.deadline is unchanged — already
	// in the past. This must take the immediate remaining<=0 branch.
	ctx2, cancel2 := context.WithCancel(context.Background())
	w.cancel = cancel2
	_, err = w.Recv()
	require.ErrorIs(t, err, ErrStreamIdle)
	require.ErrorIs(t, ctx2.Err(), context.Canceled,
		"the already-past-deadline fast path must also cancel the attempt context")
}

// ledger: A2/W-A-06#2 仅发送空控制块的流在稳态超时后被终止
func TestWatchdogEmptyControlChunksDoNotRenewTheDeadline(t *testing.T) {
	sr, sw := pipeStream()
	go func() {
		defer sw.Close()
		// One real chunk starts the steady-state clock.
		sw.Send(&schema.Message{Role: schema.Assistant, Content: "hi"}, nil)
		// Then heartbeats forever, carrying nothing.
		for i := 0; i < 100; i++ {
			sw.Send(&schema.Message{Role: schema.Assistant}, nil)
			time.Sleep(5 * time.Millisecond)
		}
	}()

	w := newWatchdogReader(sr, time.Hour, 60*time.Millisecond, func() {})
	_, err := w.Recv() // the real chunk
	require.NoError(t, err)

	start := time.Now()
	for {
		_, err = w.Recv()
		if err != nil {
			break
		}
		require.Less(t, time.Since(start), 2*time.Second, "watchdog never fired")
	}
	require.ErrorIs(t, err, ErrStreamIdle,
		"blank deltas renewed the deadline, so a heartbeat-only gateway hangs forever")
}

// ledger: A2/W-A-06#3 有实际内容持续到达的长流不被误杀
func TestWatchdogLongStreamWithContentIsNotKilled(t *testing.T) {
	sr, sw := pipeStream()
	go func() {
		defer sw.Close()
		for i := 0; i < 20; i++ {
			sw.Send(&schema.Message{Role: schema.Assistant, Content: "."}, nil)
			time.Sleep(10 * time.Millisecond)
		}
	}()

	w := newWatchdogReader(sr, time.Hour, 80*time.Millisecond, func() {})
	n := 0
	for {
		_, err := w.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(t, err, "a stream delivering content every 10ms must survive an 80ms idle budget")
		n++
	}
	require.Equal(t, 20, n)
}

// ledger: A2/W-A-06#4 两个超时值均可配置且零值表示关闭
func TestWatchdogZeroTimeoutsDisableIt(t *testing.T) {
	sr, sw := pipeStream()
	go func() {
		defer sw.Close()
		time.Sleep(120 * time.Millisecond)
		sw.Send(&schema.Message{Role: schema.Assistant, Content: "late"}, nil)
	}()

	w := newWatchdogReader(sr, 0, 0, func() {})
	msg, err := w.Recv()

	require.NoError(t, err, "zero timeouts must behave byte-identically to the pre-W-A-06 code")
	require.Equal(t, "late", msg.Content)
}
