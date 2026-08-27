// internal/ctxcompact/milestone.go
//
// C7: the retrieval labels the eviction map is built out of.
//
// The map produced in evictionmap.go is only as useful as the headlines it
// lists. A map that says "[seq:40-120] (no milestone)" tells the model that
// eighty messages went somewhere; a map that says "[seq:40-120] fixed the
// compile errors in internal/tools, tests green" tells it whether going back
// there is worth a tool call. The difference is entirely in whether somebody
// wrote a label at the time.
//
// TWO WAYS TO GET LABELS, AND THIS FILE IMPLEMENTS THE CHEAP ONE. The obvious
// route is a tool the model calls to annotate its own progress, which costs a
// tool round trip per milestone and depends on the model choosing to use it —
// the same "the model almost never calls it" failure that makes memory
// unhelpful. The other route is free: the structured summary of C4 ALREADY
// makes the model write one line per fact with a [seq:lo-hi] pointer attached,
// which is exactly a (span, headline) pair. Harvesting those costs nothing at
// call time and cannot be skipped by a model that does not feel like it,
// because the summary is not optional.
//
// So a Milestone is a SummaryItem read as an index entry rather than as prose.
package ctxcompact

import (
	"sort"
	"strings"
)

// Milestone is one labelled span of evicted history: the sequence range it
// covers and the one-line headline that describes it.
//
// It is deliberately the same shape as a summary bullet, because that is where
// they come from. Nothing else in the package produces one, and nothing needs
// to: a milestone that no summary supports would be a claim about history with
// no evidence behind it.
type Milestone struct {
	// Span is the persisted sequence range the headline describes. It is
	// always citable (see SeqRef.citable) for a milestone that
	// MilestonesFromSummary returned — an uncitable span is dropped rather
	// than carried, since the whole value of the entry is that history_read
	// can resolve it.
	Span SeqRef
	// Headline is the one-line label, already flattened to a single line and
	// bounded to MaxMilestoneHeadline runes.
	Headline string
}

// MaxMilestoneHeadline bounds one headline's length in runes.
//
// The bound is on the DURABLE value, not just the rendered view, because a
// milestone is carried across compactions inside the eviction map: an
// unbounded headline would be an unbounded thing living in a structure whose
// entire purpose is to have a size ceiling. A model that writes a paragraph
// into one bullet gets the first sentence or so of it and a marker; the full
// text is still one history_read away, which is the same bargain the map
// itself offers.
const MaxMilestoneHeadline = 160

// NoMilestone is the headline used for an evicted span that carried no
// labelled item at all.
//
// It is an explicit marker rather than an omitted entry: "eighty messages were
// dropped here and nobody said what they were" is information the model can
// act on (it can read the span), whereas leaving the span out of the map makes
// those messages invisible and therefore unrecoverable in practice.
const NoMilestone = "(no milestone)"

// Milestones harvests the summary's bullets as index entries.
//
// Every item of the four list sections contributes one milestone per source
// pointer it carries. Items are emitted in ASCENDING span order rather than
// section order, because the consumer is an index over a timeline and a reader
// scanning it is asking "what happened between here and here", not "what did
// the Constraints section say".
//
// Items whose span is not citable are dropped. That is the same rule
// ParseStructured applies when it decides whether to quote a range to the
// model: a pointer that resolves to nothing costs a wasted history_read and
// teaches the model the pointers are noise.
func (s *StructuredSummary) Milestones() []Milestone {
	if s == nil {
		return nil
	}
	var out []Milestone
	for _, it := range s.items() {
		head := boundHeadline(it.Text)
		if head == "" || strings.EqualFold(head, emptyMarker) {
			// A BULLETED empty marker ("- (none)") is not the same shape
			// ParseStructured recognises — it only treats a BARE "(none)" as an
			// empty section — so it arrives here as an ordinary item whose text
			// is "(none)". Indexing it would put a content-free label on the
			// whole covered range, which is strictly worse than the
			// NoMilestone entry AddEviction already writes for an unlabelled
			// span: same absence of information, but presented as a milestone
			// somebody wrote. Dropping it here rather than loosening the
			// parser keeps the fail-closed contract in ParseStructured intact.
			continue
		}
		for _, ref := range it.Sources {
			if !ref.citable() {
				continue
			}
			out = append(out, Milestone{Span: ref, Headline: head})
		}
	}
	sortMilestones(out)
	return out
}

// MilestonesFromSummary parses a raw summary response and harvests its
// milestones, returning nil when the text is not a structured summary.
//
// IT SWALLOWS THE PARSE ERROR ON PURPOSE, and that is the one place in this
// package where failing open is right. Everywhere else a summary that does not
// parse means history is about to be replaced by something unreadable, so the
// answer is to refuse. Here the summary has already been accepted by whatever
// gate the caller configured; the only question left is whether an INDEX can
// also be built from it. A caller wired to a summarizer that does not produce
// the structured form should get a map with coarse "(no milestone)" spans —
// degraded, still navigable — rather than no compaction at all.
func MilestonesFromSummary(text string, covered SeqRef) []Milestone {
	s, err := ParseStructured(text, covered)
	if err != nil {
		return nil
	}
	return s.Milestones()
}

// boundHeadline flattens a headline to one line and caps it at
// MaxMilestoneHeadline runes.
//
// Flattening comes first: the map is a line-oriented directory, so a headline
// containing a newline would silently produce two entries, the second of them
// unattached to any span.
func boundHeadline(text string) string {
	flat := strings.Join(strings.Fields(strings.ReplaceAll(text, "\n", " ")), " ")
	return truncateRunes(flat, MaxMilestoneHeadline)
}

// truncateRunes caps s at limit runes, marking the cut with an ellipsis so a
// reader can tell a shortened label from a terse one.
func truncateRunes(s string, limit int) string {
	if limit <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= limit {
		return s
	}
	if limit <= 1 {
		return string(r[:limit])
	}
	return strings.TrimRight(string(r[:limit-1]), " ") + "…"
}

// sortMilestones orders by span start, then span end, then headline, so the
// output is deterministic for a given input set. Determinism matters more than
// it looks: the map is rendered into the prompt on every turn, and an ordering
// that varied between renders would break the prefix the provider caches.
func sortMilestones(ms []Milestone) {
	sort.SliceStable(ms, func(i, j int) bool {
		a, b := ms[i], ms[j]
		if a.Span.Lo != b.Span.Lo {
			return a.Span.Lo < b.Span.Lo
		}
		if a.Span.Hi != b.Span.Hi {
			return a.Span.Hi < b.Span.Hi
		}
		return a.Headline < b.Headline
	})
}
