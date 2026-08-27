// internal/ctxcompact/budget.go
package ctxcompact

import (
	"errors"
	"fmt"

	"github.com/cloudwego/eino/schema"
)

// DefaultOutputReserve is the CAP on the token allowance held back for the
// model's REPLY when no reserve is configured.
//
// Every budget in this package used to be computed against the raw context
// window, which silently assumed the reply costs nothing. It never does: the
// window bounds prompt PLUS completion, so a history compacted to exactly the
// window leaves zero room to answer and the provider rejects the request — the
// one outcome compaction exists to prevent.
//
// It is a cap and not the reserve itself; see defaultReserveDenominator.
const DefaultOutputReserve = 4096

// defaultReserveDenominator makes the unconfigured reserve PROPORTIONAL to the
// window — window/4, capped at DefaultOutputReserve — rather than a flat
// constant.
//
// A flat constant is wrong at both ends of the range yanshi actually runs
// against. Against a 256K window 4096 tokens is a 1.6% haircut, which is right.
// Against the 8K window of a small fast summary model — exactly the
// configuration compaction.model exists to enable — the same constant is 50% of
// the budget, and against a 4K window it is the entire budget: the flat form
// makes compaction impossible on the models it most needs to work on. Scaling
// with the window keeps the reserve at a fixed FRACTION until the cap takes
// over, which is the shape QwenPaw's `max(256, min(4000, context_size // 4))`
// arrived at for the same reason.
//
// The difference from that reference is the absent lower floor. QwenPaw floors
// the reserve at 256 tokens and then legitimately refuses to summarize under a
// window too small to hold it. This package supports windows down to a few
// hundred tokens (its own chunking tests drive the carry loop at 300), and a
// floor there would reserve most of the window and turn every one of those into
// a refusal. Proportional all the way down keeps small windows workable; they
// were never going to fit a large completion anyway.
const defaultReserveDenominator = 4

// maxOutputReserveDenominator bounds an EXPLICITLY CONFIGURED reserve to a
// fraction of the window. Half the window is the most that may be reserved for
// output: the reply is not permitted to outweigh the entire context it is
// replying to, however large the caller believes its max_tokens to be.
const maxOutputReserveDenominator = 2

// ErrContextOverflow reports that the history still exceeds the hard token
// limit after compaction converged, so the request was NOT forwarded.
//
// It exists because the previous behaviour was to forward anyway and let the
// provider answer 400. That trades a local, typed, catchable failure for a
// remote one that arrives as an opaque HTTP error after a full round trip,
// carries no token numbers, and cannot be distinguished from any other 400.
// Reactive recovery (retry with a harder compaction, drop to a bigger model)
// needs to know "the input does not fit" specifically, and errors.Is against
// this sentinel is how it will ask.
var ErrContextOverflow = errors.New("compaction: history exceeds the context limit after compaction")

// ContextOverflowError carries the measurements behind ErrContextOverflow: what
// the history actually estimates at, and the limit it had to fit under.
//
// Both numbers are on the error because the recovery decision needs them — how
// far over the limit the history is determines whether a harder compaction can
// plausibly close the gap or whether the turn needs a larger window — and
// because an operator reading the log cannot otherwise tell a 5% overshoot from
// a 5x one.
type ContextOverflowError struct {
	// Tokens is the estimated token count of the history that was rejected.
	Tokens int
	// Limit is the budget it had to fit under: the context window less the
	// output reserve.
	Limit int
	// Window and Reserve are the inputs Limit was derived from, kept so the log
	// line can show why the limit is what it is.
	Window  int
	Reserve int
}

// Error renders the overflow with every measurement.
func (e *ContextOverflowError) Error() string {
	return fmt.Sprintf("%s: %d tokens exceeds the limit of %d (window %d less output reserve %d)",
		ErrContextOverflow.Error(), e.Tokens, e.Limit, e.Window, e.Reserve)
}

