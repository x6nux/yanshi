package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/guard"
)

func TestLoad(t *testing.T) {
	yaml := []byte(`
server:
  http_addr: "127.0.0.1:9090"
storage:
  sqlite_path: "test.db"
token: "tok-123"
llm:
  providers:
    - name: "openai"
      model: "gpt-4o"
      api_key: env://OPENAI_API_KEY_CONFIG_TEST
agents:
  - name: "orchestrator"
    type: "local"
    chain: ["openai"]
    profile: "orchestrator"
profiles:
  orchestrator:
    fs: { read: ["D:/code/**"], write: ["D:/code/**"] }
    tools: { allow: ["fs.*"] }
    shell: { policy: "allowlist", patterns: ["go *"] }
    net: { allow: false }
auth:
  legacy_insecure: false
`)
	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, yaml, 0o644))

	cfg, err := Load(path)
	require.NoError(t, err)

	assert.Equal(t, "127.0.0.1:9090", cfg.Server.HTTPAddr)
	assert.Equal(t, "test.db", cfg.Storage.SQLitePath)
	assert.Equal(t, "tok-123", cfg.Token)
	require.Len(t, cfg.LLM.Providers, 1)
	assert.Equal(t, "openai", cfg.LLM.Providers[0].Name)
	require.Len(t, cfg.Agents, 1)
	assert.Equal(t, []string{"openai"}, cfg.Agents[0].Chain)
	assert.Contains(t, cfg.Profiles, "orchestrator")
	assert.Equal(t, "allowlist", cfg.Profiles["orchestrator"].Shell.Policy)
}

func TestLoad_MissingFile(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "nope.yaml"))
	assert.Error(t, err)
}

func TestLoad_SkillsConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`
skills:
  builtin_dir: ./skills
  user_dir: ~/.yanshi/skills
  plugin_dir: ~/.yanshi/plugins
`), 0o644))
	cfg, err := Load(path)
	require.NoError(t, err)
	assert.Equal(t, "./skills", cfg.Skills.BuiltinDir)
	assert.Equal(t, "~/.yanshi/skills", cfg.Skills.UserDir)
}

func TestLoad_SkillsConfigDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte("{}"), 0o644))
	cfg, err := Load(path)
	require.NoError(t, err)
	assert.Equal(t, "", cfg.Skills.BuiltinDir) // empty when unset; bootstrap resolves defaults
}

func TestLoad_VCSConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`
vcs:
  ignore: [".cache", "coverage/*"]
  worktree_dir: ~/yanshi-wts
`), 0o644))
	cfg, err := Load(path)
	require.NoError(t, err)
	assert.Equal(t, []string{".cache", "coverage/*"}, cfg.VCS.Ignore)
	assert.Equal(t, "~/yanshi-wts", cfg.VCS.WorktreeDir)
}

func TestLoad_VCSConfigDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte("{}"), 0o644))
	cfg, err := Load(path)
	require.NoError(t, err)
	assert.Empty(t, cfg.VCS.Ignore)
	assert.Empty(t, cfg.VCS.WorktreeDir)
}

func TestLoad_CompactionDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte("{}"), 0o644))
	cfg, err := Load(path)
	require.NoError(t, err)
	assert.Equal(t, 0.8, cfg.Compaction.Threshold, "default threshold applied in Load")
	assert.Equal(t, 4, cfg.Compaction.KeepRecent, "default keep_recent applied in Load")
	assert.Equal(t, 256000, cfg.Compaction.ContextWindow, "default context_window applied in Load")
	assert.Empty(t, cfg.Compaction.Model, "no dedicated compaction model by default")
}

func TestLoad_CompactionExplicit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`
compaction:
  threshold: 0.5
  keep_recent: 2
  context_window: 16000
  model: "gpt-4o-mini"
`), 0o644))
	cfg, err := Load(path)
	require.NoError(t, err)
	assert.Equal(t, 0.5, cfg.Compaction.Threshold)
	assert.Equal(t, 2, cfg.Compaction.KeepRecent)
	assert.Equal(t, 16000, cfg.Compaction.ContextWindow)
	assert.Equal(t, "gpt-4o-mini", cfg.Compaction.Model)
}

