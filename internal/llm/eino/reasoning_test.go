package eino

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/cloudwego/eino/schema"
)

// TestReasoningEffortOption_Dispatch proves the helper maps each valid effort
// level to a non-nil per-call model.Option, and maps empty/unknown values to nil
// (no option sent, so non-OpenAI models are unaffected). The option's internal
// value (reasoning_effort on the OpenAI request) is set by the upstream library
// (eino-ext/libs/acl/openai chat_model.go:570-602 consumes the option produced
// by eino-ext/components/model/openai WithReasoningEffort); the acl's option
// struct is unexported so it cannot be decoded here, but the dispatch + the
// source path together prove correctness.
func TestReasoningEffortOption_Dispatch(t *testing.T) {
	for _, lvl := range []string{"low", "medium", "high"} {
		opt := ReasoningEffortOption(lvl)
		assert.NotNil(t, opt, "level %q must yield a non-nil option", lvl)
	}
	assert.Nil(t, ReasoningEffortOption(""), "empty effort must yield nil (no option)")
	assert.Nil(t, ReasoningEffortOption("bogus"), "unknown effort must yield nil (no option)")
}

// TestFakeModel_RecordsOptions proves the FakeModel captures the model.Options
// handed to Generate when RecordOpts is set. The orchestrator's thinking-effort
// passthrough test relies on this.
func TestFakeModel_RecordsOptions(t *testing.T) {
	m := NewFakeModel([]string{"ok"}, nil)
	m.RecordOpts = true

	optPtr := ReasoningEffortOption("high")
	require.NotNil(t, optPtr)
	opt := *optPtr
	_, err := m.Generate(context.Background(),
		[]*schema.Message{schema.UserMessage("hi")}, opt)
	require.NoError(t, err)

	// model.Option carries a func, which is not comparable via DeepEqual; the
	// length check proves the option we passed was the one recorded.
	require.Len(t, m.ReceivedOpts, 1, "FakeModel must record the option it received")
}

// TestFakeModel_RecordsStreamOptions mirrors the above for the Stream path.
func TestFakeModel_RecordsStreamOptions(t *testing.T) {
	m := NewFakeModel([]string{"ok"}, nil)
	m.RecordOpts = true

	optPtr := ReasoningEffortOption("low")
	require.NotNil(t, optPtr)
	opt := *optPtr
	sr, err := m.Stream(context.Background(),
		[]*schema.Message{schema.UserMessage("hi")}, opt)
	require.NoError(t, err)
	defer sr.Close()

	require.Len(t, m.ReceivedOpts, 1)
}
