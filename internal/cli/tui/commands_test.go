package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/cli"
	"github.com/x6nux/yanshi/internal/ctxcompact"
	"github.com/x6nux/yanshi/internal/i18n"
	"github.com/x6nux/yanshi/internal/proto"
)

// ---- T32: command palette ----

// TestModel_CommandHelp_RendersLocally proves /help appends a help entry and
// does NOT round-trip to the server (no frame, no Send).
func TestModel_CommandHelp_RendersLocally(t *testing.T) {
	rec := &recordingSession{}
	m := newModel(rec, "/proj")

	mm, _ := m.runCommand("/help")
	m = mm.(model)

	assert.Empty(t, rec.frames, "/help must not send any frame")
	var found bool
	for _, e := range m.entries {
		if _, ok := e.(helpEntry); ok {
			found = true
		}
	}
	assert.True(t, found, "/help must render a help entry")
}

// TestModel_CommandRoutesSlash proves a "/clear" submission routes to the
// command palette (sends a clear frame) rather than a user_message turn, and
// that an unknown command renders an error.
func TestModel_CommandRoutesSlash(t *testing.T) {
	rec := &recordingSession{}
	m := newModel(rec, "/proj")
	m.input.SetValue("/clear")

	mm, _ := m.submit()
	m = mm.(model)

	require.Len(t, rec.frames, 1, "submit must send exactly one frame")
	assert.Equal(t, "clear", rec.frames[0].Type)
	assert.Empty(t, rec.sentText, "slash input must NOT go through Send (user_message)")

	// Unknown command → local error entry, no frame.
	rec = &recordingSession{}
	m = newModel(rec, "/proj")
	m.input.SetValue("/nope")
	mm, _ = m.submit()
	m = mm.(model)
	assert.Empty(t, rec.frames, "unknown command sends no frame")
	var sawErr bool
	for _, e := range m.entries {
		if ee, ok := e.(errorEntry); ok && ee.text != "" {
			sawErr = true
		}
	}
	assert.True(t, sawErr, "unknown command must render an error entry")
}

// TestModel_PlainTextRoutesToUserMessage proves a non-slash submission still
// goes through Send as a user_message (regression guard on the routing fork).
func TestModel_PlainTextRoutesToUserMessage(t *testing.T) {
	rec := &recordingSession{}
	m := newModel(rec, "/proj")
	m.input.SetValue("hello there")

	mm, _ := m.submit()
	m = mm.(model)

	assert.Empty(t, rec.frames, "plain text must not send a control frame")
	require.Len(t, rec.sentText, 1)
	assert.Equal(t, "hello there", rec.sentText[0])
}

// TestModel_CommandModelThinkSendsFrames asserts the control frames the model
// and think commands emit: /model → list_models; /model foo → set_model(foo);
// /think high → set_thinking(high).
func TestModel_CommandModelThinkSendsFrames(t *testing.T) {
	cases := []struct {
		input string
		want  proto.ClientFrame
	}{
		{"/model", proto.NewListModels()},
		{"/model claude-opus-4", proto.NewSetModel("claude-opus-4")},
		{"/think high", proto.NewSetThinking("high")},
	}
	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			rec := &recordingSession{}
			m := newModel(rec, "/proj")
			mm, _ := m.runCommand(tc.input)
			_ = mm.(model)
			require.Len(t, rec.frames, 1)
			assert.Equal(t, tc.want, rec.frames[0])
		})
	}
}

// ---- T33: /model /think /clear /config rendering ----

// TestModel_StatusUpdatesHeader proves a status event stores model+thinking and
// they appear in the rendered header.
func TestModel_StatusUpdatesHeader(t *testing.T) {
	m := newModel(&fakeSession{}, "/proj")
	m = m.applyEvent(cli.StreamEvent{
		Kind: "status", Model: "claude-opus-4", Thinking: "medium",
		TokensIn: 100, TokensOut: 50, Turns: 2,
	})

	assert.Equal(t, "claude-opus-4", m.modelName)
	assert.Equal(t, "medium", m.thinking)
	assert.Equal(t, 100, m.tokensIn)
	assert.Equal(t, 50, m.tokensOut)
	assert.Equal(t, 2, m.turns)

	hdr := m.statusHeader()
	assert.Contains(t, hdr, "claude-opus-4")
	assert.Contains(t, hdr, "总消耗") // unified consumption segment replaces think/cache
}

// TestModel_ModelsRendered proves a models event appends a modelsEntry carrying
// the list.
func TestModel_ModelsRendered(t *testing.T) {
	m := newModel(&fakeSession{}, "/proj")
	m = m.applyEvent(cli.StreamEvent{Kind: "models", Items: []string{"a-model", "b-model"}})

	var me modelsEntry
	var found bool
	for _, e := range m.entries {
		if x, ok := e.(modelsEntry); ok {
			me, found = x, true
		}
	}
	require.True(t, found, "a models entry must be rendered")
	assert.Equal(t, []string{"a-model", "b-model"}, me.names)
}

