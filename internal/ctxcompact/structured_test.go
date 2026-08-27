// internal/ctxcompact/structured_test.go
package ctxcompact

import (
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"
)

// wellFormed is a valid five-section response used as the base for the
// negative cases, which each break exactly one thing about it. Keeping the
// mutations one-edit-away from a KNOWN-GOOD document is what makes each
// failure attributable: a hand-written broken fixture can fail for two reasons
// at once and still look like it proved the one the test names.
const wellFormed = `## Active Task
Wire the structured continuation summary into RunSummary.

## Current State
- ParseStructured round-trips through Render [seq:10-24]
- The estimator no longer undercounts CJK [seq:30]

## Constraints
- No new module dependencies [seq:4]

## Decisions
- Parse locally rather than using provider structured output [seq:12-18]

## Open Work
- Fold tool results under window pressure [seq:40-52]`

func TestParseStructured_WellFormed(t *testing.T) {
	got, err := ParseStructured(wellFormed, SeqRef{Lo: 1, Hi: 99})
	if err != nil {
		t.Fatalf("ParseStructured on a well-formed document: %v", err)
	}
	if got.ActiveTask != "Wire the structured continuation summary into RunSummary." {
		t.Errorf("ActiveTask = %q", got.ActiveTask)
	}
	for _, tc := range []struct {
		name  string
		items []SummaryItem
		want  int
	}{
		{SectionCurrentState, got.CurrentState, 2},
		{SectionConstraints, got.Constraints, 1},
		{SectionDecisions, got.Decisions, 1},
		{SectionOpenWork, got.OpenWork, 1},
	} {
		if len(tc.items) != tc.want {
			t.Errorf("%s: got %d items, want %d", tc.name, len(tc.items), tc.want)
		}
	}
	// Pointers are structured data, and the text is clean of them — that
	// separation is what lets a consumer act on a pointer instead of
	// regex-scraping the prose it was embedded in.
	first := got.CurrentState[0]
	if want := (SeqRef{Lo: 10, Hi: 24}); len(first.Sources) != 1 || first.Sources[0] != want {
		t.Errorf("sources = %v, want [%v]", first.Sources, want)
	}
	if strings.Contains(first.Text, "seq:") {
		t.Errorf("pointer left in item text: %q", first.Text)
	}
}

// TestParseStructured_RejectsMalformed is the fail-closed contract.
//
// Each case is a document a real model produces when it drifts, and for each
// the ONLY acceptable outcome is an error. Accepting any of them partially is
// what makes compaction dangerous rather than merely disappointing: Assemble
// deletes the summarized messages and puts the summary in their place, so a
// section that silently parsed as empty is a section of the conversation that
// no longer exists anywhere.
func TestParseStructured_RejectsMalformed(t *testing.T) {
	tests := []struct {
		name string
		text string
		why  string
	}{
		{
			"empty", "",
			"a blank response is a failed call wearing a success's clothes",
		},
		{
			"prose_with_no_headings",
			"The user asked me to wire up the summary and I did that, then ran the tests.",
			"this is exactly the pre-C4 output; accepting it silently reverts C4",
		},
		{
			"missing_a_section",
			strings.Replace(wellFormed, "## Open Work\n- Fold tool results under window pressure [seq:40-52]", "", 1),
			"a truncated response would otherwise parse as a task with nothing left to do",
		},
		{
			"sections_out_of_order",
			"## Active Task\nx\n\n## Constraints\n(none)\n\n## Current State\n(none)\n\n## Decisions\n(none)\n\n## Open Work\n(none)",
			"Render emits a fixed order; tolerating another would break the round trip",
		},
		{
			"extra_section",
			wellFormed + "\n\n## Notes\n- something the model wanted to add [seq:5]",
			"content under an unknown heading is dropped on the next Render, silently",
		},
		{
			"blank_active_task",
			strings.Replace(wellFormed, "Wire the structured continuation summary into RunSummary.", "", 1),
			"a summary that cannot say what the task is has not summarized anything",
		},
		{
			"empty_section_body",
			strings.Replace(wellFormed, "- No new module dependencies [seq:4]", "", 1),
			"blank and (none) must be distinguishable: one is a claim, the other is a stall",
		},
		{
			"none_mixed_with_items",
			strings.Replace(wellFormed, "- No new module dependencies [seq:4]",
				"(none)\n- No new module dependencies [seq:4]", 1),
			"the section both is and is not empty; neither reading is safe",
		},
		{
			"non_bullet_line_in_list_section",
			strings.Replace(wellFormed, "- No new module dependencies [seq:4]",
				"No new module dependencies [seq:4]", 1),
			"an unmarked line may be a bullet or a stray paragraph; guessing loses content either way",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseStructured(tt.text, SeqRef{Lo: 1, Hi: 99})
			if err == nil {
				t.Fatalf("accepted a malformed summary (%s).\nparsed as: %+v", tt.why, got)
			}
			if got != nil {
				t.Errorf("returned a non-nil summary alongside the error: %+v", got)
			}
		})
	}
}

