// Package eino provides LLM model adapters for yanshi.
//
// This file implements an OpenAI Responses API adapter (POST /v1/responses),
// wrapping it as model.BaseChatModel so it is a drop-in replacement for the
// OpenAI Chat Completions and Anthropic Messages adapters in the
// orchestrator's ReAct loop.
//
// The OpenAI Responses API is the unified successor to Chat Completions.
// Compared to Chat Completions, the key wire differences this adapter
// translates are:
//
//   - Request endpoint is "/v1/responses" (not "/v1/chat/completions").
//   - The input message list is the "input" field (not "messages"), and each
//     item has shape {type:"message", role, content:[{type:"input_text",text}]}
//     rather than Chat Completions' flat {role, content:string}.
//   - System messages are NOT passed in input; they go in the top-level
//     "instructions" string field (mirroring how anthropic.go routes system
//     text into its top-level "system" field).
//   - Response.output[] items have type "message" with content[] items of
//     type "output_text" (rather than a flat "choices[].message.content"
//     string).
//   - usage reports {input_tokens, output_tokens, total_tokens} (matches the
//     Anthropic naming, not Chat Completions' "prompt_tokens" — the
//     responseToMessage helper translates both field names into
//     schema.TokenUsage{PromptTokens, CompletionTokens}).
//   - Response.status is "completed" / "incomplete" / "failed" rather than a
//     finish_reason string; we map "completed"→"stop" and "incomplete"→
//     "length" so downstream code (resilient retry, TUI finish indicators)
//     keeps seeing the OpenAI finish_reason vocabulary.
//
// Streaming uses Server-Sent Events. The Responses API emits a sequence of
// events; the ones we care about are:
//
//   - "response.output_text.delta"  → append ev.Delta to the running text and
//     forward as a chunk immediately (token-by-token TUI output).
//   - "response.completed"          → terminal; carries final usage + status.
//   - "response.incomplete"         → terminal; status is "incomplete" → map
//     to FinishReason="length".
//   - "response.failed" / "error"   → terminal; surface as a stream error so
//     the resilient layer can retry.
//
// The structure of this file intentionally mirrors anthropic.go (config
// struct, constructor, Generate/Stream, buildRequest/responseToMessage/
// readStream, setHeaders) so future readers can diff the two adapters
// field-by-field. A future refactor could extract the shared HTTP+SSE
// scaffolding; for now the duplication is intentional per the A5 task brief
// (correctness first, DRY follow-up).
package eino

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

// ResponsesConfig configures an openaiResponsesModel.
type ResponsesConfig struct {
	// APIKey is sent as `Authorization: Bearer <key>`.
	APIKey string
	// Model is the OpenAI model id, e.g. "gpt-4o", "gpt-4o-mini".
	Model string
	// BaseURL overrides the default API endpoint. Default: "https://api.openai.com".
	// A trailing "/v1" is added automatically; set e.g. "https://api.openai.com/v1"
	// or via a proxy that speaks the Responses wire format. Trailing slashes are
	// trimmed.
	BaseURL string
	// MaxOutputTokens overrides the default max_output_tokens. Default: 4096.
	// (The Responses API uses "max_output_tokens" rather than "max_tokens".)
	MaxOutputTokens int
	// Temperature is the sampling temperature (M4). nil omits the field from
	// the request body entirely, leaving the provider default in force — the
	// pointer is what distinguishes "unset" from a deliberate 0.
	Temperature *float32
	// TopP is nucleus sampling mass (M4). nil omits the field; see Temperature
	// for why this is a pointer.
	TopP *float32
	// HTTPClient is the HTTP client used for API calls. If nil, a default
	// client built on http.DefaultTransport is used (no timeout, no retry —
	// ResilientChatModel is the single retry authority; see provider.go).
	HTTPClient *http.Client
	// Headers (W-C-02) are extra HTTP headers applied to every request AFTER
	// the built-ins (Content-Type/Authorization) in setHeaders, so an
	// operator entry can override a built-in name (e.g. routing through a
	// proxy that wants its own Authorization scheme) as well as add new ones
	// (an enterprise gateway token). See ProviderConfig.Headers for the
	// SECURITY note — these values must reach the redactor before this
	// struct is constructed; this type does not register them itself.
	Headers map[string]string
}

