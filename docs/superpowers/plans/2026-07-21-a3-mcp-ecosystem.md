# MCP 生态 (Batch A3) Implementation Plan

> **Batch ID:** A3
> **Spec:** `docs/feature-roadmap-codex-deepseek.md` §5 [V16][C13][MCP1]
> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 建立 yanshi 的通用 MCP 客户端：连接管理器统一管理 stdio + streamable HTTP 两类 MCP server，自动发现工具/资源，以 `mcp_<server>_<tool>` 命名空间注册为 `GuardedTool`（默认 deny，profile 显式放行），TUI `/mcp` 提供实化管理界面，命令面板按 server 分组发现 MCP 工具。

**Architecture:** 新建 `internal/mcp/` 包，三层叠加：
- 最内 `wire.go` — JSON-RPC 2.0 帧编解码（兼容 Content-Length 与 newline-delimited 两种 stdio 格式，后者与 `internal/vcs/mcp/server.go` 互通）；
- 中层 `client.go` + `stdio.go` + `httpclient.go` — `Client` 接口与两种实现；
- 外层 `manager.go` — `Manager` 按配置启动所有 server，维护连接映射，支持启动超时/重连/健康检查/工具聚合。

桥接层 `internal/tools/mcp.go` 把 Manager 发现的工具动态构造成 `GuardedTool`，编排器 per-turn 通过 `tools.WithMCP` 注入（镜像 `tools.WithVCS`）。`/mcp` 与命令面板通过新 `mcp_action`/`mcp_status` 帧与 server 通信。

**Tech Stack:** Go 1.26.4；标准库 `net/http`、`encoding/json`、`os/exec`、`io`、`net`（`net.Pipe` 用于 stdio 测试）、`net/http/httptest`（用于 HTTP 测试）。新增 `internal/mcp/` 为叶子包，不反向依赖 `internal/tools`。

**权限铁律（fail-closed）:** MCP 工具除了现有 `tools.allow` 外，还必须通过独立的 `profile.mcp.allow` 门禁。`guard.Guard.Check` 遇到 `mcp_` 前缀时先检查 `MCP.Allow`；空列表一律拒绝，再继续检查通用 `Tools.Allow`。因此现有默认 profile 即使是 `tools: { allow: ["*"] }`，只要没有显式 `mcp.allow`，所有 MCP 工具仍默认 deny。显式放行示例：`mcp: { allow: ["mcp_github_*"] }`。交互式 WS 权限回调仍可按现有 `Authorize` 语义对单次调用进行用户批准，但静态 profile 的默认结果必须是 deny。

---

## File Structure

| 文件 | 职责 | 新建/改 |
|---|---|---|
| `internal/mcp/types.go` | `ServerConfig`, `Status`, `ToolDescriptor`, `ResourceDescriptor` 等共享结构与 `QualifyToolName` | 新建 |
| `internal/mcp/wire.go` | JSON-RPC 2.0 帧编解码（Content-Length + newline-delimited 两种） | 新建 |
| `internal/mcp/client.go` | `Client` 接口 | 新建 |
| `internal/mcp/stdio.go` | `StdioClient`: 子进程 + stdio JSON-RPC | 新建 |
| `internal/mcp/httpclient.go` | `HTTPClient`: streamable HTTP 传输 | 新建 |
| `internal/mcp/manager.go` | `Manager`: 配置加载、server 启动、连接池、生命周期、工具聚合 | 新建 |
| `internal/mcp/testing.go` | `NewFakeHTTPServer` 公开测试辅助（供本包与 `tools` 包测试共用） | 新建 |
| `internal/mcp/*_test.go` | 各层 TDD 测试 | 新建 |
| `internal/tools/mcpctx.go` | `WithMCP`/`MCPFromContext`（镜像 `vcsctx.go`） | 新建 |
| `internal/tools/mcp.go` | `NewMCPTools`：遍历 Manager 构造 `*GuardedTool` 列表 | 新建 |
| `internal/tools/mcp_test.go` | MCP 工具桥 TDD 测试 | 新建 |
| `internal/proto/frame.go` | `mcp_action` ClientFrame + `mcp_status` ServerFrame + `MCPServerStatus` 类型 | 改 |
| `internal/config/config.go` | `MCPConfig` / `MCPServerConfig` 配置节 | 改 |
| `config.example.yaml` | `mcp:` 注释段 + 权限说明 | 改 |
| `internal/api/http/server.go` | `Server` 加 `mcp *mcp.Manager` 字段 + `MCP` Config 项 | 改 |
| `internal/api/http/ws.go` | `list_mcp` 改查 Manager 真实状态；`mcp_action` 新帧 dispatch | 改 |
| `internal/api/http/ws_test.go` | MCP action 测试 | 改 |
| `internal/bootstrap/bootstrap.go` | Build MCP Manager，软降级装配，注入 http.Server 与 orchestrator | 改 |
| `internal/agent/orchestrator/orchestrator.go` | `Config.MCP` + per-turn `tools.WithMCP` 注入 | 改 |
| `internal/cli/tui/commands.go` | cmdMCP 子命令化 + palette 扩展 | 改 |
| `internal/cli/tui/model.go` | `mcp_status` 事件 + `paletteMCPItems` 字段 | 改 |
| `internal/cli/tui/view.go` | mcpStatusEntry 渲染增强 | 改 |

---

## Task 1: 类型定义 + 名称规约

**Files:**
- Create: `internal/mcp/types.go`
- Create: `internal/mcp/types_test.go`

MCP server 的工具以 `mcp_<server>_<tool>` 命名暴露给模型。server 名和 tool 名中的非字母数字字符替换为 `_`、连续 `_` 合并、小写化。超过 64 字符截断 + 哈希后缀确保唯一。

- [ ] **Step 1: 写失败测试**

```go
// internal/mcp/types_test.go
package mcp

import (
	"testing"
)

func TestQualifyToolName_Basic(t *testing.T) {
	got := QualifyToolName("my-server", "get-file")
	want := "mcp_my_server_get_file"
	if got != want {
		t.Fatalf("QualifyToolName(%q,%q) = %q, want %q", "my-server", "get-file", got, want)
	}
}

func TestQualifyToolName_SanitizesSpecials(t *testing.T) {
	got := QualifyToolName("Server 1!", "read:data")
	want := "mcp_server_1_read_data"
	if got != want {
		t.Fatalf("QualifyToolName = %q, want %q", got, want)
	}
}

func TestQualifyToolName_TruncatesLong(t *testing.T) {
	longServer := "this-is-a-very-long-server-name-that-exceeds-sixty-four-characters"
	got := QualifyToolName(longServer, "a-tool-name")
	if len(got) > 64 {
		t.Fatalf("QualifyToolName length %d > 64: %q", len(got), got)
	}
	if len(got) != 64 {
		t.Fatalf("truncated name should be exactly 64 chars, got %d (%q)", len(got), got)
	}
	if got[:4] != "mcp_" {
		t.Fatalf("prefix lost: %q", got)
	}
}

func TestQualifyToolName_StableForSameInput(t *testing.T) {
	a := QualifyToolName("x-y", "z-w")
	b := QualifyToolName("x-y", "z-w")
	if a != b {
		t.Fatalf("QualifyToolName not stable: %q vs %q", a, b)
	}
}

func TestQualifyToolName_DistinctForDifferentServers(t *testing.T) {
	a := QualifyToolName("srv-a", "tool")
	b := QualifyToolName("srv-b", "tool")
	if a == b {
		t.Fatalf("different servers should produce different names: %q", a)
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/mcp/ -run TestQualify -v`
Expected: FAIL（包/类型未定义）

- [ ] **Step 3: 实现 types.go**

```go
// internal/mcp/types.go
//
// Package mcp 提供通用 MCP 客户端：连接 stdio 与 streamable HTTP 两类 MCP server，
// 发现工具的 list/call、resources 的 list/read、生命周期管理。
//
// 权限由上层 tools.GuardedTool 统一处理（工具名 mcp_<server>_<tool>），
// 本包不感知 PermissionProfile。
package mcp

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

// ServerTransport 标记 server 的传输方式。
type ServerTransport string

const (
	TransportStdio ServerTransport = "stdio"
	TransportHTTP  ServerTransport = "http" // streamable HTTP
)

// ServerConfig 是每个 MCP server 的启动/连接配置。
type ServerConfig struct {
	Name      string            `json:"name" yaml:"name"`
	Enabled   bool              `json:"enabled" yaml:"enabled"`
	Transport ServerTransport   `json:"transport" yaml:"transport"`
	Command   string            `json:"command,omitempty" yaml:"command,omitempty"`
	Args      []string          `json:"args,omitempty" yaml:"args,omitempty"`
	Env       map[string]string `json:"env,omitempty" yaml:"env,omitempty"`
	URL       string            `json:"url,omitempty" yaml:"url,omitempty"`
	Bearer    string            `json:"bearer,omitempty" yaml:"bearer,omitempty"`
	Timeout   time.Duration     `json:"timeout,omitempty" yaml:"timeout,omitempty"`
	Reconnect bool              `json:"reconnect,omitempty" yaml:"reconnect,omitempty"`
}

// ConnectionStatus 表示一个 server 的实时连接状态。
type ConnectionStatus string

const (
	StatusStarting ConnectionStatus = "starting"
	StatusReady    ConnectionStatus = "ready"
	StatusFailed   ConnectionStatus = "failed"
	StatusStopped  ConnectionStatus = "stopped"
)

// ServerStatus 是每个 server 对外暴露的实时快照（用于 /mcp 渲染与 palette 分组）。
type ServerStatus struct {
	Name      string               `json:"name"`
	Transport ServerTransport      `json:"transport"`
	Status    ConnectionStatus     `json:"status"`
	Error     string               `json:"error,omitempty"`
	Tools     []ToolDescriptor     `json:"tools,omitempty"`
	Resources []ResourceDescriptor `json:"resources,omitempty"`
}

// ToolDescriptor 是一个发现的 MCP 工具的元数据。
type ToolDescriptor struct {
	ServerName  string `json:"server_name"`
	ToolName    string `json:"tool_name"`
	Qualified   string `json:"qualified"` // mcp_<server>_<tool>
	Description string `json:"description,omitempty"`
	InputSchema string `json:"input_schema,omitempty"` // JSON 字符串
}

// ResourceDescriptor 是一个 MCP resource 的元数据。
type ResourceDescriptor struct {
	ServerName  string `json:"server_name"`
	URI         string `json:"uri"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	MimeType    string `json:"mime_type,omitempty"`
}

// sanitizeName 把非字母数字字符替换为 `_`，合并连续 `_`，去掉前后 `_`，小写化。
// 这是 QualifyToolName 的内部 helper，不导出。
func sanitizeName(s string) string {
	var b strings.Builder
	prevUnderscore := true // 抑制前导 `_`
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			prevUnderscore = false
		} else {
			if !prevUnderscore {
				b.WriteByte('_')
				prevUnderscore = true
			}
		}
	}
	out := b.String()
	return strings.TrimSuffix(out, "_")
}

// QualifyToolName 生成 `mcp_<server>_<tool>`。长度 > 64 时截到 51 + "_" + 12-hex 哈希。
// 51 + 1 + 12 = 64，符合 MCP runtime name 上限。同输入必同输出（hash 由原 full string 派生）。
func QualifyToolName(server, tool string) string {
	s := "mcp_" + sanitizeName(server) + "_" + sanitizeName(tool)
	if len(s) <= 64 {
		return s
	}
	prefix := s[:51]
	sum := sha256.Sum256([]byte(s))
	hash := hex.EncodeToString(sum[:6]) // 12 hex chars
	return prefix + "_" + hash
}

// ParseQualifiedName 从 `mcp_<server>_<tool>` 拆回 server/tool（用于显示）。
// 注意：截断/合并过的名字可能无法精确还原；生产路由应用 LookupTool 查 map 而非反向解析。
// 这里用第一个 `_` 作为 server/tool 分隔的启发式，仅用于 /mcp 与日志的可读显示。
func ParseQualifiedName(qualified string) (server, tool string, err error) {
	if !strings.HasPrefix(qualified, "mcp_") {
		return "", "", fmt.Errorf("mcp: qualified name %q missing mcp_ prefix", qualified)
	}
	rest := qualified[4:]
	idx := strings.IndexByte(rest, '_')
	if idx < 0 {
		return "", "", fmt.Errorf("mcp: qualified name %q has no server/tool delimiter", qualified)
	}
	return rest[:idx], rest[idx+1:], nil
}
```

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/mcp/ -run TestQualify -v`
Expected: PASS

- [ ] **Step 5: 提交**

```bash
git add internal/mcp/types.go internal/mcp/types_test.go
git commit -m "feat(mcp): types + QualifyToolName (mcp_<server>_<tool>) naming"
```

---

## Task 2: JSON-RPC 2.0 帧编解码（Content-Length + newline）

**Files:**
- Create: `internal/mcp/wire.go`
- Create: `internal/mcp/wire_test.go`

兼容两种 stdio 帧格式：
1. LSP 风格的 `Content-Length: <N>\r\n\r\n<JSON>`（多数 MCP server）
2. Yanshi VCS MCP server 风格的 newline-delimited JSON（每行一条）

`ReadMessage` 自动嗅探第一行前缀决定按哪种格式解析。

- [ ] **Step 1: 写失败测试**

```go
// internal/mcp/wire_test.go
package mcp

import (
	"bufio"
	"bytes"
	"encoding/json"
	"testing"
)

func TestWriteMessage_ContentLengthFraming(t *testing.T) {
	var buf bytes.Buffer
	payload := map[string]any{"jsonrpc": "2.0", "method": "tools/list"}
	if err := WriteMessage(&buf, payload); err != nil {
		t.Fatalf("WriteMessage: %v", err)
	}
	if !bytes.Contains(buf.Bytes(), []byte("Content-Length:")) {
		t.Errorf("缺少 Content-Length 头")
	}
	body := bytes.SplitN(buf.Bytes(), []byte("\r\n\r\n"), 2)[1]
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("body 非合法 JSON: %v", err)
	}
	if got["method"] != "tools/list" {
		t.Errorf("method mismatch: %v", got["method"])
	}
}

func TestReadMessage_ContentLengthRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	_ = WriteMessage(&buf, map[string]any{"jsonrpc": "2.0", "id": 1, "result": map[string]any{"ok": true}})
	got, err := ReadMessage(bufio.NewReader(&buf))
	if err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}
	res, _ := got["result"].(map[string]any)
	if res["ok"] != true {
		t.Errorf("round-trip 失败: %v", got)
	}
}

func TestReadMessage_NewlineDelimited(t *testing.T) {
	var buf bytes.Buffer
	buf.WriteString(`{"jsonrpc":"2.0","id":1,"result":"ok"}` + "\n")
	buf.WriteString(`{"jsonrpc":"2.0","id":2,"result":"ok2"}` + "\n")
	r := bufio.NewReader(&buf)
	msg1, err := ReadMessage(r)
	if err != nil {
		t.Fatalf("ReadMessage line1: %v", err)
	}
	if id, _ := msg1["id"].(float64); int(id) != 1 {
		t.Errorf("line1 id = %v", msg1["id"])
	}
	msg2, err := ReadMessage(r)
	if err != nil {
		t.Fatalf("ReadMessage line2: %v", err)
	}
	if id, _ := msg2["id"].(float64); int(id) != 2 {
		t.Errorf("line2 id = %v", msg2["id"])
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/mcp/ -run 'TestWriteMessage|TestReadMessage' -v`
Expected: FAIL（`WriteMessage`/`ReadMessage` 未定义）

