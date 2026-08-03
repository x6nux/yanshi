// Package bootstrap wires config into a running yanshi application:
// config → store → model → tools → orchestrator → HTTP server.
package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/x6nux/yanshi/internal/agent/orchestrator"
	apihttp "github.com/x6nux/yanshi/internal/api/http"
	apiV1 "github.com/x6nux/yanshi/internal/api/v1"
	"github.com/x6nux/yanshi/internal/approval"
	"github.com/x6nux/yanshi/internal/auth"
	"github.com/x6nux/yanshi/internal/config"
	"github.com/x6nux/yanshi/internal/features"
	"github.com/x6nux/yanshi/internal/guard"
	"github.com/x6nux/yanshi/internal/imagestore"
	"github.com/x6nux/yanshi/internal/instruct"
	einollm "github.com/x6nux/yanshi/internal/llm/eino"
	"github.com/x6nux/yanshi/internal/lsp"
	"github.com/x6nux/yanshi/internal/mcp"
	"github.com/x6nux/yanshi/internal/memory"
	"github.com/x6nux/yanshi/internal/netpolicy"
	obslog "github.com/x6nux/yanshi/internal/observe/log"
	otelobs "github.com/x6nux/yanshi/internal/observe/otel"
	"github.com/x6nux/yanshi/internal/sandbox"
	"github.com/x6nux/yanshi/internal/secproc"
	"github.com/x6nux/yanshi/internal/secrets"
	"github.com/x6nux/yanshi/internal/shell"
	"github.com/x6nux/yanshi/internal/skills"
	"github.com/x6nux/yanshi/internal/store"
	"github.com/x6nux/yanshi/internal/task"
	"github.com/x6nux/yanshi/internal/task/work"
	"github.com/x6nux/yanshi/internal/tools"
	"github.com/x6nux/yanshi/internal/vcs"

	agentregistry "github.com/x6nux/yanshi/internal/agent/registry"
)

// visionUsageAccumulator is the auxiliary model token usage sink (Tier G).
type visionUsageAccumulator struct {
	mu         sync.Mutex
	Prompt     int64
	Completion int64
	Total      int64
}

func (a *visionUsageAccumulator) add(prompt, completion, total int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.Prompt += int64(prompt)
	a.Completion += int64(completion)
	a.Total += int64(total)
}

// App holds the fully wired yanshi application.
type App struct {
	Server *http.Server
	Store  *store.Store
	Orch   *orchestrator.Orchestrator
	Broker *task.Broker
	Addr   string
	Model  model.BaseChatModel
	// Models is the per-name model registry (provider name → model), built from
	// the configured providers so a session can switch models mid-conversation
	// (Phase-10 /model). Empty when running with FakeModel or no providers; in
	// that case Model is the fake and switching is a no-op. The default
	// (resilient chain) stays in Model.
	Models        map[string]model.BaseChatModel
	VisionAux     model.BaseChatModel
	MultimodalMap map[string]bool
	ImageStore    *imagestore.Store
	VisionUsage   *visionUsageAccumulator

	// Skills is the loaded skill registry (M7); nil only if Build returned an
	// error before registry construction. Exposed for tests and diagnostics.
	Skills *skills.Registry

	// ToolNames lists the names of every tool registered with the
	// orchestrator, in registration order. Populated by Build from the
	// tools' own Info(). Exposed so GOV5 can assert the permission
	// profile's allow list and the registry agree — the audit found the
	// profile allowing nine shell tools that were never registered.
	ToolNames []string

	// AgentAPI is the versioned thread/turn/item service shared by HTTP and
	// JSON-RPC app-server transports. Non-nil after a successful Build; the
	// `yanshi app` subcommand consumes it directly.
	AgentAPI *apiV1.Service

	// VCS is the built-in autoVCS tracker (M8). vcs.New always returns a
	// non-nil instance; VCSRepoID is the main repo id from InitRepo, and is
	// empty when InitRepo failed at boot — in that case the app still runs
	// but the orchestrator runs without a scope (tracking disabled). Callers
	// must gate tracking on VCSRepoID != "".
	VCS       *vcs.VCS
	VCSRepoID string
	// SubagentManager is the process-wide managed sub-agent runtime (B1).
	SubagentManager *agentregistry.Manager
	// AgentTools carries all 12 agent tools (4 legacy + 8 lifecycle).
	AgentTools *tools.AgentTools
	// LSP is the post-edit diagnostics manager (soft-degrade: may be Enabled()==false).
	// Shutdown closes it to avoid gopls etc. sub-process leaks.
	LSP *lsp.Manager

	// MCP is the MCP connection manager (soft-degrade: may be Enabled()==false).
	MCP *mcp.Manager

	// VCSDBPath is the SQLite store path (cfg.Storage.SQLitePath) and
	// WorktreeDir is the expanded worktree working dir. Exposed so the goal
	// CLI can describe the yanshi-vcs MCP server to spawned ACP agents.
	VCSDBPath   string
	WorktreeDir string

	// Task 10/13: security posture. Sandbox is always non-nil after Build;
	// NetworkPolicy is always non-nil after Build. Both are injected into
	// every orchestrator turn so tools and the future SecureProcessFactory
	// can consult them via context.
	Sandbox       sandbox.Sandbox
	NetworkPolicy *netpolicy.Policy
	Approvals     *approval.Manager
	ShellManager  *shell.Manager
	SecureFactory secproc.Factory

	// Features is the runtime feature-flag registry (OBS3). Always non-nil
	// after Build; nil only when Build returned an error before registry
	// construction. Callers that want to TOGGLE flags from outside the HTTP
	// path (e.g. doctor O07, goal loop) read here.
	Features *features.Registry
	// Pricing is the per-model USD pricing table (COST1). Always populated
	// with the default + override merge; callers that compute costs read here
	// instead of re-deriving from config.
	Pricing map[string]einollm.ModelPricing
	// OTel is the OpenTelemetry runtime (OBS2). Holds the SDK tracer/meter
	// providers when OTel is enabled, or a no-op runtime otherwise. Shutdown
	// flushes and closes exporters before the store.
	OTel *otelobs.Runtime

	// Redactor is the process-wide secrets redactor (S10). Always non-nil
	// after Build; holds every provider API key resolved from secret:// /
	// env:// / legacy-insecure refs. The same instance is injected into the
	// Store and the HTTP Server so every WS/SSE/SQLite boundary redacts
	// uniformly. Callers that want to register additional secrets at runtime
	// (auth.Manager device tokens) read from here.
	Redactor *secrets.Redactor

	// Auth is the provider-neutral auth Manager (O03). Always non-nil after
	// Build; exposes Status / Logout / RunDeviceFlow for the CLI and doctor.
	// Composes the same secrets.Manager + Store used by S10 so device-flow
	// tokens land in the same backend and inherit the same redactor.
	Auth *auth.Manager

	// LogPath is the structured-log file path when logs were redirected
	// away from stderr (TUI mode or explicit observability.log.file).
	// Empty means logs go to stderr. The TUI surfaces this on the status
	// frame so /logs can tail the right file.
	LogPath string

	cancel context.CancelFunc // cancels the sweeper context
}

