package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestLoad_StreamTimeoutsFromYAML pins that llm.stream_first_chunk_timeout and
// llm.stream_idle_timeout (W-A-06) reach LLMConfig. Like loop_guard's duration
// fields, a misspelled yaml tag fails silently: yaml.v3 ignores unknown keys,
// so a typo yields the zero value, which for these two fields means "watchdog
// disabled" — an operator who configured a budget would silently get none.
// The two values are distinct so a copy-paste swap between the fields cannot
// pass.
func TestLoad_StreamTimeoutsFromYAML(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "c.yaml")
	require.NoError(t, os.WriteFile(p, []byte(
		"llm:\n"+
			"  stream_first_chunk_timeout: 15s\n"+
			"  stream_idle_timeout: 2m\n"), 0o644))

	cfg, err := Load(p)
	require.NoError(t, err)

	require.Equal(t, 15*time.Second, cfg.LLM.StreamFirstChunkTimeout)
	require.Equal(t, 2*time.Minute, cfg.LLM.StreamIdleTimeout)
}

// TestLoad_StreamTimeoutsDefaultToDisabled is the compatibility half: an
// absent llm block (or one that omits these keys) must leave both fields at
// the zero value, which is the "watchdog off" state — this is what makes
// W-A-06 byte-identical to pre-existing behaviour for every deployment that
// does not opt in.
func TestLoad_StreamTimeoutsDefaultToDisabled(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "c.yaml")
	require.NoError(t, os.WriteFile(p, []byte("server:\n  http_addr: 127.0.0.1:0\n"), 0o644))

	cfg, err := Load(p)
	require.NoError(t, err)

	require.Zero(t, cfg.LLM.StreamFirstChunkTimeout)
	require.Zero(t, cfg.LLM.StreamIdleTimeout)
}
