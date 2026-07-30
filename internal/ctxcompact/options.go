// internal/ctxcompact/options.go
package ctxcompact

import (
	"context"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

// ModelSummarizer is the narrow contract RunSummary needs from a chat model:
// just Generate + Stream. It mirrors model.BaseChatModel's two methods exactly
// (the compile-time assertion in options_test.go proves any BaseChatModel
// satisfies it — real remote model, einollm.FakeModel, or the CompactingModel's
// inner). Defining it at the consumer (this package) follows Go's "accept
// interfaces" idiom and pins the minimal surface the core depends on, rather
// than naming the full BaseChatModel type throughout.
type ModelSummarizer interface {
	Generate(ctx context.Context, msgs []*schema.Message, opts ...model.Option) (*schema.Message, error)
	Stream(ctx context.Context, msgs []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error)
}

// PlanOpts configures the pure Plan function (no IO).
type PlanOpts struct {
	// KeepRecent is the number of trailing messages kept verbatim — Plan pins
	// the last 2*KeepRecent messages regardless of role (the tail may include
	// tool messages, not strictly user/assistant pairs). EnforceToolCallPairs
	// then repairs any pair severed by the tail cut.
	KeepRecent int
}

// PlanResult is the pinned/summarize split Plan produces.
type PlanResult struct {
	// PinnedIndices are ASCENDING message indices kept verbatim. Assemble
	// iterates them in order to preserve original history order, so producers
	// must keep them sorted ascending.
	PinnedIndices []int
	// SummarizeIndices are the remaining indices (also ascending) whose content
	// is folded into the summary.
	SummarizeIndices []int
	WorkingSetPaths  []string
}

// RunOpts configures Run / RunSummary (carries the summary-model window).
type RunOpts struct {
	// ModelWindow is the summary model's context window (tokens), used to pick
	// single-vs-chunked and chunk boundaries. From provider.context_window.
	ModelWindow int
	// ChunkThreshold is the fraction of ModelWindow at which RunSummary switches
	// from single cache-aligned call to carry-style chunking. The config layer
	// (Task 13) defaults a zero value to 0.9; HERE, a zero value disables the
	// chunking guard — RunSummary falls back to the full ModelWindow as the
	// chunk budget.
	ChunkThreshold float64
	// SummaryWordLimit caps the summary length the model is asked for.
	SummaryWordLimit int
}

// Result is what Run returns on success.
type Result struct {
	// Messages is the compacted history: pinned messages verbatim in original
	// order, followed by a sentinel-prefixed user-role summary at the tail.
	Messages     []*schema.Message
	TokensBefore int
	TokensAfter  int
}
