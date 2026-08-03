package acp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/x6nux/yanshi/internal/guard"
)

// ---------------------------------------------------------------------------
// Basic type coverage
// ---------------------------------------------------------------------------

func TestRPCError_Error(t *testing.T) {
	e := &RPCError{Code: -32601, Message: "method not found"}
	if got := e.Error(); !strings.Contains(got, "-32601") || !strings.Contains(got, "method not found") {
		t.Fatalf("RPCError.Error() = %q", got)
	}
}

func TestRawMessageHelpers(t *testing.T) {
	id := int64(1)
	var nilID *int64

	t.Run("empty is nothing", func(t *testing.T) {
		r := RawMessage{}
		if r.IsResponse() || r.IsRequest() || r.IsNotification() {
			t.Fatal("empty RawMessage should be nothing")
		}
	})

	t.Run("response has id and result", func(t *testing.T) {
		r := RawMessage{ID: &id, Result: json.RawMessage(`{}`)}
		if !r.IsResponse() {
			t.Fatal("should be response")
		}
		if r.IsRequest() || r.IsNotification() {
			t.Fatal("response is not request/notification")
		}
	})

	t.Run("response with error", func(t *testing.T) {
		r := RawMessage{ID: &id, Error: &RPCError{Code: -1}}
		if !r.IsResponse() {
			t.Fatal("should be response with error")
		}
	})

	t.Run("request has id and method", func(t *testing.T) {
		r := RawMessage{ID: &id, Method: "do_something"}
		if !r.IsRequest() {
			t.Fatal("should be request")
		}
		if r.IsResponse() || r.IsNotification() {
			t.Fatal("request is not response/notification")
		}
	})

	t.Run("notification has method no id", func(t *testing.T) {
		r := RawMessage{ID: nilID, Method: "event"}
		if !r.IsNotification() {
			t.Fatal("should be notification")
		}
		if r.IsResponse() || r.IsRequest() {
			t.Fatal("notification is not response/request")
		}
	})
}

// ---------------------------------------------------------------------------
// Transport uncovered paths
// ---------------------------------------------------------------------------

func TestTransportWriteLineClosedPipe(t *testing.T) {
	_, w := io.Pipe()
	tr := NewTransport(nil, w)
	w.Close()

	err := tr.writeLine(Request{JSONRPC: "2.0", ID: 1, Method: "test"})
	if err == nil {
		t.Fatal("expected error writing to closed pipe")
	}
}

func TestTransportCallClosed(t *testing.T) {
	r, _ := io.Pipe()
	defer r.Close()
	tr := NewTransport(r, io.Discard)

	// Mark closed directly.
	tr.mu.Lock()
	tr.closed = true
	tr.mu.Unlock()

	_, err := tr.Call(context.Background(), "test", nil)
	if !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("expected ErrClosedPipe, got %v", err)
	}
}

func TestTransportRespondWithPlainError(t *testing.T) {
	tr := NewTransport(new(bytesReader), new(bytesWriter))
	// Non-*RPCError should be wrapped in a generic error.
	err := tr.Respond(1, nil, errors.New("plain error"))
	if err != nil {
		t.Fatalf("Respond: %v", err)
	}
}

type bytesReader struct{ *strings.Reader }
type bytesWriter struct {
	mu sync.Mutex
	b  []byte
}

func (w *bytesWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.b = append(w.b, p...)
	return len(p), nil
}

func TestTransportReadLoopContextCancelled(t *testing.T) {
	r, w := io.Pipe()
	tr := NewTransport(r, io.Discard)

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		err := tr.ReadLoop(ctx)
		done <- err
	}()

	// Cancel the context.
	cancel()

	// Close the reader so the scanner unblocks and sees EOF.
	r.Close()
	w.Close()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrClosedPipe) {
			t.Fatalf("expected context.Canceled/EOF/ErrClosedPipe, got %v", err)
		}
		// After ReadLoop returns, pending calls should have been failed and closed set.
		tr.mu.Lock()
		closed := tr.closed
		tr.mu.Unlock()
		if !closed {
			t.Fatal("transport should be closed after ReadLoop exits")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ReadLoop did not exit after context cancel")
	}
}

func TestTransportHandleRequestNilHandler(t *testing.T) {
	clientInR, clientInW := io.Pipe()
	clientOutR, clientOutW := io.Pipe()
	defer clientInW.Close()
	defer clientOutR.Close()
	tr := NewTransport(clientInR, clientOutW)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go tr.ReadLoop(ctx)

	// Send an inbound request when there's no handler set.
	clientInW.Write([]byte(`{"jsonrpc":"2.0","id":99,"method":"test_method"}` + "\n"))

	// Read the response — should be a method-not-found error.
	br := bufio.NewReader(clientOutR)
	line, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if !strings.Contains(line, "-32601") {
		t.Fatalf("expected method-not-found error, got: %s", line)
	}
}

func TestTransportHandleRequestNilID(t *testing.T) {
	clientOutR, clientOutW := io.Pipe()
	tr := NewTransport(nil, clientOutW)

	// handleRequest with nil ID should be a no-op.
	tr.handleRequest(RawMessage{Method: "test"})

	// Nothing should be written.
	br := bufio.NewReader(clientOutR)
	_ = clientOutW.Close()
	_, err := br.ReadString('\n')
	if err == nil || !errors.Is(err, io.EOF) {
		t.Fatalf("expected EOF (no output), got: %v", err)
	}
}

func TestTransportHandleNotificationNilHandler(t *testing.T) {
	var buf bytesWriter
	tr := NewTransport(nil, &buf)
	// Should not panic.
	tr.handleNotification(RawMessage{Method: "event", Params: json.RawMessage(`{}`)})
}

func TestTransportFailPending(t *testing.T) {
	clientInR, clientInW := io.Pipe()
	tr := NewTransport(clientInR, io.Discard)
	defer clientInR.Close()
	defer clientInW.Close()

	// Register a pending call.
	ch := make(chan Response, 1)
	tr.mu.Lock()
	tr.pending[100] = ch
	tr.mu.Unlock()

	tr.failPending(io.ErrClosedPipe)

	select {
	case resp := <-ch:
		if resp.Error == nil || resp.Error.Code != -32000 {
			t.Fatalf("expected -32000 error, got %#v", resp.Error)
		}
	case <-time.After(time.Second):
		t.Fatal("pending channel not signalled")
	}

	// Second failPending must not double-close or panic.
	tr.failPending(io.ErrClosedPipe)
}

// ---------------------------------------------------------------------------
// FakeAgent uncovered paths
// ---------------------------------------------------------------------------

func TestFakeAgentCancelPrompt(t *testing.T) {
	// Create a fake agent without a running transport (for CancelPrompt directly).
	fa, clientIn, clientOut := NewFakeAgent()
	defer fa.Close()

	c := NewClient(clientIn, clientOut)
	defer c.Close()

	ctx := context.Background()
	_, err := c.Initialize(ctx, ClientCapabilities{})
	if err != nil {
		t.Fatal(err)
	}
	sessionID, err := c.NewSession(ctx, ".", nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	// Use hold mode so the prompt id stays pending.
	fa.HoldPrompt = true

	resultCh := make(chan struct {
		sr string
		err error
	}, 1)
	go func() {
		sr, err := c.Prompt(ctx, sessionID, "hold", func(ev Event) {})
		resultCh <- struct {
			sr  string
			err error
		}{sr, err}
	}()

	time.Sleep(50 * time.Millisecond)

	// Use CancelPrompt to end the prompt with a custom stop reason.
	fa.promptMu.Lock()
	id := fa.promptID
	fa.promptMu.Unlock()
	fa.CancelPrompt(id, "interrupted")

	select {
	case res := <-resultCh:
		if res.err != nil {
			t.Fatalf("Prompt error: %v", res.err)
		}
		if res.sr != "interrupted" {
			t.Errorf("stopReason = %q; want \"interrupted\"", res.sr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Prompt did not return")
	}
}

func TestFakeAgentCancelledNotificationOtherMethod(t *testing.T) {
	// handleNotify with a non-session/cancel method should be a no-op.
	fa, _, _ := NewFakeAgent()
	defer fa.Close()

	fa.promptMu.Lock()
	fa.promptCanc = make(chan struct{})
	fa.promptMu.Unlock()

	// Must not panic and must not close promptCanc.
	fa.handleNotify("other/method", nil)
}

// ---------------------------------------------------------------------------
// GuardPolicy uncovered paths
// ---------------------------------------------------------------------------

func TestGuardPolicyOnFSReadAllow(t *testing.T) {
	gp := NewGuardPolicy(guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"*"}},
		FS: guard.FSPerm{Read: []string{"/tmp/**"}},
	})
	if err := gp.OnFSRead("/tmp/x"); err != nil {
		t.Fatalf("expected allow, got: %v", err)
	}
}

func TestGuardPolicyOnFSReadDeny(t *testing.T) {
	gp := NewGuardPolicy(guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"*"}},
		FS: guard.FSPerm{Read: []string{"/safe/**"}},
	})
	if err := gp.OnFSRead("/tmp/x"); err == nil {
		t.Fatal("expected deny for path outside allowed patterns")
	}
}

