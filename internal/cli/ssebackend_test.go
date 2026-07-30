package cli

import (
	"context"
	nhttp "net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/agent/orchestrator"
	"github.com/x6nux/yanshi/internal/api/http"
	einollm "github.com/x6nux/yanshi/internal/llm/eino"
)

func newSSEServer(t *testing.T) string {
	t.Helper()
	o, _ := orchestrator.New(orchestrator.Config{Model: einollm.NewFakeModel([]string{"sse-reply"}, nil)})
	s := http.New(http.Config{Token: "t"})
	s.Chat(o, nil, nil)
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts.URL
}

func TestSSEBackend_ParsesStructuredEvents(t *testing.T) {
	b := newSSEBackend(newSSEServer(t))
	defer b.Close()
	assert.Equal(t, "sse", b.Mode())

	ch, err := b.Send(context.Background(), "hi")
	require.NoError(t, err)
	var got string
	var done bool
	for ev := range ch {
		switch ev.Kind {
		case "agent_chunk":
			got += ev.Text
		case "done":
			done = true
		}
	}
	assert.True(t, done)
	assert.Equal(t, "sse-reply", got)
}

// TestSSEBackend_MultiTurn proves client-held history: the second Send carries
// the first turn, so an Echo model's reply contains the first message.
func TestSSEBackend_MultiTurn(t *testing.T) {
	fm := einollm.NewFakeModel(nil, nil)
	fm.Echo = true
	o, _ := orchestrator.New(orchestrator.Config{Model: fm})
	s := http.New(http.Config{Token: "t"})
	s.Chat(o, nil, nil)
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	b := newSSEBackend(ts.URL)
	defer b.Close()

	drain := func(text string) string {
		ch, err := b.Send(context.Background(), text)
		require.NoError(t, err)
		var out string
		for ev := range ch {
			if ev.Kind == "agent_chunk" {
				out += ev.Text
			}
		}
		return out
	}
	drain("remember OMEGA")
	second := drain("echo history")
	assert.Contains(t, second, "remember OMEGA", "second turn must carry the first (client-held history)")
}

// TestSSEBackend_AdoptsCompactedHistory proves the Task 35b SSE flow end-to-end
// through sseBackend: once the server-side history crosses the threshold, the
// server streams compact_chunk deltas (surfaced as StreamEvents), then a
// history_replaced frame. sseBackend adopts the compacted slice in place of what
// it sent, so the NEXT Send carries the compacted history (not the original).
func TestSSEBackend_AdoptsCompactedHistory(t *testing.T) {
	// Long assistant replies so the new Plan (which pins every user message
	// verbatim) still leaves the assistant turn large enough for compaction
	// to actually shrink tokens. SUMMARY = compaction summary; a3 = post-
	// compact turn reply (kept short to keep the assertion focused).
	longReply := strings.Repeat("r", 100)
	fm := einollm.NewFakeModel([]string{longReply, longReply, "SUMMARY", "a3"}, nil)
	o, _ := orchestrator.New(orchestrator.Config{Model: fm})
	s := http.New(http.Config{
		Token: "t",
		Compaction: http.CompactionConfig{
			Threshold: 0.5, ContextWindow: 100, KeepRecent: 1,
		},
	})
	// Register the fake model so the server's compactionModel can pick it
	// (new MaybeCompact takes a model directly, not the orchestrator).
	models := map[string]model.BaseChatModel{"fm": fm}
	s.Chat(o, models, nil)
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	b := newSSEBackend(ts.URL)
	defer b.Close()

	long := strings.Repeat("a", 100)
	// Sends 1 and 2 build up history below the split guard; Send 3 trips it.
	drainKinds := func(text string) []string {
		ch, err := b.Send(context.Background(), text)
		require.NoError(t, err)
		var kinds []string
		for ev := range ch {
			kinds = append(kinds, ev.Kind)
		}
		return kinds
	}
	drainKinds(long) // turn 1 -> longReply #1
	drainKinds(long) // turn 2 -> longReply #2

	// Turn 3 triggers auto-compaction server-side.
	kinds := drainKinds(long)
	assert.Contains(t, kinds, "compact_chunk", "compact_chunk surfaced to the TUI")

	// Under the new Plan, every user message is pinned verbatim (intent is
	// never lost) and only the older ASSISTANT turns are folded into the
	// summary. For history [u1,a1,u2,a2,u3] with KeepRecent=1 the pinned set
	// is {u1,u2,u3,a2} (tail + all users) and a1 alone gets summarized, so
	// the post-compact slice is [u1,u2,a2,u3,summary] (5 items, summary at
	// the tail as a user-role sentinel — bug④). The turn-3 reply appends one
	// more assistant message, giving 6 items total.
	b.histMu.Lock()
	got := b.history
	b.histMu.Unlock()
	require.Len(t, got, 6, "compacted history adopted: pinned users + kept recent + summary + new turn")
	// Pinned prefix preserves original order: u1, u2 (the two long users from
	// turns 1+2 that are no longer the most recent pair but ARE pinned because
	// the new Plan never folds user intent into the summary).
	assert.Equal(t, schema.User, got[0].Role, "u1: pinned user intent")
	assert.Equal(t, long, got[0].Content)
	assert.Equal(t, schema.User, got[1].Role, "u2: pinned user intent")
	assert.Equal(t, long, got[1].Content)
	assert.Equal(t, schema.Assistant, got[2].Role, "a2: kept-recent tail assistant")
	assert.Equal(t, longReply, got[2].Content)
	assert.Equal(t, schema.User, got[3].Role, "u3: current turn user message")
	assert.Equal(t, long, got[3].Content)
	// Summary is a user-role sentinel-prefixed message at the tail of the
	// compacted slice (bug④: user-role, not system; tail, not head).
	assert.Equal(t, schema.User, got[4].Role, "summary msg is user-role (bug④)")
	assert.Contains(t, got[4].Content, "SUMMARY")
	assert.Contains(t, got[4].Content, "[yanshi:conversation-summary]", "summary carries the sentinel")
	assert.Equal(t, schema.Assistant, got[5].Role, "turn-3 reply appended")
	assert.Equal(t, "a3", got[5].Content)
}

// TestSSEBackend_CancelUnblocksStalledSend proves that Cancel() propagates to
// the in-flight HTTP request: a server that stalls (sends headers but no SSE
// data) blocks the scan goroutine, and Cancel() must abort the request context
// so the goroutine exits and the channel closes promptly.
func TestSSEBackend_CancelUnblocksStalledSend(t *testing.T) {
	stallCh := make(chan struct{})
	ts := httptest.NewServer(nhttp.HandlerFunc(func(w nhttp.ResponseWriter, r *nhttp.Request) {
		flusher, ok := w.(nhttp.Flusher)
		require.True(t, ok)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(nhttp.StatusOK)
		flusher.Flush()
		<-stallCh // block forever until test cleanup
	}))
	t.Cleanup(func() {
		close(stallCh)
		ts.Close()
	})

	b := newSSEBackend(ts.URL)
	defer b.Close()

	ch, err := b.Send(context.Background(), "hi")
	require.NoError(t, err)

	// Give the goroutine a moment to enter sc.Scan(), then cancel.
	time.Sleep(50 * time.Millisecond)
	require.NoError(t, b.Cancel())

	select {
	case _, ok := <-ch:
		assert.False(t, ok, "channel should be closed after Cancel")
	case <-time.After(3 * time.Second):
		t.Fatal("Cancel() did not unblock the stalled Send channel within 3s")
	}
}
