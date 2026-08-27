// Package orchestrator is yanshi's main coordinating agent (Eino ADK ChatModelAgent).
package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"
	"github.com/x6nux/yanshi/internal/agent/registry"
	"github.com/x6nux/yanshi/internal/approval"
	"github.com/x6nux/yanshi/internal/ctxcompact"
	"github.com/x6nux/yanshi/internal/guard"
	"github.com/x6nux/yanshi/internal/imagestore"
	einollm "github.com/x6nux/yanshi/internal/llm/eino"
	"github.com/x6nux/yanshi/internal/mcp"
	"github.com/x6nux/yanshi/internal/netpolicy"
	obslog "github.com/x6nux/yanshi/internal/observe/log"
	otelobs "github.com/x6nux/yanshi/internal/observe/otel"
	"github.com/x6nux/yanshi/internal/proto"
	"github.com/x6nux/yanshi/internal/sandbox"
	"github.com/x6nux/yanshi/internal/secproc"
	"github.com/x6nux/yanshi/internal/secrets"
	"github.com/x6nux/yanshi/internal/shell"
	"github.com/x6nux/yanshi/internal/task/work"
	"github.com/x6nux/yanshi/internal/toolreg"
	"github.com/x6nux/yanshi/internal/tools"
	"github.com/x6nux/yanshi/internal/vcs"
)

// BaseTool is an alias for the tool interface the orchestrator accepts.
type BaseTool = tool.InvokableTool

// Config configures an Orchestrator.
type Config struct {
	Model           model.BaseChatModel
	Tools           []BaseTool
	Instruction     string
	SkillMetaPrompt string
	MaxIters        int
	Profile         guard.PermissionProfile
	VCSScope        tools.VCSScope
	WorkRoot        string
	Compaction      CompactionConfig
	Sandbox         sandbox.Sandbox
	NetworkPolicy   *netpolicy.Policy
	// Redactor 是进程级 secrets redactor。绑进每个 turn 的执行 context，
	// 供 GuardedTool.InvokableRun 在结果交给模型之前收口（W-A-02）。
	// nil 表示不脱敏，行为与引入前逐字节一致。
	Redactor        *secrets.Redactor
	Approvals       *approval.Manager
	ShellManager    *shell.Manager
	SecureFactory   secproc.Factory
	TaskManager     work.ManagerLike
	SubagentManager *registry.Manager
	AvailableModels map[string]bool
	// LSP is the post-edit diagnostics manager interface (nil = no injection).
	LSP tools.LSPManager
	// MCP is the MCP connection manager. When non-nil and Enabled(), per-turn
	// context is injected so MCP GuardedTools can route calls through it.
	MCP *mcp.Manager
	// MultimodalMap maps model-id to native image capability (Tier G).
	MultimodalMap map[string]bool
	// ImageStore is the session-level image store (Tier G).
	ImageStore *imagestore.Store

	// VisionAuxAvailable reports whether an auxiliary vision model is
	// configured, which is what makes the placeholder path usable: a
	// placeholder is only a reference, and image_describe is what resolves it.
	// Without the aux model the tool answers with a configuration error, so a
	// turn that inserted placeholders anyway would look like it had read the
	// image. See ErrNoVisionPath.
	VisionAuxAvailable bool
	// MemorySuffix is appended to Instruction (after SkillMetaPrompt) as an
	// independent block, so it survives instructionOverride in runSubAgentTurn.
	// It carries the user/project memory block (MEM1) so the model sees user
	// preferences across sessions and across sub-agent boundaries. Empty = no
	// memory injection (the default).
	MemorySuffix string

	// LoopGuard configures the per-turn stop conditions (repetition detection,
	// tool-call budgets, turn deadline, token budget). The zero value installs
	// nothing; see LoopGuardConfig for why nothing defaults on.
	LoopGuard LoopGuardConfig

	// Background owns tool calls moved to the background at their foreground
	// deadline (T3). nil disables offloading: every tool then keeps the plain
	// context.WithTimeout behaviour, and a long run still fails at its
	// deadline with nothing to show for it. The composition root supplies one
	// and closes it during shutdown.
	Background *tools.BackgroundManager
}

// CompactionConfig mirrors config.CompactionConfig.
type CompactionConfig struct {
	Threshold     float64
	ContextWindow int
	KeepRecent    int
	// CooldownFraction is the minimum token growth, as a fraction of the
	// turn's RESOLVED context window, before another compaction is allowed.
	// It is kept as a fraction rather than a pre-multiplied count because
	// bootstrap does not know which provider a given turn will run against:
	// multiplying there against the global fallback window sized every gate
	// for the wrong model. 0 means no token-growth cooldown.
	CooldownFraction float64
	// CooldownDuration is the minimum wall-time since last compaction. <=0
	// means no time-based cooldown.
	CooldownDuration time.Duration
	// HardForceFraction forces compaction once estimated tokens reach this
	// fraction of ContextWindow, even when inside a cooldown period. 0 disables.
	HardForceFraction float64
	// ProviderWindows maps a registry model name to that model's context
	// window, keyed exactly as TurnOpts.ModelID. A turn whose model is absent
	// falls back to ContextWindow. bootstrap already computes this map for the
	// HTTP layer; it is passed here rather than resolved locally so the
	// orchestrator never has to import internal/api/http, which GOV1 forbids.
	ProviderWindows map[string]int
	// Redactor strips registered secrets from the history handed to the summary
	// model on the MID-TURN compaction path, and from the summary it returns
	// (C11). nil disables redaction.
	//
	// It rides on this config rather than being resolved locally for the same
	// reason ProviderWindows does: bootstrap owns the process-wide
	// secrets.Redactor, and the orchestrator must not import the packages that
	// would let it find one itself. Assign only a non-nil redactor — a
	// typed-nil pointer in this interface field panics instead of disabling
	// redaction (see ctxcompact.Options.Redactor).
	Redactor ctxcompact.Redactor
}

// windowFor returns the context window to size a turn's compaction gates
// against, or 0 when the model is unknown and the global fallback applies.
func (cc CompactionConfig) windowFor(modelID string) int {
	if modelID == "" || cc.ProviderWindows == nil {
		return 0
	}
	return cc.ProviderWindows[modelID]
}

