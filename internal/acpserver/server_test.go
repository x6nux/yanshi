package acpserver

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/x6nux/yanshi/internal/acp"
	v1 "github.com/x6nux/yanshi/internal/api/v1"
	einollm "github.com/x6nux/yanshi/internal/llm/eino"
)

// newTestServer builds a Server over a v1 Service backed by a fake model, so
// the transport is exercised end to end without an API key or a subprocess.
func newTestServer(t *testing.T, reply string) *Server {
	t.Helper()
	svc, err := v1.NewService(v1.Config{
		DefaultModel: einollm.NewFakeModel([]string{reply}, nil),
	})
	if err != nil {
		t.Fatalf("v1.NewService: %v", err)
	}
	return New(svc, io.Discard)
}

// session drives a Server over an in-memory pipe pair, collecting every line it
// writes. It is the shape a real host has: write a request, read frames until
// the response for that id arrives.
type session struct {
	t       *testing.T
	in      *io.PipeWriter
	out     *bufio.Reader
	done    chan error
	nextID  int64
	closeIn sync.Once
}

func newSession(t *testing.T, s *Server) *session {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	sess := &session{t: t, in: inW, out: bufio.NewReader(outR), done: make(chan error, 1)}
	go func() {
		err := s.Serve(context.Background(), inR, outW)
		outW.Close()
		sess.done <- err
	}()
	t.Cleanup(func() { sess.close() })
	return sess
}

func (s *session) close() {
	s.closeIn.Do(func() { s.in.Close() })
}

