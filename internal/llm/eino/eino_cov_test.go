package eino

import (
	"context"
	"testing"

	"github.com/cloudwego/eino/schema"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCov_FakeModel_JudgeProbe covers the judge-probe short-circuit: when the
// final message is the orchestrator's completion-judge question, Generate
// returns a fixed complete=true verdict without consuming a scripted response.
func TestCov_FakeModel_JudgeProbe(t *testing.T) {
	m := NewFakeModel([]string{"should-not-appear"}, nil)
	resp, err := m.Generate(context.Background(), []*schema.Message{
		schema.UserMessage("You are a completion judge. Decide whether the turn is complete."),
	})
	require.NoError(t, err)
	require.NotNil(t, resp)
	assert.Contains(t, resp.Content, `"complete":true`)
}
