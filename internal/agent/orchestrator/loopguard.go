package orchestrator

import (
	"context"
	"sync"
	"time"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"

	"github.com/x6nux/yanshi/internal/loopguard"
)

// LoopGuardConfig configures the per-turn stop conditions the orchestrator
// installs on every ReAct loop.
//
// Every field is "off when zero", so the zero LoopGuardConfig installs nothing
// and the orchestrator behaves exactly as it did before this existed. That is
// deliberate: a stop condition that switches itself on with a guessed default
// would silently truncate turns on an installation whose operator never asked
// for one, and the failure would look like the model giving up.
type LoopGuardConfig struct {
	// RepetitionEnabled turns on L1 (repeated identical tool calls). The
	// thresholds are loopguard's defaults, ported from QwenPaw.
	RepetitionEnabled bool
	// RepetitionWindow overrides the sliding window size. 0 selects
	// loopguard.DefaultRepetitionWindow.
	RepetitionWindow int
	// RepetitionWarnAfter / RepetitionStopAfter override the escalation
	// thresholds. Both zero selects loopguard.DefaultRepetitionStages.
	RepetitionWarnAfter int
	// RepetitionStopAfter is the consecutive-hit count at which the turn ends.
	RepetitionStopAfter int
	// MaxToolCalls caps total tool calls in one turn. 0 = no cap.
	MaxToolCalls int
	// PerToolCalls caps calls per tool name in one turn ("shell_run": 20).
	// Names are matched exactly; see loopguard.ToolBudgetConfig.
	PerToolCalls map[string]int
	// TurnTimeout is the wall-clock limit for one turn, checked only at
	// iteration boundaries. 0 = no limit.
	TurnTimeout time.Duration
	// MaxTurnTokens caps accumulated token spend for one turn. 0 = no limit.
	MaxTurnTokens int
}

// enabled reports whether the config asks for any gate or budget at all.
func (c LoopGuardConfig) enabled() bool {
	return c.RepetitionEnabled || c.MaxToolCalls > 0 || len(c.PerToolCalls) > 0 ||
		c.TurnTimeout > 0 || c.MaxTurnTokens > 0
}

// buildHandler constructs the per-turn gate set described by c.
func (c LoopGuardConfig) buildHandler() *loopguard.Handler {
	var gates []loopguard.Gate
	if c.RepetitionEnabled {
		var stages []loopguard.RepetitionStage
		if c.RepetitionWarnAfter > 0 || c.RepetitionStopAfter > 0 {
			if c.RepetitionWarnAfter > 0 {
				stages = append(stages, loopguard.RepetitionStage{After: c.RepetitionWarnAfter})
			}
			if c.RepetitionStopAfter > 0 {
				stages = append(stages, loopguard.RepetitionStage{After: c.RepetitionStopAfter, Stop: true})
			}
		}
		gates = append(gates, loopguard.NewRepetitionGate(loopguard.RepetitionConfig{
			Window: c.RepetitionWindow,
			Stages: stages,
		}))
	}
	// The typed nils returned by these constructors for "no limit" are dropped
	// by NewHandler, which is why they can be appended unconditionally --
	// except that a typed nil inside a non-nil interface is NOT nil, so each
	// one is checked before it becomes a loopguard.Gate.
	if g := loopguard.NewTokenBudgetGate(c.MaxTurnTokens); g != nil {
		gates = append(gates, g)
	}
	if g := loopguard.NewDeadlineGate(c.TurnTimeout); g != nil {
		gates = append(gates, g)
	}
	return loopguard.NewHandler(gates...)
}

// buildToolBudget constructs the per-turn tool-call budget described by c, or
// nil when c imposes none.
func (c LoopGuardConfig) buildToolBudget() *loopguard.ToolBudget {
	return loopguard.NewToolBudget(loopguard.ToolBudgetConfig{
		PerTool:  c.PerToolCalls,
		MaxTotal: c.MaxToolCalls,
	})
}