// Options configures Build.
type Options struct {
	ConfigPath string

	// FakeModel forces a deterministic fake model so the app boots without
	// any LLM API keys. It is also used automatically when no providers are
	// configured.
	FakeModel bool

	// TUIMode tells Build the process will drive an alt-screen Bubble Tea
	// TUI. When true (and the operator hasn't opted into stderr), logs are
	// redirected to a file so structured log lines do not corrupt the TUI
	// render. Headless modes leave this false and keep stderr.
	TUIMode bool

	// Cfg is a pre-loaded config used as a test seam. Production leaves it
	// nil and Build uses ConfigPath. When non-nil, ConfigPath is ignored and
	// the caller is responsible for cfg.applyDefaults (LoadBytes does this;
	// tests constructing Config{} literally should call LoadBytes or accept
	// zero-value defaults).
	Cfg *config.Config

	// Output is the process-wide redactor/logger aggregation. Production
	// main and cli.Session inject the same pointer so all soft-degradation
	// warnings route through one SafeLogger; nil is normalized by Build to
	// a default SafeOutput writing to os.Stderr.
	Output *secrets.SafeOutput

	// ProviderBuilder overrides einollm.BuildProviders. Tests inject a
	// recording implementation to prove it observes the resolved plaintext
	// APIKeys (i.e. resolution ran BEFORE BuildProviders). nil uses the real
	// einollm.BuildProviders.
	ProviderBuilder ProviderBuilder

	// AuthDeps lets tests inject RFC 8628 providers, a fake Clock, and a fake
	// Sleeper so device-flow timing is deterministic without real network
	// I/O. Production callers leave it zero — the loop below builds providers
	// from cfg.Auth.Device.Providers and uses real Clock/Sleeper.
	AuthDeps AuthDeps
}

// ProviderBuilder is the production BuildProviders signature. Returns the
// per-name model map, the chain passed to NewResilientModel, the per-model
// context windows, and an error. bootstrap calls it AFTER credential
// resolution so cfg.LLM.Providers[i].APIKey holds plaintext.
type ProviderBuilder func(*config.Config) (
	map[string]model.BaseChatModel,
	[]model.BaseChatModel,
	map[string]int,
	error,
)

// AuthDeps is the injection seam for auth-side collaborators. All fields
// optional; zero value means "use the real adapters / cfg-derived providers".
type AuthDeps struct {
	// Providers overrides the cfg-derived device providers. When non-empty,
	// bootstrap skips the cfg.Auth.Device.Providers loop and uses exactly
	// these (already-validated) providers — so tests can supply an httptest
	// endpoint without parsing YAML. IDs still must be unique; bootstrap
	// verifies that here rather than trusting the caller.
	Providers []DeviceProviderBinding

	// Clock / Sleeper are passed to auth.Manager so device flow tests can
	// drive timing deterministically. nil => realClock / realSleeper.
	Clock   auth.Clock
	Sleeper auth.Sleeper
}

// DeviceProviderBinding pairs an ID with an auth.DeviceProvider. The ID is
// the value users put under cfg.Auth.Device.Providers[i].id (and the key
// under which the provider is registered for RunDeviceFlow lookup).
type DeviceProviderBinding struct {
	ID       string
	Provider auth.DeviceProvider
}

// parseCooldownDuration parses the compaction cooldown_duration config string
// (e.g. "3s", "500ms"). An empty or unparseable value disables the
// time-based cooldown (returns 0) — malformed values never fail boot, matching
// the best-effort posture of compaction itself.
func parseCooldownDuration(s string) time.Duration {
	if s == "" {
		return 0
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0
	}
	return d
}

// DefaultOrchestratorProfile returns the factory-default permission profile
// for the orchestrator. The orchestrator no longer falls back to
// Tools={"*"}: when the operator did not configure profiles.orchestrator, we
// ship this concrete "coding" profile naming the tools the orchestrator
// actually uses, so a forgotten profile block stays least-privilege rather
// than fail-open. Operators who need shell/net widening must declare it in
// config.yaml.
//
// Exported so GOV5 (internal/bootstrap/wiring_test.go) can compare the
// shipped allow list against the shipped tool registry without having to
// reach into Build.
func DefaultOrchestratorProfile() guard.PermissionProfile {
	return guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{
			// NB: "fs_patch" and "fs_mkdir" used to be listed here and were
			// dropped — neither has ever been a registered tool. The patch
			// tool's real name is "apply_patch" (internal/tools/fs.go:99),
			// which is already allowed below; there is no mkdir tool at all.
			"fs_read", "fs_list", "fs_search", "fs_glob", "fs_write", "fs_edit",
			"shell_run", "shell_start", "shell_read", "shell_write_stdin", "shell_wait", "shell_cancel",
			"task_shell_start", "task_shell_wait", "task_shell_stdin", "task_shell_cancel",
			"memory_search", "memory_recall", "memory_write",
			"web_fetch", "web_search", "time_now", "skill_use", "vcs_*",
			"agent_start", "workflow_start", "analysis", "summarize",
			"apply_patch",
			// B3 developer tools
			"git_status", "git_diff", "run_tests", "diagnostics",
			"github_pr_context", "github_comment", "github_approve", "github_merge",
			"review",
		}},
		Net: guard.NetPerm{Allow: true},
	}
}