// Orchestrator wraps an Eino ChatModelAgent + Runner.
type Orchestrator struct {
	profile  guard.PermissionProfile
	vcsScope tools.VCSScope
	workRoot string
	// rawModel is the default model (UNWRAPPED, straight from Config.Model)
	// so runnerFor can build mode-specific agents with the same unwrapped model.
	rawModel model.BaseChatModel
	// model is the compaction-wrapped default model, cached for sub-agent reuse.
	model model.BaseChatModel

	instruction string
	agentTools  []tool.BaseTool
	toolNames   []string
	maxIters    int
	compaction  CompactionConfig

	// runners memoizes per-model+per-mode Runners, keyed by runnerCacheKey.
	runners sync.Map

	baseInstruction string
	// memorySuffix is the user/project memory block (MEM1), preserved alongside
	// baseInstruction so sub-agents built with an instructionOverride still get
	// the memory appended (see runSubAgentTurn). New() bakes it once; no hot
	// reload (see bootstrap Task 4).
	memorySuffix string

	taskManager work.ManagerLike

	approvals       *approval.Manager
	sandbox         sandbox.Sandbox
	networkPolicy   *netpolicy.Policy
	redactor        *secrets.Redactor
	secureFactory   secproc.Factory
	shellManager    *shell.Manager
	subagentMgr     *registry.Manager
	lspMgr          tools.LSPManager
	mcpMgr          *mcp.Manager
	availableModels map[string]bool
	multimodalMap   map[string]bool
	imageStore      *imagestore.Store
	// visionAuxAvailable reports whether an auxiliary vision model is
	// configured. Only the boolean is kept: the orchestrator never calls the
	// aux model itself — image_describe does — so holding the model here would
	// be a second reference to the same thing with no consumer.
	visionAuxAvailable bool
	// loopGuard is the per-turn stop-condition policy. It is CONFIG, not
	// state: withTurnContext builds a fresh turnGuard from it for every turn,
	// because runnerFor memoises runners across turns and sessions.
	loopGuard LoopGuardConfig

	// background owns tool calls moved to the background at their foreground
	// deadline (T3). PROCESS-scoped, not per-turn: a run that is cancelled
	// when its turn ends is not a background run, it is the same timeout with
	// extra steps. nil disables offloading entirely and every tool keeps its
	// plain WithTimeout behaviour.
	background *tools.BackgroundManager

	// sessionRules holds the per-connection approval rule sets (S9), keyed by
	// connection session id. Guarded by sessionRulesMu because approvals
	// arrive on the WebSocket reader goroutine while turns read the profile.
	// Entries are removed by ReleaseSession; see sessionrules.go for why the
	// explicit teardown is not optional.
	sessionRulesMu sync.Mutex
	sessionRules   map[string]*guard.RuleSet
}

// runnerToolMode distinguishes between agent (full tools) and plan (filtered tools) runners.
type runnerToolMode uint8

const (
	runnerModeAgent runnerToolMode = iota
	runnerModePlan
)

// DefaultInstruction is the canonical orchestrator system prompt used when
// cfg.Instruction is empty. Exported so bootstrap (and only bootstrap) can
// layer project rules / output-language directives on top of the SAME base
// text instead of duplicating the wording and risking drift.
const DefaultInstruction = "You are yanshi's orchestrator. Use tools when helpful."

// runnerCacheKey uniquely identifies a cached ADK Runner by model pointer + mode.
type runnerCacheKey struct {
	model model.BaseChatModel
	mode  runnerToolMode
}

// New builds an Orchestrator from Config.
func New(cfg Config) (*Orchestrator, error) {
	if cfg.Model == nil {
		return nil, fmt.Errorf("orchestrator: Model is required")
	}

	maxIters := cfg.MaxIters
	if maxIters == 0 {
		maxIters = math.MaxInt
	}

	instruction := cfg.Instruction
	if instruction == "" {
		instruction = DefaultInstruction
	}

	if cfg.SkillMetaPrompt != "" {
		instruction = instruction + "\n\n" + cfg.SkillMetaPrompt
	}

	// MEM1: append the memory suffix as an independent block, INSIDE the
	// baseInstruction snapshot. Sub-agents that inherit baseInstruction get
	// the memory for free (NO re-append on the inherit path). Only the
	// override path in runSubAgentTurn re-appends — because override replaces
	// baseInstruction entirely and would otherwise drop the memory.
	if cfg.MemorySuffix != "" {
		instruction = instruction + "\n\n" + cfg.MemorySuffix
	}

	baseInstruction := instruction
	instruction = instruction + "\n\n--- Environment ---\n" + buildEnvInfo()

	profile := cfg.Profile

	agentTools := make([]tool.BaseTool, 0, len(cfg.Tools))
	for _, t := range cfg.Tools {
		agentTools = append(agentTools, t)
	}
	toolNames := collectToolNames(agentTools)

	// rawModel is the UNWRAPPED default model, saved for runnerFor to build
	// mode-specific agents with their own compaction wrapper.
	rawModel := cfg.Model

	return &Orchestrator{
		model:              wrapCompaction(cfg.Model, cfg.Compaction, 0),
		rawModel:           rawModel,
		profile:            profile,
		vcsScope:           cfg.VCSScope,
		workRoot:           cfg.WorkRoot,
		instruction:        instruction,
		baseInstruction:    baseInstruction,
		memorySuffix:       cfg.MemorySuffix,
		agentTools:         agentTools,
		toolNames:          toolNames,
		maxIters:           maxIters,
		compaction:         cfg.Compaction,
		taskManager:        cfg.TaskManager,
		approvals:          cfg.Approvals,
		sandbox:            cfg.Sandbox,
		networkPolicy:      cfg.NetworkPolicy,
		redactor:           cfg.Redactor,
		secureFactory:      cfg.SecureFactory,
		shellManager:       cfg.ShellManager,
		subagentMgr:        cfg.SubagentManager,
		lspMgr:             cfg.LSP,
		mcpMgr:             cfg.MCP,
		multimodalMap:      cfg.MultimodalMap,
		imageStore:         cfg.ImageStore,
		visionAuxAvailable: cfg.VisionAuxAvailable,
		availableModels:    cfg.AvailableModels,
		loopGuard:          cfg.LoopGuard,
		background:         cfg.Background,
		sessionRules:       make(map[string]*guard.RuleSet),
	}, nil
}

// wrapCompaction returns m wrapped in einollm.CompactingModel when compaction
// is enabled, else m unchanged.
//
// window is the context window resolved for the model m will run against;
// pass 0 to fall back to cc.ContextWindow. Every gate -- threshold, hard
// force, and the token cooldown -- is sized from that single number, so a
// provider with a smaller window than the global default cannot end up with
// a threshold it can never reach.
func wrapCompaction(m model.BaseChatModel, cc CompactionConfig, window int) model.BaseChatModel {
	if cc.Threshold <= 0 {
		return m
	}
	if window <= 0 {
		window = cc.ContextWindow
	}
	return &einollm.CompactingModel{
		Inner:             m,
		Threshold:         cc.Threshold,
		ContextWindow:     window,
		KeepRecent:        cc.KeepRecent,
		CooldownTokens:    int(cc.CooldownFraction * float64(window)),
		CooldownDuration:  cc.CooldownDuration,
		HardForceFraction: cc.HardForceFraction,
		Redactor:          cc.Redactor,
	}
}

// collectToolNames returns registered tool Info names (best-effort).
func collectToolNames(ts []tool.BaseTool) []string {
	names := make([]string, 0, len(ts))
	for _, t := range ts {
		if info, err := t.Info(context.Background()); err == nil && info.Name != "" {
			names = append(names, info.Name)
		}
	}
	return names
}

// unknownToolHandler builds the unknown-tool handler that returns the failure
// as tool RESULT (not error), letting the model retry with a valid name.
func unknownToolHandler(names []string) func(ctx context.Context, name, input string) (string, error) {
	avail := strings.Join(names, ", ")
	return func(_ context.Context, name, _ string) (string, error) {
		return fmt.Sprintf(
			"error: tool %q is not available. Available tools: %s. Call one of these by its exact name and retry.",
			name, avail), nil
	}
}

// Profile returns the permission profile.
func (o *Orchestrator) Profile() guard.PermissionProfile { return o.profile }

