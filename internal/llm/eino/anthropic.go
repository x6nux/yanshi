// Package eino provides LLM model adapters for yanshi.
//
// This file implements an Anthropic Claude adapter over the Messages API
// (POST /v1/messages), wrapping it as model.BaseChatModel so it is a
// drop-in replacement for the OpenAI Chat Completions adapter in the
// orchestrator's ReAct loop.

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

// AnthropicModelConfig configures an AnthropicModel.
type AnthropicModelConfig struct {
	// APIKey is the Anthropic API key (x-api-key header).
	APIKey string
	// Model is the Anthropic model id, e.g. "claude-opus-4-8", "claude-sonnet-4-20250514".
	Model string
	// BaseURL overrides the default API endpoint. Default: "https://api.anthropic.com".
	// A trailing "/v1" is added automatically; set e.g. "https://api.anthropic.com/v1"
	// or via an OpenRouter-like proxy that speaks the Anthropic wire format.
	BaseURL string
	// MaxTokens is the default max_tokens sent with every request. Default: 4096.
	MaxTokens int
	// HTTPClient is the HTTP client used for API calls.
	HTTPClient *http.Client
}

// AnthropicModel implements model.BaseChatModel via the Anthropic Messages API.
// It handles text content blocks, tool_use/tool_result blocks, and streaming
// via SSE events (content_block_start/delta/stop, message_start/delta/stop).
type AnthropicModel struct {
	config AnthropicModelConfig
}

// NewAnthropicModel builds an AnthropicModel from config.
func NewAnthropicModel(_ context.Context, cfg *AnthropicModelConfig) (*AnthropicModel, error) {
	if cfg.APIKey == "" {
		return nil, fmt.Errorf("anthropic: APIKey is required")
	}
	if cfg.Model == "" {
		return nil, fmt.Errorf("anthropic: Model is required")
	}
	base := cfg.BaseURL
	if base == "" {
		base = "https://api.anthropic.com"
	}
	base = strings.TrimRight(base, "/")
	if !strings.HasSuffix(base, "/v1") {
		base += "/v1"
	}
	maxTokens := cfg.MaxTokens
	if maxTokens <= 0 {
		maxTokens = 4096
	}
	hc := cfg.HTTPClient
	if hc == nil {
		// No retry, no Timeout: ResilientChatModel is the single retry
		// authority (see provider.go BuildProviders for the rationale).
		hc = &http.Client{Transport: http.DefaultTransport}
	}
	return &AnthropicModel{
		config: AnthropicModelConfig{
			APIKey:     cfg.APIKey,
			Model:      cfg.Model,
			BaseURL:    base,
			MaxTokens:  maxTokens,
			HTTPClient: hc,
		},
	}, nil
}

// compile-time interface check.
var _ model.BaseChatModel = (*AnthropicModel)(nil)

// ---------------------------------------------------------------------------
// Anthropic request / response wire types
// ---------------------------------------------------------------------------

type anthropicMessage struct {
	Role    string                 `json:"role"`    // "user" | "assistant"
	Content []anthropicContentPart `json:"content"` // always an array for this adapter
}

type anthropicContentPart struct {
	Type     string                `json:"type"`               // "text" | "thinking" | "tool_use" | "tool_result" | "image"
	Text     string                `json:"text,omitempty"`     // text / tool_result content
	Thinking string                `json:"thinking,omitempty"` // extended-thinking content
	ID       string                `json:"id,omitempty"`       // tool_use.id / tool_result.tool_use_id
	Name     string                `json:"name,omitempty"`     // tool_use.name
	Input    any                   `json:"input,omitempty"`    // tool_use.input (raw JSON object)
	Source   *anthropicImageSource `json:"source,omitempty"`   // image source

	// tool_result uses this nested content when the result is multi-part.
	// For simplicity we always send a single text block.
	Content []anthropicContentPart `json:"content,omitempty"`
}

// anthropicImageSource is used in content parts for images.
type anthropicImageSource struct {
	Type      string `json:"type"`                 // "base64" | "url"
	MediaType string `json:"media_type,omitempty"` // e.g. "image/jpeg"
	Data      string `json:"data,omitempty"`       // base64 data or URL
}

type anthropicTool struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	InputSchema any    `json:"input_schema"` // JSON Schema object
}

