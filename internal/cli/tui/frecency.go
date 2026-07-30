package tui

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// Frecency 记录文件路径的"频率 × 最近性"得分,提供给后续 UX 批次排序使用。
// 设计 mozilla frecency 公式简化版:score = count * decay(now - lastSeen)。
//
// 所有公共方法都在 mu 锁内——review 发现原计划自称有 mutex 但实际没加,这里显式补。
// Save 用 atomic-rename:写 <path>.tmp.<rand> 然后 os.Rename 到 <path>;random 后缀
// 防多 worker(虽然本批 saveQueue 单 worker,但留防护纵深)。
type Frecency struct {
	mu      sync.Mutex
	path    string
	entries []frecencyEntry
}

type frecencyEntry struct {
	Path      string    `json:"path"`
	Count     int       `json:"count"`
	FirstSeen time.Time `json:"first_seen"`
	LastSeen  time.Time `json:"last_seen"`
}

// LoadFrecency 从 path 载入;不存在则返回空 Frecency(path 仍保存以备 Save)。
// JSON 解析失败(文件损坏)软降级为空——与 yanshi 一贯的"自愈 JSONL"模式一致。
func LoadFrecency(path string) (*Frecency, error) {
	f := &Frecency{path: path}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return f, nil
		}
		return nil, err
	}
	// 自愈:损坏的 JSON 不阻断启动,记录空。
	var entries []frecencyEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		return f, nil
	}
	f.entries = entries
	return f, nil
}

// Record 追加或更新一条访问记录。每次调用 Count++ 并刷新 LastSeen。
// 显式实现:逐条查找,未命中时追加一条 frecencyEntry。
func (f *Frecency) Record(path string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := time.Now()
	for i := range f.entries {
		if f.entries[i].Path == path {
			f.entries[i].Count++
			f.entries[i].LastSeen = now
			return nil
		}
	}
	f.entries = append(f.entries, frecencyEntry{
		Path:      path,
		Count:     1,
		FirstSeen: now,
		LastSeen:  now,
	})
	return nil
}

// TopN 返回得分前 n 的路径,按得分降序。
func (f *Frecency) TopN(n int) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	type scored struct {
		path      string
		score     float64
		lastSeen  time.Time
		firstSeen time.Time
	}
	now := time.Now()
	scoredList := make([]scored, len(f.entries))
	for i, e := range f.entries {
		scoredList[i] = scored{
			path:      e.Path,
			score:     e.score(now),
			lastSeen:  e.LastSeen,
			firstSeen: e.FirstSeen,
		}
	}
	sort.Slice(scoredList, func(i, j int) bool {
		if scoredList[i].score != scoredList[j].score {
			return scoredList[i].score > scoredList[j].score
		}
		// Tiebreaker: equal frecency scores (e.g. count=10×0.1 vs count=1×1.0)
		// prefer the more recently seen entry, then the earlier first-seen
		// (longer-tracked path) as a final stable deterministic order.
		if !scoredList[i].lastSeen.Equal(scoredList[j].lastSeen) {
			return scoredList[i].lastSeen.After(scoredList[j].lastSeen)
		}
		return scoredList[i].firstSeen.Before(scoredList[j].firstSeen)
	})
	if n > len(scoredList) {
		n = len(scoredList)
	}
	out := make([]string, n)
	for i := 0; i < n; i++ {
		out[i] = scoredList[i].path
	}
	return out
}