// WorkRoot is the project root every filesystem boundary is measured against.
// The HTTP layer needs it to resolve @path attachments, which happen before a
// turn starts and therefore outside every tools context that would otherwise
// carry it.
func (o *Orchestrator) WorkRoot() string { return o.workRoot }

// ProfileForTest exposes the resolved profile for tests.
func (o *Orchestrator) ProfileForTest() guard.PermissionProfile { return o.profile }

// bindExecutionContext threads every orchestrator-owned security value into ctx.
func (o *Orchestrator) bindExecutionContext(ctx context.Context, connectionSessionID string) context.Context {
	// S9: the acting profile is the orchestrator's profile PLUS whatever this
	// connection's approvals widened. profileForSession is a no-op for a
	// profile with no Shell.Rules and for an empty session id, so the default
	// coding profile behaves exactly as before — see sessionrules.go for why
	// grafting rules onto a glob profile would be a hard denial of everything
	// rather than a widening.
	ctx = tools.WithProfile(ctx, o.profileForSession(connectionSessionID))
	ctx = tools.WithWorkRoot(ctx, o.workRoot)
	ctx = tools.WithTaskManager(ctx, o.taskManager)
	// S8: the names that actually exist in THIS execution scope. Bound here,
	// alongside the profile, because the two are read at the same moment and
	// answer adjacent questions — the profile says whether a tool may run, this
	// says whether the tool is a tool at all. A sub-agent built with a filtered
	// tool subset gets its own narrower set for free, since it is a separate
	// Orchestrator with its own toolNames.
	ctx = toolreg.WithRegistered(ctx, o.toolNames)
	if o.approvals != nil && connectionSessionID != "" {
		ctx = tools.WithApprovalManager(ctx, o.approvals, connectionSessionID)
	}
	if o.sandbox != nil {
		ctx = tools.WithSandbox(ctx, o.sandbox)
	}
	if o.networkPolicy != nil {
		ctx = tools.WithNetworkPolicy(ctx, o.networkPolicy)
	}
	if o.redactor != nil {
		ctx = tools.WithRedactor(ctx, o.redactor)
	}
	if o.secureFactory != nil {
		ctx = tools.WithSecureProcessFactory(ctx, o.secureFactory)
	}
	if o.shellManager != nil {
		ctx = tools.WithShellManager(ctx, o.shellManager)
	}
	if o.vcsScope.VCS != nil {
		ctx = tools.WithVCS(ctx, o.vcsScope)
	}
	if o.lspMgr != nil {
		ctx = tools.WithLSP(ctx, o.lspMgr)
	}
	// T3: nil-gated like the rest. An unbound manager means GuardedTool.Stream
	// keeps its plain WithTimeout path, which is the pre-T3 behaviour.
	if o.background != nil {
		ctx = tools.WithBackgroundManager(ctx, o.background)
	}
	return ctx
}

// BindExecutionContextForTest 暴露 bindExecutionContext 给组合根的接线测试。
//
// 存在的理由是 GOV6 够不到的那半：GOV6 只证明注入器**有**调用点，证明不了
// 真实装配出的 App 确实走到了那个调用点。w3wiring_test.go 对真 App 断言，
// 补的正是这个盲区。
func (o *Orchestrator) BindExecutionContextForTest(ctx context.Context, connectionSessionID string) context.Context {
	return o.bindExecutionContext(ctx, connectionSessionID)
}

// withTurnContext is the single helper for all 4 turn entry points: it binds
// Profile, WorkRoot, TaskManager, PlanMode, ThreadLink, and the work-event
// callback onto ctx, then binds the sub-agent runner.
func (o *Orchestrator) withTurnContext(ctx context.Context, opts TurnOpts) context.Context {
	ctx = ensureTurnIDs(ctx)
	ctx = o.bindExecutionContext(ctx, opts.ConnectionSessionID)
	ctx = tools.WithPlanMode(ctx, opts.PlanMode)
	ctx = tools.WithThreadLink(ctx, opts.ThreadID, opts.TurnID)
	// Tier G entry C: the sidecar that carries images produced by fs_read /
	// web_fetch back to the model. A tool result is a string, so the bytes cannot
	// ride the return value; they go here and imageAttacher drains them into the
	// ADK state before the next model call. The sink carries the turn model's
	// multimodal capability so the tools know whether an image part has anywhere
	// to go — an empty ModelID resolves to false, i.e. hint text, which is the
	// fail-safe answer for the entry points that do not select a model.
	ctx = tools.WithTurnImages(ctx, tools.NewTurnImages(o.IsMultimodal(opts.ModelID)))
	if opts.EmitWorkFrame != nil {
		ctx = tools.WithWorkEventCallback(ctx, func(event work.Event) {
			opts.EmitWorkFrame(workEventFrame(event))
		})
	}
	if o.mcpMgr != nil && o.mcpMgr.Enabled() {
		ctx = tools.WithMCP(ctx, o.mcpMgr)
	}
	// A FRESH loop-guard state per turn. Bound here rather than inside
	// runnerFor because runnerFor memoises: one runner (and one middleware
	// instance) serves every turn on that model, so any counter living there
	// would be process-wide.
	ctx = WithLoopGuard(ctx, o.loopGuard)
	return o.bindManagedRunner(ctx)
}

// ensureTurnIDs fills in correlation IDs when the caller hasn't already bound
// them. WS turns install their own trace/turn IDs (so each user_message carries
// a fresh trace); this helper covers orchestrator entry points reached without
// going through WS (goal loop, ACP, headless tools) so they still produce
// correlated logs and OTel spans. Existing IDs are preserved; missing fields
// are filled with fresh random IDs.
func ensureTurnIDs(ctx context.Context) context.Context {
	ids := obslog.IDsFromContext(ctx)
	if ids.TraceID == "" {
		ids.TraceID = obslog.NewTraceID()
	}
	if ids.TurnID == "" {
		ids.TurnID = obslog.NewTurnID()
	}
	return obslog.WithIDs(ctx, ids)
}

// bindManagedRunner binds the Manager, ManagedRunnerFactory, available models,
// and the legacy SubAgentRunner (as fallback) into ctx.
func (o *Orchestrator) bindManagedRunner(ctx context.Context) context.Context {
	ctx = o.bindSubAgentRunner(ctx)
	if o.subagentMgr == nil {
		return ctx
	}
	ctx = tools.WithManager(ctx, o.subagentMgr)
	if o.availableModels != nil {
		ctx = tools.WithAvailableModels(ctx, o.availableModels)
	}
	profile, workRoot, vcsScope := o.profile, o.workRoot, o.vcsScope
	mgr := o.subagentMgr
	depth := tools.SubAgentDepth(ctx)
	emit := tools.SubAgentEmitFrom(ctx)
	factory := tools.ManagedRunnerFactory(func(allowed []string, instruction string) registry.Runner {
		return &managedTurnRunner{
			o: o, mgr: mgr, profile: profile, workRoot: workRoot, vcsScope: vcsScope,
			depth: depth, emit: emit, allowed: allowed, instruction: instruction,
		}
	})
	return tools.WithManagedRunnerFactory(ctx, factory)
}

