package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"github.com/x6nux/yanshi/internal/cli"
	"github.com/x6nux/yanshi/internal/guard"
)

// newTestModel returns a fresh model wired with fakeSession — the standard
// pattern used across the suite (see model_test.go's newModel calls).
// Centralizing it here keeps the statusLine tests below terse. turnStart is
// stamped to "now" so the elapsed-time segment renders a sane value.
func newTestModel(t *testing.T) model {
	t.Helper()
	m := newModel(&fakeSession{}, "/proj")
	m.turnStart = time.Now()
	return m
}

// ---- B7: statusLine semantic coloring (Thinking pink, Running verb+tool, retry amber) ----

// TestStatusLineThinkingColored proves "Thinking…" renders with the activity
// text intact. The thinking phase is colored pink (footerThinkStyle 213) to
// match the live thinkingEntry hue and the "think:" footer segment, so the
// live activity line reads as part of the reasoning display.
func TestStatusLineThinkingColored(t *testing.T) {
	m := newTestModel(t)
	m.activity = "Thinking…"
	m.streamCh = make(chan cli.StreamEvent)
	out := m.statusLine()
	if !strings.Contains(stripANSI(out), "Thinking") {
		t.Fatalf("应含 Thinking:\n%s", out)
	}
}

// TestStatusLineRunningShowsToolName proves a "Running <tool>…" activity line
// keeps both the verb and the tool name visible. The verb is dimmed (toolMeta
// 245) and the tool name is highlighted in bright cyan (toolName 123) matching
// the tool block's friendly header — so the activity line previews the block
// that will appear in the transcript. toolDisplayName is applied so even a
// hypothetical raw tool name (fs_read) is shown as its friendly alias (Read).
func TestStatusLineRunningShowsToolName(t *testing.T) {
	m := newTestModel(t)
	m.activity = "Running fs_read…"
	m.streamCh = make(chan cli.StreamEvent)
	out := m.statusLine()
	plain := stripANSI(out)
	if !strings.Contains(plain, "Running") {
		t.Fatalf("应含 Running verb:\n%s", out)
	}
	if !strings.Contains(plain, "Read") {
		t.Fatalf("Running+fs_read 应经 toolDisplayName→Read:\n%s", out)
	}
}

// TestStatusLineRetryUsesWarn proves the retry segment ("↻ retry N/M <err>")
// is rendered (and in the amber warnStyle hue so a transient failure / mid-
// stream EOF reads as a warning rather than a crash). The plain text must
// still carry "retry N/M" so the recovery is legible.
func TestStatusLineRetryUsesWarn(t *testing.T) {
	m := newTestModel(t)
	m.activity = "Thinking…"
	m.retryAttempt = 2
	m.retryMax = 3
	m.retryErr = "unexpected EOF"
	m.streamCh = make(chan cli.StreamEvent)
	out := m.statusLine()
	if !strings.Contains(stripANSI(out), "retry 2/3") {
		t.Fatalf("应显 retry 2/3:\n%s", out)
	}
	if !strings.Contains(stripANSI(out), "unexpected EOF") {
		t.Fatalf("应显 retry 原因:\n%s", out)
	}
}

// TestStatusLineGlyphAndElapsedPreserved proves the B7 colorization does not
// disturb the leading animated glyph or the (mm:ss) elapsed segment — only
// the activity text should be colorized; the outer scaffolding stays in the
// activityStyle hue.
func TestStatusLineGlyphAndElapsedPreserved(t *testing.T) {
	m := newTestModel(t)
	m.activity = "Running fs_read…"
	m.streamCh = make(chan cli.StreamEvent)
	out := m.statusLine()
	plain := stripANSI(out)
	if !strings.Contains(plain, "✢") && !strings.Contains(plain, "✣") &&
		!strings.Contains(plain, "✤") && !strings.Contains(plain, "✥") {
		t.Fatalf("应含活动 glyph:\n%s", out)
	}
	if !strings.Contains(plain, "(0:") {
		t.Fatalf("应含 elapsed (mm:ss) 段:\n%s", out)
	}
}

// ---- footer total consumption segment ----

