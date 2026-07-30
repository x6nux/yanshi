package acp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// readLine reads a single newline-delimited line from r with a timeout.
func readLine(t *testing.T, r *bufio.Reader) string {
	t.Helper()
	type result struct {
		line string
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		line, err := r.ReadString('\n')
		ch <- result{line, err}
	}()
	select {
	case res := <-ch:
		if res.err != nil {
			t.Fatalf("readLine: %v", res.err)
		}
		return res.line
	case <-time.After(2 * time.Second):
		t.Fatal("readLine: timeout")
		return ""
	}
}

// TestCallRoundTrip verifies that Call writes a valid JSON-RPC Request line and
// correctly delivers the matching Response back to the caller.
func TestCallRoundTrip(t *testing.T) {
	// Client writes to clientOut; test reads from clientOutR.
	// Client reads from clientIn; test writes to clientInW.
	clientInR, clientInW := io.Pipe()
	clientOutR, clientOutW := io.Pipe()

	tr := NewTransport(clientInR, clientOutW)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start ReadLoop in background.
	go tr.ReadLoop(ctx)

	// Launch Call asynchronously.
	type callResult struct {
		result json.RawMessage
		err    error
	}
	callCh := make(chan callResult, 1)
	go func() {
		res, err := tr.Call(context.Background(), "initialize", map[string]any{"hi": 1})
		callCh <- callResult{res, err}
	}()

	// Read the request line written by the transport.
	br := bufio.NewReader(clientOutR)
	line := readLine(t, br)

	// Framing assertions.
	if !strings.HasSuffix(line, "\n") {
		t.Fatalf("line must end with \\n; got %q", line)
	}
	if strings.Count(line, "\n") != 1 {
		t.Fatalf("line must have exactly one \\n; got %q", line)
	}

	var req map[string]json.RawMessage
	if err := json.Unmarshal([]byte(line), &req); err != nil {
		t.Fatalf("unmarshal request: %v", err)
	}
	if string(req["jsonrpc"]) != `"2.0"` {
		t.Errorf("jsonrpc field = %s; want \"2.0\"", string(req["jsonrpc"]))
	}
	if _, ok := req["id"]; !ok {
		t.Error("request must have id")
	}
	if string(req["method"]) != `"initialize"` {
		t.Errorf("method = %s; want \"initialize\"", string(req["method"]))
	}

	// Write a matching response back.
	var idVal int64
	json.Unmarshal(req["id"], &idVal)
	resp := Response{JSONRPC: "2.0", ID: idVal, Result: json.RawMessage(`{"ok":true}`)}
	respBytes, _ := json.Marshal(resp)
	clientInW.Write(append(respBytes, '\n'))

	// Wait for Call to return.
	select {
	case cr := <-callCh:
		if cr.err != nil {
			t.Fatalf("Call returned error: %v", cr.err)
		}
		var check map[string]bool
		json.Unmarshal(cr.result, &check)
		if !check["ok"] {
			t.Errorf("result = %s; want {\"ok\":true}", string(cr.result))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Call did not return within 2s")
	}
}

// TestNotificationDelivery verifies that a Notification line is routed to onNotify.
func TestNotificationDelivery(t *testing.T) {
	clientInR, clientInW := io.Pipe()
	_, clientOutW := io.Pipe()
	tr := NewTransport(clientInR, clientOutW)

	var mu sync.Mutex
	gotMethod := ""
	gotParams := json.RawMessage{}

	tr.SetHandlers(
		func(method string, params json.RawMessage) {
			mu.Lock()
			defer mu.Unlock()
			gotMethod = method
			gotParams = params
		},
		nil,
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go tr.ReadLoop(ctx)

	// Write a notification (no id).
	notif := Notification{JSONRPC: "2.0", Method: "session/update", Params: map[string]any{"x": 1}}
	data, _ := json.Marshal(notif)
	clientInW.Write(append(data, '\n'))

	// Wait for handler.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		if gotMethod != "" {
			mu.Unlock()
			break
		}
		mu.Unlock()
		time.Sleep(time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	if gotMethod != "session/update" {
		t.Fatalf("onNotify method = %q; want \"session/update\"", gotMethod)
	}
	if string(gotParams) == "" {
		t.Fatal("onNotify params empty")
	}
}

// TestInboundRequest verifies that a server->client Request is routed to onRequest
// and the transport writes back a Response line with the same id.
func TestInboundRequest(t *testing.T) {
	clientInR, clientInW := io.Pipe()
	clientOutR, clientOutW := io.Pipe()
	tr := NewTransport(clientInR, clientOutW)

	tr.SetHandlers(nil, func(req inboundRequest) (json.RawMessage, error) {
		if req.Method != "fs/read_text_file" {
			t.Errorf("onRequest method = %q; want \"fs/read_text_file\"", req.Method)
		}
		return json.RawMessage(`{"content":"hello"}`), nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go tr.ReadLoop(ctx)

	// Write an inbound request.
	reqLine := `{"jsonrpc":"2.0","id":42,"method":"fs/read_text_file","params":{"path":"/tmp/x"}}`
	clientInW.Write([]byte(reqLine + "\n"))

	// Read the response that the transport writes back.
	br := bufio.NewReader(clientOutR)
	line := readLine(t, br)

	var resp map[string]json.RawMessage
	if err := json.Unmarshal([]byte(line), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if string(resp["id"]) != "42" {
		t.Errorf("response id = %s; want 42", string(resp["id"]))
	}
	if string(resp["result"]) != `{"content":"hello"}` {
		t.Errorf("response result = %s; want {\"content\":\"hello\"}", string(resp["result"]))
	}
}

// TestCallCancel verifies that a cancelled ctx makes Call return ctx.Err
// and does not leak the pending entry.
func TestCallCancel(t *testing.T) {
	clientInR, clientInW := io.Pipe()
	clientOutR, clientOutW := io.Pipe()
	_ = clientInW // not needed; we never respond
	tr := NewTransport(clientInR, clientOutW)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go tr.ReadLoop(ctx)

	// Drain pipe in background so writeLine doesn't block.
	go io.Copy(io.Discard, clientOutR)

	callCtx, callCancel := context.WithCancel(context.Background())

	type callResult struct {
		err error
	}
	callCh := make(chan callResult, 1)
	go func() {
		_, err := tr.Call(callCtx, "slow_method", nil)
		callCh <- callResult{err}
	}()

	// Give the transport a moment to write the request line and register pending.
	time.Sleep(50 * time.Millisecond)

	callCancel()

	select {
	case cr := <-callCh:
		if !errors.Is(cr.err, context.Canceled) {
			t.Fatalf("Call err = %v; want context.Canceled", cr.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Call did not return after cancel within 2s")
	}

	// Verify pending entry was cleaned up.
	tr.mu.Lock()
	leaked := len(tr.pending)
	tr.mu.Unlock()
	if leaked != 0 {
		t.Errorf("pending map has %d leaked entries after cancel", leaked)
	}
}