func TestLoad_EnvExpansion(t *testing.T) {
	t.Setenv("YANSHI_TEST_TOKEN", "env-token-456")
	t.Setenv("YANSHI_TEST_ADDR", "127.0.0.1:7777")

	content := []byte(`
server:
  http_addr: "${YANSHI_TEST_ADDR}"
storage:
  sqlite_path: "test.db"
token: "${YANSHI_TEST_TOKEN}"
`)
	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, content, 0o644))

	cfg, err := Load(path)
	require.NoError(t, err)
	assert.Equal(t, "env-token-456", cfg.Token)
	assert.Equal(t, "127.0.0.1:7777", cfg.Server.HTTPAddr)
}

func TestProviderConfig_ContextWindow(t *testing.T) {
	cfg := &Config{LLM: LLMConfig{Providers: []ProviderConfig{
		{Name: "openai", ContextWindow: 128000},
		{Name: "claude"}, // unset
	}}}
	assert.Equal(t, 128000, ContextWindowFor("openai", cfg.LLM.Providers, 256000))
	assert.Equal(t, 256000, ContextWindowFor("claude", cfg.LLM.Providers, 256000), "fallback when provider unset")
	assert.Equal(t, 256000, ContextWindowFor("unknown", cfg.LLM.Providers, 256000), "fallback when provider absent")
	assert.Equal(t, 0, ContextWindowFor("unknown", cfg.LLM.Providers, 0), "zero when no fallback either")
}

func TestLoad_ChunkThresholdDefault(t *testing.T) {
	tmp := t.TempDir() + "/c.yaml"
	require.NoError(t, os.WriteFile(tmp, []byte("compaction:\n  threshold: 0.8\n"), 0o644))
	cfg, err := Load(tmp)
	require.NoError(t, err)
	assert.Equal(t, 0.9, cfg.Compaction.ChunkThreshold, "chunk_threshold defaults to 0.9")
}

// TestSecuritySandboxEnabledCanDistinguishUnsetAndFalse (Task 10): the *bool
// shape must distinguish "operator wrote enabled: false" from "operator omitted
// the key entirely". The former is an explicit opt-out; the latter takes the
// default (true) so the safe posture is what you get when you do nothing.
func TestSecuritySandboxEnabledCanDistinguishUnsetAndFalse(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`
storage: {sqlite_path: ":memory:"}
security:
  sandbox:
    enabled: false
    tier: workspace-write
`), 0o644))
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Security.Sandbox.Enabled == nil || *cfg.Security.Sandbox.Enabled {
		t.Fatalf("enabled=%v", cfg.Security.Sandbox.Enabled)
	}
	if cfg.Security.Sandbox.Tier != "workspace-write" {
		t.Fatalf("tier=%q", cfg.Security.Sandbox.Tier)
	}
}

// TestSecurityDefaultsAppliedOnMissingBlock: when the security block is
// entirely absent, Enabled must default to true (the safe posture), tier to
// read-only, and shell limits to the documented non-zero values.
func TestSecurityDefaultsAppliedOnMissingBlock(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`storage: {sqlite_path: ":memory:"}`), 0o644))
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Security.Sandbox.Enabled == nil || !*cfg.Security.Sandbox.Enabled {
		t.Fatalf("default enabled should be true, got %v", cfg.Security.Sandbox.Enabled)
	}
	if cfg.Security.Sandbox.Tier != "read-only" {
		t.Fatalf("default tier should be read-only, got %q", cfg.Security.Sandbox.Tier)
	}
	if cfg.Security.Network.Default != "deny" {
		t.Fatalf("default network posture should be deny, got %q", cfg.Security.Network.Default)
	}
	if cfg.Security.Shell.MaxOutputBytes != 1<<20 {
		t.Fatalf("default shell output cap should be 1MiB, got %d", cfg.Security.Shell.MaxOutputBytes)
	}
}

func TestBatchConfigRLMDefaultsAndCostClass(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "config.yaml")
	yaml := []byte(`
llm:
  providers:
    - name: cheap-provider
      kind: openai
      model: cheap-model
      cost_class: cheap
    - name: expensive-provider
      kind: openai
      model: big-model
      cost_class: expensive
batch:
  rlm_model: cheap-provider
  rlm_max_concurrency: 8
  automation_tick_seconds: 30
`)
	require.NoError(t, os.WriteFile(path, yaml, 0o644))

	cfg, err := Load(path)
	require.NoError(t, err)
	require.NotNil(t, cfg.Batch)
	assert.Equal(t, "cheap-provider", cfg.Batch.RLMModel)
	assert.Equal(t, 8, cfg.Batch.RLMMaxConcurrency)
	assert.Equal(t, 30, cfg.Batch.AutomationTickSec)

	// CostClass 留在 provider 上；bootstrap 会校验 RLMModel 指向 cheap。
	for _, p := range cfg.LLM.Providers {
		if p.Name == "cheap-provider" {
			assert.Equal(t, "cheap", p.CostClass)
		}
	}
}

