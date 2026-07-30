package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	einollm "github.com/x6nux/yanshi/internal/llm/eino"
)

var errBoom = errors.New("boom")

// TestRunDAGWorkflow_InvalidJSON covers the first json.Unmarshal error branch
// at agent_dag.go:67-72. Input is neither a JSON object nor a JSON string.
func TestRunDAGWorkflow_InvalidJSON(t *testing.T) {
	tools := NewAgentTools(einollm.NewFakeModel([]string{"ok"}, nil))
	out, err := tools.runDAGWorkflow(context.Background(), json.RawMessage(`not-json-at-all`))
	require.NoError(t, err)
	assert.Contains(t, out, "must be a JSON object")
}

// TestRunDAGWorkflow_StringNotObject covers the inner-parse error branch at
// agent_dag.go:73-75. Input is a valid JSON string but its content is not a
// valid JSON object with steps.
func TestRunDAGWorkflow_StringNotObject(t *testing.T) {
	tools := NewAgentTools(einollm.NewFakeModel([]string{"ok"}, nil))
	encoded, _ := json.Marshal(`just a plain string`)
	out, err := tools.runDAGWorkflow(context.Background(), json.RawMessage(encoded))
	require.NoError(t, err)
	assert.Contains(t, out, "valid JSON object")
}

// TestRunDAGWorkflow_NoChatModel covers the chatModel==nil branch at
// agent_dag.go:80-82.
func TestRunDAGWorkflow_NoChatModel(t *testing.T) {
	tools := NewAgentTools(nil)
	out, err := tools.runDAGWorkflow(context.Background(), json.RawMessage(`{"steps":[{"id":"A1","prompt":"x"}]}`))
	require.NoError(t, err)
	assert.Contains(t, out, "no chat model")
}

// TestRunDAGWorkflow_EmptySteps covers the zero-steps branch at agent_dag.go:77-78.
func TestRunDAGWorkflow_EmptySteps(t *testing.T) {
	tools := NewAgentTools(einollm.NewFakeModel([]string{"ok"}, nil))
	out, err := tools.runDAGWorkflow(context.Background(), json.RawMessage(`{"steps":[]}`))
	require.NoError(t, err)
	assert.Contains(t, out, "at least one step")
}

// TestRunDAGWorkflow_StringEncodedValid covers the string-encoded success path
// at agent_dag.go:70-73 (the inner json.Unmarshal succeeds).
func TestRunDAGWorkflow_StringEncodedValid(t *testing.T) {
	tools := NewAgentTools(einollm.NewFakeModel([]string{"ok"}, nil))
	encoded, _ := json.Marshal(`{"steps":[{"id":"A1","prompt":"do x"}]}`)
	out, err := tools.runDAGWorkflow(context.Background(), json.RawMessage(encoded))
	require.NoError(t, err)
	// Should contain a result for step A1 (not an error message).
	assert.False(t, strings.Contains(out, "must be") && strings.Contains(out, "error"),
		"expected workflow result, got error: %s", out)
}

// TestRunDAGWorkflow_StepError covers the per-step error branches at
// agent_dag.go:143-144 and 155-156. A model that always errors makes every
// step fail, so the output carries "✗ <error>" per step.
func TestRunDAGWorkflow_StepError(t *testing.T) {
	tools := NewAgentTools(einollm.NewFakeModel(nil, errBoom))
	out, err := tools.runDAGWorkflow(context.Background(),
		json.RawMessage(`{"steps":[{"id":"A1","prompt":"do x"}]}`))
	require.NoError(t, err)
	assert.Contains(t, out, "✗")
}

// TestRunDAGWorkflow_MultiStepSuccess covers a multi-step workflow with
// dependencies to exercise topoSortLevels + executeLevel with real levels.
func TestRunDAGWorkflow_MultiStepSuccess(t *testing.T) {
	m := einollm.NewFakeModel([]string{"step-result"}, nil)
	m.Repeat = true // same response for every step
	tools := NewAgentTools(m)
	out, err := tools.runDAGWorkflow(context.Background(),
		json.RawMessage(`{"steps":[{"id":"A1","prompt":"first"},{"id":"B1","prompt":"second {{A1.output}}","deps":["A1"]}]}`))
	require.NoError(t, err)
	assert.Contains(t, out, "A1:")
	assert.Contains(t, out, "B1:")
}
