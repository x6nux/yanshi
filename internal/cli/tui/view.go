package tui

import (
	"encoding/base64"
	"fmt"
	"hash/fnv"
	"io"
	"os"
	"regexp"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/x6nux/yanshi/internal/guard"
)

// sessionPickerPopup renders the interactive session restore picker as a popup
// above the input. Returns "" when the picker is closed.
func (m model) sessionPickerPopup() string {
	if len(m.restoreSessions) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("  " + toolName.Render("restore session") + "\n")
	// Determine visible range (up to 8 items, scroll with cursor).
	maxShow := 8
	start := 0
	if m.restoreCursor >= maxShow {
		start = m.restoreCursor - maxShow + 1
	}
	end := start + maxShow
	if end > len(m.restoreSessions) {
		end = len(m.restoreSessions)
	}
	scrollHint := ""
	if len(m.restoreSessions) > maxShow {
		scrollHint = toolMeta.Render(fmt.Sprintf(" (%d sessions, ↑↓ scroll)", len(m.restoreSessions)))
	}
	b.WriteString(scrollHint + "\n")
	for i := start; i < end; i++ {
		s := m.restoreSessions[i]
		title := s.Title
		if title == "" {
			title = "(untitled)"
		}
		created := time.Unix(s.CreatedAt, 0).Format("Jan 02 15:04")
		modelInfo := ""
		if s.Model != "" {
			modelInfo = " " + s.Model
		}
		line := fmt.Sprintf("  %-4s  %s\n    %s  %s  %d msgs%s",
			toolMeta.Render(s.ID), title,
			created, okStyle.Render("•"), s.MsgCount, modelInfo)
		if i == m.restoreCursor {
			b.WriteString(selPaletteStyle.Render("▶ " + line))
		} else {
			b.WriteString(paletteStyle.Render("  " + line))
		}
		b.WriteString("\n")
	}
	b.WriteString("  " + toolMeta.Render("↑↓ navigate · enter restore · esc cancel") + "\n")
	return b.String()
}

// pickerPopup renders the interactive command picker popup (for /model /mode
// /theme when no argument is given). Returns "" when the picker is closed.
func (m model) pickerPopup() string {
	if m.pickerKind == "" || len(m.pickerItems) == 0 {
		return ""
	}
	var title string
	switch m.pickerKind {
	case "model":
		title = "select model"
	case "mode":
		title = "select permission mode"
	case "theme":
		title = "select theme"
	}
	var b strings.Builder
	b.WriteString("  " + toolName.Render(title) + "\n")
	for i, item := range m.pickerItems {
		line := fmt.Sprintf("  %-18s", item.name)
		if item.description != "" {
			line += "  " + toolMeta.Render(item.description)
		}
		if item.current {
			line = "  " + okStyle.Render("●") + line[2:]
		}
		if i == m.pickerCursor {
			b.WriteString(selPaletteStyle.Render("▶ " + line))
		} else {
			b.WriteString(paletteStyle.Render("  " + line))
		}
		b.WriteString("\n")
	}
		b.WriteString("  " + toolMeta.Render("↑↓ jk navigate · enter select · esc cancel") + "\n")
	return b.String()
}

// growInput sizes the textarea height to its content: default 1 line, grows
// with explicit newlines up to 3, and the bubbles textarea scrolls its content
// internally once it exceeds the set height. Called after every input change.
func (m *model) growInput() {
	lines := strings.Count(m.input.Value(), "\n") + 1
	m.input.SetHeight(clamp(lines, 1, 3))
}

