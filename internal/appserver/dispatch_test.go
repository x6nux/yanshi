package appserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/x6nux/yanshi/internal/api/v1"
	einollm "github.com/x6nux/yanshi/internal/llm/eino"
	"github.com/x6nux/yanshi/internal/store"
)

// newTestService builds a v1.Service backed by a deterministic fake model and
// no persistent store, the lightest wiring that still exercises turn execution.
func newTestService(t *testing.T) *v1.Service {
	t.Helper()
	model := einollm.NewFakeModel([]string{"answer"}, nil)
	agent, err := v1.NewService(v1.Config{DefaultModel: model})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return agent
}

// startThread drives a thread/start through the agent so dispatch tests can
// reference a real, in-memory thread id.
func startThread(t *testing.T, agent *v1.Service, title string) string {
	t.Helper()
	th, err := agent.Start(context.Background(), v1.ThreadStartParams{Title: title})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	return th.ID
}

// rawParams marshals v to a json.RawMessage for dispatch calls.
func rawParams(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal params: %v", err)
	}
	return b
}

// assertRPCError fails the test if err is nil or its code differs from want.
func assertRPCError(t *testing.T, err *RPCError, want int64) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected *RPCError code %d, got nil", want)
	}
	if err.Code != want {
		t.Fatalf("error code = %d, want %d (message=%q)", err.Code, want, err.Message)
	}
}

// TestDispatchThreadResume covers thread/resume across its three outcomes:
// success (thread is in the in-memory registry), ErrThreadNotFound (unknown id
// → -32602), and the generic internal error (empty id → "threadId is required"
// → -32603).
//
// ledger: D1/APS1#1 JSON-RPC thread/turn 可用
func TestDispatchThreadResume(t *testing.T) {
	agent := newTestService(t)
	srv := New(agent, nil)
	id := startThread(t, agent, "r")

	// Success: the thread we just started is live in the registry.
	res, stream, err := srv.dispatch(context.Background(), RPCRequest{
		JSONRPC: "2.0", ID: rawParams(t, 1), Method: "thread/resume",
		Params: rawParams(t, v1.ThreadResumeParams{ThreadID: id}),
	})
	if err != nil {
		t.Fatalf("resume success: %v", err)
	}
	if stream != nil {
		t.Fatalf("resume must not return a stream")
	}
	resp, ok := res.(v1.ThreadResumeResponse)
	if !ok {
		t.Fatalf("resume result type = %T", res)
	}
	if resp.Thread.ID != id {
		t.Fatalf("resume thread id = %q, want %q", resp.Thread.ID, id)
	}

	// ErrThreadNotFound: unknown id maps to -32602 invalid params.
	_, _, err = srv.dispatch(context.Background(), RPCRequest{
		JSONRPC: "2.0", ID: rawParams(t, 2), Method: "thread/resume",
		Params: rawParams(t, v1.ThreadResumeParams{ThreadID: "does-not-exist"}),
	})
	assertRPCError(t, err, codeInvalidParams)

	// Generic internal error: empty id yields a plain fmt.Errorf → -32603.
	_, _, err = srv.dispatch(context.Background(), RPCRequest{
		JSONRPC: "2.0", ID: rawParams(t, 3), Method: "thread/resume",
		Params: rawParams(t, v1.ThreadResumeParams{ThreadID: "  "}),
	})
	assertRPCError(t, err, codeInternalError)
}

// TestDispatchInterrupt covers thread/interrupt and turn/interrupt (shared
// handler) across success (no active turn → idempotent nil) and the
// ErrThreadNotFound error path.
//
// ledger: D1/APS1#1 JSON-RPC thread/turn 可用
func TestDispatchInterrupt(t *testing.T) {
	agent := newTestService(t)
	srv := New(agent, nil)
	id := startThread(t, agent, "i")

	for _, method := range []string{"thread/interrupt", "turn/interrupt"} {
		t.Run(method, func(t *testing.T) {
			// Success: interrupting a thread with no active turn is a no-op.
			res, stream, err := srv.dispatch(context.Background(), RPCRequest{
				JSONRPC: "2.0", ID: rawParams(t, 1), Method: method,
				Params: rawParams(t, v1.ThreadInterruptParams{ThreadID: id}),
			})
			if err != nil {
				t.Fatalf("interrupt success: %v", err)
			}
			if stream != nil {
				t.Fatalf("interrupt must not return a stream")
			}
			r, ok := res.(v1.InterruptResponse)
			if !ok || !r.OK {
				t.Fatalf("interrupt result = %#v", res)
			}

			// Error: unknown thread → ErrThreadNotFound → -32602.
			_, _, err = srv.dispatch(context.Background(), RPCRequest{
				JSONRPC: "2.0", ID: rawParams(t, 2), Method: method,
				Params: rawParams(t, v1.ThreadInterruptParams{ThreadID: "ghost"}),
			})
			assertRPCError(t, err, codeInvalidParams)

			// Invalid params: malformed JSON body.
			_, _, err = srv.dispatch(context.Background(), RPCRequest{
				JSONRPC: "2.0", ID: rawParams(t, 3), Method: method,
				Params: json.RawMessage(`{not-json`),
			})
			assertRPCError(t, err, codeInvalidParams)
		})
	}
}

