package mcp

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
)

// NewFakeHTTPServer 启动一个最小化 MCP-over-HTTP 测试端点。
func NewFakeHTTPServer(tools []ToolDescriptor) (*httptest.Server, *FakeServer) {
	fs := &FakeServer{Tools: tools}
	ts := httptest.NewServer(http.HandlerFunc(fs.handle))
	fs.Server = ts
	return ts, fs
}

// FakeServer 是 NewFakeHTTPServer 返回的可观测辅助对象。
type FakeServer struct {
	Server    *httptest.Server
	Tools     []ToolDescriptor
	CallCount int
}

func (f *FakeServer) handle(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var req struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
	}
	_ = json.Unmarshal(body, &req)

	resp := map[string]any{"jsonrpc": "2.0"}
	if len(req.ID) > 0 {
		resp["id"] = json.RawMessage(req.ID)
	}
	switch req.Method {
	case "notifications/initialized":
		w.WriteHeader(http.StatusAccepted)
		return
	case "initialize":
		resp["result"] = map[string]any{
			"protocolVersion": "2025-06-18",
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "fake-mcp", "version": "0.1"},
		}
	case "tools/list":
		toolObjs := make([]map[string]any, len(f.Tools))
		for i, td := range f.Tools {
			toolObjs[i] = map[string]any{
				"name":        td.ToolName,
				"description": td.Description,
				"inputSchema": map[string]any{"type": "object"},
			}
		}
		resp["result"] = map[string]any{"tools": toolObjs}
	case "tools/call":
		f.CallCount++
		resp["result"] = map[string]any{
			"content": []map[string]any{{"type": "text", "text": `{"ok":true}`}},
		}
	case "resources/list":
		resp["result"] = map[string]any{"resources": []map[string]any{}}
	case "ping":
		resp["result"] = map[string]any{}
	default:
		resp["error"] = map[string]any{"code": -32601, "message": "unknown method"}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}
