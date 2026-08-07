package tui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/cli"
	"github.com/x6nux/yanshi/internal/guard"
)

func init() { persistPermMode = false }

// ---- permission queue (parallel tool calls) ----

// TestPermissionQueue_DoesNotDropEarlierRequests is the regression guard for
// the bug where concurrent permission_request frames overwrote a single
// pendingPermission pointer: the first request was lost and its tool hung
// forever. With the queue, both are retained and resolved in FIFO order.
func TestPermissionQueue_DoesNotDropEarlierRequests(t *testing.T) {
	rec := &recordingSession{}
	m := newModel(rec, "/proj")

	// Two permission requests arrive before either is answered (parallel tools).
	m = m.applyEvent(cli.StreamEvent{Kind: "permission_request", ID: "r1", ToolName: "fs_write", ToolArgs: "{}"})
	m = m.applyEvent(cli.StreamEvent{Kind: "permission_request", ID: "r2", ToolName: "shell_run", ToolArgs: "{}"})

	require.Len(t, m.pendingPermissions, 2, "both requests must be queued")
	assert.Equal(t, "r1", m.pendingPermission().id, "front is the first (FIFO)")

	// Resolve the front (r1). Its response must be sent; r2 becomes the front.
	m.respondPermission("allow")
	require.Len(t, m.pendingPermissions, 1, "resolving dequeues the front")
	require.Len(t, rec.frames, 1)
	assert.Equal(t, "r1", rec.frames[0].ID)
	assert.Equal(t, "allow", rec.frames[0].Decision)
	assert.Equal(t, "r2", m.pendingPermission().id, "second is now the front")

	// Resolve r2.
	m.respondPermission("deny")
	require.Empty(t, m.pendingPermissions)
	require.Len(t, rec.frames, 2)
	assert.Equal(t, "r2", rec.frames[1].ID)
	assert.Equal(t, "deny", rec.frames[1].Decision)
}

// TestPermissionPopup_ShowsQueueBadge verifies the "(N queued)" badge renders
// when more than one request is stacked.
func TestPermissionPopup_ShowsQueueBadge(t *testing.T) {
	m := newModel(&fakeSession{}, "/proj")
	m = m.applyEvent(cli.StreamEvent{Kind: "permission_request", ID: "r1", ToolName: "fs_write", ToolArgs: "{}"})
	m = m.applyEvent(cli.StreamEvent{Kind: "permission_request", ID: "r2", ToolName: "shell_run", ToolArgs: "{}"})

	pp := m.permissionPopup()
	assert.Contains(t, pp, "1 queued", "popup must badge the stacked count")
}

// ---- Shift+Tab mode cycling ----

// TestShiftTab_CyclesNonYoloModes proves Shift+Tab advances default →
// allow-edits and emits a set_mode frame, without the yolo gate (yolo is the
// next step, gated separately).
func TestShiftTab_CyclesNonYoloModes(t *testing.T) {
	rec := &recordingSession{}
	m := newModel(rec, "/proj")
	require.Equal(t, guard.ModeDefault, m.permMode)

	mm, _ := m.Update(tea.KeyMsg{Type: tea.KeyShiftTab})
	m = mm.(model)
	assert.Equal(t, guard.ModeAllowEdits, m.permMode, "default -> allow-edits")
	require.Len(t, rec.frames, 1)
	assert.Equal(t, "set_mode", rec.frames[0].Type)
	assert.Equal(t, "allow-edits", rec.frames[0].Mode)
}