- [ ] **Step 3: 实现 wire.go**

```go
// internal/mcp/wire.go
package mcp

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
)

// WriteMessage 编码 v 为 JSON 并按 Content-Length 帧写入 w（标准 LSP stdio 帧）。
func WriteMessage(w io.Writer, v any) error {
	body, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("mcp: marshal: %w", err)
	}
	hdr := "Content-Length: " + strconv.Itoa(len(body)) + "\r\n\r\n"
	if _, err := w.Write([]byte(hdr)); err != nil {
		return fmt.Errorf("mcp: write header: %w", err)
	}
	if _, err := w.Write(body); err != nil {
		return fmt.Errorf("mcp: write body: %w", err)
	}
	return nil
}

// WriteLineMessage 编码 v 为 JSON 并以单行（带 `\n`）写入 w（Yanshi VCS MCP server 格式）。
// 用于与 internal/vcs/mcp/server.go 这类只读 newline-delimited JSON 的 server 互通。
func WriteLineMessage(w io.Writer, v any) error {
	body, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("mcp: marshal: %w", err)
	}
	if _, err := w.Write(append(body, '\n')); err != nil {
		return fmt.Errorf("mcp: write line: %w", err)
	}
	return nil
}

// ReadMessage 从 r 读一条 JSON-RPC 帧。自动检测两种格式：
//   - 第一行以 "Content-Length:" 开头 → LSP 风格多行头 + 空行 + body。
//   - 否则 → 把第一行作为 newline-delimited JSON 解析。
//
// 返回解码后的 map（字段 jsonrpc/id/method/params/result/error 按需取用）。
func ReadMessage(r *bufio.Reader) (map[string]any, error) {
	first, err := r.ReadBytes('\n')
	if err != nil && len(first) == 0 {
		return nil, fmt.Errorf("mcp: read first line: %w", err)
	}
	trimmed := bytes.TrimRight(first, "\r\n")
	if bytes.HasPrefix(trimmed, []byte("Content-Length:")) {
		val := bytes.TrimSpace(bytes.TrimPrefix(trimmed, []byte("Content-Length:")))
		length, err := strconv.Atoi(string(val))
		if err != nil {
			return nil, fmt.Errorf("mcp: bad Content-Length %q: %w", string(val), err)
		}
		for {
			line, err := r.ReadBytes('\n')
			if err != nil {
				return nil, fmt.Errorf("mcp: read header line: %w", err)
			}
			if len(bytes.TrimRight(line, "\r\n")) == 0 {
				reak
			}
		}
		body := make([]byte, length)
		if _, err := io.ReadFull(r, body); err != nil {
			return nil, fmt.Errorf("mcp: read body: %w", err)
		}
		return parseJSON(body)
	}
	return parseJSON(bytes.TrimSpace(first))
}

func parseJSON(body []byte) (map[string]any, error) {
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("mcp: unmarshal: %w", err)
	}
	return m, nil
}
```

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/mcp/ -run 'TestWriteMessage|TestReadMessage' -v`
Expected: PASS

- [ ] **Step 5: 提交**

```bash
git add internal/mcp/wire.go internal/mcp/wire_test.go
git commit -m "feat(mcp): JSON-RPC 2.0 framing (Content-Length + newline-delimited)"
```

---

## Task 3: Client 接口 + 公开测试辅助（NewFakeHTTPServer）

**Files:**
- Create: `internal/mcp/client.go`
- Create: `internal/mcp/testing.go`
- Create: `internal/mcp/testing_test.go`

`Client` 接口抽象 stdio 与 HTTP 的共同行为。把 `NewFakeHTTPServer` 放到普通 `testing.go` 文件，导出供 `tools` 包测试复用（Fake-first）。

- [ ] **Step 1: 写失败测试**

```go
// internal/mcp/testing_test.go
package mcp

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"
)

// TestNewFakeHTTPServer_Responds exercises the fake MCP server via raw HTTP
// (not the MCP Client interface, which is defined in later tasks). This
// keeps Task3 leaf-code only — clients are added in Tasks 4-5.
func TestNewFakeHTTPServer_Responds(t *testing.T) {
	ts, fake := NewFakeHTTPServer([]ToolDescriptor{
		{ServerName: "test", ToolName: "greet", Qualified: "mcp_test_greet", Description: "say hello"},
	})
	defer ts.Close()

	post := func(t *testing.T, method string, id any) map[string]any {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method})
		resp, err := http.Post(ts.URL, "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatalf("POST %s: %v", method, err)
		}
		defer resp.Body.Close()
		var out map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatalf("decode %s: %v", method, err)
		}
		return out
	}

	initResp := post(t, "initialize", 1)
	res, ok := initResp["result"].(map[string]any)
	if !ok || res["protocolVersion"] != "2025-06-18" {
		t.Fatalf("initialize: %+v", initResp)
	}

	toolResp := post(t, "tools/list", 2)
	res, ok = toolResp["result"].(map[string]any)
	if !ok {
		t.Fatalf("tools/list no result: %+v", toolResp)
	}
	tools, _ := res["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(tools))
	}

	callResp := post(t, "tools/call", 3)
	if _, ok := callResp["error"]; ok {
		t.Fatalf("tools/call error: %+v", callResp)
	}
	if fake.CallCount != 1 {
		t.Fatalf("expected 1 call, got %d", fake.CallCount)
	}

	pingResp := post(t, "ping", 4)
	if _, ok := pingResp["error"]; ok {
		t.Fatalf("ping error: %+v", pingResp)
	}
}

// TestClientInterface_CompileCheck is defined in Task 5 (httpclient_test.go)
// after both StdioClient and HTTPClient exist. TestClientInterface_CompileCheck
// was moved there because Task 3 only owns Client interface + FakeServer.

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/mcp/ -run 'TestNewFakeHTTPServer|TestClientInterface' -v`
Expected: FAIL

- [ ] **Step 3: 实现 client.go**

```go
// internal/mcp/client.go
package mcp

import (
	"context"
	"encoding/json"
)

// Client 是 MCP server 的统一协议接口。StdioClient 与 HTTPClient 都实现此接口。
// Manager 只依赖此接口，便于用 FakeClient 测试。
type Client interface {
	Initialize(ctx context.Context, rootURI string) error
	ListTools(ctx context.Context) ([]ToolDescriptor, error)
	CallTool(ctx context.Context, name string, arguments json.RawMessage) (json.RawMessage, error)
	ListResources(ctx context.Context) ([]ResourceDescriptor, error)
	ReadResource(ctx context.Context, uri string) (json.RawMessage, error)
	Ping(ctx context.Context) error
	Close() error
}
```

- [ ] **Step 4: 实现 testing.go**

```go
// internal/mcp/testing.go
package mcp

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
)

// NewFakeHTTPServer 启动一个最小化 MCP-over-HTTP 测试端点。
func NewFakeHTTPServer(tools []ToolDescriptor) (*httptest.Server, *FakeServer) {
	fs := &FakeServer{Tools: tools}
	ts := httptest.NewServer(http.HandlerFunc(fs.handle))
	fs.Server = ts
	return ts, fs
}

// FakeServer 是 NewFakeHTTPServer 返回的可观测辅助对象。
type FakeServer struct {
	Server    *httptest.Server
	Tools     []ToolDescriptor
	CallCount int
}

func (f *FakeServer) handle(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var req struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
	}
	_ = json.Unmarshal(body, &req)

	resp := map[string]any{"jsonrpc": "2.0"}
	if len(req.ID) > 0 {
		resp["id"] = json.RawMessage(req.ID)
	}
	switch req.Method {
	case "notifications/initialized":
		w.WriteHeader(http.StatusAccepted)
		return
	case "initialize":
		resp["result"] = map[string]any{
			"protocolVersion": "2025-06-18",
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "fake-mcp", "version": "0.1"},
		}
	case "tools/list":
		toolObjs := make([]map[string]any, len(f.Tools))
		for i, td := range f.Tools {
			toolObjs[i] = map[string]any{
				"name":        td.ToolName,
				"description": td.Description,
				"inputSchema": map[string]any{"type": "object"},
			}
		}
		resp["result"] = map[string]any{"tools": toolObjs}
	case "tools/call":
		f.CallCount++
		resp["result"] = map[string]any{
			"content": []map[string]any{{"type": "text", "text": `{"ok":true}`}},
		}
	case "resources/list":
		resp["result"] = map[string]any{"resources": []map[string]any{}}
	case "ping":
		resp["result"] = map[string]any{}
	default:
		resp["error"] = map[string]any{"code": -32601, "message": "unknown method"}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}
```

- [ ] **Step 5: 提交**

```bash
git add internal/mcp/client.go internal/mcp/testing.go internal/mcp/testing_test.go
git commit -m "feat(mcp): Client interface + public fake MCP HTTP server"
```

---

## Task 4: StdioClient — 握手 + 工具调用

**Files:**
- Create: `internal/mcp/stdio.go`
- Create: `internal/mcp/stdio_test.go`

- [ ] **Step 1: 写失败测试**

```go
// internal/mcp/stdio_test.go
package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"testing"
)

func fakeStdioProcess(t *testing.T) (io.Writer, io.Reader, func()) {
	t.Helper()
	srvInR, srvInW := io.Pipe()
	srvOutR, srvOutW := io.Pipe()
	srvBuf := bufio.NewReader(srvInR)
	go func() {
		defer srvInR.Close()
		defer srvOutW.Close()
		for {
			msg, err := ReadMessage(srvBuf)
			if err != nil {
				return
			}
			id := msg["id"]
			method, _ := msg["method"].(string)
			if id == nil { // initialized notification
				continue
			}
			resp := map[string]any{"jsonrpc": "2.0", "id": id}
			switch method {
			case "initialize":
				resp["result"] = map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}}
			case "tools/list":
				resp["result"] = map[string]any{"tools": []map[string]any{{"name": "echo", "description": "echo", "inputSchema": map[string]any{"type": "object"}}}}
			case "tools/call":
				resp["result"] = map[string]any{"content": []map[string]any{{"type": "text", "text": `{"echoed":true}`}}}
			case "resources/list":
				resp["result"] = map[string]any{"resources": []map[string]any{}}
			case "ping":
				resp["result"] = map[string]any{}
			}
			data, _ := json.Marshal(resp)
			_, _ = srvOutW.Write(append(data, '\n'))
		}
	}()
	return srvInW, srvOutR, func() { _ = srvInW.Close(); _ = srvOutR.Close() }
}

func TestStdioClient_InitializeListCallPing(t *testing.T) {
	w, r, cleanup := fakeStdioProcess(t)
	defer cleanup()
	cli := NewStdioClient(r, w)
	if err := cli.Initialize(context.Background(), "/test"); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	tools, err := cli.ListTools(context.Background())
	if err != nil || len(tools) != 1 || tools[0].ToolName != "echo" {
		t.Fatalf("ListTools: tools=%+v err=%v", tools, err)
	}
	res, err := cli.CallTool(context.Background(), "echo", json.RawMessage(`{}`))
	if err != nil || string(res) != `{"echoed":true}` {
		t.Fatalf("CallTool: res=%q err=%v", string(res), err)
	}
	if err := cli.Ping(context.Background()); err != nil {
		t.Fatalf("Ping: %v", err)
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/mcp/ -run TestStdioClient -v`
Expected: FAIL

- [ ] **Step 3: 实现 stdio.go**

```go
// internal/mcp/stdio.go
package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"
)

// StdioClient 通过 stdin/stdout pipe 与 MCP server 子进程通信。
// cmd 持有子进程引用，Close 时 Kill+Wait 回收；未托管子进程（如测试用 io.Pipe）
// 时 cmd 为 nil，Close 只关闭 writer。
type StdioClient struct {
	r       *bufio.Reader
	w       io.Writer
	cmd     *exec.Cmd
	mu      sync.Mutex
	nextID  int64
	timeout time.Duration
}

func NewStdioClient(r io.Reader, w io.Writer) *StdioClient {
	return &StdioClient{r: bufio.NewReader(r), w: w, timeout: 30 * time.Second}
}

// SetCmd 绑定底层子进程，使 Close 能 Kill+Wait 回收它。链式返回 *StdioClient。
// Manager.startOne 在 cmd.Start() 之后调用；测试用 io.Pipe 时无需调用。
func (c *StdioClient) SetCmd(cmd *exec.Cmd) *StdioClient {
	c.cmd = cmd
	return c
}

func (c *StdioClient) SetTimeout(d time.Duration) *StdioClient {
	if d > 0 {
		c.timeout = d
	}
	return c
}

func (c *StdioClient) Initialize(ctx context.Context, rootURI string) error {
	id := atomic.AddInt64(&c.nextID, 1)
	resp, err := c.doRequest(ctx, id, map[string]any{
		"jsonrpc": "2.0", "id": id, "method": "initialize",
		"params": map[string]any{
			"protocolVersion": "2025-06-18",
			"capabilities": map[string]any{},
			"clientInfo": map[string]any{"name": "yanshi-mcp", "version": "0.1"},
		},
	})
	if err != nil {
		return err
	}
	if e, ok := resp["error"]; ok {
		return fmt.Errorf("mcp: initialize error: %v", e)
	}
	return c.notify("notifications/initialized", map[string]any{})
}

func (c *StdioClient) doRequest(ctx context.Context, id int64, req any) (map[string]any, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := WriteLineMessage(c.w, req); err != nil {
		return nil, err
	}
	deadline := time.Now().Add(c.timeout)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		msg, err := ReadMessage(c.r)
		if err != nil {
			return nil, err
		}
		if rid, ok := toInt64(msg["id"]); ok && rid == id {
			return msg, nil
		}
	}
	return nil, fmt.Errorf("mcp: timeout waiting for id=%d", id)
}

