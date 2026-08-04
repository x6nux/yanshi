// Package guard enforces per-agent permission profiles at call time.
package guard

import (
	"errors"
	"fmt"
	"strings"

	"github.com/x6nux/yanshi/internal/execpolicy"
)

// PermissionProfile scopes what an agent is allowed to do.
type PermissionProfile struct {
	FS    FSPerm    `yaml:"fs"`
	Tools ToolsPerm `yaml:"tools"`
	// MCP is an additional fail-closed gate for dynamically discovered MCP tools.
	// A tool named mcp_<server>_<tool> must match BOTH MCP.Allow and Tools.Allow.
	// Empty MCP.Allow denies every MCP tool even when Tools.Allow contains "*".
	MCP      ToolsPerm    `yaml:"mcp"`
	Shell    ShellPerm    `yaml:"shell"`
	Net      NetPerm      `yaml:"net"`
	Subagent SubagentPerm `yaml:"subagent"`
}

// SubagentPerm restricts which model and reasoning effort spawned subagents
// may select. Empty fields mean "inherit parent without restriction" so
// existing configurations keep working.
type SubagentPerm struct {
	Models       []string `yaml:"models"`
	MaxReasoning string   `yaml:"max_reasoning_effort"`
}

var reasoningRank = map[string]int{
	"off": 0, "low": 1, "medium": 2, "high": 3,
}

// AllowsAnyModel returns true when the profile does not restrict subagent
// models (empty list means all models allowed).
func (p SubagentPerm) AllowsAnyModel() bool { return len(p.Models) == 0 }

// CheckModel returns nil when the named model is allowed by the profile, or an
// error if it is denied.
func (p SubagentPerm) CheckModel(name string) error {
	if p.AllowsAnyModel() {
		return nil
	}
	for _, allowed := range p.Models {
		if strings.EqualFold(allowed, name) {
			return nil
		}
	}
	return fmt.Errorf("subagent model %q is not allowed by profile", name)
}

// ReasoningCap returns the maximum reasoning effort the profile allows.
func (p SubagentPerm) ReasoningCap() string {
	if p.MaxReasoning == "" {
		return "high"
	}
	return strings.ToLower(p.MaxReasoning)
}

// CheckReasoning returns nil when the given reasoning effort is within the
// profile's maximum, or an error if it exceeds the cap.
func (p SubagentPerm) CheckReasoning(effort string) error {
	want, ok := reasoningRank[strings.ToLower(effort)]
	if !ok {
		return fmt.Errorf("invalid reasoning effort %q", effort)
	}
	capName := p.ReasoningCap()
	cap, ok := reasoningRank[capName]
	if !ok {
		return fmt.Errorf("invalid profile reasoning cap %q", capName)
	}
	if want > cap {
		return fmt.Errorf("reasoning effort %q exceeds profile cap %q", effort, capName)
	}
	return nil
}

// ErrOverrideDenied is returned when a subagent model/reasoning override is
// rejected by the profile.
var ErrOverrideDenied = errors.New("subagent override denied")

// FSPerm governs filesystem access via glob patterns (supporting **).
type FSPerm struct {
	Read  []string `yaml:"read"`
	Write []string `yaml:"write"`
}

// ToolsPerm lists allowed tool names (glob patterns, e.g. "fs.*", "*").
type ToolsPerm struct {
	Allow []string `yaml:"allow"`
}

// ShellPerm governs shell command execution. Two enforcement layers coexist:
//   - Policy/Patterns: the legacy allowlist/denylist/deny glob switch.
//     Empty Rules + a non-empty Policy exercises ONLY this layer.
//   - Rules: the structured execpolicy rule table. When non-empty, the guard
//     evaluates it AFTER the structural metacharacter HardDeny and BEFORE the
//     legacy switch. execpolicy outcomes (allow/prompt/hard_deny) short-circuit
//     the verdict so a Rules-only profile does not silently fall through to
//     an empty allowlist that would turn allowed commands into Prompts.
type ShellPerm struct {
	Policy   string            `yaml:"policy"`
	Patterns []string          `yaml:"patterns"`
	Rules    []execpolicy.Rule `yaml:"rules"`
}

// ShellPolicies returns the policy strings the guard understands, in the order
// checkShell's switch tests them. "" is legal and is an alias for "allowlist";
// there is deliberately NO "allow" value — an unrestricted shell is expressed
// as "denylist" with no patterns.
//
// This slice is the single source of truth for policy validation, consumed by
// ValidateShellPolicy so a typo is rejected when the config loads. It has to
// agree with checkShell's switch, and TestShellPolicyCatalogMatchesCheckShell
// holds it there in both directions: every entry here must survive Check
// without hitting the unknown-policy branch, and a value outside this list must
// hit it.
func ShellPolicies() []string { return []string{"", "allowlist", "deny", "denylist"} }

// ValidateShellPolicy reports whether policy is one the guard can enforce.
//
// This exists because an unrecognized policy is the single worst thing an
// operator can write into a profile: checkShell's default branch returns a
// STRUCTURAL HardDeny (not overridable), so the profile's shell dimension is
// dead in every permission mode — yolo and auto included — with no interactive
// escape hatch. Nothing reads the value until the first shell_run, so without
// this check a typo starts cleanly and only surfaces mid-session as an
// unexplained refusal. Validating at load turns that into a startup error that
// names the profile and the legal values.
func ValidateShellPolicy(policy string) error {
	for _, p := range ShellPolicies() {
		if policy == p {
			return nil
		}
	}
	return fmt.Errorf("unknown shell policy %q (want one of %q, where \"\" means \"allowlist\")",
		policy, ShellPolicies())
}

// NetPerm governs outbound network access.
type NetPerm struct {
	Allow bool     `yaml:"allow"`
	Hosts []string `yaml:"hosts"` // glob host patterns, e.g. "*.github.com"
}