// TestModel_CommandClearResetsEntries proves /clear sends the clear frame AND
// wipes the local transcript.
func TestModel_CommandClearResetsEntries(t *testing.T) {
	rec := &recordingSession{}
	m := newModel(rec, "/proj")
	m.entries = append(m.entries, &userEntry{text: "old"}, &toolEntry{name: "fs_read", status: "ok"})
	m.toolsRun = 3

	mm, _ := m.runCommand("/clear")
	m = mm.(model)

	require.Len(t, rec.frames, 1)
	assert.Equal(t, "clear", rec.frames[0].Type)
	assert.Empty(t, m.entries, "/clear must reset local entries")
	assert.Equal(t, 0, m.toolsRun)
}

// TestModel_CommandConfig_StatusBlock proves /config appends a status block,
// sends get_status, and the status reply fills the block in place.
func TestModel_CommandConfig_StatusBlock(t *testing.T) {
	rec := &recordingSession{}
	m := newModel(rec, "/proj")

	mm, _ := m.runCommand("/config")
	m = mm.(model)

	require.Len(t, rec.frames, 1)
	assert.Equal(t, "get_status", rec.frames[0].Type)

	se := findStatusEntry(t, m)
	require.NotNil(t, m.pendingStatus, "status block must be pending until the reply")
	assert.Equal(t, "", se.model, "unfilled block shows default model")

	m = m.applyEvent(cli.StreamEvent{
		Kind: "status", Model: "gpt-4o", Thinking: "low", TokensIn: 42, TokensOut: 7, Turns: 1,
	})
	se = findStatusEntry(t, m)
	assert.Equal(t, "gpt-4o", se.model)
	assert.Equal(t, 42, se.tokensIn)
	assert.Nil(t, m.pendingStatus, "pending block is consumed once filled")
}

func findStatusEntry(t *testing.T, m model) *statusEntry {
	t.Helper()
	for i := len(m.entries) - 1; i >= 0; i-- {
		if se, ok := m.entries[i].(*statusEntry); ok {
			return se
		}
	}
	return nil
}

// ---- T34: /cost /compact /mcp ----

// TestModel_CommandCost_StatusBlock proves /cost appends a status block, sends
// get_status, and the status reply fills it.
//
// ledger: C4/COST1#1 /cost 显示 $
func TestModel_CommandCost_StatusBlock(t *testing.T) {
	rec := &recordingSession{}
	m := newModel(rec, "/proj")

	mm, _ := m.runCommand("/cost")
	m = mm.(model)

	require.Len(t, rec.frames, 1)
	assert.Equal(t, "get_status", rec.frames[0].Type)
	require.NotNil(t, findStatusEntry(t, m), "/cost renders a status block")

	m = m.applyEvent(cli.StreamEvent{
		Kind: "status", Model: "gpt-4o", TokensIn: 42, TokensOut: 7, Turns: 1,
	})
	se := findStatusEntry(t, m)
	assert.Equal(t, "gpt-4o", se.model)
	assert.Equal(t, 42, se.tokensIn)
}

// TestModel_CommandCompact proves /compact sends the compact frame and sets the
// activity line (bug⑧: compaction status lives on the activity line, NOT as a
// transcript entry).
func TestModel_CommandCompact(t *testing.T) {
	rec := &recordingSession{}
	m := newModel(rec, "/proj")
	before := len(m.entries)

	mm, _ := m.runCommand("/compact")
	m = mm.(model)

	require.Len(t, rec.frames, 1)
	assert.Equal(t, "compact", rec.frames[0].Type)
	assert.Contains(t, m.activity, "ompact", "/compact drives the activity line")
	assert.Len(t, m.entries, before, "/compact must NOT append a transcript entry (bug⑧)")

	// A status{compacted} reply resumes normal activity so the line doesn't
	// stick on "Compacting context…" forever.
	m = m.applyEvent(cli.StreamEvent{Kind: "status", Compacted: true})
	assert.Equal(t, "Thinking…", m.activity, "status{compacted} resets the activity line")
}

// TestModel_CompactChunkStreaming proves compact_chunk deltas drive the activity
// line, not the transcript (bug⑧: compaction is a meta-op).
func TestModel_CompactChunkStreaming(t *testing.T) {
	m := newModel(&fakeSession{}, "/proj")
	before := len(m.entries)
	m = m.applyEvent(cli.StreamEvent{Kind: "compact_chunk", Text: "sum-"})
	m = m.applyEvent(cli.StreamEvent{Kind: "compact_chunk", Text: "mary"})

	assert.Contains(t, m.activity, "ompact", "compact_chunk drives the activity line")
	assert.Len(t, m.entries, before, "compact_chunk must NOT append transcript entries (bug⑧)")

	// status{compacted} resets the activity line to "Thinking…" (the meta-op
	// is over); the token reduction surfaces via the footer ctx counter, not
	// in the transcript.
	m = m.applyEvent(cli.StreamEvent{
		Kind: "status", Compacted: true, TokensBefore: 48213, TokensAfter: 9100,
		TokensIn: 9100,
	})
	assert.Equal(t, "Thinking…", m.activity, "status{compacted} resets the activity line")
	assert.Len(t, m.entries, before, "status must NOT append a transcript entry (bug⑧)")
	// The reduced token count surfaces in the header (footer ctx counter),
	// not as a transcript block.
	assert.Equal(t, 9100, m.tokensIn)
}