// TestFooterShowsTotalConsumption proves the footer surfaces a single unified
// "总消耗" segment (tokensIn + tokensOut) in place of the former separate
// think/cache breakdown. cachedTokens/reasoningTokens are subsets of in/out, so
// they no longer get their own segments — the gross total is the honest tally.
func TestFooterShowsTotalConsumption(t *testing.T) {
	m := newTestModel(t)
	m.modelName = "x"
	m.tokensIn = 12000
	m.tokensOut = 1500
	m.contextWindow = 128000
	m.reasoningTokens = 1200
	m.cachedTokens = 8000
	out := m.statusHeader()
	plain := stripANSI(out)
	if !strings.Contains(plain, "总消耗") {
		t.Fatalf("footer 应含 总消耗 段:\n%s", out)
	}
	// Total = tokensIn + tokensOut = 12000 + 1500 = 13500 → "13.5k".
	if !strings.Contains(plain, "13.5k") {
		t.Fatalf("footer 应显 总消耗 13.5k (in+out):\n%s", out)
	}
	// The old breakdown segments must NOT appear anymore.
	if strings.Contains(plain, "think:") {
		t.Fatalf("footer 不应再显 think 段:\n%s", out)
	}
	if strings.Contains(plain, "cache:") {
		t.Fatalf("footer 不应再显 cache 段:\n%s", out)
	}
}

// TestApplyStatusFillsUsage proves applyStatus routes CachedTokens and
// ReasoningTokens from a status reply into the model fields that drive the
// footer think/cache segments and the /cost breakdown. Without this wiring
// the new fields stay zero and the segments never render.
func TestApplyStatusFillsUsage(t *testing.T) {
	m := newTestModel(t)
	m.applyStatus(cli.StreamEvent{
		Kind:            "status",
		Model:           "m",
		CachedTokens:    40,
		ReasoningTokens: 8,
	})
	if m.cachedTokens != 40 {
		t.Fatalf("applyStatus 应填充 cachedTokens=40，实际 %d", m.cachedTokens)
	}
	if m.reasoningTokens != 8 {
		t.Fatalf("applyStatus 应填充 reasoningTokens=8，实际 %d", m.reasoningTokens)
	}
}

// TestApplyStatusFillsPendingStatusEntry proves applyStatus also fills the
// pendingStatus block (/cost, /config) with cached/reasoning tokens — so the
// statusEntry rendered in the transcript carries the same breakdown as the
// footer, not just the raw in/out totals. Without the pendingStatus wiring
// the /cost block would render the legacy "N in · M out" shape even after
// A6 made the numbers available.
func TestApplyStatusFillsPendingStatusEntry(t *testing.T) {
	m := newTestModel(t)
	se := &statusEntry{}
	m.pendingStatus = se
	m.applyStatus(cli.StreamEvent{
		Kind: "status", Model: "m", TokensIn: 10, TokensOut: 5,
		CachedTokens: 40, ReasoningTokens: 8,
	})
	if se.cachedTokens != 40 {
		t.Fatalf("pendingStatus.cachedTokens 应为 40，实际 %d", se.cachedTokens)
	}
	if se.reasoningTokens != 8 {
		t.Fatalf("pendingStatus.reasoningTokens 应为 8，实际 %d", se.reasoningTokens)
	}
}

// TestStatusEntryRendersBreakdown proves /cost surfaces cache hits and
// reasoning tokens inline with the in/out totals (Task B8 / T8b). The legacy
// "N in · M out" shape is preserved when the new fields are zero (older turns
// without A6 wiring); the cache/think annotations only appear when non-zero.
func TestStatusEntryRendersBreakdown(t *testing.T) {
	// Legacy shape (no cached/reasoning): renders as before. Note "thinking:"
	// is the existing field label and must not be confused with the " · think N"
	// annotation that only appears when reasoningTokens > 0.
	legacy := &statusEntry{tokensIn: 10, tokensOut: 5}
	legacyOut := stripANSI(legacy.render(80, newTestModel(t).spinner))
	if !strings.Contains(legacyOut, "tokens: 10 in · 5 out") {
		t.Fatalf("legacy shape 应保持 \"tokens: 10 in · 5 out\":\n%s", legacyOut)
	}
	if strings.Contains(legacyOut, "(cache ") || strings.Contains(legacyOut, " · think ") {
		t.Fatalf("legacy shape 不应显 cache/think 注解:\n%s", legacyOut)
	}

	// Full breakdown: cache hit + reasoning tokens annotated inline.
	full := &statusEntry{tokensIn: 12000, tokensOut: 1500, cachedTokens: 8000, reasoningTokens: 1200}
	fullOut := stripANSI(full.render(80, newTestModel(t).spinner))
	if !strings.Contains(fullOut, "tokens: 12000 in (cache 8000) · 1500 out · think 1200") {
		t.Fatalf("完整明细行格式错误:\n%s", fullOut)
	}
}

