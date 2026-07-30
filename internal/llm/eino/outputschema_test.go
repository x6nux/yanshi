package eino

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/cloudwego/eino/schema"
)

// TestOutputSchemaOption_Dispatch proves the helper returns a non-nil option
// for a non-empty schema and nil for an empty schema (so the no-schema / text
// path forwards nothing and stays byte-identical to pre-A12).
func TestOutputSchemaOption_Dispatch(t *testing.T) {
	opt := OutputSchemaOption(json.RawMessage(`{"type":"object"}`))
	require.NotNil(t, opt, "non-empty schema must yield a non-nil option")

	assert.Nil(t, OutputSchemaOption(nil), "nil schema must yield nil")
	assert.Nil(t, OutputSchemaOption(json.RawMessage(``)), "empty schema must yield nil")
}

// TestOutputSchemaOption_TypeIsolation proves the eino-owned output-schema
// option and the openai-owned reasoning-effort option do NOT cross-contaminate:
// GetImplSpecificOptions[T] applies only the setters whose func type matches
// func(*T). This is the structural guarantee that the openai (chat completions,
// eino-ext) path — which decodes its own *openaiOptions — silently ignores our
// output-schema option ("openai 路径不受影响").
func TestOutputSchemaOption_TypeIsolation(t *testing.T) {
	schemaOpt := *OutputSchemaOption(json.RawMessage(`{"type":"object"}`))
	reasonOpt := ReasoningEffortOption("high") // openai WithReasoningEffort
	require.NotNil(t, reasonOpt)

	// Decoding as outputSchemaOptions picks up the schema, drops the effort.
	got := model.GetImplSpecificOptions(&outputSchemaOptions{}, schemaOpt, *reasonOpt)
	assert.Equal(t, `{"type":"object"}`, string(got.Schema),
		"output-schema decode must see the schema and ignore the openai option")

	// Decoding as outputSchemaOptions with ONLY the openai option sees nothing.
	got2 := model.GetImplSpecificOptions(&outputSchemaOptions{}, *reasonOpt)
	assert.Empty(t, got2.Schema, "openai option must not leak into output-schema decode")
}

// TestFakeModel_RecordsOutputSchema proves FakeModel captures the decoded
// schema from the per-call options when RecordOpts is set. The orchestrator
// forwarding test (Task 4) relies on this.
func TestFakeModel_RecordsOutputSchema(t *testing.T) {
	m := NewFakeModel([]string{"ok"}, nil)
	m.RecordOpts = true

	optPtr := OutputSchemaOption(json.RawMessage(`{"type":"object","properties":{"x":{"type":"integer"}}}`))
	require.NotNil(t, optPtr)
	_, err := m.Generate(context.Background(),
		[]*schema.Message{schema.UserMessage("hi")}, *optPtr)
	require.NoError(t, err)

	assert.Equal(t, `{"type":"object","properties":{"x":{"type":"integer"}}}`,
		string(m.ReceivedOutputSchema), "FakeModel must record the decoded output schema")

	// No option -> nothing recorded.
	m.ReceivedOutputSchema = nil
	_, _ = m.Generate(context.Background(), []*schema.Message{schema.UserMessage("hi")})
	assert.Empty(t, m.ReceivedOutputSchema, "a call without the option must not record a schema")
}