// TestModel_CommandMCP proves /mcp sends mcp_action list and the mcp_status reply renders.
func TestModel_CommandMCP(t *testing.T) {
	rec := &recordingSession{}
	m := newModel(rec, "/proj")
	mm, _ := m.runCommand("/mcp")
	_ = mm.(model)
	require.Len(t, rec.frames, 1)
	assert.Equal(t, "mcp_action", rec.frames[0].Type)
	assert.Equal(t, "", rec.frames[0].MCPServer)
	assert.Equal(t, "list", rec.frames[0].MCPAction)

	// mcp_status reply renders the server status.
	m = newModel(&fakeSession{}, "/proj")
	m = m.applyEvent(cli.StreamEvent{
		Kind: "mcp_status",
		MCPServers: []proto.MCPServerStatus{
			{Name: "filesystem", Status: "ready", ToolCount: 2, Transport: "stdio"},
		},
	})
	var mc mcpStatusEntry
	var found bool
	for _, e := range m.entries {
		if x, ok := e.(mcpStatusEntry); ok {
			mc, found = x, true
		}
	}
	assert.True(t, found, "mcp_status must render a server-status entry")
	require.Len(t, mc.servers, 1)
	assert.Equal(t, "filesystem", mc.servers[0].Name)
}

// ---- T34a: /sessions /restore ----

// TestModel_CommandSessions_SendsFrame proves /sessions sends session_list.
func TestModel_CommandSessions_SendsFrame(t *testing.T) {
	rec := &recordingSession{}
	m := newModel(rec, "/proj")
	mm, _ := m.runCommand("/sessions")
	_ = mm.(model)
	require.Len(t, rec.frames, 1)
	assert.Equal(t, "session_list", rec.frames[0].Type)
}

// TestModel_Sessions_RendersList proves a sessions event fills the pending
// sessionsEntry in place.
func TestModel_Sessions_RendersList(t *testing.T) {
	m := newModel(&fakeSession{}, "/proj")
	// Pre-create the entry (cmdSessions does this).
	m.entries = append(m.entries, &sessionsEntry{})
	m = m.applyEvent(cli.StreamEvent{
		Kind: "sessions",
		Sessions: []proto.SessionInfo{
			{ID: "abc123", Title: "my chat", CreatedAt: 1000000, UpdatedAt: 1000001, MsgCount: 3},
			{ID: "def456", Title: "debug", CreatedAt: 1000002, UpdatedAt: 1000003, MsgCount: 1},
		},
	})
	se := m.lastSessionsEntry()
	require.NotNil(t, se, "sessionsEntry must exist")
	require.Len(t, se.sessions, 2)
	assert.Equal(t, "abc123", se.sessions[0].ID)
	assert.Equal(t, "my chat", se.sessions[0].Title)
	assert.Equal(t, 3, se.sessions[0].MsgCount)
	assert.Equal(t, "def456", se.sessions[1].ID)

	// Verify the rendered output contains session data.
	rendered := se.render(80, m.spinner)
	assert.Contains(t, rendered, "stored sessions")
	assert.Contains(t, rendered, "abc123")
	assert.Contains(t, rendered, "my chat")
}

// TestModel_CommandRestore_SendsFrame proves /restore sends restore_session.
func TestModel_CommandRestore_SendsFrame(t *testing.T) {
	rec := &recordingSession{}
	m := newModel(rec, "/proj")
	mm, _ := m.runCommand("/restore abc123")
	m = mm.(model)
	require.Len(t, rec.frames, 1)
	assert.Equal(t, "restore_session", rec.frames[0].Type)
	assert.Equal(t, "abc123", rec.frames[0].ID)

	// A restore with no args sends session_list and enters picker mode.
	rec2 := &recordingSession{}
	m2 := newModel(rec2, "/proj")
	mm2, _ := m2.runCommand("/restore")
	m2 = mm2.(model)
	require.Len(t, rec2.frames, 1, "/restore with no args must send a session_list frame")
	assert.Equal(t, "session_list", rec2.frames[0].Type, "must send session_list frame")
	assert.True(t, m2.restoreMode, "must set restoreMode flag")
	assert.Nil(t, m2.restoreSessions, "restoreSessions should be nil until reply arrives")
}

// TestModel_SessionRestored_UpdatesState proves a session_restored event
// re-renders the conversation from restored messages and updates model state.
func TestModel_SessionRestored_UpdatesState(t *testing.T) {
	m := newModel(&fakeSession{}, "/proj")
	m = m.applyEvent(cli.StreamEvent{
		Kind:      "session_restored",
		SessionID: "abc123",
		Model:     "claude-opus-4",
		Thinking:  "high",
		TokensIn:  100,
		TokensOut: 50,
		Turns:     2,
		Messages: []cli.MessageStub{
			{Role: "user", Content: "hello"},
			{Role: "assistant", Content: "hi"},
		},
	})
	// The restore entry is appended at the end of the re-rendered conversation.
	re := m.lastRestoreEntry()
	require.NotNil(t, re, "restoreEntry must exist")
	assert.Equal(t, "abc123", re.sessionID)
	assert.Equal(t, 2, re.count)

	// The conversation is re-rendered from messages.
	require.Len(t, m.entries, 3, "2 messages + 1 restore entry")
	ue, ok := m.entries[0].(*userEntry)
	require.True(t, ok, "first entry must be userEntry")
	assert.Equal(t, "hello", ue.text)
	ae, ok := m.entries[1].(assistantEntry)
	require.True(t, ok, "second entry must be assistantEntry")
	assert.Equal(t, "hi", ae.text)

	// Model state fields must be updated.
	assert.Equal(t, "claude-opus-4", m.modelName)
	assert.Equal(t, "high", m.thinking)
	assert.Equal(t, 100, m.tokensIn)
	assert.Equal(t, 50, m.tokensOut)
	assert.Equal(t, 2, m.turns)

	// Verify the rendered output.
	rendered := re.render(80, m.spinner)
	assert.Contains(t, rendered, "✓ restored session")
	assert.Contains(t, rendered, "abc123")
	assert.Contains(t, rendered, "2 messages")
}

