package http

import (
	"context"
	"encoding/json"
	"math"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/agent/orchestrator"
	"github.com/x6nux/yanshi/internal/features"
	einollm "github.com/x6nux/yanshi/internal/llm/eino"
	"github.com/x6nux/yanshi/internal/proto"
)

func dial(t *testing.T, url string) *websocket.Conn {
	t.Helper()
	c, _, err := websocket.DefaultDialer.DialContext(context.Background(), url, nil)
	require.NoError(t, err)
	return c
}

func TestChatWS_AgentChunkThenDone(t *testing.T) {
	o, err := orchestrator.New(orchestrator.Config{Model: einollm.NewFakeModel([]string{"hello"}, nil)})
	require.NoError(t, err)
	s := New(Config{Token: "t"})
	s.ChatWS(o, nil, nil)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	c := dial(t, "ws"+strings.TrimPrefix(ts.URL, "http")+"/api/v1/chat/ws")
	defer c.Close()
	require.NoError(t, c.WriteJSON(proto.NewUserMessage("hi")))

	var types []string
	for len(types) < 3 {
		c.SetReadDeadline(time.Now().Add(2 * time.Second))
		_, data, err := c.ReadMessage()
		require.NoError(t, err)
		var f proto.ServerFrame
		require.NoError(t, json.Unmarshal(data, &f))
		types = append(types, f.Type)
	}
	assert.Equal(t, []string{"agent_chunk", "status", "done"}, types)
}

func TestChatWS_MultiTurnMemory(t *testing.T) {
	fm := einollm.NewFakeModel(nil, nil)
	fm.Echo = true // echoes the last input message -> proves history is passed
	o, err := orchestrator.New(orchestrator.Config{Model: fm})
	require.NoError(t, err)
	s := New(Config{Token: "t"})
	s.ChatWS(o, nil, nil)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	c := dial(t, "ws"+strings.TrimPrefix(ts.URL, "http")+"/api/v1/chat/ws")
	defer c.Close()

	sendRecv := func(text string) string {
		require.NoError(t, c.WriteJSON(proto.NewUserMessage(text)))
		var out strings.Builder
		for {
			c.SetReadDeadline(time.Now().Add(2 * time.Second))
			_, data, err := c.ReadMessage()
			require.NoError(t, err)
			var f proto.ServerFrame
			require.NoError(t, json.Unmarshal(data, &f))
			if f.Type == "done" {
				break
			}
			if f.Type == "agent_chunk" {
				out.WriteString(f.Text)
			}
		}
		return out.String()
	}

	sendRecv("remember XYZ")
	// Second turn must reflect the first user message (history carried over).
	second := sendRecv("echo history")
	assert.Contains(t, second, "remember XYZ", "second turn must see first turn in history")
}

// TestChatWS_CancelMidTurn proves a cancel frame sent while a turn is running
// aborts the turn in real time. BlockingModel never returns on its own (Block
// stays open for the assertion), so the only way the turn can end is the real
// context cancel triggered by the cancel frame. It also verifies the connection
// survives the cancel and a follow-up turn completes normally.
func TestChatWS_CancelMidTurn(t *testing.T) {
	m := einollm.NewBlockingModel("hello")
	o, err := orchestrator.New(orchestrator.Config{Model: m})
	require.NoError(t, err)
	s := New(Config{Token: "t"})
	s.ChatWS(o, nil, nil)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	c := dial(t, "ws"+strings.TrimPrefix(ts.URL, "http")+"/api/v1/chat/ws")
	defer c.Close()

	// Start a turn. BlockingModel blocks until Block is closed (we never close
	// it for the cancel assertion), so this turn cannot finish on its own.
	require.NoError(t, c.WriteJSON(proto.NewUserMessage("hi")))
	// Wait until the model is actually in flight so the cancel targets a
	// running turn instead of racing turn setup.
	<-m.Started

	// Cancel mid-turn.
	require.NoError(t, c.WriteJSON(proto.NewCancel()))

	// The turn must end (a done frame) without us closing Block — the only way
	// out is the real context cancel from the cancel frame.
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	var sawDone bool
	for {
		_, data, err := c.ReadMessage()
		if err != nil {
			break // deadline / conn error — turn did not end
		}
		var f proto.ServerFrame
		require.NoError(t, json.Unmarshal(data, &f))
		if f.Type == "done" {
			sawDone = true
			break
		}
	}
	assert.True(t, sawDone, "turn should abort after cancel without releasing the model")

	// The connection must still be usable: release the model and run a second
	// turn that completes normally.
	close(m.Block)
	require.NoError(t, c.WriteJSON(proto.NewUserMessage("again")))
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	var out strings.Builder
	for {
		_, data, err := c.ReadMessage()
		require.NoError(t, err)
		var f proto.ServerFrame
		require.NoError(t, json.Unmarshal(data, &f))
		if f.Type == "agent_chunk" {
			out.WriteString(f.Text)
		}
		if f.Type == "done" {
			break
		}
	}
	assert.Contains(t, out.String(), "hello", "follow-up turn must complete after cancel")
}

