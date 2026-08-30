package eino

import (
	"context"
	"sync"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/x6nux/yanshi/internal/ctxcompact"
	otelobs "github.com/x6nux/yanshi/internal/observe/otel"
)

// compactCallbackKey is the context key for the optional OnCompact progress
// callback. The WS handler installs one per turn (see WithCompactCallback) so
// mid-turn compaction summary chunks stream to the client as compact_chunk
// frames; absent (e.g. SSE, tests) the summary is produced silently.
type compactCallbackKey struct{}

// WithCompactCallback installs an OnCompact progress callback on ctx. The
// CompactingModel wrapper (see below) consults it during a mid-turn compaction:
// each summary text delta is forwarded to cb so the UI can render the
// compaction being generated in real time. A nil cb leaves ctx unchanged.
//
// Installed once per turn on the turn context so the callback propagates into
// the ADK ReAct loop and hence into the model wrapper's summarization call.
func WithCompactCallback(ctx context.Context, cb func(string)) context.Context {
	if cb == nil {
		return ctx
	}
	return context.WithValue(ctx, compactCallbackKey{}, cb)
}

// compactCallback returns the bound OnCompact callback, or nil when none is
// installed (the common case — compaction is disabled or under threshold, or
// the caller is SSE/tests and did not install one).
func compactCallback(ctx context.Context) func(string) {
	cb, _ := ctx.Value(compactCallbackKey{}).(func(string))
	return cb
}

// newWindowKey is the context key for the W-C-14 per-turn new-window signal.
type newWindowKey struct{}

// newWindowSignal is the pointer WithNewWindowSignal binds once per turn and
// that a mid-turn tool call and the next maybeCompact call both see — the
// same context-carried-mutable-pointer idiom orchestrator.go's
// WithLoopGuard/turnGuard already uses for the identical shape of problem
// ("a tool call needs to leave a mark the NEXT model call reads"). A tool
// handler and a model wrapper never share a call stack — the tool result
// returns to the ADK ReAct loop, which THEN calls Generate/Stream again —
// so the only channel between them is a value both sides can reach by
// walking the same turn's context, which is exactly what context.WithValue
// gives when the value is a pointer bound once at the turn root.
type newWindowSignal struct {
	mu        sync.Mutex
	requested bool
	reason    string
}

// WithNewWindowSignal binds a fresh, empty W-C-14 signal onto ctx. Call once
// per turn (mirroring WithLoopGuard) — a signal shared across turns would let
// a request from one turn silently consume itself on the next.
func WithNewWindowSignal(ctx context.Context) context.Context {
	return context.WithValue(ctx, newWindowKey{}, &newWindowSignal{})
}

// newWindowSignalFromContext returns the bound signal, or (nil, false) when
// none is bound — sub-agents and most tests, matching compactCallback's own
// "absent means the feature is off" shape.
func newWindowSignalFromContext(ctx context.Context) (*newWindowSignal, bool) {
	s, ok := ctx.Value(newWindowKey{}).(*newWindowSignal)
	return s, ok
}

// RequestNewWindow is the context_new_window tool's write side: it marks
// this turn's next compaction check as "open a new window, don't
// summarize" and returns whether the request was recorded. false means no
// signal is bound on ctx (compaction disabled entirely, or the turn context
// predates W-C-14's wiring) — the tool handler surfaces that as an error
// rather than silently pretending the request landed.
func RequestNewWindow(ctx context.Context, reason string) bool {
	s, ok := newWindowSignalFromContext(ctx)
	if !ok {
		return false
	}
	s.mu.Lock()
	s.requested = true
	s.reason = reason
	s.mu.Unlock()
	return true
}

