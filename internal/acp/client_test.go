package acp

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/x6nux/yanshi/internal/guard"
)

// TestClientInitializeAndNewSession verifies that a Client connected to a
// FakeAgent can complete the initialize and session/new handshake.
func TestClientInitializeAndNewSession(t *testing.T) {
	fa, clientIn, clientOut := NewFakeAgent()
	defer fa.Close()

	c := NewClient(clientIn, clientOut)
	defer c.Close()

	ctx := context.Background()

	// Initialize — should return agentInfo.Name == "fake".
	info, err := c.Initialize(ctx, ClientCapabilities{})
	if err != nil {
		t.Fatalf("Initialize: unexpected error: %v", err)
	}
	if info.Name != "fake" {
		t.Errorf("agentInfo.Name = %q; want \"fake\"", info.Name)
	}
	if !c.initialized {
		t.Error("client not marked initialized after Initialize")
	}

	// NewSession — should return "sess_fake".
	sessionID, err := c.NewSession(ctx, ".", nil, nil)
	if err != nil {
		t.Fatalf("NewSession: unexpected error: %v", err)
	}
	if sessionID != "sess_fake" {
		t.Errorf("sessionID = %q; want \"sess_fake\"", sessionID)
	}
}

// TestClientInitializeMultipleCalls verifies that calling Initialize more than
// once does not start multiple ReadLoop goroutines.
func TestClientInitializeMultipleCalls(t *testing.T) {
	fa, clientIn, clientOut := NewFakeAgent()
	defer fa.Close()

	c := NewClient(clientIn, clientOut)
	defer c.Close()

	ctx := context.Background()

	// First Initialize starts the ReadLoop.
	_, err := c.Initialize(ctx, ClientCapabilities{})
	if err != nil {
		t.Fatalf("first Initialize: %v", err)
	}

	firstDone := c.readLoopDone

	// Second Initialize must not create a new ReadLoop.
	_, err = c.Initialize(ctx, ClientCapabilities{})
	if err != nil {
		t.Fatalf("second Initialize: %v", err)
	}

	if c.readLoopDone != firstDone {
		t.Error("Initialize started a second ReadLoop")
	}
}

// TestFakeAgentUnknownMethod verifies that the FakeAgent returns a JSON-RPC
// method-not-found error for unrecognized methods.
func TestFakeAgentUnknownMethod(t *testing.T) {
	fa, clientIn, clientOut := NewFakeAgent()
	defer fa.Close()

	c := NewClient(clientIn, clientOut)
	defer c.Close()

	ctx := context.Background()

	// Must Initialize first to start the client ReadLoop.
	_, err := c.Initialize(ctx, ClientCapabilities{})
	if err != nil {
		t.Fatalf("Initialize: %v", err)
	}

	// Unknown method should return an RPC error.
	_, err = c.tr.Call(ctx, "bogus/method", nil)
	if err == nil {
		t.Fatal("Call with unknown method: expected error, got nil")
	}
	rpcErr, ok := err.(*RPCError)
	if !ok {
		t.Fatalf("expected *RPCError, got %T: %v", err, err)
	}
	if rpcErr.Code != -32601 {
		t.Errorf("error code = %d; want -32601", rpcErr.Code)
	}
}

// TestClientPrompt verifies that Client.Prompt streams session/update events
// to the onEvent callback and returns the agent's stopReason.
func TestClientPrompt(t *testing.T) {
	fa, clientIn, clientOut := NewFakeAgent()
	defer fa.Close()

	// Script the FakeAgent to send two text chunks.
	fa.Updates = []string{"hello ", "world"}

	c := NewClient(clientIn, clientOut)
	defer c.Close()

	ctx := context.Background()

	// Initialize + NewSession.
	_, err := c.Initialize(ctx, ClientCapabilities{})
	if err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	sessionID, err := c.NewSession(ctx, ".", nil, nil)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}

	// Collect events from Prompt.
	var events []Event
	stopReason, err := c.Prompt(ctx, sessionID, "hi", func(ev Event) {
		events = append(events, ev)
	})
	if err != nil {
		t.Fatalf("Prompt: unexpected error: %v", err)
	}

	// Verify stopReason.
	if stopReason != "end_turn" {
		t.Errorf("stopReason = %q; want \"end_turn\"", stopReason)
	}

	// Verify we received both chunks in order.
	if len(events) != 2 {
		t.Fatalf("expected 2 events; got %d", len(events))
	}
	if events[0].Kind != "agent_message_chunk" || events[0].Text != "hello " {
		t.Errorf("events[0] = %+v; want Kind=\"agent_message_chunk\" Text=\"hello \"", events[0])
	}
	if events[1].Kind != "agent_message_chunk" || events[1].Text != "world" {
		t.Errorf("events[1] = %+v; want Kind=\"agent_message_chunk\" Text=\"world\"", events[1])
	}
}

