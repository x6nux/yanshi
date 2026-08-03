// Package orchestrator is yanshi's main coordinating agent (Eino ADK ChatModelAgent).
package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
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
	"github.com/x6nux/yanshi/internal/shell"
	"github.com/x6nux/yanshi/internal/task/work"
	"github.com/x6nux/yanshi/internal/tools"
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
	// MemorySuffix is appended to Instruction (after SkillMetaPrompt) as an
	// independent block, so it survives instructionOverride in runSubAgentTurn.
	// It carries the user/project memory block (MEM1) so the model sees user
	// preferences across sessions and across sub-agent boundaries. Empty = no
	// memory injection (the default).
	MemorySuffix string
}

// CompactionConfig mirrors config.CompactionConfig.
type CompactionConfig struct {
	Threshold     float64
	ContextWindow int
	KeepRecent    int
	// CooldownTokens is the minimum token growth since the last successful
	// compaction before another one is allowed (per-model instance). 0 means
	// no token-growth cooldown.
	CooldownTokens int
	// CooldownDuration is the minimum wall-time since last compaction. <=0
	// means no time-based cooldown.
	CooldownDuration time.Duration
	// HardForceFraction forces compaction once estimated tokens reach this
	// fraction of ContextWindow, even when inside a cooldown period. 0 disables.
	HardForceFraction float64
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
	secureFactory   secproc.Factory
	shellManager    *shell.Manager
	subagentMgr     *registry.Manager
	lspMgr          tools.LSPManager
	mcpMgr          *mcp.Manager
	availableModels map[string]bool
	multimodalMap   map[string]bool
	imageStore      *imagestore.Store
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
		model:           wrapCompaction(cfg.Model, cfg.Compaction),
		rawModel:        rawModel,
		profile:         profile,
		vcsScope:        cfg.VCSScope,
		workRoot:        cfg.WorkRoot,
		instruction:     instruction,
		baseInstruction: baseInstruction,
		memorySuffix:    cfg.MemorySuffix,
		agentTools:      agentTools,
		toolNames:       toolNames,
		maxIters:        maxIters,
		compaction:      cfg.Compaction,
		taskManager:     cfg.TaskManager,
		approvals:       cfg.Approvals,
		sandbox:         cfg.Sandbox,
		networkPolicy:   cfg.NetworkPolicy,
		secureFactory:   cfg.SecureFactory,
		shellManager:    cfg.ShellManager,
		subagentMgr:     cfg.SubagentManager,
		lspMgr:          cfg.LSP,
		mcpMgr:          cfg.MCP,
		multimodalMap:   cfg.MultimodalMap,
		imageStore:      cfg.ImageStore,
		availableModels: cfg.AvailableModels,
	}, nil
}

