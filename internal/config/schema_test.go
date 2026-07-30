package config_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/config"
)

func writeCfg(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(p, []byte(body), 0o644))
	return p
}

func TestSchemaVersionMissingDefaultsToOne(t *testing.T) {
	cfg, err := config.Load(writeCfg(t, `llm: {}`))
	require.NoError(t, err)
	assert.Equal(t, config.SupportedSchemaVersion, cfg.SchemaVersion,
		"omitted schema_version must read as the current supported version")
}

func TestSchemaVersionExplicitOneLoads(t *testing.T) {
	cfg, err := config.Load(writeCfg(t, "schema_version: 1\nllm: {}\n"))
	require.NoError(t, err)
	assert.Equal(t, 1, cfg.SchemaVersion)
}

func TestSchemaVersionTooHighIsRejected(t *testing.T) {
	future := config.SupportedSchemaVersion + 1
	_, err := config.Load(writeCfg(t,
		"schema_version: "+itoa(future)+"\nllm: {}\n"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "schema_version")
}

func TestMigrateConfigIsNoOpAtV1(t *testing.T) {
	// v1 has no destructive migration. migrateConfig must not mutate the cfg
	// passed in beyond setting the canonical version, and must not error.
	cfg := &config.Config{SchemaVersion: 1}
	err := config.MigrateConfig(cfg, 1, config.SupportedSchemaVersion)
	require.NoError(t, err)
	assert.Equal(t, config.SupportedSchemaVersion, cfg.SchemaVersion)
}

func TestSchemaVersionNegativeMigrates(t *testing.T) {
	_, err := config.Load(writeCfg(t, "schema_version: -1\nllm: {}\n"))
	require.NoError(t, err, "negative schema_version should migrate to current")
}

func TestLoadDoesNotWriteDisk(t *testing.T) {
	p := writeCfg(t, "schema_version: 1\nllm: {}\n")
	before, err := os.ReadFile(p)
	require.NoError(t, err)
	_, err = config.Load(p)
	require.NoError(t, err)
	after, err := os.ReadFile(p)
	require.NoError(t, err)
	assert.Equal(t, before, after, "Load must never rewrite the user's config file")
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [16]byte
	pos := len(buf)
	for n > 0 {
		pos--
		buf[pos] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[pos:])
}
