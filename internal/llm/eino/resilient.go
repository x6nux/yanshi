package eino

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"strings"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	otelobs "github.com/x6nux/yanshi/internal/observe/otel"
)

// ResilientConfig tunes retry/failover for an Eino model chain.
type ResilientConfig struct {
	// MaxRetries caps retries on TRANSIENT errors — network blips, gateway
	// mid-stream drops (notably the openai acl's "failed to receive stream
	// chunk: unexpected EOF"), 5xx, rate limits — for BOTH Generate and Stream.
	// A retry re-issues the model call with exponential backoff. Defaults to 10.
	//
	// This is the FALLBACK a provider without its own override falls back to
	// (W-C-07) — see PerProviderMaxRetries.
	MaxRetries int
	BaseDelay  time.Duration
	MaxDelay   time.Duration

	// PerProviderMaxRetries (W-C-07) overrides MaxRetries independently for
	// each entry of the chain passed to NewResilientModel, indexed the same
	// way (chain[i] ↔ PerProviderMaxRetries[i]). A value of -1 (NOT 0, which
	// is a legitimate "never retry this provider") means "not set, fall back
	// to MaxRetries" — the same nil-means-omit shape config.ProviderConfig.
	// MaxRetries uses at the config layer, mirrored here as a sentinel
	// because a parallel slice cannot hold *int cheaply. nil (the zero
	// value) or a slice shorter than chain means every index falls back to
	// MaxRetries, so a chain built without this field behaves exactly as it
	// did before W-C-07. See maxRetriesFor.
	PerProviderMaxRetries []int

	// MaxEmptyRetries is the number of times to retry a successful-but-empty
	// response (Content=="" && no ToolCalls) or an empty stream before giving
	// up. Defaults to 10. Independent of MaxRetries so empty responses get their
	// own generous cap.
	MaxEmptyRetries int

	// RateLimitBase / RateLimitMax are the backoff schedule used for RATE-LIMIT
	// errors only (M1), defaulting to RateLimitBaseDelay / RateLimitMaxDelay.
	//
	// They are separate from BaseDelay/MaxDelay rather than derived from them
	// because the two situations want opposite things. BaseDelay is tuned for
	// network blips, where retrying almost immediately is correct. A 429 is the
	// server declaring it is over capacity: retrying on the blip schedule lands
	// every attempt inside the same throttle bucket, and each one both fails and
	// counts against the limit, so our own traffic extends the throttle we are
	// waiting out. Deriving one from the other would mean any future tightening
	// of the transient schedule silently re-creates that.
	//
	// A server-sent Retry-After overrides both (see RateLimitBackoff); only
	// MaxRetryAfter bounds that.
	RateLimitBase time.Duration
	RateLimitMax  time.Duration

	// FirstChunkTimeout / IdleTimeout are the stream watchdog's two idle
	// budgets (W-A-06). consumeStream's loop otherwise only checks ctx.Err()
	// and then blocks on Recv with no timeout at all — a gateway that accepts
	// the connection and sends nothing hangs the stream forever, and
	// loopguard's DeadlineGate cannot help: it only checks at ReAct iteration
	// boundaries, and a stream stuck inside Recv never reaches one.
	//
	// FirstChunkTimeout measures "did the upstream ever start": request sent
	// to the first content-bearing chunk. IdleTimeout measures "is the
	// upstream still going": the gap between two content-bearing chunks. A
	// chunk carrying only Role, or an otherwise all-empty delta, does NOT
	// reset either clock (see watchdogReader in streamwatchdog.go) — otherwise
	// a gateway that emits heartbeats and nothing else would hang forever.
	//
	// Zero disables the corresponding budget. Both zero reproduces pre-W-A-06
	// behaviour byte-for-byte — no non-zero default is set here, mirroring
	// loopguard's principle that a stop condition switching itself on by
	// default silently truncates a response and looks like the model giving
	// up. The composition root turns this on explicitly via config.
	FirstChunkTimeout time.Duration
	IdleTimeout       time.Duration
}

// RetryableModelError marks a transient model error (rate limit, timeout, 5xx).
type RetryableModelError struct{ Err error }

// Error returns the wrapped error message.
func (e *RetryableModelError) Error() string { return e.Err.Error() }

// Unwrap returns the wrapped error for errors.Is/As support.
func (e *RetryableModelError) Unwrap() error { return e.Err }

// IsRetryableModelErr reports whether err is a RetryableModelError.
func IsRetryableModelErr(err error) bool {
	var re *RetryableModelError
	return errors.As(err, &re)
}

// retryCallbackKey is the context key for the optional retry-progress callback.
// The WS handler installs one per turn (WithRetryCallback) so each retry is
// forwarded to the client for the TUI's "↻ retry N/M…" activity line; absent
// (SSE, tests) retries proceed silently.
type retryCallbackKey struct{}

// RetryCallback is notified before each retry sleeps. attempt is 1-based and
// counts only retries (the initial try is attempt 0, not reported); maxAttempts
// is the relevant cap (MaxRetries for errors, MaxEmptyRetries for empty). err
// is the error that triggered the retry; delay is the backoff about to elapse.
type RetryCallback func(attempt, maxAttempts int, err error, delay time.Duration)

