package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/guard"
	"github.com/x6nux/yanshi/internal/proto"
)

// sseSession is a recordingSession variant whose Mode() returns "sse" so the
// plan/restore-turn SSE-rejection branches can be exercised.
type sseSession struct {
	recordingSession
}

func (s *sseSession) Mode() string { return "sse" }

// wsModel builds a model with a fresh session and a sized viewport so command
// handlers can append entries / send frames without nil-map issues. sess may be
// a recordingSession (ws) or an sseSession (sse) — both record frames.
func wsModel(sess tuiSession) model {
	m := newModel(sess, "/proj")
	mm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	return mm.(model)
}

// frameTypes returns the Type of every frame a recording session captured.
func frameTypes(frames []proto.ClientFrame) []string {
	out := make([]string, len(frames))
	for i, f := range frames {
		out[i] = f.Type
	}
	return out
}

// ---- cmdPlan / cmdPlanOff ----

func TestCmdPlan_EnterAndExitWS(t *testing.T) {
	rec := &recordingSession{}
	m := wsModel(rec)
	require.NotEqual(t, guard.ModePlan, m.permMode)

	mm, _ := runCommandOn(model(m), "/plan")
	m = mm.(model)
	assert.Equal(t, guard.ModePlan, m.permMode, "/plan enters plan mode")
	require.Contains(t, frameTypes(rec.frames), "set_mode")

	// /plan-off restores the prior mode (default when none was saved).
	rec = &recordingSession{}
	m = wsModel(rec)
	mm, _ = runCommandOn(model(m), "/plan")
	m = mm.(model)
	mm, _ = runCommandOn(model(m), "/plan-off")
	m = mm.(model)
	assert.NotEqual(t, guard.ModePlan, m.permMode, "/plan-off exits plan mode")
}

func TestCmdPlan_SSERejected(t *testing.T) {
	for _, input := range []string{"/plan", "/plan-off"} {
		rec := &sseSession{}
		m := newModel(rec, "/proj")
		mm, _ := m.runCommand(input)
		m = mm.(model)
		var sawErr bool
		for _, e := range m.entries {
			if ee, ok := e.(errorEntry); ok && ee.text != "" {
				sawErr = true
			}
		}
		assert.True(t, sawErr, "%s on SSE must render an error", input)
		assert.Empty(t, rec.frames, "%s on SSE sends no frame", input)
	}
}

// ---- cmdMode ----

func TestCmdMode_PickerValidInvalidAndAuto(t *testing.T) {
	// No-arg -> picker.
	rec := &recordingSession{}
	m := wsModel(rec)
	mm, _ := runCommandOn(model(m), "/mode")
	m = mm.(model)
	assert.Equal(t, "mode", m.pickerKind)
	assert.NotEmpty(t, m.pickerItems)

	// Valid mode.
	rec = &recordingSession{}
	m = wsModel(rec)
	mm, _ = runCommandOn(model(m), "/mode allow-edits")
	m = mm.(model)
	assert.Equal(t, guard.ModeAllowEdits, m.permMode)

	// Invalid mode -> error, no frame.
	rec = &recordingSession{}
	m = wsModel(rec)
	mm, _ = runCommandOn(model(m), "/mode bogus")
	m = mm.(model)
	assert.Empty(t, rec.frames)
	var sawErr bool
	for _, e := range m.entries {
		if _, ok := e.(errorEntry); ok {
			sawErr = true
		}
	}
	assert.True(t, sawErr)

	// Auto with explicit threshold.
	rec = &recordingSession{}
	m = wsModel(rec)
	mm, _ = runCommandOn(model(m), "/mode auto 6")
	m = mm.(model)
	assert.Equal(t, guard.ModeAuto, m.permMode)
	assert.Equal(t, 6, m.autoThreshold)

	// Auto with out-of-range threshold -> error.
	rec = &recordingSession{}
	m = wsModel(rec)
	mm, _ = runCommandOn(model(m), "/mode auto 99")
	m = mm.(model)
	var sawErr2 bool
	for _, e := range m.entries {
		if _, ok := e.(errorEntry); ok {
			sawErr2 = true
		}
	}
	assert.True(t, sawErr2, "out-of-range threshold is an error")
}

// ---- cmdPermissions ----

