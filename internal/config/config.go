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

	// OAuth configures the token grant for an HTTP MCP server. Absent means
	// the server authenticates with Bearer (or not at all), which is what
	// every config predating this field meant.
	OAuth *MCPOAuthConfig `yaml:"oauth,omitempty"`

	// ToolAllow restricts which tools from this server yanshi registers, by
	// the server's own tool name (before the mcp_<server>_ prefix). Exact
	// names or `*`-style globs ("search_*"). Empty means every advertised
	// tool is a candidate — the security gate stays downstream in the guard's
	// mcp dimension, whose empty-allow fail-closed semantics this list does
	// not touch; this is a registration-side narrowing only and can never
	// widen anything.
	ToolAllow []string `yaml:"tool_allow,omitempty"`
	// ToolDeny excludes tools by the same name/glob shape as ToolAllow.
	// Deny wins over allow, so "allow everything but the destructive one" is
	// tool_allow: ["*"] plus tool_deny: ["deploy_prod"].
	ToolDeny []string `yaml:"tool_deny,omitempty"`
}

// MCPOAuthConfig configures OAuth 2.0 token acquisition for one MCP server.
//
// This block exists on the config side because internal/mcp.ServerConfig has
// carried an OAuth field since T10, and nothing projected into it: the grant
// was reachable from Go but from no YAML an operator could write, so the
// authorization_code flow and `yanshi auth mcp-login` were unreachable in a
// real deployment. The two structs are deliberately separate — this one is the
// YAML surface, mcp.OAuthConfig is the wire/runtime shape — and buildMCPManager
// is the single projection point.
//
// ClientSecret is resolved by Load's ${VAR} expansion, so the usual form is a
// reference to an environment variable rather than a literal. It must never be
// logged or placed on a status frame; mcp.OAuthConfig tags it `json:"-"` for
// exactly that reason.
type MCPOAuthConfig struct {
	// TokenURL is the token endpoint. Required for both grants.
	TokenURL string `yaml:"token_url"`
	// AuthorizationURL is the authorize endpoint. Required for
	// authorization_code and ignored for client_credentials.
	AuthorizationURL string `yaml:"authorization_url,omitempty"`
	// ClientID identifies this client to the authorization server.
	ClientID string `yaml:"client_id"`
	// ClientSecret is the client password, when the server issues one.
	ClientSecret string `yaml:"client_secret,omitempty"`
	// Scopes are the OAuth scopes requested.
	Scopes []string `yaml:"scopes,omitempty"`
	// Grant selects the flow: "client_credentials" (the default, and what an
	// absent value means) or "authorization_code", which is established
	// interactively with `yanshi auth mcp-login <server>`.
	Grant string `yaml:"grant,omitempty"`
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
	// PolicyActive reports that a trusted policy file (S3, policy.go) supplied
	// the profiles above. It is set by Load and is NOT a YAML key: an agent
	// that could set it from config.yaml could claim to be policy-governed
	// while governing itself, which is the inversion this whole mechanism
	// exists to prevent.
	PolicyActive bool `yaml:"-"`
	// PolicyNarrowed names the profiles whose local definition was clamped by
	// the trusted policy. Reported once at boot so a narrowing that did not
	// take effect is visible then, rather than inferred from a denial later.
	PolicyNarrowed []string `yaml:"-"`
	// PolicyFileActive reports that a trusted policy file (S3, policy.go) was
	// present at load — regardless of whether it named any profiles. It is
	// DELIBERATELY not the same signal as PolicyActive, which requires
	// len(policy.Profiles) > 0: guardian_prompt_file and (B-2)
	// llm.providers[].auth are both taken over by ApplyPolicy whenever a
	// trusted file exists at all, even one that names no profiles (see
	// ApplyPolicy's comments), so a check that only cares about ONE of those
	// two keys — `yanshi doctor`'s auth-command-scope check, which does not
	// know or care whether `profiles:` was ever written — needs the coarser
	// signal. Not a YAML key for the same reason PolicyActive is not: an
	// agent that could set it from config.yaml could claim to be
	// policy-governed while governing itself.
	PolicyFileActive bool         `yaml:"-"`
	Skills           SkillsConfig `yaml:"skills"`
	VCS              VCSConfig    `yaml:"vcs"`
	Batch            BatchConfig  `yaml:"batch"`
	// Compaction configures automatic context-compaction (Task 35b): when the
	// estimated token count of the conversation history reaches
	// Threshold*ContextWindow, the older turns are summarized by a remote model
	// (optionally a dedicated fast Compaction.Model), streaming the summary back
	// in real time. Defaults are applied in Load.
	Compaction CompactionConfig `yaml:"compaction"`
	// LoopGuard configures the per-turn agent stop conditions (doom-loop
	// detection, tool-call budgets, turn deadline, token budget). Every field
	// is off when zero and no defaults are applied in Load, so an absent block
	// leaves turn behaviour byte-identical to before the guard existed. See
	// LoopGuardConfig for why that asymmetry with Compaction is deliberate.
	LoopGuard LoopGuardConfig `yaml:"loop_guard"`
	// Hooks configures the lifecycle hook bus (W-F-02): operator-configured
	// external programs the orchestrator consults around agent events. The
	// zero value runs no hooks and spawns nothing; once a hook IS configured
	// its failures fail closed — see HooksConfig. Load rejects a hook entry
	// with an empty program at load time rather than at the first tool call.
	Hooks HooksConfig `yaml:"hooks"`
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
// exist: this comment used to claim "the TUI does not read them... no
// preferences file is loaded" — that was true when written but went stale
// without being updated. As of W8, cmd/yanshi/main.go DOES read every field
// below into a tui.Preferences project layer — the lowest of the FOUR sparse
// layers (flags > env > user > project), sitting on top of a defaults
// baseline that is not itself a layer; see mergeTUIPrefs in
// internal/cli/tui/preferences.go, whose signature takes exactly those four.
// (RE-35: this used to call project "the lowest tier of the flags > env >
// user > project > defaults cascade", which puts defaults below the thing it
// calls lowest.) `yanshi doctor`
// (checkKeymapConfig / checkHighContrastConfig) separately validates and
// reports the same keys. Both consumers are real; do not let this comment
// drift again without checking cmd/yanshi/main.go's tui.Preferences{...}
// construction.
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
	// Notify gates W-E-04's desktop notifications (OSC 9, degrading to a
	// plain BEL on terminals E1's capability gate does not trust with escape
	// sequences). Default OFF, unlike Frecency: an agent that pings the
	// desktop on every finished turn by default is the "obnoxious default"
	// explicitly ruled out for this feature. *bool so an explicit false stays
	// distinguishable from unset, matching every other field here.
	Notify *bool `yaml:"notify"`
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
	// MaxSizeMB is the size the active log file may reach before it is
	// rotated into a numbered generation. Zero takes the package default
	// (obslog.DefaultRotateMaxBytes, 10 MiB); negative disables rotation by
	// size, turning the log back into a plain append-only file.
	//
	// It is expressed in megabytes rather than bytes because that is the unit
	// an operator sizing a log volume thinks in, and because a bytes field
	// invites the off-by-1024 that makes a 10 MiB intent into a 10 MB file.
	MaxSizeMB int `yaml:"max_size_mb"`
	// MaxBackups is how many rotated generations are kept. Zero takes the
	// package default (obslog.DefaultRotateMaxBackups, 5); negative keeps
	// none, truncating on rotation instead of renaming.
	MaxBackups int `yaml:"max_backups"`
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
	// GuardianPromptFile points at a file holding the instruction body the
	// `auto` permission mode shows the model (W-B-14). Empty = the built-in
	// policy in guard.AutoApprovalPrompt.
	//
	// A FILE rather than an inline string: the body is ~60 lines of prose, and
	// a YAML block scalar that long turns every edit into a whitespace hazard
	// in the one document an operator must not get wrong.
	//
	// The file is read and validated at load. Both failures — unreadable, or
	// missing one of guard.RequiredRiskCategories — refuse the start, because
	// the alternative is a deployment that believes it installed a policy and
	// is running the built-in one.
	GuardianPromptFile string `yaml:"guardian_prompt_file"`
	// GuardianPrompt is the validated contents of GuardianPromptFile, filled in
	// by Load. It is not a YAML key: writing the prose inline is the shape the
	// file indirection exists to avoid, and a key that could set it directly
	// would be a second, unvalidated way in.
	GuardianPrompt string `yaml:"-"`
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
	// InspectHTTPS turns the managed proxy's CONNECT handling from a blind
	// tunnel into a terminated one, so requests inside it are judged on method
	// as well as host. OFF by default, and that default is the decision
	// ADR-0014 made — read ADR-0023 before turning it on, especially the part
	// about which children can be made to trust the generated root and which
	// cannot.
	InspectHTTPS bool `yaml:"inspect_https"`
	// Methods narrows an allowed host to particular HTTP methods. Entries are
	// evaluated in order and the first match wins; a host the top-level Deny
	// list refuses is never reconsidered here.
	//
	// It has no effect on a request whose method nobody read — a blind CONNECT
	// or a SOCKS5 tunnel — so a method rule for an https host is inert unless
	// InspectHTTPS is on. Validation says so out loud rather than letting the
	// operator discover it from traffic.
	Methods []NetworkMethodRule `yaml:"methods"`
}

