// Package eino tests for the OpenAI Responses API adapter (responses.go).
//
// These tests drive the adapter end-to-end via an httptest.Server that
// mimics the OpenAI Responses API wire format (POST /v1/responses), so the
// adapter is exercised without hitting the real API or needing an API key.
// Both the non-streaming and streaming code paths are covered.
package eino

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"
)

// TestResponsesGenerate verifies the non-streaming path of the Responses
// adapter: it POSTs to /v1/responses, decodes the JSON response, and maps
// output_text + usage + status into a *schema.Message.
func TestResponsesGenerate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/v1/responses") {
			t.Errorf("期望 /v1/responses，实际 %s", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Errorf("期望 POST，实际 %s", r.Method)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer k" {
			t.Errorf("期望 Authorization=Bearer k，实际 %q", got)
		}
		// Echo the request input back so we can also assert request mapping.
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		if model, _ := req["model"].(string); model != "gpt-4o" {
			t.Errorf("期望 model=gpt-4o，实际 %v", req["model"])
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":     "resp_1",
			"object": "response",
			"status": "completed",
			"output": []any{map[string]any{
				"type": "message",
				"role": "assistant",
				"content": []any{
					map[string]any{"type": "output_text", "text": "hello"},
				},
			}},
			"usage": map[string]any{"input_tokens": 10, "output_tokens": 5, "total_tokens": 15},
		})
	}))
	defer srv.Close()

	m, err := NewOpenAIResponsesModel(context.Background(), &ResponsesConfig{
		APIKey:  "k",
		Model:   "gpt-4o",
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
	if msg.ResponseMeta == nil || msg.ResponseMeta.Usage == nil ||
		msg.ResponseMeta.Usage.PromptTokens != 10 ||
		msg.ResponseMeta.Usage.CompletionTokens != 5 {
		t.Fatalf("期望 PromptTokens=10 CompletionTokens=5，实际 %+v", msg.ResponseMeta)
	}
	if msg.ResponseMeta.FinishReason != "stop" {
		t.Fatalf("期望 FinishReason=stop（completed→stop），实际 %q", msg.ResponseMeta.FinishReason)
	}
}

// TestResponsesGenerate_StatusMapping covers the Responses API status →
// FinishReason mapping: "completed"→"stop", "incomplete"→"length", and an
// unknown status passes through verbatim.
func TestResponsesGenerate_StatusMapping(t *testing.T) {
	cases := map[string]string{
		"completed":   "stop",
		"incomplete":  "length",
		"cancelled":   "cancelled",
	}
	for apiStatus, wantFinish := range cases {
		apiStatus, wantFinish := apiStatus, wantFinish
		t.Run(apiStatus, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{
					"status": apiStatus,
					"output": []any{map[string]any{
						"type": "message",
						"content": []any{
							map[string]any{"type": "output_text", "text": "x"},
						},
					}},
				})
			}))
			defer srv.Close()

			m, err := NewOpenAIResponsesModel(context.Background(), &ResponsesConfig{
				APIKey: "k", Model: "gpt-4o", BaseURL: srv.URL,
			})
			if err != nil {
				t.Fatal(err)
			}
			msg, err := m.Generate(context.Background(), []*schema.Message{schema.UserMessage("hi")})
			if err != nil {
				t.Fatal(err)
			}
			if msg.ResponseMeta == nil || msg.ResponseMeta.FinishReason != wantFinish {
				t.Fatalf("status=%s：期望 FinishReason=%q，实际 %+v", apiStatus, wantFinish, msg.ResponseMeta)
			}
		})
	}
}