// openaiResponsesModel implements model.BaseChatModel via the OpenAI
// Responses API. See the file doc comment for the wire-format translation.
type openaiResponsesModel struct {
	cfg    ResponsesConfig
	client *http.Client
}

// compile-time interface check.
var _ model.BaseChatModel = (*openaiResponsesModel)(nil)

// NewOpenAIResponsesModel builds an openaiResponsesModel from cfg.
// Returns an error if APIKey or Model is empty so a misconfigured provider
// fails fast at startup rather than on the first request.
func NewOpenAIResponsesModel(_ context.Context, cfg *ResponsesConfig) (*openaiResponsesModel, error) {
	if cfg.APIKey == "" {
		return nil, fmt.Errorf("eino/responses: APIKey is required")
	}
	if cfg.Model == "" {
		return nil, fmt.Errorf("eino/responses: Model is required")
	}
	base := cfg.BaseURL
	if base == "" {
		base = "https://api.openai.com"
	}
	base = strings.TrimRight(base, "/")
	// Some users configure BaseURL with a trailing "/v1" already; respect it
	// rather than producing "/v1/v1". This mirrors anthropic.go's behavior.
	if !strings.HasSuffix(base, "/v1") {
		base += "/v1"
	}
	maxTokens := cfg.MaxOutputTokens
	if maxTokens <= 0 {
		maxTokens = 4096
	}
	hc := cfg.HTTPClient
	if hc == nil {
		// No retry, no Timeout: ResilientChatModel is the single retry
		// authority (see provider.go BuildProviders for the rationale).
		hc = &http.Client{Transport: http.DefaultTransport}
	}
	return &openaiResponsesModel{
		cfg: ResponsesConfig{
			APIKey:          cfg.APIKey,
			Model:           cfg.Model,
			BaseURL:         base,
			MaxOutputTokens: maxTokens,
			Temperature:     cfg.Temperature,
			TopP:            cfg.TopP,
			HTTPClient:      hc,
			Headers:         cfg.Headers,
		},
		client: hc,
	}, nil
}

// ---------------------------------------------------------------------------
// Responses API request / response wire types
// ---------------------------------------------------------------------------

// responsesInputItem is one entry in the request "input" array. The "message"
// type carries a role + content[] (the common case); the "function_call" and
// "function_call_output" types let us replay assistant tool calls and tool
// results back to the model (analogous to anthropic.go's tool_use/tool_result
// content blocks, just at the item level instead of nested in content).
type responsesInputItem struct {
	Type string `json:"type"` // "message" | "function_call" | "function_call_output"

	// For type="message":
	Role    string                 `json:"role,omitempty"` // "user" | "assistant" | "system" | "developer"
	Content []responsesContentPart `json:"content,omitempty"`

	// For type="function_call" (assistant tool call):
	CallID    string `json:"call_id,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`

	// For type="function_call_output" (tool result):
	Output string `json:"output,omitempty"`
}

// responsesContentPart is one entry in a message item's content[] array.
// For the request direction, the relevant types are "input_text" (user/
// system/developer input) and "output_text" (assistant output replayed back
// as conversation history).
type responsesContentPart struct {
	Type string `json:"type"` // "input_text" | "output_text" | "input_image"
	Text string `json:"text,omitempty"`
}

// responsesTool declares a function tool to the Responses API.
type responsesTool struct {
	Type        string `json:"type"` // "function"
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Parameters  any    `json:"parameters,omitempty"` // JSON Schema
}