// Build loads configuration and wires every component together.
func Build(opts Options) (*App, error) {
	// S10 SafeOutput is the process-wide redactor + safe stderr logger. It
	// must exist BEFORE config loading so even load failures and all later
	// soft-degradation paths (VCS, LSP, plugins, MCP) route through the
	// redacting logger. Production main / cli.Session pass the same pointer
	// so the in-process backend and the CLI front-end share one sink.
	output := effectiveSafeOutput(opts.Output)

	// Install a safe redacting logger BEFORE config loading so even load
	// failures and all later soft-degradation paths (VCS, LSP, plugins, MCP)
	// route through the redacting handler. Once the config is available we
	// re-install with the operator's level/format preference.
	obslog.Setup(obslog.Config{Level: "info", Format: "json"})

	var cfg *config.Config
	if opts.Cfg != nil {
		cfg = opts.Cfg
	} else {
		loaded, err := config.Load(opts.ConfigPath)
		if err != nil {
			return nil, fmt.Errorf("bootstrap: load config: %w", err)
		}
		cfg = loaded
	}
	logWriter, logPath := resolveLogWriter(cfg.Observability.Log, opts.TUIMode)
	obslog.Setup(obslog.Config{
		Level:  cfg.Observability.Log.Level,
		Format: cfg.Observability.Log.Format,
		Writer: logWriter,
	})
	if logPath != "" {
		// Bootstrapping messages before obslog.Setup are already on stderr;
		// surface the log file path once so the user knows where logs went.
		if opts.TUIMode {
			fmt.Fprintf(os.Stderr, "yanshi: logs -> %s\n", logPath)
		}
	}

	// C4 OBS3 feature-flag registry: register built-in defaults, then apply
	// the operator's YAML overrides. Strict mode (config OR env) makes
	// ApplyMap fail fast on unknown names so a typo surfaces during boot
	// rather than silently disabling a flag the operator thought they set.
	featureReg := features.NewRegistry(cfg.Features.Strict)
	for _, spec := range features.DefaultSpecs() {
		featureReg.Register(spec)
	}
	if err := featureReg.ApplyMap(cfg.Features.Overrides); err != nil {
		return nil, fmt.Errorf("bootstrap: features: %w", err)
	}

	// C4 COST1 pricing table: layer the operator's overrides on top of the
	// built-in defaults. Overlay entries replace base entries on key collision
	// (so operators can correct stale default prices without code changes).
	overlay := make(map[string]einollm.ModelPricing, len(cfg.Pricing.Overrides))
	for modelName, price := range cfg.Pricing.Overrides {
		overlay[modelName] = einollm.ModelPricing{
			InputPerM:    price.InputPerM,
			CacheHitPerM: price.CacheHitPerM,
			OutputPerM:   price.OutputPerM,
		}
	}
	priceTab := einollm.MergePricing(einollm.DefaultPricing(), overlay)

	// C4 OBS2: install the process-wide OTel providers. OTel is gated by both
	// the config block AND the "observe.otel_export" feature flag (the flag
	// lets operators turn the export off at runtime via /features without
	// reloading config). Failure of any exporter construction degrades to a
	// no-op runtime (see otelobs.Setup); the app continues without telemetry.
	otelRT := otelobs.Setup(context.Background(), otelobs.Config{
		Enabled:     cfg.Observability.OTel.Enabled && featureReg.Enabled("observe.otel_export"),
		Endpoint:    cfg.Observability.OTel.Endpoint,
		ServiceName: cfg.Observability.OTel.ServiceName,
		SampleRatio: cfg.Observability.OTel.SampleRatio,
	})

	st, err := store.OpenWith(cfg.Storage.SQLitePath, store.OpenOptions{
		MaxOpenConns:      cfg.Storage.WALMaxOpenConns,
		BusyTimeoutMs:     cfg.Storage.BusyTimeoutMs,
		WALAutoCheckpoint: cfg.Storage.WALAutoCheckpoint,
	})
	if err != nil {
		return nil, fmt.Errorf("bootstrap: open store: %w", err)
	}
	// S10 cleanup guard: every early return below (skills load, MCP, work
	// store, agent API, orchestrator, http server) used to call st.Close()
	// inline. Centralizing the close-on-error here covers Task 5/8 new
	// returns and removes the per-site duplication. The last line before the
	// successful return flips closeStoreOnError to false.
	closeStoreOnError := true
	defer func() {
		if closeStoreOnError {
			_ = st.Close()
		}
	}()

	// S10: secrets.Manager + credential resolution. Done BEFORE
	// einollm.BuildProviders so the resolved plaintext APIKeys land in
	// cfg.LLM.Providers[i].APIKey and BuildProviders sees the final values.
	// legacyRaw is the per-process opt-in: only when Auth.LegacyInsecure is
	// explicitly true do raw literal APIKeys get accepted. Otherwise
	// ParseCredentialRef fails closed and Build aborts — silently accepting a
	// pasted API key would defeat S10's threat model.
	secretMgr, err := secrets.NewManager(secrets.Config{
		Backend:       cfg.Secrets.Backend,
		FilePath:      cfg.Secrets.FilePath,
		PassphraseEnv: cfg.Secrets.PassphraseEnv,
		Stderr:        outputLoggerWriter(output),
	})
	if err != nil {
		return nil, fmt.Errorf("bootstrap: secrets manager: %w", err)
	}
	redactor := output.Redactor
	if secretMgr != nil {
		// Merge so any secrets the Manager pre-registers (none today, but
		// reserved for device-auth tokens in Task 7) join the process-wide
		// registry that Store + Server receive below.
		redactor = secrets.MergeRedactors(redactor, secretMgr.Redactor())
	}

	// O03: auth.Manager takes over credential resolution from the inline
	// Task 5 loop. Ordering is load-bearing: authMgr is constructed, metadata
	// adapter attached, device providers registered, and ALL credential
	// sources resolved BEFORE einollm.BuildProviders sees cfg. BuildProviders
	// passes cfg.LLM.Providers[i].APIKey straight to provider SDK
	// constructors, so if resolution ran after, those SDKs would receive
	// refs instead of plaintext and every API call would 401.
	authMgr := auth.NewManager(secretMgr)
	authMgr.SetMetadataStore(store.AuthMetadataFromDB(st))
	if opts.AuthDeps.Clock != nil {
		authMgr.SetClock(opts.AuthDeps.Clock)
	}
	if opts.AuthDeps.Sleeper != nil {
		authMgr.SetSleeper(opts.AuthDeps.Sleeper)
	}

	// Build and validate the complete device-provider candidate set BEFORE
	// registering anything. Configured providers are inert unless
	// DeviceAuthEnabled is true; injected providers are an explicit test
	// override that stays active regardless of the config flag.
	if cfg.Auth.Device.DeviceAuthEnabled || len(opts.AuthDeps.Providers) > 0 {
		bindings, berr := buildDeviceProviders(
			cfg.Auth.Device.Providers,
			cfg.Auth.Device.ClientID,
			redactor,
			opts.AuthDeps,
		)
		if berr != nil {
			return nil, berr
		}
		for _, b := range bindings {
			authMgr.RegisterDeviceProvider(b.ID, b.Provider)
		}
	}

	// Resolve every provider credential source. Raw literals fail closed
	// unless Auth.LegacyInsecure is true; secret:// refs require a backend;
	// env:// refs read os.Getenv. Resolved plaintext is written back to
	// cfg.LLM.Providers[i].APIKey AND registered with the redactor so
	// WS/SSE/SQLite boundaries cannot leak it.
	for i := range cfg.LLM.Providers {
		p := &cfg.LLM.Providers[i]
		if p.APIKey == "" {
			continue
		}
		src := auth.CredentialSource{
			APIKeyRef:      p.APIKey,
			LegacyInsecure: cfg.Auth.LegacyInsecure,
		}
		resolved, rerr := authMgr.ResolveAPIKey(context.Background(), src)
		if rerr != nil {
			if cfg.Auth.AutoMigrate {
				_, parseErr := secrets.ParseCredentialRef(p.APIKey, false)
				if !errors.Is(parseErr, secrets.ErrRawLiteralRefused) && parseErr != nil {
					return nil, fmt.Errorf("bootstrap: resolve credentials for %s: %w", p.Name, rerr)
				}
				svc, acct := p.Name, "default"
				if storeErr := secretMgr.Set(svc, acct, p.APIKey); storeErr != nil {
					return nil, fmt.Errorf("bootstrap: auto-migrate store key for %s: %v (set auth.auto_migrate=false to disable)", p.Name, storeErr)
				}
				refStr := "secret://" + svc + "/" + acct
				cfgPath := opts.ConfigPath
				if cfgPath == "" {
					cfgPath = "config.yaml"
				}
				raw, readErr := os.ReadFile(cfgPath)
				if readErr == nil {
					oldKey := p.APIKey
					updated := strings.Replace(string(raw), oldKey, refStr, 1)
					if writeErr := os.WriteFile(cfgPath, []byte(updated), 0644); writeErr != nil {
						fmt.Fprintf(os.Stderr, "warning: auto-migrate: wrote ref to memory but could not update %s: %v\n", cfgPath, writeErr)
					}
				} else {
					fmt.Fprintf(os.Stderr, "warning: auto-migrate: stored key but could not read %s for rewrite: %v\n", cfgPath, readErr)
				}
				p.APIKey = refStr
				src.APIKeyRef = refStr
				resolved, rerr = authMgr.ResolveAPIKey(context.Background(), src)
				if rerr != nil {
					return nil, fmt.Errorf("bootstrap: auto-migrate resolve %s: stored but cannot re-read: %w", p.Name, rerr)
				}
			} else {
				return nil, fmt.Errorf("bootstrap: resolve credentials for %s: %w (set auth.auto_migrate=true to auto-store in secrets backend)", p.Name, rerr)
			}
		}
		if resolved != "" {
			redactor.Register(resolved)
			p.APIKey = resolved
		}
	}

	// Inject the redactor into Store (CreateSession / AppendMessage /
	// UpdateSessionTitle all redact before SQL write) — the Server gets it
	// via httpCfg.Redactor below.
	st.SetRedactor(redactor)

	// M8: build the autoVCS tracker over the same store and scan the working
	// directory into an initial main commit. A failure here is non-fatal — the
	// app still boots with tracking disabled (VCSRepoID stays empty) — so we
	// log to stderr and continue rather than rejecting the whole boot.
	workRoot, _ := os.Getwd()
	worktreeDir := expandHome(firstNonEmpty(cfg.VCS.WorktreeDir, "~/.yanshi/worktrees"))
	vcsInstance := vcs.New(
		st,
		worktreeDir,
		cfg.VCS.Ignore...,
	)
	vcsRepoID, vcsErr := vcsInstance.InitRepo(workRoot)
	if vcsErr != nil {
		obslog.WarnErr(context.Background(), "vcs initialization failed; tracking disabled", vcsErr)
	}

	// B2-LSP1: post-edit diagnostics with language servers. Soft-degrade - no available
	// server results in a no-op Manager; app boots normally.
	lspTimeout, terr := time.ParseDuration(cfg.LSP.Timeout)
	if terr != nil {
		lspTimeout = 800 * time.Millisecond
	}
	langServers := lsp.DefaultLanguages()
	for lang, spec := range cfg.LSP.Override {
		langServers[lang] = lsp.LanguageServer{Command: spec.Command, Args: spec.Args}
	}
	lspMgr := lsp.New(lsp.Config{
		WorkRoot:  workRoot,
		Languages: langServers,
		Timeout:   lspTimeout,
	})
	if cfg.LSP.Enabled != nil && !*cfg.LSP.Enabled {
		lspMgr = lsp.New(lsp.Config{WorkRoot: workRoot})
	}
	if !lspMgr.Enabled() {
		fmt.Fprintf(os.Stderr, "yanshi: lsp disabled (no language server found or enabled:false)\n")
	}

	// Choose model: fake when requested or when no providers are configured.
	// The fake response is a plain-text assistant message: turns now end when
	// the model stops naturally, and JudgeCompletion accepts the fake's reply
	// as complete (a non-JSON reply fails the judge's parse, which the judge
	// treats as complete — see JudgeCompletion). Repeat=true makes the model
	// emit this same response on every call, so multi-turn sessions (e.g. the
	// TUI) don't run out of scripted responses after turn 1.
	var chatModel model.BaseChatModel
	var providerModels map[string]model.BaseChatModel
	// providerWindows is keyed by the SAME registry key as providerModels
	// (the model id, e.g. "gpt-4o", NOT p.Name like "openai") — see
	// einollm.BuildProviders. Forwarded directly to the HTTP layer so
	// contextWindowFor(cs.model / req.Model, ...) hits the per-model window
	// instead of silently falling back to the global ContextWindow.
	var providerWindows map[string]int
	if opts.FakeModel || len(cfg.LLM.Providers) == 0 {
		fm := einollm.NewFakeModelWithMessages([]*schema.Message{
			schema.AssistantMessage("(no real model configured)", nil),
		}, nil)
		fm.Repeat = true
		chatModel = fm
	} else {
		providerBuilder := opts.ProviderBuilder
		if providerBuilder == nil {
			providerBuilder = einollm.BuildProviders
		}
		named, chain, windows, err := providerBuilder(cfg)
		if err != nil {
			return nil, fmt.Errorf("bootstrap: build providers: %w", err)
		}
		rm, err := einollm.NewResilientModel(chain, einollm.ResilientConfig{})
		if err != nil {
			return nil, fmt.Errorf("bootstrap: resilient model: %w", err)
		}
		chatModel = rm
		providerModels = named
		providerWindows = windows
	}

	// Tier G: multimodal map + vision aux.
	multimodalMap := make(map[string]bool)
	var visionAux model.BaseChatModel
	used := make(map[string]bool)
	for i, p := range cfg.LLM.Providers {
		key := p.Model
		if key == "" || used[key] {
			key = p.Name
		}
		if key == "" || used[key] {
			key = fmt.Sprintf("model-%d", i)
		}
		used[key] = true
		if providerModels != nil {
			if m, ok := providerModels[key]; ok {
				multimodalMap[key] = p.Multimodal
				if p.Multimodal && visionAux == nil {
					visionAux = m
				}
			}
		}
	}
	if visionAux == nil && len(cfg.LLM.Providers) > 0 {
		if !cfg.LLM.Providers[0].Multimodal {
			fmt.Fprintf(os.Stderr, "yanshi: vision auxiliary disabled (no provider has multimodal: true); image_describe returns a config error\n")
		}
	}
	imageStore := imagestore.New(imagestore.Config{MaxItems: 20, MaxBytes: 100 << 20})

	// Build tools.
	memTools := tools.NewMemoryTools(st)
	webTools := tools.NewWebTools(0, 0) // 0 → defaults: 1 MiB body, 30s timeout

	var visionUsageSink visionUsageAccumulator

	allTools := make([]orchestrator.BaseTool, 0, len(memTools.Tools())+1)
	for _, t := range memTools.Tools() {
		allTools = append(allTools, t)
	}
	allTools = append(allTools, webTools.Fetch, webTools.Search) // B3: web_fetch + web_search

	// M7: filesystem, shell, and time tools.
	fsTools := tools.NewFSTools(workRoot)
	shellTools := tools.NewShellTools(workRoot)
	timeTools := tools.NewTimeTools()
	for _, t := range fsTools.Tools() {
		allTools = append(allTools, t)
	}
	allTools = append(allTools, shellTools.Run, timeTools.Now)

	// Agent tools: agent_start, workflow_start, analysis, and summarize for
	// sub-agent delegation, parallel workflow execution, quick code analysis,
	// and file content condensation (fs_read-only sub-agent).
	agentTools := tools.NewAgentTools(chatModel)
	for _, t := range agentTools.Tools() {
		allTools = append(allTools, t)
	}

	// B3: developer tools — git status/diff, test runner, diagnostics, GitHub,
	// and the review pipeline.
	gitTools := tools.NewGitTools()
	allTools = append(allTools, gitTools.Status, gitTools.Diff)
	allTools = append(allTools, tools.NewTestRunTool())
	allTools = append(allTools, tools.NewDiagnosticsTool(nil))
	ghTools := tools.NewGitHubTools(nil)
	allTools = append(allTools, ghTools.PRContext, ghTools.Comment, ghTools.Approve, ghTools.Merge)
	allTools = append(allTools, agentTools.Review)

	// Build orchestrator with a profile resolved from config.
	profile := DefaultOrchestratorProfile()
	if cfg.Profiles != nil {
		if p, ok := cfg.Profiles["orchestrator"]; ok {
			profile = p
		}
	}

	// B1: build the subagent manager.
	bootID := fmt.Sprintf("%d-%d", os.Getpid(), time.Now().UnixNano())
	subagentManager := agentregistry.NewManager(agentregistry.NewManagerOpts{
		RootContext:   context.Background(),
		Path:          cfg.Subagents.PersistencePath,
		SessionBootID: bootID,
		MaxConcurrent: cfg.Subagents.Limit,
	})

	availableModels := make(map[string]bool, len(providerModels))
	for name := range providerModels {
		availableModels[name] = true
	}

	// MEM1: resolve user + project memory paths and compose the memory suffix
	// (independent of Instruction). Disabled yields "" so orchestrator's
	// MemorySuffix is a no-op, AND bootstrap gates remember-tool registration
	// and apihttp.Config.MemoryPath on Enabled too (SC2 consistency). Declaring
	// these values before the skills registry block keeps them in scope for the
	// remember append below, orchConfig, and httpCfg (CB6).
	var (
		memorySuffix string
		memUserPath  string
		memProjPath  string
	)
	if cfg.Memory.Enabled {
		memUserPath, memProjPath = resolveMemoryPaths(cfg.Memory, workRoot)
		memorySuffix = memory.ComposeBlock(true, memUserPath, memProjPath, cfg.Memory.MaxSize)
	}

	// M7: build the skill registry from config dirs + discovered plugins.
	// Resolve userSkillsDir first so the same path feeds both the loader's
	// User root and httpCfg.SkillsDstRoot (FN1: Install publishes exactly
	// where the loader will scan on Reload).
	userSkillsDir := expandHome(cfg.Skills.UserDir)
	if userSkillsDir == "" {
		if home := homeDirOrDefault(); home != "" {
			userSkillsDir = filepath.Join(home, ".yanshi", "skills")
		}
	}
	roots := []skills.Root{skills.Builtin(firstNonEmpty(cfg.Skills.BuiltinDir, "skills"))}
	if userSkillsDir != "" {
		roots = append(roots, skills.User(userSkillsDir))
	}
	if pd := expandHome(cfg.Skills.PluginDir); pd != "" {
		// Plugin discovery is non-fatal: a bad plugin root logs to stderr and
		// we continue without plugin skills rather than failing the whole boot.
		// pluginRoots and err are intentionally scoped to this block so the
		// outer err (reused below for registry load) is not shadowed.
		pluginRoots, err := skills.DiscoverPlugins(pd)
		if err != nil {
			obslog.WarnErr(context.Background(), "skill plugin discovery failed; plugin skills disabled", err)
		} else {
			roots = append(roots, pluginRoots...)
		}
	}
	skillLoader := skills.NewLoader(roots...)
	registry, err := skillLoader.Load()
	if err != nil {
		// store closed via closeStoreOnError guard
		return nil, fmt.Errorf("bootstrap: load skills: %w", err)
	}
	allTools = append(allTools, tools.NewSkillUseTool(registry))

	// MEM1: remember tool — appends to user/project memory file. Paths are
	// fixed at construction so the model cannot redirect writes via args.
	// SC2 consistency: when Memory.Enabled=false we DO NOT register remember,
	// so the model can never discover/call it. Gated on cfg.Memory.Enabled.
	if cfg.Memory.Enabled {
		allTools = append(allTools, tools.NewRememberTool(memUserPath, memProjPath))
	}

	// A2: work Store + Manager from the same SQLite connection.
	workStore, err := work.FromDB(st.DB, st)
	if err != nil {
		// store closed via closeStoreOnError guard
		return nil, fmt.Errorf("bootstrap: work store: %w", err)
	}
	dispRef := &work.DispatcherRef{}
	workMgr := work.NewManager(workStore, dispRef, work.ArtifactPolicy{})
	for _, t := range tools.NewTaskTools().Tools() {
		allTools = append(allTools, t)
	}
	for _, t := range tools.NewPlanTools().Tools() {
		allTools = append(allTools, t)
	}
	for _, t := range tools.NewGateTools().Tools() {
		allTools = append(allTools, t)
	}
	for _, t := range tools.NewArtifactTools().Tools() {
		allTools = append(allTools, t)
	}

	// M8: vcs_* GuardedTools (commit/log/diff/restore/merge). They read the
	// active VCSScope from context; the orchestrator injects the main scope
	// below when InitRepo succeeded.
	for _, t := range tools.NewVCSTools().Tools() {
		allTools = append(allTools, t)
	}

	// A3: MCP connection manager — soft-degrade: empty config yields a disabled
	// manager (Enabled()==false), tools.NewMCPTools returns nil, the orchestrator
	// skips context injection. Startup failures log to stderr but do not block.
	mcpManager := buildMCPManager(cfg)
	for _, t := range tools.NewMCPTools(mcpManager) {
		allTools = append(allTools, t)
	}

	// Tier G: image_describe + screenshot.
	allTools = append(allTools, tools.NewImageDescribeTool(
		visionAux, imageStore, workRoot, func(prompt, completion, total int) {
			visionUsageSink.add(prompt, completion, total)
		}))
	allTools = append(allTools, tools.NewScreenshotTool(imageStore))

	// GOV5 seam: snapshot the registered tool names while allTools is still
	// in scope. Info() is pure metadata on every tool implementation, so the
	// background context here can never block.
	toolNames := make([]string, 0, len(allTools))
	for _, tl := range allTools {
		info, err := tl.Info(context.Background())
		if err != nil {
			return nil, fmt.Errorf("tool registry: Info failed: %w", err)
		}
		toolNames = append(toolNames, info.Name)
	}

	// Load project-level prompt file (AGENT.md > CLAUDE.md) as the default
	// system instruction when present. This lets the project advertise custom
	// instructions to the agent, similar to how Claude Desktop uses CLAUDE.md.
	// I18N1: i18n.output_language appends an INDEPENDENT directive to the model
	// — it must never read i18n.ui_locale, because UI locale is a TUI concern
	// and the model never sees the TUI. Empty output_language means "follow
	// the user's input language" (no directive appended).
	instruction := AppendOutputLanguageInstruction(
		loadProjectPrompt(workRoot), cfg.I18N.OutputLanguage,
	)

	// Task 10/13: build the security posture from config and warn the operator
	// when Phase 0 is in effect. sandbox.New returns an honest CapabilityReport
	// even when OS isolation is not enforced, so the rest of the system can
	// label itself correctly. The tier string maps to sandbox.AccessTier; an
	// unrecognized value falls through to ReadOnly (fail-safe).
	sandboxTier := sandbox.ReadOnly
	switch strings.ToLower(cfg.Security.Sandbox.Tier) {
	case "workspace-write":
		sandboxTier = sandbox.WorkspaceWrite
	case "full-access":
		sandboxTier = sandbox.FullAccess
	}
	sb := sandbox.New(sandbox.Config{
		Enabled:       cfg.Security.Sandbox.Enabled == nil || *cfg.Security.Sandbox.Enabled,
		WorkspaceRoot: workRoot,
		Tier:          sandboxTier,
		NetworkDeny:   cfg.Security.Sandbox.NetworkDeny,
	})
	if report := sb.Report(); report.Effective != sandbox.OSIsolated {
		fmt.Fprintf(os.Stderr, "yanshi: sandbox phase0 (%s): %s; OS/network isolation NOT enforced\n",
			report.Effective, report.Reason)
	}
	networkPolicy := &netpolicy.Policy{
		Default:      cfg.Security.Network.Default,
		Allow:        append([]string(nil), cfg.Security.Network.Allow...),
		Deny:         append([]string(nil), cfg.Security.Network.Deny...),
		AllowPrivate: cfg.Security.Network.AllowPrivate,
	}

	// Approval manager + audit bus: one process-wide manager mirrors persistent
	// (allow_persistent) rules to the store; one bus fans the manager's emit
	// callback out to N WS subscribers (each connection subscribes a channel so
	// permission_rule_hit frames can render live). The manager is shared across
	// all WS connections; connection-scoped rules use TTL=session with a stable
	// connectionSessionID minted in ws.go.
	approvalBus := approval.NewAuditBus()
	approvalMgr, err := approval.New(st, "yanshi", approvalBus.Publish)
	if err != nil {
		// store closed via closeStoreOnError guard
		return nil, fmt.Errorf("bootstrap: approval manager: %w", err)
	}

	// Shell manager + secure factory (Task 21): persistent session manager
	// with job persistence and the production launch pipeline.
	shellManager := shell.NewManager(shell.Config{
		Root:           workRoot,
		MaxOutputBytes: cfg.Security.Shell.MaxOutputBytes,
		IdleTimeout:    cfg.Security.Shell.IdleTimeout,
	})
	if st != nil {
		shellManager = shellManager.WithPersistence(shell.JobFromKV(st))
		if err := shellManager.RestoreJobs(); err != nil {
			// store closed via closeStoreOnError guard
			return nil, fmt.Errorf("bootstrap: restore shell jobs: %w", err)
		}
	}
	secureFactory := shell.DefaultSecureFactory{
		OS:       shell.OSProcessFactory{},
		Policy:   networkPolicy,
		ProxyURL: "",
		Sandbox:  sb,
	}

	orchConfig := orchestrator.Config{
		Model:           chatModel,
		Tools:           allTools,
		Profile:         profile,
		Instruction:     instruction,
		SkillMetaPrompt: registry.MetaPrompt(),
		MemorySuffix:    memorySuffix,
		WorkRoot:        workRoot,
		TaskManager:     workMgr,
		SubagentManager: subagentManager,
		AvailableModels: availableModels,
		// Security posture (Task 10/13/21). These MUST be part of the literal:
		// orchestrator.New takes Config by value and the package has no
		// setters, so assigning them after New writes to a discarded copy and
		// every tools.With* injection in bindExecutionContext silently no-ops.
		Sandbox:       sb,
		NetworkPolicy: networkPolicy,
		Approvals:     approvalMgr,
		ShellManager:  shellManager,
		SecureFactory: secureFactory,
		LSP:           lspMgr,
		MCP:           mcpManager,
		MultimodalMap: multimodalMap,
		ImageStore:    imageStore,
		Compaction: orchestrator.CompactionConfig{
			Threshold:         cfg.Compaction.Threshold,
			ContextWindow:     cfg.Compaction.ContextWindow,
			KeepRecent:        cfg.Compaction.KeepRecent,
			CooldownTokens:    int(cfg.Compaction.CooldownFraction * float64(cfg.Compaction.ContextWindow)),
			CooldownDuration:  parseCooldownDuration(cfg.Compaction.CooldownDuration),
			HardForceFraction: cfg.Compaction.HardForceFraction,
		},
	}
	// Wire the main scope (Agent="orchestrator") so chat/orchestrator edits
	// auto-track to main. Only set when InitRepo succeeded; otherwise the
	// orchestrator runs without a scope (no tracking) and a caller-supplied
	// scope (e.g. a task-broker worktree scope) is still respected.
	if vcsRepoID != "" {
		orchConfig.VCSScope = tools.VCSScope{VCS: vcsInstance, RepoID: vcsRepoID, Agent: "orchestrator"}
	}
	orch, err := orchestrator.New(orchConfig)
	if err != nil {
		// store closed via closeStoreOnError guard
		return nil, fmt.Errorf("bootstrap: orchestrator: %w", err)
	}
	// Clear leftover oversized-output temp files from a previous run before any
	// tool can write new ones. Best-effort: a missing dir is a no-op.
	tools.Sweep(workRoot)

	// V14: construct the shared agent API service over the orchestrator + store.
	// HTTP and the JSON-RPC app-server both consume this single instance so
	// thread/turn/item semantics cannot drift between transports. The default
	// model is the bootstrap chat model (fake when no providers are configured);
	// the per-name map lets thread/start and turn/start switch models by id.
	agentAPI, err := apiV1.NewService(apiV1.Config{
		Orchestrator: orch,
		DefaultModel: chatModel,
		Models:       providerModels,
		Store:        st,
	})
	if err != nil {
		// store closed via closeStoreOnError guard
		return nil, fmt.Errorf("bootstrap: agent api: %w", err)
	}

	// Build HTTP server and register routes. providerWindows is the
	// BuildProviders-returned map keyed by the registry's model id (see above);
	// it is nil for the FakeModel path, which is what contextWindowFor expects
	// (falls back to Compaction.ContextWindow).
	httpCfg := apihttp.Config{
		Token: cfg.Token,
		Compaction: apihttp.CompactionConfig{
			Threshold:       cfg.Compaction.Threshold,
			KeepRecent:      cfg.Compaction.KeepRecent,
			ContextWindow:   cfg.Compaction.ContextWindow,
			Model:           cfg.Compaction.Model,
			ProviderWindows: providerWindows,
		},
		Store:         st,
		VCS:           vcsInstance,
		RepoID:        vcsRepoID,
		Approvals:     approvalMgr,
		ApprovalAudit: approvalBus,
		ShellManager:  shellManager,
		MCP:           mcpManager,
	}
	if cfg.Memory.Enabled {
		// SC2: only surface the path when the subsystem is on. Empty otherwise.
		httpCfg.MemoryPath = memUserPath
	}
	httpCfg.LogPath = logPath
	// E03: skills registry + the ORIGINAL loader (FN1: all roots Reload) +
	// the writable user root. SkillsCloner stays nil so production Install
	// uses realClone (real git clone).
	httpCfg.SkillsRegistry = registry
	httpCfg.SkillsLoader = skillLoader
	httpCfg.SkillsDstRoot = userSkillsDir
	// C4: pricing table + feature registry. Both are passed via Config so the
	// http package stays decoupled from bootstrap internals. Constraint 13 in
	// the C4 plan: never type-assert app.Server.Handler back to *apihttp.Server
	// to refill these — they ride on Config.
	httpCfg.PriceTab = priceTab
	httpCfg.FeaturesReg = featureReg
	// S10: inject the process-wide redactor so the SSE writeSSEFrame and
	// the WS wsConn.write boundaries redact every outbound frame.
	httpCfg.Redactor = redactor
	srv := apihttp.New(httpCfg)
	srv.Chat(orch, providerModels, registry)
	srv.ChatWS(orch, providerModels, registry) // WebSocket endpoint (TUI primary transport)
	srv.AgentV1(agentAPI)                      // V14 versioned resource API (thread/turn/item)
	srv.Sessions(st)

	// Wire the Task API: create a broker backed by the same store, register
	// the task endpoints on the same server, and start the heartbeat sweeper.
	broker := task.NewBroker(st, 3, 30*time.Second)
	// M8: when the VCS + repo are available, wire them into the broker so Claim
	// auto-assigns a fresh worktree to each claimed task. Gated on VCSRepoID so
	// a failed InitRepo leaves the broker on its no-worktree behavior.
	if vcsRepoID != "" {
		broker.SetVCS(vcsInstance, vcsRepoID)
	}
	srv.TaskAPI(broker, cfg.Profiles)

	ctx, cancel := context.WithCancel(context.Background())
	// A2: Bind work dispatcher to the broker and start the artifact janitor.
	dispRef.Bind(work.BrokerAdapter{Broker: broker})
	work.StartArtifactJanitor(ctx, workStore, workRoot, 6*time.Hour, work.DefaultArtifactTTL)

	broker.StartSweeper(ctx, 10*time.Second)

	srv.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	addr := cfg.Server.HTTPAddr
	if addr == "" {
		addr = "127.0.0.1:8080"
	}

	httpServer := &http.Server{
		Addr:    addr,
		Handler: srv.Handler(),
	}

	// Successful Build: hand store ownership to the caller. The deferred
	// closeStoreOnError guard becomes a no-op; App.Shutdown still closes the
	// store via a.Store.Close().
	closeStoreOnError = false
	return &App{
		Server:          httpServer,
		Store:           st,
		Orch:            orch,
		Broker:          broker,
		Addr:            addr,
		Model:           chatModel,
		Models:          providerModels,
		VisionAux:       visionAux,
		MultimodalMap:   multimodalMap,
		ImageStore:      imageStore,
		VisionUsage:     &visionUsageSink,
		AgentAPI:        agentAPI,
		Skills:          registry,
		ToolNames:       toolNames,
		VCS:             vcsInstance,
		VCSRepoID:       vcsRepoID,
		VCSDBPath:       cfg.Storage.SQLitePath,
		WorktreeDir:     worktreeDir,
		Sandbox:         sb,
		NetworkPolicy:   networkPolicy,
		SubagentManager: subagentManager,
		AgentTools:      agentTools,
		LSP:             lspMgr,
		MCP:             mcpManager,
		Approvals:       approvalMgr,
		ShellManager:    shellManager,
		SecureFactory:   secureFactory,
		Features:        featureReg,
		Pricing:         priceTab,
		OTel:            otelRT,
		Redactor:        redactor,
		Auth:            authMgr,
		LogPath:         logPath,
		cancel:          cancel,
	}, nil
}