// TestModel_SessionRestored_SkipsSummaryMessage proves a compaction-produced
// summary message (user role + SummarySentinel prefix) in restored history is
// NOT rendered as a chat bubble — it's model context, not conversational
// content (bug⑧).
func TestModel_SessionRestored_SkipsSummaryMessage(t *testing.T) {
	m := newModel(&fakeSession{}, "/proj")
	m = m.applyEvent(cli.StreamEvent{
		Kind:      "session_restored",
		SessionID: "abc123",
		Messages: []cli.MessageStub{
			{Role: "user", Content: "hello"},
			{Role: "assistant", Content: "hi"},
			{Role: "user", Content: ctxcompact.SummarySentinel + "compacted summary of earlier turns"},
			{Role: "user", Content: "follow-up question"},
			{Role: "assistant", Content: "answer"},
		},
	})

	// 4 visible messages + 1 restore entry (the summary is skipped).
	require.Len(t, m.entries, 5, "summary message must be skipped on restore")
	for _, e := range m.entries[:4] {
		switch v := e.(type) {
		case *userEntry:
			assert.NotContains(t, v.text, ctxcompact.SummarySentinel,
				"summary content must not appear in any rendered userEntry")
		}
	}
}

// ---- T35: permission prompt ----

// TestModel_PermissionPrompt proves: a permission_request opens the prompt
// popup (pendingPermission set); the y/n/a keys each send the matching decision
// and close it; an unrelated key keeps it open.
func TestModel_PermissionPrompt(t *testing.T) {
	rec := &recordingSession{}
	m := newModel(rec, "/proj")

	// Feed a permission_request mid-stream.
	m.streamCh = make(chan cli.StreamEvent) // simulate an in-flight turn
	m = m.applyEvent(cli.StreamEvent{
		Kind: "permission_request", ID: "req-1", ToolName: "fs_write",
		ToolArgs: `{"path":"foo.go"}`, Reason: "write outside repo",
	})

	require.NotNil(t, m.pendingPermission(), "model must open the prompt popup")
	pe := m.pendingPermission()
	require.Equal(t, "req-1", pe.id)
	require.Equal(t, "fs_write", pe.tool)

	// An unrelated key is a no-op: still prompting, no frame sent.
	mm, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
	m = mm.(model)
	require.NotNil(t, m.pendingPermission(), "unrecognized key keeps the prompt")
	assert.Empty(t, rec.frames)

	// 'n' → deny, popup closed.
	mm, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
	m = mm.(model)
	require.Nil(t, m.pendingPermission(), "n must resolve the prompt")
	require.Len(t, rec.frames, 1)
	assert.Equal(t, proto.NewPermissionResponse("req-1", "deny"), rec.frames[0])
}

// TestModel_PermissionPrompt_ArrowEnter proves Up/Down move the popup's option
// selection and Enter confirms the highlighted one. Task 9 split "Always allow"
// into "Allow this session" (allow_session) and "Persistent allow"
// (allow_persistent), so the order is now:
//
//	0=Allow  1=Allow this session  2=Persistent allow  3=Deny
func TestModel_PermissionPrompt_ArrowEnter(t *testing.T) {
	for _, tc := range []struct {
		name     string
		moves    int // net Down presses (wraps)
		decision string
	}{
		{"enter on Allow (default)", 0, "allow"},
		{"down once → Allow this session", 1, "allow_session"},
		{"down twice → Persistent allow", 2, "allow_persistent"},
		{"down three times → Deny", 3, "deny"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := &recordingSession{}
			m := newModel(rec, "/proj")
			m = m.applyEvent(cli.StreamEvent{
				Kind: "permission_request", ID: "r", ToolName: "shell_run", ToolArgs: "{}",
			})
			require.NotNil(t, m.pendingPermission())
			for i := 0; i < tc.moves; i++ {
				mm, _ := m.Update(tea.KeyMsg{Type: tea.KeyDown})
				m = mm.(model)
			}
			mm, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
			m = mm.(model)
			require.Nil(t, m.pendingPermission(), "Enter resolves the prompt")
			require.Len(t, rec.frames, 1)
			assert.Equal(t, tc.decision, rec.frames[0].Decision)
		})
	}
}

