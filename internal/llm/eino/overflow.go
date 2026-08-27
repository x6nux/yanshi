// internal/llm/eino/overflow.go
//
// C6: reactive recovery from a provider context-overflow rejection.
//
// Proactive compaction is threshold-driven and therefore necessarily
// incomplete. CLAUDE.md states the reason in the compaction section: the
// chunking loop's real upper bound is "window + the largest indivisible segment
// in the history", which is unbounded in the window — a parallel tool-call
// group cannot be split, so a wide enough fan-out overshoots any threshold. The
// gate is a good gate; it just cannot be a total one.
//
// What happened when it missed was total, though: the provider answered 400,
// the classifier correctly filed it as ClassContextOverflow (non-retryable,
// because resending the identical prompt reproduces it exactly), and the turn
// died. All the work in that turn was lost to a condition whose fix — compact
// harder and send again — was sitting one function call away.
//
// The policy is QwenPaw's and has exactly two rules, both of which exist to
// stop a recovery from becoming a loop:
//
//  1. RETRY ONLY IF THE INPUT ACTUALLY SHRANK. A compaction that pinned
//     everything (the tail alone exceeds the window) returns the same history
//     it was given. Sending it again is a guaranteed second 400 and a second
//     charge for it.
//  2. RETRY AT MOST ONCE. A second overflow after a successful shrink means the
//     history is over the limit for a reason compaction cannot address, and
//     compacting repeatedly toward an unreachable target is how one failed turn
//     becomes a metered loop.
package eino

import (
	"context"
	"errors"
	"fmt"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/x6nux/yanshi/internal/ctxcompact"
)

// OverflowRecoveryConfig configures the forced compaction C6 runs after a
// provider rejects a request for being too long.
type OverflowRecoveryConfig struct {
	// ContextWindow is the model's token window. Zero DISABLES recovery
	// entirely: with no window there is no budget to compact toward, and the
	// forced compaction would have no way to tell success from failure.
	ContextWindow int
	// KeepRecent is how many trailing MESSAGES the forced compaction pins.
	//
	// It is deliberately smaller than the proactive path's: this runs after
	// the ordinary threshold already failed to make the history fit, so
	// repeating that path's generosity would produce the same too-large
	// result. Zero selects DefaultOverflowKeepRecent.
	KeepRecent int
	// Summarizer produces the summary. When nil the inner model is used, which
	// is the usual choice — the model that just rejected the prompt is still
	// perfectly able to summarise a smaller slice of it.
	Summarizer ctxcompact.ModelSummarizer
	// Redactor strips secrets from the copy of the history handed to the
	// summariser, exactly as the proactive path does (C11). nil disables it.
	Redactor ctxcompact.Redactor
	// OutputReserve is the token allowance held back for the reply. Zero
	// selects the proportional default (see ctxcompact.DefaultOutputReserve).
	OutputReserve int
}

// DefaultOverflowKeepRecent is how many trailing messages a forced compaction
// keeps verbatim when KeepRecent is unset.
//
// Four — two exchanges — is the smallest tail that still contains the user's
// current request and the tool result it is waiting on. Below that the model
// loses the thing it was asked to do and the recovered turn answers a question
// nobody asked; above it, the tail is a growing share of a budget that has
// already proved too small.
const DefaultOverflowKeepRecent = 4

// keepRecent returns the configured tail size or the default.
func (c OverflowRecoveryConfig) keepRecent() int {
	if c.KeepRecent > 0 {
		return c.KeepRecent
	}
	return DefaultOverflowKeepRecent
}

// runOpts projects the config onto the shared core's options.
func (c OverflowRecoveryConfig) runOpts() ctxcompact.RunOpts {
	return ctxcompact.RunOpts{
		ModelWindow:    c.ContextWindow,
		ChunkThreshold: 0.9,
		OutputReserve:  c.OutputReserve,
		Redactor:       c.Redactor,
	}
}

// IsContextOverflow reports whether err is a context-overflow rejection from
// either side of the boundary: the LOCAL pre-send gate (C9's
// ctxcompact.ErrContextOverflow) or the REMOTE provider 400 the classifier
// files as ClassContextOverflow.
//
// Both are the same condition — "this input does not fit" — and the recovery is
// identical, so the two must not be told apart by callers. They arrive
// differently only because one is measured before the round trip and the other
// after it.
func IsContextOverflow(err error) bool {
	if err == nil {
		return false
	}
	if ClassifyError(err).Class == ClassContextOverflow {
		return true
	}
	return isLocalOverflow(err)
}

// isLocalOverflow reports whether err is (or wraps) C9's local sentinel.
func isLocalOverflow(err error) bool {
	return errors.Is(err, ctxcompact.ErrContextOverflow)
}

// forceCompact runs a compaction that ignores every threshold and cooldown,
// returning the shrunk history and whether it is ACTUALLY SMALLER than the
// input.
//
// The bool is the load-bearing return, not the slice. Rule 1 of the recovery
// policy lives here: when Plan pins everything, or the summariser fails, or the
// summary comes out no smaller than what it replaced, the answer is false and
// the caller must not resend. Returning the original history alongside false
// (rather than nil) keeps the caller's error path simple — it forwards what it
// had and surfaces the provider's own error.
func forceCompact(ctx context.Context, msgs []*schema.Message, inner model.BaseChatModel,
	cfg OverflowRecoveryConfig) ([]*schema.Message, bool) {
	if cfg.ContextWindow <= 0 || len(msgs) <= cfg.keepRecent() {
		return msgs, false
	}
	summarizer := cfg.Summarizer
	if summarizer == nil {
		summarizer = inner
	}
	if summarizer == nil {
		return msgs, false
	}
	before := ctxcompact.EstimateTokens(msgs)
	res, err := ctxcompact.Run(ctx, msgs,
		ctxcompact.PlanOpts{KeepRecent: cfg.keepRecent() / 2},
		cfg.runOpts(), summarizer, nil)
	if err != nil {
		return msgs, false
	}
	after := ctxcompact.EstimateTokens(res.Messages)
	if after >= before {
		return msgs, false
	}
	return res.Messages, true
}

// overflowRetryError wraps the provider's second rejection with the sizes of
// both attempts.
//
// Without the numbers, a user seeing the second failure has no way to know a
// recovery was even attempted, and an operator cannot tell "compaction did
// nothing" from "compaction worked and the input is still too large" — which
// are different problems with different fixes (raise context_window vs. the
// history contains one indivisible segment bigger than the window).
type overflowRetryError struct {
	// Before and After are the estimated token counts of the original and the
	// force-compacted history.
	Before, After int
	// Err is the provider's error from the retried request.
	Err error
}

// Error renders both sizes alongside the provider's message.
func (e *overflowRetryError) Error() string {
	return fmt.Sprintf("eino: context overflow persisted after forced compaction (%d → %d tokens): %v",
		e.Before, e.After, e.Err)
}

// Unwrap exposes the provider error so classification and errors.Is still see
// the original condition.
func (e *overflowRetryError) Unwrap() error { return e.Err }