// BindHeadlessContext prepares a context for a caller that invokes a tool
// DIRECTLY, outside a turn.
//
// Everything a tool reads comes from context values the orchestrator binds
// per turn, so a caller that skips the turn hands the tool an empty context
// and the tool fails on its first lookup. `yanshi pr` did exactly that: it
// called tools.RunReviewHeadless with the process context, streamReview found
// no SubAgentRunner, and the command answered "review requires a bound
// sub-agent runner" on EVERY invocation -- the subcommand could never have
// worked, and nothing failed at build or startup to say so.
//
// This binds the same execution context a turn would, minus the turn-scoped
// pieces (images, thread links, work-event callbacks) that a headless caller
// has no source for.
func (o *Orchestrator) BindHeadlessContext(ctx context.Context) context.Context {
	ctx = o.bindExecutionContext(ctx, "headless")
	return o.bindSubAgentRunner(ctx)
}

// bindSubAgentRunner returns ctx with a SubAgentRunner bound.
func (o *Orchestrator) bindSubAgentRunner(ctx context.Context) context.Context {
	if o.model == nil {
		return ctx
	}
	depth := tools.SubAgentDepth(ctx)
	return tools.WithSubAgentRunner(ctx, func(ic context.Context, prompt string, allowed []string, instructionOverride string) (string, error) {
		return o.runSubAgentTurn(ic, prompt, allowed, instructionOverride, depth)
	})
}

// runnerFor returns a cached *adk.Runner for the given model and mode,
// building one on first use. The per-model agent reuses the orchestrator's
// instruction, tools, and max-iterations. The model is wrapped with the same
// compaction policy as the default.
//
// cache key is {model.BaseChatModel pointer, runnerToolMode} so switching
// between plan mode and agent mode on the same model yields separate runners
// (plan runner has a filtered tool subset).
//
// On a build error returns nil (not cached). 调用方拿到 nil 会在 .Run 处 panic，
// 这比静默用错工具集的 runner 更早暴露问题（约束 14）。
func (o *Orchestrator) runnerFor(chatModel model.BaseChatModel, plan bool, modelID string) *adk.Runner {
	mode := runnerModeAgent
	if plan {
		mode = runnerModePlan
	}
	key := runnerCacheKey{model: chatModel, mode: mode}
	if cached, ok := o.runners.Load(key); ok {
		return cached.(*adk.Runner)
	}

	registered := o.agentTools
	if plan {
		registered = filterPlanTools(o.agentTools)
	}
	names := collectToolNames(registered)

	agent, err := adk.NewChatModelAgent(context.Background(), &adk.ChatModelAgentConfig{
		Model:         wrapCompaction(chatModel, o.compaction, o.compaction.windowFor(modelID)),
		Instruction:   o.instruction,
		MaxIterations: o.maxIters,
		ToolsConfig: adk.ToolsConfig{
			ToolsNodeConfig: compose.ToolsNodeConfig{
				Tools:               registered,
				UnknownToolsHandler: unknownToolHandler(names),
			},
		},
		Handlers: orchestratorMiddlewares(),
	})
	if err != nil {
		return nil
	}
	runner := adk.NewRunner(context.Background(), adk.RunnerConfig{Agent: agent, EnableStreaming: true})
	actual, _ := o.runners.LoadOrStore(key, runner)
	return actual.(*adk.Runner)
}

// orchestratorMiddlewares returns the ADK middleware stack every runner is
// built with, IN ORDER. The order is load-bearing in three places:
//
//  1. newLoopGuardMiddleware is FIRST, so a turn the guard is about to stop
//     does not spend work preparing a model call that will never go out.
//  2. newResultHygiene runs after the guard and BEFORE the recorder, so what
//     the recorder captures — and therefore what JudgeCompletion and the
//     compactor see — is the degraded history the model was actually sent,
//     not a pre-degradation copy that would make the T4 saving invisible
//     downstream.
//  3. newImageAttacher is LAST, so a native image part appended for this call
//     is not something the hygiene pass has to reason about.
//
// It is a named function rather than a literal inside runnerFor so a test can
// assert what is installed. runnerFor needs a live model to build an agent, so
// the alternative to this seam is either an integration test with a fake
// provider or no check at all — and "the middleware exists and is correct but
// was never added to the slice" is this repository's dominant failure shape.
func orchestratorMiddlewares() []adk.ChatModelAgentMiddleware {
	return []adk.ChatModelAgentMiddleware{
		newLoopGuardMiddleware(), newResultHygiene(), newMessageRecorder(), newImageAttacher(),
	}
}

// filterPlanTools 返回只包含 plan-safe 工具的子集（PlanToolAllowed 白名单）。
// 与 runtime Authorize 的 PlanToolAllowed 共用同一真值，杜绝两份白名单漂移。
func filterPlanTools(all []tool.BaseTool) []tool.BaseTool {
	out := make([]tool.BaseTool, 0, len(all))
	for _, candidate := range all {
		info, err := candidate.Info(context.Background())
		if err == nil && info != nil && tools.PlanToolAllowed(info.Name) {
			out = append(out, candidate)
		}
	}
	return out
}

// FlushRunners clears all cached per-model runners. 下次 turn 按新 key 重建。
func (o *Orchestrator) FlushRunners() {
	o.runners.Range(func(key, value any) bool {
		o.runners.Delete(key)
		return true
	})
}

// Query runs a single user turn and returns the final assistant text.
// The turn span opens at function entry and closes when the iterator is fully
// drained (the synchronous completion point).
//
// The streaming Events* paths cannot be covered from here: they RETURN an
// iterator, so the turn ends wherever the caller stops draining it, which is
// past this function's lifetime. Their spans are therefore opened by the
// drain sites themselves — internal/api/http/ws.go (right after the turn's
// correlation IDs are bound) and internal/api/http/chat.go (around the whole
// schema-retry loop, since one client request is one turn no matter how many
// attempts it takes). This comment previously described that arrangement as
// already existing when neither site had it, so every turn a real user ran
// through either transport produced no span at all.
func (o *Orchestrator) Query(ctx context.Context, userMessage string) (answer string, retErr error) {
	answer, _, retErr = o.QueryWithUsage(ctx, userMessage)
	return answer, retErr
}

// QueryWithUsage is Query plus what the turn cost.
//
// It exists because the goal loop's lightweight path (T0–T2, one orchestrator
// turn) had no way to report into the shared UsageSink: runLightweightGoal
// called Query, Query threw the usage away, and the run record persisted for
// every T0–T2 goal therefore said zero tokens no matter what the turn spent.
// The heavy path (T3–T4) reported through the loop's own components, so the
// gap was invisible to anyone reading only the loop.
//
// Usage is the LATEST model call's, not a sum: providers report cumulative
// totals per call within a turn, so summing would double-count. That is the
// same rule TurnUsage.set follows for the transports.
func (o *Orchestrator) QueryWithUsage(ctx context.Context, userMessage string) (answer string, usage TurnUsage, retErr error) {
	ctx = o.withTurnContext(ctx, TurnOpts{})
	ctx, endTurn := otelobs.StartTurn(ctx, "")
	defer func() { endTurn(retErr) }()
	runner := o.runnerFor(o.rawModel, false, "")

	iter := runner.Query(ctx, userMessage)

	var acc finalOutputAccumulator
	observe := func(msg *schema.Message) {
		if msg != nil && msg.ResponseMeta != nil {
			usage.set(msg.ResponseMeta.Usage)
		}
		acc.observe(msg)
	}
	for {
		ev, ok := iter.Next()
		if !ok {
			break
		}
		if ev.Err != nil {
			return "", usage, fmt.Errorf("orchestrator: agent error: %w", ev.Err)
		}
		if ev.Output != nil && ev.Output.MessageOutput != nil {
			mv := ev.Output.MessageOutput
			if mv.IsStreaming && mv.MessageStream != nil {
				msg, err := mv.GetMessage()
				if err != nil {
					return "", usage, fmt.Errorf("orchestrator: drain stream: %w", err)
				}
				observe(msg)
				continue
			}
			observe(mv.Message)
		}
	}
	out, err := acc.finalize()
	return out, usage, err
}

