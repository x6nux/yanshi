// internal/tools/evictionmap_e2e_test.go
//
// The C3 end-to-end contract, tested across the seam it is most likely to
// break at.
//
// The eviction map is produced in internal/ctxcompact and consumed by
// history_read in this package, and the thing joining them is a NUMBER: the
// persisted sequence. Each side has tests that pass on its own convention —
// ctxcompact can produce a perfectly well-formed [seq:40-120] while
// history_read reads a log whose messages are numbered differently, and no
// unit test on either side notices. What fails then is only visible in
// production, as a model that follows the map, gets "No messages in that
// range", and stops trusting the map.
//
// So this test writes real messages, compacts a real history, takes the seqs
// OUT OF THE RENDERED MAP TEXT (not out of a Go value the test also built),
// and feeds them to the real history_read.
package tools

import (
	"context"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/ctxcompact"
	"github.com/x6nux/yanshi/internal/store"
)

// e2eSummarizer returns a structured summary whose milestone cites the exact
// span it is told to, standing in for a model that followed the instruction.
type e2eSummarizer struct{ reply string }

func (s *e2eSummarizer) Generate(context.Context, []*schema.Message, ...model.Option) (*schema.Message, error) {
	return &schema.Message{Role: schema.Assistant, Content: s.reply}, nil
}

func (s *e2eSummarizer) Stream(ctx context.Context, msgs []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	m, err := s.Generate(ctx, msgs, opts...)
	if err != nil {
		return nil, err
	}
	sr, sw := schema.Pipe[*schema.Message](1)
	sw.Send(m, nil)
	sw.Close()
	return sr, nil
}

// mapSeqRe pulls the spans back out of the RENDERED map. Parsing the text is
// the point: a test that reused the SeqRef values it passed in would prove the
// two halves agree with the test, not with each other.
var mapSeqRe = regexp.MustCompile(`\[seq:(\d+)(?:-(\d+))?\]`)

// TestEvictionMapSeqsResolveThroughHistoryRead is the C3 hard requirement:
// write the log, compact, read the seqs out of the map, and recover the
// original text with the tool the map names.
func TestEvictionMapSeqsResolveThroughHistoryRead(t *testing.T) {
	s := newHistoryStore(t)
	sid, err := s.CreateSession("eviction map e2e")
	require.NoError(t, err)

	// A log with a recognisable phrase per message, so a recovered body can be
	// matched back to the seq it should have come from.
	var batch []store.Message
	for i := 0; i < 24; i++ {
		batch = append(batch, store.Message{
			Role:    store.RoleAssistant,
			Content: "durable message number " + strconv.Itoa(i) + " about compaction",
		})
	}
	_, nextSeq, err := s.AppendMessages(sid, batch)
	require.NoError(t, err)
	require.Equal(t, len(batch), nextSeq, "seq should run 0..n-1 for a fresh session")

	// The span actually evicted. Inclusive bounds, matching SeqRef.
	covered := ctxcompact.SeqRef{Lo: 0, Hi: nextSeq - 1}
	summary := "## Active Task\nkeep compacting\n\n" +
		"## Current State\n- worked through the durable messages " +
		ctxcompact.SeqRef{Lo: 3, Hi: 9}.String() + "\n\n" +
		"## Constraints\n(none)\n\n## Decisions\n(none)\n\n## Open Work\n(none)"

	em := &ctxcompact.EvictionMap{}
	res, err := ctxcompact.Run(context.Background(), compactableHistory(), ctxcompact.PlanOpts{KeepRecent: 1},
		ctxcompact.RunOpts{
			ModelWindow: 8000, ChunkThreshold: 0.9,
			CoveredSeq: covered, EvictionMap: em, DisableQualityGate: true,
		}, &e2eSummarizer{reply: summary}, nil)
	require.NoError(t, err)

	var mapText string
	for _, m := range res.Messages {
		if ctxcompact.IsEvictionMapMessage(m) {
			mapText = m.Content
		}
	}
	require.NotEmpty(t, mapText, "no eviction map in the compacted history")

	spans := mapSeqRe.FindAllStringSubmatch(mapText, -1)
	require.NotEmpty(t, spans, "the rendered map contains no seq spans:\n%s", mapText)

	ht := NewHistoryTools(s)
	ctx := historyCtx(t, sid)
	for _, sp := range spans {
		lo, err := strconv.Atoi(sp[1])
		require.NoError(t, err)
		hi := lo
		if sp[2] != "" {
			hi, err = strconv.Atoi(sp[2])
			require.NoError(t, err)
		}
		// history_read's to_seq is EXCLUSIVE while SeqRef.Hi is inclusive. The
		// conversion is the single most likely place for this seam to be off
		// by one, and the assertion below is what would catch it: reading
		// [lo, hi] must return the message numbered hi.
		out, err := runHistoryTool(t, ctx, ht.runRead, map[string]any{
			"from_seq": lo, "to_seq": hi + 1, "limit": 500,
		})
		require.NoError(t, err, "history_read over the map's span [%d,%d]", lo, hi)
		if strings.Contains(out, "No messages in that range") {
			t.Fatalf("the map advertises [seq:%d-%d] but history_read returns nothing for it.\n"+
				"The map is a directory of dead addresses; the model that follows it learns to ignore it.\nmap:\n%s",
				lo, hi, mapText)
		}
		for seq := lo; seq <= hi; seq++ {
			want := "durable message number " + strconv.Itoa(seq) + " about compaction"
			if !strings.Contains(out, want) {
				t.Errorf("history_read(%d,%d) did not return the message at seq %d.\n"+
					"The map's seq numbering and the durable log's disagree.\ngot:\n%s",
					lo, hi+1, seq, out)
			}
		}
	}
}