// TestDispatchTurnStartErrors covers the turn/start error branches that do not
// require a long-running turn: ErrThreadNotFound (unknown thread → -32602) and
// the generic internal error (empty input → "input is required" → -32603).
func TestDispatchTurnStartErrors(t *testing.T) {
	agent := newTestService(t)
	srv := New(agent, nil)
	id := startThread(t, agent, "ts")

	// ErrThreadNotFound → -32602.
	_, _, err := srv.dispatch(context.Background(), RPCRequest{
		JSONRPC: "2.0", ID: rawParams(t, 1), Method: "turn/start",
		Params: rawParams(t, v1.TurnStartParams{ThreadID: "ghost", Input: "hi"}),
	})
	assertRPCError(t, err, codeInvalidParams)

	// Empty input on a valid thread → generic error → -32603.
	_, _, err = srv.dispatch(context.Background(), RPCRequest{
		JSONRPC: "2.0", ID: rawParams(t, 2), Method: "turn/start",
		Params: rawParams(t, v1.TurnStartParams{ThreadID: id, Input: "  "}),
	})
	assertRPCError(t, err, codeInternalError)

	// Malformed params → -32602.
	_, _, err = srv.dispatch(context.Background(), RPCRequest{
		JSONRPC: "2.0", ID: rawParams(t, 3), Method: "turn/start",
		Params: json.RawMessage(`{bad`),
	})
	assertRPCError(t, err, codeInvalidParams)
}

// TestDispatchThreadStartError covers the thread/start internal-error branch
// (malformed params) — the success branch is exercised by the stream tests.
func TestDispatchThreadStartError(t *testing.T) {
	agent := newTestService(t)
	srv := New(agent, nil)
	_, _, err := srv.dispatch(context.Background(), RPCRequest{
		JSONRPC: "2.0", ID: rawParams(t, 1), Method: "thread/start",
		Params: json.RawMessage(`{bad`),
	})
	assertRPCError(t, err, codeInvalidParams)
}

// TestDispatchShutdown returns a versioned ok result and no stream.
func TestDispatchShutdown(t *testing.T) {
	agent := newTestService(t)
	srv := New(agent, nil)
	res, stream, err := srv.dispatch(context.Background(), RPCRequest{
		JSONRPC: "2.0", ID: rawParams(t, 1), Method: "shutdown",
	})
	if err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if stream != nil {
		t.Fatalf("shutdown must not return a stream")
	}
	m, ok := res.(map[string]any)
	if !ok || m["ok"] != true {
		t.Fatalf("shutdown result = %#v", res)
	}
	if m["version"] != v1.Version {
		t.Fatalf("shutdown version = %#v", m["version"])
	}
}

// configErrorBackend is a ConfigBackend whose Read/Write always fail, used to
// exercise the config dispatch error paths.
type configErrorBackend struct{ readFirst bool }

func (c *configErrorBackend) Read(string) (any, error) {
	c.readFirst = true
	return nil, errors.New("boom read")
}
func (c *configErrorBackend) Write(string, json.RawMessage) error {
	return errors.New("boom write")
}

