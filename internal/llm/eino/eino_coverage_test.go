package eino

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Anthropic edge cases
// ---------------------------------------------------------------------------

// TestAnthropicModel_NilAPIKey proves NewAnthropicModel errors on empty API key.
func TestAnthropicModel_NilAPIKey(t *testing.T) {
	_, err := NewAnthropicModel(context.Background(), &AnthropicModelConfig{
		Model: "claude-opus-4-8",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "APIKey is required")
}

// TestAnthropicModel_NilModel proves NewAnthropicModel errors on empty model name.
func TestAnthropicModel_NilModel(t *testing.T) {
	_, err := NewAnthropicModel(context.Background(), &AnthropicModelConfig{
		APIKey: "k",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Model is required")
}

// TestAnthropicModel_EmptyEndpointDefaults proves a missing BaseURL defaults to the standard API.
func TestAnthropicModel_EmptyEndpointDefaults(t *testing.T) {
	m, err := NewAnthropicModel(context.Background(), &AnthropicModelConfig{
		APIKey: "k", Model: "claude",
	})
	require.NoError(t, err)
	assert.Equal(t, "https://api.anthropic.com/v1", m.config.BaseURL)
}

// TestAnthropicModel_CustomHTTPClient proves a custom HTTP client is used.
func TestAnthropicModel_CustomHTTPClient(t *testing.T) {
	hc := &http.Client{Timeout: time.Second}
	m, err := NewAnthropicModel(context.Background(), &AnthropicModelConfig{
		APIKey: "k", Model: "claude", HTTPClient: hc,
	})
	require.NoError(t, err)
	assert.Equal(t, hc, m.config.HTTPClient)
}

// TestAnthropicGenerate_SystemMessage proves system messages are routed to req.System.
func TestAnthropicGenerate_SystemMessage(t *testing.T) {
	var sawBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(body, &m)
		sawBody = m
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"role":"assistant","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn"}`))
	}))
	defer srv.Close()

	m, err := NewAnthropicModel(context.Background(), &AnthropicModelConfig{
		APIKey: "k", Model: "claude", BaseURL: srv.URL,
	})
	require.NoError(t, err)

	_, err = m.Generate(context.Background(), []*schema.Message{
		schema.SystemMessage("you are a bot"),
		schema.UserMessage("hi"),
	})
	require.NoError(t, err)

	sys, ok := sawBody["system"].(string)
	require.True(t, ok, "system field must be present")
	assert.Equal(t, "you are a bot", sys)
}

// TestAnthropicGenerate_AssistantWithToolCalls proves assistant tool calls are
// mapped to tool_use content blocks in the request.
func TestAnthropicGenerate_AssistantWithToolCalls(t *testing.T) {
	var sawMessages []any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(body, &m)
		sawMessages, _ = m["messages"].([]any)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"role":"assistant","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn"}`))
	}))
	defer srv.Close()

	m, err := NewAnthropicModel(context.Background(), &AnthropicModelConfig{
		APIKey: "k", Model: "claude", BaseURL: srv.URL,
	})
	require.NoError(t, err)

	_, err = m.Generate(context.Background(), []*schema.Message{
		schema.UserMessage("hi"),
		schema.AssistantMessage("let me check", []schema.ToolCall{
			{ID: "c1", Function: schema.FunctionCall{Name: "fs_read", Arguments: `{"path":"x"}`}},
		}),
	})
	require.NoError(t, err)

	require.Len(t, sawMessages, 2)
	assistantMsg := sawMessages[1].(map[string]any)
	assert.Equal(t, "assistant", assistantMsg["role"])
}

