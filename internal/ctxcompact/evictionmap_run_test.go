// internal/ctxcompact/evictionmap_run_test.go
package ctxcompact

import (
	"context"
	"strings"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

// scriptedSummarizer answers every summary call with the same text. It is a
// local fake rather than einollm.FakeModel because this file is an IN-PACKAGE
// test and internal/llm/eino imports this package — the fake would close an
// import cycle.
type scriptedSummarizer struct {
	reply string
	calls int
}

func (s *scriptedSummarizer) Generate(context.Context, []*schema.Message, ...model.Option) (*schema.Message, error) {
	s.calls++
	return &schema.Message{Role: schema.Assistant, Content: s.reply}, nil
}

func (s *scriptedSummarizer) Stream(ctx context.Context, msgs []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	msg, err := s.Generate(ctx, msgs, opts...)
	if err != nil {
		return nil, err
	}
	sr, sw := schema.Pipe[*schema.Message](1)
	sw.Send(msg, nil)
	sw.Close()
	return sr, nil
}

// structuredSummaryFor builds a valid five-section summary citing the given
// span, so a fake summarizer can return something the milestone harvester can
// actually read.
func structuredSummaryFor(headline string, lo, hi int) string {
	return "## Active Task\nkeep going\n\n" +
		"## Current State\n- " + headline + " " + SeqRef{Lo: lo, Hi: hi}.String() + "\n\n" +
		"## Constraints\n(none)\n\n" +
		"## Decisions\n(none)\n\n" +
		"## Open Work\n(none)"
}

// runnableHistory is a history Plan will actually split (some messages get
// summarized rather than everything being pinned).
func runnableHistory() []*schema.Message {
	return []*schema.Message{
		{Role: schema.User, Content: "start the task"},
		{Role: schema.Assistant, Content: strings.Repeat("chatter one ", 200)},
		{Role: schema.Assistant, Content: strings.Repeat("chatter two ", 200)},
		{Role: schema.Assistant, Content: strings.Repeat("chatter three ", 200)},
		{Role: schema.User, Content: "keep going"},
		{Role: schema.Assistant, Content: "on it"},
	}
}

// TestRun_EvictionMapReachesTheAssembledHistory is the wiring assertion: the
// map is not merely computable, it is IN the messages Run hands back. A map
// built and not attached is the defect this repo hits most — the write side
// finished, the read side never existed.
func TestRun_EvictionMapReachesTheAssembledHistory(t *testing.T) {
	fm := &scriptedSummarizer{reply: structuredSummaryFor("fixed the compile errors", 12, 30)}
	em := &EvictionMap{}

	res, err := Run(context.Background(), runnableHistory(), PlanOpts{KeepRecent: 1},
		RunOpts{
			ModelWindow:        8000,
			ChunkThreshold:     0.9,
			CoveredSeq:         SeqRef{Lo: 10, Hi: 40},
			EvictionMap:        em,
			DisableQualityGate: true,
		}, fm, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	var mapMsg *schema.Message
	for _, m := range res.Messages {
		if IsEvictionMapMessage(m) {
			mapMsg = m
		}
	}
	if mapMsg == nil {
		t.Fatal("no eviction map message in the assembled history; the map was built and never attached")
	}
	if !strings.Contains(mapMsg.Content, "fixed the compile errors") {
		t.Errorf("the map carries no harvested milestone:\n%s", mapMsg.Content)
	}
	if !strings.Contains(mapMsg.Content, "[seq:10-40]") {
		t.Errorf("the map lost the evicted span:\n%s", mapMsg.Content)
	}
	// The summary must still be LAST — the map is archive material, the
	// summary is the current state, and the model should carry the latter into
	// the next turn.
	last := res.Messages[len(res.Messages)-1]
	if !IsSummaryMessage(last) {
		t.Errorf("the last message is not the summary: %q", firstRunes(last.Content, 60))
	}
	// And the two markers must stay distinguishable, or Plan's
	// already-compacted short-circuit reads a map as a summary.
	if IsSummaryMessage(mapMsg) {
		t.Error("the eviction map is indistinguishable from a summary; " +
			"a history ending in one would stop being compactable")
	}
}

// TestRun_EvictionMapAccumulatesAcrossCompactions pins the pointer semantics.
// A map copied per call would make every compaction the first one, producing a
// directory that only ever describes the most recent eviction — which is
// exactly the state C3 exists to fix.
func TestRun_EvictionMapAccumulatesAcrossCompactions(t *testing.T) {
	em := &EvictionMap{}
	spans := []SeqRef{{Lo: 1, Hi: 30}, {Lo: 31, Hi: 70}, {Lo: 71, Hi: 120}}
	labels := []string{"set up the repo", "wrote the parser", "fixed the tests"}

	for i, span := range spans {
		fm := &scriptedSummarizer{reply: structuredSummaryFor(labels[i], span.Lo, span.Hi)}
		if _, err := Run(context.Background(), runnableHistory(), PlanOpts{KeepRecent: 1},
			RunOpts{
				ModelWindow: 8000, ChunkThreshold: 0.9,
				CoveredSeq: span, EvictionMap: em, DisableQualityGate: true,
			}, fm, nil); err != nil {
			t.Fatalf("compaction %d: %v", i, err)
		}
	}
	got := em.Render(0)
	for i, label := range labels {
		if !strings.Contains(got, label) {
			t.Errorf("eviction %d (%q) is missing from the accumulated map:\n%s", i, label, got)
		}
	}
	if span, _ := em.Span(); span.Lo != 1 || span.Hi != 120 {
		t.Errorf("accumulated span is %v, want [1,120]", span)
	}
}

// TestRun_NoEvictionRecordedWhenTheSummaryIsRejected is the ordering claim.
// Both gates keep the ORIGINAL history when they fire, so a map entry written
// before them would advertise as evicted a span that is still in the live
// window — and the map has no mechanism to retract an entry.
func TestRun_NoEvictionRecordedWhenTheSummaryIsRejected(t *testing.T) {
	for _, tc := range []struct {
		name    string
		reply   string
		policy  QualityPolicy
		disable bool
	}{
		{"empty summary", "   ", QualityPolicy{}, true},
		{"quality rejection", "OK, I have summarized the conversation.",
			QualityPolicy{MinChars: 400, MinCompressionDenominator: 4}, false},
		{"unstructured under RequireStructure",
			strings.Repeat("a long piece of prose that is not a structured summary. ", 40),
			QualityPolicy{RequireStructure: true}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fm := &scriptedSummarizer{reply: tc.reply}
			em := &EvictionMap{}
			_, err := Run(context.Background(), runnableHistory(), PlanOpts{KeepRecent: 1},
				RunOpts{
					ModelWindow: 8000, ChunkThreshold: 0.9,
					CoveredSeq: SeqRef{Lo: 10, Hi: 40}, EvictionMap: em,
					Quality: tc.policy, DisableQualityGate: tc.disable,
				}, fm, nil)
			if err == nil {
				t.Fatal("expected the compaction to be refused")
			}
			if !em.IsEmpty() {
				t.Errorf("an eviction was recorded for a compaction that did not happen: %+v", em.Tiers)
			}
		})
	}
}

// TestRun_MidTurnPathRecordsNothing covers the caller with no persisted seq
// numbers. Recording there would fill a permanent structure with addresses
// history_read resolves to nothing.
func TestRun_MidTurnPathRecordsNothing(t *testing.T) {
	fm := &scriptedSummarizer{reply: structuredSummaryFor("did work", 1, 5)}
	em := &EvictionMap{}
	res, err := Run(context.Background(), runnableHistory(), PlanOpts{KeepRecent: 1},
		RunOpts{
			ModelWindow: 8000, ChunkThreshold: 0.9,
			CoveredSeq:  SeqRef{}, // no persisted seqs — the mid-turn path
			EvictionMap: em, DisableQualityGate: true,
		}, fm, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !em.IsEmpty() {
		t.Errorf("recorded an eviction with no resolvable seq: %+v", em.Tiers)
	}
	for _, m := range res.Messages {
		if IsEvictionMapMessage(m) {
			t.Error("attached a map message with no resolvable spans")
		}
	}
}

// TestRun_NilEvictionMapIsThePreC3Behaviour — adopting C3 must be per-caller,
// so a caller that passes no map gets exactly what it got before.
func TestRun_NilEvictionMapIsThePreC3Behaviour(t *testing.T) {
	fm := &scriptedSummarizer{reply: structuredSummaryFor("did work", 12, 30)}
	res, err := Run(context.Background(), runnableHistory(), PlanOpts{KeepRecent: 1},
		RunOpts{
			ModelWindow: 8000, ChunkThreshold: 0.9,
			CoveredSeq: SeqRef{Lo: 10, Hi: 40}, DisableQualityGate: true,
		}, fm, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, m := range res.Messages {
		if IsEvictionMapMessage(m) {
			t.Fatal("a caller that supplied no map got one anyway")
		}
	}
	if !IsSummaryMessage(res.Messages[len(res.Messages)-1]) {
		t.Error("the summary is no longer the tail message")
	}
}

// TestOptions_EvictionMapFieldsReachRunOpts is the same one-line-omission
// guard TestOptions_FieldsReachRunOpts exists for, extended to C3: a field
// added to Options and not copied in runOpts is a setting that is silently
// inert everywhere except the test that sets it directly.
func TestOptions_EvictionMapFieldsReachRunOpts(t *testing.T) {
	em := &EvictionMap{}
	got := Options{EvictionMap: em, EvictionMapBudget: 777}.runOpts(100000)
	if got.EvictionMap != em {
		t.Error("Options.EvictionMap does not reach RunOpts; every entry-point caller would get no map")
	}
	if got.EvictionMapBudget != 777 {
		t.Errorf("EvictionMapBudget = %d, want 777", got.EvictionMapBudget)
	}
	if zero := (Options{}).runOpts(100000); zero.EvictionMap != nil {
		t.Error("the zero Options produced a map; adopting C3 must be opt-in")
	}
}