// TestShiftTab_YoloGate_RequiresDoubleEnter proves entering yolo mode needs two
// Enter presses: Shift+Tab arms the gate (stage 1), the first Enter advances to
// stage 2, and only the SECOND Enter actually switches to yolo and sends the
// frame. A non-Enter key at any stage cancels.
func TestShiftTab_YoloGate_RequiresDoubleEnter(t *testing.T) {
	rec := &recordingSession{}
	m := newModel(rec, "/proj")
	// Advance to allow-edits, then auto (so the next target is yolo).
	mm, _ := m.Update(tea.KeyMsg{Type: tea.KeyShiftTab})
	m = mm.(model)
	require.Equal(t, guard.ModeAllowEdits, m.permMode)
	require.Len(t, rec.frames, 1)

	mm, _ = m.Update(tea.KeyMsg{Type: tea.KeyShiftTab})
	m = mm.(model)
	require.Equal(t, guard.ModeAuto, m.permMode)
	require.Len(t, rec.frames, 2)

	// Shift+Tab targets yolo -> arms stage 1, NO mode change, NO frame.
	mm, _ = m.Update(tea.KeyMsg{Type: tea.KeyShiftTab})
	m = mm.(model)
	assert.Equal(t, 1, m.yoloConfirm, "first Shift+Tab arms the gate")
	assert.Equal(t, guard.ModeAuto, m.permMode, "mode unchanged at stage 1")
	assert.Len(t, rec.frames, 2, "no set_mode frame until confirmed")

	// First Enter -> stage 2 (still not yolo).
	mm, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = mm.(model)
	assert.Equal(t, 2, m.yoloConfirm)
	assert.Equal(t, guard.ModeAuto, m.permMode, "mode unchanged at stage 2")
	assert.Len(t, rec.frames, 2)

	// Second Enter -> yolo, frame sent.
	mm, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = mm.(model)
	assert.Equal(t, 0, m.yoloConfirm, "gate cleared on confirm")
	assert.Equal(t, guard.ModeYOLO, m.permMode)
	require.Len(t, rec.frames, 3)
	assert.Equal(t, "yolo", rec.frames[2].Mode)
}

// TestMidPopup_ModeSwitchAutoResolves is the core of "切换之后就直接以新权限来
// 判断": while a permission popup is showing, switching the mode must apply the
// new mode to the CURRENT pending request, not just future ones. Here an
// fs_write popup is showing; switching to allow-edits auto-allows it (it's an
// edit tool) and empties the queue.
func TestMidPopup_ModeSwitchAutoResolves(t *testing.T) {
	rec := &recordingSession{}
	m := newModel(rec, "/proj")
	m = m.applyEvent(cli.StreamEvent{Kind: "permission_request", ID: "r1", ToolName: "fs_write", ToolArgs: "{}"})
	require.NotNil(t, m.pendingPermission())

	// Shift+Tab: default -> allow-edits. fs_write is an edit tool, so the new
	// mode auto-allows the pending request.
	mm, _ := m.Update(tea.KeyMsg{Type: tea.KeyShiftTab})
	m = mm.(model)
	assert.Equal(t, guard.ModeAllowEdits, m.permMode)
	assert.Nil(t, m.pendingPermission(), "allow-edits must auto-resolve the pending edit")

	// The allow response + the set_mode frame were both sent.
	var sawAllow, sawSetMode bool
	for _, f := range rec.frames {
		if f.Type == "permission_response" && f.ID == "r1" && f.Decision == "allow" {
			sawAllow = true
		}
		if f.Type == "set_mode" && f.Mode == "allow-edits" {
			sawSetMode = true
		}
	}
	assert.True(t, sawAllow, "the pending fs_write must be answered allow")
	assert.True(t, sawSetMode, "set_mode must be sent for future requests")
}

// TestMidPopup_ModeSwitchKeepsNonEdit proves switching to allow-edits does NOT
// auto-resolve a non-edit tool (shell_run): the popup stays for the user to
// answer, but future edit tools will be auto-approved.
func TestMidPopup_ModeSwitchKeepsNonEdit(t *testing.T) {
	rec := &recordingSession{}
	m := newModel(rec, "/proj")
	m = m.applyEvent(cli.StreamEvent{Kind: "permission_request", ID: "r1", ToolName: "shell_run", ToolArgs: "{}"})
	require.NotNil(t, m.pendingPermission())

	mm, _ := m.Update(tea.KeyMsg{Type: tea.KeyShiftTab}) // -> allow-edits
	m = mm.(model)
	assert.Equal(t, guard.ModeAllowEdits, m.permMode)
	assert.NotNil(t, m.pendingPermission(), "non-edit tool stays in the popup under allow-edits")
	assert.Equal(t, "r1", m.pendingPermission().id)
}

