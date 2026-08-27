// internal/ctxcompact/evictionmap.go
//
// C3: the in-context map of what compaction threw away.
//
// Assemble replaces the summarized messages with one opaque summary. The model
// is then in the position of someone whose notes were rewritten by a stranger:
// it can read what is left, but it does not know what is MISSING, and it has no
// address for anything it might want back. history_read (C2) can fetch any seq
// range, and the C4 summary cites [seq:lo-hi] pointers for the facts it kept —
// but nothing tells the model that seq 40-120 exists at all when the summary
// chose not to mention it. "I do not know what I forgot" is not a state a tool
// can rescue you from, because you never call the tool.
//
// So the map is a directory of the evicted spans, carried in the context on
// every turn. Read it and you know that 40-120 was the stretch where the tests
// were fixed, and that history_read(40, 120) will return it.
//
// THE MAP ITSELF MUST BE BOUNDED, and that constraint is the whole reason for
// the tier structure rather than a flat list. It lives in the prompt, so an
// index that grew by one line per compaction would, after a long session, be
// the thing consuming the window it exists to relieve — a context-pressure
// relief valve that becomes the pressure. The structure is QwenPaw's odometer
// (agents/context/scroll/eviction_index.py), kept because the shape is right:
//
//	Tier 0   the newest evictions, each block listing its milestones in full.
//	Tier k   older history, carried up and squeezed to span endpoints.
//
// Each tier holds at most TierCap blocks. Every eviction drops a block on tier
// 0; when a tier fills it CARRIES — the newest block stays, the rest collapse
// to one line each and stack as a single block one tier up, cascading like a
// digit rolling past 9. Recent history stays detailed, old history rides up
// reduced to its endpoints, and the total line count is bounded by
// TierCap × tiers, which is logarithmic in the number of evictions.
//
// Nothing is lost by a carry. Every line keeps a seq span, and the messages
// themselves are in the durable log; a collapsed line is a zoomed-out view the
// model re-expands with one history_read over its span.
package ctxcompact

import (
	"fmt"
	"strings"
)

// TierCap is the number of blocks a tier holds before it carries up. The carry
// keeps the newest block and folds the other TierCap-1 into one block a tier
// higher.
//
// 10 is QwenPaw's value and is kept: it makes the map's growth base-10, so the
// first ten compactions of a session are all visible at full detail, which is
// most sessions in their entirety.
const TierCap = 10

// EvictionLine is one entry inside a block.
//
// Lo/Hi is the span the line stands for — a single milestone has Lo == Hi from
// its own SeqRef, and a collapsed child block carries the child's whole span.
// Head is the leftmost (oldest) headline in that span and Tail the rightmost,
// so a collapsed line still says what the span started as and ended as.
//
// SELF-SIMILARITY IS THE POINT of keeping both endpoints rather than one label:
// collapsing an already-collapsed line keeps its Head and Tail, so a milestone,
// a span, and a span-of-spans all reduce by the same rule. That is what lets
// the carry cascade to any depth without a special case per level.
type EvictionLine struct {
	Lo, Hi int
	Head   string
	Tail   string
}

// Text renders the line's label: one headline, or "first … last" for a span
// whose endpoints differ.
func (l EvictionLine) Text() string {
	if l.Head == l.Tail {
		return l.Head
	}
	return l.Head + " … " + l.Tail
}

// Span renders the line's sequence range in the form history_read consumes.
func (l EvictionLine) Span() SeqRef { return SeqRef{Lo: l.Lo, Hi: l.Hi} }

// EvictionBlock is one eviction's worth of lines at one tier; its span covers
// all of them.
type EvictionBlock struct {
	Lo, Hi int
	Lines  []EvictionLine
}

// first is the leftmost (oldest) headline anywhere in the block.
func (b EvictionBlock) first() string {
	if len(b.Lines) == 0 {
		return NoMilestone
	}
	return b.Lines[0].Head
}

// last is the rightmost (newest) headline anywhere in the block.
func (b EvictionBlock) last() string {
	if len(b.Lines) == 0 {
		return NoMilestone
	}
	return b.Lines[len(b.Lines)-1].Tail
}

// EvictionMap is a stack of tiers, each a list of blocks oldest-first.
//
// The zero value is a usable empty map. It is plain data with exported fields
// so a transport that persists conversation state can round-trip it through
// JSON without this package owning a serialization format.
type EvictionMap struct {
	// Tiers[0] holds the most recent evictions at full detail; higher indices
	// hold progressively coarser collapsed history.
	Tiers [][]EvictionBlock
}

// IsEmpty reports whether the map has no blocks in any tier.
func (m *EvictionMap) IsEmpty() bool {
	if m == nil {
		return true
	}
	for _, t := range m.Tiers {
		if len(t) > 0 {
			return false
		}
	}
	return true
}