// anthropicOutputConfig maps onto the Anthropic Messages API top-level
// output_config field for structured outputs ({"format":{"type":"json_schema",
// "schema":{...}}}). Pointer-optional so omitempty drops it entirely on
// text-mode turns. See plan DESIGN NOTE for field choice.
type anthropicOutputConfig struct {
	Format anthropicOutputFormat `json:"format"`
}

// anthropicOutputFormat is the output_config.format payload. Type is always
// "json_schema"; Schema is the raw JSON Schema document.
type anthropicOutputFormat struct {
	Type   string          `json:"type"`
	Schema json.RawMessage `json:"schema"`
}

type anthropicRequest struct {
	Model     string             `json:"model"`
	MaxTokens int                `json:"max_tokens"`
	System    string             `json:"system,omitempty"`
	Messages  []anthropicMessage `json:"messages"`
	Tools     []anthropicTool    `json:"tools,omitempty"`
	Stream    bool               `json:"stream"`

	// OutputConfig, when non-nil, requests provider-enforced structured output
	// for this turn (Anthropic output_config.format json_schema). nil on
	// text-mode turns so the field is omitted from the wire body entirely.
	OutputConfig *anthropicOutputConfig `json:"output_config,omitempty"`

	// Thinking, when non-nil, turns on extended thinking for this turn. nil on
	// ordinary turns so the body stays byte-identical to a request that never
	// heard of the feature.
	Thinking *anthropicThinking `json:"thinking,omitempty"`
}

// anthropicThinking is the API's extended-thinking block.
//
// budget_tokens is taken OUT of max_tokens, not added to it, so a budget at or
// above max_tokens leaves no room for an answer and the API rejects the
// request outright — the symptom is a 400 on every thinking turn.
type anthropicThinking struct {
	Type         string `json:"type"` // always "enabled"
	BudgetTokens int    `json:"budget_tokens"`
}

// thinkingBudgetFractions maps an effort level onto a fraction of max_tokens.
//
// Fractions rather than absolute counts because max_tokens is per-deployment
// configuration: a fixed 16k budget is most of a small deployment's allowance
// and a rounding error in a large one. The fractions leave at least half the
// allowance for the answer at every level.
var thinkingBudgetFractions = map[string]float64{
	"low":    0.15,
	"medium": 0.30,
	"high":   0.50,
}

// minThinkingBudget is Anthropic's floor. A request below it is rejected, so a
// small max_tokens is clamped UP to the floor and then down again below
// max_tokens by the caller — a deployment too small for both loses thinking
// rather than sending a request that cannot succeed.
const minThinkingBudget = 1024

// thinkingBlock builds the extended-thinking block for an effort level, or nil
// when the turn asked for none or the deployment has no room for one.
func thinkingBlock(effort string, maxTokens int) *anthropicThinking {
	frac, ok := thinkingBudgetFractions[effort]
	if !ok || maxTokens <= 0 {
		return nil
	}
	budget := int(float64(maxTokens) * frac)
	if budget < minThinkingBudget {
		budget = minThinkingBudget
	}
	// No room for both the floor and an answer: drop thinking rather than
	// send a request the API will refuse.
	if budget >= maxTokens {
		return nil
	}
	return &anthropicThinking{Type: "enabled", BudgetTokens: budget}
}

type anthropicResponse struct {
	ID         string                 `json:"id"`
	Type       string                 `json:"type"`
	Role       string                 `json:"role"`
	Content    []anthropicContentPart `json:"content"`
	StopReason string                 `json:"stop_reason"`
	Usage      *anthropicUsage        `json:"usage,omitempty"`
}

type anthropicUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`

	// CacheReadInputTokens is the number of prompt tokens served from
	// Anthropic's prompt cache. Mapping it to TokenUsage.PromptTokenDetails.
	// CachedTokens lets the TUI footer's "cache:" segment and the /cost
	// breakdown show how much of the input was effectively free (cache hit)
	// rather than collapsing cache hits into the generic input_tokens total.
	// cache_creation_input_tokens is intentionally not surfaced — only cache
	// reads represent a savings, creations are billed at a premium.
	CacheReadInputTokens int `json:"cache_read_input_tokens,omitempty"`
}

// ---------------------------------------------------------------------------
// SSE event types for streaming
// ---------------------------------------------------------------------------

type anthropicStreamEvent struct {
	Type         string                `json:"type"`
	Message      *anthropicResponse    `json:"message,omitempty"`
	Index        *int                  `json:"index,omitempty"`
	ContentBlock *anthropicContentPart `json:"content_block,omitempty"`
	Delta        *anthropicStreamDelta `json:"delta,omitempty"`
	Usage        *anthropicUsage       `json:"usage,omitempty"`
}

type anthropicStreamDelta struct {
	Type        string `json:"type"` // "text_delta" | "thinking_delta" | "input_json_delta"
	Text        string `json:"text,omitempty"`
	Thinking    string `json:"thinking,omitempty"`
	PartialJSON string `json:"partial_json,omitempty"`

	// StopReason is populated on a "message_delta" terminal event (delta.type
	// is not set on that event — the field lives alongside stop_sequence under
	// delta). The streaming readStream maps it onto ResponseMeta.FinishReason
	// so the resilient layer, ADK, and TUI see the OpenAI finish_reason
	// vocabulary rather than Anthropic's stop_reason names. See
	// mapAnthropicStopReason for the translation table.
	StopReason string `json:"stop_reason,omitempty"`
}

// ---------------------------------------------------------------------------
// model.BaseChatModel implementation
// ---------------------------------------------------------------------------

// Generate sends a non-streaming request and returns the complete response.
func (m *AnthropicModel) Generate(ctx context.Context, in []*schema.Message, opts ...model.Option) (*schema.Message, error) {
	options := model.GetCommonOptions(&model.Options{}, opts...)
	implOpts := model.GetImplSpecificOptions(&outputSchemaOptions{}, opts...)
	req, err := m.buildRequest(in, options, implOpts, false)
	if err != nil {
		return nil, fmt.Errorf("anthropic: build request: %w", err)
	}

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("anthropic: marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, m.config.BaseURL+"/messages", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("anthropic: create request: %w", err)
	}
	m.setHeaders(httpReq)

	resp, err := m.config.HTTPClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("anthropic: do request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("anthropic: API error (HTTP %d): %s", resp.StatusCode, string(respBody))
	}

	var anthResp anthropicResponse
	if err := json.NewDecoder(resp.Body).Decode(&anthResp); err != nil {
		return nil, fmt.Errorf("anthropic: decode response: %w", err)
	}

	msg := m.responseToMessage(&anthResp)
	if msg == nil {
		msg = schema.AssistantMessage("", nil)
	}
	return msg, nil
}

// Stream sends a streaming request and returns a reader that yields message
// chunks as SSE events arrive.
func (m *AnthropicModel) Stream(ctx context.Context, in []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	options := model.GetCommonOptions(&model.Options{}, opts...)
	implOpts := model.GetImplSpecificOptions(&outputSchemaOptions{}, opts...)
	req, err := m.buildRequest(in, options, implOpts, true)
	if err != nil {
		return nil, fmt.Errorf("anthropic: build request: %w", err)
	}

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("anthropic: marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, m.config.BaseURL+"/messages", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("anthropic: create request: %w", err)
	}
	m.setHeaders(httpReq)

	resp, err := m.config.HTTPClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("anthropic: do request: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, fmt.Errorf("anthropic: API error (HTTP %d): %s", resp.StatusCode, string(respBody))
	}

	sr, sw := schema.Pipe[*schema.Message](1)
	go m.readStream(ctx, resp, sw)
	return sr, nil
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func (m *AnthropicModel) setHeaders(r *http.Request) {
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("x-api-key", m.config.APIKey)
	r.Header.Set("anthropic-version", "2023-06-01")
}

// buildRequest converts eino messages + options into an Anthropic API request.
func (m *AnthropicModel) buildRequest(in []*schema.Message, options *model.Options, implOpts *outputSchemaOptions, stream bool) (*anthropicRequest, error) {
	req := &anthropicRequest{
		Model:     m.config.Model,
		MaxTokens: m.config.MaxTokens,
		Stream:    stream,
	}
	// A12-providers: when the turn declared an output schema (per-call
	// OutputSchemaOption), request provider-enforced structured output. Empty
	// schema = text mode, OutputConfig stays nil and is omitted from the wire
	// body entirely (byte-identical to pre-A12).
	if len(implOpts.Schema) > 0 {
		req.OutputConfig = &anthropicOutputConfig{
			Format: anthropicOutputFormat{
				Type:   "json_schema",
				Schema: implOpts.Schema,
			},
		}
	}
	// UX8: extended thinking. Absent effort leaves Thinking nil and omitempty
	// drops it, so an ordinary turn is byte-identical to before.
	req.Thinking = thinkingBlock(implOpts.ThinkingEffort, req.MaxTokens)

	// Extract system message (Anthropic uses a top-level "system" field).
	var systemParts []string
	messages := make([]*schema.Message, 0, len(in))
	for _, msg := range in {
		if msg.Role == schema.System {
			if msg.Content != "" {
				systemParts = append(systemParts, msg.Content)
			}
			continue
		}
		messages = append(messages, msg)
	}
	if len(systemParts) > 0 {
		req.System = strings.Join(systemParts, "\n\n")
	}

	// Convert tools.
	if len(options.Tools) > 0 {
		req.Tools = make([]anthropicTool, 0, len(options.Tools))
		for _, t := range options.Tools {
			at := anthropicTool{
				Name:        t.Name,
				Description: t.Desc,
				InputSchema: toolInfoToJSONSchema(t),
			}
			req.Tools = append(req.Tools, at)
		}
	}

	// Convert messages.
	req.Messages = make([]anthropicMessage, 0, len(messages))
	for _, msg := range messages {
		am := anthropicMessage{Role: string(msg.Role)}
		switch string(msg.Role) {
		case "user":
			if len(msg.UserInputMultiContent) > 0 {
				// Multi-part user input: build content blocks.
				parts := make([]anthropicContentPart, 0, len(msg.UserInputMultiContent))
				for _, part := range msg.UserInputMultiContent {
					cp := anthropicContentPart{Type: string(part.Type)}
					if part.Text != "" {
						cp.Text = part.Text
					}
					if part.Image != nil {
						img := part.Image
						mediaType := img.MIMEType
						if mediaType == "" {
							mediaType = "image/jpeg"
						}
						cp.Type = "image"
						if img.Base64Data != nil {
							cp.Source = &anthropicImageSource{
								Type:      "base64",
								MediaType: mediaType,
								Data:      *img.Base64Data,
							}
						}
						if img.URL != nil && *img.URL != "" && cp.Source == nil {
							cp.Source = &anthropicImageSource{
								Type: "url",
								Data: *img.URL,
							}
						}
					}
					parts = append(parts, cp)
				}
				am.Content = parts
			} else if msg.Content != "" {
				am.Content = []anthropicContentPart{{Type: "text", Text: msg.Content}}
			} else {
				continue // skip empty messages
			}

		case "assistant":
			// Build content parts: text and tool_use.
			parts := make([]anthropicContentPart, 0, 1+len(msg.ToolCalls))
			if msg.Content != "" {
				parts = append(parts, anthropicContentPart{Type: "text", Text: msg.Content})
			}
			for _, tc := range msg.ToolCalls {
				// Parse the JSON arguments into a generic map.
				var args any
				if tc.Function.Arguments != "" {
					if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
						args = tc.Function.Arguments // fallback: send as raw string
					}
				}
				parts = append(parts, anthropicContentPart{
					Type:  "tool_use",
					ID:    tc.ID,
					Name:  tc.Function.Name,
					Input: args,
				})
			}
			am.Content = parts

		case "tool":
			// Anthropic sends tool results as "user" role with tool_result content blocks.
			am.Role = "user"
			am.Content = []anthropicContentPart{{
				Type:    "tool_result",
				ID:      msg.ToolCallID,
				Content: []anthropicContentPart{{Type: "text", Text: msg.Content}},
			}}
		}
		req.Messages = append(req.Messages, am)
	}

	return req, nil
}

// toolInfoToJSONSchema converts a schema.ToolInfo into a JSON Schema object
// suitable for Anthropic's input_schema field. Uses ParamsOneOf.ToJSONSchema
// when available, or returns a default {"type":"object"} for nil/no-params tools.
func toolInfoToJSONSchema(ti *schema.ToolInfo) any {
	if ti == nil || ti.ParamsOneOf == nil {
		return map[string]any{"type": "object"}
	}
	js, err := ti.ToJSONSchema()
	if err != nil || js == nil {
		return map[string]any{"type": "object"}
	}
	// Marshal the jsonschema.Schema to a generic map for the Anthropic wire format.
	raw, err := json.Marshal(js)
	if err != nil {
		return map[string]any{"type": "object"}
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		return map[string]any{"type": "object"}
	}
	return out
}

// responseToMessage converts an Anthropic API response into a schema.Message.
func (m *AnthropicModel) responseToMessage(resp *anthropicResponse) *schema.Message {
	if resp == nil {
		return nil
	}
	msg := &schema.Message{
		Role: schema.Assistant,
	}
	if resp.Usage != nil {
		msg.ResponseMeta = &schema.ResponseMeta{
			Usage: &schema.TokenUsage{
				PromptTokens:     resp.Usage.InputTokens,
				CompletionTokens: resp.Usage.OutputTokens,
			},
		}
		// Surface cache_read_input_tokens so the TUI footer's "cache:" segment
		// and /cost reflect Anthropic prompt-cache hits instead of hiding them
		// in input_tokens. See anthropicUsage.CacheReadInputTokens.
		msg.ResponseMeta.Usage.PromptTokenDetails.CachedTokens = resp.Usage.CacheReadInputTokens
	}
	// Map stop_reason onto FinishReason so downstream code (resilient retry
	// classification, ADK tool-call dispatch, TUI finish indicator) sees the
	// OpenAI finish_reason vocabulary rather than Anthropic's stop_reason.
	if resp.StopReason != "" {
		if msg.ResponseMeta == nil {
			msg.ResponseMeta = &schema.ResponseMeta{}
		}
		msg.ResponseMeta.FinishReason = mapAnthropicStopReason(resp.StopReason)
	}

	var textParts []string
	var thinkingParts []string
	for _, block := range resp.Content {
		switch block.Type {
		case "text":
			if block.Text != "" {
				textParts = append(textParts, block.Text)
			}
		case "thinking":
			if block.Thinking != "" {
				thinkingParts = append(thinkingParts, block.Thinking)
			}
		case "tool_use":
			argsJSON, err := json.Marshal(block.Input)
			argsStr := ""
			if err == nil {
				argsStr = string(argsJSON)
			}
			msg.ToolCalls = append(msg.ToolCalls, schema.ToolCall{
				ID:   block.ID,
				Type: "function",
				Function: schema.FunctionCall{
					Name:      block.Name,
					Arguments: argsStr,
				},
			})
		}
	}
	msg.Content = strings.Join(textParts, "")
	msg.ReasoningContent = strings.Join(thinkingParts, "")
	if msg.Content == "" && len(msg.ToolCalls) == 0 {
		msg.Content = ""
	}
	return msg
}

// mapAnthropicStopReason translates Anthropic's stop_reason vocabulary into
// the OpenAI finish_reason vocabulary that downstream code (resilient retry
// classification, ADK tool-call dispatch, TUI finish indicator) expects.
//
//	end_turn       → "stop"       (model finished naturally)
//	stop_sequence  → "stop"       (hit a configured stop sequence; still a clean stop)
//	max_tokens     → "length"     (budget-exhausted; resilient layer should NOT retry)
//	tool_use       → "tool_calls" (model wants to call a tool; ADK must dispatch)
//
// Unknown values pass through verbatim so novel stop reasons (e.g. a future
// "refusal") surface observably rather than being silently bucketed as "stop".
func mapAnthropicStopReason(reason string) string {
	switch reason {
	case "end_turn", "stop_sequence":
		return "stop"
	case "max_tokens":
		return "length"
	case "tool_use":
		return "tool_calls"
	default:
		return reason
	}
}

// readStream drains the SSE response body and emits message chunks via sw.
//
// Text is streamed token-by-token as it arrives: each content_block_delta
// text_delta (and any initial text carried by content_block_start) is sent
// immediately via sw.Send and is NOT accumulated. Re-emitting accumulated text
// on the terminal event would double every token in consumers that concatenate
// Content across chunks (resilient.consumeStream → classify.emitAssistantContent
// → ws.go assistantText → cs.history), which is exactly the C1 bug this shape
// avoids. This mirrors responses.go's metaChunk (review I1), which drops Content
// on the terminal chunk for the same reason.
//
// tool_use input, by contrast, IS accumulated and emitted once on
// content_block_stop: the ADK ReAct loop needs a single complete ToolCall with
// valid args JSON, not partial fragments — text and tool_use are not symmetric
// here.
func (m *AnthropicModel) readStream(ctx context.Context, resp *http.Response, sw *schema.StreamWriter[*schema.Message]) {
	defer resp.Body.Close()
	defer sw.Close()

	var (
		// toolUseAccum stores partial tool_use.input_json_delta chunks; the
		// assembled JSON is emitted as a single ToolCall on content_block_stop.
		toolUseAccum strings.Builder
		toolCallID   string
		toolCallName string

		// startUsage captures the usage block from the initial message_start
		// event, and startHasUsage flags whether one was seen. Anthropic
		// splits streaming usage across two SSE events (S1):
		//   - message_start.message.usage carries input_tokens,
		//     cache_read_input_tokens, cache_creation_input_tokens, and an
		//     initial output_tokens (typically 1);
		//   - message_delta.usage carries ONLY the final accumulated
		//     output_tokens (no input_tokens, no cache fields).
		// So the terminal chunk must merge: PromptTokens + CachedTokens come
		// from the start frame, CompletionTokens from the delta frame. Without
		// this, real Anthropic streams reported PromptTokens=0 and
		// CachedTokens=0 (the TUI ctx bar, /cost prompt $, and footer cache
		// segment all read 0). A value copy is a deep copy here because
		// anthropicUsage holds only ints — no aliasing into the per-iteration
		// ev.Message.Usage pointer.
		startUsage    anthropicUsage
		startHasUsage bool
	)

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 65536), 1<<20) // 1 MiB buffer

	for scanner.Scan() {
		line := scanner.Text()

		if strings.HasPrefix(line, "event: ") {
			_ = strings.TrimPrefix(line, "event: ")
			continue
		}
		if strings.HasPrefix(line, "data: ") {
			data := strings.TrimPrefix(line, "data: ")
			if data == "[DONE]" {
				return
			}
			var ev anthropicStreamEvent
			if err := json.Unmarshal([]byte(data), &ev); err != nil {
				// Skip malformed events; the API may send keep-alives or other noise.
				continue
			}

			switch ev.Type {
			case "message_start":
				// Reset accumulators for a new message stream.
				toolUseAccum.Reset()
				toolCallID = ""
				toolCallName = ""
				// Capture the start-frame usage so it can be merged with the
				// delta-frame output_tokens on the terminal chunk. See
				// startUsage above (S1).
				if ev.Message != nil && ev.Message.Usage != nil {
					startUsage = *ev.Message.Usage
					startHasUsage = true
				}

			case "content_block_start":
				if ev.ContentBlock != nil {
					switch ev.ContentBlock.Type {
					case "text":
						// Anthropic normally starts text blocks empty and
						// delivers text via text_delta, but if a block arrives
						// with prefilled text we emit it right away. Do NOT
						// buffer — buffering + later flush is what caused C1.
						if ev.ContentBlock.Text != "" {
							_ = sw.Send(schema.AssistantMessage(ev.ContentBlock.Text, nil), nil)
						}
					case "thinking":
						if ev.ContentBlock.Thinking != "" {
							_ = sw.Send(&schema.Message{Role: schema.Assistant, ReasoningContent: ev.ContentBlock.Thinking}, nil)
						}
					case "tool_use":
						toolCallID = ev.ContentBlock.ID
						toolCallName = ev.ContentBlock.Name
						toolUseAccum.Reset()
					}
				}

			case "content_block_delta":
				if ev.Delta != nil {
					switch ev.Delta.Type {
					case "text_delta":
						// Emit each delta as it arrives for token-by-token
						// streaming. This is the single source of truth for
						// streamed text — there is no accumulator and no
						// terminal flush, so the text cannot be double-emitted.
						if ev.Delta.Text != "" {
							_ = sw.Send(schema.AssistantMessage(ev.Delta.Text, nil), nil)
						}
					case "thinking_delta":
						if ev.Delta.Thinking != "" {
							_ = sw.Send(&schema.Message{Role: schema.Assistant, ReasoningContent: ev.Delta.Thinking}, nil)
						} else if ev.Delta.Text != "" {
							// Some gateways use the generic text field for a
							// thinking_delta; preserve it as reasoning content.
							_ = sw.Send(&schema.Message{Role: schema.Assistant, ReasoningContent: ev.Delta.Text}, nil)
						}
					case "input_json_delta":
						if ev.Delta.PartialJSON != "" {
							toolUseAccum.WriteString(ev.Delta.PartialJSON)
						}
					}
				}

			case "content_block_stop":
				if toolCallID != "" {
					// Finalize the tool call from accumulated JSON.
					argsStr := toolUseAccum.String()
					// Validate JSON.
					if argsStr != "" && !json.Valid([]byte(argsStr)) {
						argsStr = "{}"
					}
					tc := schema.ToolCall{
						ID:   toolCallID,
						Type: "function",
						Function: schema.FunctionCall{
							Name:      toolCallName,
							Arguments: argsStr,
						},
					}
					_ = sw.Send(&schema.Message{
						Role:      schema.Assistant,
						ToolCalls: []schema.ToolCall{tc},
					}, nil)
					toolCallID = ""
					toolCallName = ""
					toolUseAccum.Reset()
				}

			case "message_delta":
				// Terminal usage/stop_reason update. Anthropic streams the
				// final output_tokens and the stop_reason on this single
				// message_delta event; emit them together on one terminal
				// chunk so the resilient layer / TUI see a complete
				// ResponseMeta (Usage + FinishReason + CachedTokens).
				//
				// Usage is merged across two SSE frames (S1): message_delta
				// carries only the final output_tokens, so PromptTokens and
				// CachedTokens are backfilled from the message_start frame
				// captured in startUsage. Without this merge, real Anthropic
				// streams had PromptTokens=0 and CachedTokens=0 (the fixture
				// hid it by wrongly placing input_tokens in message_delta).
				//
				// No Content is carried here — text was already streamed
				// delta-by-delta above — so this terminal chunk does not
				// double-emit text (C1).
				var meta *schema.ResponseMeta
				if ev.Usage != nil || startHasUsage {
					meta = &schema.ResponseMeta{
						Usage: &schema.TokenUsage{},
					}
					// PromptTokens + CachedTokens come from the message_start
					// frame; CompletionTokens comes from the message_delta
					// frame (the final accumulated output_tokens value).
					if startHasUsage {
						meta.Usage.PromptTokens = startUsage.InputTokens
						// Surface cache_read_input_tokens so the TUI footer's
						// "cache:" segment reflects Anthropic prompt-cache hits.
						// See anthropicUsage.CacheReadInputTokens.
						meta.Usage.PromptTokenDetails.CachedTokens = startUsage.CacheReadInputTokens
					}
					if ev.Usage != nil {
						meta.Usage.CompletionTokens = ev.Usage.OutputTokens
					}
				}
				if ev.Delta != nil && ev.Delta.StopReason != "" {
					if meta == nil {
						meta = &schema.ResponseMeta{}
					}
					// Map stop_reason → FinishReason so downstream code sees
					// the OpenAI vocabulary. Without this the streaming
					// terminal chunk carried Usage but no FinishReason, so
					// the resilient layer and TUI could not tell a tool_use
					// finish from a clean stop (review I2).
					meta.FinishReason = mapAnthropicStopReason(ev.Delta.StopReason)
				}
				if meta != nil {
					_ = sw.Send(&schema.Message{
						Role:         schema.Assistant,
						ResponseMeta: meta,
					}, nil)
				}

			case "message_stop":
				return

			case "ping":
				// Anthropic sends keep-alive pings; ignore them.
				continue

			default:
				// Unknown event type; skip.
			}
		}
	}

	// Stream ended without a message_stop — surface scanner error if any.
	// Matches responses.go: only forward a real error, otherwise let the
	// deferred sw.Close() end the consumer's Recv loop cleanly. There is no
	// text accumulator to drain (text was streamed delta-by-delta above).
	if err := scanner.Err(); err != nil {
		_ = sw.Send(nil, fmt.Errorf("eino/anthropic: read stream: %w", err))
	}
}
