package orchestrator

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

// TestWrapCompaction_NoFallbacksLeavesSummarizerNil pins the W-C-10 no-op
// default: an empty/nil fallback chain (today's production shape — the
// shipped catalog declares zero fallback_models rows, Ruling RC-8) must leave
// CompactingModel.Summarizer nil, which einollm.CompactingModel.maybeCompact
// treats as "use Inner for summarization too" — byte-identical to
// pre-W-C-10 behavior. This is the mid-turn twin of
// ws_compaction_test.go::TestCompactionModel_NoFallbackChain_ReturnsUnwrapped.
func TestWrapCompaction_NoFallbacksLeavesSummarizerNil(t *testing.T) {
	fm := einollm.NewFakeModel([]string{"ok"}, nil)
	cc := CompactionConfig{Threshold: 0.8, ContextWindow: 128000, KeepRecent: 4}

	wrapped := wrapCompaction(fm, cc, 128000, 0, nil)
	cm, ok := wrapped.(*einollm.CompactingModel)
	require.True(t, ok, "threshold >0 must wrap in CompactingModel")
	assert.Nil(t, cm.Summarizer, "no declared fallback chain must leave Summarizer nil")
	assert.Same(t, model.BaseChatModel(fm), cm.Inner, "Inner stays the unwrapped primary model")
}

// TestWrapCompaction_WithFallbacksSetsResilientSummarizer pins the W-C-10
// load-bearing behavior on the mid-turn path: a non-empty fallbacks slice
// must produce a CompactingModel whose Summarizer is a fallback-aware
// *einollm.ResilientChatModel chaining [m, fallbacks...] — NOT a change to
// Inner, since only the summarization call may use a different model than
// the one answering the turn (see wrapCompaction's doc comment).
//
// The assertion is BEHAVIORAL, not just a type assertion: it drives
// Summarizer.Generate directly and proves the PRIMARY's response is ignored
// in favor of the FALLBACK's when the primary returns a non-retryable client
// error (invalid_api_key — see errclass.go's clientErrorMarkers), which
// ResilientChatModel fails over on WITHOUT the retry backoff a generic
// error would trigger, keeping this test fast.
func TestWrapCompaction_WithFallbacksSetsResilientSummarizer(t *testing.T) {
	primary := einollm.NewFakeModel(nil, errors.New("invalid_api_key"))
	fallback := einollm.NewFakeModel([]string{"fallback answered"}, nil)
	cc := CompactionConfig{Threshold: 0.8, ContextWindow: 128000, KeepRecent: 4}

	wrapped := wrapCompaction(primary, cc, 128000, 0, []model.BaseChatModel{fallback})
	cm, ok := wrapped.(*einollm.CompactingModel)
	require.True(t, ok, "threshold >0 must wrap in CompactingModel")
	require.NotNil(t, cm.Summarizer, "a non-empty fallback chain must set Summarizer")
	assert.Same(t, model.BaseChatModel(primary), cm.Inner,
		"Inner must stay the unwrapped primary — only the summarizer gets a fallback chain")

	resilient, ok := cm.Summarizer.(*einollm.ResilientChatModel)
	require.True(t, ok, "Summarizer must be the fallback-aware wrapper, not the bare primary")

	msg, err := resilient.Generate(context.Background(), []*schema.Message{
		{Role: schema.User, Content: "summarize this"},
	})
	require.NoError(t, err, "the chain must fail over to the fallback rather than surface the primary's error")
	assert.Equal(t, "fallback answered", msg.Content,
		"the fallback model's response must be what Summarizer.Generate returns")
	assert.Equal(t, 1, fallback.GenerateCalls, "the fallback must have been invoked exactly once")
}
