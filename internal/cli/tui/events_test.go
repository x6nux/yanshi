package tui

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/cli"
)

// TestThinkingStartedAtUsesLastEventAt proves the thinking block's startedAt is
// taken from m.lastEventAt (the moment the previous NON-thinking event arrived),
// NOT from time.Now() at the first reasoning delta. The ReAct loop fires a
// non-thinking event (tool_result / agent_chunk / turn start) the instant the
// previous step ends, and the model begins reasoning right after — so
// lastEventAt ≈ the moment the current reasoning phase actually began. Using
// time.Now() at the first delta would miss the TTFT + reasoning-startup latency
// (the time between the model being invoked and the first chunk arriving),
// making "Thought for Xs" read shorter than what the user actually waited.
//
//   tool_result arrives ──┬── lastEventAt captured
//                         │  (model invoked, starts reasoning)
//                         │  ... TTFT + reasoning startup ...
//                         │  first thinking delta arrives
//                         └── startedAt should point to lastEventAt (above),
//                             not time.Now() here.
//
// This must be strictly ≤ before (the time captured just before sleeping),
// because startedAt points to lastEventAt which predates the sleep.
func TestThinkingStartedAtUsesLastEventAt(t *testing.T) {
	m := newModel(&fakeSession{}, "/proj")
	m = m.applyEvent(cli.StreamEvent{
		Kind:       "tool_call",
		ToolName:   "fs_read",
		ToolStatus: "running",
	})

	// A non-thinking event (tool_result) updates lastEventAt. The model begins
	// reasoning immediately after, so lastEventAt ≈ the reasoning start.
	m = m.applyEvent(cli.StreamEvent{
		Kind:       "tool_result",
		ToolName:   "fs_read",
		Text:       "r",
		ToolStatus: "ok",
	})

	before := time.Now()
	time.Sleep(20 * time.Millisecond)

	// First reasoning delta opens a new live thinkingEntry. Its startedAt must
	// point at lastEventAt (≤ before), not time.Now() at this delta.
	m = m.applyEvent(cli.StreamEvent{Kind: "thinking", Text: "hmm"})

	te := m.lastLiveThinking()
	require.NotNil(t, te, "expected a live thinkingEntry")
	if te.startedAt.After(before) {
		t.Fatalf("startedAt 应取 lastEventAt（≤before），实际 %v > %v（用了 time.Now()）",
			te.startedAt, before)
	}
}

// TestThinkingStartedAtFallsBackToNowWhenNoLastEventAt proves that when no
// non-thinking event has been seen yet (lastEventAt is zero — e.g. a turn whose
// very first delta is reasoning), startedAt falls back to time.Now() so the
// block still has a sensible start instant rather than the zero time.
func TestThinkingStartedAtFallsBackToNowWhenNoLastEventAt(t *testing.T) {
	m := newModel(&fakeSession{}, "/proj")
	before := time.Now()
	m = m.applyEvent(cli.StreamEvent{Kind: "thinking", Text: "first"})
	after := time.Now()

	te := m.lastLiveThinking()
	require.NotNil(t, te)
	assert.False(t, te.startedAt.Before(before),
		"startedAt should fall back to ~time.Now() when lastEventAt is zero")
	assert.False(t, te.startedAt.After(after),
		"startedAt should fall back to ~time.Now() when lastEventAt is zero")
}

// TestDiscardLastThinkingClearsAttachedThought proves discardLastThinking also
// drops the thought attached to the last assistantEntry, not just a standalone
// thinkingEntry. This is the bottom-of-the-cliff guard for retry semantics: a
// retry frame clears the partial output from the failed attempt so the
// regenerated stream can replace it. If the previous turn's assistant block
// kept its attached thought, the next finalizeLiveThinking → flushAssistant
// cycle could attach a STALE (already-finalized) timer to the new block,
// producing a misleading "Thought for Xs" header from the old reasoning phase.
func TestDiscardLastThinkingClearsAttachedThought(t *testing.T) {
	m := newModel(&fakeSession{}, "/proj")

	// Reasoning then content → done flushes the assistant block with the
	// finalized reasoning attached as ae.thought.
	m = m.applyEvent(cli.StreamEvent{Kind: "thinking", Text: "old reasoning"})
	m = m.applyEvent(cli.StreamEvent{Kind: "agent_chunk", Text: "old answer"})
	m = m.applyEvent(cli.StreamEvent{Kind: "done"})

	// Find the assistant entry; it should have a non-nil thought.
	var ae *assistantEntry
	for i := len(m.entries) - 1; i >= 0; i-- {
		if x, ok := m.entries[i].(assistantEntry); ok {
			ae = &x
			break
		}
	}
	require.NotNil(t, ae, "expected an assistantEntry")
	require.NotNil(t, ae.thought, "assistant block has the finalized thought attached")

	// A retry fires discardLastThinking; the attached thought must be cleared
	// so the retried generation can attach a fresh timer.
	m.discardLastThinking()

	// Re-read the last entry: assistantEntry.thought must now be nil.
	if n := len(m.entries); n > 0 {
		if last, ok := m.entries[n-1].(assistantEntry); ok {
			assert.Nil(t, last.thought,
				"discardLastThinking must clear attached thought on retry")
		}
	}
}

// TestThinking_InterleavedPhasesFoldIntoOne guards the "two ✻ Thought for 0:01"
// bug: when a model interleaves reasoning with content WITHIN one answer
// (thinking → agent_chunk → thinking → agent_chunk, no tool call in between),
// the two reasoning phases must fold into ONE Thought block attached to the
// assistant — not leave the first as a standalone "Thought for" line.
//
// Root cause this guards: appendThinkingDelta opens a NEW thinkingEntry when the
// last entry isn't a LIVE thinkingEntry, and the first phase finalizes between
// the two content chunks. Both renderBody's fold and flushAssistant's attach
// only looked at entries[-1], so the earlier finalized phase stayed standalone
// in the transcript — producing:
//
//	you: hi
//	✻ Thought for 0:01        ← first phase, orphaned in transcript
//	assistant:
//	✻ Thought for 0:01        ← second phase, attached
//
// i.e. two Thought lines for what the user perceives as one turn of reasoning.
func TestThinking_InterleavedPhasesFoldIntoOne(t *testing.T) {
	m := newModel(&fakeSession{}, "/proj")
	m = m.applyEvent(cli.StreamEvent{Kind: "thinking", Text: "reason 1"})
	m = m.applyEvent(cli.StreamEvent{Kind: "agent_chunk", Text: "answer"})
	m = m.applyEvent(cli.StreamEvent{Kind: "thinking", Text: "reason 2"})
	m = m.applyEvent(cli.StreamEvent{Kind: "agent_chunk", Text: " more"})
	m = m.applyEvent(cli.StreamEvent{Kind: "done"})

	// After the turn, NO standalone finalized thinkingEntry may remain — both
	// reasoning phases must fold into the assistant block as ae.thought.
	var standalone int
	var lastAssistant assistantEntry
	var hasAssistant bool
	for _, e := range m.entries {
		if te, ok := e.(*thinkingEntry); ok && !te.live {
			standalone++
		}
		if ae, ok := e.(assistantEntry); ok {
			lastAssistant = ae
			hasAssistant = true
		}
	}
	assert.Equal(t, 0, standalone,
		"interleaved reasoning phases must fold into the assistant block, not stay as standalone Thought lines")
	require.True(t, hasAssistant, "turn must produce an assistant block")
	require.NotNil(t, lastAssistant.thought, "assistant block must carry the folded thought")
}
