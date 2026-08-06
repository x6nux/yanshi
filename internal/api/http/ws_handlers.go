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
	"strings"
	"sync"

	"github.com/cloudwego/eino/schema"
	"github.com/gorilla/websocket"

	einollm "github.com/x6nux/yanshi/internal/llm/eino"
	"github.com/x6nux/yanshi/internal/proto"
	"github.com/x6nux/yanshi/internal/secrets"
	"github.com/x6nux/yanshi/internal/skills"
	"github.com/x6nux/yanshi/internal/store"
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
func handleSessionList(s *Server, conn *wsConn) {
	if s.store == nil {
		conn.write(proto.NewSessions(nil))
		return
	}
	sessions, err := s.store.ListSessions(0)
	if err != nil {
		conn.write(proto.NewSessions(nil))
		return
	}
	conn.write(proto.NewSessions(s.sessionInfos(sessions)))
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

	// Load messages.
	msgs, err := s.store.Messages(sessionID)
	if err != nil {
		conn.write(proto.NewError("failed to load session"))
		conn.write(proto.NewDone())
		return
	}

	// Build history as schema.Message slice (not pointers) for the frame.
	hist := make([]schema.Message, 0, len(msgs))
	csHist := make([]*schema.Message, 0, len(msgs))
	for _, m := range msgs {
		role := schema.User
		if m.Role == "assistant" {
			role = schema.Assistant
		}
		msg := schema.Message{Role: role, Content: m.Content}
		hist = append(hist, msg)
		csHist = append(csHist, &msg)
	}

	// Populate the connSession with stored meta.
	cs.history = csHist
	cs.sessionID = sessionID
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
// seq semantics (unified with store.ForkSession):
//
//	-1  = fork all messages.
//	>=0 = fork messages[0..seq] (inclusive).
//	<-1 or > max source seq = error; no fork created.
func handleForkSession(s *Server, conn *wsConn, cs *connSession, seq int) {
	if s.store == nil || cs.sessionID == "" {
		conn.write(proto.NewError("session recording is disabled"))
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

// skillInfo converts an internal skills.Skill snapshot into a proto.SkillInfo
// for wire transport. Returns nil when sk is nil so it can be used inline.
func skillInfo(sk *skills.Skill) *proto.SkillInfo {
	if sk == nil {
		return nil
	}
	return &proto.SkillInfo{
		Name: sk.Name, Description: sk.Description, Source: sk.Source,
		Enabled: sk.Enabled, Trusted: sk.Trusted,
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
func handleInstallSkill(s *Server, conn *wsConn, src string) {
	if s.skillsRegistry == nil || s.skillsLoader == nil || s.skillsDstRoot == "" {
		conn.write(proto.NewSkillAck("installed", nil, "skill install is disabled (registry/loader/dstRoot unavailable)"))
		return
	}
	name, err := skills.Install(src, s.skillsDstRoot, s.skillsCloner)
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
		conn.write(proto.NewSessions(nil))
		return
	}
	sessions, err := s.store.ListArchivedSessions(0)
	if err != nil {
		conn.write(proto.NewSessions(nil))
		return
	}
	conn.write(proto.NewSessions(s.sessionInfos(sessions)))
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
	out := make([]proto.MCPServerStatus, 0, 1)
	for _, st := range s.mcp.Snapshot(ctx) {
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
	conn.write(proto.NewMCPStatusFrame(out))
}