func (c *StdioClient) notify(method string, params any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return WriteLineMessage(c.w, map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

func (c *StdioClient) ListTools(ctx context.Context) ([]ToolDescriptor, error) {
	id := atomic.AddInt64(&c.nextID, 1)
	resp, err := c.doRequest(ctx, id, map[string]any{"jsonrpc": "2.0", "id": id, "method": "tools/list"})
	if err != nil { return nil, err }
	return parseToolList("stdio", resp)
}

func (c *StdioClient) CallTool(ctx context.Context, name string, args json.RawMessage) (json.RawMessage, error) {
	id := atomic.AddInt64(&c.nextID, 1)
	resp, err := c.doRequest(ctx, id, map[string]any{"jsonrpc": "2.0", "id": id, "method": "tools/call", "params": map[string]any{"name": name, "arguments": args}})
	if err != nil { return nil, err }
	return extractToolResult(resp)
}

func (c *StdioClient) ListResources(ctx context.Context) ([]ResourceDescriptor, error) {
	id := atomic.AddInt64(&c.nextID, 1)
	resp, err := c.doRequest(ctx, id, map[string]any{"jsonrpc": "2.0", "id": id, "method": "resources/list"})
	if err != nil { return nil, err }
	return parseResourceList("stdio", resp)
}

func (c *StdioClient) ReadResource(ctx context.Context, uri string) (json.RawMessage, error) {
	id := atomic.AddInt64(&c.nextID, 1)
	resp, err := c.doRequest(ctx, id, map[string]any{"jsonrpc": "2.0", "id": id, "method": "resources/read", "params": map[string]any{"uri": uri}})
	if err != nil { return nil, err }
	return extractResultRaw(resp)
}

func (c *StdioClient) Ping(ctx context.Context) error {
	id := atomic.AddInt64(&c.nextID, 1)
	_, err := c.doRequest(ctx, id, map[string]any{"jsonrpc": "2.0", "id": id, "method": "ping"})
	return err
}

func (c *StdioClient) Close() error {
	// 先回收子进程（若有）：Kill 发送 SIGKILL/TerminateProcess，Wait 释放 OS
	// 资源并回收 pipe。忽略已退出进程的 error。
	if c.cmd != nil && c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
		_ = c.cmd.Wait()
	}
	if closer, ok := c.w.(io.Closer); ok {
		_ = closer.Close()
	}
	return nil
}

func parseToolList(server string, resp map[string]any) ([]ToolDescriptor, error) {
	res, _ := resp["result"].(map[string]any)
	if res == nil { return nil, fmt.Errorf("mcp: tools/list: no result") }
	raw, _ := res["tools"].([]any)
	out := make([]ToolDescriptor, 0, len(raw))
	for _, v := range raw {
		m, _ := v.(map[string]any)
		name, _ := m["name"].(string)
		if name == "" { continue }
		schemaBytes, _ := json.Marshal(m["inputSchema"])
		out = append(out, ToolDescriptor{ServerName: server, ToolName: name, Qualified: QualifyToolName(server, name), Description: strOr(m, "description"), InputSchema: string(schemaBytes)})
	}
	return out, nil
}

func parseResourceList(server string, resp map[string]any) ([]ResourceDescriptor, error) {
	res, _ := resp["result"].(map[string]any)
	if res == nil { return nil, fmt.Errorf("mcp: resources/list: no result") }
	raw, _ := res["resources"].([]any)
	out := make([]ResourceDescriptor, 0, len(raw))
	for _, v := range raw {
		m, _ := v.(map[string]any)
		uri, _ := m["uri"].(string)
		if uri == "" { continue }
		out = append(out, ResourceDescriptor{ServerName: server, URI: uri, Name: strOr(m, "name"), Description: strOr(m, "description"), MimeType: strOr(m, "mimeType")})
	}
	return out, nil
}

func extractToolResult(resp map[string]any) (json.RawMessage, error) {
	res, _ := resp["result"].(map[string]any)
	if res == nil { return nil, fmt.Errorf("mcp: tools/call: no result") }
	content, _ := res["content"].([]any)
	if len(content) == 0 { return json.RawMessage(`{}`), nil }
	first, _ := content[0].(map[string]any)
	text, _ := first["text"].(string)
	return json.RawMessage(text), nil
}

func extractResultRaw(resp map[string]any) (json.RawMessage, error) {
	if e, ok := resp["error"]; ok { return nil, fmt.Errorf("mcp: error: %v", e) }
	b, _ := json.Marshal(resp["result"])
	return b, nil
}

func strOr(m map[string]any, key string) string { s, _ := m[key].(string); return s }
func toInt64(v any) (int64, bool) { n, ok := v.(float64); return int64(n), ok }
```

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/mcp/ -run TestStdioClient -v`
Expected: PASS

- [ ] **Step 5: 提交**

```bash
git add internal/mcp/stdio.go internal/mcp/stdio_test.go
git commit -m "feat(mcp): StdioClient"
```

---

## Task 5: HTTPClient — streamable HTTP

**Files:**
- Create: `internal/mcp/httpclient.go`
- Create: `internal/mcp/httpclient_test.go`

- [ ] **Step 1: 写失败测试**

```go
// internal/mcp/httpclient_test.go
package mcp

import (
	"context"
	"encoding/json"
	"testing"
)

func TestHTTPClient_FullFlow(t *testing.T) {
	ts, _ := NewFakeHTTPServer([]ToolDescriptor{{ToolName: "status"}})
	defer ts.Close()
	cli := NewHTTPClient(ts.URL, "")
	if err := cli.Initialize(context.Background(), "/test"); err != nil { t.Fatal(err) }
	tools, err := cli.ListTools(context.Background())
	if err != nil || len(tools) != 1 { t.Fatalf("tools=%v err=%v", tools, err) }
	res, err := cli.CallTool(context.Background(), "status", json.RawMessage(`{}`))
	if err != nil || string(res) != `{"ok":true}` { t.Fatalf("res=%s err=%v", res, err) }
	if err := cli.Ping(context.Background()); err != nil { t.Fatal(err) }
}

// TestClientInterface_CompileCheck verifies that StdioClient (Task 4) and
// HTTPClient (this task) both satisfy the Client interface. Defined here
// (rather than in Task 3) because Task 3 only owns Client + FakeServer.
func TestClientInterface_CompileCheck(t *testing.T) {
	var _ Client = (*StdioClient)(nil)
	var _ Client = (*HTTPClient)(nil)
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/mcp/ -run TestHTTPClient -v`
Expected: FAIL

- [ ] **Step 3: 实现 httpclient.go**

```go
// internal/mcp/httpclient.go
package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync/atomic"
	"time"
)

type HTTPClient struct {
	baseURL string
	token   string
	httpCli *http.Client
	nextID  int64
}

func NewHTTPClient(baseURL, token string) *HTTPClient {
	return &HTTPClient{baseURL: baseURL, token: token, httpCli: &http.Client{Timeout: 30 * time.Second}}
}
func (c *HTTPClient) SetTimeout(d time.Duration) { if d > 0 { c.httpCli.Timeout = d } }

func (c *HTTPClient) do(ctx context.Context, method string, params map[string]any) (map[string]any, error) {
	id := atomic.AddInt64(&c.nextID, 1)
	body := map[string]any{"jsonrpc": "2.0", "id": id, "method": method}
	if params != nil { body["params"] = params }
	raw, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL, bytes.NewReader(raw))
	if err != nil { return nil, err }
	req.Header.Set("Content-Type", "application/json")
	if c.token != "" { req.Header.Set("Authorization", "Bearer "+c.token) }
	resp, err := c.httpCli.Do(req)
	if err != nil { return nil, err }
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil { return nil, err }
	if resp.StatusCode >= 400 { return nil, fmt.Errorf("mcp: http %d: %s", resp.StatusCode, b) }
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil { return nil, err }
	return out, nil
}

func (c *HTTPClient) Initialize(ctx context.Context, rootURI string) error {
	resp, err := c.do(ctx, "initialize", map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "yanshi-mcp", "version": "0.1"}})
	if err != nil { return err }
	if e, ok := resp["error"]; ok { return fmt.Errorf("mcp: initialize: %v", e) }
	return nil
}
func (c *HTTPClient) ListTools(ctx context.Context) ([]ToolDescriptor, error) { r, e := c.do(ctx, "tools/list", nil); if e != nil { return nil, e }; return parseToolList("http", r) }
func (c *HTTPClient) CallTool(ctx context.Context, name string, args json.RawMessage) (json.RawMessage, error) { var a map[string]any; _ = json.Unmarshal(args, &a); r, e := c.do(ctx, "tools/call", map[string]any{"name": name, "arguments": a}); if e != nil { return nil, e }; return extractToolResult(r) }
func (c *HTTPClient) ListResources(ctx context.Context) ([]ResourceDescriptor, error) { r, e := c.do(ctx, "resources/list", nil); if e != nil { return nil, e }; return parseResourceList("http", r) }
func (c *HTTPClient) ReadResource(ctx context.Context, uri string) (json.RawMessage, error) { r, e := c.do(ctx, "resources/read", map[string]any{"uri": uri}); if e != nil { return nil, e }; return extractResultRaw(r) }
func (c *HTTPClient) Ping(ctx context.Context) error { _, e := c.do(ctx, "ping", nil); return e }
func (c *HTTPClient) Close() error { c.httpCli.CloseIdleConnections(); return nil }
```

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/mcp/ -v`
Expected: PASS

- [ ] **Step 5: 提交**

```bash
git add internal/mcp/httpclient.go internal/mcp/httpclient_test.go
git commit -m "feat(mcp): streamable HTTP client"
```

---

## Task 6: OAuth token source + 完整 streamable HTTP（JSON/SSE/session）

**Files:**
- Create: `internal/mcp/oauth.go`
- Create: `internal/mcp/oauth_test.go`
- Replace: `internal/mcp/httpclient.go`
- Replace: `internal/mcp/httpclient_test.go`
- Modify: `internal/mcp/types.go`

Task 5 先建立最短 JSON request/response 纵切；本 task 按 MCP streamable HTTP 规范补齐：
- `Accept: application/json, text/event-stream`
- response 可为 JSON 或 SSE（`data:` 拼接后解 JSON-RPC）
- 捕获并回送 `Mcp-Session-Id`
- `notifications/initialized` 使用无 `id` notification，接受 `202 Accepted`
- `Close` 在有 session 时发 HTTP DELETE
- static Bearer 或 OAuth 2.0 client credentials token source
- 401 时 invalidate token 并只重试一次

- [ ] **Step 1: 写 OAuth 失败测试**

```go
// internal/mcp/oauth_test.go
package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestClientCredentialsSource_CachesAndInvalidates(t *testing.T) {
	var calls atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatalf("ParseForm: %v", err)
		}
		if r.Form.Get("grant_type") != "client_credentials" {
			t.Errorf("grant_type = %q", r.Form.Get("grant_type"))
		}
		if r.Form.Get("client_id") != "client" || r.Form.Get("client_secret") != "secret" {
			t.Errorf("credentials missing: %v", r.Form)
		}
		n := calls.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "token-" + string(rune('0'+n)),
			"token_type":   "Bearer",
			"expires_in":   3600,
		})
	}))
	defer ts.Close()

	src := NewClientCredentialsSource(ts.URL, "client", "secret", []string{"tools"}, ts.Client())
	first, err := src.Token(context.Background())
	if err != nil {
		t.Fatalf("Token first: %v", err)
	}
	second, err := src.Token(context.Background())
	if err != nil {
		t.Fatalf("Token cached: %v", err)
	}
	if first != second || calls.Load() != 1 {
		t.Fatalf("cache miss: first=%q second=%q calls=%d", first, second, calls.Load())
	}
	src.Invalidate()
	third, err := src.Token(context.Background())
	if err != nil {
		t.Fatalf("Token after invalidate: %v", err)
	}
	if third == first || calls.Load() != 2 {
		t.Fatalf("invalidate did not refetch: first=%q third=%q calls=%d", first, third, calls.Load())
	}
}

func TestClientCredentialsSource_RefreshesBeforeExpiry(t *testing.T) {
	var calls atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "short",
			"token_type":   "Bearer",
			"expires_in":   1,
		})
	}))
	defer ts.Close()

	src := NewClientCredentialsSource(ts.URL, "c", "s", nil, ts.Client())
	src.now = func() time.Time { return time.Unix(100, 0) }
	if _, err := src.Token(context.Background()); err != nil {
		t.Fatal(err)
	}
	// 30 秒 safety skew 大于 1 秒寿命，因此第二次必须立即刷新。
	if _, err := src.Token(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Fatalf("short token should refresh, calls=%d", calls.Load())
	}
}
```

- [ ] **Step 2: 跑 OAuth 测试确认失败**

Run: `go test ./internal/mcp/ -run TestClientCredentialsSource -v`
Expected: FAIL（token source 未定义）

- [ ] **Step 3: 在 types.go 增加 OAuthConfig，并实现 oauth.go**

在 `ServerConfig` 增加 `OAuth *OAuthConfig`，并新增：

```go
// OAuthConfig 配置 OAuth 2.0 client-credentials token 获取。
// ClientSecret 已在 config.Load 的 ${VAR} 展开阶段解析；不要写入日志/status。
type OAuthConfig struct {
	TokenURL     string   `json:"token_url" yaml:"token_url"`
	ClientID     string   `json:"client_id" yaml:"client_id"`
	ClientSecret string   `json:"-" yaml:"client_secret"`
	Scopes       []string `json:"scopes,omitempty" yaml:"scopes,omitempty"`
}
```

```go
// internal/mcp/oauth.go
package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// TokenSource returns an Authorization bearer token. Invalidate forces the next
// call to fetch a new token (used after HTTP 401). Implementations must be safe
// for concurrent Manager tool calls.
type TokenSource interface {
	Token(ctx context.Context) (string, error)
	Invalidate()
}

// StaticTokenSource wraps a configured bearer token. Invalidate is a no-op.
type StaticTokenSource struct{ Value string }

func (s *StaticTokenSource) Token(context.Context) (string, error) { return s.Value, nil }
func (s *StaticTokenSource) Invalidate()                            {}

// ClientCredentialsSource caches an OAuth 2.0 client-credentials access token.
type ClientCredentialsSource struct {
	mu           sync.Mutex
	tokenURL     string
	clientID     string
	clientSecret string
	scopes       []string
	httpClient   *http.Client
	accessToken  string
	expiresAt    time.Time
	now          func() time.Time
}

// NewClientCredentialsSource constructs a caching client-credentials source.
func NewClientCredentialsSource(tokenURL, clientID, clientSecret string, scopes []string, hc *http.Client) *ClientCredentialsSource {
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	return &ClientCredentialsSource{
		tokenURL: tokenURL, clientID: clientID, clientSecret: clientSecret,
		scopes: append([]string(nil), scopes...), httpClient: hc, now: time.Now,
	}
}

func (s *ClientCredentialsSource) Token(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	if s.accessToken != "" && now.Add(30*time.Second).Before(s.expiresAt) {
		return s.accessToken, nil
	}
	form := url.Values{
		"grant_type":    {"client_credentials"},
		"client_id":     {s.clientID},
		"client_secret": {s.clientSecret},
	}
	if len(s.scopes) > 0 {
		form.Set("scope", strings.Join(s.scopes, " "))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("mcp oauth: build token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("mcp oauth: token request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("mcp oauth: token endpoint returned %s", resp.Status)
	}
	var body struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", fmt.Errorf("mcp oauth: decode token response: %w", err)
	}
	if body.AccessToken == "" {
		return "", fmt.Errorf("mcp oauth: token response has empty access_token")
	}
	if body.TokenType != "" && !strings.EqualFold(body.TokenType, "Bearer") {
		return "", fmt.Errorf("mcp oauth: unsupported token_type %q", body.TokenType)
	}
	lifetime := time.Duration(body.ExpiresIn) * time.Second
	if lifetime <= 0 {
		lifetime = 5 * time.Minute
	}
	s.accessToken = body.AccessToken
	s.expiresAt = now.Add(lifetime)
	return s.accessToken, nil
}

func (s *ClientCredentialsSource) Invalidate() {
	s.mu.Lock()
	s.accessToken = ""
	s.expiresAt = time.Time{}
	s.mu.Unlock()
}
```

- [ ] **Step 4: 写 streamable HTTP 失败测试**

```go
// internal/mcp/httpclient_test.go
package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestHTTPClient_StreamableHTTPJSONAndSession(t *testing.T) {
	const session = "session-123"
	var sawSession atomic.Bool
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Mcp-Session-Id") == session {
			sawSession.Store(true)
		}
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Mcp-Session-Id", session)
		w.Header().Set("Content-Type", "application/json")
		resp := map[string]any{"jsonrpc": "2.0", "id": req.ID}
		switch req.Method {
		case "initialize":
			resp["result"] = map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}}
		case "tools/list":
			resp["result"] = map[string]any{"tools": []map[string]any{}}
		default:
			w.WriteHeader(http.StatusAccepted)
			return
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer ts.Close()

	client := NewHTTPClient(ts.URL, "")
	if err := client.Initialize(context.Background(), "/"); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if _, err := client.ListTools(context.Background()); err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if !sawSession.Load() {
		t.Fatal("subsequent request did not send Mcp-Session-Id")
	}
}

