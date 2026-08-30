// internal/api/http/ws_handlers.go
//
// Session, skill, and MCP WebSocket handlers, plus the wsConn wrapper.
// Split from ws.go to keep that file under the 1000 pure-code-line cap.
// Contains session CRUD handlers (list/restore/fork/rename/archive/delete),
// skill management handlers (list/install/mutation), the MCP action handler,
// and the wsConn write wrapper. The model-registry name helpers live in
// internal/llm/eino (einollm.SortedModelNames / ResolveModelName) so the SSE
// and /api/v1 turn paths share the same fallback rule.

package http

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cloudwego/eino/schema"
	"github.com/gorilla/websocket"

	"github.com/x6nux/yanshi/internal/ctxcompact"
	einollm "github.com/x6nux/yanshi/internal/llm/eino"
	"github.com/x6nux/yanshi/internal/mcp"
	"github.com/x6nux/yanshi/internal/proto"
	"github.com/x6nux/yanshi/internal/secrets"
	"github.com/x6nux/yanshi/internal/skills"
	"github.com/x6nux/yanshi/internal/store"
	"github.com/x6nux/yanshi/internal/tools"
)

// sessionInfos projects stored session rows into the wire shape.
//
// Extracted because handleSessionList and handleArchivedSessionList carried
// byte-identical copies of it, and the cost gate below has to apply to both:
// a flag honoured on one of two identical projections is worse than one
// honoured on neither, because the difference looks like a data bug.
//
// observe.cost_in_status (OBS3) governs the two cost fields here for the same
// reason it governs statusFrame — /stats renders session cost, so leaving it
// populated here would keep showing spend after the operator switched cost
// reporting off. The STORED ledger is untouched: turning a display off must
// not lose accounting, so store.BillingMeta (connSession.billingMeta) stays
// unconditional and switching the flag back on shows the full history.
func (s *Server) sessionInfos(sessions []store.SessionSummary) []proto.SessionInfo {
	showCost := s.featuresReg.EnabledOrDefault("observe.cost_in_status")
	info := make([]proto.SessionInfo, 0, len(sessions))
	for _, ss := range sessions {
		count, _ := s.store.SessionMessageCount(ss.ID)
		row := proto.SessionInfo{
			ID:              ss.ID,
			Title:           ss.Title,
			CreatedAt:       ss.CreatedAt,
			UpdatedAt:       ss.UpdatedAt,
			MsgCount:        count,
			Model:           ss.Model,
			Thinking:        ss.Thinking,
			TokensIn:        ss.TokensIn,
			TokensOut:       ss.TokensOut,
			CachedTokens:    ss.CachedTokens,
			ReasoningTokens: ss.ReasoningTokens,
			Turns:           ss.Turns,
		}
		if showCost {
			row.CostUSD = ss.CostUSD
			row.CostKnown = ss.CostKnown
		}
		info = append(info, row)
	}
	return info
}

// handleSessionList replies to a session_list frame with the stored sessions.
//
// ONE PAGE, THROUGH THE KEYSET READER (W-D-10). The previous read was
// store.ListSessions(0) — every active session, with no bound of any kind — and
// each row then costs a SessionMessageCount query in sessionInfos, so a
// long-lived project paid an unbounded fan-out and shipped an unbounded frame
// every time anyone opened /sessions. store.ListSessionsPage is the reader that
// exists for this and clamps through the same clampLimit the message log uses,
// so the bound cannot drift into a second number.
//
// THE TRUNCATION IS REPORTED, not silent. A prefix rendered as the whole list is
// how "my old session disappeared" becomes a data-loss report about a store that
// never lost anything.
func handleSessionList(s *Server, conn *wsConn) {
	if s.store == nil {
		conn.write(proto.NewSessions(nil, ""))
		return
	}
	page, err := s.store.ListSessionsPage("", store.MaxMessagePageSize)
	if err != nil {
		conn.write(proto.NewSessions(nil, ""))
		return
	}
	var note string
	if page.NextCursor != "" {
		note = fmt.Sprintf("showing the %d most recently updated sessions; older ones are "+
			"not listed", len(page.Sessions))
	}
	conn.write(proto.NewSessions(s.sessionInfos(page.Sessions), note))
}

