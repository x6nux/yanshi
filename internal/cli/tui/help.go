package tui

import (
	"sort"
	"strings"

	"github.com/x6nux/yanshi/internal/guard"
)

// helpItem 是帮助面板的一条项。
type helpItem struct {
	Label  string
	Source string // "command" | "mode" | "theme" | "key"
	Hint   string
}

// keyBindings 是手工维护的静态表(Go 无法反射 keybinding)。
// 新增 KeyMsg 分支时,同步更新此表 + TestHelp_KeybindingsCoreEntries。
var keyBindings = []helpItem{
	{Label: "Enter", Source: "key", Hint: "send message (textarea)"},
	{Label: "Ctrl+Enter", Source: "key", Hint: "insert newline"},
	{Label: "Ctrl+K", Source: "key", Hint: "open action palette"},
	{Label: "Ctrl+O", Source: "key", Hint: "expand/collapse last block"},
	{Label: "Ctrl+S", Source: "key", Hint: "save draft to stash"},
	{Label: "Ctrl+C", Source: "key", Hint: "cancel in-flight turn"},
	{Label: "Alt+R", Source: "key", Hint: "search prompt history"},
	{Label: "Alt+Up", Source: "key", Hint: "edit last queued message"},
	{Label: "F1", Source: "key", Hint: "toggle this help"},
	{Label: "Esc", Source: "key", Hint: "close popup / dismiss error toast"},
	{Label: "Tab", Source: "key", Hint: "autocomplete command"},
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
		items = append(items, helpItem{Label: "/" + cmd.name, Source: "command", Hint: cmd.help})
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

// renderHelp 渲染帮助面板的全部条目(按 source 分组)。
func (m model) renderHelp() string {
	entries := m.rankedHelpEntries()
	var b strings.Builder
	currentSrc := ""
	for _, e := range entries {
		if e.Source != currentSrc {
			if currentSrc != "" {
				b.WriteString("\n")
			}
			currentSrc = e.Source
			b.WriteString(sectionHeader(e.Source) + "\n")
		}
		b.WriteString("  " + e.Label + " — " + e.Hint + "\n")
	}
	return b.String()
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
