package eino

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

// FakeModel is a test double that implements model.BaseChatModel.
// It returns scripted responses in order, allowing tests to run without LLM API keys.
type FakeModel struct {
	responses []*schema.Message
	err       error
	calls     int

	// GenerateCalls / StreamCalls count how many times each method was invoked.
	// Used by streaming tests to prove the ADK took the Stream() code path
	// (which only happens when the runner is built with EnableStreaming: true).
	GenerateCalls int
	StreamCalls   int

	// Echo, when true, makes Generate/Stream return the concatenation of every
	// input message's Content as the assistant response instead of a scripted
	// reply. This lets multi-turn tests prove what messages actually reached
	// the model (including prior turns): prior-turn text surfaces in the echo.
	// Ignored when err is non-nil.
	Echo bool

	// Repeat, when true, makes Generate/Stream always return responses[0]
	// regardless of how many times the model has been called (the calls counter
	// is bypassed). Useful for the bootstrap fake model, which must produce the
	// SAME scripted response on every turn of a multi-turn session — without
	// Repeat the counter runs past the single scripted response and the model
	// yields empty messages. Ignored when Echo is set or responses is empty.
	Repeat bool

	// RecordOpts, when true, makes Generate/Stream capture the model.Options
	// passed to the most recent call into ReceivedOpts. Used by per-turn
	// option-passthrough tests (e.g. reasoning_effort via TurnOpts).
	RecordOpts   bool
	ReceivedOpts []model.Option

	// ReceivedOutputSchema is the output schema decoded (via
	// model.GetImplSpecificOptions) from the per-call options of the most recent
	// Generate/Stream call, when RecordOpts is true. It lets orchestrator and
	// transport tests assert the schema survived the option-forwarding path
	// (TurnOpts.OutputSchema -> adk.WithChatModelOptions -> adapter) WITHOUT
	// reaching into the unexported outputSchemaOptions type. Overwrite per call,
	// like ReceivedOpts. nil when no schema option was passed.
	ReceivedOutputSchema json.RawMessage

	// RecordMessages, when true, makes Generate/Stream capture the input
	// messages passed to the most recent call into ReceivedMessages. Used by
	// retry-reminder tests (notably TestChatWS_AutoContinueRetryAddsReminder)
	// to assert what history actually reached the model on each attempt —
	// without this the retry loop's history extension is unobservable. Like
	// ReceivedOpts, only the LATEST call's messages are kept (overwrite, not
	// accumulate). Guarded by optsMu alongside ReceivedOpts.
	RecordMessages   bool
	ReceivedMessages []*schema.Message

	// Vision, when true, makes Generate/Stream return a deterministic description
	// derived from the number of image parts in the most recent input message.
	Vision bool

	// RecordImages, when true, makes Generate/Stream count image parts.
	RecordImages  bool
	LastImageCount int

	optsMu sync.Mutex
}

// NewFakeModel creates a FakeModel that returns the given text responses in order.
// Each string is wrapped as a schema.AssistantMessage with no tool calls.
// If err is non-nil, every call returns that error instead.
func NewFakeModel(textResponses []string, err error) *FakeModel {
	msgs := make([]*schema.Message, len(textResponses))
	for i, t := range textResponses {
		msgs[i] = schema.AssistantMessage(t, nil)
	}
	return &FakeModel{responses: msgs, err: err}
}

// NewFakeModelWithMessages creates a FakeModel from pre-built messages,
// allowing full control over Role, Content, ToolCalls, etc.
func NewFakeModelWithMessages(msgs []*schema.Message, err error) *FakeModel {
	return &FakeModel{responses: msgs, err: err}
}

// Generate returns the next scripted response (or err, or an empty assistant message).
// When Echo is set, it instead echoes the concatenation of every input message's content.
// When Repeat is set, it instead returns responses[0] on every call.
func (m *FakeModel) Generate(_ context.Context, messages []*schema.Message, opts ...model.Option) (*schema.Message, error) {
	m.optsMu.Lock()
	m.GenerateCalls++
	m.optsMu.Unlock()
	m.recordOpts(opts)
	m.recordMessages(messages)
	if m.err != nil {
		return nil, m.err
	}
	// Judge probe: the orchestrator's JudgeCompletion appends a fixed judge
	// question as the final user message. Answer it with a complete=true
	// verdict WITHOUT consuming a scripted response or echoing — otherwise
	// judge probes derange scripted multi-response sequences (and the usage
	// they carry) in tests that don't Repeat.
	if isJudgeProbe(messages) {
		return schema.AssistantMessage(`{"complete":true,"reason":""}`, nil), nil
	}
	if m.Vision {
		return schema.AssistantMessage(fmt.Sprintf("fake-vision(%d image%s)", m.LastImageCount, pluralImg(m.LastImageCount)), nil), nil
	}
	if m.Echo && len(messages) > 0 {
		return schema.AssistantMessage(concatMessageContents(messages), nil), nil
	}
	if m.Repeat && len(m.responses) > 0 {
		return m.responses[0], nil
	}
	if m.calls < len(m.responses) {
		resp := m.responses[m.calls]
		m.calls++
		return resp, nil
	}
	return schema.AssistantMessage("", nil), nil
}