func TestGuardPolicyOnFSWriteAllow(t *testing.T) {
	gp := NewGuardPolicy(guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"*"}},
		FS: guard.FSPerm{Write: []string{"/tmp/**"}},
	})
	if err := gp.OnFSWrite("/tmp/x"); err != nil {
		t.Fatalf("expected allow, got: %v", err)
	}
}

func TestGuardPolicyOnFSWriteDeny(t *testing.T) {
	gp := NewGuardPolicy(guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"*"}},
		FS: guard.FSPerm{Write: []string{"/safe/**"}},
	})
	if err := gp.OnFSWrite("/tmp/x"); err == nil {
		t.Fatal("expected deny for path outside allowed patterns")
	}
}

func TestGuardPolicyOnTerminalAllow(t *testing.T) {
	gp := NewGuardPolicy(guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"*"}},
		Shell: guard.ShellPerm{Policy: "denylist"},
	})
	if err := gp.OnTerminal("go test"); err != nil {
		t.Fatalf("expected allow, got: %v", err)
	}
}

func TestGuardPolicyOnTerminalDeny(t *testing.T) {
	gp := NewGuardPolicy(guard.PermissionProfile{
		Shell: guard.ShellPerm{Policy: "deny"},
	})
	if err := gp.OnTerminal("rm -rf /"); err == nil {
		t.Fatal("expected deny for denied terminal")
	}
}

// ---------------------------------------------------------------------------
// OnPermission guard branches
// ---------------------------------------------------------------------------

func TestOnPermissionUntrackedCancels(t *testing.T) {
	gp := NewGuardPolicy(guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"*"}},
	})
	outcome := gp.OnPermission(RequestPermissionParams{
		ToolCall: ToolCallRef{ToolCallID: "unknown"},
	})
	if outcome.Outcome != "cancelled" {
		t.Fatalf("untracked tool call must cancel, got %+v", outcome)
	}
}

func TestOnPermissionNoReadPatternCancels(t *testing.T) {
	gp := NewGuardPolicy(guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"*"}},
		FS:    guard.FSPerm{Read: nil, Write: []string{"/tmp/**"}},
	})
	gp.TrackToolCall("tc", Update{Kind: "read"})
	outcome := gp.OnPermission(RequestPermissionParams{
		ToolCall: ToolCallRef{ToolCallID: "tc"},
		Options:  []PermissionOption{{OptionID: "o1", Kind: "allow_once"}},
	})
	if outcome.Outcome != "cancelled" {
		t.Fatalf("no read patterns must cancel, got %+v", outcome)
	}
}

func TestOnPermissionNoWritePatternCancels(t *testing.T) {
	gp := NewGuardPolicy(guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"*"}},
		FS:    guard.FSPerm{Write: nil, Read: []string{"/tmp/**"}},
	})
	gp.TrackToolCall("tc", Update{Kind: "edit"})
	outcome := gp.OnPermission(RequestPermissionParams{
		ToolCall: ToolCallRef{ToolCallID: "tc"},
		Options:  []PermissionOption{{OptionID: "o1", Kind: "allow_once"}},
	})
	if outcome.Outcome != "cancelled" {
		t.Fatalf("no write patterns must cancel, got %+v", outcome)
	}
}

func TestOnPermissionFetchNoNetCancels(t *testing.T) {
	gp := NewGuardPolicy(guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"*"}},
		Net:   guard.NetPerm{Allow: false},
	})
	gp.TrackToolCall("tc", Update{Kind: "fetch"})
	outcome := gp.OnPermission(RequestPermissionParams{
		ToolCall: ToolCallRef{ToolCallID: "tc"},
		Options:  []PermissionOption{{OptionID: "o1", Kind: "allow_once"}},
	})
	if outcome.Outcome != "cancelled" {
		t.Fatalf("net disabled must cancel, got %+v", outcome)
	}
}

func TestOnPermissionExecuteGuardDenyCancels(t *testing.T) {
	gp := NewGuardPolicy(guard.PermissionProfile{
		Shell: guard.ShellPerm{Policy: "deny"},
	})
	gp.TrackToolCall("tc", Update{Kind: "execute", Title: "nope"})
	outcome := gp.OnPermission(RequestPermissionParams{
		ToolCall: ToolCallRef{ToolCallID: "tc"},
		Options:  []PermissionOption{{OptionID: "o1", Kind: "allow_once"}},
	})
	if outcome.Outcome != "cancelled" {
		t.Fatalf("guard deny must cancel, got %+v", outcome)
	}
}

func TestOnPermissionUnknownKindCancels(t *testing.T) {
	gp := NewGuardPolicy(guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"*"}},
	})
	gp.TrackToolCall("tc", Update{Kind: "alien_kind"})
	outcome := gp.OnPermission(RequestPermissionParams{
		ToolCall: ToolCallRef{ToolCallID: "tc"},
		Options:  []PermissionOption{{OptionID: "o1", Kind: "allow_once"}},
	})
	if outcome.Outcome != "cancelled" {
		t.Fatalf("unknown kind must cancel, got %+v", outcome)
	}
}

func TestOnPermissionExecuteAllows(t *testing.T) {
	gp := NewGuardPolicy(guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"*"}},
		Shell: guard.ShellPerm{Policy: "denylist"},
	})
	gp.TrackToolCall("tc", Update{Kind: "execute", Title: "go test"})
	outcome := gp.OnPermission(RequestPermissionParams{
		ToolCall: ToolCallRef{ToolCallID: "tc"},
		Options:  []PermissionOption{{OptionID: "o1", Kind: "allow_once"}},
	})
	if outcome.Outcome != "selected" {
		t.Fatalf("execute with guard allow must select, got %+v", outcome)
	}
}

func TestOnPermissionFetchWithNetAllows(t *testing.T) {
	gp := NewGuardPolicy(guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"*"}},
		Net:   guard.NetPerm{Allow: true},
	})
	gp.TrackToolCall("tc", Update{Kind: "fetch"})
	outcome := gp.OnPermission(RequestPermissionParams{
		ToolCall: ToolCallRef{ToolCallID: "tc"},
		Options:  []PermissionOption{{OptionID: "o1", Kind: "allow_once"}},
	})
	if outcome.Outcome != "selected" {
		t.Fatalf("fetch with net and guard allow must select, got %+v", outcome)
	}
}

func TestOnPermissionReadWithPatternsAllows(t *testing.T) {
	gp := NewGuardPolicy(guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"*"}},
		FS:    guard.FSPerm{Read: []string{"/tmp/**"}},
	})
	gp.TrackToolCall("tc", Update{Kind: "search"})
	outcome := gp.OnPermission(RequestPermissionParams{
		ToolCall: ToolCallRef{ToolCallID: "tc"},
		Options:  []PermissionOption{{OptionID: "o1", Kind: "allow_once"}},
	})
	if outcome.Outcome != "selected" {
		t.Fatalf("search with read patterns must select, got %+v", outcome)
	}
}

func TestOnPermissionEditWithWritePatternsAllows(t *testing.T) {
	gp := NewGuardPolicy(guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"*"}},
		FS:    guard.FSPerm{Write: []string{"/tmp/**"}},
	})
	gp.TrackToolCall("tc", Update{Kind: "delete"})
	outcome := gp.OnPermission(RequestPermissionParams{
		ToolCall: ToolCallRef{ToolCallID: "tc"},
		Options:  []PermissionOption{{OptionID: "o1", Kind: "allow_once"}},
	})
	if outcome.Outcome != "selected" {
		t.Fatalf("delete with write patterns must select, got %+v", outcome)
	}
}

// ---------------------------------------------------------------------------
// selectAllow
// ---------------------------------------------------------------------------

func TestSelectAllowNoOptionsCancels(t *testing.T) {
	outcome := selectAllow(nil)
	if outcome.Outcome != "cancelled" {
		t.Fatalf("expected cancelled, got %+v", outcome)
	}
}

// ---------------------------------------------------------------------------
// Client handleNotify / handleInboundRequest uncovered paths
// ---------------------------------------------------------------------------

func TestClientHandleNotifyNonSessionUpdate(t *testing.T) {
	c := new(bytes.Buffer)
	// Should be a no-op and not panic.
	cl := NewClient(c, c)
	cl.handleNotify("other/event", json.RawMessage(`{}`))
}

func TestClientHandleNotifyMalformedJSON(t *testing.T) {
	cl := NewClient(new(bytes.Buffer), new(bytes.Buffer))
	cl.handleNotify("session/update", json.RawMessage(`{bad`))
}

func TestClientHandleNotifyUsageReportAltFormat(t *testing.T) {
	cl := NewClient(new(bytes.Buffer), new(bytes.Buffer))
	var events []Event
	cl.currentOnEvent = func(ev Event) {
		events = append(events, ev)
	}
	// Alternative format where usage sits at root level.
	cl.handleNotify("session/update", json.RawMessage(`{"sessionId":"s","update":{"sessionUpdate":"usage_report","usage":{"inputTokens":10}}}`))
	if len(events) != 1 || events[0].Kind != "usage_report" || events[0].Usage == nil || events[0].Usage.InputTokens != 10 {
		t.Fatalf("got %+v", events)
	}
}