// responsesTextConfig maps onto the OpenAI Responses API top-level text field
// for structured outputs ({"format":{"type":"json_schema","name":...,
// "schema":{...},"strict":true}}). Pointer-optional so omitempty drops it on
// text-mode turns.
type responsesTextConfig struct {
	Format responsesTextFormat `json:"format"`
}

// responsesTextFormat is the text.format payload. Name is required by the
// Responses API (arbitrary identifier); Strict enforces the schema.
type responsesTextFormat struct {
	Type   string          `json:"type"`   // "json_schema"
	Name   string          `json:"name"`   // required by OpenAI Responses
	Schema json.RawMessage `json:"schema"` // the JSON Schema document
	Strict bool            `json:"strict"`
}

// responsesRequest is the POST /v1/responses body.
type responsesRequest struct {
	Model           string               `json:"model"`
	Input           []responsesInputItem `json:"input"`
	Instructions    string               `json:"instructions,omitempty"`
	Stream          bool                 `json:"stream"`
	MaxOutputTokens int                  `json:"max_output_tokens,omitempty"`
	Tools           []responsesTool      `json:"tools,omitempty"`

	// Temperature / TopP are the M4 generation parameters. Both are pointers
	// with omitempty so an unconfigured provider sends a body byte-identical
	// to the pre-M4 one, while an explicit 0 (deterministic sampling) still
	// reaches the wire — the exact case a value type would have erased.
	Temperature *float32 `json:"temperature,omitempty"`
	TopP        *float32 `json:"top_p,omitempty"`

	// Text, when non-nil, requests provider-enforced structured output for this
	// turn (Responses API text.format json_schema). nil on text-mode turns so
	// the field is omitted from the wire body entirely.
	Text *responsesTextConfig `json:"text,omitempty"`
}

// responsesResponse is the non-streaming response body.
type responsesResponse struct {
	ID     string                `json:"id"`
	Object string                `json:"object"` // "response"
	Status string                `json:"status"` // "completed" | "incomplete" | "failed"
	Output []responsesOutputItem `json:"output"`
	Usage  *responsesUsage       `json:"usage,omitempty"`
}

// responsesOutputItem is one entry in response.output[]. We care about
// type="message" (text content) and type="function_call" (tool calls).
type responsesOutputItem struct {
	Type    string                   `json:"type"`
	Role    string                   `json:"role,omitempty"`
	Status  string                   `json:"status,omitempty"`
	ID      string                   `json:"id,omitempty"`
	Content []responsesOutputContent `json:"content,omitempty"`

	// For type="function_call":
	CallID    string `json:"call_id,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

// responsesOutputContent is one entry in an output message's content[].
// type="output_text" carries the model's text. (Other types like
// "refusal" / "annotation" are tolerated but not surfaced as content.)
type responsesOutputContent struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

type responsesUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	TotalTokens  int `json:"total_tokens"`

	// OutputTokensDetails carries the per-role breakdown of output tokens.
	// For Responses API reasoning models (o1 / o3 family), reasoning_tokens
	// often dominates the cost; surfacing it lets the TUI footer ("think:"
	// segment) and /cost breakdown show the real bill instead of collapsing
	// reasoning into the generic output_tokens total.
	OutputTokensDetails *responsesOutputTokensDetails `json:"output_tokens_details,omitempty"`
}

// responsesOutputTokensDetails mirrors the OpenAI Responses API
// output_tokens_details object. ReasoningTokens is the only field defined by
// the spec today; future fields can be added here without breaking older
// payloads (the field is pointer-optional so a missing object decodes as nil).
type responsesOutputTokensDetails struct {
	ReasoningTokens int `json:"reasoning_tokens"`
}

// ---------------------------------------------------------------------------
// SSE event types for streaming
// ---------------------------------------------------------------------------

// responsesStreamEvent is the JSON payload of one `data:` line. Different
// events populate different fields; the union is small enough that one struct
// is clearer than a sum-type with per-event structs.
//
// Known event types we consume:
//   - response.output_text.delta : {delta:"text chunk"}
//   - response.completed         : {response:{status,usage,...}}
//   - response.incomplete        : {response:{status:"incomplete",usage,...}}
//   - response.failed            : {response:{status:"failed"}}
//   - error                      : {code,message}
//
// Other events (response.created, response.in_progress,
// response.output_item.added/done, response.content_part.added/done,
// response.output_text.done) are safely ignored — they describe protocol
// transitions we don't need for text streaming.
type responsesStreamEvent struct {
	Type     string             `json:"type"`
	Response *responsesResponse `json:"response,omitempty"`
	Delta    string             `json:"delta,omitempty"` // for response.output_text.delta

	// For the "error" event:
	Code    string `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
}

