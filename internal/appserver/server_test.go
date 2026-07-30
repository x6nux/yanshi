package appserver

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/x6nux/yanshi/internal/api/v1"
	einollm "github.com/x6nux/yanshi/internal/llm/eino"
)

// TestJSONRPCStreamNotificationIsVersionedItem drives a real turn/start
// stream and proves every item/updated notification carries the v1 wire
// contract: version="v1", sequence > 0, threadId/turnId populated. It runs
// setup+thread on the same agent first to resolve the thread id, then drives
// the turn and parses stdout line by line. The WaitGroup on the server
// guarantees all notifications are flushed before Serve returns.
func TestJSONRPCStreamNotificationIsVersionedItem(t *testing.T) {
	model := einollm.NewFakeModel([]string{"answer"}, nil)
	agent, err := v1.NewService(v1.Config{DefaultModel: model})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	// Setup: create a thread so we know the id to feed turn/start.
	var setupOut bytes.Buffer
	srv := New(agent, NewMemoryConfig())
	setupIn := `{"jsonrpc":"2.0","id":1,"method":"thread/start","params":{"title":"t"}}` + "\n"
	if err := srv.Serve(context.Background(), strings.NewReader(setupIn), &setupOut); err != nil {
		t.Fatalf("setup Serve: %v", err)
	}
	setupLines := strings.Split(strings.TrimRight(setupOut.String(), "\n"), "\n")
	if len(setupLines) < 1 {
		t.Fatalf("setup output empty: %q", setupOut.String())
	}
	var setup struct {
		Result struct {
			Thread struct {
				ID string `json:"id"`
			} `json:"thread"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(setupLines[0]), &setup); err != nil {
		t.Fatalf("parse setup response: %v", err)
	}
	if setup.Result.Thread.ID == "" {
		t.Fatalf("setup missing thread id: %s", setupLines[0])
	}

	// Turn: drive turn/start followed by shutdown so Serve returns cleanly.
	var out bytes.Buffer
	srv2 := New(agent, NewMemoryConfig())
	turnInput := strings.Join([]string{
		`{"jsonrpc":"2.0","id":10,"method":"turn/start","params":{"threadId":"` + setup.Result.Thread.ID + `","input":"hi"}}`,
		`{"jsonrpc":"2.0","id":12,"method":"shutdown","params":{}}`,
	}, "\n") + "\n"
	if err := srv2.Serve(context.Background(), strings.NewReader(turnInput), &out); err != nil {
		t.Fatalf("turn Serve: %v", err)
	}
	notified := false
	for _, ln := range strings.Split(strings.TrimRight(out.String(), "\n"), "\n") {
		var msg map[string]any
		if err := json.Unmarshal([]byte(ln), &msg); err != nil {
			t.Fatalf("stdout line not JSON: %v (%q)", err, ln)
		}
		if msg["method"] != "item/updated" {
			continue
		}
		notified = true
		params, ok := msg["params"].(map[string]any)
		if !ok {
			t.Fatalf("item/updated params missing: %s", ln)
		}
		if params["version"] != "v1" {
			t.Fatalf("item version = %#v, want v1", params["version"])
		}
		if seq, _ := params["sequence"].(float64); seq <= 0 {
			t.Fatalf("item sequence must be > 0: %#v", params["sequence"])
		}
		if params["threadId"] == "" || params["turnId"] == "" {
			t.Fatalf("item missing threadId/turnId: %s", ln)
		}
	}
	if !notified {
		t.Fatalf("expected at least one item/updated notification, got: %s", out.String())
	}
}

// TestJSONRPCNotificationHasNoResponseID proves a notification (request without
// id) does NOT produce a response — only the subsequent request with id=7 does.
// This is the JSON-RPC 2.0 spec rule that lets us stream item/updated events
// without ack noise.
func TestJSONRPCNotificationHasNoResponseID(t *testing.T) {
	model := einollm.NewFakeModel([]string{"answer"}, nil)
	agent, err := v1.NewService(v1.Config{DefaultModel: model})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	srv := New(agent, NewMemoryConfig())
	input := strings.Join([]string{
		`{"jsonrpc":"2.0","method":"capabilities","params":{}}`,
		`{"jsonrpc":"2.0","id":7,"method":"shutdown","params":{}}`,
	}, "\n") + "\n"
	var out bytes.Buffer
	if err := srv.Serve(context.Background(), strings.NewReader(input), &out); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) != 1 {
		t.Fatalf("notification must not produce a response; got %d lines: %q", len(lines), out.String())
	}
	var resp map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &resp); err != nil {
		t.Fatalf("response not JSON: %v", err)
	}
	id, _ := resp["id"].(float64)
	if id != 7 {
		t.Fatalf("response id = %#v, want 7 (echo of shutdown request)", resp["id"])
	}
}

// TestJSONRPCErrorCodes covers the standard error code matrix end-to-end:
// malformed JSON -> -32700, bad jsonrpc version -> -32600, unknown method ->
// -32601, invalid params (missing input) -> -32602, secret config/write ->
// -32602. Each case runs on its own Server so the agent state is isolated.
func TestJSONRPCErrorCodes(t *testing.T) {
	model := einollm.NewFakeModel([]string{"answer"}, nil)
	agent, err := v1.NewService(v1.Config{DefaultModel: model})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	cases := []struct {
		name     string
		line     string
		wantCode int64
	}{
		{"malformed json -> parse error", `{not json`, codeParseError},
		{"bad jsonrpc version", `{"jsonrpc":"1.0","id":1,"method":"initialize"}`, codeInvalidRequest},
		{"unknown method -> not found", `{"jsonrpc":"2.0","id":2,"method":"nope/x","params":{}}`, codeMethodNotFound},
		{"missing input -> invalid params", `{"jsonrpc":"2.0","id":3,"method":"turn/start","params":{"threadId":"t"}}`, codeInvalidParams},
		{"config/write secret -> invalid params", `{"jsonrpc":"2.0","id":4,"method":"config/write","params":{"key":"api_key","value":"x"}}`, codeInvalidParams},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := New(agent, NewMemoryConfig())
			var out bytes.Buffer
			input := tc.line + "\n"
			_ = srv.Serve(context.Background(), strings.NewReader(input), &out)
			lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
			if len(lines) == 0 {
				t.Fatalf("no response for %q", tc.line)
			}
			var resp struct {
				Error *struct {
					Code int64 `json:"code"`
				} `json:"error"`
			}
			if err := json.Unmarshal([]byte(lines[0]), &resp); err != nil {
				t.Fatalf("response not JSON: %v (%q)", err, lines[0])
			}
			if resp.Error == nil || resp.Error.Code != tc.wantCode {
				t.Fatalf("error = %+v, want code %d (line=%q)", resp.Error, tc.wantCode, lines[0])
			}
		})
	}
}
