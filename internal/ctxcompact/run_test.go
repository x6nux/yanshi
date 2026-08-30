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

	"github.com/x6nux/yanshi/internal/ctxcompact"
	einollm "github.com/x6nux/yanshi/internal/llm/eino"
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

func TestRun_ModelFailureFallsBackToPinsOnly(t *testing.T) {
	// W-C-04: a summary MODEL failure (the call itself never produced a
	// summary) no longer surfaces as an error — Run falls back to the pinned
	// messages and opens a new window directly. bug⑥'s original guarantee
	// (never fabricate an EMPTY summary) is unaffected by this: it is still
	// enforced one layer down, in RunSummary itself — see
	// TestRunSummary_FailureReturnsError in summarize_test.go, which calls
	// RunSummary directly and still asserts require.Error.
	fm := einollm.NewFakeModel(nil, errors.New("model down")) // non-transient -> immediate
	msgs := []*schema.Message{
		{Role: schema.User, Content: "task"},                                 // 0 user (pin)
		{Role: schema.Assistant, Content: strings.Repeat("noise ", 100)},     // 1 summarize
		{Role: schema.Assistant, Content: strings.Repeat("more ", 100)},      // 2 summarize
		{Role: schema.Assistant, Content: strings.Repeat("even more ", 100)}, // 3 summarize
		{Role: schema.User, Content: "recent"},                               // 4 user+tail
	}
	res, err := ctxcompact.Run(context.Background(), msgs, ctxcompact.PlanOpts{KeepRecent: 1},
		ctxcompact.RunOpts{ModelWindow: 10000, ChunkThreshold: 0.9}, fm, nil)
	require.NoError(t, err, "model failure is a fallback, not an error")
	assert.True(t, res.Fallback, "Result.Fallback must be set on the fallback path")
	assert.Less(t, res.TokensAfter, res.TokensBefore, "fallback still shrinks the history")
	for _, m := range res.Messages {
		assert.False(t, ctxcompact.IsSummaryMessage(m), "fallback path writes no summary message — there is nothing to summarize with")
	}
}

// TestRun_ModelFailureFiresFallbackNoticeOnChunk pins the observability half
// of W-C-04: onChunk must fire with ctxcompact.FallbackNotice so the caller's
// activity line can distinguish "opened a new window with no summary" from an
// ordinary summary delta. onChunk is never invoked from inside the failed
// model call itself here (streamOnce only calls onChunk on a path that
// produced content — see summarize.go), so every recorded chunk in this test
// comes from Run's own explicit call, isolating exactly the behaviour W-C-04
// added.
func TestRun_ModelFailureFiresFallbackNoticeOnChunk(t *testing.T) {
	fm := einollm.NewFakeModel(nil, errors.New("model down"))
	msgs := []*schema.Message{
		{Role: schema.User, Content: "task"},
		{Role: schema.Assistant, Content: strings.Repeat("noise ", 100)},
		{Role: schema.Assistant, Content: strings.Repeat("more ", 100)},
		{Role: schema.User, Content: "recent"},
	}
	var chunks []string
	_, err := ctxcompact.Run(context.Background(), msgs, ctxcompact.PlanOpts{KeepRecent: 1},
		ctxcompact.RunOpts{ModelWindow: 10000, ChunkThreshold: 0.9}, fm,
		func(s string) { chunks = append(chunks, s) })
	require.NoError(t, err)
	require.Len(t, chunks, 1, "exactly one onChunk call on the fallback path")
	assert.Equal(t, ctxcompact.FallbackNotice, chunks[0])
}

