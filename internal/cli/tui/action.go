package tui

import (
	"fmt"
	"sort"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/x6nux/yanshi/internal/guard"
)

// actionItem 是全局动作面板的一条候选项。
type actionItem struct {
	Label    string                           // 显示文本,如 "/help"、"default"、"gpt-4o"
	Source   string                           // "command" | "mode" | "model" | "theme"
	Category string                           // 用于分组显示的类目(与 Source 同义,但允许未来细分)
	Hint     string                           // 副本/说明,如 "Show this help"
	Action   func(model) (tea.Model, tea.Cmd) // 对齐现有 command.run 约定
}

// actionState 是 popup 的运行时状态。
type actionState struct {
	query   string
	cursor  int
	items   []actionItem // 排序后的当前显示项
	visible bool
}

// 本批不定义请求/完成 wrapper Msg:
// Ctrl+K 直接调用现有 sendControlFrame(NewListModels),真实 "models" reply
// 由 applyEvent case "models" 驱动 cache/popup 刷新。session/tool/MCP 均 deferred。

// collectActions 从四个 source 收集候选项:
//   - command: commandTable 的每一项(Label 加 "/" 前缀展示)
//   - mode: guard.Modes() 的每一项(防漂移)
//   - model: m.models(由 applyEvent case "models" 填充;首次 Ctrl+K 前为空)
//   - theme: themeList 的每一项
//   - file: m.atCandidates("")(frecency 排序,项目内过滤;UX4)
//   - session: m.sessionsCache(由 applyEvent case "sessions" 填充;
//     openActionPopup 每次打开都重新请求,理由同 models —— 另一个窗口里新建的
//     会话否则永远不出现)
//
// tool/MCP source remains deferred: those are not a fixed registry the TUI
// holds, so offering them means a round trip per keystroke rather than a
// cached list, which is a different design from the five sources here.
func (m model) collectActions() []actionItem {
	var items []actionItem
	// /help is the first action item even though it's not in commandTable:
	// cmdHelp needs commandTable to render, so the table initializer would
	// cycle if /help were registered there. We surface it here so the action
	// palette still offers /help.
	items = append(items, actionItem{
		Label:    "/help",
		Source:   "command",
		Category: "command",
		Hint:     "list commands",
		Action: func(mm model) (tea.Model, tea.Cmd) {
			mm.input.SetValue("/help ")
			return mm, nil
		},
	})
	// command source —— Label 统一加 "/" 前缀(执行时把真实 cmd.name 捕获到 closure)
	for _, cmd := range commandTable {
		name := cmd.name
		items = append(items, actionItem{
			Label:    "/" + name,
			Source:   "command",
			Category: "command",
			Hint:     cmd.help,
			Action: func(mm model) (tea.Model, tea.Cmd) {
				// 把 "/" + cmd.name 写入 composer,等同于用户键入 + 后续 Enter
				mm.input.SetValue("/" + name + " ")
				return mm, nil
			},
		})
	}
	// mode source(从 guard.Modes() 防漂移);closure 直接调用真实 free function
	// cmdMode(commands.go),避免在 action.go 里引入未定义的 helper。
	for _, mode := range guard.Modes() {
		modeArg := string(mode)
		items = append(items, actionItem{
			Label:    modeArg,
			Source:   "mode",
			Category: "mode",
			Hint:     "permission mode",
			Action: func(mm model) (tea.Model, tea.Cmd) {
				return cmdMode(mm, []string{modeArg})
			},
		})
	}
	// model source(由 applyEvent case "models" 填充 m.models);closure 调用真实
	// cmdModel(commands.go),它内部自己发 NewSetModel 帧,因此 action.go 不需要
	// import proto。
	for _, modelName := range m.models {
		mn := modelName
		items = append(items, actionItem{
			Label:    mn,
			Source:   "model",
			Category: "model",
			Hint:     "switch model",
			Action: func(mm model) (tea.Model, tea.Cmd) {
				return cmdModel(mm, []string{mn})
			},
		})
	}
	// theme source(从 themeList 防漂移);closure 调用真实 cmdTheme(commands.go)。
	for _, th := range themeList {
		thName := string(th.Name)
		items = append(items, actionItem{
			Label:    thName,
			Source:   "theme",
			Category: "theme",
			Hint:     "color theme",
			Action: func(mm model) (tea.Model, tea.Cmd) {
				return cmdTheme(mm, []string{thName})
			},
		})
	}
	// file source(UX4)——TopN 的第二个消费者。选中即把 @path 填进输入框
	// 而不是直接发送:附件是给下一句话用的,不是一次动作。这也是与其余四源
	// 语义不同的唯一一项,所以 Hint 明说它做什么。
	for _, rel := range m.atCandidates("") {
		p := rel
		items = append(items, actionItem{
			Label:    p,
			Source:   "file",
			Category: "file",
			Hint:     "attach to the next message",
			Action: func(mm model) (tea.Model, tea.Cmd) {
				mm.input.SetValue(strings.TrimRight(mm.input.Value(), " ") + " @" + p + " ")
				mm.input.CursorEnd()
				mm.growInput()
				return mm, nil
			},
		})
	}
	// session source(从 m.sessionsCache);closure 调用真实 cmdRestore
	// (commands.go),所以选中一项就是恢复它 —— 与其他四源一样直接执行,而不是
	// 把命令填进输入框等用户再按一次 Enter。
	for _, ss := range m.sessionsCache {
		id := ss.ID
		label := ss.Title
		if strings.TrimSpace(label) == "" {
			label = id
		}
		items = append(items, actionItem{
			Label:    label,
			Source:   "session",
			Category: "session",
			Hint:     fmt.Sprintf("restore %s (%d msgs)", id, ss.MsgCount),
			Action: func(mm model) (tea.Model, tea.Cmd) {
				return cmdRestore(mm, []string{id})
			},
		})
	}
	return items
}