// ---- B10 / T10: char-level selection + edge auto-scroll ----

// TestSelectedTextCharRange proves a selection is no longer whole-line: on a
// single screen row, only the [anchorCol, lineCol) character slice is copied.
// With the pre-T10 model (whole-line join) this would return the entire line,
// making it impossible to copy just a fragment of a line.
func TestSelectedTextCharRange(t *testing.T) {
	m := newTestModel(t)
	m.viewport.SetContent("abcde\nfghij")
	lines := splitStripANSI(m.renderScreen())
	row0 := lines[0]
	if row0 != "abcde" {
		t.Fatalf("renderScreen 第 0 行预期 abcde，实际 %q（测试假设需调整）", row0)
	}
	// Cols [1,3) on row 0 → "bc", not "abcde".
	m.selAnchorRow, m.selAnchorCol = 0, 1
	m.selLineRow, m.selLineCol = 0, 3
	got := m.selectedText()
	if got != "bc" {
		t.Fatalf("同行字符区间 [1,3)：期望 %q，实际 %q", "bc", got)
	}
}

// TestSelectedTextCrossRowCharRange proves a multi-row selection takes the
// partial head line (anchorCol → end), the full middle lines, and the partial
// tail line (start → lineCol), joined with newlines — so a drag across rows
// still copies only the char range, not the whole head/tail lines.
func TestSelectedTextCrossRowCharRange(t *testing.T) {
	m := newTestModel(t)
	m.viewport.SetContent("abcde\nfghij\nklmno")
	m.selAnchorRow, m.selAnchorCol = 0, 2 // start mid row 0
	m.selLineRow, m.selLineCol = 2, 3     // end mid row 2
	got := m.selectedText()
	want := "cde\nfghij\nklm"
	if got != want {
		t.Fatalf("跨行字符区间：期望 %q，实际 %q", want, got)
	}
}

// TestColToByteOffset proves the screen-column → byte-offset map is 1:1 for
// ASCII, clamps past-end columns to len(line), and snaps a 2-cell wide rune to
// a rune boundary (so the slice never splits a multi-byte rune).
func TestColToByteOffset(t *testing.T) {
	if got := colToByteOffset("abcde", 0); got != 0 {
		t.Fatalf("ASCII col0 → 期望 byte 0，实际 %d", got)
	}
	if got := colToByteOffset("abcde", 3); got != 3 {
		t.Fatalf("ASCII col3 → 期望 byte 3，实际 %d", got)
	}
	if got := colToByteOffset("abcde", 99); got != 5 {
		t.Fatalf("col99 → 期望 byte 5（clamp 到 len），实际 %d", got)
	}
	// "a中b": a=col0(byte0), 中=col1-2(byte1-3), b=col3(byte4). col3 → byte 4.
	if got := colToByteOffset("a中b", 3); got != 4 {
		t.Fatalf("wide rune col3 → 期望 byte 4，实际 %d", got)
	}
}