// Unwrap makes errors.Is(err, ErrContextOverflow) match.
func (e *ContextOverflowError) Unwrap() error { return ErrContextOverflow }

// effectiveReserve returns the output reserve actually applied for a window.
//
// Unconfigured (0) means window/defaultReserveDenominator capped at
// DefaultOutputReserve — proportional, so a small summary model keeps a usable
// input budget. An explicitly configured value is honoured but clamped to
// window/maxOutputReserveDenominator. A negative configured reserve is treated
// as "none" rather than as a bonus: a caller that miscomputes max_tokens must
// not be able to hand compaction a budget LARGER than the window.
//
// A window of 0 or less means "unbudgeted" throughout this package (singleBudget
// and chunkBudgetFor both special-case it), so no reserve applies either;
// returning one would make budgetFor produce a negative limit for a caller that
// simply never configured a window.
func effectiveReserve(window, configured int) int {
	if window <= 0 {
		return 0
	}
	if configured <= 0 {
		// Unconfigured (or nonsensical): proportional, capped.
		reserve := window / defaultReserveDenominator
		if reserve > DefaultOutputReserve {
			reserve = DefaultOutputReserve
		}
		return reserve
	}
	if maxReserve := window / maxOutputReserveDenominator; configured > maxReserve {
		return maxReserve
	}
	return configured
}

// budgetFor returns the token budget available for the TURN's input: the
// context window less the effective output reserve. Returns 0 for an
// unbudgeted (non-positive) window, which every caller in this package already
// reads as "no budgeting".
//
// SCOPE, and it is narrower than it looks. This is the budget the compacted
// history must fit so the ASSISTANT can still reply — it is consumed by
// checkOverflow and by nothing else. It is deliberately NOT used by
// singleBudget or chunkBudgetFor: those size the summary CALL, a separate
// request whose own output headroom is ChunkThreshold, and charging them the
// turn's reserve as well would compound the two. See the note on singleBudget.
func budgetFor(opts RunOpts) int {
	if opts.ModelWindow <= 0 {
		return 0
	}
	return opts.ModelWindow - effectiveReserve(opts.ModelWindow, opts.OutputReserve)
}

// checkOverflow returns a *ContextOverflowError when tokens exceeds the input
// budget derived from opts, and nil otherwise. An unbudgeted window (0) can
// never overflow: with no window there is no limit to be over.
func checkOverflow(tokens int, opts RunOpts) error {
	limit := budgetFor(opts)
	if limit <= 0 || tokens <= limit {
		return nil
	}
	return &ContextOverflowError{
		Tokens:  tokens,
		Limit:   limit,
		Window:  opts.ModelWindow,
		Reserve: effectiveReserve(opts.ModelWindow, opts.OutputReserve),
	}
}

// CheckContextLimit reports whether msgs fit the input budget for opts — the
// context window less the output reserve — returning a *ContextOverflowError
// (which errors.Is-matches ErrContextOverflow) when they do not.
//
// THIS IS THE PRE-SEND GATE, and it is exported because the send happens
// outside this package. Compaction shrinks a history; it cannot promise the
// result fits, because the pinned set alone may exceed the budget and no
// summary of the remainder can change that. The transport layer therefore has
// to ask separately, immediately before dispatching, and refuse locally instead
// of letting the provider answer 400 after a full round trip.
//
// Why this is not folded into Run's error return: Run's callers all treat an
// error as "keep the ORIGINAL history". A compacted-but-still-too-large history
// is strictly SMALLER than the original, so failing Run on overflow would make
// them discard the improvement and forward something bigger — the opposite of
// the intent. Run therefore reports overflow on Result.Overflow and still hands
// back the shrunk history; refusing to send is a separate decision, made here,
// by the code that owns the send.
func CheckContextLimit(msgs []*schema.Message, opts RunOpts) error {
	return checkOverflow(EstimateTokens(msgs), opts)
}
