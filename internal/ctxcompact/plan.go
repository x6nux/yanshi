// internal/ctxcompact/plan.go
package ctxcompact

import (
	"github.com/cloudwego/eino/schema"
)

// Plan computes which messages stay verbatim (pinned) and which get summarized.
// It is a PURE function (no IO) so both compaction paths share it.
//
// Pin policy (any one fires):
//  1. tail KeepRecent*2 messages (current immediate context; may include tool msgs)
//  2. every Role==User non-tool-result message (user intent never lost — codex style)
//  3. messages mentioning a working-set path (live file context)
//  4. messages carrying an error marker
//  5. messages carrying a diff/patch marker
//
// After heuristic pinning, EnforceToolCallPairs fixes up the set so tool_call
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

	// 2-5. heuristic pin
	for i, m := range msgs {
		if pinned[i] {
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