func TestBatchConfigZeroValuesAreValid(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`llm: {}`), 0o644))
	cfg, err := Load(path)
	require.NoError(t, err)
	assert.Equal(t, "", cfg.Batch.RLMModel)
	assert.Equal(t, 0, cfg.Batch.RLMMaxConcurrency)
}

func TestLoadBytesExpandsSubagents(t *testing.T) {
	yaml := []byte(`
subagents:
  limit: 5
  persistence_path: "~/yanshi/subagents.v1.json"
`)
	cfg, err := LoadBytes(yaml)
	require.NoError(t, err)
	require.Equal(t, 5, cfg.Subagents.Limit)
	require.NotContains(t, cfg.Subagents.PersistencePath, "~")
}

func TestLoadBytesRejectsInvalidSubagentLimit(t *testing.T) {
	yaml := []byte("subagents:\n  limit: 99\n")
	_, err := LoadBytes(yaml)
	require.Error(t, err)
}

func TestLoadBytesDefaultsSubagents(t *testing.T) {
	yaml := []byte("{}")
	cfg, err := LoadBytes(yaml)
	require.NoError(t, err)
	require.Equal(t, 10, cfg.Subagents.Limit)
	require.NotEmpty(t, cfg.Subagents.PersistencePath)
}

func TestLoadObservabilityFeaturesAndPricing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	body := `
server: { http_addr: "127.0.0.1:0" }
storage: { sqlite_path: ":memory:" }
observability:
  log: { level: "debug", format: "text" }
  otel:
    enabled: true
    endpoint: "http://127.0.0.1:4318"
    service_name: "yanshi-test"
    sample_ratio: 0.25
features:
  strict: true
  overrides:
    observe.otel_export: true
pricing:
  overrides:
    custom-model:
      input_per_million: 2
      cache_hit_per_million: 0.2
      output_per_million: 8
`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Observability.Log.Level != "debug" || cfg.Observability.Log.Format != "text" {
		t.Fatalf("log config = %+v", cfg.Observability.Log)
	}
	if !cfg.Observability.OTel.Enabled || cfg.Observability.OTel.SampleRatio != 0.25 {
		t.Fatalf("otel config = %+v", cfg.Observability.OTel)
	}
	if !cfg.Features.Strict || !cfg.Features.Overrides["observe.otel_export"] {
		t.Fatalf("features config = %+v", cfg.Features)
	}
	price := cfg.Pricing.Overrides["custom-model"]
	if price.InputPerM != 2 || price.CacheHitPerM != 0.2 || price.OutputPerM != 8 {
		t.Fatalf("pricing override = %+v", price)
	}
}

func TestLoadObservabilityDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("storage: { sqlite_path: ':memory:' }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Observability.Log.Level != "info" || cfg.Observability.Log.Format != "json" {
		t.Fatalf("log defaults = %+v", cfg.Observability.Log)
	}
	if cfg.Observability.OTel.ServiceName != "yanshi" {
		t.Fatalf("service default = %q", cfg.Observability.OTel.ServiceName)
	}
}

