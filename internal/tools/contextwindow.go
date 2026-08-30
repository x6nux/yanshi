// internal/tools/contextwindow.go
//
// W-C-14: the model can open a new context window ON PURPOSE, without going
// through summarization at all. Every OTHER compaction path in this codebase
// is either threshold-triggered (CompactingModel.shouldCompact) or a
// failure fallback (RunSummary erroring, W-C-04) — both are the SYSTEM
// deciding. This tool is the one path where the MODEL decides: it just
// finished a large exploratory sub-task (a long file dump it fully
// extracted the needed facts from, a big diff it already reviewed) and
// knows the surrounding detail is no longer worth its context budget, well
// before any threshold would fire.
//
// It does not itself rewrite history — it can't; a tool handler returns
// before the ReAct loop's NEXT model call even begins. It sets a one-shot
// per-turn signal (einollm.RequestNewWindow) that
// CompactingModel.maybeCompact consumes on that next call, ahead of its own
// threshold gate. See compacting.go for the read side and consumption
// order.
package tools

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/cloudwego/eino/schema"

	einollm "github.com/x6nux/yanshi/internal/llm/eino"
	"github.com/x6nux/yanshi/internal/loopguard"
)

// ContextWindowTools exposes context_new_window and context_budget as
// GuardedTools.
type ContextWindowTools struct {
	// NewWindow requests a proactive, non-summarized context reset.
	NewWindow *GuardedTool
	// Budget reports the turn's remaining token/context budget. W-C-11.
	Budget *GuardedTool
}

// NewContextWindowTools builds the W-C-14 proactive-new-window tool and the
// W-C-11 budget-query tool.
func NewContextWindowTools() *ContextWindowTools {
	ct := &ContextWindowTools{}
	ct.NewWindow = NewGuardedTool(
		"context_new_window", "Context New Window",
		"Skip summarization and start your next reply with a trimmed context "+
			"window: recent turns, your working set, and any pending errors are "+
			"kept, everything else is dropped WITHOUT being summarized first. "+
			"Use this after finishing a large exploratory step (reading many "+
			"files, a big diff, a long tool dump) whose details you have already "+
			"extracted the facts you needed from — cheaper and faster than "+
			"waiting for automatic compaction to summarize text you no longer "+
			"need. Do not use this if you still need to refer back to specifics "+
			"from the dropped portion; a summary would keep a trace of them, "+
			"this does not.",
		10*time.Second,
		params(map[string]*schema.ParameterInfo{
			"reason": {Type: schema.String, Required: true,
				Desc: "one line: what you just finished that no longer needs to stay in context"},
		}),
		SyncStream(ct.run),
	)
	ct.Budget = NewGuardedTool(
		"context_budget", "Context Budget",
		"Report how much of this turn's token budget and how much of the "+
			"model's context window are still free. Use this before deciding "+
			"whether to keep exploring (reading more files, running more "+
			"commands) or to wrap up — e.g. before starting another large "+
			"exploratory read, or when deciding whether context_new_window is "+
			"worth calling. Either figure may be absent: the turn token budget "+
			"is only tracked when the operator configured a per-turn limit, and "+
			"the context-window figure is only tracked once compaction has been "+
			"configured for the model answering this turn and at least one reply "+
			"has been generated.",
		10*time.Second,
		nil,
		SyncStream(ct.budget),
	)
	return ct
}

// Tools returns all context-window tools as a slice for convenience.
func (ct *ContextWindowTools) Tools() []*GuardedTool { return []*GuardedTool{ct.NewWindow, ct.Budget} }

type contextNewWindowArgs struct {
	Reason string `json:"reason"`
}