// reflow measures the rendered footer + (optional) status line + input blocks
// and gives the viewport EXACTLY the remaining terminal height, so the
// JoinVertical(viewport, [status,] input, footer) in View() always totals
// m.height with no overflow (which would clip the bottom of the transcript).
// The footer contributes 1 line (it now lives at the bottom); the activity
// status line contributes 1 line only while a turn is in flight.
func (m *model) reflow() {
	// Test-only hook: bump the counter when non-nil so the debounce test can
	// assert coalescing without instrumenting the real reflow path. Always nil
	// in production.
	if m.countReflow != nil {
		*m.countReflow++
	}
	// blockHeight counts the terminal lines a block actually occupies, folding
	// in wrapping (lipgloss.Height only counts explicit newlines, so a wide
	// footer/input that the terminal wraps to 2 lines would be undercounted →
	// the JoinVertical total would exceed the terminal height and clip the
	// bottom). Using the true wrapped height keeps the footer/input visible.
	footerH := blockHeight(m.statusHeader(), m.width)
	inputH := blockHeight(inputBorder.Render(m.input.View()), m.width)
	statusH := 0
	if m.streamCh != nil {
		statusH = blockHeight(m.statusLine(), m.width)
	}
	paletteH := 0
	if pb := m.paletteBlock(); pb != "" {
		paletteH = blockHeight(pb, m.width)
	}
	permH := 0
	if pp := m.permissionPopup(); pp != "" {
		permH = blockHeight(pp, m.width)
	}
	yoloH := 0
	if yp := m.yoloPopup(); yp != "" {
		yoloH = blockHeight(yp, m.width)
	}
		pickerH := 0
		if pp := m.sessionPickerPopup(); pp != "" {
			pickerH = blockHeight(pp, m.width)
		}
		cmdPickerH := 0
		if pp := m.pickerPopup(); pp != "" {
			cmdPickerH = blockHeight(pp, m.width)
		}
		queueH := 0
	if qp := m.queuePreview(); qp != "" {
		queueH = blockHeight(qp, m.width)
	}
		// C2 — UX7: toast block height. Toasts render between the viewport and
		// the status line (above all popups) so they overlay the transcript
		// without displacing input. The blockHeight is the wrapped height of
		// the rendered stack (one row per toast + the leading blank separator).
		toastH := 0
		if tb := m.toasts.render(m.width); tb != "" {
			toastH = blockHeight(tb, m.width)
		}
		// C2 — UX1: action palette popup height. The palette overlays the
		// viewport like toast/picker — when open, reserve its blockHeight so
		// the transcript viewport shrinks and the JoinVertical total stays
		// exactly terminal-height.
		actionH := 0
		if ap := m.actionPopup(); ap != "" {
			actionH = blockHeight(ap, m.width)
		}
		// C2 — UX2: F1 help panel. Modal popup; reserve its height when open.
		// NOTE: not calling helpPopup here because it transitively reads
		// commandTable, which creates an init cycle (commandTable → cmdModel →
		// sendControlFrame → reflow → helpPopup → renderHelp → commandTable).
		// We compute the height from the cached helpRendered string instead,
		// refreshed on each KeyMsg that mutates help state.
		helpH := 0
		if m.helpVisible && m.helpRendered != "" {
			helpH = blockHeight(m.helpRendered, m.width)
		}
		// C2 — UX6: history search popup (Alt+R). Modal; reserve height.
		historyH := 0
		if hp := m.historyPopup(); hp != "" {
			historyH = blockHeight(hp, m.width)
		}
		m.viewport.Width = m.width
		m.viewport.Height = max(3, m.height-footerH-inputH-statusH-paletteH-permH-yoloH-pickerH-cmdPickerH-queueH-toastH-actionH-helpH-historyH)
	m.refresh()
	// Re-clamp YOffset after a Height change. A GotoBottom from an event that
	// arrived BEFORE WindowSizeMsg (e.g. fetchInitialStatus) runs against the
	// pre-size minimum Height (3) and pins YOffset deep past short content; when
	// WindowSizeMsg then grows the viewport, that stale YOffset is never
	// corrected, so the startup banner's top — the logo — sits above the fold
	// until the user scrolls up. Clamping here fixes it generally: if the whole
	// transcript fits, pin to the top (banner fully visible); otherwise snap to
	// the new valid bottom so the latest content stays on screen.
	if n := m.viewport.TotalLineCount(); n <= m.viewport.Height {
		m.viewport.GotoTop()
	} else if m.viewport.YOffset > n-m.viewport.Height {
		m.viewport.SetYOffset(n - m.viewport.Height)
	}
}

// blockHeight returns the number of terminal lines a rendered block occupies at
// the given width, accounting for wrapping (each source line wider than width
// folds across multiple terminal lines). lipgloss.Height only counts explicit
// newlines, so it undercounts wrapped blocks; this is the wrapped-aware
// version used by reflow so the layout never overflows.
func blockHeight(s string, width int) int {
	if width <= 0 {
		return lipgloss.Height(s)
	}
	total := 0
	for _, line := range strings.Split(s, "\n") {
		w := lipgloss.Width(line)
		if w <= 0 {
			total++
			continue
		}
		total += (w + width - 1) / width
	}
	if total == 0 {
		return 1
	}
	return total
}

