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
//
// Two gates stand between the model's reply and the replacement of history, and
// both fail in the SAME direction — no replacement, error out, let the caller
// keep the original history. That direction is not a style choice: a compaction
// that goes wrong destroys the middle of the conversation, and the callers' only
// sanity test (TokensAfter < TokensBefore) scores a destroyed history as the
// best possible outcome. Costing a wasted model call is the cheaper mistake
// every time.
//
//  1. EMPTY (bug⑥) — a blank summary wearing a success's clothes.
//  2. QUALITY (C10) — an acknowledgement or a refusal that is not blank, so
//     gate 1 lets it through. See CheckQuality.
//
// OVERFLOW (C9) IS NOT A GATE HERE, and that asymmetry is deliberate. It is
// reported on Result.Overflow and the compacted history is still returned. An
// error would make every caller fall back to the ORIGINAL history, which is
// strictly larger than the one that just failed to fit — turning "too big" into
// "even bigger". The refusal to send belongs to whoever sends; CheckContextLimit
// is the gate they call, and Result.Overflow is the same verdict pre-computed
// so they need not re-estimate.
func Run(ctx context.Context, msgs []*schema.Message, planOpts PlanOpts, runOpts RunOpts, m ModelSummarizer, onChunk func(string)) (*Result, error) {
	before := EstimateTokens(msgs)
	plan := Plan(msgs, planOpts)

	if len(plan.SummarizeIndices) == 0 {
		// Nothing to summarize (everything pinned, or already-summarized
		// history). Overflow is still reported: "compaction had nothing to do"
		// and "the result fits" are independent claims, and the shape most
		// likely to overflow is exactly the one where Plan pinned everything.
		//
		// C5 STILL RUNS HERE, and this is the branch where it matters most.
		// "Plan pinned everything" is precisely the history summarization
		// cannot help — there is nothing left to fold into a summary — and
		// before folding existed it was also the branch that returned the input
		// unchanged and let the caller send something that did not fit. Folding
		// is the only lever left at this point, and it is a no-op unless the
		// pressure is real.
		folded, foldStats := FoldToolResults(msgs, FoldOptions{Budget: budgetFor(runOpts)})
		after := EstimateTokens(folded)
		return &Result{
			Messages: folded, TokensBefore: before, TokensAfter: after,
			Fold:     foldStats,
			Overflow: checkOverflow(after, runOpts),
		}, nil
	}

	toSummarize := make([]*schema.Message, 0, len(plan.SummarizeIndices))
	for _, i := range plan.SummarizeIndices {
		// ⚠️ UNPINNED, and measured so rather than assumed. Round 27 replaced
		// this condition with a constant true and nothing reddened, including
		// a history deliberately holding a nil message: the summariser path
		// tolerates one, so the guard's effect is not observable from outside.
		// It stays as defence in depth against a Plan bug or a hand-assembled
		// PlanResult, but no test asserts it, and two attempts to write one
		// were discarded rather than committed as false coverage.
		if i >= 0 && i < len(msgs) && msgs[i] != nil {
			toSummarize = append(toSummarize, msgs[i])
		}
	}

	// Measured BEFORE the call, off the original messages, for the compression
	// floor in gate 2. It is deliberately the un-redacted length: redaction
	// replaces a secret with a shorter marker, and letting that shrink the
	// denominator would make the floor laxer on exactly the histories that
	// carried credentials.
	inputRunes := transcriptRunes(toSummarize)

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
	// C10. The reasons ride out on the error (SummaryRejectedError.Issues) so
	// the layer that logs this can name the rule that fired instead of
	// reporting a generic compaction failure.
	if err := CheckQuality(summary, inputRunes, runOpts.qualityPolicy()); err != nil {
		return nil, fmt.Errorf("compaction summary over %d messages: %w", len(toSummarize), err)
	}

	// C3. The eviction is recorded only AFTER both gates passed, and that
	// ordering is the whole correctness argument for the map. Recording before
	// the gates would advertise a span as evicted on a compaction the caller
	// then abandons — the messages are still in the live window, and the map
	// tells the model to go and read something it is already looking at, in a
	// structure that persists across turns and has no mechanism to retract an
	// entry. Recording after means the map only ever describes evictions that
	// happened.
	mapText := recordEviction(runOpts, summary)

	out := AssembleWithMap(msgs, plan, summary, mapText)
	// C5. Folding runs LAST, on the assembled result, and that position is the
	// whole design:
	//
	//   - AFTER Assemble, so it only ever touches PINNED tool results. The
	//     summarized ones are already gone — folding them would be work whose
	//     output is discarded a line later.
	//   - AFTER the summary, so the model's summary was built from the FULL
	//     text of everything it summarized. Folding first would hand the
	//     summarizer truncated inputs and bake the loss into the one artefact
	//     with unbounded lifetime.
	//   - Against the TURN's budget (budgetFor), not the summary call's: the
	//     pressure that matters is on the history about to be sent, which is
	//     what these messages now are.
	//
	// It is a no-op below FoldPressureThreshold, so an ordinary compaction is
	// unaffected. It exists for the history the summary alone could not save:
	// Plan legitimately pins a large working set, and a hundred pinned 10 KiB
	// tool results survive summarization untouched.
	out, foldStats := FoldToolResults(out, FoldOptions{Budget: budgetFor(runOpts)})
	after := EstimateTokens(out)
	// C9, measured on the ASSEMBLED result — the thing that will actually be
	// sent. Reported, not raised; see the doc above for why raising it would
	// make the caller forward something larger.
	return &Result{
		Messages: out, TokensBefore: before, TokensAfter: after,
		Fold:     foldStats,
		Overflow: checkOverflow(after, runOpts),
	}, nil
}

// transcriptRunes is the rune length of the text the summarizer is shown. It
// reuses SerializeForSummary so the denominator of the compression floor is
// measured over the SAME text the model reads — counting raw Content lengths
// instead would ignore tool calls and reasoning, which is most of what a
// tool-heavy ReAct history actually contains.
func transcriptRunes(msgs []*schema.Message) int {
	return len([]rune(SerializeForSummary(msgs)))
}

// recordEviction appends this compaction to the caller's eviction map and
// returns the rendered map, or "" when there is no map to render.
//
// TWO INDEPENDENT REASONS TO DO NOTHING, and they are checked separately
// because they mean different things. No map means the caller does not want
// one. An uncitable CoveredSeq means the caller has no persisted sequence
// numbers for these messages — the mid-turn path, whose history is still in
// flight — and recording a span nothing can resolve would fill a permanent
// structure with addresses history_read returns nothing for. AddEviction
// re-checks the second, so the guard here is documentation rather than the
// enforcement; the enforcement is in the map.
//
// The milestones come from the summary the model just wrote (C7): each bullet
// with a [seq:…] pointer is a labelled span. A summary that does not parse as
// the structured form yields none, and the eviction is recorded with a
// coarse "(no milestone)" entry instead of being dropped — the span existing
// is more important than the span being labelled.
func recordEviction(runOpts RunOpts, summary string) string {
	if runOpts.EvictionMap == nil {
		return ""
	}
	if !runOpts.CoveredSeq.citable() {
		return ""
	}
	runOpts.EvictionMap.AddEviction(
		MilestonesFromSummary(summary, runOpts.CoveredSeq),
		runOpts.CoveredSeq,
	)
	return runOpts.EvictionMap.Render(runOpts.EvictionMapBudget)
}
