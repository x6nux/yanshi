package guard

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/x6nux/yanshi/internal/execpolicy"
)

// Action describes an attempted operation to be authorized.
type Action struct {
	Tool    string // tool name, e.g. "fs_read", "shell_run", "web_fetch"
	FS      FSWant // set for filesystem operations
	Shell   string // shell command, for shell operations
	NetHost string // target host, for network operations
	// Workdir is the project/work root used as the in-scope boundary for the
	// destructive-deletion dimension (checkDestructive). The shell tool
	// populates it; other tools leave it empty (the dimension no-ops on Shell=="").
	Workdir string
}

// FSWant describes a filesystem access intent.
type FSWant struct {
	Op    string   // "read" or "write"
	Paths []string // paths involved
}

// Verdict is the typed outcome of a guard check. Allow = explicit pass; Prompt
// = static profile did not allow but the action is safe enough to escalate to
// an interactive callback; HardDeny = fail-closed (no profile, empty
// Tools.Allow, shell metachar, unknown policy, deny rule, deny flag, parser
// failure) — the callback layer MUST NOT override a HardDeny on its own.
//
// HardDeny splits further by the Decision.Overridable flag:
//   - Overridable=false (structural): catastrophic mass deletion, shell
//     metachar, execpolicy parse-error, unknown shell policy, unknown
//     execpolicy verdict. Never overridable — not by the callback, not by
//     YOLO/auto. This is the immovable floor, and it is exactly the set
//     produced by hardDeny() plus the two inline HardDenies in checkShell.
//     Catastrophic deletion belongs here and is the one an operator is most
//     likely to reason about: `rm -rf /` under yolo is refused by THIS flag,
//     not by any profile.
//   - Overridable=true (profile-policy choice): empty Tools.Allow, empty FS
//     read/write list, shell policy="deny", denylist match, execpolicy hard_deny
//     rules, net.allow=false, empty MCP allowlist. These are all configuration
//     choices, so the YOLO and Auto interactive modes may set them aside via the
//     callback (tools.Authorize routes them to mode resolution). default/
//     allow-edits/plan modes and the SSE path still treat them as fail-closed.
type Verdict uint8

// Guard verdict values.
const (
	Allow Verdict = iota
	Prompt
	HardDeny
)

// Decision is the guard's verdict for an Action. Reason remains the human-
// readable explanation (kept for backwards compatibility with the old
// {Allowed bool; Reason string} shape via the IsAllowed() shim).
//
// Promptable is the single source of truth for "may the approval callback
// override this?" for a Prompt verdict — orchestrator/tools/transport consult
// Promptable rather than re-deriving it from Verdict, so the HardDeny firewall
// stays in one place. RuleID/Justification carry the execpolicy explanation
// when set.
//
// Overridable only applies to HardDeny (see the Verdict doc): it marks a
// profile-policy default deny that YOLO/Auto interactive modes may override.
// It is ignored for Allow/Prompt. Structural HardDenies set it false so they
// remain an immovable floor even under YOLO.
type Decision struct {
	Verdict       Verdict
	Reason        string
	RuleID        string
	Justification string
	Promptable    bool
	Overridable   bool
}

// IsAllowed is a binary convenience for call sites that do not need to
// distinguish Prompt from HardDeny. Security-sensitive code (Authorize) MUST
// switch on Verdict/Promptable directly.
func (d Decision) IsAllowed() bool { return d.Verdict == Allow }

// allow/prompt/hardDeny are the only constructors for Decision — they keep the
// Promptable/Verdict pairing consistent so the HardDeny firewall cannot be
// bypassed by accidentally flipping one but not the other at a call site.
func allow() Decision { return Decision{Verdict: Allow} }

func prompt(reason string) Decision {
	return Decision{Verdict: Prompt, Reason: reason, Promptable: true}
}

func hardDeny(reason string) Decision {
	return Decision{Verdict: HardDeny, Reason: reason, Promptable: false}
}