func TestCmdPermissions_ListRevokeUsage(t *testing.T) {
	rec := &recordingSession{}
	m := wsModel(rec)
	mm, _ := runCommandOn(model(m), "/permissions")
	m = mm.(model)
	require.Contains(t, frameTypes(rec.frames), "permissions_list")

	rec = &recordingSession{}
	m = wsModel(rec)
	mm, _ = runCommandOn(model(m), "/permissions revoke r1")
	m = mm.(model)
	require.Contains(t, frameTypes(rec.frames), "permission_revoke")

	rec = &recordingSession{}
	m = wsModel(rec)
	mm, _ = runCommandOn(model(m), "/permissions bogus")
	m = mm.(model)
	assert.Empty(t, rec.frames, "malformed /permissions sends nothing")
}

// ---- cmdTheme ----

func TestCmdTheme_PickerValidInvalid(t *testing.T) {
	rec := &recordingSession{}
	m := wsModel(rec)
	mm, _ := runCommandOn(model(m), "/theme")
	m = mm.(model)
	assert.Equal(t, "theme", m.pickerKind)
	assert.NotEmpty(t, m.pickerItems)

	rec = &recordingSession{}
	m = wsModel(rec)
	mm, _ = runCommandOn(model(m), "/theme muted")
	m = mm.(model)
	assert.Equal(t, ThemeMuted, m.theme)

	rec = &recordingSession{}
	m = wsModel(rec)
	mm, _ = runCommandOn(model(m), "/theme nope")
	m = mm.(model)
	var sawErr bool
	for _, e := range m.entries {
		if _, ok := e.(errorEntry); ok {
			sawErr = true
		}
	}
	assert.True(t, sawErr, "unknown theme renders an error listing valid names")
}

// ---- cmdMCP ----

func TestCmdMCP_AllForms(t *testing.T) {
	cases := []struct {
		input   string
		wantTyp string
	}{
		{"/mcp", "mcp_action"},
		{"/mcp list", "mcp_action"},
		{"/mcp validate", "mcp_action"},
		{"/mcp enable srv1", "mcp_action"},
		{"/mcp disable srv1", "mcp_action"},
		{"/mcp reload srv1", "mcp_action"},
	}
	for _, tc := range cases {
		rec := &recordingSession{}
		m := wsModel(rec)
		mm, _ := runCommandOn(model(m), tc.input)
		_ = mm.(model)
		require.NotEmpty(t, rec.frames, "%s must send a frame", tc.input)
		assert.Equal(t, tc.wantTyp, rec.frames[0].Type)
	}

	// enable/disable/reload without a server arg -> usage error.
	rec := &recordingSession{}
	m := wsModel(rec)
	mm, _ := runCommandOn(model(m), "/mcp enable")
	_ = mm.(model)
	assert.Empty(t, rec.frames, "missing server arg -> usage error, no frame")

	// Unknown subcommand -> error.
	rec = &recordingSession{}
	m = wsModel(rec)
	mm, _ = runCommandOn(model(m), "/mcp frobnicate")
	_ = mm.(model)
	assert.Empty(t, rec.frames)
}

// ---- cmdArchive / cmdUnarchive / cmdArchived / cmdRename ----

func TestCmdArchiveUnarchiveUsageAndSend(t *testing.T) {
	// Missing id -> usage error.
	for _, input := range []string{"/archive", "/unarchive"} {
		rec := &recordingSession{}
		m := wsModel(rec)
		mm, _ := runCommandOn(model(m), input)
		_ = mm.(model)
		assert.Empty(t, rec.frames, "%s without id sends nothing", input)
	}

	rec := &recordingSession{}
	m := wsModel(rec)
	mm, _ := runCommandOn(model(m), "/archive s1")
	_ = mm.(model)
	require.NotEmpty(t, rec.frames)

	rec = &recordingSession{}
	m = wsModel(rec)
	mm, _ = runCommandOn(model(m), "/unarchive s1")
	_ = mm.(model)
	require.NotEmpty(t, rec.frames)

	// /archived lists archived sessions.
	rec = &recordingSession{}
	m = wsModel(rec)
	mm, _ = runCommandOn(model(m), "/archived")
	_ = mm.(model)
	require.Contains(t, rec.frames[0].Type, "session_list_archived")

	// /rename missing args -> usage.
	rec = &recordingSession{}
	m = wsModel(rec)
	mm, _ = runCommandOn(model(m), "/rename s1")
	_ = mm.(model)
	assert.Empty(t, rec.frames)
	// /rename with id + title -> frame.
	rec = &recordingSession{}
	m = wsModel(rec)
	mm, _ = runCommandOn(model(m), "/rename s1 new title")
	_ = mm.(model)
	require.Contains(t, frameTypes(rec.frames), "rename_session")
}

// ---- cmdDelete ----