// TestPolicyFSWriteAllow verifies that when the GuardPolicy allows the path,
// the FakeAgent receives a success response for an inbound fs/write_text_file.
func TestPolicyFSWriteAllow(t *testing.T) {
	fa, clientIn, clientOut := NewFakeAgent()
	defer fa.Close()

	fa.Updates = nil // no message chunks needed
	fa.InboundRequests = []InboundSpec{
		{Method: "fs/write_text_file", Params: FSWriteParams{Path: "/tmp/ok", Content: "hi"}},
	}

	c := NewClient(clientIn, clientOut)
	defer c.Close()

	c.SetPolicy(NewGuardPolicy(guard.PermissionProfile{
		FS:    guard.FSPerm{Write: []string{"/tmp/**"}},
		Tools: guard.ToolsPerm{Allow: []string{"fs.write"}},
	}))

	ctx := context.Background()
	_, err := c.Initialize(ctx, ClientCapabilities{})
	if err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	sessionID, err := c.NewSession(ctx, ".", nil, nil)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}

	_, err = c.Prompt(ctx, sessionID, "do something", func(ev Event) {})
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	resps := fa.InboundResponses()
	if len(resps) != 1 {
		t.Fatalf("expected 1 inbound response; got %d", len(resps))
	}
	if resps[0].Err != nil {
		t.Errorf("expected success; got error: %v", resps[0].Err)
	}
}

// TestPolicyFSWriteDeny verifies that when the GuardPolicy denies the path
// (empty Write list), the FakeAgent receives a JSON-RPC error response.
func TestPolicyFSWriteDeny(t *testing.T) {
	fa, clientIn, clientOut := NewFakeAgent()
	defer fa.Close()

	fa.Updates = nil
	fa.InboundRequests = []InboundSpec{
		{Method: "fs/write_text_file", Params: FSWriteParams{Path: "/tmp/ok", Content: "hi"}},
	}

	c := NewClient(clientIn, clientOut)
	defer c.Close()

	// FS.Write is empty — all writes denied.
	c.SetPolicy(NewGuardPolicy(guard.PermissionProfile{
		FS:    guard.FSPerm{},
		Tools: guard.ToolsPerm{Allow: []string{"fs.write"}},
	}))

	ctx := context.Background()
	_, err := c.Initialize(ctx, ClientCapabilities{})
	if err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	sessionID, err := c.NewSession(ctx, ".", nil, nil)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}

	_, err = c.Prompt(ctx, sessionID, "do something", func(ev Event) {})
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	resps := fa.InboundResponses()
	if len(resps) != 1 {
		t.Fatalf("expected 1 inbound response; got %d", len(resps))
	}
	if resps[0].Err == nil {
		t.Fatal("expected error response; got success")
	}
	if resps[0].Err.Code != -32603 {
		t.Errorf("error code = %d; want -32603", resps[0].Err.Code)
	}
}

// TestPolicyRequestPermission verifies that session/request_permission with
// an allow_once option auto-selects that option.
func TestPolicyRequestPermission(t *testing.T) {
	fa, clientIn, clientOut := NewFakeAgent()
	defer fa.Close()

	fa.Updates = nil
	fa.InboundRequests = []InboundSpec{
		{
			Method: "session/request_permission",
			Params: RequestPermissionParams{
				Options: []PermissionOption{
					{OptionID: "opt_1", Name: "Allow", Kind: "allow_once"},
					{OptionID: "opt_2", Name: "Deny", Kind: "deny"},
				},
			},
		},
	}

	c := NewClient(clientIn, clientOut)
	defer c.Close()

	gp := NewGuardPolicy(guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"*"}},
	})
	// Track a tool call with Kind="think" (no side effect → auto-allow).
	gp.TrackToolCall("tc_1", Update{Kind: "think"})
	c.SetPolicy(gp)

	ctx := context.Background()
	_, err := c.Initialize(ctx, ClientCapabilities{})
	if err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	sessionID, err := c.NewSession(ctx, ".", nil, nil)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}

	// Update the permission params to reference the tracked tool call.
	fa.InboundRequests[0].Params = RequestPermissionParams{
		ToolCall: ToolCallRef{ToolCallID: "tc_1"},
		Options: []PermissionOption{
			{OptionID: "opt_1", Name: "Allow", Kind: "allow_once"},
			{OptionID: "opt_2", Name: "Deny", Kind: "deny"},
		},
	}

	_, err = c.Prompt(ctx, sessionID, "do something", func(ev Event) {})
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	resps := fa.InboundResponses()
	if len(resps) != 1 {
		t.Fatalf("expected 1 inbound response; got %d", len(resps))
	}
	if resps[0].Err != nil {
		t.Fatalf("expected success; got error: %v", resps[0].Err)
	}

	var outcome PermissionOutcome
	if err := json.Unmarshal(resps[0].Result, &outcome); err != nil {
		t.Fatalf("unmarshal outcome: %v", err)
	}
	if outcome.Outcome != "selected" {
		t.Errorf("outcome = %q; want \"selected\"", outcome.Outcome)
	}
	if outcome.OptionID != "opt_1" {
		t.Errorf("optionId = %q; want \"opt_1\"", outcome.OptionID)
	}
}

