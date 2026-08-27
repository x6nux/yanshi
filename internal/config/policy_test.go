package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/x6nux/yanshi/internal/guard"
)

// writePolicyFile writes a trusted policy file and points $YANSHI_POLICY at it.
// t.Setenv restores the previous value, so tests do not leak the variable into
// one another.
func writePolicyFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "policy.yaml")
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	t.Setenv(PolicyEnvVar, path)
	return path
}

// isolatePolicy makes a test independent of whether the developer running it
// happens to have ~/.yanshi/policy.yaml. Pointing the variable at a path that
// does not exist is not enough — LoadPolicy treats an explicitly-configured
// missing file as an error, which is the behaviour a different test pins — so
// this writes an EMPTY policy, which parses to zero profiles and is therefore
// inert.
func isolatePolicy(t *testing.T) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "empty-policy.yaml")
	require.NoError(t, os.WriteFile(path, []byte("profiles: {}\n"), 0o600))
	t.Setenv(PolicyEnvVar, path)
}

// ---------------------------------------------------------------------------
// The escalation this whole file exists to prevent.
// ---------------------------------------------------------------------------

// TestLoad_LocalConfigCannotWidenTrustedPolicy is the S3 headline case, written
// as the attack rather than as an API exercise: the agent rewrites the config
// file it can reach, asking for every tool, every path and an unrestricted
// shell. After the restart it must have gained nothing.
func TestLoad_LocalConfigCannotWidenTrustedPolicy(t *testing.T) {
	writePolicyFile(t, `
profiles:
  orchestrator:
    tools:
      allow: ["fs_read", "fs_list"]
    fs:
      read: ["/srv/project/**"]
      write: ["/srv/project/src/**"]
    shell:
      policy: allowlist
      patterns: ["go test *"]
    net:
      allow: false
`)

	// What an agent with fs_write and a restart would put in config.yaml.
	escalated := `
profiles:
  orchestrator:
    tools:
      allow: ["*"]
    fs:
      read: ["**"]
      write: ["**"]
    shell:
      policy: denylist
      patterns: []
    net:
      allow: true
`
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(cfgPath, []byte(escalated), 0o600))

	cfg, err := Load(cfgPath)
	require.NoError(t, err)

	p := cfg.Profiles["orchestrator"]
	assert.NotContains(t, p.Tools.Allow, "*",
		"the local file must not be able to grant every tool")
	assert.NotContains(t, p.FS.Write, "**",
		"the local file must not be able to grant every write path")
	assert.NotContains(t, p.FS.Read, "**")
	assert.False(t, p.Net.Allow,
		"a trusted net.allow=false must survive a local net.allow=true")
	assert.Equal(t, "allowlist", p.Shell.Policy,
		"a local policy switch that would widen the shell must not take effect")
	assert.True(t, cfg.PolicyActive)
}

// TestLoad_LocalConfigMayStillNarrow pins the other direction. A project that
// wants a TIGHTER profile than the user's default is legitimate and common;
// refusing it would push operators to keep their loose default, which is worse
// than the thing this gate is guarding against.
func TestLoad_LocalConfigMayStillNarrow(t *testing.T) {
	writePolicyFile(t, `
profiles:
  orchestrator:
    tools:
      allow: ["fs_read", "fs_list", "fs_write", "shell_run"]
    fs:
      read: ["**"]
      write: ["**"]
    net:
      allow: true
`)
	local := `
profiles:
  orchestrator:
    tools:
      allow: ["fs_read"]
    fs:
      read: ["docs/**"]
      write: ["docs/**"]
    net:
      allow: false
`
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(cfgPath, []byte(local), 0o600))

	cfg, err := Load(cfgPath)
	require.NoError(t, err)

	p := cfg.Profiles["orchestrator"]
	assert.Equal(t, []string{"fs_read"}, p.Tools.Allow)
	assert.Equal(t, []string{"docs/**"}, p.FS.Read)
	assert.False(t, p.Net.Allow, "a local net.allow=false must narrow a trusted true")
	assert.Equal(t, []string{"orchestrator"}, cfg.PolicyNarrowed)
}