// Span returns the full sequence range the map covers, and whether it covers
// anything. Callers use it to offer a single "read everything archived" range
// when the detail has been folded away.
func (m *EvictionMap) Span() (SeqRef, bool) {
	if m.IsEmpty() {
		return SeqRef{}, false
	}
	first := true
	var out SeqRef
	for _, tier := range m.Tiers {
		for _, b := range tier {
			if first {
				out = SeqRef{Lo: b.Lo, Hi: b.Hi}
				first = false
				continue
			}
			if b.Lo < out.Lo {
				out.Lo = b.Lo
			}
			if b.Hi > out.Hi {
				out.Hi = b.Hi
			}
		}
	}
	return out, true
}

// AddEviction records one compaction: the milestones harvested from its
// summary, plus the FULL span that was evicted.
//
// span is the whole evicted range — tool results and unlabelled turns included
// — not the union of the milestone spans, so that a history_read over the
// block's range recovers everything and not merely the parts somebody wrote a
// headline for. Passing the milestone union instead would produce a map that
// looks complete and silently omits the stretches nobody labelled, which are
// exactly the stretches a reader cannot reconstruct from the summary.
//
// An eviction with no milestones still gets a block, marked NoMilestone. It
// must: "80 messages went here and no one said what they were" is actionable
// (read them), while omitting the block makes them unfindable.
//
// A span that is not citable is IGNORED — the mid-turn compaction path has no
// persisted seq numbers at all, and a map full of [seq:0] entries that
// history_read resolves to nothing is worse than no map.
func (m *EvictionMap) AddEviction(milestones []Milestone, span SeqRef) {
	if !span.citable() {
		return
	}
	lines := make([]EvictionLine, 0, len(milestones))
	for _, ms := range milestones {
		if !ms.Span.citable() || ms.Headline == "" {
			continue
		}
		lines = append(lines, EvictionLine{
			Lo: ms.Span.Lo, Hi: ms.Span.Hi,
			Head: ms.Headline, Tail: ms.Headline,
		})
	}
	if len(lines) == 0 {
		lines = []EvictionLine{{Lo: span.Lo, Hi: span.Hi, Head: NoMilestone, Tail: NoMilestone}}
	}
	if len(m.Tiers) == 0 {
		m.Tiers = append(m.Tiers, nil)
	}
	m.Tiers[0] = append(m.Tiers[0], EvictionBlock{Lo: span.Lo, Hi: span.Hi, Lines: lines})
	m.carry(0)
}

// carry folds tier k up one level if it has reached TierCap, then cascades.
func (m *EvictionMap) carry(k int) {
	if k >= len(m.Tiers) || len(m.Tiers[k]) < TierCap {
		return
	}
	// Keep the newest block at this tier; carry everything older up.
	count := len(m.Tiers[k]) - 1
	older, kept := m.Tiers[k][:count], m.Tiers[k][count:]
	m.Tiers[k] = append([]EvictionBlock(nil), kept...)
	if k+1 == len(m.Tiers) {
		m.Tiers = append(m.Tiers, nil)
	}
	m.Tiers[k+1] = append(m.Tiers[k+1], collapseBlocks(older))
	m.carry(k + 1)
}

// collapseBlocks folds a run of blocks into ONE block, each input becoming a
// single line carrying that input's full span and its endpoint headlines.
func collapseBlocks(blocks []EvictionBlock) EvictionBlock {
	out := EvictionBlock{Lo: blocks[0].Lo, Hi: blocks[0].Hi}
	for _, b := range blocks {
		if b.Lo < out.Lo {
			out.Lo = b.Lo
		}
		if b.Hi > out.Hi {
			out.Hi = b.Hi
		}
		out.Lines = append(out.Lines, EvictionLine{
			Lo: b.Lo, Hi: b.Hi, Head: b.first(), Tail: b.last(),
		})
	}
	return out
}

// DefaultEvictionMapBudget is the rendered-character ceiling applied when a
// caller does not pick one.
//
// ~2000 characters is roughly 500 tokens: the price of always knowing what you
// forgot, paid once per turn. It is a CHARACTER budget rather than a token one
// because the structure being bounded is a directory of short labels, where
// characters and tokens track closely enough that the extra dependency on the
// estimator would buy nothing.
const DefaultEvictionMapBudget = 2000

// evictionMapPreamble is the constant header. It is constant on purpose: it
// sits at the front of a message that is re-sent every turn, so a header that
// varied would move the provider's cache boundary to the top of the map on
// every render.
const evictionMapPreamble = "[evicted context map] The messages below were removed from this " +
	"conversation's live window but are still stored. This is an INDEX, not the conversation — " +
	"read it top (oldest) to bottom (most recently evicted). Each line is a seq span you can " +
	"re-read in full with history_read(from_seq, to_seq); history_search finds a seq by keyword. " +
	"Anything not listed here is still in the live conversation below."

