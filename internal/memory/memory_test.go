package memory

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoad_EmptyWhenMissing(t *testing.T) {
	dir := t.TempDir()
	got := Load(filepath.Join(dir, "absent.md"), "", 0)
	if got != "" {
		t.Fatalf("缺失文件应返回空串,得到 %q", got)
	}
}

func TestLoad_OnlyUser(t *testing.T) {
	dir := t.TempDir()
	um := filepath.Join(dir, "user.md")
	os.WriteFile(um, []byte("prefer pytest\n"), 0o644)
	got := Load(um, "", 0)
	if !strings.Contains(got, "prefer pytest") {
		t.Fatalf("应返回 user 内容,得到 %q", got)
	}
}

func TestLoad_UserAndProject(t *testing.T) {
	dir := t.TempDir()
	um := filepath.Join(dir, "user.md")
	pm := filepath.Join(dir, "proj.md")
	os.WriteFile(um, []byte("USER\n"), 0o644)
	os.WriteFile(pm, []byte("PROJ\n"), 0o644)
	got := Load(um, pm, 0)
	if !strings.Contains(got, "USER") || !strings.Contains(got, "PROJ") {
		t.Fatalf("应同时包含两源,得到 %q", got)
	}
	if strings.Index(got, "USER") > strings.Index(got, "PROJ") {
		t.Fatalf("User 应排在 Project 之前,得到 %q", got)
	}
}

func TestLoad_TruncatesOversize(t *testing.T) {
	dir := t.TempDir()
	um := filepath.Join(dir, "u.md")
	os.WriteFile(um, []byte(strings.Repeat("x", 1000)), 0o644)
	got := Load(um, "", 500) // max=500 字节
	if !strings.Contains(got, "truncated") {
		t.Fatalf("超 max 应截断并打标,得到(前80字符): %q", got[:80])
	}
}

func TestSystemBlock_WrapsAndTruncates(t *testing.T) {
	big := strings.Repeat("x", 1000)
	block := SystemBlock(big, "/tmp/m.md", 500)
	if !strings.Contains(block, "<user_memory") || !strings.Contains(block, "truncated") {
		t.Fatalf("超限应截断并打标,得到 %q(前80字符: %q)", block, block[:80])
	}
}

func TestSystemBlock_EmptyReturnsEmpty(t *testing.T) {
	if got := SystemBlock("   \n  ", "/tmp/m.md", 0); got != "" {
		t.Fatalf("空内容应返回空串,得到 %q", got)
	}
}

func TestComposeBlock_DisabledByToggle(t *testing.T) {
	dir := t.TempDir()
	um := filepath.Join(dir, "u.md")
	os.WriteFile(um, []byte("hi\n"), 0o644)
	if got := ComposeBlock(false, um, "", 0); got != "" {
		t.Fatalf("enabled=false 应返回空串,得到 %q", got)
	}
	if got := ComposeBlock(true, um, "", 0); got == "" {
		t.Fatalf("enabled=true + 有内容应返回非空")
	}
}

func TestAppend_CreatesAndTimestamps(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "mem.md")
	if err := Append(p, "# remember the milk"); err != nil {
		t.Fatalf("Append: %v", err)
	}
	body, _ := os.ReadFile(p)
	if !strings.Contains(string(body), "remember the milk") {
		t.Errorf("正文应含条目: %q", body)
	}
	if !strings.HasPrefix(string(body), "- (") {
		t.Errorf("应以 '- (' 开头(时间戳 bullet): %q", body)
	}
}

func TestAppend_StripsHashPrefix(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "mem.md")
	Append(p, "### only text")
	body, _ := os.ReadFile(p)
	if strings.Contains(string(body), "###") {
		t.Errorf("'#' 前缀应被剥离: %q", body)
	}
}

func TestAppend_RejectsEmpty(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "mem.md")
	if err := Append(p, "###"); err == nil {
		t.Fatalf("剥后为空应报错")
	}
}