// statusLine renders the Claude-Code-style live activity line shown while a
// turn is in flight:
//
//	✢ Thinking… (0:12 · ↓ 2.0k)
//	✢ Running fs_search… (0:15 · ↓ 3.2k · ↻ retry 2/3 unexpected EOF)
//
// The leading glyph is selected by glyphFrame (animated via activityTickMsg).
// The token segment is omitted until the first status reply populates tokensIn.
// While the model call is being retried (a transient error / mid-stream EOF),
// a "↻ retry N/M <cause>" segment is appended so the user sees the recovery.
//
// The activity text is colorized by phase (colorizeActivity) so the user can
// tell at a glance what stage the turn is in: pink "Thinking…" mirrors the
// live thinkingEntry hue + the footer "think:" segment, while "Running
// <Tool>…" dims the verb and highlights the tool name in bright cyan to match
// the tool block's friendly header (the activity line then previews the
// block that will appear in the transcript). The retry segment is amber
// (warnStyle) so a transient failure reads as a warning, not a crash. The
// outer glyph + elapsed + tokens scaffolding stays in activityStyle.
func (m model) statusLine() string {
	glyph := activityGlyphs[m.glyphFrame%len(activityGlyphs)]
	out := fmt.Sprintf("%s %s (%s", glyph, colorizeActivity(m.activity), formatDuration(time.Since(m.turnStart)))
	if m.tokensIn > 0 {
		out += " · ↓ " + formatTokens(m.tokensIn)
	}
	if m.retryAttempt > 0 {
		// Amber (warnStyle) so a transient retry / mid-stream EOF reads as a
		// warning, not a silent or fatal-looking event — matches the perm-mode
		// and queue-mode amber used elsewhere in the footer.
		seg := fmt.Sprintf("↻ retry %d/%d", m.retryAttempt, m.retryMax)
		if m.retryErr != "" {
			seg += " " + m.retryErr
		}
		out += " · " + warnStyle.Render(seg)
	}
	out += ")"
	return activityStyle.Render(out)
}

// colorizeActivity renders the activity text with phase-appropriate colors:
//
//   - "Thinking…" → pink (footerThinkStyle 213) — same hue as the live
//     thinkingEntry header and the footer "think:" segment, so the live
//     activity line reads as part of the reasoning display.
//   - "Running <Tool>…" → dim grey "Running" verb (toolMeta 245) + bright
//     cyan tool name (toolName 123). The tool name passes through
//     toolDisplayName so a raw name (fs_read) renders as its friendly alias
//     (Read); this is idempotent when model.go has already applied the alias
//     upstream (the common case), and provides defense-in-depth when an
//     unknown/raw name slips through. The trailing "…" stays in the verb's
//     dim hue so the cursor indicator doesn't compete with the tool name.
//   - anything else (including "") → rendered unchanged inside activityStyle.
//
// The split exists (rather than coloring the whole line one hue) so the verb
// and the tool name are visually distinct: the verb is dim, the tool name
// POPS — matching the tool block's friendly header and giving the user a
// one-glance read of "we are inside Read, not Bash".
func colorizeActivity(activity string) string {
	if activity == "Thinking…" {
		return footerThinkStyle.Render(activity)
	}
	if rest, ok := strings.CutPrefix(activity, "Running "); ok {
		// Pull the trailing "…" off so only the tool name gets the bright
		// cyan; the ellipsis stays in the dim verb hue.
		name := strings.TrimSuffix(rest, "…")
		suffix := ""
		if name != rest {
			suffix = "…"
		}
		return toolMeta.Render("Running") + " " + toolName.Render(toolDisplayName(name)) + suffix
	}
	return activity
}

func (m *model) refresh() {
	m.viewport.SetContent(m.renderBody())
}

// renderScreen assembles the full visible screen (viewport + [status] + [palette]
// + input + footer) as a single string — the same composition View() renders,
// factored out so selection can operate on the whole screen (covering the
// footer too, not just the transcript).
func (m model) renderScreen() string {
	// C2 — UX7: toast stack overlays the TOP of the viewport (above the
	// transcript, below popups). Rendered first so it sits visually closest to
	// the conversation flow (turn-end receipts) rather than over the input.
	blocks := make([]string, 0, 12)
	if tb := m.toasts.render(m.width); tb != "" {
		blocks = append(blocks, tb)
	}
	blocks = append(blocks, m.viewport.View())
	if m.streamCh != nil {
		blocks = append(blocks, m.statusLine())
	}
	if yp := m.yoloPopup(); yp != "" {
		blocks = append(blocks, yp)
	}
	if pp := m.permissionPopup(); pp != "" {
		blocks = append(blocks, pp)
	}
	if sp := m.sessionPickerPopup(); sp != "" {
			blocks = append(blocks, sp)
		}
		if cp := m.pickerPopup(); cp != "" {
			blocks = append(blocks, cp)
		}
		// C2 — UX1: action palette (Ctrl+K). Rendered alongside other popups
		// so it shares the same JoinVertical layout pipeline.
		if ap := m.actionPopup(); ap != "" {
			blocks = append(blocks, ap)
		}
		// C2 — UX2: F1 help panel. Rendered alongside other popups.
		if hp := m.helpRendered; hp != "" {
			blocks = append(blocks, hp)
		}
		// C2 — UX6: history search popup (Alt+R). Rendered alongside other popups.
		if hp := m.historyPopup(); hp != "" {
			blocks = append(blocks, hp)
		}
		if pb := m.paletteBlock(); pb != "" {
		blocks = append(blocks, pb)
	}
	if qp := m.queuePreview(); qp != "" {
		blocks = append(blocks, qp)
	}
	blocks = append(blocks, inputBorder.Render(m.input.View()))
	blocks = append(blocks, m.statusHeader())
	return lipgloss.JoinVertical(lipgloss.Left, blocks...)
}

