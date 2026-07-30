package ctxcompact

import "github.com/cloudwego/eino/schema"

// EnforceToolCallPairs mutates pinned so every pinned tool_call has its
// matching tool_result also pinned (and vice versa), and removes orphans
// (a call with no result anywhere, or a result with no call). This is the
// fixpoint that prevents the compacted history from handing the API an
// unpaired tool_result — which OpenAI/Anthropic reject with 400 (bug②).
//
// Algorithm (ported from deepseek-tui's enforce_tool_call_pairs):
//   - build call_id→idx and result_id→idx over ALL msgs
//   - repeat: for each pinned msg, pull in the counterpart of any tool_call/
//     tool_result it carries; remove a msg if its counterpart is permanently
//     gone (orphan). Track permanently_removed so a later iteration can't
//     re-add a cascaded-removed idx (prevents oscillation).
//   - stop when no change in a full pass.
func EnforceToolCallPairs(msgs []*schema.Message, pinned map[int]bool) {
	if len(pinned) == 0 {
		return
	}
	callIdx := map[string]int{}
	resultIdx := map[string]int{}
	for i, m := range msgs {
		if m == nil {
			continue
		}
		for _, tc := range m.ToolCalls {
			if tc.ID != "" {
				callIdx[tc.ID] = i
			}
		}
		if m.ToolCallID != "" {
			resultIdx[m.ToolCallID] = i
		}
	}
	permanentlyRemoved := map[int]bool{}
	for {
		changed := false
		// snapshot keys so mutating pinned mid-loop is safe
		idxs := make([]int, 0, len(pinned))
		for i := range pinned {
			idxs = append(idxs, i)
		}
		for _, idx := range idxs {
			if permanentlyRemoved[idx] {
				continue
			}
			m := msgs[idx]
			if m == nil {
				continue
			}
			for _, tc := range m.ToolCalls {
				if tc.ID == "" {
					continue
				}
				ri, ok := resultIdx[tc.ID]
				if !ok || permanentlyRemoved[ri] {
					if remove(pinned, permanentlyRemoved, idx) {
						changed = true
					}
					break
				}
				if !pinned[ri] {
					pinned[ri] = true
					changed = true
				}
			}
			if m.ToolCallID != "" {
				ci, ok := callIdx[m.ToolCallID]
				if !ok || permanentlyRemoved[ci] {
					if remove(pinned, permanentlyRemoved, idx) {
						changed = true
					}
				} else if !pinned[ci] {
					pinned[ci] = true
					changed = true
				}
			}
		}
		if !changed {
			return
		}
	}
}

// remove deletes idx from pinned and marks it permanently removed. Returns
// whether it was present (and thus changed).
func remove(pinned, permanentlyRemoved map[int]bool, idx int) bool {
	if pinned[idx] {
		delete(pinned, idx)
		permanentlyRemoved[idx] = true
		return true
	}
	return false
}