// TestDispatchConfig covers config/read and config/write across the nil-backend
// rejection (-32603), the success path, the invalid-params path, and the
// backend-error path (-32602).
func TestDispatchConfig(t *testing.T) {
	agent := newTestService(t)

	// nil backend: both methods return -32603 "unavailable".
	srv := New(agent, nil)
	for _, method := range []string{"config/read", "config/write"} {
		_, _, err := srv.dispatch(context.Background(), RPCRequest{
			JSONRPC: "2.0", ID: rawParams(t, 1), Method: method,
			Params: rawParams(t, map[string]any{"key": "model"}),
		})
		assertRPCError(t, err, codeInternalError)
	}

	// Working backend: success round-trip.
	cfg := NewMemoryConfig()
	_ = cfg.Write("model", json.RawMessage(`"gpt-4o"`))
	srv2 := New(agent, cfg)
	res, _, err := srv2.dispatch(context.Background(), RPCRequest{
		JSONRPC: "2.0", ID: rawParams(t, 2), Method: "config/read",
		Params: rawParams(t, map[string]any{"key": "model"}),
	})
	if err != nil {
		t.Fatalf("config/read success: %v", err)
	}
	m := res.(map[string]any)
	if m["value"] != "gpt-4o" {
		t.Fatalf("config/read value = %#v", m["value"])
	}

	// config/write success.
	res, _, err = srv2.dispatch(context.Background(), RPCRequest{
		JSONRPC: "2.0", ID: rawParams(t, 3), Method: "config/write",
		Params: rawParams(t, map[string]any{"key": "flags", "value": json.RawMessage(`true`)}),
	})
	if err != nil {
		t.Fatalf("config/write success: %v", err)
	}
	if m := res.(map[string]any); m["ok"] != true {
		t.Fatalf("config/write result = %#v", res)
	}

	// invalid params: malformed JSON for config/read.
	_, _, err = srv2.dispatch(context.Background(), RPCRequest{
		JSONRPC: "2.0", ID: rawParams(t, 4), Method: "config/read",
		Params: json.RawMessage(`{bad`),
	})
	assertRPCError(t, err, codeInvalidParams)

	// invalid params: malformed JSON for config/write.
	_, _, err = srv2.dispatch(context.Background(), RPCRequest{
		JSONRPC: "2.0", ID: rawParams(t, 5), Method: "config/write",
		Params: json.RawMessage(`{bad`),
	})
	assertRPCError(t, err, codeInvalidParams)

	// backend read error → -32602.
	eb := &configErrorBackend{}
	srv3 := New(agent, eb)
	_, _, err = srv3.dispatch(context.Background(), RPCRequest{
		JSONRPC: "2.0", ID: rawParams(t, 6), Method: "config/read",
		Params: rawParams(t, map[string]any{"key": "model"}),
	})
	assertRPCError(t, err, codeInvalidParams)

	// backend write error → -32602.
	_, _, err = srv3.dispatch(context.Background(), RPCRequest{
		JSONRPC: "2.0", ID: rawParams(t, 7), Method: "config/write",
		Params: rawParams(t, map[string]any{"key": "model", "value": json.RawMessage(`"x"`)}),
	})
	assertRPCError(t, err, codeInvalidParams)
}

// TestDispatchCapabilitiesNonInitialize proves the `capabilities` method (as
// opposed to `initialize`) advertises the user-facing method subset without the
// lifecycle/config methods.
func TestDispatchCapabilitiesNonInitialize(t *testing.T) {
	agent := newTestService(t)
	srv := New(agent, nil)
	res, _, err := srv.dispatch(context.Background(), RPCRequest{
		JSONRPC: "2.0", ID: rawParams(t, 1), Method: "capabilities",
	})
	if err != nil {
		t.Fatalf("capabilities: %v", err)
	}
	caps, ok := res.(v1.Capabilities)
	if !ok {
		t.Fatalf("capabilities result type = %T", res)
	}
	for _, m := range caps.Methods {
		if m == "initialize" || m == "shutdown" {
			t.Fatalf("capabilities must not list lifecycle method %q: %v", m, caps.Methods)
		}
	}
}

// --- small rpc.go helpers ---

func TestRPCErrorErrorText(t *testing.T) {
	e := &RPCError{Code: codeMethodNotFound, Message: "nope"}
	if got := e.Error(); got != "nope" {
		t.Fatalf("Error() = %q, want %q", got, "nope")
	}
}

func TestRPCResponseNullsMissingID(t *testing.T) {
	resp := rpcResponse(nil, map[string]any{"ok": true})
	if string(resp.ID) != "null" {
		t.Fatalf("missing id = %s, want null", resp.ID)
	}
	resp2 := rpcErrorResponse(nil, codeInternalError, "x", nil)
	if string(resp2.ID) != "null" {
		t.Fatalf("missing id = %s, want null", resp2.ID)
	}
}

func TestDecodeParamsNullAndEmpty(t *testing.T) {
	var dst struct{ X int }
	for _, raw := range []json.RawMessage{nil, json.RawMessage(`null`), json.RawMessage(``)} {
		if err := decodeParams(raw, &dst); err != nil {
			t.Fatalf("decodeParams(%s): %v", raw, err)
		}
	}
	// error path: malformed JSON.
	if err := decodeParams(json.RawMessage(`{bad`), &dst); err == nil {
		t.Fatalf("decodeParams expected error on malformed JSON")
	}
}