// ---------------------------------------------------------------------------
// model.BaseChatModel implementation
// ---------------------------------------------------------------------------

// Generate sends a non-streaming request and returns the complete response.
func (m *openaiResponsesModel) Generate(ctx context.Context, in []*schema.Message, opts ...model.Option) (*schema.Message, error) {
	options := model.GetCommonOptions(&model.Options{}, opts...)
	implOpts := model.GetImplSpecificOptions(&outputSchemaOptions{}, opts...)
	req, err := m.buildRequest(in, options, implOpts, false)
	if err != nil {
		return nil, fmt.Errorf("eino/responses: build request: %w", err)
	}

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("eino/responses: marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, m.cfg.BaseURL+"/responses", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("eino/responses: create request: %w", err)
	}
	m.setHeaders(httpReq)

	resp, err := m.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("eino/responses: do request: %w", err)
	}
	defer resp.Body.Close()
	// W-C-08: observe quota-window headers on every completed round trip, not
	// just failed ones — see quota.go's header comment for why success must
	// be included.
	observeQuotaHeaders(ctx, resp.Header)

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		// Wrapped in a HeaderError so the retry layer can read Retry-After from
		// the response headers instead of scraping it out of the body text
		// (M1). Neither eino nor go-openai surfaces headers on a failed call,
		// and this adapter is one of the few places that still holds the
		// *http.Response.
		return nil, NewHeaderError(resp, fmt.Errorf("eino/responses: API error (HTTP %d): %s", resp.StatusCode, string(respBody)))
	}

	var apiResp responsesResponse
	if err := json.NewDecoder(resp.Body).Decode(&apiResp); err != nil {
		return nil, fmt.Errorf("eino/responses: decode response: %w", err)
	}

	msg := m.responseToMessage(&apiResp)
	if msg == nil {
		msg = schema.AssistantMessage("", nil)
	}
	return msg, nil
}

// Stream sends a streaming request and returns a reader that yields message
// chunks as SSE events arrive.
func (m *openaiResponsesModel) Stream(ctx context.Context, in []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	options := model.GetCommonOptions(&model.Options{}, opts...)
	implOpts := model.GetImplSpecificOptions(&outputSchemaOptions{}, opts...)
	req, err := m.buildRequest(in, options, implOpts, true)
	if err != nil {
		return nil, fmt.Errorf("eino/responses: build request: %w", err)
	}

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("eino/responses: marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, m.cfg.BaseURL+"/responses", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("eino/responses: create request: %w", err)
	}
	m.setHeaders(httpReq)

	resp, err := m.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("eino/responses: do request: %w", err)
	}
	// W-C-08: same as Generate above — observe on every completed round trip.
	observeQuotaHeaders(ctx, resp.Header)
	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, fmt.Errorf("eino/responses: API error (HTTP %d): %s", resp.StatusCode, string(respBody))
	}

	sr, sw := schema.Pipe[*schema.Message](1)
	go m.readStream(ctx, resp, sw)
	return sr, nil
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// setHeaders sets the built-in headers, then applies the operator's W-C-02
// Headers on top — deliberately AFTER, so an operator entry can override a
// built-in name rather than the built-in silently winning.
func (m *openaiResponsesModel) setHeaders(r *http.Request) {
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+m.cfg.APIKey)
	for k, v := range m.cfg.Headers {
		r.Header.Set(k, v)
	}
}