// TestResponsesGenerate_HTTPError verifies the adapter surfaces non-200
// responses as errors (the resilient layer relies on this contract).
func TestResponsesGenerate_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"rate limited"}}`))
	}))
	defer srv.Close()

	m, err := NewOpenAIResponsesModel(context.Background(), &ResponsesConfig{
		APIKey: "k", Model: "gpt-4o", BaseURL: srv.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = m.Generate(context.Background(), []*schema.Message{schema.UserMessage("hi")})
	if err == nil {
		t.Fatal("期望 HTTP 429 错误，实际 nil")
	}
	if !strings.Contains(err.Error(), "429") {
		t.Fatalf("期望错误信息包含 429，实际 %v", err)
	}
}

// TestResponsesGenerate_MissingConfig ensures NewOpenAIResponsesModel
// validates required fields so misconfigured providers fail fast at startup
// rather than on the first request.
func TestResponsesGenerate_MissingConfig(t *testing.T) {
	cases := []struct {
		name string
		cfg  *ResponsesConfig
	}{
		{"empty APIKey", &ResponsesConfig{Model: "gpt-4o"}},
		{"empty Model", &ResponsesConfig{APIKey: "k"}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewOpenAIResponsesModel(context.Background(), tc.cfg)
			if err == nil {
				t.Fatal("期望 error，实际 nil")
			}
		})
	}
}

// TestResponsesStream drives the SSE streaming path: the httptest server emits
// response.output_text.delta chunks followed by response.completed (with final
// usage + status). The test consumes the StreamReader and asserts the joined
// content, usage, and FinishReason.
func TestResponsesStream(t *testing.T) {
	const sseBody = "event: response.created\n" +
		`data: {"type":"response.created","response":{"id":"resp_1","status":"in_progress"}}` + "\n\n" +
		"event: response.output_text.delta\n" +
		`data: {"type":"response.output_text.delta","delta":"he"}` + "\n\n" +
		"event: response.output_text.delta\n" +
		`data: {"type":"response.output_text.delta","delta":"llo"}` + "\n\n" +
		"event: response.output_text.done\n" +
		`data: {"type":"response.output_text.done","text":"hello"}` + "\n\n" +
		"event: response.completed\n" +
		`data: {"type":"response.completed","response":{"id":"resp_1","status":"completed","usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}}}` + "\n\n"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Confirm stream=true was sent in the request body.
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		if stream, _ := req["stream"].(bool); !stream {
			t.Errorf("期望 request.stream=true，实际 %v", req["stream"])
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, sseBody)
	}))
	defer srv.Close()

	m, err := NewOpenAIResponsesModel(context.Background(), &ResponsesConfig{
		APIKey: "k", Model: "gpt-4o", BaseURL: srv.URL,
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
		if chunk.Content != "" {
			text.WriteString(chunk.Content)
		}
		if chunk.ResponseMeta != nil {
			meta = chunk.ResponseMeta
		}
	}

	if got := text.String(); got != "hello" {
		t.Fatalf("期望 stream content=hello，实际 %q", got)
	}
	if meta == nil || meta.Usage == nil ||
		meta.Usage.PromptTokens != 3 || meta.Usage.CompletionTokens != 2 {
		t.Fatalf("期望 PromptTokens=3 CompletionTokens=2，实际 %+v", meta)
	}
	if meta == nil || meta.FinishReason != "stop" {
		t.Fatalf("期望 FinishReason=stop（completed→stop），实际 %+v", meta)
	}
}