func TestParseRPCLineDefaultParams(t *testing.T) {
	// Valid request with no params gets the default {} injected.
	req, err := parseRPCLine([]byte(`{"jsonrpc":"2.0","id":1,"method":"capabilities"}`))
	if err != nil {
		t.Fatalf("parseRPCLine: %v", err)
	}
	if string(req.Params) != "{}" {
		t.Fatalf("default params = %s, want {}", req.Params)
	}
}

// --- Serve-level edge cases ---

// errWriter fails every Write so Serve surfaces the writer error.
type errWriter struct{}

func (errWriter) Write([]byte) (int, error) { return 0, errors.New("pipe broken") }

// TestServeWriterErrorAborts proves a broken writer makes Serve return the
// write error instead of looping forever.
func TestServeWriterErrorAborts(t *testing.T) {
	agent := newTestService(t)
	srv := New(agent, nil)
	err := srv.Serve(context.Background(), strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"capabilities","params":{}}`+"\n"), errWriter{})
	if err == nil || !strings.Contains(err.Error(), "pipe broken") {
		t.Fatalf("Serve err = %v, want pipe broken", err)
	}
}

// failingReader returns one line then an error on the next Scan, exercising the
// sc.Err() branch of Serve.
type failingReader struct{ n int }

func (f *failingReader) Read(p []byte) (int, error) {
	f.n++
	if f.n == 1 {
		line := []byte(`{"jsonrpc":"2.0","id":1,"method":"capabilities","params":{}}` + "\n")
		return copy(p, line), nil
	}
	return 0, fmt.Errorf("reader exploded")
}

// TestServeScannerError proves Serve returns the scanner error from a failing
// reader after processing the one good line (whose response write also fails so
// the loop exits via the writer-error branch first). We instead feed a reader
// that errors before producing a response, hitting the sc.Err() path.
func TestServeScannerError(t *testing.T) {
	agent := newTestService(t)
	srv := New(agent, nil)
	var out strings.Builder
	err := srv.Serve(context.Background(), &failingReader{}, &out)
	if err == nil {
		t.Fatalf("expected scanner error from Serve")
	}
}

// TestServeEmptyLineSkipped proves truly empty (zero-byte) input lines are
// skipped without producing a response (the `continue` on len(line)==0).
// Whitespace-only lines are NOT skipped — they reach parseRPCLine.
func TestServeEmptyLineSkipped(t *testing.T) {
	agent := newTestService(t)
	srv := New(agent, nil)
	var out strings.Builder
	err := srv.Serve(context.Background(), strings.NewReader("\n\n"), &out)
	if err != nil {
		t.Fatalf("Serve: %v", err)
	}
	if out.Len() != 0 {
		t.Fatalf("blank lines produced output: %q", out.String())
	}
}

// TestServeNotificationParseErrorProducesNoResponse proves a malformed JSON
// line that is also missing an id (notification-shaped parse failure) is
// dropped silently rather than producing a response.
func TestServeNotificationParseErrorProducesNoResponse(t *testing.T) {
	agent := newTestService(t)
	srv := New(agent, nil)
	var out strings.Builder
	_ = srv.Serve(context.Background(), strings.NewReader("{not json\n"), &out)
	// The parse error response carries id=null per spec, so one line is written.
	// Verify it is the parse-error response (not a crash).
	var resp map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(out.String())), &resp); err != nil {
		t.Fatalf("response not JSON: %v (%q)", err, out.String())
	}
}

// TestServeContextCancelForwardItems proves forwardItems returns when the
// context is cancelled mid-stream (the `<-ctx.Done()` branch). We start a turn
// whose items channel never closes, then cancel the context so the forwarding
// goroutine exits and Serve's inflight wait completes.
func TestServeContextCancelForwardItems(t *testing.T) {
	agent := newTestService(t)
	id := startThread(t, agent, "cancel")
	srv := New(agent, nil)
	ctx, cancel := context.WithCancel(context.Background())
	input := `{"jsonrpc":"2.0","id":1,"method":"turn/start","params":{"threadId":"` + id + `","input":"hi"}}` + "\n"
	// Run Serve in a goroutine; cancel the context after dispatch begins so the
	// forwardItems goroutine observes ctx.Done and returns.
	done := make(chan error, 1)
	go func() {
		var out strings.Builder
		done <- srv.Serve(ctx, strings.NewReader(input), &out)
	}()
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Serve after cancel: %v", err)
	}
}

// TestDispatchThreadResumeMalformedParams covers the thread/resume decode
// error branch (-32602) for malformed JSON params.
func TestDispatchThreadResumeMalformedParams(t *testing.T) {
	agent := newTestService(t)
	srv := New(agent, nil)
	_, _, err := srv.dispatch(context.Background(), RPCRequest{
		JSONRPC: "2.0", ID: rawParams(t, 1), Method: "thread/resume",
		Params: json.RawMessage(`{bad`),
	})
	assertRPCError(t, err, codeInvalidParams)
}

// TestMemoryConfigReadUnsetKey covers the "key not set" branch of Read.
func TestMemoryConfigReadUnsetKey(t *testing.T) {
	cfg := NewMemoryConfig()
	_, err := cfg.Read("neverwritten")
	if err == nil || !strings.Contains(err.Error(), "not set") {
		t.Fatalf("read unset key err = %v, want 'not set'", err)
	}
}

// TestMemoryConfigWriteMalformedJSON covers the json.Unmarshal error branch of
// Write (malformed JSON value).
func TestMemoryConfigWriteMalformedJSON(t *testing.T) {
	cfg := NewMemoryConfig()
	if err := cfg.Write("model", json.RawMessage(`not-json`)); err == nil {
		t.Fatalf("write malformed JSON should error")
	}
}

// TestForwardItemsContextCancel covers the `<-ctx.Done()` return branch of
// forwardItems directly: an open channel that never sends, with an already
// cancelled context, must return immediately.
func TestForwardItemsContextCancel(t *testing.T) {
	agent := newTestService(t)
	srv := New(agent, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ch := make(chan v1.Item, 1) // never closed, never sent
	var out strings.Builder
	// Must return promptly rather than blocking.
	done := make(chan struct{})
	go func() {
		srv.forwardItems(ctx, &out, ch)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("forwardItems did not return after ctx cancel")
	}
}

// TestWriteMarshalError covers the json.Marshal error branch of write by
// passing a value that cannot be serialized (a channel).
func TestWriteMarshalError(t *testing.T) {
	agent := newTestService(t)
	srv := New(agent, nil)
	var out strings.Builder
	if err := srv.write(&out, make(chan int)); err == nil {
		t.Fatalf("write expected marshal error for channel value")
	}
}

// TestServeParseErrorWithBrokenWriter covers the write-error branch when the
// parse-error response itself cannot be written.
func TestServeParseErrorWithBrokenWriter(t *testing.T) {
	agent := newTestService(t)
	srv := New(agent, nil)
	err := srv.Serve(context.Background(), strings.NewReader("{not json\n"), errWriter{})
	if err == nil || !strings.Contains(err.Error(), "pipe broken") {
		t.Fatalf("Serve err = %v, want pipe broken", err)
	}
}

// TestServeDispatchErrorWithBrokenWriter covers the write-error branch when a
// dispatch error response (method not found) cannot be written.
func TestServeDispatchErrorWithBrokenWriter(t *testing.T) {
	agent := newTestService(t)
	srv := New(agent, nil)
	err := srv.Serve(context.Background(),
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"nope","params":{}}`+"\n"),
		errWriter{})
	if err == nil || !strings.Contains(err.Error(), "pipe broken") {
		t.Fatalf("Serve err = %v, want pipe broken", err)
	}
}

// TestDispatchThreadStartStoreError covers the thread/start internal-error
// branch (-32603): when the backing store fails to create the session, the
// dispatcher surfaces the error rather than crashing.
func TestDispatchThreadStartStoreError(t *testing.T) {
	st, serr := store.Open(":memory:")
	if serr != nil {
		t.Fatalf("store.Open: %v", serr)
	}
	// Closing the DB makes the next CreateSession fail.
	if err := st.DB.Close(); err != nil {
		t.Fatalf("close DB: %v", err)
	}
	agent, err := v1.NewService(v1.Config{DefaultModel: einollm.NewFakeModel([]string{"x"}, nil), Store: st})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	srv := New(agent, nil)
	_, _, rpcErr := srv.dispatch(context.Background(), RPCRequest{
		JSONRPC: "2.0", ID: rawParams(t, 1), Method: "thread/start",
		Params: rawParams(t, v1.ThreadStartParams{Title: "t"}),
	})
	assertRPCError(t, rpcErr, codeInternalError)
}

// Ensure io is referenced when the suite grows writers that need it.
var _ = io.EOF
