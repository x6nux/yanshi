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
)

// ContextWindowTools exposes context_new_window as a GuardedTool.
type ContextWindowTools struct {
	// NewWindow requests a proactive, non-summarized context reset.
	NewWindow *GuardedTool
}

// NewContextWindowTools builds the W-C-14 proactive-new-window tool.
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
	return ct
}

// Tools returns all context-window tools as a slice for convenience.
func (ct *ContextWindowTools) Tools() []*GuardedTool { return []*GuardedTool{ct.NewWindow} }

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
