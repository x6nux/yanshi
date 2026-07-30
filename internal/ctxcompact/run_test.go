// internal/ctxcompact/run_test.go
package ctxcompact_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	einollm "github.com/x6nux/yanshi/internal/llm/eino"
	"github.com/x6nux/yanshi/internal/ctxcompact"
)

func TestRun_FullPipeline(t *testing.T) {
	fm := einollm.NewFakeModel([]string{"the compacted summary"}, nil)
	// user msgs pin verbatim (isUserOriginal); assistant noise gets summarized.
	msgs := []*schema.Message{
		{Role: schema.User, Content: "do the task"},                           // 0 user (pin)
		{Role: schema.Assistant, Content: strings.Repeat("thinking ", 100)},   // 1 summarize
		{Role: schema.Assistant, Content: strings.Repeat("more noise ", 100)}, // 2 summarize
		{Role: schema.Assistant, Content: strings.Repeat("yet more ", 100)},   // 3 summarize
		{Role: schema.User, Content: "recent"},                                // 4 user+tail (pin)
		{Role: schema.Assistant, Content: "reply"},                            // 5 tail (pin)
	}
	res, err := ctxcompact.Run(context.Background(), msgs, ctxcompact.PlanOpts{KeepRecent: 1},
		ctxcompact.RunOpts{ModelWindow: 10000, ChunkThreshold: 0.9}, fm, nil)
	require.NoError(t, err)
	assert.True(t, ctxcompact.IsSummaryMessage(res.Messages[len(res.Messages)-1]), "ends with summary")
	assert.Contains(t, res.Messages[len(res.Messages)-1].Content, "the compacted summary")
	assert.Less(t, res.TokensAfter, res.TokensBefore)
}

func TestRun_FailureDoesNotProduceEmptySummary(t *testing.T) {
	// bug⑥: the old path returned an empty summary message on failure. Run must
	// return an error so callers fall back to the original history instead.
	fm := einollm.NewFakeModel(nil, errors.New("model down")) // non-transient -> immediate
	msgs := []*schema.Message{
		{Role: schema.User, Content: "task"},                                 // 0 user (pin)
		{Role: schema.Assistant, Content: strings.Repeat("noise ", 100)},     // 1 summarize
		{Role: schema.Assistant, Content: strings.Repeat("more ", 100)},      // 2 summarize
		{Role: schema.Assistant, Content: strings.Repeat("even more ", 100)}, // 3 summarize
		{Role: schema.User, Content: "recent"},                               // 4 user+tail
	}
	_, err := ctxcompact.Run(context.Background(), msgs, ctxcompact.PlanOpts{KeepRecent: 1},
		ctxcompact.RunOpts{ModelWindow: 10000, ChunkThreshold: 0.9}, fm, nil)
	require.Error(t, err, "failure surfaces as error, never an empty summary (bug⑥)")
}

func TestRun_NoOpWhenNothingToSummarize(t *testing.T) {
	fm := einollm.NewFakeModel([]string{"UNUSED"}, nil)
	// 2 msgs, KeepRecent=2 -> everything pinned -> nothing to summarize -> model never called.
	msgs := []*schema.Message{
		{Role: schema.User, Content: "hi"},
		{Role: schema.Assistant, Content: "yo"},
	}
	res, err := ctxcompact.Run(context.Background(), msgs, ctxcompact.PlanOpts{KeepRecent: 2},
		ctxcompact.RunOpts{ModelWindow: 10000}, fm, nil)
	require.NoError(t, err)
	assert.Equal(t, 0, fm.GenerateCalls+fm.StreamCalls, "no summary call when nothing to summarize")
	assert.Equal(t, len(msgs), len(res.Messages))
}
