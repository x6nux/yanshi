package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestHTTPClient_StreamableHTTPJSONAndSession(t *testing.T) {
	const session = "session-123"
	var sawSession atomic.Bool
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Mcp-Session-Id") == session {
			sawSession.Store(true)
		}
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Mcp-Session-Id", session)
		w.Header().Set("Content-Type", "application/json")
		resp := map[string]any{"jsonrpc": "2.0", "id": req.ID}
		switch req.Method {
		case "initialize":
			resp["result"] = map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}}
		case "tools/list":
			resp["result"] = map[string]any{"tools": []map[string]any{}}
		default:
			w.WriteHeader(http.StatusAccepted)
			return
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer ts.Close()

	client := NewHTTPClient(ts.URL, "")
	if err := client.Initialize(context.Background(), "/"); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if _, err := client.ListTools(context.Background()); err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if !sawSession.Load() {
		t.Fatal("subsequent request did not send Mcp-Session-Id")
	}
}

func TestHTTPClient_DecodesSSEResponse(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID json.RawMessage `json:"id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":%s,\"result\":{\"tools\":[]}}\n\n", req.ID)
	}))
	defer ts.Close()

	client := NewHTTPClient(ts.URL, "")
	tools, err := client.ListTools(context.Background())
	if err != nil {
		t.Fatalf("ListTools SSE: %v", err)
	}
	if len(tools) != 0 {
		t.Fatalf("tools = %+v", tools)
	}
}

func TestHTTPClient_RefreshesOAuthOnceOn401(t *testing.T) {
	var tokenCalls atomic.Int32
	tokens := &rotatingTokenSource{calls: &tokenCalls}
	var requests atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := requests.Add(1)
		if n == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.Header.Get("Authorization") != "Bearer token-2" {
			t.Fatalf("Authorization = %q", r.Header.Get("Authorization"))
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0", "id": 1, "result": map[string]any{"tools": []any{}},
		})
	}))
	defer ts.Close()

	client := NewHTTPClientWithTokenSource(ts.URL, tokens)
	if _, err := client.ListTools(context.Background()); err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if requests.Load() != 2 || tokenCalls.Load() != 2 {
		t.Fatalf("requests=%d tokenCalls=%d", requests.Load(), tokenCalls.Load())
	}
}

type rotatingTokenSource struct {
	calls *atomic.Int32
}

func (s *rotatingTokenSource) Token(context.Context) (string, error) {
	return fmt.Sprintf("token-%d", s.calls.Add(1)), nil
}
func (s *rotatingTokenSource) Invalidate() {}

func TestDecodeSSERejectsMissingData(t *testing.T) {
	_, err := decodeSSE(strings.NewReader("event: message\n\n"))
	if err == nil {
		t.Fatal("missing data should fail")
	}
}