// TestModel_PermissionPrompt_AllowAlways covers the y and a decisions.
func TestModel_PermissionPrompt_AllowAlways(t *testing.T) {
	for _, tc := range []struct {
		key      string
		decision string
	}{
		{"y", "allow"},
		{"a", "always_allow"},
	} {
		t.Run(tc.key, func(t *testing.T) {
			rec := &recordingSession{}
			m := newModel(rec, "/proj")
			m = m.applyEvent(cli.StreamEvent{
				Kind: "permission_request", ID: "req-2", ToolName: "shell", ToolArgs: "{}",
			})
			require.NotNil(t, m.pendingPermission())

			mm, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(tc.key)})
			m = mm.(model)
			require.Nil(t, m.pendingPermission())
			require.Len(t, rec.frames, 1)
			assert.Equal(t, tc.decision, rec.frames[0].Decision)
		})
	}
}

// ---- B6: thinking live limit + live/done coloring ----

// TestThinkingLiveLimited proves a LIVE thinkingEntry caps its rendered body at
// the last ~10 lines of reasoning text. Without the cap, a long reasoning draft
// would push prior transcript context off screen. The header still shows, and
// the total newline count stays bounded (~header + 10 body lines + trailing).
//
// Each "line N" is its own paragraph (separated by blank lines) so glamour's
// word-wrap cannot reflow them into one line — this is the realistic shape of
// streaming reasoning (distinct steps / bullets).
func TestThinkingLiveLimited(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 30; i++ {
		fmt.Fprintf(&b, "line %d\n\n", i) // paragraph break keeps lines distinct
	}
	te := &thinkingEntry{
		text:      b.String(),
		live:      true,
		startedAt: time.Now().Add(-5 * time.Second),
	}
	out := te.render(80, spinner.Model{})
	if strings.Count(out, "\n") > 14 {
		t.Fatalf("live thinking should be capped at ~10 body lines (header + ≤10 + slack ≤14 newlines), got %d:\n%s",
			strings.Count(out, "\n"), out)
	}
	if !strings.Contains(out, "Thinking") {
		t.Fatalf("live thinking should still show the 'Thinking' header:\n%s", out)
	}
	// The cap should retain the most recent lines (tail), not the head.
	if !strings.Contains(out, "line 29") {
		t.Fatalf("live thinking tail should include the most recent line:\n%s", out)
	}
}

// TestThinkingFinalizedShowsThoughtFor proves a finalized (collapsed)
// thinkingEntry renders the "Thought for" summary line, not the live
// "Thinking…" header — the state transition is visible in the text.
func TestThinkingFinalizedShowsThoughtFor(t *testing.T) {
	te := &thinkingEntry{
		text:      "x",
		live:      false,
		startedAt: time.Now(),
		endedAt:   time.Now(),
	}
	out := te.render(80, spinner.Model{})
	if !strings.Contains(out, "Thought for") {
		t.Fatalf("finalized thinking should show 'Thought for':\n%s", out)
	}
	if strings.Contains(out, "Thinking…") {
		t.Fatalf("finalized thinking must NOT show the live 'Thinking…' header:\n%s", out)
	}
}

// TestThinkingLiveAndDoneColoredDifferently proves the live and finalized
// renders differ in styling (different ANSI color codes), so the user can tell
// at a glance whether reasoning is still streaming or has settled. The plain
// text may be similar but the raw rendered bytes must differ.
func TestThinkingLiveAndDoneColoredDifferently(t *testing.T) {
	live := &thinkingEntry{text: "x", live: true, startedAt: time.Now()}
	done := &thinkingEntry{
		text:      "x",
		live:      false,
		startedAt: time.Now(),
		endedAt:   time.Now(),
	}
	rawLive := live.render(80, spinner.Model{})
	rawDone := done.render(80, spinner.Model{})
	if rawLive == rawDone {
		t.Fatalf("live and done renders should differ (color/styling):\nlive: %q\ndone: %q",
			rawLive, rawDone)
	}
}

// ---- C07: /queue-mode registration + cycle ----

// TestModel_QueueMode_Registered verifies /queue-mode is in the command table
// (lookupCommand finds it) and /help lists it — the C07 registration gap.
func TestModel_QueueMode_Registered(t *testing.T) {
	_, ok := lookupCommand("queue-mode")
	require.True(t, ok, "/queue-mode must be registered in commandTable")
	assert.Contains(t, helpEntry{}.render(80, spinner.Model{}), "queue-mode")
}

// TestModel_QueueMode_Cycle verifies /queue-mode with no arg cycles through the
// three modes (queue → single → batch → queue) and renders the mode list.
func TestModel_QueueMode_Cycle(t *testing.T) {
	rec := &recordingSession{}
	m := newModel(rec, "/proj")
	require.Equal(t, QueueModeQueue, m.queueMode)

	for _, want := range []QueueMode{QueueModeSingle, QueueModeBatch, QueueModeQueue} {
		mm, _ := m.runCommand("/queue-mode")
		m = mm.(model)
		assert.Equal(t, want, m.queueMode)
	}
	assert.Empty(t, rec.frames, "/queue-mode is local-only (no control frame)")

	// The last cycle rendered the active mode list.
	last := m.entries[len(m.entries)-1]
	ee, ok := last.(errorEntry)
	require.True(t, ok, "cycle renders an errorEntry list")
	assert.Contains(t, ee.text, "queue mode: queue")
}

