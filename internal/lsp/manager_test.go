package lsp

import (
	"bufio"
	"io"
	"net"
	"os/exec"
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
		"main.go":     "go",
		"a/b/c.py":    "python",
		"x.ts":        "typescript",
		"README.md":   "",
		"Makefile":    "",
	}
	for path, want := range cases {
		if got := detectLanguage(path); got != want {
			t.Errorf("detectLanguage(%q)=%q, want %q", path, got, want)
		}
	}
}

func TestManager_HasGoplsConfig(t *testing.T) {
	if _, err := exec.LookPath("gopls"); err != nil {
		t.Skip("gopls 未安装,跳过(软降级)")
	}
	m := New(Config{WorkRoot: t.TempDir(), Languages: DefaultLanguages()})
	if !m.Enabled() {
		t.Fatalf("装了 gopls 时 Manager 应 enabled")
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