// overridableDeny is a HardDeny that arises from a profile POLICY default
// (empty allowlist, shell policy="deny", net.allow=false) rather than a
// structural guard. It stays fail-closed on the SSE path and under
// default/allow-edits/plan modes, but YOLO/Auto interactive modes may override
// it via the permission callback (tools.Authorize routes Overridable HardDenies
// to mode resolution).
//
// The structural floor — the denials YOLO cannot buy its way past — is exactly
// the set that reaches hardDeny() or an inline Decision without Overridable:
// catastrophic mass deletion, shell metacharacters, execpolicy parse-error, and
// unknown shell policy / unknown execpolicy verdict. Everything a profile can
// merely have an opinion about is overridable, and that includes two denials
// this comment used to misfile as structural: a denylist pattern match and an
// empty MCP allowlist. Both are profile policy, so both yield to YOLO/Auto.
// Enumerate the floor with `grep -n 'hardDeny(' internal/guard/guard.go` plus
// the two inline HardDenies in checkShell rather than trusting this list.
func overridableDeny(reason string) Decision {
	return Decision{Verdict: HardDeny, Reason: reason, Promptable: false, Overridable: true}
}

// Guard checks Actions against a PermissionProfile.
type Guard struct{}

// New returns a Guard.
func New() *Guard { return &Guard{} }

// Check returns whether the profile permits the action, checking every
// applicable dimension. The first non-Allow dimension short-circuits. The
// returned Decision carries a typed Verdict and Promptable flag so callers
// (Authorize/transport) can enforce a HardDeny firewall without re-deriving it.
//
// When every dimension passes, Check returns the Allow Decision from the LAST
// check that produced one (rather than a generic empty Allow). This preserves
// any RuleID/Justification a dimension set (e.g. the execpolicy layer in
// checkShell) so the explainability signal flows to the caller verbatim.
func (g *Guard) Check(p PermissionProfile, a Action) Decision {
	dec := allow()
	witness := dec // carries RuleID/Justification from the most-recent Allow
	if d := g.checkDestructive(a); d.Verdict != Allow {
		return d
	} else {
		witness = d
	}
	if d := g.checkMCPTools(p, a); d.Verdict != Allow {
		return d
	} else {
		witness = d
	}
	if d := g.checkTools(p, a); d.Verdict != Allow {
		return d
	} else {
		witness = d
	}
	if d := g.checkFS(p, a); d.Verdict != Allow {
		return d
	} else {
		witness = d
	}
	if d := g.checkShell(p, a); d.Verdict != Allow {
		return d
	} else {
		// Prefer the shell witness when it carries a RuleID (execpolicy path),
		// so the eventual Decision reflects which rule admitted the call.
		if d.RuleID != "" {
			witness = d
		}
	}
	if d := g.checkNet(p, a); d.Verdict != Allow {
		return d
	} else {
		if d.RuleID != "" && witness.RuleID == "" {
			witness = d
		}
	}
	_ = dec
	return witness
}

// checkDestructive is a profile-independent structural safety dimension wired
// FIRST into Check. It ensures catastrophic mass-deletion is blocked (structural
// HardDeny, every mode) and out-of-workdir deletion is escalated (Prompt) EVEN
// WHEN the configured profile would otherwise allow the command — otherwise a
// permissive profile + yolo/auto could run "rm -rf /". Non-shell and
// non-destructive actions return Allow and the profile dimensions decide as
// before. Catastrophic is the floor operators can always rely on; out-of-workdir
// is Promptable so yolo can block it (resolvePermissionMode), auto AI-judges it,
// and default/allow-edits surface it as an interactive prompt.
func (g *Guard) checkDestructive(a Action) Decision {
	if a.Shell == "" {
		return allow()
	}
	switch ClassifyDestruction(a.Shell, a.Workdir) {
	case DestructionCatastrophic:
		return hardDeny("catastrophic mass deletion blocked (rm -rf on a root/home/workdir)")
	case DestructionOutOfScope:
		return prompt("deletion outside the working directory")
	default:
		return allow()
	}
}