// handleSelectMouse drives in-app text selection from a left-button mouse
// event across the WHOLE screen (any row/col). A press starts a selection
// anchored at the screen {row, col} under the cursor; motion extends the active
// end and auto-scrolls at the viewport edges so the range can grow into
// off-screen content; release copies the selected character range to the system
// clipboard (OSC 52) and clears the selection (T10: char-level, was whole-line).
func (m *model) handleSelectMouse(msg tea.MouseMsg) tea.Cmd {
	row, col := msg.Y, msg.X // screen coords (0,0 = top-left)
	switch msg.Action {
	case tea.MouseActionPress:
		m.selecting = true
		m.selAnchorRow, m.selAnchorCol = row, col
		m.selLineRow, m.selLineCol = row, col
	case tea.MouseActionMotion:
		if m.selecting {
			m.selLineRow, m.selLineCol = row, col
			m.edgeAutoScroll()
		}
	case tea.MouseActionRelease:
		if m.selecting {
			m.selecting = false
			text := m.selectedText()
			if text != "" {
				return copyClipboard(text)
			}
		}
	}
	return nil
}

// edgeAutoScroll scrolls the viewport by one line when the active drag end sits
// on the top or bottom visible viewport line, so holding the cursor at the edge
// keeps extending the selection into content that was off-screen. The viewport
// is rendered at the top of the screen, so its on-screen row span is
// [0, viewport.Height-1]; row 0 is the top edge, row Height-1 the bottom. Rows
// in the middle (or below the viewport, over the input/footer) do nothing.
func (m *model) edgeAutoScroll() {
	switch {
	case m.selLineRow <= 0:
		m.viewport.LineUp(1)
	case m.selLineRow == m.viewport.Height-1:
		m.viewport.LineDown(1)
	}
}

// selPos is a {screen row, screen column} selection endpoint. It is the 2D
// successor to the pre-T10 whole-line row index: carrying the column is what
// makes a selection a character range rather than an integral set of lines.
type selPos struct{ row, col int }

// selLess reports whether a precedes b in reading order (an earlier row, or the
// same row and an earlier column). Used to normalize a drag's two endpoints so
// the selection logic can assume lo <= hi regardless of drag direction.
func selLess(a, b selPos) bool {
	if a.row != b.row {
		return a.row < b.row
	}
	return a.col < b.col
}

// selRange returns the current selection endpoints normalized to document order
// (lo <= hi), as 2D {row, col} positions. The anchor is where the press began
// and the line end follows the cursor; either can be earlier in the document.
func (m model) selRange() (selPos, selPos) {
	a := selPos{m.selAnchorRow, m.selAnchorCol}
	b := selPos{m.selLineRow, m.selLineCol}
	if selLess(a, b) {
		return a, b
	}
	return b, a
}

// cellWidth returns the display width of a single rune in terminal cells,
// matching what the terminal (and lipgloss) actually render. Selection column
// math MUST agree with the terminal: the in-app mouse selection carries a
// screen column (MouseMsg.X) that the terminal measured, and colToByteOffset
// maps that column back to a byte offset. Routing through runewidth.RuneWidth
// instead made the two disagree on East-Asian Ambiguous runes ('·' U+00B7,
// '│' U+2502, …) and the Powerline private-use glyphs (U+E0B0…): under a CJK
// locale mattn/go-runewidth's IsEastAsian()==true widths those as 2 cells,
// while lipgloss (charmbracelet/x/ansi's table) — and Cascadia Code / every
// Powerline font — renders each as a single cell. Each such rune on the line
// shifted the mapped byte offset one column past the cursor; the input border
// (│), its placeholder (·) and the Powerline footer (U+E0B0 / │) are full of
// them, so a bottom-area drag could land several characters off the cursor.
// Going through lipgloss.Width keeps colToByteOffset aligned with both the
// highlight re-render (highlightLines measures line width with lipgloss.Width)
// and the terminal itself.
func cellWidth(r rune) int {
	return lipgloss.Width(string(r))
}

