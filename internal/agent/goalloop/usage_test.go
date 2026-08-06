package goalloop

import (
	"testing"

	"github.com/cloudwego/eino/schema"
	"github.com/stretchr/testify/assert"
)

// ledger: B0/TD1#1 UsageSink 逐次累计不覆盖不漏计
func TestUsageSink_AddAccumulates(t *testing.T) {
	t.Parallel()
	s := &UsageSink{}
	s.Add(Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15})
	s.Add(Usage{PromptTokens: 20, CompletionTokens: 7, TotalTokens: 27})
	got := s.Snapshot()
	assert.Equal(t, 30, got.PromptTokens, "prompt tokens sum across calls")
	assert.Equal(t, 12, got.CompletionTokens, "completion tokens sum across calls")
	assert.Equal(t, 42, got.TotalTokens, "total tokens sum across calls")
}

func TestUsage_SnapshotIsCopy(t *testing.T) {
	t.Parallel()
	s := &UsageSink{}
	s.Add(Usage{PromptTokens: 1, TotalTokens: 1})
	snap := s.Snapshot()
	snap.PromptTokens = 999 // mutating the snapshot must not touch the sink
	assert.Equal(t, 1, s.Snapshot().PromptTokens, "snapshot must be a copy")
}

func TestUsage_Total_FallsBackToInOutSum(t *testing.T) {
	t.Parallel()
	// When the provider omits TotalTokens (common for some gateways), Total()
	// falls back to prompt+completion so the budget check still works.
	u := Usage{PromptTokens: 40, CompletionTokens: 2, TotalTokens: 0}
	assert.Equal(t, 42, u.Total())
	u2 := Usage{PromptTokens: 40, CompletionTokens: 2, TotalTokens: 99}
	assert.Equal(t, 99, u2.Total(), "non-zero TotalTokens wins")
}

// ledger: B0/TD1#1 UsageSink 逐次累计不覆盖不漏计
func TestAddUsage_NilSafe(t *testing.T) {
	t.Parallel()
	// Components call addUsage unconditionally; a nil sink or nil message must
	// not panic.
	assert.NotPanics(t, func() {
		addUsage(nil, &schema.Message{})
		addUsage(&UsageSink{}, nil)
	})
}

// ledger: B0/TD1#1 UsageSink 逐次累计不覆盖不漏计
func TestUsageFromMeta(t *testing.T) {
	t.Parallel()
	msg := &schema.Message{ResponseMeta: &schema.ResponseMeta{
		Usage: &schema.TokenUsage{PromptTokens: 8, CompletionTokens: 3, TotalTokens: 11},
	}}
	assert.Equal(t, Usage{PromptTokens: 8, CompletionTokens: 3, TotalTokens: 11}, usageFromMeta(msg.ResponseMeta))

	assert.Equal(t, Usage{}, usageFromMeta(nil), "nil meta → zero usage")
	assert.Equal(t, Usage{}, usageFromMeta(&schema.ResponseMeta{}), "nil usage → zero usage")
}