// TestAnthropicGenerate_ToolRequest proves tool-role messages are mapped properly.
func TestAnthropicGenerate_ToolRequest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(body, &m)
		msgs := m["messages"].([]any)
		last := msgs[len(msgs)-1].(map[string]any)
		if last["role"] != "user" {
			t.Errorf("tool result message should have role=user, got %v", last["role"])
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"role":"assistant","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn"}`))
	}))
	defer srv.Close()

	m, err := NewAnthropicModel(context.Background(), &AnthropicModelConfig{
		APIKey: "k", Model: "claude", BaseURL: srv.URL,
	})
	require.NoError(t, err)

	_, err = m.Generate(context.Background(), []*schema.Message{
		schema.UserMessage("hi"),
		{Role: schema.Tool, ToolCallID: "c1", ToolName: "fs_read", Content: "file content"},
	})
	require.NoError(t, err)
}

// TestAnthropicResponse_NilResp proves responseToMessage handles nil.
func TestAnthropicResponse_NilResp(t *testing.T) {
	m := &AnthropicModel{}
	msg := m.responseToMessage(nil)
	assert.Nil(t, msg, "nil resp returns nil message")
}

// TestAnthropicResponse_NoUsageNoStop proves responseToMessage handles missing fields.
func TestAnthropicResponse_NoUsageNoStop(t *testing.T) {
	m := &AnthropicModel{}
	resp := &anthropicResponse{
		Type:    "message",
		Role:    "assistant",
		Content: []anthropicContentPart{{Type: "text", Text: "hello"}},
	}
	msg := m.responseToMessage(resp)
	require.NotNil(t, msg)
	assert.Equal(t, "hello", msg.Content)
	assert.Nil(t, msg.ResponseMeta, "no usage means no ResponseMeta")
}

// TestAnthropicBuildRequest_EmptyMessagesSkipped proves empty messages are skipped.
func TestAnthropicBuildRequest_EmptyMessagesSkipped(t *testing.T) {
	m := &AnthropicModel{config: AnthropicModelConfig{Model: "claude", APIKey: "k", MaxTokens: 100}}
	req, err := m.buildRequest([]*schema.Message{
		{Role: schema.User, Content: ""}, // empty user → skipped
		{Role: schema.Assistant, Content: "", ToolCalls: nil},
		{Role: schema.User, Content: "actual"},
	}, &model.Options{}, &outputSchemaOptions{}, false)
	require.NoError(t, err)
	require.Len(t, req.Messages, 2) // assistant (empty) included, empty user skipped
	assert.Equal(t, "user", req.Messages[1].Role)
	assert.Equal(t, "actual", req.Messages[1].Content[0].Text)
}

// TestToolInfoToJSONSchema_Valid proves toolInfoToJSONSchema works for valid tool info.
func TestToolInfoToJSONSchema_Valid(t *testing.T) {
	ti := &schema.ToolInfo{
		Name: "test_tool",
		ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
			"x": {Type: "string", Desc: "the X field"},
		}),
	}
	got := toolInfoToJSONSchema(ti)
	m, ok := got.(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "object", m["type"])
	props, ok := m["properties"].(map[string]any)
	require.True(t, ok)
	assert.Contains(t, props, "x")
}

// TestAnthropicMapStopReason proves mapping for known and unknown stop reasons.
func TestAnthropicMapStopReason(t *testing.T) {
	assert.Equal(t, "stop", mapAnthropicStopReason("end_turn"))
	assert.Equal(t, "stop", mapAnthropicStopReason("stop_sequence"))
	assert.Equal(t, "length", mapAnthropicStopReason("max_tokens"))
	assert.Equal(t, "tool_calls", mapAnthropicStopReason("tool_use"))
	assert.Equal(t, "novel_reason", mapAnthropicStopReason("novel_reason"))
}

// ---------------------------------------------------------------------------
// FakeModel edge cases
// ---------------------------------------------------------------------------

// TestFakeModel_StreamRecordMessages proves Stream records messages when RecordMessages is set.
func TestFakeModel_StreamRecordMessages(t *testing.T) {
	m := NewFakeModel([]string{"resp"}, nil)
	m.RecordMessages = true
	sr, err := m.Stream(context.Background(), []*schema.Message{schema.UserMessage("hi")})
	require.NoError(t, err)
	sr.Close()
	require.Len(t, m.ReceivedMessages, 1)
	assert.Contains(t, m.ReceivedMessages[0].Content, "hi")
}

// TestFakeModel_EmptyResponsesFallback proves empty responses list returns empty assistant message.
func TestFakeModel_EmptyResponsesFallback(t *testing.T) {
	m := NewFakeModel(nil, nil)
	out, err := m.Generate(context.Background(), nil)
	require.NoError(t, err)
	assert.Equal(t, "", out.Content)
}

// TestFakeModel_StreamEmptyResponsesFallback proves Stream with no responses
// returns a stream that yields an empty message then EOF.
func TestFakeModel_StreamEmptyResponsesFallback(t *testing.T) {
	m := NewFakeModel(nil, nil)
	sr, err := m.Stream(context.Background(), nil)
	require.NoError(t, err)
	defer sr.Close()
	msg, err := sr.Recv()
	require.NoError(t, err)
	assert.Empty(t, msg.Content, "empty responses produce empty message")
	// Next Recv should be EOF
	_, err = sr.Recv()
	assert.ErrorIs(t, err, io.EOF)
}

// TestFakeModel_JudgeProbeInStream proves the judge probe short-circuit works on Stream.
func TestFakeModel_JudgeProbeInStream(t *testing.T) {
	m := NewFakeModel([]string{"should-not-consume"}, nil)
	m.RecordMessages = true
	sr, err := m.Stream(context.Background(), []*schema.Message{
		schema.UserMessage("You are a completion judge."),
	})
	require.NoError(t, err)
	defer sr.Close()
	msg, err := sr.Recv()
	require.NoError(t, err)
	assert.Contains(t, msg.Content, `"complete":true`)
}

// TestConcatMessageContents_NilHandling proves concatMessageContents skips nil messages.
func TestConcatMessageContents_NilHandling(t *testing.T) {
	result := concatMessageContents([]*schema.Message{nil, schema.UserMessage("hello"), nil, schema.UserMessage("world")})
	assert.Equal(t, "hello\nworld", result)
}

// ---------------------------------------------------------------------------
// CompactingModel edge cases
// ---------------------------------------------------------------------------

// TestCompactingModel_StreamDisabled proves Stream with no threshold is a pass-through.
func TestCompactingModel_StreamDisabled(t *testing.T) {
	inner := &recordingModel{reply: "ok", streamOK: true}
	cm := &CompactingModel{
		Inner:   inner,
		Threshold: 0, // disabled
	}
	sr, err := cm.Stream(context.Background(), []*schema.Message{schema.UserMessage("hi")})
	require.NoError(t, err)
	defer sr.Close()
	var content string
	for {
		msg, recvErr := sr.Recv()
		if errors.Is(recvErr, io.EOF) {
			break
		}
		require.NoError(t, recvErr)
		content += msg.Content
	}
	assert.Equal(t, "ok", content)
}

// TestCompactingModel_ShouldCompact_ContextWindowZero proves shouldCompact returns false
// when ContextWindow is 0.
func TestCompactingModel_ShouldCompact_ContextWindowZero(t *testing.T) {
	cm := &CompactingModel{
		Threshold:     0.8,
		ContextWindow: 0,
		KeepRecent:    2,
	}
	assert.False(t, cm.shouldCompact([]*schema.Message{bigMessage(100)}))
}

// TestCompactingModel_ShouldCompact_KeepRecentZero proves shouldCompact returns false
// when KeepRecent is 0.
func TestCompactingModel_ShouldCompact_KeepRecentZero(t *testing.T) {
	cm := &CompactingModel{
		Threshold:     0.8,
		ContextWindow: 1000,
		KeepRecent:    0,
	}
	assert.False(t, cm.shouldCompact([]*schema.Message{bigMessage(100)}))
}

// TestCompactingModel_InCooldown_TimeBased proves inCooldown respects CooldownDuration.
func TestCompactingModel_InCooldown_TimeBased(t *testing.T) {
	cm := &CompactingModel{
		CooldownDuration: time.Hour, // very long
		CooldownTokens:   0,         // disable token cooldown
	}
	cm.lastCompactAt = time.Now()
	cm.lastCompactTokens = 100

	tokens := 200
	assert.True(t, cm.inCooldown(tokens), "within time cooldown")
}

// TestCompactingModel_MaybeCompact_BestEffort proves when summarize fails, original msgs are returned.
func TestCompactingModel_MaybeCompact_BestEffort(t *testing.T) {
	inner := &recordingModel{reply: "ok", streamOK: true}
	cm := &CompactingModel{
		Inner:         inner,
		Threshold:     0.5,
		ContextWindow: 10, // tiny -> ctxcompact.Run will fail
		KeepRecent:    2,
	}
	msgs := []*schema.Message{bigMessage(30), bigMessage(30), bigMessage(30)}
	result, ok := cm.maybeCompact(context.Background(), msgs)
	assert.False(t, ok, "should not compact when summarization fails")
	assert.Equal(t, msgs, result, "original messages returned")
}

// ---------------------------------------------------------------------------
// ResilientChatModel edge cases
// ---------------------------------------------------------------------------

// alwaysEmptyModel always returns empty messages.
type alwaysEmptyModel struct {
	calls int32
}

func (m *alwaysEmptyModel) Generate(_ context.Context, _ []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	atomic.AddInt32(&m.calls, 1)
	return schema.AssistantMessage("", nil), nil
}

func (m *alwaysEmptyModel) Stream(_ context.Context, _ []*schema.Message, _ ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	atomic.AddInt32(&m.calls, 1)
	return schema.StreamReaderFromArray([]*schema.Message{schema.AssistantMessage("", nil)}), nil
}

var _ model.BaseChatModel = (*alwaysEmptyModel)(nil)

// TestResilientModel_GenerateUserCancel proves user cancel stops retry.
func TestResilientModel_GenerateUserCancel(t *testing.T) {
	m := &cancelCtxModel{}
	cfg := ResilientConfig{MaxRetries: 10, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond}
	r, err := NewResilientModel([]model.BaseChatModel{m}, cfg)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // user cancelled

	_, err = r.Generate(ctx, []*schema.Message{schema.UserMessage("hi")})
	require.Error(t, err)
	assert.Less(t, m.calls, 3, "must not retry after user cancel")
}

// TestResilientModel_StreamFailoverAllEmpty proves all providers exhausting the chain yields error.
func TestResilientModel_StreamFailoverAllEmpty(t *testing.T) {
	var calls int32
	e := &alwaysEmptyModel{calls: 0}
	_ = e
	f := &scriptedSeqModel{
		msgs:  []*schema.Message{schema.AssistantMessage("", nil)},
		calls: &calls,
	}
	cfg := ResilientConfig{MaxRetries: 0, MaxEmptyRetries: 2, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond}
	r, err := NewResilientModel([]model.BaseChatModel{f}, cfg)
	require.NoError(t, err)

	sr, err := r.Stream(context.Background(), []*schema.Message{schema.UserMessage("x")})
	require.NoError(t, err)
	defer sr.Close()

	_, recvErr := sr.Recv()
	require.Error(t, recvErr)
	assert.Contains(t, recvErr.Error(), "empty stream after 2 retries")
}

// TestIsRetryableStreamErr_NetError proves net.Error returns true.
func TestIsRetryableStreamErr_NetError(t *testing.T) {
	netErr := &netErrorStub{timeout: true}
	assert.True(t, isRetryableStreamErr(context.Background(), netErr))
}

// netErrorStub implements net.Error for testing.
type netErrorStub struct {
	timeout bool
}

func (e *netErrorStub) Error() string   { return "connection reset by peer" }
func (e *netErrorStub) Timeout() bool   { return e.timeout }
func (e *netErrorStub) Temporary() bool { return true }

// TestIsNonRetryableClientErr_Nil proves nil returns false.
func TestIsNonRetryableClientErr_Nil(t *testing.T) {
	assert.False(t, isNonRetryableClientErr(nil))
}

// TestResilientModel_RetryChainExhausted proves chain exhaustion returns error.
func TestResilientModel_RetryChainExhausted(t *testing.T) {
	e := &errModel{err: &RetryableModelError{Err: errors.New("always fail")}}
	r, err := NewResilientModel([]model.BaseChatModel{e}, ResilientConfig{MaxRetries: 0, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond})
	require.NoError(t, err)

	_, err = r.Generate(context.Background(), []*schema.Message{schema.UserMessage("x")})
	require.Error(t, err)
}

// TestBackoff_Overflow proves backoff caps at MaxDelay.
func TestBackoff_Overflow(t *testing.T) {
	r := &ResilientChatModel{cfg: ResilientConfig{BaseDelay: time.Second, MaxDelay: 2 * time.Second}}
	got := r.backoff(100) // 2^99 seconds would overflow
	assert.Equal(t, 2*time.Second, got, "backoff must cap at MaxDelay")
}

// TestIsBlank_WithUsageIsNotBlank proves a message with usage is not blank.
func TestIsBlank_WithUsageIsNotBlank(t *testing.T) {
	msg := &schema.Message{
		Role: schema.Assistant,
		ResponseMeta: &schema.ResponseMeta{
			Usage: &schema.TokenUsage{PromptTokens: 10},
		},
	}
	assert.False(t, isBlank(msg), "usage-carrying meta chunk is not blank")
}

// TestNewResilientModel_Defaults proves zero config gets sensible defaults.
func TestNewResilientModel_Defaults(t *testing.T) {
	r, err := NewResilientModel([]model.BaseChatModel{NewFakeModel(nil, nil)}, ResilientConfig{})
	require.NoError(t, err)
	assert.Equal(t, 10, r.cfg.MaxRetries)
	assert.Equal(t, 10, r.cfg.MaxEmptyRetries)
	assert.Equal(t, 200*time.Millisecond, r.cfg.BaseDelay)
	assert.Equal(t, 5*time.Second, r.cfg.MaxDelay)
}

// ---------------------------------------------------------------------------
// Provider edge cases
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// Responses edge cases
// ---------------------------------------------------------------------------

// TestResponsesStream_ScannerError proves readStream handles scanner errors.
func TestResponsesStream_ScannerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		// Send valid event then close abruptly
		_, _ = w.Write([]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"x\"}\n\n"))
	}))
	defer srv.Close()

	m, err := NewOpenAIResponsesModel(context.Background(), &ResponsesConfig{
		APIKey: "k", Model: "gpt-4o", BaseURL: srv.URL,
	})
	require.NoError(t, err)
	sr, err := m.Stream(context.Background(), []*schema.Message{schema.UserMessage("hi")})
	require.NoError(t, err)
	defer sr.Close()

	var content string
	for {
		chunk, recvErr := sr.Recv()
		if recvErr != nil {
			if recvErr == io.EOF {
				break
			}
			break
		}
		if chunk == nil {
			break
		}
		content += chunk.Content
	}
	assert.Contains(t, content, "x")
}

// TestResponsesBuildRequest_WithToolMsg proves tool messages are mapped.
func TestResponsesBuildRequest_WithToolMsg(t *testing.T) {
	m := &openaiResponsesModel{cfg: ResponsesConfig{APIKey: "k", Model: "gpt-4o", MaxOutputTokens: 100}}
	req, err := m.buildRequest([]*schema.Message{
		schema.UserMessage("hi"),
		{Role: schema.Tool, ToolCallID: "c1", ToolName: "fs_read", Content: "file content"},
	}, &model.Options{}, &outputSchemaOptions{}, false)
	require.NoError(t, err)
	require.Len(t, req.Input, 2)
	last := req.Input[1]
	assert.Equal(t, "function_call_output", last.Type)
	assert.Equal(t, "c1", last.CallID)
	assert.Equal(t, "file content", last.Output)
}

// TestResponsesBuildRequest_AssistantWithToolCall proves assistant with tool calls is mapped.
func TestResponsesBuildRequest_AssistantWithToolCall(t *testing.T) {
	m := &openaiResponsesModel{cfg: ResponsesConfig{APIKey: "k", Model: "gpt-4o", MaxOutputTokens: 100}}
	req, err := m.buildRequest([]*schema.Message{
		schema.AssistantMessage("thinking", []schema.ToolCall{
			{ID: "c1", Type: "function", Function: schema.FunctionCall{Name: "fs_read", Arguments: `{"path":"a.go"}`}},
		}),
	}, &model.Options{}, &outputSchemaOptions{}, false)
	require.NoError(t, err)
	require.Len(t, req.Input, 2)
	assert.Equal(t, "function_call", req.Input[1].Type)
}

// TestResponsesMapStatusToFinishReason covers mapping edge cases.
func TestResponsesMapStatusToFinishReason(t *testing.T) {
	assert.Equal(t, "stop", mapStatusToFinishReason("completed"))
	assert.Equal(t, "length", mapStatusToFinishReason("incomplete"))
	assert.Equal(t, "failed", mapStatusToFinishReason("failed"))
}

// TestResponsesResponseToMessage_Nil proves nil returns nil.
func TestResponsesResponseToMessage_Nil(t *testing.T) {
	m := &openaiResponsesModel{}
	msg := m.responseToMessage(nil)
	assert.Nil(t, msg)
}

// TestResponsesResponseToMessage_NoUsageNoStatus handles missing fields.
func TestResponsesResponseToMessage_NoUsageNoStatus(t *testing.T) {
	m := &openaiResponsesModel{}
	msg := m.responseToMessage(&responsesResponse{
		Output: []responsesOutputItem{
			{Type: "message", Content: []responsesOutputContent{{Type: "output_text", Text: "hello"}}},
		},
	})
	require.NotNil(t, msg)
	assert.Equal(t, "hello", msg.Content)
	assert.Nil(t, msg.ResponseMeta)
}

// ---------------------------------------------------------------------------
// Responses stream: error events
// ---------------------------------------------------------------------------

// TestResponsesReadStream_MalformedData proves malformed SSE data is skipped.
func TestResponsesReadStream_MalformedData(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: not-json\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	m, err := NewOpenAIResponsesModel(context.Background(), &ResponsesConfig{
		APIKey: "k", Model: "gpt-4o", BaseURL: srv.URL,
	})
	require.NoError(t, err)
	sr, err := m.Stream(context.Background(), []*schema.Message{schema.UserMessage("hi")})
	require.NoError(t, err)
	defer sr.Close()

	_, recvErr := sr.Recv()
	assert.ErrorIs(t, recvErr, io.EOF, "malformed data skipped, clean EOF")
}

// ---------------------------------------------------------------------------
// Helper used across tests
// ---------------------------------------------------------------------------