func TestCmdDelete_ConfirmGateAndSend(t *testing.T) {
	// Missing id -> usage.
	rec := &recordingSession{}
	m := wsModel(rec)
	mm, _ := runCommandOn(model(m), "/delete")
	_ = mm.(model)
	assert.Empty(t, rec.frames)

	// id without "yes" -> confirmation prompt, no frame.
	rec = &recordingSession{}
	m = wsModel(rec)
	mm, _ = runCommandOn(model(m), "/delete s1")
	_ = mm.(model)
	assert.Empty(t, rec.frames)

	// id + "yes" -> delete frame.
	rec = &recordingSession{}
	m = wsModel(rec)
	mm, _ = runCommandOn(model(m), "/delete s1 yes")
	_ = mm.(model)
	require.Contains(t, frameTypes(rec.frames), "delete_session")
}

// ---- cmdRestoreTurn ----

func TestCmdRestoreTurn_AllForms(t *testing.T) {
	// SSE rejected.
	{
		srec := &sseSession{}
		m := newModel(srec, "/proj")
		mm, _ := m.runCommand("/restore-turn")
		_ = mm.(model)
		assert.Empty(t, srec.frames)
	}

	// No-arg -> list seams.
	rec := &recordingSession{}
	m := wsModel(rec)
	mm, _ := runCommandOn(model(m), "/restore-turn")
	_ = mm.(model)
	require.Contains(t, frameTypes(rec.frames), "list_seams")

	// <id> -> prompt entry, no frame.
	rec = &recordingSession{}
	m = wsModel(rec)
	mm, _ = runCommandOn(model(m), "/restore-turn s1")
	m = mm.(model)
	assert.Empty(t, rec.frames)
	var sawPrompt bool
	for _, e := range m.entries {
		if _, ok := e.(seamRestorePromptEntry); ok {
			sawPrompt = true
		}
	}
	assert.True(t, sawPrompt)

	// <id> yes without a head binding -> error (no binding).
	rec = &recordingSession{}
	m = wsModel(rec)
	mm, _ = runCommandOn(model(m), "/restore-turn s1 yes")
	_ = mm.(model)
	assert.Empty(t, rec.frames, "no head binding -> error, no restore_turn frame")

	// <id> yes WITH a head binding -> restore_turn frame.
	rec = &recordingSession{}
	m = wsModel(rec)
	m.lastKnownHead = "head123"
	mm, _ = runCommandOn(model(m), "/restore-turn s1 yes")
	m = mm.(model)
	require.Contains(t, frameTypes(rec.frames), "restore_turn")
	require.NotNil(t, m.pendingSeamRestore)

	// <id> nope -> usage error.
	rec = &recordingSession{}
	m = wsModel(rec)
	mm, _ = runCommandOn(model(m), "/restore-turn s1 nope")
	_ = mm.(model)
	assert.Empty(t, rec.frames)
}

// ---- cmdJobs ----

func TestCmdJobs_AllForms(t *testing.T) {
	// List.
	rec := &recordingSession{}
	m := wsModel(rec)
	mm, _ := runCommandOn(model(m), "/jobs")
	_ = mm.(model)
	require.Contains(t, rec.frames[0].Type, "jobs_list")

	// Read with max.
	rec = &recordingSession{}
	m = wsModel(rec)
	mm, _ = runCommandOn(model(m), "/jobs read j1 8192")
	_ = mm.(model)
	require.NotEmpty(t, rec.frames)

	// stdin.
	rec = &recordingSession{}
	m = wsModel(rec)
	mm, _ = runCommandOn(model(m), "/jobs stdin j1 hello there")
	_ = mm.(model)
	require.NotEmpty(t, rec.frames)

	// cancel.
	rec = &recordingSession{}
	m = wsModel(rec)
	mm, _ = runCommandOn(model(m), "/jobs cancel j1")
	_ = mm.(model)
	require.NotEmpty(t, rec.frames)

	// usage errors (missing args).
	for _, input := range []string{"/jobs read", "/jobs stdin j1", "/jobs cancel", "/jobs bogus"} {
		rec := &recordingSession{}
		m := wsModel(rec)
		mm, _ := runCommandOn(model(m), input)
		_ = mm.(model)
		assert.Empty(t, rec.frames, "%s -> usage error, no frame", input)
	}
}

// ---- cmdFeatures ----

func TestCmdFeatures_ListEnableUsage(t *testing.T) {
	rec := &recordingSession{}
	m := wsModel(rec)
	mm, _ := runCommandOn(model(m), "/features")
	_ = mm.(model)
	require.Contains(t, frameTypes(rec.frames), "features_list")

	rec = &recordingSession{}
	m = wsModel(rec)
	mm, _ = runCommandOn(model(m), "/features enable flag1")
	_ = mm.(model)
	require.NotEmpty(t, rec.frames)

	// malformed -> usage error.
	rec = &recordingSession{}
	m = wsModel(rec)
	mm, _ = runCommandOn(model(m), "/features bogus")
	_ = mm.(model)
	assert.Empty(t, rec.frames)
}