// rankedActions 按 fuzzy 分数排序当前 query 的匹配项。
func (m model) rankedActions() []actionItem {
	all := m.collectActions()
	if m.action == nil || m.action.query == "" {
		return all // 空 query 显示全部(保持 registry 顺序)
	}
	type scored struct {
		item  actionItem
		score float64
	}
	var list []scored
	for _, it := range all {
		s := fuzzyScore(m.action.query, it.Label)
		if s > 0 {
			list = append(list, scored{item: it, score: s})
		}
	}
	sort.Slice(list, func(i, j int) bool {
		return list[i].score > list[j].score
	})
	out := make([]actionItem, len(list))
	for i, s := range list {
		out[i] = s.item
	}
	return out
}

// openActionPopup 在每次 Ctrl+K 打开时立即用当前 m.models cache 生成首批
// items,同时无条件标记新的 list_models 请求 pending。即便 cache 非空也要
// refresh,避免长会话中模型列表过期;真实 reply 才能清 pending。
func (m *model) openActionPopup() {
	m.action = &actionState{visible: true, cursor: 0}
	m.action.items = m.rankedActions()
	m.actionLoadingModels = true
}

// closeActionPopup 只关闭 popup。不得清 actionLoadingModels——用户可能在 reply
// 到达前按 Esc;pending 只能由 applyEvent case "models" 清理。
func (m *model) closeActionPopup() {
	m.action = nil
}

// actionConfirm 在 Enter 时执行选中项的 Action。选择动作也不伪造 models
// loaded;若 refresh reply 仍 pending,继续等真实 reply 清标志。
func (m *model) actionConfirm() (tea.Model, tea.Cmd) {
	if m.action == nil || m.action.cursor >= len(m.action.items) {
		return *m, nil
	}
	it := m.action.items[m.action.cursor]
	m.action = nil
	return it.Action(*m)
}

// actionMove 在 n==0 时 no-op,避免 (cursor±1) % n 除零 panic。
func (m *model) actionMove(delta int) {
	if m.action == nil {
		return
	}
	n := len(m.action.items)
	if n == 0 {
		return
	}
	m.action.cursor = (m.action.cursor + delta + n) % n
}

// actionPopup 渲染最多 10 条候选;选中行复用现有 palette 样式。
// model refresh pending 时保留 cache 行并显示轻量状态,不会把 loading 塞进 items。
func (m model) actionPopup() string {
	if m.action == nil || !m.action.visible {
		return ""
	}
	var b strings.Builder
	title := "Actions"
	if m.action.query != "" {
		title += fmt.Sprintf(" — search: %s", m.action.query)
	}
	b.WriteString(paletteStyle.Render(title) + "\n")
	if m.actionLoadingModels {
		b.WriteString(toolMeta.Render("  refreshing models") + "\n")
	}
	start := 0
	const maxRows = 10
	if m.action.cursor >= maxRows {
		start = m.action.cursor - maxRows + 1
	}
	end := start + maxRows
	if end > len(m.action.items) {
		end = len(m.action.items)
	}
	for i := start; i < end; i++ {
		it := m.action.items[i]
		line := fmt.Sprintf("  %-8s %-24s %s", it.Source, it.Label, it.Hint)
		if i == m.action.cursor {
			line = selPaletteStyle.Render("▶ " + strings.TrimLeft(line, " "))
		} else {
			line = paletteStyle.Render(line)
		}
		b.WriteString(line + "\n")
	}
	if len(m.action.items) == 0 {
		b.WriteString(toolMeta.Render("  no matching actions") + "\n")
	}
	return inputBorder.Render(strings.TrimRight(b.String(), "\n"))
}