// TestParseStructured_RoundTrip is the property the incremental update
// semantics stand on.
//
// On the second compaction the model is shown the PREVIOUS summary and asked
// to update it. What it is shown is Render's output; what the result is read
// back with is ParseStructured. If those two disagree the model is updating a
// document that differs from the one the parser will reconstruct, and the
// disagreement compounds on every subsequent compaction.
func TestParseStructured_RoundTrip(t *testing.T) {
	first, err := ParseStructured(wellFormed, SeqRef{Lo: 1, Hi: 99})
	if err != nil {
		t.Fatalf("first parse: %v", err)
	}
	second, err := ParseStructured(first.Render(), SeqRef{Lo: 1, Hi: 99})
	if err != nil {
		t.Fatalf("re-parsing Render output failed — the two are out of sync: %v\n%s", err, first.Render())
	}
	if first.Render() != second.Render() {
		t.Errorf("round trip is not stable:\n--- first ---\n%s\n--- second ---\n%s", first.Render(), second.Render())
	}
	if len(second.items()) != len(first.items()) {
		t.Errorf("item count changed across the round trip: %d → %d", len(first.items()), len(second.items()))
	}
	// THE POINTERS MUST SURVIVE, and asserting it takes its own check because
	// the round trip alone does not notice when they do not. Render dropping
	// every [seq:…] still produces a document that parses and re-renders
	// identically — the items just inherit the covered range on the way back
	// in, so the shapes match and the two Render outputs are equal. A mutation
	// probe that stripped pointers from Render passed both assertions above.
	//
	// What is actually lost is the property C4 exists for: the summary stops
	// being an INDEX into history (each claim citing the messages it came from,
	// recoverable with history_read) and collapses to one range covering
	// everything, which points at the whole compacted span and locates nothing.
	for i, want := range first.items() {
		got := second.items()[i]
		if len(got.Sources) != len(want.Sources) {
			t.Fatalf("item %d (%q) lost its pointers across the round trip: %v → %v. "+
				"Every claim now resolves to the same range, so history_read cannot locate any of them",
				i, want.Text, want.Sources, got.Sources)
		}
		for j := range want.Sources {
			if got.Sources[j] != want.Sources[j] {
				t.Errorf("item %d source %d changed: %v → %v", i, j, want.Sources[j], got.Sources[j])
			}
		}
	}
}

// TestParseStructured_EmptySectionsRoundTrip covers the (none) path
// specifically, because an all-empty summary is the shape most likely to lose
// its markers and become a document with blank sections — which the parser
// then rejects, turning a valid "nothing outstanding" into a failed
// compaction.
func TestParseStructured_EmptySectionsRoundTrip(t *testing.T) {
	doc := "## Active Task\nNothing in flight.\n\n## Current State\n(none)\n\n## Constraints\n(none)\n\n## Decisions\n(none)\n\n## Open Work\n(none)"
	s, err := ParseStructured(doc, SeqRef{Lo: 1, Hi: 9})
	if err != nil {
		t.Fatalf("all-empty sections must parse: %v", err)
	}
	if len(s.items()) != 0 {
		t.Errorf("expected no items, got %d", len(s.items()))
	}
	if !strings.Contains(s.Render(), emptyMarker) {
		t.Errorf("Render dropped the %q markers:\n%s", emptyMarker, s.Render())
	}
	if _, err := ParseStructured(s.Render(), SeqRef{Lo: 1, Hi: 9}); err != nil {
		t.Errorf("re-parse of an all-empty summary failed: %v", err)
	}
}