// TestRun_ModelFailurePreservesEveryPinCategory is the W-C-04 pin-preservation
// guarantee: the spec's own words are "兜底路径不丢 pin 的消息" (the fallback
// path must not drop pinned messages). It exercises all five categories
// Plan.PinnedIndices can hold in one fixture — tail (KeepRecent), user
// original intent, working-set path mention, error marker, and diff marker —
// under a model that fails outright, and asserts every one of them survives
// the fallback verbatim, in original order, alongside every summarize-only
// message being GONE (proving the fallback actually discarded the
// summarize-set rather than merely declining to error).
func TestRun_ModelFailurePreservesEveryPinCategory(t *testing.T) {
	fm := einollm.NewFakeModel(nil, errors.New("model down"))
	msgs := []*schema.Message{
		{Role: schema.User, Content: "edit internal/ctxcompact/compact.go please"}, // 0 working-set (pin)
		{Role: schema.User, Content: "first request, please keep this"},            // 1 user original (pin)
		{Role: schema.Assistant, Content: "error: undefined: Foo"},                 // 2 error marker (pin)
		{Role: schema.Assistant, Content: "--- a/x.go\n+++ b/x.go\n@@ -1 +1 @@"},   // 3 diff marker (pin)
		{Role: schema.Assistant, Content: strings.Repeat("noise ", 100)},           // 4 summarize (DROPPED)
		{Role: schema.Assistant, Content: strings.Repeat("more noise ", 100)},      // 5 tail (pin)
		{Role: schema.User, Content: "recent"},                                     // 6 tail (pin)
	}
	res, err := ctxcompact.Run(context.Background(), msgs, ctxcompact.PlanOpts{KeepRecent: 1},
		ctxcompact.RunOpts{ModelWindow: 10000, ChunkThreshold: 0.9}, fm, nil)
	require.NoError(t, err)
	require.True(t, res.Fallback)

	contents := make([]string, len(res.Messages))
	for i, m := range res.Messages {
		contents[i] = m.Content
	}
	assert.Contains(t, contents, msgs[0].Content, "working-set path mention survives")
	assert.Contains(t, contents, msgs[1].Content, "user original intent survives")
	assert.Contains(t, contents, msgs[2].Content, "error marker survives")
	assert.Contains(t, contents, msgs[3].Content, "diff marker survives")
	assert.Contains(t, contents, msgs[5].Content, "tail (KeepRecent) survives")
	assert.Contains(t, contents, msgs[6].Content, "tail (KeepRecent) survives")
	assert.NotContains(t, contents, msgs[4].Content, "summarize-only content is actually discarded by the fallback, not merely un-erred")

	// Original order is preserved, not just presence — pinsOnlyResult reuses
	// pinnedMessages, whose contract (assemble.go) is ascending PinnedIndices
	// order.
	var order []int
	for _, c := range contents {
		for i, orig := range msgs {
			if orig.Content == c {
				order = append(order, i)
				break
			}
		}
	}
	for i := 1; i < len(order); i++ {
		assert.Less(t, order[i-1], order[i], "pinned messages must stay in original order")
	}
}