// TestMidPopup_YoloAutoResolvesAll proves confirming YOLO (double-Enter) while
// popups are queued auto-allows every pending request, not just the front. Uses
// non-edit tools so the allow-edits intermediate step doesn't pre-resolve them.
func TestMidPopup_YoloAutoResolvesAll(t *testing.T) {
	rec := &recordingSession{}
	m := newModel(rec, "/proj")
	m = m.applyEvent(cli.StreamEvent{Kind: "permission_request", ID: "r1", ToolName: "shell_run", ToolArgs: "{}"})
	m = m.applyEvent(cli.StreamEvent{Kind: "permission_request", ID: "r2", ToolName: "shell_run", ToolArgs: "{}"})
	require.Len(t, m.pendingPermissions, 2)

	// The Shift+Tab cycle is 4-state: default → allow-edits → auto → yolo
	// (guard.cycleOrder), and yolo is gated behind a double-Enter confirmation.
	// So reaching the yolo gate from default takes THREE ShiftTabs; the two
	// Enters then confirm yolo, whose sendMode auto-allows every pending request.
	// shell_run is a non-edit tool, so the allow-edits/auto intermediates keep
	// both requests queued until yolo resolves them.
	mm, _ := m.Update(tea.KeyMsg{Type: tea.KeyShiftTab}) // default -> allow-edits
	m = mm.(model)
	require.Len(t, m.pendingPermissions, 2, "allow-edits keeps non-edit tools queued")
	mm, _ = m.Update(tea.KeyMsg{Type: tea.KeyShiftTab}) // allow-edits -> auto
	m = mm.(model)
	require.Len(t, m.pendingPermissions, 2, "auto keeps non-edit tools queued (no client-side risk rating)")
	mm, _ = m.Update(tea.KeyMsg{Type: tea.KeyShiftTab}) // auto -> arm yolo gate
	m = mm.(model)
	require.Equal(t, 1, m.yoloConfirm, "third Shift+Tab arms the yolo gate")
	mm, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter}) // gate stage 1 -> 2
	m = mm.(model)
	mm, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter}) // stage 2 -> confirm yolo
	m = mm.(model)

	assert.Equal(t, guard.ModeYOLO, m.permMode)
	assert.Empty(t, m.pendingPermissions, "yolo must auto-allow ALL pending requests")

	allowed := 0
	for _, f := range rec.frames {
		if f.Type == "permission_response" && f.Decision == "allow" {
			allowed++
		}
	}
	assert.Equal(t, 2, allowed, "both pending requests must be answered allow")
}
func TestShiftTab_YoloGate_CancelOnOtherKey(t *testing.T) {
	rec := &recordingSession{}
	m := newModel(rec, "/proj")
	mm, _ := m.Update(tea.KeyMsg{Type: tea.KeyShiftTab}) // default -> allow-edits
	m = mm.(model)
	mm, _ = m.Update(tea.KeyMsg{Type: tea.KeyShiftTab}) // default -> auto
	m = mm.(model)
	mm, _ = m.Update(tea.KeyMsg{Type: tea.KeyShiftTab}) // arm yolo gate
	m = mm.(model)
	require.Equal(t, 1, m.yoloConfirm)

	// A letter key cancels.
	mm, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
	m = mm.(model)
	assert.Equal(t, 0, m.yoloConfirm, "non-Enter cancels the gate")
	assert.Equal(t, guard.ModeAuto, m.permMode, "mode unchanged")
}

// TestStatus_MirrorsPermMode proves a status frame carrying PermMode updates the
// TUI's mirror so the footer reflects the server-side mode.
func TestStatus_MirrorsPermMode(t *testing.T) {
	m := newModel(&fakeSession{}, "/proj")
	m = m.applyEvent(cli.StreamEvent{Kind: "status", PermMode: "yolo"})
	assert.Equal(t, guard.ModeYOLO, m.permMode)
}

// TestFooter_ShowsPermMode proves the footer renders the permission mode
// segment (colored by danger) for non-default modes.
func TestFooter_ShowsPermMode(t *testing.T) {
	m := newModel(&fakeSession{}, "/proj")
	m.permMode = guard.ModeYOLO
	assert.Contains(t, m.statusHeader(), "bypass permissions")

	m2 := newModel(&fakeSession{}, "/proj")
	m2.permMode = guard.ModeAuto
	assert.Contains(t, m2.statusHeader(), "auto mode")

	m3 := newModel(&fakeSession{}, "/proj")
	m3.permMode = guard.ModeDefault
	assert.Contains(t, m3.statusHeader(), "manual mode", "default mode is visible in the footer")
}

// TestMandatoryPermissionSurvivesYOLOAndAuto proves that mandatory-approval
// permission requests are NOT auto-resolved when the user is in YOLO or Auto
// mode (unlike regular permissions which are auto-allowed in those modes).
// Also verifies that the popup offers only Allow/Deny (no always_allow).
func TestMandatoryPermissionSurvivesYOLOAndAuto(t *testing.T) {
	for _, mode := range []guard.PermissionMode{guard.ModeYOLO, guard.ModeAuto} {
		rec := &recordingSession{}
		m := newModel(rec, "/proj")
		m.permMode = mode
		m.pendingPermissions = []*permissionEntry{{id: "p1", tool: "github_comment", mandatory: true}}
		m.autoResolvePendingByMode()
		assert.Len(t, m.pendingPermissions, 1, "mode=%s: mandatory permission disappeared", mode)
		assert.Empty(t, rec.frames, "mode=%s: auto-resolved a mandatory permission", mode)
		opts := permissionOptions(m.pendingPermission())
		if assert.Len(t, opts, 2, "mandatory options should be Allow/Deny only") {
			assert.Equal(t, "allow", opts[0].decision)
			assert.Equal(t, "deny", opts[1].decision)
		}
	}
}