func TestHTTPClient_DecodesSSEResponse(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID json.RawMessage `json:"id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":%s,\"result\":{\"tools\":[]}}\n\n", req.ID)
	}))
	defer ts.Close()

	client := NewHTTPClient(ts.URL, "")
	tools, err := client.ListTools(context.Background())
	if err != nil {
		t.Fatalf("ListTools SSE: %v", err)
	}
	if len(tools) != 0 {
		t.Fatalf("tools = %+v", tools)
	}
}

func TestHTTPClient_RefreshesOAuthOnceOn401(t *testing.T) {
	var tokenCalls atomic.Int32
	tokens := &rotatingTokenSource{calls: &tokenCalls}
	var requests atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := requests.Add(1)
		if n == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.Header.Get("Authorization") != "Bearer token-2" {
			t.Fatalf("Authorization = %q", r.Header.Get("Authorization"))
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0", "id": 1, "result": map[string]any{"tools": []any{}},
		})
	}))
	defer ts.Close()

	client := NewHTTPClientWithTokenSource(ts.URL, tokens)
	if _, err := client.ListTools(context.Background()); err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if requests.Load() != 2 || tokenCalls.Load() != 2 {
		t.Fatalf("requests=%d tokenCalls=%d", requests.Load(), tokenCalls.Load())
	}
}

type rotatingTokenSource struct {
	calls *atomic.Int32
}

func (s *rotatingTokenSource) Token(context.Context) (string, error) {
	return fmt.Sprintf("token-%d", s.calls.Add(1)), nil
}
func (s *rotatingTokenSource) Invalidate() {}

func TestDecodeSSERejectsMissingData(t *testing.T) {
	_, err := decodeSSE(strings.NewReader("event: message\n\n"))
	if err == nil {
		t.Fatal("missing data should fail")
	}
}
```

- [ ] **Step 5: 用完整 streamable 版本替换 httpclient.go**

```go
// internal/mcp/httpclient.go
package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// HTTPClient implements MCP streamable HTTP: JSON or SSE POST responses,
// Mcp-Session-Id propagation, DELETE shutdown, and refreshable bearer auth.
type HTTPClient struct {
	baseURL string
	tokens  TokenSource
	httpCli *http.Client
	nextID  int64
	mu      sync.RWMutex
	session string
}

func NewHTTPClient(baseURL, bearer string) *HTTPClient {
	var source TokenSource
	if bearer != "" {
		source = &StaticTokenSource{Value: bearer}
	}
	return NewHTTPClientWithTokenSource(baseURL, source)
}

func NewHTTPClientWithTokenSource(baseURL string, source TokenSource) *HTTPClient {
	return &HTTPClient{
		baseURL: baseURL,
		tokens:  source,
		httpCli: &http.Client{Timeout: 30 * time.Second},
	}
}

func (c *HTTPClient) SetTimeout(d time.Duration) {
	if d > 0 {
		c.httpCli.Timeout = d
	}
}

func (c *HTTPClient) Initialize(ctx context.Context, rootURI string) error {
	_, err := c.request(ctx, "initialize", map[string]any{
		"protocolVersion": "2025-06-18",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "yanshi-mcp", "version": "0.1"},
	})
	if err != nil {
		return fmt.Errorf("mcp: initialize: %w", err)
	}
	return c.notification(ctx, "notifications/initialized", map[string]any{})
}

func (c *HTTPClient) request(ctx context.Context, method string, params any) (map[string]any, error) {
	id := atomic.AddInt64(&c.nextID, 1)
	payload := map[string]any{"jsonrpc": "2.0", "id": id, "method": method}
	if params != nil {
		payload["params"] = params
	}
	return c.post(ctx, payload, true)
}

func (c *HTTPClient) notification(ctx context.Context, method string, params any) error {
	payload := map[string]any{"jsonrpc": "2.0", "method": method}
	if params != nil {
		payload["params"] = params
	}
	_, err := c.post(ctx, payload, true)
	return err
}

func (c *HTTPClient) post(ctx context.Context, payload any, mayRetryAuth bool) (map[string]any, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("mcp: encode request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("mcp: build HTTP request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	c.mu.RLock()
	session := c.session
	c.mu.RUnlock()
	if session != "" {
		req.Header.Set("Mcp-Session-Id", session)
	}
	if c.tokens != nil {
		token, err := c.tokens.Token(ctx)
		if err != nil {
			return nil, err
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
	}
	resp, err := c.httpCli.Do(req)
	if err != nil {
		return nil, fmt.Errorf("mcp: HTTP POST: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized && mayRetryAuth && c.tokens != nil {
		c.tokens.Invalidate()
		return c.post(ctx, payload, false)
	}
	if resp.StatusCode == http.StatusAccepted || resp.StatusCode == http.StatusNoContent {
		return nil, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		limited, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<10))
		return nil, fmt.Errorf("mcp: HTTP %s: %s", resp.Status, strings.TrimSpace(string(limited)))
	}
	if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
		c.mu.Lock()
		c.session = sid
		c.mu.Unlock()
	}
	mediaType := strings.ToLower(strings.TrimSpace(strings.Split(resp.Header.Get("Content-Type"), ";")[0]))
	var message map[string]any
	if mediaType == "text/event-stream" {
		message, err = decodeSSE(resp.Body)
	} else {
		err = json.NewDecoder(resp.Body).Decode(&message)
	}
	if err != nil {
		return nil, fmt.Errorf("mcp: decode response: %w", err)
	}
	if rpcErr, ok := message["error"]; ok {
		encoded, _ := json.Marshal(rpcErr)
		return nil, fmt.Errorf("mcp: JSON-RPC error: %s", encoded)
	}
	return message, nil
}

// decodeSSE returns the first complete JSON-RPC message in an SSE stream.
// Multiple data: lines in one event are joined with newline per the SSE spec.
func decodeSSE(r io.Reader) (map[string]any, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64<<10), 4<<20)
	var data []string
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if len(data) == 0 {
				continue
			}
			var message map[string]any
			if err := json.Unmarshal([]byte(strings.Join(data, "\n")), &message); err != nil {
				return nil, err
			}
			return message, nil
		}
		if strings.HasPrefix(line, "data:") {
			data = append(data, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if len(data) > 0 {
		var message map[string]any
		if err := json.Unmarshal([]byte(strings.Join(data, "\n")), &message); err != nil {
			return nil, err
		}
		return message, nil
	}
	return nil, fmt.Errorf("mcp: SSE stream ended without data event")
}

func (c *HTTPClient) ListTools(ctx context.Context) ([]ToolDescriptor, error) {
	resp, err := c.request(ctx, "tools/list", nil)
	if err != nil {
		return nil, err
	}
	return parseToolList("http", resp)
}

func (c *HTTPClient) CallTool(ctx context.Context, name string, args json.RawMessage) (json.RawMessage, error) {
	var arguments any = map[string]any{}
	if len(args) > 0 {
		if err := json.Unmarshal(args, &arguments); err != nil {
			return nil, fmt.Errorf("mcp: invalid tool arguments: %w", err)
		}
	}
	resp, err := c.request(ctx, "tools/call", map[string]any{"name": name, "arguments": arguments})
	if err != nil {
		return nil, err
	}
	return extractToolResult(resp)
}

func (c *HTTPClient) ListResources(ctx context.Context) ([]ResourceDescriptor, error) {
	resp, err := c.request(ctx, "resources/list", nil)
	if err != nil {
		return nil, err
	}
	return parseResourceList("http", resp)
}

func (c *HTTPClient) ReadResource(ctx context.Context, uri string) (json.RawMessage, error) {
	resp, err := c.request(ctx, "resources/read", map[string]any{"uri": uri})
	if err != nil {
		return nil, err
	}
	return extractResultRaw(resp)
}

func (c *HTTPClient) Ping(ctx context.Context) error {
	_, err := c.request(ctx, "ping", nil)
	return err
}

func (c *HTTPClient) Close() error {
	c.mu.RLock()
	session := c.session
	c.mu.RUnlock()
	if session != "" {
		req, err := http.NewRequest(http.MethodDelete, c.baseURL, nil)
		if err == nil {
			req.Header.Set("Mcp-Session-Id", session)
			if resp, doErr := c.httpCli.Do(req); doErr == nil {
				_ = resp.Body.Close()
			}
		}
	}
	c.httpCli.CloseIdleConnections()
	return nil
}
```

- [ ] **Step 6: 跑测试确认通过**

Run: `go test ./internal/mcp/ -run 'TestClientCredentialsSource|TestHTTPClient|TestDecodeSSE' -v`
Expected: PASS

- [ ] **Step 7: 提交**

```bash
git add internal/mcp/types.go internal/mcp/oauth.go internal/mcp/oauth_test.go internal/mcp/httpclient.go internal/mcp/httpclient_test.go
git commit -m "feat(mcp): OAuth client credentials + streamable HTTP SSE/session transport"
```

---

## Task 7: Manager — 生命周期 + 连接池

**Files:**
- Create: `internal/mcp/manager.go`
- Create: `internal/mcp/manager_test.go`

- [ ] **Step 1: 写失败测试**

```go
// internal/mcp/manager_test.go
package mcp

import (
	"context"
	"testing"
)

func TestManager_StartDisableEnableRoute(t *testing.T) {
	ts, fake := NewFakeHTTPServer([]ToolDescriptor{{ToolName: "hello"}})
	defer ts.Close()
	m := NewManager(map[string]*ServerConfig{"srv": {Name: "srv", Enabled: true, Transport: TransportHTTP, URL: ts.URL}})
	st := m.StartAll(context.Background())
	if len(st) != 1 || st[0].Status != StatusReady { t.Fatalf("status=%+v", st) }
	tools, _ := m.ListAllTools(context.Background())
	if len(tools) != 1 || tools[0].Qualified != "mcp_srv_hello" { t.Fatalf("tools=%+v", tools) }
	if _, err := m.CallTool(context.Background(), "mcp_srv_hello", []byte(`{}`)); err != nil { t.Fatal(err) }
	if fake.CallCount != 1 { t.Fatalf("calls=%d", fake.CallCount) }
	_ = m.Disable(context.Background(), "srv")
	tools, _ = m.ListAllTools(context.Background())
	if len(tools) != 0 { t.Fatalf("disable tools=%+v", tools) }
	if err := m.Enable(context.Background(), "srv"); err != nil { t.Fatal(err) }
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/mcp/ -run TestManager -v`
Expected: FAIL

- [ ] **Step 3: 实现 manager.go**

```go
// internal/mcp/manager.go
package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"sync"
	"time"
)

type Manager struct {
	mu      sync.Mutex
	servers map[string]*ServerConfig
	clients map[string]Client
	toolMap map[string]ToolDescriptor
	status  map[string]ConnectionStatus
}

func NewManager(servers map[string]*ServerConfig) *Manager {
	m := &Manager{servers: map[string]*ServerConfig{}, clients: map[string]Client{}, toolMap: map[string]ToolDescriptor{}, status: map[string]ConnectionStatus{}}
	for name, sc := range servers { cp := *sc; cp.Name = name; m.servers[name] = &cp; if !cp.Enabled { m.status[name] = StatusStopped } }
	return m
}
func (m *Manager) Enabled() bool { return m != nil && len(m.servers) > 0 }

func (m *Manager) StartAll(ctx context.Context) []ServerStatus {
	for name, cfg := range m.servers {
		if !cfg.Enabled { continue }
		m.status[name] = StatusStarting
		cli, err := m.startOne(ctx, cfg)
		if err != nil { m.status[name] = StatusFailed; continue }
		m.clients[name] = cli
		m.status[name] = StatusReady
	}
	return m.Snapshot(ctx)
}

func (m *Manager) startOne(ctx context.Context, cfg *ServerConfig) (Client, error) {
	var cli Client
	switch cfg.Transport {
	case TransportHTTP:
		h := newHTTPClientFor(cfg); h.SetTimeout(cfg.Timeout); cli = h
	case TransportStdio:
		// SECURITY gap (tracked): cfg.Command/Args come straight from config.yaml
		// and exec.Command runs them verbatim — no sandboxing, no path validation,
		// no privilege drop. A future SecureProcessFactory should validate the
		// binary path, drop privileges (dedicated uid/gid), and apply resource
		// limits (RLIMIT_*) before cmd.Start(). Today the only gate is that the
		// operator authored config.yaml; stdio is a trusted extension point.
		// See "风险与兜底 → SecureProcessFactory".
		cmd := exec.Command(cfg.Command, cfg.Args...)
		stdin, err := cmd.StdinPipe(); if err != nil { return nil, err }
		stdout, err := cmd.StdoutPipe(); if err != nil { return nil, err }
		if err := cmd.Start(); err != nil { return nil, err }
		cli = NewStdioClient(stdout, stdin).SetTimeout(cfg.Timeout).SetCmd(cmd)
	default:
		return nil, fmt.Errorf("mcp: unknown transport %q", cfg.Transport)
	}
	startCtx, cancel := context.WithTimeout(ctx, 30*time.Second); defer cancel()
	if err := cli.Initialize(startCtx, "/"); err != nil { _ = cli.Close(); return nil, err }
	tools, err := cli.ListTools(startCtx); if err != nil { _ = cli.Close(); return nil, err }
	m.mu.Lock()
	for _, td := range tools { td.ServerName = cfg.Name; td.Qualified = QualifyToolName(cfg.Name, td.ToolName); m.toolMap[td.Qualified] = td }
	m.mu.Unlock()
	return cli, nil
}

// newHTTPClientFor 根据 cfg.OAuth 选择 bearer 或 OAuth 2.0 client-credentials token。
// 要求 cfg.OAuth 在 config.Load ${VAR} 展开后使用；ClientSecret 不写入日志/status。
func newHTTPClientFor(cfg *ServerConfig) *HTTPClient {
	if cfg.OAuth != nil {
		src := NewClientCredentialsSource(cfg.OAuth.TokenURL, cfg.OAuth.ClientID, cfg.OAuth.ClientSecret, cfg.OAuth.Scopes, nil)
		return NewHTTPClientWithTokenSource(cfg.URL, src)
	}
	return NewHTTPClient(cfg.URL, cfg.Bearer)
}

func (m *Manager) ListAllTools(context.Context) ([]ToolDescriptor, error) { m.mu.Lock(); defer m.mu.Unlock(); out := make([]ToolDescriptor, 0, len(m.toolMap)); for _, td := range m.toolMap { out = append(out, td) }; return out, nil }
func (m *Manager) LookupTool(_ context.Context, q string) (ToolDescriptor, bool) { m.mu.Lock(); defer m.mu.Unlock(); td, ok := m.toolMap[q]; return td, ok }
func (m *Manager) CallTool(ctx context.Context, q string, args []byte) (json.RawMessage, error) { td, ok := m.LookupTool(ctx, q); if !ok { return nil, fmt.Errorf("mcp: tool %q not found", q) }; m.mu.Lock(); cli := m.clients[td.ServerName]; m.mu.Unlock(); if cli == nil { return nil, fmt.Errorf("mcp: server unavailable") }; return cli.CallTool(ctx, td.ToolName, args) }

func (m *Manager) Disable(_ context.Context, name string) error { m.mu.Lock(); defer m.mu.Unlock(); if cli := m.clients[name]; cli != nil { _ = cli.Close() }; delete(m.clients, name); for q, td := range m.toolMap { if td.ServerName == name { delete(m.toolMap, q) } }; m.status[name] = StatusStopped; return nil }
func (m *Manager) Enable(ctx context.Context, name string) error { cfg := m.servers[name]; if cfg == nil { return fmt.Errorf("mcp: unknown server %q", name) }; cli, err := m.startOne(ctx, cfg); if err != nil { return err }; m.mu.Lock(); m.clients[name] = cli; m.status[name] = StatusReady; m.mu.Unlock(); return nil }
func (m *Manager) Validate(ctx context.Context) []ServerStatus { m.mu.Lock(); names := make([]string, 0, len(m.clients)); for n := range m.clients { names = append(names, n) }; m.mu.Unlock(); for _, n := range names { m.mu.Lock(); c := m.clients[n]; m.mu.Unlock(); if c != nil && c.Ping(ctx) == nil { m.status[n] = StatusReady } else { m.status[n] = StatusFailed } }; return m.Snapshot(ctx) }
func (m *Manager) Reload(ctx context.Context, name string) error { _ = m.Disable(ctx, name); return m.Enable(ctx, name) }
func (m *Manager) Snapshot(context.Context) []ServerStatus { m.mu.Lock(); defer m.mu.Unlock(); out := make([]ServerStatus, 0, len(m.servers)); for name, cfg := range m.servers { st := ServerStatus{Name: name, Transport: cfg.Transport, Status: m.status[name]}; for _, td := range m.toolMap { if td.ServerName == name { st.Tools = append(st.Tools, td) } }; out = append(out, st) }; return out }
func (m *Manager) Shutdown() { m.mu.Lock(); defer m.mu.Unlock(); for n, c := range m.clients { if c != nil { _ = c.Close() }; delete(m.clients, n); m.status[n] = StatusStopped }; m.toolMap = map[string]ToolDescriptor{} }
```

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/mcp/ -v`
Expected: PASS

- [ ] **Step 5: 提交**

```bash
git add internal/mcp/manager.go internal/mcp/manager_test.go
git commit -m "feat(mcp): connection manager"
```

---

## Task 8: Manager — 启动超时 + 后台健康检查 + 重连

**Files:**
- Modify: `internal/mcp/manager.go`
- Create: `internal/mcp/health.go`
- Create: `internal/mcp/health_test.go`

Task 7 的 Manager 是同步 start/stop 的最短纵切。本 task 按 V16 要求补齐：
- 启动超时可配置（`HealthConfig.StartupTimeout`，默认 30s，超时把 server 标记 `Failed` 并保留 error 字符串）
- 后台健康循环（周期 `ping`，失败标记 `Failed` 并触发 reconnect）
- 指数退避重连（`ReconnectInitialBackoff` → `ReconnectMaxBackoff`，可配 `ReconnectMaxAttempts`）
- 重连 single-flight（同一 server 并发 reconnect 合并为一次 `startOne`）
- `CallToolRetry` 在调用失败后重连一次再 retry（供外部使用；steady-state 由 health loop 兜底）
- `Snapshot.Error` 字段真实写入（之前一直为空）
- stdio 子进程环境变量显式继承 `os.Environ()` 后追加 server 配置（修复 Task 7 中 `cmd.Env` 会丢 PATH 的隐患）

- [ ] **Step 1: 写失败测试**

```go
// internal/mcp/health_test.go
package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestManager_StartupTimeout(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Method == "initialize" {
			time.Sleep(200 * time.Millisecond)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{
				"protocolVersion": "2025-06-18", "capabilities": map[string]any{},
			},
		})
	}))
	defer ts.Close()

	mgr := NewManager(map[string]*ServerConfig{"slow": {
		Name: "slow", Enabled: true, Transport: TransportHTTP, URL: ts.URL,
	}})
	mgr.SetHealthConfig(HealthConfig{StartupTimeout: 50 * time.Millisecond})
	statuses := mgr.StartAll(context.Background())
	if len(statuses) != 1 || statuses[0].Status != StatusFailed {
		t.Fatalf("expected StatusFailed, got %+v", statuses)
	}
	if statuses[0].Error == "" {
		t.Fatalf("expected non-empty Error, got %+v", statuses[0])
	}
}