// restoreMessages turns a persisted message log back into a ReAct history.
//
// This function exists to fix the bug it describes: the restore loop used to
// map only Role + Content, and it split role into just user/assistant, so
// the ToolCallID / ToolName / ToolArgs that store.Message already carries
// were dropped on every restore, and a store.RoleToolResult row was restored
// as if the operator had typed it. After a resume the model could not see
// the tools it had called, and read its own past tool output as user input.
//
// The role vocabulary here is store.RoleUser / store.RoleAssistant /
// store.RoleToolCall / store.RoleToolResult (internal/store/message_log.go),
// not the raw strings "user"/"assistant"/"tool" — an earlier version of this
// function matched "tool" and put ToolCallID on an "assistant" row, neither
// of which any production writer ever produces (storeMessagesFor, the only
// writer of tool rows, always uses the RoleToolCall/RoleToolResult
// constants), so that version was a no-op against real data despite passing
// tests built on an invented fixture.
//
// Pairing is load-bearing: a RoleToolCall row's ToolCallID and its matching
// RoleToolResult row's ToolCallID must both survive restore. Restoring only
// one side of the pair creates an orphan that the next compaction's
// ctxcompact.EnforceToolCallPairs fixpoint deletes — a failure with no
// error and no log, only a history that silently got shorter after resume.
//
// Regrouping is also load-bearing for parallel tool calls. storeMessagesFor
// writes ONE RoleToolCall row per call (see its doc comment): a live
// assistant message with N parallel ToolCalls becomes an optional prose row
// followed by N consecutive RoleToolCall rows, nothing interleaved. A naive
// restore that turns each row back into its own one-call assistant message
// would hand the provider N separate assistant messages before any tool
// result appears, which is not the shape providers accept (a turn's
// tool_calls must live on a single assistant message, immediately followed
// by their results). This function instead merges a run of RoleToolCall
// rows into the single preceding assistant message when there is one, and
// into one fresh assistant message otherwise — reconstructing the original
// one-message-many-calls shape rather than fragmenting it.
func restoreMessages(msgs []store.Message) []*schema.Message {
	out := make([]*schema.Message, 0, len(msgs))
	for _, m := range msgs {
		switch m.Role {
		case store.RoleToolResult:
			out = append(out, &schema.Message{
				Role:       schema.Tool,
				Content:    m.Content,
				ToolCallID: m.ToolCallID,
				ToolName:   m.ToolName,
			})
		case store.RoleToolCall:
			tc := schema.ToolCall{
				ID: m.ToolCallID,
				Function: schema.FunctionCall{
					Name:      m.ToolName,
					Arguments: m.ToolArgs,
				},
			}
			if n := len(out); n > 0 && out[n-1].Role == schema.Assistant {
				out[n-1].ToolCalls = append(out[n-1].ToolCalls, tc)
				continue
			}
			out = append(out, &schema.Message{Role: schema.Assistant, ToolCalls: []schema.ToolCall{tc}})
		default:
			out = append(out, &schema.Message{Role: restoreRole(m.Role), Content: m.Content})
		}
	}
	return out
}

// restoreRole maps a persisted store.Role* value to schema.RoleType for the
// roles that carry no tool metadata (restoreMessages handles
// store.RoleToolCall / store.RoleToolResult itself, since those also decide
// message grouping, not just a role tag).
//
// An unrecognized value falls to User, the fail-safe side: treating an
// unrecognized row as user input is safer than treating it as assistant
// (the model would believe it said that itself) or as tool (it would become
// a pairing orphan).
func restoreRole(role string) schema.RoleType {
	if role == store.RoleAssistant {
		return schema.Assistant
	}
	return schema.User
}