// WithRetryCallback installs a retry-progress callback on ctx so the UI can
// render retry progress. A nil cb leaves ctx unchanged. Installed once per turn
// on the turn context so the callback propagates through CompactingModel into
// the ResilientChatModel's stream loop.
func WithRetryCallback(ctx context.Context, cb RetryCallback) context.Context {
	if cb == nil {
		return ctx
	}
	return context.WithValue(ctx, retryCallbackKey{}, cb)
}

func retryCallbackFrom(ctx context.Context) RetryCallback {
	cb, _ := ctx.Value(retryCallbackKey{}).(RetryCallback)
	return cb
}

// userCancelCtxKey is the context key for the user-initiated cancel context.
//
// The WS handler installs the TURN context (the one wired to Ctrl-C / `/cancel`
// / CancelCurrent / exit) via WithUserCancelCtx. The retry loop then watches
// THAT context — not the per-call ctx — to decide "did the user want to stop?".
// This decouples user-cancel from network-cancel: an http.Client.Timeout or a
// transport error cancels the request's OWN derived context (callCtx/turnCtx),
// which previously surfaced as ctx.Err()!=nil and was wrongly treated as a
// user-initiated stop, suppressing retry. With userCancelCtx bound, only a
// REAL user cancel stops the retry loop; a network cancel is transient and
// retryable. Absent (SSE, tests, legacy callers), userCancelCtxFrom falls
// back to the passed ctx itself, preserving the previous behavior.
type userCancelCtxKey struct{}

// WithUserCancelCtx binds userCancelCtx to ctx so retry decisions (should we
// retry? and the backoff sleep) watch userCancelCtx rather than the per-call
// ctx. A nil userCancelCtx leaves ctx unchanged. The WS handler installs this
// once per turn with the turn's user-cancel context; SSE/tests/legacy callers
// that don't install one get the old ctx-driven behavior.
func WithUserCancelCtx(ctx, userCancelCtx context.Context) context.Context {
	if userCancelCtx == nil {
		return ctx
	}
	return context.WithValue(ctx, userCancelCtxKey{}, userCancelCtx)
}

// userCancelCtxFrom returns the bound user-cancel ctx, or ctx itself when none
// is bound (preserving legacy behavior for callers that don't install one).
func userCancelCtxFrom(ctx context.Context) context.Context {
	if c, ok := ctx.Value(userCancelCtxKey{}).(context.Context); ok {
		return c
	}
	return ctx
}

// ResilientChatModel implements model.BaseChatModel over an ordered provider
// chain, retrying on RetryableModelError with exponential backoff and failing
// over to the next provider when retries are exhausted.
type ResilientChatModel struct {
	chain []model.BaseChatModel
	cfg   ResilientConfig
}

// NewResilientModel builds a ResilientChatModel over an ordered provider chain.
func NewResilientModel(chain []model.BaseChatModel, cfg ResilientConfig) (*ResilientChatModel, error) {
	if len(chain) == 0 {
		return nil, fmt.Errorf("eino: empty model chain")
	}
	if cfg.MaxRetries <= 0 {
		cfg.MaxRetries = 10
	}
	if cfg.MaxEmptyRetries <= 0 {
		cfg.MaxEmptyRetries = 10
	}
	if cfg.BaseDelay <= 0 {
		cfg.BaseDelay = 200 * time.Millisecond
	}
	if cfg.MaxDelay <= 0 {
		cfg.MaxDelay = 5 * time.Second
	}
	if cfg.RateLimitBase <= 0 {
		cfg.RateLimitBase = RateLimitBaseDelay
	}
	if cfg.RateLimitMax <= 0 {
		cfg.RateLimitMax = RateLimitMaxDelay
	}
	return &ResilientChatModel{chain: chain, cfg: cfg}, nil
}

// maxRetriesFor returns the retry ceiling for r.chain[i]: chain[i]'s own
// PerProviderMaxRetries override (W-C-07) when the composition root set one
// (sentinel -1 means "not set"), else the shared cfg.MaxRetries every
// provider fell back to before this field existed.
func (r *ResilientChatModel) maxRetriesFor(i int) int {
	if i >= 0 && i < len(r.cfg.PerProviderMaxRetries) && r.cfg.PerProviderMaxRetries[i] >= 0 {
		return r.cfg.PerProviderMaxRetries[i]
	}
	return r.cfg.MaxRetries
}

// isEmpty reports whether msg is a successful-but-empty model response: an
// assistant/user message with no Content and no ToolCalls. Tool messages and
// nil are NOT empty (nil is absent; tool messages aren't model responses).
func isEmpty(msg *schema.Message) bool {
	if msg == nil {
		return false
	}
	if msg.Role != schema.Assistant && msg.Role != schema.User {
		return false
	}
	return msg.Content == "" && len(msg.ToolCalls) == 0
}

