package eino

import (
	"context"
	"errors"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// toolInfoToJSONSchema: full coverage
// ---------------------------------------------------------------------------

// TestToolInfoToJSONSchema_ValidWithToJSONSchema proves toolInfoToJSONSchema
// works end-to-end with valid schema produced by ToJSONSchema.
func TestToolInfoToJSONSchema_ValidWithToJSONSchema(t *testing.T) {
	ti := &schema.ToolInfo{
		Name: "my_tool",
		Desc: "does something",
		ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
			"x": {Type: "string", Desc: "the X parameter", Required: true},
		}),
	}
	got := toolInfoToJSONSchema(ti)
	m, ok := got.(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "object", m["type"])
	props, ok := m["properties"].(map[string]any)
	require.True(t, ok)
	xProp, ok := props["x"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "string", xProp["type"])
}

// TestToolInfoToJSONSchema_NilParamsOneOf proves nil ParamsOneOf returns fallback.
func TestToolInfoToJSONSchema_NilParamsOneOf(t *testing.T) {
	ti := &schema.ToolInfo{Name: "nil_tool"}
	got := toolInfoToJSONSchema(ti)
	m, ok := got.(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "object", m["type"])
}

// ---------------------------------------------------------------------------
// Resilient runStream: mid-stream error after content delivered
// ---------------------------------------------------------------------------

// partialThenErrorModel sends content then errors mid-stream.
type partialThenErrorModel struct {
	calls int32
}

func (m *partialThenErrorModel) Generate(_ context.Context, _ []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	return schema.AssistantMessage("ok", nil), nil
}

func (m *partialThenErrorModel) Stream(_ context.Context, _ []*schema.Message, _ ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	atomic.AddInt32(&m.calls, 1)
	sr, sw := schema.Pipe[*schema.Message](1)
	go func() {
		defer sw.Close()
		_ = sw.Send(schema.AssistantMessage("partial", nil), nil)
		_ = sw.Send(nil, errors.New("mid-stream error"))
	}()
	return sr, nil
}

var _ model.BaseChatModel = (*partialThenErrorModel)(nil)

// TestResilientStream_MidStreamErrorCountsAttempt proves that content delivered
// (sawSubstantive=true) causes the stream not to be treated as empty, so the
// error propagates rather than retrying.
func TestResilientStream_MidStreamErrorCountsAttempt(t *testing.T) {
	f := &partialThenErrorModel{}
	cfg := ResilientConfig{MaxRetries: 3, MaxEmptyRetries: 3, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond}
	r, err := NewResilientModel([]model.BaseChatModel{f}, cfg)
	require.NoError(t, err)

	sr, err := r.Stream(context.Background(), []*schema.Message{schema.UserMessage("x")})
	require.NoError(t, err)
	defer sr.Close()

	var got string
	var recvErr error
	for {
		msg, e := sr.Recv()
		if e != nil {
			if e == io.EOF {
				break
			}
			recvErr = e
			break
		}
		got += msg.Content
	}
	assert.Contains(t, got, "partial", "the partial content before the error")
	assert.Error(t, recvErr, "the error must be surfaced")
	assert.Equal(t, int32(1), atomic.LoadInt32(&f.calls), "only 1 call (error with content is not retried as empty)")
}

// ---------------------------------------------------------------------------
// consumeStream: all-blank stream → streamEmpty
// ---------------------------------------------------------------------------

// TestConsumeStream_OnlyReasoningIsEmpty proves a reasoning-only stream is empty.
func TestConsumeStream_OnlyReasoningIsEmpty(t *testing.T) {
	reasoningMsg := &schema.Message{Role: schema.Assistant, ReasoningContent: "thinking..."}
	sr := schema.StreamReaderFromArray([]*schema.Message{reasoningMsg})
	_, sw := schema.Pipe[*schema.Message](1)
	defer sr.Close()
	defer sw.Close()

	var deliveredTools bool
	outcome, err := consumeStream(context.Background(), sr, sw, &deliveredTools)
	assert.Equal(t, streamEmpty, outcome)
	assert.NoError(t, err)
}

// TestConsumeStream_AllBlank proves a stream of all-blank messages is empty.
func TestConsumeStream_AllBlank(t *testing.T) {
	blankMsg := &schema.Message{}
	sr := schema.StreamReaderFromArray([]*schema.Message{blankMsg})
	_, sw := schema.Pipe[*schema.Message](1)
	defer sr.Close()
	defer sw.Close()

	var deliveredTools bool
	outcome, err := consumeStream(context.Background(), sr, sw, &deliveredTools)
	assert.Equal(t, streamEmpty, outcome)
	assert.NoError(t, err)
}

// ---------------------------------------------------------------------------
// FakeModel Generate all paths
// ---------------------------------------------------------------------------

// TestFakeModel_GenerateRepeatEmptyResponse tests Repeat with empty responses list.
func TestFakeModel_GenerateRepeatEmptyResponse(t *testing.T) {
	m := NewFakeModel(nil, nil)
	m.Repeat = true // repeat with no responses → uses EmptyResponse path
	out, err := m.Generate(context.Background(), nil)
	require.NoError(t, err)
	assert.Empty(t, out.Content, "repeat with empty responses returns empty message")
}

// TestFakeModel_RecordMessagesGenerate tests RecordMessages with Generate.
func TestFakeModel_RecordMessagesGenerate(t *testing.T) {
	m := NewFakeModel([]string{"ok"}, nil)
	m.RecordMessages = true
	_, err := m.Generate(context.Background(), []*schema.Message{schema.UserMessage("hi")})
	require.NoError(t, err)
	require.NotEmpty(t, m.ReceivedMessages)
	assert.Equal(t, "hi", m.ReceivedMessages[0].Content)
}

// TestFakeModel_GenerateCallsCount proves GenerateCalls increments.
func TestFakeModel_GenerateCallsCount(t *testing.T) {
	m := NewFakeModel([]string{"a", "b"}, nil)
	_, _ = m.Generate(context.Background(), nil)
	_, _ = m.Generate(context.Background(), nil)
	assert.Equal(t, 2, m.GenerateCalls)
}

// TestFakeModel_StreamCallsCount proves StreamCalls increments.
func TestFakeModel_StreamCallsCount(t *testing.T) {
	m := NewFakeModel([]string{"a"}, nil)
	_, _ = m.Stream(context.Background(), nil)
	assert.Equal(t, 1, m.StreamCalls)
}

// ---------------------------------------------------------------------------
// Pricing edge cases
// ---------------------------------------------------------------------------

// TestComputeCost_ZeroValues proves cost computation with zero usage.
func TestComputeCost_ZeroValues(t *testing.T) {
	price := ModelPricing{InputPerM: 10, CacheHitPerM: 1, OutputPerM: 50}
	cost := computeCost(price, Usage{})
	assert.Equal(t, 0.0, cost)
}

// TestFormatCost_Negative proves negative cost returns N/A.
func TestFormatCost_Negative(t *testing.T) {
	assert.Equal(t, "N/A", FormatCost(-1, true))
}

// TestClampNonNeg_Positive proves positive values pass through.
func TestClampNonNeg_Positive(t *testing.T) {
	assert.Equal(t, 5, clampNonNeg(5))
}