// Save 原子写入:tmp 文件带随机后缀,再 rename 到最终路径。
func (f *Frecency) Save() error {
	f.mu.Lock()
	entries := make([]frecencyEntry, len(f.entries))
	copy(entries, f.entries)
	path := f.path
	f.mu.Unlock()

	data, err := json.Marshal(entries)
	if err != nil {
		return err
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	// random 后缀:6 字节 hex(12 字符),防多 worker rename 冲突。
	randBytes := make([]byte, 6)
	if _, err := rand.Read(randBytes); err != nil {
		return err
	}
	tmp := path + ".tmp." + hex.EncodeToString(randBytes)
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// score 实现 frecency 公式:count * decay(now - lastSeen)。
// decay: <1h → 1.0;<1d → 0.9;<7d → 0.5;>=7d → 0.1。
func (e frecencyEntry) score(now time.Time) float64 {
	delta := now.Sub(e.LastSeen)
	decay := 0.1
	switch {
	case delta < time.Hour:
		decay = 1.0
	case delta < 24*time.Hour:
		decay = 0.9
	case delta < 7*24*time.Hour:
		decay = 0.5
	}
	return float64(e.Count) * decay
}

// frecencyPath 返回 frecency 持久化文件路径,与 permModeFile 同一目录。
func frecencyPath(root string) string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "yanshi", "frecency.json")
}

// saveCmd 包装一个异步 save 任务。所有 JSONL 持久化(Frecency/Stash/History)
// 都以 saveCmd 形式推入 model.saveQueue,由单 worker(waitSave listener)
// 串行消费,保证旧快照不会覆盖新状态。
type saveCmd struct {
	fn func() error
}

// waitSave 是 saveQueue 的 listener:阻塞读一个 saveCmd 并以 tea.Msg 形式
// 投递给 Update。必须在 Init/startup 的 tea.Batch 中挂入一次(只挂一次),
// Update 的 case saveCmd 分支在执行 msg.fn() 后无条件 re-arm waitSave,
// 形成 "consumer re-arm" 链,保证队列始终有读者。
func waitSave(queue <-chan saveCmd) tea.Cmd {
	if queue == nil {
		return nil
	}
	return func() tea.Msg {
		c, ok := <-queue
		if !ok {
			return nil // channel 关闭:返回 nil Msg(bubbletea 会忽略)
		}
		return c
	}
}

// enqueueSave 非阻塞地把 save 任务推入 model.saveQueue。由 applyEvent
// (tool_result)/Update(Ctrl+S/cmdStash/dispatchSend)调用。真正的 I/O 由
// waitSave listener → Update case saveCmd 串行执行。队列满时丢弃(下次
// 写操作会再入队)——frecency/stash/history 都不需要"每次都精确持久化"。
func (m *model) enqueueSave(fn func() error) {
	if m.saveQueue == nil {
		return
	}
	select {
	case m.saveQueue <- saveCmd{fn: fn}:
	default:
	}
}

// extractPathFromToolArgs 从 fs_write/fs_edit/fs_mkdir 的 args JSON 中解析 path 字段。
// 其他工具或解析失败返回空串。不引第三方 JSON 库:手工 strings.Index 找 "path" 键,
// 失败就返回 ""(frecency 不需要精确)。
func extractPathFromToolArgs(toolName, argsJSON string) string {
	switch toolName {
	case "fs_write", "fs_edit", "fs_mkdir":
		// argsJSON 例如 {"path":"/proj/main.go"}
		// 简化解析:找 "path" 键,取其字符串值。
		idx := strings.Index(argsJSON, `"path"`)
		if idx < 0 {
			return ""
		}
		rest := argsJSON[idx+len(`"path"`):]
		colon := strings.IndexByte(rest, ':')
		if colon < 0 {
			return ""
		}
		rest = rest[colon+1:]
		// 跳过空白与开引号
		for len(rest) > 0 && (rest[0] == ' ' || rest[0] == '\t' || rest[0] == '"') {
			rest = rest[1:]
		}
		// 截到下一个引号或逗号
		end := strings.IndexAny(rest, `",}`)
		if end < 0 {
			return strings.TrimSpace(rest)
		}
		return rest[:end]
	}
	return ""
}

// recordToolFrecency 把"记录 + 首次即安排持久化"封装成一个调用点。
// applyEvent 只调用它,测试可直接锁定第一次 Record 已入 saveQueue。
func (m *model) recordToolFrecency(toolName, argsJSON string) {
	path := extractPathFromToolArgs(toolName, argsJSON)
	if path == "" || m.frecency == nil {
		return
	}
	if err := m.frecency.Record(path); err != nil {
		return
	}
	m.enqueueSave(m.frecency.Save)
}
