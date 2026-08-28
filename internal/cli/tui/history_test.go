package tui

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

// TestHistory_TrimsAtByteLimit: 上限必须同时是条数和字节。单条大 prompt 逐条
// 都在字节上限之内,但累计超过 defaultHistoryBytes 时应从最旧端裁到预算内,
// 而不是任由文件无界增长。
func TestHistory_TrimsAtByteLimit(t *testing.T) {
	dir := t.TempDir()
	h, _ := LoadHistory(filepath.Join(dir, "history.jsonl"), 500)

	big := strings.Repeat("x", 900*1024) // 900 KiB,单条远小于 defaultHistoryBytes
	h.Add("a:" + big)
	h.Add("b:" + big)
	h.Add("c:" + big) // 三条合计 ~2.6 MiB,超过 2 MiB 字节上限

	items := h.Items()
	if got := historyItemsBytes(items); got > defaultHistoryBytes {
		t.Fatalf("总字节应被裁到 <= %d,得到 %d", defaultHistoryBytes, got)
	}
	if len(items) == 0 {
		t.Fatal("字节裁剪不应把历史清空")
	}
	if last := items[len(items)-1].Text; !strings.HasPrefix(last, "c:") {
		t.Errorf("应保留最新一条(c),得到最后一条前缀 %q", last[:2])
	}
	if first := items[0].Text; strings.HasPrefix(first, "a:") {
		t.Errorf("最旧一条(a)应被字节上限丢弃,但仍在 items[0]")
	}
}

// TestHistory_OversizedSingleEntryRejected: 单条 Text 本身就超过整条字节预算
// 时,Add 拒绝存入(而不是截断或死循环)——该 prompt 已经正常发送,只是不会
// 出现在历史召回里。这条测试同时确保裁剪循环不会因为一条打死不退的超大条目
// 而失去终止性。
func TestHistory_OversizedSingleEntryRejected(t *testing.T) {
	dir := t.TempDir()
	h, _ := LoadHistory(filepath.Join(dir, "history.jsonl"), 500)
	h.Add("normal")
	h.Add(strings.Repeat("y", defaultHistoryBytes+1))
	items := h.Items()
	if len(items) != 1 || items[0].Text != "normal" {
		t.Fatalf("单条超过字节上限应被拒绝存入,得到 %d 条:%v", len(items), items)
	}
}

// TestHistory_ConcurrentSavesDoNotLoseEntries: 两个进程各自 Add 后 Save,后写
// 的不得整份覆盖先写的。多窗口是本仓的常规用法,今天第二个窗口一保存,第一个
// 窗口这一整场的输入历史不该消失。夹具用两个独立指向同一路径的 *History 实例
// 模拟两个进程——同一实例并发只会测到 h.mu 那把锁,证明不了跨进程的东西。
func TestHistory_ConcurrentSavesDoNotLoseEntries(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "history.jsonl")

	h1, err := LoadHistory(path, 500)
	if err != nil {
		t.Fatalf("LoadHistory h1: %v", err)
	}
	h2, err := LoadHistory(path, 500)
	if err != nil {
		t.Fatalf("LoadHistory h2: %v", err)
	}

	h1.Add("from window 1")
	h2.Add("from window 2")

	if err := h1.Save(); err != nil {
		t.Fatalf("h1.Save: %v", err)
	}
	if err := h2.Save(); err != nil {
		t.Fatalf("h2.Save: %v", err)
	}

	h3, err := LoadHistory(path, 500)
	if err != nil {
		t.Fatalf("LoadHistory h3: %v", err)
	}
	items := h3.Items()
	var got1, got2 bool
	for _, it := range items {
		if it.Text == "from window 1" {
			got1 = true
		}
		if it.Text == "from window 2" {
			got2 = true
		}
	}
	if !got1 || !got2 {
		t.Fatalf("两个窗口各自 Save 后都应保留对方的条目,得到 %v", items)
	}
}

