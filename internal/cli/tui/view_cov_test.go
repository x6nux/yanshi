package tui

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/x6nux/yanshi/internal/proto"
)

// ---- highlightLines (direct edge cases) ----

// TestCov_HighlightLines_Edges covers the negative-row skip, the startCol/endCol
// clamps, the empty-span skip, and the multi-row first/middle/last spans.
func TestCov_HighlightLines_Edges(t *testing.T) {
	// Negative row (lo.row < 0) → skipped.
	highlightLines("a\nb", selPos{row: -1, col: 0}, selPos{row: 1, col: 1})
	// startCol < 0 → clamped to 0.
	highlightLines("abc", selPos{row: 0, col: -5}, selPos{row: 0, col: 2})
	// endCol > width → clamped to width.
	highlightLines("ab", selPos{row: 0, col: 0}, selPos{row: 0, col: 100})
	// startCol >= endCol → nothing highlighted on the row.
	highlightLines("abc", selPos{row: 0, col: 3}, selPos{row: 0, col: 3})
	// Multi-row: first (lo.col→end), middle (whole), last (start→hi.col).
	out := highlightLines("aaa\nbbb\nccc", selPos{row: 0, col: 1}, selPos{row: 2, col: 2})
	assert.Contains(t, out, "aaa")
}

// ---- selRange ----

// TestCov_SelRange_Swapped covers the "return b, a" branch when the anchor is
// past the live end.
func TestCov_SelRange_Swapped(t *testing.T) {
	m := newModel(&fakeSession{}, "/proj")
	m.selAnchorRow, m.selAnchorCol = 5, 0
	m.selLineRow, m.selLineCol = 2, 0
	lo, hi := m.selRange()
	assert.Equal(t, 2, lo.row)
	assert.Equal(t, 5, hi.row)
}

// ---- pickerPopup ----

// TestCov_PickerPopup_CursorAndOther covers both the cursor item and a
// non-cursor item (the else branch).
func TestCov_PickerPopup_CursorAndOther(t *testing.T) {
	m := newModel(&fakeSession{}, "/proj")
	m.pickerKind = "model"
	m.pickerItems = []pickerItem{{name: "alpha"}, {name: "beta"}}
	m.pickerCursor = 0
	out := m.pickerPopup()
	assert.Contains(t, out, "select model")
	assert.Contains(t, out, "▶") // cursor item rendered with the selection style
}

// ---- statusHeader ----

// TestCov_StatusHeader_InvalidThemeAndGit covers the invalid-theme fallback and
// the git-branch segment.
func TestCov_StatusHeader_InvalidThemeAndGit(t *testing.T) {
	m := newModel(&fakeSession{}, "/proj")
	m.width = 80
	m.theme = ThemeName("does-not-exist") // → themeList[0] fallback
	m.gitBranch = "main"
	out := m.statusHeader()
	assert.Contains(t, out, "main")
}

// ---- queuePreview ----

// TestCov_QueuePreview_NarrowWidth covers the limit<1 → 1 clamp.
func TestCov_QueuePreview_NarrowWidth(t *testing.T) {
	m := newModel(&fakeSession{}, "/proj")
	m.width = 4
	m.msgQueue = []string{"a message"}
	out := m.queuePreview()
	assert.Contains(t, out, "↳")
}

// ---- View (selecting path) ----

// TestCov_View_Selecting covers the m.selecting branch that re-renders the
// screen with the drag highlight.
func TestCov_View_Selecting(t *testing.T) {
	m := newModel(&fakeSession{}, "/proj")
	m.width = 80
	m.height = 24
	m.selecting = true
	m.selAnchorRow, m.selAnchorCol = 0, 0
	m.selLineRow, m.selLineCol = 0, 3
	out := m.View()
	assert.NotEmpty(t, out)
}

// ---- selectedText ----

// TestCov_SelectedText_SingleAndCrossRow covers the single-row slice and the
// cross-row head/middle/tail join.
func TestCov_SelectedText_SingleAndCrossRow(t *testing.T) {
	m := newModel(&fakeSession{}, "/proj")
	m.width = 80
	m.height = 24

	// Single row: a column slice of line 0.
	m.selAnchorRow, m.selAnchorCol = 0, 0
	m.selLineRow, m.selLineCol = 0, 3
	_ = m.selectedText()

	// Cross-row: head + middle + tail.
	m.selAnchorRow, m.selAnchorCol = 0, 0
	m.selLineRow, m.selLineCol = 2, 2
	_ = m.selectedText()
}

// ---- renderScreen (popup stacking) ----

// TestCov_RenderScreen_Popups covers the popup-append branches by stacking
// several overlays at once.
func TestCov_RenderScreen_Popups(t *testing.T) {
	m := newModel(&fakeSession{}, "/proj")
	m.width = 80
	m.height = 24
	m.helpRendered = "help-overlay"
	m.paletteItems = []command{{name: "clear"}} // paletteBlock
	m.msgQueue = []string{"queued message"}     // queuePreview
	m.pickerKind = "theme"                      // pickerPopup
	m.pickerItems = []pickerItem{{name: "dark"}}
	m.restoreSessions = []proto.SessionInfo{{ID: "s1"}}           // sessionPickerPopup
	m.action = &actionState{visible: true}                        // actionPopup
	m.yoloConfirm = 2                                             // yoloPopup
	m.pendingPermissions = []*permissionEntry{{tool: "fs_write"}} // permissionPopup
	m.historySearch = &historyState{visible: true, items: []historyItem{{Text: "x"}}}
	out := m.renderScreen()
	assert.Contains(t, out, "help-overlay")
}

// TestCov_Reflow_PaletteAndYolo covers the palette/yolo height branches in
// reflow (computed only when those popups are open).
func TestCov_Reflow_PaletteAndYolo(t *testing.T) {
	m := newModel(&fakeSession{}, "/proj")
	m.width = 80
	m.paletteItems = []command{{name: "clear"}} // paletteBlock non-empty
	m.yoloConfirm = 2                           // yoloPopup non-empty
	m.reflow()                                  // exercises paletteH + yoloH branches
}

// TestCov_RenderScreen_Toast covers the toast-overlay append branch.
func TestCov_RenderScreen_Toast(t *testing.T) {
	m := newModel(&fakeSession{}, "/proj")
	m.width = 80
	m.pushToast("info", "hello toast")
	out := m.renderScreen()
	assert.Contains(t, out, "hello toast")
}

// TestCov_RenderFooter_DimSeparator covers the "both segments on default bg →
// dim separator" branch (no Powerline arrow between two footerBg segments).
func TestCov_RenderFooter_DimSeparator(t *testing.T) {
	out := renderFooter([]segmentDef{
		{text: " a ", fg: "255", bg: "236"},
		{text: " b ", fg: "255", bg: "236"},
	}, 80)
	assert.Contains(t, out, "│", "two default-bg segments use the dim separator")
}
