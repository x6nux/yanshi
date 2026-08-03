// Package eino tests for the Anthropic Messages API adapter (anthropic.go).
//
// These tests drive the adapter end-to-end via an httptest.Server that mimics
// the Anthropic Messages API wire format (POST /v1/messages), so the adapter
// is exercised without hitting the real API or needing an API key. Both the
// non-streaming and streaming code paths are covered, including the
// stop_reason → FinishReason mapping (review I2) and cache_read_input_tokens
// mapping (review I3).
package eino

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"
)

// TestAnthropicGenerate verifies the non-streaming path of the Anthropic
// adapter: it POSTs to /v1/messages, decodes the JSON response, and maps text
// content + tool_use + usage + stop_reason into a *schema.Message.
func TestAnthropicGenerate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/v1/messages") {
			t.Errorf("期望 /v1/messages，实际 %s", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Errorf("期望 POST，实际 %s", r.Method)
		}
		if got := r.Header.Get("x-api-key"); got != "k" {
			t.Errorf("期望 x-api-key=k，实际 %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":          "msg_1",
			"type":        "message",
			"role":        "assistant",
			"stop_reason": "end_turn",
			"content": []any{map[string]any{
				"type": "text",
				"text": "hello",
			}},
			"usage": map[string]any{
				"input_tokens":  10,
				"output_tokens": 5,
			},
		})
	}))
	defer srv.Close()

	m, err := NewAnthropicModel(context.Background(), &AnthropicModelConfig{
		APIKey:  "k",
		Model:   "claude-opus-4-8",
		BaseURL: srv.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	msg, err := m.Generate(context.Background(), []*schema.Message{schema.UserMessage("hi")})
	if err != nil {
		t.Fatal(err)
	}
	if msg.Content != "hello" {
		t.Fatalf("期望 content=hello，实际 %q", msg.Content)
	}
}

func TestAnthropicGenerate_ThinkingContent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"type": "message", "role": "assistant", "stop_reason": "end_turn",
			"content": []any{
				map[string]any{"type": "thinking", "thinking": "plan first"},
				map[string]any{"type": "text", "text": "answer"},
			},
		})
	}))
	defer srv.Close()

	m, err := NewAnthropicModel(context.Background(), &AnthropicModelConfig{APIKey: "k", Model: "claude", BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	msg, err := m.Generate(context.Background(), []*schema.Message{schema.UserMessage("hi")})
	if err != nil {
		t.Fatal(err)
	}
	if msg.ReasoningContent != "plan first" || msg.Content != "answer" {
		t.Fatalf("thinking/content mismatch: reasoning=%q content=%q", msg.ReasoningContent, msg.Content)
	}
}

func TestAnthropicStream_ThinkingDelta(t *testing.T) {
	sseBody := "event: message_start\n" +
		`data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","content":[],"stop_reason":null}}` + "\n\n" +
		"event: content_block_start\n" +
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"plan "}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"first"}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"answer"}}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"}}` + "\n\n" +
		"event: message_stop\n" + `data: {"type":"message_stop"}` + "\n\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, sseBody)
	}))
	defer srv.Close()

	m, err := NewAnthropicModel(context.Background(), &AnthropicModelConfig{APIKey: "k", Model: "claude", BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	sr, err := m.Stream(context.Background(), []*schema.Message{schema.UserMessage("hi")})
	if err != nil {
		t.Fatal(err)
	}
	defer sr.Close()
	var reasoning, content string
	for {
		chunk, recvErr := sr.Recv()
		if recvErr == io.EOF {
			break
		}
		if recvErr != nil {
			t.Fatal(recvErr)
		}
		if chunk == nil {
			continue
		}
		reasoning += chunk.ReasoningContent
		content += chunk.Content
	}
	if reasoning != "plan first" || content != "answer" {
		t.Fatalf("stream thinking/content mismatch: reasoning=%q content=%q", reasoning, content)
	}
}

// TestAnthropicGenerate_CachedTokens verifies the adapter surfaces
// cache_read_input_tokens (review I3): Anthropic's prompt-cache hit count
// should land on TokenUsage.PromptTokenDetails.CachedTokens so the TUI footer
// "cache:" segment and the /cost breakdown can report the savings.
func TestAnthropicGenerate_CachedTokens(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":          "msg_1",
			"type":        "message",
			"role":        "assistant",
			"stop_reason": "end_turn",
			"content": []any{map[string]any{
				"type": "text", "text": "hi",
			}},
			"usage": map[string]any{
				"input_tokens":                12000,
				"output_tokens":               500,
				"cache_read_input_tokens":     8000,
				"cache_creation_input_tokens": 1000,
			},
		})
	}))
	defer srv.Close()

	m, err := NewAnthropicModel(context.Background(), &AnthropicModelConfig{
		APIKey: "k", Model: "claude", BaseURL: srv.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	msg, err := m.Generate(context.Background(), []*schema.Message{schema.UserMessage("hi")})
	if err != nil {
		t.Fatal(err)
	}
	if msg.ResponseMeta == nil || msg.ResponseMeta.Usage == nil {
		t.Fatalf("期望 Usage 非 nil，实际 %+v", msg.ResponseMeta)
	}
	if got := msg.ResponseMeta.Usage.PromptTokenDetails.CachedTokens; got != 8000 {
		t.Fatalf("期望 PromptTokenDetails.CachedTokens=8000，实际 %d", got)
	}
}

// TestAnthropicStream_FinishReasonMapping verifies the streaming path maps
// message_delta.delta.stop_reason to the terminal chunk's FinishReason (I2):
// end_turn→stop, max_tokens→length, tool_use→tool_calls, stop_sequence→stop,
// unknown passes through verbatim. Before I2 the streaming terminal chunk
// carried Usage but no FinishReason, so the resilient layer and TUI could not
// tell a tool_use finish from a clean stop.
func TestAnthropicStream_FinishReasonMapping(t *testing.T) {
	cases := map[string]string{
		"end_turn":      "stop",
		"max_tokens":    "length",
		"tool_use":      "tool_calls",
		"stop_sequence": "stop",
		"weird":         "weird",
	}
	for stopReason, wantFinish := range cases {
		stopReason, wantFinish := stopReason, wantFinish
		t.Run(stopReason, func(t *testing.T) {
			sseBody := "event: message_start\n" +
				`data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","content":[],"stop_reason":null,"usage":{"input_tokens":10,"output_tokens":1}}}` + "\n\n" +
				"event: content_block_start\n" +
				`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n" +
				"event: content_block_delta\n" +
				`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"x"}}` + "\n\n" +
				"event: content_block_stop\n" +
				`data: {"type":"content_block_stop","index":0}` + "\n\n" +
				"event: message_delta\n" +
				"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":" + jsonQuote(stopReason) + ",\"stop_sequence\":null},\"usage\":{\"output_tokens\":2}}" + "\n\n" +
				"event: message_stop\n" +
				`data: {"type":"message_stop"}` + "\n\n"

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, sseBody)
			}))
			defer srv.Close()

			m, err := NewAnthropicModel(context.Background(), &AnthropicModelConfig{
				APIKey: "k", Model: "claude", BaseURL: srv.URL,
			})
			if err != nil {
				t.Fatal(err)
			}
			sr, err := m.Stream(context.Background(), []*schema.Message{schema.UserMessage("hi")})
			if err != nil {
				t.Fatal(err)
			}
			defer sr.Close()

			var meta *schema.ResponseMeta
			for {
				chunk, recvErr := sr.Recv()
				if recvErr != nil {
					if recvErr == io.EOF {
						break
					}
					t.Fatalf("Recv 错误：%v", recvErr)
				}
				if chunk == nil {
					break
				}
				if chunk.ResponseMeta != nil {
					meta = chunk.ResponseMeta
				}
			}
			if meta == nil {
				t.Fatal("期望终止帧带 ResponseMeta，实际 nil")
			}
			if meta.FinishReason != wantFinish {
				t.Fatalf("stop_reason=%s：期望 FinishReason=%q，实际 %q", stopReason, wantFinish, meta.FinishReason)
			}
		})
	}
}

// TestAnthropicStream_TextNoDoubleEmit guards against the streaming text
// double-emit bug (review C1): each content_block_delta text_delta must reach
// the consumer exactly once. Before C1 readStream both sent each delta
// immediately via sw.Send AND appended it to contentTextAccum, then re-emitted
// the whole accumulation on message_stop via flushText — so consumers that
// concatenate Content across chunks (resilient.consumeStream →
// classify.emitAssistantContent → ws.go assistantText accumulation →
// cs.history) saw every text token twice in the TUI and in session history.
//
// The fix aligns with responses.go's metaChunk (review I1): text deltas are
// streamed immediately and never accumulated. These cases verify both the
// many-delta path and a content_block_start that carries initial text.
func TestAnthropicStream_TextNoDoubleEmit(t *testing.T) {
	cases := []struct {
		name    string
		sseBody string
		want    string
	}{
		{
			// Three separate text_delta chunks must concatenate to "abc",
			// not "abcabc" (the pre-C1 behavior from flushText re-emitting).
			name: "multi delta",
			sseBody: "event: message_start\n" +
				`data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","content":[],"stop_reason":null,"usage":{"input_tokens":10,"output_tokens":1}}}` + "\n\n" +
				"event: content_block_start\n" +
				`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n" +
				"event: content_block_delta\n" +
				`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"a"}}` + "\n\n" +
				"event: content_block_delta\n" +
				`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"b"}}` + "\n\n" +
				"event: content_block_delta\n" +
				`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"c"}}` + "\n\n" +
				"event: content_block_stop\n" +
				`data: {"type":"content_block_stop","index":0}` + "\n\n" +
				"event: message_delta\n" +
				`data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":2}}` + "\n\n" +
				"event: message_stop\n" +
				`data: {"type":"message_stop"}` + "\n\n",
			want: "abc",
		},
		{
			// A content_block_start that arrives with non-empty text (rare but
			// possible) plus a following delta must emit "xy" once, not the
			// pre-C1 "yxy" (delta sent live, then start-text+delta re-flushed).
			name: "block start carries text",
			sseBody: "event: message_start\n" +
				`data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","content":[],"stop_reason":null,"usage":{"input_tokens":10,"output_tokens":1}}}` + "\n\n" +
				"event: content_block_start\n" +
				`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":"x"}}` + "\n\n" +
				"event: content_block_delta\n" +
				`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"y"}}` + "\n\n" +
				"event: content_block_stop\n" +
				`data: {"type":"content_block_stop","index":0}` + "\n\n" +
				"event: message_delta\n" +
				`data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":2}}` + "\n\n" +
				"event: message_stop\n" +
				`data: {"type":"message_stop"}` + "\n\n",
			want: "xy",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, tc.sseBody)
			}))
			defer srv.Close()

			m, err := NewAnthropicModel(context.Background(), &AnthropicModelConfig{
				APIKey: "k", Model: "claude", BaseURL: srv.URL,
			})
			if err != nil {
				t.Fatal(err)
			}
			sr, err := m.Stream(context.Background(), []*schema.Message{schema.UserMessage("hi")})
			if err != nil {
				t.Fatal(err)
			}
			defer sr.Close()

			var text strings.Builder
			for {
				chunk, recvErr := sr.Recv()
				if recvErr != nil {
					if recvErr == io.EOF {
						break
					}
					t.Fatalf("Recv 错误：%v", recvErr)
				}
				if chunk == nil {
					break
				}
				if chunk.Content != "" {
					text.WriteString(chunk.Content)
				}
			}
			if got := text.String(); got != tc.want {
				t.Fatalf("期望 stream content=%q（无重复），实际 %q", tc.want, got)
			}
		})
	}
}