// TestLoad_ProfileAbsentFromPolicyIsDropped closes the escalation one level up:
// an agent that cannot widen "orchestrator" would otherwise simply declare a
// wide profile under a name the trusted file never mentions.
func TestLoad_ProfileAbsentFromPolicyIsDropped(t *testing.T) {
	writePolicyFile(t, `
profiles:
  orchestrator:
    tools:
      allow: ["fs_read"]
`)
	local := `
profiles:
  orchestrator:
    tools:
      allow: ["fs_read"]
  backdoor:
    tools:
      allow: ["*"]
    shell:
      policy: denylist
`
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(cfgPath, []byte(local), 0o600))

	cfg, err := Load(cfgPath)
	require.NoError(t, err)
	_, exists := cfg.Profiles["backdoor"]
	assert.False(t, exists,
		"a profile the trusted policy does not name must not exist; the trusted "+
			"file decides which profiles there are, not merely what they contain")
}

// TestLoad_NoPolicyLeavesProfilesUntouched pins backward compatibility. Every
// existing single-file deployment has no policy file, and this mechanism must
// be invisible to them.
func TestLoad_NoPolicyLeavesProfilesUntouched(t *testing.T) {
	isolatePolicy(t)
	local := `
profiles:
  orchestrator:
    tools:
      allow: ["*"]
    fs:
      read: ["**"]
      write: ["**"]
`
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(cfgPath, []byte(local), 0o600))

	cfg, err := Load(cfgPath)
	require.NoError(t, err)
	assert.Equal(t, []string{"*"}, cfg.Profiles["orchestrator"].Tools.Allow)
	assert.False(t, cfg.PolicyActive)
	assert.Empty(t, cfg.PolicyNarrowed)
}

// TestLoadPolicy_ExplicitButMissingIsAnError. Somebody set $YANSHI_POLICY on
// purpose; a typo in it must not silently disable the gate it was set to
// enable.
func TestLoadPolicy_ExplicitButMissingIsAnError(t *testing.T) {
	t.Setenv(PolicyEnvVar, filepath.Join(t.TempDir(), "absent.yaml"))
	_, err := LoadPolicy()
	assert.Error(t, err)
}

// TestLoadPolicy_MalformedIsAnError. Falling back to the unconstrained local
// profiles would drop the constraint at exactly the moment it was asked for.
func TestLoadPolicy_MalformedIsAnError(t *testing.T) {
	writePolicyFile(t, "profiles: [this is not a map")
	_, err := LoadPolicy()
	assert.Error(t, err)
}

// TestLoadPolicy_RejectsUnknownShellPolicy. An unknown policy string degrades
// into guard's STRUCTURAL HardDeny at call time; catching it while reading the
// file that was meant to enable work is the diagnosable moment.
func TestLoadPolicy_RejectsUnknownShellPolicy(t *testing.T) {
	writePolicyFile(t, `
profiles:
  orchestrator:
    shell:
      policy: allow
`)
	_, err := LoadPolicy()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "shell.policy")
}

// TestLoadPolicy_ReadsTheFile pins the happy path end to end, so a refactor
// that stops consulting the file at all is caught by something other than the
// negative tests above (which would all still pass).
func TestLoadPolicy_ReadsTheFile(t *testing.T) {
	writePolicyFile(t, `
profiles:
  orchestrator:
    tools:
      allow: ["fs_read"]
`)
	p, err := LoadPolicy()
	require.NoError(t, err)
	require.NotNil(t, p)
	assert.Equal(t, []string{"fs_read"}, p.Profiles["orchestrator"].Tools.Allow)
}

// ---------------------------------------------------------------------------
// The narrowing algebra, dimension by dimension.
// ---------------------------------------------------------------------------