// TestForcePromptPermissionSurvivesYOLOAndAuto is the ForcePrompt twin of
// TestMandatoryPermissionSurvivesYOLOAndAuto. The server refuses to
// auto-resolve force-prompt requests (forcePromptTools -> task_cancel) and
// RequireApproval's destructive actions (revert_turn), but that refusal is
// undone client-side unless the flag reaches the TUI: autoResolvePendingByMode
// runs on every mode switch and answers "allow" for anything it does not
// consider mandatory. Before force_prompt went on the wire this test failed
// with `pendingPermissions` empty and one permission_response{allow} frame
// sent — an authorization the user never gave.
//
// It enters at cli.StreamEvent and drives StreamEvent -> applyEvent on purpose:
// constructing the permissionEntry directly would set `mandatory` by hand and
// pass even with the wire field missing. It does NOT cover the two hops in
// front of it — this package cannot reach the unexported mapper — so the chain
// is pinned in three pieces and this is only the last one:
// `internal/api/http/ws_perm_test.go::TestPermissionRequestFrameCarriesForcePrompt`
// (server flags -> ServerFrame),
// `internal/cli/streamevent_parity_test.go::TestWSBackend_ForcePromptReachesStreamEvent`
// (ServerFrame -> wire -> StreamEvent), then this one. The middle piece was
// missing until a mutation showed the whole suite staying green with
// toStreamEvent's ForcePrompt assignment deleted.
func TestForcePromptPermissionSurvivesYOLOAndAuto(t *testing.T) {
	for _, tool := range []string{"task_cancel", "revert_turn"} {
		for _, mode := range []guard.PermissionMode{guard.ModeYOLO, guard.ModeAuto} {
			rec := &recordingSession{}
			m := newModel(rec, "/proj")
			m = m.applyEvent(cli.StreamEvent{
				Kind: "permission_request", ID: "p1", ToolName: tool,
				ToolArgs: "{}", Reason: "tool requires explicit approval",
				ForcePrompt: true,
			})
			require.NotNil(t, m.pendingPermission(), "tool=%s: prompt did not open", tool)
			require.True(t, m.pendingPermission().mandatory,
				"tool=%s: force_prompt must mark the entry mandatory", tool)

			m.permMode = mode
			m.autoResolvePendingByMode()
			assert.Len(t, m.pendingPermissions, 1,
				"tool=%s mode=%s: force-prompt permission disappeared", tool, mode)
			assert.Empty(t, rec.frames,
				"tool=%s mode=%s: TUI answered a force-prompt permission on the user's behalf", tool, mode)

			// Sticky-allow must not be offered either: the server discards a
			// sticky answer for these tools, so showing the option would
			// promise a durability that does not exist.
			opts := permissionOptions(m.pendingPermission())
			if assert.Len(t, opts, 2, "tool=%s: force-prompt options should be Allow/Deny only", tool) {
				assert.Equal(t, "allow", opts[0].decision)
				assert.Equal(t, "deny", opts[1].decision)
			}
		}
	}
}

// TestOrdinaryPermissionStillAutoResolvesInYOLO is the reverse probe for the
// test above: without force_prompt / approval_required, YOLO must still
// auto-allow. A fix that simply made every pending permission survive would
// pass the forward test and silently disable YOLO.
func TestOrdinaryPermissionStillAutoResolvesInYOLO(t *testing.T) {
	rec := &recordingSession{}
	m := newModel(rec, "/proj")
	m = m.applyEvent(cli.StreamEvent{
		Kind: "permission_request", ID: "p1", ToolName: "fs_write", ToolArgs: "{}",
	})
	require.NotNil(t, m.pendingPermission())
	m.permMode = guard.ModeYOLO
	m.autoResolvePendingByMode()
	assert.Empty(t, m.pendingPermissions, "YOLO must still auto-allow ordinary permissions")
	require.Len(t, rec.frames, 1)
	assert.Equal(t, "allow", rec.frames[0].Decision)
}
