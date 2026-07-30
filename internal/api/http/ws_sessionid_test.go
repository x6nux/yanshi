package http

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/agent/orchestrator"
	einollm "github.com/x6nux/yanshi/internal/llm/eino"
	"github.com/x6nux/yanshi/internal/proto"
	"github.com/x6nux/yanshi/internal/store"
)

// TestChatWS_StatusFrameCarriesSessionID proves the WS handler surfaces the
// server-side session id on the status frame emitted after a turn, so a headless
// client (`yanshi exec`) can print it for the next --resume. Requires a
// non-nil store so ensureSession creates a real session row; with store == nil
// the id stays empty and resume is unavailable (matching today's behavior).
func TestChatWS_StatusFrameCarriesSessionID(t *testing.T) {
	st, err := store.Open(":memory:")
	require.NoError(t, err)

	o, err := orchestrator.New(orchestrator.Config{Model: einollm.NewFakeModel([]string{"hi"}, nil)})
	require.NoError(t, err)
	s := New(Config{Token: "t", Store: st})
	s.ChatWS(o, nil, nil)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	c, _, err := websocket.DefaultDialer.DialContext(context.Background(),
		"ws"+strings.TrimPrefix(ts.URL, "http")+"/api/v1/chat/ws", nil)
	require.NoError(t, err)
	defer c.Close()
	require.NoError(t, c.WriteJSON(proto.NewUserMessage("hello")))

	var statusID string
	// Read frames until done. A no-tool-call fake model yields agent_chunk,
	// status, done. The status frame is what exec reads for the session id.
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	for {
		_, data, err := c.ReadMessage()
		require.NoError(t, err)
		var f proto.ServerFrame
		require.NoError(t, json.Unmarshal(data, &f))
		if f.Type == "status" {
			statusID = f.SessionID
		}
		if f.Type == "done" {
			break
		}
	}
	assert.NotEmpty(t, statusID, "status frame after a turn must carry the session id for --resume")
}