// colToByteOffset maps a screen column to the byte offset it corresponds to in
// line (an ANSI-stripped plain-text string). ASCII is 1:1 (one column per
// byte/rune); a wide rune (East Asian, emoji — 2 cells via cellWidth) consumes 2
// columns, so a column that lands in the middle of such a rune snaps past it to
// keep the returned offset on a rune boundary (a mid-rune slice would produce
// invalid UTF-8). Columns past the end clamp to len(line); non-positive columns
// clamp to 0.
func colToByteOffset(line string, col int) int {
	if col <= 0 {
		return 0
	}
	consumed := 0
	for i, r := range line {
		if consumed >= col {
			return i
		}
		consumed += cellWidth(r)
	}
	return len(line)
}

// selectedText returns the ANSI-stripped text spanning the current selection's
// CHARACTER range (T10 — previously the whole lines were joined). On a single
// row only the [lo.col, hi.col) slice of that line is taken; across rows the
// partial head line (lo.col → end), the full middle lines, and the partial tail
// line (start → hi.col) are joined with newlines. Columns are mapped to byte
// offsets via colToByteOffset (ASCII 1:1, wide-rune aligned). Computed from the
// rendered screen so it covers the footer/input too, not just the transcript.
func (m model) selectedText() string {
	lines := splitStripANSI(m.renderScreen())
	if len(lines) == 0 {
		return ""
	}
	lo, hi := m.selRange()
	lo.row = clamp(lo.row, 0, len(lines)-1)
	hi.row = clamp(hi.row, 0, len(lines)-1)
	if lo.row > hi.row {
		return ""
	}
	if lo.row == hi.row {
		s := lines[lo.row]
		start := colToByteOffset(s, lo.col)
		end := colToByteOffset(s, hi.col)
		if end < start {
			start, end = end, start
		}
		return s[start:end]
	}
	// Cross-row: partial head + full middle lines + partial tail.
	var b strings.Builder
	first := lines[lo.row]
	b.WriteString(first[colToByteOffset(first, lo.col):])
	for r := lo.row + 1; r < hi.row; r++ {
		b.WriteByte('\n')
		b.WriteString(lines[r])
	}
	last := lines[hi.row]
	b.WriteByte('\n')
	b.WriteString(last[:colToByteOffset(last, hi.col)])
	return b.String()
}

// highlightLines re-renders screen with the [a,b] line range given a solid
// highlight background (each selected line is de-styled first so the highlight
// highlightLines renders the drag selection as a char-level highlight: within
// each touched row, only the [lo.col, hi.col) screen-column span gets selStyle;
// the rest of the row stays as plain text. lo/hi are 2D (row,col), already
// normalized by selRange (lo <= hi). Wide runes are column-aligned via
// colToByteOffset so the span never splits a multi-byte rune. (Matches the
// char-precise text that selectedText copies on release.)
func highlightLines(screen string, lo, hi selPos) string {
	lines := strings.Split(screen, "\n")
	for i := lo.row; i <= hi.row && i < len(lines); i++ {
		if i < 0 {
			continue
		}
		plain := stripANSI(lines[i])
		width := lipgloss.Width(plain) // display width = end-of-line column
		var startCol, endCol int
		switch {
		case i == lo.row && i == hi.row: // single-row span
			startCol, endCol = lo.col, hi.col
		case i == lo.row: // first row of multi-row: lo.col → end of line
			startCol, endCol = lo.col, width
		case i == hi.row: // last row of multi-row: start of line → hi.col
			startCol, endCol = 0, hi.col
		default: // middle row: whole line
			startCol, endCol = 0, width
		}
		if startCol < 0 {
			startCol = 0
		}
		if endCol > width {
			endCol = width
		}
		if startCol >= endCol {
			continue // nothing to highlight on this row
		}
		sb := colToByteOffset(plain, startCol)
		eb := colToByteOffset(plain, endCol)
		lines[i] = plain[:sb] + selStyle.Render(plain[sb:eb]) + plain[eb:]
	}
	return strings.Join(lines, "\n")
}

// copyClipboard returns a Cmd that writes the OSC 52 clipboard sequence to the
// terminal, setting the system clipboard to text. OSC 52 carries no screen
// state, so emitting it from the Cmd goroutine (concurrent with frame redraws)
// is safe: the terminal processes it as a control sequence regardless of where
// it lands in the byte stream. Supported by Windows Terminal, iTerm2, kitty,
// alacritty, gnome-terminal.
func copyClipboard(text string) tea.Cmd {
	seq := "\x1b]52;c;" + base64.StdEncoding.EncodeToString([]byte(text)) + "\x07"
	return func() tea.Msg {
		_, _ = os.Stdout.WriteString(seq)
		return nil
	}
}

