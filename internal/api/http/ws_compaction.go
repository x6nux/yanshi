// internal/api/http/ws_compaction.go
//
// Compaction, billing, token/elapsed formatting, and session persistence
// utilities. Split from ws.go to keep that file under the 1000 pure-code-line
// cap. Contains token/elapsed formatters, usage billing, billing reset/meta,
// feature-flag helpers, statusFrame, model selection, session persistence
// (ensureSession/persistMessages/loadSession), and auto/manual compaction.

package http

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/x6nux/yanshi/internal/agent/orchestrator"
	"github.com/x6nux/yanshi/internal/approval"
	"github.com/x6nux/yanshi/internal/ctxcompact"
	"github.com/x6nux/yanshi/internal/features"
	einollm "github.com/x6nux/yanshi/internal/llm/eino"
	otelinstr "github.com/x6nux/yanshi/internal/observe/otel"
	"github.com/x6nux/yanshi/internal/proto"
	"github.com/x6nux/yanshi/internal/store"
)

// formatTokenCount renders a token count as e.g. "77.7k" at/above 1000, plain
// decimal below — matching the "X.Xk tokens" shape the summary line uses.
func formatTokenCount(n int) string {
	if n >= 1000 {
		return fmt.Sprintf("%.1fk", float64(n)/1000)
	}
	return strconv.Itoa(n)
}

// formatElapsed renders a duration as "1h 59m 59s", dropping leading zero
// fields for short turns ("5m 3s", "42s").
func formatElapsed(d time.Duration) string {
	d = d.Round(time.Second)
	h := int(d / time.Hour)
	m := int(d/time.Minute) % 60
	s := int(d/time.Second) % 60
	switch {
	case h > 0:
		return fmt.Sprintf("%dh %dm %ds", h, m, s)
	case m > 0:
		return fmt.Sprintf("%dm %ds", m, s)
	default:
		return fmt.Sprintf("%ds", s)
	}
}

