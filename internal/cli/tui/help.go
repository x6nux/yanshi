package tui

import (
	"fmt"
	"sort"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/x6nux/yanshi/internal/guard"
)

// helpItem 是帮助面板的一条项。
type helpItem struct {
	Label  string
	Source string // "command" | "mode" | "theme" | "key"
	Hint   string
}

// keyBindings 是手工维护的静态表(Go 无法反射 keybinding)。
// 新增 KeyMsg 分支时,同步更新此表 + TestHelp_KeybindingsCoreEntries —— 且
// handleKeyMsg 顶层 switch(handlers.go)的 case 现在由
// keybindings_wiring_test.go::keyBindingsCensus 做双向机器核对,漏改这张表
// 会让 TestKeyBindingsCensusMatchesSwitch 变红(见该文件的口径说明)。
var keyBindings = []helpItem{
	{Label: "Enter", Source: "key", Hint: "send message (textarea)"},
	{Label: "Ctrl+Enter", Source: "key", Hint: "insert newline"},
	{Label: "Ctrl+K", Source: "key", Hint: "open action palette"},
	{Label: "Ctrl+O", Source: "key", Hint: "expand/collapse last block"},
	{Label: "Ctrl+S", Source: "key", Hint: "save draft to stash"},
	{Label: "Ctrl+E", Source: "key", Hint: "open $VISUAL/$EDITOR on the input"},
	{Label: "Ctrl+T", Source: "key", Hint: "fullscreen transcript pager"},
	{Label: "Ctrl+V", Source: "key", Hint: "attach clipboard image (silent if none)"},
	{Label: "Ctrl+C", Source: "key", Hint: "cancel in-flight turn"},
	{Label: "Ctrl+Z", Source: "key", Hint: "suspend to background (Unix only; fg to resume)"},
	{Label: "Alt+R", Source: "key", Hint: "search prompt history"},
	{Label: "Alt+Up", Source: "key", Hint: "edit last queued message"},
	{Label: "Shift+Tab", Source: "key", Hint: "cycle permission mode"},
	{Label: "F1", Source: "key", Hint: "toggle this help"},
	{Label: "Esc", Source: "key", Hint: "close popup / dismiss error toast"},
	{Label: "Tab", Source: "key", Hint: "autocomplete command"},
	{Label: "Up/Down", Source: "key", Hint: "navigate autocomplete list / permission prompt"},
	{Label: "PgUp/PgDn", Source: "key", Hint: "scroll transcript"},
}

// collectHelpEntries 从四个 source 收集帮助项。modes/commands/themes 三段动态
// (防漂移),keys 静态。command Label 统一带 "/" 前缀(与 Task 4 一致,匹配
// TestHelp_CommandsFromTable 断言)。
//
// 注:helpPopup 走到这里,但 reflow 不直接调用 helpPopup(改读 m.helpRendered
// 缓存);缓存由 Update 的 F1/Runes/Backspace 分支刷新。这样既避免每次 reflow
// 重渲染,也避免 commandTable → cmdModel → sendControlFrame → reflow →
// helpPopup → ... → commandTable 的静态初始化依赖环。
func (m model) collectHelpEntries() []helpItem {
	var items []helpItem
	for _, cmd := range commandTable {
		items = append(items, helpItem{
			Label: "/" + cmd.name, Source: "command", Hint: localizedHelp(m.bundle, cmd),
		})
	}
	for _, mode := range guard.Modes() { // 防漂移
		items = append(items, helpItem{Label: string(mode), Source: "mode", Hint: "permission mode"})
	}
	for _, th := range themeList { // 防漂移
		items = append(items, helpItem{Label: string(th.Name), Source: "theme", Hint: "color theme"})
	}
	items = append(items, keyBindings...)
	return items
}

// rankedHelpEntries 按 fuzzy 排序当前 query。
func (m model) rankedHelpEntries() []helpItem {
	all := m.collectHelpEntries()
	if m.helpQuery == "" {
		return all
	}
	type scored struct {
		item  helpItem
		score float64
	}
	var list []scored
	for _, it := range all {
		s := fuzzyScore(m.helpQuery, it.Label+" "+it.Hint) // 也搜 Hint
		if s > 0 {
			list = append(list, scored{item: it, score: s})
		}
	}
	sort.Slice(list, func(i, j int) bool {
		return list[i].score > list[j].score
	})
	out := make([]helpItem, len(list))
	for i, s := range list {
		out[i] = s.item
	}
	return out
}

// helpChromeLines is what the panel costs before a single entry: the border's
// two rows and the title. Subtracted from the terminal height so the window
// below is the number of ENTRY rows that actually fit.
const helpChromeLines = 3

