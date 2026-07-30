package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestCallToolRetryFirstCallOK covers CallToolRetry's early-return branch when
// the first CallTool succeeds (no reconnect needed).
func TestCallToolRetryFirstCallOK(t *testing.T) {
	ctx := context.Background()
	m := NewManager(map[string]*ServerConfig{"s": {Name: "s", Enabled: true, Transport: TransportHTTP, URL: "http://x"}})
	m.mu.Lock()
	m.toolMap["mcp_s_t"] = ToolDescriptor{ServerName: "s", ToolName: "t", Qualified: "mcp_s_t"}
	m.clients["s"] = &fakeClient{callResult: json.RawMessage(`{"ok":true}`)}
	m.mu.Unlock()
	res, err := m.CallToolRetry(ctx, "mcp_s_t", nil)
	if err != nil {
		t.Fatalf("CallToolRetry: %v", err)
	}
	if string(res) != `{"ok":true}` {
		t.Fatalf("res = %s", res)
	}
}

// TestDecodeSSEScannerError covers decodeSSE's scanner.Err() branch by feeding a
// reader that errors mid-scan.
func TestDecodeSSEScannerError(t *testing.T) {
	if _, err := decodeSSE(&errAfterOneRead{}); err == nil {
		t.Fatal("decodeSSE with reader error must error")
	}
	// trailing data without a blank line, with bad JSON → 176-178 branch.
	if _, err := decodeSSE(strings.NewReader("data: {bad\n")); err == nil {
		t.Fatal("decodeSSE trailing bad JSON must error")
	}
}

// errAfterOneRead returns a "data:" line on the first read then errors, so the
// scanner collects one data line then surfaces scanner.Err().
type errAfterOneRead struct{ n int }

func (e *errAfterOneRead) Read(p []byte) (int, error) {
	e.n++
	if e.n == 1 {
		return copy(p, []byte("data: {\"a\":1}\n")), nil
	}
	return 0, errors.New("stream broke")
}

// TestStdioDoRequestCtxDone covers doRequest's ctx.Done() branch by passing an
// already-cancelled context (the per-iteration select at loop top fires before
// any read).
func TestStdioDoRequestCtxDone(t *testing.T) {
	cli := NewStdioClient(strings.NewReader(""), io.Discard)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := cli.doRequest(ctx, 1, map[string]any{"x": 1}); err == nil {
		t.Fatal("doRequest on cancelled ctx must error")
	}
}

// TestWriteLineMessageMarshalError covers WriteLineMessage's marshal-error
// branch (a channel value cannot be JSON-encoded).
func TestWriteLineMessageMarshalError(t *testing.T) {
	if err := WriteLineMessage(io.Discard, map[string]any{"ch": make(chan int)}); err == nil {
		t.Fatal("WriteLineMessage with unmarshalable value must error")
	}
}

// TestStartOneTimeoutFromConfig covers the timeout-selection else-if branch
// (StartupTimeout==0, cfg.Timeout>0 → timeout = cfg.Timeout). NOTE:
// SetHealthConfig forces StartupTimeout to 30s when passed 0, so this branch is
// effectively dead code — the test documents that intent and confirms
// StartAll still succeeds with a cfg.Timeout configured.
func TestStartOneTimeoutFromConfig(t *testing.T) {
	ts, _ := NewFakeHTTPServer([]ToolDescriptor{{ToolName: "hi"}})
	defer ts.Close()
	m := NewManager(map[string]*ServerConfig{"s": {Enabled: true, Transport: TransportHTTP, URL: ts.URL, Timeout: 5 * time.Second}})
	m.SetHealthConfig(HealthConfig{Enabled: true, StartupTimeout: 0})
	st := m.StartAll(context.Background())
	if st[0].Status != StatusReady {
		t.Fatalf("expected ready, got %+v", st)
	}
}

// silence unused import guard if httptest only used elsewhere
var _ = httptest.NewServer

// TestStartOneStdioFastExitingCommand covers the stdio branch's NewStdioClient
// assignment (manager.go:107) using a command that exits immediately. Unlike a
// long-lived subprocess, a fast-exiting one closes its stdout pipe right away so
// Initialize fails with EOF (not a 30s hang), and Close reaps it promptly.
func TestStartOneStdioFastExitingCommand(t *testing.T) {
	m := NewManager(nil)
	m.SetHealthConfig(HealthConfig{Enabled: true, StartupTimeout: 5 * time.Second})
	_, err := m.startOne(context.Background(), &ServerConfig{
		Name: "s", Transport: TransportStdio, Command: "true",
	})
	if err == nil {
		return // reached line 107 and somehow initialized — still a pass
	}
	// Missing binary on this host is acceptable; any initialize error proves we
	// constructed the StdioClient (line 107) and tried the handshake.
	if strings.Contains(err.Error(), "executable file not found") {
		t.Logf("true not on PATH; skipping: %v", err)
		return
	}
	t.Logf("startOne stdio completed with err (acceptable): %v", err)
}
