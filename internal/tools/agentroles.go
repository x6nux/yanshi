package tools

import "fmt"

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

// MustRole returns the role definition for name, panicking if not found (the
// set of known roles is fixed at compile time).
func MustRole(name string) RoleDef {
	for _, r := range AgentRoles() {
		if r.Name == name {
			return r
		}
	}
	panic(fmt.Sprintf("agent role %q not found", name))
}
