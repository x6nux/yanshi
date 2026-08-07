package tui

import (
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/guard"
	"github.com/x6nux/yanshi/internal/proto"
)

// ---- view.go pickers ----

func TestSessionPickerPopup_Render(t *testing.T) {
	m := wsModel(&recordingSession{})
	assert.Equal(t, "", m.sessionPickerPopup(), "no sessions -> empty")

	m.restoreSessions = []proto.SessionInfo{
		{ID: "s1", Title: "first", Model: "gpt", MsgCount: 5, CreatedAt: time.Now().Unix()},
		{ID: "s2", Title: "", Model: "", MsgCount: 0, CreatedAt: time.Now().Unix()},
	}
	out := m.sessionPickerPopup()
	assert.Contains(t, out, "restore session")
	assert.Contains(t, out, "s1")
	assert.Contains(t, out, "first")
	assert.Contains(t, out, "(untitled)", "empty title falls back")

	// Many sessions -> scroll hint + windowed render.
	m.restoreCursor = 8
	for i := 0; i < 12; i++ {
		m.restoreSessions = append(m.restoreSessions, proto.SessionInfo{ID: "sx", Title: "x"})
	}
	out = m.sessionPickerPopup()
	assert.Contains(t, out, "scroll")
}

func TestPickerPopup_Render(t *testing.T) {
	m := wsModel(&recordingSession{})
	assert.Equal(t, "", m.pickerPopup(), "closed picker -> empty")

	for _, kind := range []string{"model", "mode", "theme"} {
		m.pickerKind = kind
		m.pickerItems = []pickerItem{{name: "a", description: "desc", current: true}}
		m.pickerCursor = 0
		out := m.pickerPopup()
		assert.NotEmpty(t, out)
		assert.Contains(t, out, "a")
		assert.Contains(t, out, "desc")
	}

	// Unknown kind still renders the item rows (title stays empty).
	m.pickerKind = "unknown"
	out := m.pickerPopup()
	assert.Contains(t, out, "a")
}

// ---- handleSelectMouse ----

func TestHandleSelectMouse_PressMotionRelease(t *testing.T) {
	m := wsModel(&recordingSession{})

	// Press starts a selection.
	cmd := m.handleSelectMouse(tea.MouseMsg{Action: tea.MouseActionPress, X: 2, Y: 3})
	assert.Nil(t, cmd)
	assert.True(t, m.selecting)
	assert.Equal(t, 3, m.selAnchorRow)

	// Motion extends the selection (no cmd returned).
	cmd = m.handleSelectMouse(tea.MouseMsg{Action: tea.MouseActionMotion, X: 5, Y: 6})
	assert.Nil(t, cmd)
	assert.Equal(t, 6, m.selLineRow)

	// Release ends the selection (a copy cmd is returned when text was selected;
	// the key assertion is that selecting is cleared and no panic occurs).
	cmd = m.handleSelectMouse(tea.MouseMsg{Action: tea.MouseActionRelease, X: 5, Y: 6})
	assert.False(t, m.selecting, "release clears selecting")

	// Release when NOT selecting is a no-op.
	cmd = m.handleSelectMouse(tea.MouseMsg{Action: tea.MouseActionRelease, X: 0, Y: 0})
	assert.Nil(t, cmd)
}

// ---- parse helpers ----

func TestParseEditStrings(t *testing.T) {
	oldS, newS, ok := parseEditStrings("")
	assert.False(t, ok)

	oldS, newS, ok = parseEditStrings("{bad json")
	assert.False(t, ok)

	oldS, newS, ok = parseEditStrings(`{"old_string":"","new_string":""}`)
	assert.False(t, ok, "both empty -> not ok")

	oldS, newS, ok = parseEditStrings(`{"old_string":"a","new_string":"b"}`)
	require.True(t, ok)
	assert.Equal(t, "a", oldS)
	assert.Equal(t, "b", newS)

	// Only new_string (fs_write-style) is still ok.
	_, newS, ok = parseEditStrings(`{"old_string":"","new_string":"only new"}`)
	require.True(t, ok)
	assert.Equal(t, "only new", newS)
}

