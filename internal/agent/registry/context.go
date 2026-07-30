package registry

import "context"

// agentIDKey is the context key for the current sub-agent's ID.
type agentIDKey struct{}

// roleKey is the context key for the current sub-agent's role.
type roleKey struct{}

// WithCurrentAgentID binds the current sub-agent ID into ctx so that nested
// spawn calls can derive the ParentID automatically.
func WithCurrentAgentID(ctx context.Context, agentID string) context.Context {
	return context.WithValue(ctx, agentIDKey{}, agentID)
}

// CurrentAgentID returns the bound sub-agent ID, or empty string if none is
// bound.
func CurrentAgentID(ctx context.Context) string {
	id, _ := ctx.Value(agentIDKey{}).(string)
	return id
}

// WithRole binds the current sub-agent's role into ctx.
func WithRole(ctx context.Context, role string) context.Context {
	return context.WithValue(ctx, roleKey{}, role)
}

// RoleFromContext returns the bound role, or empty string if none is bound.
func RoleFromContext(ctx context.Context) string {
	role, _ := ctx.Value(roleKey{}).(string)
	return role
}
