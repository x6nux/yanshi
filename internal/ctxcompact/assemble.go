package ctxcompact

import (
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
//
// It is DERIVED from KindEvictionMap rather than spelled out, so the constant
// and the kind cannot drift apart; the value is byte-identical to the literal
// it replaced. See fragment.go for why the two kinds share one form and never
// one name.
const EvictionMapSentinel = fragmentOpen + string(KindEvictionMap) + fragmentClose

// IsEvictionMapMessage reports whether m is a compaction-produced eviction map
// (a user message prefixed with EvictionMapSentinel). nil-safe.
func IsEvictionMapMessage(m *schema.Message) bool {
	return hasFragmentKind(m, KindEvictionMap)
}

// pinnedMessages collects msgs[plan.PinnedIndices] verbatim, in original
// order, skipping any out-of-range or nil index defensively. It is the first
// half of AssembleWithMap's job — the part shared with W-C-04's pins-only
// fallback (run.go's pinsOnlyResult), which needs exactly this prefix and
// nothing appended after it (no summary, no eviction map: there IS no
// summary on that path, that's the point of the fallback). Extracted rather
// than duplicated per CLAUDE.md's "重复逻辑必须抽成公共函数".
func pinnedMessages(msgs []*schema.Message, plan *PlanResult) []*schema.Message {
	out := make([]*schema.Message, 0, len(plan.PinnedIndices))
	for _, i := range plan.PinnedIndices {
		if i >= 0 && i < len(msgs) && msgs[i] != nil {
			out = append(out, msgs[i])
		}
	}
	return out
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
//
// # A kind appears at most once, and the freshest one wins
//
// The pinned prefix can already contain fragments from an EARLIER compaction,
// and both ways it happens were measured on the shipped code rather than
// supposed. A stale eviction map is pinned because isUserOriginal reads it as
// user intent (user role, no ToolCallID); a stale summary is pinned once two
// more turns push it inside the tail window. Appending the fresh ones on top
// left the model two directories of evicted spans, or two messages each
// claiming to be the summary of the conversation.
//
// So the kinds being appended are stripped from the prefix first — AND ONLY
// THOSE. A blanket purge would be worse than the duplication it fixes: the
// mid-turn path runs with no EvictionMap (its messages have no persisted seq
// numbers to cite), produces an empty mapText, and would then delete the
// structured directory a pre-turn compaction built with nothing to put in its
// place. Replacing a fragment is safe because the replacement supersedes it —
// the map is a cumulative re-render, and the summary is built from a set that
// includes whatever the stale one covered (Plan unpins stale summaries for
// exactly that reason). Deleting one with no replacement is just loss.
func AssembleWithMap(msgs []*schema.Message, plan *PlanResult, summary, mapText string) []*schema.Message {
	out := pinnedMessages(msgs, plan)

	fresh := make([]*schema.Message, 0, 2)
	replaced := make([]FragmentKind, 0, 2)
	if mapText != "" {
		fresh = append(fresh, MarkFragment(KindEvictionMap, mapText))
		replaced = append(replaced, KindEvictionMap)
	}
	fresh = append(fresh, MarkFragment(KindSummary, summary))
	replaced = append(replaced, KindSummary)

	return append(StripFragments(out, replaced...), fresh...)
}