// TestChatWS_ListMCP proves the list_mcp control frame (TUI /mcp) is answered
// with an mcp_list reply. No MCP servers are registered for the session, so the
// list is empty — the handler replies rather than dropping the frame.
func TestChatWS_ListMCP(t *testing.T) {
	o, err := orchestrator.New(orchestrator.Config{Model: einollm.NewFakeModel([]string{"x"}, nil)})
	require.NoError(t, err)
	s := New(Config{Token: "t"})
	s.ChatWS(o, nil, nil)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	c := dial(t, "ws"+strings.TrimPrefix(ts.URL, "http")+"/api/v1/chat/ws")
	defer c.Close()

	require.NoError(t, c.WriteJSON(proto.NewListMCP()))

	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, data, err := c.ReadMessage()
	require.NoError(t, err)
	var f proto.ServerFrame
	require.NoError(t, json.Unmarshal(data, &f))
	assert.Equal(t, "mcp_status", f.Type)
	assert.Empty(t, f.MCPServers, "no MCP configured -> empty list")
}

// TestChatWS_AutoCompaction_StreamsChunksAndStatus proves the AUTO-compaction
// path (Task 35b): once the session history exceeds the configured threshold,
// the next user_message turn first streams compact_chunk deltas, then emits a
// status{compacted=true} with before/after tokens, BEFORE the turn's own
// agent_chunk frames. Under-threshold turns do not compact.
func TestChatWS_AutoCompaction_StreamsChunksAndStatus(t *testing.T) {
	// Four scripted responses: two under-threshold LONG turn replies (long so
	// the new Plan — which pins user messages verbatim — still leaves the
	// assistant turns large enough for compaction to actually shrink tokens),
	// then the compaction summary, then the over-threshold turn reply.
	longReply := strings.Repeat("a", 100)
	fm := einollm.NewFakeModel([]string{longReply, longReply, "COMPACTIONSUMMARY", "r3"}, nil)
	o, err := orchestrator.New(orchestrator.Config{Model: fm})
	require.NoError(t, err)
	// Small budget so a handful of 100-char messages exceed it; KeepRecent=1
	// keeps the last pair (so len > 3 triggers a split on the 3rd turn).
	s := New(Config{Token: "t", Compaction: CompactionConfig{Threshold: 0.5, ContextWindow: 100, KeepRecent: 1}})
	// Register the fake model so compactionModel can pick it (the new
	// MaybeCompact signature takes a model directly, not the orchestrator).
	models := map[string]model.BaseChatModel{"fm": fm}
	s.ChatWS(o, models, nil)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	c := dial(t, "ws"+strings.TrimPrefix(ts.URL, "http")+"/api/v1/chat/ws")
	defer c.Close()

	long := strings.Repeat("u", 100) // ~33 tokens each

	// Turn 1: history=[u1]. Below both the threshold and the split guard.
	require.NoError(t, c.WriteJSON(proto.NewUserMessage(long)))
	recvTurn(t, c) // longReply #1

	// Turn 2: history=[u1,a1,u2] (len 3). Over the budget but len <= 2*1+1=3
	// -> too few to split -> still no compaction.
	require.NoError(t, c.WriteJSON(proto.NewUserMessage(long)))
	recvTurn(t, c) // longReply #2

	// Turn 3: history=[u1,a1,u2,a2,u3] (len 5 > 3) and well over budget ->
	// AUTO-compaction fires before the turn.
	require.NoError(t, c.WriteJSON(proto.NewUserMessage(long)))

	var types []string
	var compactStatus *proto.ServerFrame
	for {
		c.SetReadDeadline(time.Now().Add(3 * time.Second))
		_, data, rerr := c.ReadMessage()
		require.NoError(t, rerr)
		var f proto.ServerFrame
		require.NoError(t, json.Unmarshal(data, &f))
		if f.Type == "done" {
			break
		}
		types = append(types, f.Type)
		if f.Type == "status" && f.Compacted {
			sf := f
			compactStatus = &sf
		}
	}

	require.NotNil(t, compactStatus, "expected a status{compacted=true} frame")
	assert.Greater(t, compactStatus.TokensBefore, compactStatus.TokensAfter,
		"compaction must shrink the token estimate")
	assert.Contains(t, types, "compact_chunk", "compaction streams compact_chunk deltas")
	assert.Contains(t, types, "agent_chunk", "the turn still runs after compaction")

	// Ordering: every compact_chunk must precede the status{compacted}.
	ci := indexOf(types, "compact_chunk")
	si := indexOf(types, "status")
	require.True(t, ci >= 0 && si >= 0 && ci < si, "compact_chunk (%d) must precede status (%d)", ci, si)
}