// resolveLogWriter decides where structured logs go. Priority:
//  1. cfg.File (explicit operator path) — opened append, created if missing;
//  2. TUI mode + not StderrInTUI → default ~/.yanshi/logs/yanshi.log;
//  3. nil (obslog defaults to stderr).
//
// Returns the writer and the resolved path ("" for stderr). The path is
// surfaced on the status frame so the TUI /logs command tails the right file.
func resolveLogWriter(cfg config.LogConfig, tuiMode bool) (io.Writer, string) {
	if cfg.File != "" {
		w, err := openLogFile(cfg.File)
		if err == nil {
			return w, cfg.File
		}
		// Fall through to TUI/stderr paths; the operator will see boot
		// fail elsewhere if the path is truly unusable.
		fmt.Fprintf(os.Stderr, "yanshi: could not open log file %s: %v; falling back\n", cfg.File, err)
	}
	if tuiMode && !cfg.StderrInTUI {
		dir, err := defaultLogDir()
		if err == nil {
			path := filepath.Join(dir, "yanshi.log")
			if mkErr := os.MkdirAll(dir, 0755); mkErr == nil {
				if w, oErr := openLogFile(path); oErr == nil {
					return w, path
				}
			}
		}
	}
	return nil, ""
}

// openLogFile opens path for append, creating it (and its parent dir) if
// missing. The caller owns closing; bootstrap never closes the log file so
// it lives for the process lifetime and flushes naturally on exit.
func openLogFile(path string) (io.Writer, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(abs, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return nil, err
	}
	return f, nil
}

