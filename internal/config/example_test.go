package config

import (
	"os"
	"path/filepath"
	"sort"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/stretchr/testify/require"
)

// TestExampleConfigDocumentsSubagents pins the shipped config.example.yaml as
// the discovery surface for the managed sub-agent runtime (Batch B1). Users copy
// this file to config.yaml, so a knob missing here is a knob nobody finds.
//
// It deliberately parses the raw YAML into a generic map instead of calling
// Load: Load validates against the host environment (e.g. it rejects a raw
// literal api_key, which a developer with ANTHROPIC_API_KEY exported would trip
// on), which would make this test pass in CI and fail on a real workstation.
// Key presence is all this assertion needs, and it is environment-independent.
func TestExampleConfigDocumentsSubagents(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "config.example.yaml"))
	require.NoError(t, err, "config.example.yaml must be readable from the repo root")

	var top map[string]any
	require.NoError(t, yaml.Unmarshal(raw, &top), "config.example.yaml must be valid YAML")

	keys := make([]string, 0, len(top))
	for k := range top {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	require.Contains(t, keys, "subagents",
		"config.example.yaml must document the subagents block (limit / persistence_path)")

	block, ok := top["subagents"].(map[string]any)
	require.True(t, ok, "the subagents block must be a mapping, got %T", top["subagents"])
	require.Contains(t, block, "limit", "subagents must document the concurrency limit")
	require.Contains(t, block, "persistence_path", "subagents must document the persistence path")
}

// TestExampleConfigProfilesAreEnforceable runs the shipped example's profiles
// through the same validation Load applies, so the file operators copy can
// never ship a profile the guard would refuse to start on.
//
// It calls validate() rather than Load for the reason documented on
// TestExampleConfigDocumentsSubagents: Load also validates api_key references
// against the host environment, which would make this assertion depend on
// whether the developer has ANTHROPIC_API_KEY exported.
//
// require.NoError alone would not be evidence of anything. Gutting
// validateProfiles to `return nil` leaves this test green, and so would an
// example that stopped setting shell.policy at all — the assertion would then
// be vacuously true forever. Hence the second half: at least one shipped
// profile must actually reach the policy check.
//
// "Reach" is the load-bearing word, and it is why the guard tests BOTH halves
// of the condition. validateProfiles skips any profile whose shell.rules is
// non-empty (the policy switch is unreachable there, so the value is inert and
// rejecting it would be a behavioural regression — see its doc comment). A
// guard that only demanded a non-empty policy would therefore go vacuous the
// day someone adds a rules table to every example profile: validateProfiles
// would `continue` past all of them, require.NoError would be trivially true
// again, and the guard meant to detect exactly that would still be green.
// Today both example profiles have no rules, so the pair is satisfied by the
// same profiles either way.
//
// The rejection itself is pinned by TestLoadBytesRejectsUnknownShellPolicy,
// which does fail when the validation is gutted.
func TestExampleConfigProfilesAreEnforceable(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "config.example.yaml"))
	require.NoError(t, err, "config.example.yaml must be readable from the repo root")

	var cfg Config
	require.NoError(t, yaml.Unmarshal(raw, &cfg), "config.example.yaml must be valid YAML")
	require.NotEmpty(t, cfg.Profiles, "the example must ship at least one profile to validate")
	require.NoError(t, cfg.validate(),
		"config.example.yaml must pass the validation Load performs")

	var checked []string
	for name, p := range cfg.Profiles {
		// Both halves: an explicit policy AND an empty rules table, which is
		// exactly the pair validateProfiles requires before it looks at the
		// policy at all.
		if p.Shell.Policy != "" && len(p.Shell.Rules) == 0 {
			checked = append(checked, name)
		}
	}
	require.NotEmpty(t, checked,
		"no example profile has both an explicit shell.policy and an empty shell.rules, so "+
			"validateProfiles skipped every one of them and the NoError above certifies "+
			"nothing about policy validation; keep at least one rules-free profile with an "+
			"explicit policy")
}