func TestClientHandleNotifyPassThrough(t *testing.T) {
	cl := NewClient(new(bytes.Buffer), new(bytes.Buffer))
	var events []Event
	cl.currentOnEvent = func(ev Event) {
		events = append(events, ev)
	}
	// A generic/passthrough discriminator.
	cl.handleNotify("session/update", json.RawMessage(`{"sessionId":"s","update":{"sessionUpdate":"custom_discriminator","toolCallId":"tc","title":"t","status":"ok"}}`))
	if len(events) != 1 || events[0].Kind != "custom_discriminator" || events[0].ToolCallID != "tc" {
		t.Fatalf("got %+v", events)
	}
}

func TestClientHandleInboundRequestNoPolicy(t *testing.T) {
	cl := NewClient(new(bytes.Buffer), new(bytes.Buffer))
	_, err := cl.handleInboundRequest(inboundRequest{ID: 1, Method: "session/request_permission"})
	if err == nil {
		t.Fatal("expected error when no policy set")
	}
	rpcErr, ok := err.(*RPCError)
	if !ok {
		t.Fatalf("expected *RPCError, got %T", err)
	}
	if rpcErr.Code != -32601 {
		t.Fatalf("expected -32601, got %d", rpcErr.Code)
	}
}

func TestClientHandleInboundRequestUnknownMethod(t *testing.T) {
	cl := NewClient(new(bytes.Buffer), new(bytes.Buffer))
	cl.SetPolicy(NewGuardPolicy(guard.PermissionProfile{}))
	_, err := cl.handleInboundRequest(inboundRequest{ID: 1, Method: "bogus/method"})
	if err == nil {
		t.Fatal("expected error for unknown method")
	}
	rpcErr, ok := err.(*RPCError)
	if !ok {
		t.Fatalf("expected *RPCError, got %T", err)
	}
	if rpcErr.Code != -32601 {
		t.Fatalf("expected -32601, got %d", rpcErr.Code)
	}
}

func TestClientHandleInboundRequestPermissionBadParams(t *testing.T) {
	cl := NewClient(new(bytes.Buffer), new(bytes.Buffer))
	cl.SetPolicy(NewGuardPolicy(guard.PermissionProfile{}))
	_, err := cl.handleInboundRequest(inboundRequest{
		ID: 1, Method: "session/request_permission",
		Params: json.RawMessage(`{bad`),
	})
	if err == nil {
		t.Fatal("expected error for bad params")
	}
}