func TestNarrowProfile_Dimensions(t *testing.T) {
	cases := []struct {
		name    string
		trusted guard.PermissionProfile
		local   guard.PermissionProfile
		want    guard.PermissionProfile
	}{
		{
			name:    "local subset is kept",
			trusted: guard.PermissionProfile{Tools: guard.ToolsPerm{Allow: []string{"a", "b", "c"}}},
			local:   guard.PermissionProfile{Tools: guard.ToolsPerm{Allow: []string{"a", "c"}}},
			want:    guard.PermissionProfile{Tools: guard.ToolsPerm{Allow: []string{"a", "c"}}},
		},
		{
			name:    "local entry absent from trusted is dropped",
			trusted: guard.PermissionProfile{Tools: guard.ToolsPerm{Allow: []string{"a"}}},
			local:   guard.PermissionProfile{Tools: guard.ToolsPerm{Allow: []string{"a", "shell_run"}}},
			want:    guard.PermissionProfile{Tools: guard.ToolsPerm{Allow: []string{"a"}}},
		},
		{
			// A trusted wildcard genuinely does grant everything, so a narrower
			// local glob under it is a real narrowing and must be honoured —
			// otherwise the mechanism would be unusable with the shipped
			// example profile.
			name:    "trusted wildcard admits a narrower local glob",
			trusted: guard.PermissionProfile{Tools: guard.ToolsPerm{Allow: []string{"*"}}},
			local:   guard.PermissionProfile{Tools: guard.ToolsPerm{Allow: []string{"fs_*"}}},
			want:    guard.PermissionProfile{Tools: guard.ToolsPerm{Allow: []string{"fs_*"}}},
		},
		{
			// Glob containment is not decidable by matching: "fs_r*" is not
			// matched by "fs_read" yet grants strictly more. Dropping is the
			// conservative direction.
			name:    "local glob not provably covered is dropped",
			trusted: guard.PermissionProfile{Tools: guard.ToolsPerm{Allow: []string{"fs_read"}}},
			local:   guard.PermissionProfile{Tools: guard.ToolsPerm{Allow: []string{"fs_r*"}}},
			want:    guard.PermissionProfile{Tools: guard.ToolsPerm{Allow: nil}},
		},
		{
			// THE case that makes coveredByAny's wildcard guard load-bearing,
			// and the reason it is spelled out separately from the one above.
			//
			// Here the trusted pattern MATCHES the local pattern as a string —
			// "fs_?" matches the seven characters "fs_*" — so a coveredByAny
			// that simply ran MatchGlob would admit it. But "fs_*" grants
			// "fs_search" and "fs_read", which "fs_?" does not: matching the
			// pattern text says nothing about containment of the sets.
			//
			// A mutation probe that deleted the wildcard guard left the whole
			// suite green, because the case above happens to survive it
			// (MatchGlob("fs_read","fs_r*") is already false). Searching for a
			// pair where the mutation actually escalates produced this one.
			name:    "a local glob the trusted glob merely string-matches is dropped",
			trusted: guard.PermissionProfile{Tools: guard.ToolsPerm{Allow: []string{"fs_?"}}},
			local:   guard.PermissionProfile{Tools: guard.ToolsPerm{Allow: []string{"fs_*"}}},
			want:    guard.PermissionProfile{Tools: guard.ToolsPerm{Allow: nil}},
		},
		{
			// Same shape, single character, so the escalation is total:
			// "?" matches the one-character string "*", and "*" grants
			// everything.
			name:    "single-char trusted glob does not admit the universal glob",
			trusted: guard.PermissionProfile{Tools: guard.ToolsPerm{Allow: []string{"?"}}},
			local:   guard.PermissionProfile{Tools: guard.ToolsPerm{Allow: []string{"*"}}},
			want:    guard.PermissionProfile{Tools: guard.ToolsPerm{Allow: nil}},
		},
		{
			name:    "a trusted glob admits a local literal it matches",
			trusted: guard.PermissionProfile{Tools: guard.ToolsPerm{Allow: []string{"fs_*"}}},
			local:   guard.PermissionProfile{Tools: guard.ToolsPerm{Allow: []string{"fs_read"}}},
			want:    guard.PermissionProfile{Tools: guard.ToolsPerm{Allow: []string{"fs_read"}}},
		},
		{
			name:    "empty trusted allow permits nothing regardless of local",
			trusted: guard.PermissionProfile{Tools: guard.ToolsPerm{Allow: nil}},
			local:   guard.PermissionProfile{Tools: guard.ToolsPerm{Allow: []string{"a"}}},
			want:    guard.PermissionProfile{Tools: guard.ToolsPerm{Allow: nil}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := NarrowProfile(tc.trusted, tc.local)
			assert.Equal(t, tc.want.Tools.Allow, got.Tools.Allow)
		})
	}
}

