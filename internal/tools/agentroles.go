package tools

import (
	"fmt"
	"strings"
)

// RoleDef describes a sub-agent role: its name, system-prompt prefix, the tools
// it may call, and any additional role-level policy restrictions.
type RoleDef struct {
	Name         string
	PromptPrefix string
	AllowedTools []string
	Policy       *RolePolicy // nil = no additional role restriction; non-nil == tighten parent
}

// AgentRoles returns the built-in set of sub-agent role definitions.
func AgentRoles() []RoleDef {
	return []RoleDef{
		{
			Name:         "general",
			PromptPrefix: outputContractPrefix("general-purpose assistant"),
			AllowedTools: []string{"*"},
			Policy:       nil,
		},
		{
			Name:         "explore",
			PromptPrefix: outputContractPrefix("read-only explorer. Map and quote evidence; do NOT edit."),
			AllowedTools: []string{"fs_read", "fs_glob", "fs_search", "shell_run", "time_now"},
			Policy:       &RolePolicy{ReadOnlyShell: true},
		},
		{
			Name:         "plan",
			PromptPrefix: outputContractPrefix("planner. Produce a writing-plans style plan; write only to plan workspace."),
			AllowedTools: []string{"fs_read", "fs_glob", "fs_search", "fs_write", "fs_edit", "shell_run", "time_now"},
			Policy: &RolePolicy{
				ReadOnlyShell: true,
				WritePatterns: []string{"docs/plans/*.md", "docs/superpowers/plans/*.md"},
			},
		},
		{
			Name:         "review",
			PromptPrefix: outputContractPrefix("reviewer. Read-only; flag risks and blockers."),
			AllowedTools: []string{"fs_read", "fs_glob", "fs_search", "shell_run", "time_now"},
			Policy:       &RolePolicy{ReadOnlyShell: true},
		},
		{
			Name:         "implementer",
			PromptPrefix: outputContractPrefix("implementer. Apply code changes following TDD and project conventions."),
			AllowedTools: []string{"fs_read", "fs_glob", "fs_search", "fs_write", "fs_edit", "shell_run", "time_now", "memory_*"},
			Policy:       nil,
		},
		{
			Name:         "verifier",
			PromptPrefix: outputContractPrefix("verifier. Run tests/linters/vet; cite exact output."),
			AllowedTools: []string{"fs_read", "fs_glob", "fs_search", "shell_run", "time_now"},
			Policy:       &RolePolicy{ReadOnlyShell: true},
		},
		{
			Name:         "custom",
			PromptPrefix: outputContractPrefix("custom subagent. Follow the caller-supplied role instruction without exceeding parent policy."),
			AllowedTools: nil,
			Policy:       nil,
		},
	}
}

func outputContractPrefix(role string) string {
	return fmt.Sprintf(`You are a %s.

Reply with EXACTLY these five sections:
SUMMARY:
CHANGES:
EVIDENCE:
RISKS:
BLOCKERS:
`, role)
}

// LookupRole resolves a role name to its definition, ignoring surrounding
// whitespace and ASCII case. Role names reach us through model-authored tool
// arguments, where "Explore" is as likely a spelling as "explore"; without
// folding, the capitalized form would match no catalog entry and the caller
// would have to treat a perfectly reasonable request as unknown. The second
// return value is false for names outside the catalog so callers can reject
// them instead of silently falling back to an unrestricted role.
func LookupRole(name string) (RoleDef, bool) {
	name = strings.ToLower(strings.TrimSpace(name))
	for _, r := range AgentRoles() {
		if r.Name == name {
			return r, true
		}
	}
	return RoleDef{}, false
}

// AgentRoleNames returns the legal role names in catalog order. It exists so a
// rejection message can enumerate the valid choices: an error that only says
// "unknown role" leaves the caller (usually a model) with no way to correct
// itself, and it will just guess again.
func AgentRoleNames() []string {
	roles := AgentRoles()
	names := make([]string, 0, len(roles))
	for _, r := range roles {
		names = append(names, r.Name)
	}
	return names
}

// MustRole returns the role definition for name, panicking if not found (the
// set of known roles is fixed at compile time).
func MustRole(name string) RoleDef {
	if r, ok := LookupRole(name); ok {
		return r
	}
	panic(fmt.Sprintf("agent role %q not found", name))
}