// TestEdgeAutoScroll proves dragging to the top or bottom visible viewport line
// scrolls the viewport by one line, so a selection can extend into off-screen
// content by holding the cursor at the edge. The viewport sits at screen row 0,
// so its on-screen edges are row 0 (top) and row Height-1 (bottom); a middle
// row must not trigger any scroll.
func TestEdgeAutoScroll(t *testing.T) {
	m := newTestModel(t)
	// 20 content lines, viewport height 10 → scrollable in both directions.
	var b strings.Builder
	for i := 0; i < 20; i++ {
		if i > 0 {
			b.WriteByte('\n')
		}
		fmt.Fprintf(&b, "line%02d", i)
	}
	m.viewport.SetContent(b.String())
	m.viewport.SetYOffset(5) // middle: both edges can move
	m.selecting = true

	// Top edge (screen row 0) → LineUp.
	m.selLineRow = 0
	m.edgeAutoScroll()
	if m.viewport.YOffset != 4 {
		t.Fatalf("贴顶应 LineUp：YOffset 5 → 4，实际 %d", m.viewport.YOffset)
	}

	// Bottom edge (Height-1) → LineDown.
	m.viewport.SetYOffset(5)
	m.selLineRow = m.viewport.Height - 1
	m.edgeAutoScroll()
	if m.viewport.YOffset != 6 {
		t.Fatalf("贴底应 LineDown：YOffset 5 → 6，实际 %d", m.viewport.YOffset)
	}

	// Middle row → no scroll.
	m.viewport.SetYOffset(5)
	m.selLineRow = 3
	m.edgeAutoScroll()
	if m.viewport.YOffset != 5 {
		t.Fatalf("中间行不应滚动：YOffset 应保持 5，实际 %d", m.viewport.YOffset)
	}
}

// TestColToByteOffsetTerminalWidth proves colToByteOffset agrees with the
// terminal's actual column layout on East-Asian Ambiguous runes and Powerline
// private-use glyphs. colToByteOffset maps a mouse screen column (msg.X) to a
// byte offset, so it MUST use the same width model the terminal renders with.
// Before the fix it routed through runewidth.RuneWidth, which widths '·' (U+
// 00B7), '│' (U+2502) and the Powerline arrow U+E0B0 as 2 under a CJK locale
// (IsEastAsian()==true), while lipgloss — and Cascadia Code / every Powerline
// font — renders each of them in a single cell. Each such rune on the dragged
// line therefore made the highlight drift one column past the cursor; the
// input border (│), its placeholder (·) and the footer (U+E0B0 / │) are full
// of them, so a bottom-area selection could land several characters off.
//
// The check walks each line rune-by-rune: at every rune's start column (as
// the terminal sees it, via lipgloss.Width), colToByteOffset must return that
// rune's byte offset.
func TestColToByteOffsetTerminalWidth(t *testing.T) {
	cases := []string{
		"  项目  main  opus",                              // footer with powerline arrows
		"│ Send a message…  (Enter = send · newline)   │", // input border + placeholder ·
		"proj │ main │ opus",                              // muted-footer separators
	}
	for _, line := range cases {
		col := 0
		for byteOff, r := range line {
			if got := colToByteOffset(line, col); got != byteOff {
				t.Errorf("line=%q col=%d (start of rune %q): colToByteOffset → byte %d, want %d",
					line, col, r, got, byteOff)
			}
			col += lipgloss.Width(string(r))
		}
	}
}

// TestReflow_ClampsStaleYOffsetWhenContentFits reproduces the startup race where
// an event's GotoBottom ran against the pre-WindowSizeMsg minimum Height (3)
// while the banner was tall, pinning YOffset deep past the content. WindowSizeMsg
// must clamp it back to 0 so the banner top — the block logo — is visible
// without the user scrolling up.
func TestReflow_ClampsStaleYOffsetWhenContentFits(t *testing.T) {
	m := newTestModel(t)
	// Stand in for the tall startup banner: set content directly, force the
	// minimum Height, and GotoBottom as an early event would.
	m.viewport.SetContent(strings.Repeat("banner-line\n", 21))
	m.viewport.Height = 3
	m.viewport.GotoBottom() // pins YOffset = 18
	if m.viewport.YOffset == 0 {
		t.Fatalf("setup failed: pre-size GotoBottom should have pinned YOffset past 0")
	}

	// WindowSizeMsg grows the viewport so the whole banner fits; reflow must
	// clamp the stale YOffset back to the top.
	mm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 40})
	m = mm.(model)
	if m.viewport.YOffset != 0 {
		t.Fatalf("content fits → YOffset must clamp to 0 (logo visible), got %d", m.viewport.YOffset)
	}
}