// TestModel_QueueMode_ExplicitSet verifies /queue-mode <name> sets the mode and
// rejects unknown names without changing state.
func TestModel_QueueMode_ExplicitSet(t *testing.T) {
	for _, tc := range []struct {
		arg  string
		want QueueMode
	}{
		{"queue", QueueModeQueue},
		{"single", QueueModeSingle},
		{"batch", QueueModeBatch},
	} {
		t.Run(tc.arg, func(t *testing.T) {
			m := newModel(&recordingSession{}, "/proj")
			mm, _ := m.runCommand("/queue-mode " + tc.arg)
			m = mm.(model)
			assert.Equal(t, tc.want, m.queueMode)
		})
	}

	// Unknown arg → local error, mode unchanged.
	m := newModel(&recordingSession{}, "/proj")
	m.queueMode = QueueModeBatch
	mm, _ := m.runCommand("/queue-mode nope")
	m = mm.(model)
	assert.Equal(t, QueueModeBatch, m.queueMode, "unknown arg must not change the mode")
}

func TestCmdRename_SendsRenameFrame(t *testing.T) {
	rs := &recordingSession{}
	m := newModel(rs, "/proj")
	mm, _ := cmdRename(m, []string{"s1", "new", "title"})
	m = mm.(model)
	require.Len(t, rs.frames, 1)
	assert.Equal(t, "rename_session", rs.frames[0].Type)
	assert.Equal(t, "s1", rs.frames[0].ID)
	assert.Equal(t, "new title", rs.frames[0].Text)
}

func TestCmdRename_MissingArgs(t *testing.T) {
	rs := &recordingSession{}
	m := newModel(rs, "/proj")
	mm, _ := cmdRename(m, []string{"s1"})
	m = mm.(model)
	assert.Empty(t, rs.frames, "no frame when title missing")
	// An error entry is rendered locally.
	_, isErr := m.entries[len(m.entries)-1].(errorEntry)
	assert.True(t, isErr, "missing-arg should render an error entry")
}

func TestCmdDelete_RequiresConfirmToken(t *testing.T) {
	rs := &recordingSession{}
	m := newModel(rs, "/proj")
	// No "yes" token -> confirmation prompt, NO frame sent.
	mm, _ := cmdDelete(m, []string{"s1"})
	m = mm.(model)
	assert.Empty(t, rs.frames)
	assert.Contains(t, renderLast(m), "yes")

	// With "yes" -> frame sent.
	mm, _ = cmdDelete(m, []string{"s1", "yes"})
	m = mm.(model)
	require.Len(t, rs.frames, 1)
	assert.Equal(t, "delete_session", rs.frames[0].Type)
	assert.Equal(t, "s1", rs.frames[0].ID)
}

func TestCmdArchive_CmdUnarchive_CmdArchived(t *testing.T) {
	rs := &recordingSession{}
	m := newModel(rs, "/proj")

	mm, _ := cmdArchive(m, []string{"s1"})
	m = mm.(model)
	mm, _ = cmdUnarchive(m, []string{"s1"})
	m = mm.(model)
	mm, _ = cmdArchived(m, nil)
	m = mm.(model)

	require.Len(t, rs.frames, 3)
	assert.Equal(t, "archive_session", rs.frames[0].Type)
	assert.Equal(t, "unarchive_session", rs.frames[1].Type)
	assert.Equal(t, "session_list_archived", rs.frames[2].Type)
}

func TestApplyEvent_SessionAck_RendersAck(t *testing.T) {
	m := newModel(&fakeSession{}, "/proj")
	m = m.applyEvent(cli.StreamEvent{Kind: "session_ack", Action: "renamed", SessionID: "s1", Text: "hello"})
	last := renderLast(m)
	assert.Contains(t, last, "renamed")
	assert.Contains(t, last, "hello")

	m = m.applyEvent(cli.StreamEvent{Kind: "session_ack", Action: "deleted", SessionID: "s1"})
	assert.Contains(t, renderLast(m), "deleted")
}

// renderLast returns the rendered text of the last entry (helper for command tests).
func renderLast(m model) string {
	if len(m.entries) == 0 {
		return ""
	}
	return stripANSI(m.entries[len(m.entries)-1].render(120, m.spinner))
}

// TestStatsEntryRendersHistogram proves /stats renders a per-session token
// histogram: bars scaled to the max consumer, the token totals, and a summary
// line. The biggest consumer gets a full bar; smaller ones scale down.
func TestStatsEntryRendersHistogram(t *testing.T) {
	e := &statsEntry{sessions: []proto.SessionInfo{
		{ID: "a", Title: "big", TokensIn: 10000, TokensOut: 5000, UpdatedAt: 3},
		{ID: "b", Title: "small", TokensIn: 1000, TokensOut: 0, UpdatedAt: 2},
	}}
	out := stripANSI(e.render(120, newSpinner()))

	// Both sessions appear with their totals (15k full bar, 1k scaled down).
	assert.Contains(t, out, "15k")
	assert.Contains(t, out, "1k")
	assert.Contains(t, out, "big")
	assert.Contains(t, out, "small")
	// The biggest consumer fills the whole bar width (20 full blocks).
	assert.Contains(t, out, strings.Repeat("█", statsBarWidth))
	// Summary line aggregates.
	assert.Contains(t, out, "2 sessions")
	assert.Contains(t, out, "total 16k")
}

