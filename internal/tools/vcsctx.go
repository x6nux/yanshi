package tools

import (
	"context"

	"github.com/x6nux/yanshi/internal/vcs"
)

// VCSScope identifies the active editing scope for auto-tracking edits.
//   - VCS is the VCS instance (nil ⇒ tracking not configured; fs hooks no-op).
//   - RepoID is the main working-copy repo id (chat/orchestrator edits land here).
//   - WorktreeID, when non-empty, routes edits to that worktree's changeset.
//   - Agent is the acting agent id (commit author attribution).
type VCSScope struct {
	VCS        *vcs.VCS
	RepoID     string
	WorktreeID string
	Agent      string
}

type vcsCtxKey struct{}

// WithVCS binds a VCSScope to ctx.
func WithVCS(ctx context.Context, s VCSScope) context.Context {
	return context.WithValue(ctx, vcsCtxKey{}, s)
}

// VCSScopeFromContext returns the bound scope, if any.
func VCSScopeFromContext(ctx context.Context) (VCSScope, bool) {
	s, ok := ctx.Value(vcsCtxKey{}).(VCSScope)
	return s, ok
}