// ---- W-E-01: renderFooter must honor the detected terminal color profile ----
//
// renderFooter (the Powerline-style bottom status bar) builds every ANSI
// escape by hand rather than through a lipgloss.Style, so it does NOT
// automatically pick up ApplyColorProfile's effect on lipgloss's shared
// renderer — it needs its own read via footerColorSeq/footerSGRParts/sgrWrap
// (see renderFooter's doc comment). Before that plumbing existed, a real
// tuidbg capture under NO_COLOR=1 showed the footer still emitting full
// ANSI-256 escapes while every other lipgloss-rendered element correctly
// suppressed color. These tests pin that fix at the unit level: they fail if
// renderFooter reverts to raw "\x1b[48;5;…m" string concatenation, or if
// footerSGRParts stops matching termenv's own Ascii-suppresses-everything
// rule (see TestRenderFooterSuppressesBoldUnderAsciiToo).

// footerTestSegs is a representative segment list: colored pills (bg != the
// default "236" footer background) so the Powerline-arrow transition path
// (not just the plain-separator path) is exercised.
var footerTestSegs = []segmentDef{
	{text: " yanshi ", fg: "255", bg: "17", bold: false},
	{text: " main ", fg: "255", bg: "24", bold: false},
}

// TestRenderFooterSuppressesColorUnderAscii proves acceptance criterion 1
// (NO_COLOR) for the footer specifically: under termenv.Ascii, renderFooter
// emits not a single ANSI escape byte for colored-pill segments.
func TestRenderFooterSuppressesColorUnderAscii(t *testing.T) {
	withColorProfile(t, termenv.Ascii)
	out := renderFooter(footerTestSegs, 0)
	if strings.ContainsRune(out, '\x1b') {
		t.Fatalf("Ascii profile: renderFooter output still contains an ANSI escape byte: %q", out)
	}
}

// footerBoldTestSegs pins a bold segment (theme tables carry bold:true pills,
// e.g. the "perm_yolo" mode indicator) alongside a plain one.
var footerBoldTestSegs = []segmentDef{
	{text: " YOLO ", fg: "255", bg: "196", bold: true},
	{text: " main ", fg: "255", bg: "24", bold: false},
}

// TestRenderFooterSuppressesBoldUnderAsciiToo closes a gap the first version
// of this fix left open: footerSGRParts used to append bold's "1" SGR code
// unconditionally, regardless of profile. That was inconsistent with every
// other lipgloss-rendered element in this package — termenv.Style.Styled
// (style.go) has an unconditional "if t.profile == Ascii { return s }" up
// front, which drops ALL styling under Ascii, not just color; a real tuidbg
// capture under NO_COLOR=1 confirmed roleUser (Bold+Foreground, rendering
// "you:") emits zero escape bytes end to end. So a footer pill using the
// theme's bold:true entries (e.g. "perm_yolo") must degrade the same way.
func TestRenderFooterSuppressesBoldUnderAsciiToo(t *testing.T) {
	withColorProfile(t, termenv.Ascii)
	out := renderFooter(footerBoldTestSegs, 0)
	if strings.ContainsRune(out, '\x1b') {
		t.Fatalf("Ascii profile: renderFooter output still contains an ANSI escape byte for a bold segment: %q", out)
	}
}

// TestRenderFooterBoldSurvivesUnderColorProfiles proves the flip side: bold
// is only dropped under Ascii specifically, not under every non-256 profile
// — under ANSI (16-color) a bold segment still emits "\x1b[1m" alongside the
// degraded color code.
func TestRenderFooterBoldSurvivesUnderColorProfiles(t *testing.T) {
	withColorProfile(t, termenv.ANSI)
	out := renderFooter(footerBoldTestSegs, 0)
	if !strings.Contains(out, "\x1b[1;") && !strings.Contains(out, ";1;") && !strings.Contains(out, ";1m") {
		t.Fatalf("ANSI profile: expected bold's \"1\" SGR code to survive alongside the degraded color, got %q", out)
	}
}

