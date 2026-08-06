package http

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/x6nux/yanshi/internal/agent/orchestrator"
	"github.com/x6nux/yanshi/internal/approval"
	"github.com/x6nux/yanshi/internal/guard"
	einollm "github.com/x6nux/yanshi/internal/llm/eino"
	obslog "github.com/x6nux/yanshi/internal/observe/log"
	otelobs "github.com/x6nux/yanshi/internal/observe/otel"
	"github.com/x6nux/yanshi/internal/proto"
	"github.com/x6nux/yanshi/internal/shell"
	"github.com/x6nux/yanshi/internal/skills"
	"github.com/x6nux/yanshi/internal/tools"
	"github.com/x6nux/yanshi/internal/vcs"
)

// connSession is the per-WebSocket session state: accumulated history, the
// selected model + thinking effort, and running usage/turn counters. Each WS
// connection owns one; control frames (set_model / clear / compact / …) mutate
// it, and user_message turns append to it. The model map is shared across
// connections (it is the App.Models registry); only the selected name lives
// here, so cs never retains a model pointer after a switch.
type connSession struct {
	history  []*schema.Message
	model    string // selected model name; "" = orchestrator default
	thinking string // "" | "low" | "medium" | "high"

	// perm is the live, mutex-guarded interactive permission mode
	// (default|allow-edits|yolo|auto) + the ModeAuto risk ceiling. The reader
	// goroutine writes it the instant a set_mode frame arrives (bypassing the
	// frames channel the blocked main loop drains) so a mid-turn mode switch
	// reaches sub-agent tool calls; statusFrame + the permission callback read
	// it. See permModeState and applySetMode.
	perm *permModeState

	transientThreadID string

	// defaultModel is the model name shown in status frames when no model is
	// explicitly selected (cs.model == ""), so the footer always reflects the
	// active model. It is the first registry name (sorted) — exact for the
	// common single-model setup; with multiple models it is a best-effort
	// display default the user can override with /model.
	defaultModel string

	tokensIn  int
	tokensOut int
	// cachedTokens / reasoningTokens (Task A6) break out the cumulative prompt-
	// cache-hit and reasoning-model-spend totals so /cost can surface them. The
	// API reports cumulative counts per call, so — like tokensIn — each turn
	// OVERWRITES the latest value (the current context's hit/total), rather than
	// summing across turns.
	cachedTokens    int
	reasoningTokens int
	turns           int

	// toolCalls counts tool_call frames emitted this session (accumulated
	// across turns, including judge-retried turns) for the turn-end
	// "Done N tools uses X tokens Y" summary line. startedAt is the session
	// start wall clock for the elapsed-time field.
	toolCalls int
	startedAt time.Time

	// sessionID is the DB session id for recording conversations. It is set
	// lazily on the first user_message (when s.store != nil). Empty = no recording.
	sessionID string
	// seq is the next message sequence number for the recorded session.
	seq int

	// inTurn is true while a user_message turn is running on the main loop,
	// false otherwise. The reader goroutine Load()s it to reject list_seams /
	// restore_turn frames INLINE (without this gate they would queue behind the
	// running turn and only execute post-turn). atomic.Bool (NOT mutex+bool):
	// the reader must consult it WITHOUT holding a lock, because the reject
	// path calls conn.write (itself mutex-guarded) — a mutex+bool would create
	// a lock-order cycle with wsConn.write's mutex.
	inTurn atomic.Bool

	// sideStack (V11) holds snapshots for nested ephemeral conversations. Each
	// enter_side pushes a snapshot of (history, sessionID, seq, counters,
	// toolCalls, startedAt); each exit_side pops. The stack is non-empty
	// exactly while inside a side conversation; recordingSuppressed() reports
	// this so ensureSession / persistMessages / UpdateSessionMeta short-circuit
	// and NO DB writes happen during side turns. The stack is bounded by
	// maxSideDepth (refusal on overflow) so a runaway model cannot push
	// indefinitely. Not persisted: a server restart drops the stack.
	sideStack []sideSnapshot

	// C4 COST1 per-session billing ledger. billing accumulates billable
	// tokens across every provider usage (including the judge call); costUSD
	// is the running USD total; costKnown=false means the session referenced
	// at least one model absent from the price table (render "N/A" not "$0");
	// hasBilledUsage flags whether costKnown has been seeded yet so the FIRST
	// usage can set the known/unknown state instead of being short-circuited
	// by a default-true AND-fold.
	billing        einollm.Ledger
	costUSD        float64
	costKnown      bool
	hasBilledUsage bool
}

// maxSideDepth caps nested ephemeral side conversations (V11) so a runaway
// model cannot push indefinitely. Entering beyond this depth returns an error
// frame.
const maxSideDepth = 3

// sideSnapshot is the push/pop payload for V11 side conversations. It captures
// everything that a side turn would otherwise mutate, so exit_side restores the
// main thread's state exactly. The snapshot is by-value (history is a fresh
// slice) so subsequent side-turn appends cannot mutate the snapshotted history.
type sideSnapshot struct {
	history         []*schema.Message
	sessionID       string
	seq             int
	tokensIn        int
	tokensOut       int
	cachedTokens    int
	reasoningTokens int
	turns           int
	toolCalls       int
	startedAt       time.Time
}

// recordingSuppressed reports whether DB writes should be skipped (V11 side).
// ensureSession / persistMessages / UpdateSessionMeta consult this to enforce
// the core promise "side never writes DB". cs.sessionID == "" is the
// coincident signal (enterSide clears it), but checking sideStack explicitly
// is defense-in-depth — review #2 mandates both.
func (cs *connSession) recordingSuppressed() bool { return len(cs.sideStack) > 0 }

// enterSide pushes a snapshot of the current state and clears sessionID (so
// ensureSession cannot create a session for side turns). Returns an error if
// the nesting depth exceeds maxSideDepth.
func (cs *connSession) enterSide() error {
	if len(cs.sideStack) >= maxSideDepth {
		return fmt.Errorf("side depth limit (%d) reached", maxSideDepth)
	}
	histCopy := make([]*schema.Message, len(cs.history))
	copy(histCopy, cs.history)
	cs.sideStack = append(cs.sideStack, sideSnapshot{
		history:         histCopy,
		sessionID:       cs.sessionID,
		seq:             cs.seq,
		tokensIn:        cs.tokensIn,
		tokensOut:       cs.tokensOut,
		cachedTokens:    cs.cachedTokens,
		reasoningTokens: cs.reasoningTokens,
		turns:           cs.turns,
		toolCalls:       cs.toolCalls,
		startedAt:       cs.startedAt,
	})
	// Clearing sessionID makes ensureSession's "cs.sessionID != ''" guard pass,
	// but ensureSession ALSO checks recordingSuppressed so even a future code
	// change that mis-handles sessionID cannot create a side session.
	cs.sessionID = ""
	cs.seq = 0
	return nil
}