// Events runs a turn and returns the raw ADK event iterator.
func (o *Orchestrator) Events(ctx context.Context, query string) *adk.AsyncIterator[*adk.AgentEvent] {
	ctx = o.withTurnContext(ctx, TurnOpts{})
	runner := o.runnerFor(o.rawModel, false, "")
	return runner.Query(ctx, query)
}

// EventsWithHistory runs one turn against the full conversation history.
func (o *Orchestrator) EventsWithHistory(ctx context.Context, messages []*schema.Message) *adk.AsyncIterator[*adk.AgentEvent] {
	ctx = o.withTurnContext(ctx, TurnOpts{})
	runner := o.runnerFor(o.rawModel, false, "")
	return runner.Run(ctx, messages)
}

// TurnOpts selects per-turn model + plan mode + context values.
type TurnOpts struct {
	Model               model.BaseChatModel
	ThinkingEffort      string
	OutputSchema        json.RawMessage
	PlanMode            bool
	ThreadID            string
	TurnID              string
	EmitWorkFrame       func(proto.ServerFrame)
	ConnectionSessionID string

	// ModelID is the registry model name for this turn — i.e. the key of
	// Config.MultimodalMap. It is a separate field rather than something
	// derived from Model because Model is a model.BaseChatModel interface
	// value: the orchestrator can call it but cannot recover the name it was
	// registered under, and the multimodal capability lookup is name-keyed.
	// Empty means "the default model", which IsMultimodal reports as
	// non-multimodal (fail-safe: placeholders rather than an unsupported
	// native image part).
	ModelID string

	// Images are the attachments for this turn. EventsWithHistoryOpts applies
	// them exactly once, onto a clone of the caller's message slice, never in
	// place: the WS turn loop retries a turn for schema validation and
	// task_end enforcement and hands the SAME slice (the session's own
	// cs.history on attempt 0) to EventsWithHistoryOpts on every attempt, so
	// an in-place fan-out would append another copy of every image to the
	// persistent session history on each retry.
	Images []proto.ImageAttach

	// AuxUsage, when non-nil, accumulates token usage from AUXILIARY model
	// calls made during this turn — currently image_describe's vision model.
	//
	// Those tokens are spent on the caller's behalf and cost money, but they
	// never pass through the turn's own event stream, so the transport cannot
	// see them without being handed a place to collect them. Same shape as
	// Images: a per-turn field the transport owns, rather than a process-wide
	// counter (which is what the previous sink was, and why it could never
	// reach a per-session cost).
	AuxUsage *registry.Usage
}

// EventsWithHistoryOpts runs one turn with full history and per-turn opts.
func (o *Orchestrator) EventsWithHistoryOpts(ctx context.Context, messages []*schema.Message, opts TurnOpts) *adk.AsyncIterator[*adk.AgentEvent] {
	ctx = o.withTurnContext(ctx, opts)

	// Auxiliary model spend (image_describe) reports through the usage sink,
	// the same channel sub-agent spend uses. Bound here so a tool called on
	// the MAIN turn has somewhere to report; managedTurnRunner binds its own
	// for nested turns, and the inner binding wins there.
	if opts.AuxUsage != nil {
		var mu sync.Mutex
		acc := opts.AuxUsage
		ctx = tools.WithUsageSink(ctx, tools.UsageSink(func(u registry.Usage) {
			mu.Lock()
			defer mu.Unlock()
			*acc = acc.Add(u)
		}))
	}

	// Tier G entry B: "@path" references in the trailing user message become
	// attachments and join TurnOpts.Images before the fan-out below.
	//
	// This sits HERE, not in the transports, because resolution needs exactly
	// two things — the work root and the permission profile — and withTurnContext
	// has just bound both onto ctx one line above. WS/SSE/v1 have neither at
	// hand; pushing the root and the profile out to three transports to do the
	// same parse three times would widen their contract for no gain. It also
	// means the sub-agent path (which reaches this same method with a
	// model-composed prompt) is jailed by construction rather than by remembering
	// to call the parser at a fourth site.
	messages, opts.Images = o.expandPathRefs(ctx, messages, opts.Images)

	// Single convergence point for the image fan-out. All three turn entry
	// points (WS, /api/v1, SSE) only have to fill TurnOpts.ModelID/Images —
	// the "is this model multimodal, embed a native part or store the bytes
	// and leave a placeholder" decision lives here once instead of being
	// reimplemented (and drifting) in three transports.
	//
	// The clone is mandatory, not defensive style. Callers reuse their slice
	// across retries (see TurnOpts.Images), and ApplyImages rewrites the
	// trailing element of whatever slice it is given. A shallow clone is
	// sufficient and is all we pay for: ApplyImages copies the trailing
	// *schema.Message by value and writes the copy back into the slice, so it
	// never mutates a message the caller still points at.
	if len(opts.Images) > 0 {
		applied, err := o.applyImages(slices.Clone(messages), opts.ModelID, opts.Images)
		if err != nil {
			// Reported as a turn error rather than swallowed: the whole point
			// of this branch is that silence here produces a confident answer
			// about an image nobody looked at.
			return singleErrorIterator(err)
		}
		messages = applied
	}

	// adk.WithChatModelOptions ASSIGNS the option slice rather than appending
	// to it, so calling it twice drops the first set. Accumulate every
	// per-turn model option here and hand them over in a single call — a turn
	// can legitimately carry both a reasoning effort and an output schema.
	var modelOpts []model.Option
	// BOTH thinking options go out, every turn. They target different
	// providers and neither can see the other's struct: ReasoningEffortOption
	// produces openai.WithReasoningEffort, which the Anthropic adapter never
	// decodes, and ThinkingOption rides outputSchemaOptions, which the openai
	// path never reads. Sending only the first is what made /think high a
	// no-op on Claude models while every layer above the provider behaved as
	// though it had worked.
	if optPtr := einollm.ReasoningEffortOption(opts.ThinkingEffort); optPtr != nil {
		modelOpts = append(modelOpts, *optPtr)
	}
	if optPtr := einollm.ThinkingOption(opts.ThinkingEffort); optPtr != nil {
		modelOpts = append(modelOpts, *optPtr)
	}
	if optPtr := einollm.OutputSchemaOption(opts.OutputSchema); optPtr != nil {
		modelOpts = append(modelOpts, *optPtr)
	}
	var runOpts []adk.AgentRunOption
	if len(modelOpts) > 0 {
		runOpts = append(runOpts, adk.WithChatModelOptions(modelOpts))
	}

	selectedModel := o.rawModel
	if opts.Model != nil {
		selectedModel = opts.Model
	}
	runner := o.runnerFor(selectedModel, opts.PlanMode, opts.ModelID)
	return runner.Run(ctx, messages, runOpts...)
}