// TestRenderFooterDegradesTo16ColorUnderANSI proves acceptance criterion 4's
// 16-color half for the footer: under termenv.ANSI, the 256-color pill
// backgrounds ("17", "24") degrade to plain 16-color SGR codes, never an
// 8-bit "38;5;"/"48;5;" palette index.
func TestRenderFooterDegradesTo16ColorUnderANSI(t *testing.T) {
	withColorProfile(t, termenv.ANSI)
	out := renderFooter(footerTestSegs, 0)
	if strings.Contains(out, "38;5;") || strings.Contains(out, "48;5;") {
		t.Fatalf("ANSI (16-color) profile: renderFooter should degrade to plain 16-color codes, got %q", out)
	}
	if !strings.ContainsRune(out, '\x1b') {
		t.Fatalf("ANSI (16-color) profile: expected a 16-color escape sequence, got none: %q", out)
	}
}

// TestStatusHeaderANSIOverridesFixMeasuredCollisions proves RE-E (fix-e1
// review of W-E-01) end to end through the production entry point
// (statusHeader, not a hand-built segmentDef): under termenv.ANSI, the
// "ctx"/"perm_auto" pills no longer degrade to SGR 43 (yellow — the review
// measured white-on-yellow as low contrast) and "total" no longer degrades to
// SGR 101 (bright red — misleading, since red is this same footer's
// perm_yolo error color). ANSI256 is exercised as a negative control in the
// same test: those pills MUST still show their original 8-bit backgrounds
// there, proving this is a profile-scoped override, not a change to the
// underlying theme table.
func TestStatusHeaderANSIOverridesFixMeasuredCollisions(t *testing.T) {
	m := newModel(&fakeSession{}, "/proj")
	m = m.applyEvent(cli.StreamEvent{
		Kind: "status", Model: "claude-opus-4", TokensIn: 2000, TokensOut: 500, ContextWindow: 128000,
	})
	m.permMode = guard.ModeAuto

	withColorProfile(t, termenv.ANSI)
	out := m.statusHeader()
	if strings.Contains(out, "43m") || strings.Contains(out, ";43;") || strings.Contains(out, ";43m") {
		t.Errorf("ANSI profile: footer still degrades a pill to yellow (SGR 43): %q", out)
	}
	if strings.Contains(out, "101m") || strings.Contains(out, ";101;") || strings.Contains(out, ";101m") {
		t.Errorf("ANSI profile: footer still degrades a pill to bright-red (SGR 101): %q", out)
	}
	if !strings.ContainsRune(out, '\x1b') {
		t.Fatalf("ANSI profile: expected escape sequences in the footer, got none: %q", out)
	}

	withColorProfile(t, termenv.ANSI256)
	out256 := m.statusHeader()
	if !strings.Contains(out256, "48;5;58") {
		t.Errorf("negative control failed: ANSI256 profile should still render \"ctx\" at its original 48;5;58, got %q", out256)
	}
	if !strings.Contains(out256, "48;5;130") {
		t.Errorf("negative control failed: ANSI256 profile should still render \"total\" at its original 48;5;130, got %q", out256)
	}
	if !strings.Contains(out256, "48;5;94") {
		t.Errorf("negative control failed: ANSI256 profile should still render \"perm_auto\" at its original 48;5;94, got %q", out256)
	}
}

// TestRenderFooterUnchangedUnderANSI256 proves the fix is behavior-preserving
// under the profile this file hardcoded before W-E-01: the colored pills
// still render as 8-bit "48;5;17"/"48;5;24" backgrounds, byte-for-byte what
// the pre-fix raw literals produced.
func TestRenderFooterUnchangedUnderANSI256(t *testing.T) {
	withColorProfile(t, termenv.ANSI256)
	out := renderFooter(footerTestSegs, 0)
	if !strings.Contains(out, "48;5;17") {
		t.Fatalf("ANSI256 profile: expected the first pill's \"48;5;17\" background, got %q", out)
	}
	if !strings.Contains(out, "48;5;24") {
		t.Fatalf("ANSI256 profile: expected the second pill's \"48;5;24\" background, got %q", out)
	}
}