// TestHistory_LargeAngleBracketEntrySaveDoesNotInflate 是评审复现过的回归:
// json.Encoder 默认会把 `<`/`>`/`&` 从 1 字节转义成 6 字节的 unicode 转义
// 序列。端到端走 Add -> Save -> LoadHistory,断言两件事:
// 落盘文件没有被转义撑到数倍体积(Save 已 SetEscapeHTML(false)),以及大条目
// 之后追加的历史在重新加载后仍然都在。
func TestHistory_LargeAngleBracketEntrySaveDoesNotInflate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "history.jsonl")

	h, _ := LoadHistory(path, 500)
	angleHeavy := strings.Repeat("<", 400*1024) // 转义后会变成约 2.4 MiB
	h.Add(angleHeavy)
	h.Add("after-1")
	h.Add("after-2")
	if err := h.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if got := len(raw); got > 2*len(angleHeavy) {
		t.Errorf("落盘文件 %d 字节,疑似仍在做 HTML 转义(未转义时应接近原文长度的量级),"+
			"检查 Save 里的 enc.SetEscapeHTML(false) 是否被移除", got)
	}
	// 用逐字节构造而非字符串字面量,避免转义序列本身在源码/工具链里被误读:
	// 这 6 个字节是 JSON 对小于号做 unicode 转义后的样子——反斜杠、u、0、0、3、c。
	escapeSeq := []byte{'\\', 'u', '0', '0', '3', 'c'}
	if bytes.Contains(raw, escapeSeq) {
		t.Errorf("落盘内容里出现了 HTML 转义序列,SetEscapeHTML(false) 应已关闭它")
	}

	reloaded, err := LoadHistory(path, 500)
	if err != nil {
		t.Fatalf("LoadHistory: %v", err)
	}
	texts := map[string]bool{}
	for _, it := range reloaded.Items() {
		texts[it.Text] = true
	}
	for _, want := range []string{"after-1", "after-2"} {
		if !texts[want] {
			t.Errorf("大 `<` 条目之后追加的历史应保留,缺 %q,得到 %v", want, reloaded.Items())
		}
	}
}

// TestHistory_ReadSurvivesHTMLEscapedLegacyLine 直接在磁盘上构造一行"看起来
// 像旧版本(关闭 SetEscapeHTML 之前)写下的、被 HTML 转义膨胀过"的历史,
// 跳过 Save,只测 LoadHistory/readHistoryFile 本身。这条独立于上一条:上一条
// 覆盖"新写入不再膨胀",这条覆盖"哪怕磁盘上已经有一行被膨胀过的旧数据,
// 它之后的条目也不会因为按裸字节数估的行长上限而被整体丢弃"——也就是
// readHistoryFile 改用 os.ReadFile(无单行长度上限)而不是 bufio.Scanner
// (有上限)这个修复本身。
func TestHistory_ReadSurvivesHTMLEscapedLegacyLine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "history.jsonl")

	// 模拟旧版本会写出的转义行:400 KiB 的 `<`,json.Marshal 默认转义后编码
	// 体积膨胀到约 2.4 MiB,远超旧实现按 defaultHistoryBytes+8*1024 估出的
	// scanner 缓冲区上限(~2.1 MiB)。
	escaped, err := json.Marshal(historyItem{
		Text: strings.Repeat("<", 400*1024),
		TS:   time.Now().Add(-time.Minute),
	})
	if err != nil {
		t.Fatalf("Marshal escaped line: %v", err)
	}
	after1, err := json.Marshal(historyItem{Text: "after-1", TS: time.Now()})
	if err != nil {
		t.Fatalf("Marshal after-1: %v", err)
	}
	after2, err := json.Marshal(historyItem{Text: "after-2", TS: time.Now().Add(time.Second)})
	if err != nil {
		t.Fatalf("Marshal after-2: %v", err)
	}

	var buf bytes.Buffer
	for _, line := range [][]byte{escaped, after1, after2} {
		buf.Write(line)
		buf.WriteByte('\n')
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	h, err := LoadHistory(path, 500)
	if err != nil {
		t.Fatalf("LoadHistory: %v", err)
	}
	texts := map[string]bool{}
	for _, it := range h.Items() {
		texts[it.Text] = true
	}
	for _, want := range []string{"after-1", "after-2"} {
		if !texts[want] {
			t.Errorf("被 HTML 转义膨胀过的历史行之后的条目应保留,缺 %q,得到 %v", want, h.Items())
		}
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
