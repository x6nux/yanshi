// internal/ctxcompact/structured.go
package ctxcompact

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Section names for the structured continuation summary, in the order the
// model is asked to emit them and the order Render writes them back.
//
// WHY FIVE FIXED SECTIONS AND NOT FREE PROSE. The instruction this replaced
// asked for "a concise but comprehensive summary" and got exactly that: a
// paragraph. A paragraph survives ONE compaction. On the second, the model is
// handed its own prose and asked to summarize it again, and the two things it
// most needs to keep apart — work that is still outstanding, and decisions that
// have since been reversed — read identically in prose ("we decided to use X",
// "X did not work"). They merge, and what comes out the far side is a summary
// that lists a withdrawn approach as the current plan. Fixed headings make the
// merge a per-section operation the model can actually perform, and make the
// result parseable, which is what lets the quality gate check that a section
// did not silently vanish.
//
// The five are QwenPaw's, kept verbatim rather than re-invented: Active Task,
// Current State, Constraints, Decisions, Open Work. That set has one slot for
// each thing a resuming agent asks — what am I doing, where did I get to, what
// am I not allowed to do, what have I already settled, what is left.
const (
	// SectionActiveTask holds a one-line statement of the task in flight. It
	// is singular and prose, not a list, because "what are we doing" has
	// exactly one answer and a list invites the model to enumerate history.
	SectionActiveTask = "Active Task"
	// SectionCurrentState holds verified facts and progress.
	SectionCurrentState = "Current State"
	// SectionConstraints holds requirements and preferences still in force.
	SectionConstraints = "Constraints"
	// SectionDecisions holds decisions that are CURRENTLY effective. A
	// superseded decision is replaced here, not appended — see the update
	// instruction.
	SectionDecisions = "Decisions"
	// SectionOpenWork holds outstanding work, blockers, and next actions.
	SectionOpenWork = "Open Work"
)

// SummarySections is the canonical ordered section list. Both the prompt and
// the parser read it, so a section cannot be added to one and forgotten in the
// other.
var SummarySections = []string{
	SectionActiveTask,
	SectionCurrentState,
	SectionConstraints,
	SectionDecisions,
	SectionOpenWork,
}

// itemSections are the four sections whose bodies are bullet lists. Active
// Task is excluded: it is a single statement.
var itemSections = SummarySections[1:]

// emptyMarker is what a section with nothing in it must contain. An explicit
// marker rather than an empty body, because "the model had nothing to say" and
// "the model stopped emitting" produce the same empty section otherwise, and
// only one of those is a valid summary.
const emptyMarker = "(none)"

// SeqRef is a source pointer: the half-open range of persisted message
// sequence numbers a summary item was derived from.
//
// THESE POINTERS ARE NOT DECORATION. The full message log — tool calls and
// tool results included — is persisted with a seq per message, and the
// history_read tool takes from_seq/to_seq. So an item carrying [seq:120-134]
// is an instruction the model can execute: it can go and read the original
// exchange back verbatim instead of relying on the summary's compression of
// it. That turns the summary from a lossy replacement for history into an
// INDEX over history, which is the difference between "the details are gone"
// and "the details are one tool call away".
type SeqRef struct {
	// Lo and Hi are inclusive sequence bounds. A single-message reference has
	// Lo == Hi.
	Lo, Hi int
}

// String renders the reference in the wire form the model reads and writes:
// "[seq:12]" for one message, "[seq:12-40]" for a range.
func (r SeqRef) String() string {
	if r.Hi <= r.Lo {
		return fmt.Sprintf("[seq:%d]", r.Lo)
	}
	return fmt.Sprintf("[seq:%d-%d]", r.Lo, r.Hi)
}

// valid reports whether the range is well formed (non-negative, ascending).
func (r SeqRef) valid() bool { return r.Lo >= 0 && r.Hi >= r.Lo }

// citable reports whether the range is worth quoting to the model.
//
// The ZERO SeqRef means "no sequence numbers are available", not "the range
// starts at zero": the mid-turn compaction path summarizes messages still in
// flight that have no persisted seq yet, while the pre-turn path is reading a
// stored session and does. Quoting 0-0 to the first would invite a pointer
// that resolves to nothing — worse than no pointer, because it costs a wasted
// history_read and teaches the model that the pointers are noise.
func (r SeqRef) citable() bool { return r.valid() && r.Hi > 0 }

// SummaryItem is one statement plus the sequence ranges backing it.
type SummaryItem struct {
	// Text is the statement with its source pointers stripped out, so the
	// prose reads cleanly and the pointers are structured data.
	Text string
	// Sources are the ranges cited. May be empty when the model omitted a
	// pointer; see StructuredSummary.CoveredSeq for the fallback.
	Sources []SeqRef
}

// String renders the item as the model wrote it: text followed by pointers.
func (it SummaryItem) String() string {
	if len(it.Sources) == 0 {
		return it.Text
	}
	parts := make([]string, 0, len(it.Sources))
	for _, s := range it.Sources {
		parts = append(parts, s.String())
	}
	return it.Text + " " + strings.Join(parts, "")
}

