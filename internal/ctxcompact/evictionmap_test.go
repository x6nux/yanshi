// internal/ctxcompact/evictionmap_test.go
package ctxcompact

import (
	"fmt"
	"strings"
	"testing"
)

// mapWith builds a map by replaying n evictions of `per` milestones each,
// spans laid out contiguously so a test can reason about what should be
// recoverable.
func mapWith(t *testing.T, n, per int) *EvictionMap {
	t.Helper()
	m := &EvictionMap{}
	seq := 1
	for i := 0; i < n; i++ {
		lo := seq
		var ms []Milestone
		for j := 0; j < per; j++ {
			ms = append(ms, Milestone{
				Span:     SeqRef{Lo: seq, Hi: seq + 1},
				Headline: fmt.Sprintf("eviction %d milestone %d", i, j),
			})
			seq += 2
		}
		if per == 0 {
			seq += 10
		}
		m.AddEviction(ms, SeqRef{Lo: lo, Hi: seq - 1})
	}
	return m
}

// TestEvictionMap_CarriesLikeAnOdometer is the structural claim: a tier never
// exceeds TierCap, because when it would, the older blocks roll up.
func TestEvictionMap_CarriesLikeAnOdometer(t *testing.T) {
	for _, n := range []int{1, 5, TierCap, TierCap + 1, TierCap * TierCap, 250} {
		t.Run(fmt.Sprintf("%d_evictions", n), func(t *testing.T) {
			m := mapWith(t, n, 2)
			for k, tier := range m.Tiers {
				if len(tier) > TierCap {
					t.Errorf("tier %d holds %d blocks, over the cap of %d — the carry did not fire",
						k, len(tier), TierCap)
				}
			}
			if n >= TierCap && len(m.Tiers) < 2 {
				t.Errorf("%d evictions produced %d tier(s); the carry never cascaded",
					n, len(m.Tiers))
			}
		})
	}
}

// TestEvictionMap_GrowsLogarithmically is C3's headline constraint stated as a
// measurement: the map lives in the prompt on every turn, so a structure that
// grew linearly with the number of compactions would become the context
// pressure it exists to relieve.
//
// The bound asserted is on the RAW line count, before any rendering budget is
// applied. The budget alone would make the render bounded no matter how the
// structure grew — it would just degrade to the global-span floor early and
// stay there — so measuring after it would prove nothing about the odometer.
//
// The comparison is against the LINEAR projection rather than a hand-picked
// ratio. A ratio ("no more than 4x") is a fit to today's TierCap and today's
// milestones-per-eviction, and re-tuning it whenever the shape changes turns
// the test into a record of what the code does. What the code must never do is
// scale with the history, and the linear projection is that claim written down.
func TestEvictionMap_GrowsLogarithmically(t *testing.T) {
	const smallN, hugeN = 10, 1000
	small := len(mapWith(t, smallN, 3).expandedLines(0))
	huge := len(mapWith(t, hugeN, 3).expandedLines(0))

	linear := small * (hugeN / smallN)
	// An eighth of linear growth is still far above anything logarithmic
	// (log10 predicts roughly 3x here); the point of the loose bound is that it
	// fails on a REGRESSION to linear rather than on a change to the carry.
	if huge > linear/8 {
		t.Errorf("%dx more evictions produced %d lines; linear growth would be %d and the carry "+
			"is supposed to keep this near the logarithm (%d lines at %d evictions)",
			hugeN/smallN, huge, linear, small, smallN)
	}
	t.Logf("directory lines: %d evictions → %d, %d evictions → %d (linear would be %d)",
		smallN, small, hugeN, huge, linear)
}

// TestEvictionMap_RenderRespectsTheBudget walks every degradation step and
// asserts the two things that must hold at each: the output fits, and it still
// tells the model how to recover what it lists.
//
// Below MinEvictionMapChars the budget cannot be met — the floor render is the
// shortest TRUE map there is — so the assertion there is the floor size rather
// than the requested budget. Pretending otherwise would either require a
// render that names a span without saying it can be read, or an empty render,
// which tells the model nothing was evicted.
func TestEvictionMap_RenderRespectsTheBudget(t *testing.T) {
	m := mapWith(t, 400, 4)
	for _, budget := range []int{50, 200, 800, 2000, 100000} {
		t.Run(fmt.Sprintf("budget_%d", budget), func(t *testing.T) {
			got := m.Render(budget)
			if got == "" {
				t.Fatal("a non-empty map rendered as nothing")
			}
			body := strings.TrimPrefix(got, evictionMapPreamble+"\n\n")
			want := budget
			if want < MinEvictionMapChars {
				want = MinEvictionMapChars
			}
			if len(body) > want {
				t.Errorf("directory is %d chars, over the effective ceiling of %d "+
					"(requested %d, floor %d)", len(body), want, budget, MinEvictionMapChars)
			}
			if !strings.Contains(got, "history_read") {
				t.Error("the map never names the tool that recovers what it lists; " +
					"a directory with no way to open an entry is decoration")
			}
			if !strings.Contains(got, "[seq:") {
				t.Error("the map lists no seq span, so nothing in it is addressable")
			}
		})
	}
}