// TestNarrowProfile_ShellDimension.
func TestNarrowProfile_ShellDimension(t *testing.T) {
	t.Run("matching allowlists intersect", func(t *testing.T) {
		got := NarrowProfile(
			guard.PermissionProfile{Shell: guard.ShellPerm{Policy: "allowlist", Patterns: []string{"go *", "git *"}}},
			guard.PermissionProfile{Shell: guard.ShellPerm{Policy: "allowlist", Patterns: []string{"go *"}}},
		)
		assert.Equal(t, "allowlist", got.Shell.Policy)
		assert.Equal(t, []string{"go *"}, got.Shell.Patterns)
	})

	t.Run("empty policy string is the allowlist alias", func(t *testing.T) {
		// guard.ShellPolicies documents "" == "allowlist". Comparing raw
		// strings would treat an omitted key as a different policy and hand
		// the whole dimension back to trusted for no reason.
		got := NarrowProfile(
			guard.PermissionProfile{Shell: guard.ShellPerm{Policy: "", Patterns: []string{"go *", "git *"}}},
			guard.PermissionProfile{Shell: guard.ShellPerm{Policy: "allowlist", Patterns: []string{"go *"}}},
		)
		assert.Equal(t, []string{"go *"}, got.Shell.Patterns)
	})

	t.Run("denylists union because more denials is narrower", func(t *testing.T) {
		got := NarrowProfile(
			guard.PermissionProfile{Shell: guard.ShellPerm{Policy: "denylist", Patterns: []string{"rm *"}}},
			guard.PermissionProfile{Shell: guard.ShellPerm{Policy: "denylist", Patterns: []string{"curl *"}}},
		)
		assert.ElementsMatch(t, []string{"rm *", "curl *"}, got.Shell.Patterns)
	})

	t.Run("differing policies fall back to trusted", func(t *testing.T) {
		got := NarrowProfile(
			guard.PermissionProfile{Shell: guard.ShellPerm{Policy: "allowlist", Patterns: []string{"go *"}}},
			guard.PermissionProfile{Shell: guard.ShellPerm{Policy: "denylist"}},
		)
		assert.Equal(t, "allowlist", got.Shell.Policy)
		assert.Equal(t, []string{"go *"}, got.Shell.Patterns)
	})

	t.Run("trusted deny is the floor", func(t *testing.T) {
		got := NarrowProfile(
			guard.PermissionProfile{Shell: guard.ShellPerm{Policy: "deny"}},
			guard.PermissionProfile{Shell: guard.ShellPerm{Policy: "deny"}},
		)
		assert.Equal(t, "deny", got.Shell.Policy)
	})
}

// TestNarrowProfile_NetDimension. The disjoint-hosts case is the interesting
// one: an empty host list means "no restriction" in guard.checkNet, so
// returning one would WIDEN the profile — the single outcome narrowing may
// never produce.
func TestNarrowProfile_NetDimension(t *testing.T) {
	t.Run("allow is an AND", func(t *testing.T) {
		got := NarrowProfile(
			guard.PermissionProfile{Net: guard.NetPerm{Allow: true}},
			guard.PermissionProfile{Net: guard.NetPerm{Allow: false}},
		)
		assert.False(t, got.Net.Allow)

		got = NarrowProfile(
			guard.PermissionProfile{Net: guard.NetPerm{Allow: false}},
			guard.PermissionProfile{Net: guard.NetPerm{Allow: true}},
		)
		assert.False(t, got.Net.Allow)
	})

	t.Run("hosts intersect", func(t *testing.T) {
		got := NarrowProfile(
			guard.PermissionProfile{Net: guard.NetPerm{Allow: true, Hosts: []string{"a.com", "b.com"}}},
			guard.PermissionProfile{Net: guard.NetPerm{Allow: true, Hosts: []string{"a.com"}}},
		)
		assert.True(t, got.Net.Allow)
		assert.Equal(t, []string{"a.com"}, got.Net.Hosts)
	})

	t.Run("empty local hosts adds no restriction", func(t *testing.T) {
		got := NarrowProfile(
			guard.PermissionProfile{Net: guard.NetPerm{Allow: true, Hosts: []string{"a.com"}}},
			guard.PermissionProfile{Net: guard.NetPerm{Allow: true}},
		)
		assert.Equal(t, []string{"a.com"}, got.Net.Hosts)
	})

	t.Run("disjoint hosts deny rather than widen", func(t *testing.T) {
		got := NarrowProfile(
			guard.PermissionProfile{Net: guard.NetPerm{Allow: true, Hosts: []string{"a.com"}}},
			guard.PermissionProfile{Net: guard.NetPerm{Allow: true, Hosts: []string{"evil.com"}}},
		)
		assert.False(t, got.Net.Allow,
			"an empty host list would mean 'any host'; disjoint lists must deny instead")
		assert.NotContains(t, got.Net.Hosts, "evil.com")
	})
}