// TestPolicyRequestPermissionNoAllow verifies that session/request_permission
// with no allow options returns a cancelled outcome.
func TestPolicyRequestPermissionNoAllow(t *testing.T) {
	fa, clientIn, clientOut := NewFakeAgent()
	defer fa.Close()

	fa.Updates = nil
	fa.InboundRequests = []InboundSpec{
		{
			Method: "session/request_permission",
			Params: RequestPermissionParams{
				Options: []PermissionOption{
					{OptionID: "opt_1", Name: "Deny", Kind: "deny"},
				},
			},
		},
	}

	c := NewClient(clientIn, clientOut)
	defer c.Close()

	gp := NewGuardPolicy(guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"*"}},
	})
	gp.TrackToolCall("tc_2", Update{Kind: "think"})
	c.SetPolicy(gp)

	ctx := context.Background()
	_, err := c.Initialize(ctx, ClientCapabilities{})
	if err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	sessionID, err := c.NewSession(ctx, ".", nil, nil)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}

	// Update the permission params to reference the tracked tool call.
	fa.InboundRequests[0].Params = RequestPermissionParams{
		ToolCall: ToolCallRef{ToolCallID: "tc_2"},
		Options: []PermissionOption{
			{OptionID: "opt_1", Name: "Deny", Kind: "deny"},
		},
	}

	_, err = c.Prompt(ctx, sessionID, "do something", func(ev Event) {})
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	resps := fa.InboundResponses()
	if len(resps) != 1 {
		t.Fatalf("expected 1 inbound response; got %d", len(resps))
	}
	if resps[0].Err != nil {
		t.Fatalf("expected success; got error: %v", resps[0].Err)
	}

	var outcome PermissionOutcome
	if err := json.Unmarshal(resps[0].Result, &outcome); err != nil {
		t.Fatalf("unmarshal outcome: %v", err)
	}
	if outcome.Outcome != "cancelled" {
		t.Errorf("outcome = %q; want \"cancelled\"", outcome.Outcome)
	}
}

// TestClientCancel verifies that Cancel sends a session/cancel notification
// that causes a held prompt (HoldPrompt=true) to return with stopReason:"cancelled".
func TestClientCancel(t *testing.T) {
	fa, clientIn, clientOut := NewFakeAgent()
	defer fa.Close()

	fa.HoldPrompt = true

	c := NewClient(clientIn, clientOut)
	defer c.Close()

	ctx := context.Background()
	_, err := c.Initialize(ctx, ClientCapabilities{})
	if err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	sessionID, err := c.NewSession(ctx, ".", nil, nil)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}

	// Start Prompt in a goroutine. With HoldPrompt=true the FakeAgent emits
	// updates but does not auto-resolve — it blocks until session/cancel arrives.
	type promptResult struct {
		stopReason string
		err        error
	}
	resultCh := make(chan promptResult, 1)
	go func() {
		sr, err := c.Prompt(ctx, sessionID, "hold this", func(ev Event) {})
		resultCh <- promptResult{sr, err}
	}()

	// Give the prompt a brief moment to reach the FakeAgent and block.
	select {
	case <-resultCh:
		t.Fatal("Prompt returned before Cancel was called")
	case <-time.After(50 * time.Millisecond):
		// Prompt is blocked — good, proceed to Cancel.
	}

	if err := c.Cancel(ctx, sessionID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}

	select {
	case res := <-resultCh:
		if res.err != nil {
			t.Fatalf("Prompt returned error: %v", res.err)
		}
		if res.stopReason != "cancelled" {
			t.Errorf("stopReason = %q; want \"cancelled\"", res.stopReason)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Prompt did not return after Cancel within 2s")
	}
}

