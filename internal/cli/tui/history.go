package tui

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const defaultHistoryCap = 500

// defaultHistoryBytes 是历史文件的总字节上限(仅计 Text,不计 JSON 换行/字段
// 名开销)。条数上限本身管不住体积——几条几十 KB 的粘贴 prompt 就能把
// 500 条撑到几十 MB,而这份文件每次 TUI 启动都要整份读进内存。2 MiB 大约是
// "一整场会话里正常量级的粘贴(diff、日志片段)都装得下,但装不下一次性把
// 一个大文件整个粘进来"的量级,足够把启动读取成本钉在个位数 MB。
const defaultHistoryBytes = 2 * 1024 * 1024

// History 是已真正发送 prompt 的有界 FIFO;重复项移动到尾部(最新)。
// JSONL 持久化沿用 Stash 的自愈读取 + random-suffix atomic rename。
type History struct {
	mu    sync.Mutex
	path  string
	cap   int
	items []historyItem // oldest -> newest
}

type historyItem struct {
	Text string    `json:"text"`
	TS   time.Time `json:"ts"`
}

type historyState struct {
	query   string
	cursor  int
	items   []historyItem
	visible bool
}

// LoadHistory loads history from a JSONL file, creating an empty history if the
// file does not exist.
func LoadHistory(path string, capacity int) (*History, error) {
	if capacity <= 0 {
		capacity = defaultHistoryCap
	}
	h := &History{path: path, cap: capacity}
	items, err := readHistoryFile(path)
	if err != nil {
		return nil, err
	}
	h.items = trimHistoryItems(items, h.cap)
	return h, nil
}

// readHistoryFile 读取一份 JSONL 历史文件,坏行跳过(自愈),单条 Text 超过
// defaultHistoryBytes 的行也跳过(拒绝存储,理由见 History.Add)。文件不存在
// 时返回空切片、无错误。返回顺序是文件里的顺序(oldest -> newest)。
//
// scanner 的 token 上限必须大于 defaultHistoryBytes 才能读到一整行去判断它
// 是否超限——否则 bufio.Scanner 会在这行上报 ErrTooLong 并让 Scan() 直接
// 返回 false,把这行之后的全部历史都静默丢掉,而不是只丢这一行。
func readHistoryFile(path string) ([]historyItem, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), defaultHistoryBytes+8*1024)
	var items []historyItem
	for scanner.Scan() {
		var it historyItem
		if err := json.Unmarshal(scanner.Bytes(), &it); err != nil {
			continue // JSONL 自愈:坏行跳过
		}
		if it.Text == "" || len(it.Text) > defaultHistoryBytes {
			continue
		}
		items = append(items, it)
	}
	return items, nil
}

// trimHistoryItems 把 items(必须按 TS 升序,oldest -> newest)裁到条数
// ≤ capacity 且总字节(仅计 Text)≤ defaultHistoryBytes,两个上限都从最旧端丢。
//
// 单条 Text 本身超过 defaultHistoryBytes 的情况在 readHistoryFile 与
// History.Add 处就已经被拒绝写入,所以传入这里的每一条都 ≤
// defaultHistoryBytes——按字节裁剪那个循环每丢一条就让总字节数严格变小,
// 因此必然终止,不会出现"丢到只剩一条、这一条自己就超限、永远退不出"的情形。
func trimHistoryItems(items []historyItem, capacity int) []historyItem {
	if capacity > 0 && len(items) > capacity {
		items = items[len(items)-capacity:]
	}
	for historyItemsBytes(items) > defaultHistoryBytes && len(items) > 0 {
		items = items[1:]
	}
	return items
}

// historyItemsBytes 是 items 里所有 Text 字段的字节数之和(不计 JSON 框架
// 开销——那部分是每条几十字节的常量,不值得单独核算)。
func historyItemsBytes(items []historyItem) int {
	n := 0
	for _, it := range items {
		n += len(it.Text)
	}
	return n
}

// mergeHistoryItems 按 Text 去重合并两组历史条目,冲突时保留 TS 较新的一份
// ——与 Add 现有的"重复项移到尾部(最新)"语义一致。返回按 TS 升序排列
// (oldest -> newest),供 Save 与磁盘上的当前内容归并、避免多进程互相覆盖。
func mergeHistoryItems(a, b []historyItem) []historyItem {
	byText := make(map[string]historyItem, len(a)+len(b))
	for _, it := range a {
		byText[it.Text] = it
	}
	for _, it := range b {
		if existing, ok := byText[it.Text]; !ok || it.TS.After(existing.TS) {
			byText[it.Text] = it
		}
	}
	out := make([]historyItem, 0, len(byText))
	for _, it := range byText {
		out = append(out, it)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TS.Before(out[j].TS) })
	return out
}

// Add appends a prompt text to the history, deduplicating by moving existing
// entries to the tail.
func (h *History) Add(text string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(text) > defaultHistoryBytes {
		// 单条超过整条字节预算:拒绝存入历史,而不是截断。prompt 本身已经
		// 正常发送(Add 只在 dispatchSend 之后调用),拒绝存入只是让它不出现
		// 在历史召回(↑ / Alt+R 搜索)里——截断则会让 Enter 恢复出一份看起来
		// 完整、实际被砍掉尾部的假内容,比"搜不到"更容易造成误用。
		return
	}
	// 去重:删掉已有同文本,再追加到尾部(最新)
	out := h.items[:0]
	for _, it := range h.items {
		if it.Text != text {
			out = append(out, it)
		}
	}
	h.items = append(out, historyItem{Text: text, TS: time.Now()})
	h.items = trimHistoryItems(h.items, h.cap)
}

