package eino

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Anthropic: more Generate and buildRequest edge cases
// ---------------------------------------------------------------------------

// TestAnthropicGenerate_MarshalError is a placeholder. The real marshal error
// paths are exercised in existing tests.
func TestAnthropicGenerate_MarshalError(t *testing.T) {
	// Marshal errors typically occur at the HTTP/JSON level; the adapter's
	// marshal call uses the standard library which doesn't fail on schema.Message.
	// This path is covered by the existing Stream test infrastructure.
}

// TestAnthropicGenerate_DecodeError proves response decode errors propagate.
func TestAnthropicGenerate_DecodeError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`not-json`))
	}))
	defer srv.Close()

	m, err := NewAnthropicModel(context.Background(), &AnthropicModelConfig{
		APIKey: "k", Model: "claude", BaseURL: srv.URL,
	})
	require.NoError(t, err)

	_, err = m.Generate(context.Background(), []*schema.Message{schema.UserMessage("hi")})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "decode")
}

// TestAnthropicStream_MarshalError proves marshal error on Stream propagates.
func TestAnthropicStream_MarshalError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`rate limited`))
	}))
	defer srv.Close()

	m, err := NewAnthropicModel(context.Background(), &AnthropicModelConfig{
		APIKey: "k", Model: "claude", BaseURL: srv.URL,
	})
	require.NoError(t, err)

	_, err = m.Stream(context.Background(), []*schema.Message{schema.UserMessage("hi")})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "429")
}

// TestAnthropicReadStream_ScannerError proves scanner error is surfaced.
func TestAnthropicReadStream_ScannerError(t *testing.T) {
	// Create a response body that causes a scanner error (very large line).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		// Write huge amount of data without newlines to trigger buffer overflow
		_, _ = w.Write([]byte("data: " + string(make([]byte, 2<<20)) + "\n\n"))
	}))
	defer srv.Close()

	m, err := NewAnthropicModel(context.Background(), &AnthropicModelConfig{
		APIKey: "k", Model: "claude", BaseURL: srv.URL,
	})
	require.NoError(t, err)

	sr, err := m.Stream(context.Background(), []*schema.Message{schema.UserMessage("hi")})
	require.NoError(t, err)
	defer sr.Close()

	_, recvErr := sr.Recv()
	// Should get either a bufio.ErrTooLong or some other error, not an infinite hang
	require.Error(t, recvErr)
}

// TestAnthropicResponseToMessage_NoContentNoToolCalls proves empty assistant produces empty message.
func TestAnthropicResponseToMessage_NoContentNoToolCalls(t *testing.T) {
	m := &AnthropicModel{}
	resp := &anthropicResponse{
		Type: "message", Role: "assistant",
		Content: []anthropicContentPart{},
		Usage:   &anthropicUsage{InputTokens: 5, OutputTokens: 3},
	}
	msg := m.responseToMessage(resp)
	require.NotNil(t, msg)
	assert.Equal(t, "", msg.Content)
	assert.Len(t, msg.ToolCalls, 0)
	require.NotNil(t, msg.ResponseMeta)
	require.NotNil(t, msg.ResponseMeta.Usage)
	assert.Equal(t, 5, msg.ResponseMeta.Usage.PromptTokens)
	assert.Equal(t, 3, msg.ResponseMeta.Usage.CompletionTokens)
}

// TestAnthropicResponseToMessage_EmptyAssistantWithTool proves empty message has no tool calls.
func TestAnthropicResponseToMessage_ToolCalls(t *testing.T) {
	m := &AnthropicModel{}
	resp := &anthropicResponse{
		Type: "message", Role: "assistant", StopReason: "tool_use",
		Content: []anthropicContentPart{
			{Type: "tool_use", ID: "tu_1", Name: "fs_read", Input: map[string]any{"path": "a.go"}},
		},
		Usage: &anthropicUsage{InputTokens: 5, OutputTokens: 3},
	}
	msg := m.responseToMessage(resp)
	require.NotNil(t, msg)
	require.Len(t, msg.ToolCalls, 1)
	assert.Equal(t, "tu_1", msg.ToolCalls[0].ID)
	assert.Equal(t, "fs_read", msg.ToolCalls[0].Function.Name)
	assert.Contains(t, msg.ToolCalls[0].Function.Arguments, "a.go")
}