// exitSide pops the most recent side snapshot and restores the captured state.
// It discards everything the side turn appended (history, tokens, turns). If
// the stack is empty, it is a no-op.
func (cs *connSession) exitSide() {
	if len(cs.sideStack) == 0 {
		return
	}
	snap := cs.sideStack[len(cs.sideStack)-1]
	cs.sideStack = cs.sideStack[:len(cs.sideStack)-1]
	cs.history = snap.history
	cs.sessionID = snap.sessionID
	cs.seq = snap.seq
	cs.tokensIn = snap.tokensIn
	cs.tokensOut = snap.tokensOut
	cs.cachedTokens = snap.cachedTokens
	cs.reasoningTokens = snap.reasoningTokens
	cs.turns = snap.turns
	cs.toolCalls = snap.toolCalls
	cs.startedAt = snap.startedAt
}

// setInTurn is called by the main loop (turn start / end).
func (cs *connSession) setInTurn(v bool) { cs.inTurn.Store(v) }

// isInTurn is called by the reader goroutine (lock-free) to decide whether
// list_seams / restore_turn should be rejected inline.
func (cs *connSession) isInTurn() bool { return cs.inTurn.Load() }

// turnEndSummary formats the per-turn "Done N tools uses X tokens Y" summary
// line: N is the session's accumulated tool_call count, the token figure is
// the current context (tokensIn) plus accumulated output (tokensOut), and the
// duration is wall-clock since the session started.
func turnEndSummary(cs *connSession) string {
	return fmt.Sprintf("Done %d tools uses %s tokens %s",
		cs.toolCalls,
		formatTokenCount(cs.tokensIn+cs.tokensOut),
		formatElapsed(time.Since(cs.startedAt)))
}