// Generate tries each provider, retrying within a provider on transient errors.
func (r *ResilientChatModel) Generate(ctx context.Context, in []*schema.Message, opts ...model.Option) (*schema.Message, error) {
	var lastErr error
	for i, p := range r.chain {
		msg, err := r.retry(ctx, r.maxRetriesFor(i), func() (*schema.Message, error) { return p.Generate(ctx, in, opts...) })
		if err == nil {
			return msg, nil
		}
		lastErr = err
		// Ruling RC-11 (review-whole.md M-5): a content-safety refusal
		// rejected the REQUEST, not provider i — every other entry in the
		// chain would receive the exact same content. Advancing anyway would
		// silently resend it hoping a laxer provider says yes, overriding
		// provider i's safety judgment instead of surfacing it to the
		// caller. Return the raw refusal here, unwrapped by the "chain
		// exhausted" framing below (the chain was not exhausted — only
		// provider i was ever asked). See isFailoverEligibleErr's doc
		// comment for the Stream path's symmetric carve-out, and
		// docs/adr/0026-content-safety-refusals-do-not-fail-over.md for the
		// ruling itself.
		if isContentSafetyRefusal(err) {
			return nil, err
		}
		// W-C-10: provider i exhausted its own retry budget and there is a
		// next entry to try — record the ADVANCE, not the exhaustion (a
		// failure on the LAST entry is chain exhaustion, reported below via
		// the wrapped error, not a fallback: there is nowhere left to fall
		// back to).
		if i+1 < len(r.chain) {
			otelobs.RecordFallback(ctx, i, i+1, err)
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("eino: all providers failed")
	}
	return nil, fmt.Errorf("eino: model chain exhausted: %w", lastErr)
}

// Stream returns a reader that transparently retries the provider call on
// transient failures — including MID-STREAM drops such as the openai acl's
// "failed to receive stream chunk: unexpected EOF" (chat_model.go:898), which
// surfaces as a Recv error AFTER some content has already streamed. The earlier
// implementation only retried setup/peek errors, so a mid-stream EOF aborted
// the whole turn. Now a retryable Recv error re-opens the stream up to
// MaxRetries times with exponential backoff, reporting each retry via the
// WithRetryCallback bound in ctx (so the TUI can show "↻ retry N/M…").
//
// Overwrite (not dedup): a retry regenerates the response from the start, so
// the partial content/reasoning already forwarded is now SUPERSEDED — the
// retry callback signals the consumer to DISCARD its accumulated partial
// output before the regenerated stream is re-fed in full. The WS handler
// resets its assistant-text accumulation on the callback; the TUI clears its
// pending block. This is robust to non-deterministic regeneration (the model
// need not reproduce the failed attempt's prefix exactly), at the cost of a
// brief visible replace of the partial answer.
//
// Safety bound: once a tool call has been delivered, mid-stream errors are NOT
// retried (a retry would re-emit tool calls and cause duplicate invocations);
// the error propagates instead. The retry window therefore covers the
// content/reasoning streaming phase — where gateway EOFs occur.
//
// Empty streams (EOF with no content delivered, ever) retry up to
// MaxEmptyRetries, mirroring Generate's empty handling. Per-round failover
// across the provider chain is preserved for setup errors.
func (r *ResilientChatModel) Stream(ctx context.Context, in []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	onRetry := retryCallbackFrom(ctx)
	sr, sw := schema.Pipe[*schema.Message](1)
	go r.runStream(ctx, in, opts, sw, onRetry)
	return sr, nil
}

// streamOutcome labels how consuming one provider stream ended.
type streamOutcome int

const (
	streamDone  streamOutcome = iota // clean EOF
	streamEmpty                      // EOF with no content delivered (ever) → empty retry
	streamErr                        // non-EOF recv error (or ctx cancel)
)

// runStream is the retry loop driving the reader returned by Stream: open a
// stream (with per-round failover), consume it, and retry on empty /
// retryable-error outcomes. On a mid-stream retry the regenerated stream is
// re-fed in FULL; the consumer discards its accumulated partial on the retry
// callback (see Stream's "Overwrite" note) — no prefix-skip is done here.
func (r *ResilientChatModel) runStream(ctx context.Context, in []*schema.Message, opts []model.Option,
	sw *schema.StreamWriter[*schema.Message], onRetry RetryCallback) {
	defer sw.Close()
	var (
		deliveredTools             bool
		emptyAttempts, errAttempts int
		lastErr                    error
		// curIdx is the chain index openStreamChain last resolved to (-1
		// before the first open). W-C-07: errAttempts is this PROVIDER's
		// retry count, so a failover to a different provider must reset it
		// — otherwise a chain of two providers each configured for 1 retry
		// could be driven to 2 combined retries under provider 0's budget
		// alone, and provider 1 would inherit whatever was left rather than
		// its own configured ceiling.
		curIdx = -1
		// nextOpenStart (W-C-13) is the chain index the NEXT openStreamChain
		// call should start searching from. It is 0 on every ordinary retry
		// (a retryable open/mid-stream error means "try again", and trying
		// again means giving provider 0 another chance first, exactly the
		// pre-W-C-13 behavior — see the reset right after each open call
		// below). It is only ever set to a nonzero value by the streamErr
		// case's non-retryable-class branch, which advances it past the
		// provider that just refused so openStreamChain does not immediately
		// re-open the same provider that will just refuse again.
		nextOpenStart = 0
	)
	for {
		// attemptCtx is a per-attempt child of ctx so the watchdog can cancel
		// JUST this attempt's provider call on timeout, without touching ctx
		// itself (which callers still use for turn-level cancellation). This
		// is what lets a timeout actually unblock the goroutine parked in
		// sr.Recv (see newWatchdogReader's doc comment) instead of merely
		// abandoning it: cancelling attemptCtx aborts the in-flight HTTP
		// request in every provider in the chain (AnthropicModel and
		// openaiResponsesModel both build their request with
		// http.NewRequestWithContext(ctx, ...); the eino-ext openai provider
		// passes ctx into CreateChatCompletionStream), which makes the
		// provider's own read goroutine observe an error and run its
		// deferred sw.Close()/sw.Send() — unblocking Recv from the writer
		// side, the only side that actually can (see stream.go: closeSend
		// closes the items channel; closeRecv, which is what sr.Close() on
		// OUR side calls, closes an unrelated channel recv() never selects
		// on).
		attemptCtx, cancelAttempt := context.WithCancel(ctx)
		sr, openIdx, openErr := r.openStreamChain(attemptCtx, in, opts, nextOpenStart)
		// Every open call consumes the advance requested by the previous
		// iteration's failover, if any: the NEXT retry (whether triggered by
		// this open failing, or by a later mid-stream error) goes back to
		// preferring provider 0 unless a fresh non-retryable-class mid-stream
		// error requests another advance below. This keeps every existing
		// retryable-error code path (W-C-07's per-provider budgets, the
		// openErr branch's shared-budget fallback) searching the chain
		// exactly as it did before nextOpenStart existed.
		nextOpenStart = 0
		if openErr != nil {
			cancelAttempt()
			lastErr = openErr
			// openIdx is -1 here: EVERY provider's setup failed this round,
			// so there is no single provider to attribute the budget to —
			// fall back to the shared cfg.MaxRetries, same as before W-C-07.
			if isRetryableStreamErr(ctx, openErr) && errAttempts < r.cfg.MaxRetries {
				errAttempts++
				if !r.sleepRetry(ctx, sw, onRetry, errAttempts, r.cfg.MaxRetries, lastErr) {
					return
				}
				continue
			}
			_ = sw.Send(nil, openErr)
			return
		}
		if openIdx != curIdx {
			// A different provider than last attempt served this stream —
			// either the very first open, or a failover after the previous
			// provider's setup started failing. Either way, this provider's
			// retry budget starts fresh (see curIdx's doc above).
			//
			// W-C-10: curIdx >= 0 excludes the very first open (nothing was
			// "fallen back from" yet — recording one there would count every
			// stream's initial provider choice as a fallback).
			if curIdx >= 0 {
				otelobs.RecordFallback(ctx, curIdx, openIdx, lastErr)
			}
			curIdx = openIdx
			errAttempts = 0
		}
		// Wrap with the idle watchdog only when at least one budget is set, so
		// a default (zero, zero) config never allocates the extra goroutine —
		// see ResilientConfig.FirstChunkTimeout for why that matters.
		var rd streamRecver = sr
		if r.cfg.FirstChunkTimeout > 0 || r.cfg.IdleTimeout > 0 {
			rd = newWatchdogReader(sr, r.cfg.FirstChunkTimeout, r.cfg.IdleTimeout, cancelAttempt)
		}
		outcome, recvErr := consumeStream(ctx, rd, sw, &deliveredTools)
		sr.Close()
		// Always cancel: on a clean EOF/normal error this just releases
		// attemptCtx's bookkeeping a little early (harmless, ctx.Done()
		// unrelated); on a watchdog timeout the watchdog already called this
		// same func, and CancelFunc is idempotent.
		cancelAttempt()
		switch outcome {
		case streamDone:
			return
		case streamEmpty:
			if emptyAttempts < r.cfg.MaxEmptyRetries {
				emptyAttempts++
				lastErr = errors.New("empty stream")
				if !r.sleepRetry(ctx, sw, onRetry, emptyAttempts, r.cfg.MaxEmptyRetries, lastErr) {
					return
				}
				continue
			}
			_ = sw.Send(nil, fmt.Errorf("eino: empty stream after %d retries", r.cfg.MaxEmptyRetries))
			return
		case streamErr:
			lastErr = recvErr
			// Retry only while no tool call has been delivered (retrying after a
			// tool call would duplicate it) and the error is transient.
			retryable := isRetryableStreamErr(ctx, recvErr) && !deliveredTools
			// curIdx is well-defined here: the stream that just failed
			// mid-flight was successfully opened by openStreamChain above,
			// so this IS a single provider's retry budget (W-C-07), unlike
			// the openErr branch's forced-global fallback.
			maxRetries := r.maxRetriesFor(curIdx)
			if retryable && errAttempts < maxRetries {
				errAttempts++
				if !r.sleepRetry(ctx, sw, onRetry, errAttempts, maxRetries, lastErr) {
					return
				}
				continue
			}
			// W-C-13: a mid-stream error that classifies as non-retryable
			// (real 4xx, context overflow — see isNonRetryableClientErr) means
			// THIS provider will keep saying no to the same request; it does
			// not mean the whole chain should give up. Before this branch
			// existed, only OPEN-time errors ever advanced past a provider
			// (openStreamChain's unconditional loop), so a provider that
			// accepted the connection and only failed once the request itself
			// was inspected (e.g. a 404 on an unknown model) terminated the
			// entire call even with healthy providers left in the chain — the
			// gap TestResilientModel_StreamFailoverOnNonRetryableMidStreamErr
			// pins closed. Scoped to isFailoverEligibleErr specifically (not
			// "any non-retryable outcome") so this never fires for a provider
			// that is merely out of its own retry budget on an
			// otherwise-retryable error — that case is unaffected and still
			// terminates here, exactly as before this branch existed.
			//
			// isFailoverEligibleErr, not isNonRetryableClientErr: a
			// content-safety refusal is ALSO non-retryable (W-C-13 — see
			// isNonRetryableClientErr's own doc comment), but Ruling RC-11
			// (review-whole.md M-5) carves it out of failover specifically —
			// see isFailoverEligibleErr's doc comment for why, and
			// TestResilientModel_StreamContentSafetyDoesNotFailOver for the
			// pinning test.
			if !deliveredTools && curIdx+1 < len(r.chain) && isFailoverEligibleErr(recvErr) {
				// No RecordFallback call here: the next iteration's
				// openStreamChain call will land on some provider >=
				// curIdx+1, and its openIdx != curIdx branch above already
				// records curIdx -> (the provider that ACTUALLY served the
				// retry) using lastErr (== recvErr, untouched since the
				// assignment above). Recording here too would double-count
				// the fallback, and — if curIdx+1 itself fails to open and
				// the search cascades further — would record the wrong
				// target index.
				//
				// sleepRetry IS still called here, purely for its onRetry
				// side effect: Stream's "Overwrite" contract (see its doc
				// comment) requires every mid-stream retry — same-provider or
				// not — to arm the consumer's discard-partial signal before a
				// regenerated stream is re-fed, or the WS handler's saved
				// transcript concatenates this provider's abandoned partial
				// with curIdx+1's full answer instead of replacing it
				// (TestResilientModel_StreamFailoverArmsPartialDiscard
				// pins this). attempt/maxAttempts are both 1: curIdx+1 has
				// never been tried this round, so there is no same-provider
				// budget to report — "1/1" names this hand-off, not a retry
				// count.
				if !r.sleepRetry(ctx, sw, onRetry, 1, 1, recvErr) {
					return
				}
				nextOpenStart = curIdx + 1
				errAttempts = 0
				continue
			}
			_ = sw.Send(nil, recvErr)
			return
		}
	}
}

// openStreamChain opens a stream from the first provider at or after start
// whose setup succeeds, failing over across the rest of the chain. Returns
// the last setup error when all of them fail.
//
// start exists for W-C-13: runStream's streamErr case sets it past a provider
// that just gave a failover-eligible mid-stream refusal (a real 4xx or
// context overflow — see isFailoverEligibleErr; NOT a content-safety
// rejection, carved out by Ruling RC-11) so this search does not immediately
// re-open the same provider that will just refuse again. Every OTHER caller
// of this method passes 0 — the pre-W-C-13 "always search from the top"
// behavior — so start does not change how any existing retryable-error path
// resolves its next provider.
//
// The returned int is the chain index of the provider that actually served
// the stream (-1 on total failure), so the caller (runStream) can resolve
// that provider's own retry ceiling (W-C-07's maxRetriesFor) instead of the
// single shared cfg.MaxRetries every open used before this field existed.
func (r *ResilientChatModel) openStreamChain(ctx context.Context, in []*schema.Message, opts []model.Option, start int) (*schema.StreamReader[*schema.Message], int, error) {
	var lastErr error
	for i := start; i < len(r.chain); i++ {
		p := r.chain[i]
		sr, err := p.Stream(ctx, in, opts...)
		if err == nil {
			return sr, i, nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = errors.New("eino: no stream providers")
	}
	return nil, -1, lastErr
}

// streamRecver is the one capability consumeStream needs from its source:
// receive the next message or an error. It exists so watchdogReader —  which
// is not, and cannot pretend to be, a *schema.StreamReader[*schema.Message] —
// can sit between the real provider stream and consumeStream without
// consumeStream's signature growing a config parameter. The interface has two
// real implementations (the raw *schema.StreamReader and watchdogReader), not
// one, so it is not the "interface for a single implementation" pattern this
// codebase otherwise avoids.
type streamRecver interface {
	Recv() (*schema.Message, error)
}

// consumeStream drains sr, forwarding each non-blank message to sw. It sets
// *deliveredTools once any tool call is seen. Blank deltas (no content,
// reasoning, tool calls, or usage) are dropped so an all-empty stream stays
// undelivered and triggers an empty-retry. Returns streamDone on clean EOF,
// streamEmpty on an EOF that delivered no SUBSTANTIVE content (reasoning alone
// does not count — a "thought but produced nothing" response is incomplete and
// is retried), or streamErr with the recv error. No prefix-skip: on retry the
// caller re-feeds the regenerated stream in full and the consumer discards its
// partial (overwrite semantics).
func consumeStream(ctx context.Context, sr streamRecver, sw *schema.StreamWriter[*schema.Message],
	deliveredTools *bool) (streamOutcome, error) {
	// sawSubstantive tracks whether THIS attempt delivered real output — content
	// or a tool call (NOT reasoning). A reasoning-only response is treated as
	// incomplete (empty) so the model gets another chance to produce an answer.
	sawSubstantive := false
	for {
		if err := ctx.Err(); err != nil {
			return streamErr, err
		}
		msg, err := sr.Recv()
		if errors.Is(err, io.EOF) {
			if !sawSubstantive && !*deliveredTools {
				return streamEmpty, nil
			}
			return streamDone, nil
		}
		if err != nil {
			return streamErr, err
		}
		if msg == nil {
			continue
		}
		if isBlank(msg) {
			continue // no-op delta; dropping keeps empty-stream detection clean
		}
		if sw.Send(msg, nil) {
			return streamDone, nil // consumer closed the stream
		}
		if msg.Content != "" || len(msg.ToolCalls) > 0 {
			sawSubstantive = true
		}
		if len(msg.ToolCalls) > 0 {
			*deliveredTools = true
		}
	}
}

// isBlank reports whether msg carries nothing useful (no content, reasoning,
// tool calls, or usage) — a no-op delta that can be dropped without affecting
// the consumer. Used to keep empty-stream detection clean (an all-blank stream
// yields streamEmpty, not streamDone).
func isBlank(msg *schema.Message) bool {
	if msg == nil {
		return true
	}
	hasUsage := msg.ResponseMeta != nil && msg.ResponseMeta.Usage != nil
	return msg.Content == "" && msg.ReasoningContent == "" && len(msg.ToolCalls) == 0 && !hasUsage
}

// sleepRetry fires the progress callback, then sleeps for the exponential
// backoff delay. It returns false (after sending the user-cancel error) when
// the USER-CANCEL context is cancelled during the sleep, true otherwise.
//
// The sleep watches userCancelCtx (bound via WithUserCancelCtx), NOT the per-
// call ctx: a network/transport cancel of the http request's own context
// (callCtx/turnCtx) is transient and must NOT abort the backoff — only a real
// user-initiated stop (Ctrl-C / turn deadline) should. When no userCancelCtx
// is bound, userCancelCtxFrom falls back to ctx, preserving legacy behavior.
func (r *ResilientChatModel) sleepRetry(ctx context.Context, sw *schema.StreamWriter[*schema.Message],
	onRetry RetryCallback, attempt, maxAttempts int, err error) bool {
	delay := r.backoffFor(attempt, err)
	if onRetry != nil {
		onRetry(attempt, maxAttempts, err, delay)
	}
	// C4 OBS2: record the retry metric. Constraint 6: this is the ONLY retry
	// point in the system. otelobs.RecordRetry uses the global otel.Meter, so
	// no constructor or setter wiring is needed.
	otelobs.RecordRetry(ctx, attempt, maxAttempts, err)
	ucCtx := userCancelCtxFrom(ctx)
	select {
	case <-ucCtx.Done():
		_ = sw.Send(nil, ucCtx.Err())
		return false
	case <-time.After(delay):
		return true
	}
}

// isRetryableStreamErr reports whether a stream/model error is worth retrying:
// a dropped or malformed upstream stream (the common gateway failure), a
// transport error, or a server/rate-limit condition. The openai acl wraps mid-
// stream Recv failures as "failed to receive stream chunk: <cause>"; "unexpected
// EOF" and "net/http: request canceled (Client.Timeout …)" are typical causes.
//
// The verdict itself comes from ClassifyError (errclass.go), which is a real
// classifier: typed status first, anchored status patterns second, keywords
// last. It replaced two flat substring tables whose most visible defect was a
// bare match on "404" — any error whose BODY mentioned that number was filed as
// a non-retryable client error, so an upstream 500 quoting a 404 from its own
// origin gave up without a single retry. The anchored patterns require the
// digits to be introduced by a status-shaped phrase, so the body text no longer
// decides.
//
// Two conditions still short-circuit ahead of classification:
//
// User-initiated cancellation is detected via the user-cancel context (bound by
// WithUserCancelCtx, falling back to the passed ctx when absent), NOT the error
// chain and NOT the per-call ctx. The per-call ctx (callCtx/turnCtx) may be
// canceled by an http.Client.Timeout or a transport error — those are transient
// and the caller still wants the answer, so they must not suppress retry. The
// WS handler binds the TURN context (the one wired to Ctrl-C / turn deadline)
// as the user-cancel ctx; when THAT is canceled the user wants to stop, so we
// don't retry. (Checking errors.Is(err, context.Canceled) instead would wrongly
// suppress retry on every client-timeout, since the timeout's cancel is wrapped
// in the error even though the turn itself is fine.)
//
// ClassUnknown falls back to the legacy transient markers. Those markers are
// deliberately looser than the classifier's (a bare "eof", a bare "retry"), and
// keeping them as a floor means this change can only ever ADD retries relative
// to the previous behavior, never remove one that used to happen.
func isRetryableStreamErr(ctx context.Context, err error) bool {
	if err == nil {
		return false
	}
	// User-initiated cancel/deadline → the caller wants to stop; do not retry.
	// Watch the user-cancel ctx (not the per-call ctx) so a network/transport
	// cancel of the request's own context is NOT misread as a user cancel.
	if ucCtx := userCancelCtxFrom(ctx); ucCtx.Err() != nil {
		return false
	}
	// ErrStreamIdle (W-A-06) is our own sentinel, not a provider error: a
	// gateway that connects and then sends nothing is a network-level stall,
	// always transient. Checked by identity, ahead of ClassifyError, so this
	// does not depend on any keyword list (M8 replaced exactly this kind of
	// fragile substring matching for provider errors; ErrStreamIdle's message
	// deliberately avoids "timeout"/"eof"/etc. so no keyword table accidentally
	// covers for a missing check here — see the wording note on ErrStreamIdle).
	if errors.Is(err, ErrStreamIdle) {
		return true
	}
	switch ClassifyError(err).Class {
	case ClassTransient, ClassRateLimit:
		return true
	case ClassClientError, ClassContextOverflow, ClassContentSafety:
		// Real 4xx client error (bad key, unknown model, malformed request), an
		// over-long prompt, or a content-policy rejection (W-C-13): retry is
		// pointless and masks the root cause. Overflow additionally needs
		// compaction to shrink the context first — an unchanged retry
		// reproduces the same prompt. Content-safety is listed explicitly here
		// (rather than left to fall through to the legacy marker floor below)
		// so it can never accidentally match a legacy transient marker by
		// coincidence — the same reason ClassClientError/ClassContextOverflow
		// are listed explicitly instead of falling through.
		return false
	}
	// ClassUnknown: fall back to the historical loose markers so nothing that
	// used to retry stops retrying.
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	s := strings.ToLower(err.Error())
	for _, m := range legacyTransientMarkers {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}

// isNonRetryableClientErr reports whether err is a real 4xx client error, a
// context overflow, or a content-safety rejection (W-C-13) — the conditions on
// which retry wastes time and masks the underlying problem. Shared by the
// Generate (retry) and Stream (isRetryableStreamErr) paths so both agree by
// construction on the "don't retry THIS provider" question.
//
// It is NOT the predicate for "should the chain fail over to the next
// provider" — that question has a different, narrower answer since Ruling
// RC-11 (review-whole.md M-5): see isFailoverEligibleErr and Generate's own
// isContentSafetyRefusal check, both of which exclude ClassContentSafety
// from failover while this function continues to include it in "don't retry
// this provider", per W-C-13's unchanged requirement.
//
// It delegates to ClassifyError rather than scanning text itself; see
// isRetryableStreamErr for what that fixed.
func isNonRetryableClientErr(err error) bool {
	if err == nil {
		return false
	}
	c := ClassifyError(err).Class
	return c == ClassClientError || c == ClassContextOverflow || c == ClassContentSafety
}

// isFailoverEligibleErr reports whether a mid-stream error should advance
// runStream's streamErr branch to the next provider in the chain, rather
// than surface directly. It is isNonRetryableClientErr with ClassContentSafety
// excluded — Ruling RC-11 (review-whole.md M-5): a content-safety refusal
// means the REQUEST was refused on content-policy grounds, not that this
// provider is unhealthy. Every other provider in the chain would receive the
// exact same content; advancing anyway would silently resend it hoping a
// laxer provider says yes, overriding the refusing provider's safety
// judgment instead of surfacing it to the caller. isNonRetryableClientErr
// itself is unchanged — content safety still means "don't retry this
// provider" (W-C-13), it just no longer also means "try the next one."
//
// Generate has its own symmetric carve-out (isContentSafetyRefusal, checked
// directly in the Generate loop) rather than reusing this function, because
// Generate's chain loop has no isNonRetryableClientErr gate to subtract
// from in the first place — every OTHER error class there already advances
// unconditionally (see Generate's own comment), so only content safety needs
// an explicit stop, not a "which classes may advance" allowlist.
func isFailoverEligibleErr(err error) bool {
	if err == nil {
		return false
	}
	c := ClassifyError(err).Class
	return c == ClassClientError || c == ClassContextOverflow
}

// isContentSafetyRefusal reports whether err classifies as ClassContentSafety
// — the Ruling RC-11 failover carve-out (see isFailoverEligibleErr's doc
// comment for the full reasoning). Used directly by Generate's chain loop
// and, via isFailoverEligibleErr's exclusion, indirectly by runStream's
// streamErr branch.
func isContentSafetyRefusal(err error) bool {
	return err != nil && ClassifyError(err).Class == ClassContentSafety
}

// legacyTransientMarkers are the pre-classifier substrings that marked a
// stream/model error as transient. They are retained as the ClassUnknown
// FLOOR, not as the primary rule: several are too loose to classify on (a bare
// "eof" matches any message ending in that word, "retry" matches our own retry
// bookkeeping), but they only ever run after ClassifyError declined to answer,
// so a loose match can add a retry and can never suppress one.
var legacyTransientMarkers = []string{
	"unexpected eof",
	"failed to receive stream chunk",
	"request canceled",
	"connection reset",
	"broken pipe",
	"connection refused",
	"no such host",
	"transport",
	"timeout",
	"timed out",
	"eof",
	"502 bad gateway",
	"503 service unavailable",
	"504 gateway time-out",
	"server returned non-200",
	"rate limit",
	"too many requests",
	"retry",
}

// retry runs call, retrying transient failures with exponential backoff up to
// maxRetries (W-C-07: the caller resolves this per-provider via
// maxRetriesFor before calling in). Empty-response retries are independent
// and always use cfg.MaxEmptyRetries — W-C-07 only scoped the error-retry
// axis, since the spec's acceptance criterion ("每 provider 独立
// MaxRetries") names retries, not the separate empty-response cap.
func (r *ResilientChatModel) retry(ctx context.Context, maxRetries int, call func() (*schema.Message, error)) (*schema.Message, error) {
	maxEmpty := r.cfg.MaxEmptyRetries
	var lastErr error
	// errAttempts / emptyAttempts are independent caps on consecutive retries
	// of each kind within one provider. Either being exhausted returns the
	// relevant error; a successful non-empty call returns immediately.
	errAttempts := 0
	emptyAttempts := 0
	for {
		// Backoff grows with total consecutive retries of either kind.
		attempt := errAttempts + emptyAttempts
		if attempt > 0 {
			// Watch userCancelCtx (not the per-call ctx): a network/transport
			// cancel of the request's own context must NOT abort the backoff —
			// only a real user-initiated stop should. Falls back to ctx when
			// no userCancelCtx is bound (legacy callers).
			ucCtx := userCancelCtxFrom(ctx)
			select {
			case <-ucCtx.Done():
				return nil, ucCtx.Err()
			case <-time.After(r.backoffFor(attempt, lastErr)):
			}
		}
		msg, err := call()
		if err == nil {
			if isEmpty(msg) {
				emptyAttempts++
				if emptyAttempts > maxEmpty {
					return nil, fmt.Errorf("eino: empty response after %d retries", maxEmpty)
				}
				lastErr = &RetryableModelError{Err: errors.New("empty response")}
				continue
			}
			return msg, nil
		}
		lastErr = err
		// Real 4xx client error (bad key, unknown model, …): don't retry. This
		// short-circuit mirrors isRetryableStreamErr's so both paths share one
		// chokepoint (isNonRetryableClientErr). Checked before IsRetryableModelErr
		// because a RetryableModelError wrapping a 4xx payload would otherwise
		// retry pointlessly.
		if isNonRetryableClientErr(err) {
			return nil, err
		}
		if !IsRetryableModelErr(err) {
			return nil, err
		}
		errAttempts++
		if errAttempts > maxRetries {
			return nil, lastErr
		}
	}
}

// peekStream and forwardStream were removed: Stream now uses a retrying reader
// (runStream + consumeStream + skipMessage) that retries the provider call on
// mid-stream errors with prefix-skip dedup, instead of peeking once.

func (r *ResilientChatModel) backoff(attempt int) time.Duration {
	d := time.Duration(float64(r.cfg.BaseDelay) * math.Pow(2, float64(attempt-1)))
	if d < 0 || d > r.cfg.MaxDelay {
		return r.cfg.MaxDelay
	}
	return d
}

// backoffFor picks the backoff schedule appropriate to WHY the retry is
// happening (M1). Rate limits get their own schedule; everything else keeps the
// ordinary exponential.
//
// The two cases are not the same problem. A transient network failure wants a
// fast retry — the connection may already be back, and the config's 200ms/5s
// pair is tuned for that. A 429 is the server saying it is over capacity, and
// the ordinary schedule attacks it: ten retries at 200ms..5s all land inside
// the same one-minute bucket, all fail, and all count against the limit, so a
// throttle that would have cleared in 30 seconds is extended by our own
// traffic. RateLimitBackoff honours the server's Retry-After when it sent one
// (it knows when the bucket refills; guessing shorter only burns another
// request) and otherwise starts at 5s.
//
// The rate-limit delay deliberately ignores cfg.MaxDelay. That cap exists to
// bound the transient schedule, and applying it here would clamp a server's
// explicit "wait 30s" back down to 5s — reintroducing the exact hammering this
// function exists to stop. The rate-limit path has its own ceilings instead:
// RateLimitMaxDelay for the exponential, MaxRetryAfter for a requested wait.
func (r *ResilientChatModel) backoffFor(attempt int, err error) time.Duration {
	if c := ClassifyError(err); c.Class == ClassRateLimit {
		return RateLimitBackoffWith(attempt, c.RetryAfter, r.cfg.RateLimitBase, r.cfg.RateLimitMax)
	}
	return r.backoff(attempt)
}

var _ model.BaseChatModel = (*ResilientChatModel)(nil)
