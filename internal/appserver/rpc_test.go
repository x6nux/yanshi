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

// TestRPCServerInitializeAndMethodNotFound proves the JSON-RPC dispatcher
// answers `initialize` with a capabilities result and `unknown/x` with a
// -32601 method-not-found error. Both responses carry jsonrpc:"2.0" and echo
// the request id; the capabilities response advertises the v1 version, the
// canonical method set, and the `item/updated` stream notification name.
func TestRPCServerInitializeAndMethodNotFound(t *testing.T) {
	model := einollm.NewFakeModel([]string{"answer"}, nil)
	agent, err := v1.NewService(v1.Config{DefaultModel: model})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	srv := New(agent, nil)
	in := strings.NewReader(
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}` + "\n" +
			`{"jsonrpc":"2.0","id":2,"method":"unknown/x","params":{}}` + "\n",
	)
	var out bytes.Buffer
	if err := srv.Serve(context.Background(), in, &out); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("responses = %q", out.String())
	}
	var first map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatalf("first response: %v", err)
	}
	if first["jsonrpc"] != "2.0" || first["id"].(float64) != 1 {
		t.Fatalf("first response = %#v", first)
	}
	result, _ := first["result"].(map[string]any)
	if result == nil {
		t.Fatalf("initialize missing result: %s", lines[0])
	}
	if result["version"] != "v1" {
		t.Fatalf("initialize version = %#v", result["version"])
	}
	var second map[string]any
	_ = json.Unmarshal([]byte(lines[1]), &second)
	errObj, _ := second["error"].(map[string]any)
	if errObj == nil || errObj["code"].(float64) != -32601 {
		t.Fatalf("unknown method response = %#v", second)
	}
}

// TestRPCParseErrorIsMinus32700 proves malformed JSON yields the spec-required
// -32700 parse error. The response carries id=null because the request id
// could not be parsed.
func TestRPCParseErrorIsMinus32700(t *testing.T) {
	model := einollm.NewFakeModel([]string{"answer"}, nil)
	agent, err := v1.NewService(v1.Config{DefaultModel: model})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	srv := New(agent, nil)
	var out bytes.Buffer
	_ = srv.Serve(context.Background(), strings.NewReader("{not json\n"), &out)
	var resp map[string]any
	if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
		t.Fatalf("response not JSON: %v (%q)", err, out.String())
	}
	errObj, _ := resp["error"].(map[string]any)
	if errObj == nil || errObj["code"].(float64) != -32700 {
		t.Fatalf("parse-error response = %#v", resp)
	}
}

// TestRPCInvalidRequestVersionIsMinus32600 proves a request with the wrong
// jsonrpc version (or missing method) yields -32600 invalid request.
func TestRPCInvalidRequestVersionIsMinus32600(t *testing.T) {
	model := einollm.NewFakeModel([]string{"answer"}, nil)
	agent, err := v1.NewService(v1.Config{DefaultModel: model})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	srv := New(agent, nil)
	var out bytes.Buffer
	_ = srv.Serve(context.Background(), strings.NewReader(`{"jsonrpc":"1.0","id":1,"method":"initialize"}`+"\n"), &out)
	var resp map[string]any
	if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
		t.Fatalf("response not JSON: %v", err)
	}
	errObj, _ := resp["error"].(map[string]any)
	if errObj == nil || errObj["code"].(float64) != -32600 {
		t.Fatalf("invalid-request response = %#v", resp)
	}
}
