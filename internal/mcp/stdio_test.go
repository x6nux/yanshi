package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"sync"
	"testing"
	"time"
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

// --- bidirectional readLoop tests ---

// bidirServer is a test helper that runs a fake MCP server over io.Pipe.
// It handles synchronous request/response (for handshake and SendRequest)
// and can send notifications and requests to the client.
type bidirServer struct {
	t      *testing.T
	conn   io.ReadCloser  // server reads from client's writes
	writer io.WriteCloser // server writes to client's reads
	w      *bufio.Writer
	buf    *bufio.Reader
	mu     sync.Mutex // serializes all reads from buf
}

func newBidirServer(t *testing.T) (*bidirServer, *StdioClient) {
	t.Helper()
	srvInR, srvInW := io.Pipe()   // client → server
	srvOutR, srvOutW := io.Pipe() // server → client

	srv := &bidirServer{
		t:      t,
		conn:   srvInR,
		writer: srvOutW,
		w:      bufio.NewWriter(srvOutW),
		buf:    bufio.NewReader(srvInR),
	}

	cli := NewStdioClient(srvOutR, srvInW)
	return srv, cli
}

// handleOne reads and responds to one client request. Used by handshake.
func (s *bidirServer) handleOne() {
	s.mu.Lock()
	defer s.mu.Unlock()
	msg, err := ReadMessage(s.buf)
	if err != nil {
		return
	}
	id := msg["id"]
	if id == nil {
		return
	}
	method, _ := msg["method"].(string)
	resp := map[string]any{"jsonrpc": "2.0", "id": id}
	switch method {
	case "initialize":
		resp["result"] = map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}}
	case "ping":
		resp["result"] = map[string]any{}
	case "tools/list":
		resp["result"] = map[string]any{"tools": []any{}}
	}
	data, _ := json.Marshal(resp)
	s.w.Write(data)
	s.w.WriteByte('\n')
	s.w.Flush()
}

// handshake performs the standard client→server initialize + ping.
func (s *bidirServer) handshake(cli *StdioClient) {
	s.t.Helper()
	// Initialize sends: initialize request + notifications/initialized notification.
	// handleOne reads and responds to the initialize request, then continues reading
	// to drain the notifications/initialized notification (otherwise the client's
	// notify write blocks on the pipe).
	go func() {
		s.handleOne()
		// Drain the notifications/initialized notification (id=nil, just consume).
		s.mu.Lock()
		_, _ = ReadMessage(s.buf)
		s.mu.Unlock()
	}()
	if err := cli.Initialize(context.Background(), "/"); err != nil {
		s.t.Fatalf("Initialize: %v", err)
	}
}

// SendNotification writes a server-initiated notification (no id) to the client.
func (s *bidirServer) SendNotification(method string, params map[string]any) {
	s.t.Helper()
	msg := map[string]any{"jsonrpc": "2.0", "method": method}
	if params != nil {
		msg["params"] = params
	}
	data, _ := json.Marshal(msg)
	s.w.Write(data)
	s.w.WriteByte('\n')
	s.w.Flush()
}

// SendRequest writes a server-initiated request (with id + method) and reads
// the client's response synchronously.
func (s *bidirServer) SendRequest(id int64, method string, params map[string]any) map[string]any {
	s.t.Helper()
	msg := map[string]any{"jsonrpc": "2.0", "id": id, "method": method}
	if params != nil {
		msg["params"] = params
	}
	data, _ := json.Marshal(msg)
	s.w.Write(data)
	s.w.WriteByte('\n')
	s.w.Flush()

	s.mu.Lock()
	defer s.mu.Unlock()
	resp, err := ReadMessage(s.buf)
	if err != nil {
		s.t.Fatalf("SendRequest: read response: %v", err)
	}
	return resp
}

// SendRequestNoReply writes a server-initiated request without reading a response.
func (s *bidirServer) SendRequestNoReply(id int64, method string, params map[string]any) {
	s.t.Helper()
	msg := map[string]any{"jsonrpc": "2.0", "id": id, "method": method}
	if params != nil {
		msg["params"] = params
	}
	data, _ := json.Marshal(msg)
	s.w.Write(data)
	s.w.WriteByte('\n')
	s.w.Flush()
}