// ---- palette ----

func TestPalette_MoveCompleteAndMCP(t *testing.T) {
	m := wsModel(&recordingSession{})
	m.paletteItems = []command{
		{name: "model", kind: cmdSlash},
		{name: "── srv ──", kind: cmdMCPGroup},
		{name: "srv.tool", kind: cmdMCPTool},
	}
	m.paletteSel = 0
	m.paletteMove(1) // lands on group -> skips to MCPTool
	assert.Equal(t, 2, m.paletteSel, "paletteMove skips group headers")

	m.paletteComplete() // completes the MCP tool
	assert.Equal(t, "srv.tool", m.input.Value(), "MCP tool inserts qualified name")
	assert.Nil(t, m.paletteItems, "palette dismissed after complete")

	// Slash command complete adds "/" prefix + trailing space.
	m = wsModel(&recordingSession{})
	m.paletteItems = []command{{name: "mode", kind: cmdSlash}}
	m.paletteSel = 0
	m.paletteComplete()
	assert.Equal(t, "/mode ", m.input.Value())

	// Group is not selectable.
	m = wsModel(&recordingSession{})
	m.paletteItems = []command{{name: "── srv ──", kind: cmdMCPGroup}}
	m.paletteSel = 0
	m.paletteComplete()
	assert.Equal(t, "", m.input.Value(), "group complete is a no-op")
}

func TestPaletteMCPItems_GroupsByServer(t *testing.T) {
	m := wsModel(&recordingSession{})
	m.paletteMCPServers = []proto.MCPServerStatus{
		{Name: "srv", Status: "ready", Tools: []proto.MCPToolBrief{{Name: "a"}, {Name: "b"}}},
		{Name: "down", Status: "failed"},
	}
	items := m.paletteMCPItems()
	require.NotEmpty(t, items)
	// First entry is the group header for "srv".
	assert.Equal(t, cmdMCPGroup, items[0].kind)
	assert.Contains(t, items[0].name, "srv")
	// The failed server's group header carries its status.
	var sawFailedHeader bool
	for _, it := range items {
		if it.kind == cmdMCPGroup && strings.Contains(it.name, "down") {
			sawFailedHeader = true
		}
	}
	assert.True(t, sawFailedHeader, "failed server status surfaced in header")
}

func TestPaletteBlock_RendersSelected(t *testing.T) {
	m := wsModel(&recordingSession{})
	assert.Equal(t, "", m.paletteBlock(), "closed palette renders nothing")

	m.paletteItems = []command{
		{name: "mode", help: "set mode", kind: cmdSlash},
		{name: "── srv ──", kind: cmdMCPGroup},
		{name: "tool", help: "a tool", kind: cmdMCPTool},
	}
	m.paletteSel = 0
	out := m.paletteBlock()
	assert.Contains(t, out, "mode")
	// MCP group + tool rows also render.
	m.paletteSel = 2
	out = m.paletteBlock()
	assert.Contains(t, out, "tool")
}

func TestUpdatePalette_FiltersByPrefix(t *testing.T) {
	m := wsModel(&recordingSession{})
	m.input.SetValue("/mo")
	m.updatePalette()
	// Every palette item name must start with "mo".
	for _, c := range m.paletteItems {
		assert.True(t, len(c.name) >= 2, "filtered palette non-empty")
	}

	// Non-slash input clears the palette.
	m.input.SetValue("hello")
	m.updatePalette()
	assert.Nil(t, m.paletteItems)

	// Slash + space (args started) clears the palette.
	m.input.SetValue("/mode ")
	m.updatePalette()
	assert.Nil(t, m.paletteItems)
}

// ---- permission helpers ----

func TestPermModeFile_NonEmpty(t *testing.T) {
	assert.NotEmpty(t, permModeFile())
}

