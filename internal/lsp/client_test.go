package lsp

import (
	"bufio"
	"context"
	"io"
	"net"
	"testing"
	"time"
)

// fakeServerConn 起一个最小 LSP server:在 srv 端用持久 bufio.Reader 循环读,
// 每条消息交给 handle。返回 client 端的 net.Conn。
func fakeServerConn(t *testing.T, handle func(srv net.Conn, br *bufio.Reader, msg map[string]any)) net.Conn {
	t.Helper()
	srv, cli := net.Pipe()
	go func() {
		defer srv.Close()
		br := bufio.NewReader(srv)
		for {
			msg, err := ReadMessage(br)
			if err != nil {
				return
			}
			handle(srv, br, msg)
		}
	}()
	return cli
}

func TestClient_Initialize(t *testing.T) {
	cli := fakeServerConn(t, func(srv net.Conn, _ *bufio.Reader, msg map[string]any) {
		if method, _ := msg["method"].(string); method == "initialize" {
			_ = WriteMessage(srv, map[string]any{
				"jsonrpc": "2.0", "id": msg["id"],
				"result": map[string]any{"capabilities": map[string]any{}},
			})
			return
		}
		// 其它(initialized / shutdown / exit)读后忽略;吞到对端关 pipe。
		io.Copy(io.Discard, srv)
	})

	cl := newClient(cli, cli, 2*time.Second)
	cl.Start()
	if err := cl.initialize("/work"); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	cl.Close()
	cli.Close()
}

// TestClient_NotifyChange_PublishesFreshDiagnostics 验证完整路径:
// initialize → notifyChange(didOpen) → server 推 publishDiagnostics →
// Diagnostics 在 generation 边界内拿到本次诊断。
func TestClient_NotifyChange_PublishesFreshDiagnostics(t *testing.T) {
	uri := "file:///work/main.go"
	cli := fakeServerConn(t, func(srv net.Conn, _ *bufio.Reader, msg map[string]any) {
		switch msg["method"] {
		case "initialize":
			_ = WriteMessage(srv, map[string]any{
				"jsonrpc": "2.0", "id": msg["id"],
				"result": map[string]any{"capabilities": map[string]any{}},
			})
		case "textDocument/didOpen", "textDocument/didChange":
			// 收到变更即推一条 error 诊断。
			_ = WriteMessage(srv, map[string]any{
				"jsonrpc": "2.0", "method": "textDocument/publishDiagnostics",
				"params": map[string]any{
					"uri": uri,
					"diagnostics": []map[string]any{{
						"range":    map[string]any{"start": map[string]any{"line": 0, "character": 0}},
						"severity": 1,
						"message":  "undefined: x",
						"source":   "gopls",
					}},
				},
			})
		}
	})

	cl := newClient(cli, cli, 2*time.Second)
	cl.Start()
	if err := cl.initialize("/work"); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	if err := cl.notifyChange(uri, "package main\n"); err != nil {
		t.Fatalf("notifyChange: %v", err)
	}
	diags := cl.Diagnostics(uri, time.Second)
	if len(diags) != 1 || diags[0].Message != "undefined: x" {
		t.Fatalf("期望 1 条诊断 undefined: x,得到 %+v", diags)
	}
	_ = cl.Shutdown(context.Background())
	cl.Close()
	cli.Close()
}

// TestClient_DiagnosticsNoEditNoBlock 验证 generation 边界:
// 没有任何 didChange 时 editGen=0,Diagnostics 不等(直接返回当前 diags,可能空);
// 不会把"还没到的本次编辑诊断"误判为已就绪。这里只断言不阻塞、不 panic。
func TestClient_DiagnosticsNoEditNoBlock(t *testing.T) {
	cli := fakeServerConn(t, func(srv net.Conn, _ *bufio.Reader, msg map[string]any) {
		if msg["method"] == "initialize" {
			_ = WriteMessage(srv, map[string]any{
				"jsonrpc": "2.0", "id": msg["id"],
				"result": map[string]any{"capabilities": map[string]any{}},
			})
		}
	})
	cl := newClient(cli, cli, time.Second)
	cl.Start()
	if err := cl.initialize("/work"); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	// 无编辑(editGen=0):Diagnostics 立即返回(nil),不等到超时。
	start := time.Now()
	diags := cl.Diagnostics("file:///work/x.go", 500*time.Millisecond)
	if diags != nil {
		t.Fatalf("未编辑的 uri 应返回 nil,得到 %+v", diags)
	}
	if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
		t.Fatalf("未编辑时 Diagnostics 不应阻塞 %v", elapsed)
	}
	cl.Close()
	cli.Close()
}