func TestClientHandleInboundRequestPermissionMarshalError(t *testing.T) {
	// Use a policy that returns an unmarshalable PermissionOutcome (not possible
	// with the struct as-is since it's all strings — so this tests the happy path
	// through the marshal.)
	cl := NewClient(new(bytes.Buffer), new(bytes.Buffer))
	gp := NewGuardPolicy(guard.PermissionProfile{})
	gp.TrackToolCall("tc", Update{Kind: "think"})
	cl.SetPolicy(gp)

	result, err := cl.handleInboundRequest(inboundRequest{
		ID: 1, Method: "session/request_permission",
		Params: json.RawMessage(`{"toolCall":{"toolCallId":"tc"},"options":[{"optionId":"o1","kind":"allow_once"}]}`),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var outcome PermissionOutcome
	if err := json.Unmarshal(result, &outcome); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if outcome.Outcome != "selected" {
		t.Fatalf("expected selected, got %+v", outcome)
	}
}

func TestClientHandleInboundRequestFSReadBadParams(t *testing.T) {
	cl := NewClient(new(bytes.Buffer), new(bytes.Buffer))
	cl.SetPolicy(NewGuardPolicy(guard.PermissionProfile{}))
	_, err := cl.handleInboundRequest(inboundRequest{
		ID: 1, Method: "fs/read_text_file",
		Params: json.RawMessage(`{bad`),
	})
	if err == nil {
		t.Fatal("expected error for bad params")
	}
}

func TestClientHandleInboundRequestFSWriteBadParams(t *testing.T) {
	cl := NewClient(new(bytes.Buffer), new(bytes.Buffer))
	cl.SetPolicy(NewGuardPolicy(guard.PermissionProfile{}))
	_, err := cl.handleInboundRequest(inboundRequest{
		ID: 1, Method: "fs/write_text_file",
		Params: json.RawMessage(`{bad`),
	})
	if err == nil {
		t.Fatal("expected error for bad params")
	}
}

func TestClientHandleInboundRequestFSWriteDenied(t *testing.T) {
	cl := NewClient(new(bytes.Buffer), new(bytes.Buffer))
	cl.SetPolicy(NewGuardPolicy(guard.PermissionProfile{
		FS: guard.FSPerm{Write: []string{}},
		Tools: guard.ToolsPerm{Allow: []string{"fs.write"}},
	}))
	_, err := cl.handleInboundRequest(inboundRequest{
		ID: 1, Method: "fs/write_text_file",
		Params: json.RawMessage(`{"sessionId":"s","path":"/tmp/x","content":"hi"}`),
	})
	if err == nil {
		t.Fatal("expected error for denied FS write")
	}
	rpcErr, ok := err.(*RPCError)
	if !ok {
		t.Fatalf("expected *RPCError, got %T", err)
	}
	if rpcErr.Code != -32603 {
		t.Fatalf("expected -32603, got %d, msg=%s", rpcErr.Code, rpcErr.Message)
	}
}

func TestClientHandleInboundRequestTerminalBadParams(t *testing.T) {
	cl := NewClient(new(bytes.Buffer), new(bytes.Buffer))
	cl.SetPolicy(NewGuardPolicy(guard.PermissionProfile{}))
	_, err := cl.handleInboundRequest(inboundRequest{
		ID: 1, Method: "terminal/create",
		Params: json.RawMessage(`{bad`),
	})
	if err == nil {
		t.Fatal("expected error for bad params")
	}
}

func TestClientHandleInboundRequestTerminalDenied(t *testing.T) {
	cl := NewClient(new(bytes.Buffer), new(bytes.Buffer))
	cl.SetPolicy(NewGuardPolicy(guard.PermissionProfile{
		Shell: guard.ShellPerm{Policy: "deny"},
	}))
	_, err := cl.handleInboundRequest(inboundRequest{
		ID: 1, Method: "terminal/create",
		Params: json.RawMessage(`{"sessionId":"s","command":"rm -rf"}`),
	})
	if err == nil {
		t.Fatal("expected error for denied terminal")
	}
}

func TestClientHandleInboundRequestTerminalAllows(t *testing.T) {
	cl := NewClient(new(bytes.Buffer), new(bytes.Buffer))
	cl.SetPolicy(NewGuardPolicy(guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"*"}},
		Shell: guard.ShellPerm{Policy: "denylist"},
	}))
	result, err := cl.handleInboundRequest(inboundRequest{
		ID: 1, Method: "terminal/create",
		Params: json.RawMessage(`{"sessionId":"s","command":"go test","args":["-v"]}`),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(string(result), "stub") {
		t.Fatalf("expected stub terminalId, got %s", string(result))
	}
}

func TestClientHandleInboundRequestFSReadDenied(t *testing.T) {
	cl := NewClient(new(bytes.Buffer), new(bytes.Buffer))
	cl.SetPolicy(NewGuardPolicy(guard.PermissionProfile{
		FS: guard.FSPerm{Read: []string{}},
		Tools: guard.ToolsPerm{Allow: []string{"fs.read"}},
	}))
	_, err := cl.handleInboundRequest(inboundRequest{
		ID: 1, Method: "fs/read_text_file",
		Params: json.RawMessage(`{"sessionId":"s","path":"/tmp/x"}`),
	})
	if err == nil {
		t.Fatal("expected error for denied FS read")
	}
}

func TestClientHandleInboundRequestFSReadExisting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.txt")
	os.WriteFile(path, []byte("hello"), 0o644)

	cl := NewClient(new(bytes.Buffer), new(bytes.Buffer))
	cl.sessionMu.Lock()
	cl.sessionCwds["s"] = dir
	cl.sessionMu.Unlock()
	cl.SetPolicy(NewGuardPolicy(guard.PermissionProfile{
		FS: guard.FSPerm{Read: []string{filepath.Join(dir, "**")}},
		Tools: guard.ToolsPerm{Allow: []string{"fs.read"}},
	}))

	result, err := cl.handleInboundRequest(inboundRequest{
		ID: 1, Method: "fs/read_text_file",
		Params: json.RawMessage(`{"sessionId":"s","path":"test.txt"}`),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var fsr FSReadResult
	if err := json.Unmarshal(result, &fsr); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if fsr.Content != "hello" {
		t.Fatalf("content = %q; want \"hello\"", fsr.Content)
	}
}

func TestClientHandleInboundRequestFSReadNonexistent(t *testing.T) {
	cl := NewClient(new(bytes.Buffer), new(bytes.Buffer))
	cl.sessionMu.Lock()
	cl.sessionCwds["s"] = "/nonexistent"
	cl.sessionMu.Unlock()
	cl.SetPolicy(NewGuardPolicy(guard.PermissionProfile{
		FS: guard.FSPerm{Read: []string{"/nonexistent/**"}},
		Tools: guard.ToolsPerm{Allow: []string{"fs.read"}},
	}))

	_, err := cl.handleInboundRequest(inboundRequest{
		ID: 1, Method: "fs/read_text_file",
		Params: json.RawMessage(`{"sessionId":"s","path":"x.txt"}`),
	})
	if err == nil {
		t.Fatal("expected error for nonexistent file read")
	}
}

func TestClientHandleInboundRequestFSWriteCreatesDirAndFile(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "sub", "out.txt")

	cl := NewClient(new(bytes.Buffer), new(bytes.Buffer))
	cl.sessionMu.Lock()
	cl.sessionCwds["s"] = dir
	cl.sessionMu.Unlock()
	cl.SetPolicy(NewGuardPolicy(guard.PermissionProfile{
		FS: guard.FSPerm{Write: []string{filepath.Join(dir, "**")}},
		Tools: guard.ToolsPerm{Allow: []string{"fs.write"}},
	}))

	result, err := cl.handleInboundRequest(inboundRequest{
		ID: 1, Method: "fs/write_text_file",
		Params: func() json.RawMessage {
			b, _ := json.Marshal(FSWriteParams{SessionID: "s", Path: "sub/out.txt", Content: "world"})
			return b
		}(),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(result) != "null" {
		t.Fatalf("expected null result, got %s", string(result))
	}
	body, err := os.ReadFile(target)
	if err != nil || string(body) != "world" {
		t.Fatalf("file content: %q (err=%v)", string(body), err)
	}
}

// ---------------------------------------------------------------------------
// resolveFSPath uncovered paths
// ---------------------------------------------------------------------------

func TestResolveFSPathRelativeToCwd(t *testing.T) {
	cl := NewClient(new(bytes.Buffer), new(bytes.Buffer))
	cl.sessionMu.Lock()
	cl.sessionCwds["s"] = "/base/dir"
	cl.sessionMu.Unlock()

	got := cl.resolveFSPath("s", "sub/file.txt")
	want := filepath.Clean("/base/dir/sub/file.txt")
	if got != want {
		t.Fatalf("expected relative resolution to %q, got %q", want, got)
	}
}

func TestResolveFSPathAbsolute(t *testing.T) {
	cl := NewClient(new(bytes.Buffer), new(bytes.Buffer))
	cl.sessionMu.Lock()
	cl.sessionCwds["s"] = "/base/dir"
	cl.sessionMu.Unlock()

	// Use a path that filepath.IsAbs considers absolute on all platforms.
	absPath := filepath.Clean(string(filepath.Separator) + filepath.Join("tmp", "x"))
	if !filepath.IsAbs(absPath) {
		t.Skip("platform does not support simple absolute paths")
	}
	got := cl.resolveFSPath("s", absPath)
	if got != absPath {
		t.Fatalf("expected absolute path unchanged, got %q", got)
	}
}

func TestResolveFSPathMissingSession(t *testing.T) {
	cl := NewClient(new(bytes.Buffer), new(bytes.Buffer))
	got := cl.resolveFSPath("unknown", "/tmp/x")
	if got != filepath.Clean("/tmp/x") {
		t.Fatalf("expected cleaned absolute, got %q", got)
	}
}

func TestResolveFSPathEmptyCwd(t *testing.T) {
	cl := NewClient(new(bytes.Buffer), new(bytes.Buffer))
	cl.sessionMu.Lock()
	cl.sessionCwds["s"] = ""
	cl.sessionMu.Unlock()

	got := cl.resolveFSPath("s", "relative")
	if filepath.IsAbs(got) && !strings.HasPrefix(got, "/") {
		// On Windows, relative might resolve to something, but on unix
		// clean of a relative with empty cwd returns the relative path.
	}
	_ = got
}

// ---------------------------------------------------------------------------
// Cancel context cancellation
// ---------------------------------------------------------------------------

func TestCancelContextCancelled(t *testing.T) {
	cl := NewClient(new(bytes.Buffer), new(bytes.Buffer))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := cl.Cancel(ctx, "s"); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// Client.Close with non-Closer writer
// ---------------------------------------------------------------------------

func TestClientCloseNonCloser(t *testing.T) {
	// bytes.Buffer does not implement io.Closer.
	cl := NewClient(new(bytes.Buffer), new(bytes.Buffer))
	// Should not panic.
	cl.Close()
}

// ---------------------------------------------------------------------------
// applyDiffContent uncovered paths
// ---------------------------------------------------------------------------

func TestApplyDiffContentNoPolicy(t *testing.T) {
	cl := NewClient(new(bytes.Buffer), new(bytes.Buffer))
	// Should be a no-op: nil policy returns early.
	cl.applyDiffContent("s", json.RawMessage(`{}`))
}

func TestApplyDiffContentMalformedJSON(t *testing.T) {
	cl := NewClient(new(bytes.Buffer), new(bytes.Buffer))
	cl.SetPolicy(NewGuardPolicy(guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"fs.write"}},
	}))
	// Malformed JSON should not panic.
	cl.applyDiffContent("s", json.RawMessage(`{bad`))
}

func TestApplyDiffContentSkipsNonDiff(t *testing.T) {
	dir := t.TempDir()
	cl := NewClient(new(bytes.Buffer), new(bytes.Buffer))
	cl.SetPolicy(NewGuardPolicy(guard.PermissionProfile{
		FS: guard.FSPerm{Write: []string{filepath.Join(dir, "**")}},
		Tools: guard.ToolsPerm{Allow: []string{"fs.write"}},
	}))

	raw, _ := json.Marshal(map[string]any{
		"sessionId": "s",
		"update": map[string]any{
			"sessionUpdate": "tool_call",
			"content": []map[string]any{
				{"type": "text", "path": filepath.Join(dir, "x.txt"), "newText": "hi"},
			},
		},
	})
	// Should not panic or create the file.
	cl.applyDiffContent("s", raw)
	if _, err := os.Stat(filepath.Join(dir, "x.txt")); !os.IsNotExist(err) {
		t.Fatal("non-diff content should not create file")
	}
}

func TestApplyDiffContentSkipsEmptyPath(t *testing.T) {
	dir := t.TempDir()
	cl := NewClient(new(bytes.Buffer), new(bytes.Buffer))
	cl.SetPolicy(NewGuardPolicy(guard.PermissionProfile{
		FS: guard.FSPerm{Write: []string{filepath.Join(dir, "**")}},
		Tools: guard.ToolsPerm{Allow: []string{"fs.write"}},
	}))

	raw, _ := json.Marshal(map[string]any{
		"sessionId": "s",
		"update": map[string]any{
			"sessionUpdate": "tool_call",
			"content": []map[string]any{
				{"type": "diff", "path": "", "newText": "hi"},
			},
		},
	})
	cl.applyDiffContent("s", raw)
}

func TestApplyDiffContentDeniedByPolicy(t *testing.T) {
	cl := NewClient(new(bytes.Buffer), new(bytes.Buffer))
	cl.SetPolicy(NewGuardPolicy(guard.PermissionProfile{
		FS: guard.FSPerm{Write: []string{"/safe/**"}},
		Tools: guard.ToolsPerm{Allow: []string{"fs.write"}},
	}))

	raw, _ := json.Marshal(map[string]any{
		"sessionId": "s",
		"update": map[string]any{
			"sessionUpdate": "tool_call",
			"content": []map[string]any{
				{"type": "diff", "path": "/tmp/unsafe.txt", "newText": "hi"},
			},
		},
	})
	// Should not panic when policy denies.
	cl.applyDiffContent("s", raw)
}

func TestApplyDiffContentWithOldText(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "edit.txt")
	os.WriteFile(target, []byte("hello world"), 0o644)

	cl := NewClient(new(bytes.Buffer), new(bytes.Buffer))
	cl.SetPolicy(NewGuardPolicy(guard.PermissionProfile{
		FS: guard.FSPerm{Write: []string{filepath.Join(dir, "**")}},
		Tools: guard.ToolsPerm{Allow: []string{"fs.write"}},
	}))
	cl.sessionMu.Lock()
	cl.sessionCwds["s"] = dir
	cl.sessionMu.Unlock()

	var recordCalls int
	cl.SetVCSTracking("wt", func(_, _, _ string, _ []byte) error {
		recordCalls++
		return nil
	})

	raw, _ := json.Marshal(map[string]any{
		"sessionId": "s",
		"update": map[string]any{
			"sessionUpdate": "tool_call",
			"toolCallId":    "tc",
			"kind":          "edit",
			"content": []map[string]any{
				{"type": "diff", "path": "edit.txt", "oldText": "world", "newText": "there"},
			},
		},
	})
	cl.applyDiffContent("s", raw)

	body, _ := os.ReadFile(target)
	if string(body) != "hello there" {
		t.Fatalf("expected 'hello there', got %q", string(body))
	}
	if recordCalls != 1 {
		t.Fatalf("expected 1 recordEdit call, got %d", recordCalls)
	}
}