// buildRequest converts eino messages + options into a Responses API request.
//
// Mapping rules:
//   - schema.System messages are concatenated into the top-level Instructions
//     field (the Responses API equivalent of Anthropic's "system" and Chat
//     Completions' system-role messages).
//   - schema.User messages become input items of type "message" with role
//     "user" and content type "input_text". Multi-part user input
//     (UserInputMultiContent) is flattened to text parts.
//   - schema.Assistant messages become input items of type "message" with
//     role "assistant" and content type "output_text", followed by one
//     function_call item per ToolCall (so the API can reconstruct the prior
//     turn's tool invocation).
//   - schema.Tool messages (tool results) become input items of type
//     "function_call_output" with the tool's Content as the output string.
//
// Empty messages are skipped to keep the input array tidy.
func (m *openaiResponsesModel) buildRequest(in []*schema.Message, options *model.Options, implOpts *outputSchemaOptions, stream bool) (*responsesRequest, error) {
	req := &responsesRequest{
		Model:           m.cfg.Model,
		Stream:          stream,
		MaxOutputTokens: m.cfg.MaxOutputTokens,
		Temperature:     m.cfg.Temperature,
		TopP:            m.cfg.TopP,
	}
	// A12-providers: when the turn declared an output schema (per-call
	// OutputSchemaOption), request provider-enforced structured output. Empty
	// schema = text mode, Text stays nil and is omitted from the wire body
	// entirely (byte-identical to pre-A12). Name is required by the Responses
	// API but otherwise arbitrary; "output" is the conventional default.
	if len(implOpts.Schema) > 0 {
		req.Text = &responsesTextConfig{
			Format: responsesTextFormat{
				Type:   "json_schema",
				Name:   "output",
				Schema: implOpts.Schema,
				Strict: true,
			},
		}
	}

	// Pull system messages out into Instructions (Responses API convention).
	var systemParts []string
	for _, msg := range in {
		if msg.Role == schema.System {
			if msg.Content != "" {
				systemParts = append(systemParts, msg.Content)
			}
		}
	}
	if len(systemParts) > 0 {
		req.Instructions = strings.Join(systemParts, "\n\n")
	}

	// Translate tools (function-tool declarations only; other tool kinds are
	// not surfaced today).
	if len(options.Tools) > 0 {
		req.Tools = make([]responsesTool, 0, len(options.Tools))
		for _, t := range options.Tools {
			req.Tools = append(req.Tools, responsesTool{
				Type:        "function",
				Name:        t.Name,
				Description: t.Desc,
				Parameters:  toolInfoToJSONSchema(t),
			})
		}
	}

	// Build input items.
	for _, msg := range in {
		switch string(msg.Role) {
		case string(schema.System):
			// already hoisted into Instructions
			continue
		case "user":
			if len(msg.UserInputMultiContent) > 0 {
				parts := make([]responsesContentPart, 0, len(msg.UserInputMultiContent))
				for _, part := range msg.UserInputMultiContent {
					if part.Text != "" {
						parts = append(parts, responsesContentPart{Type: "input_text", Text: part.Text})
					}
				}
				if len(parts) == 0 {
					continue
				}
				req.Input = append(req.Input, responsesInputItem{
					Type:    "message",
					Role:    "user",
					Content: parts,
				})
			} else if msg.Content != "" {
				req.Input = append(req.Input, responsesInputItem{
					Type: "message",
					Role: "user",
					Content: []responsesContentPart{{
						Type: "input_text",
						Text: msg.Content,
					}},
				})
			}
		case "assistant":
			if msg.Content != "" {
				req.Input = append(req.Input, responsesInputItem{
					Type: "message",
					Role: "assistant",
					Content: []responsesContentPart{{
						Type: "output_text",
						Text: msg.Content,
					}},
				})
			}
			for _, tc := range msg.ToolCalls {
				req.Input = append(req.Input, responsesInputItem{
					Type:      "function_call",
					CallID:    tc.ID,
					Name:      tc.Function.Name,
					Arguments: tc.Function.Arguments,
				})
			}
		case "tool":
			req.Input = append(req.Input, responsesInputItem{
				Type:   "function_call_output",
				CallID: msg.ToolCallID,
				Output: msg.Content,
			})
		}
	}

	return req, nil
}

