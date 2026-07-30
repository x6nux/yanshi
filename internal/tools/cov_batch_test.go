package tools

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// runAgentBatch — error paths
// ---------------------------------------------------------------------------

func TestRunAgentBatch_InvalidOuterJSON(t *testing.T) {
	set := NewBatchTools(nil)
	ch := runAgentBatch(context.Background(), set, `not-json`)
	result := readAutomationChunkT(t, ch)
	assert.NotEmpty(t, result)
}

func TestRunAgentBatch_InvalidInnerJSON(t *testing.T) {
	set := NewBatchTools(nil)
	ch := runAgentBatch(context.Background(), set, `{"input":"not-json"}`)
	result := readAutomationChunkT(t, ch)
	assert.NotEmpty(t, result)
}

func TestRunAgentBatch_EmptyPrompt(t *testing.T) {
	set := NewBatchTools(nil)
	ch := runAgentBatch(context.Background(), set, `{"input":"{\"rows\":[{\"k\":\"v\"}]}"}`)
	result := readAutomationChunkT(t, ch)
	assert.Contains(t, result, "prompt is required")
}

func TestRunAgentBatch_BothCSVAndRows(t *testing.T) {
	set := NewBatchTools(nil)
	// Use escaped JSON without raw newlines
	ch := runAgentBatch(context.Background(), set, `{"input":"{\"prompt\":\"x\",\"csv\":\"a,b\",\"rows\":[{\"k\":\"v\"}]}"}`)
	result := readAutomationChunkT(t, ch)
	assert.Contains(t, result, "exactly one of")
}

func TestRunAgentBatch_NeitherCSVNorRows(t *testing.T) {
	set := NewBatchTools(nil)
	ch := runAgentBatch(context.Background(), set, `{"input":"{\"prompt\":\"x\"}"}`)
	result := readAutomationChunkT(t, ch)
	assert.Contains(t, result, "exactly one of")
}

func TestRunAgentBatch_NoRunner(t *testing.T) {
	set := NewBatchTools(nil)
	ch := runAgentBatch(context.Background(), set, `{"input":"{\"prompt\":\"x\",\"rows\":[{\"k\":\"v\"}]}"}`)
	result := readAutomationChunkT(t, ch)
	assert.Contains(t, result, "runner")
}

func TestRunAgentBatch_NoManager(t *testing.T) {
	set := NewBatchTools(nil)
	runner := SubAgentRunner(func(ctx context.Context, prompt string, allowed []string, instr string) (string, error) {
		return "ok", nil
	})
	ctx := WithSubAgentRunner(context.Background(), runner)
	ch := runAgentBatch(ctx, set, `{"input":"{\"prompt\":\"x\",\"rows\":[{\"k\":\"v\"}]}"}`)
	result := readAutomationChunkT(t, ch)
	assert.Contains(t, result, "manager")
}

func TestRunAgentBatch_BadCSV(t *testing.T) {
	set := NewBatchTools(nil)
	ch := runAgentBatch(context.Background(), set, `{"input":"{\"prompt\":\"x\",\"csv\":\"\"}"}`)
	result := readAutomationChunkT(t, ch)
	// empty csv string is treated as no csv → error about exactly one
	assert.NotEmpty(t, result)
}

func TestRunAgentBatch_Success(t *testing.T) {
	mgr, _ := newTestManager(t)
	set := NewBatchTools(mgr)
	runner := SubAgentRunner(func(ctx context.Context, prompt string, allowed []string, instr string) (string, error) {
		return "ok: " + prompt, nil
	})
	ctx := WithSubAgentRunner(context.Background(), runner)
	ch := runAgentBatch(ctx, set, `{"input":"{\"prompt\":\"test\",\"rows\":[{\"name\":\"a\"},{\"name\":\"b\"}]}"}`)
	result := readAutomationChunkT(t, ch)
	require.NotEmpty(t, result, "expected non-empty result")
}
