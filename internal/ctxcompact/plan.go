// internal/ctxcompact/plan.go
package ctxcompact

import (
	"github.com/cloudwego/eino/schema"
)

// Plan computes which messages stay verbatim (pinned) and which get summarized.
// It is a PURE function (no IO) so both compaction paths share it.
//
// Pin policy (any one fires):
//  0. every Role==System message — the INITIAL CONTEXT (see pinInitialContext)
//  1. tail KeepRecent*2 messages (current immediate context; may include tool msgs)
//  2. every Role==User non-tool-result message (user intent never lost — codex style)
//  3. messages mentioning a working-set path (live file context)
//  4. messages carrying an error marker
//  5. messages carrying a diff/patch marker
//
// Then two corrections, in order. unpinStaleSummaries removes any summary the
// rules above caught, and EnforceToolCallPairs fixes up the set so tool_call
// and tool_result stay paired (bug②). If history already ends in a summary,
// returns an empty summarize set (bug⑦ — no summary-of-summary).
func Plan(msgs []*schema.Message, opts PlanOpts) *PlanResult {
	res := &PlanResult{}
	if len(msgs) == 0 {
		return res
	}
	// bug⑦: short-circuit if already compacted.
	if lastMessageIsSummary(msgs) {
		res.PinnedIndices = allIndices(msgs)
		return res
	}

	pinned := map[int]bool{}

	// 1. tail: pin the last 2*KeepRecent messages regardless of role.
	pairCount := opts.KeepRecent
	if pairCount < 0 {
		pairCount = 0
	}
	tailStart := len(msgs) - pairCount*2
	if tailStart < 0 {
		tailStart = 0
	}
	for i := tailStart; i < len(msgs); i++ {
		pinned[i] = true
	}

	// derive working set from the tail seed + recent window
	seed := make([]int, 0, pairCount*2)
	for i := tailStart; i < len(msgs); i++ {
		seed = append(seed, i)
	}
	res.WorkingSetPaths = deriveWorkingSetPaths(msgs, seed)
	wsSet := map[string]bool{}
	for _, p := range res.WorkingSetPaths {
		wsSet[p] = true
	}

	// 0 and 2-5. heuristic pin
	for i, m := range msgs {
		if pinned[i] {
			continue
		}
		if pinInitialContext(m) { // 0. the agent instruction
			pinned[i] = true
			continue
		}
		if isUserOriginal(m) { // 2. user intent
			pinned[i] = true
			continue
		}
		if shouldPin(m, wsSet) { // 3/4/5. working-set / error / diff
			pinned[i] = true
		}
	}

	unpinStaleSummaries(msgs, pinned)

	// fixpoint: keep tool pairs intact, drop orphans
	EnforceToolCallPairs(msgs, pinned)

	// collect ascending (Assemble depends on ascending PinnedIndices)
	for i := 0; i < len(msgs); i++ {
		if pinned[i] {
			res.PinnedIndices = append(res.PinnedIndices, i)
		} else {
			res.SummarizeIndices = append(res.SummarizeIndices, i)
		}
	}
	return res
}

// pinInitialContext reports whether m is the agent instruction — yanshi's
// INITIAL CONTEXT (W-D-14).
//
// The mid-turn path sees one on EVERY call. einollm.CompactingModel wraps the
// model itself, so the slice it compacts is adk's model input, and
// defaultGenModelInput prepends the instruction as a schema.System message
// ahead of the history.
//
// The pre-turn path usually does not, but "never" would be wrong and the rule
// here is deliberately about the ROLE rather than about which path is calling.
// The WS handler's cs.history is only ever appended with user/assistant/tool
// messages, so that caller supplies none. SSE is different: chat.go takes
// `history := req.Messages` straight off the wire with no role validation, so a
// client can put a System message into the history it asks to have compacted.
// A path-keyed rule would drop that one; a role-keyed rule preserves it, which
// is the right answer for a message the caller asked to be treated as
// instructions either way.
//
// # It used to be dropped, and that was measured
//
// None of rules 1-5 describes a system message: it is not in the tail of a long
// history, isUserOriginal rejects the role, and shouldPin only catches it when
// the prompt happens to contain a word from the error-marker list. So a
// mid-turn compaction assembled a history with NO system message and handed it
// to the provider — operator instruction, tool guidance, skill meta-prompt and
// environment block all gone for the remainder of the turn, with nothing in the
// transcript to say so. Worse, the loss flattered the caller's only sanity
// check: deleting the instruction makes TokensAfter smaller, which is exactly
// what a successful compaction looks like.
//
// # Keeping it, rather than re-injecting a copy
//
// codex re-inserts its initial context before the last user message, because
// there it is a user-role rollout item that compaction genuinely removes. Here
// the instruction is already in the slice and pinning leaves it at index 0,
// which is both the position providers expect and the byte-stable prefix their
// caches key on. Re-inserting would also need a source for the text, and this
// package has none other than the copy it was about to throw away.
func pinInitialContext(m *schema.Message) bool {
	return m != nil && m.Role == schema.System
}