// wrapCompaction returns m wrapped in einollm.CompactingModel when compaction
// is enabled, else m unchanged.
func wrapCompaction(m model.BaseChatModel, cc CompactionConfig) model.BaseChatModel {
	if cc.Threshold <= 0 {
		return m
	}
	return &einollm.CompactingModel{
		Inner:             m,
		Threshold:         cc.Threshold,
		ContextWindow:     cc.ContextWindow,
		KeepRecent:        cc.KeepRecent,
		CooldownTokens:    cc.CooldownTokens,
		CooldownDuration:  cc.CooldownDuration,
		HardForceFraction: cc.HardForceFraction,
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

// ProfileForTest exposes the resolved profile for tests.
func (o *Orchestrator) ProfileForTest() guard.PermissionProfile { return o.profile }

// bindExecutionContext threads every orchestrator-owned security value into ctx.
func (o *Orchestrator) bindExecutionContext(ctx context.Context, connectionSessionID string) context.Context {
	ctx = tools.WithProfile(ctx, o.profile)
	ctx = tools.WithWorkRoot(ctx, o.workRoot)
	ctx = tools.WithTaskManager(ctx, o.taskManager)
	if o.approvals != nil && connectionSessionID != "" {
		ctx = tools.WithApprovalManager(ctx, o.approvals, connectionSessionID)
	}
	if o.sandbox != nil {
		ctx = tools.WithSandbox(ctx, o.sandbox)
	}
	if o.networkPolicy != nil {
		ctx = tools.WithNetworkPolicy(ctx, o.networkPolicy)
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
	return ctx
}

// withTurnContext is the single helper for all 4 turn entry points: it binds
// Profile, WorkRoot, TaskManager, PlanMode, ThreadLink, and the work-event
// callback onto ctx, then binds the sub-agent runner.
func (o *Orchestrator) withTurnContext(ctx context.Context, opts TurnOpts) context.Context {
	ctx = ensureTurnIDs(ctx)
	ctx = o.bindExecutionContext(ctx, opts.ConnectionSessionID)
	ctx = tools.WithPlanMode(ctx, opts.PlanMode)
	ctx = tools.WithThreadLink(ctx, opts.ThreadID, opts.TurnID)
	if opts.EmitWorkFrame != nil {
		ctx = tools.WithWorkEventCallback(ctx, func(event work.Event) {
			opts.EmitWorkFrame(workEventFrame(event))
		})
	}
	if o.mcpMgr != nil && o.mcpMgr.Enabled() {
		ctx = tools.WithMCP(ctx, o.mcpMgr)
	}
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
func (o *Orchestrator) runnerFor(chatModel model.BaseChatModel, plan bool) *adk.Runner {
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
		Model:         wrapCompaction(chatModel, o.compaction),
		Instruction:   o.instruction,
		MaxIterations: o.maxIters,
		ToolsConfig: adk.ToolsConfig{
			ToolsNodeConfig: compose.ToolsNodeConfig{
				Tools:               registered,
				UnknownToolsHandler: unknownToolHandler(names),
			},
		},
		Handlers: []adk.ChatModelAgentMiddleware{newMessageRecorder()},
	})
	if err != nil {
		return nil
	}
	runner := adk.NewRunner(context.Background(), adk.RunnerConfig{Agent: agent, EnableStreaming: true})
	actual, _ := o.runners.LoadOrStore(key, runner)
	return actual.(*adk.Runner)
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
// drained (the synchronous completion point). Streaming Events* paths manage
// their own spans at the WS/SSE drain boundary.
func (o *Orchestrator) Query(ctx context.Context, userMessage string) (answer string, retErr error) {
	ctx = o.withTurnContext(ctx, TurnOpts{})
	ctx, endTurn := otelobs.StartTurn(ctx, "")
	defer func() { endTurn(retErr) }()
	runner := o.runnerFor(o.rawModel, false)

	iter := runner.Query(ctx, userMessage)

	var acc finalOutputAccumulator
	for {
		ev, ok := iter.Next()
		if !ok {
			break
		}
		if ev.Err != nil {
			return "", fmt.Errorf("orchestrator: agent error: %w", ev.Err)
		}
		if ev.Output != nil && ev.Output.MessageOutput != nil {
			mv := ev.Output.MessageOutput
			if mv.IsStreaming && mv.MessageStream != nil {
				msg, err := mv.GetMessage()
				if err != nil {
					return "", fmt.Errorf("orchestrator: drain stream: %w", err)
				}
				acc.observe(msg)
				continue
			}
			acc.observe(mv.Message)
		}
	}
	return acc.finalize()
}

// Events runs a turn and returns the raw ADK event iterator.
func (o *Orchestrator) Events(ctx context.Context, query string) *adk.AsyncIterator[*adk.AgentEvent] {
	ctx = o.withTurnContext(ctx, TurnOpts{})
	runner := o.runnerFor(o.rawModel, false)
	return runner.Query(ctx, query)
}

// EventsWithHistory runs one turn against the full conversation history.
func (o *Orchestrator) EventsWithHistory(ctx context.Context, messages []*schema.Message) *adk.AsyncIterator[*adk.AgentEvent] {
	ctx = o.withTurnContext(ctx, TurnOpts{})
	runner := o.runnerFor(o.rawModel, false)
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
}

// EventsWithHistoryOpts runs one turn with full history and per-turn opts.
func (o *Orchestrator) EventsWithHistoryOpts(ctx context.Context, messages []*schema.Message, opts TurnOpts) *adk.AsyncIterator[*adk.AgentEvent] {
	ctx = o.withTurnContext(ctx, opts)

	// adk.WithChatModelOptions ASSIGNS the option slice rather than appending
	// to it, so calling it twice drops the first set. Accumulate every
	// per-turn model option here and hand them over in a single call — a turn
	// can legitimately carry both a reasoning effort and an output schema.
	var modelOpts []model.Option
	if optPtr := einollm.ReasoningEffortOption(opts.ThinkingEffort); optPtr != nil {
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
	runner := o.runnerFor(selectedModel, opts.PlanMode)
	return runner.Run(ctx, messages, runOpts...)
}

// selectSubAgentTools filters the orchestrator's tool set.
func (o *Orchestrator) selectSubAgentTools(allowed []string) []BaseTool {
	pick := func(t tool.BaseTool) (BaseTool, bool) {
		info, err := t.Info(context.Background())
		if err != nil || info == nil {
			return nil, false
		}
		if len(allowed) > 0 && !contains(allowed, info.Name) {
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

func withoutOrchestrationTools(in []BaseTool) []BaseTool {
	constNames := map[string]struct{}{
		"agent_start":    {},
		"workflow_start": {},
		"analysis":       {},
		"spawn_agent":    {},
	}
	out := make([]BaseTool, 0, len(in))
	for _, t := range in {
		info, err := t.Info(context.Background())
		if err != nil || info == nil {
			continue
		}
		if _, blocked := constNames[info.Name]; blocked {
			continue
		}
		out = append(out, t)
	}
	return out
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
func (o *Orchestrator) runSubAgentTurn(ctx context.Context, prompt string, allowed []string, instructionOverride string, depth int) (string, error) {
	if depth >= tools.MaxSubAgentDepth {
		return "", fmt.Errorf("sub-agent nesting depth %d exceeds limit %d", depth+1, tools.MaxSubAgentDepth)
	}
	selected := o.selectSubAgentTools(allowed)
	if tools.LeafSubAgentTools(ctx) {
		selected = withoutOrchestrationTools(selected)
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

	sub, err := New(Config{
		Model:       o.rawModel, // 用未包裹的 rawModel，让子 Orchestrator 独立构建 compaction
		Tools:       selected,
		Profile:     o.profile,
		MaxIters:    o.maxIters,
		Compaction:  o.compaction,
		Instruction: subInstruction,
		WorkRoot:    o.workRoot,
		LSP:         o.lspMgr,
	})
	if err != nil {
		resp, gerr := o.model.Generate(ctx, []*schema.Message{schema.UserMessage(prompt)})
		if gerr != nil || resp == nil {
			return "", fmt.Errorf("sub-agent: build failed (%w) and fallback generate failed", err)
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
			if info, err := t.Info(ctx); err == nil && info != nil {
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

	if errMsg != "" {
		return "", fmt.Errorf("sub-agent: %s", errMsg)
	}
	return content.String(), nil
}

// managedTurnRunner is a concrete registry.Runner. It re-binds the turn context
// (profile / workroot / VCS / emit / depth / currentAgentID + role) on the
// Manager's cancellation-only child context, enforcing role policy and routing
// usage back to the Manager via AddUsage.
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