// TestEvictionMap_FloorFitsItsDeclaredSize keeps MinEvictionMapChars honest.
// It is a documented constant callers may size a budget against, so a floor
// render that outgrew it would make the budget contract quietly wrong.
func TestEvictionMap_FloorFitsItsDeclaredSize(t *testing.T) {
	// Large seq numbers: the floor line embeds them twice, so the widest
	// realistic case is the one to measure.
	m := &EvictionMap{}
	m.AddEviction(nil, SeqRef{Lo: 1, Hi: 9999999})
	m.AddEviction(nil, SeqRef{Lo: 10000000, Hi: 99999999})
	got := renderedSize(m.globalSpanLines())
	if got > MinEvictionMapChars {
		t.Errorf("the floor render is %d chars but MinEvictionMapChars claims %d; "+
			"a caller sizing a budget from the constant would get a map that does not fit",
			got, MinEvictionMapChars)
	}
	t.Logf("floor render: %d chars (declared ceiling %d)", got, MinEvictionMapChars)
}

// TestEvictionMap_FloorStillRecoversEverything pins the bottom of the
// degradation ladder. Under an absurd budget the map keeps ONE line — and that
// line must still be a real, complete span, because the alternative (drop the
// map) tells the model nothing was evicted, which is a lie it will act on.
func TestEvictionMap_FloorStillRecoversEverything(t *testing.T) {
	m := mapWith(t, 500, 5)
	span, ok := m.Span()
	if !ok {
		t.Fatal("a populated map reports no span")
	}
	got := m.Render(10)
	if !strings.Contains(got, fmt.Sprintf("%d", span.Lo)) || !strings.Contains(got, fmt.Sprintf("%d", span.Hi)) {
		t.Errorf("the floor render lost the archive bounds %v:\n%s", span, got)
	}
	if !strings.Contains(got, "history_read") {
		t.Errorf("the floor render dropped the recovery instruction:\n%s", got)
	}
}

// TestEvictionMap_CarryPreservesTotalSpan is the losslessness claim about the
// carry: folding blocks up a tier is a change of RESOLUTION, never of extent.
// If a carry could shrink the covered range, the messages in the lost part
// would be unreachable in practice — present in the log, absent from every map
// the model will ever see.
func TestEvictionMap_CarryPreservesTotalSpan(t *testing.T) {
	m := &EvictionMap{}
	wantLo, wantHi := 1, 0
	for i := 0; i < 400; i++ {
		lo := 1 + i*10
		hi := lo + 9
		m.AddEviction([]Milestone{{Span: SeqRef{Lo: lo, Hi: hi}, Headline: fmt.Sprintf("step %d", i)}},
			SeqRef{Lo: lo, Hi: hi})
		wantHi = hi
		got, ok := m.Span()
		if !ok {
			t.Fatalf("after %d evictions the map reports no span", i+1)
		}
		if got.Lo != wantLo || got.Hi != wantHi {
			t.Fatalf("after %d evictions the covered span is %v, want [%d,%d] — a carry lost history",
				i+1, got, wantLo, wantHi)
		}
	}
}

// TestEvictionMap_UnlabelledSpanStillGetsABlock is the "(no milestone)" case.
// An eviction with nothing to say about it is exactly the eviction the model
// most needs an address for, since the summary will not mention it either.
func TestEvictionMap_UnlabelledSpanStillGetsABlock(t *testing.T) {
	m := &EvictionMap{}
	m.AddEviction(nil, SeqRef{Lo: 40, Hi: 120})
	if m.IsEmpty() {
		t.Fatal("an unlabelled eviction was dropped entirely; those 81 messages are now invisible")
	}
	got := m.Render(0)
	if !strings.Contains(got, NoMilestone) {
		t.Errorf("the unlabelled span is not marked as such:\n%s", got)
	}
	if !strings.Contains(got, "[seq:40-120]") {
		t.Errorf("the unlabelled span lost its address:\n%s", got)
	}
}