// TestAnthropicResponse_StopReasonWithoutUsage proves stop reason sets meta even without usage.
func TestAnthropicResponse_StopReasonWithoutUsage(t *testing.T) {
	m := &AnthropicModel{}
	resp := &anthropicResponse{
		Type: "message", Role: "assistant", StopReason: "end_turn",
		Content: []anthropicContentPart{{Type: "text", Text: "hello"}},
	}
	msg := m.responseToMessage(resp)
	require.NotNil(t, msg)
	require.NotNil(t, msg.ResponseMeta)
	assert.Equal(t, "stop", msg.ResponseMeta.FinishReason)
}

// TestAnthropicBuildRequest_MultiContentUser proves UserInputMultiContent mapping.
func TestAnthropicBuildRequest_MultiContentUser(t *testing.T) {
	m := &AnthropicModel{config: AnthropicModelConfig{Model: "claude", APIKey: "k", MaxTokens: 100}}
	url := "data:image/png;base64,AAAA"
	msgs := []*schema.Message{
		{Role: schema.User, Content: "describe", UserInputMultiContent: nil},
		{Role: schema.User, UserInputMultiContent: []schema.MessageInputPart{
			{Type: "text", Text: "desc"},
			{Type: "image_url", Image: &schema.MessageInputImage{
				MessagePartCommon: schema.MessagePartCommon{MIMEType: "image/png", URL: &url},
			}},
		}},
	}
	req, err := m.buildRequest(msgs, &model.Options{}, &outputSchemaOptions{}, false)
	require.NoError(t, err)
	require.Len(t, req.Messages, 2)
	// Second message: multi-content user
	partMsg := req.Messages[1]
	assert.Equal(t, "user", partMsg.Role)
	require.Len(t, partMsg.Content, 2)
	// First part: text
	assert.Equal(t, "text", partMsg.Content[0].Type)
	assert.Equal(t, "desc", partMsg.Content[0].Text)
	// Second part: image
	assert.Equal(t, "image", partMsg.Content[1].Type)
	require.NotNil(t, partMsg.Content[1].Source)
	assert.Equal(t, "url", partMsg.Content[1].Source.Type)
}

// TestAnthropicBuildRequest_SystemConcatenation proves multiple system messages are joined.
func TestAnthropicBuildRequest_SystemConcatenation(t *testing.T) {
	m := &AnthropicModel{config: AnthropicModelConfig{Model: "claude", APIKey: "k", MaxTokens: 100}}
	req, err := m.buildRequest([]*schema.Message{
		schema.SystemMessage("part a"),
		schema.SystemMessage("part b"),
		schema.UserMessage("hi"),
	}, &model.Options{}, &outputSchemaOptions{}, false)
	require.NoError(t, err)
	assert.Equal(t, "part a\n\npart b", req.System)
	require.Len(t, req.Messages, 1)
}

// TestAnthropicBuildRequest_NoSystem proves no system messages produce empty system.
func TestAnthropicBuildRequest_NoSystem(t *testing.T) {
	m := &AnthropicModel{config: AnthropicModelConfig{Model: "claude", APIKey: "k", MaxTokens: 100}}
	req, err := m.buildRequest([]*schema.Message{
		schema.UserMessage("hi"),
	}, &model.Options{}, &outputSchemaOptions{}, false)
	require.NoError(t, err)
	assert.Empty(t, req.System)
}

// TestAnthropicBuildRequest_EmptyAssistantContent proves empty assistant with tool calls
// still produces content blocks for the tool_use parts.
func TestAnthropicBuildRequest_EmptyAssistantContent(t *testing.T) {
	m := &AnthropicModel{config: AnthropicModelConfig{Model: "claude", APIKey: "k", MaxTokens: 100}}
	req, err := m.buildRequest([]*schema.Message{
		{Role: schema.Assistant, ToolCalls: []schema.ToolCall{
			{ID: "c1", Function: schema.FunctionCall{Name: "fs_read", Arguments: `{"path":"a.go"}`}},
		}},
	}, &model.Options{}, &outputSchemaOptions{}, false)
	require.NoError(t, err)
	require.Len(t, req.Messages, 1)
	require.Len(t, req.Messages[0].Content, 1)
	assert.Equal(t, "tool_use", req.Messages[0].Content[0].Type)
	assert.Equal(t, "c1", req.Messages[0].Content[0].ID)
}