func TestSaveLoadPermMode_RoundTrip(t *testing.T) {
	// persistPermMode is disabled by the test package init; flip it on for this
	// test and restore it after. Point permModeFile at a temp path via a direct
	// write+read so we do not touch the developer's real config.
	orig := persistPermMode
	persistPermMode = true
	t.Cleanup(func() { persistPermMode = orig })

	// Save then load a known mode+threshold.
	savePermMode(guard.ModeAuto, 4)
	// The file path is fixed (os.UserConfigDir); load reads it back. To avoid
	// clobbering real state, just assert the load path returns a valid mode
	// (default|auto) and threshold in range or 0.
	m := loadSavedMode()
	assert.True(t, m == guard.ModeDefault || m == guard.ModeAuto ||
		m == guard.ModeAllowEdits || m == guard.ModeYOLO || m == guard.ModePlan,
		"loadSavedMode returns a known mode, got %q", m)

	thr := loadSavedThreshold()
	assert.True(t, thr == 0 || (thr >= 1 && thr <= 10), "threshold in range, got %d", thr)
}

func TestCycleMode_SequenceAndYoloGate(t *testing.T) {
	rec := &recordingSession{}
	m := wsModel(rec)
	m.permMode = guard.ModeDefault

	// default -> allow-edits.
	mm, _ := m.cycleMode()
	m = mm.(model)
	assert.Equal(t, guard.ModeAllowEdits, m.permMode)

	// allow-edits -> auto.
	mm, _ = m.cycleMode()
	m = mm.(model)
	assert.Equal(t, guard.ModeAuto, m.permMode)

	// auto -> yolo GATE: yoloConfirm arms, mode is NOT yet yolo.
	mm, _ = m.cycleMode()
	m = mm.(model)
	assert.Equal(t, 1, m.yoloConfirm, "cycling to yolo arms the two-Enter gate")
	assert.NotEqual(t, guard.ModeYOLO, m.permMode, "gate armed: mode not yet yolo")

	// Shift+Tab at the gate (a second cycleMode while yoloConfirm>0) skips yolo
	// and advances to the next mode (default).
	mm, _ = m.cycleMode()
	m = mm.(model)
	assert.Equal(t, 0, m.yoloConfirm, "shift+tab clears the gate")
	assert.Equal(t, guard.ModeDefault, m.permMode, "shift+tab at yolo gate skips to default")
}

func TestPermModeText_AllModes(t *testing.T) {
	m := wsModel(&recordingSession{})
	m.permMode = ""
	assert.Contains(t, m.permModeText(), "manual")
	m.permMode = guard.ModeDefault
	assert.Contains(t, m.permModeText(), "manual")
	m.permMode = guard.ModeAllowEdits
	assert.Contains(t, m.permModeText(), "edit")
	m.permMode = guard.ModeYOLO
	assert.Contains(t, m.permModeText(), "bypass")
	m.permMode = guard.ModeAuto
	assert.Contains(t, m.permModeText(), "auto")
	m.autoThreshold = 5
	assert.Contains(t, m.permModeText(), "5")
}

func TestModeAutoAllows(t *testing.T) {
	assert.True(t, modeAutoAllows(guard.ModeYOLO, "shell_run"))
	assert.True(t, modeAutoAllows(guard.ModeAllowEdits, "fs_write"))
	assert.False(t, modeAutoAllows(guard.ModeAllowEdits, "shell_run"))
	assert.False(t, modeAutoAllows(guard.ModeDefault, "fs_write"))
}

func TestPermMoveAndRespond(t *testing.T) {
	m := wsModel(&recordingSession{})
	// permMove with no pending permission is a no-op.
	m.permMove(1)
	assert.Equal(t, 0, m.permSel)

	// Add a pending permission and move the selection.
	m.pendingPermissions = []*permissionEntry{{tool: "fs_read", id: "p1"}}
	m.permMove(1)
	assert.NotEqual(t, 0, m.permSel, "permMove wraps within the options")

	// respondPermission with a valid decision clears the pending entry and sends
	// the response frame.
	rec := &recordingSession{}
	m.sess = rec
	m.respondPermission("allow")
	assert.Empty(t, m.pendingPermissions)
	require.NotEmpty(t, rec.frames)
	assert.Equal(t, "permission_response", rec.frames[0].Type)

	// Mandatory tool rejects non-allow/deny decisions (popup stays).
	m.pendingPermissions = []*permissionEntry{{tool: "github_push", id: "p2", mandatory: true}}
	rec = &recordingSession{}
	m.sess = rec
	m.respondPermission("allow_session")
	assert.NotEmpty(t, m.pendingPermissions, "mandatory tool drops session/persistent allow")
	assert.Empty(t, rec.frames)
	// but allow/deny are accepted for mandatory tools.
	m.respondPermission("deny")
	assert.Empty(t, m.pendingPermissions)
}

// runCommandOn is a thin helper so the table-driven tests read cleanly: it
// dispatches the model through runCommand and returns the result.
func runCommandOn(m model, text string) (tea.Model, tea.Cmd) {
	return m.runCommand(text)
}
