// Package config loads yanshi's YAML configuration.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/x6nux/yanshi/internal/guard"
	"gopkg.in/yaml.v3"
)

// BatchConfig is the C1 batch and automation configuration block.
type BatchConfig struct {
	RLMModel          string `yaml:"rlm_model"`
	RLMMaxConcurrency int    `yaml:"rlm_max_concurrency"`
	AutomationTickSec int    `yaml:"automation_tick_seconds"`
}

// MCPConfig holds the MCP server configurations. An empty or absent block
// creates a disabled Manager.
type MCPConfig struct {
	Servers map[string]*MCPServerConfig `yaml:"servers"`
}

// MCPServerConfig configures a single MCP server connection.
type MCPServerConfig struct {
	Enabled   bool              `yaml:"enabled"`
	Transport string            `yaml:"transport"`
	Command   string            `yaml:"command,omitempty"`
	Args      []string          `yaml:"args,omitempty"`
	Env       map[string]string `yaml:"env,omitempty"`
	URL       string            `yaml:"url,omitempty"`
	Bearer    string            `yaml:"bearer,omitempty"`
	Timeout   string            `yaml:"timeout,omitempty"`
	Reconnect bool              `yaml:"reconnect,omitempty"`
}

// Config is the top-level yanshi configuration. Zero values are post-processed
// by applyDefaults to ensure safe defaults.
type Config struct {
	// SchemaVersion is the config schema generation. Omitted (zero) on older
	// configs is normalized to SupportedSchemaVersion by Load — A–D config
	// evolution was purely additive, so a missing field is forward-compatible.
	// A value above SupportedSchemaVersion is rejected (the user must upgrade
	// yanshi). Bump SupportedSchemaVersion on the first destructive change and
	// add a case to MigrateConfig.
	SchemaVersion int                                `yaml:"schema_version"`
	Server        ServerConfig                       `yaml:"server"`
	Storage       StorageConfig                      `yaml:"storage"`
	Token         string                             `yaml:"token"`
	LLM           LLMConfig                          `yaml:"llm"`
	Agents        []AgentConfig                      `yaml:"agents"`
	Profiles      map[string]guard.PermissionProfile `yaml:"profiles"`
	Skills        SkillsConfig                       `yaml:"skills"`
	VCS           VCSConfig                          `yaml:"vcs"`
	Batch         BatchConfig                        `yaml:"batch"`
	// Compaction configures automatic context-compaction (Task 35b): when the
	// estimated token count of the conversation history reaches
	// Threshold*ContextWindow, the older turns are summarized by a remote model
	// (optionally a dedicated fast Compaction.Model), streaming the summary back
	// in real time. Defaults are applied in Load.
	Compaction CompactionConfig `yaml:"compaction"`
	// Memory configures MEM1 user memory (cross-session preference notes
	// injected into the system prompt as an independent suffix). All fields
	// optional; Enabled=false (default) makes bootstrap skip the subsystem.
	Memory MemoryConfig `yaml:"memory"`
	// Security configures yanshi's security posture (Task 10+): sandbox tier /
	// enable flag, network policy defaults, shell runtime limits. The zero
	// value is post-processed by applyDefaults so an unset block behaves like
	// an opt-in (sandbox on, default-deny network, conservative shell limits)
	// rather than a permissive free-for-all.
	Security  SecurityConfig  `yaml:"security"`
	Subagents SubagentsConfig `yaml:"subagents"`
	// Goal configures the self-driven goal loop's budget. Both limits default
	// to 0 = unlimited, and the default is load-bearing: a non-zero default
	// would silently start cutting off runs that work today, so the flags and
	// this block can only ever tighten a limit the operator asked for.
	Goal GoalConfig `yaml:"goal"`
	LSP  LSPConfig  `yaml:"lsp"`
	MCP  MCPConfig  `yaml:"mcp"`
	// Observability configures structured logging (OBS1) and OpenTelemetry
	// trace/metric export (OBS2). Zero values are post-processed by
	// applyDefaults so omitting the block yields safe defaults (info/json,
	// service "yanshi", OTel disabled).
	Observability ObservabilityConfig `yaml:"observability"`
	// Features configures the feature-flag registry (OBS3): Strict treats
	// unknown flags as errors; Overrides seeds runtime values.
	Features FeaturesConfig `yaml:"features"`
	// Pricing configures per-model USD cost overrides (COST1). Overrides map
	// model name → ModelPricingOverride; bootstrap converts these into
	// einollm.ModelPricing entries on the price table.
	Pricing PricingConfig `yaml:"pricing"`
	// Secrets configures the S10 credential storage backend (auto / keyring /
	// file / none). Defaults are post-processed by applyDefaults so the zero
	// value behaves like "auto" with the standard user-config-dir path.
	Secrets SecretsConfig `yaml:"secrets"`
	// Auth configures O03 provider-neutral authentication: legacy insecure
	// raw literals, RFC 8628 device authorization providers, and the SQLite
	// metadata toggle. Zero value is fully disabled (no device auth, no
	// legacy opt-in).
	Auth AuthConfig `yaml:"auth"`
	// I18N configures I18N1 locales: UILocale picks the TUI/catalog language
	// ("auto" re-resolves from LC_ALL/LANG at every startup), OutputLanguage
	// independently steers the model's response language.
	I18N I18NConfig `yaml:"i18n"`
	// TUI carries C15 keymap / Vim / theme preferences. See TUIConfig for the
	// wiring status — these are read by `yanshi doctor` only, not by the TUI.
	TUI TUIConfig `yaml:"tui"`
}