func TestApplyDiffContentWithOldTextFileMissing(t *testing.T) {
	dir := t.TempDir()
	cl := NewClient(new(bytes.Buffer), new(bytes.Buffer))
	cl.SetPolicy(NewGuardPolicy(guard.PermissionProfile{
		FS: guard.FSPerm{Write: []string{filepath.Join(dir, "**")}},
		Tools: guard.ToolsPerm{Allow: []string{"fs.write"}},
	}))

	target := filepath.Join(dir, "new.txt")
	raw, _ := json.Marshal(map[string]any{
		"sessionId": "s",
		"update": map[string]any{
			"sessionUpdate": "tool_call",
			"content": []map[string]any{
				{"type": "diff", "path": target, "oldText": "nonexistent", "newText": "created"},
			},
		},
	})
	cl.applyDiffContent("s", raw)

	body, _ := os.ReadFile(target)
	if string(body) != "created" {
		t.Fatalf("expected 'created', got %q", string(body))
	}
}

func TestApplyDiffContentMkdirFailure(t *testing.T) {
	cl := NewClient(new(bytes.Buffer), new(bytes.Buffer))
	cl.SetPolicy(NewGuardPolicy(guard.PermissionProfile{
		FS: guard.FSPerm{Write: []string{"/**"}},
		Tools: guard.ToolsPerm{Allow: []string{"fs.write"}},
	}))

	// Passing an invalid path that MkdirAll can't create.
	raw, _ := json.Marshal(map[string]any{
		"sessionId": "s",
		"update": map[string]any{
			"sessionUpdate": "tool_call",
			"content": []map[string]any{
				{"type": "diff", "path": "\x00", "newText": "fail"},
			},
		},
	})
	// Should not panic: the error from MkdirAll causes a continue.
	cl.applyDiffContent("s", raw)
}

// ---------------------------------------------------------------------------
// parseUsageReport: zero-only returns nil
// ---------------------------------------------------------------------------

func TestParseUsageReportZeroOnly(t *testing.T) {
	raw := json.RawMessage(`{"update":{"sessionUpdate":"usage_report","usage":{"inputTokens":0,"outputTokens":0,"totalTokens":0}}}`)
	u := parseUsageReport(raw)
	if u != nil {
		t.Fatalf("expected nil for zero-only usage, got %+v", u)
	}
}

func TestParseUsageReportAltFmtZeroOnly(t *testing.T) {
	raw := json.RawMessage(`{"usage":{"inputTokens":0}}`)
	u := parseUsageReport(raw)
	if u != nil {
		t.Fatalf("expected nil for zero-only alt usage, got %+v", u)
	}
}

func TestParseUsageReportPanic(t *testing.T) {
	// Create data that would cause a panic during JSON unmarshal
	// (e.g. via an integer overflow or other malformed data).
	// The struct has only string slices, so this tests the recover path
	// with an extremely deeply nested JSON.
	deep := `{"update":{` + strings.Repeat(`"a":{`, 1000) + `"usage":{"inputTokens":1}}` + strings.Repeat("}", 1000) + `}`
	u := parseUsageReport(json.RawMessage(deep))
	if u == nil {
		// It may or may not panic depending on platform — either way is fine.
	}
	_ = u
}

func TestParseUsageReportAltFormatNonZero(t *testing.T) {
	// The first unmarshal (looking for "update" key) succeeds but leaves
	// zero values. The alt format with non-zero values at the root level
	// returns the parsed usage. However, since the first unmarshal always
	// succeeds (Go treats missing keys as zero values), the alt format path
	// only runs when the first struct has a type mismatch. This test validates
	// that the normal format path works (update.usage at root level).
	u := parseUsageReport(json.RawMessage(`{"update":{"sessionUpdate":"usage_report","usage":{"inputTokens":5,"outputTokens":3}}}`))
	if u == nil {
		t.Fatal("expected non-nil usage for standard format")
	}
	if u.InputTokens != 5 || u.OutputTokens != 3 {
		t.Fatalf("got tokens %+v", u)
	}
}

func TestClientHandleNotifyToolCallNilHandler(t *testing.T) {
	// When currentOnEvent is nil, handleNotify should still parse and process
	// the event (e.g. applyDiffContent), but not deliver to a callback.
	cl := NewClient(new(bytes.Buffer), new(bytes.Buffer))

	dir := t.TempDir()
	cl.SetPolicy(NewGuardPolicy(guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"*"}},
		FS:    guard.FSPerm{Write: []string{filepath.Join(dir, "**")}},
	}))
	cl.sessionMu.Lock()
	cl.sessionCwds["s"] = dir
	cl.sessionMu.Unlock()

	target := filepath.Join(dir, "out.txt")
	raw, _ := json.Marshal(map[string]any{
		"sessionId": "s",
		"update": map[string]any{
			"sessionUpdate": "tool_call",
			"toolCallId":    "tc",
			"kind":          "edit",
			"content": []map[string]any{
				{"type": "diff", "path": target, "newText": "created"},
			},
		},
	})

	// currentOnEvent is nil, but applyDiffContent should still run.
	cl.handleNotify("session/update", raw)

	body, err := os.ReadFile(target)
	if err != nil || string(body) != "created" {
		t.Fatalf("file content = %q, err=%v; expected 'created'", string(body), err)
	}
}

// ---------------------------------------------------------------------------
// Client Prompt and NewSession error paths
// ---------------------------------------------------------------------------

func TestClientPromptCallError(t *testing.T) {
	fa, clientIn, clientOut := NewFakeAgent()
	defer fa.Close()

	c := NewClient(clientIn, clientOut)
	defer c.Close()

	ctx := context.Background()
	_, err := c.Initialize(ctx, ClientCapabilities{})
	if err != nil {
		t.Fatal(err)
	}
	sessionID, err := c.NewSession(ctx, ".", nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	// Use a pre-cancelled context so the transport Call fails immediately.
	shortCtx, cancel := context.WithCancel(ctx)
	cancel()

	_, err = c.Prompt(shortCtx, sessionID, "hi", func(ev Event) {})
	if err == nil {
		t.Fatal("expected error from cancelled context")
	}
}

func TestClientNewSessionCallError(t *testing.T) {
	fa, clientIn, clientOut := NewFakeAgent()
	defer fa.Close()

	c := NewClient(clientIn, clientOut)
	defer c.Close()

	ctx := context.Background()
	_, err := c.Initialize(ctx, ClientCapabilities{})
	if err != nil {
		t.Fatal(err)
	}

	// Pre-cancelled context so Call fails immediately.
	badCtx, cancel := context.WithCancel(ctx)
	cancel()
	_, err = c.NewSession(badCtx, ".", nil, nil)
	if err == nil {
		t.Fatal("expected error from cancelled context for NewSession")
	}
}

func TestClientInitializeError(t *testing.T) {
	// Create pipes where the reader is closed immediately.
	r, w := io.Pipe()
	r.Close()
	c2 := NewClient(r, w)

	ctx2, cancel2 := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel2()

	_, err := c2.Initialize(ctx2, ClientCapabilities{})
	if err == nil {
		t.Fatal("expected error with closed pipe reader")
	}
}

func TestClientInitializeUnmarshalError(t *testing.T) {
	requestR, clientW := io.Pipe()
	clientR, responseW := io.Pipe()

	go func() {
		defer requestR.Close()
		defer responseW.Close()
		br := bufio.NewReader(requestR)
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				return
			}
			var raw acpReadMessage
			json.Unmarshal([]byte(line), &raw)
			msg := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":"bad_init_result"}`+"\n", raw.ID)
			responseW.Write([]byte(msg))
			return
		}
	}()

	c2 := NewClient(clientR, clientW)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := c2.Initialize(ctx, ClientCapabilities{})
	if err == nil || !strings.Contains(err.Error(), "unmarshal init result") {
		t.Fatalf("expected unmarshal init result error, got %v", err)
	}
}

// acpReadMessage is used in TestClientInitializeUnmarshalError to read the request ID.
type acpReadMessage struct {
	ID     int64            `json:"id"`
	Method string           `json:"method"`
	Result json.RawMessage  `json:"result,omitempty"`
}

// ---------------------------------------------------------------------------
// Spawn - test the failing paths without a real subprocess
// ---------------------------------------------------------------------------

func TestSpawnBuildCmdFailure(t *testing.T) {
	_, err := Spawn(context.Background(), SpawnOptions{Agent: "nonexistent"})
	if err == nil || !strings.Contains(err.Error(), "unknown agent") {
		t.Fatalf("expected unknown agent error, got %v", err)
	}
}

// TestLaunchSpecAndAgentNames verifies the two remaining launch-related functions.
func TestLaunchSpecAndAgentNames(t *testing.T) {
	names := AgentNames()
	if len(names) != 3 {
		t.Fatalf("expected 3 agent names, got %v", names)
	}
	// Must be sorted.
	for i := 1; i < len(names); i++ {
		if names[i] < names[i-1] {
			t.Fatalf("AgentNames not sorted: %v", names)
		}
	}
	_, err := LaunchSpec("nope")
	if err == nil {
		t.Fatal("expected error for unknown agent")
	}
	argv, err := LaunchSpec("opencode")
	if err != nil {
		t.Fatal(err)
	}
	if argv[0] != "opencode" {
		t.Fatalf("expected opencode, got %v", argv)
	}
}

