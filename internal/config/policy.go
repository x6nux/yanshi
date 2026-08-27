// internal/config/policy.go
//
// S3: keeping an agent from widening its own permission profile.
//
// # What was already true, and what was not
//
// The first thing to establish was whether guard re-reads the profile. It does
// not: config.Load runs exactly once in bootstrap.Build, the resolved
// PermissionProfile is copied by value into Orchestrator.profile, and every
// Check consults that copy. So the in-memory snapshot half of this problem was
// already solved, and building a second snapshot layer would have been building
// a thing that already existed.
//
// The half that was NOT solved is the one the snapshot cannot reach: the file
// is inside the agent's own write scope. `-config` defaults to the relative
// path "config.yaml", the shipped example profile carries fs.write "**", and
// bootstrap re-reads that file on the next boot. One fs_write plus one restart
// is a complete self-escalation, and no amount of in-memory immutability
// touches it, because the escalation lands in a process the snapshot has no
// opinion about.
//
// # The fix: authority lives where the agent cannot write
//
// A trusted policy file OUTSIDE the working directory ($YANSHI_POLICY, else
// ~/.yanshi/policy.yaml) becomes the authority for `profiles:`. When one
// exists, the working-directory config can only NARROW it — every widening in
// the local file is discarded at load, so writing to config.yaml stops being a
// way to gain anything.
//
// Why narrowing rather than ignoring the local file outright: a project that
// wants a TIGHTER profile than the user's default is a legitimate and common
// case, and refusing it would push people to keep their loose default. The
// safety of allowing it is structural rather than careful — an agent that edits
// the local file can only ever move the effective profile toward the trusted
// baseline, never past it, so the worst it can do to itself is undo its own
// project's restrictions.
//
// # Every narrowing rule is conservative in the same direction
//
// The algebra below is not a general set intersection, because globs do not
// intersect cleanly: `fs_*` and `*_read` have a non-empty overlap that neither
// pattern can express. Wherever the result is not exactly representable, the
// rules fall back to the TRUSTED value. That is always sound — the trusted
// value is the baseline the operator already accepted — and it is always the
// less surprising failure, because "my narrowing did not take effect" is
// visible while "my narrowing widened something" is not.
//
// # Backward compatibility
//
// With no trusted policy file present, nothing changes: the local profiles are
// used verbatim, exactly as before. That is deliberate — this must not break
// existing single-file deployments — and it is why `yanshi doctor` reports the
// unprotected posture rather than this code refusing to run.

package config

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/x6nux/yanshi/internal/guard"
	"gopkg.in/yaml.v3"
)

// PolicyEnvVar names the environment variable that points at the trusted
// policy file. It takes precedence over the default location.
//
// The path is read from the ENVIRONMENT and never from the config file. A
// `policy_path:` key in config.yaml would have been the obvious ergonomic
// choice and is exactly the hole this file exists to close: the agent can write
// config.yaml, so it could point the authority at a file it also controls.
const PolicyEnvVar = "YANSHI_POLICY"

// DefaultPolicyFileName is the trusted policy file's name under the user's
// yanshi home directory (~/.yanshi/policy.yaml).
const DefaultPolicyFileName = "policy.yaml"

// Policy is the trusted policy document. It intentionally carries ONLY the
// permission profiles.
//
// Everything else in Config — providers, storage paths, ports — is either
// harmless for an agent to change or is already protected by other means, and
// a policy file that could set them would be a second, partial copy of the
// config schema that drifts from the first. Authorization is the one thing
// whose authority has to live outside the writable tree, so it is the one thing
// this document holds.
type Policy struct {
	Profiles map[string]guard.PermissionProfile `yaml:"profiles"`
}

// PolicyPath returns the path of the trusted policy file that would be
// consulted, and whether one was configured at all.
//
// Resolution order is $YANSHI_POLICY, then ~/.yanshi/policy.yaml. The returned
// path may not exist; existence is LoadPolicy's question, because "configured
// but missing" and "not configured" need different handling and collapsing them
// here would lose that.
func PolicyPath() (string, bool) {
	if p := strings.TrimSpace(os.Getenv(PolicyEnvVar)); p != "" {
		return expandHome(p), true
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "", false
	}
	return filepath.Join(home, ".yanshi", DefaultPolicyFileName), true
}

