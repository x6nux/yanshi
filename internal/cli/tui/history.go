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
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return h, nil
		}
		return nil, err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		var it historyItem
		if err := json.Unmarshal(scanner.Bytes(), &it); err != nil {
			continue // JSONL 自愈:坏行跳过
		}
		if it.Text != "" {
			h.items = append(h.items, it)
		}
	}
	if len(h.items) > h.cap {
		h.items = h.items[len(h.items)-h.cap:]
	}
	return h, nil
}

// Add appends a prompt text to the history, deduplicating by moving existing
// entries to the tail.
func (h *History) Add(text string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	// 去重:删掉已有同文本,再追加到尾部(最新)
	out := h.items[:0]
	for _, it := range h.items {
		if it.Text != text {
			out = append(out, it)
		}
	}
	h.items = append(out, historyItem{Text: text, TS: time.Now()})
	if len(h.items) > h.cap {
		h.items = h.items[len(h.items)-h.cap:]
	}
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
func (h *History) Save() error {
	h.mu.Lock()
	items := make([]historyItem, len(h.items))
	copy(items, h.items)
	path := h.path
	h.mu.Unlock()

	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
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
	for _, it := range items {
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