// SecretsConfig configures the credential storage backend. Backend is "auto"
// (default; prefer OS keyring, fall back to encrypted file if passphrase
// available), "keyring" (force OS keyring, fail if unavailable), "file"
// (force encrypted file), or "none" (no secret storage; secret:// refs fail
// to resolve). FilePath defaults to os.UserConfigDir()/yanshi/secrets.enc.
// PassphraseEnv is the env var name holding the master passphrase; if unset
// and backend is "auto", fileStore is skipped (text secrets still resolve).
type SecretsConfig struct {
	Backend       string `yaml:"backend"`
	FilePath      string `yaml:"file_path"`
	PassphraseEnv string `yaml:"passphrase_env"`
}

// AuthConfig configures provider-neutral authentication. Device.ClientID is
// used for RFC 8628 device authorization flows.
type AuthConfig struct {
	// LegacyInsecure accepts raw literal API keys in config.yaml.
	// This bypasses S10's fail-closed threat model — only enable temporarily
	// during migration.
	LegacyInsecure bool `yaml:"legacy_insecure"`

	// AutoMigrate automatically stores raw literal API keys into the secrets
	// backend and rewrites config.yaml with secret:// references. It does NOT
	// re-encrypt existing secret:// or env:// references — only raw literals
	// (sk-..., gsk_..., etc.) that would otherwise be rejected by
	// validateAPIKeyRefs. The rewrite is best-effort: if config.yaml is
	// unwritable the boot proceeds with the ref in memory but prints a warning.
	AutoMigrate bool `yaml:"auto_migrate"`

	Device struct {
		ClientID          string                 `yaml:"client_id"`
		DeviceAuthEnabled bool                   `yaml:"device_auth_enabled"`
		Providers         []DeviceProviderConfig `yaml:"providers"`
	} `yaml:"device"`
}

// DeviceProviderConfig is one RFC 8628 provider declaration. Endpoints are
// validated HTTPS-only at load/build time; loopback HTTP is accepted only for
// deterministic httptest acceptance.
type DeviceProviderConfig struct {
	ID        string   `yaml:"id"`
	ClientID  string   `yaml:"client_id"`
	DeviceURL string   `yaml:"device_url"`
	TokenURL  string   `yaml:"token_url"`
	Scopes    []string `yaml:"scopes"`
}

// I18NConfig configures localization. UILocale is one of "auto" (default;
// re-resolved at every startup from LC_ALL/LANG), "en", or "zh-Hans".
// OutputLanguage independently controls the model's response language; empty
// means "follow user input language" (no system-prompt directive).
type I18NConfig struct {
	UILocale       string `yaml:"ui_locale"`
	OutputLanguage string `yaml:"output_language"`
}

