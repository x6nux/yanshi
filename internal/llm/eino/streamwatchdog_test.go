package eino

import (
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
func TestWatchdogFirstChunkTimeout(t *testing.T) {
	sr, sw := pipeStream()
	defer sw.Close()

	w := newWatchdogReader(sr, 50*time.Millisecond, time.Hour)
	_, err := w.Recv()

	require.ErrorIs(t, err, ErrStreamIdle)
	require.True(t, IsRetryableModelErr(err),
		"a stalled gateway is transient; a non-retryable verdict would burn the whole failover chain")
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

	w := newWatchdogReader(sr, time.Hour, 60*time.Millisecond)
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

	w := newWatchdogReader(sr, time.Hour, 80*time.Millisecond)
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

	w := newWatchdogReader(sr, 0, 0)
	msg, err := w.Recv()

	require.NoError(t, err, "zero timeouts must behave byte-identically to the pre-W-A-06 code")
	require.Equal(t, "late", msg.Content)
}