// responseToMessage converts a Responses API response into a schema.Message.
// Output is scanned for message items' output_text content; function_call
// items are appended as ToolCalls on the returned message. status →
// FinishReason mapping: "completed"→"stop", "incomplete"→"length"; any other
// status is passed through verbatim so the resilient layer can recognize
// novel failures (e.g. "failed", "cancelled") by their original string.
func (m *openaiResponsesModel) responseToMessage(resp *responsesResponse) *schema.Message {
	if resp == nil {
		return nil
	}
	msg := &schema.Message{Role: schema.Assistant}

	if resp.Usage != nil {
		msg.ResponseMeta = &schema.ResponseMeta{
			Usage: &schema.TokenUsage{
				PromptTokens:     resp.Usage.InputTokens,
				CompletionTokens: resp.Usage.OutputTokens,
				TotalTokens:      resp.Usage.TotalTokens,
			},
		}
		// Surface reasoning_tokens (o1/o3 family) so the TUI footer's "think:"
		// segment and /cost reflect the real reasoning-token bill instead of
		// hiding it inside CompletionTokens. See responsesOutputTokensDetails.
		if resp.Usage.OutputTokensDetails != nil {
			msg.ResponseMeta.Usage.CompletionTokensDetails.ReasoningTokens =
				resp.Usage.OutputTokensDetails.ReasoningTokens
		}
	}
	if resp.Status != "" {
		if msg.ResponseMeta == nil {
			msg.ResponseMeta = &schema.ResponseMeta{}
		}
		msg.ResponseMeta.FinishReason = mapStatusToFinishReason(resp.Status)
	}

	var textParts []string
	for _, item := range resp.Output {
		switch item.Type {
		case "message":
			for _, c := range item.Content {
				if c.Type == "output_text" && c.Text != "" {
					textParts = append(textParts, c.Text)
				}
			}
		case "function_call":
			msg.ToolCalls = append(msg.ToolCalls, schema.ToolCall{
				ID:   item.CallID,
				Type: "function",
				Function: schema.FunctionCall{
					Name:      item.Name,
					Arguments: item.Arguments,
				},
			})
		}
	}
	msg.Content = strings.Join(textParts, "")
	return msg
}

// mapStatusToFinishReason translates Responses API status values into the
// finish_reason vocabulary that downstream code (resilient retry classification,
// TUI finish indicator) expects. "completed" is the canonical success; any
// other known non-success status maps to the closest OpenAI finish_reason
// equivalent. Unknown statuses pass through verbatim so new statuses surface
// observably rather than being silently bucketed.
func mapStatusToFinishReason(status string) string {
	switch status {
	case "completed":
		return "stop"
	case "incomplete":
		return "length"
	default:
		return status
	}
}