// scopeJSON renders an approval.Scope to a stable JSON string for transport
// (PermissionInfo.Scope is a string, not a nested object, so clients do not
// need to import the approval package to consume it). Returns "{}" on marshal
// failure as a fail-safe — the Scope struct's fields are all primitives so
// json.Marshal should never fail in practice, but a future field addition must
// never crash the audit pump.
func scopeJSON(scope approval.Scope) string {
	b, err := json.Marshal(scope)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// usageForPricing maps orchestrator.TurnUsage to the einollm.Usage leaf type.
// ws.go cannot collapse this into one struct because orchestrator must not
// import einollm (and vice versa) — the WS handler is the bridge.
func usageForPricing(u orchestrator.TurnUsage) einollm.Usage {
	return einollm.Usage{
		Prompt:     u.PromptTokens,
		Cached:     u.CachedTokens,
		Completion: u.CompletionTokens,
		Reasoning:  u.ReasoningTokens,
	}
}

// addProviderUsage folds ONE provider usage event into the session ledger and
// updates the running cost/known state. Skipped for all-zero usages (some
// providers emit a final empty usage on the last chunk). Constraint 9 (C4
// plan): every provider usage is billed exactly once; the streaming-chunk
// running total is only ever read once (here, from onUsage) so we never double
// count the "API reports cumulative" totals.
func (cs *connSession) addProviderUsage(ctx context.Context, s *Server, u orchestrator.TurnUsage) {
	priced := usageForPricing(u)
	if priced.Prompt <= 0 && priced.Cached <= 0 && priced.Completion <= 0 {
		return
	}
	cs.billing.Add(priced)
	// The ctx parameter existed and was discarded (`_ context.Context`), which
	// is why otel.RecordUsage had zero production callers and the
	// yanshi.llm.tokens counter never emitted: "tokens are observable" was
	// true of the instrument and false of the system.
	//
	// Here specifically, because this is the one place with the
	// exactly-once guarantee. Recording at the streaming callback instead
	// would double-count providers that report cumulative totals per chunk --
	// the same constraint this function's doc already states for billing.
	usageRecorder(ctx, cs.displayModel(), priced.Prompt, priced.Cached, priced.Completion, u.ReasoningTokens)
	cost, known := einollm.CostOK(s.priceTab, cs.displayModel(), priced)
	if !cs.hasBilledUsage {
		// Seed the known state from the first usage (don't AND-fold against
		// a default true or a single unknown model would silently stay "known").
		cs.costKnown = known
		cs.hasBilledUsage = true
	} else {
		// Once ANY usage is unknown the whole session is N/A — operator can't
		// tell which turn was the culprit from "$0.42" so we surface the loss
		// of confidence.
		cs.costKnown = cs.costKnown && known
	}
	if known {
		cs.costUSD += cost
	}
}

// resetBilling clears the session ledger. Called on session creation, clear,
// and side-exit so a fresh turn starts from a clean spend state. The post-
// reset known state is "is the current display model in the price table" so a
// turn on a brand-new model doesn't briefly render the legacy cost.
func (cs *connSession) resetBilling(s *Server) {
	cs.billing = einollm.Ledger{}
	cs.costUSD = 0
	cs.hasBilledUsage = false
	_, cs.costKnown = s.priceTab[cs.displayModel()]
}

// billingMeta snapshots the ledger into the persistence DTO. Called once per
// turn-end so the DB row's billed columns reflect the cumulative spend.
func (cs *connSession) billingMeta() store.BillingMeta {
	return store.BillingMeta{
		InputTokens:  cs.billing.Billed.InputTokens,
		CachedTokens: cs.billing.Billed.CachedTokens,
		OutputTokens: cs.billing.Billed.OutputTokens,
		CostUSD:      cs.costUSD,
		CostKnown:    cs.costKnown,
	}
}

// featureRows projects the registry's List() into the proto wire shape. nil
// registry → nil rows so the renderer can detect "no registry configured"
// rather than rendering an empty table as if the registry were live but empty.
func featureRows(reg *features.Registry) []proto.FeatureRow {
	if reg == nil {
		return nil
	}
	listed := reg.List()
	rows := make([]proto.FeatureRow, 0, len(listed))
	for _, row := range listed {
		rows = append(rows, proto.FeatureRow{
			Key:     row.Key,
			Stage:   row.Stage,
			Enabled: row.Enabled,
			Owner:   row.Owner,
		})
	}
	return rows
}

// setFeature validates and applies one /features_set payload. Rejects: no
// registry, missing key, missing Enabled (*bool — nil means the client sent a
// payload without the field, which is a wire contract violation). Returns the
// underlying registry error so the caller can echo it verbatim.
func setFeature(reg *features.Registry, payload proto.FeaturesSetPayload) error {
	if reg == nil {
		return errors.New("feature registry is unavailable")
	}
	if payload.Key == "" {
		return errors.New("feature key is required")
	}
	if payload.Enabled == nil {
		return errors.New("feature enabled value is required")
	}
	return reg.Set(payload.Key, *payload.Enabled)
}

// statusFrame snapshots the session as a status ServerFrame, including the
// compaction context-window budget so the client can render "ctx: <in>/<window>"
// and the permission mode so the footer reflects it. It also carries the
// server-side session id so a headless client (yanshi exec) can print it for
// --resume: SessionID is created lazily on the first user_message (ensureSession)
// and was previously only surfaced on session_restored.
func (cs *connSession) statusFrame(s *Server) proto.ServerFrame {
	mode := cs.perm.get()
	st := proto.NewStatusWithMode(cs.displayModel(), cs.thinking, cs.tokensIn, cs.tokensOut, cs.turns,
		contextWindowFor(cs.model, s.compaction), string(mode))
	// CachedTokens / ReasoningTokens are populated after construction so
	// NewStatusWithMode's signature (and its many callers) stay unchanged. The
	// omitempty JSON tag drops them when zero (pre-record / non-reporting model).
	st.CachedTokens = cs.cachedTokens
	st.ReasoningTokens = cs.reasoningTokens
	// SessionID: empty when recording is disabled (store == nil) or before the
	// first user_message; omitempty drops it on the wire so legacy clients and
	// the no-store path are unchanged. Headless exec reads it to print the id.
	st.SessionID = cs.sessionID
	// ADR-0015 constraint 6: a refused compaction has to be visible on every
	// status frame, not just logged once. It stays set until a compaction
	// succeeds, because the condition persists — the window keeps growing.
	st.CompactionBlocked = cs.compactionBlocked
	// B2-RB1: carry both the display short hash and the FULL main_head id. The
	// TUI caches Head and re-sends it as the next restore_turn's ConfirmedHead
	// (D6: full id binding — short-hash collision is a real risk across long
	// histories).
	st.CommitShort = s.shortHead()
	st.Head = s.fullHead()
	// MEM1: surface the memory path so the TUI can display it.
	st.MemoryPath = s.memoryPath
	// C4: surface the log file path so the TUI /logs command tails the
	// right file (empty when logs go to stderr — /logs then reports that).
	st.LogPath = s.logPath
	// V11: surface the current side-conversation depth so the footer can render
	// "in side (N)".
	st.SideDepth = len(cs.sideStack)
	// C4 COST1: per-session cumulative USD estimate. costKnown=false is the
	// N/A signal — the TUI renders "N/A" rather than "$0.0000" so operators
	// can distinguish "unknown model" from "known model, zero spend".
	//
	// observe.cost_in_status (OBS3) gates it. Off leaves both fields zero,
	// which omitempty drops from the wire and the TUI renders as N/A — the
	// honest reading of "cost reporting is switched off". EnabledOrDefault
	// because this flag defaults ON and s.featuresReg may be nil.
	if s.featuresReg.EnabledOrDefault("observe.cost_in_status") {
		st.CostUSD = cs.costUSD
		st.CostKnown = cs.costKnown
	}
	return st
}

// displayModel returns the model name to show in a status frame: the explicitly
// selected model when set, else the defaultModel fallback (so the footer always
// shows the active model even before the user picks one).
func (cs *connSession) displayModel() string {
	if cs.model != "" {
		return cs.model
	}
	return cs.defaultModel
}

// selectModel returns the per-turn model for the session (nil when none selected
// or the name is absent from the registry — e.g. nil map under FakeModel).
func (cs *connSession) selectModel(models map[string]model.BaseChatModel) model.BaseChatModel {
	if cs.model == "" {
		return nil
	}
	return models[cs.model] // nil when map is nil or name absent
}

// ensureSession creates a DB session on first use. No-op when the store is nil
// (recording disabled) or the session already exists. The title is set from the
// first user message text (truncated).
func (cs *connSession) ensureSession(s *Server, firstMsg string) {
	if s.store == nil || cs.sessionID != "" || cs.recordingSuppressed() {
		return
	}
	title := firstMsg
	if len(title) > 80 {
		title = title[:80]
	}
	id, err := s.store.CreateSession(title)
	if err != nil {
		// Recording is best-effort: log and continue.
		return
	}
	cs.sessionID = id
	cs.seq = 0
}

// storeMessagesFor converts a live context window into durable-log rows.
//
// The mapping is deliberately lossy in one direction only: everything the model
// SAW becomes a row, and nothing that was never in the window is invented. An
// assistant message carrying tool calls yields its prose row (when it has prose)
// plus one store.RoleToolCall row per call; a tool message yields one
// store.RoleToolResult row linked by ToolCallID. The system message is skipped —
// it is regenerated from the prompt on every turn, so persisting it would store
// a copy of configuration as if it were conversation.
//
// Empty prose with no tool calls yields nothing rather than an empty row.
func storeMessagesFor(hist []*schema.Message) []store.Message {
	out := make([]store.Message, 0, len(hist))
	for _, m := range hist {
		if m == nil || m.Role == schema.System {
			continue
		}
		switch m.Role {
		case schema.Tool:
			out = append(out, store.Message{
				Role:       store.RoleToolResult,
				Content:    m.Content,
				ToolCallID: m.ToolCallID,
				ToolName:   m.ToolName,
			})
		case schema.Assistant:
			if m.Content != "" {
				out = append(out, store.Message{
					Role:    store.RoleAssistant,
					Content: m.Content,
				})
			}
			for _, tc := range m.ToolCalls {
				out = append(out, store.Message{
					Role:       store.RoleToolCall,
					ToolCallID: tc.ID,
					ToolName:   tc.Function.Name,
					ToolArgs:   tc.Function.Arguments,
				})
			}
		default:
			if m.Content == "" {
				continue
			}
			out = append(out, store.Message{
				Role:    store.RoleUser,
				Content: m.Content,
			})
		}
	}
	return out
}

// flushHistory persists the ENTIRE live window to the durable log and reports
// whether it is now safe to discard any of it.
//
// Returns true when the window is durable — including the cases where there is
// nothing to persist (recording disabled, side conversation, empty window),
// because "nothing was lost" is the same answer for the caller as "everything
// was saved".
//
// Flushing the whole window rather than a delta is what makes this correct
// without a watermark: store.AppendMessages deduplicates on a per-session key,
// so rows already durable are skipped and only genuinely new messages are
// inserted. A watermark would have to track a live slice that compaction
// rewrites underneath it — which is exactly the bookkeeping that gets a message
// dropped.
func (cs *connSession) flushHistory(s *Server) bool {
	if s.store == nil || cs.sessionID == "" || cs.recordingSuppressed() {
		return true
	}
	rows := storeMessagesFor(cs.history)
	if len(rows) == 0 {
		return true
	}
	_, next, err := s.store.AppendMessages(cs.sessionID, rows)
	if err != nil {
		slog.Warn("history flush failed; context will not be evicted",
			"session", cs.sessionID, "messages", len(rows), "error", err)
		return false
	}
	cs.seq = next
	return true
}

// windowBoundary splits the compacted window's row positions into ADR-0015's
// two fields: where the contiguous kept tail starts, and the scattered
// survivors below it.
//
// kept is every seq the compacted window occupies, logTop is the log's next
// free seq after the post-compaction flush, and flushedFrom is where that flush
// began writing. The result reproduces `kept` EXACTLY — the projection selects
// `seq >= hidden OR seq IN pinned`, every seq in [hidden, logTop) is kept by
// construction of the walk, and every kept seq below hidden is listed — which
// is what ADR-0015's fifth constraint demands. No message Plan pinned is lost
// and no evicted message is readmitted, in either direction.
//
// It walks DOWN FROM THE LOG TOP over real row positions rather than counting
// backwards through the window's messages. The earlier version did the latter,
// assuming the window's trailing messages occupy the log's trailing rows, and
// that assumption is false: AppendMessages identifies rows by dedup key, so a
// window containing byte-identical duplicates has its tail messages resolve to
// EARLIER rows and the log's real tail is something else entirely. Measured on
// a session compacted twice — the count-backwards boundary readmitted the first
// compaction's summary, a message the second compaction had evicted.
//
// The clamp to flushedFrom is the fail-safe. Rows at or above it were written by
// the flush that just ran (the summary, and C3's eviction map), so they are in
// the window whether or not the lookup that produced `kept` found them. Without
// it a lookup that resolved nothing would place hidden at the log top and
// project an EMPTY window — an agent that has forgotten the conversation and
// says nothing about it.
func windowBoundary(kept []int, logTop, flushedFrom int) (hidden int, pinned []int) {
	set := make(map[int]bool, len(kept))
	for _, s := range kept {
		set[s] = true
	}
	hidden = logTop
	for hidden > 0 && set[hidden-1] {
		hidden--
	}
	if hidden > flushedFrom {
		hidden = flushedFrom
	}
	if hidden < 0 {
		hidden = 0
	}
	for _, s := range kept {
		if s < hidden {
			pinned = append(pinned, s)
		}
	}
	return hidden, pinned
}

// reportCompactionBlocked records a refused compaction and tells the client
// immediately, so the oversized context is observable at the moment it is
// decided rather than several turns later as a provider length error.
//
// The flag persists on the session (statusFrame re-sends it) because the
// CONDITION persists: nothing retries a refusal, so the window keeps growing
// until whatever caused it changes. A one-shot frame would be the same silence
// with an extra step.
func (cs *connSession) reportCompactionBlocked(s *Server, conn *wsConn, why string) {
	cs.compactionBlocked = why
	conn.write(cs.statusFrame(s))
}

// compactionNotDurable is the message the manual path shows when a compaction
// could not be made durable. Shared by the two failure branches so a change to
// the wording cannot describe only one of them.
const compactionNotDurable = "compaction skipped: the conversation could not be saved, " +
	"so nothing was dropped from the context"

// compactionNotAligned is shown when the live window and the durable log stopped
// agreeing. Distinct from compactionNotDurable because the cause and the operator's
// next step differ: nothing failed to write, the two views diverged, and the
// honest report is that the context is larger than it should be rather than that
// the disk is broken.
const compactionNotAligned = "compaction skipped: the conversation and its saved copy " +
	"no longer line up, so nothing was dropped from the context"

// commitCompaction installs the compacted window, makes it durable, and records
// the boundary that lets a later restore rebuild it (INF3 / ADR-0015).
//
// The order is load-bearing in both directions. The summary is flushed BEFORE
// the boundary is computed, because the boundary is a log coordinate: it is the
// seq the summary block lands at, minus the rows behind the window's kept tail.
// And the boundary is recorded BEFORE the caller reports success, because a
// compaction with no marker is strictly worse than no compaction — the next
// restore pulls back everything that was just evicted, i.e. exactly the bug
// this exists to fix, except now with a summary already paid for.
//
// Any failure puts the previous window back rather than leaving a half-applied
// one. The originals were flushed by the caller before compaction ran, so
// nothing is lost either way; what the rollback avoids is a live window whose
// durable boundary describes something else.
func (cs *connSession) commitCompaction(s *Server, newHist []*schema.Message) (bool, string) {
	oldHist := cs.history
	// cs.seq is the next free row in the durable log, i.e. where the summary
	// (and C3's eviction map) are about to land.
	boundary := cs.seq
	recording := s.store != nil && cs.sessionID != "" && !cs.recordingSuppressed()

	// Locate the OLD window's rows BEFORE the summary is written. At this moment
	// the projection is exactly the live window — the caller flushed it before
	// compacting, and the window is by construction the previous boundary plus
	// everything appended since — so row i of the window is row i of this slice,
	// and POSITION rather than content decides which log row each message
	// occupies. See keptWindowSeqs for why that distinction is the whole point.
	var oldRows []store.Message
	if recording {
		var err error
		oldRows, err = s.store.ProjectWindow(cs.sessionID)
		if err != nil {
			slog.Warn("compaction boundary not recorded; context will not be evicted",
				"session", cs.sessionID, "error", err)
			return false, compactionNotDurable
		}
		// Constraint 6: checked here, before a single message is evicted, so a
		// refusal costs nothing that was not already spent and leaves the window
		// untouched rather than half-applied.
		if !alignedWithLog(oldHist, oldRows) {
			return false, compactionNotAligned
		}
	}

	cs.history = newHist
	if !cs.flushHistory(s) {
		cs.history = oldHist
		return false, compactionNotDurable
	}
	if !recording {
		// The same short circuit flushHistory applies: nothing was persisted,
		// so there is no boundary to record and nothing to restore from.
		return true, ""
	}
	kept, ok := keptWindowSeqs(oldHist, newHist, oldRows)
	if !ok {
		slog.Warn("compaction window stopped matching the log mid-commit; boundary not recorded",
			"session", cs.sessionID,
			"action", "the context stays oversized but complete")
		cs.history = oldHist
		return false, compactionNotAligned
	}
	// The rows the flush just wrote (the summary, and C3's eviction map) are in
	// the window too. Adding them lets windowBoundary collapse the contiguous
	// tail into the range half instead of listing every survivor as a pin.
	for seq := boundary; seq < cs.seq; seq++ {
		kept = append(kept, seq)
	}
	hidden, pinned := windowBoundary(kept, cs.seq, boundary)
	if err := s.store.AppendContextEvent(cs.sessionID, store.ContextEventCompact, hidden, pinned); err != nil {
		slog.Warn("compaction boundary not recorded; context will not be evicted",
			"session", cs.sessionID, "hidden_seq", hidden, "error", err)
		cs.history = oldHist
		return false, compactionNotDurable
	}
	return true, ""
}

// alignedWithLog reports whether the live window still maps one-to-one onto the
// rows the projection returned, which is the cross-layer invariant every
// boundary calculation here rests on: THE PRE-FLUSH PROJECTION IS THE ACTIVE
// WINDOW.
//
// That invariant is not guaranteed, and assuming it was is the mistake this
// function exists to stop repeating. It can be broken by an ordinary
// conversation: flushHistory de-duplicates against the WHOLE log including rows
// already hidden behind a boundary, so a model that repeats a sentence
// byte-identical to a hidden one has that sentence silently dropped by ON
// CONFLICT and never written. Measured on the real WS path: 6 window rows, 5
// projected.
//
// A FAILURE HERE MUST REFUSE THE COMPACTION (ADR-0015 constraint 6). The
// caller therefore checks this BEFORE it evicts anything, so refusing costs
// only the summary call that was already spent, and the window is left oversized
// but complete — the same direction C1 chose for a failed flush. The previous
// version instead carried on and wrote a boundary with no pins, whose measured
// effect was NOT the "keeps its recent tail" this comment used to claim: with no
// kept seqs, windowBoundary clamps to the start of the post-compaction flush and
// the restored window is the SUMMARY ALONE. A five-message window came back as
// one. Guessing a boundary is worse than not compacting, every time.
//
// Content is deliberately not compared, only role and the tool identifiers.
// Those three are stored verbatim while content is redacted on write, so
// comparing text would fail on every session that ever carried a secret and
// refuse all of their compactions.
func alignedWithLog(oldHist []*schema.Message, oldRows []store.Message) bool {
	want := storeMessagesFor(oldHist)
	if len(want) != len(oldRows) {
		slog.Warn("live window does not match the durable log; refusing to compact",
			"window_rows", len(want), "log_rows", len(oldRows),
			"action", "the context stays oversized but complete; a message the model "+
				"repeated verbatim from evicted history is the known cause")
		return false
	}
	for i := range want {
		if want[i].Role != oldRows[i].Role ||
			want[i].ToolCallID != oldRows[i].ToolCallID ||
			want[i].ToolName != oldRows[i].ToolName {
			slog.Warn("live window does not match the durable log; refusing to compact",
				"row", i, "action", "the context stays oversized but complete")
			return false
		}
	}
	return true
}

// keptWindowSeqs maps the compacted window back onto the durable log rows it
// occupies, BY POSITION.
//
// The previous implementation asked the log "which row has this content", via
// the dedup key. That is unsound, and it was measured to be: a dedup key's only
// discriminator between byte-identical siblings is their ordinal within the
// flushed batch, and compaction is precisely the operation that changes the
// batch. Flush [X, "ok", Y, "ok"] and the two get keys ok#0 and ok#1; compact
// down to ["ok"(the second one), Z] and the survivor is now ordinal 0, collides
// with the FIRST "ok", and resolves to its seq. The window then projects back in
// the wrong ORDER — and in the shape a review constructed, a tool_result landed
// ahead of its tool_call, which providers reject outright rather than degrade.
// Returning per-row seqs from AppendMessages would not have helped: the seq it
// returns is the aliased one.
//
// Position has no such ambiguity. oldRows is the projection taken before the
// post-compaction flush, which IS the live window row for row, so the i-th row
// of storeMessagesFor(oldHist) lives at oldRows[i].Seq whatever its content.
//
// WHICH messages survived is answered by pointer identity: ctxcompact.Assemble
// appends the very *schema.Message values it was handed. The one exception is
// measured and handled — ctxcompact.FoldToolResults runs after Assemble and
// REPLACES the tool results it rewrites (out[i] = folded), so a folded result
// fails the identity test. completeToolPairs puts its row back, which is also
// what keeps ADR-0015 constraint 5(b) true no matter how this set was derived.
//
// Alignment is CHECKED SEPARATELY AND FIRST, by alignedWithLog, because a
// failure there is a refusal rather than a degradation. See its doc.
//
// The second return is that check made structural. Indexing oldRows by a
// position derived from oldHist PANICS when the two disagree — measured, with a
// misaligned window this function ran off the end of the slice and took the WS
// connection down with it — so it reports failure rather than trusting a caller
// to have checked. Returning "no pins" instead would be the wrong direction:
// windowBoundary then clamps to the post-compaction flush and the restored
// window is the summary alone.
func keptWindowSeqs(oldHist, newHist []*schema.Message, oldRows []store.Message) ([]int, bool) {
	survived := make(map[*schema.Message]bool, len(newHist))
	for _, m := range newHist {
		survived[m] = true
	}
	var kept []int
	row := 0
	for _, m := range oldHist {
		n := len(storeMessagesFor([]*schema.Message{m}))
		if row+n > len(oldRows) {
			return nil, false
		}
		if m != nil && survived[m] {
			for k := 0; k < n; k++ {
				kept = append(kept, oldRows[row+k].Seq)
			}
		}
		row += n
	}
	if row != len(oldRows) {
		return nil, false
	}
	return completeToolPairs(kept, oldRows), true
}

// completeToolPairs enforces ADR-0015 constraint 5(b) on the boundary itself: a
// tool result and its call are both in the window or the pairing is broken.
//
// It ADDS the missing partner rather than dropping the survivor. Dropping would
// cost the model a tool result it is actively working with; adding costs a few
// tokens. The case it exists for is a folded tool result, whose durable row
// still holds the unfolded text — restoring more detail than the live window had
// is the harmless direction.
//
// Rows with an empty tool_call_id are left alone: they cannot be paired up
// unambiguously, and guessing would be how an orphan gets manufactured rather
// than avoided.
func completeToolPairs(kept []int, rows []store.Message) []int {
	if len(kept) == 0 {
		return kept
	}
	in := make(map[int]bool, len(kept))
	for _, s := range kept {
		in[s] = true
	}
	partners := map[string][]int{}
	for _, r := range rows {
		if r.ToolCallID == "" {
			continue
		}
		if r.Role == store.RoleToolCall || r.Role == store.RoleToolResult {
			partners[r.ToolCallID] = append(partners[r.ToolCallID], r.Seq)
		}
	}
	for _, r := range rows {
		if !in[r.Seq] || r.ToolCallID == "" {
			continue
		}
		for _, seq := range partners[r.ToolCallID] {
			if !in[seq] {
				in[seq] = true
				kept = append(kept, seq)
			}
		}
	}
	sort.Ints(kept)
	return kept
}

// persistMessages makes the completed turn durable.
//
// It takes NO message arguments any more. It used to take the user text and the
// assistant text and write exactly those two rows, which is why every tool call
// and every tool result a turn produced — the test output, the diffs, the
// compiler errors, i.e. most of what the turn actually learned — was never
// written anywhere. Compaction then removed them from the window, and they were
// gone for good.
//
// The turn's messages are already in cs.history by the time this runs, so
// flushing the window IS persisting the turn, and it captures the tool rows the
// two-argument form could not see.
func (cs *connSession) persistMessages(s *Server) {
	cs.flushHistory(s)
}

// loadSession populates the connSession from a stored session. It loads the
// message history, model config, and token counters from the DB. Returns a
// non-nil error message for the reply frame when the session is not found.
func (cs *connSession) loadSession(s *Server, sessionID string) error {
	if s.store == nil {
		return nil // no store = no recording, silently ignore
	}
	// Verify the session exists.
	ss, err := s.store.GetSession(sessionID)
	if err != nil {
		return err
	}
	if ss == nil {
		return nil
	}

	// Load the WINDOW, not the transcript (INF3 / ADR-0015). Messages() returns
	// every row the session ever wrote, so restoring from it re-expands the
	// originals a compaction already replaced with a summary — and the summary
	// comes back too, leaving the window larger than it was before compacting.
	// ProjectWindow folds the context-event log first and reads only what
	// survives; a session that never compacted has no events and runs the exact
	// query this line used to.
	window, err := s.store.ProjectWindow(sessionID)
	if err != nil {
		return err
	}
	// Reuse the same snapshot mapper that WS restore uses
	// (applySessionRevertSnapshot in ws_seam.go), so reconnect + undo restore
	// share one role/meta mapping (B2-RB1).
	applySessionRevertSnapshot(cs, store.SessionRevertSnapshot{
		Meta:     *ss,
		Messages: window,
	})
	// applySessionRevertSnapshot derives cs.seq from the slice it is handed,
	// which is the durable watermark only when that slice IS the whole log. The
	// projection is a suffix, so take the watermark from the row count instead —
	// the same value the pre-projection code produced. It matters because
	// commitCompaction reads cs.seq as a log coordinate.
	total, err := s.store.SessionMessageCount(sessionID)
	if err != nil {
		return err
	}
	cs.seq = total
	// COST1: restore the in-memory ledger from the DB row so post-reconnect
	// turns continue accumulating from the prior spend instead of resetting
	// to zero. hasBilledUsage is seeded from the non-zero check so the
	// NEXT usage AND-folds against a real state rather than a default.
	cs.billing.Billed = einollm.BilledUsage{
		InputTokens:  ss.BilledInputTokens,
		CachedTokens: ss.BilledCachedTokens,
		OutputTokens: ss.BilledOutputTokens,
	}
	cs.costUSD = ss.CostUSD
	cs.costKnown = ss.CostKnown
	cs.hasBilledUsage = ss.BilledInputTokens+ss.BilledCachedTokens+ss.BilledOutputTokens > 0
	return nil
}

// compactionOptions builds the ctxcompact.Options for this server, wiring the
// process-wide secrets redactor into compaction (C11) and naming the path the
// options serve (W-F-08: the trigger rides the lifecycle events so a hook can
// tell a pre-turn auto-compact from a manual /compact).
//
// THE NIL CHECK IS LOAD-BEARING AND CANNOT BE DROPPED. s.redactor is a
// *secrets.Redactor and is documented as nil when no secrets backend is
// configured, while ctxcompact.Options.Redactor is an INTERFACE. Assigning a
// nil *secrets.Redactor to it yields a non-nil interface holding a nil pointer,
// so ctxcompact's own `r == nil` guard does not fire and (*Redactor).Redact
// runs on a nil receiver, dereferencing r.mu and panicking — inside the
// pre-turn path of every chat request on any deployment without a secrets
// backend. Returning the zero Options instead keeps that case on the exact
// historical code path.
func (s *Server) compactionOptions(trigger string) ctxcompact.Options {
	if s.redactor == nil {
		return ctxcompact.Options{Trigger: trigger}
	}
	return ctxcompact.Options{Redactor: s.redactor, Trigger: trigger}
}

// maybeAutoCompact runs threshold-gated compaction before a user_message turn.
// When compaction fires it streams compact_chunk deltas, replaces cs.history,
// and emits status{compacted} with the before/after token estimates. Disabled
// (threshold <= 0), under-threshold, or no-model-available is a silent no-op.
//
// DURABILITY FIRST (C1). The window is flushed to the durable log before a
// single message is evicted, and a failed flush ABORTS the compaction. The
// order is the whole point: compaction's output is a summary, and a summary is
// a lossy encoding of what it replaces. Evicting first and writing after would
// mean that a write failure — a full disk, a locked database — silently
// converts a turn's tool output into a paragraph about it, with nothing left to
// recover from. Refusing to compact instead trades a larger context (and,
// eventually, a provider-side length error the operator can see) for never
// destroying data that was never written down. QwenPaw's ScrollContextManager
// makes the same trade in compress(), which returns without evicting when its
// guarded persist reports degraded durability.
func maybeAutoCompact(ctx context.Context, s *Server,
	models map[string]model.BaseChatModel, conn *wsConn, cs *connSession) {

	// Re-evaluated every turn: the flag describes the CURRENT state, so a turn
	// that compacts cleanly must clear a refusal the previous one recorded.
	cs.compactionBlocked = ""
	kr := keepRecentOrDefault(s.compaction.KeepRecent)
	cw := contextWindowFor(cs.model, s.compaction)
	sumModel := compactionModel(s.compaction, models, cs.model)
	if sumModel == nil {
		return // no model available — compaction disabled
	}
	if !cs.flushHistory(s) {
		// Not durable — see the durability note above. Reported rather than
		// silent for the same reason a misalignment is: the context is now
		// oversized, and the only alternative signal is a provider length error
		// several turns later that looks like something else entirely.
		cs.reportCompactionBlocked(s, conn, compactionNotDurable)
		return
	}
	// W-F-08: bind the compaction lifecycle sink (if any hooks are configured)
	// and name this path pre_turn. The ctx reaching MaybeCompactWithOptions is
	// the connection ctx — no turn has started, so nothing else has bound a bus.
	newHist, tb, ta, did := ctxcompact.MaybeCompactWithOptions(
		ctxcompact.WithLifecycleSink(ctx, s.compactionHooks), cs.history,
		thresholdFor(cs.model, s.compaction), cw, kr, sumModel,
		func(chunk string) { conn.write(proto.NewCompactChunk(chunk)) },
		s.compactionOptions(ctxcompact.TriggerPreTurn))
	if !did {
		return
	}
	if ok, why := cs.commitCompaction(s, newHist); !ok {
		cs.reportCompactionBlocked(s, conn, why)
		return // window unchanged, but no longer silently so
	}
	cs.tokensIn = ta // refresh the footer ctx counter (statusFrame reads cs.tokensIn)
	st := cs.statusFrame(s)
	st.Compacted, st.TokensBefore, st.TokensAfter = true, tb, ta
	conn.write(st)
}

// compactNow handles the manual /compact command. It forces compaction
// regardless of the threshold (a user asking to compact wants it to happen now,
// even on a sub-threshold history) while still respecting keepRecent so a
// too-short conversation isn't summarized into nothing. When compaction runs it
// streams compact_chunk deltas and emits status{compacted}; otherwise it emits a
// plain status so the TUI's compact block resolves either way.
//
// DURABILITY FIRST (C1), on the same terms as maybeAutoCompact: the window is
// flushed before anything is evicted and a failed flush refuses the compaction.
// A user typing /compact is asking to shrink the window, not authorising the
// loss of whatever has not been written down — and a manual request is if
// anything MORE likely to run on a session whose history matters.
//
// Force delegates to ctxcompact.ForceCompact (DRY: the threshold-skip + the
// did-means-actual-shrink + too-few-messages guard live there, shared with
// MaybeCompact and CompactingModel.maybeCompact). The WS-layer decisions kept
// here are: (1) which window to pass — the configured per-model window via
// contextWindowFor, or a 256000 default when unset (matching
// config.Config.applyDefaults' Compaction.ContextWindow default); (2) the WS
// status / compact_chunk streaming.
func compactNow(ctx context.Context, s *Server,
	models map[string]model.BaseChatModel, conn *wsConn, cs *connSession) {

	// Same rule as the auto path: the flag describes the outcome of the MOST
	// RECENT attempt, so every attempt starts by clearing it and only a refusal
	// sets it. Clearing on success instead would leave a stale warning on any
	// path that returns before the success line.
	cs.compactionBlocked = ""
	kr := keepRecentOrDefault(s.compaction.KeepRecent)
	sumModel := compactionModel(s.compaction, models, cs.model)
	if sumModel == nil {
		conn.write(cs.statusFrame(s))
		return
	}
	if !cs.flushHistory(s) {
		// Not durable — refuse rather than evict. The status frame still goes
		// out so the TUI's compact block resolves; the error frame is what tells
		// the user their explicit request did not happen and why, which a bare
		// "nothing changed" status would not.
		cs.compactionBlocked = compactionNotDurable
		conn.write(proto.NewError(compactionNotDurable))
		conn.write(cs.statusFrame(s))
		return
	}
	// ModelWindow for RunSummary's chunk budget. The force path decouples this
	// from MaybeCompact's threshold-coupled contextWindow; fall back to the same
	// default config.Config.applyDefaults uses for Compaction.ContextWindow so
	// the budget math always has room for the instruction plus at least one
	// chunk, and behavior matches a normally-loaded config when unset.
	//
	// The window must be the SUMMARY model's, not the session's: when
	// compaction.model names a small fast model, sizing chunks against a
	// 256K session window hands that model a chunk it cannot accept, and the
	// provider rejects the whole compaction. Falls back to the session model
	// when no summary model is configured, which is when they are the same.
	windowOwner := cs.model
	if s.compaction.Model != "" {
		windowOwner = s.compaction.Model
	}
	cw := contextWindowFor(windowOwner, s.compaction)
	if cw <= 0 {
		cw = 256000
	}
	newHist, tb, ta, did := ctxcompact.ForceCompactWithOptions(
		ctxcompact.WithLifecycleSink(ctx, s.compactionHooks), cs.history, cw, kr, sumModel,
		func(chunk string) { conn.write(proto.NewCompactChunk(chunk)) },
		s.compactionOptions(ctxcompact.TriggerManual))
	if !did {
		conn.write(cs.statusFrame(s))
		return
	}
	if ok, why := cs.commitCompaction(s, newHist); !ok {
		// Same reasoning as the pre-compaction flush above: an explicit request
		// that did not happen has to say so, not resolve as "nothing changed".
		cs.compactionBlocked = why
		conn.write(proto.NewError(why))
		conn.write(cs.statusFrame(s))
		return
	}
	cs.tokensIn = ta // refresh the footer ctx counter (statusFrame reads cs.tokensIn)
	st := cs.statusFrame(s)
	st.Compacted, st.TokensBefore, st.TokensAfter = true, tb, ta
	conn.write(st)
}

// compactionModel returns the model.BaseChatModel to summarize with: a dedicated
// compaction.Model when configured and registered, else the active session
// model, else the first registered model by sorted name. Returns nil if none
// available (caller skips compaction rather than guessing).
//
// The sorted-name fallback is deterministic so a fresh WS session (cs.model==""
// before the user picks a model via /model) summarizes on the same provider
// every time. Iterating the map directly (the historical implementation) picked
// a random provider per call — and the first user_message triggers
// maybeAutoCompact, so the random branch IS reachable in production.
func compactionModel(cc CompactionConfig, models map[string]model.BaseChatModel, sessionModel string) model.BaseChatModel {
	if cc.Model != "" {
		if m := models[cc.Model]; m != nil {
			return wrapCompactionFallback(m, cc.Model, cc)
		}
	}
	if sessionModel != "" {
		if m := models[sessionModel]; m != nil {
			return wrapCompactionFallback(m, sessionModel, cc)
		}
	}
	// Fallback: deterministic (sorted) first registered model — a new session
	// may hit this before the user picks a model via /model.
	names := make([]string, 0, len(models))
	for k := range models {
		names = append(names, k)
	}
	sort.Strings(names)
	if len(names) > 0 {
		return wrapCompactionFallback(models[names[0]], names[0], cc)
	}
	return nil
}

// wrapCompactionFallback (W-C-10) wraps the PRE-TURN compaction summarizer m
// in a fallback-aware ResilientChatModel when cc.ProviderFallbacks declares a
// chain for id — the pre-turn twin of orchestrator.wrapCompaction's fallback
// branch. An empty/absent chain returns m unchanged: this is the common case
// today (the shipped catalog ships zero fallback_models rows, Ruling RC-8),
// and it is what keeps every existing compactionModel() caller's behavior
// byte-identical until an operator's registry actually resolves one.
//
// Unlike the mid-turn path, there is no separate "Summarizer" field to set
// here — compactionModel()'s return value IS the summarizer (there is no
// separate turn-answering call sharing this model on the pre-turn path), so
// wrapping m directly, rather than something alongside it, is correct.
func wrapCompactionFallback(m model.BaseChatModel, id string, cc CompactionConfig) model.BaseChatModel {
	fallbacks := cc.ProviderFallbacks[id]
	if len(fallbacks) == 0 {
		return m
	}
	chain := append([]model.BaseChatModel{m}, fallbacks...)
	resilient, err := einollm.NewResilientModel(chain, einollm.ResilientConfig{})
	if err != nil {
		// Only reachable for an empty chain (see NewResilientModel), which
		// chain cannot be here — it always has m as its first entry. Fail-safe:
		// fall back to the unwrapped model rather than dropping compaction
		// entirely.
		return m
	}
	return resilient
}

// contextWindowFor returns the context-window budget for model: the per-provider
// override if set, else the configured fallback. Zero result lets the caller
// disable compaction for unconfigured models.
func contextWindowFor(model string, cc CompactionConfig) int {
	if w, ok := cc.ProviderWindows[model]; ok && w > 0 {
		return w
	}
	return cc.ContextWindow
}

// thresholdFor returns the auto-compact threshold budget for model: the
// per-provider/catalog override if set, else the configured global fallback.
// Mirrors contextWindowFor exactly (W-C-01 / INF2) — this is the PRE-TURN
// sibling of orchestrator.CompactionConfig.thresholdFor/wrapCompaction, the
// mid-turn path.
//
// The global-off check runs FIRST and short-circuits before ProviderThresholds
// is even consulted (ADR-0024 C2, mirroring orchestrator.wrapCompaction's own
// `cc.Threshold <= 0` gate — see that function for why the two must stay
// literally the same check, ADR-0024 C1). Without it, an operator who wrote
// `compaction.threshold: -1` (or a bare `0`, which only stays 0 on a Config
// that bypassed applyDefaults) to turn compaction off globally would have it
// silently reopened the moment a per-model catalog/config threshold existed
// for their model — the value a per-model opinion may only RESIZE an
// already-open gate with, never use to reopen a closed one.
//
// A resolved per-model value that is itself negative is an explicit
// PER-PROVIDER disable (W-C-04 / F-10): it is returned as-is, and
// MaybeCompactWithOptions's own `threshold <= 0` gate turns it into "off" for
// that one provider without touching the global Threshold. This is the
// mirror image of the global sentinel above, and the reason the lookup below
// tests `t != 0` (not `t > 0`) — negative must pass through, while a stray
// literal 0 (defensive: BuildProviders never stores one, since
// ResolveAutoCompactThreshold never returns ok=true with a 0 value) still
// falls through to the fallback rather than firing compaction every turn.
func thresholdFor(model string, cc CompactionConfig) float64 {
	if cc.Threshold <= 0 {
		return cc.Threshold
	}
	if t, ok := cc.ProviderThresholds[model]; ok && t != 0 {
		return t
	}
	return cc.Threshold
}

// keepRecentOrDefault applies the conventional default (4) when the configured
// value is non-positive, which is the case for in-memory Servers built without a
// loaded config (e.g. most tests). A loaded config carries KeepRecent=4 already.
func keepRecentOrDefault(k int) int {
	if k <= 0 {
		return 4
	}
	return k
}

// usageRecorder is the seam the OTel counter is emitted through.
//
// A package-level var rather than a direct call because the gap this closed
// was a WIRING gap: otel.RecordUsage worked fine and had no callers, so a
// test that called it directly would have passed throughout. The only useful
// assertion is that addProviderUsage reaches it, and that needs an
// observation point.
var usageRecorder = otelinstr.RecordUsage

// swapUsageRecorder replaces the recorder for a test and returns a restore
// func.
func swapUsageRecorder(f func(ctx context.Context, model string, prompt, cached, completion, reasoning int)) func() {
	prev := usageRecorder
	usageRecorder = f
	return func() { usageRecorder = prev }
}