// indexOf returns the first index of s in xs, or -1.
func indexOf(xs []string, s string) int {
	for i, v := range xs {
		if v == s {
			return i
		}
	}
	return -1
}

// --- A12-core: per-turn structured output (schema validation + retry) ---

// personSchema is a minimal JSON Schema reused across the structured-output
// tests: an object with a required "name" string and an optional "age" int,
// disallowing extra properties so missing/extra fields fail validation
// deterministically.
var personSchema = json.RawMessage(`{
  "type": "object",
  "properties": {"name": {"type": "string"}, "age": {"type": "integer"}},
  "required": ["name"],
  "additionalProperties": false
}`)

// TestWSStructuredOutputSuccess proves the happy path: when a user_message
// declares an output schema and the model's single attempt satisfies it, the
// turn emits a structured_result frame carrying the validated JSON before done.
// No retry happens (the model is called exactly once).
func TestWSStructuredOutputSuccess(t *testing.T) {
	fm := einollm.NewFakeModel([]string{`{"name":"Ada","age":36}`}, nil)
	o, err := orchestrator.New(orchestrator.Config{Model: fm})
	require.NoError(t, err)

	s := New(Config{Token: "t"})
	s.ChatWS(o, nil, nil)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	c := dial(t, "ws"+strings.TrimPrefix(ts.URL, "http")+"/api/v1/chat/ws")
	defer c.Close()
	require.NoError(t, c.WriteJSON(proto.NewUserMessageWithSchema("list users", personSchema)))

	var sawStructured bool
	var structuredJSON string
	var assistantText strings.Builder
	for {
		f := recvFrame(t, c)
		if f.Type == "done" {
			break
		}
		switch f.Type {
		case "agent_chunk":
			assistantText.WriteString(f.Text)
		case "structured_result":
			sawStructured = true
			structuredJSON = string(f.StructuredResult)
		}
	}
	assert.True(t, sawStructured, "schema-constrained turn must emit a structured_result frame")
	assert.JSONEq(t, `{"name":"Ada","age":36}`, structuredJSON,
		"structured_result must carry the validated JSON")
	assert.Equal(t, 1, fm.StreamCalls,
		"valid first attempt must not trigger a retry")
	_ = assistantText // agent_chunk stream still flowed
}