// selectSubAgentTools filters the orchestrator's tool set.
func (o *Orchestrator) selectSubAgentTools(allowed []string) []BaseTool {
	pick := func(t tool.BaseTool) (BaseTool, bool) {
		info, err := t.Info(context.Background())
		if err != nil || info == nil {
			return nil, false
		}
		if len(allowed) > 0 && !allowedToolMatches(allowed, info.Name) {
			return nil, false
		}
		it, ok := t.(tool.InvokableTool)
		return it, ok
	}
	out := make([]BaseTool, 0, len(o.agentTools))
	for _, t := range o.agentTools {
		if it, ok := pick(t); ok {
			out = append(out, it)
		}
	}
	return out
}

// OrchestrationToolNames are the delegation tools a sub-agent must not be
// given, because handing them down is how one turn becomes an unbounded tree.
//
// Exported so bootstrap can check the names against the real tool registry.
// It carried "spawn_agent" until W9: that tool lives in internal/agent/spawn,
// a package whose every file begins with //go:build ignore, so it is not
// merely unwired — it does not compile into the binary at all, and the name
// occupied a "block this in sub-agents" slot that could never match anything.
// GOV5 could not see it: that gate reconciles the guard PROFILE against the
// registry, and this map is neither.
var OrchestrationToolNames = map[string]struct{}{
	"agent_start":    {},
	"workflow_start": {},
	"analysis":       {},
}

func withoutOrchestrationTools(in []BaseTool) []BaseTool {
	out := make([]BaseTool, 0, len(in))
	for _, t := range in {
		info, err := t.Info(context.Background())
		if err != nil || info == nil {
			continue
		}
		if _, blocked := OrchestrationToolNames[info.Name]; blocked {
			continue
		}
		out = append(out, t)
	}
	return out
}