// defaultLogDir returns the canonical yanshi log directory under the OS
// user-config dir (e.g. ~/.yanshi/logs on Unix, %AppData%/yanshi/logs on
// Windows).
func defaultLogDir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "yanshi", "logs"), nil
}

// effectiveSafeOutput returns the caller-provided output or a default that
// writes to os.Stderr with a fresh empty Redactor. The default is used by
// callers that don't care about secret redaction (unit tests, the doctor
// subcommand) so they still get a non-nil sink.
func effectiveSafeOutput(output *secrets.SafeOutput) *secrets.SafeOutput {
	if output != nil {
		return output
	}
	return secrets.NewSafeOutput(os.Stderr, nil)
}

// outputLoggerWriter returns the writer behind output.Logger, or os.Stderr if
// the SafeOutput doesn't carry one. We need a plain io.Writer for
// secrets.Config.Stderr; the SafeLogger wrapping happens inside secrets.
func outputLoggerWriter(output *secrets.SafeOutput) io.Writer {
	if output == nil || output.Logger == nil {
		return os.Stderr
	}
	// The SafeLogger does not expose its writer; pass os.Stderr directly
	// since secrets.NewSafeLogger already wraps it through the same Redactor.
	return os.Stderr
}

// buildDeviceProviders returns a fully validated candidate set. It performs
// ALL validation before Build registers anything, so a duplicate/empty ID or
// invalid URL cannot leave auth.Manager partially configured.
//
// Injection rule: when deps.Providers is non-empty it replaces (not merges
// with) configProviders. This makes tests deterministic and avoids ambiguous
// duplicate precedence. Production leaves deps empty and uses config only.
// Returning an error here (rather than panicking) lets bootstrap wrap the
// cause with the provider id and surface it as a Build failure.
func buildDeviceProviders(
	configProviders []config.DeviceProviderConfig,
	defaultClientID string,
	redactor *secrets.Redactor,
	deps AuthDeps,
) ([]DeviceProviderBinding, error) {
	candidates := append([]DeviceProviderBinding(nil), deps.Providers...)
	if len(candidates) == 0 {
		for _, dp := range configProviders {
			clientID := strings.TrimSpace(dp.ClientID)
			if clientID == "" {
				clientID = strings.TrimSpace(defaultClientID)
			}
			provider, err := auth.NewGenericRFC8628Provider(auth.GenericRFC8628Config{
				ClientID:  clientID,
				DeviceURL: dp.DeviceURL,
				TokenURL:  dp.TokenURL,
				Scopes:    append([]string(nil), dp.Scopes...),
				Redactor:  redactor,
			})
			if err != nil {
				return nil, fmt.Errorf("bootstrap: device provider %q: %w", dp.ID, err)
			}
			candidates = append(candidates, DeviceProviderBinding{
				ID:       dp.ID,
				Provider: provider,
			})
		}
	}

	seen := make(map[string]bool, len(candidates))
	for _, c := range candidates {
		if strings.TrimSpace(c.ID) == "" {
			return nil, fmt.Errorf("bootstrap: device provider id must not be empty")
		}
		if c.Provider == nil {
			return nil, fmt.Errorf("bootstrap: device provider %q is nil", c.ID)
		}
		if seen[c.ID] {
			return nil, fmt.Errorf("bootstrap: duplicate device provider id %q", c.ID)
		}
		seen[c.ID] = true
	}
	return candidates, nil
}