// TestResponsesStream_Incomplete verifies that response.incomplete maps to
// FinishReason="length" in the terminal stream chunk.
func TestResponsesStream_Incomplete(t *testing.T) {
	const sseBody = "event: response.output_text.delta\n" +
		`data: {"type":"response.output_text.delta","delta":"partial"}` + "\n\n" +
		"event: response.incomplete\n" +
		`data: {"type":"response.incomplete","response":{"id":"resp_1","status":"incomplete","usage":{"input_tokens":1,"output_tokens":1}}}` + "\n\n"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, sseBody)
	}))
	defer srv.Close()

	m, err := NewOpenAIResponsesModel(context.Background(), &ResponsesConfig{
		APIKey: "k", Model: "gpt-4o", BaseURL: srv.URL,
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
	if meta.FinishReason != "length" {
		t.Fatalf("期望 incomplete→FinishReason=length，实际 %q", meta.FinishReason)
	}
}

// TestResponsesStream_HTTPError confirms non-200 responses are returned as
// errors from Stream (rather than being streamed as broken SSE).
func TestResponsesStream_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`gateway down`))
	}))
	defer srv.Close()

	m, err := NewOpenAIResponsesModel(context.Background(), &ResponsesConfig{
		APIKey: "k", Model: "gpt-4o", BaseURL: srv.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = m.Stream(context.Background(), []*schema.Message{schema.UserMessage("hi")})
	if err == nil {
		t.Fatal("期望 HTTP 500 错误，实际 nil")
	}
}

// TestResponsesRequest_Mapping sanity-checks the request body mapping for
// system + user + assistant + tool messages by inspecting the serialized JSON.
func TestResponsesRequest_Mapping(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &got)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"completed","output":[]}`))
	}))
	defer srv.Close()

	m, err := NewOpenAIResponsesModel(context.Background(), &ResponsesConfig{
		APIKey: "k", Model: "gpt-4o", BaseURL: srv.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = m.Generate(context.Background(), []*schema.Message{
		schema.SystemMessage("you are bot"),
		schema.UserMessage("hello"),
		schema.AssistantMessage("hi back", nil),
	})
	if err != nil {
		t.Fatal(err)
	}

	if ins, _ := got["instructions"].(string); ins != "you are bot" {
		t.Errorf("期望 instructions=\"you are bot\"，实际 %v", got["instructions"])
	}
	input, _ := got["input"].([]any)
	if len(input) != 2 {
		t.Fatalf("期望 input 长度=2（user+assistant；system 已提出为 instructions），实际 %d", len(input))
	}
	first, _ := input[0].(map[string]any)
	if first["role"] != "user" {
		t.Errorf("期望 input[0].role=user，实际 %v", first["role"])
	}
}

// TestResponsesStream_ConnClosed guards against the server closing the
// connection mid-stream without a terminal event: the reader must still
// terminate (Recv returns io.EOF) so downstream consumers are not stuck.
func TestResponsesStream_ConnClosed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "event: response.output_text.delta\n"+
			`data: {"type":"response.output_text.delta","delta":"x"}`+"\n\n")
		// connection ends here without response.completed — reader must still terminate
	}))
	defer srv.Close()

	m, err := NewOpenAIResponsesModel(context.Background(), &ResponsesConfig{
		APIKey: "k", Model: "gpt-4o", BaseURL: srv.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	sr, err := m.Stream(context.Background(), []*schema.Message{schema.UserMessage("hi")})
	if err != nil {
		t.Fatal(err)
	}
	defer sr.Close()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			_, recvErr := sr.Recv()
			if recvErr != nil {
				return
			}
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("stream 未在 5 秒内终止")
	}
}

// TestResponsesStream_FunctionCall verifies the terminal stream chunk carries
// ToolCalls extracted from response.completed's output[] (review I1).
//
// Before I1, metaChunk only filled ResponseMeta (Usage + FinishReason) and
// silently dropped function_call items, so a Responses-API stream never surfaced
// tool calls to the ADK ReAct loop — the model appeared to produce no tool
// invocation even when the wire payload carried one. The fix makes metaChunk
// reuse responseToMessage so the terminal chunk carries ToolCalls too.
//
// Content must NOT be re-emitted on the terminal chunk: response.output_text.
// delta already streamed the text token-by-token, and downstream consumers
// concatenate Content across chunks — re-emitting would double the text.
func TestResponsesStream_FunctionCall(t *testing.T) {
	const sseBody = "event: response.output_text.delta\n" +
		`data: {"type":"response.output_text.delta","delta":"thinking…"}` + "\n\n" +
		"event: response.completed\n" +
		`data: {"type":"response.completed","response":{"id":"resp_1","status":"completed","output":[` +
		`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"thinking…"}]},` +
		`{"type":"function_call","call_id":"call_42","name":"fs_read","arguments":"{\"path\":\"a.go\"}"}` +
		`],"usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}}}` + "\n\n"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, sseBody)
	}))
	defer srv.Close()

	m, err := NewOpenAIResponsesModel(context.Background(), &ResponsesConfig{
		APIKey: "k", Model: "gpt-4o", BaseURL: srv.URL,
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
	var toolCalls []schema.ToolCall
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
		if chunk.Content != "" {
			text.WriteString(chunk.Content)
		}
		if len(chunk.ToolCalls) > 0 {
			toolCalls = append(toolCalls, chunk.ToolCalls...)
		}
		if chunk.ResponseMeta != nil {
			meta = chunk.ResponseMeta
		}
	}

	if got := text.String(); got != "thinking…" {
		t.Fatalf("期望 stream content=thinking…（不重复），实际 %q", got)
	}
	if len(toolCalls) != 1 {
		t.Fatalf("期望 1 个 ToolCall（response.completed 的 function_call 项），实际 %d（%+v）", len(toolCalls), toolCalls)
	}
	tc := toolCalls[0]
	if tc.ID != "call_42" || tc.Function.Name != "fs_read" || tc.Function.Arguments != `{"path":"a.go"}` {
		t.Fatalf("ToolCall 字段映射错误：got %+v", tc)
	}
	if meta == nil || meta.FinishReason != "stop" {
		t.Fatalf("期望终止帧 FinishReason=stop，实际 %+v", meta)
	}
}

