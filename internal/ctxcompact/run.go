// internal/ctxcompact/run.go
package ctxcompact

import (
	"context"
	"fmt"
	"strings"

	"github.com/cloudwego/eino/schema"
)

// Run is the unified compaction entry both paths (mid-turn CompactingModel and
// pre-turn MaybeCompact) delegate to. It Plans, summarizes the summarize set,
// and assembles the result. On summary failure it returns an error — callers
// decide (mid-turn falls back to original msgs, pre-turn keeps history and
// warns) — it NEVER produces an empty summary (bug⑥).
func Run(ctx context.Context, msgs []*schema.Message, planOpts PlanOpts, runOpts RunOpts, m ModelSummarizer, onChunk func(string)) (*Result, error) {
	before := EstimateTokens(msgs)
	plan := Plan(msgs, planOpts)

	if len(plan.SummarizeIndices) == 0 {
		// nothing to summarize (everything pinned, or already-summarized history).
		return &Result{Messages: msgs, TokensBefore: before, TokensAfter: before}, nil
	}

	toSummarize := make([]*schema.Message, 0, len(plan.SummarizeIndices))
	for _, i := range plan.SummarizeIndices {
		if i >= 0 && i < len(msgs) && msgs[i] != nil {
			toSummarize = append(toSummarize, msgs[i])
		}
	}

	summary, err := RunSummary(ctx, toSummarize, runOpts, m, onChunk)
	if err != nil {
		return nil, fmt.Errorf("compaction summary: %w", err)
	}
	if strings.TrimSpace(summary) == "" {
		// An empty summary is a failed summarization wearing a success's
		// clothes. Assemble REPLACES the summarized messages with the summary,
		// so proceeding drops them and leaves nothing in their
		// place -- and the callers' best-effort gate cannot catch it, because
		// TokensAfter < TokensBefore is exactly what a truncation looks like.
		// Erroring here makes MaybeCompact and CompactingModel keep the
		// original history, which costs one wasted model call instead of the
		// middle of the conversation.
		return nil, fmt.Errorf("compaction summary: summarizer returned nothing for %d messages", len(toSummarize))
	}

	out := Assemble(msgs, plan, summary)
	return &Result{Messages: out, TokensBefore: before, TokensAfter: EstimateTokens(out)}, nil
}