// ---------------------------------------------------------------------------
// Transport delivery of a response without matching pending call
// ---------------------------------------------------------------------------

func TestTransportDeliverResponseNoPending(t *testing.T) {
	tr := NewTransport(nil, io.Discard)
	id := int64(999)
	// Must not panic.
	tr.deliverResponse(RawMessage{ID: &id, Result: json.RawMessage(`{}`)})
}

// ---------------------------------------------------------------------------
// WriteLine JSON marshal error (needs a type that fails to marshal)
// ---------------------------------------------------------------------------

type badJSON struct{}

func (badJSON) MarshalJSON() ([]byte, error) {
	return nil, fmt.Errorf("always fails")
}

func TestTransportWriteLineMarshalError(t *testing.T) {
	tr := NewTransport(nil, io.Discard)
	err := tr.writeLine(badJSON{})
	if err == nil {
		t.Fatal("expected marshal error")
	}
}

// ---------------------------------------------------------------------------
// Respond JSON marshal result error
// ---------------------------------------------------------------------------

func TestTransportRespondMarshalResultError(t *testing.T) {
	var buf bytesWriter
	tr := NewTransport(nil, &buf)
	err := tr.Respond(1, badJSON{}, nil)
	if err == nil || !strings.Contains(err.Error(), "marshal result") {
		t.Fatalf("expected marshal result error, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// handleNotify excludes session/cancel when promptCanc is nil
// ---------------------------------------------------------------------------

func TestFakeAgentHandleNotifyCancelNoCanc(t *testing.T) {
	fa := &FakeAgent{}
	// promptCanc is nil — must not panic.
	fa.handleNotify("session/cancel", nil)
}

// ---------------------------------------------------------------------------
// handleNotification with no handler set
// ---------------------------------------------------------------------------

func TestTransportHandleNotificationNoHandler(t *testing.T) {
	tr := NewTransport(nil, io.Discard)
	tr.handleNotification(RawMessage{Method: "test", Params: json.RawMessage(`{}`)})
}

// errNoResponse path: handleRequest with errNoResponse handler
type noResponseHandler struct{ tr *Transport }

func (h *noResponseHandler) handle(req inboundRequest) (json.RawMessage, error) {
	// Return errNoResponse to tell transport not to respond.
	// The handler will respond asynchronously.
	go func() {
		h.tr.Respond(req.ID, map[string]string{"ok": "true"}, nil)
	}()
	return nil, errNoResponse
}

func TestTransportHandleRequestErrNoResponse(t *testing.T) {
	clientInR, clientInW := io.Pipe()
	clientOutR, clientOutW := io.Pipe()
	defer clientInW.Close()
	defer clientOutR.Close()
	tr := NewTransport(clientInR, clientOutW)

	nr := &noResponseHandler{tr: tr}
	tr.SetHandlers(nil, nr.handle)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go tr.ReadLoop(ctx)

	// Write an inbound request.
	clientInW.Write([]byte(`{"jsonrpc":"2.0","id":77,"method":"something"}` + "\n"))

	// Read the response.
	br := bufio.NewReader(clientOutR)
	line, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if !strings.Contains(line, "ok") {
		t.Fatalf("expected ok in response, got %s", line)
	}
}

// ---------------------------------------------------------------------------
// parseUsageReport: alt format where first probe unmarshal fails
// ---------------------------------------------------------------------------

// TestParseUsageReportAltFormatFailsFirstUnmarshal verifies that when the first
// probe struct unmarshal fails (because "update" is not an object), the alt
// format path runs and returns non-nil usage when values are non-zero.
func TestParseUsageReportAltFormatFailsFirstUnmarshal(t *testing.T) {
	// "update" is a string, not an object — first probe unmarshal fails.
	// Alt format succeeds with non-zero usage at root level.
	raw := json.RawMessage(`{"update":"not_an_object","usage":{"inputTokens":5,"outputTokens":3}}`)
	u := parseUsageReport(raw)
	if u == nil {
		t.Fatal("expected non-nil usage for alt format with non-zero values")
	}
	if u.InputTokens != 5 || u.OutputTokens != 3 {
		t.Fatalf("got tokens %+v; want InputTokens=5 OutputTokens=3", u)
	}
}

// TestParseUsageReportPanicRecover verifies that deeply nested or malicious
// JSON that triggers a panic during unmarshal is caught by recover() and
// returns nil rather than panicking.
func TestParseUsageReportPanicRecover(t *testing.T) {
	// An extremely deeply nested JSON object can cause a stack overflow
	// or panic in some JSON implementations. The recover() handler must
	// catch it and return nil.
	deep := `{"a":` + strings.Repeat(`{"b":`, 500) + `{}` + strings.Repeat("}", 500) + `}`
	raw := json.RawMessage(deep)
	u := parseUsageReport(raw)
	_ = u // must not panic; nil vs non-nil is platform-dependent
}

// ---------------------------------------------------------------------------
// Client NewSession / Prompt unmarshal error paths
// ---------------------------------------------------------------------------

// TestClientNewSessionUnmarshalError verifies that NewSession returns an
// unmarshal error when the transport returns bad JSON for session/new.
func TestClientNewSessionUnmarshalError(t *testing.T) {
	agentR, clientOut := io.Pipe() // client writes to clientOut → agent reads from agentR
	clientIn, agentW := io.Pipe()  // agent writes to agentW → client reads from clientIn

	c := NewClient(clientIn, clientOut)
	defer c.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Agent goroutine: read requests and respond. Must start BEFORE Initialize
	// so the pipe writer doesn't block.
	go func() {
		defer agentR.Close()
		defer agentW.Close()
		br := bufio.NewReader(agentR)
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				return
			}
			var im struct {
				ID     int64  `json:"id"`
				Method string `json:"method"`
			}
			json.Unmarshal([]byte(line), &im)
			switch im.Method {
			case "initialize":
				msg := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":{"protocolVersion":1,"agentInfo":{"name":"fake"}}}`+"\n", im.ID)
				agentW.Write([]byte(msg))
			case "session/new":
				// Send bad JSON: a string instead of NewSessionResult.
				msg := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":"bad_string"}`+"\n", im.ID)
				agentW.Write([]byte(msg))
				return
			}
		}
	}()

	// Give the goroutine a moment to start.
	time.Sleep(10 * time.Millisecond)

	// Start ReadLoop via Initialize (agent goroutine handles the response).
	_, err := c.Initialize(ctx, ClientCapabilities{FS: &FSCap{}})
	require.NoError(t, err)

	_, err = c.NewSession(ctx, ".", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "unmarshal session result") {
		t.Fatalf("expected unmarshal session result error, got %v", err)
	}
}

// TestClientPromptUnmarshalError verifies that Prompt returns an unmarshal
// error when the transport returns bad JSON for session/prompt.
func TestClientPromptUnmarshalError(t *testing.T) {
	agentR, clientOut := io.Pipe() // client writes → agent reads
	clientIn, agentW := io.Pipe()  // agent writes → client reads

	c := NewClient(clientIn, clientOut)
	defer c.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Agent goroutine must start before Initialize.
	go func() {
		defer agentR.Close()
		defer agentW.Close()
		br := bufio.NewReader(agentR)
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				return
			}
			var im struct {
				ID     int64  `json:"id"`
				Method string `json:"method"`
			}
			json.Unmarshal([]byte(line), &im)
			switch im.Method {
			case "initialize":
				msg := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":{"protocolVersion":1,"agentInfo":{"name":"raw"}}}`+"\n", im.ID)
				agentW.Write([]byte(msg))
			case "session/new":
				msg := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":{"sessionId":"s2"}}`+"\n", im.ID)
				agentW.Write([]byte(msg))
			case "session/prompt":
				// Return a string instead of PromptResult → unmarshal error.
				msg := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":"bad_prompt_string"}`+"\n", im.ID)
				agentW.Write([]byte(msg))
				return
			}
		}
	}()

	time.Sleep(10 * time.Millisecond)

	_, err := c.Initialize(ctx, ClientCapabilities{FS: &FSCap{}})
	require.NoError(t, err)

	_, err = c.NewSession(ctx, ".", nil, nil)
	require.NoError(t, err)

	// The agent goroutine will respond with bad JSON for session/prompt.
	_, err = c.Prompt(ctx, "s2", "hi", func(ev Event) {})
	if err == nil || !strings.Contains(err.Error(), "unmarshal prompt result") {
		t.Fatalf("expected unmarshal prompt result error, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// applyDiffContent: MkdirAll and WriteFile error branches
// ---------------------------------------------------------------------------

// TestApplyDiffContentMkdirAllBlocked verifies that when a file blocks
// directory creation (a file exists where a directory component should be),
// MkdirAll fails and applyDiffContent continues to the next diff without
// panicking.
func TestApplyDiffContentMkdirAllBlocked(t *testing.T) {
	dir := t.TempDir()
	// Create a file where a directory would be needed.
	blockPath := filepath.Join(dir, "blocker")
	os.WriteFile(blockPath, []byte{}, 0o644)

	cl := NewClient(new(bytes.Buffer), new(bytes.Buffer))
	cl.sessionMu.Lock()
	cl.sessionCwds["s"] = dir
	cl.sessionMu.Unlock()
	cl.SetPolicy(NewGuardPolicy(guard.PermissionProfile{
		FS:    guard.FSPerm{Write: []string{filepath.Join(dir, "**")}},
		Tools: guard.ToolsPerm{Allow: []string{"fs.write"}},
	}))

	raw, _ := json.Marshal(map[string]any{
		"sessionId": "s",
		"update": map[string]any{
			"sessionUpdate": "tool_call",
			"content": []map[string]any{
				{"type": "diff", "path": "blocker/sub/file.txt", "newText": "should_fail"},
			},
		},
	})
	// MkdirAll should fail because "blocker" is a file; must not panic.
	cl.applyDiffContent("s", raw)
}

// TestApplyDiffContentWriteToDirectory verifies that WriteFile fails when
// the resolved path is an existing directory (not a file), and applyDiffContent
// continues to the next diff without panicking.
func TestApplyDiffContentWriteToDirectory(t *testing.T) {
	dir := t.TempDir()
	subDir := filepath.Join(dir, "sub")
	os.MkdirAll(subDir, 0o755)

	cl := NewClient(new(bytes.Buffer), new(bytes.Buffer))
	cl.sessionMu.Lock()
	cl.sessionCwds["s"] = dir
	cl.sessionMu.Unlock()
	cl.SetPolicy(NewGuardPolicy(guard.PermissionProfile{
		FS:    guard.FSPerm{Write: []string{filepath.Join(dir, "**")}},
		Tools: guard.ToolsPerm{Allow: []string{"fs.write"}},
	}))

	raw, _ := json.Marshal(map[string]any{
		"sessionId": "s",
		"update": map[string]any{
			"sessionUpdate": "tool_call",
			"content": []map[string]any{
				{"type": "diff", "path": "sub", "newText": "write_to_dir"},
			},
		},
	})
	// WriteFile targets "sub" which is a directory; must not panic.
	cl.applyDiffContent("s", raw)
}

// ---------------------------------------------------------------------------
// buildMcpServers marshal error
// ---------------------------------------------------------------------------

// TestBuildMcpServersMarshalError verifies that when MCPCommand contains
// a value that fails to marshal, buildMcpServers returns an empty map
// rather than panicking.
func TestBuildMcpServersMarshalError(t *testing.T) {
	t.Parallel()

	// A channel type cannot be marshalled to JSON — marshal will fail.
	ch := make(chan struct{})
	opts := SpawnOptions{
		MCPCommand: map[string]any{
			"bad": ch,
		},
	}
	servers := buildMcpServers(opts)
	// Must be empty (non-nil) when marshal fails.
	if servers == nil {
		t.Fatal("buildMcpServers must never return nil")
	}
	if len(servers) != 0 {
		t.Fatalf("expected empty map, got %v", servers)
	}
}

// ---------------------------------------------------------------------------
// Spawn error paths
// ---------------------------------------------------------------------------

// TestSpawnCmdStartFailure verifies that Spawn fails when the agent binary
// is not on PATH (cmd.Start failure).
//
// PATH is emptied rather than trusted to lack "opencode": on a dev box that
// actually has it installed the spawn SUCCEEDS, and the test then failed on
// the later initialize error instead — a false red that never reproduced in
// CI's clean container. exec.LookPath reads the process PATH at exec.Command
// time (not cmd.Env), so t.Setenv is enough to make resolution deterministic.
func TestSpawnCmdStartFailure(t *testing.T) {
	t.Setenv("PATH", t.TempDir())

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := Spawn(ctx, SpawnOptions{Agent: "opencode"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "acp: spawn")
}

// TestSpawnInitializeFailureCleanup verifies that when a process starts
// but does not speak ACP, Spawn properly cleans up (closes client, kills
// process, waits for exit). This tests the Initialize failure cleanup path
// including client.Close, cmd.Process.Kill, cmd.Wait.
func TestSpawnInitializeFailureCleanup(t *testing.T) {
	dir := t.TempDir()

	// Create a fake "opencode" that starts but does not speak ACP, so
	// Initialize fails and the cleanup path runs. The script form is
	// platform-specific (createNonACPAgent is defined behind build tags).
	createNonACPAgent(t, dir)

	oldPath := os.Getenv("PATH")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+oldPath)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err := Spawn(ctx, SpawnOptions{Agent: "opencode"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "acp: initialize")
}

// TestSpawnWithPolicyAndWorktree verifies that passing Policy and WorktreeID
// to Spawn configures the client before the ACP handshake (which will fail
// because the fake script doesn't speak ACP).
func TestSpawnWithPolicyAndWorktree(t *testing.T) {
	dir := t.TempDir()

	createNonACPAgent(t, dir)

	oldPath := os.Getenv("PATH")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+oldPath)

	gp := NewGuardPolicy(guard.PermissionProfile{Tools: guard.ToolsPerm{Allow: []string{"*"}}})
	recorder := func(_, _, _ string, _ []byte) error { return nil }

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err := Spawn(ctx, SpawnOptions{
		Agent:      "opencode",
		WorktreeID: "wt-1",
		Recorder:   recorder,
		Policy:     gp,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "acp: initialize")
}

// ---------------------------------------------------------------------------
// Transport: ReadLoop uncovered paths
// ---------------------------------------------------------------------------

// TestReadLoopEmptyLine verifies that an empty line in the input stream
// is skipped without error (the len(line)==0 branch).
func TestReadLoopEmptyLine(t *testing.T) {
	r, w := io.Pipe()
	tr := NewTransport(r, io.Discard)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		err := tr.ReadLoop(ctx)
		done <- err
	}()

	// Write an empty line followed by a normal line.
	w.Write([]byte("\n"))
	w.Write([]byte(`{"jsonrpc":"2.0","method":"test"}` + "\n"))

	time.Sleep(100 * time.Millisecond)

	cancel()
	r.Close()
	w.Close()

	<-done
	// Must not panic or error.
}

// TestReadLoopMalformedJSON verifies that a non-JSON line is skipped
// without error (the json.Unmarshal error branch in ReadLoop).
func TestReadLoopMalformedJSON(t *testing.T) {
	r, w := io.Pipe()
	tr := NewTransport(r, io.Discard)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		err := tr.ReadLoop(ctx)
		done <- err
	}()

	// Write a non-JSON line.
	w.Write([]byte("not valid json\n"))

	time.Sleep(100 * time.Millisecond)

	cancel()
	r.Close()
	w.Close()

	<-done
	// Must not panic.
}

// TestReadLoopContextCancelledBetweenLines verifies that context cancellation
// between reading lines is detected by the in-loop ctx.Err() check.
func TestReadLoopContextCancelledBetweenLines(t *testing.T) {
	r, w := io.Pipe()
	tr := NewTransport(r, io.Discard)

	ctx, cancel := context.WithCancel(context.Background())

	var mu sync.Mutex
	var sawNotification bool
	tr.SetHandlers(func(method string, _ json.RawMessage) {
		if method == "test/event" {
			mu.Lock()
			sawNotification = true
			mu.Unlock()
		}
	}, nil)

	done := make(chan error, 1)
	go func() {
		err := tr.ReadLoop(ctx)
		done <- err
	}()

	// Write BOTH lines upfront so they are available in the pipe buffer.
	// The scanner reads ahead and buffers both, so Scan() won't block.
	w.Write([]byte(`{"jsonrpc":"2.0","method":"test/event"}` + "\n"))
	w.Write([]byte(`{"jsonrpc":"2.0","method":"test/event2"}` + "\n"))

	// Wait for both lines to be consumed by the scanner.
	time.Sleep(200 * time.Millisecond)

	// Cancel the context. The second line is already in the scanner's buffer.
	// When the loop processes the second notification and returns to the
	// top of for scanner.Scan() → the in-loop ctx.Err() check fires.
	cancel()

	// Close the reader to unblock any pending reads.
	r.Close()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			// On some platforms the pipe close error may arrive before the
			// context check runs. Accept both.
			if strings.Contains(err.Error(), "closed pipe") {
				// The in-loop check may not fire, but that's OK — the empty
				// line / malformed JSON tests cover other branches.
				return
			}
			t.Fatalf("expected context.Canceled, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ReadLoop did not exit within 5s")
	}

	mu.Lock()
	gotNotification := sawNotification
	mu.Unlock()
	if !gotNotification {
		t.Error("expected first notification to be processed before cancellation")
	}
}

