package tools

import (
	"context"

	einollm "github.com/x6nux/yanshi/internal/llm/eino"
)

// truncationPolicyKey keys the resolved tool-output truncation policy in the
// tool-execution context.
type truncationPolicyKey struct{}

// WithTruncationPolicy binds the resolved head/tail line-retention policy
// (W-C-09) into ctx so spillover.go's spillPreview knows how much of an
// oversized tool result to keep verbatim before replacing the rest with an
// explicit "[... N lines omitted ...]" marker. The orchestrator resolves this
// ONCE at bootstrap (einollm.ResolveTruncationPolicy against the primary
// provider's override and model id, falling back to
// einollm.DefaultTruncationSpec) and installs it alongside WithWorkRoot at
// each turn's tool-execution context — see bindExecutionContext.
//
// Unlike WithWorkRoot, spec is a struct value with no meaningful "empty"
// state (a zero TruncationSpec would mean "keep nothing," which is never
// correct), so this injector is unconditional: the orchestrator always has a
// concrete resolved value to bind, never a nil to gate on.
func WithTruncationPolicy(ctx context.Context, spec einollm.TruncationSpec) context.Context {
	return context.WithValue(ctx, truncationPolicyKey{}, spec)
}

// TruncationPolicyFromContext returns the bound truncation policy, and
// whether one was bound at all. A false second return means no orchestrator
// installed a policy on this context (a sub-agent or test path that never
// called WithTruncationPolicy) — spillPreview then falls back to
// einollm.DefaultTruncationSpec, exactly as if bootstrap itself had never
// found an opinion.
func TruncationPolicyFromContext(ctx context.Context) (einollm.TruncationSpec, bool) {
	spec, ok := ctx.Value(truncationPolicyKey{}).(einollm.TruncationSpec)
	return spec, ok
}