// NetworkMethodRule is one entry of NetworkConfig.Methods. Host uses the same
// pattern syntax as Allow/Deny; Methods is a list of HTTP verbs, and an empty
// list means every method. Action must be "allow" or "deny" — there is no
// default, because a rule whose verdict was guessed is a rule that silently
// does the opposite of what its author meant.
type NetworkMethodRule struct {
	Host    string   `yaml:"host"`
	Methods []string `yaml:"methods"`
	Action  string   `yaml:"action"`
}

// ShellRuntimeConfig caps the shell v2 runtime (Task 16+). MaxOutputBytes
// bounds how much stdout/stderr a session accumulates before the ring buffer
// starts dropping head; IdleTimeout is how long a session can go without a
// client before the manager reaps it.
type ShellRuntimeConfig struct {
	MaxOutputBytes int           `yaml:"max_output_bytes"`
	IdleTimeout    time.Duration `yaml:"idle_timeout"`
	// MaxConcurrent caps how many shell sessions may hold a live OS process at
	// once. 0 (the default) means no cap. Over the cap a start QUEUES rather
	// than failing — see shell.Config.MaxConcurrent for why refusing would be
	// the wrong reflex here.
	MaxConcurrent int `yaml:"max_concurrent"`
	// CaptureProfile runs the operator's login shell once at startup and
	// layers the environment it produces under every child launch, so a yanshi
	// started from a desktop launcher can still find the toolchains the
	// operator's rc files put on PATH.
	//
	// OFF by default: it executes the operator's startup files, which is a
	// side effect nobody asked for at boot, and the failure it fixes is
	// invisible to an operator who launched yanshi from their own terminal.
	CaptureProfile bool `yaml:"capture_profile"`
	// ProfileShell selects which shell CaptureProfile asks
	// (bash|zsh|sh|powershell). Empty means $SHELL on unix, powershell on
	// Windows.
	ProfileShell string `yaml:"profile_shell"`
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

// LoopGuardConfig configures the per-turn agent stop conditions.
//
// Every field is "off when zero" and Load applies NO defaults to this block —
// deliberately unlike CompactionConfig. A stop condition that switches itself
// on with a guessed default would silently truncate turns on an installation
// whose operator never asked for a limit, and the failure mode is
// indistinguishable from the model simply giving up. Opting in is one line of
// YAML; diagnosing a turn that was cut short by an unrequested budget is not.
type LoopGuardConfig struct {
	// RepetitionEnabled turns on doom-loop detection: identical tool calls
	// (name plus arguments) repeated within a sliding window first draw a
	// warning injected into the prompt, then end the turn.
	RepetitionEnabled bool `yaml:"repetition_enabled"`
	// RepetitionWindow is the sliding window size in tool calls. 0 selects
	// loopguard.DefaultRepetitionWindow. The concrete numbers deliberately
	// live there and are not restated here or in config.example.yaml: a
	// second copy of a default drifts silently the first time the real one
	// is tuned.
	RepetitionWindow int `yaml:"repetition_window"`
	// RepetitionWarnAfter is the consecutive-repeat count at which the turn is
	// nudged with a warning. 0 selects loopguard.DefaultRepetitionStages.
	RepetitionWarnAfter int `yaml:"repetition_warn_after"`
	// RepetitionStopAfter is the consecutive-repeat count at which the turn
	// ends. 0 selects loopguard.DefaultRepetitionStages.
	RepetitionStopAfter int `yaml:"repetition_stop_after"`
	// MaxToolCalls caps total tool calls in one turn. 0 = unlimited.
	MaxToolCalls int `yaml:"max_tool_calls"`
	// PerToolCalls caps calls per tool name in one turn, e.g.
	// {shell_run: 20, agent_spawn: 3}. Names match exactly; absent names are
	// unlimited. A budget refusal is returned to the model as a tool RESULT,
	// not a Go error, so the turn can adapt instead of aborting.
	PerToolCalls map[string]int `yaml:"per_tool_calls"`
	// TurnTimeout is the wall-clock limit for one turn, checked only at ReAct
	// iteration boundaries — it bounds the loop, not any single tool call.
	// 0 = unlimited.
	TurnTimeout time.Duration `yaml:"turn_timeout"`
	// MaxTurnTokens caps accumulated token spend for one turn. Accumulation
	// counts prompt GROWTH rather than raw per-call prompt totals, because
	// providers report prompt tokens cumulatively and summing them would fire
	// the budget at a fraction of its nominal value. 0 = unlimited.
	MaxTurnTokens int `yaml:"max_turn_tokens"`
}

// HooksConfig configures the lifecycle hook bus (W-F-02, spec §1.2 INF4).
//
// Zero value runs no hooks: nothing is spawned, nothing is consulted, every
// tool call behaves exactly as it did before the bus existed. Once a hook IS
// configured, its failures fail closed — a hook that times out, crashes or
// emits unreadable output refuses THAT tool call (as a tool result, the turn
// continues). That is the orchestrator's Ruling, recorded in ADR-0027 and the
// hook bus's doc comment; the short version is that the model writes the tool
// arguments, so a model that can crash a weak hook must not thereby silence
// it.
type HooksConfig struct {
	// PreToolUse lists the programs consulted before each tool call, in
	// declaration order. Later hooks see earlier hooks' rewritten arguments;
	// the permission guard re-judges the final arguments from scratch, and a
	// hook can never turn a guard denial into an allow. A hook blocks by
	// answering {"block":true,"reason":"..."} on stdout.
	PreToolUse []HookConfig `yaml:"pre_tool_use"`
	// PreCompact / PostCompact (W-F-08) run before and after every compaction
	// attempt, on all three compaction paths (pre-turn, mid-turn, manual
	// /compact). They are OBSERVERS: the hook reads one lifecycle-event JSON
	// object on stdin (event, trigger, token counts, failure/fallback flags)
	// and exits; its stdout is discarded. A failed compaction hook is logged
	// and skipped — the deliberate opposite of pre_tool_use's fail-closed,
	// see the ctxcompact lifecycle-bus doc for the reasoning.
	PreCompact  []HookConfig `yaml:"pre_compact"`
	PostCompact []HookConfig `yaml:"post_compact"`
}

// HookConfig is one hook program: an executable path (never a shell line —
// no quoting or expansion happens; an operator who wants shell semantics
// writes the /bin/sh -c shape explicitly and it passes the guard's shell
// dimension like any other spawn), fixed arguments, and a per-call timeout.
type HookConfig struct {
	// Program is the hook executable. Required: Load rejects an entry with an
	// empty program at load time, because a typo would otherwise surface only
	// as a fail-closed refusal of every tool call at runtime. A path that does
	// not exist (or is not executable) still fails at first use — load checks
	// presence of the string, not of the file.
	Program string `yaml:"program"`
	// Args are the fixed arguments passed to Program, uninterpreted.
	Args []string `yaml:"args"`
	// Timeout bounds one hook invocation. "10s" style strings per time.ParseDuration;
	// 0 selects the orchestrator's default (10s). A unitless number fails at
	// load, exactly like loop_guard.turn_timeout.
	Timeout time.Duration `yaml:"timeout"`
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

	// RetentionDays 是冷会话压缩的空闲阈值（W-D-04）：超过这么多天没有活动的
	// 会话，其消息被整体序列化 + gzip 存进 cold_sessions，原行删除。读取侧透明
	// 回退解压，所以对话内容一条不少。
	//
	// **0/省略 = 永久保留，不压缩任何东西**，与引入本字段前逐字节一致。这不是
	// 保守的默认值而是唯一安全的默认值：压缩会把冷会话移出 FTS 索引，
	// history_search 从此找不到它们，这个代价必须由操作员显式选择。
	//
	// **到期是压缩不是删除。** 本字段永远不会让任何一条消息消失；删除只有
	// store.DeleteSession 这一条显式路径。名字里的 "retention" 沿用 spec 的
	// 措辞，语义以这段注释为准。
	RetentionDays int `yaml:"retention_days"`

	// MemoryAutoExtract 打开 W-D-03 的后台跨会话记忆抽取：会话安静
	// upkeep.MemoryIdle 之后，worker 读取它的窗口、向模型要一批"以后还成立的
	// 事实"、逐条写进 memories，然后调 W-A-05 那条既有的蒸馏入口做合并。
	//
	// **默认 false，因为它花钱。** 每个结束的会话一次 provider 调用，用的是
	// batch.rlm_model（没配就退回主模型）。没人升级一次程序就同意开始产生这笔
	// 开销，所以它必须是操作员显式打开的。
	MemoryAutoExtract bool `yaml:"memory_auto_extract"`

	// MemoryQuota 限制 memories 表的行数上限；**0/省略 = 不限制**，与引入前
	// 一致。超额时只删 use_count = 0（从未被检索命中过）的行，最老的先删；
	// 被用过的记忆无论多老都不删 —— 这正是配额与"过期"的区别。
	//
	// 与 MemoryAutoExtract 相互独立：memory_write 一样往这张表里写，配额管的是
	// 表的大小而不是谁写的。
	MemoryQuota int `yaml:"memory_quota"`
}

// LLMConfig configures the available LLM providers.
type LLMConfig struct {
	Providers []ProviderConfig `yaml:"providers"`

	// RateLimit throttles outgoing model calls (M7). It is the DEFAULT applied
	// to every model that does not set its own qpm/burst. QPM 0 — the zero
	// value, and therefore the value an absent block yields — means no limit,
	// so adding this field changed no existing deployment's behaviour.
	RateLimit RateLimitConfig `yaml:"rate_limit"`

	// SanitizeToolSchemas selects the M6 tool-schema rewriting policy:
	//
	//   "auto"   (default) rewrite only for a model that has actually rejected
	//            a schema. Costs one failed request per model per process and
	//            pays the rewrite's lossiness only where it is needed.
	//   "always" rewrite for every model. For a deployment that talks only to
	//            a strict gateway and does not want to spend that first 400.
	//   "never"  never rewrite. For diagnosing whether the rewriter itself is
	//            the problem.
	//
	// An unrecognised value normalises to "auto" rather than failing the load:
	// a typo here must not stop a server from starting, because the safe
	// behaviour is also the default behaviour.
	SanitizeToolSchemas string `yaml:"sanitize_tool_schemas"`

	// Preflight enables the M9 startup model-name check, which asks each
	// provider for its model catalogue and warns about a name that is not in
	// it (with the nearest matches). Default true.
	//
	// It is a *bool because the default is true: a plain bool cannot express
	// "the operator explicitly turned this off". The check can never block or
	// fail a startup — it only logs — so the flag exists to silence a probe
	// that would time out on every boot of an air-gapped deployment.
	Preflight *bool `yaml:"preflight"`

	// StreamFirstChunkTimeout (W-A-06) bounds how long the streaming path
	// waits for the FIRST chunk of a response. 0 (the default) disables it —
	// no non-zero default, mirroring loopguard's "zero means off" principle,
	// so an unconfigured deployment behaves exactly as it did before this
	// field existed. See einollm.watchdogReader.
	StreamFirstChunkTimeout time.Duration `yaml:"stream_first_chunk_timeout"`

	// StreamIdleTimeout (W-A-06) bounds the gap between content-bearing
	// chunks once streaming has started. It is independent of
	// StreamFirstChunkTimeout because the two measure different things: a
	// gateway that accepts a connection and then sends nothing hangs inside
	// schema.StreamReader.Recv forever — a stall loopguard's DeadlineGate
	// cannot catch, since it only checks between ReAct iterations, and a
	// stuck Recv never reaches the next one. 0 (the default) disables it,
	// reproducing pre-W-A-06 behaviour byte-for-byte. See
	// einollm.watchdogReader.
	StreamIdleTimeout time.Duration `yaml:"stream_idle_timeout"`

	// MaxRetries (W-C-07) is the global per-attempt retry ceiling
	// ResilientChatModel falls back to for a provider that does not set its
	// own ProviderConfig.MaxRetries. 0 (the zero value, and therefore what an
	// absent llm block yields) means "use the resilient layer's own
	// built-in default" (10, see NewResilientModel) — this field only
	// becomes an override once the operator sets it, so a config that never
	// mentions retries behaves exactly as it did before this field existed.
	MaxRetries int `yaml:"max_retries"`
}

// RateLimitConfig bounds how fast yanshi issues model calls. It appears both
// as the process-wide default (llm.rate_limit) and per provider.
type RateLimitConfig struct {
	// QPM is the sustained ceiling in requests per minute. 0 means no limit.
	QPM int `yaml:"qpm"`
	// Burst is how many requests may be issued back-to-back after an idle
	// period. 0 derives it from QPM, capped so a large QPM cannot authorise an
	// unbounded simultaneous fan-out.
	Burst int `yaml:"burst"`
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
	// provider session instead of guessing one global budget. Negative is
	// rejected by validateProviderThresholds (review-whole.md I-1): unlike
	// AutoCompactThreshold, this field has no "negative means explicitly
	// disabled" reading — ResolveContextWindow's judge is `> 0`, so a
	// negative value silently fell through to the catalog/128K default
	// with no diagnostic, the one dimension out of four the review found
	// still silent.
	ContextWindow int `yaml:"context_window"`
	// Multimodal declares native image support (Tier G).
	// When the main model is non-multimodal, bootstrap auto-selects
	// the first Multimodal==true provider as the vision auxiliary.
	// Zero value (false) = text-only; valid default.
	Multimodal bool `yaml:"multimodal"`
	// Local forces this provider to be treated as locally served, overriding
	// the BaseURL/Kind heuristic. Set it when a local runtime sits behind a
	// hostname the heuristic cannot recognise (a reverse proxy, a container
	// DNS name); set it to an explicit false to opt a LAN-hosted gateway back
	// into the cloud catalog. *bool so "unset" (heuristic decides) stays
	// distinguishable from an explicit false. See einollm.IsLocalProvider.
	Local *bool `yaml:"local"`
	// AutoCompactThreshold overrides the auto-compact trigger point for THIS
	// provider's model, as a FRACTION of the resolved context window (e.g.
	// 0.8), never an absolute token count (ADR-0013's dimensional
	// constraint — mixing the two units silently mis-sizes the compaction
	// gate; validate() rejects a value > 1 at load time precisely because
	// that shape — an operator writing an absolute token budget like 8000 —
	// silently disables compaction instead of erroring, since the downstream
	// gate is `tokens < threshold*window`). 0 (the zero value) means "unset":
	// unlike CompactionConfig.Threshold, applyDefaults never coerces this
	// field, so 0 reliably means "no explicit override" and resolution falls
	// through to the model catalog, then to the operator's global
	// CompactionConfig.Threshold. A NEGATIVE value is a different signal —
	// an explicit per-provider DISABLE (W-C-04), independent of the global
	// switch. See einollm.ResolveAutoCompactThreshold (W-C-01 / INF2).
	AutoCompactThreshold float64 `yaml:"auto_compact_threshold"`

	// --- Generation parameters (M4) ----------------------------------------
	//
	// All three are POINTERS on purpose: for every one of them zero is a legal
	// value the operator may genuinely want (temperature 0 for deterministic
	// judge calls, top_p 0 for greedy decoding), so a plain int/float cannot
	// distinguish "not configured" from "configured to zero". nil means "leave
	// the provider default alone" and the field is omitted from the wire body
	// entirely, keeping an unconfigured provider byte-identical to before.

	// MaxTokens caps the tokens generated per response. nil keeps the adapter
	// default (4096), which silently truncates long patches — raise it when
	// the model is expected to emit whole files. Maps to `max_tokens`
	// (OpenAI Chat Completions, Anthropic Messages) and `max_output_tokens`
	// (OpenAI Responses).
	MaxTokens *int `yaml:"max_tokens"`
	// Temperature is the sampling temperature. nil keeps the provider default.
	// Lower it (0 .. 0.2) for judgement-shaped calls where a stable verdict
	// matters more than variety.
	Temperature *float32 `yaml:"temperature"`
	// TopP is nucleus sampling mass. nil keeps the provider default. Providers
	// generally recommend tuning this OR Temperature, not both.
	TopP *float32 `yaml:"top_p"`

	// QPM caps THIS provider's requests per minute (M7), overriding
	// llm.rate_limit.qpm. 0 inherits the global default. Set it per provider
	// when one plan is tighter than the rest — the limiter is keyed by model,
	// so a shared global would throttle the fast provider at the slow one's
	// rate.
	QPM int `yaml:"qpm"`
	// Burst is this provider's back-to-back allowance after an idle period.
	// 0 derives it from the effective QPM.
	Burst int `yaml:"burst"`

	// Headers (W-C-02) are extra HTTP headers sent with every request to this
	// provider — Azure's `api-key`, an enterprise gateway's auth token, a
	// tracing header a proxy requires. Values go through the same
	// os.ExpandEnv pass LoadBytes applies to the whole document before
	// unmarshal, so `${AZURE_KEY}` resolves like every other config value.
	//
	// SECURITY: a header value is exactly as sensitive as APIKey — it is
	// registered with the secrets.Redactor at boot (bootstrap.go, mirroring
	// the APIKey registration loop) so it cannot reach a log line, a crash
	// dump, or a compaction summary in the clear. Do not add a new sink that
	// reads ProviderConfig.Headers without registering its values first.
	Headers map[string]string `yaml:"headers"`

	// MaxRetries (W-C-07) overrides LLMConfig.MaxRetries for THIS provider.
	// nil (unset) inherits the global value — a plain int cannot distinguish
	// "operator did not say" from "operator explicitly wants 0 retries",
	// which is the same nil-means-omit convention MaxTokens/Temperature/TopP
	// already use above (M4). See ResilientConfig.PerProviderMaxRetries.
	MaxRetries *int `yaml:"max_retries"`

	// Auth (W-C-12) configures command-based token authentication: a
	// credential produced by running an external command rather than
	// supplied as a static APIKey. Nil means "use APIKey as usual". See
	// ProviderAuthConfig.
	Auth *ProviderAuthConfig `yaml:"auth"`

	// TruncationPolicy (W-C-09) overrides the head/tail line-retention policy
	// internal/tools/spillover.go applies to an oversized tool result before
	// it reaches the model, as "head=<N>,tail=<M>" (either key may be
	// omitted; see einollm.ParseTruncationPolicy for the exact grammar).
	// Empty (the zero value) means "no override": resolution falls through
	// to the model catalog (einollm.KnownTruncationPolicy), then to
	// einollm.DefaultTruncationSpec — see einollm.ResolveTruncationPolicy,
	// the mirror of ResolveAutoCompactThreshold above.
	//
	// UNLIKE AutoCompactThreshold, this field's FORMAT is deliberately NOT
	// validated here at load time. AutoCompactThreshold's check (validate's
	// "> 1" rejection below) is a plain numeric range test with no
	// dependency outside this package; validating a truncation_policy string
	// needs einollm.ParseTruncationPolicy, and internal/llm/eino already
	// imports internal/config (provider.go's providerShape) — importing it
	// back from here would be an import cycle (GOV1/R1). A malformed value
	// therefore does not fail config load: ResolveTruncationPolicy falls
	// through to the catalog/default exactly as if the field were empty, and
	// bootstrap.go logs a warning at boot time so a typo is still observable,
	// just not a refused start. See ResolveTruncationPolicy's doc comment for
	// the full reasoning.
	TruncationPolicy string `yaml:"truncation_policy"`

	// FallbackModels (M-2 / W-C-10) names, in priority order, the registry
	// model ids (llm.providers[].model — the SAME key context_window /
	// auto_compact_threshold / truncation_policy resolve against, not the
	// config `name` label) THIS provider's compaction-summary call falls
	// back to when it errors. Empty (the zero value) means "no override":
	// resolution falls through to the model catalog
	// (einollm.KnownFallbackModels), then to no fallback at all — see
	// einollm.ResolveFallbackModels, the fourth and last of the four
	// provider-level resolution ladders (context_window /
	// auto_compact_threshold / truncation_policy / fallback_models all
	// share the same "explicit field wins, then catalog, then nothing"
	// shape).
	//
	// Before this field existed, W-C-10 had NO config-level rung: the
	// shipped models.yaml ships zero fallback_models catalog rows (Ruling
	// RC-8), so with no override the feature was unreachable by any
	// deployment — buildProviderFallbacks always returned an empty map,
	// byte-identical to pre-W-C-10 behavior no matter what an operator
	// wrote, because there was nowhere to write it.
	//
	// An entry naming a model id no configured provider resolves to is
	// silently dropped (bootstrap's buildProviderFallbacks), not a load-time
	// error — the same tolerance the catalog path already has, since
	// validating an id here would need the provider registry this field is
	// itself an input to.
	FallbackModels []string `yaml:"fallback_models"`
}

// ProviderAuthConfig configures W-C-12 command-based token authentication for
// one provider: the credential is the stdout of running Command, refreshed
// every RefreshInterval and re-run once after a 401.
//
// SECURITY: Command is executed. It is configuration, not model input, but
// config is not automatically trusted either — a config.yaml can come from a
// shared template, a team repo, or (since agents write files through the
// same fs tools that can touch config.yaml) from the agent's own prior edit.
// The command therefore goes through secproc.Launch exactly like any other
// untrusted spawn (shell_run, an ACP agent CLI): it is Authorized against a
// purpose-built minimal profile, its environment is scrubbed of ambient
// credentials, and it runs under sandbox.FullAccess. It calls secproc.Launch
// directly rather than through internal/tools.LaunchSecureProcess (a thin,
// behavior-identical wrapper around the same call) because
// internal/llm/eino cannot import internal/tools — see runAuthCommand's doc
// comment in internal/llm/eino/cmdauth.go for the import-cycle this avoids.
// The Authorize firewall is unaffected either way: internal/tools' init
// registers internal/tools.Authorize as secproc.Launch's Authorizer
// process-wide, independent of which package calls Launch.
//
// No bootstrap-level wiring is needed for this specifically: the
// secproc.Factory the command spawns through is whatever the calling
// request's context already carries. Every orchestrator turn binds one
// unconditionally (orchestrator's effectiveSecureFactory falls back to
// shell.UnsandboxedSecureFactory when none is configured) — but that is NOT
// every caller of a provider's Generate/Stream. At least three production
// call sites invoke Generate/Stream on a context that never passed through
// bindExecutionContext, so no Factory is bound there: the SSE pre-turn
// compaction summarizer call in internal/api/http/chat.go, the WebSocket
// pre-turn compaction summarizer call in internal/api/http/ws_compaction.go
// (both call ctxcompact.MaybeCompactWithOptions, which may run the summary
// model against a provider that has auth.command configured), and the
// memory-distillation Generate call in internal/agent/upkeep/memory.go. Each
// of those gets the SAME fail-closed error any other secproc caller gets
// without a bound Factory (see runAuthCommand's doc comment in
// internal/llm/eino/cmdauth.go) — not a panic, not a silently-empty
// credential — but it is a real gap: if the provider chosen for compaction
// or memory distillation uses auth.command, that call fails until a Factory
// is threaded onto those contexts too. Nothing in this package enforces
// that; it is a property of which callers happen to route through
// bindExecutionContext today.
//
// SECURITY (B-2, 2026-08-29 review): the escalation the first paragraph above
// accepts as a given — config.yaml sits in the agent's own write scope by
// default, so an fs_write plus a restart chooses this Command — is closed by
// the trusted policy file the same way it is closed for `profiles:` and
// security.guardian_prompt_file: when a policy file exists (policy.go), every
// provider's Auth is replaced by policy.Security.ProviderAuth, keyed by
// ProviderConfig.Name, and a provider the trusted map does not name has its
// Auth cleared to nil regardless of what config.yaml says. See
// PolicySecurity.ProviderAuth's doc comment for the mechanism and
// ApplyPolicy's for the exact override semantics.
type ProviderAuthConfig struct {
	// Command is the argv to run: Command[0] is the program, the rest are
	// its arguments. The command's stdout, trimmed of trailing whitespace,
	// becomes the bearer/api-key credential for this provider. Required —
	// an Auth block with an empty Command is a config error (validate()).
	Command []string `yaml:"command"`

	// RefreshInterval is how long a cached credential is reused before the
	// command is re-run. 0 defaults to 15 minutes (applyDefaults) — long
	// enough that a fast-issuing command is not re-run every turn, short
	// enough that a typical cloud STS token (usually valid 15-60m) is
	// refreshed well before it expires. The command is also re-run once,
	// out of band from this interval, immediately after a 401 (see
	// CommandTokenSource.Refresh).
	RefreshInterval time.Duration `yaml:"refresh_interval"`
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
//
// It additionally applies the S3 trusted policy (see policy.go): when a policy
// file exists outside the working directory, it becomes the authority for
// `profiles:` and the local file may only narrow it.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	cfg, err := LoadBytes(data)
	if err != nil {
		return nil, err
	}
	// Applied HERE rather than inside LoadBytes because LoadBytes is the pure
	// bytes→Config function the test suite uses everywhere; reaching for the
	// environment and the home directory from it would make every one of those
	// tests depend on the developer's machine.
	//
	// A policy that exists but cannot be read or parsed FAILS the load. The
	// operator wrote it in order to constrain the agent, so falling back to the
	// unconstrained local profiles would drop the constraint at exactly the
	// moment it was being asked for.
	policy, err := LoadPolicy()
	if err != nil {
		return nil, err
	}
	cfg.PolicyNarrowed = cfg.ApplyPolicy(policy)
	cfg.PolicyActive = policy != nil && len(policy.Profiles) > 0
	cfg.PolicyFileActive = policy != nil
	// ApplyPolicy may have replaced security.guardian_prompt_file with the
	// trusted one, so the body LoadBytes read from the local value is stale and
	// was cleared. Re-read from whichever path is now authoritative, through the
	// same validating function — a trusted policy that names an unreadable or
	// hollow guardian file must fail the load for the reason loadGuardianPrompt
	// gives, not fall back to the file the agent could write.
	if err := cfg.loadGuardianPrompt(); err != nil {
		return nil, err
	}
	return cfg, nil
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

	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
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
	// W-C-12: an Auth block with no explicit refresh_interval gets 15m — see
	// ProviderAuthConfig.RefreshInterval's doc for why that value.
	for i := range c.LLM.Providers {
		if c.LLM.Providers[i].Auth != nil && c.LLM.Providers[i].Auth.RefreshInterval == 0 {
			c.LLM.Providers[i].Auth.RefreshInterval = 15 * time.Minute
		}
	}
}

func (c *Config) validate() error {
	if c.Subagents.Limit != 0 && (c.Subagents.Limit < 1 || c.Subagents.Limit > 20) {
		return errors.New("subagents.limit must be within 1..20")
	}
	if err := c.validateProviderThresholds(); err != nil {
		return err
	}
	if err := c.validateProviderRetriesAndAuth(); err != nil {
		return err
	}
	if err := c.loadGuardianPrompt(); err != nil {
		return err
	}
	if err := c.validateNetworkMethods(); err != nil {
		return err
	}
	if err := c.validateHooks(); err != nil {
		return err
	}
	return c.validateProfiles()
}

// validateHooks rejects a hook entry whose program is empty. The failure this
// closes is the silent one: yaml.v3 drops unknown/mistyped keys without a
// word, so "program:" with the value on the wrong indentation level would
// reach the orchestrator as an empty string and — because hook failures fail
// closed — refuse every tool call with "hook  failed: ..." at runtime, in
// every turn, forever. Load is the one moment the operator is guaranteed to
// be looking; the error names the event and the entry index.
//
// Presence of the FILE is deliberately not checked here: the program may sit
// on a different machine's PATH shape, be built after boot, or live behind a
// mount that comes up later. First use reports it fail-closed with the spawn
// error verbatim.
func (c *Config) validateHooks() error {
	for i, h := range c.Hooks.PreToolUse {
		if strings.TrimSpace(h.Program) == "" {
			return fmt.Errorf("hooks.pre_tool_use[%d]: program is required (a hook with no program would fail closed on every tool call)", i)
		}
	}
	// 压缩段（W-F-08）走同一条检查。错误文案不提 fail-closed —— 这两段
	// 的 hook 失败是 fail-open，空 program 的实际后果是每次压缩各记一条
	// 日志；仍然在加载期拒绝，是因为一个缩错进的空值同样只在运行期才显形，
	// 而日志不是操作员保证在看的地方。
	for _, section := range []struct {
		name  string
		hooks []HookConfig
	}{
		{"hooks.pre_compact", c.Hooks.PreCompact},
		{"hooks.post_compact", c.Hooks.PostCompact},
	} {
		for i, h := range section.hooks {
			if strings.TrimSpace(h.Program) == "" {
				return fmt.Errorf("%s[%d]: program is required", section.name, i)
			}
		}
	}
	return nil
}

// validateProviderThresholds rejects a per-provider auto_compact_threshold
// (F-3) greater than 1, and a per-provider context_window (review-whole.md
// I-1) that is negative.
//
// auto_compact_threshold: the field is a FRACTION of the resolved context
// window, never an absolute token count (ADR-0024 C3); a value above 1 is
// the tell-tale shape of an operator who wrote an absolute token budget
// instead (e.g. 8000, meant as "8000 tokens"). That mistake used to be
// silent: the value published straight through to ResolveAutoCompactThreshold
// / thresholdFor and permanently failed the downstream gate
// (tokens < threshold*window, e.g. 8000*context_window), which reads as
// "compaction never fires" with no diagnostic anywhere — the same failure
// shape ADR-0013 fixed once already for the global threshold, recurring here
// on the new per-model knob.
//
// 0 (unset, falls through to the catalog/global default) and any negative
// value (an explicit per-provider DISABLE, W-C-04/F-10 — see
// ProviderConfig.AutoCompactThreshold's doc) are both valid and intentionally
// NOT rejected here: only the positive-override lane has an upper bound,
// because only that lane is a ratio someone could mistake for a token count.
//
// context_window: unlike the threshold field, negative has no "explicit
// disable" reading here — ResolveContextWindow/ContextWindowFor both judge
// on `> 0`, so a negative value was previously indistinguishable from unset
// and fell through to the catalog/128K default with zero diagnostic. The
// review found this was the only one of four adjacent zero/negative/invalid
// provider-config readings (context_window, auto_compact_threshold,
// truncation_policy, max_retries) that was completely silent; the other
// three already reject, warn, or have a documented and deliberate meaning.
// This closes that gap the same way max_retries < 0 is already closed, by
// validateProviderRetriesAndAuth, in this same file.
func (c *Config) validateProviderThresholds() error {
	for i, p := range c.LLM.Providers {
		if p.AutoCompactThreshold > 1 {
			return fmt.Errorf("llm.providers[%d] (name=%q): auto_compact_threshold must be a fraction of the context window (<= 1), got %v — this looks like an absolute token count, not a ratio", i, p.Name, p.AutoCompactThreshold)
		}
		if p.ContextWindow < 0 {
			return fmt.Errorf("llm.providers[%d] (name=%q): context_window must be >= 0, got %d", i, p.Name, p.ContextWindow)
		}
	}
	return nil
}

// validateProviderRetriesAndAuth rejects the two shapes W-C-07 and W-C-12
// cannot mean anything sensible: a negative retry ceiling (a retry loop
// cannot run a command a negative number of times, so this is always an
// operator typo, not a policy) and an auth block with no command to run
// (nothing would ever produce a credential, so every call would fail closed
// with a confusing "auth command produced no token" rather than a clear
// config error at boot).
func (c *Config) validateProviderRetriesAndAuth() error {
	if c.LLM.MaxRetries < 0 {
		return fmt.Errorf("llm.max_retries must be >= 0, got %d", c.LLM.MaxRetries)
	}
	for i, p := range c.LLM.Providers {
		if p.MaxRetries != nil && *p.MaxRetries < 0 {
			return fmt.Errorf("llm.providers[%d] (name=%q): max_retries must be >= 0, got %d", i, p.Name, *p.MaxRetries)
		}
		if p.Auth != nil && len(p.Auth.Command) == 0 {
			return fmt.Errorf("llm.providers[%d] (name=%q): auth.command must not be empty when auth is configured", i, p.Name)
		}
		if p.Auth != nil && p.Auth.RefreshInterval < 0 {
			return fmt.Errorf("llm.providers[%d] (name=%q): auth.refresh_interval must be >= 0, got %v", i, p.Name, p.Auth.RefreshInterval)
		}
	}
	return nil
}

// validateNetworkMethods rejects a method rule whose verdict or subject is
// missing.
//
// Both checks refuse the start rather than dropping the rule, for the reason
// validateProfiles gives: an ignored security rule is discovered from traffic,
// months later, by someone who has no reason to suspect their config. A rule
// with no host names nothing; a rule with no action ("action: alow") would
// silently become a deny under any zero-value reading, which is the opposite
// verdict from the one an operator writing an allow rule intended.
//
// It does NOT refuse a method rule for an https host while inspect_https is
// off. That combination is inert rather than wrong — the rule applies the
// moment inspection is enabled, and plain-HTTP requests through the proxy
// carry a readable method with no inspection at all — so refusing it would
// reject a valid configuration. bootstrap says it out loud via slog instead
// (security posture belongs in the log file an operator audits later, not
// only on the terminal of whoever started the process — see bootstrap.go's
// own comment next to that slog.Warn call).
func (c *Config) validateNetworkMethods() error {
	for i, rule := range c.Security.Network.Methods {
		if strings.TrimSpace(rule.Host) == "" {
			return fmt.Errorf("security.network.methods[%d]: host is required", i)
		}
		switch strings.ToLower(strings.TrimSpace(rule.Action)) {
		case "allow", "deny":
		default:
			return fmt.Errorf("security.network.methods[%d] (host %q): action must be \"allow\" or \"deny\", got %q",
				i, rule.Host, rule.Action)
		}
	}
	return nil
}

// loadGuardianPrompt reads and validates security.guardian_prompt_file (W-B-14).
//
// Both failure modes refuse the start, for the same reason validateProfiles
// refuses an unknown shell policy: the value is not consulted until the first
// auto-mode call, so a silent fallback would be discovered as "auto mode is
// using the wrong policy" hours later, if ever. The prompt is the ENTIRE
// verdict in auto mode — there is no static list beside it — so "we ignored
// your policy file" is not a warning-level event.
func (c *Config) loadGuardianPrompt() error {
	path := strings.TrimSpace(c.Security.GuardianPromptFile)
	if path == "" {
		return nil
	}
	data, err := os.ReadFile(expandHome(path))
	if err != nil {
		return fmt.Errorf("security.guardian_prompt_file: %w", err)
	}
	body := string(data)
	if err := guard.ValidateAutoApprovalTemplate(body); err != nil {
		return fmt.Errorf("security.guardian_prompt_file %q: %w", path, err)
	}
	c.Security.GuardianPrompt = body
	return nil
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