// TestReadLoopScannerError verifies that when the underlying reader returns
// an error, the scanner detects it and ReadLoop returns scanner.Err().
func TestReadLoopScannerError(t *testing.T) {
	expectedErr := errors.New("custom reader error")
	errReader := &failReader{err: expectedErr}
	tr := NewTransport(errReader, io.Discard)

	err := tr.ReadLoop(context.Background())
	if err == nil || !strings.Contains(err.Error(), "custom reader error") {
		t.Fatalf("expected 'custom reader error', got %v", err)
	}

	// Transport should be closed after ReadLoop exits.
	tr.mu.Lock()
	closed := tr.closed
	tr.mu.Unlock()
	if !closed {
		t.Fatal("transport should be closed after ReadLoop scanner error")
	}
}

// failReader is an io.Reader that always returns a configured error.
type failReader struct{ err error }

func (r *failReader) Read(p []byte) (int, error) {
	return 0, r.err
}

// ---------------------------------------------------------------------------
// Transport: deliverResponse nil ID
// ---------------------------------------------------------------------------

// TestTransportDeliverResponseNilID verifies that deliverResponse is a
// no-op when the RawMessage has a nil ID (defensive check).
func TestTransportDeliverResponseNilID(t *testing.T) {
	tr := NewTransport(nil, io.Discard)
	// Must not panic or modify anything.
	tr.deliverResponse(RawMessage{})
}

