package orchestrator

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSubAgentUsageForSink pins the mapping that carries a delegated turn's
// spend into the parent's budget.
//
// A dropped field here fails silently and permanently: no error, no missing
// event, the parent simply under-counts by that much and the budget runs
// longer than the operator asked for. Measured in W3 review round 5 — zeroing
// CompletionTokens left the whole suite green.
func TestSubAgentUsageForSink(t *testing.T) {
	t.Run("nothing spent reports nothing", func(t *testing.T) {
		assert.Nil(t, subAgentUsageForSink(TurnUsage{}),
			"a zero turn must not push a no-op entry into the sink")
	})

	t.Run("every field survives", func(t *testing.T) {
		got := subAgentUsageForSink(TurnUsage{PromptTokens: 90, CompletionTokens: 10})
		require.NotNil(t, got)
		assert.Equal(t, int64(90), got.PromptTokens)
		assert.Equal(t, int64(10), got.CompletionTokens)
		assert.Equal(t, int64(100), got.TotalTokens, "the total must equal the parts, not one of them")
	})

	t.Run("either field alone still reports", func(t *testing.T) {
		promptOnly := subAgentUsageForSink(TurnUsage{PromptTokens: 5})
		require.NotNil(t, promptOnly, "a prompt-only turn still spent tokens")
		assert.Equal(t, int64(5), promptOnly.TotalTokens)

		completionOnly := subAgentUsageForSink(TurnUsage{CompletionTokens: 7})
		require.NotNil(t, completionOnly, "a completion-only turn still spent tokens")
		assert.Equal(t, int64(7), completionOnly.TotalTokens)
	})
}
