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
	"strings"
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

// String makes AccessTier satisfy fmt.Stringer so log lines, the diagnostics
// tool, and anything wrapping it in %s / %q render the operator-facing token
// rather than a bare integer. An out-of-range value renders as "unknown"
// rather than panicking — it can only arise from a corrupted config parse,
// and a diagnostic must not take the process down.
func (t AccessTier) String() string {
	switch t {
	case ReadOnly:
		return "read-only"
	case WorkspaceWrite:
		return "workspace-write"
	case FullAccess:
		return "full-access"
	}
	return "unknown"
}

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
	OSIsolated        EffectiveMode = "os-isolated"
	DegradedHostGuard EffectiveMode = "host-guard-degraded"
	Disabled          EffectiveMode = "disabled"
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

	// Unenforced names the Config fields the operator asked something of and
	// that this backend does NOT enforce, one entry per field (W-B-13).
	//
	// Enforced/Effective answer at BACKEND granularity, and that granularity is
	// too coarse to be actionable on the backends where it matters. The Landlock
	// path reports Enforced=true and OSIsolated because it really does confine
	// the filesystem, while `network_deny: true` means nothing at all there
	// unless the seccomp filter also loaded; the Windows restricted-token path
	// confines writes and installs no WFP filter at all, so the same setting is
	// permanently inert there. An operator reading "os-isolated" has no way to
	// learn that half of what they configured is doing nothing. Each constructor
	// therefore DECLARES the field set it enforces and UnenforcedFields
	// subtracts it from what the operator actually requested.
	//
	// Empty means "everything the operator asked for is enforced" — including
	// the case where they asked for nothing (see requestedFields: a
	// FullAccess/no-network-deny config requests nothing, so even a backend
	// that enforces nothing has nothing to warn about).
	Unenforced []string
}

// Config field names used by CapabilityReport.Unenforced and by each backend's
// enforcement declaration. They are the YAML spellings under
// security.sandbox, so an operator can grep a warning straight back to the line
// they wrote.
const (
	FieldTier          = "tier"
	FieldWorkspaceRoot = "workspace_root"
	FieldNetworkDeny   = "network_deny"
	FieldProxyURL      = "proxy_url"
)

// requestedFields lists the Config fields the operator asked something of, in
// a stable order.
//
// "Asked something of" is narrower than "set": a FullAccess tier is the absence
// of a write restriction, so a backend that cannot restrict writes is not
// failing to honour anything. Reporting it anyway would put a WARNING in front
// of every operator who left the sandbox at its widest setting, which is how a
// warning list stops being read.
//
// WorkspaceRoot rides on the tier for the same reason: it is the boundary a
// non-FullAccess tier is measured against and it constrains nothing on its own.
func requestedFields(cfg Config) []string {
	var out []string
	if cfg.Tier != FullAccess {
		out = append(out, FieldTier)
		if cfg.WorkspaceRoot != "" {
			out = append(out, FieldWorkspaceRoot)
		}
	}
	if cfg.NetworkDeny {
		out = append(out, FieldNetworkDeny)
	}
	if cfg.ProxyURL != "" {
		out = append(out, FieldProxyURL)
	}
	return out
}

// landlockEnforcedFields is the Landlock backend's enforcement declaration
// (W-B-13): the filesystem half always, the network half only when the seccomp
// filter actually loaded.
//
// It lives HERE rather than beside the backend it describes, and the build tag
// is the whole reason: sandbox_linux_landlock.go is linux-only, so a test for
// this function next to it could only ever run on one leg of the CI matrix —
// and the field it decides (network_deny) is the one field in this package
// whose enforcement is conditional. A declaration nobody can test on the
// developer's machine is how the conditional silently becomes unconditional.
//
// A free function taking the flag rather than a method on the backend, because
// it is called from newLandlock while the report is still being built.
func landlockEnforcedFields(seccomp bool) []string {
	fields := []string{FieldTier, FieldWorkspaceRoot}
	if seccomp {
		fields = append(fields, FieldNetworkDeny)
	}
	return fields
}

// UnenforcedFields returns the requested Config fields that enforced does not
// cover, preserving requestedFields' order.
//
// Backends pass the fields they genuinely enforce; passing none is the honest
// declaration for every degraded path. The subtraction direction matters: a
// backend that names a field it does not enforce silently removes a warning,
// so the declaration is deliberately a positive list that has to be typed out
// next to the code that does the enforcing.
func UnenforcedFields(cfg Config, enforced ...string) []string {
	if len(requestedFields(cfg)) == 0 {
		return nil
	}
	have := make(map[string]bool, len(enforced))
	for _, f := range enforced {
		have[f] = true
	}
	var out []string
	for _, f := range requestedFields(cfg) {
		if !have[f] {
			out = append(out, f)
		}
	}
	return out
}

// ParseTier maps an operator's tier string to an AccessTier, falling back to
// ReadOnly for anything unrecognised.
//
// Fail-safe is the whole point of the fallback: a typo in the config must not
// widen the sandbox. It lives here rather than inline in bootstrap because
// doctor has to report the posture the process will ACTUALLY run under, and
// two copies of this switch would drift the moment a tier is added -- doctor
// would then confidently print a tier the runtime does not use.
func ParseTier(s string) AccessTier {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "workspace-write":
		return WorkspaceWrite
	case "full-access":
		return FullAccess
	default:
		return ReadOnly
	}
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
