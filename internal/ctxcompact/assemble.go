package ctxcompact

import (
	"strings"

	"github.com/cloudwego/eino/schema"
)

// Assemble builds the compacted history: the pinned messages verbatim (in
// original order, per PinnedIndices which Plan keeps ascending) followed by the
// summary as a sentinel-prefixed USER message at the tail. Summary-as-user (not
// System) avoids the double-system conflict with the orchestrator's own system
// prompt (bug④). Putting it at the tail keeps the pinned prefix byte-stable for
// cache hits on subsequent turns.
//
// It is AssembleWithMap with no eviction map.
func Assemble(msgs []*schema.Message, plan *PlanResult, summary string) []*schema.Message {
	return AssembleWithMap(msgs, plan, summary, "")
}

// EvictionMapSentinel prefixes the C3 eviction-map message.
//
// It has its own sentinel rather than sharing SummarySentinel because a
// consumer already depends on telling the two apart: Plan short-circuits when
// history ENDS in a summary, and IsSummaryMessage is what decides that. A map
// wearing the summary's marker would be read as a summary, and any history
// ending in one would stop being compactable.
const EvictionMapSentinel = "[yanshi:evicted-context-map]\n"

// IsEvictionMapMessage reports whether m is a compaction-produced eviction map
// (a user message prefixed with EvictionMapSentinel). nil-safe.
func IsEvictionMapMessage(m *schema.Message) bool {
	return m != nil && m.Role == schema.User && strings.HasPrefix(m.Content, EvictionMapSentinel)
}

// AssembleWithMap is Assemble plus C3's eviction map.
//
// ORDER AT THE TAIL IS map, THEN summary, and it is chosen rather than
// inherited. Both are archive material and both sit after the pinned prefix,
// so the only question is which the model reads last before the live request.
// The summary states what is currently true; the map states what is no longer
// visible. Ending on the map would leave a directory of absences as the
// freshest thing in the context, when what the model should carry into the
// next turn is the state of the task, with the addresses available above it.
//
// An empty mapText adds no message at all. A map that exists but lists nothing
// would be a header announcing an index with no entries — a claim that context
// was evicted, made about a conversation where none was.
func AssembleWithMap(msgs []*schema.Message, plan *PlanResult, summary, mapText string) []*schema.Message {
	out := make([]*schema.Message, 0, len(plan.PinnedIndices)+2)
	for _, i := range plan.PinnedIndices {
		if i >= 0 && i < len(msgs) && msgs[i] != nil {
			out = append(out, msgs[i])
		}
	}
	if mapText != "" {
		out = append(out, &schema.Message{Role: schema.User, Content: EvictionMapSentinel + mapText})
	}
	out = append(out, &schema.Message{Role: schema.User, Content: SummarySentinel + summary})
	return out
}