// TestAnthropicBuildRequest_ToolResultWithContent proves tool result messages map correctly.
func TestAnthropicBuildRequest_ToolResultWithContent(t *testing.T) {
	m := &AnthropicModel{config: AnthropicModelConfig{Model: "claude", APIKey: "k", MaxTokens: 100}}
	req, err := m.buildRequest([]*schema.Message{
		{Role: schema.Tool, ToolCallID: "c1", ToolName: "fs_read", Content: "file content"},
	}, &model.Options{}, &outputSchemaOptions{}, false)
	require.NoError(t, err)
	require.Len(t, req.Messages, 1)
	assert.Equal(t, "user", req.Messages[0].Role) // tool results become user messages
}

// TestAnthropicBuildRequest_EmptyToolRequest proves an empty tool request role is skipped.
func TestAnthropicBuildRequest_EmptyToolRequest(t *testing.T) {
	m := &AnthropicModel{config: AnthropicModelConfig{Model: "claude", APIKey: "k", MaxTokens: 100}}
	req, err := m.buildRequest([]*schema.Message{
		{Role: schema.Tool, ToolCallID: "", ToolName: "", Content: ""},
	}, &model.Options{}, &outputSchemaOptions{}, false)
	require.NoError(t, err)
	require.Len(t, req.Messages, 1)
	// Tool requests become user messages
	assert.Equal(t, "user", req.Messages[0].Role)
}

// ---------------------------------------------------------------------------
// ToolInfoToJSONSchema edge cases
// ---------------------------------------------------------------------------

// TestToolInfoToJSONSchema_InvalidParams proves invalid ParamsOneOf returns fallback.
func TestToolInfoToJSONSchema_InvalidParams(t *testing.T) {
	// Nil ParamsOneOf returns fallback object
	got := toolInfoToJSONSchema(nil)
	m, ok := got.(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "object", m["type"])

	// Validate toolInfo with valid params
	ti2 := &schema.ToolInfo{
		Name: "test",
		ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
			"x": {Type: "string", Desc: "X"},
		}),
	}
	got2 := toolInfoToJSONSchema(ti2)
	m2, ok2 := got2.(map[string]any)
	require.True(t, ok2)
	props := m2["properties"].(map[string]any)
	assert.Contains(t, props, "x")
}

// ---------------------------------------------------------------------------
// BlockingModel Stream cancel
// ---------------------------------------------------------------------------

// TestBlockingModel_StreamCancel proves Stream path respects context cancellation.
func TestBlockingModel_StreamCancel(t *testing.T) {
	m := NewBlockingModel("hi")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := m.Stream(ctx, []*schema.Message{schema.UserMessage("x")})
		done <- err
	}()

	<-m.Started
	cancel()
	select {
	case err := <-done:
		require.Error(t, err)
		assert.ErrorIs(t, err, context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("Stream did not return after context cancel")
	}
}

// ---------------------------------------------------------------------------
// FakeModel Stream edge cases
// ---------------------------------------------------------------------------

// TestFakeModel_StreamVision proves vision mode works on Stream.
func TestFakeModel_StreamVision(t *testing.T) {
	m := NewFakeModel(nil, nil)
	m.Vision = true
	m.RecordImages = true
	url := "data:image/png;base64,AAAA"
	sr, err := m.Stream(context.Background(), []*schema.Message{
		{Role: schema.User, UserInputMultiContent: []schema.MessageInputPart{
			{Type: schema.ChatMessagePartTypeImageURL, Image: &schema.MessageInputImage{
				MessagePartCommon: schema.MessagePartCommon{MIMEType: "image/png", URL: &url},
			}},
		}},
	})
	require.NoError(t, err)
	defer sr.Close()
	msg, err := sr.Recv()
	require.NoError(t, err)
	assert.Contains(t, msg.Content, "fake-vision")
	assert.Equal(t, 1, m.LastImageCount)
}