func TestConfig_NewSecretsBlock(t *testing.T) {
	yaml := []byte(`
secrets:
  backend: auto
  file_path: ~/.yanshi/secrets.enc
auth:
  device:
    client_id: test-id
i18n:
  ui_locale: auto
  output_language: en
tui:
  vim: true
  high_contrast: false
  bindings:
    ctrl+k: scroll_down
`)
	tmp := t.TempDir() + "/cfg.yaml"
	if err := os.WriteFile(tmp, yaml, 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(tmp)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Secrets.Backend != "auto" || cfg.Auth.Device.ClientID != "test-id" ||
		cfg.I18N.UILocale != "auto" || cfg.I18N.OutputLanguage != "en" ||
		cfg.TUI.Vim == nil || !*cfg.TUI.Vim ||
		cfg.TUI.HighContrast == nil || *cfg.TUI.HighContrast ||
		cfg.TUI.Bindings["ctrl+k"] != "scroll_down" {
		t.Fatalf("new fields not parsed: %+v", cfg)
	}
}

// TestSecretsFilePath_UserConfigDirFail covers the error fallback when
// os.UserConfigDir() fails. The secrets.FilePath should still be set to a
// sensible default using "." as the config directory fallback.
func TestSecretsFilePath_UserConfigDirFail(t *testing.T) {
	// On Windows, UserConfigDir uses %APPDATA%; on Unix it uses $HOME/.config.
	// Clearing these makes UserConfigDir return an error, triggering the
	// configDir = "." fallback in applyDefaults.
	appdata := os.Getenv("APPDATA")
	home := os.Getenv("HOME")
	t.Setenv("APPDATA", "")
	t.Setenv("HOME", "")
	t.Cleanup(func() {
		os.Setenv("APPDATA", appdata)
		os.Setenv("HOME", home)
	})
	cfg, err := LoadBytes([]byte("{}\n"))
	require.NoError(t, err)
	assert.NotEmpty(t, cfg.Secrets.FilePath, "secrets file path must be set even when UserConfigDir fails")
	assert.Contains(t, cfg.Secrets.FilePath, "yanshi", "file path should contain app name")
}

func TestConfig_D3Defaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Secrets.Backend != "auto" || cfg.Secrets.FilePath == "" ||
		cfg.I18N.UILocale != "auto" || cfg.TUI.KeymapName != "default" ||
		cfg.TUI.Theme != "default" {
		t.Fatalf("D3 defaults not applied: %+v", cfg)
	}
}

// TestConfig_RejectsExpandedVarAsRawLiteral verifies that a ${VAR} reference
// in api_key is expanded by os.ExpandEnv BEFORE unmarshalling, and the
// resulting plaintext value is rejected unless auth.legacy_insecure is true.
// secret:// and env:// refs must always bypass this check (they are resolved
// by auth.Manager later in bootstrap).
func TestConfig_RejectsExpandedVarAsRawLiteral(t *testing.T) {
	cases := []struct {
		name         string
		configYAML   string
		env          map[string]string
		wantLoadFail bool
	}{
		{
			name: "expanded var is rejected without legacy opt-in",
			configYAML: `
llm:
  providers:
    - name: test
      kind: openai
      model: gpt-4
      api_key: ${MY_KEY}
auth:
  legacy_insecure: false
`,
			env:          map[string]string{"MY_KEY": "sk-raw-expanded"},
			wantLoadFail: true,
		},
		{
			name: "expanded var accepted when legacy_insecure=true",
			configYAML: `
llm:
  providers:
    - name: test
      kind: openai
      model: gpt-4
      api_key: ${MY_KEY}
auth:
  legacy_insecure: true
`,
			env:          map[string]string{"MY_KEY": "sk-raw-expanded"},
			wantLoadFail: false,
		},
		{
			name: "secret:// ref bypasses validation",
			configYAML: `
llm:
  providers:
    - name: test
      kind: openai
      model: gpt-4
      api_key: secret://openai/main
auth:
  legacy_insecure: false
`,
			wantLoadFail: false,
		},
		{
			name: "env:// ref bypasses validation",
			configYAML: `
llm:
  providers:
    - name: test
      kind: openai
      model: gpt-4
      api_key: env://MY_KEY
auth:
  legacy_insecure: false
`,
			wantLoadFail: false,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			for k, v := range c.env {
				t.Setenv(k, v)
			}
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(c.configYAML), 0600); err != nil {
				t.Fatal(err)
			}
			cfg, err := Load(path)
			if c.wantLoadFail {
				if err == nil {
					t.Fatalf("expected Load to reject expanded raw literal, got cfg=%+v", cfg)
				}
				if !strings.Contains(err.Error(), "raw literal") &&
					!strings.Contains(err.Error(), "api_key") &&
					!strings.Contains(err.Error(), "legacy_insecure") {
					t.Fatalf("error message does not mention the relevant constraint: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected Load failure: %v", err)
			}
		})
	}
}

// TestConfig_UnsetEnvVarBecomesEmpty verifies that a ${UNSET_VAR} reference
// expands to empty (os.ExpandEnv behavior), and empty api_key is always
// accepted (the provider simply has no credential configured).
func TestStorageDefaults_WAL(t *testing.T) {
	cfg, err := LoadBytes([]byte("storage:\n  sqlite_path: yanshi.db\n"))
	require.NoError(t, err)
	assert.Equal(t, 4, cfg.Storage.WALMaxOpenConns)
	assert.Equal(t, 5000, cfg.Storage.BusyTimeoutMs)
	assert.Equal(t, 1000, cfg.Storage.WALAutoCheckpoint)
}

