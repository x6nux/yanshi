package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"testing"
)

func fakeStdioProcess(t *testing.T) (io.Writer, io.Reader, func()) {
	t.Helper()
	srvInR, srvInW := io.Pipe()
	srvOutR, srvOutW := io.Pipe()
	srvBuf := bufio.NewReader(srvInR)
	go func() {
		defer srvInR.Close()
		defer srvOutW.Close()
		for {
			msg, err := ReadMessage(srvBuf)
			if err != nil {
				return
			}
			id := msg["id"]
			method, _ := msg["method"].(string)
			if id == nil { // initialized notification
				continue
			}
			resp := map[string]any{"jsonrpc": "2.0", "id": id}
			switch method {
			case "initialize":
				resp["result"] = map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}}
			case "tools/list":
				resp["result"] = map[string]any{"tools": []map[string]any{{"name": "echo", "description": "echo", "inputSchema": map[string]any{"type": "object"}}}}
			case "tools/call":
				resp["result"] = map[string]any{"content": []map[string]any{{"type": "text", "text": `{"echoed":true}`}}}
			case "resources/list":
				resp["result"] = map[string]any{"resources": []map[string]any{}}
			case "ping":
				resp["result"] = map[string]any{}
			}
			data, _ := json.Marshal(resp)
			_, _ = srvOutW.Write(append(data, '\n'))
		}
	}()
	return srvInW, srvOutR, func() { _ = srvInW.Close(); _ = srvOutR.Close() }
}

func TestStdioClient_InitializeListCallPing(t *testing.T) {
	w, r, cleanup := fakeStdioProcess(t)
	defer cleanup()
	cli := NewStdioClient(r, w)
	if err := cli.Initialize(context.Background(), "/test"); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	tools, err := cli.ListTools(context.Background())
	if err != nil || len(tools) != 1 || tools[0].ToolName != "echo" {
		t.Fatalf("ListTools: tools=%+v err=%v", tools, err)
	}
	res, err := cli.CallTool(context.Background(), "echo", json.RawMessage(`{}`))
	if err != nil || string(res) != `{"echoed":true}` {
		t.Fatalf("CallTool: res=%q err=%v", string(res), err)
	}
	if err := cli.Ping(context.Background()); err != nil {
		t.Fatalf("Ping: %v", err)
	}
}
