package cli

import (
	"context"
	"encoding/json"
	nethttp "net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/proto"
)

// TestToStreamEvent_StructuredResult verifies the toStreamEvent mapping
// surfaces a structured_result ServerFrame's payload as the corresponding
// StreamEvent field. This covers the WS client path: wsBackend.readLoop calls
// toStreamEvent directly for every incoming frame.
func TestToStreamEvent_StructuredResult(t *testing.T) {
	payload := json.RawMessage(`{"x":1}`)
	ev := toStreamEvent(proto.NewStructuredResult(payload))
	assert.Equal(t, "structured_result", ev.Kind)
	assert.Equal(t, payload, ev.StructuredResult, "payload must round-trip as raw JSON")
}

// TestWSBackend_StructuredResultEndToEnd drives the real WS readLoop with a raw
// server that emits one structured_result frame then closes the turn. Proves the
// WS client path surfaces the payload intact end-to-end.
func TestWSBackend_StructuredResultEndToEnd(t *testing.T) {
	payload := json.RawMessage(`{"x":1}`)
	upgrader := websocket.Upgrader{CheckOrigin: func(r *nethttp.Request) bool { return true }}
	ts := httptest.NewServer(nethttp.HandlerFunc(func(w nethttp.ResponseWriter, r *nethttp.Request) {
		sc, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer sc.Close()
		// Drain the user_message the client writes so the WS read on the
		// server side does not block close; ordering still favors the client
		// because TCP guarantees these writes arrive after the request.
		if _, _, err := sc.ReadMessage(); err != nil {
			return
		}
		if err := sc.WriteJSON(proto.NewStructuredResult(payload)); err != nil {
			return
		}
		if err := sc.WriteJSON(proto.NewDone()); err != nil {
			return
		}
	}))
	t.Cleanup(ts.Close)
	url := "ws" + strings.TrimPrefix(ts.URL, "http") + "/api/v1/chat/ws"

	b, err := newWSBackend(context.Background(), url)
	require.NoError(t, err)
	t.Cleanup(func() { _ = b.Close() })

	ch, err := b.Send(context.Background(), "hi")
	require.NoError(t, err)

	var sawStructured, sawDone bool
	for ev := range ch {
		switch ev.Kind {
		case "structured_result":
			sawStructured = true
			assert.Equal(t, payload, ev.StructuredResult, "ws: payload must round-trip")
		case "done":
			sawDone = true
		}
	}
	assert.True(t, sawStructured, "ws path must surface structured_result")
	assert.True(t, sawDone, "ws turn must terminate with done")
}

// TestSSEBackend_StructuredResultEndToEnd drives the real SSE parsing path
// (sseBackend.flush -> switch f.Type -> toStreamEvent) with a server that emits
// a structured_result event. Proves the SSE client path surfaces the payload
// intact end-to-end.
func TestSSEBackend_StructuredResultEndToEnd(t *testing.T) {
	payload := json.RawMessage(`{"x":1}`)
	structEvent, structData := proto.NewStructuredResult(payload).SSEEvent()
	doneEvent, doneData := proto.NewDone().SSEEvent()
	ts := httptest.NewServer(nethttp.HandlerFunc(func(w nethttp.ResponseWriter, r *nethttp.Request) {
		flusher, ok := w.(nethttp.Flusher)
		require.True(t, ok)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(nethttp.StatusOK)
		_, _ = w.Write([]byte("event: " + structEvent + "\n"))
		_, _ = w.Write([]byte("data: " + string(structData) + "\n\n"))
		_, _ = w.Write([]byte("event: " + doneEvent + "\n"))
		_, _ = w.Write([]byte("data: " + string(doneData) + "\n\n"))
		flusher.Flush()
	}))
	t.Cleanup(ts.Close)

	b := newSSEBackend(ts.URL)
	t.Cleanup(func() { _ = b.Close() })

	ch, err := b.Send(context.Background(), "hi")
	require.NoError(t, err)

	var sawStructured, sawDone bool
	for ev := range ch {
		switch ev.Kind {
		case "structured_result":
			sawStructured = true
			assert.Equal(t, payload, ev.StructuredResult, "sse: payload must round-trip")
		case "done":
			sawDone = true
		}
	}
	assert.True(t, sawStructured, "sse path must surface structured_result")
	assert.True(t, sawDone, "sse turn must terminate with done")
}