// checkMCPTools applies only to runtime names with the reserved mcp_ prefix.
// It is deliberately separate from checkTools so a broad Tools.Allow pattern
// (notably the historical "*") cannot silently authorize newly configured MCP
// servers. The profile must opt in to the exact server/tool name or a matching
// MCP-specific glob. The empty-allowlist deny is OVERRIDABLE: yolo/auto treat
// MCP opt-in like any other profile policy (yolo bypasses, auto AI-judges),
// while default/allow-edits/SSE stay fail-closed. A non-matching mcp_ name under
// a non-empty allowlist is PROMPTABLE (interactive user may approve).
func (g *Guard) checkMCPTools(p PermissionProfile, a Action) Decision {
	if !strings.HasPrefix(a.Tool, "mcp_") {
		return Decision{Verdict: Allow}
	}
	if len(p.MCP.Allow) == 0 {
		return overridableDeny("no MCP tools permitted by profile")
	}
	name := filepath.ToSlash(a.Tool)
	for _, pat := range p.MCP.Allow {
		if ok, err := MatchGlob(filepath.ToSlash(pat), name); err == nil && ok {
			return allow()
		}
	}
	return prompt(fmt.Sprintf("MCP tool %q not permitted", a.Tool))
}

func (g *Guard) checkTools(p PermissionProfile, a Action) Decision {
	if len(p.Tools.Allow) == 0 {
		return overridableDeny("no tools permitted by profile")
	}
	for _, pat := range p.Tools.Allow {
		if ok, err := MatchGlob(filepath.ToSlash(pat), filepath.ToSlash(a.Tool)); err == nil && ok {
			return allow()
		}
	}
	// Tool-not-on-allowlist is PROMPTABLE: an interactive user may approve a
	// new tool. Only the structural "no tools allowed at all" case above is
	// HardDeny.
	return prompt(fmt.Sprintf("tool %q not permitted", a.Tool))
}

func (g *Guard) checkFS(p PermissionProfile, a Action) Decision {
	if len(a.FS.Paths) == 0 {
		return allow()
	}
	var allowed []string
	if a.FS.Op == "read" {
		allowed = p.FS.Read
	} else {
		allowed = p.FS.Write
	}
	if len(allowed) == 0 {
		return overridableDeny(fmt.Sprintf("no paths permitted for op %q", a.FS.Op))
	}
	for _, raw := range a.FS.Paths {
		path := filepath.ToSlash(filepath.Clean(raw))
		ok := false
		for _, pat := range allowed {
			if m, err := MatchGlob(filepath.ToSlash(pat), path); err == nil && m {
				ok = true
				break
			}
		}
		if !ok {
			return prompt(fmt.Sprintf("path %q not permitted for op %q", raw, a.FS.Op))
		}
	}
	return allow()
}

