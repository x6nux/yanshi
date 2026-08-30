// Package bootstrap wires config into a running yanshi application:
// config → store → model → tools → orchestrator → HTTP server.
package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/x6nux/yanshi/internal/agent/automation"
	"github.com/x6nux/yanshi/internal/agent/orchestrator"
	apihttp "github.com/x6nux/yanshi/internal/api/http"
	apiV1 "github.com/x6nux/yanshi/internal/api/v1"
	"github.com/x6nux/yanshi/internal/approval"
	"github.com/x6nux/yanshi/internal/auth"
	"github.com/x6nux/yanshi/internal/config"
	"github.com/x6nux/yanshi/internal/features"
	"github.com/x6nux/yanshi/internal/imagestore"
	"github.com/x6nux/yanshi/internal/instruct"
	einollm "github.com/x6nux/yanshi/internal/llm/eino"
	"github.com/x6nux/yanshi/internal/lockfile"
	"github.com/x6nux/yanshi/internal/lsp"
	"github.com/x6nux/yanshi/internal/mcp"
	"github.com/x6nux/yanshi/internal/memory"
	"github.com/x6nux/yanshi/internal/netpolicy"
	obslog "github.com/x6nux/yanshi/internal/observe/log"
	otelobs "github.com/x6nux/yanshi/internal/observe/otel"
	"github.com/x6nux/yanshi/internal/proto"
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

	"github.com/x6nux/yanshi/internal/agent/upkeep"
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

	// ServerCompaction is the apihttp.CompactionConfig actually handed to
	// apihttp.New — the pre-turn (WS/SSE) half of the W-C-01 (INF2)
	// per-model window/threshold ladder, the sibling of Orch's own copy
	// (Orchestrator.CompactionForTest, the mid-turn half). Captured here,
	// at the point Build already computes it, rather than read back off
	// Server: Server is *net/http.Server (net/http, not apihttp), and
	// constraint 13 of the C4 plan forbids type-asserting its Handler back
	// to *apihttp.Server to refill fields that only exist to be read by
	// tests — the same reason ToolNames/ToolTimeouts above are captured at
	// the source rather than reconstructed from the assembled object.
	ServerCompaction apihttp.CompactionConfig

	// Background owns tool calls moved to the background when they hit their
	// foreground deadline (T3). Closed by Shutdown; a wedged subprocess would
	// otherwise outlive the process that spawned it.
	Background *tools.BackgroundManager

	// ToolTimeouts maps each registered tool's name to the default execution
	// timeout it was constructed with. Taken from the assembled registry
	// rather than from the source, for the same reason ToolNames is: a
	// registration that never reaches the orchestrator cannot be checked by
	// reading the call site.
	//
	// Its purpose is a wiring assertion. A tool registered with timeout 0 gets
	// an already-expired context and fails on its first line every turn,
	// returning "context deadline exceeded" as a tool RESULT — so the model
	// retries in a loop and nothing ever crashes or logs. Eight agent_* tools
	// shipped that way. NewGuardedTool now panics on 0, and this snapshot is
	// the second line: it also catches a tool that reaches the registry
	// through some path that skips the constructor.
	ToolTimeouts map[string]time.Duration

	// LaunchProxyURLs maps each production launch factory to the managed-proxy
	// URL it publishes to its children ("secproc" and "shell_v2").
	//
	// Its purpose is a wiring assertion, for the same reason ToolNames and
	// ToolTimeouts are exposed: the two factories share one childLaunchPosture,
	// so their env SEMANTICS cannot drift — and that is exactly why nothing
	// noticed when their INPUTS did. shell_v2's literal omitted ProxyURL while
	// secproc's set it, so every shell v2 tool published an empty http_proxy,
	// which a child reads as "no proxy" and answers by connecting directly.
	// Reading the call site could not catch it; comparing the assembled values
	// can.
	LaunchProxyURLs map[string]string

	// NetProxy is the managed egress proxy, or nil when it failed to start
	// (which is non-fatal — see the Build site). Shutdown closes it.
	NetProxy *netpolicy.Proxy

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
	// ToolBatch is the T12 batch tool (tool_batch). Exposed for the same
	// reason ToolNames is: its registration is two-step (construct before the
	// name snapshot, Bind after it), and only the assembled value can show
	// that the second step happened. A tool_batch present in ToolNames but
	// never bound answers every call with a wiring error — a failure the
	// registry snapshot alone cannot distinguish from a working tool.
	ToolBatch *tools.ToolBatchTool

	// Automation is the C1 scheduled-task manager, or nil when C1 soft-degraded.
	//
	// Exposed because `yanshi schedule` must operate through THIS instance:
	// Manager serialises its read-modify-write with an in-process mutex, so a
	// second process reading the tables directly would race the scheduler's own
	// tick and produce a listing that is only correct until the next one. The
	// operator cannot tell that apart from a scheduler bug, which is why the
	// CLI fails loudly against a daemon that has none rather than falling back
	// to a direct read.
	Automation *automation.Manager

	// BootConfig is the configuration this App was assembled from.
	//
	// Exposed so `yanshi daemon reload` can diff a freshly loaded config
	// against what is actually running and report only genuinely changed
	// sections. Without a baseline the control handler must call every section
	// changed — noisy, and it teaches operators to ignore the output.
	BootConfig *config.Config

	// LSP is the post-edit diagnostics manager (soft-degrade: may be Enabled()==false).
	// Shutdown closes it to avoid gopls etc. sub-process leaks.
	LSP *lsp.Manager

	// MCP is the MCP connection manager (soft-degrade: may be Enabled()==false).
	MCP *mcp.Manager

	// mcpHealthCancel stops the MCP health loop. Unexported because nothing
	// outside Shutdown has any business stopping it: a caller that cancelled
	// it early would leave every dead server permanently marked Ready.
	mcpHealthCancel context.CancelFunc

	// Upkeep is the W-D background maintenance loop (cold-session compression,
	// cross-session memory extraction). Nil only when the store is nil.
	//
	// Shutdown must Wait() on it for the same reason C1Scheduler does: a sweep
	// in flight is mid-transaction, and closing the store underneath it turns a
	// routine compression into "database is closed" — with the rows already
	// deleted if the timing is unlucky.
	Upkeep *upkeep.Worker

	// C1Scheduler is the automation tick loop started by BuildAutomation as a
	// goroutine over the root context. Nil when C1 failed to build.
	//
	// Shutdown must Wait() on it, not merely cancel it: a tick in flight is
	// mid-transaction against the store, and closing the store underneath it
	// yields "database is closed" errors and a run row stuck in `running`
	// forever. Cancelling only asks it to stop; Wait proves it did.
	C1Scheduler *automation.Scheduler

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

	// SelfHeal lets Build move an UNREADABLE database aside and start an empty
	// one instead of failing. It defaults to false, and the default is the
	// safety property: healing renames the user's history away, which only a
	// process that owns the database has any business doing.
	//
	// Build has six callers and they are not alike. Two set this:
	//
	//   - cmd/yanshi runServe — the daemon; it IS the backend for the project.
	//   - cli.bootstrapOwner, and only when the caller is the interactive TUI
	//     — it wins the lockfile election and becomes the backend. This is the
	//     scenario healing exists for: a corrupt yanshi.db otherwise means the
	//     TUI never comes up and the user has no second tool to repair it.
	//
	// The other four must not, because they are short-lived processes attached
	// to someone else's database, and several of them run concurrently:
	//
	//   - runACPServer — one per editor window.
	//   - runPR — one-shot.
	//   - runApp — a JSON-RPC subprocess.
	//   - runGoal — a batch run; failing loudly is more useful than discarding
	//     history, since a human is reading its output.
	//
	// cli's non-interactive entries (exec, headless) share bootstrapOwner but
	// leave this false for the same reason: they report the error to a user who
	// is right there and can act on it, rather than deciding to discard data.
	SelfHeal bool

	// WorkRoot overrides the directory autoVCS scans into its initial main
	// commit. Production leaves it empty and Build uses os.Getwd().
	//
	// It exists because that Getwd call made the soft-degrade branch below
	// unreachable from a test: InitRepo only fails on a root it cannot
	// canonicalise, and a test cannot make the process's own cwd invalid.
	// TestBuild_VCSSoftDegrade therefore asserted only that App.VCS was
	// non-nil after a NORMAL boot — the degraded path it names had never
	// executed. Pointing this at a non-existent directory makes
	// canonicalRepoRoot's EvalSymlinks fail, which is reliable on every
	// platform.
	WorkRoot string

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
// context windows, the per-model auto-compact thresholds (W-C-01 / INF2),
// the per-model truncation policies (M-4), the per-model declared
// compaction-summary fallback ids (M-2 / W-C-10), and an error. bootstrap
// calls it AFTER credential resolution so cfg.LLM.Providers[i].APIKey holds
// plaintext.
//
// The trailing ...einollm.SecretRegistrar (W-C-12 review B-2) is how Build
// hands einollm.BuildProviders the SAME redactor instance every other
// dynamic credential (APIKey, Headers) is already registered with, so an
// auth.command-produced token gets that protection too. A test
// ProviderBuilder that ignores the parameter is unaffected — nil registrar
// simply disables registration, same as every other soft-degradation
// default in einollm.
type ProviderBuilder func(*config.Config, ...einollm.SecretRegistrar) (
	map[string]model.BaseChatModel,
	[]model.BaseChatModel,
	map[string]int,
	map[string]float64,
	map[string]einollm.TruncationSpec,
	map[string][]string,
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
		// Returning 0 disables the time cooldown, which is indistinguishable
		// from having left the key empty on purpose. Everywhere else this repo
		// rejects a malformed config value outright (see
		// guard.ValidateShellPolicy via Config.validateProfiles), but doing so
		// here would refuse to start over one optional tuning knob. Warn
		// instead, matching how bootstrap already reports a subsystem that
		// degraded rather than failed.
		fmt.Fprintf(os.Stderr,
			"yanshi: compaction.cooldown_duration %q is not a duration (%v): time-based cooldown disabled\n", s, err)
		return 0
	}
	return d
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
		// Forwarded, never hardcoded. Build has six callers and only two of
		// them own the database; see Options.SelfHeal.
		SelfHeal: opts.SelfHeal,
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

	// secrets.Manager backs device-flow token storage only. Provider api_keys
	// do NOT pass through it: config takes them verbatim (a literal, or the
	// value os.ExpandEnv already substituted for ${VAR}) and they reach the
	// provider SDKs unchanged. The Manager is still built here because
	// auth.Manager needs its Store for RFC 8628 tokens.
	secretMgr, err := secrets.NewManager(secrets.Config{
		Backend:       cfg.Secrets.Backend,
		FilePath:      cfg.Secrets.FilePath,
		PassphraseEnv: cfg.Secrets.PassphraseEnv,
		Stderr:        outputLoggerWriter(output),
	})
	if err != nil {
		return nil, fmt.Errorf("bootstrap: secrets manager: %w", err)
	}
	// redactor MUST stay aliased to output.Redactor for the whole function:
	// output.Logger was built over that pointer, so rebinding this variable to
	// a fresh union would take SafeLogger out of the loop for every
	// Register(resolved) below. Absorb folds in place precisely so this
	// assignment never needs to move. See Redactor.Absorb's doc.
	redactor := output.Redactor
	if secretMgr != nil {
		// Absorb so any secrets the Manager pre-registers (none today, but
		// reserved for device-auth tokens) join the process-wide registry
		// that Store + Server + SafeLogger all share.
		redactor.Absorb(secretMgr.Redactor())
	}

	// O03: auth.Manager owns RFC 8628 device flows and their token storage.
	// It no longer resolves provider api_keys — those are plaintext by the
	// time config finishes loading.
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

	// Provider api_keys are used verbatim: config accepts a literal or an
	// ${VAR} that os.ExpandEnv already substituted, and BuildProviders hands
	// that string straight to the provider SDK. Nothing is resolved here, but
	// every key is still registered with the redactor — that is what keeps it
	// out of logs, WS/SSE frames and SQLite, and it is independent of where
	// the key came from.
	for i := range cfg.LLM.Providers {
		key := cfg.LLM.Providers[i].APIKey
		if key == "" {
			continue
		}
		// Register silently drops values below secrets.MinSecretLength.
		// Silence is right inside the Redactor (it cannot tell a junk value
		// from a real one) but wrong here, where we know this string is meant
		// to be a credential: a key that short will not be redacted anywhere,
		// and the operator should hear about it rather than discover it in a
		// log. The warning names neither the value nor its length.
		if len(key) < secrets.MinSecretLength {
			output.Logger.Printf("warning: provider %q: api_key is too short to redact safely; it will appear verbatim in logs and stored messages\n", cfg.LLM.Providers[i].Name)
		}
		redactor.Register(key)
	}

	// W-C-02: provider Headers are extra HTTP headers, most commonly a
	// gateway token (Azure API key, enterprise proxy auth) rather than
	// arbitrary text. Register every value with the redactor for the exact
	// same reason as APIKey above — a header is at least as likely to carry
	// a credential as the api_key field is, and it flows through the same
	// logs/WS/SSE/SQLite surfaces the redactor already guards. Header NAMES
	// are never secret and are not registered; only values are.
	for i := range cfg.LLM.Providers {
		for _, v := range cfg.LLM.Providers[i].Headers {
			if v == "" {
				continue
			}
			if len(v) < secrets.MinSecretLength {
				output.Logger.Printf("warning: provider %q: a configured header value is too short to redact safely; it will appear verbatim in logs and stored messages\n", cfg.LLM.Providers[i].Name)
			}
			redactor.Register(v)
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
	workRoot := opts.WorkRoot
	if workRoot == "" {
		workRoot, _ = os.Getwd()
	}
	worktreeDir := expandHome(firstNonEmpty(cfg.VCS.WorktreeDir, "~/.yanshi/worktrees"))
	vcsInstance := vcs.New(
		st,
		worktreeDir,
		cfg.VCS.Ignore...,
	)
	// V7: hand the VCS the platform PID-liveness probe. internal/vcs is a port
	// package and GOV1's allowlist does not admit internal/lockfile, so the
	// probe is INJECTED rather than imported (see internal/vcs/worktreestate.go).
	// Without this line the orphan scan reports nothing at all — fail-safe, so
	// nothing is ever deleted wrongly, but the whole feature is inert and the
	// inertness is invisible: an empty orphan list is exactly what a healthy
	// repo also returns.
	vcsInstance.SetProcessAlive(func(pid int) bool {
		return lockfile.Lockfile{PID: pid}.Alive()
	})
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
	langServers := lspLanguages(cfg.LSP.Override)
	lspMgr := lsp.New(lsp.Config{
		WorkRoot:  workRoot,
		Languages: langServers,
		Timeout:   lspTimeout,
	})
	if cfg.LSP.Enabled != nil && !*cfg.LSP.Enabled {
		lspMgr = lsp.New(lsp.Config{WorkRoot: workRoot})
	}
	if !lspMgr.Enabled() {
		slog.Warn("lsp disabled", "reason", "no language server found or enabled:false")
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
	// providerThresholds mirrors providerWindows for the auto-compact
	// threshold (W-C-01 / INF2) — same registry key, forwarded to both the
	// orchestrator's mid-turn CompactionConfig and the http layer's pre-turn
	// one so a per-model catalog/config threshold reaches both compaction
	// paths (see CLAUDE.md's compaction section on why both must be wired).
	var providerThresholds map[string]float64
	// providerTruncationPolicies (M-4) mirrors providerWindows/
	// providerThresholds for W-C-09's tool-output truncation policy — same
	// registry key, forwarded to orchestrator.Config.ProviderTruncationPolicies
	// so a turn running on a NON-primary provider (reached via /model) gets
	// that provider's own truncation_policy override/catalog entry instead of
	// the primary provider's, which is all `truncationPolicy` below (the
	// single-value fallback) ever resolves. Before this map existed,
	// ProviderConfig.TruncationPolicy's doc comment documented a per-provider
	// override that only ever took effect for cfg.LLM.Providers[0].
	var providerTruncationPolicies map[string]einollm.TruncationSpec
	// providerFallbacks (W-C-10, config rung added by M-2) maps registry key
	// -> resolved compaction-summary fallback chain, computed once by
	// buildProviderFallbacks from providerModels + BuildProviders' declared
	// map (config override, else the embedded catalog) and forwarded to
	// both the orchestrator's mid-turn CompactionConfig and the http
	// layer's pre-turn one, mirroring providerWindows/providerThresholds
	// exactly. nil on the fake-model path (buildProviderFallbacks is only
	// called in the real-provider branch below), which fallbacksFor/its
	// apihttp twin already treat as "no fallback for anyone".
	var providerFallbacks map[string][]model.BaseChatModel
	// fakeChatModel stays nil on the production path ON PURPOSE: SelectRLMModel
	// treats a non-nil fake as "rlm_query may fall back to it", so handing it a
	// real model here would silently defeat the cheap-provider requirement and
	// route rlm_query at the expensive main model. Only the fake branch below
	// assigns it.
	var fakeChatModel model.BaseChatModel
	if opts.FakeModel || len(cfg.LLM.Providers) == 0 {
		fm := einollm.NewFakeModelWithMessages([]*schema.Message{
			schema.AssistantMessage("(no real model configured)", nil),
		}, nil)
		fm.Repeat = true
		// Report a plausible token count. A fake that reports zero makes every
		// accounting path — the /cost line, the goal loop's budget, the
		// persisted run record — look correct while measuring nothing, and
		// --fake-model is the mode most of those paths are exercised in. The
		// numbers are arbitrary; being non-zero is the point.
		fm.Usage = &schema.TokenUsage{PromptTokens: 12, CompletionTokens: 8, TotalTokens: 20}
		chatModel = fm
		fakeChatModel = fm
	} else {
		providerBuilder := opts.ProviderBuilder
		if providerBuilder == nil {
			providerBuilder = einollm.BuildProviders
		}
		// redactor (B-2): the SAME instance APIKey/Headers were just
		// registered with above, so an auth.command-produced token gets
		// identical protection — see ProviderBuilder's doc comment.
		named, chain, windows, thresholds, truncations, fallbackDecls, err := providerBuilder(cfg, redactor)
		if err != nil {
			return nil, fmt.Errorf("bootstrap: build providers: %w", err)
		}
		// M5/M6/M7/M10/C6: decorate each provider BEFORE the failover chain is
		// assembled. See BuildAdaptiveModels for why inside and not outside.
		named, chain = BuildAdaptiveModels(named, chain, adaptiveDeps{
			Cfg: cfg, Store: st, Windows: windows, Redactor: redactor,
		})
		// W-C-07: PerProviderMaxRetries[i] tracks cfg.LLM.Providers[i] one for
		// one — BuildProviders (and any ProviderBuilder standing in for it)
		// appends to chain in the SAME order it iterates cfg.LLM.Providers,
		// which is why this can be derived here rather than plumbed back
		// through BuildProviders' return signature. -1 (not 0, a legitimate
		// "never retry") is the "not set" sentinel maxRetriesFor reads.
		perProviderMaxRetries := make([]int, len(cfg.LLM.Providers))
		for i, p := range cfg.LLM.Providers {
			if p.MaxRetries != nil {
				perProviderMaxRetries[i] = *p.MaxRetries
			} else {
				perProviderMaxRetries[i] = -1
			}
		}
		rm, err := einollm.NewResilientModel(chain, einollm.ResilientConfig{
			FirstChunkTimeout:     cfg.LLM.StreamFirstChunkTimeout,
			IdleTimeout:           cfg.LLM.StreamIdleTimeout,
			MaxRetries:            cfg.LLM.MaxRetries,
			PerProviderMaxRetries: perProviderMaxRetries,
		})
		if err != nil {
			return nil, fmt.Errorf("bootstrap: resilient model: %w", err)
		}
		chatModel = rm
		providerModels = named
		providerWindows = windows
		providerThresholds = thresholds
		providerTruncationPolicies = truncations
		providerFallbacks = buildProviderFallbacks(named, fallbackDecls)
		// M9: warn about a model name the provider does not list, naming the
		// nearest matches. Non-blocking by construction — see RunPreflight.
		// It takes context.Background() rather than the app's root context
		// because that context does not exist yet at this point in Build, and
		// because RunPreflight imposes its own timeout: the probe must be
		// bounded by how long a boot may pause, not by the process lifetime.
		RunPreflight(context.Background(), cfg)
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
			slog.Warn("vision auxiliary disabled",
				"reason", "no provider has multimodal: true",
				"effect", "image_describe returns a config error")
		}
	}
	imageStore := imagestore.New(imagestore.Config{MaxItems: 20, MaxBytes: 100 << 20})

	// Build tools.
	backgroundMgr := BuildBackgroundManager()
	memTools := tools.NewMemoryTools(st)
	webTools := tools.NewWebTools(0, 0) // 0 → defaults: 1 MiB body, 30s timeout

	var visionUsageSink visionUsageAccumulator

	allTools := make([]orchestrator.BaseTool, 0, len(memTools.Tools())+1)
	for _, t := range memTools.Tools() {
		allTools = append(allTools, t)
	}

	// C2: session-history recall. The durable conversation log (C1) is written
	// by the WS layer before compaction evicts anything; without these tools
	// the model has no way to read it back, so eviction stays forgetting
	// instead of becoming paging. S8 also requires the registration — an
	// unregistered name is refused by toolreg.Check at runtime.
	for _, t := range tools.NewHistoryTools(st).Tools() {
		allTools = append(allTools, t)
	}

	// W-C-14: context_new_window — the model-driven counterpart to C2's
	// read-back tools above. Both exist because compaction is lossy in
	// different directions: C2 lets the model recover detail AFTER it was
	// evicted, this lets the model skip straight to a trimmed window BEFORE
	// the threshold gate would ever fire, when it already knows it does not
	// need what is about to be dropped.
	//
	// W-C-11: context_budget — a read-only query the model can make BEFORE
	// deciding whether context_new_window is worth calling at all. Both tools
	// are registered from the same tools.NewContextWindowTools().Tools()
	// call, so a future third tool added to that package needs no change
	// here.
	for _, t := range tools.NewContextWindowTools().Tools() {
		allTools = append(allTools, t)
	}

	// S6: give permission decisions a durable sink. Before this they reached
	// slog only, so under yolo/auto "what did it approve last night" was
	// unanswerable once the terminal scrolled. A write failure is swallowed by
	// the sink — the archive must never be able to change a guard verdict.
	tools.SetPermissionAuditSink(&tools.StoreAuditSink{
		Append: func(rec tools.PermissionAuditRecord) error {
			return st.AppendPermissionAudit(store.PermissionAudit{
				SessionID:  rec.SessionID,
				AgentID:    rec.AgentID,
				Tool:       rec.Tool,
				Decision:   rec.Decision,
				Source:     rec.Source,
				ReasonCode: rec.ReasonCode,
				CmdDigest:  rec.CmdDigest,
			})
		},
	})

	allTools = append(allTools, webTools.Fetch, webTools.Search) // B3: web_fetch + web_search

	// M7: filesystem, shell, and time tools.
	fsTools := tools.NewFSTools(workRoot)
	shellTools := tools.NewShellTools(workRoot)
	timeTools := tools.NewTimeTools()
	for _, t := range fsTools.Tools() {
		allTools = append(allTools, t)
	}
	allTools = append(allTools, shellTools.Run, timeTools.Now)

	// W-B-12: the model asks for a specific permission BEFORE hitting the wall.
	// Registered unconditionally and with no constructor arguments — everything
	// it needs (profile, approval manager, permission callback, work root)
	// rides the turn context. Its grant path fails closed on a transport with
	// no interactive channel, so registering it on SSE too costs nothing and
	// keeps the tool schema identical across transports.
	allTools = append(allTools, tools.NewRequestPermissionTool())

	// Shell v2 (W1): the ten persistent-session / background-job tools. They
	// are constructed here — before the toolNames snapshot — even though the
	// shell.Manager they drive is not built until the security posture below,
	// because the tools reach the manager through the per-turn context
	// (tools.WithShellManager), never through a constructor argument. Wiring
	// them any later would silently drop them from the registry while the
	// default profile keeps advertising the names (the GOV5 failure this fixes).
	shellV2 := tools.NewShellV2Tools(workRoot)
	allTools = append(allTools, shellV2.Start, shellV2.Read, shellV2.Write, shellV2.Wait,
		shellV2.Cancel, shellV2.Resize, shellV2.TaskStart, shellV2.TaskWait, shellV2.TaskWrite,
		shellV2.TaskCancel)

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

	// T1: LSP code navigation. Registered UNCONDITIONALLY — deliberately not
	// gated on lspMgr.Enabled(). The tools read the manager from the turn
	// context and, when none is bound or none is enabled, return a
	// model-facing "unavailable, use fs_search instead" RESULT rather than a
	// Go error. Gating registration would make the schema itself differ
	// between machines that happen to have gopls installed and machines that
	// do not, so a model trained on the former hallucinates the names on the
	// latter — and a hallucinated name is a worse failure than an honest
	// "unavailable", because it aborts the turn instead of redirecting it.
	for _, t := range tools.NewLSPNavTools().Tools() {
		allTools = append(allTools, t)
	}

	ghTools := tools.NewGitHubTools(nil)
	allTools = append(allTools, ghTools.PRContext, ghTools.Comment, ghTools.Approve, ghTools.Merge)
	allTools = append(allTools, agentTools.Review)

	// Build orchestrator with a profile resolved from config.
	//
	// usingDefaultProfile records whether the operator overrode it: only the
	// factory default gets extended with the conditionally-registered tools
	// further down (see extendProfileWithConditionalTools). The extension must
	// wait for the toolNames snapshot, which is not taken until the whole
	// registry — C1 included — has been assembled.
	profile := DefaultOrchestratorProfile()
	usingDefaultProfile := true
	if cfg.Profiles != nil {
		if p, ok := cfg.Profiles["orchestrator"]; ok {
			profile = p
			usingDefaultProfile = false
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

	// A compaction.model naming something unregistered is silently ignored at
	// use time: compactionModel falls through to the session model, so the
	// operator keeps paying full price for summaries and nothing says why.
	// Checked here rather than during config validation because the registry
	// key comes from chooseKey; recomputing that rule inside config would be
	// one more place for it to drift.
	if name := cfg.Compaction.Model; name != "" && !availableModels[name] {
		fmt.Fprintf(os.Stderr,
			"yanshi: compaction.model %q is not a configured provider: summaries will use the session model\n", name)
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
	// T7: let the model record a reusable procedure as a skill. Nil when there
	// is no user skills dir to write into (see BuildSkillWriteTool).
	if swt := BuildSkillWriteTool(userSkillsDir, registry, skillLoader); swt != nil {
		allTools = append(allTools, swt)
	}
	// T3 background query surface, C7 milestones, T9 ACP delegation.
	allTools = append(allTools, BuildBackgroundTools()...)
	allTools = append(allTools, BuildMilestoneTools(st)...)
	allTools = append(allTools, BuildACPDelegateTool(cfg.Storage.SQLitePath, worktreeDir))

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
	// L7: WithVerifyRoot makes file_exists checklist conditions resolvable. An
	// empty root leaves every such condition unsatisfied by design, which
	// blocks task completion rather than silently passing it.
	workMgr := work.NewManager(workStore, dispRef, work.ArtifactPolicy{}).WithVerifyRoot(workRoot)

	// The broker, the root context and the dispatcher binding are created HERE
	// rather than next to srv.TaskAPI further down, because C1's A2Adapter
	// needs a live broker while the tool registry is still being assembled,
	// and BuildAutomation needs the root context to derive its scheduler
	// goroutine from. srv.TaskAPI / StartArtifactJanitor / StartSweeper stay
	// below: they depend on srv, which does not exist yet.
	broker := task.NewBroker(st, 3, 30*time.Second)
	// M8: when the VCS + repo are available, wire them into the broker so Claim
	// auto-assigns a fresh worktree to each claimed task. Gated on VCSRepoID so
	// a failed InitRepo leaves the broker on its no-worktree behavior.
	if vcsRepoID != "" {
		broker.SetVCS(vcsInstance, vcsRepoID)
	}
	ctx, cancel := context.WithCancel(context.Background())
	// Cancel-on-error guard, the ctx twin of closeStoreOnError above. It is
	// load-bearing: BuildAutomation starts `go scheduler.Start(ctx)`, and that
	// goroutine only exits when this ctx is cancelled. Without the guard, every
	// error return between here and the successful one would leak a live
	// scheduler ticking against a store that closeStoreOnError just closed.
	cancelOnError := true
	defer func() {
		if cancelOnError {
			cancel()
		}
	}()
	// A2: bind the work dispatcher to the broker.
	dispRef.Bind(work.BrokerAdapter{Broker: broker})
	// …and the return path. Without it the broker moves its own row through
	// pending → running → completed while the durable task_work row stays at
	// pending forever: work.Manager.Start and work.Manager.Finish had no
	// production caller at all, so task_read reported "pending" for a task
	// that had already finished.
	mirror := work.NewLifecycleMirror(workMgr)
	mirror.OnError = func(workTaskID string, err error) {
		slog.Warn("durable task status update failed", "task_id", workTaskID, "err", err)
	}
	broker.Work = mirror
	// Restart recovery, before the sweeper starts: a row still marked running
	// after a restart describes a worker that no longer exists, and nothing
	// else would ever move it. Non-fatal — a store that cannot be recovered
	// still serves new tasks.
	if n, rerr := workMgr.RecoverInterrupted(ctx); rerr != nil {
		slog.Warn("durable task recovery failed", "err", rerr)
	} else if n > 0 {
		slog.Info("returned interrupted durable tasks to the queue", "count", n)
	}

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
	mcpManager := buildMCPManager(cfg, secretMgr)
	// The loop runs for the process lifetime; its cancel is invoked from
	// App.Shutdown alongside MCP.Shutdown.
	mcpHealthCancel := mcpManager.StartHealthLoop(context.Background())
	for _, t := range tools.NewMCPTools(mcpManager) {
		allTools = append(allTools, t)
	}

	// Tier G: image_describe + screenshot.
	allTools = append(allTools, tools.NewImageDescribeTool(
		visionAux, imageStore, workRoot, func(prompt, completion, total int) {
			visionUsageSink.add(prompt, completion, total)
		}))
	allTools = append(allTools, tools.NewScreenshotTool(imageStore))

	// C1: automation_* (8) + agent_batch + rlm_query. Soft-degrade like VCS
	// and plugins — a C1 build failure disables the capability and logs, it
	// does not reject the boot. See wireC1 for the warning/error split.
	c1Tools, c1Scheduler, c1Manager, c1Err := wireC1(
		ctx, *cfg, st, workMgr, broker, subagentManager, providerModels, fakeChatModel)
	if c1Err != nil {
		fmt.Fprintf(os.Stderr,
			"yanshi: C1 disabled (automation/agent_batch/rlm_query unavailable): %v\n", c1Err)
	}
	allTools = append(allTools, c1Tools...)

	// T12: tool_batch dispatches over the assembled registry, so it is a
	// member of the list it reads. Construction must happen BEFORE the
	// toolNames snapshot (so GOV5 sees the name and the profile entry is not
	// a phantom) and Bind must happen AFTER it (so the table is complete).
	// Splitting the two is what makes the cycle resolvable; an unbound
	// tool_batch refuses every call with a wiring error rather than reporting
	// eight phantom "unknown tool" failures that read like a model mistake.
	batchTool := tools.NewToolBatchTool()
	allTools = append(allTools, batchTool.Tool)

	// GOV5 seam: snapshot the registered tool names while allTools is still
	// in scope. Info() is pure metadata on every tool implementation, so the
	// background context here can never block.
	toolNames := make([]string, 0, len(allTools))
	toolTimeouts := make(map[string]time.Duration, len(allTools))
	for _, tl := range allTools {
		info, err := tl.Info(context.Background())
		if err != nil {
			return nil, fmt.Errorf("tool registry: Info failed: %w", err)
		}
		toolNames = append(toolNames, info.Name)
		// The assertion names tools.Tool rather than an inline
		// interface{ DefaultTimeout() time.Duration } on purpose. allTools is
		// typed []orchestrator.BaseTool — an alias for Eino's InvokableTool —
		// so nothing in the type system requires a registry member to satisfy
		// yanshi's own tool contract (DisplayName + DefaultTimeout + Stream).
		// A structural probe for one method would accept a type that carries
		// that method and none of the others; naming the contract makes this
		// map a record of which members honour it in full, which is what
		// TestEveryRegisteredToolImplementsTheToolContract reads.
		//
		// A member that does not satisfy it is skipped rather than defaulted,
		// so the snapshot never claims a bound that does not exist.
		if ct, ok := tl.(tools.Tool); ok {
			toolTimeouts[info.Name] = ct.DefaultTimeout()
		}
	}

	// T12 second half: the registry is final, so tool_batch can now see every
	// tool it may be asked to dispatch to. Bind is placed after the snapshot
	// loop and not inside it because a partially-built table would silently
	// make whichever tools were appended later unreachable from a batch.
	batchTool.Bind(allTools)

	// Authorize the conditionally-registered tools that this boot actually
	// got. Derived from the snapshot above, so the allow list cannot name a
	// tool the registry does not hold.
	if usingDefaultProfile {
		profile = extendProfileWithConditionalTools(profile, toolNames)
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
	// networkPolicy is enforced for yanshi's OWN in-process HTTP (web_fetch and
	// web_search, via netpolicy.NewTransport/PolicyDialer) AND, through the
	// managed proxy started just below, for the subprocesses launched by the
	// shell/secproc factories. See shell.childLaunchPosture's proxy() for
	// exactly which clients and which launchers that second half reaches.
	networkPolicy := &netpolicy.Policy{
		Default:      cfg.Security.Network.Default,
		Allow:        append([]string(nil), cfg.Security.Network.Allow...),
		Deny:         append([]string(nil), cfg.Security.Network.Deny...),
		AllowPrivate: cfg.Security.Network.AllowPrivate,
		Methods:      networkMethodRules(cfg.Security.Network.Methods),
	}
	// Start the managed proxy so children have something real to be pointed
	// at. Before this, no proxy existed and the launch posture published
	// http://127.0.0.1:0 — a placeholder that read as enforcement, broke
	// proxy-aware clients, let every other client out, and recorded nothing.
	//
	// A failure here is not fatal: the proxy is one of several launch-posture
	// inputs, and refusing to boot over it would be worse than running with
	// the honest unenforced posture. It is reported like the other degraded
	// subsystems, and proxyURL stays empty so no placeholder is published.
	//
	// This block runs BEFORE sandbox.New on purpose. The darwin backend needs
	// the proxy URL: its Seatbelt profile only re-permits loopback when
	// Config.ProxyURL is non-empty, so a sandboxed child built in the other
	// order was pointed at a proxy the sandbox then forbade it to reach.
	var proxyURL, socksURL string
	netProxy, proxyErr := netpolicy.NewProxy(*networkPolicy, nil)
	if proxyErr != nil {
		fmt.Fprintf(os.Stderr,
			"yanshi: network: managed proxy not started (%v): subprocess egress is UNFILTERED\n", proxyErr)
	} else {
		// NewProxy already listens and serves in its own goroutine.
		proxyURL = netProxy.URL().String()
		socksURL = netProxy.SOCKSURL()
	}
	// HTTPS inspection (W-B-17, ADR-0023). Off unless the operator asked, and
	// a failure to establish the root leaves it off rather than failing the
	// boot: CONNECT then behaves exactly as ADR-0014 specified, which is a
	// working posture, and the stderr line says the method rules will not
	// fire.
	//
	// These three go through slog rather than stderr for the reason the sandbox
	// warning below states: they are security POSTURE, and an operator auditing
	// after the fact needs them in the log file and the collector. stderr on a
	// detached server is nobody's inbox. (The stderr budget in
	// wiring_test.go::TestStderrIsReservedForPreLoggerAndTTYMessages is the
	// mechanical half of the same rule.)
	caFile := ""
	if cfg.Security.Network.InspectHTTPS && netProxy != nil {
		ca, caErr := netpolicy.LoadOrCreateCA(inspectionCADir())
		if caErr != nil {
			slog.Warn("HTTPS inspection requested but the certificate authority could not be prepared",
				"error", caErr,
				"effect", "CONNECT stays a blind tunnel and security.network.methods will not apply to https")
		} else {
			netProxy.SetCertAuthority(ca)
			caFile = ca.CertPath()
			slog.Warn("HTTPS inspection is ON: the managed proxy decrypts CONNECT tunnels "+
				"from factory-launched subprocesses",
				"root", caFile,
				"recorded", "host and method only; never bodies, headers or URLs",
				"adr", "docs/adr/0023-inspecting-proxy-trust-boundary.md")
		}
	}
	if len(cfg.Security.Network.Methods) > 0 && caFile == "" {
		// A method rule the operator can see in their config and that can
		// never fire on https is the "written but unread" shape; say so at the
		// only moment anyone is looking.
		slog.Warn("security.network.methods is configured but inspect_https is off",
			"rules", len(cfg.Security.Network.Methods),
			"effect", "the rules apply to plain http:// through the proxy only — an https "+
				"request's method is not readable inside a blind CONNECT tunnel")
	}

	// Task 10/13: build the security posture from config and warn the operator
	// when Phase 0 is in effect. sandbox.New returns an honest CapabilityReport
	// even when OS isolation is not enforced, so the rest of the system can
	// label itself correctly. The tier string maps to sandbox.AccessTier; an
	// unrecognized value falls through to ReadOnly (fail-safe).
	sandboxTier := sandbox.ParseTier(cfg.Security.Sandbox.Tier)
	sb := sandbox.New(sandbox.Config{
		Enabled:       cfg.Security.Sandbox.Enabled == nil || *cfg.Security.Sandbox.Enabled,
		WorkspaceRoot: workRoot,
		Tier:          sandboxTier,
		NetworkDeny:   cfg.Security.Sandbox.NetworkDeny,
		// The field had no production producer until now: bootstrap left it
		// empty and only the darwin backend read it, so on macOS the Seatbelt
		// profile denied the loopback connection the managed proxy needs. It
		// is wired rather than deleted — the consumer was correct and the
		// producer was missing.
		ProxyURL: proxyURL,
	})
	if report := sb.Report(); report.Effective != sandbox.OSIsolated {
		// Security posture: goes through slog so it lands in the log file and
		// any collector, not only on the terminal of whoever started the
		// process. An operator auditing after the fact needs this line, and
		// stderr on a detached server is nobody's inbox.
		slog.Warn("sandbox not enforcing OS/network isolation",
			"effective", string(report.Effective), "reason", report.Reason)
	}

	// W-B-21: capture the operator's login-shell environment once, so children
	// can find the toolchains their rc files put on PATH. Opt-in, and every
	// failure path here is a warning plus the zero Snapshot, whose Apply is
	// the identity function — a shell that is missing, slow or unparseable
	// costs the extra PATH entries and nothing else.
	var shellSnapshot shell.Snapshot
	if cfg.Security.Shell.CaptureProfile {
		captureCtx, cancelCapture := context.WithTimeout(ctx, shellSnapshotTimeout)
		snap, snapErr := shell.CaptureSnapshot(captureCtx, cfg.Security.Shell.ProfileShell)
		cancelCapture()
		if snapErr != nil {
			slog.Warn("login-shell environment not captured",
				"error", snapErr,
				"effect", "subprocesses use yanshi's own environment")
		} else {
			shellSnapshot = snap
		}
	}

	// Say it out loud, next to the sandbox phase0 line. Without this the
	// failure reaches the operator as a bare "connect to 127.0.0.1 port 0
	// failed" from gh / go mod download / npm, with nothing tying it back to
	// security.network — and the tool that broke looks broken rather than
	// blocked.
	//
	// The wording is deliberately narrow, because the previous version of this
	// line ("subprocess egress is blocked by a dead-port proxy") was three
	// different kinds of false and an operator would have read it as a
	// containment guarantee:
	//
	//  1. It is a PROXY VARIABLE, not a block. Only clients that honor
	//     http_proxy/https_proxy are affected; raw sockets, SSH, DNS
	//     tunnelling and any client that ignores the variables are untouched.
	//  2. It reaches only children launched through the shell/secproc
	//     factories. ACP agent CLIs (internal/acp), stdio MCP servers
	//     (internal/mcp), LSP servers (internal/lsp) and `gh`/`git` spawned
	//     directly build their env from os.Environ() and are NOT covered —
	//     they also inherit the operator's real proxy.
	//  3. Point 3 of this list used to read "it consults none of the
	//     security.network fields, so `allow` does not widen it". That stopped
	//     being true when W5 started a real proxy: the address published below
	//     belongs to a listener that evaluates security.network per host. The
	//     message is emitted AFTER the proxy starts, so it must branch on
	//     whether it did — an operator reading "dead port" while a live proxy
	//     enforces their allowlist would debug the wrong thing entirely.
	if proxyURL != "" {
		fmt.Fprintf(os.Stderr, "yanshi: network: factory-launched subprocesses route through "+
			"the managed proxy at %s (SOCKS5 at %s), which applies security.network per host. "+
			"It stops only proxy-aware clients (curl/gh/go/npm and anything honouring "+
			"ALL_PROXY) and is NOT a containment boundary — raw sockets, SSH and ACP/MCP/LSP "+
			"subprocesses bypass it entirely (see "+
			"docs/adr/0014-managed-proxy-is-the-only-governed-egress-channel.md)\n",
			proxyURL, socksURL)
	} else {
		fmt.Fprintf(os.Stderr, "yanshi: network: the managed proxy is NOT running, so "+
			"factory-launched subprocesses get no proxy variables and their egress is "+
			"UNFILTERED; security.network still applies to in-process web_fetch/web_search\n")
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
	// Factory is NOT optional: Manager.Start returns "no process factory
	// configured" without it, so every shell v2 tool would fail at runtime
	// while still passing every test that substitutes its own factory. It is
	// built from the same primitives as secureFactory below (netpolicy +
	// sandbox) so the two launch paths share one posture.
	shellManager := shell.NewManager(shell.Config{
		Root:           workRoot,
		MaxOutputBytes: cfg.Security.Shell.MaxOutputBytes,
		IdleTimeout:    cfg.Security.Shell.IdleTimeout,
		// W-B-22. Zero means uncapped, which is the previous behaviour.
		MaxConcurrent: cfg.Security.Shell.MaxConcurrent,
		Factory: shell.NewSecureLaunchFactory(shell.SecureLaunchFactory{
			Policy: networkPolicy,
			// ProxyURL was MISSING here while DefaultSecureFactory below had
			// it, so the two production launch paths had different egress
			// postures: shell_run went through the managed proxy and every
			// shell v2 tool (shell_start, task_shell_start, …) was published
			// an EMPTY http_proxy, which a child reads as "no proxy" and
			// answers by connecting directly. The shared childLaunchPosture
			// made the env semantics identical; it could not make the inputs
			// identical.
			ProxyURL: proxyURL,
			SOCKSURL: socksURL,
			CAFile:   caFile,
			Sandbox:  sb,
			Snapshot: shellSnapshot,
		}),
	})
	if st != nil {
		shellManager = shellManager.WithPersistence(shell.JobFromKV(st))
		if err := shellManager.RestoreJobs(); err != nil {
			// store closed via closeStoreOnError guard
			return nil, fmt.Errorf("bootstrap: restore shell jobs: %w", err)
		}
	}
	// Reclaim idle sessions. Config.IdleTimeout previously had one consumer —
	// the timeout branch inside Manager.Wait, which bounds how long a CALLER
	// waits and leaves the session and its process untouched. Without this
	// loop a client that started sessions and stopped reading them leaked one
	// process per session for the life of the server. No-op when the timeout
	// is unset, since zero means "no policy" rather than "reap immediately".
	shellManager.StartReaper(ctx)
	secureFactory := shell.DefaultSecureFactory{
		OS:       shell.OSProcessFactory{},
		Policy:   networkPolicy,
		ProxyURL: proxyURL,
		SOCKSURL: socksURL,
		CAFile:   caFile,
		Sandbox:  sb,
		Snapshot: shellSnapshot,
	}

	// W-C-09: resolve the tool-output head/tail truncation policy once, from
	// the primary (first-configured) provider's override / the model
	// catalog, and hand orchestrator.New the single resolved value as the
	// DEFAULT (see Orchestrator.truncationPolicy) — the policy a turn falls
	// back to when its active model has no entry in providerTruncationPolicies
	// below (M-4's per-provider map, the fix for the mismatch this comment
	// used to describe: a config with providers[1].truncation_policy set had
	// that value resolved by nothing, because this block only ever read
	// providers[0]). A malformed override does NOT fail boot —
	// internal/config.Config's TruncationPolicy field is deliberately
	// unvalidated at load time (import cycle; see that field's doc comment)
	// — so a typo degrades to the catalog/default here, observably, via this
	// warning rather than by refusing to start.
	truncationPolicy := einollm.DefaultTruncationSpec
	if len(cfg.LLM.Providers) > 0 {
		primary := cfg.LLM.Providers[0]
		if spec, ok := einollm.ResolveTruncationPolicy(primary.TruncationPolicy, primary.Model); ok {
			truncationPolicy = spec
		} else if primary.TruncationPolicy != "" {
			slog.Warn("truncation_policy is set but could not be resolved; falling back to the default policy",
				"provider", primary.Name, "truncation_policy", primary.TruncationPolicy)
		}
	}

	orchConfig := orchestrator.Config{
		Model:                      chatModel,
		Tools:                      allTools,
		Profile:                    profile,
		Instruction:                instruction,
		SkillMetaPrompt:            registry.MetaPrompt(),
		MemorySuffix:               memorySuffix,
		WorkRoot:                   workRoot,
		TruncationPolicy:           truncationPolicy,
		ProviderTruncationPolicies: providerTruncationPolicies,
		TaskManager:                workMgr,
		SubagentManager:            subagentManager,
		AvailableModels:            availableModels,
		// T3: the process-wide background manager. Bound into every turn ctx
		// by bindExecutionContext; without it the three background_* tools
		// find no manager and offload has no registry to record runs in.
		Background: backgroundMgr,
		// Security posture (Task 10/13/21). These MUST be part of the literal:
		// orchestrator.New takes Config by value and the package has no
		// setters, so assigning them after New writes to a discarded copy and
		// every tools.With* injection in bindExecutionContext silently no-ops.
		Sandbox:            sb,
		NetworkPolicy:      networkPolicy,
		Redactor:           redactor,
		Approvals:          approvalMgr,
		ShellManager:       shellManager,
		SecureFactory:      secureFactory,
		LSP:                lspMgr,
		MCP:                mcpManager,
		MultimodalMap:      multimodalMap,
		ImageStore:         imageStore,
		VisionAuxAvailable: visionAux != nil,
		Compaction: orchestrator.CompactionConfig{
			Threshold:          cfg.Compaction.Threshold,
			ContextWindow:      cfg.Compaction.ContextWindow,
			KeepRecent:         cfg.Compaction.KeepRecent,
			CooldownFraction:   cfg.Compaction.CooldownFraction,
			CooldownDuration:   parseCooldownDuration(cfg.Compaction.CooldownDuration),
			HardForceFraction:  cfg.Compaction.HardForceFraction,
			ProviderWindows:    providerWindows,
			ProviderThresholds: providerThresholds,
			ProviderFallbacks:  providerFallbacks,
			// C11: mid-turn compaction redacts its summarizer input. The
			// pre-turn/SSE path is wired separately via httpCfg.Redactor;
			// without this line only that half would be covered and secrets
			// captured between ReAct iterations would still reach the
			// summarizer. redactor is documented always non-nil here, so the
			// typed-nil-in-interface hazard that compactionOptions guards
			// against does not apply.
			Redactor: redactor,
		},
		// L1-L4 per-turn stop conditions. Like Compaction above, this MUST be
		// part of the literal: orchestrator.New takes Config by value, so a
		// post-New assignment writes to a discarded copy and every gate would
		// silently never install. The zero config installs nothing, so an
		// operator who never writes a loop_guard block keeps today's behaviour.
		LoopGuard: orchestrator.LoopGuardConfig{
			RepetitionEnabled:   cfg.LoopGuard.RepetitionEnabled,
			RepetitionWindow:    cfg.LoopGuard.RepetitionWindow,
			RepetitionWarnAfter: cfg.LoopGuard.RepetitionWarnAfter,
			RepetitionStopAfter: cfg.LoopGuard.RepetitionStopAfter,
			MaxToolCalls:        cfg.LoopGuard.MaxToolCalls,
			PerToolCalls:        cfg.LoopGuard.PerToolCalls,
			TurnTimeout:         cfg.LoopGuard.TurnTimeout,
			MaxTurnTokens:       cfg.LoopGuard.MaxTurnTokens,
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
			Threshold:          cfg.Compaction.Threshold,
			KeepRecent:         cfg.Compaction.KeepRecent,
			ContextWindow:      cfg.Compaction.ContextWindow,
			Model:              cfg.Compaction.Model,
			ProviderWindows:    providerWindows,
			ProviderThresholds: providerThresholds,
			ProviderFallbacks:  providerFallbacks,
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
	// A2/W-A-05: prefer the cheap batch.rlm_model provider for the memory
	// consolidation pass, falling back to the main chat model when none is
	// configured. Unlike SelectRLMModel (used by RLM1's rlm_query tool),
	// which hard-errors without an explicit batch.rlm_model, this never
	// fails: distillation summarizes existing memory rows -- not the kind
	// of reasoning that needs the expensive model or a dedicated one -- so
	// running the pass on the main model beats not running it at all, and
	// chatModel is always non-nil by this point (real provider or
	// FakeModel). See Config.DistillModel's doc comment for the full
	// reasoning this mirrors.
	distillModel := chatModel
	if cfg.Batch.RLMModel != "" {
		if selected, ok := providerModels[cfg.Batch.RLMModel]; ok && selected != nil {
			distillModel = selected
		}
	}
	httpCfg.DistillModel = distillModel
	// W-D: the background upkeep sweep, over the SAME root ctx as the
	// automation scheduler, so App.cancel stops both and App.Shutdown joins
	// both before closing the store.
	//
	// Assembled HERE rather than beside the other subsystems because it shares
	// distillModel: its memory job calls the same tools.DistillMemories entry
	// point the WS handler does, and giving it its own model selection would be
	// a second place for an operator's batch.rlm_model to be honoured or not.
	upkeepWorker := BuildUpkeep(ctx, *cfg, st, distillModel)
	// S10: inject the process-wide redactor so the SSE writeSSEFrame and
	// the WS wsConn.write boundaries redact every outbound frame.
	httpCfg.Redactor = redactor
	// W-B-14: the operator's auto-mode risk policy, already read and validated
	// by config.Load (an unreadable or category-incomplete file refuses the
	// start there, so reaching this line means it is usable).
	httpCfg.GuardianPrompt = cfg.Security.GuardianPrompt
	srv := apihttp.New(httpCfg)
	// W-B-16: give the managed proxy something to ask. Wired here rather than
	// at NewProxy because the server is what can reach a human and it does not
	// exist until now — the proxy URL had to be known first, since the launch
	// posture the server's tools use is built out of it.
	//
	// Until this line runs, and whenever no client is connected afterwards,
	// ApproveEgress answers false and an unapproved host stays refused. That
	// is the fail-closed direction: a subprocess reaching a host nobody
	// allowed does not get out because there was nobody to ask.
	if netProxy != nil {
		netProxy.SetApprover(srv)
	}
	srv.Chat(orch, providerModels, registry)
	srv.ChatWS(orch, providerModels, registry) // WebSocket endpoint (TUI primary transport)
	srv.AgentV1(agentAPI)                      // V14 versioned resource API (thread/turn/item)
	srv.Sessions(st)

	// Register the task endpoints on the server and start the heartbeat
	// sweeper. The broker itself, ctx and the dispatcher binding were created
	// earlier, next to workMgr — C1 needs them at tool-registry assembly time.
	srv.TaskAPI(broker, cfg.Profiles)

	// Push durable-task transitions to connected clients. The mirror runs on a
	// broker worker goroutine with no turn, so TurnOpts.EmitWorkFrame — the
	// path every tool-emitted work event takes — cannot reach it: that
	// callback lives in a turn context that no longer exists by the time a
	// worker finishes. Without this the TUI shows a durable task at "pending"
	// until the user runs task_read again, which is the same "state is
	// correct, nobody can see it" gap the mirror itself was written to close.
	//
	// Wired here rather than next to broker.Work because srv does not exist
	// yet at that point. Read failures are dropped: this is a notification,
	// and the authoritative state is one task_read away.
	mirror.OnTransition = func(workTaskID string) {
		task, rerr := workMgr.Read(ctx, workTaskID)
		if rerr != nil {
			return
		}
		srv.Broadcast(proto.NewTaskUpdate(task))
	}

	work.StartArtifactJanitor(ctx, workStore, workRoot, 6*time.Hour, work.DefaultArtifactTTL)

	broker.StartSweeper(ctx, 10*time.Second)

	srv.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	// O7 readiness. Registered HERE — after every subsystem above has been
	// assembled and immediately before Build returns — because the ONLY thing
	// that distinguishes it from /healthz is its position in this function.
	//
	// /healthz answers "a process is listening". That is true from the moment
	// net/http binds, which is before the store is migrated, before the VCS
	// repo is initialised, and before the tool registry is populated. A
	// discovery client that trusts it adopts a backend that will fail the
	// first real request, and a supervisor that trusts it routes traffic
	// there. /readyz answers the question they were both actually asking:
	// "did Build finish". Moving this registration earlier does not weaken the
	// endpoint, it deletes it — the route would answer 200 for exactly the
	// window it exists to report on.
	srv.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
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

	// Successful Build: hand store and root-context ownership to the caller.
	// The deferred closeStoreOnError / cancelOnError guards become no-ops;
	// App.Shutdown closes the store via a.Store.Close() and cancels the root
	// context via a.cancel.
	closeStoreOnError = false
	cancelOnError = false
	return &App{
		Server:           httpServer,
		Store:            st,
		Orch:             orch,
		Broker:           broker,
		Addr:             addr,
		Model:            chatModel,
		Models:           providerModels,
		VisionAux:        visionAux,
		MultimodalMap:    multimodalMap,
		ImageStore:       imageStore,
		VisionUsage:      &visionUsageSink,
		AgentAPI:         agentAPI,
		Skills:           registry,
		ToolNames:        toolNames,
		ServerCompaction: httpCfg.Compaction,
		LaunchProxyURLs:  launchProxyURLs(secureFactory, shellManager),
		NetProxy:         netProxy,
		mcpHealthCancel:  mcpHealthCancel,
		ToolTimeouts:     toolTimeouts,
		VCS:              vcsInstance,
		VCSRepoID:        vcsRepoID,
		VCSDBPath:        cfg.Storage.SQLitePath,
		WorktreeDir:      worktreeDir,
		Sandbox:          sb,
		NetworkPolicy:    networkPolicy,
		SubagentManager:  subagentManager,
		AgentTools:       agentTools,
		ToolBatch:        batchTool,
		Automation:       c1Manager,
		BootConfig:       cfg,
		LSP:              lspMgr,
		MCP:              mcpManager,
		C1Scheduler:      c1Scheduler,
		Upkeep:           upkeepWorker,
		Approvals:        approvalMgr,
		ShellManager:     shellManager,
		SecureFactory:    secureFactory,
		Features:         featureReg,
		Pricing:          priceTab,
		OTel:             otelRT,
		Redactor:         redactor,
		Auth:             authMgr,
		LogPath:          logPath,
		Background:       backgroundMgr,
		cancel:           cancel,
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
		w, err := openLogFile(cfg.File, cfg)
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
				if w, oErr := openLogFile(path, cfg); oErr == nil {
					return w, path
				}
			}
		}
	}
	return nil, ""
}

// openLogFile opens path as a SIZE-ROTATING append log, creating it (and its
// parent dir) if missing. The caller owns closing; bootstrap never closes the
// log file so it lives for the process lifetime.
//
// It returns obslog.RotatingWriter rather than a bare *os.File because the
// long-running shape here is `yanshi serve` on a workstation or a container
// with no logrotate(8) in front of it, where an unbounded structured log is
// the process's only unbounded resource — it fills the volume and takes the
// database down with it. Rotation is failure-tolerant by construction: a
// rotate that cannot rename keeps writing to the handle it already holds and
// records the error on RotateError(), so retention degrading never becomes
// logging failing.
func openLogFile(path string, cfg config.LogConfig) (io.Writer, error) {
	// MaxSizeMB is megabytes on the operator's side and bytes here. A
	// negative value is passed through as negative rather than clamped: that
	// is RotateConfig's "never rotate by size" sentinel, and clamping it to
	// zero would silently mean "use the default" — the opposite request.
	maxBytes := int64(cfg.MaxSizeMB)
	if maxBytes > 0 {
		maxBytes *= 1 << 20
	}
	return obslog.NewRotatingWriter(path, obslog.RotateConfig{
		MaxBytes:   maxBytes,
		MaxBackups: cfg.MaxBackups,
	})
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

// MCPHealthRunning reports whether the MCP health loop was started. Exposed
// for the wiring assertion: the loop is a goroutine with no other observable
// effect, so nothing else can tell a started loop from an unstarted one until
// a server dies and nobody notices.
func (a *App) MCPHealthRunning() bool { return a.mcpHealthCancel != nil }

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
	// The automation scheduler observes a.cancel() above; join it before any
	// store-touching teardown so an in-flight tick cannot race the close.
	if a.C1Scheduler != nil {
		a.C1Scheduler.Wait()
	}
	// Same reasoning, same ordering: the upkeep sweep observes a.cancel()
	// above, and a compression in flight must finish before the store closes —
	// it has already written the blob and is about to delete the rows.
	a.Upkeep.Wait()
	if err := a.Server.Shutdown(ctx); err != nil {
		errs = append(errs, err)
	}
	// The managed proxy owns a loopback listener and two goroutines per
	// connection, and nothing closed it: every App built in a process — every
	// bootstrap test, every in-process TUI restart — left one behind, still
	// accepting. Closed AFTER the HTTP server so an in-flight tool call is not
	// cut off mid-fetch by a proxy that vanished. Its error is deliberately
	// dropped: a teardown failure on a facility that is going away cannot be
	// acted on and would mask the errors that can.
	if a.NetProxy != nil {
		_ = a.NetProxy.Close()
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
		if a.mcpHealthCancel != nil {
			a.mcpHealthCancel()
		}
		a.MCP.Shutdown()
	}
	// Flush OTel exporters BEFORE the store so spans/metrics referencing
	// store-backed attributes are emitted while the store is still open.
	if a.OTel != nil {
		if err := a.OTel.Shutdown(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	// T3: cancel every offloaded tool run and wait, bounded. This runs BEFORE
	// the store close because a run finishing here still writes its result,
	// and spillIfTooLong touches the work root on the way out. A false return
	// means at least one subprocess outlived the close grace period — worth
	// reporting, but never a reason to block process exit.
	if a.Background != nil && !a.Background.Close() {
		errs = append(errs, errors.New("bootstrap: background runs did not unwind within the close grace period"))
	}
	// Close the sandbox AFTER the graceful background unwind above, because on
	// Windows this call IS the kill: the Job Object is created with
	// KILL_ON_JOB_CLOSE, so closing the handle terminates every process still
	// assigned to it — including double-forked grandchildren that no pid we
	// hold can reach. Doing it first would shoot runs that were about to finish
	// writing their results.
	//
	// It was previously not called at all. On darwin and Linux that is
	// invisible (Seatbelt and bwrap hold no such handle and Close is a no-op),
	// which is exactly why it stayed missing: the one platform where it matters
	// is the one nobody runs during development. The kernel does close the
	// handle at process exit either way, so this is about deterministic reaping
	// at shutdown rather than about whether the children eventually die.
	if a.Sandbox != nil {
		if cerr := a.Sandbox.Close(); cerr != nil {
			errs = append(errs, cerr)
		}
	}
	// Release the autoVCS cross-process lock-file descriptors and reclaim the
	// files nobody else holds. Before this, every process that opened a repo
	// left one zero-byte file in the user's cache directory forever; a
	// measurement on this repository found 27,968 of them. It runs BEFORE the
	// store close only for tidiness — the lock files are independent of the
	// database — and its error is reported rather than swallowed so a cache
	// directory that cannot be tidied is visible.
	if a.VCS != nil {
		if cerr := a.VCS.Close(); cerr != nil {
			errs = append(errs, cerr)
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

// launchProxyURLs reads back what each production launch factory will publish
// to its children. See App.LaunchProxyURLs for why this is read from the
// assembled objects instead of from the literals a few hundred lines up.
//
// A factory the manager was given but that is not the shell v2 one yields no
// entry, which the assertion reads as "shell v2 publishes nothing" — the same
// verdict as an empty URL, and the correct one: a substituted factory is not
// the production egress posture either.
func launchProxyURLs(secure shell.DefaultSecureFactory, manager *shell.Manager) map[string]string {
	out := map[string]string{"secproc": secure.ProxyURL}
	if manager != nil {
		if v2, ok := manager.Factory().(shell.SecureLaunchFactory); ok {
			out["shell_v2"] = v2.ProxyURL
		}
	}
	return out
}

// shellSnapshotTimeout bounds the login-shell capture (W-B-21). It sits on the
// path to the first prompt, and an rc file that waits on a network call or a
// keypress would otherwise hold the whole start-up there.
const shellSnapshotTimeout = 5 * time.Second

// inspectionCADir is where the HTTPS-inspection root lives: ~/.yanshi/tls,
// beside the other per-user state this process owns. The directory is created
// 0700 and the key 0600 by netpolicy.LoadOrCreateCA.
//
// Falls back to a temp directory when the home directory is unknowable, which
// makes the root ephemeral rather than making inspection unavailable. A
// per-boot root is worse for the operator (children re-trust a new one each
// start) and strictly better than a root in a world-readable location.
func inspectionCADir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return filepath.Join(os.TempDir(), "yanshi-tls")
	}
	return filepath.Join(home, ".yanshi", "tls")
}

// networkMethodRules converts the YAML method table into the policy's form.
//
// Action was already validated by config.Load (an unrecognised verdict refuses
// the start), so the comparison here is the only reading of it and cannot fall
// through to a guessed default: anything that is not "allow" is a deny, and
// nothing that is not "allow" or "deny" reaches this function.
func networkMethodRules(in []config.NetworkMethodRule) []netpolicy.MethodRule {
	if len(in) == 0 {
		return nil
	}
	out := make([]netpolicy.MethodRule, 0, len(in))
	for _, rule := range in {
		out = append(out, netpolicy.MethodRule{
			Host:    rule.Host,
			Methods: append([]string(nil), rule.Methods...),
			Allow:   strings.EqualFold(strings.TrimSpace(rule.Action), "allow"),
		})
	}
	return out
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

// lspLanguages applies the operator's per-language overrides on top of the
// built-in table.
//
// Split out so the mapping can be asserted without standing up a Manager:
// lsp.New prunes by PATH, so a test that went through Build would skip on
// every machine that lacks the configured binary, and a permanently skipped
// test asserts nothing.
func lspLanguages(overrides map[string]config.LanguageServerSpec) map[string]lsp.LanguageServer {
	langServers := lsp.DefaultLanguages()
	for lang, spec := range overrides {
		// Preserve the default workspace markers. The config key is named
		// Override and its fields are command/args: an operator pointing
		// yanshi at their own gopls build is overriding the COMMAND, not
		// asking to stop checking whether this is a Go workspace. Rebuilding
		// the entry from scratch dropped the markers, so overriding one
		// language silently gave it different gating from every other -- a
		// server that spawns in directories it has no business in, and no
		// diagnostic anywhere tying that back to the override.
		entry := langServers[lang] // zero value for a language with no default
		entry.Command = spec.Command
		entry.Args = spec.Args
		langServers[lang] = entry
	}
	return langServers
}

// LSPLanguagesForTest exposes lspLanguages to the external bootstrap_test
// package. Named ForTest because nothing in production should re-derive this
// table -- Build already holds the one that matters.
func LSPLanguagesForTest(overrides map[string]config.LanguageServerSpec) map[string]lsp.LanguageServer {
	return lspLanguages(overrides)
}