// handleRestoreSession replies to a restore_session frame by loading the
// session from the store and populating the connSession.
func handleRestoreSession(s *Server, conn *wsConn, cs *connSession, sessionID string) {
	if s.store == nil || sessionID == "" {
		conn.write(proto.NewError("session recording is disabled"))
		conn.write(proto.NewDone())
		return
	}
	// Verify the session exists.
	ss, err := s.store.GetSession(sessionID)
	if err != nil || ss == nil {
		conn.write(proto.NewError("session not found"))
		conn.write(proto.NewDone())
		return
	}

	// Two reads, because the two consumers want different things (INF3 /
	// ADR-0015). The client is rebuilding a TRANSCRIPT — the user is looking for
	// the conversation they left, and handing them a compacted window would read
	// as "restore deleted my history". The model is being handed a CONTEXT
	// WINDOW, and that must be the projection: Messages() returns the
	// pre-compaction originals AND the summary that superseded them, so
	// restoring the model's window from it undoes every compaction the session
	// ever ran and leaves the window bigger than it was before compacting.
	// Measured on this path: 11 messages, compacted to 4, restored as 11.
	//
	// The predecessor comment noted that hist and csHist were once built
	// independently and merely happened to agree; they now differ on purpose,
	// and each is derived from exactly one read so neither can drift.
	msgs, err := s.store.Messages(sessionID)
	if err != nil {
		conn.write(proto.NewError("failed to load session"))
		conn.write(proto.NewDone())
		return
	}
	window, err := s.store.ProjectWindow(sessionID)
	if err != nil {
		conn.write(proto.NewError("failed to load session"))
		conn.write(proto.NewDone())
		return
	}

	// Build history as schema.Message slice (not pointers) for the frame.
	hist := make([]schema.Message, 0, len(msgs))
	for _, m := range restoreMessages(msgs) {
		hist = append(hist, *m)
	}

	// Populate the connSession with stored meta.
	cs.history = restoreMessages(window)
	cs.sessionID = sessionID
	// The durable watermark is the whole log, not the window: commitCompaction
	// reads cs.seq as a log coordinate.
	cs.seq = len(msgs)
	cs.model = ss.Model
	cs.thinking = ss.Thinking
	cs.tokensIn = ss.TokensIn
	cs.tokensOut = ss.TokensOut
	cs.cachedTokens = ss.CachedTokens
	cs.reasoningTokens = ss.ReasoningTokens
	cs.turns = ss.Turns
	// COST1: restore the ledger to match. The post-restore turn continues
	// accumulating from the persisted spend; renderers (TUI footer, /cost)
	// read the same costUSD/costKnown pair so the user sees a faithful total.
	cs.billing.Billed = einollm.BilledUsage{
		InputTokens:  ss.BilledInputTokens,
		CachedTokens: ss.BilledCachedTokens,
		OutputTokens: ss.BilledOutputTokens,
	}
	cs.costUSD = ss.CostUSD
	cs.costKnown = ss.CostKnown
	cs.hasBilledUsage = ss.BilledInputTokens+ss.BilledCachedTokens+ss.BilledOutputTokens > 0

	// Reply with the restored session state. Carry CostUSD/CostKnown so the
	// TUI doesn't briefly render "$0.0000" between restore and first usage.
	restored := proto.NewSessionRestored(sessionID, hist, cs.model, cs.thinking, cs.tokensIn, cs.tokensOut, cs.turns)
	if s.featuresReg.EnabledOrDefault("observe.cost_in_status") {
		restored.CostUSD = cs.costUSD
		restored.CostKnown = cs.costKnown
	}
	conn.write(restored)
}

// handleForkSession copies the current session (or prefix), then switches the
// SAME connSession to the fork before acknowledging it. This keeps TUI and
// server persistence aligned: the next user turn writes to forkID.
//
// Two ways to pick the target, mutually exclusive (W-E-11):
//
//   - turnsBack > 0: resolved via resolveRollbackSeq into a concrete seq,
//     which may land on store.ForkSession's -2 "empty fork" sentinel (rolling
//     back to before the session's very first message — see that function's
//     doc comment for why -1 cannot express this). seq is ignored.
//   - turnsBack == 0: seq is used directly, unified with store.ForkSession:
//     -1  = fork all messages.
//     >=0 = fork messages[0..seq] (inclusive).
//     Anything else — including -2, the resolver-only empty-fork sentinel —
//     is rejected right here. A plain client-supplied seq must never reach
//     -2's store-level meaning; only resolveRollbackSeq may produce it.
func handleForkSession(s *Server, conn *wsConn, cs *connSession, seq, turnsBack int) {
	if s.store == nil || cs.sessionID == "" {
		conn.write(proto.NewError("session recording is disabled"))
		return
	}
	if turnsBack > 0 {
		resolved, err := resolveRollbackSeq(s.store, cs.sessionID, turnsBack)
		if err != nil {
			conn.write(proto.NewError("fork: " + err.Error()))
			return
		}
		seq = resolved
	} else if seq < -1 {
		conn.write(proto.NewError(fmt.Sprintf("fork: invalid seq %d (want -1 for all, or >=0 for inclusive upper bound)", seq)))
		return
	}
	forkID, err := s.store.ForkSession(cs.sessionID, seq)
	if err != nil {
		conn.write(proto.NewError("fork: " + err.Error()))
		return
	}
	// connSession.loadSession restores history/model/thinking and the reset
	// fork usage counters from DB without emitting a second control frame.
	if err := cs.loadSession(s, forkID); err != nil {
		conn.write(proto.NewError("fork created but switch failed: " + err.Error()))
		return
	}
	conn.write(proto.NewSessionForked(forkID))
}