func TestParseWriteContent(t *testing.T) {
	_, ok := parseWriteContent("")
	assert.False(t, ok)
	_, ok = parseWriteContent("{bad")
	assert.False(t, ok)
	content, ok := parseWriteContent(`{"content":"hello"}`)
	require.True(t, ok)
	assert.Equal(t, "hello", content)
	// content absent -> ok=false.
	_, ok = parseWriteContent(`{"other":"x"}`)
	assert.False(t, ok)
}

// ---- history ----

func TestHistory_SaveRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/history.jsonl"
	h := &History{path: path, cap: 10, items: []historyItem{{Text: "a"}, {Text: "b"}}}
	require.NoError(t, h.Save())

	loaded, err := LoadHistory(path, 10)
	require.NoError(t, err)
	assert.Equal(t, []string{"a", "b"}, historyTexts(loaded))
}

func TestRefreshHistorySearch_AndPopup(t *testing.T) {
	m := wsModel(&recordingSession{})
	// nil history/search -> no-op.
	m.refreshHistorySearch()

	m.history = &History{items: []historyItem{{Text: "find me"}, {Text: "other"}}}
	m.historySearch = &historyState{visible: true, query: "find"}
	m.refreshHistorySearch()
	require.NotEmpty(t, m.historySearch.items)
	assert.Contains(t, m.historySearch.items[0].Text, "find")

	// Popup renders the matched items.
	out := m.historyPopup()
	assert.Contains(t, out, "History")
	assert.Contains(t, out, "find me")

	// Empty query -> all items; popup renders them.
	m.historySearch.query = ""
	m.refreshHistorySearch()
	out = m.historyPopup()
	assert.Contains(t, out, "other")

	// No matches -> "no matching prompts".
	m.historySearch.query = "zzzzz"
	m.refreshHistorySearch()
	out = m.historyPopup()
	assert.Contains(t, out, "no matching prompts")

	// Cursor beyond items is clamped.
	m.historySearch.cursor = 999
	m.historySearch.query = ""
	m.refreshHistorySearch()
	assert.Less(t, m.historySearch.cursor, len(m.historySearch.items))

	// historySearch nil/hidden -> popup empty.
	m.historySearch = nil
	assert.Equal(t, "", m.historyPopup())
}

// historyTexts is a small helper extracting the text of each item.
func historyTexts(h *History) []string {
	items := h.Search("", 100)
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = it.Text
	}
	return out
}

// ---- activityTick closure ----

func TestActivityTick_FiresMsg(t *testing.T) {
	cmd := activityTick()
	require.NotNil(t, cmd)
	// Executing the Cmd waits ~42ms then fires an activityTickMsg.
	msg := cmd()
	_, ok := msg.(activityTickMsg)
	assert.True(t, ok, "activityTick fires an activityTickMsg")
}

// ---- cmdModel plan-mode branch ----

func TestCmdModel_PlanModeClearsPrePlanOnSwitch(t *testing.T) {
	// In plan mode, /model <name> (non-plan) clears the saved pre-plan mode.
	rec := &recordingSession{}
	m := wsModel(rec)
	m.permMode = guard.ModePlan
	m.prePlanMode = guard.ModeAuto
	mm, _ := runCommandOn(model(m), "/model gpt-4o")
	m = mm.(model)
	assert.Equal(t, guard.PermissionMode(""), m.prePlanMode, "switching model from plan clears prePlanMode")

	// /model plan delegates to cmdPlan.
	rec = &recordingSession{}
	m = wsModel(rec)
	mm, _ = runCommandOn(model(m), "/model plan")
	m = mm.(model)
	assert.Equal(t, guard.ModePlan, m.permMode)

	// /model (no args) opens the picker + list_models.
	rec = &recordingSession{}
	m = wsModel(rec)
	mm, _ = runCommandOn(model(m), "/model")
	m = mm.(model)
	assert.Equal(t, "model", m.pickerKind)
	require.Contains(t, frameTypes(rec.frames), "list_models")
}
