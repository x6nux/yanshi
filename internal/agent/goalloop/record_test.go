package goalloop

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewRunRecord_FieldsAndReason(t *testing.T) {
	t.Parallel()
	sink := &UsageSink{}
	sink.Add(Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15})
	d := Decision{
		Complete:   false,
		Summary:    "budget exceeded after plan (215 tokens > 200)",
		StopReason: StopReasonTokenBudget,
		Usage:      sink.Snapshot(),
	}
	rec := NewRunRecord(TierDesigned, d, sink.Snapshot(), 3)
	assert.Equal(t, "designed", rec.Tier)
	assert.False(t, rec.Complete)
	assert.Equal(t, StopReasonTokenBudget, rec.StopReason)
	assert.Equal(t, 3, rec.Iterations)
	assert.Equal(t, 15, rec.Usage.TotalTokens)
	assert.Equal(t, "budget exceeded after plan (215 tokens > 200)", rec.Summary)
}

func TestRunRecord_RoundTripsJSON(t *testing.T) {
	t.Parallel()
	rec := NewRunRecord(
		TierAutonomous,
		Decision{Complete: true, Summary: "all 3 evaluators passed", StopReason: StopReasonComplete},
		Usage{PromptTokens: 1, CompletionTokens: 2, TotalTokens: 3},
		4,
	)
	data, err := json.Marshal(rec)
	require.NoError(t, err)

	var back RunRecord
	require.NoError(t, json.Unmarshal(data, &back))
	assert.Equal(t, rec, back, "record must survive a JSON round-trip")

	// The stop reason must be present in the wire form so a future reader of the
	// kv row can tell WHY the run ended without re-deriving it.
	assert.Contains(t, string(data), `"stop_reason"`)
	assert.Contains(t, string(data), `"tier":"autonomous"`)
}