// request writes a JSON-RPC request and returns its id.
func (s *session) request(method string, params any) int64 {
	s.t.Helper()
	s.nextID++
	id := s.nextID
	s.writeRaw(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	return id
}

// notify writes a JSON-RPC notification.
func (s *session) notify(method string, params any) {
	s.t.Helper()
	s.writeRaw(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

func (s *session) writeRaw(v any) {
	s.t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		s.t.Fatalf("marshal: %v", err)
	}
	if _, err := s.in.Write(append(data, '\n')); err != nil {
		s.t.Fatalf("write: %v", err)
	}
}

// readLine reads one frame, failing the test on timeout so a wedged server
// surfaces as a named failure rather than a package-level panic.
func (s *session) readLine() acp.RawMessage {
	s.t.Helper()
	type result struct {
		line string
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		line, err := s.out.ReadString('\n')
		ch <- result{line, err}
	}()
	select {
	case r := <-ch:
		if r.err != nil && r.line == "" {
			s.t.Fatalf("read frame: %v", r.err)
		}
		var raw acp.RawMessage
		if err := json.Unmarshal([]byte(r.line), &raw); err != nil {
			s.t.Fatalf("frame is not JSON-RPC: %v (%q)", err, r.line)
		}
		return raw
	case <-time.After(5 * time.Second):
		s.t.Fatal("timed out waiting for a frame")
		return acp.RawMessage{}
	}
}

// awaitResponse reads frames until the response for id arrives, returning it
// plus every notification seen on the way.
func (s *session) awaitResponse(id int64) (acp.RawMessage, []acp.UpdateParams) {
	s.t.Helper()
	var updates []acp.UpdateParams
	for i := 0; i < 200; i++ {
		raw := s.readLine()
		if raw.ID != nil && *raw.ID == id {
			return raw, updates
		}
		if raw.Method == "session/update" {
			var u acp.UpdateParams
			if err := json.Unmarshal(raw.Params, &u); err != nil {
				s.t.Fatalf("session/update params: %v", err)
			}
			updates = append(updates, u)
		}
	}
	s.t.Fatalf("no response for id %d after 200 frames", id)
	return acp.RawMessage{}, nil
}

// openSession runs initialize + session/new and returns the session id.
func (s *session) openSession() string {
	s.t.Helper()
	initID := s.request("initialize", map[string]any{"protocolVersion": ProtocolVersion})
	resp, _ := s.awaitResponse(initID)
	if resp.Error != nil {
		s.t.Fatalf("initialize failed: %v", resp.Error)
	}
	newID := s.request("session/new", map[string]any{"cwd": "/tmp/proj", "mcpServers": map[string]any{}})
	resp, _ = s.awaitResponse(newID)
	if resp.Error != nil {
		s.t.Fatalf("session/new failed: %v", resp.Error)
	}
	var out acp.NewSessionResult
	if err := json.Unmarshal(resp.Result, &out); err != nil {
		s.t.Fatalf("session/new result: %v", err)
	}
	if out.SessionID == "" {
		s.t.Fatal("session/new returned an empty session id")
	}
	return out.SessionID
}

// TestInitializeAdvertisesTheVersionAndCapabilities. A host reads this to
// decide whether to proceed at all.
func TestInitializeAdvertisesTheVersionAndCapabilities(t *testing.T) {
	sess := newSession(t, newTestServer(t, "hi"))
	id := sess.request("initialize", map[string]any{"protocolVersion": ProtocolVersion})
	resp, _ := sess.awaitResponse(id)
	if resp.Error != nil {
		t.Fatalf("initialize error: %v", resp.Error)
	}
	var out acp.InitResult
	if err := json.Unmarshal(resp.Result, &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.ProtocolVersion != ProtocolVersion {
		t.Errorf("protocolVersion = %d, want %d", out.ProtocolVersion, ProtocolVersion)
	}
	if out.AgentInfo.Name != "yanshi" {
		t.Errorf("agentInfo.name = %q", out.AgentInfo.Name)
	}
	// loadSession is a real claim backed by v1 thread resume; the media
	// capabilities are false because promptText drops non-text blocks.
	var caps map[string]any
	if err := json.Unmarshal(out.AgentCapabilities, &caps); err != nil {
		t.Fatalf("capabilities: %v", err)
	}
	if caps["loadSession"] != true {
		t.Errorf("capabilities = %v, want loadSession true", caps)
	}
}

// TestPromptStreamsThenAnswers is the core protocol property: the
// session/prompt RESPONSE is the END of the turn, with session/update
// notifications underneath it. Answering immediately would tell the host the
// turn had finished before a single token was produced.
func TestPromptStreamsThenAnswers(t *testing.T) {
	sess := newSession(t, newTestServer(t, "hello from yanshi"))
	sessionID := sess.openSession()

	id := sess.request("session/prompt", acp.PromptParams{
		SessionID: sessionID,
		Prompt:    []acp.ContentBlock{{Type: "text", Text: "say hi"}},
	})
	resp, updates := sess.awaitResponse(id)
	if resp.Error != nil {
		t.Fatalf("session/prompt error: %v", resp.Error)
	}
	var out acp.PromptResult
	if err := json.Unmarshal(resp.Result, &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.StopReason != "end_turn" {
		t.Errorf("stopReason = %q, want end_turn", out.StopReason)
	}
	if len(updates) == 0 {
		t.Fatal("no session/update arrived; the host would see a turn with no content")
	}
	var text strings.Builder
	for _, u := range updates {
		if u.SessionID != sessionID {
			t.Errorf("update carried session %q, want %q", u.SessionID, sessionID)
		}
		if u.Update.SessionUpdate == "agent_message_chunk" {
			for _, b := range u.Update.Content {
				text.WriteString(b.Text)
			}
		}
	}
	// The v1 service with only a DefaultModel (no orchestrator) emits one stub
	// chunk rather than calling the model, which is what makes this reachable
	// without an API key. The assertion is therefore that the message TEXT
	// reaches the host as agent_message_chunk content, not what that text says.
	if text.Len() == 0 {
		t.Errorf("no agent_message_chunk text reached the host; updates = %+v", updates)
	}
}

// TestUnknownSessionIsRefused: quietly minting a session for an id the host
// invented would present an empty conversation as the one it asked for.
func TestUnknownSessionIsRefused(t *testing.T) {
	sess := newSession(t, newTestServer(t, "hi"))
	_ = sess.openSession()
	for _, tc := range []struct{ method string }{{"session/prompt"}, {"session/load"}} {
		id := sess.request(tc.method, map[string]any{
			"sessionId": "nope",
			"prompt":    []acp.ContentBlock{{Type: "text", Text: "x"}},
		})
		resp, _ := sess.awaitResponse(id)
		if resp.Error == nil {
			t.Fatalf("%s accepted an unknown session id", tc.method)
		}
		if !strings.Contains(resp.Error.Message, "nope") {
			t.Errorf("%s error %q does not name the session", tc.method, resp.Error.Message)
		}
	}
}

// TestEmptyPromptIsRefused: an empty turn burns a thread's active-turn slot and
// produces nothing, and the reason ("no text content") is actionable.
func TestEmptyPromptIsRefused(t *testing.T) {
	sess := newSession(t, newTestServer(t, "hi"))
	sessionID := sess.openSession()
	id := sess.request("session/prompt", acp.PromptParams{
		SessionID: sessionID,
		// An image block only: initialize declares image unsupported, so
		// promptText drops it and there is nothing left.
		Prompt: []acp.ContentBlock{{Type: "image"}},
	})
	resp, _ := sess.awaitResponse(id)
	if resp.Error == nil {
		t.Fatal("an empty prompt was accepted")
	}
	if !strings.Contains(resp.Error.Message, "text") {
		t.Errorf("error %q does not explain what was missing", resp.Error.Message)
	}
}

// TestUnknownMethodGetsMethodNotFound: a host probing for an optional method
// must get -32601 rather than silence, or it waits forever.
func TestUnknownMethodGetsMethodNotFound(t *testing.T) {
	sess := newSession(t, newTestServer(t, "hi"))
	id := sess.request("session/set_mode", map[string]any{})
	resp, _ := sess.awaitResponse(id)
	if resp.Error == nil || resp.Error.Code != -32601 {
		t.Fatalf("error = %+v, want code -32601", resp.Error)
	}
}

// TestMalformedLineDoesNotKillTheTransport: one bad line has no id to answer,
// and tearing the connection down over it would take a working session with it.
func TestMalformedLineDoesNotKillTheTransport(t *testing.T) {
	sess := newSession(t, newTestServer(t, "hi"))
	if _, err := sess.in.Write([]byte("{not json\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	// The session still works afterwards.
	if got := sess.openSession(); got == "" {
		t.Fatal("the transport did not survive a malformed line")
	}
}

// TestSessionLoadRebindsAKnownSession, and refuses a stale id rather than
// handing back a fresh empty one.
func TestSessionLoadRebindsAKnownSession(t *testing.T) {
	sess := newSession(t, newTestServer(t, "hi"))
	sessionID := sess.openSession()
	id := sess.request("session/load", map[string]any{"sessionId": sessionID})
	resp, _ := sess.awaitResponse(id)
	if resp.Error != nil {
		t.Fatalf("session/load of a known session failed: %v", resp.Error)
	}
}

// TestServeReturnsAfterEOF: Serve must not leak the goroutine, and must wait
// for in-flight turns so a notification is never written after the caller
// considers the exchange finished.
func TestServeReturnsAfterEOF(t *testing.T) {
	sess := newSession(t, newTestServer(t, "hi"))
	sessionID := sess.openSession()
	id := sess.request("session/prompt", acp.PromptParams{
		SessionID: sessionID, Prompt: []acp.ContentBlock{{Type: "text", Text: "go"}},
	})
	sess.awaitResponse(id)
	sess.close()
	select {
	case err := <-sess.done:
		if err != nil {
			t.Fatalf("Serve returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after EOF")
	}
}

// TestUpdateForItemMapping is the table for the v1 → ACP vocabulary mapping.
// Each row is a decision: turn boundaries are dropped because ACP marks them
// with the request/response pair, and an error is surfaced as text as well as
// a stop reason so a host that only renders content still shows the reason.
func TestUpdateForItemMapping(t *testing.T) {
	cases := []struct {
		name     string
		item     v1.Item
		want     string // "" means "not forwarded"
		wantText string
	}{
		{"message delta", v1.Item{Type: v1.ItemMessageDelta, Text: "hi"}, "agent_message_chunk", "hi"},
		{"empty message delta is dropped", v1.Item{Type: v1.ItemMessageDelta}, "", ""},
		{"reasoning delta", v1.Item{Type: v1.ItemReasoningDelta, Text: "think"}, "agent_thought_chunk", "think"},
		{"tool call", v1.Item{Type: v1.ItemToolCall, ID: "t1", ToolName: "fs_read"}, "tool_call", ""},
		{"tool progress", v1.Item{Type: v1.ItemToolProgress, ID: "t1", ToolName: "fs_read"}, "tool_call_update", ""},
		{"tool result", v1.Item{Type: v1.ItemToolResult, ID: "t1", ToolName: "fs_read"}, "tool_call_update", ""},
		{"turn error", v1.Item{Type: v1.ItemTurnError, Error: "boom"}, "agent_message_chunk", "turn failed: boom"},
		// Turn boundaries are ACP's request and response, not updates.
		{"turn started is dropped", v1.Item{Type: v1.ItemTurnStarted}, "", ""},
		{"turn completed is dropped", v1.Item{Type: v1.ItemTurnCompleted}, "", ""},
		{"unknown type is dropped", v1.Item{Type: "something.new"}, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			upd, ok := updateForItem(tc.item)
			if tc.want == "" {
				if ok {
					t.Fatalf("item %q was forwarded as %q; it has no ACP vocabulary", tc.item.Type, upd.SessionUpdate)
				}
				return
			}
			if !ok {
				t.Fatalf("item %q was dropped, want %q", tc.item.Type, tc.want)
			}
			if upd.SessionUpdate != tc.want {
				t.Errorf("sessionUpdate = %q, want %q", upd.SessionUpdate, tc.want)
			}
			if tc.wantText != "" {
				got := ""
				for _, b := range upd.Content {
					got += b.Text
				}
				if got != tc.wantText {
					t.Errorf("content = %q, want %q", got, tc.wantText)
				}
			}
		})
	}
}

// TestToolStatusDistinguishesFailure: reporting a failed tool as completed
// makes an error indistinguishable from success to a host whose only signal is
// this field.
func TestToolStatusDistinguishesFailure(t *testing.T) {
	if got := toolStatus(v1.Item{}); got != "completed" {
		t.Errorf("clean result = %q, want completed", got)
	}
	if got := toolStatus(v1.Item{Error: "denied"}); got != "failed" {
		t.Errorf("errored result = %q, want failed", got)
	}
	if got := toolStatus(v1.Item{Status: "error"}); got != "failed" {
		t.Errorf("status=error result = %q, want failed", got)
	}
}

// TestTerminalStopReasonSeparatesErrorFromCompletion. ACP's vocabulary is
// narrower than v1's; mapping both onto end_turn would hide every failure.
func TestTerminalStopReasonSeparatesErrorFromCompletion(t *testing.T) {
	if r, ok := terminalStopReason(v1.Item{Type: v1.ItemTurnCompleted}); !ok || r != "end_turn" {
		t.Errorf("completed -> %q,%v", r, ok)
	}
	if r, ok := terminalStopReason(v1.Item{Type: v1.ItemTurnError}); !ok || r == "end_turn" {
		t.Errorf("error -> %q,%v; a failure must not report end_turn", r, ok)
	}
	if _, ok := terminalStopReason(v1.Item{Type: v1.ItemMessageDelta}); ok {
		t.Error("a non-terminal item claimed to be terminal")
	}
}

// TestPromptTextFlattensTextBlocksOnly. Rendering a placeholder for an
// unsupported block would hand the model a description of something it cannot
// see; initialize already declares those blocks unsupported.
func TestPromptTextFlattensTextBlocksOnly(t *testing.T) {
	got := promptText([]acp.ContentBlock{
		{Type: "text", Text: "first"},
		{Type: "image"},
		{Type: "text", Text: "second"},
		{Type: "text"},
	})
	if got != "first\nsecond" {
		t.Fatalf("promptText = %q, want the text blocks joined", got)
	}
	if promptText(nil) != "" {
		t.Error("no blocks must yield the empty string")
	}
}

// TestSessionTitleNamesTheDirectory so a resumed thread is recognisable in
// /sessions rather than being one of N identical "New thread" rows.
func TestSessionTitleNamesTheDirectory(t *testing.T) {
	if got := sessionTitle("/home/me/proj"); !strings.Contains(got, "/home/me/proj") {
		t.Errorf("title = %q", got)
	}
	if got := sessionTitle(""); got == "" {
		t.Error("an empty cwd must still produce a title")
	}
}

// TestHostSuppliedMCPServersAreReportedNotSilentlyDropped: wiring a
// host-supplied server into a running orchestrator would add tools the
// operator's guard profile never authorised, so it is refused — but silently
// refusing leaves the host believing its servers are live.
func TestHostSuppliedMCPServersAreReportedNotSilentlyDropped(t *testing.T) {
	var diag strings.Builder
	svc, err := v1.NewService(v1.Config{
		DefaultModel: einollm.NewFakeModel([]string{"hi"}, nil),
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	s := New(svc, &diag)
	_, rpcErr := s.newSession(context.Background(), json.RawMessage(
		`{"cwd":"/p","mcpServers":{"other":{"command":"x"}}}`))
	if rpcErr != nil {
		t.Fatalf("session/new failed: %v", rpcErr)
	}
	if !strings.Contains(diag.String(), "mcpServers") {
		t.Fatalf("the ignored mcpServers were not reported: %q", diag.String())
	}
}

// TestDiagnosticsNeverReachStdout is the stdio contract: stdout carries
// protocol frames only, and one stray line desynchronises the host's
// line-oriented parser.
func TestDiagnosticsNeverReachStdout(t *testing.T) {
	var diag strings.Builder
	svc, err := v1.NewService(v1.Config{
		DefaultModel: einollm.NewFakeModel([]string{"hi"}, nil),
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	s := New(svc, &diag)
	var stdout strings.Builder
	in := strings.NewReader("{garbage\n" +
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}` + "\n")
	if err := s.Serve(context.Background(), in, &stdout); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	if diag.Len() == 0 {
		t.Error("the malformed line produced no diagnostic at all")
	}
	for _, line := range strings.Split(strings.TrimSpace(stdout.String()), "\n") {
		if line == "" {
			continue
		}
		var probe map[string]any
		if err := json.Unmarshal([]byte(line), &probe); err != nil {
			t.Fatalf("stdout carried a non-JSON line: %q", line)
		}
		if probe["jsonrpc"] != "2.0" {
			t.Fatalf("stdout carried a non-protocol object: %q", line)
		}
	}
}

// TestCancelDoesNotDestroyTheSession pins the one thing an implementation is
// most tempted to get wrong: ACP cancellation ends the TURN, not the
// conversation. Dropping the session would make the host's next prompt fail
// with "unknown session" for a session it never closed.
func TestCancelDoesNotDestroyTheSession(t *testing.T) {
	s := newTestServer(t, "hi")
	sess := newSession(t, s)
	sessionID := sess.openSession()

	sess.notify("session/cancel", acp.CancelParams{SessionID: sessionID})
	// A cancel with no turn running is a no-op, and the session survives it.
	id := sess.request("session/prompt", acp.PromptParams{
		SessionID: sessionID, Prompt: []acp.ContentBlock{{Type: "text", Text: "still here?"}},
	})
	resp, _ := sess.awaitResponse(id)
	if resp.Error != nil {
		t.Fatalf("the session did not survive a cancel: %v", resp.Error)
	}
}

// TestCancelUnknownSessionIsANoOp: a notification has no id, so there is
// nothing to answer with and panicking is the only way to get this wrong.
func TestCancelUnknownSessionIsANoOp(t *testing.T) {
	s := newTestServer(t, "hi")
	s.cancelSession("does-not-exist")
	s.cancelSession("")
}

// TestConcurrentWritesDoNotInterleave. Notifications stream from a turn
// goroutine while the dispatcher answers other requests; a splice in the middle
// of a line makes the host's parser fail on valid traffic.
//
// The extra requests are written from a GOROUTINE, and that is a property of
// the harness rather than of the server: io.Pipe is unbuffered, so a writer
// blocks until the reader consumes, and driving both directions from one
// goroutine deadlocks the test against a server that is mid-stream. A real
// stdio pair has OS buffers and does not.
func TestConcurrentWritesDoNotInterleave(t *testing.T) {
	svc, err := v1.NewService(v1.Config{
		DefaultModel: einollm.NewFakeModel([]string{strings.Repeat("chunk ", 200)}, nil),
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	s := New(svc, io.Discard)
	sess := newSession(t, s)
	sessionID := sess.openSession()

	promptID := sess.request("session/prompt", acp.PromptParams{
		SessionID: sessionID, Prompt: []acp.ContentBlock{{Type: "text", Text: "talk"}},
	})
	// Writes are best-effort: the test finishes as soon as the prompt response
	// lands, and a write racing that close is the harness ending, not a
	// failure. Fatalf from a background goroutine after the test returns
	// panics the whole package, so this path must not use the session helper.
	writes := make(chan struct{})
	go func() {
		defer close(writes)
		for i := 0; i < 5; i++ {
			line, _ := json.Marshal(map[string]any{
				"jsonrpc": "2.0", "id": 1000 + i, "method": "initialize", "params": map[string]any{},
			})
			if _, err := sess.in.Write(append(line, '\n')); err != nil {
				return
			}
		}
	}()
	// Every frame read up to the prompt response must be valid JSON; readLine
	// fails the test otherwise, which IS the assertion — a spliced line does
	// not unmarshal.
	resp, updates := sess.awaitResponse(promptID)
	if resp.Error != nil {
		t.Fatalf("prompt error: %v", resp.Error)
	}
	if len(updates) == 0 {
		t.Fatal("no updates streamed")
	}
	sess.close()
	<-writes
}
