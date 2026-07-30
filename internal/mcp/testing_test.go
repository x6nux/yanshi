package mcp

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"
)

// TestNewFakeHTTPServer_Responds exercises the fake MCP server via raw HTTP
// (not the MCP Client interface, which is defined in later tasks). This
// keeps Task3 leaf-code only — clients are added in Tasks 4-5.
func TestNewFakeHTTPServer_Responds(t *testing.T) {
	ts, fake := NewFakeHTTPServer([]ToolDescriptor{
		{ServerName: "test", ToolName: "greet", Qualified: "mcp_test_greet", Description: "say hello"},
	})
	defer ts.Close()

	post := func(t *testing.T, method string, id any) map[string]any {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method})
		resp, err := http.Post(ts.URL, "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatalf("POST %s: %v", method, err)
		}
		defer resp.Body.Close()
		var out map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatalf("decode %s: %v", method, err)
		}
		return out
	}

	initResp := post(t, "initialize", 1)
	res, ok := initResp["result"].(map[string]any)
	if !ok || res["protocolVersion"] != "2025-06-18" {
		t.Fatalf("initialize: %+v", initResp)
	}

	toolResp := post(t, "tools/list", 2)
	res, ok = toolResp["result"].(map[string]any)
	if !ok {
		t.Fatalf("tools/list no result: %+v", toolResp)
	}
	tools, _ := res["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(tools))
	}

	callResp := post(t, "tools/call", 3)
	if _, ok := callResp["error"]; ok {
		t.Fatalf("tools/call error: %+v", callResp)
	}
	if fake.CallCount != 1 {
		t.Fatalf("expected 1 call, got %d", fake.CallCount)
	}

	pingResp := post(t, "ping", 4)
	if _, ok := pingResp["error"]; ok {
		t.Fatalf("ping error: %+v", pingResp)
	}
}
