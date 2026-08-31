package tools

import (
	"context"
	"sync/atomic"
)

// errCtKey is a context key for a per-turn consecutive-tool-error counter.
// When this counter reaches 5, the GuardedTool returns a Go error instead of
// converting the failure to a tool result, aborting the turn so the user sees
// "tool failed 5 consecutive times" instead of the model retrying forever.
// The counter is reset to 0 on every successful tool call.
type errCtKey struct{}

// errCounter is the per-turn consecutive-error counter.
//
// W-F-22: it must be ATOMIC. One turn's tool calls share one counter (it is
// bound once per turn), and the ADK dispatches a turn's tool calls in
// parallel — two GuardedTool.InvokableRun calls racing on a plain *int
// (`*c++` vs `*c = 0`) is a data race that -race flags and that in the worst
// interleaving loses a failure count. Add/Store make the increment and the
// reset each atomic; the "5 consecutive" semantics are best-effort under
// concurrency either way (parallel calls have no total order), which is fine:
// the breaker exists to stop a retry loop, not to arbitrate parallel runs.
type errCounter struct {
	n atomic.Int64
}

// fail increments the counter and reports whether the breaker threshold is
// reached.
func (c *errCounter) fail(threshold int64) bool {
	return c.n.Add(1) >= threshold
}

// reset zeroes the counter after a successful call.
func (c *errCounter) reset() { c.n.Store(0) }

// value snapshots the count (diagnostics).
func (c *errCounter) value() int64 { return c.n.Load() }

// WithErrCounter injects a fresh consecutive-error counter into ctx.
// Call before each turn (WS handler turnCtx / SSE handler ctx).
func WithErrCounter(ctx context.Context) context.Context {
	return context.WithValue(ctx, errCtKey{}, &errCounter{})
}

// getErrCounter returns the per-turn consecutive-error counter from ctx, or nil.
func getErrCounter(ctx context.Context) *errCounter {
	c, _ := ctx.Value(errCtKey{}).(*errCounter)
	return c
}