// TestSeqRefParsing covers the pointer forms a model actually emits, including
// the ones it gets wrong.
func TestSeqRefParsing(t *testing.T) {
	covered := SeqRef{Lo: 100, Hi: 200}
	tests := []struct {
		name string
		line string
		want []SeqRef
		text string
	}{
		{"single", "did a thing [seq:42]", []SeqRef{{Lo: 42, Hi: 42}}, "did a thing"},
		{"range", "did a thing [seq:10-24]", []SeqRef{{Lo: 10, Hi: 24}}, "did a thing"},
		{"en_dash", "did a thing [seq:10–24]", []SeqRef{{Lo: 10, Hi: 24}}, "did a thing"},
		{"multiple", "did a thing [seq:1][seq:5-9]", []SeqRef{{Lo: 1, Hi: 1}, {Lo: 5, Hi: 9}}, "did a thing"},
		{"mid_sentence", "changed [seq:7] then reverted", []SeqRef{{Lo: 7, Hi: 7}}, "changed  then reverted"},
		// A backwards range is a worse POINTER, not a worse summary: the item
		// keeps its text and inherits the covered range, exactly as an item
		// with no pointer would. Rejecting the whole compaction over it would
		// throw away everything else the model got right.
		{"backwards_range_dropped", "did a thing [seq:40-12]", nil, "did a thing"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			refs, clean := extractSeqRefs(tt.line)
			if len(refs) != len(tt.want) {
				t.Fatalf("got %d refs %v, want %d %v", len(refs), refs, len(tt.want), tt.want)
			}
			for i := range refs {
				if refs[i] != tt.want[i] {
					t.Errorf("ref[%d] = %v, want %v", i, refs[i], tt.want[i])
				}
			}
			if strings.TrimSpace(clean) != strings.TrimSpace(tt.text) {
				t.Errorf("clean text = %q, want %q", clean, tt.text)
			}
		})
	}
	// And the fallback: an item the model left unpointed must still end up
	// with a usable range, because provenance cannot be recovered later.
	s, err := ParseStructured("## Active Task\nx\n\n## Current State\n- unpointed claim\n\n## Constraints\n(none)\n\n## Decisions\n(none)\n\n## Open Work\n(none)", covered)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(s.CurrentState) != 1 || len(s.CurrentState[0].Sources) != 1 || s.CurrentState[0].Sources[0] != covered {
		t.Errorf("unpointed item did not inherit the covered range: %+v", s.CurrentState)
	}
}

// TestSeqRefString pins the wire form, which appears in the instruction as an
// example and must match what the parser accepts. A drift between them is
// invisible: the model copies the example, the parser drops it, and every item
// silently falls back to the covered range.
func TestSeqRefString(t *testing.T) {
	for _, tt := range []struct {
		ref  SeqRef
		want string
	}{
		{SeqRef{Lo: 5, Hi: 5}, "[seq:5]"},
		{SeqRef{Lo: 5}, "[seq:5]"},
		{SeqRef{Lo: 5, Hi: 40}, "[seq:5-40]"},
	} {
		if got := tt.ref.String(); got != tt.want {
			t.Errorf("SeqRef%+v.String() = %q, want %q", tt.ref, got, tt.want)
		}
		refs, _ := extractSeqRefs("x " + tt.ref.String())
		if len(refs) != 1 {
			t.Errorf("the parser does not accept the form the instruction advertises: %q", tt.ref.String())
		}
	}
}

// TestInstructionAsksForEverySection pins the prompt/parser handshake in the
// direction that breaks silently.
//
// If a section is added to SummarySections but not to the instruction, the
// model never emits it, ParseStructured rejects every response, and compaction
// fails outright — loudly, at least. The reverse (instruction asks for a
// heading the parser does not know) fails on the extra-heading check, also
// loudly. What this test buys is that neither takes a production compaction to
// discover.
func TestInstructionAsksForEverySection(t *testing.T) {
	for _, window := range []int{0, 200, 8000, 200000} {
		for _, hasPrior := range []bool{false, true} {
			got := summaryInstruction(300, hasPrior, SeqRef{Lo: 1, Hi: 50}, window)
			for _, section := range SummarySections {
				if !strings.Contains(got, "## "+section) {
					t.Errorf("window=%d hasPrior=%v: instruction never names %q, so the model cannot emit it and ParseStructured will reject every response",
						window, hasPrior, section)
				}
			}
			if !strings.Contains(got, emptyMarker) {
				t.Errorf("window=%d hasPrior=%v: instruction never mentions %q, so an empty section comes back blank and fails to parse",
					window, hasPrior, emptyMarker)
			}
			if !strings.Contains(got, "[seq:") {
				t.Errorf("window=%d hasPrior=%v: instruction never shows the source-pointer form", window, hasPrior)
			}
		}
	}
}