// TestResponsesGenerate_ReasoningTokens verifies the Responses API adapter
// surfaces output_tokens_details.reasoning_tokens (review I3). The Responses
// API is the primary interface for OpenAI's o1/o3 reasoning models, where the
// reasoning-token bill often dominates cost; without this mapping the TUI
// footer's "think:" segment stays at 0 even though the API reported thousands
// of reasoning tokens.
func TestResponsesGenerate_ReasoningTokens(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":     "resp_1",
			"object": "response",
			"status": "completed",
			"output": []any{map[string]any{
				"type": "message",
				"content": []any{
					map[string]any{"type": "output_text", "text": "answer"},
				},
			}},
			"usage": map[string]any{
				"input_tokens":  10,
				"output_tokens": 5,
				"total_tokens":  15,
				"output_tokens_details": map[string]any{
					"reasoning_tokens": 1200,
				},
			},
		})
	}))
	defer srv.Close()

	m, err := NewOpenAIResponsesModel(context.Background(), &ResponsesConfig{
		APIKey: "k", Model: "o3-mini", BaseURL: srv.URL,
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
	if got := msg.ResponseMeta.Usage.CompletionTokensDetails.ReasoningTokens; got != 1200 {
		t.Fatalf("期望 CompletionTokensDetails.ReasoningTokens=1200，实际 %d", got)
	}
}

// helper for manual debugging: silence unused-import warnings if a future
// refactor temporarily drops a dependency.
var _ = fmt.Sprintf

// TestResponsesGenerate_OutputSchema proves a per-turn OutputSchemaOption makes
// the adapter emit text.format = {type:"json_schema", name, schema, strict} on
// the POST /v1/responses body (the OpenAI Responses API structured-output
// field).
func TestResponsesGenerate_OutputSchema(t *testing.T) {
	var captured map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &captured)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "resp_1", "object": "response", "status": "completed",
			"output": []any{map[string]any{
				"type": "message", "role": "assistant",
				"content": []any{map[string]any{"type": "output_text", "text": "{}"}},
			}},
		})
	}))
	defer srv.Close()

	m, err := NewOpenAIResponsesModel(context.Background(), &ResponsesConfig{APIKey: "k", Model: "gpt-4o", BaseURL: srv.URL})
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

	txt, ok := captured["text"].(map[string]any)
	if !ok {
		t.Fatalf("期望请求体含 text，实际 %v", captured)
	}
	fmtField, ok := txt["format"].(map[string]any)
	if !ok || fmtField["type"] != "json_schema" {
		t.Fatalf("期望 text.format.type=json_schema，实际 %v", txt["format"])
	}
	if fmtField["name"] == nil || fmtField["name"] == "" {
		t.Fatalf("期望 text.format.name 非空（OpenAI 要求），实际 %v", fmtField["name"])
	}
	sch, _ := fmtField["schema"].(map[string]any)
	if sch == nil || sch["type"] != "object" {
		t.Fatalf("期望 text.format.schema.type=object，实际 %v", fmtField["schema"])
	}
}

