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
// # Why the historical constants are still literals
//
// SummarySentinel (sentinel.go) keeps its own literal spelling rather than
// being redefined in terms of KindSummary. Its semantics are consumed outside
// this package — internal/cli/tui reads it to keep the summary out of the chat
// transcript — and the point of this change is that nothing about those
// semantics moves. The two spellings are pinned equal by
// TestContextFragment_SummarySentinelUsesTheSameMechanism, which is the same
// enforcement style the rest of the repo uses for facts a compiler cannot hold.

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

// Fragment is one compaction fragment located inside a history: what it is,
// what it says, and where it sits.
//
// Index makes the value LOCATABLE — the property that lets a caller act on a
// fragment (strip it, replace it, count duplicates) rather than merely observe
// that one exists. It is an index into the slice the Fragment was parsed from
// and is meaningless against any other slice.
type Fragment struct {
	Kind  FragmentKind
	Body  string
	Index int
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
func parseFragment(m *schema.Message) (Fragment, bool) {
	if m == nil || m.Role != schema.User {
		return Fragment{}, false
	}
	for _, kind := range fragmentKinds {
		if marker := fragmentMarker(kind); strings.HasPrefix(m.Content, marker) {
			return Fragment{Kind: kind, Body: strings.TrimPrefix(m.Content, marker)}, true
		}
	}
	return Fragment{}, false
}

// hasFragmentKind reports whether m is a fragment of exactly kind. nil-safe.
func hasFragmentKind(m *schema.Message, kind FragmentKind) bool {
	f, ok := parseFragment(m)
	return ok && f.Kind == kind
}

// ParseFragments returns every compaction fragment in msgs, in order, each
// carrying its index in msgs. Messages that are not fragments are skipped;
// nil entries are skipped rather than treated as errors, matching the
// nil-tolerance the rest of this package's message walks already have.
func ParseFragments(msgs []*schema.Message) []Fragment {
	var out []Fragment
	for i, m := range msgs {
		if f, ok := parseFragment(m); ok {
			f.Index = i
			out = append(out, f)
		}
	}
	return out
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