// TestInstructionSwitchesToUpdateSemantics pins C4's incremental half.
//
// The distinction is the whole point: told to "summarize", a model handed its
// own prior summary plus new turns writes a summary OF BOTH, stacking the old
// state next to the new. Told to "update" and "replace superseded items in
// place", it merges. Nothing downstream can tell those apart after the fact,
// which is why the instruction has to be right rather than checked.
func TestInstructionSwitchesToUpdateSemantics(t *testing.T) {
	for _, window := range []int{0, 200, 200000} {
		fresh := summaryInstruction(300, false, SeqRef{Lo: 1, Hi: 50}, window)
		update := summaryInstruction(300, true, SeqRef{Lo: 1, Hi: 50}, window)
		if fresh == update {
			t.Fatalf("window=%d: the update instruction is identical to the fresh one; C4's incremental semantics are not reaching the model", window)
		}
		if !strings.Contains(strings.ToLower(update), "update") {
			t.Errorf("window=%d: update instruction never says to update: %q", window, firstRunes(update, 120))
		}
		if !strings.Contains(strings.ToLower(update), "replace") {
			t.Errorf("window=%d: update instruction never says to replace superseded items, which is the clause that stops the two versions being stacked", window)
		}
		if strings.Contains(strings.ToLower(fresh), "previous") || strings.Contains(strings.ToLower(fresh), "update") {
			t.Errorf("window=%d: the FRESH instruction refers to a previous summary that does not exist: %q", window, firstRunes(fresh, 120))
		}
	}
}

// TestInstructionSizesAreBounded is the budget guard.
//
// The instruction is charged against the model window on EVERY chunk of the
// carry loop, so its size is not a cosmetic property: an instruction that eats
// an eighth of a small window turns compaction into a sequence of calls that
// mostly transmit the instruction, and one that exceeds the window makes
// chunkBudgetFor negative and prevents compaction entirely.
//
// THE ASSERTION IS "FITS ITS SHARE **OR** IS ALREADY MINIMAL", and the second
// disjunct is not a let-out — it is a measured fact about the design. The
// terse form's floor is the five headings plus the bullet/pointer/empty-marker
// contract, ~124 tokens (this test prints the current number), and that floor
// is what ParseStructured requires; anything below it produces summaries that
// do not parse, which is a worse outcome than an expensive instruction. So
// under roughly a 1000-token window the structured summary genuinely costs
// more than an eighth, and the honest statement of the invariant is that the
// code spends the minimum it can rather than that the minimum is always small.
// Asserting the share unconditionally would be asserting something no
// implementation can satisfy, and the only way to make it pass would be to
// weaken the parser.
func TestInstructionSizesAreBounded(t *testing.T) {
	floor := estimateTextTokens(terseInstruction(300, true))
	t.Logf("terse floor (the parser's minimum): %d tok", floor)
	for _, window := range []int{300, 800, 2000, 8000, 32000, 200000} {
		for _, hasPrior := range []bool{false, true} {
			got := summaryInstruction(300, hasPrior, SeqRef{Lo: 1, Hi: 50}, window)
			tok := estimateTextTokens(got)
			share := window / instructionBudgetDenominator
			t.Logf("window=%6d hasPrior=%-5v tok=%4d (share %d)", window, hasPrior, tok, share)
			if tok <= share {
				continue
			}
			// Over its share: the only acceptable reason is that it is
			// already the terse form and cannot shrink further.
			if terse := terseInstruction(300, hasPrior); got != terse {
				t.Errorf("window=%d hasPrior=%v: instruction is %d tok, over its %d-tok share, "+
					"and is NOT the terse form (%d tok) — the budget switch did not fire",
					window, hasPrior, tok, share, estimateTextTokens(terse))
			}
		}
	}
}

// TestInstructionUsesTheTerseFormOnSmallWindows pins the switch itself, in
// both directions. Without the second half the switch could be hard-wired to
// "always terse" and every test above would still pass while the elaborated
// rules — the ones that keep a superseded decision from being appended rather
// than replaced — silently never reached any model.
func TestInstructionUsesTheTerseFormOnSmallWindows(t *testing.T) {
	for _, hasPrior := range []bool{false, true} {
		small := summaryInstruction(300, hasPrior, SeqRef{Lo: 1, Hi: 50}, 800)
		if small != terseInstruction(300, hasPrior) {
			t.Errorf("hasPrior=%v: an 800-token window did not select the terse form (%d tok of budget %d)",
				hasPrior, estimateTextTokens(small), 800/instructionBudgetDenominator)
		}
		big := summaryInstruction(300, hasPrior, SeqRef{Lo: 1, Hi: 50}, 200000)
		if big == terseInstruction(300, hasPrior) {
			t.Errorf("hasPrior=%v: a 200K window still used the terse form; the elaborated rules never reach any model", hasPrior)
		}
		if !strings.Contains(big, "PRESERVE VERBATIM") {
			t.Errorf("hasPrior=%v: the full form lost its preserve-verbatim rule", hasPrior)
		}
		// Unbudgeted (0) means "no window known", which must not be read as
		// "a tiny window" — that would silently downgrade every caller that
		// has not wired a window through.
		if unbudgeted := summaryInstruction(300, hasPrior, SeqRef{Lo: 1, Hi: 50}, 0); unbudgeted != big {
			t.Errorf("hasPrior=%v: window=0 did not select the full form; an unknown window is not a small one", hasPrior)
		}
	}
}