// checkShell checks the shell command against the profile's shell policy.
// Before any allowlist/denylist glob matching, it rejects commands containing
// shell control metacharacters (&&, ||, ;, |, backticks, $(), newlines, >, <).
// This applies to ALL policies — denylist too — because a single glob pattern
// can never safely cover chained commands; a trailing "*" would match the
// entire chain and auto-approve hidden malicious commands.
//
// The metacharacter rejection is a STRUCTURAL HardDeny: no glob can ever
// safely cover a chained command, so the interactive callback MUST NOT
// override it. This is the second layer of defense on top of execpolicy
// parsing (Task 4) — both stay. The metachar HardDeny is one of FIVE
// structural denials that remain even under YOLO/Auto; the authoritative
// enumeration lives on overridableDeny, and this comment deliberately does
// not restate it. It used to, and it listed three — omitting catastrophic
// mass deletion and the unknown-execpolicy-verdict default — which is how a
// closed "only:" enumeration of the YOLO floor ended up copied into CLAUDE.md
// two commits after the authoritative one was corrected.
// Everything the profile can say "no" with is an OVERRIDABLE HardDeny —
// policy="deny", a denylist match, execpolicy hard_deny rules, net.allow=false,
// and the empty-MCP-allowlist gate — so YOLO bypasses it and Auto AI-judges it.
// A non-allowlisted command under "allowlist" is PROMPTABLE — an interactive
// user may approve it.
func (g *Guard) checkShell(p PermissionProfile, a Action) Decision {
	if a.Shell == "" {
		return allow()
	}
	for _, m := range []string{"&&", "&", "||", ";", "|", "`", "$(", "\n", "\r", ">", "<"} {
		if strings.Contains(a.Shell, m) {
			return hardDeny("shell metacharacter rejected: " + m)
		}
	}
	// execpolicy layer (Task 6): when the profile carries structured Rules,
	// evaluate them BEFORE the legacy glob switch. The structural metachar
	// HardDeny above already fired, so a command that reaches this point is a
	// single, non-chained shell word — exactly what execpolicy is designed to
	// reason about. Parse failure is HardDeny("parse-error") and is NOT
	// promptable: a model must not be able to expand the policy's accepted
	// syntax by feeding malformed input. An execpolicy "allow" short-circuits
	// to Allow (with RuleID/Justification) so a Rules-only profile does not
	// fall through to an empty legacy allowlist and turn allowed commands
	// into Prompts.
	if len(p.Shell.Rules) > 0 {
		cmd, err := execpolicy.Parse(a.Shell)
		if err != nil {
			return Decision{Verdict: HardDeny, RuleID: "parse-error", Reason: err.Error(), Justification: "execpolicy parser rejected unsupported shell syntax", Promptable: false}
		}
		result := execpolicy.Evaluate(cmd, p.Shell.Rules)
		switch result.Verdict {
		case "allow":
			return Decision{Verdict: Allow, RuleID: result.RuleID, Reason: result.Reason, Justification: result.Justification, Promptable: false}
		case "prompt":
			return Decision{Verdict: Prompt, RuleID: result.RuleID, Reason: result.Reason, Justification: result.Justification, Promptable: true}
		case "hard_deny", "deny":
			return Decision{Verdict: HardDeny, RuleID: result.RuleID, Reason: result.Reason, Justification: result.Justification, Promptable: false, Overridable: true}
		default:
			return Decision{Verdict: HardDeny, RuleID: result.RuleID, Reason: "unknown execpolicy verdict", Justification: result.Justification, Promptable: false}
		}
	}
	switch p.Shell.Policy {
	case "deny":
		return overridableDeny("shell denied by policy")
	case "", "allowlist":
		for _, pat := range p.Shell.Patterns {
			if ok, err := MatchGlob(pat, a.Shell); err == nil && ok {
				return allow()
			}
		}
		return prompt(fmt.Sprintf("shell command %q not on allowlist", a.Shell))
	case "denylist":
		for _, pat := range p.Shell.Patterns {
			if ok, err := MatchGlob(pat, a.Shell); err == nil && ok {
				return overridableDeny(fmt.Sprintf("shell command %q denied by denylist", a.Shell))
			}
		}
		return allow()
	}
	return hardDeny(fmt.Sprintf("unknown shell policy %q", p.Shell.Policy))
}

func (g *Guard) checkNet(p PermissionProfile, a Action) Decision {
	if a.NetHost == "" {
		return allow()
	}
	if !p.Net.Allow {
		return overridableDeny("network access denied")
	}
	if len(p.Net.Hosts) == 0 {
		return allow() // allow=true with no host restriction
	}
	host := strings.ToLower(a.NetHost)
	for _, pat := range p.Net.Hosts {
		if ok, err := MatchGlob(strings.ToLower(pat), host); err == nil && ok {
			return allow()
		}
	}
	return prompt(fmt.Sprintf("host %q not permitted", a.NetHost))
}
