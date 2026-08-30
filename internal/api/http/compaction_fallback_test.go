package http

import (
	"context"
	"errors"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	einollm "github.com/x6nux/yanshi/internal/llm/eino"
)

// TestCompactionModel_NoFallbackChain_ReturnsUnwrapped pins the W-C-10 no-op
// default on the PRE-TURN path: with cc.ProviderFallbacks nil or missing an
// entry for the resolved key, compactionModel must return the primary model
// UNCHANGED (same pointer identity) — not merely equal, but the exact value,
// so callers that type-assert on a specific model (none do today, but the
// contract is "unwrapped") see no difference from before W-C-10. This is the
// pre-turn twin of orchestrator's TestWrapCompaction_NoFallbacksLeavesSummarizerNil.
func TestCompactionModel_NoFallbackChain_ReturnsUnwrapped(t *testing.T) {
	primary := einollm.NewFakeModel([]string{"ok"}, nil)
	models := map[string]model.BaseChatModel{"gpt-4o": primary}
	cc := CompactionConfig{Model: "gpt-4o"}

	got := compactionModel(cc, models, "")
	assert.Same(t, model.BaseChatModel(primary), got,
		"no declared fallback chain must return the primary model unwrapped")
}

// TestCompactionModel_CcModelBranchFailsOverToFallback exercises the FIRST of
// compactionModel's three return points (cc.Model set and registered): a
// declared ProviderFallbacks entry for that key must wrap the primary in a
// fallback chain that a caller driving Generate actually observes failing
// over. Uses a non-retryable client error (invalid_api_key) so the chain
// fails over immediately, without the retry backoff a generic error would
// trigger — keeping this test fast, matching
// orchestrator's TestWrapCompaction_WithFallbacksSetsResilientSummarizer.
func TestCompactionModel_CcModelBranchFailsOverToFallback(t *testing.T) {
	primary := einollm.NewFakeModel(nil, errors.New("invalid_api_key"))
	fallback := einollm.NewFakeModel([]string{"fallback answered"}, nil)
	models := map[string]model.BaseChatModel{"gpt-4o": primary}
	cc := CompactionConfig{
		Model:             "gpt-4o",
		ProviderFallbacks: map[string][]model.BaseChatModel{"gpt-4o": {fallback}},
	}

	got := compactionModel(cc, models, "")
	msg, err := got.Generate(context.Background(), []*schema.Message{
		{Role: schema.User, Content: "summarize"},
	})
	require.NoError(t, err, "the chain must fail over to the fallback rather than surface the primary's error")
	assert.Equal(t, "fallback answered", msg.Content)
	assert.Equal(t, 1, fallback.GenerateCalls)
}

// TestCompactionModel_SessionModelBranchFailsOverToFallback exercises the
// SECOND return point (cc.Model unset, sessionModel set and registered) —
// distinct from the first because a mutation could wire fallback-wrapping
// into only one of the three branches and still pass the first test.
func TestCompactionModel_SessionModelBranchFailsOverToFallback(t *testing.T) {
	primary := einollm.NewFakeModel(nil, errors.New("invalid_api_key"))
	fallback := einollm.NewFakeModel([]string{"fallback answered"}, nil)
	models := map[string]model.BaseChatModel{"claude-x": primary}
	cc := CompactionConfig{
		ProviderFallbacks: map[string][]model.BaseChatModel{"claude-x": {fallback}},
	}

	got := compactionModel(cc, models, "claude-x")
	msg, err := got.Generate(context.Background(), []*schema.Message{
		{Role: schema.User, Content: "summarize"},
	})
	require.NoError(t, err)
	assert.Equal(t, "fallback answered", msg.Content)
}

// TestCompactionModel_SortedFirstBranchFailsOverToFallback exercises the
// THIRD return point: neither cc.Model nor sessionModel set, so
// compactionModel falls back to the deterministic sorted-first registered
// name. A fallback chain declared for THAT resolved key must still apply —
// this is the branch most likely to be missed by a wrapping fix that only
// patches the two explicit-key branches, since it is reached through a loop
// and a sort rather than a direct map lookup.
func TestCompactionModel_SortedFirstBranchFailsOverToFallback(t *testing.T) {
	primary := einollm.NewFakeModel(nil, errors.New("invalid_api_key"))
	fallback := einollm.NewFakeModel([]string{"fallback answered"}, nil)
	models := map[string]model.BaseChatModel{
		"a-model": primary, // sorts first
		"z-model": einollm.NewFakeModel([]string{"unused"}, nil),
	}
	cc := CompactionConfig{
		ProviderFallbacks: map[string][]model.BaseChatModel{"a-model": {fallback}},
	}

	got := compactionModel(cc, models, "")
	msg, err := got.Generate(context.Background(), []*schema.Message{
		{Role: schema.User, Content: "summarize"},
	})
	require.NoError(t, err)
	assert.Equal(t, "fallback answered", msg.Content)
}