// consumeNewWindowRequest is maybeCompact's read side: it reports whether a
// new-window request is pending and, if so, clears it — a request is
// good for exactly ONE upcoming model call, not every call for the rest of
// the turn, the same one-shot shape RunSummary's fallback already has (one
// failure, one fallback, not a sticky mode).
func consumeNewWindowRequest(ctx context.Context) (string, bool) {
	s, ok := newWindowSignalFromContext(ctx)
	if !ok {
		return "", false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.requested {
		return "", false
	}
	reason := s.reason
	s.requested = false
	s.reason = ""
	return reason, true
}

// CompactingModel wraps a model.BaseChatModel and transparently compresses its
// input when the estimated token count reaches Threshold*ContextWindow. It is
// the MID-TURN compaction path: the ADK ReAct loop calls Generate/Stream on
// every iteration with the FULL accumulated message history, so intercepting
// here compacts BETWEEN ReAct iterations. When the budget is crossed mid-turn,
// the older messages are summarized via the shared ctxcompact core and the
// inner model then sees [pinned..., summary-as-user-at-tail], letting the turn
// continue inside the context window.
//
// Compaction is best-effort in two different ways, and they now diverge
// after W-C-04. If the summary MODEL call itself fails (exhausted retries,
// quota, overload — RunSummary's own error), ctxcompact.Run no longer treats
// that as a Run-level failure: it falls back to a pins-only Result
// (Result.Fallback=true) instead of calling the model again, and that result
// — smaller than the input, just not summarized — is what maybeCompact
// forwards. Only the two gates ABOVE the model call (an empty summary, or one
// that fails the quality gate) still make Run return an error; on THAT
// narrower failure maybeCompact keeps its original behavior of forwarding the
// unmodified input so the real model call (not the summary) surfaces the
// error rather than aborting a turn that may still fit.
//
// Implements model.BaseChatModel. Only Generate and Stream are real; tool
// binding flows through model.WithTools options (forwarded unchanged), so no
// BindTools/WithTools method is required.
type CompactingModel struct {
	Inner model.BaseChatModel

	// Summarizer (W-C-10), when non-nil, is used for the ctxcompact.Run summary
	// call INSTEAD of Inner — Inner still answers the turn itself (Generate/
	// Stream's post-compaction call), only the summarization step inside
	// maybeCompact prefers Summarizer. nil means "use Inner for summarization
	// too", which is every behavior that predates W-C-10 (this field's zero
	// value is a no-op).
	//
	// The split exists because Inner is not always resilient: a per-turn
	// `/model`-pinned single provider is looked up as one bare entry from a
	// provider registry map, with no failover chain wrapped around it. Wiring
	// a fallback chain into Summarizer alone (see wrapCompaction in
	// orchestrator.go, which sets this from CompactionConfig.ProviderFallbacks)
	// lets a failed compaction summary retry against a DIFFERENT model without
	// silently changing which model answers the turn — that would be a much
	// bigger, riskier behavior change than falling back for a summarization
	// call, and W-C-10 does not do it.
	Summarizer model.BaseChatModel

	// Threshold is the fraction of ContextWindow at which compaction fires
	// (e.g. 0.8). Threshold <= 0 disables compaction (pass-through).
	Threshold float64
	// ContextWindow is the token budget used for the threshold check.
	ContextWindow int
	// KeepRecent is the number of TRAILING MESSAGES kept verbatim (a raw message
	// count, NOT a pair count — legacy bridge: it is halved when passed to
	// ctxcompact.PlanOpts.KeepRecent which expects a pair count — the conversion is
	// planKeepRecent, cited by symbol because line numbers drift silently).
	KeepRecent int

	// CooldownTokens is the minimum token growth since the last successful
	// compaction before another one is allowed. 0 means no token-growth cooldown
	// (the prevailing non-regression default). Used in combination with
	// HardForceFraction — on approach of the hard-force threshold, cooldown is
	// overridden. The cooldown is instance-local (per-model pointer in runners
	// sync.Map) so it persists across turns.
	CooldownTokens int
	// CooldownDuration is the minimum wall-time since the last compaction before
	// another one is allowed. ≤0 means no time-based cooldown (default).
	CooldownDuration time.Duration
	// HardForceFraction forces compaction once estimated tokens reach this
	// fraction of ContextWindow, even when inside a cooldown period. 0 disables
	// (not recommended — the token budget safety net). Default via config: 0.95.
	HardForceFraction float64
	// Redactor strips registered secrets from the copy of the history handed to
	// the summary model, and from the summary that comes back (C11). nil
	// disables redaction, which is the historical behaviour.
	//
	// The mid-turn path needs this as much as the pre-turn one: a tool_result
	// carrying an API key gets folded into a PINNED summary message, so the key
	// is re-sent to the provider on every later turn long after the tool_result
	// it came from was compacted away. Callers must not assign a typed-nil
	// *secrets.Redactor here — see ctxcompact.Options.Redactor for why that
	// would panic rather than disable redaction.
	Redactor ctxcompact.Redactor
	// didCompact records whether any compaction has happened yet, replacing a
	// lastCompactTokens == 0 sentinel.
	//
	// ⚠️ UNPINNED, and deliberately so: now that lastCompactTokens holds the
	// pre-compaction size -- never legitimately zero -- the sentinel and this
	// flag agree in every reachable state, so swapping the flag back for the
	// sentinel reddens nothing (measured W4 review round 1). The flag is
	// defence in depth against a future change that stores something which
	// CAN be zero, not a fix for a live bug. Do not claim otherwise.
	didCompact bool
	// lastCompactTokens is the TokensBefore (from ctxcompact.Result) of the most
	// recent successful compaction, or 0 if no compaction has occurred yet on
	// this model instance. Guarded by cmMu.
	lastCompactTokens int
	// lastCompactAt is the wall clock time of the most recent successful
	// compaction, or zero if none yet. Guarded by cmMu.
	lastCompactAt time.Time

	// cmMu guards the mutable compaction state that is shared across concurrent
	// turns on the same model wrapper (runners sync.Map may be hit from multiple
	// WS sessions). Always lock before reading or writing cooldown/force fields.
	cmMu sync.Mutex
}

// compile-time interface check.
var _ model.BaseChatModel = (*CompactingModel)(nil)

// Generate forwards to Inner, compacting the input first when the threshold
// gate fires.
func (c *CompactingModel) Generate(ctx context.Context, msgs []*schema.Message, opts ...model.Option) (*schema.Message, error) {
	if out, ok := c.maybeCompact(ctx, msgs); ok {
		return c.Inner.Generate(ctx, out, opts...)
	}
	return c.Inner.Generate(ctx, msgs, opts...)
}

// Stream forwards to Inner, compacting the input first when the threshold gate
// fires.
func (c *CompactingModel) Stream(ctx context.Context, msgs []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	if out, ok := c.maybeCompact(ctx, msgs); ok {
		return c.Inner.Stream(ctx, out, opts...)
	}
	return c.Inner.Stream(ctx, msgs, opts...)
}

// maybeCompact returns the compacted message list and true when the threshold
// gate fires AND the shared core actually shrank the history. Returns
// (msgs, false) — the original slice unchanged — when compaction is disabled,
// the history is not yet over budget, the history is too short, the
// summarization failed, or Plan pinned everything (no actual shrink). On a
// summarization failure it returns the original input so the inner call
// surfaces any real error.
//
// # The cooldown is armed on BOTH outcomes, and that is the fix for a retry storm
//
// It used to be armed only on success. In isolation that reads right — a
// rejected compaction is one the context still needs — but this is the MID-TURN
// path, and the ADK ReAct loop calls Generate on EVERY iteration with the full
// accumulated history. An unarmed gate therefore re-attempted the identical
// doomed compaction once per iteration, each attempt a paid summary model call
// against a history that had not changed and against which nothing could
// succeed that had not already failed. The failure was completely silent: no
// error, no event, a correct final answer, and a provider bill proportional to
// the length of the turn.
//
// Arming on failure does not strand an over-large context, because
// shouldCompact checks HardForceFraction BEFORE the cooldown gate: at the
// hard-force fraction the cooldown is overridden and compaction retries anyway.
// The net effect is to rate-limit a failing retry to once per CooldownTokens of
// growth instead of once per iteration.
//
// ⚠️ THAT ORDERING IS LOAD-BEARING, NOT INCIDENTAL. It is the only thing making
// this safe, and it is exactly what the rule this replaced was protecting: a
// context refused a retry keeps growing until the provider call does not fit.
// Move the hard-force check below the cooldown gate, or let HardForceFraction
// reach a production path as 0, and that failure returns — silently, since the
// symptom is a context that quietly stops being compacted.
// TestHardForceBypassesCooldown_SoArmingOnFailureIsSafe guards the coupling and
// names it; read it before changing either gate.
func (c *CompactingModel) maybeCompact(ctx context.Context, msgs []*schema.Message) ([]*schema.Message, bool) {
	// W-C-14: a pending tool-set request wins over the threshold gate — the
	// model asked for this explicitly, so it fires even when shouldCompact
	// would say no (under threshold, in cooldown). Checked BEFORE
	// shouldCompact, not after, precisely so a short history the threshold
	// gate would never touch can still be proactively reset.
	if reason, ok := consumeNewWindowRequest(ctx); ok {
		return c.openNewWindow(ctx, msgs, reason)
	}
	if !c.shouldCompact(msgs) {
		return msgs, false
	}
	// Measured before the call and used for the cooldown on either outcome. On
	// the success path this is exactly Result.TokensBefore, which ctxcompact.Run
	// computes the same way from the same slice; taking it here means the
	// failure path — where res may be nil — has a size to arm with too.
	attempted := ctxcompact.EstimateTokens(msgs)

	// W-C-10: the summarization call prefers Summarizer (a fallback-wrapped
	// model, when the catalog declares one for this model id) over Inner. The
	// turn-answering calls in Generate/Stream above are untouched — only this
	// one call site, feeding ctxcompact.Run, is affected.
	summarizer := c.Inner
	if c.Summarizer != nil {
		summarizer = c.Summarizer
	}

	cb := compactCallback(ctx)
	res, err := ctxcompact.Run(ctx, msgs,
		ctxcompact.PlanOpts{KeepRecent: c.planKeepRecent()},
		ctxcompact.RunOpts{ModelWindow: c.ContextWindow, ChunkThreshold: 0.9, Redactor: c.Redactor},
		summarizer, cb)

	c.cmMu.Lock()
	c.lastCompactTokens = attempted
	c.didCompact = true
	c.lastCompactAt = time.Now()
	c.cmMu.Unlock()

	if err != nil || res.TokensAfter >= res.TokensBefore {
		// best-effort: forward the original history. If it's over-window, the
		// real inner call surfaces the error rather than aborting mid-turn.
		return msgs, false
	}
	return res.Messages, true
}

// openNewWindow is W-C-14's proactive counterpart to the RunSummary-failure
// path inside maybeCompact's main call to ctxcompact.Run: same pins-only
// shape (ctxcompact.OpenNewWindow shares pinsOnlyResult with Run's own
// fallback), reached by the model asking rather than the summary model
// erroring. ContextWindow is reused as-is — already sourced from the INF2
// provider-window table by wrapCompaction, so "新窗口大小取自 INF2 表"
// requires no new plumbing here.
//
// Recording (the onChunk notice and the otel counter) fires whenever the
// request was ACTED on, regardless of whether pins-only turned out smaller
// than the input — "the model asked and this ran" is the fact being
// recorded, not "and it helped". Cooldown bookkeeping is armed the same way
// the main path arms it on every outcome (see maybeCompact's doc comment on
// why arming can't be success-only): a proactive reset is itself a reason
// the next threshold-triggered compaction should wait.
//
// reason is accepted but not attached anywhere below — RecordNewWindow's
// doc comment explains why a model-authored freeform string does not become
// a metric attribute. It is threaded this far only so a future consumer
// (a log line, not a metric) has it without re-plumbing the call chain; that
// is not speculative scaffolding since consumeNewWindowRequest already
// carries it as its return value regardless.
func (c *CompactingModel) openNewWindow(ctx context.Context, msgs []*schema.Message, reason string) ([]*schema.Message, bool) {
	attempted := ctxcompact.EstimateTokens(msgs)

	if cb := compactCallback(ctx); cb != nil {
		cb(ctxcompact.NewWindowNotice)
	}
	otelobs.RecordNewWindow(ctx)

	res := ctxcompact.OpenNewWindow(msgs,
		ctxcompact.PlanOpts{KeepRecent: c.planKeepRecent()},
		ctxcompact.RunOpts{ModelWindow: c.ContextWindow, ChunkThreshold: 0.9, Redactor: c.Redactor})

	c.cmMu.Lock()
	c.lastCompactTokens = attempted
	c.didCompact = true
	c.lastCompactAt = time.Now()
	c.cmMu.Unlock()

	if res.TokensAfter >= res.TokensBefore {
		return msgs, false
	}
	return res.Messages, true
}

// planKeepRecent converts this model's KeepRecent — a count of MESSAGES — into
// ctxcompact.PlanOpts.KeepRecent, which counts PAIRS.
//
// It is a named method rather than an inline `/ 2` so the conversion has
// somewhere to be asserted. The two fields share a name and mean different
// things; a drift in either direction changes how much tail survives a
// compaction, and does so silently — no error, no missing event, just a
// different amount of history than the configuration asks for.
func (c *CompactingModel) planKeepRecent() int { return c.KeepRecent / 2 }

// shouldCompact reports whether the threshold gate fires for msgs, taking
// the cooldown and hard-force fraction into account.
func (c *CompactingModel) shouldCompact(msgs []*schema.Message) bool {
	if c.Threshold <= 0 || c.ContextWindow <= 0 || c.KeepRecent <= 0 {
		return false
	}
	if len(msgs) <= c.KeepRecent {
		return false
	}
	tokens := ctxcompact.EstimateTokens(msgs)

	// Hard force: when approaching the window edge we compact even inside a
	// cooldown period so the inner model call does not over-window.
	if c.HardForceFraction > 0 && tokens >= int(c.HardForceFraction*float64(c.ContextWindow)) {
		return true
	}

	// Threshold gate: compact once the history reaches the configured
	// threshold. AT the threshold counts -- the comparison is < on the
	// skip side, not <=, and TestCompactingModel_ThresholdBoundaries pins
	// that. The previous wording said "over", which reads as strictly
	// greater and would send the next reader the wrong way.
	if tokens < int(c.Threshold*float64(c.ContextWindow)) {
		return false
	}

	// Cooldown gate: if we recently compacted and the history hasn't grown
	// past the post-compact size by CooldownTokens (or not enough time elapsed),
	// defer re-compaction.
	if c.inCooldown(tokens) {
		return false
	}
	return true
}

// inCooldown reports whether the cooldown period is still active relative to
// the last successful compaction. Either dimension (token-growth or time) being
// unmet is enough to be in cooldown.
func (c *CompactingModel) inCooldown(tokens int) bool {
	c.cmMu.Lock()
	lastT := c.lastCompactTokens
	lastAt := c.lastCompactAt
	did := c.didCompact
	c.cmMu.Unlock()

	if !did {
		return false // no prior compact → no cooldown
	}
	// lastT is the size of the history that triggered the last compaction, so
	// tokens-lastT is genuine growth since then. Comparing against the
	// post-compaction size (the old behaviour) mixed dimensions and made the
	// difference large enough to re-fire on an unchanged history.
	tokenCool := c.CooldownTokens > 0 && tokens-lastT < c.CooldownTokens
	timeCool := c.CooldownDuration > 0 && !lastAt.IsZero() && time.Since(lastAt) < c.CooldownDuration
	return tokenCool || timeCool
}