// resolveRollbackSeq translates "roll back turnsBack user turns" (W-E-11)
// into the seq handleForkSession/store.ForkSession need: the seq immediately
// BEFORE the target turn's own row, so the fork's history ends right where
// that turn began. turnsBack=1 targets the most recent user message,
// turnsBack=2 the one before it, and so on.
//
// Walking from the end (not the start) is what makes "N-th user turn back"
// well-defined without the caller having to know how many turns exist.
//
// The target's Seq-1 collides with store.ForkSession's -1 ("copy all") when
// the target is the session's very first message (Seq==0, so Seq-1==-1) —
// that must mean "copy nothing", not "copy everything", so this returns -2,
// ForkSession's dedicated empty-fork sentinel, for that one case.
func resolveRollbackSeq(st *store.Store, sessionID string, turnsBack int) (int, error) {
	msgs, err := st.Messages(sessionID)
	if err != nil {
		return 0, fmt.Errorf("resolveRollbackSeq: %w", err)
	}
	remaining := turnsBack
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role != store.RoleUser {
			continue
		}
		// RE-14 (fix-e3a): a compaction summary is persisted as a plain
		// store.RoleUser row (ws_compaction.go writes it through the same
		// path as any other turn) but is never rendered as a user turn in
		// the TUI — internal/cli/tui/model.go's history-load filters out
		// any msg.Role=="user" whose Content has this prefix before turning
		// rows into userEntry values. rollbackCandidates() (client) counts
		// only rendered userEntry values, so it never sees this row; before
		// this skip, resolveRollbackSeq (server) counted it anyway, and the
		// two turnsBack counters disagreed for any session that had ever
		// been compacted. See
		// TestResolveRollbackSeq_SkipsCompactionSummaryRow for the
		// regression this closes.
		if strings.HasPrefix(msgs[i].Content, ctxcompact.SummarySentinel) {
			continue
		}
		remaining--
		if remaining == 0 {
			if msgs[i].Seq == 0 {
				return -2, nil
			}
			return msgs[i].Seq - 1, nil
		}
	}
	return 0, fmt.Errorf("resolveRollbackSeq: turnsBack %d exceeds the session's %d user turn(s)", turnsBack, turnsBack-remaining)
}

// runDistillPass runs one memory-consolidation pass over dims and reports the
// outcome by logging, never by returning the underlying error (A2/W-A-05).
//
// It backs BOTH call sites: the synchronous distill_memories handler below
// (which also wants the DistillResult, to build the reply frame) and the
// automatic post-turn pass (ws.go's runUserTurn), which fires this in a
// goroutine and discards the return values entirely. The two correctness
// requirements this file exists to satisfy — "distillation failure must not
// affect the turn" for the background path, and "don't scare the user with
// an error for a background-safe, always-retryable operation" for the
// interactive path — collapse into the same rule if the error never leaves
// this function: nothing downstream has to remember to swallow it.
// tools.DistillMemories is itself failure-safe by construction (see its doc
// comment: every failure path leaves the original memories untouched), so
// logging and returning a zero result on error loses nothing a retry
// wouldn't recover.
func runDistillPass(ctx context.Context, st *store.Store, m tools.DistillModel, dims store.MemoryFilter) (tools.DistillResult, error) {
	res, err := tools.DistillMemories(ctx, st, m, dims)
	if err != nil {
		slog.Warn("memory distillation failed", "session", dims.SessionID, "error", err)
		return tools.DistillResult{}, nil
	}
	return res, nil
}

// distillTimeout bounds one consolidation pass.
//
// It is a var, not a const, only so the wedge regression test can shorten it;
// nothing in production assigns to it.
//
// A distillation pass is one provider call over a bounded candidate set, so
// minutes is already generous. The bound is not about latency, it is about
// what an UNbounded call does at each of the two call sites: the interactive
// one runs synchronously inside the WS frame loop, so a provider that accepts
// the connection and then says nothing stops every other control frame on that
// connection — /model, /compact, cancel, permission replies; the post-turn one
// runs detached under context.WithoutCancel, so it leaks one goroutine and one
// in-flight request per turn instead. ResilientChatModel's stall watchdog does
// NOT cover this: W-A-06 wraps Stream, and distillation calls Generate.
var distillTimeout = 3 * time.Minute

// handleDistillMemories replies to a distill_memories frame by running one
// consolidation pass over the connSession's stored memories (A2/W-A-05).
// This is the interactive half of the entry point /distill triggers; see
// runUserTurn in ws.go for the automatic post-turn half. Both wire up the
// same previously-uncalled tools.DistillMemories + store.ApplyDistillation
// chain documented in docs/feature-status.yaml under A2/W-A-05.
//
// ctx MUST be the connection context. This handler runs inline on the frame
// loop, so its context is the only thing that can release the loop when the
// client goes away: the loop's own `case <-connCtx.Done()` is unreachable
// while it is blocked in here. The first version passed context.Background(),
// which cannot be cancelled by anything — closing the client left the whole
// control channel wedged behind a stalled provider for the life of the
// process. That is the W-A-06 defect class, reintroduced by W-A-05 one commit
// later and one scope wider.
func handleDistillMemories(ctx context.Context, s *Server, conn *wsConn, cs *connSession) {
	if s.store == nil || s.distillModel == nil {
		conn.write(proto.NewError("memory distillation is disabled"))
		return
	}
	ctx, cancel := context.WithTimeout(ctx, distillTimeout)
	defer cancel()
	res, _ := runDistillPass(ctx, s.store, s.distillModel, store.MemoryFilter{SessionID: cs.sessionID})
	conn.write(proto.NewMemoriesDistilled(res.Considered, res.Merged))
}

