package ctxcompact

import "github.com/cloudwego/eino/schema"

// Assemble builds the compacted history: the pinned messages verbatim (in
// original order, per PinnedIndices which Plan keeps ascending) followed by the
// summary as a sentinel-prefixed USER message at the tail. Summary-as-user (not
// System) avoids the double-system conflict with the orchestrator's own system
// prompt (bug④). Putting it at the tail keeps the pinned prefix byte-stable for
// cache hits on subsequent turns.
func Assemble(msgs []*schema.Message, plan *PlanResult, summary string) []*schema.Message {
	out := make([]*schema.Message, 0, len(plan.PinnedIndices)+1)
	for _, i := range plan.PinnedIndices {
		if i >= 0 && i < len(msgs) && msgs[i] != nil {
			out = append(out, msgs[i])
		}
	}
	out = append(out, &schema.Message{Role: schema.User, Content: SummarySentinel + summary})
	return out
}