// Stream returns a StreamReader that yields the next scripted message once, then io.EOF.
// When Echo is set, it instead echoes the concatenation of every input message's content.
// When Repeat is set, it instead returns responses[0] on every call.
func (m *FakeModel) Stream(_ context.Context, messages []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	m.optsMu.Lock()
	m.StreamCalls++
	m.optsMu.Unlock()
	m.recordOpts(opts)
	m.recordMessages(messages)
	if m.err != nil {
		return nil, m.err
	}
	var msg *schema.Message
	if isJudgeProbe(messages) {
		msg = schema.AssistantMessage(`{"complete":true,"reason":""}`, nil)
	} else if m.Vision {
		msg = schema.AssistantMessage(fmt.Sprintf("fake-vision(%d image%s)", m.LastImageCount, pluralImg(m.LastImageCount)), nil)
	} else if m.Echo && len(messages) > 0 {
		msg = schema.AssistantMessage(concatMessageContents(messages), nil)
	} else if m.Repeat && len(m.responses) > 0 {
		msg = m.responses[0]
	} else if m.calls < len(m.responses) {
		msg = m.responses[m.calls]
		m.calls++
	} else {
		msg = schema.AssistantMessage("", nil)
	}
	return schema.StreamReaderFromArray[*schema.Message]([]*schema.Message{msg}), nil
}

// recordOpts captures the call's model.Options when RecordOpts is set, and
// decodes the per-turn output schema (if any) into ReceivedOutputSchema so
// higher-layer tests can assert schema forwarding without importing the
// unexported option type.
func (m *FakeModel) recordOpts(opts []model.Option) {
	if !m.RecordOpts {
		return
	}
	m.optsMu.Lock()
	m.ReceivedOpts = opts
	implOpts := model.GetImplSpecificOptions(&outputSchemaOptions{}, opts...)
	m.ReceivedOutputSchema = implOpts.Schema
	m.optsMu.Unlock()
}

// recordMessages captures the call's input messages when RecordMessages is set.
// Mirrors recordOpts: overwrite (not accumulate) so only the latest call's
// messages are kept — a follow-up assertion checks what the most recent attempt
// saw, which is exactly what the retry-reminder test needs.
func (m *FakeModel) recordMessages(messages []*schema.Message) {
	if !m.RecordMessages && !m.Vision && !m.RecordImages {
		return
	}
	m.optsMu.Lock()
	defer m.optsMu.Unlock()
	if m.RecordMessages {
		m.ReceivedMessages = messages
	}
	if m.RecordImages || m.Vision {
		m.LastImageCount = countImageParts(messages)
	}
}

func countImageParts(messages []*schema.Message) int {
	n := 0
	for _, msg := range messages {
		if msg == nil {
			continue
		}
		for _, part := range msg.UserInputMultiContent {
			if part.Image != nil {
				n++
			}
		}
	}
	return n
}

// isJudgeProbe reports whether messages ends with the orchestrator's
// completion-judge question (see orchestrator.judgePrompt — it starts with
// "You are a completion judge."). Recognizing it lets the fake answer the
// judge with a fixed complete=true verdict without consuming a scripted
// response, so judge probes don't derange multi-response test scripts.
func isJudgeProbe(messages []*schema.Message) bool {
	if len(messages) == 0 {
		return false
	}
	last := messages[len(messages)-1]
	return last != nil && last.Role == schema.User &&
		strings.Contains(last.Content, "completion judge")
}

// concatMessageContents joins the Content of every message with newlines. Used
// by Echo mode so prior-turn text is visible in the response.
func concatMessageContents(messages []*schema.Message) string {
	parts := make([]string, 0, len(messages))
	for _, m := range messages {
		if m == nil {
			continue
		}
		parts = append(parts, m.Content)
	}
	return strings.Join(parts, "\n")
}

func pluralImg(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

var _ model.BaseChatModel = (*FakeModel)(nil)
