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
	"sort"
	"strconv"
	"time"

	"github.com/cloudwego/eino/components/model"

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
	mode, auto := cs.perm.get()
	st := proto.NewStatusWithMode(cs.displayModel(), cs.thinking, cs.tokensIn, cs.tokensOut, cs.turns,
		contextWindowFor(cs.model, s.compaction), string(mode), auto)
	// CachedTokens / ReasoningTokens are populated after construction so
	// NewStatusWithMode's signature (and its many callers) stay unchanged. The
	// omitempty JSON tag drops them when zero (pre-record / non-reporting model).
	st.CachedTokens = cs.cachedTokens
	st.ReasoningTokens = cs.reasoningTokens
	// SessionID: empty when recording is disabled (store == nil) or before the
	// first user_message; omitempty drops it on the wire so legacy clients and
	// the no-store path are unchanged. Headless exec reads it to print the id.
	st.SessionID = cs.sessionID
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

// persistMessages writes the user and assistant messages from a completed turn
// to the DB. No-op when recording is disabled.
func (cs *connSession) persistMessages(s *Server, userText, assistantText string) {
	if s.store == nil || cs.sessionID == "" || cs.recordingSuppressed() {
		return
	}
	// Persist user message.
	if err := s.store.AppendMessage(cs.sessionID, cs.seq, "user", userText); err != nil {
		return
	}
	cs.seq++
	// Persist assistant message.
	if assistantText != "" {
		if err := s.store.AppendMessage(cs.sessionID, cs.seq, "assistant", assistantText); err != nil {
			return
		}
		cs.seq++
	}
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

	// Load messages and reuse the same snapshot mapper that WS restore uses
	// (applySessionRevertSnapshot in ws_seam.go), so reconnect + undo restore
	// share one role/meta mapping (B2-RB1).
	msgs, err := s.store.Messages(sessionID)
	if err != nil {
		return err
	}
	applySessionRevertSnapshot(cs, store.SessionRevertSnapshot{
		Meta:     *ss,
		Messages: msgs,
	})
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

// maybeAutoCompact runs threshold-gated compaction before a user_message turn.
// When compaction fires it streams compact_chunk deltas, replaces cs.history,
// and emits status{compacted} with the before/after token estimates. Disabled
// (threshold <= 0), under-threshold, or no-model-available is a silent no-op.
func maybeAutoCompact(ctx context.Context, s *Server,
	models map[string]model.BaseChatModel, conn *wsConn, cs *connSession) {

	kr := keepRecentOrDefault(s.compaction.KeepRecent)
	cw := contextWindowFor(cs.model, s.compaction)
	sumModel := compactionModel(s.compaction, models, cs.model)
	if sumModel == nil {
		return // no model available — compaction disabled
	}
	newHist, tb, ta, did := ctxcompact.MaybeCompact(ctx, cs.history,
		s.compaction.Threshold, cw, kr, sumModel,
		func(chunk string) { conn.write(proto.NewCompactChunk(chunk)) })
	if !did {
		return
	}
	cs.history = newHist
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
// Force delegates to ctxcompact.ForceCompact (DRY: the threshold-skip + the
// did-means-actual-shrink + too-few-messages guard live there, shared with
// MaybeCompact and CompactingModel.maybeCompact). The WS-layer decisions kept
// here are: (1) which window to pass — the configured per-model window via
// contextWindowFor, or a 256000 default when unset (matching
// config.Config.applyDefaults' Compaction.ContextWindow default); (2) the WS
// status / compact_chunk streaming.
func compactNow(ctx context.Context, s *Server,
	models map[string]model.BaseChatModel, conn *wsConn, cs *connSession) {

	kr := keepRecentOrDefault(s.compaction.KeepRecent)
	sumModel := compactionModel(s.compaction, models, cs.model)
	if sumModel == nil {
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
	newHist, tb, ta, did := ctxcompact.ForceCompact(ctx, cs.history, cw, kr, sumModel,
		func(chunk string) { conn.write(proto.NewCompactChunk(chunk)) })
	if !did {
		conn.write(cs.statusFrame(s))
		return
	}
	cs.history = newHist
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
			return m
		}
	}
	if sessionModel != "" {
		if m := models[sessionModel]; m != nil {
			return m
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
		return models[names[0]]
	}
	return nil
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
