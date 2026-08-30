package loopguard

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// TokenBudgetGate stops a turn once its accumulated token spend reaches a
// configured ceiling.
//
// # Why this gate has to accumulate rather than read a total
//
// Providers report token usage CUMULATIVELY PER CALL: the prompt count of the
// third model call already includes the whole conversation resent on that
// call. yanshi's TurnUsage therefore overwrites rather than sums (that is what
// makes the TUI's context bar show real context fill instead of an
// ever-growing number), and a budget built on that value would be measuring
// context size, not spend.
//
// So this gate keeps its own accumulator: each iteration it adds the LATEST
// call's completion tokens, plus the growth in prompt tokens since the
// previous observation. Prompt growth rather than the raw prompt count,
// because the resent prefix is billed on every call but was already counted
// when it was first sent — summing raw prompt counts would over-count a long
// turn quadratically, and a budget that fires at a quarter of its nominal
// value is a budget nobody can size.
//
// The residual inaccuracy is one-directional and known: providers that cache a
// prompt prefix bill it at a discount, and this gate counts it at full price
// on first send. It therefore stops slightly EARLY on cached providers, never
// late.
type TokenBudgetGate struct {
	maxTotal int

	mu           sync.Mutex
	prompt       int
	completion   int
	lastPrompt   int
	observedOnce bool
}

// NewTokenBudgetGate builds a gate that stops the turn once total accumulated
// tokens reach maxTotal. A non-positive maxTotal returns nil, so a caller can
// pass the configured value straight through and get "no gate" for "no limit"
// (NewHandler drops nil gates).
func NewTokenBudgetGate(maxTotal int) *TokenBudgetGate {
	if maxTotal <= 0 {
		return nil
	}
	return &TokenBudgetGate{maxTotal: maxTotal}
}

// Name implements Gate.
func (g *TokenBudgetGate) Name() string { return "token-budget" }

// Priority implements Gate. 20, matching QwenPaw's TokenBudgetGate.
func (g *TokenBudgetGate) Priority() int { return 20 }

// Used reports the tokens accumulated so far. Exposed so the caller can render
// budget consumption without a second accumulator that could disagree with the
// one actually enforcing the limit.
func (g *TokenBudgetGate) Used() int {
	if g == nil {
		return 0
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.prompt + g.completion
}

// Max reports the configured ceiling this gate stops the turn at, mirroring
// Used so a caller can render "N of M used" without a second copy of the
// configured limit that could drift from the one actually enforcing it. 0 for
// a nil gate, matching NewTokenBudgetGate's "non-positive maxTotal means no
// gate" convention.
func (g *TokenBudgetGate) Max() int {
	if g == nil {
		return 0
	}
	return g.maxTotal
}

// Check implements Gate.
func (g *TokenBudgetGate) Check(obs Observation) Result {
	if g == nil {
		return Result{}
	}
	g.mu.Lock()
	defer g.mu.Unlock()

	if obs.PromptTokens > 0 || obs.CompletionTokens > 0 {
		if !g.observedOnce {
			g.prompt = obs.PromptTokens
			g.observedOnce = true
		} else if growth := obs.PromptTokens - g.lastPrompt; growth > 0 {
			g.prompt += growth
		}
		g.lastPrompt = obs.PromptTokens
		g.completion += obs.CompletionTokens
	}

	total := g.prompt + g.completion
	if total < g.maxTotal {
		return Result{}
	}
	return Result{
		Action: ActionStop,
		Reason: fmt.Sprintf("token budget exhausted: %d of %d tokens used this turn", total, g.maxTotal),
	}
}

// DeadlineGate stops a turn once it has run longer than a configured
// wall-clock limit.
//
// # Why the check is at an iteration boundary and not a context deadline
//
// The obvious implementation is context.WithTimeout on the turn context, and
// it is wrong here for a specific reason, which QwenPaw's TimeoutGate also
// documents ("Stop at a loop boundary after a monotonic timeout"): the timer
// can fire in the middle of a tool call, after the assistant message carrying
// the tool_call has been committed to the conversation but before the
// corresponding tool_result exists. Every provider rejects a history with a
// dangling tool_call, so the session would be unusable from that point on —
// the user's next message fails validation and the only recovery is dropping
// history.
//
// Checking only between iterations means the deadline is soft by up to the
// duration of the slowest single tool call. That is the price of a history
// that stays valid, and it is the right trade: a hard cut leaves the session
// broken, a soft one leaves it merely late.
type DeadlineGate struct {
	limit time.Duration
}

// NewDeadlineGate builds a gate that stops the turn once Observation.Elapsed
// reaches limit. A non-positive limit returns nil ("no limit"), matching
// NewTokenBudgetGate.
func NewDeadlineGate(limit time.Duration) *DeadlineGate {
	if limit <= 0 {
		return nil
	}
	return &DeadlineGate{limit: limit}
}

// Name implements Gate.
func (g *DeadlineGate) Name() string { return "deadline" }

// Priority implements Gate. 30, matching QwenPaw's TimeoutGate.
func (g *DeadlineGate) Priority() int { return 30 }

// Check implements Gate. It is stateless: elapsed time is supplied by the
// caller so tests can drive it without sleeping.
func (g *DeadlineGate) Check(obs Observation) Result {
	if g == nil || obs.Elapsed < g.limit {
		return Result{}
	}
	return Result{
		Action: ActionStop,
		Reason: fmt.Sprintf("turn time limit reached: %s elapsed, limit %s (checked at an iteration boundary so tool_call/tool_result pairs stay intact)",
			obs.Elapsed.Round(time.Second), g.limit),
	}
}

// tokenBudgetKey is the context key for the per-turn *TokenBudgetGate binding.
type tokenBudgetKey struct{}

// WithTokenBudgetGate binds gate onto ctx.
//
// W-C-11: a tool that answers "how many turn tokens do I have left" must read
// the SAME accumulator WrapInvokableToolCall/BeforeModelRewriteState enforce
// the limit with — Used()/Max() on this exact instance — not a second
// counter built from the same MaxTurnTokens config that could disagree with
// it (a re-observed prompt-growth accumulator is exactly the kind of state
// that drifts: see TokenBudgetGate's own doc comment on why it cannot be
// recomputed from a raw total). loopguard has no accessor to pull a specific
// typed gate back out of a Handler's private gate slice, so the caller that
// builds the gate hands the pointer here directly.
//
// A nil gate (MaxTurnTokens unconfigured) leaves ctx unchanged, so
// TokenBudgetGateFromContext correctly reports "not bound" rather than a fake
// gate whose Max() is 0 and whose Used() looks like an exhausted budget.
func WithTokenBudgetGate(ctx context.Context, gate *TokenBudgetGate) context.Context {
	if gate == nil {
		return ctx
	}
	return context.WithValue(ctx, tokenBudgetKey{}, gate)
}

// TokenBudgetGateFromContext returns the gate bound by WithTokenBudgetGate, or
// (nil, false) when none is bound — MaxTurnTokens unconfigured, a sub-agent
// turn (which does not share the managing turn's context), or most tests.
func TokenBudgetGateFromContext(ctx context.Context) (*TokenBudgetGate, bool) {
	g, ok := ctx.Value(tokenBudgetKey{}).(*TokenBudgetGate)
	return g, ok && g != nil
}