// TestEvictionMap_IgnoresUncitableSpans is the mid-turn path's protection.
// Those messages have no persisted seq, so recording them would fill a
// permanent structure with [seq:0] entries that history_read resolves to
// nothing — which costs a wasted tool call and teaches the model the whole map
// is noise.
func TestEvictionMap_IgnoresUncitableSpans(t *testing.T) {
	m := &EvictionMap{}
	m.AddEviction([]Milestone{{Span: SeqRef{}, Headline: "x"}}, SeqRef{})
	if !m.IsEmpty() {
		t.Errorf("recorded an eviction with no resolvable seq: %+v", m.Tiers)
	}
	if got := m.Render(0); got != "" {
		t.Errorf("an empty map rendered %q; a header announcing an index with no entries "+
			"claims context was evicted when none was", got)
	}
	// A citable block span with an uncitable milestone keeps the block and
	// drops only the label: the span is what makes the messages reachable.
	m.AddEviction([]Milestone{{Span: SeqRef{}, Headline: "x"}}, SeqRef{Lo: 5, Hi: 9})
	if m.IsEmpty() {
		t.Fatal("a citable span was dropped because its milestone was not citable")
	}
	if !strings.Contains(m.Render(0), NoMilestone) {
		t.Error("an uncitable milestone was rendered as if it were a real label")
	}
}

// TestEvictionMap_RenderIsDeterministic matters for the provider's prefix
// cache: the map is re-sent every turn, so a render that varied between calls
// on unchanged state would invalidate the cache on every turn for no reason.
func TestEvictionMap_RenderIsDeterministic(t *testing.T) {
	m := mapWith(t, 37, 3)
	first := m.Render(0)
	for i := 0; i < 5; i++ {
		if got := m.Render(0); got != first {
			t.Fatalf("render %d differs from the first on unchanged state", i)
		}
	}
}

// TestEvictionLine_TextKeepsBothEndpoints pins the self-similarity that lets
// the carry cascade. A folded line that showed only its first headline would
// make a long stretch look like it ended where it began.
func TestEvictionLine_TextKeepsBothEndpoints(t *testing.T) {
	single := EvictionLine{Lo: 3, Hi: 3, Head: "one thing", Tail: "one thing"}
	if got := single.Text(); got != "one thing" {
		t.Errorf("a single-headline line rendered as %q", got)
	}
	span := EvictionLine{Lo: 3, Hi: 90, Head: "started here", Tail: "ended there"}
	got := span.Text()
	if !strings.Contains(got, "started here") || !strings.Contains(got, "ended there") {
		t.Errorf("a folded line dropped an endpoint: %q", got)
	}
	if s := span.Span(); s.Lo != 3 || s.Hi != 90 {
		t.Errorf("Span() = %v, want [3,90]", s)
	}
}

// TestBoundLineText_ShortensBothEndsOrNeither is the same claim at the
// rendering layer, where the temptation is to spend the whole per-line budget
// on the first headline.
func TestBoundLineText_ShortensBothEndsOrNeither(t *testing.T) {
	ln := EvictionLine{
		Lo: 1, Hi: 50,
		Head: strings.Repeat("a", 200),
		Tail: strings.Repeat("b", 200),
	}
	got := boundLineText(ln, 40)
	if len([]rune(got)) > 40+len(" … ") {
		t.Errorf("bounded line is %d runes, well over the limit: %q", len([]rune(got)), got)
	}
	if !strings.Contains(got, "a") || !strings.Contains(got, "b") {
		t.Errorf("one endpoint was shortened out of existence: %q", got)
	}
	// limit <= 0 means "no view limit", used for the full-detail render.
	if got := boundLineText(ln, 0); !strings.Contains(got, strings.Repeat("a", 200)) {
		t.Error("a zero limit truncated anyway; the full-detail render would be lossy")
	}
}

// TestEvictionMap_ZeroValueIsUsable — the map is session state a transport
// carries, so it will be reached through a zero value on the first compaction
// of every conversation.
func TestEvictionMap_ZeroValueIsUsable(t *testing.T) {
	var m *EvictionMap
	if !m.IsEmpty() {
		t.Error("a nil map reports itself non-empty")
	}
	if got := m.Render(0); got != "" {
		t.Errorf("a nil map rendered %q", got)
	}
	if _, ok := m.Span(); ok {
		t.Error("a nil map reports a span")
	}
	var v EvictionMap
	v.AddEviction([]Milestone{{Span: SeqRef{Lo: 1, Hi: 2}, Headline: "x"}}, SeqRef{Lo: 1, Hi: 2})
	if v.IsEmpty() {
		t.Error("the zero value could not record an eviction")
	}
}
