package tui

import (
	"time"

	"github.com/x6nux/yanshi/internal/cli"
	"github.com/x6nux/yanshi/internal/guard"
)

// startTurn initializes the live activity status line state for a new in-flight
// turn: resets the elapsed timer, activity text, and glyph animation frame.
// Also stamps lastEventAt so the first reasoning phase of the turn has a
// pre-TTFT anchor for its startedAt (see appendThinkingDelta).
func (m *model) startTurn() {
	m.turnStart = time.Now()
	m.lastEventAt = m.turnStart
	m.activity = "Thinking…"
	m.glyphFrame = 0
	m.retryAttempt = 0
	m.retryErr = ""
	m.selecting = false
	m.assistantContinuation = false
}

// applyStatus updates the session-state fields from a status frame, fills a
// pending /cost or /config status block if one is waiting, and clears the
// activity line on a compaction reply (history was replaced).
func (m *model) applyStatus(ev cli.StreamEvent) {
	m.modelName = ev.Model
	m.thinking = ev.Thinking
	m.tokensIn = ev.TokensIn
	m.tokensOut = ev.TokensOut
	m.turns = ev.Turns
	m.contextWindow = ev.ContextWindow
	// Cached/reasoning token breakdown (Task B8 / T8b): A6 threads these up
	// from schema.TokenUsage through turn→frame→store→StreamEvent. Wiring them
	// here makes both the footer segments and the /cost entry reflect the
	// split — so the user sees cache savings and reasoning spend, not just the
	// aggregate in/out totals.
	m.cachedTokens = ev.CachedTokens
	m.reasoningTokens = ev.ReasoningTokens
	// MEM1: assign even when empty so reconnecting to an Enabled=false backend
	// clears any stale path from the prior status (SC2).
	m.memoryPath = ev.MemoryPath
	// C4: log file path sync so /logs tails the right file.
	m.logPath = ev.LogPath
	// C4: log file path sync so /logs tails the right file. Empty means
	// logs are on stderr — /logs reports that instead of guessing.
	m.logPath = ev.LogPath
	// Session id sync (V09): every status frame carries the current id (empty
	// when recording is off or before first user_message); /fork updates it to
	// the new id and /clear resets to "".
	m.sessionID = ev.SessionID
	// V11: side depth sync from status too (side_state frame also has a direct
	// branch in applyEvent for when it arrives as a standalone control reply).
	if ev.Kind == "status" {
		m.sideDepth = ev.SideDepth
	}
	if ev.PermMode != "" {
		if pm, ok := guard.NormalizeMode(ev.PermMode); ok {
			m.permMode = pm
		}
	}
	// C4 COST1: per-session cumulative USD. costKnown=false renders as N/A,
	// never as $0, so operators can distinguish "missing price" from "zero spend".
	m.costUSD = ev.CostUSD
	m.costKnown = ev.CostKnown

	if m.pendingStatus != nil {
		m.pendingStatus.model = ev.Model
		m.pendingStatus.thinking = ev.Thinking
		m.pendingStatus.tokensIn = ev.TokensIn
		m.pendingStatus.tokensOut = ev.TokensOut
		m.pendingStatus.turns = ev.Turns
		m.pendingStatus.cachedTokens = ev.CachedTokens
		m.pendingStatus.reasoningTokens = ev.ReasoningTokens
		m.pendingStatus.costUSD = ev.CostUSD
		m.pendingStatus.costKnown = ev.CostKnown
		m.pendingStatus = nil
	}
	// bug⑧: compaction status lived on the activity line (set by cmdCompact /
	// the compact_chunk branch). On the status reply the meta-op is over —
	// resume normal "Thinking…" activity so the line doesn't stick on
	// "Compacting context…" forever. The token reduction shows in the footer
	// ctx counter (tokensIn/contextWindow), not in the transcript.
	if ev.Compacted {
		m.activity = "Thinking…"
	}
	// A refused compaction is the one status the user has to see: the context
	// is now larger than it should be and nothing will retry on its own, so the
	// alternative to saying so is a provider length error some turns later that
	// reads like an unrelated failure.
	m.compactionBlocked = ev.CompactionBlocked
}

// lastSessionsEntry returns the most recent *sessionsEntry, or nil if none.
func (m model) lastSessionsEntry() *sessionsEntry {
	for i := len(m.entries) - 1; i >= 0; i-- {
		if se, ok := m.entries[i].(*sessionsEntry); ok {
			return se
		}
	}
	return nil
}

// lastStatsEntry returns the most recent *statsEntry, or nil if none.
func (m model) lastStatsEntry() *statsEntry {
	for i := len(m.entries) - 1; i >= 0; i-- {
		if se, ok := m.entries[i].(*statsEntry); ok {
			return se
		}
	}
	return nil
}

// lastRestoreEntry returns the most recent *restoreEntry, or nil if none.
func (m model) lastRestoreEntry() *restoreEntry {
	for i := len(m.entries) - 1; i >= 0; i-- {
		if re, ok := m.entries[i].(*restoreEntry); ok {
			return re
		}
	}
	return nil
}