// handleClearMemories deletes memories in one dimension (W-D-12).
//
// THE SCOPE IS RESOLVED HERE, FAIL-CLOSED, AND AN UNKNOWN ONE IS AN ERROR
// RATHER THAN A FALLBACK. store.ClearMemories treats a zero filter as "delete
// everything", so any path that turns an unrecognised word into a zero filter
// has turned a typo into a full wipe. memoryFilterFor makes the same choice on
// the read side for a much cheaper reason (a wrong answer); here the cost of
// the same mistake is the whole table.
//
// The confirmation is the client's (see the TUI's /memory-clear). That split is
// the one /delete already uses: the token never reaches the wire, so there is
// no protocol state to get out of step, and the server's job is to refuse
// anything it cannot name rather than to re-ask.
func handleClearMemories(s *Server, conn *wsConn, cs *connSession, scope, agentID string) {
	if s.store == nil {
		conn.write(proto.NewError("memory is disabled"))
		return
	}
	var dims store.MemoryFilter
	switch scope {
	case proto.MemoryClearAll:
		// The zero filter: every memory in this project's database.
	case proto.MemoryClearSession:
		if cs.sessionID == "" {
			conn.write(proto.NewError("clear_memories: this connection has no stored session"))
			return
		}
		dims.SessionID = cs.sessionID
	case proto.MemoryClearAgent:
		if agentID == "" {
			conn.write(proto.NewError("clear_memories: the agent scope needs an agent id"))
			return
		}
		dims.AgentID = agentID
	default:
		conn.write(proto.NewError("clear_memories: unknown scope " + strconv.Quote(scope)))
		return
	}
	n, err := s.store.ClearMemories(dims)
	if err != nil {
		conn.write(proto.NewError("clear_memories: " + err.Error()))
		return
	}
	conn.write(proto.NewMemoriesCleared(n, clearedButRetainedNote(s.store, n)))
}

// clearedButRetainedNote tells the user what the wipe did NOT reach.
//
// A bare "cleared N memories" reads as erasure, and it is not one: W-D-06 gzips
// the WHOLE memories table into every checkpoint, so anything cleared here is
// still on disk in those blobs and comes back verbatim from a plain
// `/checkpoint restore <id> memory yes`. Shredding the blobs instead was
// rejected — it would delete the "undo an accidental wipe" the checkpoint
// feature exists for, and would still not be erasure, since the text a memory
// was distilled from stays in `messages`. See store.ClearMemories.
//
// So the honest move is to say so, once, at the only moment the user is looking:
// they can then take the extra step (/checkpoint list) if erasure is what they
// meant. Silent when nothing was deleted or no checkpoint exists — a warning
// about a copy that is not there is noise, and noise is how a real warning stops
// being read.
func clearedButRetainedNote(st *store.Store, deleted int) string {
	if st == nil || deleted == 0 {
		return ""
	}
	cps, err := st.Checkpoints(1)
	if err != nil || len(cps) == 0 {
		return ""
	}
	return "note: checkpoints still hold a copy of these memories and can restore them " +
		"(/checkpoint list). Clearing does not erase the text — the conversations they " +
		"were derived from are also unchanged."
}

// skillInfo converts an internal skills.Skill snapshot into a proto.SkillInfo
// for wire transport. Returns nil when sk is nil so it can be used inline.
func skillInfo(sk *skills.Skill) *proto.SkillInfo {
	if sk == nil {
		return nil
	}
	return &proto.SkillInfo{
		Name: sk.Name, Description: sk.Description, Source: sk.Source,
		Enabled: sk.Enabled, Trusted: sk.Trusted, Missing: sk.Missing,
	}
}

// attachShadowed folds the registry's conflict records into the wire rows.
//
// Done here rather than inside skillInfo because a Conflict is a property of
// the LOAD, not of a Skill: the same skill is unshadowed in a project that has
// no competing copy. Keeping it out of the struct also means the ack paths,
// which describe one skill in isolation, do not have to pretend to know.
func attachShadowed(rows []proto.SkillInfo, conflicts []skills.Conflict) []proto.SkillInfo {
	if len(conflicts) == 0 {
		return rows
	}
	byName := map[string][]proto.ShadowedSkill{}
	for _, c := range conflicts {
		byName[c.Name] = append(byName[c.Name], proto.ShadowedSkill{
			Source: c.ShadowedSource, Dir: c.ShadowedDir,
		})
	}
	for i := range rows {
		rows[i].Shadowed = byName[rows[i].Name]
	}
	return rows
}