func TestManager_HealthLoop_MarksFailed(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		switch req.Method {
		case "initialize":
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}}})
		case "tools/list":
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"tools": []map[string]any{}}})
		case "ping":
			w.WriteHeader(http.StatusInternalServerError)
		default:
			w.WriteHeader(http.StatusAccepted)
		}
	}))
	defer ts.Close()

	mgr := NewManager(map[string]*ServerConfig{"flaky": {
		Name: "flaky", Enabled: true, Transport: TransportHTTP, URL: ts.URL, Reconnect: false,
	}})
	mgr.SetHealthConfig(HealthConfig{Enabled: true, Interval: 20 * time.Millisecond})
	st := mgr.StartAll(context.Background())
	if len(st) != 1 || st[0].Status != StatusReady {
		t.Fatalf("StartAll should succeed, got %+v", st)
	}
	cancel := mgr.StartHealthLoop(context.Background())
	defer cancel()

	deadline := time.After(2 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatal("server never marked failed by health loop")
		default:
		}
		st = mgr.Snapshot(context.Background())
		if len(st) == 1 && st[0].Status == StatusFailed && st[0].Error != "" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestManager_CallToolRetry_Reconnects(t *testing.T) {
	var callAttempts, inits atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		switch req.Method {
		case "initialize":
			inits.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}}})
		case "tools/list":
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"tools": []map[string]any{{"name": "ping", "inputSchema": map[string]any{"type": "object"}}}}})
		case "tools/call":
			if callAttempts.Add(1) == 1 {
				w.WriteHeader(http.StatusBadGateway)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"content": []map[string]any{{"type": "text", "text": `{"ok":true}`}}}})
		case "ping":
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{}})
		default:
			w.WriteHeader(http.StatusAccepted)
		}
	}))
	defer ts.Close()

	mgr := NewManager(map[string]*ServerConfig{"r": {
		Name: "r", Enabled: true, Transport: TransportHTTP, URL: ts.URL, Reconnect: true,
	}})
	mgr.SetHealthConfig(HealthConfig{
		Enabled:                 false,
		ReconnectInitialBackoff: 1 * time.Millisecond,
		ReconnectMaxBackoff:     5 * time.Millisecond,
	})
	st := mgr.StartAll(context.Background())
	if len(st) != 1 || st[0].Status != StatusReady {
		t.Fatalf("StartAll: %+v", st)
	}

	// CallToolRetry: first CallTool fails (502) -> reconnect -> second CallTool succeeds
	res, err := mgr.CallToolRetry(context.Background(), "mcp_r_ping", []byte(`{}`))
	if err != nil {
		t.Fatalf("CallToolRetry: %v", err)
	}
	if string(res) != `{"ok":true}` {
		t.Fatalf("res=%s", res)
	}
	if callAttempts.Load() != 2 {
		t.Fatalf("expected 2 call attempts, got %d", callAttempts.Load())
	}
	if inits.Load() != 2 {
		t.Fatalf("expected 2 initialize calls (StartAll + reconnect), got %d", inits.Load())
	}
}

func TestManager_Reconnect_SingleFlight(t *testing.T) {
	var inits atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		switch req.Method {
		case "initialize":
			inits.Add(1)
			time.Sleep(40 * time.Millisecond)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{
					"protocolVersion": "2025-06-18", "capabilities": map[string]any{},
				},
			})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{}})
		}
	}))
	defer ts.Close()

	mgr := NewManager(map[string]*ServerConfig{"s": {
		Name: "s", Enabled: true, Transport: TransportHTTP, URL: ts.URL, Reconnect: true,
	}})
	mgr.SetHealthConfig(HealthConfig{
		Enabled:                 false,
		ReconnectInitialBackoff: 1 * time.Millisecond,
		ReconnectMaxBackoff:     5 * time.Millisecond,
	})
	mgr.StartAll(context.Background())

	mgr.markFailed("s", "test-failure")
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() { errs <- mgr.reconnect(context.Background(), "s") }()
	}
	for i := 0; i < 2; i++ {
		if err := <-errs; err != nil {
			t.Logf("reconnect returned %v (single-flight coalesced)", err)
		}
	}
	if inits.Load() != 2 {
		t.Fatalf("single-flight broken: inits=%d", inits.Load())
	}
}

func TestManager_reconnect_InFlightConcurrency(t *testing.T) {
	// placeholder for additional concurrency regression
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/mcp/ -run 'TestManager_StartupTimeout|TestManager_HealthLoop|TestManager_CallToolRetry|TestManager_Reconnect' -v`
Expected: FAIL（`HealthConfig`、`SetHealthConfig`、`StartHealthLoop`、`markFailed`、`reconnect`、`CallToolRetry` 均未定义）

- [ ] **Step 3: 修改 manager.go（新字段 + StartupTimeout + env 继承 + errs 同步）**

把 `manager.go` 中的 `Manager` struct 与相关方法替换为：

```go
type Manager struct {
	mu                sync.Mutex
	servers           map[string]*ServerConfig
	clients           map[string]Client
	toolMap           map[string]ToolDescriptor
	status            map[string]ConnectionStatus
	errs              map[string]string
	health            HealthConfig
	reconnectInflight map[string]struct{}
}

func NewManager(servers map[string]*ServerConfig) *Manager {
	m := &Manager{
		servers:           map[string]*ServerConfig{},
		clients:           map[string]Client{},
		toolMap:           map[string]ToolDescriptor{},
		status:            map[string]ConnectionStatus{},
		errs:              map[string]string{},
		reconnectInflight: map[string]struct{}{},
		health:            DefaultHealthConfig(),
	}
	for name, sc := range servers {
		cp := *sc
		cp.Name = name
		m.servers[name] = &cp
		if !cp.Enabled {
			m.status[name] = StatusStopped
		}
	}
	return m
}

func (m *Manager) Enabled() bool {
	return m != nil && len(m.servers) > 0
}

func (m *Manager) StartAll(ctx context.Context) []ServerStatus {
	m.mu.Lock()
	names := make([]string, 0, len(m.servers))
	for name, cfg := range m.servers {
		if cfg.Enabled {
			names = append(names, name)
			m.status[name] = StatusStarting
		}
	}
	m.mu.Unlock()

	for _, name := range names {
		m.mu.Lock()
		cfg := m.servers[name]
		m.mu.Unlock()
		cli, err := m.startOne(ctx, cfg)
		if err != nil {
			m.markFailed(name, err.Error())
			continue
		}
		m.mu.Lock()
		m.clients[name] = cli
		m.status[name] = StatusReady
		delete(m.errs, name)
		m.mu.Unlock()
	}
	return m.Snapshot(ctx)
}

func (m *Manager) startOne(ctx context.Context, cfg *ServerConfig) (Client, error) {
	var cli Client
	switch cfg.Transport {
	case TransportHTTP:
		h := newHTTPClientFor(cfg)
		h.SetTimeout(cfg.Timeout)
		cli = h
	case TransportStdio:
		// SECURITY gap (tracked): cfg.Command/Args come straight from config.yaml
		// and exec.Command runs them verbatim — no sandboxing, no path validation,
		// no privilege drop. A future SecureProcessFactory should validate the
		// binary path, drop privileges (dedicated uid/gid), and apply resource
		// limits (RLIMIT_*) before cmd.Start(). Today the only gate is that the
		// operator authored config.yaml; stdio is a trusted extension point.
		// See "风险与兜底 → SecureProcessFactory".
		cmd := exec.Command(cfg.Command, cfg.Args...)
		// 继承父进程环境（PATH/HOME/等）再叠加 server 配置，避免子进程找不到命令。
		env := os.Environ()
		for k, v := range cfg.Env {
			env = append(env, k+"="+v)
		}
		cmd.Env = env
		stdin, err := cmd.StdinPipe()
		if err != nil {
			return nil, fmt.Errorf("stdin pipe: %w", err)
		}
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			return nil, fmt.Errorf("stdout pipe: %w", err)
		}
		if err := cmd.Start(); err != nil {
			return nil, fmt.Errorf("start %q: %w", cfg.Command, err)
		}
		cli = NewStdioClient(stdout, stdin).SetTimeout(cfg.Timeout).SetCmd(cmd)
	default:
		return nil, fmt.Errorf("mcp: unknown transport %q", cfg.Transport)
	}
	timeout := m.health.StartupTimeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	startCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := cli.Initialize(startCtx, "/"); err != nil {
		_ = cli.Close()
		return nil, fmt.Errorf("initialize: %w", err)
	}
	tools, err := cli.ListTools(startCtx)
	if err != nil {
		_ = cli.Close()
		return nil, fmt.Errorf("tools/list: %w", err)
	}
	m.mu.Lock()
	for _, td := range tools {
		td.ServerName = cfg.Name
		td.Qualified = QualifyToolName(cfg.Name, td.ToolName)
		if _, exists := m.toolMap[td.Qualified]; exists {
			m.mu.Unlock()
			_ = cli.Close()
			return nil, fmt.Errorf("mcp: tool name collision on %q (server %q)", td.Qualified, cfg.Name)
		}
		m.toolMap[td.Qualified] = td
	}
	m.mu.Unlock()
	return cli, nil
}

// newHTTPClientFor 根据 cfg.OAuth 选择 bearer 或 OAuth 2.0 client-credentials token。
// 要求 cfg.OAuth 在 config.Load ${VAR} 展开后使用；ClientSecret 不写入日志/status。
func newHTTPClientFor(cfg *ServerConfig) *HTTPClient {
	if cfg.OAuth != nil {
		src := NewClientCredentialsSource(cfg.OAuth.TokenURL, cfg.OAuth.ClientID, cfg.OAuth.ClientSecret, cfg.OAuth.Scopes, nil)
		return NewHTTPClientWithTokenSource(cfg.URL, src)
	}
	return NewHTTPClient(cfg.URL, cfg.Bearer)
}

func (m *Manager) ListAllTools(context.Context) ([]ToolDescriptor, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]ToolDescriptor, 0, len(m.toolMap))
	for _, td := range m.toolMap {
		out = append(out, td)
	}
	return out, nil
}

func (m *Manager) LookupTool(_ context.Context, q string) (ToolDescriptor, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	td, ok := m.toolMap[q]
	return td, ok
}

func (m *Manager) CallTool(ctx context.Context, q string, args []byte) (json.RawMessage, error) {
	td, ok := m.LookupTool(ctx, q)
	if !ok {
		return nil, fmt.Errorf("mcp: tool %q not found", q)
	}
	m.mu.Lock()
	cli := m.clients[td.ServerName]
	m.mu.Unlock()
	if cli == nil {
		return nil, fmt.Errorf("mcp: server %q unavailable", td.ServerName)
	}
	return cli.CallTool(ctx, td.ToolName, args)
}

func (m *Manager) Disable(_ context.Context, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if cli := m.clients[name]; cli != nil {
		_ = cli.Close()
	}
	delete(m.clients, name)
	for q, td := range m.toolMap {
		if td.ServerName == name {
			delete(m.toolMap, q)
		}
	}
	m.status[name] = StatusStopped
	delete(m.errs, name)
	return nil
}

func (m *Manager) Enable(ctx context.Context, name string) error {
	m.mu.Lock()
	cfg := m.servers[name]
	m.mu.Unlock()
	if cfg == nil {
		return fmt.Errorf("mcp: unknown server %q", name)
	}
	cli, err := m.startOne(ctx, cfg)
	if err != nil {
		m.markFailed(name, err.Error())
		return err
	}
	m.mu.Lock()
	m.clients[name] = cli
	m.status[name] = StatusReady
	delete(m.errs, name)
	m.mu.Unlock()
	return nil
}