func (ct *ContextWindowTools) run(ctx context.Context, argsJSON string) (string, error) {
	var a contextNewWindowArgs
	if err := ParseArgs(argsJSON, &a); err != nil {
		return "", err
	}
	reason := strings.TrimSpace(a.Reason)
	if reason == "" {
		return "", fmt.Errorf("context_new_window: reason is required")
	}
	// false means no signal is bound on this ctx at all — not "compaction is
	// disabled" (that case still returns true; see orchestrator.go's
	// unconditional WithNewWindowSignal bind) but a turn entry point that
	// predates or bypasses W-C-14's wiring entirely (e.g. a sub-agent turn,
	// which does not share the managing turn's context). Reported as an
	// error rather than a silent no-op so the model does not believe a
	// request landed that nothing will ever read.
	if !einollm.RequestNewWindow(ctx, reason) {
		return "", fmt.Errorf("context_new_window: not available on this turn")
	}
	return "New window requested. If compaction is configured for the model " +
		"answering this turn, your NEXT reply will see the trimmed history " +
		"instead of the full one, with no summary produced. If compaction is " +
		"disabled entirely for this turn, this request has no effect.", nil
}

// turnTokenBudget is the context_budget tool's view of loopguard's
// per-turn token accumulator.
type turnTokenBudget struct {
	// Used is loopguard.TokenBudgetGate.Used() — the SAME accumulator
	// loopguard's middleware stops the turn with, not a second count.
	Used int `json:"used"`
	// Max is loopguard.TokenBudgetGate.Max() — the configured ceiling.
	Max int `json:"max"`
	// Remaining is Max-Used. Never negative in practice: the gate stops the
	// turn at or before Used reaches Max (see TokenBudgetGate.Check), so a
	// tool call answering this query only ever runs while Remaining>=0.
	Remaining int `json:"remaining"`
}

// contextWindowBudget is the context_budget tool's view of the most recent
// ctxcompact-derived snapshot CompactingModel.maybeCompact published.
type contextWindowBudget struct {
	// Window is the model's context window in tokens.
	Window int `json:"window"`
	// Used is ctxcompact.EstimateTokens of the history as of the last model
	// call this turn.
	Used int `json:"used"`
	// Remaining is ctxcompact.RemainingBudget for that same history and
	// window — the SAME arithmetic the overflow gate refuses a send with, not
	// a re-estimated number. Can be negative: it mirrors the deficit
	// ctxcompact.CheckContextLimit's error would report.
	Remaining int `json:"remaining"`
}

// contextBudgetResult is the context_budget tool's JSON payload. Either field
// may be nil — see NewContextWindowTools' description of the tool for why.
type contextBudgetResult struct {
	TurnTokens *turnTokenBudget     `json:"turn_tokens,omitempty"`
	Context    *contextWindowBudget `json:"context_window,omitempty"`
	// Note explains a missing field, or notes that neither is tracked. Never
	// set when both fields are present.
	Note string `json:"note,omitempty"`
}

// budget implements context_budget. It has no arguments: the two figures it
// reports are read straight off state other production code already
// maintains (loopguard's TokenBudgetGate and einollm's context-budget
// signal) — there is nothing for a caller to parameterize.
//
// W-C-11: both reads go through the SAME accessors the rest of the codebase
// uses to consume that state (loopguard.TokenBudgetGateFromContext,
// einollm.ContextBudgetFromContext), so this handler cannot compute a number
// that disagrees with the mechanism actually enforcing the turn-token limit
// or actually deciding when to compact — see
// TestContextBudget_AgreesWithLoopGuardAndCompactingModel for the pin.
func (ct *ContextWindowTools) budget(ctx context.Context, _ string) (string, error) {
	var res contextBudgetResult
	var missing []string

	if gate, ok := loopguard.TokenBudgetGateFromContext(ctx); ok {
		res.TurnTokens = &turnTokenBudget{
			Used:      gate.Used(),
			Max:       gate.Max(),
			Remaining: gate.Max() - gate.Used(),
		}
	} else {
		missing = append(missing, "turn token budget (no MaxTurnTokens limit configured for this session)")
	}

	if snap, ok := einollm.ContextBudgetFromContext(ctx); ok {
		res.Context = &contextWindowBudget{
			Window:    snap.Window,
			Used:      snap.Used,
			Remaining: snap.Remaining,
		}
	} else {
		missing = append(missing, "context window budget (compaction is not configured for the model answering this turn, or no reply has been generated yet)")
	}

	if len(missing) > 0 {
		res.Note = "not tracked: " + strings.Join(missing, "; ")
	}
	return toJSON(res), nil
}