func (m *model) flushAssistant() {
	// Reasoning ends as soon as visible content / a tool / the turn end arrives,
	// so collapse any live thinking block before flushing the assistant text.
	m.finalizeLiveThinking()
	if len(m.pending) == 0 {
		return
	}
	ae := assistantEntry{
		text:         m.pending,
		continuation: m.assistantContinuation,
	}
	m.pending = ""
	m.assistantContinuation = true
	// Attach the run of finalized thinking blocks preceding the answer so they
	// render between the "assistant:" label and the content. Detaching the WHOLE
	// trailing run (not just entries[-1]) folds interleaved reasoning phases
	// (thinking → content → thinking → content) into one Thought — otherwise the
	// first phase stays standalone in the transcript (the "two Thought for 0:01"
	// bug). See detachTrailingThoughts.
	m.entries, ae.thought = detachTrailingThoughts(m.entries)
	m.entries = append(m.entries, ae)
}

// detachTrailingThoughts removes the run of finalized thinkingEntries from the
// end of entries, merges them into one thought, and returns the trimmed entries
// + the merged thought (nil when there are none). Shared by flushAssistant
// (attach to the assistant block) and renderBody (fold under the streaming
// assistant label) so interleaved reasoning phases within one answer collapse to
// ONE "Thought for Xs" line instead of one per phase. The merge concatenates
// each phase's text (newline-separated) and spans the earliest startedAt to the
// latest endedAt, so the collapsed header reflects the combined reasoning.
func detachTrailingThoughts(entries []entry) ([]entry, *thinkingEntry) {
	var run []*thinkingEntry
	for len(entries) > 0 {
		te, ok := entries[len(entries)-1].(*thinkingEntry)
		if !ok || te.live {
			break
		}
		run = append(run, te)
		entries = entries[:len(entries)-1]
	}
	if len(run) == 0 {
		return entries, nil
	}
	// run is last→first; reverse to earliest-first so the merge reads in order.
	for i, j := 0, len(run)-1; i < j; i, j = i+1, j-1 {
		run[i], run[j] = run[j], run[i]
	}
	merged := *run[0] // copy so the merge doesn't mutate the detached entry
	for _, te := range run[1:] {
		if merged.text != "" && te.text != "" {
			merged.text += "\n"
		}
		merged.text += te.text
		if te.endedAt.After(merged.endedAt) {
			merged.endedAt = te.endedAt
		}
		if te.startedAt.Before(merged.startedAt) {
			merged.startedAt = te.startedAt
		}
	}
	return entries, &merged
}

// appendThinkingDelta adds a reasoning delta to the last transcript entry when
// it is a live thinkingEntry; otherwise starts a new live thinking block. This
// keeps consecutive reasoning deltas in one block while a later reasoning phase
// (after some content) opens a fresh block.
//
// startedAt uses m.lastEventAt (the moment the previous NON-thinking event
// arrived — tool_result / agent_chunk / submit / etc.) rather than time.Now()
// at the first reasoning delta. In the ReAct loop the model begins reasoning
// immediately after the previous step ends, so lastEventAt ≈ the instant the
// current reasoning phase actually began. This is more accurate than BOTH
// alternatives:
//   - time.Now() at the first delta MISSES the TTFT (time-to-first-token) +
//     reasoning-startup latency the user already waited through between the
//     model being invoked and the first chunk arriving, making "Thought for
//     Xs" read shorter than the perceived wait.
//   - turnStart (the whole turn's start) would include the DURATION OF THE
//     PRECEDING TOOL CALLS in ReAct iterations (thinking → tool → thinking →
//     …), making the SECOND and later thinking blocks report many minutes when
//     the model only reasoned for seconds.
//
// lastEventAt threads this needle: it includes TTFT (set before the model is
// invoked) but excludes cross-tool accumulated time (updated after each tool
// step), so "Thought for 3s" means the user actually waited ~3s for this
// particular reasoning step. When lastEventAt is zero (e.g. a turn whose very
// first delta is reasoning with no preceding non-thinking event), it falls
// back to time.Now() so the block still has a sensible start instant.
func (m *model) appendThinkingDelta(text string) {
	if n := len(m.entries); n > 0 {
		if te, ok := m.entries[n-1].(*thinkingEntry); ok && te.live {
			te.text += text
			return
		}
	}
	start := m.lastEventAt
	if start.IsZero() {
		start = time.Now()
	}
	te := &thinkingEntry{live: true, startedAt: start}
	te.text += text
	m.entries = append(m.entries, te)
}