func (m *Manager) Validate(ctx context.Context) []ServerStatus {
	m.mu.Lock()
	names := make([]string, 0, len(m.clients))
	for n := range m.clients {
		names = append(names, n)
	}
	m.mu.Unlock()
	for _, n := range names {
		m.mu.Lock()
		c := m.clients[n]
		m.mu.Unlock()
		if c == nil {
			continue
		}
		pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err := c.Ping(pingCtx)
		cancel()
		if err != nil {
			m.markFailed(n, "ping: "+err.Error())
		} else {
			m.mu.Lock()
			m.status[n] = StatusReady
			delete(m.errs, n)
			m.mu.Unlock()
		}
	}
	return m.Snapshot(ctx)
}

func (m *Manager) Reload(ctx context.Context, name string) error {
	_ = m.Disable(ctx, name)
	return m.Enable(ctx, name)
}

func (m *Manager) Snapshot(context.Context) []ServerStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]ServerStatus, 0, len(m.servers))
	for name, cfg := range m.servers {
		st := ServerStatus{
			Name: name, Transport: cfg.Transport,
			Status: m.status[name], Error: m.errs[name],
		}
		for _, td := range m.toolMap {
			if td.ServerName == name {
				st.Tools = append(st.Tools, td)
			}
		}
		out = append(out, st)
	}
	return out
}

func (m *Manager) Shutdown() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for n, c := range m.clients {
		if c != nil {
			_ = c.Close()
		}
		delete(m.clients, n)
		m.status[n] = StatusStopped
	}
	m.toolMap = map[string]ToolDescriptor{}
}
```

注意 imports 需要追加 `"os"`（用于 `os.Environ`）。

- [ ] **Step 4: 实现 health.go**

```go
// internal/mcp/health.go
package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"
)

// HealthConfig controls Manager background health checking and reconnection.
// Zero values fall back to DefaultHealthConfig() in SetHealthConfig.
type HealthConfig struct {
	// Enabled turns on the background health ticker. If false, StartHealthLoop
	// returns a no-op cancel func and steady-state reconnection is disabled.
	Enabled bool
	// Interval between health checks. Default 60s.
	Interval time.Duration
	// StartupTimeout caps the total time spent in startOne (Initialize +
	// tools/list). Default 30s. A server that does not become Ready within
	// this window is marked Failed with the underlying error.
	StartupTimeout time.Duration
	// ReconnectMaxAttempts caps reconnection attempts per failure. 0 = unlimited.
	ReconnectMaxAttempts int
	// ReconnectInitialBackoff is the first backoff delay between attempts.
	// Default 1s.
	ReconnectInitialBackoff time.Duration
	// ReconnectMaxBackoff caps the exponential backoff. Default 60s.
	ReconnectMaxBackoff time.Duration
}

// DefaultHealthConfig returns production-safe defaults.
func DefaultHealthConfig() HealthConfig {
	return HealthConfig{
		Enabled:                 true,
		Interval:                60 * time.Second,
		StartupTimeout:          30 * time.Second,
		ReconnectInitialBackoff: 1 * time.Second,
		ReconnectMaxBackoff:     60 * time.Second,
	}
}

// SetHealthConfig replaces the manager's health configuration. Apply before
// StartHealthLoop. Safe to call concurrently with other Manager methods.
func (m *Manager) SetHealthConfig(cfg HealthConfig) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if cfg.Interval == 0 {
		cfg.Interval = 60 * time.Second
	}
	if cfg.StartupTimeout == 0 {
		cfg.StartupTimeout = 30 * time.Second
	}
	if cfg.ReconnectInitialBackoff == 0 {
		cfg.ReconnectInitialBackoff = 1 * time.Second
	}
	if cfg.ReconnectMaxBackoff == 0 {
		cfg.ReconnectMaxBackoff = 60 * time.Second
	}
	m.health = cfg
}

// StartHealthLoop spawns a goroutine that pings each Ready server at
// m.health.Interval. A failed ping marks the server Failed and schedules a
// background reconnect with exponential backoff up to ReconnectMaxBackoff.
// The returned cancel func stops the loop but does not shut down clients
// (call Shutdown for that).
func (m *Manager) StartHealthLoop(ctx context.Context) context.CancelFunc {
	m.mu.Lock()
	cfg := m.health
	m.mu.Unlock()
	if !cfg.Enabled {
		return func() {}
	}
	loopCtx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		t := time.NewTicker(cfg.Interval)
		defer t.Stop()
		for {
			select {
			case <-loopCtx.Done():
				return
			case <-t.C:
				m.healthOnce(loopCtx)
			}
		}
	}()
	return func() {
		cancel()
		wg.Wait()
	}
}

func (m *Manager) healthOnce(ctx context.Context) {
	m.mu.Lock()
	names := make([]string, 0, len(m.clients))
	for n := range m.clients {
		names = append(names, n)
	}
	m.mu.Unlock()
	for _, name := range names {
		m.mu.Lock()
		cli := m.clients[name]
		cfg := m.servers[name]
		m.mu.Unlock()
		if cli == nil || cfg == nil {
			continue
		}
		pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err := cli.Ping(pingCtx)
		cancel()
		if err == nil {
			continue
		}
		m.markFailed(name, "ping: "+err.Error())
		if cfg.Reconnect {
			go func() { _ = m.reconnect(context.Background(), name) }()
		}
	}
}

// markFailed records the failure reason and status under the manager mutex.
// Safe to call concurrently with all Manager methods.
func (m *Manager) markFailed(name, reason string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.status[name] = StatusFailed
	m.errs[name] = reason
}

// reconnect attempts to ring a failed/disconnected server back to Ready
// using exponential backoff. Single-flight per server: concurrent calls for
// the same name coalesce — only the first triggers startOne, others return a
// sentinel error. Returns nil on successful reconnection.
func (m *Manager) reconnect(ctx context.Context, name string) error {
	m.mu.Lock()
	if _, running := m.reconnectInflight[name]; running {
		m.mu.Unlock()
		return fmt.Errorf("mcp: reconnect already in flight for %q", name)
	}
	m.reconnectInflight[name] = struct{}{}
	cfg := m.servers[name]
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		delete(m.reconnectInflight, name)
		m.mu.Unlock()
	}()
	if cfg == nil {
		return fmt.Errorf("mcp: unknown server %q", name)
	}
	if !cfg.Reconnect {
		return fmt.Errorf("mcp: server %q has reconnect disabled", name)
	}
	// Tear down any lingering client before retrying so HTTP session IDs and
	// stdio pipes do not leak across attempts.
	_ = m.Disable(ctx, name)
	m.mu.Lock()
	cfgCopy := *cfg
	health := m.health
	m.mu.Unlock()
	backoff := health.ReconnectInitialBackoff
	attempt := 0
	for {
		attempt++
		if health.ReconnectMaxAttempts > 0 && attempt > health.ReconnectMaxAttempts {
			return fmt.Errorf("mcp: reconnect exhausted for %q after %d attempts", name, attempt-1)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		cli, err := m.startOne(ctx, &cfgCopy)
		if err == nil {
			m.mu.Lock()
			m.clients[name] = cli
			m.status[name] = StatusReady
			delete(m.errs, name)
			m.mu.Unlock()
			return nil
		}
		m.markFailed(name, fmt.Sprintf("reconnect attempt %d: %v", attempt, err))
		backoff *= 2
		if backoff > health.ReconnectMaxBackoff {
			backoff = health.ReconnectMaxBackoff
		}
	}
}

// CallToolRetry is Manager.CallTool with a one-shot reconnect on failure.
// Use for ad-hoc external callers; steady-state coverage is the health loop.
func (m *Manager) CallToolRetry(ctx context.Context, qualified string, args []byte) (json.RawMessage, error) {
	res, err := m.CallTool(ctx, qualified, args)
	if err == nil {
		return res, nil
	}
	td, ok := m.LookupTool(ctx, qualified)
	if !ok {
		return nil, fmt.Errorf("mcp: tool %q not found: %w", qualified, err)
	}
	if reconnectErr := m.reconnect(ctx, td.ServerName); reconnectErr != nil {
		return nil, fmt.Errorf("mcp: tool %q call failed (%v); reconnect failed: %w", qualified, err, reconnectErr)
	}
	return m.CallTool(ctx, qualified, args)
}
```

- [ ] **Step 5: 跑测试确认通过**

Run: `go test ./internal/mcp/ -v`
Expected: PASS（包括 Task 7 的 StartDisableEnableRoute、Task 8 的四条新测试）

- [ ] **Step 6: 提交**

```bash
git add internal/mcp/manager.go internal/mcp/health.go internal/mcp/health_test.go
git commit -m "feat(mcp): startup timeout, health loop, exponential reconnect with single-flight"
```

---

## Task 9: Context 注入

**Files:**
- Create: `internal/tools/mcpctx.go`
- Create: `internal/tools/mcpctx_test.go`

- [ ] **Step 1: 写失败测试**

```go
// internal/tools/mcpctx_test.go
package tools

import (
	"context"
	"testing"
	"github.com/x6nux/yanshi/internal/mcp"
)