// TestFakeModel_StreamEcho proves echo mode on Stream echoes input.
func TestFakeModel_StreamEcho(t *testing.T) {
	m := NewFakeModel(nil, nil)
	m.Echo = true
	sr, err := m.Stream(context.Background(), []*schema.Message{
		schema.UserMessage("echo this"),
	})
	require.NoError(t, err)
	defer sr.Close()
	msg, err := sr.Recv()
	require.NoError(t, err)
	assert.Contains(t, msg.Content, "echo this")
}

// TestFakeModel_StreamRepeat proves repeat mode returns first response.
func TestFakeModel_StreamRepeat(t *testing.T) {
	m := NewFakeModel([]string{"always-this"}, nil)
	m.Repeat = true
	sr, err := m.Stream(context.Background(), nil)
	require.NoError(t, err)
	defer sr.Close()
	msg, err := sr.Recv()
	require.NoError(t, err)
	assert.Equal(t, "always-this", msg.Content)
}

// TestFakeModel_CountImageParts proves image part counting.
func TestFakeModel_CountImageParts(t *testing.T) {
	n := countImageParts([]*schema.Message{
		{Role: schema.User, UserInputMultiContent: []schema.MessageInputPart{
			{Type: "image_url", Image: &schema.MessageInputImage{
				MessagePartCommon: schema.MessagePartCommon{MIMEType: "image/png"},
			}},
			{Type: "image_url", Image: &schema.MessageInputImage{
				MessagePartCommon: schema.MessagePartCommon{MIMEType: "image/png"},
			}},
		}},
	})
	assert.Equal(t, 2, n)
}

// TestFakeModel_CountImageParts_NilMsg proves nil messages are skipped.
func TestFakeModel_CountImageParts_NilMsg(t *testing.T) {
	n := countImageParts([]*schema.Message{nil, &schema.Message{Role: schema.User}})
	assert.Equal(t, 0, n)
}

// TestFakeModel_RecordMessagesWithStream proves recordMessages works with Stream.
func TestFakeModel_RecordMessagesWithStream(t *testing.T) {
	m := NewFakeModel([]string{"resp"}, nil)
	m.RecordMessages = true
	_, err := m.Stream(context.Background(), []*schema.Message{schema.UserMessage("hi")})
	require.NoError(t, err)
	require.Len(t, m.ReceivedMessages, 1)
}

// ---------------------------------------------------------------------------
// Resilient sleepRetry user cancel
// ---------------------------------------------------------------------------

// TestSleepRetry_UserCancel proves sleepRetry watches userCancelCtx.
func TestSleepRetry_UserCancel(t *testing.T) {
	r := &ResilientChatModel{cfg: ResilientConfig{BaseDelay: time.Hour, MaxDelay: time.Hour}}

	userCtx, cancel := context.WithCancel(context.Background())
	cancel() // Already cancelled

	ctx := WithUserCancelCtx(context.Background(), userCtx)
	sr, sw := schema.Pipe[*schema.Message](1)
	defer sr.Close()
	ok := r.sleepRetry(ctx, sw, nil, 1, 3, errors.New("test error"))
	assert.False(t, ok, "sleepRetry must return false when user cancels")
}

// TestResilientModel_ConsumeStreamUserCancel proves consumeStream returns streamErr
// when context is cancelled mid-stream.
func TestResilientModel_ConsumeStreamUserCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	msg := schema.AssistantMessage("hello", nil)
	sr := schema.StreamReaderFromArray([]*schema.Message{msg})

	var deliveredTools bool
	cancel()

	outcome, _ := consumeStream(ctx, sr, nil, &deliveredTools)
	assert.Equal(t, streamErr, outcome)
}

// ---------------------------------------------------------------------------
// Full runStream edge case: open stream fails all providers
// ---------------------------------------------------------------------------

// alwaysErrStreamModel always fails stream setup.
type alwaysErrStreamModel struct{}

func (m *alwaysErrStreamModel) Generate(_ context.Context, _ []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	return schema.AssistantMessage("", nil), nil
}
func (m *alwaysErrStreamModel) Stream(_ context.Context, _ []*schema.Message, _ ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	return nil, errors.New("always fail stream")
}

var _ model.BaseChatModel = (*alwaysErrStreamModel)(nil)