// TUIConfig holds C15 keymap / Vim / theme preferences. Vim and HighContrast
// are *bool so an explicit false stays distinguishable from unset.
//
// WIRING STATUS, stated here because this is the authoritative doc comment for
// the shape and an earlier version of it advertised capabilities that do not
// exist: every field below is consumed by `yanshi doctor` alone
// (internal/cli/doctor.go, the checkKeymapConfig / checkHighContrastConfig
// pair). The TUI does not read them — it hardcodes its theme and keymap
// defaults — no runtime command writes them back, and no preferences file is
// loaded (internal/cli/tui/preferences.go is not wired into the TUI). So these
// keys are validated and reported, not applied. Do not describe them as
// runtime-settable or as persisted until a consumer exists.
type TUIConfig struct {
	Vim          *bool             `yaml:"vim"`
	KeymapName   string            `yaml:"keymap"`
	Bindings     map[string]string `yaml:"bindings"`
	Theme        string            `yaml:"theme"`
	HighContrast *bool             `yaml:"high_contrast"`
	// Frecency gates the file-usage ranking behind @path completion (UX4).
	// Default ON. *bool so an explicit false is distinguishable from unset:
	// switching it off stops the RECORDING too, not just the ordering, because
	// a user who declined the feature did not ask for a quieter version of the
	// same usage profile.
	Frecency *bool `yaml:"frecency"`
}

// ObservabilityConfig groups process logging and OpenTelemetry settings.
type ObservabilityConfig struct {
	Log  LogConfig  `yaml:"log"`
	OTel OTelConfig `yaml:"otel"`
}

// LogConfig configures the slog-backed process logger.
type LogConfig struct {
	Level  string `yaml:"level"`
	Format string `yaml:"format"`
	// File redirects structured logs to this path instead of stderr. Empty
	// falls back to stderr (for headless modes) or the default log file
	// (~/.yanshi/logs/yanshi.log) when the TUI launches.
	File string `yaml:"file"`
	// StderrInTUI keeps logs on stderr even when the TUI is running.
	// Default false: the TUI boot path redirects logs to a file so the
	// alt-screen render is not corrupted by structured log lines. Set true
	// only when debugging the TUI itself.
	StderrInTUI bool `yaml:"stderr_in_tui"`
}

// OTelConfig configures OpenTelemetry export. Enabled=false (default) makes
// bootstrap install a no-op provider; SampleRatio is ignored when Enabled=false
// and defaults to 1.0 when OTel is on and the operator leaves it at 0.
type OTelConfig struct {
	Enabled     bool    `yaml:"enabled"`
	Endpoint    string  `yaml:"endpoint"`
	ServiceName string  `yaml:"service_name"`
	SampleRatio float64 `yaml:"sample_ratio"`
}

// FeaturesConfig seeds the OBS3 features registry. Strict makes IsEnabled /
// IsKnown return errors on unknown flag names so typos don't silently fall back
// to defaults. Overrides is the operator's seed table applied after the
// registry's built-in defaults.
type FeaturesConfig struct {
	Strict    bool            `yaml:"strict"`
	Overrides map[string]bool `yaml:"overrides"`
}

// PricingConfig wraps a model → ModelPricingOverride map used to extend or
// override the built-in price table.
type PricingConfig struct {
	Overrides map[string]ModelPricingOverride `yaml:"overrides"`
}

// ModelPricingOverride is config's transport-neutral YAML DTO. bootstrap
// converts it to einollm.ModelPricing so config remains a leaf package that
// does not import internal/llm.
type ModelPricingOverride struct {
	InputPerM    float64 `yaml:"input_per_million"`
	CacheHitPerM float64 `yaml:"cache_hit_per_million"`
	OutputPerM   float64 `yaml:"output_per_million"`
}

// SecurityConfig is the operator-facing security block. Sub-structs map 1:1
// to internal packages (Sandbox → internal/sandbox, Network → internal/netpolicy
// in Task 11, Shell → internal/shell in Task 16) so config and code stay in
// sync without stringly-typed indirection.
type SecurityConfig struct {
	Sandbox SandboxConfig      `yaml:"sandbox"`
	Network NetworkConfig      `yaml:"network"`
	Shell   ShellRuntimeConfig `yaml:"shell"`
}

// SandboxConfig drives internal/sandbox.New. Enabled is *bool so an unset YAML
// key can be distinguished from `enabled: false`; the former applies the
// default (true), the latter is an explicit opt-out. Tier is the string form
// of sandbox.AccessTier so the YAML stays portable ("read-only" /
// "workspace-write" / "full-access"); the sandbox layer normalizes.
type SandboxConfig struct {
	Enabled     *bool  `yaml:"enabled"`
	Tier        string `yaml:"tier"`
	NetworkDeny bool   `yaml:"network_deny"`
}

