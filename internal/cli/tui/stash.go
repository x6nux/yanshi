package tui

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
)

// Stash 是多行草稿的 LIFO 栈,JSONL 持久化。
// 单 worker save:所有写操作经 saveQueue 串行,防旧快照覆盖新状态。
type Stash struct {
	mu    sync.Mutex
	path  string
	items []stashItem
}

type stashItem struct {
	Text string    `json:"text"`
	TS   time.Time `json:"ts"`
}

// LoadStash loads a stash from a JSONL file, creating an empty stash if the
// file does not exist. Corrupted lines are skipped (self-healing).
func LoadStash(path string) (*Stash, error) {
	s := &Stash{path: path}
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024) // 允许长行
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var it stashItem
		if err := json.Unmarshal(line, &it); err != nil {
			continue // 自愈:跳过损坏行
		}
		s.items = append(s.items, it)
	}
	return s, nil
}

// Push appends a draft text to the stash stack.
func (s *Stash) Push(text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.items = append(s.items, stashItem{Text: text, TS: time.Now()})
}

// Pop removes and returns the most recent draft from the stash stack. Returns
// "" when the stash is empty.
func (s *Stash) Pop() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.items) == 0 {
		return ""
	}
	last := s.items[len(s.items)-1]
	s.items = s.items[:len(s.items)-1]
	return last.Text
}

// Drop removes the draft at the given index.
func (s *Stash) Drop(index int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if index < 0 || index >= len(s.items) {
		return fmt.Errorf("stash: index out of range: %d", index)
	}
	s.items = append(s.items[:index], s.items[index+1:]...)
	return nil
}

// List returns a copy of all stash items (oldest first).
func (s *Stash) List() []stashItem {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]stashItem, len(s.items))
	copy(out, s.items)
	return out
}

// Save 原子写入(复用 frecency 的 random-suffix 模式)。
func (s *Stash) Save() error {
	s.mu.Lock()
	items := make([]stashItem, len(s.items))
	copy(items, s.items)
	path := s.path
	s.mu.Unlock()

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

// stashPath 返回 stash 持久化文件路径,与 permModeFile / frecencyPath 同一目录。
func stashPath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "yanshi", "stash.jsonl")
}

// stashListEntry 渲染 /stash list 的结果:每条草稿一行"[idx] preview"。
// items 固定为 []string；cmdStash 从 Stash.List() 提取 Text 后构造。
// preview 用 truncateToast(对齐 width-safe 行为);长草稿折叠到一行(与
// queuePreview 一致)。
type stashListEntry struct{ items []string }

// render 遵循现有 entry 的 (width int, sp spinner.Model) string 契约。
func (e *stashListEntry) render(width int, _ spinner.Model) string {
	if len(e.items) == 0 {
		return "" // 不应发生(list 空时 cmdStash 走 toast 分支),防御性返回空
	}
	// 计算行宽预算:每行格式 "[idx] preview"
	limit := width - 10 // index、空格和边距预留
	if limit < 8 {
		limit = 8 // 极窄终端下至少保留 8 字符 preview
	}
	var b strings.Builder
	b.WriteString(stashHeaderStyle.Render("Stash (most recent last):") + "\n")
	for i, text := range e.items {
		oneLine := strings.ReplaceAll(strings.TrimSpace(text), "\n", " ")
		preview := truncateToast(oneLine, limit)
		line := fmt.Sprintf("  [%d] %s", i, preview)
		b.WriteString(stashItemStyle.Render(line) + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// cmdStash 实现 /stash:
//
//	/stash              pop 最近一条草稿到 textarea
//	/stash list         列出全部草稿(每条一行 preview)
//	/stash drop <N>     删除第 N 条(0-索引, oldest=0)
//
// 写操作经 saveQueue 串行化;每条分支返回 toast Cmd 反馈给用户。
func cmdStash(m model, args []string) (tea.Model, tea.Cmd) {
	switch {
	case len(args) > 0 && args[0] == "list":
		stashItems := m.stash.List()
		if len(stashItems) == 0 {
			return m, m.pushToast("info", "stash is empty")
		}
		items := make([]string, len(stashItems))
		for i, it := range stashItems {
			items[i] = it.Text
		}
		m.entries = append(m.entries, &stashListEntry{items: items})
		m.refresh()
		m.viewport.GotoBottom()
		return m, nil
	case len(args) >= 2 && args[0] == "drop":
		n, err := strconv.Atoi(args[1])
		if err != nil {
			return m, m.pushToast("error", "usage: /stash drop N")
		}
		if err := m.stash.Drop(n); err != nil {
			return m, m.pushToast("error", err.Error())
		}
		m.enqueueSave(m.stash.Save)
		return m, m.pushToast("info", fmt.Sprintf("dropped stash[%d]", n))
	default: // 无参或未知 → pop 最近一条到 textarea
		text := m.stash.Pop()
		if text == "" {
			return m, m.pushToast("warn", "stash is empty")
		}
		m.input.SetValue(text)
		m.growInput()
		m.enqueueSave(m.stash.Save)
		return m, m.pushToast("info", "draft restored")
	}
}