// AppendOutputLanguageInstruction layers the i18n.output_language directive
// on top of base. Empty outputLanguage returns base unchanged so the model
// follows the user's input language by default. A non-empty value with an
// empty base uses orchestrator.DefaultInstruction so the directive never
// REPLACES the orchestrator's capability description. The directive itself
// preserves code/commands/identifiers/paths/quoted text — only the natural-
// language response language is constrained.
//
// I18N1 design: ui_locale and output_language are independent dimensions.
// The model never sees ui_locale; only this directive steers its output.
// Reading ui_locale here would couple model behavior to TUI state and
// break that contract. Exported so tests can lock the independence
// contract without a full Build.
func AppendOutputLanguageInstruction(base, outputLanguage string) string {
	outputLanguage = strings.TrimSpace(outputLanguage)
	if outputLanguage == "" {
		return base
	}
	if strings.TrimSpace(base) == "" {
		base = orchestrator.DefaultInstruction
	}
	directive := "Respond to the user in " + outputLanguage +
		". Keep code, commands, identifiers, file paths, and quoted source text unchanged."
	return strings.TrimRight(base, "\n") + "\n\n" + directive
}

// Start begins listening for HTTP connections.
func (a *App) Start() error {
	return a.Server.ListenAndServe()
}

// Serve serves HTTP on the given listener and blocks until it is closed. Used
// by the in-process CLI path to bind an ephemeral loopback port
// (net.Listen("tcp","127.0.0.1:0")) and read back the chosen address —
// ListenAndServe does not expose the chosen port. Shutdown still works.
func (a *App) Serve(ln net.Listener) error {
	return a.Server.Serve(ln)
}