// TestResponsesGenerate_NoSchemaOmitsText is the text-mode regression guard:
// with no OutputSchemaOption the request body must NOT contain a text field.
func TestResponsesGenerate_NoSchemaOmitsText(t *testing.T) {
	var sawText bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(body, &m)
		if _, ok := m["text"]; ok {
			sawText = true
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "resp_1", "object": "response", "status": "completed",
			"output": []any{map[string]any{
				"type": "message", "role": "assistant",
				"content": []any{map[string]any{"type": "output_text", "text": "hi"}},
			}},
		})
	}))
	defer srv.Close()

	m, err := NewOpenAIResponsesModel(context.Background(), &ResponsesConfig{APIKey: "k", Model: "gpt-4o", BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Generate(context.Background(), []*schema.Message{schema.UserMessage("hi")}); err != nil {
		t.Fatal(err)
	}
	if sawText {
		t.Fatal("text 模式请求体不得含 text 字段")
	}
}

// TestResponsesStream_Failed proves a response.failed event surfaces as an error.
func TestResponsesStream_Failed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "event: response.failed\n"+
			`data: {"type":"response.failed","response":{"id":"resp_1","status":"failed"}}`+"\n\n")
	}))
	defer srv.Close()

	m, err := NewOpenAIResponsesModel(context.Background(), &ResponsesConfig{
		APIKey: "k", Model: "gpt-4o", BaseURL: srv.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	sr, err := m.Stream(context.Background(), []*schema.Message{schema.UserMessage("hi")})
	if err != nil {
		t.Fatal(err)
	}
	defer sr.Close()

	_, recvErr := sr.Recv()
	if recvErr == nil {
		t.Fatal("expected error from response.failed event")
	}
}

// TestResponsesStream_Error proves an error SSE event surfaces as an error.
func TestResponsesStream_Error(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "event: error\n"+
			`data: {"type":"error","code":"server_error","message":"internal error"}`+"\n\n")
	}))
	defer srv.Close()

	m, err := NewOpenAIResponsesModel(context.Background(), &ResponsesConfig{
		APIKey: "k", Model: "gpt-4o", BaseURL: srv.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	sr, err := m.Stream(context.Background(), []*schema.Message{schema.UserMessage("hi")})
	if err != nil {
		t.Fatal(err)
	}
	defer sr.Close()

	_, recvErr := sr.Recv()
	if recvErr == nil {
		t.Fatal("expected error from SSE error event")
	}
}

// TestResponsesMetaChunk_Nil proves metaChunk returns a valid message for nil.
func TestResponsesMetaChunk_Nil(t *testing.T) {
	chunk := (&openaiResponsesModel{}).metaChunk(nil)
	if chunk == nil {
		t.Fatal("metaChunk(nil) must return a non-nil message")
	}
	if chunk.Content != "" {
		t.Fatalf("metaChunk should have empty content, got %q", chunk.Content)
	}
}

// TestResponsesStream_DoneTerminator proves [DONE] terminates cleanly.
func TestResponsesStream_DoneTerminator(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	m, err := NewOpenAIResponsesModel(context.Background(), &ResponsesConfig{
		APIKey: "k", Model: "gpt-4o", BaseURL: srv.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	sr, err := m.Stream(context.Background(), []*schema.Message{schema.UserMessage("hi")})
	if err != nil {
		t.Fatal(err)
	}
	defer sr.Close()

	_, recvErr := sr.Recv()
	if recvErr != nil && recvErr != io.EOF {
		t.Fatalf("unexpected error after [DONE]: %v", recvErr)
	}
}