// ansiRe matches CSI (SGR cursor/color) and OSC escape sequences so they can be
// stripped from styled text, yielding plain text for the selection clipboard and
// the highlight re-render.
var ansiRe = regexp.MustCompile("\x1b\\[[0-9;]*[A-Za-z]|\x1b\\][^\x07\x1b]*(\x07|\x1b\\\\)")

func stripANSI(s string) string { return ansiRe.ReplaceAllString(s, "") }

// splitStripANSI splits body into lines and ANSI-strips each, returning the
// plain-text line index of the rendered screen. Trailing spaces are trimmed per
// line so copied markdown (which glamour pads to the wrap width) doesn't drag
// along a train of trailing blanks.
func splitStripANSI(body string) []string {
	lines := strings.Split(body, "\n")
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = strings.TrimRight(stripANSI(l), " \t")
	}
	return out
}

func (m model) View() string {
	// The full screen (viewport + [status] + [palette] + input + footer) is
	// assembled by renderScreen. While an in-app mouse selection is in progress,
	// the dragged screen-line range is highlighted (this covers the whole
	// screen, including the footer — not just the transcript).
	screen := m.renderScreen()
	if m.selecting {
		// Char-level highlight: within the dragged row span, only the
		// [lo.col, hi.col) column range is highlighted (matches the
		// char-precise text copied on release).
		lo, hi := m.selRange()
		screen = highlightLines(screen, lo, hi)
	}
	return screen
}

// segmentDef defines a Powerline footer segment with background/foreground color
// codes, used by renderFooter to build the status bar.
type segmentDef struct {
	text string
	fg   string // ANSI 256-color foreground code e.g. "255"
	bg   string // ANSI 256-color background code e.g. "17"
	bold bool
}

// renderFooter assembles a Powerline-style footer bar from segments. Each
// segment has its own background color; adjacent segments are joined by a
// Powerline arrow (U+E0B0) whose foreground transitions from the previous
// segment's background to the current segment's background — the signature
// colour-flow effect from tools like airline / CCometixLine.
//
// When an adjacent pair BOTH use the default footer background ("236") no
// Powerline arrow is rendered: a simple dim "│" separator is used instead,
// so the muted theme (which places every segment on the default bg) still
// has legible spacing without coloured pills.
//
// The bar is right-filled to width with the default footer background so it
// spans the entire terminal width edge-to-edge. When width < 4 (uninitialised)
// no right-fill is applied.
func renderFooter(segs []segmentDef, width int) string {
	const footerBg = "236"
	var b strings.Builder

	// Left padding: space on the default footer background.
	b.WriteString("\x1b[48;5;" + footerBg + "m \x1b[0m")

	for i, s := range segs {
		if i > 0 {
			prevBg := segs[i-1].bg
			if s.bg == footerBg && prevBg == footerBg {
				// Both on default bg → simple dim separator, no colour transition.
				b.WriteString("\x1b[38;5;240;48;5;" + footerBg + "m │ \x1b[0m")
			} else {
				// Powerline arrow: foreground = previous bg, background = this bg.
				b.WriteString("\x1b[38;5;" + prevBg + ";48;5;" + s.bg + "m\U0000E0B0\x1b[0m")
			}
		}
		// Segment body with its own background and foreground.
		if s.bold {
			b.WriteString("\x1b[1;38;5;" + s.fg + ";48;5;" + s.bg + "m" + s.text + "\x1b[0m")
		} else {
			b.WriteString("\x1b[38;5;" + s.fg + ";48;5;" + s.bg + "m" + s.text + "\x1b[0m")
		}
	}

	// Right-fill: extend the default footer background to the terminal width
	// so the bar spans the entire screen edge-to-edge.
	if width > 0 {
		plain := ansiRe.ReplaceAllString(b.String(), "")
		visWidth := lipgloss.Width(plain)
		if remaining := width - visWidth; remaining > 0 {
			b.WriteString("\x1b[48;5;" + footerBg + "m" + strings.Repeat(" ", remaining) + "\x1b[0m")
		}
	}
	return b.String()
}