// turnGuard is the per-turn mutable state behind the loop guard.
//
// It lives in the turn's context rather than on the middleware because
// runnerFor MEMOISES runners: one *adk.Runner, and therefore one middleware
// instance, is reused by every turn of every session that runs on the same
// model and mode. A counter on the middleware would be a process-wide counter,
// so the first user to exhaust a budget would exhaust it for everyone until
// the process restarted. Everything mutable is here, created fresh per turn.
type turnGuard struct {
	handler *loopguard.Handler
	budget  *loopguard.ToolBudget
	started time.Time
	// now is the clock, injectable so deadline tests need not sleep.
	now func() time.Time

	mu        sync.Mutex
	iteration int
	// seen is the number of leading state.Messages already scanned for tool
	// calls, so each call is fed to the gates exactly once.
	seen int
	// nudged counts injected continuation messages, used to keep a gate that
	// stays unhappy from appending the same nudge on every iteration.
	nudges map[string]int
}

// maxNudgesPerGate bounds how many continuation messages ONE gate may inject
// in a single turn.
//
// Without it a gate whose condition is still true after the nudge (the model
// ignored it, which is a thing models do) appends another copy on every
// iteration, and the conversation fills with identical warnings that push out
// the actual work. The repetition gate escalates to a hard stop on its own, so
// this bound is a backstop for gate implementations that only ever nudge.
const maxNudgesPerGate = 2

// loopGuardKey is the context key for the per-turn guard state.
type loopGuardKey struct{}

// WithLoopGuard binds a fresh per-turn loop-guard state onto ctx, built from
// cfg. Returns ctx unchanged when cfg asks for no gate at all, so the
// middleware's context lookup fails and every hook short-circuits.
//
// Exported because the goal loop and any future embedder that drives the
// orchestrator through a non-turn entry point needs the same guarantees a WS
// turn gets; unexported, the only way to obtain them would be to route through
// withTurnContext, which binds a dozen unrelated things.
func WithLoopGuard(ctx context.Context, cfg LoopGuardConfig) context.Context {
	if !cfg.enabled() {
		return ctx
	}
	return context.WithValue(ctx, loopGuardKey{}, &turnGuard{
		handler: cfg.buildHandler(),
		budget:  cfg.buildToolBudget(),
		started: time.Now(),
		now:     time.Now,
		nudges:  make(map[string]int),
	})
}

// loopGuardFromContext reads back the state bound by WithLoopGuard.
func loopGuardFromContext(ctx context.Context) (*turnGuard, bool) {
	g, ok := ctx.Value(loopGuardKey{}).(*turnGuard)
	return g, ok && g != nil
}

