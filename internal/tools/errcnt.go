package tools

import "context"

// errCtKey is a context key for a per-turn consecutive-tool-error counter.
// When this counter reaches 5, the GuardedTool returns a Go error instead of
// converting the failure to a tool result, aborting the turn so the user sees
// "tool failed 5 consecutive times" instead of the model retrying forever.
// The counter is reset to 0 on every successful tool call.
type errCtKey struct{}

// WithErrCounter injects a fresh consecutive-error counter (*int) into ctx.
// Call before each turn (WS handler turnCtx / SSE handler ctx).
func WithErrCounter(ctx context.Context) context.Context {
	return context.WithValue(ctx, errCtKey{}, new(int))
}

// getErrCounter returns the per-turn consecutive-error counter from ctx, or nil.
func getErrCounter(ctx context.Context) *int {
	c, _ := ctx.Value(errCtKey{}).(*int)
	return c
}