// Shutdown gracefully shuts down the HTTP server, stops the broker sweeper,
// flushes OTel exporters, and closes the store.
func (a *App) Shutdown(ctx context.Context) error {
	if a.cancel != nil {
		a.cancel()
	}
	var errs []error
	if err := a.Server.Shutdown(ctx); err != nil {
		errs = append(errs, err)
	}
	// Close the ShellManager BEFORE the store so pending jobs are flushed.
	if a.ShellManager != nil {
		if cerr := a.ShellManager.Close(); cerr != nil {
			errs = append(errs, cerr)
		}
	}
	if a.SubagentManager != nil {
		a.SubagentManager.Close()
	}
	if a.LSP != nil {
		_ = a.LSP.Close()
	}
	if a.MCP != nil {
		a.MCP.Shutdown()
	}
	// Flush OTel exporters BEFORE the store so spans/metrics referencing
	// store-backed attributes are emitted while the store is still open.
	if a.OTel != nil {
		if err := a.OTel.Shutdown(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	if a.Store != nil {
		if cerr := a.Store.Close(); cerr != nil {
			errs = append(errs, cerr)
		}
	}
	return errors.Join(errs...)
}

// firstNonEmpty returns the first non-empty argument, or "skills" if all are
// empty (the conventional default builtin skills directory).
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return "skills"
}

// expandHome resolves a leading "~" to the user's home directory. Empty input
// returns empty so callers can treat "not configured" and "configured" uniformly.
func expandHome(p string) string {
	if p == "" {
		return ""
	}
	if strings.HasPrefix(p, "~") {
		home, err := os.UserHomeDir()
		if err == nil {
			return filepath.Join(home, p[1:])
		}
	}
	return p
}

// loadProjectPrompt returns the root-level project instructions, preferring
// AGENTS.md then AGENT.md then CLAUDE.md. It delegates to instruct.LoadHierarchical
// so the root level uses the SAME fallback order and hard size cap as the nested
// per-file injection (A11); at the root level the chain is a single directory, so
// this is equivalent to "read the first present root instruction file" (the
// loader wraps it in a descriptive "## Instructions" header, which is harmless in
// the system prompt). An empty string is returned when none exists, letting the
// orchestrator use its built-in default instruction.
func loadProjectPrompt(dir string) string {
	return instruct.LoadHierarchical(dir, dir)
}

// resolveMemoryPaths returns the (userPath, projectPath) absolute paths for the
// memory subsystem. userPath: cfg.UserPath if set (with ~ expanded), else
// ~/.yanshi/memory.md. projectPath: cfg.ProjectPath if set (resolved against
// workRoot), else <workRoot>/.yanshi/memory.md. Both may be "" when the source
// is intentionally disabled (e.g. projectPath when workRoot is ""). Caller
// MUST gate the call on cfg.Memory.Enabled (SC2).
func resolveMemoryPaths(cfg config.MemoryConfig, workRoot string) (userPath, projectPath string) {
	if cfg.UserPath != "" {
		userPath = expandHome(cfg.UserPath)
	} else if home := homeDirOrDefault(); home != "" {
		userPath = filepath.Join(home, ".yanshi", "memory.md")
	}
	switch {
	case cfg.ProjectPath != "" && filepath.IsAbs(cfg.ProjectPath):
		projectPath = cfg.ProjectPath
	case cfg.ProjectPath != "" && workRoot != "":
		projectPath = filepath.Join(workRoot, cfg.ProjectPath)
	case cfg.ProjectPath != "":
		projectPath = cfg.ProjectPath
	case workRoot != "":
		projectPath = filepath.Join(workRoot, ".yanshi", "memory.md")
	}
	return userPath, projectPath
}

// homeDirOrDefault returns the user home dir, or "" if unavailable (the caller
// treats "" as "skip this source").
func homeDirOrDefault() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return home
}

func buildMCPManager(cfg *config.Config) *mcp.Manager {
	servers := make(map[string]*mcp.ServerConfig, len(cfg.MCP.Servers))
	for name, sc := range cfg.MCP.Servers {
		d := 30 * time.Second
		if sc.Timeout != "" {
			if parsed, err := time.ParseDuration(sc.Timeout); err == nil {
				d = parsed
			}
		}
		transport := mcp.TransportStdio
		if sc.Transport == "http" {
			transport = mcp.TransportHTTP
		}
		servers[name] = &mcp.ServerConfig{
			Name: name, Enabled: sc.Enabled, Transport: transport,
			Command: sc.Command, Args: sc.Args, Env: sc.Env,
			URL: sc.URL, Bearer: sc.Bearer, Timeout: d, Reconnect: sc.Reconnect,
		}
	}
	mgr := mcp.NewManager(servers)
	for _, st := range mgr.StartAll(context.Background()) {
		if st.Status == mcp.StatusFailed {
			fmt.Fprintf(os.Stderr, "yanshi: mcp server %q failed: %s\n", st.Name, st.Error)
		}
	}
	return mgr
}
