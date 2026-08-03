package tools

import (
	"context"
	"fmt"

	"github.com/x6nux/yanshi/internal/guard"
)

type availableModelsKey struct{}

// WithAvailableModels stores the set of available model names in context.
func WithAvailableModels(ctx context.Context, models map[string]bool) context.Context {
	return context.WithValue(ctx, availableModelsKey{}, models)
}

// AvailableModelsFromContext retrieves the set of available model names from
// context, or nil if none was set.
func AvailableModelsFromContext(ctx context.Context) map[string]bool {
	if v, ok := ctx.Value(availableModelsKey{}).(map[string]bool); ok {
		return v
	}
	return nil
}

// ValidateOverride checks whether a model name and/or reasoning effort override
func ValidateOverride(ctx context.Context, model, reasoning string, profile guard.PermissionProfile, available map[string]bool) error {
	if model == "" && reasoning == "" {
		return nil
	}
	if available == nil {
		available = AvailableModelsFromContext(ctx)
	}
	if model != "" {
		if available == nil || !available[model] {
			return fmt.Errorf("model %q is not available in provider registry: %w", model, guard.ErrOverrideDenied)
		}
		if err := profile.Subagent.CheckModel(model); err != nil {
			return fmt.Errorf("%w: %v", guard.ErrOverrideDenied, err)
		}
	}
	if reasoning != "" {
		if err := profile.Subagent.CheckReasoning(reasoning); err != nil {
			return fmt.Errorf("%w: %v", guard.ErrOverrideDenied, err)
		}
	}
	return nil
}

// EnsureOverrideForResume validates override params when resuming a sub-agent.
func EnsureOverrideForResume(ctx context.Context, model, reasoning string, profile guard.PermissionProfile, available map[string]bool) error {
	return ValidateOverride(ctx, model, reasoning, profile, available)
}