// unpinStaleSummaries removes from pinned every summary left over from an
// EARLIER compaction, so it reaches the summarizer instead.
//
// isUserOriginal already declines to read a summary as user intent, but that is
// one of three routes into the pinned set. The tail rule pins by position, and
// catches a summary as soon as two more turns have pushed it inside the window;
// shouldPin catches one whose text carries an error or diff marker, which a
// summary of a debugging session carries as a matter of course.
//
// It matters because AssembleWithMap REPLACES the summary kind: a stale summary
// that is pinned is one the assembled history drops without the summarizer
// having read it, so its content is lost rather than compressed. Unpinned, it
// lands in the summarize set and the fresh summary is built from a superset of
// what it covered.
//
// The eviction map is deliberately NOT unpinned. It has no guaranteed
// replacement — the mid-turn path records no evictions, so it renders no map —
// and Assemble strips only the kinds it is actually replacing. Summarizing a
// directory of citable addresses into prose would lose the addresses.
//
// # It does nothing unless there is real content to fold the summary into
//
// Run has a FREE exit for an empty summarize set: nothing to fold, no model
// call. Unpinning unconditionally MANUFACTURES work for that branch — when a
// stale summary is the only thing left unpinned, Run pays for a summary call
// whose entire input is a previous summary, gets nothing smaller back, and the
// caller discards it on TokensAfter >= TokensBefore. Measured: calls 0→1,
// tokens 1647→1717, result thrown away.
//
// Mid-turn that is not a one-off, because CompactingModel.maybeCompact arms its
// cooldown only on success: a discarded compaction leaves the gate open and the
// next ReAct iteration repeats it, once per iteration for the rest of the turn.
//
// So the guard is not an optimisation. It is also the same judgement bug⑦'s
// short-circuit makes — re-summarizing a summary with no new content added is
// pointless — reached by a different route.
func unpinStaleSummaries(msgs []*schema.Message, pinned map[int]bool) {
	if !hasUnpinnedContent(msgs, pinned) {
		return
	}
	for i, m := range msgs {
		if pinned[i] && IsSummaryMessage(m) {
			delete(pinned, i)
		}
	}
}

// hasUnpinnedContent reports whether the summarize set already holds a message
// that is CONVERSATION rather than a compaction artefact — the only thing a
// fresh summary can actually be built out of.
func hasUnpinnedContent(msgs []*schema.Message, pinned map[int]bool) bool {
	for i, m := range msgs {
		if pinned[i] || m == nil {
			continue
		}
		if _, isFragment := parseFragment(m); !isFragment {
			return true
		}
	}
	return false
}

// isUserOriginal reports whether m is a genuine user message (not a tool
// result, and not a compaction summary).
func isUserOriginal(m *schema.Message) bool {
	if m == nil || m.Role != schema.User {
		return false
	}
	if IsSummaryMessage(m) {
		return false
	}
	// eino encodes tool results as Role==Tool + ToolCallID; if a provider uses
	// Role==User for tool results, ToolCallID is still set.
	return m.ToolCallID == ""
}

func allIndices(msgs []*schema.Message) []int {
	out := make([]int, len(msgs))
	for i := range msgs {
		out[i] = i
	}
	return out
}