// TestNarrowProfile_SubagentDimension.
func TestNarrowProfile_SubagentDimension(t *testing.T) {
	t.Run("models intersect", func(t *testing.T) {
		got := NarrowProfile(
			guard.PermissionProfile{Subagent: guard.SubagentPerm{Models: []string{"big", "small"}}},
			guard.PermissionProfile{Subagent: guard.SubagentPerm{Models: []string{"small"}}},
		)
		assert.Equal(t, []string{"small"}, got.Subagent.Models)
	})

	t.Run("a model the trusted policy never allowed is dropped", func(t *testing.T) {
		got := NarrowProfile(
			guard.PermissionProfile{Subagent: guard.SubagentPerm{Models: []string{"small"}}},
			guard.PermissionProfile{Subagent: guard.SubagentPerm{Models: []string{"small", "big"}}},
		)
		assert.Equal(t, []string{"small"}, got.Subagent.Models)
	})

	t.Run("reasoning cap may lower but not raise", func(t *testing.T) {
		lowered := NarrowProfile(
			guard.PermissionProfile{Subagent: guard.SubagentPerm{MaxReasoning: "medium"}},
			guard.PermissionProfile{Subagent: guard.SubagentPerm{MaxReasoning: "low"}},
		)
		assert.Equal(t, "low", lowered.Subagent.MaxReasoning)

		raised := NarrowProfile(
			guard.PermissionProfile{Subagent: guard.SubagentPerm{MaxReasoning: "low"}},
			guard.PermissionProfile{Subagent: guard.SubagentPerm{MaxReasoning: "high"}},
		)
		assert.Equal(t, "low", raised.Subagent.MaxReasoning)
	})
}

