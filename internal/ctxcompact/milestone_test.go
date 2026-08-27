// internal/ctxcompact/milestone_test.go
package ctxcompact

import (
	"strings"
	"testing"
)

// TestMilestonesFromSummary_HarvestsEveryPointedBullet is C7's core claim: the
// structured summary the model already writes IS the label source, so an index
// can be built with no extra model call and no reliance on the model choosing
// to annotate anything.
func TestMilestonesFromSummary_HarvestsEveryPointedBullet(t *testing.T) {
	doc := `## Active Task
Wire the eviction map.

## Current State
- fixed the compile errors in internal/tools [seq:40-60]
- tests are green [seq:61-70]

## Constraints
- no new dependencies [seq:5]

## Decisions
- keep the map in the prompt [seq:80-92]

## Open Work
- (none)`
	got := MilestonesFromSummary(doc, SeqRef{Lo: 1, Hi: 99})
	if len(got) != 4 {
		t.Fatalf("harvested %d milestones, want 4: %+v", len(got), got)
	}
	// Ascending span order, not section order — the index is a timeline.
	wantOrder := []int{5, 40, 61, 80}
	for i, lo := range wantOrder {
		if got[i].Span.Lo != lo {
			t.Errorf("milestone %d starts at seq %d, want %d (order must be by span, not by section): %+v",
				i, got[i].Span.Lo, lo, got)
		}
	}
	if !strings.Contains(got[1].Headline, "compile errors") {
		t.Errorf("headline lost its text: %q", got[1].Headline)
	}
	if strings.Contains(got[1].Headline, "seq:") {
		t.Errorf("pointer left in the headline: %q", got[1].Headline)
	}
}

// TestMilestonesFromSummary_TableOfShapes covers what is kept and what is
// dropped, in one table, because each row is a different reason a bullet does
// not become an index entry.
func TestMilestonesFromSummary_TableOfShapes(t *testing.T) {
	const head = "## Active Task\nt\n\n## Current State\n"
	const tail = "\n\n## Constraints\n(none)\n\n## Decisions\n(none)\n\n## Open Work\n(none)"

	cases := []struct {
		name    string
		bullets string
		covered SeqRef
		want    int
		why     string
	}{
		{
			"one pointer one milestone", "- did a thing [seq:10-20]",
			SeqRef{Lo: 1, Hi: 99}, 1, "",
		},
		{
			"two pointers on one bullet", "- did a thing [seq:10-20][seq:30]",
			SeqRef{Lo: 1, Hi: 99}, 2,
			"a bullet citing two spans labels both of them; collapsing to one would leave a span unlabelled",
		},
		{
			"unpointed bullet inherits covered", "- did a thing",
			SeqRef{Lo: 1, Hi: 99}, 1,
			"ParseStructured supplies the covered range as the fallback, so the entry is coarse but real",
		},
		{
			"unpointed bullet with no covered range", "- did a thing",
			SeqRef{}, 0,
			"the fallback is the zero range, which history_read cannot resolve; a dead address is worse than none",
		},
		{
			"(none) section yields nothing", "(none)",
			SeqRef{Lo: 1, Hi: 99}, 0, "",
		},
		{
			"bulleted (none) yields nothing", "- (none)",
			SeqRef{Lo: 1, Hi: 99}, 0,
			"ParseStructured only treats a BARE (none) as an empty section, so a bulleted one " +
				"arrives as an item; indexing it labels the whole covered range with the word (none)",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := MilestonesFromSummary(head+tc.bullets+tail, tc.covered)
			if len(got) != tc.want {
				t.Errorf("got %d milestones, want %d (%s): %+v", len(got), tc.want, tc.why, got)
			}
			for _, m := range got {
				if !m.Span.citable() {
					t.Errorf("emitted an uncitable span %v — history_read resolves it to nothing", m.Span)
				}
			}
		})
	}
}

// TestMilestonesFromSummary_UnparseableYieldsNothing pins the ONE place in
// this package that fails open, and pins that it fails open QUIETLY rather
// than inventing an entry. A caller wired to a summarizer that does not
// produce the structured form must still get a compaction; what it loses is
// the labels, not the compaction.
func TestMilestonesFromSummary_UnparseableYieldsNothing(t *testing.T) {
	got := MilestonesFromSummary("I summarized the conversation for you.", SeqRef{Lo: 1, Hi: 99})
	if got != nil {
		t.Errorf("prose produced milestones: %+v", got)
	}
}

// TestBoundHeadline_IsBoundedAndSingleLine pins both halves of the durable
// bound. The rune cap keeps a paragraph out of a structure whose whole purpose
// is to have a ceiling; the flattening keeps a multi-line label from silently
// becoming two directory lines, the second of them attached to no span.
func TestBoundHeadline_IsBoundedAndSingleLine(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{"long", strings.Repeat("verylongword ", 60)},
		{"multiline", "first line\nsecond line\nthird line"},
		{"cjk long", strings.Repeat("上下文压缩", 100)},
		{"whitespace collapse", "a    b\t\tc\n\nd"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := boundHeadline(tc.in)
			if n := len([]rune(got)); n > MaxMilestoneHeadline {
				t.Errorf("headline is %d runes, over the %d cap: %q", n, MaxMilestoneHeadline, got)
			}
			if strings.ContainsAny(got, "\n\r") {
				t.Errorf("headline still contains a line break: %q", got)
			}
		})
	}
}

// TestTruncateRunes_CutsOnRunesNotBytes is the CJK case stated directly: a
// byte-based cut through a multi-byte rune produces mojibake in the one
// artefact that is re-shown on every turn.
func TestTruncateRunes_CutsOnRunesNotBytes(t *testing.T) {
	in := strings.Repeat("压", 50)
	got := truncateRunes(in, 10)
	if n := len([]rune(got)); n != 10 {
		t.Errorf("truncated to %d runes, want 10: %q", n, got)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("a shortened label must say so, else it reads as a terse one: %q", got)
	}
	if strings.Contains(got, "�") {
		t.Errorf("cut through a rune: %q", got)
	}
	// Short enough to keep is kept untouched, marker and all.
	if got := truncateRunes("short", 10); got != "short" {
		t.Errorf("truncateRunes altered a string under the limit: %q", got)
	}
}