// handleValidateSkill re-runs the install-time checks against skills already
// on disk, which nothing could do before: the rules lived inline in Install,
// so a skill edited by hand after installation was unverifiable.
func handleValidateSkill(s *Server, conn *wsConn, name string) {
	if s.skillsRegistry == nil {
		conn.write(proto.NewSkillAck("validated", nil, "skills are disabled"))
		return
	}
	targets := s.skillsRegistry.List()
	if name != "" {
		sk, ok := s.skillsRegistry.Get(name)
		if !ok {
			conn.write(proto.NewSkillAck("validated", nil, "no such skill: "+name))
			return
		}
		targets = []*skills.Skill{sk}
	}
	var problems []string
	for _, sk := range targets {
		if err := skills.ValidateSkillDir(sk.Dir); err != nil {
			problems = append(problems, sk.Name+": "+err.Error())
		}
	}
	if len(problems) > 0 {
		conn.write(proto.NewSkillAck("validated", nil, strings.Join(problems, "; ")))
		return
	}
	conn.write(proto.NewSkillAck("validated", nil,
		fmt.Sprintf("%d skill(s) valid", len(targets))))
}

// handleListSkills replies with the current registry snapshot (E03).
func handleListSkills(s *Server, conn *wsConn) {
	if s.skillsRegistry == nil {
		conn.write(proto.NewSkillsList(nil))
		return
	}
	list := s.skillsRegistry.List() // mutex-protected snapshots (Task 11)
	out := make([]proto.SkillInfo, 0, len(list))
	for _, sk := range list {
		out = append(out, *skillInfo(sk))
	}
	conn.write(proto.NewSkillsList(attachShadowed(out, s.skillsRegistry.Conflicts())))
}

// handleInstallSkill publishes into the writable user root, then Reloads via
// the ORIGINAL all-roots loader (FN1). Registry/list/explicit skill_use update
// immediately; the running orchestrator's baked discovery prompt needs restart.
//
// Routing between the two registries (github: via git, https:// via archive
// download) happens inside skills.InstallAny rather than here, so the WS layer
// has one install verb and cannot come to disagree with the TUI or the docs
// about which sources exist.
func handleInstallSkill(s *Server, conn *wsConn, src string) {
	if s.skillsRegistry == nil || s.skillsLoader == nil || s.skillsDstRoot == "" {
		conn.write(proto.NewSkillAck("installed", nil, "skill install is disabled (registry/loader/dstRoot unavailable)"))
		return
	}
	name, err := skills.InstallAny(src, s.skillsDstRoot, s.skillsCloner, s.skillsFetcher)
	if err != nil {
		conn.write(proto.NewSkillAck("installed", nil, err.Error()))
		return
	}
	if err := s.skillsRegistry.Reload(s.skillsLoader); err != nil {
		conn.write(proto.NewSkillAck("installed", nil, "installed but all-roots reload failed: "+err.Error()))
		return
	}
	sk, ok := s.skillsRegistry.Get(name)
	if !ok {
		conn.write(proto.NewSkillAck("installed", nil,
			"installed but all-roots reload did not expose the new skill"))
		return
	}
	if sk.Source != "user" {
		// First-seen-wins may leave a same-name builtin/plugin active. Full
		// conflict diagnostics/source-prefix selection are SC1 scope cuts; do
		// not falsely ack the shadowed user copy as the active skill.
		conn.write(proto.NewSkillAck("installed", skillInfo(sk),
			fmt.Sprintf("installed user copy %q but active entry is from %s; restart will not change root precedence", name, sk.Source)))
		return
	}
	conn.write(proto.NewSkillAck("installed", skillInfo(sk), ""))
}

// skillMutationAction is the explicit canonical action name map. Required
// because mutation+"ed" would produce "enableed"/"disableed" (FN6).
var skillMutationAction = map[string]string{
	"uninstall": "uninstalled",
	"trust":     "trusted",
	"untrust":   "untrusted",
	"enable":    "enabled",
	"disable":   "disabled",
}