func TestMCPContext(t *testing.T) {
	if _, ok := MCPFromContext(context.Background()); ok { t.Fatal("unexpected") }
	mgr := mcp.NewManager(nil)
	got, ok := MCPFromContext(WithMCP(context.Background(), mgr))
	if !ok || got != mgr { t.Fatal("round-trip failed") }
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/tools/ -run TestMCPContext -v`
Expected: FAIL

- [ ] **Step 3: 实现 mcpctx.go**

```go
// internal/tools/mcpctx.go
package tools

import (
	"context"
	"github.com/x6nux/yanshi/internal/mcp"
)

type mcpCtxKey struct{}
func WithMCP(ctx context.Context, mgr *mcp.Manager) context.Context { if mgr == nil { return ctx }; return context.WithValue(ctx, mcpCtxKey{}, mgr) }
func MCPFromContext(ctx context.Context) (*mcp.Manager, bool) { m, ok := ctx.Value(mcpCtxKey{}).(*mcp.Manager); return m, ok }
```

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/tools/ -run TestMCPContext -v`
Expected: PASS

- [ ] **Step 5: 提交**

```bash
git add internal/tools/mcpctx.go internal/tools/mcpctx_test.go
git commit -m "feat(tools): MCP context injection"
```

---

## Task 10: MCP 独立权限门禁（默认 deny）

**Files:**
- Modify: `internal/guard/profile.go`
- Modify: `internal/guard/guard.go`
- Modify: `internal/guard/guard_test.go`
- Modify: `config.example.yaml`

现有默认 orchestrator profile 是 `Tools.Allow: ["*"]`。如果只复用 tools 维度，所有新 MCP 工具会被自动放行，违反 V16“每个 server 工具默认 deny，只有 profile 显式放行”的要求。本 task 新增独立 `MCP ToolsPerm` 维度；`mcp_*` action 必须同时通过 `MCP.Allow` 和 `Tools.Allow`。

- [ ] **Step 1: 写失败测试**

```go
// 追加到 internal/guard/guard_test.go
func TestGuard_MCPDefaultDenyDespiteToolsWildcard(t *testing.T) {
	profile := PermissionProfile{
		Tools: ToolsPerm{Allow: []string{"*"}},
	}
	got := New().Check(profile, Action{Tool: "mcp_github_create_issue"})
	if got.Allowed {
		t.Fatalf("MCP tool must be denied when MCP.Allow is empty: %+v", got)
	}
	if got.Reason != "no MCP tools permitted by profile" {
		t.Fatalf("reason = %q", got.Reason)
	}
}

func TestGuard_MCPExplicitServerGlobAllows(t *testing.T) {
	profile := PermissionProfile{
		Tools: ToolsPerm{Allow: []string{"*"}},
		MCP:   ToolsPerm{Allow: []string{"mcp_github_*"}},
	}
	got := New().Check(profile, Action{Tool: "mcp_github_create_issue"})
	if !got.Allowed {
		t.Fatalf("explicit MCP server glob should allow: %+v", got)
	}
}

func TestGuard_MCPAllowStillRequiresGeneralToolAllow(t *testing.T) {
	profile := PermissionProfile{
		Tools: ToolsPerm{Allow: []string{"fs_*"}},
		MCP:   ToolsPerm{Allow: []string{"mcp_github_*"}},
	}
	got := New().Check(profile, Action{Tool: "mcp_github_create_issue"})
	if got.Allowed {
		t.Fatalf("MCP.Allow must not bypass Tools.Allow: %+v", got)
	}
}

func TestGuard_NonMCPToolIgnoresMCPAllow(t *testing.T) {
	profile := PermissionProfile{
		Tools: ToolsPerm{Allow: []string{"fs_read"}},
	}
	got := New().Check(profile, Action{Tool: "fs_read"})
	if !got.Allowed {
		t.Fatalf("non-MCP behavior changed: %+v", got)
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/guard/ -run TestGuard_MCP -v`
Expected: FAIL（`PermissionProfile.MCP` 不存在，通用 `*` 会错误放行 MCP 工具）

- [ ] **Step 3: 改 profile.go**

把 `PermissionProfile` 完整替换为：

```go
// PermissionProfile scopes what an agent is allowed to do.
type PermissionProfile struct {
	FS    FSPerm    `yaml:"fs"`
	Tools ToolsPerm `yaml:"tools"`
	// MCP is an additional fail-closed gate for dynamically discovered MCP tools.
	// A tool named mcp_<server>_<tool> must match BOTH MCP.Allow and Tools.Allow.
	// Empty MCP.Allow denies every MCP tool even when Tools.Allow contains "*".
	MCP   ToolsPerm `yaml:"mcp"`
	Shell ShellPerm `yaml:"shell"`
	Net   NetPerm   `yaml:"net"`
}
```

- [ ] **Step 4: 改 guard.go**

把 `Check` 与新增 helper 写成：

```go
// Check returns whether the profile permits the action, checking every
// applicable dimension. The first failing dimension short-circuits.
func (g *Guard) Check(p PermissionProfile, a Action) Decision {
	if d := g.checkMCPTools(p, a); !d.Allowed {
		return d
	}
	if d := g.checkTools(p, a); !d.Allowed {
		return d
	}
	if d := g.checkFS(p, a); !d.Allowed {
		return d
	}
	if d := g.checkShell(p, a); !d.Allowed {
		return d
	}
	if d := g.checkNet(p, a); !d.Allowed {
		return d
	}
	return Decision{Allowed: true}
}

// checkMCPTools applies only to runtime names with the reserved mcp_ prefix.
// It is deliberately separate from checkTools so a road Tools.Allow pattern
// (notably the historical "*") cannot silently authorize newly configured MCP
// servers. The profile must opt in to the exact server/tool name or a matching
// MCP-specific glob.
func (g *Guard) checkMCPTools(p PermissionProfile, a Action) Decision {
	if !strings.HasPrefix(a.Tool, "mcp_") {
		return Decision{Allowed: true}
	}
	if len(p.MCP.Allow) == 0 {
		return Decision{false, "no MCP tools permitted by profile"}
	}
	name := filepath.ToSlash(a.Tool)
	for _, pat := range p.MCP.Allow {
		if ok, err := MatchGlob(filepath.ToSlash(pat), name); err == nil && ok {
			return Decision{Allowed: true}
		}
	}
	return Decision{false, fmt.Sprintf("MCP tool %q not permitted", a.Tool)}
}
```

现有 imports 已有 `fmt`、`filepath`、`strings`，无需新增 import。

- [ ] **Step 5: 更新 config.example.yaml**

把 profile 示例改成显式 MCP allow：

```yaml
profiles:
  orchestrator:
    fs: { read: ["**"], write: [] }
    tools: { allow: ["*"] }
    # Empty or absent mcp.allow denies every mcp_<server>_<tool>, even though
    # tools.allow contains "*". Opt in per trusted server/tool.
    mcp: { allow: [] }
    shell: { policy: "deny" }
    net: { allow: false }
  coding:
    tools: { allow: ["*"] }
    # Example: permit only tools discovered from server "github".
    mcp:   { allow: ["mcp_github_*"] }
    fs:    { read: ["**"], write: ["**"] }
    shell: { policy: "allowlist", patterns: ["go *", "git *", "npm *"] }
    net:   { allow: true, hosts: ["*"] }
```

- [ ] **Step 6: 跑测试确认通过**

Run: `go test ./internal/guard/ ./internal/tools/ -v`
Expected: PASS；已有非 MCP guard 测试不变，新增 MCP 测试证明默认 deny。

- [ ] **Step 7: 提交**

```bash
git add internal/guard/profile.go internal/guard/guard.go internal/guard/guard_test.go config.example.yaml
git commit -m "feat(guard): fail-closed MCP permission dimension"
```

---

## Task 11: 工具桥（NewMCPTools）

**Files:**
- Create: `internal/tools/mcp.go`
- Create: `internal/tools/mcp_test.go`

- [ ] **Step 1: 写失败测试**

```go
// internal/tools/mcp_test.go
package tools

import (
	"context"
	"strings"
	"testing"
	"github.com/x6nux/yanshi/internal/guard"
	"github.com/x6nux/yanshi/internal/mcp"
)

func TestMCPToolBridge_PermissionAndCall(t *testing.T) {
	ts, _ := mcp.NewFakeHTTPServer([]mcp.ToolDescriptor{{ToolName: "echo"}})
	defer ts.Close()
	mgr := mcp.NewManager(map[string]*mcp.ServerConfig{"s": {Name: "s", Enabled: true, Transport: mcp.TransportHTTP, URL: ts.URL}})
	mgr.StartAll(context.Background())
	list := NewMCPTools(mgr)
	if len(list) != 1 { t.Fatalf("len=%d", len(list)) }
	// MCP 工具必须同时通过 MCP.Allow 和通用 Tools.Allow。
	allowed := WithMCP(WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"mcp_*"}},
		MCP:   guard.ToolsPerm{Allow: []string{"mcp_s_*"}},
	}), mgr)
	res, err := list[0].InvokableRun(allowed, `{}`)
	if err != nil || !strings.Contains(res, `"ok":true`) { t.Fatalf("res=%s err=%v", res, err) }

	// fail-closed: MCP.Allow 空 → 拒绝，即使 Tools.Allow 是 "*"。
	noMCP := WithMCP(WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"*"}},
	}), mgr)
	res, _ = list[0].InvokableRun(noMCP, `{}`)
	if !strings.Contains(res, "permission denied") { t.Fatalf("expected MCP deny, got %s", res) }

	// fail-closed: MCP.Allow 放行但通用 Tools.Allow 未覆盖 → 仍拒绝。
	mcpOnly := WithMCP(WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"fs_*"}},
		MCP:   guard.ToolsPerm{Allow: []string{"mcp_s_*"}},
	}), mgr)
	res, _ = list[0].InvokableRun(mcpOnly, `{}`)
	if !strings.Contains(res, "permission denied") { t.Fatalf("expected Tools deny, got %s", res) }
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/tools/ -run TestMCPToolBridge -v`
Expected: FAIL

- [ ] **Step 3: 实现 mcp.go**

```go
// internal/tools/mcp.go
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
	"github.com/cloudwego/eino/schema"
	"github.com/x6nux/yanshi/internal/mcp"
)

func NewMCPTools(mgr *mcp.Manager) []*GuardedTool {
	if mgr == nil || !mgr.Enabled() { return nil }
	descs, err := mgr.ListAllTools(context.Background())
	if err != nil { return nil }
	out := make([]*GuardedTool, 0, len(descs))
	for _, td := range descs {
		td := td
		desc := td.Description; if desc == "" { desc = fmt.Sprintf("MCP tool from %q", td.ServerName) }
		p := buildMCPParams(td.InputSchema)
		if p == nil { p = params(map[string]*schema.ParameterInfo{"arguments": &schema.ParameterInfo{Type: schema.String, Desc: "tool arguments as JSON"}}) }
		out = append(out, NewGuardedTool(td.Qualified, "MCP:"+td.ServerName, desc, 5*time.Minute, p, SyncStream(func(ctx context.Context, args string) (string, error) {
			bound, ok := MCPFromContext(ctx); if !ok { return errorResult("MCP manager not bound"), nil }
			res, err := bound.CallTool(ctx, td.Qualified, []byte(args)); if err != nil { return errorResult(err.Error()), nil }
			return string(res), nil
		})))
	}
	return out
}

func buildMCPParams(raw string) *schema.ParamsOneOf {
	var doc map[string]any
	if json.Unmarshal([]byte(raw), &doc) != nil { return nil }
	props, _ := doc["properties"].(map[string]any)
	infos := make(map[string]*schema.ParameterInfo, len(props))
	for name, pv := range props { p, _ := pv.(map[string]any); typ, _ := p["type"].(string); desc, _ := p["description"].(string); infos[name] = &schema.ParameterInfo{Type: schema.Type(typ), Desc: desc} }
	return params(infos)
}
```

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/tools/ -run TestMCPToolBridge -v`
Expected: PASS

- [ ] **Step 5: 提交**

```bash
git add internal/tools/mcp.go internal/tools/mcp_test.go
git commit -m "feat(tools): MCP GuardedTool ridge"
```

---

## Task 12: 配置模型

**Files:**
- Modify: `internal/config/config.go`
- Create: `internal/config/mcp_test.go`
- Modify: `config.example.yaml`

- [ ] **Step 1: 写失败测试**

```go
// internal/config/mcp_test.go
package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMCPConfig_Parse(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	data := []byte("mcp:\n  servers:\n    s:\n      enabled: true\n      transport: http\n      url: http://localhost/mcp\n      timeout: 30s\n")
	if err := os.WriteFile(path, data, 0644); err != nil { t.Fatal(err) }
	cfg, err := Load(path); if err != nil { t.Fatal(err) }
	if cfg.MCP.Servers["s"].URL != "http://localhost/mcp" { t.Fatalf("cfg=%+v", cfg.MCP) }
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/config/ -run TestMCPConfig -v`
Expected: FAIL

- [ ] **Step 3: 改 config.go**

```go
// 在 Config 中新增：
MCP MCPConfig `yaml:"mcp"`

// 新增类型：
type MCPConfig struct { Servers map[string]*MCPServerConfig `yaml:"servers"` }
type MCPServerConfig struct {
	Enabled bool `yaml:"enabled"`
	Transport string `yaml:"transport"`
	Command string `yaml:"command,omitempty"`
	Args []string `yaml:"args,omitempty"`
	Env map[string]string `yaml:"env,omitempty"`
	URL string `yaml:"url,omitempty"`
	Bearer string `yaml:"bearer,omitempty"`
	Timeout string `yaml:"timeout,omitempty"`
	Reconnect bool `yaml:"reconnect,omitempty"`
}
```

- [ ] **Step 4: 改 config.example.yaml**

```yaml
mcp:
  servers: {}
  # my-tools:
  #   enabled: true
  #   transport: http
  #   url: "http://localhost:3000/mcp"
  #   bearer: "${MCP_BEARER_TOKEN}"
  #   timeout: 30s
```

- [ ] **Step 5: 跑测试确认通过**

Run: `go test ./internal/config/ -v`
Expected: PASS

- [ ] **Step 6: 提交**

```bash
git add internal/config/config.go internal/config/mcp_test.go config.example.yaml
git commit -m "feat(config): MCP server config"
```

---

## Task 13: Bootstrap + Orchestrator 装配

**Files:**
- Modify: `internal/bootstrap/bootstrap.go`
- Modify: `internal/agent/orchestrator/orchestrator.go`
- Modify: `internal/api/http/server.go`

- [ ] **Step 1: 写失败测试**

```go
// internal/bootstrap/bootstrap_mcp_test.go
package bootstrap

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/x6nux/yanshi/internal/mcp"
)

// buildFakeApp writes a minimal config.yaml (extended with extraYAML) to a
// temp dir, builds the app with FakeModel, and returns it plus a cleanup.
// Mirrors the existing TestBuild_FakeModel harness in bootstrap_test.go.
func buildFakeApp(t *testing.T, extraYAML string) (*App, func()) {
	t.Helper()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	dbPath := toYAMLPath(filepath.Join(dir, "test.db"))
	cfg := "server:
  http_addr: \"127.0.0.1:0\"
storage:
  sqlite_path: \"" + dbPath + "\"
token: \"test-token\"
" + extraYAML
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	app, err := Build(Options{ConfigPath: cfgPath, FakeModel: true})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	return app, func() { _ = app.Shutdown(context.Background()) }
}

// TestBuild_MCP_EmptyConfig ensures App.MCP is always non-nil (soft-degrade):
// an empty mcp.servers map yields a disabled manager whose Enabled()==false,
// so NewMCPTools can be called unconditionally without a nil check.
func TestBuild_MCP_EmptyConfig(t *testing.T) {
	app, cleanup := buildFakeApp(t, "")
	defer cleanup()
	if app.MCP == nil {
		t.Fatal("App.MCP must be non-nil even with no mcp servers")
	}
	if app.MCP.Enabled() {
		t.Fatal("App.MCP should be disabled when the servers map is empty")
	}
	if snaps := app.MCP.Snapshot(context.Background()); len(snaps) != 0 {
		t.Fatalf("expected zero servers in snapshot, got %+v", snaps)
	}
}

// TestBuild_MCP_FakeHTTPServer wires a real mcp.NewFakeHTTPServer through
// Build's config.yaml and asserts StartAll marked the server ready.
func TestBuild_MCP_FakeHTTPServer(t *testing.T) {
	ts, _ := mcp.NewFakeHTTPServer([]mcp.ToolDescriptor{{ToolName: "echo"}})
	defer ts.Close()
	extra := "
mcp:
  servers:
    s:
      enabled: true
      transport: http
      url: \"" + ts.URL + "\"
"
	app, cleanup := buildFakeApp(t, extra)
	defer cleanup()
	if app.MCP == nil || !app.MCP.Enabled() {
		t.Fatal("App.MCP must be non-nil and enabled with a configured server")
	}
	snaps := app.MCP.Snapshot(context.Background())
	if len(snaps) != 1 || snaps[0].Status != mcp.StatusReady {
		t.Fatalf("expected server ready, got %+v", snaps)
	}
}
```

- [ ] **Step 2: 改 bootstrap.go**

```go
// App 新增：
MCP *mcp.Manager

// Build 中 VCS 初始化后调用：
mcpManager := buildMCPManager(cfg)

// 工具列表追加：
for _, t := range tools.NewMCPTools(mcpManager) { allTools = append(allTools, t) }

// orchConfig 加：
MCP: mcpManager,

// apihttp.Config 加：
MCP: mcpManager,

// App 返回加：
MCP: mcpManager,

// Shutdown 中加：
if a.MCP != nil { a.MCP.Shutdown() }

func buildMCPManager(cfg *config.Config) *mcp.Manager {
	servers := make(map[string]*mcp.ServerConfig, len(cfg.MCP.Servers))
	for name, sc := range cfg.MCP.Servers {
		d := 30 * time.Second
		if sc.Timeout != "" { if parsed, err := time.ParseDuration(sc.Timeout); err == nil { d = parsed } }
		transport := mcp.TransportStdio; if sc.Transport == "http" { transport = mcp.TransportHTTP }
		servers[name] = &mcp.ServerConfig{Name: name, Enabled: sc.Enabled, Transport: transport, Command: sc.Command, Args: sc.Args, Env: sc.Env, URL: sc.URL, Bearer: sc.Bearer, Timeout: d, Reconnect: sc.Reconnect}
	}
	mgr := mcp.NewManager(servers)
	for _, st := range mgr.StartAll(context.Background()) { if st.Status == mcp.StatusFailed { fmt.Fprintf(os.Stderr, "yanshi: mcp server %q failed\n", st.Name) } }
	return mgr
}
```

**改 apihttp (server.go)：** 在 `Config` struct 和 `Server` struct 分别加 `MCP` 字段，`New` 保存（Task 14 仅加 proto 帧 + WS dispatch，依赖这里已存在 `s.mcp`）：

```go
// Config 新增：
MCP *mcp.Manager

// Server 新增：
mcp *mcp.Manager

// New 中保存 cfg.MCP：
mcp: cfg.MCP,
```

- [ ] **Step 3: 改 orchestrator.go**

```go
// Config 增加：
MCP *mcp.Manager
// Orchestrator 增加：
mcpMgr *mcp.Manager
// New 保存：
mcpMgr: cfg.MCP,
// 每个 turn 入口 Query/Events/EventsWithHistory/EventsWithHistoryOpts 注入：
if o.mcpMgr != nil && o.mcpMgr.Enabled() { ctx = tools.WithMCP(ctx, o.mcpMgr) }
// runSubAgentTurn 构建嵌套 orchestrator 的 Config{} 中加（与 Model/Tools/Profile/MaxIters 同级）：
//   sub, err := New(Config{
//       Model:    o.model,
//       Tools:    selected,
//       Profile:  o.profile,
//       MCP:      o.mcpMgr,   // ← 子代理继承父级 MCP manager
//       ...
//   })
// 嵌套 New 存其为 sub.mcpMgr；子代理每个 turn 入口同样注入
// tools.WithMCP(ctx, o.mcpMgr)，MCP 桥从 ctx 读 mgr 后才转发 CallTool。
// 注意：子代理工具列表来自 selectSubAgentTools(allowed)（已含 NewMCPTools
// 产生的 GuardedTool），无需在子代理中重新调用 NewMCPTools。
MCP: o.mcpMgr,
```

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/bootstrap/ ./internal/agent/orchestrator/ -v`
Expected: PASS

- [ ] **Step 5: 提交**

```bash
git add internal/bootstrap/bootstrap.go internal/agent/orchestrator/orchestrator.go
git commit -m "feat(mcp): bootstrap and orchestrator wiring"
```

---

## Task 14: proto + WS 管理帧

**Files:**
- Modify: `internal/proto/frame.go`
- Modify: `internal/api/http/ws.go`

- [ ] **Step 1: 写失败测试**

```go
// TestMCPFrames (package proto, bare names) 验证新帧类型与构造器。
func TestMCPFrames(t *testing.T) {
	cf := NewMCPAction("s", "enable")
	if cf.Type != "mcp_action" || cf.MCPServer != "s" { t.Fatal(cf) }
	sf := NewMCPStatusFrame([]MCPServerStatus{{Name: "s", Status: "ready", Tools: []MCPToolBrief{}}})
	if sf.Type != "mcp_status" { t.Fatal(sf) }
}
```

- [ ] **Step 2: 改 frame.go**

```go
// ClientFrame 新增：
MCPServer string `json:"mcp_server,omitempty"`
MCPAction string `json:"mcp_action,omitempty"`
// ServerFrame 新增：
MCPServers []MCPServerStatus `json:"mcp_servers,omitempty"`

// 类型与构造器：
type MCPServerStatus struct {
	Name      string         `json:"name"`
	Transport string         `json:"transport"`
	Status    string         `json:"status"`
	Error     string         `json:"error,omitempty"`
	ToolCount int            `json:"tool_count"`
	Tools     []MCPToolBrief `json:"tools,omitempty"`
}

// MCPToolBrief 是 palette 分组用的工具摘要（qualified 运行时名 + 描述）。
// 放在 proto 而非 TUI 包，是因为 Manager 快照经 WS 透传到 TUI 需要它在 ServerFrame 上序列化。
type MCPToolBrief struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

func NewMCPAction(server, action string) ClientFrame { return ClientFrame{Type: "mcp_action", MCPServer: server, MCPAction: action} }
func NewMCPStatusFrame(s []MCPServerStatus) ServerFrame { return ServerFrame{Type: "mcp_status", MCPServers: s} }
```

- [ ] **Step 3: 改 ws.go（双路统一）**

`list_mcp`（历史帧）与 `mcp_action` 的 `list`/`""` 动作共用同一快照处理器 `handleMCPAction`，二者都回 `mcp_status`（不再回 names-only 的 `mcp_list`）。这样 `/mcp`（走 `mcp_action`）与遗留 `list_mcp` 调用方看到一致、完整的快照。

```go
case "list_mcp":
	// Legacy alias: route through the same snapshot handler as mcp_action:list
	// so both /mcp (mcp_action) and any legacy list_mcp caller see mcp_status.
	handleMCPAction(s, conn, "", "list")
case "mcp_action":
	handleMCPAction(s, conn, cf.MCPServer, cf.MCPAction)

// handleMCPAction 执行 enable/disable/reload/validate/list 后回 mcp_status 快照。
// Tools[] 填每个 ToolDescriptor.Qualified + Description 供 TUI palette 分组。
func handleMCPAction(s *Server, conn *wsConn, name, action string) {
	if s.mcp == nil {
		conn.write(proto.NewMCPStatusFrame(nil))
		return
	}
	ctx := context.Background()
	switch action {
	case "enable":
		_ = s.mcp.Enable(ctx, name)
	case "disable":
		_ = s.mcp.Disable(ctx, name)
	case "reload":
		_ = s.mcp.Reload(ctx, name)
	case "validate":
		s.mcp.Validate(ctx)
	case "list", "":
	}
	out := make([]proto.MCPServerStatus, 0, 1)
	for _, st := range s.mcp.Snapshot(ctx) {
		briefs := make([]proto.MCPToolBrief, 0, len(st.Tools))
		for _, td := range st.Tools {
			briefs = append(briefs, proto.MCPToolBrief{Name: td.Qualified, Description: td.Description})
		}
		out = append(out, proto.MCPServerStatus{
			Name: st.Name, Transport: string(st.Transport),
			Status: string(st.Status), Error: st.Error,
			ToolCount: len(st.Tools), Tools: briefs,
		})
	}
	conn.write(proto.NewMCPStatusFrame(out))
}
```

（server.go 的 `Config.MCP` / `Server.mcp` / `New` 保存已在 Task 13 完成；本 task 只加 proto 帧 + WS dispatch。）

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/proto/ ./internal/api/http/ -v`
Expected: PASS

- [ ] **Step 5: 提交**

```bash
git add internal/proto/frame.go internal/api/http/ws.go
git commit -m "feat(mcp): management frames and WS dispatch"
```

---

## Task 15: TUI `/mcp` + palette

**Files:**
- Modify: `internal/cli/tui/commands.go`
- Modify: `internal/cli/tui/model.go`
- Modify: `internal/cli/tui/view.go`

- [ ] **Step 1: 写失败测试**

```go
// internal/cli/tui/commands_test.go
func TestCmdMCPActions(t *testing.T) {
	// /mcp (no args) → mcp_action list, no server.
	rec := &recordingSession{}
	m := newModel(rec, "/proj")
	mm, _ := m.runCommand("/mcp")
	_ = mm.(model)
	require.Len(t, rec.frames, 1, "/mcp must send one frame")
	assert.Equal(t, "mcp_action", rec.frames[0].Type)
	assert.Equal(t, "", rec.frames[0].MCPServer)
	assert.Equal(t, "list", rec.frames[0].MCPAction)

	// /mcp enable <server> → mcp_action enable with that server.
	rec = &recordingSession{}
	m = newModel(rec, "/proj")
	mm, _ = m.runCommand("/mcp enable github")
	_ = mm.(model)
	require.Len(t, rec.frames, 1)
	assert.Equal(t, "mcp_action", rec.frames[0].Type)
	assert.Equal(t, "github", rec.frames[0].MCPServer)
	assert.Equal(t, "enable", rec.frames[0].MCPAction)

	// /mcp reload <server> → mcp_action reload.
	rec = &recordingSession{}
	m = newModel(rec, "/proj")
	mm, _ = m.runCommand("/mcp reload srv")
	_ = mm.(model)
	require.Len(t, rec.frames, 1)
	assert.Equal(t, "reload", rec.frames[0].MCPAction)

	// /mcp enable (missing server) → local error entry, NO frame.
	rec = &recordingSession{}
	m = newModel(rec, "/proj")
	mm, _ = m.runCommand("/mcp enable")
	m = mm.(model)
	assert.Empty(t, rec.frames, "missing server must not send a frame")
	var sawError bool
	for _, e := range m.entries {
		if _, ok := e.(errorEntry); ok {
			sawError = true
		}
	}
	assert.True(t, sawError, "missing server must render an error entry")

	// /mcp bogus → unknown subcommand error, no frame.
	rec = &recordingSession{}
	m = newModel(rec, "/proj")
	mm, _ = m.runCommand("/mcp bogus")
	_ = mm.(model)
	assert.Empty(t, rec.frames, "unknown subcommand must not send a frame")
}
```

- [ ] **Step 2: 改 cmdMCP**

```go
func cmdMCP(m model, args []string) (tea.Model, tea.Cmd) {
	if len(args) == 0 { return m.sendControlFrame(proto.NewMCPAction("", "list")) }
	action := args[0]
	switch action {
	case "list", "validate": return m.sendControlFrame(proto.NewMCPAction("", action))
	case "enable", "disable", "reload":
		if len(args) < 2 { m.entries = append(m.entries, errorEntry{text: "usage: /mcp " + action + " <server>"}); m.refresh(); return m, nil }
		return m.sendControlFrame(proto.NewMCPAction(args[1], action))
	default: m.entries = append(m.entries, errorEntry{text: "unknown /mcp subcommand: " + action}); m.refresh(); return m, nil
	}
}
```

- [ ] **Step 3a: cli.StreamEvent 桥接（cli 层新增字段 + isControlReply + toStreamEvent）**

	`internal/cli/` 的 `StreamEvent` struct 新增字段：
	```go
	MCPServers []proto.MCPServerStatus `json:"mcp_servers,omitempty"`
	```

	辅助函数 `isControlReply` 识别 `"mcp_status"` 为控制帧（不进入 transcript）。
	`toStreamEvent` 映射 `mcp_status` ServerFrame → StreamEvent{Type:"mcp_status", MCPServers: frame.MCPServers}。
	`internal/cli/tui/events.go` 的 `parseServerFrame` 在收到 `mcp_status` 时发 `tui.MCPServersMsg`。

- [ ] **Step 3b: model.go 处理 mcp_status**

```go
case "mcp_status":
	m.flushAssistant()
	m.entries = append(m.entries, mcpStatusEntry{servers: ev.MCPServers})
	m.paletteMCPServers = ev.MCPServers
```

并在 model struct 加：

```go
paletteMCPServers []proto.MCPServerStatus
```

- [ ] **Step 4: view.go 新增 mcpStatusEntry**

```go
type mcpStatusEntry struct { servers []proto.MCPServerStatus }
func (e mcpStatusEntry) render(_ int, _ spinner.Model) string {
	var b strings.Builder
	b.WriteString("  " + toolName.Render("MCP servers") + "\n")
	if len(e.servers) == 0 { return b.String() + "    " + warnStyle.Render("(none configured)") + "\n\n" }
	for _, s := range e.servers { marker := "○"; if s.Status == "ready" { marker = "●" }; if s.Status == "failed" { marker = "✗" }; b.WriteString(fmt.Sprintf("    %s %s (%s) %d tools", marker, s.Name, s.Transport, s.ToolCount)); if s.Error != "" { b.WriteString(" " + s.Error) }; b.WriteString("\n") }
	return b.String() + "\n"
}
```

- [ ] **Step 5: palette 分组（commandKind 区分条目类型）**

在 `internal/cli/tui/commands.go` 的 `command` struct 加 `kind commandKind` 字段（零值 `cmdSlash` 保持现有 commandTable 条目不变）。palette 分组条目用 `cmdMCPGroup`，MCP 工具条目用 `cmdMCPTool`，不再用 `run==nil` 这种隐式区分（`run` 可能有合法的 nil 边界，不可靠）。

```go
// commandKind 区分 palette 条目类型：slash 命令 / MCP 工具 / MCP 分组标题。
// paletteComplete 按 kind 决定如何补全输入；paletteBlock 按 kind 决定渲染。
type commandKind int

const (
	cmdSlash    commandKind = iota // 普通 / 命令（commandTable 默认）
	cmdMCPTool                      // MCP 工具条目（name=qualified 运行时名）
	cmdMCPGroup                     // 分组标题（不可选）
)

type command struct {
	name string
	help string
	kind commandKind
	run  func(m model, args []string) (tea.Model, tea.Cmd)
}
```

`updatePalette` 在 slash 命令列表后追加 MCP 条目（仅当 `m.paletteMCPServers` 非空）：每个 server 一个 `cmdMCPGroup` 标题（`name:"── "+server.Name+" ──"`，help 拼接 status），该 server 下每个 tool 一个 `cmdMCPTool`（`name: tool.Name`，`help: tool.Description`）。disabled/failed server 的标题 help 追加 `[failed]`/`[disabled]`。`proto.MCPToolBrief` 已在 Task 14 定义，本步只消费 `m.paletteMCPServers[i].Tools`。

```go
func (m *model) paletteMCPItems() []command {
	var items []command
	for _, srv := range m.paletteMCPServers {
		label := "── " + srv.Name + " ──"
		if srv.Status != "ready" {
			label += " [" + srv.Status + "]"
		}
		items = append(items, command{name: label, kind: cmdMCPGroup})
		for _, tool := range srv.Tools {
			items = append(items, command{name: tool.Name, help: tool.Description, kind: cmdMCPTool})
		}
	}
	return items
}
```

`paletteBlock` 按 kind 渲染：`cmdMCPGroup` 用分组样式（不可选时灰显）、`cmdMCPTool` 缩进显示工具名 + help、`cmdSlash` 维持 `/%-7s` 格式。`paletteComplete` 按 kind 行为：
- `cmdSlash`：`m.input.SetValue("/" + sel.name + " ")`（原行为）。
- `cmdMCPTool`：`m.input.SetValue(sel.name)`（插入运行时名，**不加** `/`）。
- `cmdMCPGroup`：no-op（不可选）。`paletteMove` 跳过 `cmdMCPGroup` 选中。

- [ ] **Step 6: 跑测试确认通过**

Run: `go test ./internal/cli/tui/ -v`
Expected: PASS

- [ ] **Step 7: 提交**

```bash
git add internal/cli/tui/commands.go internal/cli/tui/model.go internal/cli/tui/view.go internal/proto/frame.go internal/api/http/ws.go
git commit -m "feat(tui): /mcp management UI + grouped MCP palette"
```

---

## Task 16: 集成验证

**Files:**
- Create: `internal/api/http/mcp_e2e_test.go`

- [ ] **Step 1: 测试流程**

```go
// 启动 mcp.NewFakeHTTPServer → NewManager.StartAll → apihttp.New(Config{MCP:mgr})
// → WS 发送 proto.NewMCPAction("","list") → 断言 mcp_status ready/tool 名一致。
```

- [ ] **Step 2: 跑测试**

Run: `go test ./internal/api/http/ -run TestMCP -v`
Expected: PASS

- [ ] **Step 3: 全量门禁**

Run: `go test ./...`
Expected: PASS（e2e_real 按 CLAUDE.md 预期 skip）

- [ ] **Step 4: 静态检查**

Run: `go vet ./...`
Expected: PASS

- [ ] **Step 5: 提交**

```bash
git add internal/api/http/mcp_e2e_test.go
git commit -m "test(mcp): end-to-end management flow"
```

---

## Self-Review

1. **Spec 覆盖**: V16/C13/MCP1 均有对应 task。stdio/HTTP、tools/list+call、resources/list+read、manager、命名空间、超时、权限、TUI、palette 全覆盖。
2. **路径一致**: 新代码放 `internal/mcp/`；桥接在 `internal/tools/mcp.go`；配置在 `internal/config/`；组合根只在 `bootstrap.Build`。
3. **签名一致**: `Client`、`Manager`、`WithMCP`、`NewMCPTools`、proto 帧的生产者/消费者成对。
4. **权限 fail-closed**: MCP 工具也是 `GuardedTool`，执行入口在 `GuardedTool.Stream` 调 `Authorize`；无 profile/allowlist 一律 deny。
5. **WS/SSE 词表**: 新 `ServerFrame` 自动被 `SSEEvent()` 序列化；管理动作仍以 WS 为主要路径。
6. **软降级**: manager 启动失败只标记 failed/打印 stderr，不阻塞 Build。

## 风险与兜底

- **OAuth**: 计划当前只含 Bearer token；完整 OAuth discovery/refresh 需要单独 task（见待决策点）。
- **HTTP SSE**: 当前实现整段 JSON response；真正 `text/event-stream` 解析需追加 SSE decoder。
- **stdio 超时**: 阻塞 ReadMessage 不能仅靠时间轮询中断，生产实现应持有子进程并在超时 cancel/kill。
- **SecureProcessFactory**: stdio transport 直接 `exec.Command(cfg.Command, cfg.Args...)`，命令来自 config.yaml，无沙箱/路径校验/降权。生产硬化应引入 SecureProcessFactory（验证二进制路径、专用 uid/gid、RLIMIT_* 资源限制）并拒绝绝对路径/shell 元字符。当前唯一防线是操作者本人编写 config.yaml —— 把 stdio 视为受信扩展点。
- **动态工具集**: bootstrap 时注册到 ADK，运行期 reload 新工具需要重启才能被模型看到；TUI/palette 可显示最新 catalog。
- **名称冲突**: sanitize 后可能冲突；应在 Manager.startOne 检测并把冲突 server 标记 failed。

## 待决策点

1. OAuth 是仅 Bearer/token env，还是完整 discovery + refresh + PKCE/device flow？
2. HTTP streamable transport 是否本批必须解析 SSE response？
3. stdio transport 是否只支持 MCP 标准 newline JSON，还是继续兼容 Content-Length？
4. 运行期 enable/reload 是否要求动态重建 ADK runner（当前建议 restart-required）？
5. palette 是否允许选中 MCP 工具后直接把运行时名插入输入框，还是只展示不可选择？
6. runtime name 最大长度与冲突时是 fail-fast 还是哈希后缀？

## 最终统计

- **覆盖范围**: V16 通用 MCP client、C13 `/mcp` 实化管理、MCP1 palette 发现。
- **Task 数**: 13。
- **待决策点**: 6。
- **计划文件**: `D:\code\yanshi\docs\superpowers\plans\2026-07-21-a3-mcp-ecosystem.md`