// statusHeader renders the persistent bottom status bar (footer) as a
// themed segmented bar. Each information group gets its own background colour
// ("pill") from the active theme (m.theme); segments are joined by Powerline
// arrows (U+E0B0) with colour transitions for the default/high-contrast themes,
// or by simple "│" separators for the muted theme. The theme is switched at
// runtime via /theme <name>.
func (m model) statusHeader() string {
	// Resolve active theme; fall back to default on typo.
	t, ok := themeByName(m.theme)
	if !ok {
		t = themeList[0]
	}
	tc := func(key string) segmentColors {
		if c, ok2 := t.Colors[key]; ok2 {
			return c
		}
		return segmentColors{fg: "255", bg: "236", bold: false}
	}
		var segs []segmentDef
		var c segmentColors

		// 1. Working directory.
		if m.workDir != "" {
		c = tc("dir")
		segs = append(segs, segmentDef{text: " " + m.workDir + " ", fg: c.fg, bg: c.bg, bold: c.bold})
	}

	// 4. Git branch.
	if m.gitBranch != "" {
		c = tc("git")
		segs = append(segs, segmentDef{text: " " + m.gitBranch + " ", fg: c.fg, bg: c.bg, bold: c.bold})
	}

	// 5. Model name.
	if m.modelName != "" {
		c = tc("model")
		segs = append(segs, segmentDef{text: " " + m.modelName + " ", fg: c.fg, bg: c.bg, bold: c.bold})
	}

	// 6. Context usage "percentage · tokens".
	if m.tokensIn > 0 || m.contextWindow > 0 {
		c = tc("ctx")
		var ctxText string
		if m.contextWindow > 0 {
			pct := float64(m.tokensIn) / float64(m.contextWindow) * 100
			ctxText = fmt.Sprintf(" %.1f%%  %s tokens ", pct, formatTokens(m.tokensIn))
		} else {
			ctxText = " " + formatTokens(m.tokensIn) + " tokens "
		}
		segs = append(segs, segmentDef{
			text: ctxText,
			fg:   c.fg, bg: c.bg, bold: c.bold,
		})
	}

	// 7. Total consumption "总消耗:N" — the unified token tally (in + out),
	// replacing the former separate think/cache breakdown. Cached/reasoning are
	// subsets of in/out, so their sum double-counts; a single gross total is the
	// honest "what did this session burn" figure. (Per-session history +
	// histogram live behind /stats.)
	if m.tokensIn > 0 || m.tokensOut > 0 {
		c = tc("total")
		segs = append(segs, segmentDef{
			text: " 总消耗 " + formatTokens(m.tokensIn+m.tokensOut) + " ",
			fg:   c.fg, bg: c.bg, bold: c.bold,
		})
	}

	// 9. Permission mode — colour varies by mode severity.
	if pmText := m.permModeText(); pmText != "" {
		permKey := "perm_default"
		switch m.permMode {
		case guard.ModeAllowEdits:
			permKey = "perm_edits"
		case guard.ModeAuto:
			permKey = "perm_auto"
		case guard.ModeYOLO:
			permKey = "perm_yolo"
		}
		c = tc(permKey)
		segs = append(segs, segmentDef{
			text: " " + pmText + " ", fg: c.fg, bg: c.bg, bold: c.bold,
		})
	}

	// 10. Tool call count.
	if m.toolsRun > 0 {
		c = tc("tools")
		segs = append(segs, segmentDef{
			text: fmt.Sprintf(" %d tool calls ", m.toolsRun),
			fg:   c.fg, bg: c.bg, bold: c.bold,
		})
	}

	// 11. Queue mode.
	if len(m.msgQueue) > 0 || m.queueMode != QueueModeQueue {
		c = tc("queue")
		q := "queue:" + m.queueMode.String()
		if n := len(m.msgQueue); n > 0 {
			q += fmt.Sprintf("·%d", n)
		}
		segs = append(segs, segmentDef{text: " " + q + " ", fg: c.fg, bg: c.bg, bold: c.bold})
	}

	// 12. MEM1 memory file path. Use filepath.Base so a long path stays
	// compact in the footer. Passing an empty root intentionally makes the
	// existing 2-arg shortenPath fall back to the basename for absolute paths.
	// Disabled (empty) → omitted.
	if m.memoryPath != "" {
		c = tc("ctx") // reuse ctx colour pill; swap for a dedicated "mem" key if desired
		segs = append(segs, segmentDef{
			text: " mem:" + shortenPath(m.memoryPath, "") + " ",
			fg:   c.fg, bg: c.bg, bold: c.bold,
		})
	}

	// 13. V11 side depth. Warn-coloured pill so it stands out while inside
	// an ephemeral conversation.
	if m.sideDepth > 0 {
		c = tc("perm_yolo") // eye-catching
		segs = append(segs, segmentDef{
			text: fmt.Sprintf(" in side (%d) ", m.sideDepth),
			fg:   c.fg, bg: c.bg, bold: c.bold,
		})
	}

	return renderFooter(segs, m.width)
}

