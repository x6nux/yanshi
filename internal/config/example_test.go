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