// Render produces the map as prompt text, bounded to budget characters.
//
// budget <= 0 selects DefaultEvictionMapBudget. The bound applies to the
// DIRECTORY only — the preamble is constant and always present, because a
// directory the model cannot tell is a directory is worse than none. Degrading
// happens in three steps, each preserving strictly less detail and strictly
// the same recoverability:
//
//  1. every block, every line, headlines shortened to the view limit;
//  2. one endpoint line per block, so every block span survives;
//  3. a single line naming the whole archived span.
//
// Step 3 is the floor and it is still USEFUL: one history_read recovers
// everything. That is what makes the budget safe to enforce — there is no
// point at which running out of room causes the map to lie about what exists.
//
// THE FLOOR IS ALSO AN IRREDUCIBLE MINIMUM, and a budget below it is not
// honoured. Roughly 130 characters buy the span and the instruction for
// recovering it; there is no shorter form that is still true. Rather than
// shave that to fit, Render exceeds a sub-floor budget — the alternative is to
// emit a map that names a range without saying it can be read, or to emit
// nothing, and "nothing" reads to the model as "nothing was evicted", which is
// the one message this structure exists to prevent. Callers do not pick
// budgets that small; the behaviour is stated here so nobody has to discover
// it from a truncated prompt.
func (m *EvictionMap) Render(budget int) string {
	if m.IsEmpty() {
		return ""
	}
	if budget <= 0 {
		budget = DefaultEvictionMapBudget
	}
	lines := m.directory(budget)
	return evictionMapPreamble + "\n\n" + strings.Join(lines, "\n")
}

// headlineViewChars bounds ONE rendered headline. Milestones are already
// bounded at MaxMilestoneHeadline when stored; this is the tighter view limit
// applied when the directory must fit a budget.
const headlineViewChars = 80

// MinEvictionMapChars is the size of the irreducible floor render: the whole
// archived span plus the instruction for recovering it. A budget below it
// cannot be met and Render exceeds it rather than emit a map that is smaller
// by being untrue. Exported so a caller sizing a budget can see the floor
// instead of measuring it.
const MinEvictionMapChars = 160

// directory renders the tier listing under the character budget, stepping down
// through the three detail levels. The last level is returned whether or not
// it fits — see Render.
func (m *EvictionMap) directory(budget int) []string {
	full := m.expandedLines(0)
	if renderedSize(full) <= budget {
		return full
	}
	shortened := m.expandedLines(headlineViewChars)
	if renderedSize(shortened) <= budget {
		return shortened
	}
	endpoints := m.endpointLines()
	if renderedSize(endpoints) <= budget {
		return endpoints
	}
	return m.globalSpanLines()
}

// renderedSize is the character count of the joined lines, matching what
// Render will emit.
func renderedSize(lines []string) int { return len(strings.Join(lines, "\n")) }

// tierLabel names a tier for the reader. Tier 0 is the most recent eviction;
// higher tiers are older and coarser.
func tierLabel(k int) string {
	if k == 0 {
		return "most recently evicted"
	}
	return "older, folded"
}

// expandedLines renders every block and every line. limit <= 0 keeps headlines
// at their stored length.
func (m *EvictionMap) expandedLines(limit int) []string {
	var out []string
	for k := len(m.Tiers) - 1; k >= 0; k-- {
		if len(m.Tiers[k]) == 0 {
			continue
		}
		out = append(out, fmt.Sprintf("== tier %d (%s) ==", k, tierLabel(k)))
		for _, b := range m.Tiers[k] {
			out = append(out, "  "+SeqRef{Lo: b.Lo, Hi: b.Hi}.String())
			for _, ln := range b.Lines {
				out = append(out, "    - "+ln.Span().String()+" "+boundLineText(ln, limit))
			}
		}
	}
	return out
}

// endpointLines renders one line per block, keeping every block's span.
func (m *EvictionMap) endpointLines() []string {
	var out []string
	for k := len(m.Tiers) - 1; k >= 0; k-- {
		if len(m.Tiers[k]) == 0 {
			continue
		}
		out = append(out, fmt.Sprintf("== tier %d (%s; details folded) ==", k, tierLabel(k)))
		for _, b := range m.Tiers[k] {
			ln := EvictionLine{Lo: b.Lo, Hi: b.Hi, Head: b.first(), Tail: b.last()}
			out = append(out, "  - "+ln.Span().String()+" "+boundLineText(ln, headlineViewChars/2))
		}
	}
	return out
}

// globalSpanLines is the floor: the whole archive as one recoverable span.
func (m *EvictionMap) globalSpanLines() []string {
	span, ok := m.Span()
	if !ok {
		return nil
	}
	return []string{
		"== archived index (details folded to fit) ==",
		"  - " + span.String() + " index details omitted; recover this span with history_read(" +
			fmt.Sprintf("%d, %d)", span.Lo, span.Hi+1),
	}
}

// boundLineText renders a line's label under a per-headline rune limit,
// keeping BOTH endpoints visible for a collapsed span. Shortening only one end
// would make a folded stretch look like it started where it ended.
func boundLineText(ln EvictionLine, limit int) string {
	if limit <= 0 {
		return ln.Text()
	}
	if ln.Head == ln.Tail {
		return truncateRunes(ln.Head, limit)
	}
	half := limit / 2
	if half < 1 {
		half = 1
	}
	return truncateRunes(ln.Head, half) + " … " + truncateRunes(ln.Tail, half)
}
