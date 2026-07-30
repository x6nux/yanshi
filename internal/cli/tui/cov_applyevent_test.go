package tui

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/cli"
	"github.com/x6nux/yanshi/internal/proto"
)

// aeModel is a fresh model for applyEvent tests: a scripted session + sized
// viewport, so event-driven entry appends work without nil-map issues.
func aeModel() model { return wsModel(newScriptedSession(nil)) }

// lastEntry returns the most-recently appended entry (fails if none).
func lastEntry(t *testing.T, m model) entry {
	t.Helper()
	require.NotEmpty(t, m.entries, "expected at least one entry")
	return m.entries[len(m.entries)-1]
}

// TestApplyEvent_ControlReplies drives every control-reply event kind through
// applyEvent so each branch is covered.
func TestApplyEvent_ControlReplies(t *testing.T) {
	t.Run("models_list", func(t *testing.T) {
		m := aeModel()
		m = m.applyEvent(cli.StreamEvent{Kind: "models", Items: []string{"a", "b"}})
		_, ok := lastEntry(t, m).(modelsEntry)
		assert.True(t, ok)
	})

	t.Run("models_into_picker", func(t *testing.T) {
		m := aeModel()
		m.pickerKind = "model"
		m = m.applyEvent(cli.StreamEvent{Kind: "models", Items: []string{"a"}})
		require.NotEmpty(t, m.pickerItems)
		assert.Equal(t, "a", m.pickerItems[0].name)
		// picker path does NOT append a transcript entry.
		var sawModels bool
		for _, e := range m.entries {
			if _, ok := e.(modelsEntry); ok {
				sawModels = true
			}
		}
		assert.False(t, sawModels)
	})

	t.Run("mcp_status", func(t *testing.T) {
		m := aeModel()
		m = m.applyEvent(cli.StreamEvent{Kind: "mcp_status",
			MCPServers: []proto.MCPServerStatus{{Name: "srv", Status: "ready"}}})
		e, ok := lastEntry(t, m).(mcpStatusEntry)
		require.True(t, ok)
		assert.NotEmpty(t, e.servers)
	})

	t.Run("sessions_restore", func(t *testing.T) {
		m := aeModel()
		m.restoreMode = true
		m = m.applyEvent(cli.StreamEvent{Kind: "sessions", Sessions: []proto.SessionInfo{{ID: "s1"}}})
		require.NotEmpty(t, m.restoreSessions)
		assert.False(t, m.restoreMode)
	})

	t.Run("sessions_stats", func(t *testing.T) {
		m := aeModel()
		se := &statsEntry{}
		m.pendingStatsEntry = se
		m = m.applyEvent(cli.StreamEvent{Kind: "sessions", Sessions: []proto.SessionInfo{{ID: "s1"}}})
		assert.NotEmpty(t, se.sessions)
		assert.Nil(t, m.pendingStatsEntry)
	})

	t.Run("sessions_list", func(t *testing.T) {
		m := aeModel()
		m.entries = append(m.entries, &sessionsEntry{})
		m = m.applyEvent(cli.StreamEvent{Kind: "sessions", Sessions: []proto.SessionInfo{{ID: "s1"}}})
		se := m.lastSessionsEntry()
		require.NotNil(t, se)
		assert.NotEmpty(t, se.sessions)
	})

	t.Run("session_restored", func(t *testing.T) {
		m := aeModel()
		m.entries = append(m.entries, errorEntry{text: "old"})
		m = m.applyEvent(cli.StreamEvent{
			Kind: "session_restored", SessionID: "newid", Model: "gpt",
			Messages: []cli.MessageStub{
				{Role: "user", Content: "hi"},
				{Role: "assistant", Content: "hello"},
				{Role: "user", Content: "ctxcompact-sentinel rest"}, // kept (no real sentinel)
			},
		})
		_, ok := lastEntry(t, m).(*restoreEntry)
		assert.True(t, ok)
		assert.Equal(t, "newid", m.sessionID)
		assert.Equal(t, "gpt", m.modelName)
	})

	t.Run("session_ack", func(t *testing.T) {
		m := aeModel()
		m = m.applyEvent(cli.StreamEvent{Kind: "session_ack", Action: "renamed", SessionID: "1234567890", Text: "New"})
		_, ok := lastEntry(t, m).(ackEntry)
		assert.True(t, ok)
	})

	t.Run("session_forked", func(t *testing.T) {
		m := aeModel()
		m = m.applyEvent(cli.StreamEvent{Kind: "session_forked", SessionID: "fork-1"})
		assert.Equal(t, "fork-1", m.sessionID)
	})

	t.Run("side_state_enter_and_main", func(t *testing.T) {
		m := aeModel()
		m = m.applyEvent(cli.StreamEvent{Kind: "side_state", SideDepth: 1})
		assert.Equal(t, 1, m.sideDepth)
		m = m.applyEvent(cli.StreamEvent{Kind: "side_state", SideDepth: 0})
		assert.Equal(t, 0, m.sideDepth)
	})

	t.Run("skills_list", func(t *testing.T) {
		m := aeModel()
		m = m.applyEvent(cli.StreamEvent{Kind: "skills_list", Skills: []proto.SkillInfo{{Name: "plan"}}})
		e, ok := lastEntry(t, m).(skillsEntry)
		require.True(t, ok)
		assert.NotEmpty(t, e.skills)
	})

	t.Run("features", func(t *testing.T) {
		m := aeModel()
		m = m.applyEvent(cli.StreamEvent{Kind: "features", Features: []proto.FeatureRow{{Key: "f"}}})
		_, ok := lastEntry(t, m).(featuresEntry)
		assert.True(t, ok)
	})

	t.Run("skill_ack_ok_and_err", func(t *testing.T) {
		m := aeModel()
		m = m.applyEvent(cli.StreamEvent{Kind: "skill_ack", Action: "installed", Skill: &proto.SkillInfo{Name: "plan"}})
		_, ok := lastEntry(t, m).(ackEntry)
		assert.True(t, ok)

		m = aeModel()
		m = m.applyEvent(cli.StreamEvent{Kind: "skill_ack", Text: "not found"})
		_, ok = lastEntry(t, m).(errorEntry)
		assert.True(t, ok)
	})

	t.Run("permissions_and_rule_hit", func(t *testing.T) {
		m := aeModel()
		m = m.applyEvent(cli.StreamEvent{Kind: "permissions", Permissions: []proto.PermissionInfo{{ID: "r1"}}})
		_, ok := lastEntry(t, m).(permissionsEntry)
		assert.True(t, ok)

		m = m.applyEvent(cli.StreamEvent{Kind: "permission_rule_hit", ID: "r1", ToolStatus: "hit"})
		_, ok = lastEntry(t, m).(summaryEntry)
		assert.True(t, ok)
	})

	t.Run("jobs_and_event", func(t *testing.T) {
		m := aeModel()
		m = m.applyEvent(cli.StreamEvent{Kind: "jobs", Jobs: []proto.JobInfo{{ID: "j1", PID: 1}}})
		_, ok := lastEntry(t, m).(jobsEntry)
		assert.True(t, ok)

		m = m.applyEvent(cli.StreamEvent{Kind: "job_event", ID: "j1", ToolStatus: "read", Text: "output"})
		_, ok = lastEntry(t, m).(summaryEntry)
		assert.True(t, ok)
	})

	t.Run("status_sets_head", func(t *testing.T) {
		m := aeModel()
		m = m.applyEvent(cli.StreamEvent{Kind: "status", Head: "head-abc", Model: "m", ContextWindow: 8000})
		assert.Equal(t, "head-abc", m.lastKnownHead)
		assert.Equal(t, "m", m.modelName)
	})

	t.Run("seams_filters", func(t *testing.T) {
		m := aeModel()
		m = m.applyEvent(cli.StreamEvent{Kind: "seams", Head: "h", Seams: []proto.SeamInfo{
			{ID: "s1", Kind: "pre-turn"},
			{ID: "s2", Kind: "post-turn"},
			{ID: "s3", Kind: "pre-revert"},
		}})
		e, ok := lastEntry(t, m).(seamsEntry)
		require.True(t, ok)
		assert.Len(t, e.items, 2, "only pre-turn + pre-revert seams shown")
		assert.Equal(t, "h", m.lastKnownHead)
	})

	t.Run("seam_restored", func(t *testing.T) {
		m := aeModel()
		m.pendingSeamRestore = &pendingSeamRestoreState{seamID: "s1"}
		m = m.applyEvent(cli.StreamEvent{Kind: "seam_restored", Head: "h2", UndoSeamID: "undo1", Text: "3 turns"})
		e, ok := lastEntry(t, m).(seamRestoredEntry)
		require.True(t, ok)
		assert.Equal(t, "undo1", e.undoID)
		assert.Nil(t, m.pendingSeamRestore)
		assert.Equal(t, "h2", m.lastKnownHead)
	})

	t.Run("compact_chunk_sets_activity", func(t *testing.T) {
		m := aeModel()
		m = m.applyEvent(cli.StreamEvent{Kind: "compact_chunk"})
		assert.Contains(t, m.activity, "Compacting")
	})

	t.Run("retry_sets_state", func(t *testing.T) {
		m := aeModel()
		m.pending = "partial"
		m = m.applyEvent(cli.StreamEvent{Kind: "retry", RetryAttempt: 1, RetryMax: 3, Text: "eof"})
		assert.Equal(t, "", m.pending, "retry discards the partial output")
		assert.Equal(t, 1, m.retryAttempt)
		assert.Equal(t, 3, m.retryMax)
		assert.Equal(t, "eof", m.retryErr)
	})

	t.Run("permission_request", func(t *testing.T) {
		m := aeModel()
		m = m.applyEvent(cli.StreamEvent{Kind: "permission_request", ID: "p1", ToolName: "shell_run", ToolArgs: "rm", Reason: "dangerous"})
		require.NotEmpty(t, m.pendingPermissions)
		assert.Equal(t, "p1", m.pendingPermissions[0].id)
	})

	t.Run("done_clears_state", func(t *testing.T) {
		m := aeModel()
		ch := make(chan cli.StreamEvent)
		m.streamCh = ch
		m.activity = "Thinking…"
		m = m.applyEvent(cli.StreamEvent{Kind: "done", Text: "Done 1 tools"})
		assert.Nil(t, m.streamCh)
		assert.Equal(t, "", m.activity)
		_, ok := lastEntry(t, m).(summaryEntry)
		assert.True(t, ok)
	})
}

