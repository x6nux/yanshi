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
	"github.com/x6nux/yanshi/internal/mcp"
	"github.com/x6nux/yanshi/internal/proto"
)

// TestMCP_E2E_WaitsForInitialization confirms the ws handler routes
// mcp_action and returns mcp_status.
func TestMCP_E2E_WaitsForInitialization(t *testing.T) {
	// Start a fake MCP HTTP server.
	ts, _ := mcp.NewFakeHTTPServer([]mcp.ToolDescriptor{{ToolName: "echo"}})
	defer ts.Close()

	// Build an MCP manager with the fake server.
	mgr := mcp.NewManager(map[string]*mcp.ServerConfig{"s": {
		Name: "s", Enabled: true, Transport: mcp.TransportHTTP, URL: ts.URL,
	}})
	st := mgr.StartAll(context.Background())
	require.Len(t, st, 1, "expected one server")
	require.Equal(t, mcp.StatusReady, st[0].Status, "server should be ready")

	// Create a minimal HTTP server with MCP wired.
	o, err := orchestrator.New(orchestrator.Config{Model: einollm.NewFakeModel([]string{"ok"}, nil)})
	require.NoError(t, err)
	s := New(Config{Token: "t", MCP: mgr})
	s.ChatWS(o, nil, nil)
	hs := httptest.NewServer(s.Handler())
	defer hs.Close()

	// Connect via WS with token auth.
	u := "ws" + strings.TrimPrefix(hs.URL, "http") + "/api/v1/chat/ws"
	hdr := map[string][]string{"Authorization": {"Bearer t"}}
	c, _, err := websocket.DefaultDialer.Dial(u, hdr)
	require.NoError(t, err)
	defer c.Close()

	// Send mcp_action:list.
	err = c.WriteJSON(proto.NewMCPAction("", "list"))
	require.NoError(t, err)

	// Read messages until we get mcp_status (skip initial session_list frame).
	var f proto.ServerFrame
	for i := 0; i < 3; i++ {
		c.SetReadDeadline(time.Now().Add(2 * time.Second))
		_, data, err := c.ReadMessage()
		require.NoError(t, err)
		err = json.Unmarshal(data, &f)
		require.NoError(t, err)
		if f.Type == "mcp_status" {
			break
		}
	}
	assert.Equal(t, "mcp_status", f.Type, "expected mcp_status after mcp_action:list")
	require.Len(t, f.MCPServers, 1, "expected one server in snapshot")
	assert.Equal(t, "s", f.MCPServers[0].Name)
	assert.Equal(t, "ready", f.MCPServers[0].Status)
	assert.Equal(t, 1, f.MCPServers[0].ToolCount)
	require.Len(t, f.MCPServers[0].Tools, 1, "expected one tool brief")
	assert.Equal(t, "mcp_s_echo", f.MCPServers[0].Tools[0].Name)

	// Send mcp_action:disable and read the mcp_status response.
	err = c.WriteJSON(proto.NewMCPAction("s", "disable"))
	require.NoError(t, err)

	for i := 0; i < 3; i++ {
		c.SetReadDeadline(time.Now().Add(2 * time.Second))
		_, data, err := c.ReadMessage()
		require.NoError(t, err)
		err = json.Unmarshal(data, &f)
		require.NoError(t, err)
		if f.Type == "mcp_status" {
			break
		}
	}
	assert.Equal(t, "mcp_status", f.Type)
	require.Len(t, f.MCPServers, 1, "expected one server after disable")
	assert.Equal(t, "stopped", f.MCPServers[0].Status)
	assert.Equal(t, 0, f.MCPServers[0].ToolCount, "tools should be removed on disable")
}