// TestInstructionOmitsUncitableRange pins that the zero SeqRef is treated as
// "no sequence numbers exist" rather than "the range starts at 0".
//
// The mid-turn compaction path summarizes messages that have not been
// persisted and have no seq. Quoting "0-0" to it would advertise a range that
// history_read resolves to nothing — a pointer that looks recoverable and is
// not, which costs a wasted tool call and teaches the model the pointers are
// noise.
func TestInstructionOmitsUncitableRange(t *testing.T) {
	const window = 200000
	withRange := summaryInstruction(300, false, SeqRef{Lo: 10, Hi: 90}, window)
	if !strings.Contains(withRange, "10-90") {
		t.Error("a real covered range is not quoted to the model")
	}
	without := summaryInstruction(300, false, SeqRef{}, window)
	if strings.Contains(without, "sequence range 0-0") {
		t.Error("the zero SeqRef was quoted as a real range; history_read would resolve it to nothing")
	}
	// The pointer CONTRACT still has to be stated even with no range to cite:
	// items carry their own pointers regardless.
	if !strings.Contains(without, "[seq:") {
		t.Error("dropping the range clause also dropped the pointer contract")
	}
}

// TestTerseInstructionKeepsWhatTheParserNeeds pins the cut line.
//
// The terse form exists to fit a small window, and the temptation when
// trimming further is to drop a heading or the empty marker — both of which
// are cheap in tokens and load-bearing for ParseStructured. A terse
// instruction that produces unparseable summaries is worse than no compaction:
// it burns a model call per attempt and never succeeds.
func TestTerseInstructionKeepsWhatTheParserNeeds(t *testing.T) {
	for _, hasPrior := range []bool{false, true} {
		got := terseInstruction(200, hasPrior)
		for _, section := range SummarySections {
			if !strings.Contains(got, "## "+section) {
				t.Errorf("hasPrior=%v: terse form dropped heading %q, so nothing it produces will parse", hasPrior, section)
			}
		}
		if !strings.Contains(got, emptyMarker) {
			t.Errorf("hasPrior=%v: terse form dropped %q", hasPrior, emptyMarker)
		}
		if !strings.Contains(got, "[seq:") {
			t.Errorf("hasPrior=%v: terse form dropped the source-pointer form", hasPrior)
		}
	}
}

// TestContainsPriorSummary covers the detector that selects update semantics
// across compactions, not just within one carry loop.
func TestContainsPriorSummary(t *testing.T) {
	plain := []*schema.Message{
		{Role: schema.User, Content: "do the thing"},
		{Role: schema.Assistant, Content: "done"},
	}
	if containsPriorSummary(plain) {
		t.Error("plain history reported as containing a prior summary")
	}
	// The aged-out case: a summary that is no longer the last message. Plan
	// only short-circuits while it IS last, so this is the shape that reaches
	// the summarizer on a second compaction.
	aged := []*schema.Message{
		{Role: schema.User, Content: SummarySentinel + "earlier state"},
		{Role: schema.User, Content: "next request"},
		{Role: schema.Assistant, Content: "working"},
	}
	if !containsPriorSummary(aged) {
		t.Error("a summary that has aged out of the tail was not detected, so the second compaction would be told to write a fresh summary over the top of it")
	}
	if containsPriorSummary(nil) {
		t.Error("nil history reported as containing a prior summary")
	}
}