// TestApplyEvent_ToolProgressAndChunk covers the tool_progress and tool_chunk
// branches (which append/overwrite a running tool's progress).
func TestApplyEvent_ToolProgressAndChunk(t *testing.T) {
	m := aeModel()
	// Start a running tool.
	m = m.applyEvent(cli.StreamEvent{Kind: "tool_call", ToolName: "shell_run", ToolStatus: "running"})
	t1 := m.lastRunningTool("shell_run")
	require.NotNil(t, t1)

	// tool_progress appends a line.
	m = m.applyEvent(cli.StreamEvent{Kind: "tool_progress", ToolName: "shell_run", Text: "line 1\n"})
	assert.NotEmpty(t, t1.progress)

	// tool_chunk append (non-overwrite).
	m = m.applyEvent(cli.StreamEvent{Kind: "tool_chunk", ToolName: "shell_run", Text: "more\n"})
	assert.Contains(t, t1.progress[len(t1.progress)-1], "more")

	// tool_chunk overwrite replaces progress.
	m = m.applyEvent(cli.StreamEvent{Kind: "tool_chunk", ToolName: "shell_run", Text: "replaced\n", Overwrite: true})
	assert.True(t, t1.progressOverwrite)

	// tool_chunk with a status panel.
	m = m.applyEvent(cli.StreamEvent{Kind: "tool_chunk", ToolName: "shell_run", ToolStatus: "1 tools 10k"})
	assert.Equal(t, "1 tools 10k", t1.statusPanel)
}