// StructuredSummary is the parsed five-section continuation summary.
//
// It is the value that survives across compactions: Render turns it back into
// the exact Markdown the model is shown on the NEXT compaction, so the model
// updates a document in the shape it produced rather than re-deriving one from
// prose. Round-tripping (Parse ∘ Render == identity) is what makes the
// incremental update semantics possible at all, and is pinned by test.
type StructuredSummary struct {
	// CoveredSeq is the sequence range this summary as a whole was built
	// from. It is the fallback attribution for an item whose own pointer the
	// model omitted, and it is what the update prompt widens each round.
	CoveredSeq SeqRef

	// ActiveTask is the single-line task statement.
	ActiveTask string

	// CurrentState, Constraints, Decisions, OpenWork are the four bullet
	// sections, in Render order.
	CurrentState []SummaryItem
	Constraints  []SummaryItem
	Decisions    []SummaryItem
	OpenWork     []SummaryItem
}

// items returns every bullet across the four list sections, in section order.
func (s *StructuredSummary) items() []SummaryItem {
	out := make([]SummaryItem, 0, len(s.CurrentState)+len(s.Constraints)+len(s.Decisions)+len(s.OpenWork))
	out = append(out, s.CurrentState...)
	out = append(out, s.Constraints...)
	out = append(out, s.Decisions...)
	out = append(out, s.OpenWork...)
	return out
}

// section returns the slice for a section name, and whether the name is one of
// the four list sections.
func (s *StructuredSummary) section(name string) ([]SummaryItem, bool) {
	switch name {
	case SectionCurrentState:
		return s.CurrentState, true
	case SectionConstraints:
		return s.Constraints, true
	case SectionDecisions:
		return s.Decisions, true
	case SectionOpenWork:
		return s.OpenWork, true
	}
	return nil, false
}

// setSection assigns a section's items by name. Used by the parser so it can
// walk SummarySections rather than repeat a switch per section.
func (s *StructuredSummary) setSection(name string, items []SummaryItem) {
	switch name {
	case SectionCurrentState:
		s.CurrentState = items
	case SectionConstraints:
		s.Constraints = items
	case SectionDecisions:
		s.Decisions = items
	case SectionOpenWork:
		s.OpenWork = items
	}
}