// TestAnthropicStream_UsageFromStartFrame guards the streaming usage merge
// (review S1). Anthropic's streaming SSE splits usage across two events:
//
//	message_start.message.usage: input_tokens, output_tokens (initial 1),
//	  cache_read_input_tokens, cache_creation_input_tokens
//	message_delta.usage: output_tokens ONLY (final accumulated value)
//
// Before S1, readStream only read ev.Usage on message_delta, so on real
// Anthropic streams PromptTokens and CachedTokens were always 0
// (CompletionTokens was correct). The pre-S1 test fixture hid this by putting
// input_tokens inside message_delta.usage — which the real API never sends.
// This test mirrors the real wire format (input + cache_read on message_start,
// output_tokens alone on message_delta) and asserts all three counters flow
// into the terminal chunk non-zero.
func TestAnthropicStream_UsageFromStartFrame(t *testing.T) {
	sseBody := "event: message_start\n" +
		`data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","content":[],"stop_reason":null,"usage":{"input_tokens":12000,"output_tokens":1,"cache_read_input_tokens":8000,"cache_creation_input_tokens":1000}}}` + "\n\n" +
		"event: content_block_start\n" +
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}` + "\n\n" +
		"event: content_block_stop\n" +
		`data: {"type":"content_block_stop","index":0}` + "\n\n" +
		// message_delta.usage carries ONLY output_tokens on the real API —
		// no input_tokens, no cache_read_input_tokens. S1 is exactly that
		// these absent fields must be backfilled from message_start.
		"event: message_delta\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":500}}` + "\n\n" +
		"event: message_stop\n" +
		`data: {"type":"message_stop"}` + "\n\n"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, sseBody)
	}))
	defer srv.Close()

	m, err := NewAnthropicModel(context.Background(), &AnthropicModelConfig{
		APIKey: "k", Model: "claude", BaseURL: srv.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	sr, err := m.Stream(context.Background(), []*schema.Message{schema.UserMessage("hi")})
	if err != nil {
		t.Fatal(err)
	}
	defer sr.Close()

	var meta *schema.ResponseMeta
	for {
		chunk, recvErr := sr.Recv()
		if recvErr != nil {
			if recvErr == io.EOF {
				break
			}
			t.Fatalf("Recv 错误：%v", recvErr)
		}
		if chunk == nil {
			break
		}
		if chunk.ResponseMeta != nil {
			meta = chunk.ResponseMeta
		}
	}
	if meta == nil || meta.Usage == nil {
		t.Fatalf("期望终止帧带 Usage，实际 %+v", meta)
	}
	if got := meta.Usage.PromptTokens; got != 12000 {
		t.Errorf("期望 PromptTokens=12000（来自 message_start），实际 %d", got)
	}
	if got := meta.Usage.CompletionTokens; got != 500 {
		t.Errorf("期望 CompletionTokens=500（来自 message_delta），实际 %d", got)
	}
	if got := meta.Usage.PromptTokenDetails.CachedTokens; got != 8000 {
		t.Errorf("期望 CachedTokens=8000（来自 message_start），实际 %d", got)
	}
}

// jsonQuote wraps s as a JSON string literal (without surrounding whitespace)
// so it can be embedded directly into a hand-built SSE fixture.
func jsonQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// TestAnthropicGenerate_OutputSchema proves a per-turn OutputSchemaOption makes
// the adapter emit output_config.format = {type:"json_schema", schema:{...}} on
// the POST /v1/messages body (the Anthropic Messages API structured-output
// field). See the plan's DESIGN NOTE for why output_config.format is used over
// a top-level output_schema.
func TestAnthropicGenerate_OutputSchema(t *testing.T) {
	var captured map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &captured)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"type": "message", "role": "assistant", "stop_reason": "end_turn",
			"content": []any{map[string]any{"type": "text", "text": "{}"}},
		})
	}))
	defer srv.Close()

	m, err := NewAnthropicModel(context.Background(), &AnthropicModelConfig{APIKey: "k", Model: "claude", BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	optPtr := OutputSchemaOption(json.RawMessage(`{"type":"object","properties":{"x":{"type":"integer"}},"required":["x"],"additionalProperties":false}`))
	if optPtr == nil {
		t.Fatal("OutputSchemaOption must be non-nil for a non-empty schema")
	}
	if _, err := m.Generate(context.Background(), []*schema.Message{schema.UserMessage("hi")}, *optPtr); err != nil {
		t.Fatal(err)
	}

	oc, ok := captured["output_config"].(map[string]any)
	if !ok {
		t.Fatalf("期望请求体含 output_config，实际 %v", captured)
	}
	fmtField, ok := oc["format"].(map[string]any)
	if !ok || fmtField["type"] != "json_schema" {
		t.Fatalf("期望 output_config.format.type=json_schema，实际 %v", oc["format"])
	}
	sch, _ := fmtField["schema"].(map[string]any)
	if sch == nil || sch["type"] != "object" {
		t.Fatalf("期望 output_config.format.schema.type=object，实际 %v", fmtField["schema"])
	}
}

// TestAnthropicGenerate_NoSchemaOmitsOutputConfig is the text-mode regression
// guard: with no OutputSchemaOption the request body must NOT contain
// output_config (byte-identical to pre-A12 on the wire).
func TestAnthropicGenerate_NoSchemaOmitsOutputConfig(t *testing.T) {
	var sawOutputConfig bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(body, &m)
		if _, ok := m["output_config"]; ok {
			sawOutputConfig = true
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"type": "message", "role": "assistant", "stop_reason": "end_turn",
			"content": []any{map[string]any{"type": "text", "text": "hi"}},
		})
	}))
	defer srv.Close()

	m, err := NewAnthropicModel(context.Background(), &AnthropicModelConfig{APIKey: "k", Model: "claude", BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Generate(context.Background(), []*schema.Message{schema.UserMessage("hi")}); err != nil {
		t.Fatal(err)
	}
	if sawOutputConfig {
		t.Fatal("text 模式请求体不得含 output_config")
	}
}

// TestAnthropicToolInfoToJSONSchema proves toolInfoToJSONSchema returns a valid
// map for nil tool info.
func TestAnthropicToolInfoToJSONSchema(t *testing.T) {
	got := toolInfoToJSONSchema(nil)
	m, ok := got.(map[string]any)
	if !ok || m["type"] != "object" {
		t.Fatalf("nil tool must return {type:object}, got %#v", got)
	}
}

// TestAnthropicGenerate_HTTPError surfaces non-200 errors.
func TestAnthropicGenerate_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"rate limited"}}`))
	}))
	defer srv.Close()

	m, err := NewAnthropicModel(context.Background(), &AnthropicModelConfig{
		APIKey: "k", Model: "claude", BaseURL: srv.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = m.Generate(context.Background(), []*schema.Message{schema.UserMessage("hi")})
	if err == nil {
		t.Fatal("HTTP 429 must produce an error")
	}
}

// TestAnthropicStream_ToolUse proves the streaming path handles tool_use events.
func TestAnthropicStream_ToolUse(t *testing.T) {
	sseBody := "event: message_start\n" +
		`data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","content":[],"stop_reason":null,"usage":{"input_tokens":10,"output_tokens":1}}}` + "\n\n" +
		"event: content_block_start\n" +
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"tu_1","name":"fs_read"}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"path\":"}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"\"a.go\"}"}}` + "\n\n" +
		"event: content_block_stop\n" +
		`data: {"type":"content_block_stop","index":0}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":5}}` + "\n\n" +
		"event: message_stop\n" +
		`data: {"type":"message_stop"}` + "\n\n"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, sseBody)
	}))
	defer srv.Close()

	m, err := NewAnthropicModel(context.Background(), &AnthropicModelConfig{
		APIKey: "k", Model: "claude", BaseURL: srv.URL,
	})
	if err != nil {
		t.Fatal(err)
	}

	sr, err := m.Stream(context.Background(), []*schema.Message{schema.UserMessage("hi")})
	if err != nil {
		t.Fatal(err)
	}
	defer sr.Close()

	var toolCalls []schema.ToolCall
	var meta *schema.ResponseMeta
	for {
		chunk, recvErr := sr.Recv()
		if recvErr != nil {
			if recvErr == io.EOF {
				break
			}
			t.Fatalf("Recv error: %v", recvErr)
		}
		if chunk == nil {
			break
		}
		if len(chunk.ToolCalls) > 0 {
			toolCalls = append(toolCalls, chunk.ToolCalls...)
		}
		if chunk.ResponseMeta != nil {
			meta = chunk.ResponseMeta
		}
	}
	if len(toolCalls) != 1 {
		t.Fatalf("expected 1 ToolCall, got %d", len(toolCalls))
	}
	tc := toolCalls[0]
	if tc.ID != "tu_1" || tc.Function.Name != "fs_read" {
		t.Fatalf("ToolCall mismatch: %+v", tc)
	}
	if meta == nil || meta.FinishReason != "tool_calls" {
		t.Fatalf("expected FinishReason=tool_calls, got %+v", meta)
	}
}