// handleSkillMutation routes trust/untrust/enable/disable/uninstall through
// the registry (and the original loader for uninstall's Reload).
func handleSkillMutation(s *Server, conn *wsConn, mutation, name string) {
	action, known := skillMutationAction[mutation]
	if !known {
		conn.write(proto.NewSkillAck("", nil, fmt.Sprintf("unknown mutation %q", mutation)))
		return
	}
	if s.skillsRegistry == nil {
		conn.write(proto.NewSkillAck(action, nil, "skill registry is nil"))
		return
	}

	// Capture the target before uninstall so the ack can carry Skill.Name even
	// after the registry entry is gone (CB4: no ServerFrame.Name).
	before, exists := s.skillsRegistry.Get(name)
	if !exists {
		conn.write(proto.NewSkillAck(action, nil, fmt.Sprintf("unknown skill %q", name)))
		return
	}
	// Install/uninstall owns only the writable user root. With first-seen-wins
	// a builtin/plugin entry may shadow a same-name user directory; source-
	// prefix disambiguation is an explicit SC1 scope cut, so fail rather than
	// deleting a path that is not the active user skill.
	if mutation == "uninstall" && before.Source != "user" {
		conn.write(proto.NewSkillAck(action, skillInfo(before),
			fmt.Sprintf("skill %q comes from %s; only active user skills can be uninstalled", name, before.Source)))
		return
	}
	var err error
	switch mutation {
	case "uninstall":
		if s.skillsLoader == nil || s.skillsDstRoot == "" {
			err = fmt.Errorf("skill uninstall is disabled (loader/dstRoot unavailable)")
			break
		}
		err = skills.Uninstall(name, s.skillsDstRoot)
		if err == nil {
			err = s.skillsRegistry.Reload(s.skillsLoader) // FN1: all roots
		}
	case "trust":
		err = s.skillsRegistry.Trust(name)
	case "untrust":
		err = s.skillsRegistry.Untrust(name)
	case "enable":
		err = s.skillsRegistry.Enable(name)
	case "disable":
		err = s.skillsRegistry.Disable(name)
	}
	if err != nil {
		conn.write(proto.NewSkillAck(action, skillInfo(before), err.Error()))
		return
	}
	after, _ := s.skillsRegistry.Get(name)
	if mutation == "uninstall" {
		after = before // preserve identity for the ack even though registry dropped it
	}
	conn.write(proto.NewSkillAck(action, skillInfo(after), ""))
}

// handleRenameSession renames a stored session. Reply: session_ack{renamed} or
// error when recording is disabled / id empty / title blank. Title is server-
// trimmed and clamped to 200 runes to bound list rendering.
func handleRenameSession(s *Server, conn *wsConn, cs *connSession, sessionID, title string) {
	if s.store == nil || sessionID == "" {
		conn.write(proto.NewError("session recording is disabled"))
		return
	}
	title = strings.TrimSpace(title)
	if title == "" {
		conn.write(proto.NewError("rename: title must be non-empty"))
		return
	}
	if r := []rune(title); len(r) > 200 {
		title = string(r[:200])
	}
	if err := s.store.UpdateSessionTitle(sessionID, title); err != nil {
		conn.write(proto.NewError("rename: " + err.Error()))
		return
	}
	conn.write(proto.NewSessionAck("renamed", sessionID, title))
}

// handleArchiveSession flips a session's archived flag to true (hides it from
// the active list without deleting). Reply: session_ack{archived}.
func handleArchiveSession(s *Server, conn *wsConn, cs *connSession, sessionID string) {
	if s.store == nil || sessionID == "" {
		conn.write(proto.NewError("session recording is disabled"))
		return
	}
	if err := s.store.SetSessionArchived(sessionID, true); err != nil {
		conn.write(proto.NewError("archive: " + err.Error()))
		return
	}
	conn.write(proto.NewSessionAck("archived", sessionID, ""))
}

// handleUnarchiveSession restores an archived session to the active list.
// Reply: session_ack{unarchive}.
func handleUnarchiveSession(s *Server, conn *wsConn, sessionID string) {
	if s.store == nil || sessionID == "" {
		conn.write(proto.NewError("session recording is disabled"))
		return
	}
	if err := s.store.SetSessionArchived(sessionID, false); err != nil {
		conn.write(proto.NewError("unarchive: " + err.Error()))
		return
	}
	conn.write(proto.NewSessionAck("unarchive", sessionID, ""))
}

// handleDeleteSession permanently deletes a stored session and its messages
// (transactional). If the deleted session is the connSession's live recording,
// reset history/counters so the client is not left holding a dangling id. The
// TUI confirms before sending; the server executes unconditionally. Reply:
// session_ack{deleted}.
func handleDeleteSession(s *Server, conn *wsConn, cs *connSession, sessionID string) {
	if s.store == nil || sessionID == "" {
		conn.write(proto.NewError("session recording is disabled"))
		return
	}
	if err := s.store.DeleteSession(sessionID); err != nil {
		conn.write(proto.NewError("delete: " + err.Error()))
		return
	}
	if cs.sessionID == sessionID {
		cs.history = nil
		cs.tokensIn, cs.tokensOut, cs.turns = 0, 0, 0
		cs.cachedTokens, cs.reasoningTokens = 0, 0
		cs.sessionID = ""
		cs.seq = 0
	}
	conn.write(proto.NewSessionAck("deleted", sessionID, ""))
}