// NetworkConfig seeds internal/netpolicy. Default="deny" is the fail-closed
// posture; Allow/Deny are host-pattern lists consulted deny-wins; AllowPrivate
// toggles loopback / RFC1918 / link-local access (default false because most
// callers have no reason to reach a metadata service).
type NetworkConfig struct {
	Default      string   `yaml:"default"`
	Allow        []string `yaml:"allow"`
	Deny         []string `yaml:"deny"`
	AllowPrivate bool     `yaml:"allow_private"`
}

// ShellRuntimeConfig caps the shell v2 runtime (Task 16+). MaxOutputBytes
// bounds how much stdout/stderr a session accumulates before the ring buffer
// starts dropping head; IdleTimeout is how long a session can go without a
// client before the manager reaps it.
type ShellRuntimeConfig struct {
	MaxOutputBytes int           `yaml:"max_output_bytes"`
	IdleTimeout    time.Duration `yaml:"idle_timeout"`
}

// GoalConfig configures the self-driven goal loop's budget.
//
// Both limits are 0 = unlimited by default. That default is a compatibility
// requirement, not a preference: a non-zero default would start hard-stopping
// runs that complete today, without the operator having asked for a limit.
//
// MaxTokens can only constrain agents that actually report token usage. An
// agent that reports none is unmetered — goalloop warns when it detects this —
// and MaxIterations is the limit that still binds it.
type GoalConfig struct {
	// MaxTokens caps the whole run's token spend. 0 = unlimited.
	MaxTokens int `yaml:"max_tokens"`
	// MaxIterations caps plan-implement-evaluate-judge cycles. 0 = use the
	// -max-iters flag default.
	MaxIterations int `yaml:"max_iterations"`
}

// SubagentsConfig configures the managed sub-agent runtime (Batch B1).
type SubagentsConfig struct {
	Limit           int    `yaml:"limit"`
	PersistencePath string `yaml:"persistence_path"`
}

// CompactionConfig configures automatic context-compaction (Task 35b). Zero
// values on a Config loaded via Load are replaced with the defaults below; a
// zero value held only in memory (e.g. http.New without setting it) leaves
// compaction disabled, which is what existing transport tests rely on.
type CompactionConfig struct {
	// Threshold is the fraction of ContextWindow at which compaction fires
	// (default 0.8). Threshold <= 0 disables auto-compaction.
	Threshold float64 `yaml:"threshold"`
	// KeepRecent is the number of trailing user/assistant pairs (2*KeepRecent
	// messages) kept verbatim; everything before them is summarized
	// (default 4).
	KeepRecent int `yaml:"keep_recent"`
	// ContextWindow is the token budget used for the threshold check
	// (default 256000). This is the fallback used when a provider hasn't
	// set its own ContextWindow; ContextWindowFor layers a per-model value
	// on top of it.
	ContextWindow int `yaml:"context_window"`
	// Model optionally names a dedicated (fast) REMOTE model used for the
	// summarization turn. Empty means use the active session model.
	Model string `yaml:"model"`
	// ChunkThreshold is the fraction of ContextWindow at which a single
	// summarization turn is split into carriage-style chunks (default 0.9).
	// Below this the summarizer ingests the tail in one shot; above it the
	// input wouldn't fit alongside the model's own output, so we walk the
	// tail in overlapping chunks and fold each summary into the next pass.
	// <= 0 (only possible on a Config that bypassed Load) disables chunking.
	ChunkThreshold float64 `yaml:"chunk_threshold"`
	// CooldownFraction is the fraction of ContextWindow that sets CooldownTokens
	// (CooldownTokens = int(CooldownFraction * ContextWindow)). 0→no token-growth
	// cooldown. Default 0.05 delivers meaningful "no re-compact for trivial growth"
	// per CCL1 design.
	CooldownFraction float64 `yaml:"cooldown_fraction"`
	// CooldownDuration is the minimum wall-time since the last compaction before
	// re-compaction is allowed. Parsed via time.ParseDuration (e.g. "3s", "500ms").
	// "" or "0s" disables time-based cooldown. Default "" (disabled) — reduces CI
	// timing nondeterminism and keeps cooldown purely token-based.
	CooldownDuration string `yaml:"cooldown_duration"`
	// HardForceFraction forces compaction once estimated tokens reach this fraction
	// of ContextWindow, even when inside a cooldown. Default 0.95 (safety fallback).
	HardForceFraction float64 `yaml:"hard_force_fraction"`
}