// readStream drains the SSE response body and emits message chunks via sw.
//
// The Responses API emits a sequence of SSE events. We forward text deltas
// immediately (token-by-token TUI output) and emit a single terminal chunk
// carrying ResponseMeta (Usage + FinishReason) when response.completed or
// response.incomplete arrives. On response.failed / error events we send the
// error through the pipe so the resilient layer can retry.
//
// If the server closes the connection mid-stream without a terminal event
// (proxy timeout, network drop), the scanner simply ends and we close the
// writer — the consumer sees io.EOF. The resilient layer treats this as a
// retryable mid-stream drop.
func (m *openaiResponsesModel) readStream(ctx context.Context, resp *http.Response, sw *schema.StreamWriter[*schema.Message]) {
	defer resp.Body.Close()
	defer sw.Close()

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 65536), 1<<20) // 1 MiB buffer

	for scanner.Scan() {
		line := scanner.Text()

		// SSE: "event: <type>\n" and "data: <json>\n" lines come in pairs,
		// separated by a blank line. We only need the data payload — its
		// embedded "type" field tells us which event it is. The "event:"
		// line is informational; we parse it just to help debugging.
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			return
		}

		var ev responsesStreamEvent
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			// Skip malformed payloads; the API may emit keep-alive comments.
			continue
		}

		switch ev.Type {
		case "response.output_text.delta":
			if ev.Delta != "" {
				// Forward the delta verbatim so the TUI streams token-by-token.
				_ = sw.Send(schema.AssistantMessage(ev.Delta, nil), nil)
			}

		case "response.completed":
			// Terminal success. Emit a final chunk carrying usage + status so
			// downstream consumers (turn accounting, TUI status line) see the
			// canonical ResponseMeta. ToolCalls parsed from the terminal
			// response.output[] are surfaced here too — see metaChunk.
			_ = sw.Send(m.metaChunk(ev.Response), nil)
			return

		case "response.incomplete":
			// Terminal but truncated (e.g. max_output_tokens hit). Map to
			// FinishReason="length" so the resilient layer does not retry —
			// this is a budget outcome, not a transient failure.
			_ = sw.Send(m.metaChunk(ev.Response), nil)
			return

		case "response.failed":
			// Surface a stream error; the resilient layer will retry.
			errMsg := "responses API stream failed"
			if ev.Response != nil && ev.Response.Status != "" {
				errMsg = "responses API stream failed: status=" + ev.Response.Status
			}
			_ = sw.Send(nil, fmt.Errorf("%s", errMsg))
			return

		case "error":
			errMsg := "responses API stream error"
			if ev.Message != "" {
				errMsg = "responses API stream error: " + ev.Message
			}
			_ = sw.Send(nil, fmt.Errorf("%s", errMsg))
			return

		default:
			// Other events (response.created, response.in_progress,
			// response.output_item.added/done, response.content_part.*,
			// response.output_text.done) are not needed for text streaming.
		}
	}

	// Stream ended without a terminal event — surface scanner error if any.
	if err := scanner.Err(); err != nil {
		_ = sw.Send(nil, fmt.Errorf("eino/responses: read stream: %w", err))
	}
	// Otherwise: EOF without terminal event; just close (deferred) so the
	// consumer's Recv loop ends cleanly.
}

// metaChunk builds the terminal stream chunk carrying usage + finish reason +
// tool calls from a response.completed / response.incomplete payload.
//
// It reuses responseToMessage (DRY: the same output[] scan extracts text,
// function_call, usage, and status) and then drops the Content field — Content
// was already streamed token-by-token via response.output_text.delta, and
// re-emitting it on the terminal chunk would double the text in consumers that
// concatenate Content across chunks (see TestResponsesStream). ToolCalls have
// NOT been streamed incrementally (the Responses API does not give us per-call
// deltas the way Anthropic content_block_* does), so the terminal chunk is the
// single place they are surfaced for the ADK ReAct loop to dispatch.
//
// Returns a chunk with ResponseMeta + ToolCalls populated; Content is empty so
// consumers can distinguish delta chunks (Content set) from the terminal meta
// chunk.
func (m *openaiResponsesModel) metaChunk(resp *responsesResponse) *schema.Message {
	full := m.responseToMessage(resp)
	if full == nil {
		return &schema.Message{Role: schema.Assistant}
	}
	return &schema.Message{
		Role:         schema.Assistant,
		ToolCalls:    full.ToolCalls,
		ResponseMeta: full.ResponseMeta,
	}
}