// handleArchivedSessionList replies with the ARCHIVED sessions (sessions frame),
// so the TUI's /archived can list ids to unarchive. Mirrors handleSessionList
// (active) — only the store query differs.
func handleArchivedSessionList(s *Server, conn *wsConn) {
	if s.store == nil {
		conn.write(proto.NewSessions(nil, ""))
		return
	}
	sessions, err := s.store.ListArchivedSessions(0)
	if err != nil {
		conn.write(proto.NewSessions(nil, ""))
		return
	}
	conn.write(proto.NewSessions(s.sessionInfos(sessions), ""))
}

// wsConn wraps a gorilla WebSocket connection with a write mutex. gorilla/
// websocket allows only ONE concurrent writer: two unsynchronized WriteMessage
// calls interleave bytes and corrupt the frame stream (surfacing as
// "continuation after FIN, bad MASK" on the client read). Writes here originate
// from multiple goroutines — the main loop (emit/onUsage frames) AND the ADK
// worker goroutine (the interactive permission callback and the mid-turn
// compaction callback, both invoked from inside the model/tool execution the
// runner drives on its own goroutine). With a buffered event iterator the ADK
// can push a tool_call event and immediately run the tool (firing the
// permission callback) while the main loop concurrently drains that event and
// writes its frame, so this mutex is load-bearing, not theoretical. Reads stay
// single-goroutine (the reader goroutine), so no read lock is needed.
type wsConn struct {
	*websocket.Conn
	mu       sync.Mutex
	redactor *secrets.Redactor
}

// write marshals f and writes it as one text frame under the mutex. Write
// errors are ignored: the read loop surfaces a closed connection on its next
// ReadMessage.
func (w *wsConn) write(f proto.ServerFrame) {
	data, err := json.Marshal(f)
	if err != nil {
		return
	}
	if w.redactor != nil {
		// Redact the marshaled JSON, including escaped forms of secrets
		// (quotes, backslashes, control chars), so a key like `abc"def` is
		// still replaced after json.Marshal escapes the quote. Operating on
		// marshaled bytes means new ServerFrame fields inherit coverage
		// without per-field work here.
		data = w.redactor.RedactJSON(data)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	_ = w.Conn.WriteMessage(websocket.TextMessage, data)
}

// handleMCPAction dispatches MCP management actions and replies with a
// mcp_status snapshot. name is the server name (empty for list/validate), action
// is one of enable|disable|reload|validate|list.
func handleMCPAction(s *Server, conn *wsConn, name, action string) {
	if s.mcp == nil {
		conn.write(proto.NewMCPStatusFrame(nil))
		return
	}
	ctx := context.Background()
	switch action {
	case "enable":
		_ = s.mcp.Enable(ctx, name)
	case "disable":
		_ = s.mcp.Disable(ctx, name)
	case "reload":
		_ = s.mcp.Reload(ctx, name)
	case "validate":
		s.mcp.Validate(ctx)
	case "list", "":
	}
	conn.write(proto.NewMCPStatusFrame(MCPStatusSnapshot(s.mcp.Snapshot(ctx))))
}

// MCPStatusSnapshot converts the manager's server statuses into the wire shape
// the TUI palette renders from.
//
// Extracted from handleMCPAction so the mapping can be asserted against the
// tool registry. The palette shows td.Qualified and the orchestrator registers
// td.Qualified, but they travel through different code — Snapshot to this
// function to a frame on one side, ListAllTools to NewGuardedTool on the other
// — and "the name the operator sees is the name the model was given" only holds
// while both ends keep agreeing. Inline in a handler it could not be checked
// without a websocket.
func MCPStatusSnapshot(snapshot []mcp.ServerStatus) []proto.MCPServerStatus {
	out := make([]proto.MCPServerStatus, 0, len(snapshot))
	for _, st := range snapshot {
		briefs := make([]proto.MCPToolBrief, 0, len(st.Tools))
		for _, td := range st.Tools {
			briefs = append(briefs, proto.MCPToolBrief{Name: td.Qualified, Description: td.Description})
		}
		out = append(out, proto.MCPServerStatus{
			Name: st.Name, Transport: string(st.Transport),
			Status: string(st.Status), Error: st.Error,
			ToolCount: len(st.Tools), Tools: briefs,
		})
	}
	return out
}