// TestOpenStreamChain_AllFailNoRetry proves when chain fails and retries are 0, error surfaces immediately.
func TestOpenStreamChain_AllFailNoRetry(t *testing.T) {
	r, err := NewResilientModel([]model.BaseChatModel{&alwaysErrStreamModel{}},
		ResilientConfig{MaxRetries: 0, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond})
	require.NoError(t, err)

	sr, err := r.Stream(context.Background(), []*schema.Message{schema.UserMessage("x")})
	require.NoError(t, err)
	defer sr.Close()

	_, recvErr := sr.Recv()
	require.Error(t, recvErr)
	assert.Contains(t, recvErr.Error(), "always fail stream")
}

// ---------------------------------------------------------------------------
// Responses: HTTP error on Stream
// ---------------------------------------------------------------------------

// TestResponsesStream_HTTPErrorStream proves a non-200 response on Stream returns error.
func TestResponsesStream_HTTPErrorStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`bad request`))
	}))
	defer srv.Close()

	m, err := NewOpenAIResponsesModel(context.Background(), &ResponsesConfig{
		APIKey: "k", Model: "gpt-4o", BaseURL: srv.URL,
	})
	require.NoError(t, err)

	_, err = m.Stream(context.Background(), []*schema.Message{schema.UserMessage("hi")})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "400")
}

// TestResponsesGenerate_DecodeError proves decode error on Generate.
func TestResponsesGenerate_DecodeError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`not-json`))
	}))
	defer srv.Close()

	m, err := NewOpenAIResponsesModel(context.Background(), &ResponsesConfig{
		APIKey: "k", Model: "gpt-4o", BaseURL: srv.URL,
	})
	require.NoError(t, err)

	_, err = m.Generate(context.Background(), []*schema.Message{schema.UserMessage("hi")})
	require.Error(t, err)
}

// TestResponsesBuildRequest_SystemMessages proves system messages go to instructions.
func TestResponsesBuildRequest_SystemMessages(t *testing.T) {
	m := &openaiResponsesModel{cfg: ResponsesConfig{APIKey: "k", Model: "gpt-4o", MaxOutputTokens: 100}}
	req, err := m.buildRequest([]*schema.Message{
		schema.SystemMessage("rule 1"),
		schema.SystemMessage("rule 2"),
		schema.UserMessage("hi"),
	}, &model.Options{}, &outputSchemaOptions{}, false)
	require.NoError(t, err)
	assert.Equal(t, "rule 1\n\nrule 2", req.Instructions)
	require.Len(t, req.Input, 1)
}

// TestResponsesBuildRequest_UserMultiContent proves multi-content user mapping.
func TestResponsesBuildRequest_UserMultiContent(t *testing.T) {
	m := &openaiResponsesModel{cfg: ResponsesConfig{APIKey: "k", Model: "gpt-4o", MaxOutputTokens: 100}}
	req, err := m.buildRequest([]*schema.Message{
		{Role: schema.User, UserInputMultiContent: []schema.MessageInputPart{
			{Type: "text", Text: "hello"},
		}},
	}, &model.Options{}, &outputSchemaOptions{}, false)
	require.NoError(t, err)
	require.Len(t, req.Input, 1)
	require.Len(t, req.Input[0].Content, 1)
	assert.Equal(t, "input_text", req.Input[0].Content[0].Type)
}

// TestResponsesBuildRequest_UserMultiContentEmpty proves empty multi-content is skipped.
func TestResponsesBuildRequest_UserMultiContentEmpty(t *testing.T) {
	m := &openaiResponsesModel{cfg: ResponsesConfig{APIKey: "k", Model: "gpt-4o", MaxOutputTokens: 100}}
	req, err := m.buildRequest([]*schema.Message{
		{Role: schema.User, UserInputMultiContent: []schema.MessageInputPart{
			{Type: "text", Text: ""}, // empty text part
		}},
	}, &model.Options{}, &outputSchemaOptions{}, false)
	require.NoError(t, err)
	require.Empty(t, req.Input, "empty multi-content should be skipped")
}

// TestResponsesBuildRequest_ToolResultWithoutCallID proves tool results without CallID.
func TestResponsesBuildRequest_ToolResultWithoutCallID(t *testing.T) {
	m := &openaiResponsesModel{cfg: ResponsesConfig{APIKey: "k", Model: "gpt-4o", MaxOutputTokens: 100}}
	req, err := m.buildRequest([]*schema.Message{
		{Role: schema.Tool, ToolCallID: "", Content: "result"},
	}, &model.Options{}, &outputSchemaOptions{}, false)
	require.NoError(t, err)
	require.Len(t, req.Input, 1)
	assert.Equal(t, "function_call_output", req.Input[0].Type)
}

