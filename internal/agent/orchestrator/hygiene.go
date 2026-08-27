// Per-iteration context hygiene: T4 tool-result degradation and T3 background
// completion notices.
//
// Both ride the SAME ADK middleware because both are things that must happen
// at a ReAct iteration boundary, to the same message slice, and doing them in
// two middlewares would mean two passes over the history and an ordering
// question nobody would remember to answer.
//
// The ordering inside is fixed and load-bearing: notices are APPENDED first,
// then degradation runs. A completion notice carries the result of a run that
// may itself be large, and appending after degradation would let it skip the
// pass entirely. It is not degraded on this iteration either way (it is not a
// tool result), but the next iteration's pass sees it in place.
package orchestrator

import (
	"context"
	"slices"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/schema"

	"github.com/x6nux/yanshi/internal/tools"
)

// resultHygiene is the adk.ChatModelAgentMiddleware that runs T3 reinjection
// and T4 degradation at every iteration boundary.
//
// Stateless, like loopGuardMiddleware and for the same reason: runnerFor
// memoises runners, so one instance serves every turn on that model. All the
// state it needs (the background manager, the work root) is in the turn's
// context.
type resultHygiene struct {
	*adk.BaseChatModelAgentMiddleware
}

// newResultHygiene builds a resultHygiene ready for installation on an
// adk.ChatModelAgentConfig.Handlers slice.
func newResultHygiene() *resultHygiene {
	return &resultHygiene{BaseChatModelAgentMiddleware: &adk.BaseChatModelAgentMiddleware{}}
}

// BeforeModelRewriteState reinjects finished background runs and degrades tool
// results that are no longer recent.
//
// This hook is chosen for the same reason imageAttacher uses it: it is the only
// point in the loop whose returned state the ADK PERSISTS. A model wrapper
// could rewrite the messages for exactly one call, and the ADK would rebuild
// from its own state on the next iteration — the degradation would be undone
// every time and the notice would be delivered forever.
func (h *resultHygiene) BeforeModelRewriteState(
	ctx context.Context,
	state *adk.ChatModelAgentState,
	_ *adk.ModelContext,
) (context.Context, *adk.ChatModelAgentState, error) {
	if state == nil {
		return ctx, state, nil
	}
	msgs := state.Messages
	if notices := backgroundNotices(ctx); len(notices) > 0 {
		msgs = append(slices.Clone(msgs), notices...)
	}
	msgs, changed := degradeHistory(ctx, msgs)
	if changed || len(msgs) != len(state.Messages) {
		state.Messages = msgs
	}
	return ctx, state, nil
}

// backgroundNotices drains the turn's background manager and renders each
// finished run as a USER message.
//
// USER and not TOOL, and this is the constraint the whole T3 design bends
// around: a role=tool message needs a matching assistant tool_call to pair
// with, and this run's call was answered several iterations ago by the offload
// acknowledgement. ctxcompact.EnforceToolCallPairs runs a fixpoint over
// exactly that pairing, so an unpaired tool message is not merely untidy — it
// is a message the compactor cannot classify and providers reject. QwenPaw
// makes the same choice for the same reason (tool_calls/_hint.py flattens the
// finished response into an ordinary message with no ToolResultBlock).
//
// The <system-notification> envelope in tools.CompletionNotice is what keeps
// the user role from being a lie to the model.
func backgroundNotices(ctx context.Context) []*schema.Message {
	mgr, ok := tools.BackgroundManagerFromContext(ctx)
	if !ok {
		return nil
	}
	runs := mgr.DrainNotices()
	if len(runs) == 0 {
		return nil
	}
	out := make([]*schema.Message, 0, len(runs))
	for _, run := range runs {
		out = append(out, schema.UserMessage(tools.CompletionNotice(run)))
	}
	return out
}

// degradeHistory shrinks every tool result except the most recent
// tools.DegradeKeepRecent, writing each full text to disk first.
//
// # Why here and not in the tool
//
// The tool cannot know it is no longer recent. At the moment shell_run returns,
// its result is THE result — the model is about to act on it. Only the loop
// knows that two more calls have happened since, and only the loop can see the
// slice in which "recent" is defined.
//
// # Why this is not ctxcompact's fold
//
// See tools/spilldegrade.go. Briefly: fold is pressure-driven and window-time,
// this is unconditional and production-time, and running this first is what
// guarantees fold finds a strong recovery pointer (a spillover path) instead
// of falling back to a tool-call id.
//
// The input slice is never modified: degraded messages are copies and untouched
// ones keep their pointer, so a history with nothing to degrade allocates only
// the outer slice — and only when something else already forced a clone.
func degradeHistory(ctx context.Context, msgs []*schema.Message) ([]*schema.Message, bool) {
	if len(msgs) == 0 {
		return msgs, false
	}
	out := msgs
	cloned := false
	seen := 0
	for i := len(msgs) - 1; i >= 0; i-- {
		m := msgs[i]
		if m == nil || m.Role != schema.Tool || m.ToolCallID == "" {
			continue
		}
		if seen < tools.DegradeKeepRecent {
			seen++
			continue // the model is working with these right now
		}
		seen++
		body, did := tools.DegradeToolResult(ctx, degradeToolName(m), m.Content)
		if !did {
			continue
		}
		if !cloned {
			out = slices.Clone(msgs)
			cloned = true
		}
		cp := *m
		cp.Content = body
		out[i] = &cp
	}
	return out, cloned
}

// degradeToolName is the label for the spillover file a degrade writes. It
// falls back to a constant when the message carries no tool name, which some
// providers omit: the name only names the temp file, so an unknown one costs
// nothing but a less readable filename.
func degradeToolName(m *schema.Message) string {
	if m.ToolName != "" {
		return m.ToolName
	}
	return "tool_result"
}
