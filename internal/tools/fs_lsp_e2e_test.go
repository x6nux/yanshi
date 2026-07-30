package tools

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/x6nux/yanshi/internal/guard"
	"github.com/x6nux/yanshi/internal/lsp"
)

// TestE2E_FSEdit_FakeLSPServer 是评审 #20 的完整 E2E:
// net.Pipe fake gopls → lsp.Manager(Dial seam)→ WithLSP → fs_edit → JSON
// "diagnostics" 字段含 fake server 推送的消息。证明"编辑 → 诊断回喂"整链成立。
func TestE2E_FSEdit_FakeLSPServer(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "main.go")
	require.NoError(t, os.WriteFile(target, []byte("package main\n"), 0o644))

	srv, cli := net.Pipe()
	go func() {
		defer srv.Close()
		br := bufio.NewReader(srv)
		for {
			msg, err := lsp.ReadMessage(br)
			if err != nil {
				return
			}
			switch msg["method"] {
			case "initialize":
				_ = lsp.WriteMessage(srv, map[string]any{
					"jsonrpc": "2.0", "id": msg["id"],
					"result": map[string]any{"capabilities": map[string]any{}},
				})
			case "textDocument/didOpen", "textDocument/didChange":
				params, _ := msg["params"].(map[string]any)
				td, _ := params["textDocument"].(map[string]any)
				uri, _ := td["uri"].(string)
				_ = lsp.WriteMessage(srv, map[string]any{
					"jsonrpc": "2.0", "method": "textDocument/publishDiagnostics",
					"params": map[string]any{
						"uri": uri,
						"diagnostics": []map[string]any{{
							"range":    map[string]any{"start": map[string]any{"line": 2, "character": 4}},
							"severity": 1,
							"message":  "undefined: missingSym",
							"source":   "gopls",
						}},
					},
				})
			}
		}
	}()

	mgr := lsp.New(lsp.Config{
		WorkRoot:  root,
		Languages: map[string]lsp.LanguageServer{"go": {Command: "fake"}},
		Timeout:   2 * time.Second,
		Dial: func(lang string) (io.Reader, io.Writer, func() error, error) {
			return cli, cli, func() error { return cli.Close() }, nil
		},
	})
	require.True(t, mgr.Enabled(), "Dial 非空 → 不剪枝 → enabled")
	defer mgr.Close()

	fs := NewFSTools(root)
	ctx := context.Background()
	ctx = WithProfile(ctx, guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"fs_*"}},
		FS:    guard.FSPerm{Write: []string{root + "/**"}, Read: []string{root + "/**"}},
	})
	ctx = WithLSP(ctx, mgr)

	out, err := runTool(ctx, fs.Edit, `{"path":"main.go","old_string":"package main","new_string":"package main\nvar _ = missingSym"}`)
	require.NoError(t, err)

	var m map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &m))
	d, ok := m["diagnostics"]
	require.True(t, ok, "fs_edit 结果应含 diagnostics 字段,out=%s", out)
	assert.Contains(t, fmt.Sprint(d), "undefined: missingSym", "诊断消息应来自 fake server")
}
