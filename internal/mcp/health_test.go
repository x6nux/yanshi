package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// ledger: A3/V16#3 启动超时/重连/权限检查有测试
func TestManager_StartupTimeout(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Method == "initialize" {
			time.Sleep(200 * time.Millisecond)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{
				"protocolVersion": "2025-06-18", "capabilities": map[string]any{},
			},
		})
	}))
	defer ts.Close()

	mgr := NewManager(map[string]*ServerConfig{"slow": {
		Name: "slow", Enabled: true, Transport: TransportHTTP, URL: ts.URL,
	}})
	mgr.SetHealthConfig(HealthConfig{StartupTimeout: 50 * time.Millisecond})
	statuses := mgr.StartAll(context.Background())
	if len(statuses) != 1 || statuses[0].Status != StatusFailed {
		t.Fatalf("expected StatusFailed, got %+v", statuses)
	}
	if statuses[0].Error == "" {
		t.Fatalf("expected non-empty Error, got %+v", statuses[0])
	}
}

func TestManager_HealthLoop_MarksFailed(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		switch req.Method {
		case "initialize":
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}}})
		case "tools/list":
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"tools": []map[string]any{}}})
		case "ping":
			w.WriteHeader(http.StatusInternalServerError)
		default:
			w.WriteHeader(http.StatusAccepted)
		}
	}))
	defer ts.Close()

	mgr := NewManager(map[string]*ServerConfig{"flaky": {
		Name: "flaky", Enabled: true, Transport: TransportHTTP, URL: ts.URL, Reconnect: false,
	}})
	mgr.SetHealthConfig(HealthConfig{Enabled: true, Interval: 20 * time.Millisecond})
	st := mgr.StartAll(context.Background())
	if len(st) != 1 || st[0].Status != StatusReady {
		t.Fatalf("StartAll should succeed, got %+v", st)
	}
	cancel := mgr.StartHealthLoop(context.Background())
	defer cancel()

	deadline := time.After(2 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatal("server never marked failed by health loop")
		default:
		}
		st = mgr.Snapshot(context.Background())
		if len(st) == 1 && st[0].Status == StatusFailed && st[0].Error != "" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// ledger: A3/V16#3 启动超时/重连/权限检查有测试
func TestManager_CallToolRetry_Reconnects(t *testing.T) {
	var callAttempts, inits atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		switch req.Method {
		case "initialize":
			inits.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}}})
		case "tools/list":
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"tools": []map[string]any{{"name": "ping", "inputSchema": map[string]any{"type": "object"}}}}})
		case "tools/call":
			if callAttempts.Add(1) == 1 {
				w.WriteHeader(http.StatusBadGateway)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"content": []map[string]any{{"type": "text", "text": `{"ok":true}`}}}})
		case "ping":
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{}})
		default:
			w.WriteHeader(http.StatusAccepted)
		}
	}))
	defer ts.Close()

	mgr := NewManager(map[string]*ServerConfig{"r": {
		Name: "r", Enabled: true, Transport: TransportHTTP, URL: ts.URL, Reconnect: true,
	}})
	mgr.SetHealthConfig(HealthConfig{
		Enabled:                 false,
		ReconnectInitialBackoff: 1 * time.Millisecond,
		ReconnectMaxBackoff:     5 * time.Millisecond,
	})
	st := mgr.StartAll(context.Background())
	if len(st) != 1 || st[0].Status != StatusReady {
		t.Fatalf("StartAll: %+v", st)
	}

	res, err := mgr.CallToolRetry(context.Background(), "mcp_r_ping", []byte(`{}`))
	if err != nil {
		t.Fatalf("CallToolRetry: %v", err)
	}
	if string(res) != `{"ok":true}` {
		t.Fatalf("res=%s", res)
	}
	if callAttempts.Load() != 2 {
		t.Fatalf("expected 2 call attempts, got %d", callAttempts.Load())
	}
	if inits.Load() != 2 {
		t.Fatalf("expected 2 initialize calls (StartAll + reconnect), got %d", inits.Load())
	}
}

func TestManager_Reconnect_SingleFlight(t *testing.T) {
	var inits atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		switch req.Method {
		case "initialize":
			inits.Add(1)
			time.Sleep(40 * time.Millisecond)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{
					"protocolVersion": "2025-06-18", "capabilities": map[string]any{},
				},
			})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{}})
		}
	}))
	defer ts.Close()

	mgr := NewManager(map[string]*ServerConfig{"s": {
		Name: "s", Enabled: true, Transport: TransportHTTP, URL: ts.URL, Reconnect: true,
	}})
	mgr.SetHealthConfig(HealthConfig{
		Enabled:                 false,
		ReconnectInitialBackoff: 1 * time.Millisecond,
		ReconnectMaxBackoff:     5 * time.Millisecond,
	})
	mgr.StartAll(context.Background())

	mgr.markFailed("s", "test-failure")
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() { errs <- mgr.reconnect(context.Background(), "s") }()
	}
	for i := 0; i < 2; i++ {
		if err := <-errs; err != nil {
			t.Logf("reconnect returned %v (single-flight coalesced)", err)
		}
	}
	if inits.Load() != 2 {
		t.Fatalf("single-flight broken: inits=%d", inits.Load())
	}
}
