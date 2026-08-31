package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestLoad_HooksFromYAML pins that every hooks key reaches its struct field.
// Pure plumbing between YAML and the orchestrator: yaml.v3 silently drops
// unknown or mistyped keys, so a missing yaml tag would leave the zero value,
// which for this block means "no hooks run" — an operator who configured a
// PreToolUse gate would get no gate and no error. Values are pairwise
// distinct so a copy-paste swap between entries cannot pass.
func TestLoad_HooksFromYAML(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "c.yaml")
	require.NoError(t, os.WriteFile(p, []byte(
		"hooks:\n"+
			"  pre_tool_use:\n"+
			"    - program: /usr/local/bin/check-policy\n"+
			"      args: [\"--strict\", \"--project\", myproj]\n"+
			"      timeout: 1500ms\n"+
			"    - program: /usr/local/bin/audit-hook\n"+
			"      args: []\n"), 0o644))

	cfg, err := Load(p)
	require.NoError(t, err)

	require.Len(t, cfg.Hooks.PreToolUse, 2)
	first := cfg.Hooks.PreToolUse[0]
	require.Equal(t, "/usr/local/bin/check-policy", first.Program)
	require.Equal(t, []string{"--strict", "--project", "myproj"}, first.Args)
	require.Equal(t, 1500*time.Millisecond, first.Timeout)
	second := cfg.Hooks.PreToolUse[1]
	require.Equal(t, "/usr/local/bin/audit-hook", second.Program)
	require.Empty(t, second.Args)
	require.Equal(t, time.Duration(0), second.Timeout)
}

// TestLoad_HooksAbsentIsZero is the compatibility half, same decision as the
// loop_guard block: Load applies no defaults here, so an installation with no
// hooks block runs no hook processes at all and turns behave byte-identically
// to before the bus existed.
func TestLoad_HooksAbsentIsZero(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(p, []byte("server:\n  http_addr: 127.0.0.1:0\n"), 0o644))

	cfg, err := Load(p)
	require.NoError(t, err)
	require.Equal(t, HooksConfig{}, cfg.Hooks)
}

// TestLoad_HooksRejectsEmptyProgram pins the load-time half of the fail-closed
// ruling: a hook with no program would otherwise fail closed on EVERY tool
// call at runtime ("hook  failed: ..."), in every turn, forever — and the
// operator would have to read a turn transcript to learn why. The mistake is
// easy to make because yaml.v3 silently drops mistyped keys, so "program:"
// with the value at the wrong indentation decodes to an empty string. Load is
// the one moment the operator is guaranteed to be looking.
func TestLoad_HooksRejectsEmptyProgram(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "c.yaml")
	require.NoError(t, os.WriteFile(p, []byte(
		"hooks:\n"+
			"  pre_tool_use:\n"+
			"    - program: \"\"\n"), 0o644))

	_, err := Load(p)
	require.ErrorContains(t, err, "hooks.pre_tool_use[0]")
	require.ErrorContains(t, err, "program is required")
}

// TestLoad_HooksTimeoutRejectsBareNumbers documents the same time.Duration
// edge as loop_guard's turn_timeout: "1500ms" decodes, a bare "1500" does NOT
// and fails at load. A silent 1500ns hook budget would refuse every tool call
// with a timeout — fail-closed applied to a typo.
func TestLoad_HooksTimeoutRejectsBareNumbers(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "c.yaml")
	require.NoError(t, os.WriteFile(p, []byte(
		"hooks:\n  pre_tool_use:\n    - program: /bin/true\n      timeout: 1500\n"), 0o644))

	_, err := Load(p)
	require.Error(t, err, "a unitless hook timeout must fail loudly, not become 1500ns")
}