// formatTokens renders a token count compactly for the header: 0 → "0", 100 →
// "100", 1500 → "1.5k", 2000 → "2k", 128000 → "128k". Used for the
// "ctx: <in>/<window>" indicator.
func formatTokens(n int) string {
	if n < 1000 {
		return fmt.Sprintf("%d", n)
	}
	if n%1000 == 0 {
		return fmt.Sprintf("%dk", n/1000)
	}
	return fmt.Sprintf("%.1fk", float64(n)/1000)
}

func (m model) renderBody() string {
	var b strings.Builder
	// While assistant text streams (pending), if the last committed entry is a
	// finalized thinking block, fold it into the streaming assistant block so it
	// renders between the "assistant:" label and the content (matching the
	// flushed assistantEntry layout) — no jump when the text finalizes.
	entries := m.entries
	var streamingThought entry
	if len(m.pending) > 0 {
		// Fold the whole trailing run of finalized thinking blocks (not just
		// entries[-1]) so interleaved reasoning phases render as one Thought under
		// the streaming assistant label — matches flushAssistant's attach. See
		// detachTrailingThoughts.
		var thought *thinkingEntry
		entries, thought = detachTrailingThoughts(entries)
		if thought != nil {
			streamingThought = thought
		}
	}
	for _, e := range entries {
		// assistantEntry is by far the most expensive entry to render (full
		// glamour markdown pass over a long answer), so memoize it by content
		// fingerprint. Other entries are cheap (style-only, no markdown) and
		// skip the cache to keep the working set small.
		if ae, ok := e.(assistantEntry); ok {
			key := assistantRenderKey(ae, m.width)
			b.WriteString(cachedEntryRender(key, func() string {
				return ae.render(m.width, m.spinner)
			}))
			continue
		}
		b.WriteString(e.render(m.width, m.spinner))
	}
	if len(m.pending) > 0 {
		b.WriteString(roleAsst.Render("assistant:"))
		if streamingThought != nil {
			b.WriteString("\n" + strings.TrimRight(streamingThought.render(m.width, m.spinner), "\n"))
		}
		// Streaming pending text is rendered PLAIN (no markdown) so the UI can
		// keep up with high-frequency agent_chunk deltas — glamour per chunk is
		// the dominant CPU cost of streaming. On flushAssistant the text moves
		// into an assistantEntry and is markdown-rendered exactly once (cached).
		b.WriteString("\n" + strings.TrimRight(pendingStyle.Render(m.pending), "\n") + "\n")
	}
	return b.String()
}

// assistantRenderKey returns the fingerprint used to memoize an assistantEntry's
// rendered output: text + width + (when present) the attached thinkingEntry's
// state. The thought pointer is mutable — ctrl+o flips its expanded flag in
// place through the value-copied model — so the key includes expanded + text +
// endedAt; otherwise toggling expand would return the stale pre-toggle render.
// startedAt is folded into endedAt (the rendered "Thought for Xs" duration is
// endedAt-startedAt, so endedAt alone disambiguates each toggle/flush).
func assistantRenderKey(ae assistantEntry, width int) uint64 {
	h := fnv.New64a()
	_, _ = io.WriteString(h, ae.text)
	_, _ = fmt.Fprintf(h, "|w=%d|", width)
	if ae.thought != nil {
		_, _ = fmt.Fprintf(h, "t=%q|exp=%v|ed=%d|",
			ae.thought.text, ae.thought.expanded, ae.thought.endedAt.UnixNano())
	}
	return h.Sum64()
}

// queuePreview renders the queued-messages block above the input composer (C07),
// codex-PendingInputPreview-style: each queued message on its own line as
// "  ↳ <msg>" in dim italic, so the backlog is visible as pending context
// without polluting the transcript (where it used to sit as a 📨 marker under
// the user's last message). Returns "" when the queue is empty, so the block is
// omitted from renderScreen and costs no height in reflow. Each message is
// flattened to one line and truncated to the width so a chatty queued paste
// cannot blow out the block.
func (m model) queuePreview() string {
	if len(m.msgQueue) == 0 {
		return ""
	}
	// Width defaults to 80 when unset (tests, pre-reflow) so the preview is
	// still renderable; truncate then guards each line against narrow terminals.
	width := m.width
	if width < 4 {
		width = 80
	}
	limit := width - 4
	if limit < 1 {
		limit = 1
	}
	var b strings.Builder
	for _, msg := range m.msgQueue {
		oneLine := strings.ReplaceAll(strings.TrimSpace(msg), "\n", " ")
		b.WriteString(queuePreviewStyle.Render("  ↳ "+truncate(oneLine, limit)) + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}