// TestPromptResultUsageIsDelivered verifies that the token usage ACP carries on
// the session/prompt RESULT reaches the onEvent callback.
//
// ledger: F2/LEAK3#1 ACP turn usage 进 sink
//
// This is the sole data source of the whole goal-loop token budget: nothing
// else populates UsageSink, so if this delivery breaks, MaxTokens can be set to
// any value and the gate still never fires.
func TestPromptResultUsageIsDelivered(t *testing.T) {
	fa, clientIn, clientOut := NewFakeAgent()
	defer fa.Close()
	fa.Updates = []string{"thinking…"}
	// Script the usage where the protocol puts it: on the prompt response.
	fa.PromptUsage = &Usage{InputTokens: 100, OutputTokens: 50, TotalTokens: 150}

	cl := NewClient(clientIn, clientOut)
	defer cl.Close()
	_, err := cl.Initialize(context.Background(), ClientCapabilities{})
	require.NoError(t, err)

	sessionID, err := cl.NewSession(context.Background(), t.TempDir(), nil, nil)
	require.NoError(t, err)

	var capturedUsage *Usage
	onEvent := func(ev Event) {
		if ev.Usage != nil {
			capturedUsage = ev.Usage
		}
	}
	stopReason, err := cl.Prompt(context.Background(), sessionID, "do it", onEvent)
	require.NoError(t, err)
	require.Equal(t, "end_turn", stopReason)
	require.NotNil(t, capturedUsage, "prompt-result usage must be delivered as an Event")
	assert.Equal(t, 100, capturedUsage.InputTokens)
	assert.Equal(t, 50, capturedUsage.OutputTokens)
	assert.Equal(t, 150, capturedUsage.TotalTokens)
}

// TestNoPromptUsageDoesNotSetUsage verifies that an agent that omits the
// optional usage field leaves Event.Usage nil rather than delivering a zero
// Usage the sink would accumulate as if it were real.
func TestNoPromptUsageDoesNotSetUsage(t *testing.T) {
	fa, clientIn, clientOut := NewFakeAgent()
	defer fa.Close()
	fa.Updates = []string{"hello"}
	// No PromptUsage → no usage on events.

	cl := NewClient(clientIn, clientOut)
	defer cl.Close()
	_, err := cl.Initialize(context.Background(), ClientCapabilities{})
	require.NoError(t, err)

	sessionID, err := cl.NewSession(context.Background(), t.TempDir(), nil, nil)
	require.NoError(t, err)

	var gotUsage bool
	onEvent := func(ev Event) {
		if ev.Usage != nil {
			gotUsage = true
		}
	}
	_, err = cl.Prompt(context.Background(), sessionID, "step", onEvent)
	require.NoError(t, err)
	assert.False(t, gotUsage, "an omitted usage field must leave Event.Usage nil")
}

// TestZeroUsageIsNotDelivered pins the rule the deleted parseUsageReport used
// to carry: an all-zero report is dropped rather than forwarded.
//
// A zero Usage is not free. UsageSink accumulates whatever it is handed, so a
// long run against an agent that reports {0,0,0} every turn would look to the
// budget exactly like a run that genuinely spent nothing — and "spent nothing"
// is the one reading that can never trip the gate.
func TestZeroUsageIsNotDelivered(t *testing.T) {
	fa, clientIn, clientOut := NewFakeAgent()
	defer fa.Close()
	fa.Updates = []string{"nop"}
	fa.PromptUsage = &Usage{} // reported, but all counters zero

	cl := NewClient(clientIn, clientOut)
	defer cl.Close()
	_, err := cl.Initialize(context.Background(), ClientCapabilities{})
	require.NoError(t, err)
	sessionID, err := cl.NewSession(context.Background(), t.TempDir(), nil, nil)
	require.NoError(t, err)

	var gotUsage bool
	_, err = cl.Prompt(context.Background(), sessionID, "step", func(ev Event) {
		if ev.Usage != nil {
			gotUsage = true
		}
	})
	require.NoError(t, err)
	assert.False(t, gotUsage, "an all-zero usage report must not reach the sink")
}

// TestMalformedUsageDegradesButKeepsTheTurn pins the safe-degradation clause of
// F2/LEAK3: a usage field the agent typed wrong costs us that turn's accounting
// and nothing else.
//
// ledger: F2/LEAK3#3 解析失败安全降级
//
// Without the two-pass parse this is a turn-killer rather than a lost metric:
// json.Unmarshal fails on the whole result, so a wrong-typed optional field
// would abort work the agent had already completed.
func TestMalformedUsageDegradesButKeepsTheTurn(t *testing.T) {
	var pr PromptResult
	raw := json.RawMessage(`{"stopReason":"end_turn","usage":{"inputTokens":"one hundred"}}`)
	require.Error(t, json.Unmarshal(raw, &pr),
		"guard: this fixture must actually break the strict parse, else the test proves nothing")

	stop, usage, err := decodePromptResult(raw)
	require.NoError(t, err, "a bad usage field must not fail the decode")
	assert.Equal(t, "end_turn", stop, "the turn's outcome must survive a bad usage field")
	assert.Nil(t, usage, "a usage that does not parse must be dropped, not zero-filled")
}