// observe advances the iteration counter, harvests everything new in msgs, and
// asks the handler for a verdict.
func (g *turnGuard) observe(msgs []*schema.Message) loopguard.Result {
	g.mu.Lock()
	obs := loopguard.Observation{
		Iteration: g.iteration,
		Elapsed:   g.now().Sub(g.started),
	}
	obs.Calls = collectToolCalls(msgs, g.seen)
	obs.PromptTokens, obs.CompletionTokens = latestUsage(msgs, g.seen)
	g.iteration++
	g.seen = len(msgs)
	g.mu.Unlock()

	res := g.handler.Evaluate(obs)
	if res.Action != loopguard.ActionModifyPrompt {
		return res
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.nudges[res.Gate] >= maxNudgesPerGate {
		return loopguard.Result{}
	}
	g.nudges[res.Gate]++
	return res
}

// collectToolCalls extracts every tool call recorded in msgs[from:].
func collectToolCalls(msgs []*schema.Message, from int) []loopguard.ToolCall {
	if from < 0 {
		from = 0
	}
	if from >= len(msgs) {
		return nil
	}
	var out []loopguard.ToolCall
	for _, m := range msgs[from:] {
		if m == nil {
			continue
		}
		for _, tc := range m.ToolCalls {
			name := tc.Function.Name
			if name == "" {
				continue
			}
			out = append(out, loopguard.ToolCall{
				Name:     name,
				ArgsHash: loopguard.HashArgs(tc.Function.Arguments),
			})
		}
	}
	return out
}

// latestUsage returns the token usage of the most recent message in msgs[from:]
// that carries any, or (0, 0) when none does.
//
// Scanning only the new tail rather than the whole history is what keeps the
// budget gate's "prompt growth since last observation" arithmetic honest: a
// re-scan from zero would keep re-reporting the same first call's usage on
// every iteration once a provider stopped attaching usage.
func latestUsage(msgs []*schema.Message, from int) (prompt, completion int) {
	if from < 0 {
		from = 0
	}
	if from >= len(msgs) {
		return 0, 0
	}
	for i := len(msgs) - 1; i >= from; i-- {
		m := msgs[i]
		if m == nil || m.ResponseMeta == nil || m.ResponseMeta.Usage == nil {
			continue
		}
		return m.ResponseMeta.Usage.PromptTokens, m.ResponseMeta.Usage.CompletionTokens
	}
	return 0, 0
}

// loopGuardMiddleware is the ADK-facing half of the loop guard.
//
// It is deliberately stateless (see turnGuard): the instance is shared by every
// turn that runs on a memoised runner, so all it may do is find the turn's own
// state in ctx and delegate.
type loopGuardMiddleware struct {
	*adk.BaseChatModelAgentMiddleware
}

// newLoopGuardMiddleware builds a loopGuardMiddleware ready for installation on
// an adk.ChatModelAgentConfig.Handlers slice.
func newLoopGuardMiddleware() *loopGuardMiddleware {
	return &loopGuardMiddleware{BaseChatModelAgentMiddleware: &adk.BaseChatModelAgentMiddleware{}}
}

// BeforeModelRewriteState is the iteration boundary: every ReAct iteration
// passes through here exactly once, after the previous iteration's tool results
// have been appended and before the next model call goes out.
//
// That position is what makes it the ONLY safe place to end a turn early. At
// this point the message history is guaranteed to have no assistant tool_call
// awaiting a tool_result — a state every provider rejects — so returning an
// error here leaves the session usable, while a mid-flight cancellation would
// not. It is also why the deadline gate is checked here rather than through a
// context deadline (see loopguard.DeadlineGate).
//
// A stop is reported as an error, which the ADK surfaces on the event stream
// as ev.Err and the transports render as an error frame. A nudge is appended
// as a user message, the same shape the judge-driven retry uses.
func (m *loopGuardMiddleware) BeforeModelRewriteState(
	ctx context.Context,
	state *adk.ChatModelAgentState,
	_ *adk.ModelContext,
) (context.Context, *adk.ChatModelAgentState, error) {
	g, ok := loopGuardFromContext(ctx)
	if !ok || state == nil {
		return ctx, state, nil
	}
	res := g.observe(state.Messages)
	switch res.Action {
	case loopguard.ActionStop:
		return ctx, state, loopguard.NewStopError(res)
	case loopguard.ActionModifyPrompt:
		state.Messages = append(append([]*schema.Message(nil), state.Messages...),
			schema.UserMessage(res.Continuation))
	}
	return ctx, state, nil
}

// WrapInvokableToolCall enforces the per-turn tool-call budget (L2).
//
// Over-budget calls return their refusal AS THE TOOL RESULT, with a nil error.
// This is the same rule UnknownToolsHandler follows and for the same reason: a
// Go error out of a tool node is a NodeRunError, which tears down the entire
// turn, whereas a result is handed back to the model, which reads "you have
// used your shell_run budget" and can do something else. Returning an error
// would turn "you may not run one more command" into "your session died".
func (m *loopGuardMiddleware) WrapInvokableToolCall(
	ctx context.Context,
	endpoint adk.InvokableToolCallEndpoint,
	tCtx *adk.ToolContext,
) (adk.InvokableToolCallEndpoint, error) {
	g, ok := loopGuardFromContext(ctx)
	if !ok || g.budget == nil || tCtx == nil || tCtx.Name == "" {
		return endpoint, nil
	}
	name := tCtx.Name
	budget := g.budget
	return func(callCtx context.Context, argsJSON string, opts ...tool.Option) (string, error) {
		if allowed, refusal := budget.Consume(name); !allowed {
			return refusal, nil
		}
		return endpoint(callCtx, argsJSON, opts...)
	}, nil
}
