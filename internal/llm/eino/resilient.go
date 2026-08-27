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
	MaxRetries int
	BaseDelay  time.Duration
	MaxDelay   time.Duration

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
	for _, p := range r.chain {
		msg, err := r.retry(ctx, func() (*schema.Message, error) { return p.Generate(ctx, in, opts...) })
		if err == nil {
			return msg, nil
		}
		lastErr = err
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
	)
	for {
		sr, openErr := r.openStreamChain(ctx, in, opts)
		if openErr != nil {
			lastErr = openErr
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
		outcome, recvErr := consumeStream(ctx, sr, sw, &deliveredTools)
		sr.Close()
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
			if retryable && errAttempts < r.cfg.MaxRetries {
				errAttempts++
				if !r.sleepRetry(ctx, sw, onRetry, errAttempts, r.cfg.MaxRetries, lastErr) {
					return
				}
				continue
			}
			_ = sw.Send(nil, recvErr)
			return
		}
	}
}

// openStreamChain opens a stream from the first provider whose setup succeeds,
// failing over across the chain. Returns the last setup error when all fail.
func (r *ResilientChatModel) openStreamChain(ctx context.Context, in []*schema.Message, opts []model.Option) (*schema.StreamReader[*schema.Message], error) {
	var lastErr error
	for _, p := range r.chain {
		sr, err := p.Stream(ctx, in, opts...)
		if err == nil {
			return sr, nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = errors.New("eino: no stream providers")
	}
	return nil, lastErr
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
func consumeStream(ctx context.Context, sr *schema.StreamReader[*schema.Message], sw *schema.StreamWriter[*schema.Message],
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
	switch ClassifyError(err).Class {
	case ClassTransient, ClassRateLimit:
		return true
	case ClassClientError, ClassContextOverflow:
		// Real 4xx client error (bad key, unknown model, malformed request) or
		// an over-long prompt: retry is pointless and masks the root cause.
		// Overflow additionally needs compaction to shrink the context first —
		// an unchanged retry reproduces the same prompt.
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

// isNonRetryableClientErr reports whether err is a real 4xx client error or a
// context overflow — the two conditions on which retry wastes time and masks
// the underlying problem. Shared by the Generate (retry) and Stream
// (isRetryableStreamErr) paths so both agree by construction.
//
// It delegates to ClassifyError rather than scanning text itself; see
// isRetryableStreamErr for what that fixed.
func isNonRetryableClientErr(err error) bool {
	if err == nil {
		return false
	}
	c := ClassifyError(err).Class
	return c == ClassClientError || c == ClassContextOverflow
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

func (r *ResilientChatModel) retry(ctx context.Context, call func() (*schema.Message, error)) (*schema.Message, error) {
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
		if errAttempts > r.cfg.MaxRetries {
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