// TestWSStructuredOutputRetryThenSuccess proves the retry path: when the model's
// first attempt fails schema validation, the loop re-runs the turn with a
// reminder extension. The second attempt's history must contain the reminder
// (RecordMessages captures it). A structured_result with the second attempt's
// JSON is emitted.
func TestWSStructuredOutputRetryThenSuccess(t *testing.T) {
	fm := einollm.NewFakeModel([]string{
		"not json",       // attempt 0: invalid → retry
		`{"name":"Ada"}`, // attempt 1: valid → break
	}, nil)
	fm.RecordMessages = true

	o, err := orchestrator.New(orchestrator.Config{Model: fm})
	require.NoError(t, err)

	s := New(Config{Token: "t"})
	s.ChatWS(o, nil, nil)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	c := dial(t, "ws"+strings.TrimPrefix(ts.URL, "http")+"/api/v1/chat/ws")
	defer c.Close()
	require.NoError(t, c.WriteJSON(proto.NewUserMessageWithSchema("dump", personSchema)))

	var sawStructured bool
	var structuredJSON string
	var errs []string
	for {
		f := recvFrame(t, c)
		if f.Type == "done" {
			break
		}
		switch f.Type {
		case "error":
			errs = append(errs, f.Text)
		case "structured_result":
			sawStructured = true
			structuredJSON = string(f.StructuredResult)
		}
	}
	assert.Empty(t, errs, "retry-then-success path must not emit an error frame")
	assert.True(t, sawStructured, "must emit structured_result on the second attempt")
	assert.JSONEq(t, `{"name":"Ada"}`, structuredJSON,
		"structured_result carries the SECOND attempt's JSON")
	assert.GreaterOrEqual(t, fm.StreamCalls, 2,
		"invalid first attempt must trigger a retry")

	// The retry's input history must carry the schema-retry reminder. This is
	// the load-bearing assertion: without the reminder the model would re-emit
	// the same invalid output, making the retry useless.
	require.NotNil(t, fm.ReceivedMessages,
		"RecordMessages=true must capture the retry's input history")
	var sawReminder, sawPrevAssistant bool
	for _, m := range fm.ReceivedMessages {
		if m == nil {
			continue
		}
		if m.Role == schema.User && strings.Contains(m.Content, "did not satisfy the required output schema") {
			sawReminder = true
		}
		if m.Role == schema.Assistant && m.Content == "not json" {
			sawPrevAssistant = true
		}
	}
	assert.True(t, sawReminder, "retry history must include the schema-retry reminder")
	assert.True(t, sawPrevAssistant, "retry history must include the previous attempt's text")
}

// TestWSStructuredOutputRetryCapError proves the cap-exhaustion path: when the
// model NEVER emits schema-valid JSON, the loop exhausts maxSchemaRetries and
// emits an error frame mentioning the schema; no structured_result is emitted.
// The turn still ends with done (the loop breaks after the error marker).
func TestWSStructuredOutputRetryCapError(t *testing.T) {
	// 4 invalid responses: maxSchemaRetries=3 → attempts 0,1,2,3.
	fm := einollm.NewFakeModel([]string{"bad1", "bad2", "bad3", "bad4"}, nil)
	o, err := orchestrator.New(orchestrator.Config{Model: fm})
	require.NoError(t, err)

	s := New(Config{Token: "t"})
	s.ChatWS(o, nil, nil)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	c := dial(t, "ws"+strings.TrimPrefix(ts.URL, "http")+"/api/v1/chat/ws")
	defer c.Close()
	require.NoError(t, c.WriteJSON(proto.NewUserMessageWithSchema("x", personSchema)))

	var sawStructured bool
	var errs []string
	for {
		f := recvFrame(t, c)
		if f.Type == "done" {
			break
		}
		switch f.Type {
		case "error":
			errs = append(errs, f.Text)
		case "structured_result":
			sawStructured = true
		}
	}
	require.NotEmpty(t, errs, "cap exhaustion must emit at least one error frame")
	assert.Contains(t, errs[len(errs)-1], "schema",
		"the cap-exhaustion error must mention the schema")
	assert.False(t, sawStructured,
		"no structured_result when validation never succeeded")
	assert.Equal(t, 4, fm.StreamCalls,
		"must run exactly maxSchemaRetries+1 attempts before giving up")
}

