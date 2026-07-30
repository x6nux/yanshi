// Package sandbox is the single abstraction layer yanshi uses to describe
// process-level isolation for spawned commands. Phase 0 (this commit) ships
// the interface and per-platform skeletons ONLY — no OS-level enforcement is
// in effect. Each adapter reports Effective=DegradedHostGuard honestly so the
// rest of the system can label itself correctly rather than over-claim.
//
// The interface is intentionally small (Prepare / Report / Close) so a real
// enforcement backend (Job Object + restricted token on Windows; Landlock /
// seccomp / bubblewrap on Linux; Seatbelt on macOS) can drop in later without
// touching call sites. Until then the guard layer remains the sole choke
// point that prevents unauthorized actions.
package sandbox

import (
	"context"
	"os/exec"
)

// AccessTier names the resource-access class a sandboxed command is run under.
// ReadOnly = no writes anywhere; WorkspaceWrite = writes only under
// WorkspaceRoot; FullAccess = no write restrictions (network still subject to
// the network policy). The ordering (ReadOnly < WorkspaceWrite < FullAccess)
// is load-bearing — callers compare tiers by value to decide whether an action
// is permitted at the current tier.
type AccessTier uint8

// Access tiers ordered from least to most permissive.
const (
	ReadOnly AccessTier = iota
	WorkspaceWrite
	FullAccess
)

// EffectiveMode is the truth about what the sandbox is actually enforcing.
// OSIsolated = real OS-level mechanisms in effect; DegradedHostGuard = no OS
// isolation, only the host guard layer (Phase 0 honesty); Disabled = the
// operator turned the sandbox off. Callers MUST consult this rather than
// Config.Enabled so a future "auto-degrade when backend missing" path fails
// closed (the report would say Degraded even though the operator asked for
// OS-isolated).
type EffectiveMode string

// Effective modes a sandbox reports, ordered from full OS isolation to disabled.
const (
	OSIsolated         EffectiveMode = "os-isolated"
	DegradedHostGuard EffectiveMode = "host-guard-degraded"
	Disabled           EffectiveMode = "disabled"
)

// CapabilityReport is the answer to "what is this sandbox actually doing for
// me right now?". It is the value callers show in the TUI / WS status and
// gate decisions on (e.g. SecureProcessFactory refuses to advertise
// KillTree when CanKillTree=false). Phase 0 always reports Enforced=false
// and CanKillTree=false because there is no OS backing.
type CapabilityReport struct {
	Platform    string
	Requested   AccessTier
	Effective   EffectiveMode
	Backend     string
	Reason      string
	Enforced    bool
	CanKillTree bool
}

// Config is the operator-facing sandbox configuration. *bool is used for
// Enabled so a missing YAML key can be distinguished from `enabled: false`
// (the former applies the default; the latter is an explicit opt-out).
type Config struct {
	Enabled       bool
	WorkspaceRoot string
	Tier          AccessTier
	NetworkDeny   bool
	ProxyURL      string
}

// CommandSpec describes one process the sandbox is asked to prepare. Path/Args
// are the program and its arguments; Dir is the working directory; Tier is the
// access class requested for this specific invocation (may differ from
// Config.Tier when a more privileged helper is allowed).
type CommandSpec struct {
	Path string
	Args []string
	Dir  string
	Tier AccessTier
}

// Sandbox prepares an exec.Cmd to run under isolation and reports its
// capabilities. Phase 0's Prepare is a no-op (it leaves the host guard path
// in place); future Phase 1+ implementations set Job Object attributes
// (Windows), Landlock/seccomp filters (Linux), or Seatbelt profiles (macOS).
type Sandbox interface {
	Prepare(context.Context, *exec.Cmd, CommandSpec) error
	Report() CapabilityReport
	Close() error
}