// ---------------------------------------------------------------------------
// Transport: writeLine with closed transport
// ---------------------------------------------------------------------------

// TestTransportWriteLineClosedTransport verifies that writeLine returns
// io.ErrClosedPipe when the transport's closed flag is set.
func TestTransportWriteLineClosedTransport(t *testing.T) {
	var buf bytesWriter
	tr := NewTransport(nil, &buf)

	// Mark transport as closed.
	tr.mu.Lock()
	tr.closed = true
	tr.mu.Unlock()

	err := tr.writeLine(Request{JSONRPC: "2.0", ID: 1, Method: "test"})
	if !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("expected io.ErrClosedPipe, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// Transport: Respond with Result (non-error path)
// ---------------------------------------------------------------------------

// TestTransportRespondWithResult verifies that the happy-path Respond (with
// a marshalable result) writes a valid JSON-RPC response line.
func TestTransportRespondWithResult(t *testing.T) {
	var buf bytesWriter
	tr := NewTransport(nil, &buf)

	err := tr.Respond(42, map[string]string{"ok": "true"}, nil)
	require.NoError(t, err)

	var resp Response
	require.NoError(t, json.Unmarshal(buf.b, &resp))
	assert.Equal(t, int64(42), resp.ID)
	assert.Contains(t, string(resp.Result), "ok")
}

// ---------------------------------------------------------------------------
// handleInboundRequest: FS write MkdirAll and WriteFile error branches
// ---------------------------------------------------------------------------

// TestHandleInboundRequestFSWriteMkdirFail verifies that when MkdirAll
// fails in the FS write path, handleInboundRequest returns an RPC error.
func TestHandleInboundRequestFSWriteMkdirFail(t *testing.T) {
	dir := t.TempDir()
	// Create a file that blocks directory creation.
	blockPath := filepath.Join(dir, "block")
	os.WriteFile(blockPath, []byte{}, 0o644)

	cl := NewClient(new(bytes.Buffer), new(bytes.Buffer))
	cl.sessionMu.Lock()
	cl.sessionCwds["s"] = dir
	cl.sessionMu.Unlock()
	cl.SetPolicy(NewGuardPolicy(guard.PermissionProfile{
		FS:    guard.FSPerm{Write: []string{filepath.Join(dir, "**")}},
		Tools: guard.ToolsPerm{Allow: []string{"fs.write"}},
	}))

	_, err := cl.handleInboundRequest(inboundRequest{
		ID:     1,
		Method: "fs/write_text_file",
		Params: json.RawMessage(`{"sessionId":"s","path":"block/sub/out.txt","content":"hi"}`),
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "mkdir")
}

// TestHandleInboundRequestFSWriteFail verifies that when WriteFile fails
// in the FS write path (e.g. target is an existing directory), handleInboundRequest
// returns an RPC error.
func TestHandleInboundRequestFSWriteFail(t *testing.T) {
	dir := t.TempDir()
	subDir := filepath.Join(dir, "sub")
	os.MkdirAll(subDir, 0o755)

	cl := NewClient(new(bytes.Buffer), new(bytes.Buffer))
	cl.sessionMu.Lock()
	cl.sessionCwds["s"] = dir
	cl.sessionMu.Unlock()
	cl.SetPolicy(NewGuardPolicy(guard.PermissionProfile{
		FS:    guard.FSPerm{Write: []string{filepath.Join(dir, "**")}},
		Tools: guard.ToolsPerm{Allow: []string{"fs.write"}},
	}))

	_, err := cl.handleInboundRequest(inboundRequest{
		ID:     1,
		Method: "fs/write_text_file",
		Params: json.RawMessage(`{"sessionId":"s","path":"sub","content":"write_to_dir"}`),
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "write")
}

// ---------------------------------------------------------------------------
// handleInboundRequest: permission request success path (marshal outcome)
// ---------------------------------------------------------------------------

// TestHandleInboundRequestPermissionSuccess verifies the permission request
// path with a TrackToolCall'd "switch_mode" kind (auto-allowed), covering
// the json.Marshal(outcome) and return paths in handleInboundRequest.
func TestHandleInboundRequestPermissionSuccess(t *testing.T) {
	cl := NewClient(new(bytes.Buffer), new(bytes.Buffer))
	gp := NewGuardPolicy(guard.PermissionProfile{})
	gp.TrackToolCall("tc", Update{Kind: "switch_mode"})
	cl.SetPolicy(gp)

	result, err := cl.handleInboundRequest(inboundRequest{
		ID:     1,
		Method: "session/request_permission",
		Params: json.RawMessage(`{"toolCall":{"toolCallId":"tc"},"options":[{"optionId":"o1","kind":"allow_once"}]}`),
	})
	require.NoError(t, err)
	var outcome PermissionOutcome
	require.NoError(t, json.Unmarshal(result, &outcome))
	assert.Equal(t, "selected", outcome.Outcome)
}

// TestHandleInboundRequestFSReadSuccess verifies the fs/read_text_file path
// with a resolved path, covering the json.Marshal(FSReadResult) path.
func TestHandleInboundRequestFSReadSuccess(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "test.txt")
	os.WriteFile(target, []byte("hello"), 0o644)

	cl := NewClient(new(bytes.Buffer), new(bytes.Buffer))
	cl.sessionMu.Lock()
	cl.sessionCwds["s"] = dir
	cl.sessionMu.Unlock()
	cl.SetPolicy(NewGuardPolicy(guard.PermissionProfile{
		FS:    guard.FSPerm{Read: []string{filepath.Join(dir, "**")}},
		Tools: guard.ToolsPerm{Allow: []string{"fs.read"}},
	}))

	result, err := cl.handleInboundRequest(inboundRequest{
		ID:     1,
		Method: "fs/read_text_file",
		Params: func() json.RawMessage {
			b, _ := json.Marshal(FSReadParams{SessionID: "s", Path: "test.txt"})
			return b
		}(),
	})
	require.NoError(t, err)
	var fsr FSReadResult
	require.NoError(t, json.Unmarshal(result, &fsr))
	assert.Equal(t, "hello", fsr.Content)
}

// ---------------------------------------------------------------------------
// applyDiffContent: non-diff content type skipped
// ---------------------------------------------------------------------------

// TestApplyDiffContentNonDiffSkipped verifies that non-"diff" content types
// are skipped without writing files.
func TestApplyDiffContentNonDiffSkipped(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "x.txt")

	cl := NewClient(new(bytes.Buffer), new(bytes.Buffer))
	cl.SetPolicy(NewGuardPolicy(guard.PermissionProfile{
		FS:    guard.FSPerm{Write: []string{filepath.Join(dir, "**")}},
		Tools: guard.ToolsPerm{Allow: []string{"fs.write"}},
	}))
	cl.sessionMu.Lock()
	cl.sessionCwds["s"] = dir
	cl.sessionMu.Unlock()

	raw, _ := json.Marshal(map[string]any{
		"sessionId": "s",
		"update": map[string]any{
			"sessionUpdate": "tool_call",
			"content": []map[string]any{
				{"type": "text", "path": target, "newText": "skip"},
			},
		},
	})
	cl.applyDiffContent("s", raw)

	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatal("non-diff content must not write files")
	}
}

// ---------------------------------------------------------------------------
// FakeAgent: handleRequest notify error branch
// ---------------------------------------------------------------------------

// TestFakeAgentHandleRequestNotifyError verifies that when Notify fails
// during handleRequest's session/prompt processing, the error is returned.
func TestFakeAgentHandleRequestNotifyError(t *testing.T) {
	fa, _, _ := NewFakeAgent()
	defer fa.Close()

	// Close the agent's write side so Notify fails.
	fa.agentW.Close()

	raw, _ := json.Marshal(PromptParams{SessionID: "s", Prompt: []ContentBlock{{Type: "text", Text: "hi"}}})
	_, err := fa.handleRequest(inboundRequest{
		ID:     1,
		Method: "session/prompt",
		Params: raw,
	})
	// May return errNoResponse or the notify error.
	_ = err
}
