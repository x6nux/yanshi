// internal/llm/eino/ratelimit.go
//
// M7: per-model request rate limiting.
//
// Sub-agent fan-out is the motivating case. `agent_start` on five sub-agents
// puts five ReAct loops on the same provider key within a second, and a
// provider whose plan allows 20 requests per minute answers most of them with
// 429. Every one of those becomes a retry on the rate-limit schedule (M1), so
// the burst that took a second to send takes minutes to drain — and each failed
// attempt still counted against the quota that caused it. Throttling BEFORE the
// request is the only version of this that does not spend the quota to discover
// the limit.
//
// WHY A HAND-WRITTEN BUCKET. golang.org/x/time/rate is the obvious choice and
// is NOT in go.mod (checked: the module requires golang.org/x/crypto, /sys,
// /term, /text, /net, /exp and /arch — no /time). Adding a module dependency
// for ~60 lines of arithmetic is a worse trade than writing the arithmetic,
// especially for a package that already vendors a bubbletea fork to avoid a
// different upstream problem.
//
// The bucket is a classic continuous-refill token bucket rather than the
// sliding-window deque QwenPaw uses. Same steady-state rate; the difference is
// burst behaviour, and continuous refill is the better fit here because a fan-
// out arrives all at once and a deque would let the whole burst through and
// then stall the next full window.
package eino

import (
	"context"
	"sync"
	"time"
)

// RateLimitConfig configures one model's throttle.
type RateLimitConfig struct {
	// QPM is the sustained ceiling in requests per minute. ZERO MEANS NO
	// LIMIT and is the default, so an operator who configures nothing keeps
	// exactly the behaviour they had before this existed.
	QPM int
	// Burst is how many requests may be issued back-to-back after an idle
	// period. Zero defaults to one minute's worth capped at defaultMaxBurst;
	// see that constant for why the cap exists.
	Burst int
}

// defaultMaxBurst caps the burst a zero Burst derives from QPM.
//
// Without a cap, `qpm: 600` would authorise 600 simultaneous requests, which
// is not a rate limit in any sense the operator meant — the point of the
// setting is that the provider cannot absorb an unbounded burst. Ten is enough
// to absorb a realistic sub-agent fan-out without serialising it.
const defaultMaxBurst = 10

// TokenBucket is a continuous-refill token bucket: capacity `burst`, refilled
// at `qpm/60` tokens per second. Safe for concurrent use.
//
// A zero-QPM bucket is UNLIMITED, not empty. That inversion is deliberate and
// is the reason Wait has an early return: the disabled state must cost no lock,
// no clock read and no scheduling, because it is the default and it is on the
// hot path of every model call.
type TokenBucket struct {
	mu       sync.Mutex
	capacity float64
	perSec   float64
	tokens   float64
	last     time.Time
	// now is the clock, injected so tests can drive refill deterministically
	// rather than by sleeping. nil means time.Now.
	now func() time.Time
}

// NewTokenBucket returns a bucket for cfg. A non-positive QPM yields a bucket
// whose Wait always returns immediately.
func NewTokenBucket(cfg RateLimitConfig) *TokenBucket {
	if cfg.QPM <= 0 {
		return &TokenBucket{}
	}
	burst := cfg.Burst
	if burst <= 0 {
		burst = cfg.QPM
		if burst > defaultMaxBurst {
			burst = defaultMaxBurst
		}
	}
	if burst < 1 {
		burst = 1
	}
	return &TokenBucket{
		capacity: float64(burst),
		perSec:   float64(cfg.QPM) / 60,
		tokens:   float64(burst),
	}
}

// Enabled reports whether the bucket limits anything.
func (b *TokenBucket) Enabled() bool { return b != nil && b.perSec > 0 }

// clock returns the injected clock or time.Now.
func (b *TokenBucket) clock() time.Time {
	if b.now != nil {
		return b.now()
	}
	return time.Now()
}

// reserve takes one token, returning how long the caller must wait before it is
// actually available. A zero duration means "go now". The token is deducted
// even when the wait is positive, so concurrent callers queue at distinct times
// instead of all waking to find one token.
func (b *TokenBucket) reserve() time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.clock()
	if b.last.IsZero() {
		b.last = now
	}
	if elapsed := now.Sub(b.last); elapsed > 0 {
		b.tokens += elapsed.Seconds() * b.perSec
		if b.tokens > b.capacity {
			b.tokens = b.capacity
		}
		b.last = now
	}
	b.tokens--
	if b.tokens >= 0 {
		return 0
	}
	// tokens is now negative: -tokens is how many refill periods away this
	// caller's turn is.
	return time.Duration(-b.tokens / b.perSec * float64(time.Second))
}

// Wait blocks until this call is allowed to proceed, or until ctx is done.
//
// It returns ctx.Err() on cancellation and nil otherwise. Honouring ctx is not
// optional: a user pressing Ctrl-C while a fan-out is queued behind a 20 QPM
// limit would otherwise wait out the whole queue before the turn noticed it had
// been cancelled.
func (b *TokenBucket) Wait(ctx context.Context) error {
	if !b.Enabled() {
		return nil
	}
	delay := b.reserve()
	if delay <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(delay)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// RateLimiter holds one TokenBucket per model id.
//
// Per-model and not per-process because the limits being respected are
// per-model: a background summarisation hammering a cheap model must not stall
// the user's turn on an expensive one, and two models behind the same key have
// independent quotas at every provider yanshi supports.
type RateLimiter struct {
	mu      sync.Mutex
	buckets map[string]*TokenBucket
	// def is the config applied to a model with no specific entry.
	def RateLimitConfig
	// perModel holds explicit per-model configuration.
	perModel map[string]RateLimitConfig
}

// NewRateLimiter builds a limiter with a default config and optional per-model
// overrides. A model absent from perModel uses def; a def of {QPM: 0} (the
// zero value) leaves every unconfigured model unlimited.
func NewRateLimiter(def RateLimitConfig, perModel map[string]RateLimitConfig) *RateLimiter {
	cp := make(map[string]RateLimitConfig, len(perModel))
	for k, v := range perModel {
		cp[k] = v
	}
	return &RateLimiter{buckets: map[string]*TokenBucket{}, def: def, perModel: cp}
}

// bucketFor returns (creating on first use) the bucket for model.
func (l *RateLimiter) bucketFor(model string) *TokenBucket {
	l.mu.Lock()
	defer l.mu.Unlock()
	if b, ok := l.buckets[model]; ok {
		return b
	}
	cfg := l.def
	if specific, ok := l.perModel[model]; ok {
		cfg = specific
	}
	b := NewTokenBucket(cfg)
	l.buckets[model] = b
	return b
}

// Wait throttles one call to model, blocking until allowed or ctx is done.
// A nil limiter never throttles, so callers need no nil check.
func (l *RateLimiter) Wait(ctx context.Context, model string) error {
	if l == nil {
		return nil
	}
	return l.bucketFor(model).Wait(ctx)
}

// Enabled reports whether any throttle would apply to model. Used by callers
// that want to skip bookkeeping (and by tests) rather than to decide policy.
func (l *RateLimiter) Enabled(model string) bool {
	if l == nil {
		return false
	}
	return l.bucketFor(model).Enabled()
}