// AC1: server notification is dispatched to handler, not dropped.
func TestStdioClient_ServerNotificationDispatched(t *testing.T) {
	srv, cli := newBidirServer(t)

	var mu sync.Mutex
	var received []string
	cli.SetHandler(func(method string, params map[string]any) {
		mu.Lock()
		received = append(received, method)
		mu.Unlock()
	})

	srv.handshake(cli)

	// Server pushes a notification.
	srv.SendNotification("notifications/tools/list_changed", nil)

	// Give readLoop time to dispatch.
	time.Sleep(50 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if len(received) != 1 || received[0] != "notifications/tools/list_changed" {
		t.Fatalf("expected [notifications/tools/list_changed], got %v", received)
	}
}

// AC3: progress notifications from a long task reach the handler.
func TestStdioClient_ProgressNotificationDelivered(t *testing.T) {
	srv, cli := newBidirServer(t)

	var mu sync.Mutex
	var progresses []float64
	cli.SetHandler(func(method string, params map[string]any) {
		if method == "notifications/progress" {
			mu.Lock()
			if p, ok := params["progress"].(float64); ok {
				progresses = append(progresses, p)
			}
			mu.Unlock()
		}
	})

	srv.handshake(cli)

	// Simulate a series of progress notifications.
	for _, p := range []float64{10, 50, 90, 100} {
		srv.SendNotification("notifications/progress", map[string]any{
			"progressToken": "task-1",
			"progress":      p,
			"total":         float64(100),
		})
	}

	time.Sleep(50 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if len(progresses) != 4 {
		t.Fatalf("expected 4 progress values, got %d: %v", len(progresses), progresses)
	}
	expected := []float64{10, 50, 90, 100}
	for i, v := range progresses {
		if v != expected[i] {
			t.Fatalf("progress[%d] = %v, want %v", i, v, expected[i])
		}
	}
}

// AC4: listChanged notification reaches handler (the mechanism; tool refresh
// is the Manager's responsibility).
func TestStdioClient_ListChangedNotification(t *testing.T) {
	srv, cli := newBidirServer(t)

	var mu sync.Mutex
	var listChanged bool
	cli.SetHandler(func(method string, params map[string]any) {
		if method == "notifications/tools/list_changed" {
			mu.Lock()
			listChanged = true
			mu.Unlock()
		}
	})

	srv.handshake(cli)
	srv.SendNotification("notifications/tools/list_changed", nil)
	time.Sleep(50 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if !listChanged {
		t.Fatal("expected listChanged notification to reach handler")
	}
}

// No handler registered: notifications are silently dropped without panic.
func TestStdioClient_NoHandlerDropsSilently(t *testing.T) {
	srv, cli := newBidirServer(t)
	// No SetHandler call.

	srv.handshake(cli)

	// Send a notification — should not panic.
	srv.SendNotification("notifications/tools/list_changed", nil)

	time.Sleep(50 * time.Millisecond)
	// No assertion needed — we're testing that it doesn't panic.
}

// Close wakes up blocked doRequest and readLoop exits cleanly.
func TestStdioClient_CloseWakesPending(t *testing.T) {
	srv, cli := newBidirServer(t)

	cli.SetHandler(func(string, map[string]any) {})

	srv.handshake(cli)

	// Drain messages from the server side so client writes don't block on a
	// pipe with no reader (io.Pipe is synchronous: a write blocks until read).
	go func() {
		for {
			srv.mu.Lock()
			_, err := ReadMessage(srv.buf)
			srv.mu.Unlock()
			if err != nil {
				return
			}
		}
	}()

	// Ping in a goroutine; Close should wake its pending wait.
	errCh := make(chan error, 1)
	go func() {
		errCh <- cli.Ping(context.Background())
	}()

	// Give Ping time to register its pending entry and write.
	time.Sleep(20 * time.Millisecond)
	if err := cli.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("expected error from Ping after Close")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Ping did not return after Close")
	}

	// After Close, done should be closed.
	select {
	case <-cli.Done():
	default:
		t.Fatal("expected Done() to be closed after Close")
	}
}