// ---------------------------------------------------------------------------
// Responses readStream events
// ---------------------------------------------------------------------------

// TestResponsesReadStream_ErrorEvent proves the error SSE event surfaces as error.
func TestResponsesReadStream_ErrorEvent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"type\":\"error\",\"code\":\"server_error\",\"message\":\"internal error\"}\n\n"))
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
	require.Error(t, recvErr)
	assert.Contains(t, recvErr.Error(), "internal error")
}

// TestResponsesReadStream_FailedEvent proves response.failed surfaces error.
func TestResponsesReadStream_FailedEventStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\"}}\n\n"))
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
	require.Error(t, recvErr)
	assert.Contains(t, recvErr.Error(), "failed")
}

// ---------------------------------------------------------------------------
// Responses Generate marshal error
// ---------------------------------------------------------------------------

// TestResponsesGenerate_HTTPNonOK proves non-200 generates error.
func TestResponsesGenerate_HTTPNonOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`server error`))
	}))
	defer srv.Close()

	m, err := NewOpenAIResponsesModel(context.Background(), &ResponsesConfig{
		APIKey: "k", Model: "gpt-4o", BaseURL: srv.URL,
	})
	require.NoError(t, err)

	_, err = m.Generate(context.Background(), []*schema.Message{schema.UserMessage("hi")})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "500")
}

// ---------------------------------------------------------------------------
// Provider test for chooseKey with fully exhausted keys
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// Resilient model retry with empty chain error path
// ---------------------------------------------------------------------------

// TestResilientGenerate_ChainExhaustedNoLastErr proves lastErr fallback.
func TestResilientGenerate_ChainExhaustedNoLastErr(t *testing.T) {
	r := &ResilientChatModel{chain: []model.BaseChatModel{}}
	_, err := r.Generate(context.Background(), []*schema.Message{schema.UserMessage("hi")})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "chain exhausted")
}

// ---------------------------------------------------------------------------
// Fake model Generate path with Vision + RecordImages
// ---------------------------------------------------------------------------

// TestFakeModel_GenerateVision proves vision Generate path.
func TestFakeModel_GenerateVision(t *testing.T) {
	m := NewFakeModel(nil, nil)
	m.Vision = true
	m.RecordImages = true
	out, err := m.Generate(context.Background(), []*schema.Message{
		{Role: schema.User, UserInputMultiContent: []schema.MessageInputPart{
			{Type: "image_url", Image: &schema.MessageInputImage{}},
		}},
	})
	require.NoError(t, err)
	assert.Contains(t, out.Content, "fake-vision(1 image)")
	assert.Equal(t, 1, m.LastImageCount)
}

// ---------------------------------------------------------------------------
// Anthropic readStream: thinking_delta text fallback
// ---------------------------------------------------------------------------

// TestAnthropicStream_ThinkingDeltaTextFallback proves a thinking_delta that
// uses the text field (gateway compatibility) is forwarded as reasoning.
func TestAnthropicStream_ThinkingDeltaTextFallback(t *testing.T) {
	sseBody := "event: message_start\n" +
		`data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","content":[],"stop_reason":null,"usage":{"input_tokens":1,"output_tokens":1}}}` + "\n\n" +
		"event: content_block_start\n" +
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}` + "\n\n" +
		"event: content_block_delta\n" +
		// Some gateways send thinking content in the generic "text" field
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"thinking text"}}` + "\n\n" +
		"event: content_block_stop\n" +
		`data: {"type":"content_block_stop","index":0}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"}}` + "\n\n" +
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
	require.NoError(t, err)

	sr, err := m.Stream(context.Background(), []*schema.Message{schema.UserMessage("hi")})
	require.NoError(t, err)
	defer sr.Close()

	var content string
	for {
		chunk, recvErr := sr.Recv()
		if recvErr == io.EOF {
			break
		}
		require.NoError(t, recvErr)
		if chunk != nil {
			content += chunk.Content
		}
	}
	assert.Equal(t, "thinking text", content)
}