// TestRun_ConfigFailureIsNotFallback is M-3 (2026-08-29 review): a
// credential/wiring failure — the summarizer never sent a request because it
// could not obtain its own auth.command credential — must NOT take the same
// W-C-04 pins-only fallback path TestRun_ModelFailureFallsBackToPinsOnly
// (above) proves an ordinary model failure takes. The review's own warning
// was that this package had previously let two different failure causes
// collapse into "the same code" (an earlier finding, unrelated to this one);
// this test is the mechanical check that they no longer do: the ONLY
// difference between this fixture and TestRun_ModelFailureFallsBackToPinsOnly
// is the error text the fake model returns (an auth.command marker here vs.
// "model down" there), and that difference alone must flip every one of
// require.Error/Fallback/onChunk. Deleting isConfigOrWiringFailure's check in
// run.go (reverting to the pre-M-3 unconditional fallback) turns this test's
// require.Error into a failure, since Run would then return (result, nil)
// exactly like the model-failure case.
func TestRun_ConfigFailureIsNotFallback(t *testing.T) {
	fm := einollm.NewFakeModel(nil, errors.New(
		"eino: fetching auth.command credential: eino: auth.command launch: secproc: no Factory in context (fail-closed)"))
	msgs := []*schema.Message{
		{Role: schema.User, Content: "task"},
		{Role: schema.Assistant, Content: strings.Repeat("noise ", 100)},
		{Role: schema.Assistant, Content: strings.Repeat("more ", 100)},
		{Role: schema.User, Content: "recent"},
	}
	var chunks []string
	res, err := ctxcompact.Run(context.Background(), msgs, ctxcompact.PlanOpts{KeepRecent: 1},
		ctxcompact.RunOpts{ModelWindow: 10000, ChunkThreshold: 0.9}, fm,
		func(s string) { chunks = append(chunks, s) })
	require.Error(t, err, "a credential/wiring failure must be reported, not silently swallowed into a fallback")
	assert.Nil(t, res, "no Result on this path — callers must fall back to their OWN original history, not a partial one")
	assert.Contains(t, err.Error(), "auth.command", "the underlying cause must still be legible in the returned error")
	assert.Empty(t, chunks, "no FallbackNotice — this path never reaches the W-C-04 onChunk call")
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

// TestOpenNewWindow_NeverCallsAModel is W-C-14's structural pin: unlike
// Run/RunSummary, OpenNewWindow's signature takes no ModelSummarizer
// argument at all — there is nothing here that COULD call a model, which is
// the whole acceptance criterion ("模型可不摘要直接开新窗"). The fixture is
// deliberately the exact same shape as TestRun_ModelFailureFallsBackToPinsOnly
// (same pin categories, same summarize-only noise), proving OpenNewWindow
// produces the identical fallback SHAPE by construction (it shares
// pinsOnlyResult) without needing a model to fail first.
func TestOpenNewWindow_NeverCallsAModel(t *testing.T) {
	msgs := []*schema.Message{
		{Role: schema.User, Content: "task"},                                 // 0 user (pin)
		{Role: schema.Assistant, Content: strings.Repeat("noise ", 100)},     // 1 summarize (DROPPED)
		{Role: schema.Assistant, Content: strings.Repeat("more ", 100)},      // 2 summarize (DROPPED)
		{Role: schema.Assistant, Content: strings.Repeat("even more ", 100)}, // 3 summarize (DROPPED)
		{Role: schema.User, Content: "recent"},                               // 4 user+tail (pin)
	}
	res := ctxcompact.OpenNewWindow(msgs, ctxcompact.PlanOpts{KeepRecent: 1},
		ctxcompact.RunOpts{ModelWindow: 10000, ChunkThreshold: 0.9})
	assert.True(t, res.Fallback, "OpenNewWindow shares the W-C-04 fallback Result shape")
	assert.Less(t, res.TokensAfter, res.TokensBefore, "the summarize-only messages are dropped, not kept")
	for _, m := range res.Messages {
		assert.False(t, ctxcompact.IsSummaryMessage(m), "no summary was ever produced — there was no model call to produce one")
	}
}

// TestOpenNewWindow_PreservesEveryPinCategory is TestRun_ModelFailurePreservesEveryPinCategory's
// twin for the proactive path: same five pin categories (tail, user
// original, working-set path, error marker, diff marker), same survival
// requirement, reached without a failing model — proving W-C-14's guarantee
// ("模型可不摘要直接开新窗" must not cost the pinned messages) independently
// of W-C-04's model-failure fixture.
func TestOpenNewWindow_PreservesEveryPinCategory(t *testing.T) {
	msgs := []*schema.Message{
		{Role: schema.User, Content: "edit internal/ctxcompact/compact.go please"}, // 0 working-set (pin)
		{Role: schema.User, Content: "first request, please keep this"},            // 1 user original (pin)
		{Role: schema.Assistant, Content: "error: undefined: Foo"},                 // 2 error marker (pin)
		{Role: schema.Assistant, Content: "--- a/x.go\n+++ b/x.go\n@@ -1 +1 @@"},   // 3 diff marker (pin)
		{Role: schema.Assistant, Content: strings.Repeat("noise ", 100)},           // 4 summarize (DROPPED)
		{Role: schema.Assistant, Content: strings.Repeat("more noise ", 100)},      // 5 tail (pin)
		{Role: schema.User, Content: "recent"},                                     // 6 tail (pin)
	}
	res := ctxcompact.OpenNewWindow(msgs, ctxcompact.PlanOpts{KeepRecent: 1},
		ctxcompact.RunOpts{ModelWindow: 10000, ChunkThreshold: 0.9})
	require.True(t, res.Fallback)

	contents := make([]string, len(res.Messages))
	for i, m := range res.Messages {
		contents[i] = m.Content
	}
	assert.Contains(t, contents, msgs[0].Content, "working-set path mention survives")
	assert.Contains(t, contents, msgs[1].Content, "user original intent survives")
	assert.Contains(t, contents, msgs[2].Content, "error marker survives")
	assert.Contains(t, contents, msgs[3].Content, "diff marker survives")
	assert.Contains(t, contents, msgs[5].Content, "tail (KeepRecent) survives")
	assert.Contains(t, contents, msgs[6].Content, "tail (KeepRecent) survives")
	assert.NotContains(t, contents, msgs[4].Content, "summarize-only content is actually discarded, not merely un-erred")
}
