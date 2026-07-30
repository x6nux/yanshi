package tui

import (
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// toast 是 UX7 的堆叠通知条。与 activity line 不同,toast 跨 turn 存活:
// activity 是 turn 内的进度指示(Thinking…、tool running),turn 结束即清;
// toast 是 turn 外的回执/警告(save done、auth failed、connection lost),有独立 TTL。
//
// 堆叠上限 5 条,超出 FIFO 丢最旧。error 级永不自动过期,需 Esc 手动关闭。
type toast struct {
	Level     string // "info" | "warn" | "error"
	Text      string
	CreatedAt time.Time
	ExpiresAt time.Time // 零值 = 不过期(error 级)
}

type toastQueue struct {
	items []toast
}

// toastTickMsg 是独立 tick(不复用 activityTickMsg),频率 500ms,用于 prune 过期项。
// 独立的必要性:activity 是 turn 内的,toast 是 turn 外的,共用 tick 会让 toast
// 随 turn 结束被意外清空(activity tick 在 done 帧后停 re-arm)。
type toastTickMsg struct{}

const (
	toastMaxVisible   = 5
	toastInfoTTL      = 3 * time.Second
	toastWarnTTL      = 5 * time.Second
	toastTickInterval = 500 * time.Millisecond
)

func (q *toastQueue) push(t toast) {
	if t.CreatedAt.IsZero() {
		t.CreatedAt = time.Now()
	}
	if t.ExpiresAt.IsZero() && t.Level != "error" {
		switch t.Level {
		case "info":
			t.ExpiresAt = t.CreatedAt.Add(toastInfoTTL)
		case "warn":
			t.ExpiresAt = t.CreatedAt.Add(toastWarnTTL)
		}
	}
	q.items = append(q.items, t)
	// FIFO 丢最旧
	if len(q.items) > toastMaxVisible {
		q.items = q.items[len(q.items)-toastMaxVisible:]
	}
}

// prune 移除过期项(error 级不受影响)。每次调用后,渲染方应重新计算 blockHeight。
func (q *toastQueue) prune(now time.Time) {
	out := q.items[:0]
	for _, t := range q.items {
		if t.Level == "error" || t.ExpiresAt.IsZero() || t.ExpiresAt.After(now) {
			out = append(out, t)
		}
	}
	q.items = out
}

// dismissLastError 关闭最近一条 error toast(Esc 键)。
func (q *toastQueue) dismissLastError() {
	for i := len(q.items) - 1; i >= 0; i-- {
		if q.items[i].Level == "error" {
			q.items = append(q.items[:i], q.items[i+1:]...)
			return
		}
	}
}

// hasErrorToast reports whether the queue currently holds any error-level
// toast. Used by the Esc handler to decide whether to consume the keystroke
// (dismiss error) or let it fall through to other bindings (close picker).
func (q toastQueue) hasErrorToast() bool {
	for _, t := range q.items {
		if t.Level == "error" {
			return true
		}
	}
	return false
}

// hasErrorToast on model so Update's KeyEscape branch can guard cleanly.
func (m model) hasErrorToast() bool { return m.toasts.hasErrorToast() }

// blockHeight 返回 toast 堆叠占用的行数(每条 1 行 + 1 行 padding)。
// prune 后调用方应重新读取此值并 reflow viewport,避免错位(review 必修)。
func (q toastQueue) blockHeight() int {
	n := len(q.items)
	if n == 0 {
		return 0
	}
	return n + 1 // 1 行 padding(顶部空行隔开 transcript)
}

// truncateToast 把 s 按终端 display width 截断到 width(含省略号)。
// width<6 时返回原文,既避免负宽度,也让 lipgloss 在极窄窗口自行 wrap。
func truncateToast(s string, width int) string {
	if width < 6 {
		return s
	}
	if lipgloss.Width(s) <= width {
		return s
	}
	const ellipsis = "…"
	limit := width - lipgloss.Width(ellipsis)
	var b strings.Builder
	for _, r := range s {
		candidate := b.String() + string(r)
		if lipgloss.Width(candidate) > limit {
			break
		}
		b.WriteRune(r)
	}
	return b.String() + ellipsis
}

// render 渲染 toast 堆叠为多行字符串,供 view.go 在 transcript 之上插入。
func (q toastQueue) render(width int) string {
	if len(q.items) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n") // 顶部空行隔开 transcript
	for _, t := range q.items {
		text := truncateToast(t.Text, width-4) // -4 留给前缀 "[!] "
		var prefix, body string
		switch t.Level {
		case "info":
			prefix = " "
			body = infoToastStyle.Render(text)
		case "warn":
			prefix = warnStyle.Render("[!]")
			body = warnToastStyle.Render(text)
		case "error":
			prefix = errStyle.Render("[X]")
			body = errToastStyle.Render(text)
		default:
			prefix = " "
			body = text
		}
		b.WriteString(prefix + " " + body + "\n")
	}
	return b.String()
}

// pushToast 向 toast 堆叠追加一条;若当前没有 tick 在飞,返回一个启动 tick 的 Cmd。
// 调用方必须把返回的 Cmd 并入 Update 的返回值(tea.Batch),否则 tick 不会启动,
// 非 error 级 toast 永不消失。error 级靠 Esc 关闭,但仍需 tick 来驱动 prune。
func (m *model) pushToast(level, text string) tea.Cmd {
	m.toasts.push(toast{Level: level, Text: text})
	if m.toastTickActive {
		return nil // 已有 tick:不重复启动
	}
	m.toastTickActive = true
	return tea.Tick(toastTickInterval, func(time.Time) tea.Msg {
		return toastTickMsg{}
	})
}