// Items returns a copy of all history entries (newest last).
func (h *History) Items() []historyItem {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]historyItem, len(h.items))
	copy(out, h.items)
	return out
}

// Search fuzzy 过滤并按 score 降序;同分时 TS 新的优先。
func (h *History) Search(query string, limit int) []historyItem {
	items := h.Items()
	type scored struct {
		item  historyItem
		score float64
	}
	var list []scored
	for _, it := range items {
		s := fuzzyScore(query, it.Text)
		if s > 0 {
			list = append(list, scored{item: it, score: s})
		}
	}
	sort.SliceStable(list, func(i, j int) bool {
		if list[i].score == list[j].score {
			return list[i].item.TS.After(list[j].item.TS)
		}
		return list[i].score > list[j].score
	})
	if limit <= 0 || limit > len(list) {
		limit = len(list)
	}
	out := make([]historyItem, limit)
	for i := 0; i < limit; i++ {
		out[i] = list[i].item
	}
	return out
}

// Save 与 Stash.Save 同构:复制锁内快照,锁外编码;tmp 带随机后缀再 rename。
//
// 多窗口是本仓的常规用法(见 CLAUDE.md 后端发现一节),而每个窗口的 *History
// 都是独立的进程内实例。此前 Save 是"整份内存快照覆盖整个文件",后写的窗口
// 会把先写窗口那一整场的输入历史抹掉。现在改成"先读回磁盘上的当前内容,
// 与内存快照按 Text 去重合并(TS 新的赢),再整份写回"——把"丢一整场"降级
// 成"极窄窗口里丢一条"。
//
// 这不是完全的并发安全:两个进程仍可能在"本进程读盘"与"本进程 rename 落盘"
// 之间交错——进程 A 读盘之后、rename 之前,进程 B 也读盘(读到的还是 A rename
// 之前的旧内容)并 rename,A 随后再 rename 会覆盖掉 B 刚合并写入的那条。窗口
// 极窄(两次磁盘 IO 之间),但确实存在。不要为了补这最后一点用文件锁——跨
// 平台文件锁在本仓要新写一套(`internal/lockfile` 是给后端选举用的,语义不同
// 且没有导出通用文件锁 API),代价远大于它买到的东西。
func (h *History) Save() error {
	h.mu.Lock()
	items := make([]historyItem, len(h.items))
	copy(items, h.items)
	path := h.path
	capacity := h.cap
	h.mu.Unlock()

	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}

	disk, err := readHistoryFile(path)
	if err != nil {
		return err
	}
	merged := trimHistoryItems(mergeHistoryItems(disk, items), capacity)

	randBytes := make([]byte, 6)
	if _, err := rand.Read(randBytes); err != nil {
		return err
	}
	tmp := path + ".tmp." + hex.EncodeToString(randBytes)
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(f)
	for _, it := range merged {
		if err := enc.Encode(it); err != nil {
			f.Close()
			os.Remove(tmp)
			return err
		}
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

// refreshHistorySearch 重算当前 query 的最多 50 条结果并夹紧 cursor。
func (m *model) refreshHistorySearch() {
	if m.historySearch == nil || m.history == nil {
		return
	}
	m.historySearch.items = m.history.Search(m.historySearch.query, 50)
	if len(m.historySearch.items) == 0 {
		m.historySearch.cursor = 0
		return
	}
	if m.historySearch.cursor >= len(m.historySearch.items) {
		m.historySearch.cursor = len(m.historySearch.items) - 1
	}
}

// historyPopup 渲染最近优先的搜索结果,最多显示 8 行。
func (m model) historyPopup() string {
	if m.historySearch == nil || !m.historySearch.visible {
		return ""
	}
	var b strings.Builder
	b.WriteString(paletteStyle.Render("History — search: "+m.historySearch.query) + "\n")
	if len(m.historySearch.items) == 0 {
		b.WriteString(toolMeta.Render("  no matching prompts") + "\n")
	}
	start := 0
	const maxRows = 8
	if m.historySearch.cursor >= maxRows {
		start = m.historySearch.cursor - maxRows + 1
	}
	end := start + maxRows
	if end > len(m.historySearch.items) {
		end = len(m.historySearch.items)
	}
	for i := start; i < end; i++ {
		preview := strings.ReplaceAll(strings.TrimSpace(m.historySearch.items[i].Text), "\n", " ")
		preview = truncateToast(preview, max(8, m.width-12))
		line := fmt.Sprintf("  %s", preview)
		if i == m.historySearch.cursor {
			line = selPaletteStyle.Render("▶ " + strings.TrimLeft(line, " "))
		} else {
			line = paletteStyle.Render(line)
		}
		b.WriteString(line + "\n")
	}
	return inputBorder.Render(strings.TrimRight(b.String(), "\n"))
}

func historyPath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "yanshi", "history.jsonl")
}