// VCSConfig configures the built-in autoVCS tracker. Zero value (no vcs block
// in config) still enables tracking with defaults — Ignore augments the built-in
// ignore list, and WorktreeDir overrides where worktree working dirs are created.
type VCSConfig struct {
	Ignore      []string `yaml:"ignore"`
	WorktreeDir string   `yaml:"worktree_dir"`
}

// SkillsConfig configures skill discovery directories.
type SkillsConfig struct {
	BuiltinDir string `yaml:"builtin_dir"`
	UserDir    string `yaml:"user_dir"`
	PluginDir  string `yaml:"plugin_dir"`
}

// ServerConfig configures the HTTP and task server addresses.
type ServerConfig struct {
	HTTPAddr string `yaml:"http_addr"`
	TaskAddr string `yaml:"task_addr"`
}

// StorageConfig configures the SQLite database path.
type StorageConfig struct {
	SQLitePath string `yaml:"sqlite_path"`
	// WALMaxOpenConns 放开读连接池上限（F1）。0/省略=4；1=旧行为（单连接，排障）。
	// 写仍由进程内 writeMu 串行，故此值只影响读并行度。
	WALMaxOpenConns int `yaml:"wal_max_open_conns"`
	// BusyTimeoutMs 是 SQLite busy_timeout（F1），跨进程锁冲突重试窗口。0/省略=5000。
	BusyTimeoutMs int `yaml:"busy_timeout_ms"`
	// WALAutoCheckpoint 是 wal_autocheckpoint 页阈值（F1）。0/省略=1000；负数=禁用被动 checkpoint（不推荐）。
	WALAutoCheckpoint int `yaml:"wal_auto_checkpoint"`
}

// LLMConfig configures the available LLM providers.
type LLMConfig struct {
	Providers []ProviderConfig `yaml:"providers"`
}

// ProviderConfig configures a single LLM provider (kind, model, API key, etc.).
type ProviderConfig struct {
	Name    string `yaml:"name"`
	Kind    string `yaml:"kind"` // "openai" | "openai-responses" | "anthropic"
	Model   string `yaml:"model"`
	APIKey  string `yaml:"api_key"`
	BaseURL string `yaml:"base_url"`
	// CostClass classifies this provider's cost tier ("cheap", "expensive").
	// Used by batch.rlm_model to select a cheap provider for fan-out queries.
	CostClass string `yaml:"cost_class"`
	// ContextWindow is this model's token window; 0 means fall back to
	// CompactionConfig.ContextWindow via ContextWindowFor. Setting it per
	// provider lets compaction size against the actual model in a multi-
	// provider session instead of guessing one global budget.
	ContextWindow int `yaml:"context_window"`
	// Multimodal declares native image support (Tier G).
	// When the main model is non-multimodal, bootstrap auto-selects
	// the first Multimodal==true provider as the vision auxiliary.
	// Zero value (false) = text-only; valid default.
	Multimodal bool `yaml:"multimodal"`
}

// AgentConfig configures a named sub-agent.
type AgentConfig struct {
	Name         string   `yaml:"name"`
	Type         string   `yaml:"type"` // local | external | remote
	Chain        []string `yaml:"chain"`
	Profile      string   `yaml:"profile"`
	Capabilities []string `yaml:"capabilities"`
}

// Load reads and parses the YAML config at path. Environment variables in
// the form ${VAR} are expanded before unmarshalling.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return LoadBytes(data)
}

// SupportedSchemaVersion is the current config schema generation. Load rejects
// configs whose schema_version is higher; MigrateConfig upgrades lower ones.
// Bump this only on a destructive change (and add a migration + CHANGELOG
// major entry). Today v1 == 1.
const SupportedSchemaVersion = 1