// TestStatsEntryEmptyShowsPlaceholder proves /stats with no usage renders a
// friendly placeholder instead of an empty histogram.
func TestStatsEntryEmptyShowsPlaceholder(t *testing.T) {
	e := &statsEntry{sessions: []proto.SessionInfo{{ID: "x"}}} // zero tokens
	out := stripANSI(e.render(120, newSpinner()))
	assert.Contains(t, out, "no token usage")
}

// TestModel_CommandStats_RoutesSessionReply proves /stats appends a statsEntry
// and the "sessions" reply fills IT (not the sessionsEntry) while it's pending.
func TestModel_CommandStats_RoutesSessionReply(t *testing.T) {
	m := newModel(&fakeSession{}, "/proj")
	mm, _ := cmdStats(m, nil)
	m = mm.(model)
	require.NotNil(t, m.pendingStatsEntry, "cmdStats must stash a pending statsEntry")

	// The session-list reply arrives.
	m = m.applyEvent(cli.StreamEvent{
		Kind:     "sessions",
		Sessions: []proto.SessionInfo{{ID: "s1", Title: "t", TokensIn: 5, TokensOut: 5}},
	})
	assert.Nil(t, m.pendingStatsEntry, "reply must clear pendingStatsEntry")
	se := m.lastStatsEntry()
	require.NotNil(t, se)
	require.Len(t, se.sessions, 1)
	assert.Equal(t, "s1", se.sessions[0].ID)
}

// TestCmdReviewDispatchesViaSend verifies that /review forwards its input as a
// normal user message (prefixed with "/review ") via dispatchSend, so the agent
// can decide to invoke the review tool. It must not bypass the send path.
func TestCmdReviewDispatchesViaSend(t *testing.T) {
	rec := &recordingSession{}
	m := newModel(rec, "/proj")
	cmd, ok := lookupCommand("review")
	require.True(t, ok, "/review command not registered")
	require.NotNil(t, cmd.run)
	mm, c := cmd.run(m, []string{"some diff text"})
	assert.NotNil(t, c, "cmd.run must return a non-nil tea.Cmd when dispatching")
	var saw bool
	for _, sent := range rec.sentText {
		if strings.Contains(sent, "/review") && strings.Contains(sent, "some diff text") {
			saw = true
			break
		}
	}
	assert.True(t, saw, "Send not invoked with /review text; sentText=%q", rec.sentText)
	_ = mm
}

// ---- C4 OBS3/COST1: T10 feature and cost rendering ----

func TestModel_CommandFeaturesSendsListEnableDisable(t *testing.T) {
	TT := func(b bool) *bool { return &b }
	cases := []struct {
		input string
		want  proto.ClientFrame
	}{
		{input: "/features", want: proto.NewFeaturesList()},
		{input: "/features enable observe.cost_in_status", want: proto.ClientFrame{Type: "features_set", FeaturesSet: &proto.FeaturesSetPayload{Key: "observe.cost_in_status", Enabled: TT(true)}}},
		{input: "/features disable observe.cost_in_status", want: proto.ClientFrame{Type: "features_set", FeaturesSet: &proto.FeaturesSetPayload{Key: "observe.cost_in_status", Enabled: TT(false)}}},
	}
	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			rec := &recordingSession{}
			m := newModel(rec, "/proj")
			_, _ = m.runCommand(tc.input)
			require.Equal(t, []proto.ClientFrame{tc.want}, rec.frames)
		})
	}
}

// TestStatusEntryRendersKnownAndUnknownCost.
//
// ledger: C4/COST1#1 /cost 显示 $
func TestStatusEntryRendersKnownAndUnknownCost(t *testing.T) {
	known := stripANSI((&statusEntry{tokensIn: 100, tokensOut: 20, costUSD: 0.012345, costKnown: true}).render(120, newSpinner()))
	assert.Contains(t, known, "$0.012") // FormatCost bands: <1 -> %.3f (was a bespoke $%.6f)
	unknown := stripANSI((&statusEntry{tokensIn: 100, costKnown: false}).render(120, newSpinner()))
	assert.Contains(t, unknown, "N/A")
	assert.NotContains(t, unknown, "$0.000000")
}

// TestStatsEntryAggregatesKnownCostAndNamesUnknown was dormant: it shipped
// named testStats...(lowercase t), so `go test` never ran it, and it asserted
// "$0.2500" -- a format that has never existed in this codebase. FormatCost
// has been banded since it was written (<0.01 -> %.4f, <1 -> %.3f), so 0.25
// renders as $0.250.
//
// Enabling it is what surfaced the actual defect: statsEntry.render never read
// CostUSD at all, while its doc comment claimed "with USD cost". The plan's
// instruction was to run it first and see how it fails before deciding which
// side to change -- and both sides were wrong, in different ways.
//
// ledger: C4/COST1#2 聚合正确
func TestStatsEntryAggregatesKnownCostAndNamesUnknown(t *testing.T) {
	e := &statsEntry{sessions: []proto.SessionInfo{
		{Title: "known", TokensIn: 100, CostUSD: 0.25, CostKnown: true},
		{Title: "unknown", TokensOut: 10, CostKnown: false},
	}}
	out := stripANSI(e.render(120, newSpinner()))
	assert.Contains(t, out, "known")
	assert.Contains(t, out, "$0.250") // FormatCost bands: <1 -> %.3f
	assert.Contains(t, out, "unknown")
	assert.Contains(t, out, "N/A")
	assert.Contains(t, out, "1 unknown")
}

