package lsp

import (
	"bufio"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestManager_DisabledWhenNoServer(t *testing.T) {
	m := New(Config{WorkRoot: t.TempDir(), Languages: nil})
	if m.Enabled() {
		t.Fatalf("无语言配置时 Manager 应 disabled")
	}
	m.DidChange("/tmp/x.go", "package main") // no-op,不 panic
	if diags := m.Diagnostics("/tmp/x.go", 0); diags != nil {
		t.Fatalf("disabled Manager Diagnostics 应为 nil,得到 %+v", diags)
	}
	_ = m.Close() // disabled Manager Close 也是 no-op,不 panic
}

func TestDetectLanguage_ByExtension(t *testing.T) {
	cases := map[string]string{
		"main.go":   "go",
		"a/b/c.py":  "python",
		"x.ts":      "typescript",
		"README.md": "",
		"Makefile":  "",
	}
	for path, want := range cases {
		if got := detectLanguage(path); got != want {
			t.Errorf("detectLanguage(%q)=%q, want %q", path, got, want)
		}
	}
}

// TestManager_HasGoplsConfig 断言的语义在 W6 收紧了一次:从"装了 gopls 就
// enabled"变成"装了 gopls **且这确实是一个 Go 工作区**才 enabled"。
//
// 旧版把工作区设成空的 t.TempDir() 却期望 enabled——那正是标志文件闸门要挡的
// 情形:在没有 go.mod 的目录里,gopls 对每个请求都报错,于是这个 Manager 会
// 一直持有一个永远失败的子进程。测试原本钉住的是那个行为本身。
func TestManager_HasGoplsConfig(t *testing.T) {
	if _, err := exec.LookPath("gopls"); err != nil {
		t.Skip("gopls 未安装,跳过(软降级)")
	}
	root := t.TempDir()
	if m := New(Config{WorkRoot: root, Languages: DefaultLanguages()}); m.Enabled() {
		t.Fatal("没有 go.mod 的目录不该拉起 gopls")
	}
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := New(Config{WorkRoot: root, Languages: DefaultLanguages()})
	if !m.Enabled() {
		t.Fatalf("装了 gopls 且有 go.mod 时 Manager 应 enabled")
	}
	_ = m.Close()
}

// TestPathToURL_Escape 验证 net/url 对空格/非 ASCII 的 escape(跨平台:无论
// 前导路径如何解析,escape 都生效)。Windows 盘符 URL 形状由 build 标签保护
// 的 TestPathToURL_WindowsDrive(manager_windows_test.go)覆盖。
func TestPathToURL_Escape(t *testing.T) {
	got := pathToURL("/tmp/my proj/中文.go")
	if !strings.Contains(got, "my%20proj") {
		t.Errorf("space escape 失败: %q", got)
	}
	if !strings.Contains(got, "%E4%B8%AD%E6%96%87.go") {
		t.Errorf("中文 escape 失败: %q", got)
	}
}

func TestPathToURL_Posix(t *testing.T) {
	got := pathToURL("/home/x/main.go")
	// On Windows, filepath.Abs prepends the current drive (e.g., D:), so
	// the result includes a drive letter like "file:///D:/home/x/main.go".
	// On Unix it stays as "file:///home/x/main.go".
	if !strings.HasPrefix(got, "file:///") || !strings.HasSuffix(got, "/home/x/main.go") {
		t.Errorf("posix 路径结果异常: %q", got)
	}
}

// TestManager_DialSeam_ConnectsFakeServer 用 Dial 测试缝注入 net.Pipe fake server,
// 验证 Manager.clientFor → Client.Start → initialize → notifyChange → Diagnostics 的
// 真实代码路径(评审 #20 的 Manager 层 E2E,无需 gopls)。
func TestManager_DialSeam_ConnectsFakeServer(t *testing.T) {
	srv, cli := net.Pipe()
	go func() {
		defer srv.Close()
		runFakeGopls(t, srv)
	}()
	m := New(Config{
		WorkRoot:  t.TempDir(),
		Languages: map[string]LanguageServer{"go": {Command: "fake"}},
		Timeout:   2 * time.Second,
		Dial: func(lang string) (io.Reader, io.Writer, func() error, error) {
			return cli, cli, func() error { return cli.Close() }, nil
		},
	})
	if !m.Enabled() {
		t.Fatalf("Dial 非空时 Languages 不经 LookPath 剪枝,应 enabled")
	}
	m.DidChange("main.go", "package main\nvar x = y\n")
	diags := m.Diagnostics("main.go", time.Second)
	if len(diags) != 1 || diags[0].Message != "undefined: y" {
		t.Fatalf("期望 1 条 undefined: y,得到 %+v", diags)
	}
	_ = m.Close()
}

// runFakeGopls 在 srv 端运行假 gopls:initialize 回 capabilities,
// didOpen/didChange 推一条 "undefined: y" 诊断。
func runFakeGopls(t *testing.T, srv net.Conn) {
	t.Helper()
	br := bufio.NewReader(srv)
	for {
		msg, err := ReadMessage(br)
		if err != nil {
			return
		}
		switch msg["method"] {
		case "initialize":
			_ = WriteMessage(srv, map[string]any{
				"jsonrpc": "2.0", "id": msg["id"],
				"result": map[string]any{"capabilities": map[string]any{}},
			})
		case "textDocument/didOpen", "textDocument/didChange":
			params, _ := msg["params"].(map[string]any)
			td, _ := params["textDocument"].(map[string]any)
			uri, _ := td["uri"].(string)
			_ = WriteMessage(srv, map[string]any{
				"jsonrpc": "2.0", "method": "textDocument/publishDiagnostics",
				"params": map[string]any{
					"uri": uri,
					"diagnostics": []map[string]any{{
						"range":    map[string]any{"start": map[string]any{"line": 2, "character": 4}},
						"severity": 1,
						"message":  "undefined: y",
						"source":   "gopls",
					}},
				},
			})
		}
	}
}

// TestOpenDocumentsTracksDidChange pins the source that made
// open_diagnostics_count meaningful.
//
// The diagnostics tool asks the manager which files to query. That list used
// to come from a stub returning nil, so the count was structurally 0: the
// field said "no problems" while never having looked at anything. DidChange
// already sees every file the agent edits, which is precisely the LSP notion
// of an open document -- this process has no editor, so "open" can only mean
// "notified the server about".
func TestOpenDocumentsTracksDidChange(t *testing.T) {
	m := &Manager{enabled: true, clients: map[string]*Client{}, cmds: map[string]*exec.Cmd{}}

	if got := m.OpenDocuments(); len(got) != 0 {
		t.Fatalf("a fresh manager has no open documents, got %v", got)
	}

	m.rememberOpen("/w/a.go")
	m.rememberOpen("/w/b.go")
	m.rememberOpen("/w/a.go") // touched again

	got := m.OpenDocuments()
	if len(got) != 2 {
		t.Fatalf("re-touching a file must not duplicate it: %v", got)
	}
	if got[0] != "/w/a.go" {
		t.Fatalf("most recent must come first, got %v", got)
	}

	// The cap exists because diagnostics does one LSP round trip per file on
	// a shared budget: an unbounded list makes the tool slower the longer the
	// session runs.
	for i := 0; i < openDocsLimit*2; i++ {
		m.rememberOpen("/w/f" + strconv.Itoa(i) + ".go")
	}
	if got := m.OpenDocuments(); len(got) > openDocsLimit {
		t.Fatalf("open document list is unbounded: %d entries", len(got))
	}

	// A disabled manager reports nothing rather than a stale list.
	m.enabled = false
	if got := m.OpenDocuments(); got != nil {
		t.Fatalf("disabled manager returned %v", got)
	}
}

// TestEveryDetectedLanguageHasAServer pins the two tables against each other.
//
// detectLanguage recognised 12 extensions across 6 languages while
// DefaultLanguages held servers for 2. Editing a .ts, .rs or .c file therefore
// resolved a language, found no server, and DidChange silently did nothing --
// no error anywhere, diagnostics permanently empty, indistinguishable from
// "this file is fine". A table that promises more than its partner delivers is
// the failure mode here, so the assertion is equality in both directions.
func TestEveryDetectedLanguageHasAServer(t *testing.T) {
	servers := DefaultLanguages()
	// One representative extension per language detectLanguage knows.
	for _, ext := range []string{".go", ".py", ".ts", ".tsx", ".js", ".jsx", ".c", ".cc", ".cpp", ".h", ".hpp", ".rs"} {
		lang := detectLanguage("x" + ext)
		if lang == "" {
			t.Fatalf("%s no longer resolves to a language", ext)
		}
		if _, ok := servers[lang]; !ok {
			t.Errorf("%s resolves to language %q, which has no server: DidChange will "+
				"silently do nothing for every such file", ext, lang)
		}
	}
	// And nothing in the server table that detectLanguage cannot produce: a
	// server keyed on a language no file ever maps to can never be spawned.
	reachable := map[string]bool{}
	for _, ext := range []string{".go", ".py", ".ts", ".js", ".c", ".rs"} {
		reachable[detectLanguage("x"+ext)] = true
	}
	for lang := range servers {
		if !reachable[lang] {
			t.Errorf("server for %q is unreachable: no extension maps to it", lang)
		}
	}
}

// TestMarkerFilesGateSpawning covers the confirmation half. Without it a
// directory holding a few .py scripts also starts gopls, which then errors on
// every request in a workspace with no go.mod -- a permanently failing
// subprocess occupying a client slot.
func TestMarkerFilesGateSpawning(t *testing.T) {
	root := t.TempDir()
	langs := map[string]LanguageServer{
		"go":     {Command: "sh", Markers: []string{"go.mod"}},
		"python": {Command: "sh"}, // no markers: always eligible
	}
	m := New(Config{WorkRoot: root, Languages: langs})
	if _, ok := m.cfg.Languages["go"]; ok {
		t.Error("go server kept in a workspace with no go.mod")
	}
	if _, ok := m.cfg.Languages["python"]; !ok {
		t.Error("a server with no markers must stay eligible")
	}

	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m2 := New(Config{WorkRoot: root, Languages: langs})
	if _, ok := m2.cfg.Languages["go"]; !ok {
		t.Error("go server dropped despite go.mod being present")
	}
}