// TestAnthropicStream_PingIgnored proves ping events are ignored.
func TestAnthropicStream_PingIgnored(t *testing.T) {
	sseBody := "event: ping\n" +
		`data: {"type":"ping"}` + "\n\n" +
		"event: message_start\n" +
		`data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","content":[],"stop_reason":null,"usage":{"input_tokens":1,"output_tokens":1}}}` + "\n\n" +
		"event: content_block_start\n" +
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}` + "\n\n" +
		"event: content_block_stop\n" +
		`data: {"type":"content_block_stop","index":0}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":1}}` + "\n\n" +
		"event: message_stop\n" +
		`data: {"type":"message_stop"}` + "\n\n"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, sseBody)
	}))
	defer srv.Close()

	m, err := NewAnthropicModel(context.Background(), &AnthropicModelConfig{
		APIKey: "k", Model: "claude", BaseURL: srv.URL,
	})
	if err != nil {
		t.Fatal(err)
	}

	sr, err := m.Stream(context.Background(), []*schema.Message{schema.UserMessage("hi")})
	if err != nil {
		t.Fatal(err)
	}
	defer sr.Close()

	var content string
	for {
		chunk, recvErr := sr.Recv()
		if recvErr != nil {
			if recvErr == io.EOF {
				break
			}
			t.Fatalf("Recv error: %v", recvErr)
		}
		if chunk == nil {
			break
		}
		content += chunk.Content
	}
	if content != "ok" {
		t.Fatalf("expected 'ok', got %q", content)
	}
}