// LoadPolicy reads the trusted policy file, returning (nil, nil) when no policy
// file exists.
//
// An absent file is not an error: the overwhelming majority of deployments have
// none, and this must not become a reason yanshi refuses to start. A file that
// EXISTS but does not parse IS an error, and deliberately so — the operator
// wrote it in order to constrain the agent, and silently falling back to the
// unconstrained local profiles would defeat the entire point at the exact
// moment the operator was trying to use it.
//
// $YANSHI_POLICY pointing at a missing file is likewise an error. Somebody set
// that variable on purpose; a typo in it must not silently disable the gate.
func LoadPolicy() (*Policy, error) {
	path, configured := PolicyPath()
	if !configured {
		return nil, nil
	}
	explicit := strings.TrimSpace(os.Getenv(PolicyEnvVar)) != ""
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) && !explicit {
			return nil, nil
		}
		return nil, fmt.Errorf("config: read policy %q: %w", path, err)
	}
	var p Policy
	if err := yaml.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("config: parse policy %q: %w", path, err)
	}
	for name, prof := range p.Profiles {
		if len(prof.Shell.Rules) > 0 {
			continue
		}
		if err := guard.ValidateShellPolicy(prof.Shell.Policy); err != nil {
			return nil, fmt.Errorf("config: policy %q: profiles.%s.shell.policy: %w", path, name, err)
		}
	}
	return &p, nil
}

// ApplyPolicy replaces c.Profiles with the trusted profiles narrowed by the
// local ones, and reports which profile names were altered.
//
// It is a no-op when policy is nil or declares no profiles, which is what keeps
// every existing single-file deployment working byte-identically.
//
// The returned names are for the operator's benefit — a local profile that was
// silently clamped is exactly the kind of thing somebody should be told about
// once, at boot, rather than discover from a denial three hours later.
func (c *Config) ApplyPolicy(policy *Policy) []string {
	if policy == nil || len(policy.Profiles) == 0 {
		return nil
	}
	effective := make(map[string]guard.PermissionProfile, len(policy.Profiles))
	var narrowed []string
	for name, trusted := range policy.Profiles {
		local, hasLocal := c.Profiles[name]
		if !hasLocal {
			effective[name] = trusted
			continue
		}
		result := NarrowProfile(trusted, local)
		effective[name] = result
		narrowed = append(narrowed, name)
	}
	// A profile the trusted policy does not name is DROPPED, not carried over.
	//
	// Carrying it would be the whole hole reopened one level up: an agent that
	// cannot widen "orchestrator" would instead add a wide profile under a new
	// name. The trusted file decides which profiles exist, not just what they
	// may contain.
	c.Profiles = effective
	sort.Strings(narrowed)
	return narrowed
}

// NarrowProfile returns the effective profile: trusted, restricted by every
// restriction local expresses, and unaffected by every widening it attempts.
//
// The result is never wider than trusted in any dimension. See the file header
// for why unrepresentable intersections fall back to the trusted value.
func NarrowProfile(trusted, local guard.PermissionProfile) guard.PermissionProfile {
	out := trusted
	out.Tools = guard.ToolsPerm{Allow: narrowAllow(trusted.Tools.Allow, local.Tools.Allow)}
	out.MCP = guard.ToolsPerm{Allow: narrowAllow(trusted.MCP.Allow, local.MCP.Allow)}
	out.FS = guard.FSPerm{
		Read:  narrowAllow(trusted.FS.Read, local.FS.Read),
		Write: narrowAllow(trusted.FS.Write, local.FS.Write),
	}
	out.Shell = narrowShell(trusted.Shell, local.Shell)
	out.Net = narrowNet(trusted.Net, local.Net)
	out.Subagent = narrowSubagent(trusted.Subagent, local.Subagent)
	return out
}