// TestQualityGateRequiresStructure wires C4's parser to C10's gate: with
// RequireStructure on, a summary that does not parse is rejected before
// Assemble is allowed to delete the messages it was meant to replace.
//
// The prose case is the one that matters. Before C4 that string was the BEST
// output this package could ask for, so it clears every other rule in the
// policy — long enough, not an acknowledgement, mentions real work. It is only
// detectable as a regression once there is a shape to check it against, which
// is the whole reason the two features are worth having together.
func TestQualityGateRequiresStructure(t *testing.T) {
	const prose = "I wired the structured summary into RunSummary, fixed the token " +
		"estimator so it no longer undercounts Chinese text, and updated the tests " +
		"in internal/ctxcompact. The suite passes. Next up is folding tool results."

	tests := []struct {
		name       string
		summary    string
		policy     QualityPolicy
		wantReject bool
		why        string
	}{
		{
			"structured_passes", wellFormed,
			QualityPolicy{RequireStructure: true}, false,
			"",
		},
		{
			"prose_rejected", prose,
			QualityPolicy{RequireStructure: true}, true,
			"prose clears every other rule; only the structure check can see it",
		},
		{
			"missing_section_rejected",
			strings.Replace(wellFormed, "## Open Work\n- Fold tool results under window pressure [seq:40-52]", "", 1),
			QualityPolicy{RequireStructure: true}, true,
			"a dropped section reads downstream as a task with nothing left to do",
		},
		{
			"prose_passes_when_structure_not_required", prose,
			QualityPolicy{MinChars: 40, MinCompressionDenominator: 1000}, false,
			"the check is opt-in; a caller with a non-structured summarizer keeps working",
		},
		{
			"substring_check_is_weaker",
			"I finished the Active Task, recorded Current State and Constraints, " +
				"noted the Decisions, and there is no Open Work left to do at all.",
			QualityPolicy{RequiredSections: SummarySections}, false,
			"documents this: RequiredSections passes on a sentence that merely NAMES the headings",
		},
		{
			"structure_check_catches_what_substring_misses",
			"I finished the Active Task, recorded Current State and Constraints, " +
				"noted the Decisions, and there is no Open Work left to do at all.",
			QualityPolicy{RequireStructure: true}, true,
			"the same string, rejected by the stronger check",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := CheckQuality(tt.summary, 40000, tt.policy)
			if tt.wantReject && err == nil {
				t.Fatalf("accepted a summary that should have been rejected (%s)", tt.why)
			}
			if !tt.wantReject && err != nil {
				t.Fatalf("rejected a summary that should have passed (%s): %v", tt.why, err)
			}
		})
	}
}

// TestRequireStructureIsOffByDefault pins that turning C4 on did not silently
// arm a gate every existing caller has to satisfy. A caller whose summarizer
// is a different model — or a fake in a test — must keep working; a gate that
// rejects everything is indistinguishable from compaction being broken.
func TestRequireStructureIsOffByDefault(t *testing.T) {
	if DefaultQualityPolicy.RequireStructure {
		t.Error("DefaultQualityPolicy demands the structured form, so every caller with a non-structured summarizer now fails every compaction")
	}
	if (QualityPolicy{}).RequireStructure {
		t.Error("the zero policy demands structure")
	}
	// But asking for it alone must ENABLE the gate — otherwise the field is
	// settable and inert, which is worse than absent.
	if !(QualityPolicy{RequireStructure: true}).enabled() {
		t.Error("RequireStructure alone does not enable the gate, so setting it does nothing")
	}
}

// TestOptionsCoveredSeqReachesTheInstruction pins the transport-facing seam.
//
// Options is the ONLY way MaybeCompact/ForceCompact callers can configure a
// compaction, so a field that is settable there but never copied into RunOpts
// is a field with a full test suite and zero effect in production — the exact
// shape C11's Redactor nearly shipped as, and the reason Options exists at
// all. This asserts the copy by following it all the way to the text the model
// receives, rather than comparing two structs (which would pass if runOpts
// copied it into a field nothing reads).
func TestOptionsCoveredSeqReachesTheInstruction(t *testing.T) {
	const window = 200000
	opts := Options{CoveredSeq: SeqRef{Lo: 314, Hi: 592}}
	got := opts.runOpts(window)
	if got.CoveredSeq != opts.CoveredSeq {
		t.Fatalf("Options.CoveredSeq %v did not reach RunOpts (%v); the field is settable and inert",
			opts.CoveredSeq, got.CoveredSeq)
	}
	instr := summaryInstruction(got.SummaryWordLimit, false, got.CoveredSeq, got.ModelWindow)
	if !strings.Contains(instr, "314-592") {
		t.Errorf("the configured range never reaches the model, so every [seq:…] pointer it writes is a guess:\n%s",
			firstRunes(instr, 400))
	}
}