// MigrateConfig upgrades an in-memory cfg from `from` to `to`. v1 has no
// destructive migration, so this is a no-op beyond asserting the target. It is
// the single insertion point for future schema migrations — Load never rewrites
// the user's disk file; migration is in-memory only (callers may opt in to
// writing a backup explicitly).
func MigrateConfig(cfg *Config, from, to int) error {
	if cfg == nil {
		return fmt.Errorf("config: nil cfg")
	}
	if from > to {
		return fmt.Errorf("config: cannot migrate schema %d down to %d", from, to)
	}
	// No-op at v1: A–D evolution was additive. Future migrations chain here:
	//   for v := from; v < to; v++ { switch v { case 1: /* ... */ } }
	cfg.SchemaVersion = to
	return nil
}

// LoadBytes parses YAML config data, expanding env vars and applying defaults.
func LoadBytes(data []byte) (*Config, error) {
	expanded := os.ExpandEnv(string(data))
	var cfg Config
	if err := yaml.Unmarshal([]byte(expanded), &cfg); err != nil {
		return nil, err
	}
	cfg.applyDefaults()

	// Schema version gate (UPG1). Missing field (0) -> treat as current
	// (A–D was additive). Higher than supported -> reject (user must upgrade
	// yanshi). Lower -> in-memory migration (no disk rewrite).
	switch {
	case cfg.SchemaVersion == 0:
		cfg.SchemaVersion = SupportedSchemaVersion
	case cfg.SchemaVersion > SupportedSchemaVersion:
		return nil, fmt.Errorf(
			"config: schema_version=%d exceeds supported=%d; upgrade yanshi",
			cfg.SchemaVersion, SupportedSchemaVersion)
	case cfg.SchemaVersion < SupportedSchemaVersion:
		if err := MigrateConfig(&cfg, cfg.SchemaVersion, SupportedSchemaVersion); err != nil {
			return nil, err
		}
	}

	if err := cfg.validateAPIKeyRefs(cfg.Auth.LegacyInsecure); err != nil {
		return nil, err
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// validateAPIKeyRefs ensures every non-empty, non-reference api_key is either
// legacy-opted-in or rejected. This is the second gate after the auth
// Manager's ParseCredentialRef: it catches raw literals that arrived via
// os.ExpandEnv of ${VAR} references, where the original YAML text was a
// legitimate-looking env var reference but the expanded result is a
// plaintext value that would otherwise bypass credential resolution.
//
// Empty api_key is always accepted (the provider is unauthenticated and will
// fail at first API call). secret:// and env:// refs are deferred to the
// auth layer's resolution.
func (c *Config) validateAPIKeyRefs(legacyAllowed bool) error {
	for i := range c.LLM.Providers {
		p := &c.LLM.Providers[i]
		if p.APIKey == "" {
			continue
		}
		if strings.HasPrefix(p.APIKey, "secret://") ||
			strings.HasPrefix(p.APIKey, "env://") {
			continue
		}
		// At this point p.APIKey is a plaintext value — either a raw literal
		// from the YAML or an ${VAR}-expanded value. Both bypass credential
		// resolution and fail closed without explicit opt-in.
		if !legacyAllowed && !c.Auth.AutoMigrate {
			return fmt.Errorf(
				"config: provider %q api_key %q is a raw literal or expanded ${VAR}; "+
					"use secret://service/account or env://VAR, "+
					"or set auth.legacy_insecure=true to accept plaintext keys",
				p.Name, ellipsis(p.APIKey),
			)
		}
	}
	return nil
}

// ellipsis truncates a string to 32 chars for safe error messages. A
// truncated key still lets the operator recognise "yes that's the key I
// pasted" without echoing the full value into logs.
func ellipsis(s string) string {
	if len(s) <= 32 {
		return s
	}
	return s[:16] + "..." + s[len(s)-13:]
}

// applyDefaults fills zero Compaction fields with the conventional defaults.
// Threshold stays 0 when unset by the user ONLY when the whole compaction
// block is absent — but the common-case default for a loaded config is an
// enabled 0.8/4/256000 policy, so absent values are treated as "default"
// rather than "disabled". Tests that build a Config without Load keep zeros
// (compaction disabled).
func (c *Config) applyDefaults() {
	if c.Compaction.Threshold == 0 {
		c.Compaction.Threshold = 0.8
	}
	if c.Compaction.KeepRecent == 0 {
		c.Compaction.KeepRecent = 4
	}
	if c.Compaction.ContextWindow == 0 {
		c.Compaction.ContextWindow = 256000
	}
	if c.Compaction.ChunkThreshold == 0 {
		c.Compaction.ChunkThreshold = 0.9
	}
	if c.Compaction.CooldownFraction == 0 {
		c.Compaction.CooldownFraction = 0.05
	}
	if c.Compaction.HardForceFraction == 0 {
		c.Compaction.HardForceFraction = 0.95
	}
	// CooldownDuration defaults to "" (disabled).
	// Security defaults: sandbox opt-IN (operators get the safe posture when
	// they forget to configure security); tier read-only; network default
	// deny; shell output 1 MiB / idle 30 min. These mirror the documented
	// `security:` block in config.example.yaml so a config without it behaves
	// identically to one that explicitly pastes the example.
	if c.Security.Sandbox.Enabled == nil {
		enabled := true
		c.Security.Sandbox.Enabled = &enabled
	}
	if c.Security.Sandbox.Tier == "" {
		c.Security.Sandbox.Tier = "read-only"
	}
	if c.Security.Network.Default == "" {
		c.Security.Network.Default = "deny"
	}
	if c.Security.Shell.MaxOutputBytes == 0 {
		c.Security.Shell.MaxOutputBytes = 1 << 20
	}
	if c.Security.Shell.IdleTimeout == 0 {
		c.Security.Shell.IdleTimeout = 30 * time.Minute
	}
	if c.Subagents.Limit == 0 {
		c.Subagents.Limit = 10
	}
	if c.Subagents.PersistencePath == "" {
		c.Subagents.PersistencePath = "~/.yanshi/subagents.v1.json"
	}
	c.Subagents.PersistencePath = expandHome(c.Subagents.PersistencePath)

	// LSP 默认启用(Enabled==nil 视为 true)。Timeout 缺省 800ms;非法值降级 800ms。
	if c.LSP.Enabled == nil {
		t := true
		c.LSP.Enabled = &t
	}
	if c.LSP.Timeout == "" {
		c.LSP.Timeout = "800ms"
	}

	// Observability defaults: info/json logger, service "yanshi"; OTel stays
	// disabled unless the operator flips it on. SampleRatio defaults to 1.0
	// when OTel is enabled and the operator didn't set one, so traces are not
	// silently dropped.
	if c.Observability.Log.Level == "" {
		c.Observability.Log.Level = "info"
	}
	if c.Observability.Log.Format == "" {
		c.Observability.Log.Format = "json"
	}
	if c.Observability.OTel.ServiceName == "" {
		c.Observability.OTel.ServiceName = "yanshi"
	}
	if c.Observability.OTel.Enabled && c.Observability.OTel.SampleRatio == 0 {
		c.Observability.OTel.SampleRatio = 1
	}
	// D3 defaults: secrets backend auto + standard user-config-dir file path,
	// i18n ui_locale "auto" (re-resolved at each startup), TUI default keymap
	// and theme names. These keep a config without a secrets/i18n/tui block
	// behaving identically to one that explicitly pastes the example.
	if c.Secrets.Backend == "" {
		c.Secrets.Backend = "auto"
	}
	if c.Secrets.FilePath == "" {
		configDir, err := os.UserConfigDir()
		if err != nil {
			configDir = "."
		}
		c.Secrets.FilePath = filepath.Join(configDir, "yanshi", "secrets.enc")
	}
	if c.I18N.UILocale == "" {
		c.I18N.UILocale = "auto"
	}
	if c.TUI.KeymapName == "" {
		c.TUI.KeymapName = "default"
	}
	if c.TUI.Theme == "" {
		c.TUI.Theme = "default"
	}
	// F1: WAL + connection pool defaults. Zero means "use default". Negative
	// wal_auto_checkpoint passes through (disable passive checkpoint).
	if c.Storage.WALMaxOpenConns == 0 {
		c.Storage.WALMaxOpenConns = 4
	}
	if c.Storage.BusyTimeoutMs == 0 {
		c.Storage.BusyTimeoutMs = 5000
	}
	if c.Storage.WALAutoCheckpoint == 0 {
		c.Storage.WALAutoCheckpoint = 1000
	}
}

func (c *Config) validate() error {
	if c.Subagents.Limit != 0 && (c.Subagents.Limit < 1 || c.Subagents.Limit > 20) {
		return errors.New("subagents.limit must be within 1..20")
	}
	return c.validateProfiles()
}

// validateProfiles rejects profile fields whose illegal values would otherwise
// stay dormant until the guard evaluates them mid-session.
//
// Only shell.policy is checked, and the asymmetry is deliberate: it is the one
// profile field where a typo degrades into guard's STRUCTURAL HardDeny — the
// tier no permission mode can override, so neither yolo nor auto can rescue the
// session, and the operator sees a refusal with no path back except editing
// config and restarting. Every other profile field either has no enumeration
// (glob lists, booleans) or degrades toward the restrictive end in a way the
// interactive modes can still override.
//
// Rejecting the config outright is a tightening only in appearance, but ONLY
// because the check is scoped to profiles whose shell.rules is empty. That
// scoping is the whole correctness argument and must not be widened casually:
//
//   - shell.rules empty — the legacy switch is the live code path, so an
//     unknown policy denies every shell_run unconditionally. The profile is
//     already dead; refusing to start turns a mid-session structural HardDeny
//     into a startup error that names the field. Nothing that works stops
//     working.
//   - shell.rules non-empty — checkShell returns from inside the execpolicy
//     branch on every path, so the policy switch is unreachable and the bad
//     value is INERT. Such a profile enforces its rules today exactly as
//     written; a `policy: "allow"` typo next to a working rules table costs
//     nothing at runtime. Validating it would be a real behavioural
//     regression: a deployment that runs fine would refuse to boot after an
//     upgrade, and it would be refused over a field docs/user-guide/guard.md
//     tells the operator is ignored when rules is present.
//
// The inert typo is not lost, only deferred to the moment it stops being
// inert. A value only becomes load-bearing when the rules table is emptied,
// which requires editing config, and the edit is followed by a reload through
// this same function — where Rules is now empty and the check fires. So the
// narrow form still catches every configuration in which the policy can
// actually deny anything; it just declines to fail a boot over a field the
// guard never reads.
//
// Profiles is a bare map[string]guard.PermissionProfile, so profile names are
// sorted before reporting to keep the error deterministic across runs.
func (c *Config) validateProfiles() error {
	names := make([]string, 0, len(c.Profiles))
	for name := range c.Profiles {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		p := c.Profiles[name]
		if len(p.Shell.Rules) > 0 {
			continue
		}
		if err := guard.ValidateShellPolicy(p.Shell.Policy); err != nil {
			return fmt.Errorf("profiles.%s.shell.policy: %w", name, err)
		}
	}
	return nil
}

func expandHome(p string) string {
	if p == "" {
		return ""
	}
	if strings.HasPrefix(p, "~") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, p[1:])
		}
	}
	return p
}

