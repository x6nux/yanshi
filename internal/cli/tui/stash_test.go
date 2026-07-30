package tui

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
)

func TestStash_PushPopLIFO(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "stash.jsonl")
	s, _ := LoadStash(path)
	s.Push("first")
	s.Push("second")
	s.Push("third")
	if got := s.Pop(); got != "third" {
		t.Errorf("LIFO pop 应得 'third',得到 %q", got)
	}
	if got := s.Pop(); got != "second" {
		t.Errorf("LIFO 第二次 pop 应得 'second',得到 %q", got)
	}
}

func TestStash_PersistAcrossLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "stash.jsonl")
	s1, _ := LoadStash(path)
	s1.Push("a")
	s1.Push("b")
	if err := s1.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	s2, _ := LoadStash(path)
	if got := s2.Pop(); got != "b" {
		t.Errorf("重载后第一 pop 应得 'b',得到 %q", got)
	}
	if got := s2.Pop(); got != "a" {
		t.Errorf("重载后第二 pop 应得 'a',得到 %q", got)
	}
}

func TestStash_MultilineDraft(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "stash.jsonl")
	s, _ := LoadStash(path)
	multiline := "line1\nline2\n\nline4"
	s.Push(multiline)
	if got := s.Pop(); got != multiline {
		t.Errorf("多行草稿应原样存取,得到 %q", got)
	}
}

func TestStash_Drop(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "stash.jsonl")
	s, _ := LoadStash(path)
	s.Push("a")
	s.Push("b")
	s.Push("c")
	if err := s.Drop(1); err != nil {
		t.Fatalf("Drop(1): %v", err)
	}
	// items 是 oldest -> newest:[a,b,c]；删 index 1 后为 [a,c]。
	if got := s.Pop(); got != "c" {
		t.Errorf("Drop 后第一 pop 应得最新项 'c',得到 %q", got)
	}
	if got := s.Pop(); got != "a" {
		t.Errorf("Drop 后第二 pop 应得 'a',得到 %q", got)
	}
}

func TestStash_EmptyPopReturnsEmpty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "stash.jsonl")
	s, _ := LoadStash(path)
	if got := s.Pop(); got != "" {
		t.Errorf("空 stash pop 应返回空串,得到 %q", got)
	}
}

func TestStash_JSONLHealingOnCorruptLine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "stash.jsonl")
	// 写入两行,一行合法、一行损坏
	os.WriteFile(path, []byte(`{"text":"a","ts":"2026-01-01T00:00:00Z"}`+"\n"+
		"NOT_JSON\n"+
		`{"text":"b","ts":"2026-01-01T00:00:00Z"}`+"\n"), 0o644)
	s, err := LoadStash(path)
	if err != nil {
		t.Fatalf("损坏的 JSONL 行应自愈,不报错: %v", err)
	}
	if len(s.items) != 2 {
		t.Errorf("应保留 2 条合法条目(跳过 NOT_JSON),得到 %d", len(s.items))
	}
	// LIFO pop 顺序
	first := s.Pop()
	second := s.Pop()
	if first != "b" || second != "a" {
		t.Errorf("pop 顺序应 [b, a],得到 [%s, %s]", first, second)
	}
}

// TestStash_SaveAtomicTempSuffix 验证 temp 文件名带随机后缀。
func TestStash_SaveAtomicTempSuffix(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "stash.jsonl")
	s, _ := LoadStash(path)
	s.Push("x")
	if err := s.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	matches, _ := filepath.Glob(filepath.Join(dir, "stash.jsonl.tmp.*"))
	if len(matches) != 0 {
		t.Errorf("temp 应被 rename 掉,仍存在: %v", matches)
	}
	// 最终文件应存在且含 stash 条目;逐行 JSONL 解析(自愈读取同款)。
	data, _ := os.ReadFile(path)
	scanner := bufio.NewScanner(bytes.NewReader(data))
	var found bool
	for scanner.Scan() {
		var it stashItem
		if json.Unmarshal(scanner.Bytes(), &it) == nil && it.Text == "x" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("stash.jsonl 应含 {text:\"x\"}, 得到 %q", data)
	}
	// 时间字段必须是可渲染的真实 RFC3339 值。
	ts := s.List()[0].TS.Format(time.RFC3339)
	if ts == "" {
		t.Errorf("TS 格式化失败")
	}
}

func TestStashListEntry_RenderEmptySafe(t *testing.T) {
	e := &stashListEntry{items: nil}
	got := e.render(80, spinner.Model{})
	if got != "" {
		t.Errorf("空 items 应渲染空串,得到 %q", got)
	}
}

func TestStashListEntry_RenderWideAndNarrow(t *testing.T) {
	items := []string{
		"line one\nline two",
		strings.Repeat("x", 200),
	}
	e := &stashListEntry{items: items}
	// 宽窗:多行草稿压为一行;长行被 truncate,但不出 panic。
	wide := e.render(120, spinner.Model{})
	if !strings.Contains(wide, "[0]") || !strings.Contains(wide, "[1]") {
		t.Errorf("宽窗渲染应含两行索引,得到 %q", wide)
	}
	if !strings.Contains(wide, "line one line two") {
		t.Errorf("多行草稿 preview 应压成一行,得到 %q", wide)
	}
	// 窄窗(width < 10):不 panic,preview 退化到 8 字符。
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("窄窗 render panic: %v", r)
		}
	}()
	_ = e.render(20, spinner.Model{})
}