func TestFeaturesEntryRendersStageOwnerAndDisabled(t *testing.T) {
	e := featuresEntry{rows: []proto.FeatureRow{{
		Key: "observe.cost_in_status", Stage: "beta", Enabled: false, Owner: "runtime",
	}}}
	out := stripANSI(e.render(120, newSpinner()))
	assert.Contains(t, out, "observe.cost_in_status")
	assert.Contains(t, out, "beta")
	assert.Contains(t, out, "disabled")
	assert.Contains(t, out, "runtime")
}

// TestCmdHelpEntry_PreRendered verifies the cmdHelpEntry constructed by
// newCmdHelpEntry contains the localized title and each command from the
// supplied table (so /help is decoupled from the live commandTable iteration
// in the legacy render path).
func TestCmdHelpEntry_PreRendered(t *testing.T) {
	b, _ := i18n.NewBundle("en")
	table := []command{
		{name: "alpha", help: "alpha help", helpKey: "tui.command.help.help"},
		{name: "beta", help: "beta help", helpKey: "tui.command.help.model"},
	}
	e := newCmdHelpEntry(b, table)
	out := e.render(80, spinner.Model{})
	if !strings.Contains(out, b.Get("tui.command.help.title")) {
		t.Fatalf("render missing localized title: %q", out)
	}
	for _, c := range table {
		if !strings.Contains(out, "/"+c.name) {
			t.Fatalf("render missing /%s", c.name)
		}
		if !strings.Contains(out, b.Get(c.helpKey)) {
			t.Fatalf("render missing localized help %q for /%s", c.helpKey, c.name)
		}
	}
}

// TestCmdHelpEntry_NoHardcodedEnglish asserts the rendering does not
// hardcode "Commands" or other untranslated text. It uses the bundle for
// every visible surface.
func TestCmdHelpEntry_NoHardcodedEnglish(t *testing.T) {
	b, _ := i18n.NewBundle("zh-Hans")
	table := []command{{name: "alpha", helpKey: "tui.command.help.help"}}
	e := newCmdHelpEntry(b, table)
	out := e.render(80, spinner.Model{})
	if strings.Contains(out, "Commands") {
		t.Fatalf("hardcoded English 'Commands' in zh-Hans rendering: %q", out)
	}
}

// TestCommandTableNamesAreUnique guards a duplicate that reached three
// surfaces at once.
//
// commandTable was registered twice for the same name at one point, and
// help.go's renderer walks the table without deduplicating -- so /help, the
// Ctrl+K palette and the / palette each listed that command twice. Nothing
// broke, the count was just quietly wrong everywhere a user looks.
//
// Removing the duplicate line fixes it once; this makes it stay fixed. The
// table is edited by hand every time a command is added, which is exactly the
// edit that reintroduces a name.
func TestCommandTableNamesAreUnique(t *testing.T) {
	seen := map[string]int{}
	for _, c := range commandTable {
		seen[c.name]++
	}
	for name, n := range seen {
		if n > 1 {
			t.Errorf("command %q registered %d times: /help and both palettes render it once per entry",
				name, n)
		}
	}
	if len(seen) != len(commandTable) {
		t.Errorf("commandTable has %d entries but %d distinct names", len(commandTable), len(seen))
	}
}

// TestClearMemories_RequiresConfirmation is W-D-12's first clause, asserted on
// the surface the user actually types at. Every branch that stops short of the
// frame is checked, because "no frame was sent" is only reassuring if the
// command could have sent one.
func TestClearMemories_RequiresConfirmation(t *testing.T) {
	for _, args := range [][]string{
		nil,                     // no scope
		{"all"},                 // scope, no confirmation
		{"session"},             //
		{"agent", "a1"},         // agent id, no confirmation
		{"agent"},               // agent scope with no id
		{"all", "no"},           // a token that is not "yes"
		{"everything", "yes"},   // unknown scope, even confirmed
		{"all", "yes", "extra"}, // "yes" must be the last word
		{"agent", "a1", "yes", "x"},
	} {
		rs := &recordingSession{}
		m := newModel(rs, "/proj")
		mm, _ := cmdMemoryClear(m, args)
		m = mm.(model)
		assert.Emptyf(t, rs.frames, "%v must not clear anything", args)
		_, isErr := m.entries[len(m.entries)-1].(errorEntry)
		assert.Truef(t, isErr, "%v must explain itself", args)
	}

	for _, tc := range []struct {
		args         []string
		scope, agent string
	}{
		{[]string{"all", "yes"}, "all", ""},
		{[]string{"session", "yes"}, "session", ""},
		{[]string{"agent", "a1", "yes"}, "agent", "a1"},
	} {
		rs := &recordingSession{}
		m := newModel(rs, "/proj")
		mm, _ := cmdMemoryClear(m, tc.args)
		_ = mm.(model)
		require.Lenf(t, rs.frames, 1, "%v is confirmed and must be sent", tc.args)
		assert.Equal(t, "clear_memories", rs.frames[0].Type)
		assert.Equal(t, tc.scope, rs.frames[0].Name)
		assert.Equal(t, tc.agent, rs.frames[0].ID)
	}
}