// TestWSNoSchemaIsTextMode is the regression guard: a plain user_message (no
// OutputSchema) must NEVER emit a structured_result frame, and the turn must
// complete normally with a single model call. This proves the hasSchema gate
// keeps the text path byte-identical to pre-A12 behavior.
func TestWSNoSchemaIsTextMode(t *testing.T) {
	fm := einollm.NewFakeModel([]string{"hello"}, nil)
	o, err := orchestrator.New(orchestrator.Config{Model: fm})
	require.NoError(t, err)

	s := New(Config{Token: "t"})
	s.ChatWS(o, nil, nil)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	c := dial(t, "ws"+strings.TrimPrefix(ts.URL, "http")+"/api/v1/chat/ws")
	defer c.Close()
	require.NoError(t, c.WriteJSON(proto.NewUserMessage("hi")))

	var sawStructured bool
	var assistantText strings.Builder
	for {
		f := recvFrame(t, c)
		if f.Type == "done" {
			break
		}
		switch f.Type {
		case "agent_chunk":
			assistantText.WriteString(f.Text)
		case "structured_result":
			sawStructured = true
		}
	}
	assert.False(t, sawStructured,
		"text-mode turn must NOT emit structured_result (hasSchema gate)")
	assert.Equal(t, "hello", assistantText.String(),
		"text-mode turn must still stream the model's reply")
	assert.Equal(t, 1, fm.StreamCalls,
		"text-mode turn runs a single attempt (no schema retry)")
}

// TestWS_OutputSchemaReachesModel proves the WS user_message branch fills
// TurnOpts.OutputSchema from cf.OutputSchema so the schema reaches the model
// (end-to-end through the transport -> orchestrator -> adapter path). The model
// returns schema-valid JSON so A12-core's validation passes on the first
// attempt; the assertion is purely about option forwarding (RecordOpts +
// ReceivedOutputSchema), not retry behavior.
func TestWS_OutputSchemaReachesModel(t *testing.T) {
	fm := einollm.NewFakeModel([]string{`{"name":"Ada","age":36}`}, nil)
	fm.RecordOpts = true
	o, err := orchestrator.New(orchestrator.Config{Model: fm})
	require.NoError(t, err)

	s := New(Config{Token: "t"})
	s.ChatWS(o, nil, nil)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	c := dial(t, "ws"+strings.TrimPrefix(ts.URL, "http")+"/api/v1/chat/ws")
	defer c.Close()
	require.NoError(t, c.WriteJSON(proto.NewUserMessageWithSchema("list users", personSchema)))

	for {
		f := recvFrame(t, c)
		if f.Type == "done" {
			break
		}
	}
	assert.JSONEq(t, string(personSchema), string(fm.ReceivedOutputSchema),
		"cf.OutputSchema must reach the model via TurnOpts.OutputSchema -> option -> adapter")
}

// TestWS_NoSchemaLeavesModelEmpty is the text-mode regression guard for option
// forwarding: a plain user_message (no output_schema) must NOT forward a schema
// option, so ReceivedOutputSchema stays empty (byte-identical to pre-A12 on the
// option path).
func TestWS_NoSchemaLeavesModelEmpty(t *testing.T) {
	fm := einollm.NewFakeModel([]string{"hello"}, nil)
	fm.RecordOpts = true
	o, err := orchestrator.New(orchestrator.Config{Model: fm})
	require.NoError(t, err)

	s := New(Config{Token: "t"})
	s.ChatWS(o, nil, nil)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	c := dial(t, "ws"+strings.TrimPrefix(ts.URL, "http")+"/api/v1/chat/ws")
	defer c.Close()
	require.NoError(t, c.WriteJSON(proto.NewUserMessage("hi")))

	for {
		f := recvFrame(t, c)
		if f.Type == "done" {
			break
		}
	}
	assert.Empty(t, fm.ReceivedOutputSchema,
		"text-mode turn must not forward a schema option to the model")
}