// discardLastThinking removes the most recent entry when it is a thinkingEntry
// (live or finalized), so a retried generation's reasoning REPLACES (not
// appends to) the failed attempt's partial reasoning. Called on a retry frame
// (overwrite semantics). Earlier thinking blocks from prior ReAct iterations
// are untouched.
//
// Bottom-of-the-cliff guard: if the last entry is an assistantEntry whose
// attached thought is non-nil, clear it too. This happens when the previous
// ReAct iteration's flushAssistant already folded a finalized thinking block
// into the assistant block as ae.thought. Without this clear, the next
// finalizeLiveThinking → flushAssistant cycle could re-attach a STALE timer
// from the old reasoning phase to the new block, producing a misleading
// "Thought for Xs" header from the failed attempt rather than the retry.
func (m *model) discardLastThinking() {
	if n := len(m.entries); n > 0 {
		if _, ok := m.entries[n-1].(*thinkingEntry); ok {
			m.entries = m.entries[:n-1]
			return
		}
		if ae, ok := m.entries[n-1].(assistantEntry); ok && ae.thought != nil {
			ae.thought = nil
			m.entries[n-1] = ae
		}
	}
}

// finalizeLiveThinking closes the most recent live thinkingEntry (if any):
// records endedAt and sets live=false so it collapses to "Thought for Xs".
// Called on the first non-thinking event after reasoning and from flushAssistant.
func (m *model) finalizeLiveThinking() {
	if te := m.lastLiveThinking(); te != nil {
		te.endedAt = time.Now()
		te.live = false
	}
}

// lastLiveThinking returns the most recent live thinkingEntry, or nil if the
// last entry isn't a live thinking block. Only the LAST entry is checked because
// earlier blocks are already finalized (collapsed) and a new reasoning phase
// should open a new block rather than reopen an old one.
func (m model) lastLiveThinking() *thinkingEntry {
	if n := len(m.entries); n > 0 {
		if te, ok := m.entries[n-1].(*thinkingEntry); ok && te.live {
			return te
		}
	}
	return nil
}

// toggleExpand flips expanded on the most recent COLLAPSIBLE block — a
// finalized thinking block (standalone or attached to an assistant entry), a
// resolved tool result (one-line ↔ full), or a long user paste (collapsed ↔
// full text) — collapsing it back on the next press. This is the single
// expand binding (ctrl+o); the earlier bare-"e" binding was removed because it
// shadowed typing "e". Returns whether a block was toggled.
func (m *model) toggleExpand() bool {
	for i := len(m.entries) - 1; i >= 0; i-- {
		switch e := m.entries[i].(type) {
		case assistantEntry:
			if e.thought != nil {
				e.thought.expanded = !e.thought.expanded
				return true
			}
		case *thinkingEntry:
			if !e.live {
				e.expanded = !e.expanded
				return true
			}
		case *toolEntry:
			if e.status != "running" {
				e.expanded = !e.expanded
				return true
			}
		case *userEntry:
			// Only pasted-and-long entries ever collapse, so only they can be
			// expanded/collapsed by ctrl+o (B9/T11). A typed long message is
			// already rendered in full; flipping its expanded flag would be a
			// silent no-op.
			if e.pasted && e.isLong() {
				e.expanded = !e.expanded
				return true
			}
		}
	}
	return false
}

func (m model) anyToolRunning() bool {
	for _, e := range m.entries {
		if t, ok := e.(*toolEntry); ok && t.status == "running" {
			return true
		}
	}
	return false
}

func (m *model) lastRunningTool(name string) *toolEntry {
	for i := len(m.entries) - 1; i >= 0; i-- {
		if t, ok := m.entries[i].(*toolEntry); ok && t.name == name && t.status == "running" {
			return t
		}
	}
	return nil
}

// lastRunningNestedTool returns the most recent running toolEntry flagged
// nested (an agent-class tool whose ReAct loop streams reasoning back through
// the parent). Used by applyEvent's "thinking" branch to attribute deltas to
// the agent block instead of opening a standalone thinkingEntry above the call.
// Only the LAST running nested tool is considered: a parent agent might spawn
// a grandchild, but reasoning streams linearly through the deepest active
// agent, so the most recent running nested entry is the right attribution
// target. Returns nil when no nested tool is running (the common case — caller
// falls back to appendThinkingDelta).
func (m model) lastRunningNestedTool() *toolEntry {
	for i := len(m.entries) - 1; i >= 0; i-- {
		if t, ok := m.entries[i].(*toolEntry); ok && t.status == "running" && t.nested {
			return t
		}
	}
	return nil
}

// lastNestedTool returns the most recent toolEntry flagged nested, regardless of
// status (running, ok, or error). Used by the nested_usage case to attribute
// the sub-agent's total tokens when the Analysis block has ALREADY resolved
// (lastRunningNestedTool returns nil once status flips to ok/error) — the
// nested_usage frame is emitted by runSubAgentTurn AFTER the child's event
// stream drains, which normally lands while Analysis is still running, but a
// reordering (emit flush timing) must not lose the tokens. Only the LAST nested
// entry is considered so the tokens land on the correct (most recent) Analysis
// block, not an earlier sibling.
func (m model) lastNestedTool() *toolEntry {
	for i := len(m.entries) - 1; i >= 0; i-- {
		if t, ok := m.entries[i].(*toolEntry); ok && t.nested {
			return t
		}
	}
	return nil
}