// helpMinRows keeps the panel usable on an absurdly short terminal. Showing
// one entry is worse than showing a few and letting the border overflow by a
// row, which the renderer trims from a place the user can scroll back to.
const helpMinRows = 5

// helpStart is the single place the visible window is anchored on the cursor.
//
// It lives alone because the first draft computed it twice — once when sizing
// the window and again inside the shrink loop — and the duplication made the
// scroll behaviour untestable: deleting either copy left the other one
// covering for it, so a mutation probe that removed the anchoring came back
// green.
func helpStart(cursor, rows, total int) int {
	if rows >= total {
		return 0
	}
	start := 0
	if cursor >= rows {
		start = cursor - rows + 1
	}
	if start+rows > total {
		start = total - rows
	}
	if start < 0 {
		start = 0
	}
	return start
}

// renderHelp 渲染帮助面板中当前窗口内的条目(按 source 分组)。
//
// helpPopup used to render every entry — 60-odd lines across four sections.
// view.go accounts for the block's height but does not clip it, so the trim
// happened in bubbletea's renderer, which keeps the LAST height lines: the
// title, the "Commands:" header and the first ~35 commands were off the top of
// the screen with no way to reach them, since the panel absorbs every key
// except printable search characters.
//
// The row count is found by rendering and measuring rather than estimated,
// because section headers and the blank line between sections consume screen
// rows too, and how many of those a window contains depends on which entries
// the current query matched.
func (m model) renderHelp() string {
	entries := m.rankedHelpEntries()
	budget := m.height - helpChromeLines
	if budget < helpMinRows {
		budget = helpMinRows
	}
	for rows := len(entries); rows > 0; rows-- {
		start := helpStart(m.helpCursor, rows, len(entries))
		out, height := renderHelpSlice(entries, start, rows, m.helpCursor)
		if height <= budget || rows == 1 {
			return out
		}
	}
	return ""
}

// renderHelpSlice renders entries[start:start+rows] and reports the number of
// screen lines it occupies. The cursor row is marked so a user scrolling with
// the arrow keys can see where they are.
func renderHelpSlice(entries []helpItem, start, rows, cursor int) (string, int) {
	end := start + rows
	if end > len(entries) {
		end = len(entries)
	}
	var b strings.Builder
	lines := 0
	currentSrc := ""
	for i := start; i < end; i++ {
		e := entries[i]
		if e.Source != currentSrc {
			if currentSrc != "" {
				b.WriteString("\n")
				lines++
			}
			currentSrc = e.Source
			b.WriteString(sectionHeader(e.Source) + "\n")
			lines++
		}
		marker := "  "
		if i == cursor {
			marker = "▶ "
		}
		b.WriteString(marker + e.Label + " — " + e.Hint + "\n")
		lines++
	}
	if start > 0 || end < len(entries) {
		b.WriteString(toolMeta.Render(fmt.Sprintf("  %d-%d of %d — ↑/↓ to scroll",
			start+1, end, len(entries))) + "\n")
		lines++
	}
	return b.String(), lines
}

// helpPopup 给 renderHelp 加可见性、搜索提示、空结果和边框。
func (m model) helpPopup() string {
	if !m.helpVisible {
		return ""
	}
	header := "Help"
	if m.helpQuery != "" {
		header += " — search: " + m.helpQuery
	}
	body := strings.TrimRight(m.renderHelp(), "\n")
	if body == "" {
		body = toolMeta.Render("  no matching help entries")
	}
	return inputBorder.Render(paletteStyle.Render(header) + "\n" + body)
}

func sectionHeader(src string) string {
	switch src {
	case "command":
		return "Commands:"
	case "mode":
		return "Permission modes:"
	case "theme":
		return "Themes:"
	case "key":
		return "Keys:"
	}
	return src + ":"
}

// helpPageStep is how far PgUp/PgDn move the help cursor. A fixed step rather
// than "one window" because the window size depends on the rendered section
// layout, which is only known after rendering — and a page key that sometimes
// moves 8 rows and sometimes 20 is harder to use than one that always moves 10.
const helpPageStep = 10

// moveHelpCursor clamps cursor movement to the list. An empty list keeps the
// cursor at 0 rather than going negative, which would render an empty window
// and look like the panel had broken.
func moveHelpCursor(cursor, total int, key tea.KeyType) int {
	switch key {
	case tea.KeyDown:
		cursor++
	case tea.KeyUp:
		cursor--
	case tea.KeyPgDown:
		cursor += helpPageStep
	case tea.KeyPgUp:
		cursor -= helpPageStep
	}
	if cursor >= total {
		cursor = total - 1
	}
	if cursor < 0 {
		cursor = 0
	}
	return cursor
}
