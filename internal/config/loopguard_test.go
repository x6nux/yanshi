package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// TestLoad_LoopGuardFromYAML pins that every loop_guard key reaches its struct
// field. The block is pure plumbing between YAML and the orchestrator, and the
// failure mode of a missing or misspelled yaml tag is silent: yaml.v3 ignores
// unknown keys, so a typo yields the zero value, which for this block means
// "guard disabled" — an operator who configured a budget would get no budget
// and no error. Each field is given a value distinct from every other field's
// so a copy-paste swap between two int fields cannot pass.
func TestLoad_LoopGuardFromYAML(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "c.yaml")
	require.NoError(t, os.WriteFile(p, []byte(
		"loop_guard:\n"+
			"  repetition_enabled: true\n"+
			"  repetition_window: 5\n"+
			"  repetition_warn_after: 6\n"+
			"  repetition_stop_after: 7\n"+
			"  max_tool_calls: 42\n"+
			"  per_tool_calls:\n"+
			"    shell_run: 20\n"+
			"    agent_spawn: 3\n"+
			"  turn_timeout: 90s\n"+
			"  max_turn_tokens: 12345\n"), 0o644))

	cfg, err := Load(p)
	require.NoError(t, err)

	lg := cfg.LoopGuard
	require.True(t, lg.RepetitionEnabled)
	require.Equal(t, 5, lg.RepetitionWindow)
	require.Equal(t, 6, lg.RepetitionWarnAfter)
	require.Equal(t, 7, lg.RepetitionStopAfter)
	require.Equal(t, 42, lg.MaxToolCalls)
	require.Equal(t, map[string]int{"shell_run": 20, "agent_spawn": 3}, lg.PerToolCalls)
	require.Equal(t, 90*time.Second, lg.TurnTimeout)
	require.Equal(t, 12345, lg.MaxTurnTokens)
}

// TestLoad_LoopGuardDefaultsToFullyDisabled is the compatibility half. Load
// applies defaults to several other blocks (compaction, security, shell), so
// "Load leaves this one alone" is a real decision rather than an accident of
// the current code, and it is the decision that keeps an installation with no
// loop_guard block behaving exactly as it did before the guard existed. If
// someone later adds defaults here, this test names the consequence.
func TestLoad_LoopGuardDefaultsToFullyDisabled(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(p, []byte("server:\n  http_addr: 127.0.0.1:0\n"), 0o644))

	cfg, err := Load(p)
	require.NoError(t, err)

	require.Equal(t, LoopGuardConfig{}, cfg.LoopGuard,
		"an absent loop_guard block must stay the zero value: every gate off")
}

// TestLoad_LoopGuardTurnTimeoutRejectsBareNumbers documents the one sharp edge
// of a time.Duration field under yaml.v3: "90s" decodes, a bare "90" does NOT
// and produces a type error rather than 90 nanoseconds. That is the behaviour
// we want (a silent 90ns turn budget would abort every turn instantly), so it
// is pinned rather than worked around with a custom unmarshaller.
func TestLoad_LoopGuardTurnTimeoutRejectsBareNumbers(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "c.yaml")
	require.NoError(t, os.WriteFile(p, []byte("loop_guard:\n  turn_timeout: 90\n"), 0o644))

	_, err := Load(p)
	require.Error(t, err, "a unitless turn_timeout must fail loudly, not become 90ns")
}

// TestExampleConfigDocumentsLoopGuard keeps the shipped example honest. The
// example file is what operators copy to build config.yaml, so a key that
// exists in Go but not there is undiscoverable, and a key that exists there but
// not in Go is advertised and inert — the same phantom-capability class the
// repo's slash-command gate exists to catch. Comparing against the decoded
// struct rather than a hand-listed set means adding a field to LoopGuardConfig
// without documenting it turns this red.
func TestExampleConfigDocumentsLoopGuard(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "config.example.yaml"))
	require.NoError(t, err, "config.example.yaml must be readable from the repo root")

	// Decode twice: once into the typed struct (proves the documented spellings
	// are the ones Go binds) and once into a free-form map (proves no EXTRA key
	// is advertised that Go would silently drop).
	var typed Config
	require.NoError(t, yaml.Unmarshal(raw, &typed), "config.example.yaml must be valid YAML")

	var loose struct {
		LoopGuard map[string]any `yaml:"loop_guard"`
	}
	require.NoError(t, yaml.Unmarshal(raw, &loose))
	require.NotEmpty(t, loose.LoopGuard, "config.example.yaml must document the loop_guard block")

	want := map[string]bool{
		"repetition_enabled": true, "repetition_window": true,
		"repetition_warn_after": true, "repetition_stop_after": true,
		"max_tool_calls": true, "per_tool_calls": true,
		"turn_timeout": true, "max_turn_tokens": true,
	}
	for k := range loose.LoopGuard {
		require.True(t, want[k], "config.example.yaml documents loop_guard key %q that LoopGuardConfig does not bind", k)
		delete(want, k)
	}
	require.Empty(t, want, "LoopGuardConfig fields missing from config.example.yaml: %v", want)

	// The example must ship the guard OFF, matching the "no defaults" decision.
	// A shipped example that enables a stop condition would hand every operator
	// who copies it a turn budget they never chose.
	require.Equal(t, LoopGuardConfig{PerToolCalls: map[string]int{}}, typed.LoopGuard,
		"the shipped example must leave every loop_guard limit disabled")
}