// narrowAllow intersects two ALLOW lists, where a shorter list is stricter.
//
// The empty-list cases are asymmetric on purpose, because an empty allow list
// means "nothing is permitted" in this codebase (see guard.checkFS and
// checkTools, both of which deny on len(allowed) == 0):
//
//   - trusted empty → nothing is permitted, whatever local says. Returns nil.
//   - local empty → local permits nothing either. Returns nil.
//
// Both directions are therefore deny, and neither is a case where the local
// file gets to add an entry the trusted file never had.
func narrowAllow(trusted, local []string) []string {
	if len(trusted) == 0 || len(local) == 0 {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	for _, l := range local {
		if seen[l] {
			continue
		}
		if coveredByAny(trusted, l) {
			seen[l] = true
			out = append(out, l)
		}
	}
	return out
}

// coveredByAny reports whether some trusted pattern provably grants everything
// the local pattern would grant.
//
// "Provably" is the operative word and is why this is not simply a glob match.
// Three cases are accepted:
//
//   - A universal trusted pattern ("*" / "**") grants everything.
//   - An exact string match: the local pattern IS a trusted pattern.
//   - A local LITERAL (no glob metacharacter) that a trusted pattern matches.
//
// A local pattern that itself contains wildcards and is not literally present
// in the trusted list is REJECTED even when it looks narrower, because glob
// containment is not decidable by matching: `fs_r*` is not matched by `fs_read`
// yet grants strictly more than it. Rejecting is the conservative direction —
// the entry is dropped, so the effective profile loses a permission rather than
// gaining one.
func coveredByAny(trusted []string, local string) bool {
	for _, t := range trusted {
		if t == "*" || t == "**" || t == local {
			return true
		}
	}
	if strings.ContainsAny(local, "*?[") {
		return false
	}
	for _, t := range trusted {
		if ok, err := guard.MatchGlob(t, local); err == nil && ok {
			return true
		}
	}
	return false
}

// narrowShell restricts the shell dimension.
//
// Two situations hand the whole dimension back to trusted, both because the
// result would otherwise not be representable as one ShellPerm:
//
//   - The trusted profile carries execpolicy Rules. Rules and the legacy
//     policy/patterns switch are alternative enforcement layers (guard's
//     checkShell returns inside the Rules branch), so merging a local
//     policy string into a rules-based profile would produce a field guard
//     never reads while looking like it did something.
//   - The two policies differ. "allowlist" narrowed by "denylist" has no
//     single-policy spelling, and picking either one would be guessing at
//     which the operator meant.
//
// When the policies agree the merge is exact: allowlists intersect, denylists
// union, and "deny" is already the floor.
func narrowShell(trusted, local guard.ShellPerm) guard.ShellPerm {
	if len(trusted.Rules) > 0 {
		return trusted
	}
	if normalizeShellPolicy(trusted.Policy) != normalizeShellPolicy(local.Policy) {
		return trusted
	}
	out := trusted
	switch normalizeShellPolicy(trusted.Policy) {
	case "deny":
		return trusted
	case "denylist":
		// More denials is narrower, so the local file's denials are ADDED.
		// This is the one dimension where the local file may contribute an
		// entry the trusted file lacks, and it is safe for the same reason it
		// is the exception: the entry can only take a capability away.
		out.Patterns = unionPatterns(trusted.Patterns, local.Patterns)
	default: // "" and "allowlist"
		out.Patterns = narrowAllow(trusted.Patterns, local.Patterns)
	}
	return out
}

// normalizeShellPolicy folds the empty policy string onto its documented alias.
// guard.ShellPolicies records that "" IS "allowlist"; comparing the raw strings
// would treat a profile that omits the key as different from one that spells
// the default out, and hand the whole dimension back to trusted for no reason.
func normalizeShellPolicy(p string) string {
	if p == "" {
		return "allowlist"
	}
	return p
}

// unionPatterns concatenates two pattern lists, preserving order and dropping
// duplicates.
func unionPatterns(a, b []string) []string {
	out := make([]string, 0, len(a)+len(b))
	seen := map[string]bool{}
	for _, list := range [][]string{a, b} {
		for _, p := range list {
			if seen[p] {
				continue
			}
			seen[p] = true
			out = append(out, p)
		}
	}
	return out
}

// narrowNet restricts the network dimension. Allow is an AND: either side
// saying no is no. Hosts narrow like an allow list, except that an EMPTY host
// list means "no host restriction" here rather than "nothing permitted" (see
// guard.checkNet), so an empty side contributes no restriction instead of
// denying everything.
func narrowNet(trusted, local guard.NetPerm) guard.NetPerm {
	out := guard.NetPerm{Allow: trusted.Allow && local.Allow}
	switch {
	case len(trusted.Hosts) == 0:
		out.Hosts = append([]string(nil), local.Hosts...)
	case len(local.Hosts) == 0:
		out.Hosts = append([]string(nil), trusted.Hosts...)
	default:
		out.Hosts = narrowAllow(trusted.Hosts, local.Hosts)
		if len(out.Hosts) == 0 {
			// An empty result would mean "no restriction" and thus WIDEN the
			// profile — the one outcome narrowing may never produce. Two
			// disjoint host lists mean nothing is jointly permitted, and the
			// only way to say that with this struct is Allow=false.
			out.Allow = false
			out.Hosts = append([]string(nil), trusted.Hosts...)
		}
	}
	return out
}

// narrowSubagent restricts the subagent dimension. An empty model list means
// "no restriction" (SubagentPerm.AllowsAnyModel), so an empty side contributes
// nothing rather than denying everything.
//
// The reasoning cap is compared through guard's OWN CheckReasoning rather than
// a second rank table here: a duplicated ordering is a thing that can disagree
// with the one guard enforces, and the disagreement would be silent.
func narrowSubagent(trusted, local guard.SubagentPerm) guard.SubagentPerm {
	out := guard.SubagentPerm{MaxReasoning: trusted.MaxReasoning}
	switch {
	case len(trusted.Models) == 0:
		out.Models = append([]string(nil), local.Models...)
	case len(local.Models) == 0:
		out.Models = append([]string(nil), trusted.Models...)
	default:
		for _, l := range local.Models {
			if trusted.CheckModel(l) == nil {
				out.Models = append(out.Models, l)
			}
		}
		if len(out.Models) == 0 {
			// Disjoint lists: no model is jointly allowed. An empty list would
			// mean "all models", so fall back to the trusted list — narrowing
			// failed to apply, which is the visible failure, rather than
			// widening, which is not.
			out.Models = append([]string(nil), trusted.Models...)
		}
	}
	if local.MaxReasoning != "" && trusted.CheckReasoning(local.ReasoningCap()) == nil {
		out.MaxReasoning = local.MaxReasoning
	}
	return out
}

// PolicyProtectedPaths returns the files whose contents decide what the agent
// is allowed to do, and which therefore must not be writable by it.
//
// configPath is the path `-config` resolved to. The list is absolute where it
// can be made absolute, and skips entries that do not resolve — a relative path
// that cannot be made absolute cannot be compared against a write target
// either, and emitting it would produce a rule that silently never matches.
//
// The neighbouring `profiles.d` directory is included because it is the
// conventional place a split-out profile lands, and a protection that covers
// config.yaml while leaving profiles.d/ writable protects nothing.
func PolicyProtectedPaths(configPath string) []string {
	var out []string
	add := func(p string) {
		if p == "" {
			return
		}
		abs, err := filepath.Abs(expandHome(p))
		if err != nil {
			return
		}
		for _, existing := range out {
			if existing == abs {
				return
			}
		}
		out = append(out, abs)
	}
	add(configPath)
	if configPath != "" {
		add(filepath.Join(filepath.Dir(configPath), "profiles.d"))
	}
	if p, ok := PolicyPath(); ok {
		add(p)
	}
	return out
}

// ConfigInAgentWriteScope reports whether the config file sits inside workRoot,
// i.e. inside the tree the agent's fs_write tools are jailed to.
//
// This is the risk `yanshi doctor` surfaces. It is a positional fact, not a
// permission check: the profile's own globs are irrelevant, because the shipped
// example profile writes "**" and any profile at all can be edited into one
// that writes the config once the file is reachable.
//
// The comparison is on cleaned absolute paths at a segment boundary, so a
// sibling directory whose name merely shares a prefix ("/srv/app-old" against
// root "/srv/app") is not counted as inside.
func ConfigInAgentWriteScope(configPath, workRoot string) bool {
	if configPath == "" || workRoot == "" {
		return false
	}
	cfgAbs, err := filepath.Abs(expandHome(configPath))
	if err != nil {
		return false
	}
	rootAbs, err := filepath.Abs(expandHome(workRoot))
	if err != nil {
		return false
	}
	if cfgAbs == rootAbs {
		return true
	}
	return strings.HasPrefix(cfgAbs, rootAbs+string(filepath.Separator))
}
