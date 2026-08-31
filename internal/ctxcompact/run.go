// internal/ctxcompact/run.go
package ctxcompact

import (
	"context"
	"fmt"
	"strings"

	"github.com/cloudwego/eino/schema"
)

// FallbackNotice is the onChunk text Run sends on the W-C-04 fallback path —
// RunSummary could not obtain a summary at all (the model call itself failed:
// exhausted retries, quota, overload, whatever the underlying provider chain
// gave up on) and Run opened a new window directly instead of calling the
// model again. Both transports (WS's compact_chunk frame, SSE's mirror)
// forward onChunk text verbatim, so this string travels to the client
// unchanged; internal/cli/tui/model.go prefix-matches it to render a
// fallback-specific activity line rather than the generic "Compacting
// context…" every other chunk produces. Exported (not a private const)
// because the consumer lives in a different package, mirroring
// SummarySentinel's existing cross-package pattern.
const FallbackNotice = "[compaction-fallback] "

// NewWindowNotice is the onChunk text OpenNewWindow's callers send on the
// W-C-14 proactive path — the MODEL asked for a fresh window (via the
// context_new_window tool) rather than Run hitting a summary-model failure.
// Same transport as FallbackNotice (both travel over the compact_chunk
// frame verbatim), deliberately a DIFFERENT string so
// internal/cli/tui/model.go can render a distinct activity line: a model
// choosing to skip summarization is a different event from summarization
// having failed, even though both land on the same pins-only Result shape.
const NewWindowNotice = "[compaction-new-window] "

// Run is the unified compaction entry both paths (mid-turn CompactingModel and
// pre-turn MaybeCompact) delegate to. It Plans, summarizes the summarize set,
// and assembles the result.
//
// On a summary MODEL failure (RunSummary's own error — the call itself never
// produced a summary) Run does NOT return an error. It falls back (W-C-04):
// discard the summarize-set history, keep only what Plan pinned, and report
// Fallback=true on the Result. This is deliberately narrower than "any
// failure downstream of RunSummary" — the two gates below (EMPTY, QUALITY)
// still return an error, because those cases DID get a reply from the model
// and rejected it; retrying a compaction that already produced illegible
// output is exactly the case those gates exist to catch, and folding it into
// the model-failure fallback would silently launder an illegible summary into
// "we simply had no summary". See pinsOnlyResult for what the fallback keeps.
//
// It is narrower still (M-3, 2026-08-29 review): a RunSummary error whose
// text carries isConfigOrWiringFailure's marker never reached the model at
// all — the call could not get a credential — and also returns a real
// error rather than the fallback, for the same reason the EMPTY/QUALITY
// gates do: a config/wiring failure recurs identically on every future
// call, so silently discarding history for it is strictly worse than
// costing one failed compaction and keeping the original history. See
// isConfigOrWiringFailure for the full argument.
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
	// W-F-08: the lifecycle pre event fires once per ATTEMPT — Run is only
	// reached after the caller's gates passed, so "pre" means "a compaction
	// is actually being attempted", not "a threshold was crossed". The post
	// event below fires at EVERY exit; the sink is read from ctx per emit,
	// so a path that never binds the bus simply emits nothing.
	emitLifecycle(ctx, LifecycleEvent{
		Phase: LifecyclePreCompact, Trigger: runOpts.Trigger, TokensBefore: before,
	})
	// post reports the attempt's outcome. failure is non-empty exactly when
	// this function returns an error; the caller then keeps the original
	// history, which is the fact the hook cares about.
	post := func(failure string, res *Result, fallback bool) {
		ev := LifecycleEvent{
			Phase: LifecyclePostCompact, Trigger: runOpts.Trigger,
			TokensBefore: before, Failure: failure, Fallback: fallback,
		}
		if res != nil {
			ev.TokensAfter = res.TokensAfter
			ev.Overflow = res.Overflow != nil
		}
		emitLifecycle(ctx, ev)
	}
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
		res := &Result{
			Messages: folded, TokensBefore: before, TokensAfter: after,
			Fold:     foldStats,
			Overflow: checkOverflow(after, runOpts),
		}
		post("", res, false)
		return res, nil
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
		// M-3 (2026-08-29 review): a credential/wiring failure — the
		// summarizer never sent a request at all — is not the failure
		// W-C-04's fallback below exists for, and must not take the same
		// path. See isConfigOrWiringFailure's doc comment for the full
		// argument; short version: a model failure recovers on its own next
		// turn, a wiring failure repeats identically forever, so folding it
		// into the silent fallback means every future pre-turn compaction on
		// that provider quietly and permanently drops history. Returning a
		// real error here instead routes back through the SAME gate the
		// EMPTY/QUALITY errors above already use: MaybeCompactWithOptions's
		// err != nil branch (compact.go) keeps the original, uncompacted
		// history rather than replacing it. TestRun_ConfigFailureIsNotFallback
		// (run_test.go) is the red assertion — deleting this branch turns
		// that test's require.Error into require.NoError-shaped failure.
		if isConfigOrWiringFailure(err) {
			post(fmt.Sprintf("summary unavailable (config or wiring): %v", err), nil, false)
			return nil, fmt.Errorf("compaction summary unavailable: %w", err)
		}
		// W-C-04: the model never gave us a summary — don't call it again,
		// open a new window directly. onChunk still fires (with a DIFFERENT
		// text than a normal summary delta) so the fallback is observable on
		// the activity line, not a silent behavior change from the caller's
		// point of view.
		if onChunk != nil {
			onChunk(FallbackNotice)
		}
		res := pinsOnlyResult(msgs, plan, before, runOpts)
		post("", res, true)
		return res, nil
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
		failure := fmt.Sprintf("summarizer returned nothing for %d messages", len(toSummarize))
		post(failure, nil, false)
		return nil, fmt.Errorf("compaction summary: %s", failure)
	}
	// C10. The reasons ride out on the error (SummaryRejectedError.Issues) so
	// the layer that logs this can name the rule that fired instead of
	// reporting a generic compaction failure.
	if err := CheckQuality(summary, inputRunes, runOpts.qualityPolicy()); err != nil {
		post(fmt.Sprintf("quality gate rejected the summary: %v", err), nil, false)
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
	res := &Result{
		Messages: out, TokensBefore: before, TokensAfter: after,
		Fold:     foldStats,
		Overflow: checkOverflow(after, runOpts),
	}
	post("", res, false)
	return res, nil
}

