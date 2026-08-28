package tui

import (
	"bytes"
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
// 500 条撑到几十 MB,而这份文件每次 TUI 启动都要整份读进内存。
//
// 2 MiB 是估算值,不是从真实用户粘贴大小分布量出来的。推理:多数正常 prompt
// 是几 KB 到几十 KB;偶尔出现的大粘贴(一份 diff、一段日志)量级在几十到几百
// KB;2 MiB 大致对应"一整场会话里能装下十几条这种偏大的粘贴,但装不下一次性
// 把一个大文件整个粘进来",同时把启动读取成本钉在个位数 MB。这个数字如果与
// 真实使用不符,应该按实测调整,而不是被当成已经量过的常量。
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
// 用 os.ReadFile + bytes.Split 整段读入,而不是 bufio.Scanner 按行扫描:
// Scanner 有固定的单行 token 上限,一旦某一行超过这个上限就会返回
// bufio.ErrTooLong 并让 Scan() 直接结束,把这行之后的全部历史都静默丢掉,
// 而不是只丢这一行。这里踩过一次——json.Encoder 默认会把 Text 里的
// `<`/`>`/`&`/控制字符做 HTML 转义,1 字节可以膨胀到 6 字节,单条 Text 在
// defaultHistoryBytes 预算内也可能编码后超过按预算估出来的 Scanner 缓冲区
// 上限。Save 已经关闭了新写入的 HTML 转义(见下方 enc.SetEscapeHTML),但
// 存量文件里可能还有旧版本写入的转义行,而"某一行到底会不会超过某个缓冲区"
// 这件事没有稳妥的上界可以事先算出来,所以干脆换成没有单行长度上限的读法。
// os.ReadFile 仍然会把整份文件读进内存,但这与调用方本来就要把全部历史读进
// 内存的前提一致,不是新增的成本。
func readHistoryFile(path string) ([]historyItem, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var items []historyItem
	for _, line := range bytes.Split(data, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var it historyItem
		if err := json.Unmarshal(line, &it); err != nil {
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
	// 顺序必须是确定的,而这需要两处一起保证,少任何一处结果都随机:
	//
	//  1. 候选按 a 再 b 的**出现顺序**收集,不是遍历 byText —— Go 的 map 迭代
	//     顺序是随机的,从它建切片等于先把顺序打散再排序。
	//  2. 排序用 SliceStable —— TS 相等的条目(同一瞬间写入,或都还没有时间戳)
	//     必须保留上面那个顺序,`sort.Slice` 对它们的相对位置不作保证。
	//
	// 实测:两条零值 TS 的条目在 `-count=8` 下会翻转,而 `go test ./...` 只跑
	// 一遍,看不见。归并是 Save 用来避免多窗口互相覆盖的那一步,顺序不定
	// 意味着同一份历史每次保存都可能换个排列写回磁盘。
	out := make([]historyItem, 0, len(byText))
	emitted := make(map[string]bool, len(byText))
	take := func(items []historyItem) {
		for _, it := range items {
			if emitted[it.Text] {
				continue
			}
			emitted[it.Text] = true
			out = append(out, byText[it.Text])
		}
	}
	take(a)
	take(b)
	sort.SliceStable(out, func(i, j int) bool { return out[i].TS.Before(out[j].TS) })
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
	// 这是本地 JSONL 文件,不是要塞进 <script> 的网页内容:HTML 转义在这里
	// 没有任何安全收益,只会把 Text 里的 `<`/`>`/`&`/控制字符从 1 字节膨胀成
	// 6 字节,徒增文件体积,也是上面 readHistoryFile 注释里那次静默截断的
	// 根源之一。关掉它,新写入的历史不再受这个膨胀影响。
	enc.SetEscapeHTML(false)
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