// Render writes the summary back as the Markdown the model both produces and
// consumes.
//
// The output is DETERMINISTIC — fixed heading order, fixed empty marker — so
// that feeding a rendered summary back through ParseStructured returns an
// equal value. Without that round trip the incremental update semantics have
// no ground to stand on: the model would be asked to update a document that
// does not match what the parser will read back.
func (s *StructuredSummary) Render() string {
	var b strings.Builder
	fmt.Fprintf(&b, "## %s\n%s\n", SectionActiveTask, s.ActiveTask)
	for _, name := range itemSections {
		items, _ := s.section(name)
		fmt.Fprintf(&b, "\n## %s\n", name)
		if len(items) == 0 {
			b.WriteString(emptyMarker + "\n")
			continue
		}
		for _, it := range items {
			b.WriteString("- " + it.String() + "\n")
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

var (
	// headingRe matches a section heading line ("## Current State").
	headingRe = regexp.MustCompile(`^##\s+(.+?)\s*$`)
	// seqRefRe matches a source pointer, accepting both the ASCII hyphen and
	// the en dash a model reaches for when writing a range in prose.
	seqRefRe = regexp.MustCompile(`\[seq:(\d+)(?:[-–](\d+))?\]`)
	// fenceRe matches a whole response wrapped in a Markdown code fence,
	// which models add unprompted often enough that rejecting it would fail
	// good summaries over punctuation.
	fenceRe = regexp.MustCompile("(?s)^```(?:markdown|md)?\\s*\n(.*?)\n```$")
)

// ParseStructured parses a model response into a StructuredSummary.
//
// IT FAILS CLOSED, and that is the whole reason parsing happens locally
// instead of through the provider's structured-output mode. A response that
// does not have the five headings is not coerced, defaulted, or partially
// accepted — it returns an error, Run keeps the original history, and the
// caller pays one wasted model call. The alternative (accept what parsed,
// silently drop the rest) fails in the direction this package cannot afford:
// the sections that went missing are replaced by nothing, and Assemble has
// already thrown the underlying messages away by the time anyone notices.
//
// covered is the sequence range the summarized messages came from. It is the
// attribution for any item the model left unpointed, so that every item has a
// usable pointer even when the model was sloppy — an item with no pointer at
// all is an item whose provenance cannot be checked, and there is no way to
// recover one after the fact.
func ParseStructured(text string, covered SeqRef) (*StructuredSummary, error) {
	raw := strings.TrimSpace(text)
	if m := fenceRe.FindStringSubmatch(raw); m != nil {
		raw = strings.TrimSpace(m[1])
	}
	if raw == "" {
		return nil, fmt.Errorf("structured summary: empty response")
	}

	bodies := map[string][]string{}
	var seen []string
	current := ""
	for _, line := range strings.Split(raw, "\n") {
		trimmed := strings.TrimSpace(line)
		if m := headingRe.FindStringSubmatch(trimmed); m != nil {
			current = strings.TrimSpace(m[1])
			seen = append(seen, current)
			continue
		}
		if trimmed == "" || current == "" {
			continue
		}
		bodies[current] = append(bodies[current], trimmed)
	}

	if err := checkSections(seen); err != nil {
		return nil, err
	}

	s := &StructuredSummary{CoveredSeq: covered}
	s.ActiveTask = strings.TrimSpace(strings.Join(stripBullets(bodies[SectionActiveTask]), " "))
	if s.ActiveTask == "" {
		return nil, fmt.Errorf("structured summary: %q section is empty", SectionActiveTask)
	}
	for _, name := range itemSections {
		items, err := parseItems(name, bodies[name], covered)
		if err != nil {
			return nil, err
		}
		s.setSection(name, items)
	}
	return s, nil
}

// checkSections requires exactly the five headings, in order and without
// extras.
//
// ORDER AND EXACTNESS ARE BOTH CHECKED, because a lenient version of this
// check is how the whole scheme quietly degrades. If extra headings are
// tolerated, a model that invents "## Notes" gets a pass and the content it
// put there is dropped on the next Render. If a missing heading is tolerated,
// a truncated response parses as a summary whose Open Work is simply empty —
// indistinguishable, downstream, from a task with nothing left to do.
func checkSections(seen []string) error {
	if len(seen) != len(SummarySections) {
		return fmt.Errorf("structured summary: got %d headings %v, want exactly %d %v",
			len(seen), seen, len(SummarySections), SummarySections)
	}
	for i, want := range SummarySections {
		if seen[i] != want {
			return fmt.Errorf("structured summary: heading %d is %q, want %q (order is fixed: %v)",
				i+1, seen[i], want, SummarySections)
		}
	}
	return nil
}

// parseItems turns a section body into items, resolving each one's source
// pointers and falling back to covered when the model wrote none.
func parseItems(section string, lines []string, covered SeqRef) ([]SummaryItem, error) {
	if len(lines) == 0 {
		return nil, fmt.Errorf("structured summary: section %q has no body; write %q when it is empty",
			section, emptyMarker)
	}
	if len(lines) == 1 && strings.EqualFold(lines[0], emptyMarker) {
		return nil, nil
	}
	items := make([]SummaryItem, 0, len(lines))
	for _, line := range lines {
		if strings.EqualFold(line, emptyMarker) {
			return nil, fmt.Errorf("structured summary: section %q mixes %q with %d other lines; %q must stand alone",
				section, emptyMarker, len(lines)-1, emptyMarker)
		}
		text, ok := stripBullet(line)
		if !ok {
			return nil, fmt.Errorf("structured summary: section %q has a non-bullet line %q; every entry must start with \"- \"",
				section, firstRunes(line, 40))
		}
		refs, clean := extractSeqRefs(text)
		if clean == "" {
			continue // a bullet that was nothing but pointers carries no claim
		}
		if len(refs) == 0 {
			refs = []SeqRef{covered}
		}
		items = append(items, SummaryItem{Text: clean, Sources: refs})
	}
	if len(items) == 0 {
		return nil, fmt.Errorf("structured summary: section %q has bullets but no statements", section)
	}
	return items, nil
}

// extractSeqRefs pulls every [seq:lo-hi] out of a line and returns them along
// with the line stripped of them.
//
// Malformed and out-of-order pointers are DROPPED rather than raised, and the
// item keeps its text. The alternative — rejecting the whole summary because
// one bullet cited [seq:40-12] — throws away a compaction that is otherwise
// entirely usable over a provenance detail; the item simply inherits the
// covered range instead, which is the same fallback an unpointed item gets.
// A malformed pointer is a worse pointer, not a worse summary.
func extractSeqRefs(line string) ([]SeqRef, string) {
	matches := seqRefRe.FindAllStringSubmatch(line, -1)
	var refs []SeqRef
	for _, m := range matches {
		lo, err := strconv.Atoi(m[1])
		if err != nil {
			continue
		}
		hi := lo
		if m[2] != "" {
			if h, err := strconv.Atoi(m[2]); err == nil {
				hi = h
			}
		}
		ref := SeqRef{Lo: lo, Hi: hi}
		if !ref.valid() {
			continue
		}
		refs = append(refs, ref)
	}
	clean := strings.TrimSpace(seqRefRe.ReplaceAllString(line, ""))
	clean = strings.TrimSpace(strings.TrimRight(clean, ";,"))
	return refs, clean
}

// stripBullet removes a leading "- " or "* " marker, reporting whether one was
// present.
func stripBullet(line string) (string, bool) {
	for _, marker := range []string{"- ", "* "} {
		if strings.HasPrefix(line, marker) {
			return strings.TrimSpace(line[len(marker):]), true
		}
	}
	return line, false
}

// stripBullets removes bullet markers from every line, for the Active Task
// section where a model sometimes writes its single statement as a bullet.
func stripBullets(lines []string) []string {
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		s, _ := stripBullet(l)
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}
