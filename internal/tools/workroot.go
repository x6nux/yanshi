package tools

import "context"

// workRootKey keys the project work root in the tool-execution context.
type workRootKey struct{}

// WithWorkRoot binds the project work root into ctx so the spillover layer
// (spillIfTooLong) knows where to write oversized tool outputs
// (<root>/.yanshi/tmp/spillover/). The orchestrator installs it alongside
// WithProfile at each turn's tool-execution context. An empty root is stored
// verbatim; spillIfTooLong treats "" as ".".
func WithWorkRoot(ctx context.Context, root string) context.Context {
	return context.WithValue(ctx, workRootKey{}, root)
}

// WorkRootFromContext returns the bound work root, or "" when none is bound
// (spillIfTooLong then falls back to ".").
func WorkRootFromContext(ctx context.Context) string {
	r, _ := ctx.Value(workRootKey{}).(string)
	return r
}