// allowedToolMatches reports whether name is permitted by an allow list, using
// the SAME glob semantics as guard.Tools.Allow.
//
// Exact comparison was the original, and it made one entry unrepresentable: the
// "general" role's allow list is ["*"], written in the vocabulary every other
// allow list in this repo uses, and against an exact match "*" is simply a tool
// name nothing has. A sub-agent spawned with the general role therefore got
// ZERO tools while its allow list said "everything" — no error, no log, just an
// agent that answers from the prompt alone.
//
// Sharing MatchGlob rather than special-casing "*" keeps the two vocabularies
// from drifting: a role allow list of ["fs_*"] now means what the reader
// expects it to.
func allowedToolMatches(allowed []string, name string) bool {
	for _, pat := range allowed {
		if pat == name {
			return true
		}
		if ok, err := guard.MatchGlob(pat, name); err == nil && ok {
			return true
		}
	}
	return false
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// runSubAgentTurn builds a nested orchestrator and runs one turn.
// resolveSharedWorkRoot resolves the work root a non-isolated sub-agent turn
// should execute against: whatever WithWorkRoot already put on ctx, falling
// back to the orchestrator's own workRoot when ctx carries none.
// bindExecutionContext binds o.workRoot onto ctx for every top-level turn, so
// for an ordinary (non-isolated) call this returns exactly o.workRoot — no
// behavior change from before isolated sub-agents existed.
//
// It does double duty for the isolated case too: managedTurnRunner.Run binds
// its own (possibly isolated) workRoot field onto ctx via WithWorkRoot before
// making its recursive call into runSubAgentTurn (see that method's doc
// comment). Reading ctx first, rather than going straight to o.workRoot, is
// what lets that recursive second entry pick up the SAME root the first
// entry already resolved instead of silently falling back to the shared one.
func (o *Orchestrator) resolveSharedWorkRoot(ctx context.Context) string {
	if root := tools.WorkRootFromContext(ctx); root != "" {
		return root
	}
	return o.workRoot
}

// workRootForSubAgentTurn resolves and immediately settles a sub-agent
// workspace, discarding the VCS scope: it exists for tests and diagnostics
// that only care which work root a given ctx/agent pairing would produce,
// without driving a full runSubAgentTurn. Settling with nil merges an
// isolated worktree that received no edits as a truthful empty no-op (see
// acquireSubAgentWorkspace's doc comment on CommitWorktree/ErrNoChanges).
func (o *Orchestrator) workRootForSubAgentTurn(ctx context.Context, agent string) string {
	root, _, settle := o.acquireSubAgentWorkspace(ctx, agent)
	settle(nil)
	return root
}

// acquireSubAgentWorkspace resolves the work root and VCS scope a single
// runSubAgentTurn invocation should run against, and returns a settle func
// the caller MUST invoke exactly once (via defer, keyed off the turn's own
// named error return) with the turn's outcome.
//
// Isolation is opt-in via tools.WithSubAgentIsolation, set only at the
// concurrent fan-out points — agent_dag's executeLevel and agent_batch's
// per-row spawn (via AgentTools/BatchTools) — where two sub-agents can
// genuinely run at the same time and would otherwise share, and clobber, the
// same work root. Every other caller (agent_start/agent_spawn, and the
// recursive second entry runSubAgentTurn reaches through
// managedTurnRunner.Run) takes the "not requested" branch below, which is a
// pure passthrough to resolveSharedWorkRoot/the ambient VCS scope:
// allocating a worktree per single-shot delegation would fill
// ~/.yanshi/worktrees/ for no concurrency benefit.
//
// settle's contract:
//   - Isolation not requested, or requested with nothing to isolate against
//     (no VCS configured — every --fake-model run and most unit tests):
//     settle is a no-op.
//   - runErr != nil: marks the worktree abandoned. A failed sub-agent's
//     partial edits must never reach main.
//   - runErr == nil: first folds the worktree's pending edits into a commit
//     (CommitWorktree — fs_edit only ever leaves them as uncommitted state;
//     ErrNoChanges just means the sub-agent touched nothing and is not a
//     failure), then merges the worktree to main (MergeToMain records
//     WorktreeMerged on success itself; no separate call needed here). A
//     merge conflict is NOT reported back as a turn error — the sub-agent's
//     own work succeeded — but the conflicting worktree is abandoned rather
//     than force-merged: forcing on conflict would silently discard one
//     side's edits, which is exactly the data loss isolation exists to
//     prevent. Two concurrent sub-agents editing the same file is an
//     expected outcome of this mechanism, not a bug in it.
func (o *Orchestrator) acquireSubAgentWorkspace(ctx context.Context, agent string) (string, tools.VCSScope, func(error)) {
	noop := func(error) {}
	scopeFallback := func() tools.VCSScope {
		if scope, ok := tools.VCSScopeFromContext(ctx); ok {
			return scope
		}
		return o.vcsScope
	}
	if !tools.SubAgentIsolationRequested(ctx) {
		return o.resolveSharedWorkRoot(ctx), scopeFallback(), noop
	}
	base := o.vcsScope
	if base.VCS == nil || base.RepoID == "" {
		return o.resolveSharedWorkRoot(ctx), scopeFallback(), noop
	}
	wt, err := base.VCS.AddWorktree(base.RepoID, []string{agent})
	if err != nil {
		// Degrade to the shared root rather than failing the whole sub-agent
		// turn over an isolation nicety.
		obslog.WarnErr(ctx, "sub-agent worktree allocation failed; falling back to shared work root", err)
		return o.resolveSharedWorkRoot(ctx), scopeFallback(), noop
	}
	scope := tools.VCSScope{VCS: base.VCS, RepoID: base.RepoID, WorktreeID: wt.ID, Agent: agent}
	settle := func(runErr error) {
		if runErr != nil {
			if aerr := base.VCS.MarkWorktreeAbandoned(wt.ID); aerr != nil {
				obslog.WarnErr(ctx, "sub-agent worktree: mark abandoned after failure", aerr)
			}
			return
		}
		// fs_edit only records edits into the worktree's uncommitted changeset
		// (RecordEditWorktree); MergeToMain diffs against the worktree's last
		// COMMIT (worktreeTip), not its uncommitted state. Skipping this fold
		// would merge nothing every time, silently discarding a successful
		// sub-agent's edits — the same data loss isolation exists to prevent,
		// just moved one step later. ErrNoChanges (the sub-agent edited
		// nothing) is expected and not a failure; mirrors acpdelegate.go's
		// mergeWorktree, which is the existing precedent for this sequence.
		if _, cerr := base.VCS.CommitWorktree(wt.ID, agent, "subagent: "+agent); cerr != nil && !errors.Is(cerr, vcs.ErrNoChanges) {
			obslog.WarnErr(ctx, "sub-agent worktree: commit failed, abandoning", cerr)
			if aerr := base.VCS.MarkWorktreeAbandoned(wt.ID); aerr != nil {
				obslog.WarnErr(ctx, "sub-agent worktree: mark abandoned after commit failure", aerr)
			}
			return
		}
		if _, conflicts, merr := base.VCS.MergeToMain(wt.ID, agent, false); merr != nil {
			if len(conflicts) > 0 {
				obslog.WarnErr(ctx, fmt.Sprintf("sub-agent worktree %s: merge conflicts %v, abandoning", wt.ID, conflicts), merr)
			} else {
				obslog.WarnErr(ctx, "sub-agent worktree: merge to main failed, abandoning", merr)
			}
			if aerr := base.VCS.MarkWorktreeAbandoned(wt.ID); aerr != nil {
				obslog.WarnErr(ctx, "sub-agent worktree: mark abandoned after merge failure", aerr)
			}
		}
	}
	return wt.Path, scope, settle
}

func (o *Orchestrator) runSubAgentTurn(ctx context.Context, prompt string, allowed []string, instructionOverride string, depth int) (out string, err error) {
	if depth >= tools.MaxSubAgentDepth {
		return "", fmt.Errorf("sub-agent nesting depth %d exceeds limit %d", depth+1, tools.MaxSubAgentDepth)
	}
	selected := o.selectSubAgentTools(allowed)
	if tools.LeafSubAgentTools(ctx) {
		selected = withoutOrchestrationTools(selected)
	}

	// Resolve the work root/VCS scope for THIS invocation once, and settle it
	// (merge or abandon — see acquireSubAgentWorkspace's doc comment) exactly
	// once no matter which return path below fires. Named returns make that a
	// single defer instead of a runErr variable duplicated at every return
	// site, which is what keeps every failure path — including ones added
	// later — routed through settle without having to remember to.
	root, scope, settle := o.acquireSubAgentWorkspace(ctx, "subagent")
	defer func() { settle(err) }()

	// Route through the Manager when one is bound. This is what puts a
	// delegated turn under the concurrency cap: run inline and the sub-agent
	// takes no slot, so the cap describes a population nobody is a member of.
	//
	// It is also why Park exists. A parent blocks in Manager.Wait while holding
	// its own slot, and its child retries Spawn until one frees; at cap N, N
	// delegating parents would hold every slot waiting for children that can
	// never start. ManagedSubAgentRun parks the caller across that wait, so the
	// two changes are a pair — this routing without parking is a livelock.
	//
	// Falls through to the inline path when either the Manager or the runner
	// factory is absent, which is the shape every non-orchestrator caller and
	// most tests see.
	if mgr := tools.ManagerFromContext(ctx); mgr != nil {
		if factory := tools.ManagedRunnerFactoryFromContext(ctx); factory != nil {
			runner := factory(allowed, instructionOverride)
			// The factory closure captures workRoot/vcsScope ONCE per top-level
			// turn (see bindManagedRunner), so every concurrent DAG step/batch
			// row would otherwise get the same (non-isolated) values. Mutating
			// THIS call's freshly-constructed *managedTurnRunner in place is
			// safe: factory returns a new pointer per call, so concurrent
			// callers never share one. managedTurnRunner.Run rebinds these
			// fields onto its own (Manager-detached) ctx before recursing back
			// into runSubAgentTurn, which is how the isolated root/scope
			// survives that detachment (see resolveSharedWorkRoot).
			if mtr, ok := runner.(*managedTurnRunner); ok {
				mtr.workRoot = root
				mtr.vcsScope = scope
			}
			res, rerr := tools.ManagedSubAgentRun(ctx, tools.ManagedSubAgentSpec{
				ParentID:     registry.CurrentAgentID(ctx),
				Prompt:       prompt,
				AllowedTools: allowed,
				Instruction:  instructionOverride,
				Runner:       runner,
			})
			if rerr != nil {
				return "", rerr
			}
			// mgr.Wait only errors on WAIT mechanics (context cancellation,
			// timeout, unknown id) — a sub-agent that ran to a failed,
			// cancelled, or interrupted terminal state returns (res, nil).
			// Terminal must be checked explicitly, or every managed failure
			// would report success with empty content AND settle would merge
			// a failed/partial worktree instead of abandoning it.
			if res.Terminal != registry.StatusCompleted {
				msg := res.Error
				if msg == "" {
					msg = fmt.Sprintf("sub-agent ended with status %s", res.Terminal)
				}
				return "", fmt.Errorf("sub-agent: %s", msg)
			}
			return res.Text, nil
		}
	}

	subInstruction := instructionOverride
	if subInstruction == "" {
		// Inherit path: o.baseInstruction already contains memorySuffix, so we
		// use it verbatim and MUST NOT append again. v2 erroneously appended
		// unconditionally → double injection (FN4). The Inherit behavioral
		// test catches this by counting markers in the captured system prompt.
		subInstruction = o.baseInstruction
	} else {
		// Override path: the override replaces baseInstruction wholesale, so
		// memorySuffix would be lost. Re-append once. The Override behavioral
		// test asserts exactly one marker here.
		if o.memorySuffix != "" {
			subInstruction = subInstruction + "\n\n" + o.memorySuffix
		}
	}

	sub, serr := New(Config{
		Model:       o.rawModel, // 用未包裹的 rawModel，让子 Orchestrator 独立构建 compaction
		Tools:       selected,
		Profile:     o.profile,
		MaxIters:    o.maxIters,
		Compaction:  o.compaction,
		Instruction: subInstruction,
		WorkRoot:    root,
		VCSScope:    scope,
		LSP:         o.lspMgr,
		// W-A-02 fix round 2: without this the sub-Orchestrator built here has
		// a nil redactor and bindExecutionContext's `if o.redactor != nil`
		// gate silently skips WithRedactor for this sub-agent's entire turn.
		Redactor: o.redactor,
		// The sub-agent inherits the loop-guard POLICY and gets its own fresh
		// per-turn STATE (its EventsWithHistoryOpts call below runs
		// withTurnContext, which rebinds). Without this line delegation is a
		// budget escape hatch: an agent that has burnt its shell_run budget
		// spawns a sub-agent and gets a full one.
		LoopGuard: o.loopGuard,
	})
	if serr != nil {
		resp, gerr := o.model.Generate(ctx, []*schema.Message{schema.UserMessage(prompt)})
		if gerr != nil || resp == nil {
			return "", fmt.Errorf("sub-agent: build failed (%w) and fallback generate failed", serr)
		}
		return resp.Content, nil
	}
	subCtx := tools.WithSubAgentDepth(ctx, depth+1)
	subCtx = tools.WithErrCounter(subCtx)

	emit := tools.SubAgentEmitFrom(ctx)

	// sub-agent 继承父的 plan mode / thread link（已决策约束 §4 sub-agent 不扩大工具面）。
	link, _ := tools.ThreadLinkFromContext(ctx)
	iter := sub.EventsWithHistoryOpts(subCtx, []*schema.Message{schema.UserMessage(prompt)}, TurnOpts{
		PlanMode: tools.PlanModeActive(ctx),
		ThreadID: link.ThreadID,
		TurnID:   link.TurnID,
	})

	var content strings.Builder
	var errMsg string
	var latest, subUsage TurnUsage
	displayOf := make(map[string]string)
	for _, tl := range selected {
		if t, ok := tl.(tools.Tool); ok {
			if info, ierr := t.Info(ctx); ierr == nil && info != nil {
				displayOf[info.Name] = t.DisplayName()
			}
		}
	}

	progress := tools.SubAgentProgressFromContext(ctx)
	ClassifyEventsWithUsage(iter, &latest, func(f proto.ServerFrame) {
		if emit != nil {
			emit(f)
		}
		switch f.Type {
		case "agent_chunk":
			content.WriteString(f.Text)
		case "error":
			errMsg = f.Text
		case "tool_call":
			if progress != nil && f.Status == "running" {
				display := f.ToolName
				if d, ok := displayOf[f.ToolName]; ok {
					display = d
				}
				progress(tools.SubAgentEvent{Kind: tools.SubAgentToolStart, ToolDisplay: display, ToolArgs: f.ToolArgs})
			}
		case "tool_result":
			if progress != nil {
				progress(tools.SubAgentEvent{Kind: tools.SubAgentToolEnd})
			}
		}
	}, func(u TurnUsage) {
		subUsage.PromptTokens += u.PromptTokens
		subUsage.CompletionTokens += u.CompletionTokens
		if progress != nil {
			progress(tools.SubAgentEvent{Kind: tools.SubAgentTokens, Tokens: u.PromptTokens + u.CompletionTokens})
		}
	})

	// Forward what the sub-agent spent to whoever is metering this turn.
	// subUsage was accumulated and then dropped: tools.UsageSinkFrom had zero
	// production callers, so every token a sub-agent burned was invisible to
	// the parent's budget. A delegating agent could therefore spend without
	// limit by the simple expedient of delegating.
	//
	// Reported even when the turn errored — the tokens were spent either way,
	// and a budget that only counts successful work is a budget a failing loop
	// can run past.
	if sink := tools.UsageSinkFrom(ctx); sink != nil {
		if u := subAgentUsageForSink(subUsage); u != nil {
			sink(*u)
		}
	}

	if errMsg != "" {
		return "", fmt.Errorf("sub-agent: %s", errMsg)
	}
	return content.String(), nil
}

// subAgentUsageForSink converts a sub-agent turn's spend into the shape the
// parent's sink accepts, or nil when there is nothing to report.
//
// Split out because the mapping is where this silently goes wrong: drop one
// field and the parent under-counts by exactly that much forever, with no
// error and no missing event — the budget simply runs longer than it should.
// Nothing observed the mapping until W3 review round 5 zeroed
// CompletionTokens and watched the suite stay green.
//
// TotalTokens is derived rather than copied: TurnUsage has no total, and a
// sink entry whose total disagrees with its parts would make every downstream
// sum depend on which field the reader trusted.
func subAgentUsageForSink(u TurnUsage) *registry.Usage {
	if u.PromptTokens == 0 && u.CompletionTokens == 0 {
		return nil
	}
	return &registry.Usage{
		PromptTokens:     int64(u.PromptTokens),
		CompletionTokens: int64(u.CompletionTokens),
		TotalTokens:      int64(u.PromptTokens + u.CompletionTokens),
	}
}

// managedTurnRunner is a concrete registry.Runner. It re-binds the turn context
// (profile / workroot / VCS / redactor / emit / depth / currentAgentID + role)
// on the Manager's cancellation-only child context, enforcing role policy and
// routing usage back to the Manager via AddUsage.
//
// W-A-02 fix round 2: this list was wrong for a whole class of agents. The
// Manager's RootContext is context.Background() (bootstrap.go), so a managed
// sub-agent turn gets NOTHING from the main turn's context except what this
// method re-binds explicitly — WithRedactor was missing, so every
// shell_run/fs_read result a managed sub-agent produced reached its model
// call unredacted. Any future field added to bindExecutionContext that a
// sub-agent turn must also see needs the same treatment here.
type managedTurnRunner struct {
	o           *Orchestrator
	mgr         *registry.Manager
	profile     guard.PermissionProfile
	workRoot    string
	vcsScope    tools.VCSScope
	depth       int
	emit        tools.SubAgentEmit
	allowed     []string
	instruction string
}

func (r *managedTurnRunner) Run(ctx context.Context, agentID, assignment string) (string, error) {
	ctx = tools.WithProfile(ctx, r.profile)
	ctx = tools.WithWorkRoot(ctx, r.workRoot)
	if r.vcsScope.VCS != nil {
		ctx = tools.WithVCS(ctx, r.vcsScope)
	}
	if r.o.redactor != nil {
		ctx = tools.WithRedactor(ctx, r.o.redactor)
	}
	if r.emit != nil {
		ctx = tools.WithSubAgentEmit(ctx, r.emit)
	}
	ctx = tools.WithSubAgentDepth(ctx, r.depth)
	ctx = registry.WithCurrentAgentID(ctx, agentID)
	ctx = tools.WithUsageSink(ctx, tools.UsageSink(func(u registry.Usage) {
		_ = r.mgr.AddUsage(agentID, u)
	}))
	instruction := r.instruction
	if role := registry.RoleFromContext(ctx); role != "" {
		if def := roleForSubagent(role); def != nil {
			if def.PromptPrefix != "" {
				instruction = def.PromptPrefix + "\n\n" + instruction
			}
			if def.Policy != nil {
				ctx = tools.WithRolePolicy(ctx, *def.Policy)
			}
		}
	}
	return r.o.runSubAgentTurn(ctx, assignment, r.allowed, instruction, r.depth)
}

func roleForSubagent(name string) *tools.RoleDef {
	for _, r := range tools.AgentRoles() {
		if r.Name == name {
			def := r
			return &def
		}
	}
	return nil
}

// MemorySuffix returns the memory suffix string (MEM1) configured on this
// orchestrator. Exposed for diagnostics and bootstrap tests; production code
// passes it via Config.
func (o *Orchestrator) MemorySuffix() string { return o.memorySuffix }
