package tools

import (
	"context"
	"errors"
	"path/filepath"
	"strings"

	"github.com/x6nux/yanshi/internal/guard"
)

// ErrRolePolicyDenied is returned when a sub-agent role policy rejects a tool

// RolePolicy restricts a sub-agent's tool usage beyond what the parent guard
var ErrRolePolicyDenied = errors.New("denied by subagent role policy")

// RolePolicy restricts a sub-agent's tool usage beyond what the parent guard
type RolePolicy struct {
	ReadOnlyShell bool
	WritePatterns []string
	// WithRolePolicy stores a role policy in context for downstream enforcement.
}

type rolePolicyKey struct{}

// RolePolicyFromContext retrieves the role policy from context, or false if
// WithRolePolicy stores a role policy in context for downstream enforcement.
func WithRolePolicy(ctx context.Context, p RolePolicy) context.Context {
	return context.WithValue(ctx, rolePolicyKey{}, p)
}

// RolePolicyFromContext retrieves the role policy from context, or false if
func RolePolicyFromContext(ctx context.Context) (RolePolicy, bool) {
	p, ok := ctx.Value(rolePolicyKey{}).(RolePolicy)
	return p, ok
}

// CheckRolePolicy enforces role-level restrictions BEFORE Authorize consults
// session allowlist / static guard / interactive callback so that neither
// parent profile nor human approval can widen the role.
func CheckRolePolicy(ctx context.Context, action guard.Action) error {
	p, ok := RolePolicyFromContext(ctx)
	if !ok {
		return nil
	}

	if action.Tool == "shell_run" && p.ReadOnlyShell {
		cmd := strings.TrimSpace(action.Shell)
		fields := strings.Fields(cmd)
		if len(fields) == 0 {
			return ErrRolePolicyDenied
		}
		name := strings.TrimSuffix(fields[0], ".exe")
		name = filepath.Base(name)
		if !safeShellCommands[name] || hasShellMetachar(cmd) {
			return ErrRolePolicyDenied
		}
	}
	// Empty WritePatterns means no additional write restrictions (inherit parent
	// guard). Non-empty must be matched.
	if action.FS.Op == "write" && len(p.WritePatterns) > 0 {
		for _, target := range action.FS.Paths {
			if !anyGlobMatch(p.WritePatterns, target) {
				return ErrRolePolicyDenied
			}
		}
	}
	return nil
}

func anyGlobMatch(patterns []string, target string) bool {
	target = filepath.ToSlash(target)
	for _, pattern := range patterns {
		if ok, _ := filepath.Match(filepath.ToSlash(pattern), target); ok {
			return true
		}
	}
	return false
}