// ChatWS registers GET /api/v1/chat/ws — a bidirectional WebSocket. Each
// connection holds its own conversation history (multi-turn) and session config
// (selected model, thinking effort, usage counters). Client frames:
//
//	user_message       a user turn (Text may start with "/skill …")
//	cancel             cancel the in-flight turn
//	set_model          switch the session model (Name = provider name)
//	set_thinking       set reasoning effort (Effort = low|medium|high|off)
//	clear              reset history + counters
//	list_models        request available model names (reply: models)
//	get_status         request session status (reply: status)
//	compact            compact the conversation context (reply: status)
//	list_mcp           request configured MCP server names (reply: mcp_list)
//	session_list       list stored sessions (reply: sessions)
//	restore_session    restore a stored session by ID (reply: session_restored)
//
// Server frames: agent_chunk, tool_call, tool_result, error, done, models,
// status (see internal/proto). "/skill <name> [task]" prefixes are resolved the
// same way as the SSE endpoint, so both transports share one event vocabulary.
//
// Reader goroutine + select. conn.ReadMessage runs in its own goroutine so a
// cancel frame can be handled even while the main loop is blocked running a
// turn (ClassifyEventsWithUsage runs synchronously in the main loop). Without
// that, a cancel sent mid-turn would sit unread until the turn finished — by
// which point cancel is meaningless. Cancel frames fire the in-flight
// turnCancel directly; other frames are forwarded to the main loop.
func (s *Server) ChatWS(o *orchestrator.Orchestrator, models map[string]model.BaseChatModel, reg *skills.Registry) {
	// Capture the orchestrator's profile so the WS handler can authorize
	// control-frame actions (job_write/job_cancel) against the same permission
	// profile the in-flight turns use (Task 22).
	s.controlProfile = o.Profile()
	s.HandleFunc("GET /api/v1/chat/ws", func(w http.ResponseWriter, r *http.Request) {
		raw, err := wsUpgrader.Upgrade(w, r, nil)
		if err != nil {
			return // upgrade already wrote an error response
		}
		defer raw.Close()
		conn := &wsConn{Conn: raw, redactor: s.redactor}

		// Connection context, created exactly once. Its cancel is captured by
		// the deferred closure so every return path runs it — the cancel is
		// never reassigned, which is what keeps `go vet` lostcancel happy. All
		// per-turn contexts derive from connCtx.
		connCtx, cancel := context.WithCancel(r.Context())
		defer func() { cancel() }()

		// Per-connection permission-request registry. The ask callback (main
		// loop) writes here; the reader goroutine delivers responses. See
		// permTracker for why this is split across goroutines.
		pt := newPermTracker()

		// Per-connection approval manager: an always_allow / allow_session /
		// allow_persistent in one turn must suppress the prompt for the
		// identical action in later turns. Installed on the connection context
		// (the parent of every turn context) so each turn's
		// WithPermissionCallback reuses it rather than starting fresh.
		//
		// Task 9 wiring: prefer the shared s.approvals manager so persistent
		// rules survive across reconnects and are visible to /permissions. Fall
		// back to a per-connection manager when Server.Config.Approvals was not
		// set (e.g. unit tests that don't wire it). connectionSessionID is
		// independent of the conversation DB session id so a restored session
		// does not inherit a previous connection's approval scoping.
		connectionSessionID := fmt.Sprintf("ws-%d", time.Now().UnixNano())
		approvalManager := s.approvals
		if approvalManager == nil {
			approvalManager, err = approval.New(nil, "ws-conn", nil)
			if err != nil {
				conn.write(proto.NewError(fmt.Sprintf("approval manager init: %v", err)))
				return
			}
		}
		connCtx = tools.WithApprovalManager(connCtx, approvalManager, connectionSessionID)

		// Subagent relay: forwards lifecycle events while the connection is open.
		// Detach on handler exit so background agents don't hold a reference to the
		// connection after it closes. Agent terminal states are persisted regardless.
		relay := newSubagentRelay(func(f proto.ServerFrame) { conn.write(f) })
		defer relay.Detach()

		// Audit pump: forward permission_rule_hit events to this connection so
		// the operator can audit which rule admitted which call. The manager
		// calls bus.Publish once per event; each WS connection subscribes one
		// channel so a slow client cannot stall the manager (buffered + drop).
		if s.approvalAudit != nil {
			auditCh := s.approvalAudit.Subscribe()
			defer s.approvalAudit.Unsubscribe(auditCh)
			go func() {
				for {
					select {
					case <-connCtx.Done():
						return
					case event, ok := <-auditCh:
						if !ok {
							return
						}
						conn.write(proto.NewPermissionRuleHit(event.RuleID, event.Action, scopeJSON(event.Scope), event.Kind))
					}
				}
			}()
		}

		frames := make(chan proto.ClientFrame, 16)
		errCh := make(chan error, 1)

		// turnCancel cancels the in-flight turn's context, or nil when idle. It
		// is read by the reader goroutine (cancel frame / read error) and
		// written by the main loop (turn start / turn end), so a mutex guards
		// every access.
		var (
			mu         sync.Mutex
			turnCancel context.CancelFunc
		)

		// cancelTurn fires the current turnCancel (if any) under the mutex.
		cancelTurn := func() {
			mu.Lock()
			tc := turnCancel
			mu.Unlock()
			if tc != nil {
				tc()
			}
		}

		// makeTurnCtx creates the three-layer turn context shared by the compact
		// and user_message paths, and publishes userCancel to the connection's
		// turnCancel so the reader goroutine can fire it on a cancel frame or
		// disconnect. Layers:
		//
		//   userCancelCtx — an independent cancel derived from connCtx that
		//     expresses ONLY user intent. The reader goroutine fires it on a
		//     cancel frame / disconnect; tc() never touches it. The
		//     ResilientChatModel watches userCancelCtx (bound via
		//     WithUserCancelCtx) to tell user-initiated cancel apart from a
		//     network/transport cancel of the request's own ctx — so a transient
		//     gateway drop does NOT suppress retry while a real user stop
		//     (Ctrl-C, or a client cancel frame) does.
		//
		//   turnCtx — the request lifetime, derived from userCancelCtx so a user
		//     cancel cascades into the in-flight model call.
		//
		// The returned release function clears turnCancel and invokes BOTH
		// cancels (both idempotent; userCancel() releases userCancelCtx and
		// cascades to turnCtx, tc() is kept for defensive symmetry). Callers must
		// call it at turn end (NOT defer — the outer loop reuses the goroutine,
		// so a defer would not fire between turns) to avoid leaking
		// userCancelCtx. Safe to call even after the reader goroutine already
		// fired userCancel mid-turn.
		makeTurnCtx := func() (ctx context.Context, release func()) {
			userCancelCtx, userCancel := context.WithCancel(connCtx)
			tctx, tc := context.WithCancel(userCancelCtx)
			tctx = einollm.WithUserCancelCtx(tctx, userCancelCtx)
			// Forward sub-agent lifecycle events via the detachable relay so
			// background agents continue running after WS disconnect.
			tctx = tools.WithSubAgentEmit(tctx, relay.Emit)
			mu.Lock()
			turnCancel = userCancel
			mu.Unlock()
			return tctx, func() {
				mu.Lock()
				turnCancel = nil
				mu.Unlock()
				userCancel()
				tc()
			}
		}

		var cs connSession
		cs.perm = &permModeState{}
		cs.startedAt = time.Now()
		// Default display model: the first registry name (sorted), shown in
		// status frames until the user picks one with /model.
		cs.defaultModel = einollm.ResolveModelName(models, "")
		// Seed the cost-known flag from the price table (N/A when the default
		// model isn't priced). Subsequent turns AND-fold against this so any
		// single unknown model flips the whole session to N/A.
		cs.resetBilling(s)

		// Reader goroutine: decodes frames independently of the main loop. A
		// cancel frame triggers turnCancel immediately (real mid-turn cancel);
		// every other frame is forwarded to `frames`. On read error it aborts
		// any in-flight turn and signals errCh.
		go func() {
			for {
				_, data, rerr := conn.ReadMessage()
				if rerr != nil {
					cancelTurn() // disconnect aborts any in-flight turn
					select {
					case errCh <- rerr:
					default:
					}
					return
				}
				var cf proto.ClientFrame
				if jerr := json.Unmarshal(data, &cf); jerr != nil {
					// Empty Type marks an invalid frame; Text carries the parse
					// error so the main loop can report it.
					cf = proto.ClientFrame{Text: jerr.Error()}
				}
				if cf.Type == "cancel" {
					cancelTurn()
					continue
				}
				// permission_response is handled here (not forwarded to the
				// main loop) so it can be delivered while the turn runner is
				// blocked inside the ask callback waiting for this very reply.
				if cf.Type == "permission_response" {
					// The live mode is read here, not captured at ask-time:
					// deliver compares the two to spot an `allow` produced by
					// a mode switch rather than by the user answering the
					// question we actually asked.
					curMode, _ := cs.perm.get()
					pt.deliver(cf.ID, tools.PermissionDecision(cf.Decision), curMode)
					continue
				}
				// set_mode is applied to the LIVE permission state here — not
				// only forwarded to the frames channel — so a mode switch takes
				// effect even while a turn is running and the main loop (the
				// only frames drainer) is blocked. Without this, mid-turn tool
				// calls inside a sub-agent would keep consulting the stale
				// pre-switch mode. The frame is still forwarded so the main
				// loop emits the status echo; the live state is what the
				// permission callback consults for mid-turn decisions.
				if cf.Type == "set_mode" {
					cs.applySetMode(cf)
				}
				// B2-RB1: list_seams / restore_turn are rejected INLINE when a
				// turn is running — the main loop is blocked, so queueing them
				// through `frames` would delay the response until post-turn
				// (confusing UX). The reject error frame is written from the
				// reader goroutine directly (conn.write is mutex-guarded). When
				// idle they fall through to `frames` and are handled by the main
				// loop's list_seams / restore_turn cases.
				if cf.Type == "list_seams" || cf.Type == "restore_turn" {
					if cs.isInTurn() {
						conn.write(proto.NewError(
							"cannot " + cf.Type + " while a turn is running"))
						continue
					}
				}
				select {
				case frames <- cf:
				case <-connCtx.Done():
					return
				}
			}
		}()

		// runUserTurn executes the full user_message lifecycle (resolveQuery →
		// maybeAutoCompact → ADK runner → post-turn persistence). It is extracted
		// from the case body so function-level defers cover EVERY exit path
		// (normal / model error / tool error / cancel / disconnect / panic) —
		// specifically the post-turn seam (B2-RB1 必修项 F). The closure captures
		// existing outer state (cs, conn, s, o, models, reg, pt, makeTurnCtx,
		// connectionSessionID) by reference.
		runUserTurn := func(cf proto.ClientFrame) {
			// Register reset first: Go LIFO makes this the final cleanup, so the
			// reader bypass stays closed through seam sealing and ctx release.
			cs.setInTurn(true)
			defer cs.setInTurn(false)

			lastUserText := cf.Text // keep the original text for persistence
			query, errMsg := resolveQuery(reg, cf.Text)
			if errMsg != "" {
				conn.write(proto.NewError(errMsg))
				conn.write(proto.NewDone())
				return
			}
			cs.history = append(cs.history, &schema.Message{Role: schema.User, Content: query})

			// Auto-create the DB session on the first user message.
			cs.ensureSession(s, lastUserText)

			// Per-turn ctx; see makeTurnCtx for the layering rationale. Register
			// release() BEFORE the post-turn seam defer; LIFO then executes the
			// seam first, the release last — matching the requirement that the
			// reader bypass stays closed until AFTER the post-turn seam is sealed.
			turnCtx, release := makeTurnCtx()
			defer release()
			// sealPostTurn seals the post-turn seam. It MUST run before the
			// "done" frame on the normal path: a client that sees "done" and
			// immediately records a VCS edit (e.g. the RB1 integration test's
			// advance()) would race the defer-based seal — the seal folds the
			// pending edit into its own commit (commitScope clears the
			// changeset), so the client's follow-up CommitMain hits
			// "no changes to commit". Sealing before "done" makes the done
			// frame a true turn-end barrier. The defer remains a safety net
			// for early-return error paths that don't reach the normal done.
			sealed := false
			sealPostTurn := func() {
				if sealed {
					return
				}
				sealed = true
				s.sealTurnBoundary(cs.sessionID, cs.turns, len(cs.history),
					string(vcs.SeamPostTurn), "post-turn:"+strconv.Itoa(cs.turns))
			}
			defer sealPostTurn()

			// Bind safe correlation IDs (trace/session/turn) for the redacting
			// logger and the turn span opened just below. The IDs are random
			// per-turn; nothing sensitive (no prompts, tool args, paths) flows
			// through them. The turn-start log records only boolean/numeric
			// diagnostics so an operator can reconstruct turn boundaries
			// without seeing content.
			//
			// observe.slog_trace_id gates the binding itself, which is the only
			// place it CAN be gated: the ids are read straight off the context
			// by the redacting handler (internal/observe/log) and by StartTurn,
			// and neither of those may import features without inverting the
			// dependency direction. Off therefore means no correlation ids
			// anywhere for the turn -- logs AND the span -- because there is
			// one set of ids, not two. EnabledOrDefault (not Enabled): this
			// flag is Stable with Default true, and a nil registry must not
			// silently turn it off.
			if s.featuresReg.EnabledOrDefault("observe.slog_trace_id") {
				turnCtx = obslog.WithIDs(turnCtx, obslog.IDs{
					TraceID:   obslog.NewTraceID(),
					SessionID: cs.sessionID,
					TurnID:    obslog.NewTurnID(),
				})
			} else {
				// NOT merely "skip the binding": the orchestrator mints ids for
				// any turn that arrives without them (ensureTurnIDs), so a bare
				// context comes back correlated a few frames later. WithoutIDs
				// is the only form of "off" that survives that.
				turnCtx = obslog.WithoutIDs(turnCtx)
			}

			// THIS is the WS drain boundary the orchestrator's Query doc comment
			// points at. Query opens its own span because it is synchronous and
			// owns its whole turn; the streaming paths return an iterator whose
			// drain happens here, so the span has to be opened and closed here
			// or not at all. It used to be "not at all": StartTurn had a single
			// production caller (Query) that neither transport uses, so every
			// turn a real user ran produced no span.
			//
			// StartTurn reads the IDs bound above, so the ordering is
			// load-bearing: opened earlier, the span would carry no session.id
			// or turn.id and could not be joined against these log lines.
			// turnCtx is re-derived many times below (callbacks, counters); all
			// of those preserve the span, so a tool span opened deep inside the
			// runner still nests under this one.
			var turnErr error
			turnCtx, endTurn := otelobs.StartTurn(turnCtx, cs.displayModel())
			defer func() { endTurn(turnErr) }()

			slog.InfoContext(turnCtx, "turn started",
				"model", cs.displayModel(),
				"thinking", cs.thinking != "",
				"has_output_schema", len(cf.OutputSchema) > 0,
			)

			// PRE-TURN SEAM (B2-RB1): capture the state right after the user msg
			// is appended (revert preserves the question and re-runs the model).
			s.sealTurnBoundary(cs.sessionID, cs.turns, len(cs.history),
				string(vcs.SeamPreTurn), "pre-turn:"+strconv.Itoa(cs.turns+1))

			// Auto context-compaction (Task 35b): if the history (now
			// including this turn) exceeds the configured threshold,
			// summarize the older turns on a remote model, streaming
			// compact_chunk deltas for real-time progress, then emit
			// status{compacted} with the before/after token estimate
			// BEFORE running the turn. Under threshold or disabled it
			// is a no-op. Compaction shares turnCtx so a cancel aborts
			// both the summary and the turn.
			maybeAutoCompact(turnCtx, s, models, conn, &cs)

			// Interactive permissions (WS-only): install a callback the
			// GuardedTool layer consults when the static profile would
			// deny a tool call. It mints a request id, writes a
			// permission_request frame, then BLOCKS for the user's reply
			// (delivered by the reader goroutine). While blocked, the
			// turn runner is paused — the reader goroutine is separate,
			// so it can still read permission_response. Default-deny on
			// turn cancel / disconnect / 60s timeout. When the static
			// profile already allows the action the callback is never
			// invoked, so this is a no-op for ordinary tool calls. SSE
			// installs no callback and stays on the static profile.
			turnCtx = tools.WithErrCounter(turnCtx)
			turnCtx = tools.WithPermissionCallback(turnCtx, func(req tools.PermissionRequest) tools.PermissionDecision {
				// B2-RB1 D3: resolvePermissionRequest skips auto-resolution for
				// req.Force (set by tools.RequireApproval) so destructive actions
				// (revert_turn) always reach the interactive prompt below.
				if d, resolved := resolvePermissionRequest(turnCtx, &cs, models, req); resolved {
					return d
				}
				id := pt.newID()
				ch := make(chan tools.PermissionDecision, 1)
				askMode, _ := cs.perm.get()
				pt.register(id, ch, req, askMode)
				defer pt.take(id) // remove the entry on every return path
				// req.ForcePrompt (forcePromptTools) and req.Force
				// (RequireApproval) both mean "no auto-approval, ever".
				// They MUST go on the wire: resolvePermissionMode already
				// refuses to auto-resolve them here, but the TUI runs its
				// own auto-approve pass when the user switches to YOLO, and
				// that pass can only honour flags it can see.
				conn.write(proto.NewPermissionRequest(id, req.Tool, req.Args, req.Reason,
					req.ApprovalRequired, forcePromptFlag(req)))
				select {
				case d := <-ch:
					return d
				case <-turnCtx.Done():
					return tools.PermissionDeny
				case <-time.After(60 * time.Second):
					return tools.PermissionDeny
				}
			})

			// Mid-turn compaction progress (Task 35c): the CompactingModel
			// wrapper around the orchestrator's model consults this callback
			// when it summarizes the history BETWEEN ReAct iterations. Each
			// summary text delta is forwarded to the client as a
			// compact_chunk frame so the TUI's compacting block renders live
			// (the same frame type the pre-turn maybeAutoCompact emits). A
			// no-op when the turn never crosses the threshold.
			turnCtx = einollm.WithCompactCallback(turnCtx, func(chunk string) {
				conn.write(proto.NewCompactChunk(chunk))
			})

			// Tool-chunk streaming: each tool's Stream chunks (Text/Status/
			// Overwrite/Err) are forwarded as tool_chunk frames to the TUI for
			// live fixed-template rendering. guard.go's ToolChunkCallback tees
			// every chunk from the tool's Stream to this callback (TUI) in
			// addition to the InvokableRun channel (model collects Result) —
			// resolving the ADK InvokableRun-vs-TUI-Stream tension without
			// rewriting the ADK tool-call loop.
			// One ordered, bounded writer per turn. Tool Stream callbacks only
			// enqueue frames (non-blocking); a single consumer calls conn.write,
			// preserving Text append order while ensuring a slow WS client can
			// never stall the tool goroutine. On overload, live TUI chunks may
			// be dropped; Result still reaches the model through InvokableRun.
			toolChunkFrames := make(chan proto.ServerFrame, 256)
			// Snapshot the Done channel before the goroutine starts: turnCtx is
			// reassigned below (WithToolChunkCallback / WithRetryCallback /
			// WithNewTurnRecorder), and this goroutine must read a stable value,
			// not the shared turnCtx variable (-race would flag the racy read).
			chunkDone := turnCtx.Done()
			go func() {
				for {
					select {
					case <-chunkDone:
						return
					case frame := <-toolChunkFrames:
						conn.write(frame)
					}
				}
			}()
			turnCtx = tools.WithToolChunkCallback(turnCtx, func(display string, c tools.ToolChunk) {
				frame := proto.ServerFrame{
					Type:      "tool_chunk",
					ToolName:  display,
					Text:      c.Text,
					Status:    c.Status,
					Overwrite: c.Overwrite,
				}
				select {
				case toolChunkFrames <- frame:
				default: // queue full: drop live UI update, never block the tool
				}
			})

			// Retry progress: the ResilientChatModel consults this
			// callback before each retry sleep (transient error or a
			// mid-stream "unexpected EOF"). Forwarding a retry frame
			// lets the TUI render "↻ retry N/M…" in its activity line,
			// so a retried gateway drop is visible instead of silent.
			// retryReset arms the overwrite-discard of the partial assistant
			// text when a mid-stream retry happens (see the emit closure
			// below). Declared before the retry callback so the callback
			// can capture it.
			var retryReset atomic.Bool
			turnCtx = einollm.WithRetryCallback(turnCtx, func(attempt, max int, err error, delay time.Duration) {
				// Overwrite: a retry supersedes the partial output already
				// streamed this turn. Arm the reset flag (consumed by the
				// emit closure below) so the accumulated assistant text is
				// discarded before the regenerated stream is re-fed, keeping
				// the saved history clean (partial + full → full).
				retryReset.Store(true)
				conn.write(proto.NewRetry(attempt, max, int(delay.Milliseconds()), err.Error()))
			})

			// Plan mode is decided ONCE per turn, from the live mode state, and
			// carried in TurnOpts: that field is the single entry point for the
			// entire plan implementation (tools.WithPlanMode for the runtime
			// authorize firewall, runnerFor's plan-keyed runner, and
			// filterPlanTools' read-only tool set). Without it /plan would only
			// change the permission-callback wording and still hand the model a
			// write-capable tool set. Reading it here (not per tool call) keeps
			// one turn on one runner even if the user switches mode mid-turn.
			turnMode, _ := cs.perm.get()
			// ModelID/Images are the client's attachments reaching the model.
			// displayModel() is the registry NAME of the model this turn runs on
			// (the /model selection, else the session default) — the orchestrator
			// needs it to decide between a native image part and a stored
			// placeholder. Both fields are read here, once per turn, for the same
			// reason PlanMode is: TurnOpts is the single entry point.
			opts := orchestrator.TurnOpts{
				// Correlation IDs. Empty here means WithThreadLink binds two
				// empty strings, and every log line and tool audit record for
				// the turn loses its way back to the conversation.
				// TurnID stays empty on purpose: ensureTurnIDs mints one, and
				// this path has no id of its own that outlives a single turn.
				ThreadID:            cs.sessionID,
				Model:               cs.selectModel(models),
				ModelID:             cs.displayModel(),
				ThinkingEffort:      cs.thinking,
				OutputSchema:        cf.OutputSchema,
				Images:              cf.Images,
				PlanMode:            turnMode == guard.ModePlan,
				ConnectionSessionID: connectionSessionID,
			}

			// A12-core: per-turn structured output. When the client declares
			// an output schema (NewUserMessageWithSchema), the loop below
			// validates the model's final assistantText against it and
			// retries with a reminder on failure, mirroring the task_end
			// mandatory-completion path. hasSchema=false keeps the text path
			// byte-identical to pre-A12.
			hasSchema := len(cf.OutputSchema) > 0
			var structuredResult json.RawMessage // set on schema success; emitted before done

			// Bind a per-turn message recorder into turnCtx so the
			// messageRecorder middleware captures the ADK state's messages
			// on each model call. JudgeCompletion reads the final capture
			// after the turn to judge completion against the full
			// conversation (replaying it with the judge question appended,
			// so the provider's KV cache covers the prefix). Binding through
			// context (not a handler field) means recording works for the
			// default runner AND each per-model memoized runner (which
			// installs its own recorder but shares the same turn ctx); each
			// turn has its own recorder, so concurrent WS turns never alias.
			turnCtx = orchestrator.WithNewTurnRecorder(turnCtx)

			// onUsage fires after each model response during the turn
			// (between tool calls, and as the model generates) with the
			// latest per-turn usage. The API reports CUMULATIVE counts per
			// call: prompt includes the full context for that call, so the
			// latest prompt value IS the current context size (overwrite, not
			// a running sum — summing is the doubling bug). Completion is
			// per-call output, so it accumulates across turns for /cost.
			// Cached/Reasoning (Task A6) follow the same overwrite rule:
			// each call's latest value reflects the current context's hit
			// rate and reasoning spend.
			onUsage := func(u orchestrator.TurnUsage) {
				cs.addProviderUsage(turnCtx, s, u)
				st := cs.statusFrame(s)
				st.TokensIn = u.PromptTokens                     // current context fill
				st.TokensOut = cs.tokensOut + u.CompletionTokens // prior turns + this turn
				st.CachedTokens = u.CachedTokens                 // latest cumulative cache hits
				st.ReasoningTokens = u.ReasoningTokens           // latest cumulative reasoning spend
				conn.write(st)
			}

			var usage orchestrator.TurnUsage
			var judgeCompletionTokens int
			var judgeUsage orchestrator.TurnUsage
			var assistantText string

			// Stop-judge auto-continue. The model ends a turn by stopping
			// naturally; after each attempt JudgeCompletion asks the main
			// model whether the turn fully addressed the user's request.
			// When the judge says incomplete, the loop retries up to
			// maxIncompleteRetries times so the model gets another chance,
			// with the judge's reason injected as a reminder.
			//
			// Break conditions (checked in order after each attempt):
			//   - judge says complete → completed, break.
			//   - error frame emitted (hadError) → real failure (model
			//     error, max-iters), don't retry — retrying would reproduce
			//     the same failure.
			//   - turnCtx.Err() → user cancelled (A3 userCancelCtx), don't
			//     retry. turnCtx derives from userCancelCtx, so a cancel
			//     frame / disconnect surfaces here.
			//   - cap exhausted → accept the last attempt's output and emit
			//     an error marker so the user knows the turn ended abnormally.
			//
			// SIDE-EFFECT CAVEAT: retrying re-runs the turn, which may
			// re-execute side-effecting tools (shell_run, fs_write) the
			// model already called on a prior attempt. This is the inherent
			// trade-off of the stop judge (completeness over idempotency).
			// No de-duplication is performed (out of scope). The frames from
			// earlier attempts have already been forwarded to the client, so
			// the user may see duplicate streamed output across retries. The
			// same caveat applies to schema-validation retries (A12-core): a
			// retried turn re-runs the model and any tools it calls.
			const maxIncompleteRetries = 3
			// retryCap covers whichever path is active: stop-judge
			// completion (maxIncompleteRetries) or schema validation
			// (maxSchemaRetries). Taking the max keeps a single loop bound
			// for both paths; when neither is active the loop still runs a
			// single attempt (both break conditions trip immediately).
			retryCap := maxIncompleteRetries
			if hasSchema && maxSchemaRetries > retryCap {
				retryCap = maxSchemaRetries
			}
			// prevAssistantText captures the previous attempt's final
			// assistant text so the retry can extend cs.history with it
			// plus a reminder (see below). Only read when attempt > 0;
			// the first iteration uses cs.history as-is. Set at the end
			// of each iteration ONLY when the loop is about to retry
			// (no break), so breaks don't leak partial output into a
			// subsequent iteration's reminder.
			var prevAssistantText string
			// reminder is set on the retry path that caused the loop to
			// continue: schema retries carry the validation error (so the
			// model knows what to fix); stop-judge retries carry the judge's
			// reason. Reset to "" each iteration is NOT needed — it is only
			// set immediately before `continue`, so the next iteration sees
			// the right reminder; on the first attempt it stays "".
			var reminder string
			for attempt := 0; attempt <= retryCap; attempt++ {
				// Reset per-attempt state so earlier attempts' partial output
				// is discarded — the FINAL attempt's output is what the user
				// keeps in history. usage holds the last attempt's totals
				// (overwritten, not summed), so cs.tokensOut accumulates only
				// the accepted attempt's completion tokens.
				assistantText = ""
				usage = orchestrator.TurnUsage{}
				retryReset.Store(false)

				// Build the history for this attempt. Attempt 0 uses
				// cs.history unchanged. attempt > 0 extends a COPY of
				// cs.history with the previous attempt's assistant output
				// (as an assistant message) plus a user reminder. The
				// reminder differs by path: schema retries carry the
				// validation error (so the model knows what to fix),
				// task_end retries use the default nudge telling the model
				// to call task_end. Without this extension every retry
				// would re-send the SAME history, the model would reproduce
				// the same behavior, and the retry would be useless.
				//
				// The copy is mandatory: Go append may share cs.history's
				// backing array, and mutating it would leak the reminder
				// into the persistent session history (the post-loop
				// cs.history append would then double-count turns and
				// corrupt multi-turn memory).
				//
				// prevAssistantText may be empty (e.g. a turn that only
				// streamed tool_call frames with no assistant text); in
				// that case the assistant echo is skipped but the reminder
				// is still added so the model is guided toward completion.
				history := cs.history
				if attempt > 0 {
					extra := make([]*schema.Message, 0, 2)
					if prevAssistantText != "" {
						extra = append(extra, schema.AssistantMessage(prevAssistantText, nil))
					}
					msg := reminder
					if msg == "" {
						msg = "Continue and finish addressing the user's request."
					}
					extra = append(extra, schema.UserMessage(msg))
					history = make([]*schema.Message, 0, len(cs.history)+len(extra))
					history = append(history, cs.history...)
					history = append(history, extra...)
				}

				iter := o.EventsWithHistoryOpts(turnCtx, history, opts)
				var hadError bool
				orchestrator.ClassifyEventsWithUsage(iter, &usage, func(f proto.ServerFrame) {
					// A real failure (model error, MaxIterations) surfaces as
					// an error frame — don't retry after it.
					if f.Type == "error" {
						hadError = true
					}
					// A retry happened since the last frame → drop the partial
					// assistant text so the regenerated output replaces it
					// (overwrite, not append). CompareAndSwap resets the arm so
					// only the first frame after a retry triggers the discard.
					if retryReset.CompareAndSwap(true, false) {
						assistantText = ""
					}
					if f.Type == "agent_chunk" {
						assistantText += f.Text
					}
					if f.Type == "tool_call" {
						cs.toolCalls++
					}
					conn.write(f)
				}, onUsage)

				// Hard failures break regardless of mode: a model error or
				// a user cancel must not trigger a schema/stop-judge retry.
				// The error frame has already been emitted upstream; the
				// post-loop path still persists state and emits done.
				if hadError {
					break
				}
				if turnCtx.Err() != nil {
					break // user cancelled (A3 userCancelCtx) — don't retry
				}

				if hasSchema {
					// A12-core structured-output path: validate the final
					// assistant text against the declared schema. Success
					// sets structuredResult and breaks; failure either
					// retries with a reminder (carrying the validation
					// error) or, at the cap, emits an error frame.
					validated, verr := ValidateStructuredOutput(assistantText, cf.OutputSchema)
					if verr == nil {
						structuredResult = validated
						break // schema satisfied — done
					}
					if attempt == retryCap {
						turnErr = fmt.Errorf("output did not match the required schema after %d attempt(s): %w",
							attempt+1, verr)
						conn.write(proto.NewError(turnErr.Error()))
						break
					}
					prevAssistantText = assistantText
					reminder = schemaRetryReminder(assistantText, verr)
					continue
				}

				// Stop-judge path: ask the model to judge whether the turn
				// is complete against the full conversation the recorder
				// captured. complete → accept; incomplete → retry with the
				// judge's reason as the reminder.
				complete, reason, ju := o.JudgeCompletion(turnCtx)
				// COST1: judge usage is an independent provider usage and is
				// billed as such — constraint 9 in the C4 plan ("judge usage
				// is independent provider usage, also 计费"). Add BEFORE
				// storing into judgeUsage so a buggy judge that reports zero
				// tokens doesn't accidentally un-bill a real call.
				cs.addProviderUsage(turnCtx, s, ju)
				judgeCompletionTokens += ju.CompletionTokens
				judgeUsage = ju
				if complete {
					break // judge accepted the turn as complete
				}
				if attempt == retryCap {
					// Cap exhausted: the judge kept flagging the turn as
					// incomplete. Accept the last attempt's output and warn
					// the user.
					turnErr = errors.New("turn ended without the completion judge accepting it; output may be incomplete")
					conn.write(proto.NewError(turnErr.Error()))
					break
				}
				// About to retry: capture this attempt's assistant text and
				// set the reminder to the judge's reason so the next attempt
				// addresses it.
				prevAssistantText = assistantText
				reminder = orchestrator.JudgeRetryNudge(reason)
			}

			// Prompt tokens overwrite (the API reports the cumulative context
			// per call, so the latest value is the current context size — the
			// ctx bar must reflect fill, not a forever-growing sum, which is
			// the doubling bug). Completion tokens accumulate across turns
			// (each call's output is separate), giving a real /cost total.
			// Cached/Reasoning (Task A6) follow prompt's overwrite rule: the
			// API reports the cumulative totals per call, so the latest value
			// is the current context's hit rate and reasoning spend — summing
			// would double-count on every model call.
			cs.tokensIn = usage.PromptTokens
			cs.tokensOut += usage.CompletionTokens + judgeCompletionTokens
			cs.cachedTokens = usage.CachedTokens
			cs.reasoningTokens = usage.ReasoningTokens
			// The judge call is the turn's last model invocation on a real
			// model; reflect its (cache-heavy) usage as the current context
			// so /cost and the ctx bar account for the judge probe. Skipped
			// when the judge usage is zero (FakeModel's judge probe, or a
			// provider that omits usage on non-streaming Generate) so it
			// doesn't clobber the turn's real context with zeros.
			if judgeUsage.PromptTokens > 0 {
				cs.tokensIn = judgeUsage.PromptTokens
				cs.cachedTokens = judgeUsage.CachedTokens
				cs.reasoningTokens = judgeUsage.ReasoningTokens
			}
			cs.turns++

			if assistantText != "" {
				cs.history = append(cs.history, &schema.Message{Role: schema.Assistant, Content: assistantText})
			}
			// Persist the turn to the DB (best-effort).
			cs.persistMessages(s, lastUserText, assistantText)
			// Persist session meta (model, thinking, token counters).
			if s.store != nil && cs.sessionID != "" && !cs.recordingSuppressed() {
				_ = s.store.UpdateSessionMeta(cs.sessionID, cs.model, cs.thinking, cs.tokensIn, cs.tokensOut, cs.turns, cs.cachedTokens, cs.reasoningTokens, cs.billingMeta())
			}
			// Emit updated status (usage + turns + context window) so the
			// TUI's ctx bar reflects real token usage after every turn.
			conn.write(cs.statusFrame(s))
			// A12-core: emit the validated structured result before done so
			// a schema-constrained consumer (exec --output-schema, API
			// client, later the TUI) can take the parsed JSON without
			// re-parsing the stream. Skipped on text-mode turns
			// (structuredResult stays nil — hasSchema=false never sets it).
			if structuredResult != nil {
				conn.write(proto.NewStructuredResult(structuredResult))
			}
			// Turn-end summary "Done N tools uses X tokens Y" rides on the
			// done frame's Text so it neither disturbs the agent_chunk
			// stream nor pollutes cs.history (the assistantText append
			// above already captured the model's real answer). The TUI
			// renders it as the turn's closing line.
			slog.InfoContext(turnCtx, "turn finished",
				"model", cs.displayModel(),
				"turns", cs.turns,
				"tool_calls", cs.toolCalls,
				"completion_tokens", usage.CompletionTokens+judgeCompletionTokens,
			)
			// Seal the post-turn seam BEFORE the done frame so a client that
			// acts on "done" (records a VCS edit, commits) does not race the
			// seal's pending-edit fold. See sealPostTurn for the full rationale.
			sealPostTurn()
			conn.write(proto.ServerFrame{Type: "done", Text: turnEndSummary(&cs)})
		}

		for {
			select {
			case <-connCtx.Done():
				return
			case <-errCh:
				return // client closed / read error
			case cf := <-frames:
				switch cf.Type {
				case "list_models":
					conn.write(proto.NewModels(einollm.SortedModelNames(models)))
				case "set_model":
					// Only honor names present in the registry. A nil map (the
					// FakeModel path) or an unknown name leaves the session on
					// the default — both look like "not found" here.
					if cf.Name != "" && models[cf.Name] != nil {
						cs.model = cf.Name
						// COST1: if no billed usage yet, refresh the costKnown
						// flag against the new model (so /cost on a freshly
						// picked unknown model doesn't briefly render the prior
						// model's "$0.0000"). Once billing has started, the
						// already-accumulated spend stays; the next addProviderUsage
						// AND-folds in the new model's known/unknown state.
						if !cs.hasBilledUsage {
							_, cs.costKnown = s.priceTab[cs.displayModel()]
						}
					}
					conn.write(cs.statusFrame(s))
				case "set_thinking":
					switch cf.Effort {
					case "low", "medium", "high":
						cs.thinking = cf.Effort
					case "off", "":
						cs.thinking = ""
						// unrecognized values: leave the current effort unchanged
					}
					conn.write(cs.statusFrame(s))
				case "set_mode":
					// Switch the interactive permission mode. The reader goroutine
					// already applied this to the live permission state the instant
					// the frame arrived (so mid-turn tool calls — e.g. inside a
					// sub-agent — see it without waiting for this blocked loop);
					// re-applying here is idempotent and keeps this path as the
					// canonical update that also emits the status echo.
					cs.applySetMode(cf)
					conn.write(cs.statusFrame(s))
				case "clear":
					cs.history = nil
					cs.tokensIn, cs.tokensOut, cs.turns = 0, 0, 0
					cs.cachedTokens, cs.reasoningTokens = 0, 0
					cs.sessionID = ""
					cs.seq = 0
					// COST1: /clear also resets the ledger so the next turn's
					// cost reflects only the post-clear conversation. The DB
					// row's billed columns will catch up on the next turn's
					// UpdateSessionMeta call.
					cs.resetBilling(s)
					conn.write(cs.statusFrame(s))
				case "get_status":
					conn.write(cs.statusFrame(s))
				case "compact":
					compactNow(connCtx, s, models, conn, &cs)
				case "list_mcp":
					handleMCPAction(s, conn, "", "list")
				case "mcp_action":
					handleMCPAction(s, conn, cf.MCPServer, cf.MCPAction)
				case "session_list":
					handleSessionList(s, conn)
				case "restore_session":
					handleRestoreSession(s, conn, &cs, cf.ID)
				case "rename_session":
					handleRenameSession(s, conn, &cs, cf.ID, cf.Text)
				case "archive_session":
					handleArchiveSession(s, conn, &cs, cf.ID)
				case "unarchive_session":
					handleUnarchiveSession(s, conn, cf.ID)
				case "delete_session":
					handleDeleteSession(s, conn, &cs, cf.ID)
				case "session_list_archived":
					handleArchivedSessionList(s, conn)
				case "fork_session":
					handleForkSession(s, conn, &cs, cf.Seq)
				case "enter_side":
					if err := cs.enterSide(); err != nil {
						conn.write(proto.NewError("enter_side: " + err.Error()))
						continue
					}
					conn.write(proto.NewSideState(len(cs.sideStack)))
				case "exit_side":
					cs.exitSide()
					conn.write(proto.NewSideState(len(cs.sideStack)))
				case "list_skills":
					handleListSkills(s, conn)
				case "install_skill":
					handleInstallSkill(s, conn, cf.Source)
				case "uninstall_skill":
					handleSkillMutation(s, conn, "uninstall", cf.Name)
				case "trust_skill":
					handleSkillMutation(s, conn, "trust", cf.Name)
				case "untrust_skill":
					handleSkillMutation(s, conn, "untrust", cf.Name)
				case "enable_skill":
					handleSkillMutation(s, conn, "enable", cf.Name)
				case "disable_skill":
					handleSkillMutation(s, conn, "disable", cf.Name)
				case "permissions_list":
					// Task 9: list all approval rules visible to this connection's
					// session scope. Each rule is rendered as a flat
					// PermissionInfo (Scope becomes a JSON string for client
					// portability). An absent approval manager replies with an
					// empty list rather than an error so legacy clients still
					// render /permissions cleanly.
					if s.approvals == nil {
						conn.write(proto.NewPermissions(nil))
						break
					}
					rules := s.approvals.List(connectionSessionID, time.Now())
					items := make([]proto.PermissionInfo, 0, len(rules))
					for _, rule := range rules {
						items = append(items, proto.PermissionInfo{
							ID:        rule.ID,
							Action:    rule.Action,
							Scope:     scopeJSON(rule.Scope),
							TTL:       string(rule.TTL),
							Source:    string(rule.Source),
							CreatedAt: proto.UnixOrZero(rule.CreatedAt),
							ExpiresAt: proto.UnixOrZero(rule.ExpiresAt),
						})
					}
					conn.write(proto.NewPermissions(items))
				case "permission_revoke":
					// Task 9: remove the rule with cf.ID. Replies with a
					// permission_rule_hit frame carrying Kind="revoke" on
					// success, or an error frame if the manager is unavailable
					// / rule not found. The TUI renders the revoke event as a
					// one-line audit so the operator sees it land.
					if s.approvals == nil {
						conn.write(proto.NewError("approval manager unavailable"))
						break
					}
					if err := s.approvals.Revoke(connectionSessionID, cf.ID); err != nil {
						conn.write(proto.NewError(err.Error()))
						break
					}
					conn.write(proto.NewPermissionRuleHit(cf.ID, "", "", "revoke"))
				case "jobs_list":
					// Task 22: /jobs TUI command — list every job the shell
					// manager currently tracks (live OR stale after
					// RestoreJobs). An absent shellManager replies with an
					// empty list rather than an error so legacy clients render
					// /jobs cleanly.
					if s.shellManager == nil {
						conn.write(proto.NewJobs(nil))
						break
					}
					items := make([]proto.JobInfo, 0)
					for _, job := range s.shellManager.ListJobs() {
						items = append(items, jobInfo(job))
					}
					conn.write(proto.NewJobs(items))
				case "job_read":
					// Task 22: read up to max bytes of a job's accumulated
					// output. cf.Text carries the max-bytes hint as a decimal
					// string; missing/garbage values default to 4096.
					if s.shellManager == nil {
						conn.write(proto.NewError("shell manager unavailable"))
						break
					}
					max, _ := strconv.Atoi(cf.Text)
					if max <= 0 {
						max = 4096
					}
					out, err := s.shellManager.ReadJob(cf.ID, max)
					if err != nil {
						conn.write(proto.NewError(err.Error()))
						break
					}
					conn.write(proto.NewJobEvent(proto.JobInfo{ID: cf.ID, Output: out, State: string(shell.StateRunning)}))
				case "job_write":
					// Task 22: write stdin to a job's session. Authorized as
					// task_shell_stdin so the profile's tool allowlist gates
					// this independently of the read-only jobs_list.
					if s.shellManager == nil {
						conn.write(proto.NewError("shell manager unavailable"))
						break
					}
					if err := s.authorizeControlAction(connCtx, connectionSessionID, &cs, "task_shell_stdin"); err != nil {
						conn.write(proto.NewError(err.Error()))
						break
					}
					if _, err := s.shellManager.Write(cf.ID, []byte(cf.Text)); err != nil {
						conn.write(proto.NewError(err.Error()))
					}
				case "job_cancel":
					// Task 22: cancel a job. Authorized as task_shell_cancel.
					if s.shellManager == nil {
						conn.write(proto.NewError("shell manager unavailable"))
						break
					}
					if err := s.authorizeControlAction(connCtx, connectionSessionID, &cs, "task_shell_cancel"); err != nil {
						conn.write(proto.NewError(err.Error()))
						break
					}
					if err := s.shellManager.Cancel(cf.ID); err != nil {
						conn.write(proto.NewError(err.Error()))
					}
				case "features_list":
					// OBS3: reply with the full flag table. nil registry → empty
					// rows so the TUI can still render the table as "no flags
					// registered" (e.g. an older backend that predates OBS3).
					conn.write(proto.NewFeaturesReply(featureRows(s.featuresReg)))
				case "features_set":
					// OBS3: toggle one flag. The *bool payload preserves explicit
					// false (constraint 8); nil payload means the client violated
					// the wire contract. setFeature returns the underlying error
					// verbatim so the operator sees the exact reason.
					if cf.FeaturesSet == nil {
						conn.write(proto.NewError("features_set payload is required"))
						continue
					}
					if err := setFeature(s.featuresReg, *cf.FeaturesSet); err != nil {
						conn.write(proto.NewError(err.Error()))
						continue
					}
					conn.write(proto.NewFeaturesReply(featureRows(s.featuresReg)))
				case "user_message":
					runUserTurn(cf)
				case "list_seams":
					handleListSeams(s, conn, &cs)
				case "restore_turn":
					handleRestoreTurn(s, conn, &cs, cf.ID, cf.ConfirmedHead)
				default:
					// Invalid frame (empty Type); Text holds the parse error.
					msg := "invalid frame"
					if cf.Text != "" {
						msg = "invalid frame: " + cf.Text
					}
					conn.write(proto.NewError(msg))
				}
			}
		}
	})
}
