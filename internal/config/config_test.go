package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

// TestValidate_AutoCompactThresholdAboveOneIsRejected pins F-3: an operator
// who writes auto_compact_threshold as an absolute token count instead of a
// window fraction (ADR-0024 C3) — the failure scenario the finding reproduced
// was auto_compact_threshold: 8000 — must have the mistake refused at load
// time, not published silently into a threshold that can never fire.
func TestValidate_AutoCompactThresholdAboveOneIsRejected(t *testing.T) {
	yaml := []byte(`
llm:
  providers:
    - name: broken
      kind: anthropic
      model: claude-opus
      auto_compact_threshold: 8000
`)
	_, err := LoadBytes(yaml)
	require.Error(t, err, "a threshold > 1 (an absolute token count, not a window fraction) must be rejected at load time")
	assert.Contains(t, err.Error(), "auto_compact_threshold")
}

// TestValidate_AutoCompactThresholdBoundaryAndNegativeAreAccepted pins the
// two values validateProviderThresholds must NOT reject: exactly 1.0 (the
// inclusive upper bound of a fraction) and a negative value (F-10/W-C-04's
// explicit per-provider disable sentinel, not a malformed ratio).
func TestValidate_AutoCompactThresholdBoundaryAndNegativeAreAccepted(t *testing.T) {
	yaml := []byte(`
llm:
  providers:
    - name: at-the-boundary
      kind: anthropic
      model: claude-opus
      auto_compact_threshold: 1
    - name: explicitly-disabled
      kind: anthropic
      model: claude-haiku
      auto_compact_threshold: -1
`)
	cfg, err := LoadBytes(yaml)
	require.NoError(t, err)
	require.Len(t, cfg.LLM.Providers, 2)
	assert.Equal(t, 1.0, cfg.LLM.Providers[0].AutoCompactThreshold)
	assert.Equal(t, -1.0, cfg.LLM.Providers[1].AutoCompactThreshold)
}

// TestValidate_NegativeContextWindowIsRejected pins review-whole.md I-1: a
// negative context_window used to be silently indistinguishable from unset
// (ResolveContextWindow/ContextWindowFor both judge on `> 0`), the one
// dimension out of four adjacent provider-config zero/negative/invalid
// readings the review found with no diagnostic at all. It must now be
// refused at load time, the same way max_retries < 0 already is.
func TestValidate_NegativeContextWindowIsRejected(t *testing.T) {
	yaml := []byte(`
llm:
  providers:
    - name: broken
      kind: anthropic
      model: claude-opus
      context_window: -1
`)
	_, err := LoadBytes(yaml)
	require.Error(t, err, "a negative context_window must be rejected at load time, not silently fall through to the catalog/128K default")
	assert.Contains(t, err.Error(), "context_window")
}

// TestValidate_ZeroContextWindowIsAccepted pins the value
// validateProviderThresholds must NOT reject: 0 (unset, falls through to
// CompactionConfig.ContextWindow via ContextWindowFor) is a legitimate,
// intentional reading — only negative is a malformed value with no meaning.
func TestValidate_ZeroContextWindowIsAccepted(t *testing.T) {
	yaml := []byte(`
llm:
  providers:
    - name: unset
      kind: anthropic
      model: claude-opus
`)
	cfg, err := LoadBytes(yaml)
	require.NoError(t, err)
	require.Len(t, cfg.LLM.Providers, 1)
	assert.Equal(t, 0, cfg.LLM.Providers[0].ContextWindow)
}

func TestLoadBytesDefaultsSubagents(t *testing.T) {
	yaml := []byte("{}")
	cfg, err := LoadBytes(yaml)
	require.NoError(t, err)
	require.Equal(t, 10, cfg.Subagents.Limit)
	require.NotEmpty(t, cfg.Subagents.PersistencePath)
}

// TestLoadObservabilityFeaturesAndPricing.
//
// ledger: C4/COST1#3 价格可配
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