// TestConnSessionBillsEveryProviderUsageAndJudge proves the C4 COST1 WS ledger
// bills every provider usage (not just the last) and that cost accumulates
// across the cache/non-cache/output split. Constraint 9 in the C4 plan: each
// provider usage is billed exactly once; the billed ledger is independent of
// the overwrite-semantics tokensIn (constraint 10).
func TestConnSessionBillsEveryProviderUsageAndJudge(t *testing.T) {
	s := &Server{priceTab: map[string]einollm.ModelPricing{
		"test-model": {InputPerM: 2, CacheHitPerM: 0.5, OutputPerM: 8},
	}}
	cs := connSession{model: "test-model"}
	cs.resetBilling(s)
	cs.addProviderUsage(context.Background(), s, orchestrator.TurnUsage{PromptTokens: 100, CachedTokens: 40, CompletionTokens: 10})
	cs.addProviderUsage(context.Background(), s, orchestrator.TurnUsage{PromptTokens: 80, CachedTokens: 20, CompletionTokens: 5})
	cs.addProviderUsage(context.Background(), s, orchestrator.TurnUsage{PromptTokens: 50, CachedTokens: 50, CompletionTokens: 2})
	if cs.billing.Billed.InputTokens != 120 || cs.billing.Billed.CachedTokens != 110 || cs.billing.Billed.OutputTokens != 17 {
		t.Fatalf("wrong billed ledger: %+v", cs.billing.Billed)
	}
	want := (120*2.0 + 110*0.5 + 17*8.0) / 1_000_000
	if math.Abs(cs.costUSD-want) > 1e-12 || !cs.costKnown {
		t.Fatalf("cost=(%f,%v), want=(%f,true)", cs.costUSD, cs.costKnown, want)
	}
}

// TestConnSessionUnknownModelIsNotZeroCost proves an unknown model surfaces as
// costKnown=false so renderers show "N/A" (constraint 11 in the C4 plan: N/A is
// distinct from $0.0000).
func TestConnSessionUnknownModelIsNotZeroCost(t *testing.T) {
	s := &Server{priceTab: einollm.DefaultPricing()}
	cs := connSession{model: "unknown-model"}
	cs.resetBilling(s)
	cs.addProviderUsage(context.Background(), s, orchestrator.TurnUsage{PromptTokens: 10})
	if cs.costKnown {
		t.Fatal("unknown model must be N/A")
	}
}

// TestFeatureRowsAndUnknownSet proves the /features WS handlers: listing,
// toggling a known flag, rejecting an unknown flag (named error), and
// rejecting a missing Enabled payload.
func TestFeatureRowsAndUnknownSet(t *testing.T) {
	reg := features.NewRegistry(true)
	reg.Register(features.Spec{Key: "observe.cost_in_status", Stage: features.Beta, Default: false, Owner: "runtime"})
	if err := setFeature(reg, proto.FeaturesSetPayload{Key: "observe.cost_in_status", Enabled: boolPtr(true)}); err != nil {
		t.Fatal(err)
	}
	rows := featureRows(reg)
	if len(rows) != 1 || !rows[0].Enabled {
		t.Fatalf("rows=%+v", rows)
	}
	err := setFeature(reg, proto.FeaturesSetPayload{Key: "typo", Enabled: boolPtr(true)})
	if err == nil || !strings.Contains(err.Error(), "typo") {
		t.Fatalf("err=%v", err)
	}
	err = setFeature(reg, proto.FeaturesSetPayload{Key: "observe.cost_in_status"})
	if err == nil || !strings.Contains(err.Error(), "enabled") {
		t.Fatalf("err=%v", err)
	}
}

func boolPtr(v bool) *bool { return &v }