// ContextWindowFor returns provider p's context_window if set; else the fallback
// (typically CompactionConfig.ContextWindow). Returns 0 when neither is set,
// letting the caller disable compaction rather than guess a window.
func ContextWindowFor(providerName string, providers []ProviderConfig, fallback int) int {
	for _, p := range providers {
		if p.Name == providerName && p.ContextWindow > 0 {
			return p.ContextWindow
		}
	}
	return fallback
}

// LSPConfig 配置编辑后诊断回喂(B2-LSP1)。Enabled 为 *bool:yaml 里省略 → nil
// → 默认 true;显式 false → disabled(评审 #15:区分 unset 与 false)。Timeout
// 是诊断等待,字符串形式(applyDefaults 里 time.ParseDuration),默认 800ms。
type LSPConfig struct {
	Enabled  *bool                         `yaml:"enabled"`
	Timeout  string                        `yaml:"diag_timeout"`
	Override map[string]LanguageServerSpec `yaml:"languages"`
}

// LanguageServerSpec 是 config 里语言→server 的描述(yaml 友好名;bootstrap
// 转 lsp.LanguageServer)。
type LanguageServerSpec struct {
	Command string   `yaml:"command"`
	Args    []string `yaml:"args"`
}

// MemoryConfig configures MEM1 user memory file. All fields optional;
// Enabled=false (default) makes bootstrap skip the subsystem entirely.
type MemoryConfig struct {
	Enabled     bool   `yaml:"enabled"`
	UserPath    string `yaml:"user_path"`    // ~ expanded by bootstrap; "" = ~/.yanshi/memory.md
	ProjectPath string `yaml:"project_path"` // optional, relative to work root; "" = <workRoot>/.yanshi/memory.md
	MaxSize     int    `yaml:"max_size"`     // per-file byte cap; 0 = memory.defaultMaxBytes
}