// TestConfig_AcceptsLiteralAndExpandedVarAPIKeys pins the two accepted
// api_key shapes. ${VAR} is expanded by os.ExpandEnv BEFORE unmarshalling, so
// by the time a Config exists both shapes are indistinguishable plaintext —
// and that is the point: Load has no opinion about where the value came from.
//
// The last two cases are the regression guard. secret:// and env:// used to
// be resolved refs; they are now ordinary strings that Load must pass through
// untouched rather than reject or interpret, so a stale config keeps loading
// and simply presents whatever it says to the provider.
func TestConfig_AcceptsLiteralAndExpandedVarAPIKeys(t *testing.T) {
	cases := []struct {
		name       string
		configYAML string
		env        map[string]string
		wantKey    string
	}{
		{
			name: "literal",
			configYAML: `
llm:
  providers:
    - name: test
      kind: openai
      model: gpt-4
      api_key: sk-plain-literal
`,
			wantKey: "sk-plain-literal",
		},
		{
			name: "expanded var",
			configYAML: `
llm:
  providers:
    - name: test
      kind: openai
      model: gpt-4
      api_key: ${MY_KEY}
`,
			env:     map[string]string{"MY_KEY": "sk-raw-expanded"},
			wantKey: "sk-raw-expanded",
		},
		{
			name: "secret:// is not special-cased any more",
			configYAML: `
llm:
  providers:
    - name: test
      kind: openai
      model: gpt-4
      api_key: secret://openai/main
`,
			wantKey: "secret://openai/main",
		},
		{
			name: "env:// is not special-cased any more",
			configYAML: `
llm:
  providers:
    - name: test
      kind: openai
      model: gpt-4
      api_key: env://MY_KEY
`,
			wantKey: "env://MY_KEY",
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
			if err != nil {
				t.Fatalf("Load must accept any api_key value: %v", err)
			}
			if got := cfg.LLM.Providers[0].APIKey; got != c.wantKey {
				t.Fatalf("api_key = %q, want %q", got, c.wantKey)
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

// TestLoadBytesAllowsInertShellPolicyBesideRules pins the scope of
// validateProfiles: a profile that carries an execpolicy rules table keeps
// loading even when shell.policy is a value the guard cannot enforce.
//
// This is not leniency for its own sake. The test drives the real guard on the
// same profile first and asserts the command is ALLOWED — that is the proof
// that the config is a working deployment, not an already-dead one. checkShell
// returns from inside the `len(Shell.Rules) > 0` branch on every path, so the
// policy switch never runs and the bad value never denies anything. Rejecting
// this shape at startup would be a behavioural regression: the operator is
// told by docs/user-guide/guard.md that `policy` is not read when `rules` is
// present, and would then be refused a boot over exactly that field.
//
// The second half is the other side of the scope: emptying the rules table
// makes the same policy load-bearing, and the check fires on that reload. The
// deferred typo is caught at the moment it stops being inert, not never.
func TestLoadBytesAllowsInertShellPolicyBesideRules(t *testing.T) {
	const withRules = `
profiles:
  coding:
    tools: { allow: ["*"] }
    net: { allow: true }
    shell:
      policy: "allow"
      rules:
        - id: go-test
          program: go
          prefix: ["test"]
          decision: allow
          justification: "tests are safe"
`
	cfg, err := LoadBytes([]byte(withRules))
	require.NoError(t, err, "an inert shell.policy beside a rules table must still load")

	d := guard.New().Check(cfg.Profiles["coding"], guard.Action{Tool: "shell_run", Shell: "go test"})
	require.Equal(t, guard.Allow, d.Verdict,
		"the rules table decides; the unenforceable policy must be unreachable")
	require.Equal(t, "go-test", d.RuleID)

	// Same profile, rules removed: the policy is now the live code path.
	const withoutRules = `
profiles:
  coding:
    tools: { allow: ["*"] }
    net: { allow: true }
    shell:
      policy: "allow"
`
	_, err = LoadBytes([]byte(withoutRules))
	require.Error(t, err, "with rules emptied the policy is live and must be rejected")
	require.Contains(t, err.Error(), "profiles.coding.shell.policy")
}

// TestLoadBytesReadsGoalBlock pins the YAML keys of the goal: block.
//
// Everything else about the budget is tested against a hand-built GoalConfig,
// which cannot catch a wrong or renamed yaml tag: the struct still compiles,
// LoadBytes still succeeds, and the block in config.example.yaml silently
// deserializes to zero — indistinguishable from an operator who set no limit.
// Measured before this test existed: renaming max_tokens to maxTokens left the
// entire suite green.
//
// The keys asserted here are the ones config.example.yaml documents; changing
// either is a breaking change to a published config surface, not a rename.
func TestLoadBytesReadsGoalBlock(t *testing.T) {
	cfg, err := LoadBytes([]byte(`
llm:
  providers: []
goal:
  max_tokens: 50000
  max_iterations: 9
`))
	if err != nil {
		t.Fatalf("LoadBytes: %v", err)
	}
	if cfg.Goal.MaxTokens != 50000 {
		t.Errorf("Goal.MaxTokens = %d; want 50000 (is the yaml key still max_tokens?)", cfg.Goal.MaxTokens)
	}
	if cfg.Goal.MaxIterations != 9 {
		t.Errorf("Goal.MaxIterations = %d; want 9 (is the yaml key still max_iterations?)", cfg.Goal.MaxIterations)
	}
}

// TestLoadBytesWithoutGoalBlockIsUnlimited pins the compatibility default: a
// config that predates the block must keep running unbounded rather than
// picking up a limit nobody asked for.
func TestLoadBytesWithoutGoalBlockIsUnlimited(t *testing.T) {
	cfg, err := LoadBytes([]byte("llm:\n  providers: []\n"))
	if err != nil {
		t.Fatalf("LoadBytes: %v", err)
	}
	if cfg.Goal.MaxTokens != 0 || cfg.Goal.MaxIterations != 0 {
		t.Errorf("Goal = %+v; an absent block must mean unlimited", cfg.Goal)
	}
}

// TestLoad_CompactionDefaultsAreDeliberate pins every compaction default that
// changes behaviour when it moves.
//
// Only chunk_threshold had an assertion, and that is the one key nothing reads
// (all three call sites hardcode 0.9 -- see docs/compaction.md). The defaults
// that do decide when and how much gets compacted had none: setting
// cooldown_fraction's default to 0 disables the token cooldown for every
// deployment that never wrote the key, restoring the repeated-compaction bug
// W4 fixed, and measured green across config and bootstrap.
//
// These are not tautologies. Each number is a judgement -- 0.8 for when to
// start, 0.95 for when to stop waiting, 0.05 for how much growth is worth a
// second pass -- and an operator who omits the key is choosing to trust it.
func TestLoad_CompactionDefaultsAreDeliberate(t *testing.T) {
	tmp := t.TempDir() + "/c.yaml"
	// A config that mentions compaction but sets nothing inside it, so every
	// value below comes from applyDefaults rather than the file.
	require.NoError(t, os.WriteFile(tmp, []byte("compaction:\n  model: \"\"\n"), 0o644))
	cfg, err := Load(tmp)
	require.NoError(t, err)

	assert.Equal(t, 0.8, cfg.Compaction.Threshold,
		"threshold: when compaction starts")
	assert.Equal(t, 4, cfg.Compaction.KeepRecent,
		"keep_recent: how much tail survives verbatim")
	assert.Equal(t, 256000, cfg.Compaction.ContextWindow,
		"context_window: the fallback for providers that declare none")
	assert.Equal(t, 0.9, cfg.Compaction.ChunkThreshold,
		"chunk_threshold: currently inert, asserted so a future wiring starts from 0.9")
	assert.Equal(t, 0.05, cfg.Compaction.CooldownFraction,
		"cooldown_fraction: zero here silently disables the token cooldown")
	assert.Equal(t, 0.95, cfg.Compaction.HardForceFraction,
		"hard_force_fraction: zero here means nothing overrides the cooldown near the window edge")
}

// TestExampleConfigExecPolicyRulesParse pins that the commented-out rules
// block in config.example.yaml is real YAML for the real struct.
//
// The block exists because execpolicy was unreachable from the factory
// config: rules had zero mentions, so an operator had no way to discover the
// capability. A commented example is only useful if uncommenting it works,
// and nothing else checks that -- the config loader never sees comments, and
// gendocs reflects the struct rather than the file.
//
// Extracts the block by stripping its comment markers and feeding it to the
// same loader an operator would, so a renamed field or a changed shape fails
// here rather than in their terminal.
func TestExampleConfigExecPolicyRulesParse(t *testing.T) {
	raw, err := os.ReadFile("../../config.example.yaml")
	if err != nil {
		t.Fatalf("read example config: %v", err)
	}

	var block []string
	collecting := false
	for _, line := range strings.Split(string(raw), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "# rules:") {
			collecting = true
		}
		if !collecting {
			continue
		}
		if !strings.HasPrefix(trimmed, "#") {
			break
		}
		body := strings.TrimPrefix(trimmed, "#")
		if strings.HasPrefix(strings.TrimSpace(body), "#") {
			continue // an explanatory comment inside the block
		}
		if strings.TrimSpace(body) == "" {
			break
		}
		block = append(block, strings.TrimPrefix(body, " "))
	}
	if len(block) == 0 {
		t.Fatal("config.example.yaml no longer shows a rules: example, so operators " +
			"have no way to discover execpolicy from the factory config")
	}

	tmp := t.TempDir() + "/c.yaml"
	doc := "profiles:\n  demo:\n    shell:\n" + indentBlock(block, "      ")
	require.NoError(t, os.WriteFile(tmp, []byte(doc), 0o644))

	cfg, err := Load(tmp)
	require.NoError(t, err, "the example rules block must load as written")
	rules := cfg.Profiles["demo"].Shell.Rules
	require.NotEmpty(t, rules, "the example produced no rules")

	var sawDeny, sawAllow bool
	for _, r := range rules {
		require.NotEmpty(t, r.ID, "every example rule needs an id")
		require.NotEmpty(t, r.Justification, "every example rule needs a justification")
		switch r.Decision {
		case "deny":
			sawDeny = true
			require.NotEmpty(t, r.DenyFlags, "the deny example must show deny_flags, its whole point")
		case "allow":
			sawAllow = true
		}
	}
	require.True(t, sawDeny && sawAllow,
		"the example must show both decisions: a deny alone reads as a blanket ban")
}

// indentBlock re-indents an extracted YAML block under a parent key.
func indentBlock(lines []string, prefix string) string {
	var b strings.Builder
	for _, l := range lines {
		b.WriteString(prefix + l + "\n")
	}
	return b.String()
}

// TestProviderGenerationParams_DistinguishUnsetFromZero is the M4 config-layer
// assertion. All three params are pointers precisely so an explicit 0 stays
// distinguishable from an omitted key — 0 is a value operators genuinely want
// (temperature 0 for a deterministic judge call, top_p 0 for greedy decoding),
// and a value type would collapse the two into "provider default".
func TestProviderGenerationParams_DistinguishUnsetFromZero(t *testing.T) {
	cfg, err := LoadBytes([]byte(`
llm:
  providers:
    - name: "tuned"
      model: "gpt-4o"
      max_tokens: 32000
      temperature: 0.2
      top_p: 0.95
    - name: "deterministic"
      model: "judge"
      temperature: 0
      top_p: 0
      max_tokens: 0
    - name: "untouched"
      model: "default-model"
`))
	require.NoError(t, err)
	require.Len(t, cfg.LLM.Providers, 3)

	tuned := cfg.LLM.Providers[0]
	require.NotNil(t, tuned.MaxTokens)
	assert.Equal(t, 32000, *tuned.MaxTokens)
	require.NotNil(t, tuned.Temperature)
	assert.InDelta(t, 0.2, *tuned.Temperature, 1e-6)
	require.NotNil(t, tuned.TopP)
	assert.InDelta(t, 0.95, *tuned.TopP, 1e-6)

	det := cfg.LLM.Providers[1]
	require.NotNil(t, det.Temperature, "an explicit temperature: 0 must not read as unset")
	assert.InDelta(t, 0, *det.Temperature, 1e-6)
	require.NotNil(t, det.TopP, "an explicit top_p: 0 must not read as unset")
	assert.InDelta(t, 0, *det.TopP, 1e-6)
	require.NotNil(t, det.MaxTokens)
	assert.Equal(t, 0, *det.MaxTokens)

	untouched := cfg.LLM.Providers[2]
	assert.Nil(t, untouched.MaxTokens, "an omitted key must stay nil so the adapter default applies")
	assert.Nil(t, untouched.Temperature)
	assert.Nil(t, untouched.TopP)
}

// TestProviderLocalFlag_DistinguishesUnsetFromFalse covers the M3 override.
// The three states are all meaningful: unset lets the base_url/kind heuristic
// decide, true forces local for a runtime behind an unrecognisable hostname,
// and false opts a LAN-addressed gateway back into the cloud catalog. A plain
// bool would erase the last one.
func TestProviderLocalFlag_DistinguishesUnsetFromFalse(t *testing.T) {
	cfg, err := LoadBytes([]byte(`
llm:
  providers:
    - name: "heuristic"
      model: "a"
    - name: "forced-local"
      model: "b"
      local: true
    - name: "forced-cloud"
      model: "c"
      local: false
`))
	require.NoError(t, err)
	require.Len(t, cfg.LLM.Providers, 3)

	assert.Nil(t, cfg.LLM.Providers[0].Local, "omitted local must leave the heuristic in charge")
	require.NotNil(t, cfg.LLM.Providers[1].Local)
	assert.True(t, *cfg.LLM.Providers[1].Local)
	require.NotNil(t, cfg.LLM.Providers[2].Local, "an explicit local: false must not read as unset")
	assert.False(t, *cfg.LLM.Providers[2].Local)
}

// TestProviderConfig_HeadersExpandEnvVars pins the W-C-02 acceptance clause
// "${VAR} 展开生效" at the config layer: LoadBytes runs os.ExpandEnv over the
// whole document before unmarshal, so a header value written as ${VAR} must
// come out expanded — the same mechanism APIKey already relies on
// (TestConfig_UnsetEnvVarBecomesEmpty), exercised here for the new field.
func TestProviderConfig_HeadersExpandEnvVars(t *testing.T) {
	t.Setenv("W_C_02_GATEWAY_TOKEN", "canary-secret-value")
	cfg, err := LoadBytes([]byte(`
llm:
  providers:
    - name: "azure"
      kind: openai
      model: "gpt-4o"
      headers:
        api-key: ${W_C_02_GATEWAY_TOKEN}
        x-static: "plain-value"
`))
	require.NoError(t, err)
	require.Len(t, cfg.LLM.Providers, 1)
	assert.Equal(t, "canary-secret-value", cfg.LLM.Providers[0].Headers["api-key"])
	assert.Equal(t, "plain-value", cfg.LLM.Providers[0].Headers["x-static"])
}

// TestProviderConfig_MaxRetriesDistinguishesUnsetFromZero pins the W-C-07
// nil-means-omit convention (same shape as MaxTokens/Temperature/TopP, M4):
// an explicit 0 (never retry) must not collapse into "not configured".
func TestProviderConfig_MaxRetriesDistinguishesUnsetFromZero(t *testing.T) {
	cfg, err := LoadBytes([]byte(`
llm:
  max_retries: 4
  providers:
    - name: "capped"
      model: "a"
      max_retries: 1
    - name: "never-retry"
      model: "b"
      max_retries: 0
    - name: "inherits-global"
      model: "c"
`))
	require.NoError(t, err)
	require.Len(t, cfg.LLM.Providers, 3)
	assert.Equal(t, 4, cfg.LLM.MaxRetries)
	require.NotNil(t, cfg.LLM.Providers[0].MaxRetries)
	assert.Equal(t, 1, *cfg.LLM.Providers[0].MaxRetries)
	require.NotNil(t, cfg.LLM.Providers[1].MaxRetries, "an explicit max_retries: 0 must not read as unset")
	assert.Equal(t, 0, *cfg.LLM.Providers[1].MaxRetries)
	assert.Nil(t, cfg.LLM.Providers[2].MaxRetries, "an omitted key must stay nil so the global value applies")
}

// TestValidate_NegativeMaxRetriesRejected pins validateProviderRetriesAndAuth's
// negative-ceiling checks, both the global and the per-provider override.
func TestValidate_NegativeMaxRetriesRejected(t *testing.T) {
	_, err := LoadBytes([]byte("llm:\n  max_retries: -1\n"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "llm.max_retries")

	_, err = LoadBytes([]byte(`
llm:
  providers:
    - name: "broken"
      model: "a"
      max_retries: -2
`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "max_retries")
}

// TestProviderConfig_AuthCommandDefaultsRefreshInterval pins the W-C-12
// applyDefaults clause: an Auth block with no explicit refresh_interval gets
// 15 minutes, and Command's argv round-trips unchanged.
func TestProviderConfig_AuthCommandDefaultsRefreshInterval(t *testing.T) {
	cfg, err := LoadBytes([]byte(`
llm:
  providers:
    - name: "sts"
      model: "a"
      auth:
        command: ["aws", "sts", "get-session-token"]
`))
	require.NoError(t, err)
	require.Len(t, cfg.LLM.Providers, 1)
	require.NotNil(t, cfg.LLM.Providers[0].Auth)
	assert.Equal(t, []string{"aws", "sts", "get-session-token"}, cfg.LLM.Providers[0].Auth.Command)
	assert.Equal(t, 15*time.Minute, cfg.LLM.Providers[0].Auth.RefreshInterval)
}

// TestProviderConfig_AuthCommandExplicitRefreshIntervalPreserved proves a
// non-zero refresh_interval is left alone by applyDefaults (only the zero
// value gets the 15m default).
func TestProviderConfig_AuthCommandExplicitRefreshIntervalPreserved(t *testing.T) {
	cfg, err := LoadBytes([]byte(`
llm:
  providers:
    - name: "sts"
      model: "a"
      auth:
        command: ["aws", "sts", "get-session-token"]
        refresh_interval: 5m
`))
	require.NoError(t, err)
	assert.Equal(t, 5*time.Minute, cfg.LLM.Providers[0].Auth.RefreshInterval)
}

// TestValidate_AuthCommandEmptyRejected and
// TestValidate_AuthCommandNegativeRefreshIntervalRejected pin the W-C-12
// SECURITY-adjacent config-error clauses: an auth block that could never
// produce a credential must fail at boot, not at first request.
func TestValidate_AuthCommandEmptyRejected(t *testing.T) {
	_, err := LoadBytes([]byte(`
llm:
  providers:
    - name: "broken"
      model: "a"
      auth:
        command: []
`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "auth.command")
}

func TestValidate_AuthCommandNegativeRefreshIntervalRejected(t *testing.T) {
	_, err := LoadBytes([]byte(`
llm:
  providers:
    - name: "broken"
      model: "a"
      auth:
        command: ["echo", "token"]
        refresh_interval: -1m
`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "refresh_interval")
}

// W-F-12: per-server tool allow/deny parse from YAML and land on the config
// struct, from where buildMCPManager projects them onto the mcp.ServerConfig
// the Manager enforces. Absent fields stay nil — every config predating the
// fields keeps meaning "no registration-side filter".
func TestLoad_MCPToolAllowDeny(t *testing.T) {
	cfg, err := LoadBytes([]byte(`
mcp:
  servers:
    github:
      enabled: true
      transport: http
      url: "https://mcp.example.com"
      tool_allow: ["search_*", "get_file"]
      tool_deny: ["search_web"]
    plain:
      enabled: true
      transport: http
      url: "https://mcp2.example.com"
`))
	require.NoError(t, err)
	gh := cfg.MCP.Servers["github"]
	require.NotNil(t, gh)
	assert.Equal(t, []string{"search_*", "get_file"}, gh.ToolAllow)
	assert.Equal(t, []string{"search_web"}, gh.ToolDeny)
	plain := cfg.MCP.Servers["plain"]
	require.NotNil(t, plain)
	assert.Empty(t, plain.ToolAllow)
	assert.Empty(t, plain.ToolDeny)
}

// TestWFS27_WebSearchConfigPinsTheConfigSeam pins the tools.web_search block
// end to end at the config layer: the default is the empty pair (tools package
// falls back to duckduckgo), a named backend reaches the struct, searxng
// without an endpoint is rejected at load (the operator is looking), an
// unknown backend is rejected with the known names, and a non-URL endpoint is
// rejected rather than failing on every search forever.
func TestWFS27_WebSearchConfigPinsTheConfigSeam(t *testing.T) {
	t.Run("default keeps built-in backend", func(t *testing.T) {
		cfg, err := LoadBytes([]byte("server:\n  addr: \":0\"\n"))
		require.NoError(t, err)
		require.Equal(t, WebSearchConfig{}, cfg.Tools.WebSearch)
	})

	t.Run("searxng with endpoint loads", func(t *testing.T) {
		cfg, err := LoadBytes([]byte(
			"tools:\n  web_search:\n    backend: searxng\n    endpoint: https://searx.internal/search\n"))
		require.NoError(t, err)
		require.Equal(t, "searxng", cfg.Tools.WebSearch.Backend)
		require.Equal(t, "https://searx.internal/search", cfg.Tools.WebSearch.Endpoint)
	})

	t.Run("searxng without endpoint is rejected", func(t *testing.T) {
		_, err := LoadBytes([]byte("tools:\n  web_search:\n    backend: searxng\n"))
		require.Error(t, err)
		require.Contains(t, err.Error(), "endpoint")
	})

	t.Run("unknown backend is rejected naming the known ones", func(t *testing.T) {
		_, err := LoadBytes([]byte("tools:\n  web_search:\n    backend: startpage\n"))
		require.Error(t, err)
		require.Contains(t, err.Error(), "duckduckgo")
	})

	t.Run("non-URL endpoint is rejected", func(t *testing.T) {
		_, err := LoadBytes([]byte(
			"tools:\n  web_search:\n    backend: duckduckgo\n    endpoint: not a url\n"))
		require.Error(t, err)
	})
}