// pinsOnlyResult builds Run's W-C-04 fallback Result: everything Plan pinned,
// verbatim, in original order — NO summary, NO eviction map, because there is
// no summary to build either from (that's the whole point of the fallback).
//
// It reuses pinnedMessages (the same helper AssembleWithMap's pinned prefix is
// built from — CLAUDE.md's "重复逻辑必须抽成公共函数") rather than duplicating
// the index-walk, so the two pinned-message paths cannot drift on what "kept
// verbatim" means.
//
// Fold still runs, against the same budgetFor(runOpts) an ordinary compaction
// uses — pins-only is a real (if smaller) history that can still be sent, and
// large pinned tool results are exactly what C5 exists to shrink. Overflow is
// still reported for the same reason Run's main path reports it: "the
// fallback ran" and "the result fits" are independent facts.
//
// TokensAfter < TokensBefore holds structurally whenever this is reached:
// pinsOnlyResult is only called when plan.SummarizeIndices is non-empty (Run's
// len==0 shortcut already returned before RunSummary was ever invoked), so the
// pinned-only slice strictly excludes at least one message that had non-zero
// estimated tokens in the overwhelming common case. The one theoretical
// exception — every excluded message estimating to exactly zero tokens — is a
// no-op fallback, no worse than the pre-W-C-04 behavior of returning an error
// and leaving history untouched.
func pinsOnlyResult(msgs []*schema.Message, plan *PlanResult, before int, runOpts RunOpts) *Result {
	pinned := pinnedMessages(msgs, plan)
	out, foldStats := FoldToolResults(pinned, FoldOptions{Budget: budgetFor(runOpts)})
	after := EstimateTokens(out)
	return &Result{
		Messages: out, TokensBefore: before, TokensAfter: after,
		Fold:     foldStats,
		Overflow: checkOverflow(after, runOpts),
		Fallback: true,
	}
}

// OpenNewWindow is W-C-14's entry point: the model asked to open a fresh
// window directly (via the context_new_window tool), skipping
// summarization entirely rather than Run hitting a summary-model failure.
// It shares the exact fallback SHAPE W-C-04 already established — Plan,
// then keep only what was pinned — because "no summary" is "no summary"
// regardless of why: pinsOnlyResult does not know or care whether the
// caller is here because the summarizer errored or because nobody asked it
// to run at all. Exported (unlike pinsOnlyResult) because its caller lives
// in a different package (einollm.CompactingModel), mirroring Run's own
// cross-package export.
func OpenNewWindow(ctx context.Context, msgs []*schema.Message, planOpts PlanOpts, runOpts RunOpts) *Result {
	before := EstimateTokens(msgs)
	// The trigger is FORCED here, not read from runOpts: OpenNewWindow IS the
	// model-requested reset, whoever calls it. A caller-supplied Trigger would
	// let a future mid-turn caller mislabel this event. The sink comes from
	// the caller's ctx — the same one the turn bound the bus on.
	emitLifecycle(ctx, LifecycleEvent{
		Phase: LifecyclePreCompact, Trigger: TriggerNewWindow, TokensBefore: before,
	})
	plan := Plan(msgs, planOpts)
	res := pinsOnlyResult(msgs, plan, before, runOpts)
	emitLifecycle(ctx, LifecycleEvent{
		Phase: LifecyclePostCompact, Trigger: TriggerNewWindow,
		TokensBefore: before, TokensAfter: res.TokensAfter, Overflow: res.Overflow != nil,
	})
	return res
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
