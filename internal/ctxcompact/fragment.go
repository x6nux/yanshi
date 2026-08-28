// internal/ctxcompact/fragment.go
package ctxcompact

import (
	"strings"

	"github.com/cloudwego/eino/schema"
)

// Compaction fragments — the messages compaction WRITES INTO a history, as
// opposed to the conversation it compacts.
//
// Two of them existed before this file did (the summary and C3's eviction map),
// each with its own hand-spelled sentinel, its own predicate, and its own
// construction site inside Assemble. The shape was already identical —
// "[yanshi:<kind>]\n" prefix, USER role — so what was missing was not a
// convention but a place to state it once, and a way for a third kind to
// inherit it instead of re-deriving it from whichever of the two its author
// happened to read.
//
// # Unifying the MECHANISM is not unifying the MARKER
//
// The kinds must stay distinguishable, and the reason is load-bearing rather
// than tidy: Plan short-circuits when history ENDS in a summary, and
// IsSummaryMessage is the whole of that decision. A map wearing the summary's
// marker would be read as a summary, and any history ending in one would stop
// being compactable — silently, since "nothing to summarize" is also what a
// genuinely compacted history looks like. So the kind is the discriminator and
// the marker is derived from it; there is one form and two names, never one
// name. TestFragment_SummaryAndMapRemainDistinguishable asserts both
// directions of that.
//
// # The historical constants are derived, not respelled
//
// SummarySentinel (sentinel.go) and EvictionMapSentinel (assemble.go) are const
// expressions over their kinds, so a kind and its marker cannot drift. Both
// values are byte-identical to the literals they replaced, which matters
// because internal/cli/tui reads SummarySentinel by name to keep the summary
// out of the chat transcript. Nothing about the two predicates' semantics
// moves; TestContextFragment_SummarySentinelUsesTheSameMechanism asserts the
// marker equality and the predicate behaviour in both directions.
//
// # What is NOT here
//
// There is no exported locator. One shipped in c34ed67 (ParseFragments, plus a
// Fragment.Index field) and was deleted in the following round: review hollowed
// it out and the entire binary still compiled, because StripFragments walks
// parseFragment itself. An exported API whose only callers are its own tests is
// the defect this repo names as its most common, so the rule here is that the
// locator gets exported in the same commit as the caller that needs it.

// FragmentKind names one kind of compaction fragment. Its value IS the text
// inside the marker's brackets, so a kind and its marker cannot drift.
type FragmentKind string

const (
	// KindSummary is the conversation summary compaction produces. See
	// SummarySentinel for why the bracketed form is collision-proof against
	// ordinary user text.
	KindSummary FragmentKind = "conversation-summary"
	// KindEvictionMap is C3's in-context directory of evicted spans. See
	// EvictionMapSentinel for why it does not share the summary's kind.
	KindEvictionMap FragmentKind = "evicted-context-map"
)

// fragmentKinds is the closed set of kinds this package recognises. A
// bracketed marker naming anything else is not a fragment of ours: user text
// that happens to imitate the form must not be strippable by StripFragments,
// which is the one operation here that DELETES history.
var fragmentKinds = []FragmentKind{KindSummary, KindEvictionMap}

const (
	fragmentOpen  = "[yanshi:"
	fragmentClose = "]\n"
)

// fragmentMarker renders the prefix that introduces a fragment of kind.
func fragmentMarker(kind FragmentKind) string {
	return fragmentOpen + string(kind) + fragmentClose
}

// fragment is one compaction fragment: what it is and what it says.
//
// It is UNEXPORTED, and an exported version with an Index field was deleted
// rather than kept. The exported form shipped in c34ed67 claiming "locatable"
// as an API property, and review found it had zero production consumers —
// StripFragments walks parseFragment directly, so hollowing out the exported
// locator left the whole binary compiling and reddened only its own tests. The
// standard defence ("someone may want to locate fragments one day") is exactly
// the argument that produces the written-but-unread defects this repo keeps
// finding. When a caller outside this package needs to locate one, export it
// then, with that caller in the same commit.
type fragment struct {
	Kind FragmentKind
	Body string
}

// MarkFragment builds the message carrying body as a fragment of kind.
//
// It returns a MESSAGE rather than a marked string because the USER role is
// half of the form: every predicate here checks role and prefix together, and
// a fragment built with any other role would be invisible to all of them —
// including Plan's short-circuit, which is how a summary stops being
// re-summarized. A constructor that cannot get the role wrong is cheaper than
// remembering it at each construction site.
func MarkFragment(kind FragmentKind, body string) *schema.Message {
	return &schema.Message{Role: schema.User, Content: fragmentMarker(kind) + body}
}

// parseFragment reads m as a fragment. nil-safe; the second result is false for
// anything that is not one.
//
// The marker must be a PREFIX, not a substring: a user message quoting the
// marker while discussing compaction is conversation, and treating it as a
// fragment would let StripFragments delete it.
func parseFragment(m *schema.Message) (fragment, bool) {
	if m == nil || m.Role != schema.User {
		return fragment{}, false
	}
	for _, kind := range fragmentKinds {
		if marker := fragmentMarker(kind); strings.HasPrefix(m.Content, marker) {
			return fragment{Kind: kind, Body: strings.TrimPrefix(m.Content, marker)}, true
		}
	}
	return fragment{}, false
}

// hasFragmentKind reports whether m is a fragment of exactly kind. nil-safe.
func hasFragmentKind(m *schema.Message, kind FragmentKind) bool {
	f, ok := parseFragment(m)
	return ok && f.Kind == kind
}

// StripFragments returns msgs without the fragments whose kind appears in
// kinds, preserving order and sharing the surviving message pointers.
//
// AN EMPTY kinds LIST STRIPS NOTHING, which falls out of the empty drop set
// rather than needing a guard — a guard for it was written, and removed once a
// mutation probe showed deleting it changed no behaviour. The property is worth
// stating anyway, because the variadic form makes "strip these kinds" and
// "strip everything" look alike at the call site: the caller most likely to
// pass an empty list is one that computed the list and found nothing to
// replace, for which purging every fragment is the opposite of what was asked
// and would delete an eviction map no fresh map is coming to replace.
// TestContextFragment_IsLocatableStrippableDedupable pins it.
func StripFragments(msgs []*schema.Message, kinds ...FragmentKind) []*schema.Message {
	drop := make(map[FragmentKind]bool, len(kinds))
	for _, k := range kinds {
		drop[k] = true
	}
	out := make([]*schema.Message, 0, len(msgs))
	for _, m := range msgs {
		if f, ok := parseFragment(m); ok && drop[f.Kind] {
			continue
		}
		out = append(out, m)
	}
	return out
}
