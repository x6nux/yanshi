package loopguard

import (
	"fmt"
	"sort"
	"strings"
	"sync"
)

// ToolBudget enforces a per-turn cap on how many times individual tools may be
// called, and a cap on the total across all tools.
//
// # Why this is not a Gate
//
// Every other condition in this package answers "may the loop take another
// iteration?" and its worst verdict ends the turn. A tool budget must not:
// "you may not run a seventh shell_run" is a fact about ONE call, and the
// right response is to let the model try something else, not to kill work that
// may be nearly finished.
//
// So the budget is consulted at the tool-call boundary and, when exceeded,
// answers with the text that becomes the TOOL RESULT. This is the same
// reasoning the orchestrator's UnknownToolsHandler is built on: an error
// returned from a tool node aborts the whole ADK turn (NodeRunError), whereas a
// result is fed back to the model, which can read it and adapt. A budget that
// aborts the turn is indistinguishable from a crash from the user's side.
//
// A ToolBudget is per turn and is discarded with it. Its methods are safe for
// concurrent use because the ADK may run tool calls in parallel within one
// iteration.
type ToolBudget struct {
	perTool  map[string]int
	maxTotal int

	mu     sync.Mutex
	counts map[string]int
	total  int
}

// ToolBudgetConfig configures a ToolBudget.
type ToolBudgetConfig struct {
	// PerTool maps a tool name to the maximum number of calls permitted in one
	// turn. Names are matched exactly (not as globs): a budget is an operator
	// statement about a specific expensive tool, and glob matching would make
	// "how many calls do I have left" depend on pattern precedence rules the
	// model cannot see. Entries with a non-positive limit are ignored.
	PerTool map[string]int
	// MaxTotal caps calls across ALL tools in one turn. Non-positive means no
	// total cap.
	MaxTotal int
}

// NewToolBudget builds a ToolBudget from cfg, or returns nil when cfg imposes
// no limit at all. Returning nil for "unlimited" lets callers hold a possibly
// nil *ToolBudget and call Consume on it unconditionally — every method is
// nil-safe.
func NewToolBudget(cfg ToolBudgetConfig) *ToolBudget {
	perTool := make(map[string]int, len(cfg.PerTool))
	for name, limit := range cfg.PerTool {
		if name != "" && limit > 0 {
			perTool[name] = limit
		}
	}
	if len(perTool) == 0 && cfg.MaxTotal <= 0 {
		return nil
	}
	return &ToolBudget{
		perTool:  perTool,
		maxTotal: cfg.MaxTotal,
		counts:   make(map[string]int, len(perTool)),
	}
}

// Limits returns a copy of the configured per-tool limits, for diagnostics and
// tests.
func (b *ToolBudget) Limits() map[string]int {
	if b == nil {
		return nil
	}
	out := make(map[string]int, len(b.perTool))
	for k, v := range b.perTool {
		out[k] = v
	}
	return out
}

// Consume records one call of the named tool and reports whether it may
// proceed.
//
// When it may not, refusal is the text to return to the model AS THE TOOL
// RESULT (see the type doc). The counter is NOT incremented on refusal: a
// refused call did no work, and counting it would make the reported "N of N
// used" grow without bound across repeated refusals, which is confusing in the
// very message that is trying to explain the limit.
func (b *ToolBudget) Consume(name string) (allowed bool, refusal string) {
	if b == nil {
		return true, ""
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	if limit, ok := b.perTool[name]; ok && b.counts[name] >= limit {
		return false, fmt.Sprintf(
			"error: tool %q has reached its per-turn budget of %d calls and will not run again this turn. Do not retry it. Use a different approach, or report to the user what remains and why.",
			name, limit)
	}
	if b.maxTotal > 0 && b.total >= b.maxTotal {
		return false, fmt.Sprintf(
			"error: this turn has reached its total tool-call budget of %d calls, so %q will not run. Do not retry. Summarise what you found and what still needs doing.",
			b.maxTotal, name)
	}
	b.counts[name]++
	b.total++
	return true, ""
}

// Used reports how many times the named tool has run this turn.
func (b *ToolBudget) Used(name string) int {
	if b == nil {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.counts[name]
}

// Total reports how many tool calls have run this turn.
func (b *ToolBudget) Total() int {
	if b == nil {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.total
}

// Describe renders the configured limits as a stable, sorted string for logs
// and status output.
func (b *ToolBudget) Describe() string {
	if b == nil {
		return "no tool budget"
	}
	names := make([]string, 0, len(b.perTool))
	for name := range b.perTool {
		names = append(names, name)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names)+1)
	if b.maxTotal > 0 {
		parts = append(parts, fmt.Sprintf("total<=%d", b.maxTotal))
	}
	for _, name := range names {
		parts = append(parts, fmt.Sprintf("%s<=%d", name, b.perTool[name]))
	}
	return strings.Join(parts, " ")
}
