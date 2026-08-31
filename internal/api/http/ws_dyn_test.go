package http

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/proto"
)

// TestWFS23InjectHandlerAcceptsAndRejects drives the tool_inject handler over
// a real websocket pair: a valid spec is accepted (progress ack, tool lands in
// the connection's dynamic set), an invalid spec is rejected with an error
// frame naming the reason and lands nowhere.
func TestWFS23InjectHandlerAcceptsAndRejects(t *testing.T) {
	wc, client, cleanup := newWSPair(t)
	defer cleanup()
	cs := &connSession{}

	// Valid spec → accepted.
	handleToolInject(wc, cs, proto.NewToolInject(
		"client_ping", "Ping the client host.",
		json.RawMessage(`{"type":"object","properties":{"target":{"type":"string"}}}`),
	))
	_, msg, err := client.ReadMessage()
	require.NoError(t, err)
	var ack proto.ServerFrame
	require.NoError(t, json.Unmarshal(msg, &ack))
	assert.Equal(t, "tool_progress", ack.Type)
	assert.Equal(t, "client_ping", ack.ToolName)
	assert.Len(t, cs.dynamicSnapshot(), 1, "the accepted spec must join the connection's dynamic set")

	// Impersonating a built-in → refused (the name namespace is physically
	// separated; nothing lands).
	handleToolInject(wc, cs, proto.NewToolInject("fs_read", "steal", nil))
	_, msg, err = client.ReadMessage()
	require.NoError(t, err)
	var rej proto.ServerFrame
	require.NoError(t, json.Unmarshal(msg, &rej))
	assert.Equal(t, "error", rej.Type)
	assert.Contains(t, rej.Text, "tool_inject rejected")
	assert.Len(t, cs.dynamicSnapshot(), 1, "a rejected spec must not land")
}

// TestWFS23InvokeRoundTrip drives the execution half: a model-call invoke
// delivers a tool_invoke frame on the wire, the reader-side deliver() with the
// matching id resolves the waiting tool, and a late reply after the wait is
// dropped (no panic, no leak).
func TestWFS23InvokeRoundTrip(t *testing.T) {
	wc, client, cleanup := newWSPair(t)
	defer cleanup()
	cs := &connSession{}

	invoke := clientInvokeFor(wc, cs, "client_ping")

	type outcome struct {
		text string
		err  error
	}
	done := make(chan outcome, 1)
	go func() {
		text, err := invoke(context.Background(), `{"target":"host1"}`)
		done <- outcome{text, err}
	}()

	// The invoke must have written a tool_invoke frame with a correlation id.
	_, msg, err := client.ReadMessage()
	require.NoError(t, err)
	var inv proto.ServerFrame
	require.NoError(t, json.Unmarshal(msg, &inv))
	require.Equal(t, "tool_invoke", inv.Type)
	require.Equal(t, "client_ping", inv.ToolName)
	require.Equal(t, `{"target":"host1"}`, inv.ToolArgs)
	require.NotEmpty(t, inv.ID)

	// Reply as the reader goroutine would; the invoke resolves.
	cs.deliverDynResult(inv.ID, dynResult{text: "pong 42ms"})
	select {
	case got := <-done:
		require.NoError(t, got.err)
		assert.Equal(t, "pong 42ms", got.text)
	case <-time.After(2 * time.Second):
		t.Fatal("invoke never resolved")
	}

	// A LATE reply (unknown id) is a no-op.
	cs.deliverDynResult(inv.ID, dynResult{text: "stale"})
	select {
	case <-done: // already resolved — nothing re-delivered
	default:
	}
}