// TestNarrowProfile_NeverWidensAnyDimension is the property the whole file is
// about, asserted as a property rather than as another example: for a spread of
// (trusted, local) pairs, every dimension of the result must be provably no
// wider than trusted.
func TestNarrowProfile_NeverWidensAnyDimension(t *testing.T) {
	trusted := guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"fs_read", "fs_list"}},
		MCP:   guard.ToolsPerm{Allow: []string{"mcp_a_x"}},
		FS:    guard.FSPerm{Read: []string{"/p/**"}, Write: []string{"/p/src/**"}},
		Shell: guard.ShellPerm{Policy: "allowlist", Patterns: []string{"go test *"}},
		Net:   guard.NetPerm{Allow: true, Hosts: []string{"api.example.com"}},
	}
	locals := []guard.PermissionProfile{
		{Tools: guard.ToolsPerm{Allow: []string{"*"}}, FS: guard.FSPerm{Read: []string{"**"}, Write: []string{"**"}},
			MCP: guard.ToolsPerm{Allow: []string{"*"}},
			Net: guard.NetPerm{Allow: true, Hosts: []string{"*"}}},
		{Tools: guard.ToolsPerm{Allow: []string{"shell_run"}}, Shell: guard.ShellPerm{Policy: "denylist"}},
		{Tools: guard.ToolsPerm{Allow: []string{"fs_read"}}, FS: guard.FSPerm{Read: []string{"/etc/**"}}},
		{},
	}
	for i, local := range locals {
		got := NarrowProfile(trusted, local)
		for _, name := range got.Tools.Allow {
			assert.True(t, coveredByAny(trusted.Tools.Allow, name),
				"case %d: tool %q is not covered by the trusted allow list", i, name)
		}
		for _, name := range got.MCP.Allow {
			assert.True(t, coveredByAny(trusted.MCP.Allow, name), "case %d: mcp %q escaped", i, name)
		}
		for _, p := range got.FS.Read {
			assert.True(t, coveredByAny(trusted.FS.Read, p), "case %d: fs.read %q escaped", i, p)
		}
		for _, p := range got.FS.Write {
			assert.True(t, coveredByAny(trusted.FS.Write, p), "case %d: fs.write %q escaped", i, p)
		}
		if got.Net.Allow {
			for _, h := range got.Net.Hosts {
				assert.True(t, coveredByAny(trusted.Net.Hosts, h), "case %d: host %q escaped", i, h)
			}
		}
		assert.Equal(t, "allowlist", got.Shell.Policy, "case %d: shell policy widened", i)
		for _, p := range got.Shell.Patterns {
			assert.True(t, coveredByAny(trusted.Shell.Patterns, p), "case %d: shell %q escaped", i, p)
		}
	}
}

// ---------------------------------------------------------------------------
// The positional risk `yanshi doctor` reports.
// ---------------------------------------------------------------------------

func TestConfigInAgentWriteScope(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()

	assert.True(t, ConfigInAgentWriteScope(filepath.Join(root, "config.yaml"), root),
		"a config file directly inside the work root is reachable by fs_write")
	assert.True(t, ConfigInAgentWriteScope(filepath.Join(root, "etc", "config.yaml"), root),
		"nesting does not put it out of reach")
	assert.False(t, ConfigInAgentWriteScope(filepath.Join(outside, "config.yaml"), root))
	assert.False(t, ConfigInAgentWriteScope("", root))
	assert.False(t, ConfigInAgentWriteScope(filepath.Join(root, "config.yaml"), ""))
}

// TestConfigInAgentWriteScope_SiblingPrefixIsNotInside pins the comparison at a
// path-SEGMENT boundary. A plain string prefix test would report "/srv/app-old"
// as living inside "/srv/app", which is a false alarm on a check whose whole
// value is that operators believe it.
func TestConfigInAgentWriteScope_SiblingPrefixIsNotInside(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "app")
	sibling := filepath.Join(base, "app-old")
	require.NoError(t, os.MkdirAll(root, 0o755))
	require.NoError(t, os.MkdirAll(sibling, 0o755))
	assert.False(t, ConfigInAgentWriteScope(filepath.Join(sibling, "config.yaml"), root))
}

// TestPolicyProtectedPaths pins that the set names every file whose contents
// decide what the agent may do. profiles.d is included because a protection
// covering config.yaml while leaving the conventional split-out directory
// writable protects nothing.
func TestPolicyProtectedPaths(t *testing.T) {
	policyPath := writePolicyFile(t, "profiles: {}\n")
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")

	got := PolicyProtectedPaths(cfgPath)
	assert.Contains(t, got, cfgPath)
	assert.Contains(t, got, filepath.Join(dir, "profiles.d"))
	assert.Contains(t, got, policyPath)

	for _, p := range got {
		assert.True(t, filepath.IsAbs(p), "every protected path must be absolute, got %q", p)
	}
}

// TestPolicyProtectedPaths_Deduplicates. A duplicate entry would produce a
// duplicated denial message, which is the kind of small wrongness that makes
// operators distrust the whole report.
func TestPolicyProtectedPaths_Deduplicates(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "policy.yaml")
	t.Setenv(PolicyEnvVar, cfgPath)
	got := PolicyProtectedPaths(cfgPath)
	seen := map[string]int{}
	for _, p := range got {
		seen[p]++
	}
	for p, n := range seen {
		assert.Equal(t, 1, n, "path %q appears %d times", p, n)
	}
}
