package tui

import (
	"os"
	"path/filepath"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestHistory_AddDedupMovesLatestToEnd(t *testing.T) {
	dir := t.TempDir()
	h, _ := LoadHistory(filepath.Join(dir, "history.jsonl"), 500)
	h.Add("first")
	h.Add("second")
	h.Add("first") // 去重 + 移到尾部
	items := h.Items()
	if len(items) != 2 {
		t.Fatalf("去重后应有 2 条,得到 %d:%v", len(items), items)
	}
	if items[0].Text != "second" || items[1].Text != "first" {
		t.Errorf("重复项应移到尾部(最新),得到 %v", items)
	}
}

func TestHistory_CapacityDropsOldest(t *testing.T) {
	dir := t.TempDir()
	h, _ := LoadHistory(filepath.Join(dir, "history.jsonl"), 3)
	for _, s := range []string{"a", "b", "c", "d"} {
		h.Add(s)
	}
	items := h.Items()
	if len(items) != 3 {
		t.Fatalf("cap=3 应保留 3 条,得到 %d", len(items))
	}
	if items[0].Text != "b" || items[2].Text != "d" {
		t.Errorf("应丢最旧 a,保留 [b,c,d],得到 %v", items)
	}
}

func TestHistory_PersistAndHealCorruptLine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "history.jsonl")
	h, _ := LoadHistory(path, 500)
	h.Add("hello")
	h.Add("world")
	if err := h.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	// 追加一条损坏行,Load 应跳过
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	_, _ = f.WriteString("NOT_JSON\n")
	_ = f.Close()
	h2, err := LoadHistory(path, 500)
	if err != nil {
		t.Fatalf("损坏行应自愈: %v", err)
	}
	if got := len(h2.Items()); got != 2 {
		t.Errorf("重载应保留 2 条合法历史,得到 %d", got)
	}
}

func TestHistory_FuzzySearchNewestFirst(t *testing.T) {
	dir := t.TempDir()
	h, _ := LoadHistory(filepath.Join(dir, "history.jsonl"), 500)
	h.Add("write tests")
	h.Add("fix model picker")
	h.Add("write docs")
	got := h.Search("write", 10)
	if len(got) != 2 {
		t.Fatalf("search 'write' 应返回 2 条,得到 %d:%v", len(got), got)
	}
	if got[0].Text != "write docs" {
		t.Errorf("同分时最新优先,Top1 应 write docs,得到 %q", got[0].Text)
	}
}

// TestHistory_EditLastQueued_AltUp 是 review 必修遗漏项。
func TestHistory_EditLastQueued_AltUp(t *testing.T) {
	m := newTestModel(t)
	m.msgQueue = []string{"first queued", "last queued"}
	m.input.SetValue("")

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyUp, Alt: true})
	got := updated.(model)
	if got.input.Value() != "last queued" {
		t.Errorf("Alt+↑ 应把最后 queued 填回 textarea,得到 %q", got.input.Value())
	}
	if len(got.msgQueue) != 1 || got.msgQueue[0] != "first queued" {
		t.Errorf("Alt+↑ 应从 queue 删除尾项,剩余 [first queued],得到 %v", got.msgQueue)
	}
}

func TestHistory_EditLastQueued_EmptyNoop(t *testing.T) {
	m := newTestModel(t)
	m.input.SetValue("current draft")
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyUp, Alt: true})
	got := updated.(model)
	if got.input.Value() != "current draft" {
		t.Errorf("queue 空时 Alt+↑ 不应覆盖当前草稿,得到 %q", got.input.Value())
	}
}

func TestHistory_AltROpensSearch(t *testing.T) {
	m := newTestModel(t)
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'r'}, Alt: true})
	got := updated.(model)
	if got.historySearch == nil || !got.historySearch.visible {
		t.Errorf("Alt+R 应打开 history search popup")
	}
}

func TestHistory_PopupEmptyNavigationSafe(t *testing.T) {
	m := newTestModel(t)
	m.historySearch = &historyState{visible: true}
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyDown})
	got := updated.(model)
	if got.historySearch == nil || got.historySearch.cursor != 0 {
		t.Fatalf("空结果按 Down 应 no-op,得到 %+v", got.historySearch)
	}
	updated, _ = got.Update(tea.KeyMsg{Type: tea.KeyUp})
	got = updated.(model)
	if got.historySearch.cursor != 0 {
		t.Errorf("空结果按 Up 应 no-op,cursor=%d", got.historySearch.cursor)
	}
}

func TestHistory_PopupEnterRestoresSelectedPrompt(t *testing.T) {
	m := newTestModel(t)
	m.input.SetValue("draft to replace")
	m.historySearch = &historyState{
		visible: true,
		cursor:  1,
		items: []historyItem{
			{Text: "first prompt"},
			{Text: "selected\nmultiline prompt"},
		},
	}
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	got := updated.(model)
	if got.input.Value() != "selected\nmultiline prompt" {
		t.Errorf("Enter 应恢复选中 prompt,得到 %q", got.input.Value())
	}
	if got.historySearch != nil {
		t.Errorf("Enter 后应关闭 history popup")
	}
}

func TestHistory_PopupEscapePreservesDraft(t *testing.T) {
	m := newTestModel(t)
	m.input.SetValue("keep this draft")
	m.historySearch = &historyState{
		visible: true,
		items:   []historyItem{{Text: "old prompt"}},
	}
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyEscape})
	got := updated.(model)
	if got.input.Value() != "keep this draft" {
		t.Errorf("Esc 不应覆盖当前草稿,得到 %q", got.input.Value())
	}
	if got.historySearch != nil {
		t.Errorf("Esc 后应关闭 history popup")
	}
}

func TestHistory_RecordsOnlyWhenDispatching(t *testing.T) {
	h, _ := LoadHistory(filepath.Join(t.TempDir(), "history.jsonl"), 500)
	m := newTestModel(t)
	m.history = h
	m.saveQueue = make(chan saveCmd, 1)
	m.msgQueue = []string{"queued but unsent"}
	if got := len(m.history.Items()); got != 0 {
		t.Fatalf("仅入 queue 不应写 history,得到 %d 条", got)
	}

	m, _ = m.dispatchSend("actually sent", false)
	items := m.history.Items()
	if len(items) != 1 || items[0].Text != "actually sent" {
		t.Fatalf("dispatchSend 应记录真实发送 prompt,得到 %+v", items)
	}
	select {
	case save := <-m.saveQueue:
		if save.fn == nil {
			t.Fatal("dispatchSend 应 enqueue 非空 history save")
		}
	default:
		t.Fatal("dispatchSend 应把 history save 放入单 worker queue")
	}
}