func TestStorageWAL_ExplicitValues(t *testing.T) {
	cfg, err := LoadBytes([]byte(`
storage:
  sqlite_path: yanshi.db
  wal_max_open_conns: 1
  busy_timeout_ms: 3000
  wal_auto_checkpoint: 500
`))
	require.NoError(t, err)
	assert.Equal(t, 1, cfg.Storage.WALMaxOpenConns)
	assert.Equal(t, 3000, cfg.Storage.BusyTimeoutMs)
	assert.Equal(t, 500, cfg.Storage.WALAutoCheckpoint)
}

func TestStorageWAL_NegativeAutockptPassthrough(t *testing.T) {
	cfg, err := LoadBytes([]byte(`
storage:
  sqlite_path: yanshi.db
  wal_auto_checkpoint: -1
`))
	require.NoError(t, err)
	assert.Equal(t, -1, cfg.Storage.WALAutoCheckpoint, "negative should pass through as 'disable passive checkpoint'")
}

// TestMigrateConfig_NilCfg covers the nil check in MigrateConfig.
func TestMigrateConfig_NilCfg(t *testing.T) {
	err := MigrateConfig(nil, 0, 1)
	if err == nil {
		t.Fatal("expected error for nil cfg")
	}
}

// TestMigrateConfig_FromGtTo covers the from>to error in MigrateConfig.
func TestMigrateConfig_FromGtTo(t *testing.T) {
	err := MigrateConfig(&Config{}, 3, 1)
	if err == nil {
		t.Fatal("expected error for from>to")
	}
	if !strings.Contains(err.Error(), "cannot migrate") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestLoadBytes_InvalidYAML covers the yaml.Unmarshal error path in LoadBytes.
func TestLoadBytes_InvalidYAML(t *testing.T) {
	_, err := LoadBytes([]byte("{{  invalid "))
	if err == nil {
		t.Fatal("expected error for invalid YAML")
	}
}

// TestEllipsis_Truncation covers the len>32 branch of ellipsis.
func TestEllipsis_Truncation(t *testing.T) {
	s := "abcdefghijklmnopqrstuvwxyz0123456789" // 36 chars
	got := ellipsis(s)
	if len(got) != 32 {
		t.Fatalf("ellipsis(%q) = %q (len=%d), want 32", s, got, len(got))
	}
	// Verify the "first16...last13" structure.
	if !strings.HasPrefix(got, "abcdefghijklmnop") {
		t.Fatalf("ellipsis output unexpected prefix: %q", got)
	}
	if !strings.HasSuffix(got, "xyz0123456789") {
		t.Fatalf("ellipsis output unexpected suffix: %q", got)
	}
}

// TestEllipsis_Short does not truncate.
func TestEllipsis_Short(t *testing.T) {
	s := "short"
	got := ellipsis(s)
	if got != s {
		t.Fatalf("ellipsis(%q) = %q", s, got)
	}
}

// TestLoadBytes_OTelEnabledZeroSample covers OTel sample_ratio defaulting to 1
// when enabled but left at 0.
func TestLoadBytes_OTelEnabledZeroSample(t *testing.T) {
	cfg, err := LoadBytes([]byte(`
storage:
  sqlite_path: ":memory:"
observability:
  otel:
    enabled: true
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Observability.OTel.SampleRatio != 1 {
		t.Fatalf("expected default sample_ratio=1 when OTel enabled and unset, got %f",
			cfg.Observability.OTel.SampleRatio)
	}
}

// TestExpandHome_Empty covers expandHome with an empty string.
func TestExpandHome_Empty(t *testing.T) {
	got := expandHome("")
	if got != "" {
		t.Fatalf("expandHome('') = %q, want ''", got)
	}
}

// TestExpandHome_NoTilde covers expandHome with a non-~ path (passthrough).
func TestExpandHome_NoTilde(t *testing.T) {
	got := expandHome("plain/path/to/config")
	if got != "plain/path/to/config" {
		t.Fatalf("expandHome('plain/path/to/config') = %q, want passthrough", got)
	}
}

// TestExpandHome_Tilde covers the ~ expansion path (happy path).
func TestExpandHome_Tilde(t *testing.T) {
	got := expandHome("~/sub")
	if got == "~/sub" {
		t.Fatalf("expandHome('~/sub') = %q, expected ~ expansion", got)
	}
	if !strings.HasSuffix(got, "sub") {
		t.Fatalf("expandHome('~/sub') = %q, expected path ending in 'sub'", got)
	}
}

func TestConfig_UnsetEnvVarBecomesEmpty(t *testing.T) {
	t.Setenv("UNSET_VAR_EXISTS", "set")
	yaml := []byte(`
llm:
  providers:
    - name: test
      kind: openai
      model: gpt-4
      api_key: ${DOES_NOT_EXIST}
auth:
  legacy_insecure: false
`)
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, yaml, 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("empty after expansion should be accepted: %v", err)
	}
	if cfg.LLM.Providers[0].APIKey != "" {
		t.Fatalf("unset env var should expand to empty, got %q", cfg.LLM.Providers[0].APIKey)
	}
}

func TestProviderConfigMultimodalParses(t *testing.T) {
	yaml := `
llm:
  providers:
    - name: deepseek
      kind: openai
      model: deepseek-chat
      multimodal: false
    - name: claude
      kind: anthropic
      model: claude-opus-4-8
      multimodal: true
`
	cfg, err := LoadBytes([]byte(yaml))
	if err != nil {
		t.Fatalf("LoadBytes: %v", err)
	}
	if len(cfg.LLM.Providers) != 2 {
		t.Fatalf("providers = %d", len(cfg.LLM.Providers))
	}
	if cfg.LLM.Providers[0].Multimodal {
		t.Fatalf("provider 0 should be non-multimodal, got %#v", cfg.LLM.Providers[0])
	}
	if !cfg.LLM.Providers[1].Multimodal {
		t.Fatalf("provider 1 should be multimodal, got %#v", cfg.LLM.Providers[1])
	}
}

func TestProviderConfigMultimodalDefaultsFalse(t *testing.T) {
	yaml := `
llm:
  providers:
    - name: text-only
      model: gpt-3.5
`
	cfg, err := LoadBytes([]byte(yaml))
	if err != nil {
		t.Fatalf("LoadBytes: %v", err)
	}
	if cfg.LLM.Providers[0].Multimodal {
		t.Fatal("omitted multimodal must default to false (text-only)")
	}
}

// TestLoadBytesRejectsUnknownShellPolicy pins the startup rejection of a
// profile whose shell.policy the guard cannot enforce.
//
// The value under test, "allow", is not arbitrary: it is what
// docs/user-guide/guard.md advertised, so an operator following the docs wrote
// a profile whose shell dimension was permanently dead. checkShell's default
// branch is a structural HardDeny, which no permission mode (yolo and auto
// included) can override, and nothing read the field until the first
// shell_run — so the config started clean and failed mid-session with no
// remedy available from inside the running process.
//
// The error has to name the offending profile, because Profiles is a bare map
// and an operator with several profiles otherwise cannot tell which one to fix.
func TestLoadBytesRejectsUnknownShellPolicy(t *testing.T) {
	const bad = `
profiles:
  coding:
    tools: { allow: ["*"] }
    shell: { policy: "allow" }
`
	_, err := LoadBytes([]byte(bad))
	require.Error(t, err, "an unenforceable shell policy must not load")
	require.Contains(t, err.Error(), "profiles.coding.shell.policy")
	require.Contains(t, err.Error(), `"allow"`)
	require.Contains(t, err.Error(), "allowlist",
		"the error must list the legal values, not just reject")
}

// TestLoadBytesAcceptsEveryEnforceableShellPolicy is the negative probe for
// TestLoadBytesRejectsUnknownShellPolicy: a gate that rejects a legal policy
// would be worse than no gate, because it would refuse configs that work.
// Driving guard.ShellPolicies() rather than a local copy means a future policy
// gains coverage here without anyone remembering to add it.
func TestLoadBytesAcceptsEveryEnforceableShellPolicy(t *testing.T) {
	for _, policy := range guard.ShellPolicies() {
		src := fmt.Sprintf("profiles:\n  coding:\n    shell: { policy: %q }\n", policy)
		cfg, err := LoadBytes([]byte(src))
		require.NoError(t, err, "policy %q is enforceable and must load", policy)
		require.Equal(t, policy, cfg.Profiles["coding"].Shell.Policy)
	}
	// A profile block with no shell key at all is the most common shape and
	// must keep loading: the zero value is "" and "" is allowlist.
	cfg, err := LoadBytes([]byte("profiles:\n  coding:\n    tools: { allow: [\"*\"] }\n"))
	require.NoError(t, err)
	require.Empty(t, cfg.Profiles["coding"].Shell.Policy)
}