// TestEvictionMapMilestoneSpanResolvesToItsOwnMessages is the finer claim.
// The block span covering everything would pass the test above even if the
// per-milestone pointers were nonsense — so this one checks that the span a
// HEADLINE cites returns the messages that headline is about.
func TestEvictionMapMilestoneSpanResolvesToItsOwnMessages(t *testing.T) {
	s := newHistoryStore(t)
	sid, err := s.CreateSession("milestone span e2e")
	require.NoError(t, err)

	var batch []store.Message
	for i := 0; i < 20; i++ {
		body := "routine work " + strconv.Itoa(i)
		if i >= 5 && i <= 8 {
			body = "compiler error in internal/tools at step " + strconv.Itoa(i)
		}
		batch = append(batch, store.Message{Role: store.RoleAssistant, Content: body})
	}
	_, nextSeq, err := s.AppendMessages(sid, batch)
	require.NoError(t, err)

	summary := "## Active Task\nfix the build\n\n" +
		"## Current State\n- fixed the compiler errors in internal/tools [seq:5-8]\n\n" +
		"## Constraints\n(none)\n\n## Decisions\n(none)\n\n## Open Work\n(none)"

	em := &ctxcompact.EvictionMap{}
	_, err = ctxcompact.Run(context.Background(), compactableHistory(), ctxcompact.PlanOpts{KeepRecent: 1},
		ctxcompact.RunOpts{
			ModelWindow: 8000, ChunkThreshold: 0.9,
			CoveredSeq:  ctxcompact.SeqRef{Lo: 0, Hi: nextSeq - 1},
			EvictionMap: em, DisableQualityGate: true,
		}, &e2eSummarizer{reply: summary}, nil)
	require.NoError(t, err)

	mapText := em.Render(0)
	require.Contains(t, mapText, "[seq:5-8]",
		"the milestone's own span did not survive into the map:\n%s", mapText)

	ht := NewHistoryTools(s)
	out, err := runHistoryTool(t, historyCtx(t, sid), ht.runRead,
		map[string]any{"from_seq": 5, "to_seq": 9, "limit": 100})
	require.NoError(t, err)
	for i := 5; i <= 8; i++ {
		require.Contains(t, out, "compiler error in internal/tools at step "+strconv.Itoa(i),
			"the span the headline cites does not contain what the headline describes")
	}
	require.NotContains(t, out, "routine work 4",
		"the cited span leaked a message outside it; the pointer is wider than the claim")
}

// compactableHistory is a live window Plan will actually split. Its CONTENT is
// irrelevant to these tests — what is under test is the seq bookkeeping, which
// comes from CoveredSeq and the durable log, not from these messages.
func compactableHistory() []*schema.Message {
	return []*schema.Message{
		{Role: schema.User, Content: "start"},
		{Role: schema.Assistant, Content: strings.Repeat("live window filler one ", 200)},
		{Role: schema.Assistant, Content: strings.Repeat("live window filler two ", 200)},
		{Role: schema.Assistant, Content: strings.Repeat("live window filler three ", 200)},
		{Role: schema.User, Content: "continue"},
		{Role: schema.Assistant, Content: "ok"},
	}
}
