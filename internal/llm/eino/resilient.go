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
	streamDone streamOutcome = iota // clean EOF
	streamEmpty                     // EOF with no content delivered (ever) → empty retry
	streamErr                       // non-EOF recv error (or ctx cancel)
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
	delay := r.backoff(attempt)
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
// transport error, or a server/rate-limit marker. The openai acl wraps mid-
// stream Recv failures as "failed to receive stream chunk: <cause>"; "unexpected
// EOF" and "net/http: request canceled (Client.Timeout …)" are typical causes.
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
	// Real 4xx client error (bad key, unknown model, malformed request): retry
	// is pointless and masks the root cause. Checked AFTER user-cancel (so a
	// user cancel still wins) but BEFORE the transient markers — a wrapped
	// RetryableModelError carrying a 4xx payload must not slip through via the
	// "retry" marker or net.Error branches.
	if isNonRetryableClientErr(err) {
		return false
	}
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	s := strings.ToLower(err.Error())
	for _, m := range retryableStreamMarkers {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}

// nonRetryableClientMarkers are substrings (lowercase) that mark a model error
// as a REAL 4xx client error: bad API key, unknown model, malformed request,
// authentication failure, etc. Retrying these is pointless (the next attempt
// will hit the same config bug) and actively harmful (it masks the root cause
// behind backoff delays and retry noise). The resilient layer short-circuits
// retry when any marker matches, even if the error is wrapped in
// RetryableModelError (which would otherwise trigger retries).
//
// Kept broad on purpose: providers phrase these failures inconsistently
// ("invalid_api_key", "Incorrect API key provided", "401 Unauthorized", …), so
// we match on the most stable substring of each family. Order does not matter.
var nonRetryableClientMarkers = []string{
	"invalid_api_key",
	"incorrect api key",
	"model_not_found",
	"invalid_request_error",
	"authentication",
	"401",
	"403",
	"404",
	"422",
}

// isNonRetryableClientErr reports whether err is a real 4xx client error on
// which retry would waste time and mask the underlying config/auth problem. It
// inspects the full error string (including wrapped causes) so it works on
// RetryableModelError-wrapped forms too. Shared by the Generate (retry) and
// Stream (isRetryableStreamErr) paths — the single chokepoint for the 4xx
// short-circuit, so adding a marker here fixes both paths at once.
func isNonRetryableClientErr(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	for _, m := range nonRetryableClientMarkers {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}

// retryableStreamMarkers are substrings (lowercase) that mark a stream/model
// error as transient and worth retrying.
var retryableStreamMarkers = []string{
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
			case <-time.After(r.backoff(attempt)):
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

var _ model.BaseChatModel = (*ResilientChatModel)(nil)
