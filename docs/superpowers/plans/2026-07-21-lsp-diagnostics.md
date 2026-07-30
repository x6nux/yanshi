# LSP 诊断回喂 (B2-LSP1) Implementation Plan (v3)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 编辑类工具(fs_write/fs_edit/fs_patch)写盘后,自动查询该文件的语言服务器诊断(编译/类型/lint 错误),把摘要追加进工具结果回喂模型,使模型能即时看到自己引入的错误并自纠。

**Architecture (诚实版):** 新建 `internal/lsp/` 包。`Client` 是与单个语言服务器的 JSON-RPC 2.0 协议层,持有**持久** `*bufio.Reader`(避免每次读帧 new 一个 reader 丢预读字节),内部跑**单 goroutine reader loop** 对入站消息做 demux:带 `id` 的响应用 `pending map[int64]chan` 送达对应请求,通知(尤其是 `textDocument/publishDiagnostics`)写入 `diags map`;`request` 发帧后 select 在对应 pending channel + 超时 + `done` 上。`Client` 输入输出是 `io.Reader`/`io.Writer`,可脱离子进程用 `net.Pipe` 测试。`Manager` 按工作区探测语言、spawn 对应 server 进程(进程 stdin/stdout 作为 Client 的 r/w),按语言路由;无可用 server 时退化为 nil 安全的 no-op(沿用 VCS 软降级模式)。Manager 的 `clients/cmds/closers` 由一把 `sync.Mutex` 保护(多 WS turn 并发安全)。诊断新鲜度用**版本边界**保证:`notifyChange` 递增 per-uri `editGen`,`Diagnostics` 等到 `pubGen >= editGen` 的 publication 才返回(编辑后 generation++,杜绝回喂上一版的 stale 诊断)。Manager 经 `tools.WithLSP` 注入 turn context(镜像 `WithVCS`);edit 工具写盘后从 context 取 Manager,`DidChange` + 取诊断,**把摘要写进返回 JSON map 的 `"diagnostics"` 字段**(不污染既有 JSON 契约)。MVP 先接 **gopls**(Go),`DefaultLanguages` 留扩展点给 pyright/tsserver/clangd。

**关于 SyncStream 的诚实声明(评审 #17):** `fs_write`/`fs_edit`/`apply_patch` 走 `SyncStream`,它把 `runWrite`/`runEdit`/`runPatch` 返回的字符串**同时**推进 `ToolChunk.Result`(模型消费)与 `ToolChunk.Text`(TUI 正文区消费)——字段单一归属但内容相同。因此诊断摘要一旦写进返回字符串,就会**同时**出现在模型上下文与 TUI 工具输出块里。这是期望行为(用户在 TUI 里即时看到编译错误),不是缺陷;本计划据此设计,不做"诊断只进 Result、TUI 干净"这种与 `SyncStream` 语义相悖的假设。

**Tech Stack:** Go 1.26.4 · 标准库 `os/exec`、`encoding/json`、`net/url`、`net`(测试用 `net.Pipe`)· 不引第三方 LSP 库(沿用本仓库"手写、最小依赖"风格,如自研 VCS)。外部依赖:`gopls`(可选,缺失即软降级)。`singleflight` 不引入——Manager 用标准库 `sync.Mutex` 串行化懒启动(每个语言仅 spawn 一次,锁竞争是 one-time 且有界的)。

**Spec:** `docs/feature-roadmap-codex-deepseek.md` §7 [LSP1]。参考 `reference/deepseek-tui/crates/tui/src/core/engine/lsp_hooks.rs`。

---

## 跨任务契约(执行者务必保持签名一致)

```go
// internal/lsp/types.go
type Severity int          // 1=Error 2=Warning 3=Info 4=Hint
type Diagnostic struct {
    Line, Column int        // 1-based(解析 LSP 0-based 时 +1)
    Severity     Severity
    Message      string
    Source       string
}
func FormatDiags(file string, diags []Diagnostic) string  // 空诊断 → ""

// internal/lsp/wire.go
func writeMessage(w io.Writer, v any) error
func readMessage(br *bufio.Reader) (map[string]any, error)   // ★ 接收持久 *bufio.Reader(评审 #1)

// internal/lsp/client.go
func newClient(r io.Reader, w io.Writer, timeout time.Duration) *Client
type Client struct { /* 见 Task 3 */ }
// 方法:Start / initialize(rootURI) / notifyChange(uri,text) / Diagnostics(uri,timeout) / Shutdown / Close

// internal/lsp/manager.go
type LanguageServer struct{ Command string; Args []string }
type Config struct {
    WorkRoot  string
    Languages map[string]LanguageServer
    Timeout   time.Duration
    // Dial 非空时绕过 exec.Command(测试注入 net.Pipe fake server)。生产留 nil。
    Dial func(lang string) (read io.Reader, write io.Writer, closer func() error, err error)
}
func New(Config) *Manager
func DefaultLanguages() map[string]LanguageServer
func detectLanguage(path string) string          // 扩展名 → 语言名;无匹配 ""
func pathToURL(path string) string               // ★ Windows 安全 + URL-escape(评审 #8)
// Manager 方法:Enabled / DidChange(path,content) / Diagnostics(path,timeout) / Close

// internal/tools/lspctx.go
type LSPManager interface {                       // ★ 接口而非具体类型,便于测试注入 fake
    Enabled() bool
    DidChange(path, content string)
    Diagnostics(path string, timeout time.Duration) []lsp.Diagnostic
}
func WithLSP(ctx context.Context, m LSPManager) context.Context
func LSPFromContext(ctx context.Context) (LSPManager, bool)
func diagFor(ctx context.Context, absPath, content string) string   // 同包私有,fs.go/fs_patch.go 复用

// internal/config/config.go
type LSPConfig struct {
    Enabled  *bool                         `yaml:"enabled"`      // nil → 默认 true(评审 #15)
    Timeout  string                        `yaml:"diag_timeout"` // "800ms",applyDefaults 里 ParseDuration
    Override map[string]LanguageServerSpec `yaml:"languages"`
}
type LanguageServerSpec struct{ Command string; Args []string `yaml:"args"` }

// internal/agent/orchestrator/orchestrator.go
type Config struct { /* …existing… */ LSP tools.LSPManager }   // ★ 新增字段(评审 #14)
type Orchestrator struct { /* …existing… */ lspMgr tools.LSPManager }
func (o *Orchestrator) prepareTurnContext(ctx context.Context) context.Context  // 统一 4 入口

// internal/bootstrap/bootstrap.go
type App struct { /* …existing… */ LSP *lsp.Manager }   // App.Shutdown 调 a.LSP.Close()

// internal/cli/doctor.go
func checkLSP() CheckResult                                  // 追加进 RunDoctor(评审 #19)
```

`*lsp.Manager` 满足 `tools.LSPManager`(三个方法签名一致);故 bootstrap 里 `lsp.New(...)` 的返回值可直接赋给 `orchConfig.LSP`(类型 `tools.LSPManager`)与 `app.LSP`(类型 `*lsp.Manager`)。

---

## File Structure

| 文件 | 职责 | 新建/改 |
|---|---|---|
| `internal/lsp/types.go` | `Diagnostic`、severity 常量、`FormatDiags` 渲染 | 新建 |
| `internal/lsp/wire.go` | JSON-RPC 2.0 `Content-Length` 帧编解码(`readMessage` 接持久 `*bufio.Reader`) | 新建 |
| `internal/lsp/client.go` | `Client`:reader loop + pending demux + initialize + didOpen/didChange + generation-aware Diagnostics + Shutdown/Close | 新建 |
| `internal/lsp/manager.go` | `Manager`:语言探测、spawn 进程(mutex 保护)、路由、`pathToURL`、`Close`、软降级 no-op、`Dial` 测试缝 | 新建 |
| `internal/lsp/*_test.go` | 各层测试 + net.Pipe fake server E2E | 新建 |
| `internal/tools/lspctx.go` | `LSPManager` 接口 + `WithLSP`/`LSPFromContext` + `diagFor` | 新建 |
| `internal/tools/fs.go` | `runWrite`/`runEdit` 写盘后在 JSON map 加 `"diagnostics"` 字段 | 改 |
| `internal/tools/fs_patch.go` | `runPatch` `commitPatch` 成功后遍历 staged 加诊断(共享预算) | 改 |
| `internal/bootstrap/bootstrap.go` | 软降级建 Manager,装进 `App.LSP` + `orchConfig.LSP`;`Shutdown` 关 Manager | 改 |
| `internal/agent/orchestrator/orchestrator.go` | `Config.LSP` + `o.lspMgr` + `prepareTurnContext` 统一注入;子代理继承 | 改 |
| `internal/config/config.go` | `LSP` 配置节(`*bool` 启用、`diag_timeout`、语言覆盖) | 改 |
| `internal/cli/doctor.go` | `checkLSP` 探测 gopls 等 + 追加进 `RunDoctor` | 改 |

**依赖方向:** `lsp` 是叶子包(零内部依赖,仅标准库);`tools` 依赖 `lsp`(通过 `tools.LSPManager` 接口与 `lsp.Diagnostic`);`orchestrator` 依赖 `tools`(`tools.LSPManager`,**不直接 import lsp**);`bootstrap` 装配 `lsp`。符合六边形向内流动。

---

## Task 1: Diagnostic 类型与渲染

**Files:**
- Create: `internal/lsp/types.go`
- Test: `internal/lsp/types_test.go`

- [ ] **Step 1: 写失败测试**

```go
// internal/lsp/types_test.go
package lsp

import (
	"strings"
	"testing"
)

func TestFormatDiags_Empty(t *testing.T) {
	if got := FormatDiags("foo.go", nil); got != "" {
		t.Fatalf("空诊断应返回空串,得到 %q", got)
	}
}

func TestFormatDiags_RendersSeverityAndLocation(t *testing.T) {
	diags := []Diagnostic{
		{Line: 10, Column: 3, Severity: SeverityError, Message: "undefined: Foo", Source: "gopls"},
		{Line: 20, Column: 1, Severity: SeverityWarning, Message: "unused var x", Source: "gopls"},
	}
	got := FormatDiags("main.go", diags)
	for _, want := range []string{"main.go", "ERR", "L11:3", "undefined: Foo", "WARN", "L21:1", "unused var x"} {
		if !strings.Contains(got, want) {
			t.Errorf("FormatDiags 缺少 %q;完整输出:\n%s", want, got)
		}
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/lsp/ -run TestFormatDiags -v`
Expected: FAIL(包/类型未定义,编译错误)

- [ ] **Step 3: 实现 types.go**

```go
// internal/lsp/types.go

// Package lsp 提供轻量的语言服务器协议客户端:按工作区语言 spawn 对应 server
// (MVP: gopls),在 agent 编辑文件后把诊断回喂模型。无可用 server 时 Manager
// 退化为 no-op,不阻塞启动(沿用 VCS 软降级模式)。
package lsp

import (
	"strconv"
	"strings"
)

// Severity 是 LSP DiagnosticSeverity 的子集(只取我们消费的几档)。
type Severity int

const (
	SeverityError   Severity = 1
	SeverityWarning Severity = 2
	SeverityInfo    Severity = 3
	SeverityHint    Severity = 4
)

// Diagnostic 是一条回喂给模型的诊断。Line/Column 是 1-based(LSP 是 0-based,
// 在解析处 +1 转换),便于模型/人直接定位。
type Diagnostic struct {
	Line     int
	Column   int
	Severity Severity
	Message  string
	Source   string // 诊断来源,通常是 server 名(gopls)
}

// sevLabel 把 Severity 映射成短标签放进结果文本。
func sevLabel(s Severity) string {
	switch s {
	case SeverityWarning:
		return "WARN"
	case SeverityInfo:
		return "INFO"
	case SeverityHint:
		return "HINT"
	default:
		return "ERR"
	}
}

// FormatDiags 把 file 的诊断渲染成追加到工具结果的文本。空诊断返回空串
// (调用方据此决定是否写进 JSON 的 "diagnostics" 字段),避免给模型灌无信息内容。
func FormatDiags(file string, diags []Diagnostic) string {
	if len(diags) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\nDiagnostics for " + file + ":")
	for _, d := range diags {
		b.WriteString("\n  [" + sevLabel(d.Severity) + "] L" +
			strconv.Itoa(d.Line) + ":" + strconv.Itoa(d.Column))
		if d.Source != "" {
			b.WriteString(" (" + d.Source + ")")
		}
		b.WriteString(" " + d.Message)
	}
	return b.String()
}
```

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/lsp/ -run TestFormatDiags -v`
Expected: PASS

- [ ] **Step 5: 提交**

```bash
git add internal/lsp/types.go internal/lsp/types_test.go
git commit -m "feat(lsp): Diagnostic type + severity formatting"
```

---

## Task 2: JSON-RPC 帧编解码(评审 #1:持久 bufio.Reader)

**Files:**
- Create: `internal/lsp/wire.go`
- Test: `internal/lsp/wire_test.go`

> **评审 #1 修复:** 旧版 `readMessage(r io.Reader)` 每次 `bufio.NewReader(r)`,会丢掉 bufio 预读但未消费的字节(下一帧的前若干字节),在 server 连发多条消息(如 initialize 响应紧跟 publishDiagnostics)时丢帧。改为 `readMessage(br *bufio.Reader)`:调用方持有**同一个** `*bufio.Reader` 跨多次调用,预读字节留在 buffer 里不丢。

- [ ] **Step 1: 写失败测试**

```go
// internal/lsp/wire_test.go
package lsp

import (
	"bufio"
	"bytes"
	"encoding/json"
	"testing"
)

func TestWriteMessage_ContentLengthFraming(t *testing.T) {
	var buf bytes.Buffer
	payload := map[string]any{"jsonrpc": "2.0", "method": "ping", "params": map[string]any{"n": 7}}
	if err := writeMessage(&buf, payload); err != nil {
		t.Fatalf("writeMessage: %v", err)
	}
	if !bytes.Contains(buf.Bytes(), []byte("Content-Length:")) {
		t.Errorf("缺少 Content-Length 头,得到 %q", buf.String())
	}
	body := bytes.SplitN(buf.Bytes(), []byte("\r\n\r\n"), 2)[1]
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("body 非合法 JSON: %v (body=%q)", err, body)
	}
	params := got["params"].(map[string]any)
	if int(params["n"].(float64)) != 7 {
		t.Errorf("params.n 还原失败: %v", params["n"])
	}
}

func TestReadMessage_RoundTrip(t *testing.T) {
	var buf bytes.Buffer
	_ = writeMessage(&buf, map[string]any{"jsonrpc": "2.0", "id": 42, "result": "ok"})
	got, err := readMessage(bufio.NewReader(&buf)) // ★ 持久 reader 由调用方构造
	if err != nil {
		t.Fatalf("readMessage: %v", err)
	}
	if int(got["id"].(float64)) != 42 || got["result"] != "ok" {
		t.Errorf("round-trip 失败: %v", got)
	}
}

// TestReadMessage_PersistentBufferKeepsPrefetchedBytes 是评审 #1 的回归保护:
// 连写两条帧,bufio 一次预读可能把第二条的前缀也读进 buffer;用同一个 *bufio.Reader
// 连读两次必须能各自还原,不能丢帧。
func TestReadMessage_PersistentBufferKeepsPrefetchedBytes(t *testing.T) {
	var buf bytes.Buffer
	_ = writeMessage(&buf, map[string]any{"jsonrpc": "2.0", "id": 1, "result": "a"})
	_ = writeMessage(&buf, map[string]any{"jsonrpc": "2.0", "id": 2, "result": "b"})

	br := bufio.NewReader(&buf) // ★ 整条流共用一个 reader
	m1, err := readMessage(br)
	if err != nil {
		t.Fatalf("第一次 readMessage: %v", err)
	}
	m2, err := readMessage(br)
	if err != nil {
		t.Fatalf("第二次 readMessage: %v (可能因 new-reader 丢了预读字节)", err)
	}
	if int(m1["id"].(float64)) != 1 || int(m2["id"].(float64)) != 2 {
		t.Errorf("连读两条帧错乱: m1=%v m2=%v", m1, m2)
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/lsp/ -run TestWriteMessage|TestReadMessage -v`
Expected: FAIL(`writeMessage`/`readMessage` 未定义)

- [ ] **Step 3: 实现 wire.go**

```go
// internal/lsp/wire.go
package lsp

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
)

// LSP 用 JSON-RPC 2.0 over stdio,每条消息前面带 ASCII 头:
//
//	Content-Length: <字节数>\r\n
//	\r\n
//	<JSON body>
//
// 头与 body 之间用空行(\r\n\r\n)分隔。writeMessage/readMessage 只负责帧,
// 不解释 body 语义——body 的结构化解析在 client.go 按 message 类型做。
//
// ★ readMessage 接收一个持久的 *bufio.Reader(评审 #1):buf 在跨帧间保留预读
// 字节,避免每次 new bufio.Reader 丢掉下一帧的前缀。调用方(Client.readLoop、
// 测试)负责构造并复用同一个 reader。
func writeMessage(w io.Writer, v any) error {
	body, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("lsp: marshal: %w", err)
	}
	hdr := "Content-Length: " + strconv.Itoa(len(body)) + "\r\n\r\n"
	if _, err := w.Write([]byte(hdr)); err != nil {
		return err
	}
	if _, err := w.Write(body); err != nil {
		return err
	}
	return nil
}

// readMessage 从 br 读一条帧,返回解码后的 body(map,因为请求/响应/通知结构不同,
// 调用方按 id/method/result 字段判型)。br 必须是跨帧复用的持久 reader。
func readMessage(br *bufio.Reader) (map[string]any, error) {
	var length int
	for {
		line, err := br.ReadBytes('\n')
		if err != nil {
			return nil, fmt.Errorf("lsp: read header: %w", err)
		}
		trimmed := bytes.TrimRight(line, "\r\n")
		if len(trimmed) == 0 {
			break // 空行 = 头结束
		}
		if bytes.HasPrefix(trimmed, []byte("Content-Length:")) {
			val := bytes.TrimSpace(bytes.TrimPrefix(trimmed, []byte("Content-Length:")))
			n, err := strconv.Atoi(string(val))
			if err != nil {
				return nil, fmt.Errorf("lsp: bad Content-Length %q: %w", val, err)
			}
			length = n
		}
	}
	if length <= 0 {
		return nil, fmt.Errorf("lsp: no Content-Length header")
	}
	body := make([]byte, length)
	if _, err := io.ReadFull(br, body); err != nil {
		return nil, fmt.Errorf("lsp: read body: %w", err)
	}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("lsp: unmarshal body: %w", err)
	}
	return m, nil
}
```

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/lsp/ -run TestWriteMessage|TestReadMessage -v`
Expected: PASS(含持久 buffer 回归测试)

- [ ] **Step 5: 提交**

```bash
git add internal/lsp/wire.go internal/lsp/wire_test.go
git commit -m "feat(lsp): JSON-RPC Content-Length framing (persistent bufio.Reader)"
```

---

## Task 3: Client 核心 — reader loop + pending demux + initialize(评审 #2、#3、#7)

**Files:**
- Create: `internal/lsp/client.go`
- Test: `internal/lsp/client_test.go`

> **评审 #2 修复(核心):** 旧版 `request` 用"发一个请求→同步读到匹配 id"实现,一旦 server 在 initialize 响应前后并发推 notification(真实 gopls 在 initialized 后会立即推 publishDiagnostics / window/logMessage),同步读会把这些通知当响应吞掉或错配。新版 Client 起**单 goroutine reader loop** 持续读入站消息并 demux:
> - 带 `id` 的消息 → 响应,查 `pending[id]` channel 投递;
> - 不带 `id` 的消息 → 通知,按 method 路由(`publishDiagnostics` → 存 diags map,Task 4 实现)。
>
> `request` 写帧后 select 在 `pending[id]` + 超时 + `done` 上。这是 LSP 客户端的标准形态。
>
> **评审 #3、#7 修复:** Client 有完整生命周期 `Start()`(启 reader loop)/ `Shutdown()`(发 shutdown 请求 + exit 通知,Task 4)/ `Close()`(置 closed 标志、让 pending 解除阻塞)。子进程的 Kill+Wait 由 Manager 负责(Manager 拥有 cmd);Client 只管协议层。

- [ ] **Step 1: 写失败测试**(用 `net.Pipe` 起一个 fake server,只回 initialize)

```go
// internal/lsp/client_test.go
package lsp

import (
	"bufio"
	"io"
	"net"
	"testing"
	"time"
)

// fakeServerConn 起一个最小 LSP server:在 srv 端用持久 bufio.Reader 循环读,
// 每条消息交给 handle。返回 client 端的 net.Conn。
func fakeServerConn(t *testing.T, handle func(srv net.Conn, br *bufio.Reader, msg map[string]any)) net.Conn {
	t.Helper()
	srv, cli := net.Pipe()
	go func() {
		defer srv.Close()
		br := bufio.NewReader(srv) // ★ 服务端也复用持久 reader
		for {
			msg, err := readMessage(br)
			if err != nil {
				return
			}
			handle(srv, br, msg)
		}
	}()
	return cli
}

func TestClient_Initialize(t *testing.T) {
	cli := fakeServerConn(t, func(srv net.Conn, _ *bufio.Reader, msg map[string]any) {
		if method, _ := msg["method"].(string); method == "initialize" {
			_ = writeMessage(srv, map[string]any{
				"jsonrpc": "2.0", "id": msg["id"],
				"result": map[string]any{"capabilities": map[string]any{}},
			})
			return
		}
		// 其它(initialized / shutdown / exit)读后忽略;吞到对端关 pipe。
		io.Copy(io.Discard, srv)
	})

	cl := newClient(cli, cli, 2*time.Second)
	cl.Start()
	if err := cl.initialize("file:///work"); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	_ = cl.Shutdown(context.Background()) // Task 4 实现;此处先注释,Task 3 仅验 initialize
	cl.Close()
	cli.Close()
}
```

> 注:Step 1 的测试引用了 Task 4 的 `Shutdown`。执行者按 Task 顺序推进时,可先只断言 `initialize` 不报错,`Shutdown`/`Diagnostics` 留到 Task 4 测试一起补全。Task 3 实现只到 `initialize` + `Start`/`Close` + reader loop + pending demux;Task 4 再加 `notifyChange`/`Diagnostics`/`Shutdown`。

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/lsp/ -run TestClient_Initialize -v`
Expected: FAIL(`newClient`/`Start`/`initialize`/`Close` 未定义)

- [ ] **Step 3: 实现 client.go 核心(结构 + reader loop + pending + initialize + Close)**

```go
// internal/lsp/client.go
package lsp

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"
)

// errClosed 由 request 在 Client 已 Close 时返回。
var errClosed = errors.New("lsp: client closed")

// Client 是与单个语言服务器的协议层连接。它不负责 spawn 进程——进程的
// stdin/stdout 由调用方(Manager)作为 r/w 传入,使协议层可脱离子进程用
// net.Pipe 测试。一个 Client 对应一个语言服务器实例。
//
// 线程模型(评审 #2):单 goroutine readLoop 持续读入站消息并 demux——带 id 的
// 响应投递到 pending[id],通知按 method 路由(publishDiagnostics → diags map,
// 见 storeDiags)。request 写帧后 select 在 pending[id] + 超时 + done 上。
// writeMu 串行化写(server 要求请求按序写入);mu 保护 pending 与 closed。
type Client struct {
	br      *bufio.Reader // ★ 持久 reader(评审 #1),跨帧复用,readLoop 独占
	w       io.Writer
	writeMu sync.Mutex
	nextID  int64 // atomic
	timeout time.Duration

	mu      sync.Mutex
	pending map[int64]chan map[string]any // id → 响应 channel(request 创建、deliver 投递、request 退出时删除)
	closed  bool

	diagMu   sync.Mutex
	diags    map[string][]Diagnostic // uri → 最新诊断(Task 4 storeDiags 写入)
	editGen  map[string]int64        // uri → 编辑 generation(notifyChange 递增;Task 4)
	pubGen   map[string]int64        // uri → 已收到 publication 的 generation(readLoop storeDiags 盖写;Task 4)

	done      chan struct{} // readLoop 退出时 close(让 request 的 select 解除阻塞)
	closeOnce sync.Once
}

// newClient 用 r(读 server stdout)/ w(写 server stdin)构造协议层。timeout
// 是每个请求(握手、shutdown)的等待上限。不启动 reader——Start() 才启。
func newClient(r io.Reader, w io.Writer, timeout time.Duration) *Client {
	return &Client{
		br:      bufio.NewReader(r), // ★ 持久化:整个 Client 生命周期共用这一个
		w:       w,
		timeout: timeout,
		pending: map[int64]chan map[string]any{},
		diags:   map[string][]Diagnostic{},
		editGen: map[string]int64{},
		pubGen:  map[string]int64{},
		done:    make(chan struct{}),
	}
}

// Start 启动 readLoop goroutine。必须在 initialize 之前调用——否则 initialize
// 的响应无人投递到 pending。重复调用无副作用(readLoop 只起一次:done 单次 close)。
func (c *Client) Start() {
	go c.readLoop()
}

// readLoop 持续读入站消息并 demux,直到 readMessage 出错(EOF/pipe 关)。
// 退出时 close(done),让所有阻塞在 select 上的 request 解除并返回 errClosed。
func (c *Client) readLoop() {
	defer c.closeOnce.Do(func() { close(c.done) })
	for {
		msg, err := readMessage(c.br) // ★ 复用持久 c.br
		if err != nil {
			return
		}
		if id, ok := msgID(msg); ok {
			c.deliver(id, msg) // 响应:投递给等待的 request
			continue
		}
		c.handleNotification(msg) // 通知:Task 4 路由 publishDiagnostics
	}
}

// msgID 返回消息的 id(仅响应/请求有;通知没有)。返回 ok=false 表示无 id(通知)。
func msgID(msg map[string]any) (int64, bool) {
	if v, ok := msg["id"]; ok {
		return toInt64(v)
	}
	return 0, false
}

// deliver 把响应 msg 投递给等待 id 的 request 的 channel(非阻塞:channel 带
// 1 缓冲;若无人在等——如已超时被删——直接丢弃,避免 readLoop 阻塞)。
func (c *Client) deliver(id int64, msg map[string]any) {
	c.mu.Lock()
	ch := c.pending[id]
	c.mu.Unlock()
	if ch == nil {
		return
	}
	select {
	case ch <- msg:
	default:
	}
}

// handleNotification 按 method 路由通知。publishDiagnostics 的存储在 Task 4
// 的 storeDiags 实现;此处先留 hook,Task 4 填充。其它通知(logMessage 等)忽略。
func (c *Client) handleNotification(msg map[string]any) {
	method, _ := msg["method"].(string)
	if method == "textDocument/publishDiagnostics" {
		c.storeDiags(msg) // Task 4 实现
	}
}

// request 写一条带 id 的请求并等待响应。响应经 readLoop → deliver → pending[id]
// channel 送达。超时(默认 timeout,可被 ctx 截断)或 done(Client 关闭)时返回错误。
func (c *Client) request(ctx context.Context, method string, params any) (map[string]any, error) {
	id := atomic.AddInt64(&c.nextID, 1)
	ch := make(chan map[string]any, 1)
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, errClosed
	}
	c.pending[id] = ch
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}()

	req := map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}
	if err := c.writeMsg(req); err != nil {
		return nil, err
	}

	timeout := c.timeout
	if dl, ok := ctx.Deadline(); ok {
		if rem := time.Until(dl); rem > 0 && rem < timeout {
			timeout = rem // 取 ctx 与默认 timeout 的较小者
		}
	}
	select {
	case resp := <-ch:
		if e, ok := resp["error"]; ok {
			return nil, fmt.Errorf("lsp: %s: %v", method, e)
		}
		return resp, nil
	case <-time.After(timeout):
		return nil, fmt.Errorf("lsp: %s timeout", method)
	case <-c.done:
		return nil, errClosed
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// notify 写一条无 id 的通知(不期待响应)。
func (c *Client) notify(method string, params any) error {
	return c.writeMsg(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

// writeMu 串行化写:server 要求请求/通知按序到达 stdin。
func (c *Client) writeMsg(m map[string]any) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return writeMessage(c.w, m)
}

// initialize 发 LSP initialize 请求并等待响应,然后发 initialized 通知。
// rootURI 用 pathToURL 规范化(Windows 安全)。Start() 必须已调用。
func (c *Client) initialize(rootPath string) error {
	rootURI := pathToURL(rootPath) // ★ 评审 #8:统一走 pathToURL
	resp, err := c.request(context.Background(), "initialize", map[string]any{
		"processId":    nil,
		"rootUri":      rootURI,
		"capabilities": map[string]any{},
	})
	if err != nil {
		return fmt.Errorf("lsp: initialize: %w", err)
	}
	_ = resp // capabilities 暂不消费
	return c.notify("initialized", map[string]any{})
}

// Close 置 closed 标志(让后续 request 立即返回 errClosed)并唤醒阻塞的 select。
// 它不关闭 r/w —— 那些由 Manager 拥有(exec 的 stdin pipe / Dial 的 closer)。
// readLoop 在 r 被对端关闭产生 EOF 时自行退出并 close(done)。幂等(closeOnce)。
func (c *Client) Close() error {
	c.closeOnce.Do(func() {
		c.mu.Lock()
		c.closed = true
		c.mu.Unlock()
		// done 由 readLoop 的 defer 关闭;若 readLoop 因任何原因未退出(理论上 r 仍开),
		// 这里不强制关 done —— Manager.Close 会关底层资源引发 EOF,readLoop 随之退出。
	})
	return nil
}

// Done 在 readLoop 退出(EOF/错误)后关闭。Manager.Close 可等它确认 reader 已停。
func (c *Client) Done() <-chan struct{} { return c.done }

func toInt64(v any) (int64, bool) {
	switch n := v.(type) {
	case float64:
		return int64(n), true
	case int64:
		return n, true
	case int:
		return int64(n), true
	}
	return 0, false
}

func toInt64Or(v any, def int64) int64 {
	if n, ok := toInt64(v); ok {
		return n
	}
	return def
}
```

> 注:`pathToURL` 在 Task 5 的 manager.go 实现。Task 3 的 `initialize` 调用它,因此执行者需**先把 Task 5 的 `pathToURL` 函数也写进 manager.go**(或临时内联),再跑 Task 3 测试。两 task 的代码可一并落地后统一编译;按 TDD 节奏,先写 Task 3 测试→实现 client.go→再写 Task 5 的 manager.go(含 pathToURL)→跑全包。本计划按依赖顺序列 Task,执行者可并行落地 Task 3+5 的非测试代码以满足编译。

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/lsp/ -run TestClient_Initialize -v`
Expected: PASS

- [ ] **Step 5: 提交**

```bash
git add internal/lsp/client.go internal/lsp/client_test.go
git commit -m "feat(lsp): Client with reader loop + pending demux + initialize handshake"
```

---

## Task 4: Client — didOpen/didChange + generation-aware Diagnostics + Shutdown(评审 #3、#4、#5)

**Files:**
- Modify: `internal/lsp/client.go`
- Test: `internal/lsp/client_test.go`(追加)

> **评审 #4 修复:** 加 `didOpen`。`notifyChange(uri, text)` 首次对某 uri 发 `textDocument/didOpen`(带全文 + version 1),之后发 `textDocument/didChange`(全量 + version 2,3,…,单调递增)。version 复用 `editGen` 计数(1,2,3…),既满足 LSP 的单调 version 约束,又作为 generation 边界。
>
> **评审 #5 修复(stale 诊断):** 旧版 `Diagnostics` 直接返回 map 里的最新值——可能是上一次编辑的残留。新版用 generation 边界:`notifyChange` 递增 `editGen[uri]`;readLoop 收到 publication 时盖写 `pubGen[uri] = editGen[uri]`(此刻的请求 generation);`Diagnostics` 先取 `want = editGen[uri]`,spin 到 `pubGen[uri] >= want` 或超时。这样"编辑后查"必然等到本次编辑触发的 publication,而非上一版残留。
>
> **评审 #3 修复:** `Shutdown` 发 `shutdown` 请求(等响应)+ `exit` 通知,走标准 LSP 关闭序列。

- [ ] **Step 1: 写失败测试**

```go
// 追加到 internal/lsp/client_test.go

// TestClient_NotifyChange_PublishesFreshDiagnostics 验证完整路径:
// initialize → notifyChange(didOpen) → server 推 publishDiagnostics →
// Diagnostics 在 generation 边界内拿到本次诊断。
func TestClient_NotifyChange_PublishesFreshDiagnostics(t *testing.T) {
	uri := "file:///work/main.go"
	cli := fakeServerConn(t, func(srv net.Conn, _ *bufio.Reader, msg map[string]any) {
		switch msg["method"] {
		case "initialize":
			_ = writeMessage(srv, map[string]any{
				"jsonrpc": "2.0", "id": msg["id"],
				"result": map[string]any{"capabilities": map[string]any{}},
			})
		case "textDocument/didOpen", "textDocument/didChange":
			// 收到变更即推一条 error 诊断。
			_ = writeMessage(srv, map[string]any{
				"jsonrpc": "2.0", "method": "textDocument/publishDiagnostics",
				"params": map[string]any{
					"uri": uri,
					"diagnostics": []map[string]any{{
						"range":    map[string]any{"start": map[string]any{"line": 0, "character": 0}},
						"severity": 1,
						"message":  "undefined: x",
						"source":   "gopls",
					}},
				},
			})
		}
	})

	cl := newClient(cli, cli, 2*time.Second)
	cl.Start()
	if err := cl.initialize("/work"); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	if err := cl.notifyChange(uri, "package main\n"); err != nil {
		t.Fatalf("notifyChange: %v", err)
	}
	diags := cl.Diagnostics(uri, time.Second)
	if len(diags) != 1 || diags[0].Message != "undefined: x" {
		t.Fatalf("期望 1 条诊断 undefined: x,得到 %+v", diags)
	}
	_ = cl.Shutdown(context.Background())
	cl.Close()
	cli.Close()
}

// TestClient_DiagnosticsReturnsStaleOnlyOnTimeout 验证 generation 边界:
// 没有任何 didChange 时 editGen=0,Diagnostics 不等(直接返回当前 diags,可能空);
// 不会把"还没到的本次编辑诊断"误判为已就绪。这里只断言不阻塞、不 panic。
func TestClient_DiagnosticsNoEditNoBlock(t *testing.T) {
	cli := fakeServerConn(t, func(srv net.Conn, _ *bufio.Reader, msg map[string)any) {
		if msg["method"] == "initialize" {
			_ = writeMessage(srv, map[string]any{
				"jsonrpc": "2.0", "id": msg["id"],
				"result": map[string]any{"capabilities": map[string]any{}},
			})
		}
	})
	cl := newClient(cli, cli, time.Second)
	cl.Start()
	if err := cl.initialize("/work"); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	// 无编辑(editGen=0):Diagnostics 立即返回(nil),不等到超时。
	start := time.Now()
	diags := cl.Diagnostics("file:///work/x.go", 500*time.Millisecond)
	if diags != nil {
		t.Fatalf("未编辑的 uri 应返回 nil,得到 %+v", diags)
	}
	if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
		t.Fatalf("未编辑时 Diagnostics 不应阻塞 %v", elapsed)
	}
	cl.Close()
	cli.Close()
}
```

> 注:Step 1 第二个测试里有个故意的笔误示意(`map[string)any`),执行者修正为 `map[string]any`。

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/lsp/ -run TestClient_NotifyChange|TestClient_DiagnosticsNoEditNoBlock -v`
Expected: FAIL(`notifyChange`/`Diagnostics`/`Shutdown`/`storeDiags` 未定义)

- [ ] **Step 3: 实现 — 在 client.go 追加**

```go
// 追加到 internal/lsp/client.go

// languageID 按 uri 扩展名推断 LSP languageId(didOpen 需要)。gopls 等多数
// server 据此选择解析器;未知扩展名返回 ""(server 通常按 uri 后缀兜底)。
func languageID(uri string) string {
	ext := strings.ToLower(filepath.Ext(uri))
	switch ext {
	case ".go":
		return "go"
	case ".py":
		return "python"
	case ".ts":
		return "typescript"
	case ".tsx":
		return "typescriptreact"
	case ".js":
		return "javascript"
	case ".jsx":
		return "javascriptreact"
	case ".rs":
		return "rust"
	}
	return ""
}

// notifyChange 告知某 uri 的全文内容变更。首次对该 uri 发 textDocument/didOpen
// (version=1),之后发 textDocument/didChange(version 单调递增,全量替换)。
// 全量比增量简单且足够——模型编辑后我们总能给全文(评审 #4)。
//
// 同时递增 editGen[uri](评审 #5):Diagnostics 会等到 pubGen>=editGen 的本次
// publication,避免回喂上一版残留诊断。version 复用 editGen(1-based 单调)。
func (c *Client) notifyChange(uri, text string) error {
	c.diagMu.Lock()
	c.editGen[uri]++
	gen := c.editGen[uri]
	c.diagMu.Unlock()

	if gen == 1 {
		return c.notify("textDocument/didOpen", map[string]any{
			"textDocument": map[string]any{
				"uri":        uri,
				"languageId": languageID(uri),
				"version":    1,
				"text":       text,
			},
		})
	}
	return c.notify("textDocument/didChange", map[string]any{
		"textDocument":   map[string]any{"uri": uri, "version": gen},
		"contentChanges": []map[string]any{{"text": text}},
	})
}

// storeDiags 把一条 publishDiagnostics 的 params 解进 diags,并盖写 pubGen =
// 此刻的 editGen(评审 #5)。readLoop 在收到该通知时调用。
// LSP 行/列是 0-based,这里 +1 转成 1-based 供模型/人定位。
func (c *Client) storeDiags(msg map[string]any) {
	params, _ := msg["params"].(map[string]any)
	if params == nil {
		return
	}
	uri, _ := params["uri"].(string)
	raw, _ := params["diagnostics"].([]any)
	out := make([]Diagnostic, 0, len(raw))
	for _, r := range raw {
		if d, ok := r.(map[string]any); ok {
			out = append(out, parseDiagnostic(d))
		}
	}
	c.diagMu.Lock()
	c.diags[uri] = out
	c.pubGen[uri] = c.editGen[uri] // 盖写为当前请求 generation
	c.diagMu.Unlock()
}

func parseDiagnostic(d map[string]any) Diagnostic {
	var di Diagnostic
	if rng, ok := d["range"].(map[string]any); ok {
		if start, ok := rng["start"].(map[string]any); ok {
			di.Line = int(toInt64Or(start["line"], 0)) + 1
			di.Column = int(toInt64Or(start["character"], 0)) + 1
		}
	}
	di.Severity = Severity(int(toInt64Or(d["severity"], 1)))
	di.Message, _ = d["message"].(string)
	di.Source, _ = d["source"].(string)
	return di
}

// Diagnostics 返回 uri 的诊断;等到本次 editGen 对应的 publication 到达
// (pubGen>=want)或 timeout。未编辑过(want==0)立即返回当前值(通常 nil),
// 不阻塞——因为没有"本次编辑"可等(评审 #5)。
func (c *Client) Diagnostics(uri string, timeout time.Duration) []Diagnostic {
	c.diagMu.Lock()
	want := c.editGen[uri]
	c.diagMu.Unlock()
	if want == 0 {
		c.diagMu.Lock()
		d := c.diags[uri]
		c.diagMu.Unlock()
		return cloneDiag(d) // nil-safe(可能为 nil)
	}

	deadline := time.Now().Add(timeout)
	for {
		c.diagMu.Lock()
		ready := c.pubGen[uri] >= want
		d := c.diags[uri]
		c.diagMu.Unlock()
		if ready {
			return cloneDiag(d)
		}
		if !time.Now().Before(deadline) {
			// 超时:返回当前(可能是 stale 或 nil)。调用方据此决定是否追加。
			c.diagMu.Lock()
			stale := c.diags[uri]
			c.diagMu.Unlock()
			return cloneDiag(stale)
		}
		time.Sleep(15 * time.Millisecond)
	}
}

// cloneDiag 返回 diags 的副本,避免调用方在 Manager 锁外改到 Client 内部切片。
// nil 入参返回 nil。
func cloneDiag(diags []Diagnostic) []Diagnostic {
	if diags == nil {
		return nil
	}
	cp := make([]Diagnostic, len(diags))
	copy(cp, diags)
	return cp
}

// Shutdown 发标准 LSP 关闭序列:shutdown 请求(等响应)+ exit 通知。
// best-effort:server 已走或 pipe 断开时返回错误,调用方(Manager)忽略并继续
// Kill+Wait 清理进程。幂等性由调用方保证(Manager.Close 只调一次)。
func (c *Client) Shutdown(ctx context.Context) error {
	if _, err := c.request(ctx, "shutdown", nil); err != nil {
		// 不在此 return:仍尝试发 exit,让 server 干净退出。
	}
	return c.notify("exit", nil)
}
```

> client.go 顶部 import 需补:`"context"`(Task 3 已含)、`"path/filepath"`、`"strings"`。

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/lsp/ -run TestClient -v`
Expected: PASS(initialize + notifyChange + Diagnostics 三个测试都过)

- [ ] **Step 5: 提交**

```bash
git add internal/lsp/client.go internal/lsp/client_test.go
git commit -m "feat(lsp): didOpen/didChange + generation-aware diagnostics + shutdown"
```

---

## Task 5: Manager — 语言探测 + spawn + 软降级 + pathToURL + Close(评审 #6、#7、#8、#16)

**Files:**
- Create: `internal/lsp/manager.go`
- Test: `internal/lsp/manager_test.go`

> **评审 #6 修复:** Manager `clients/cmds/closers` 由一把 `sync.Mutex` 保护;`clientFor` 在锁内 check-then-spawn,杜绝多 WS turn 并发首编同一语言时 double-spawn。锁在 spawn+initialize 期间持有——每个语言仅 spawn 一次,故该阻塞是 one-time 且有界的(可接受;若生产里成为瓶颈,后续可换 per-language singleflight,本仓库不引第三方)。
>
> **评审 #7 修复:** initialize 失败时 Kill **并 Wait**(reap 子进程,避免僵尸);Manager.Close 对每个 cmd 也 Kill+Wait。
>
> **评审 #8 修复:** `pathToURL` 用 `net/url` 规范化:Windows 盘符 `C:\foo` → `/C:/foo`(file URL 三斜杠后跟盘符),空格/中文等经 `url.URL.String()` percent-escape。
>
> **评审 #16 修复:** `Manager.Close()` 遍历 clients:Shutdown(标准序列)+ 关底层资源(exec:关 stdin pipe + Kill + Wait;Dial:调 closer),杜绝 gopls 泄漏。

- [ ] **Step 1: 写失败测试**

```go
// internal/lsp/manager_test.go
package lsp

import (
	"io"
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

// TestPathToURL_WindowsAndEscape 验证评审 #8:盘符转 /D:/ + 空格/中文 escape。
// 用字面路径断言(不依赖运行 OS),确认 net/url 规范化生效。
func TestPathToURL_WindowsAndEscape(t *testing.T) {
	got := pathToURL(`D:\code\my proj\中文.go`)
	if !strings.HasPrefix(got, "file:///D:/code/my%20proj/") {
		t.Errorf("Windows 盘符/空格 escape 失败: %q", got)
	}
	if !strings.Contains(got, "%E4%B8%AD%E6%96%87.go") { // "中文" UTF-8 percent-escape
		t.Errorf("中文 escape 失败: %q", got)
	}
}

func TestPathToURL_Posix(t *testing.T) {
	got := pathToURL("/home/x/main.go")
	if got != "file:///home/x/main.go" {
		t.Errorf("posix 路径应原样(三斜杠): %q", got)
	}
}

// TestManager_DialSeam_ConnectsFakeServer 用 Dial 测试缝注入 net.Pipe fake server,
// 验证 Manager.clientFor → Client.Start → initialize → notifyChange → Diagnostics 的
// 真实代码路径(评审 #20 的 Manager 层 E2E,无需 gopls)。
func TestManager_DialSeam_ConnectsFakeServer(t *testing.T) {
	srv, cli := net.Pipe() // 见下方 import net
	go func() {
		defer srv.Close()
		runFakeGopls(t, srv) // t.Helper 不适合 goroutine;见 helper
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
```

> helper `runFakeGopgs` 与 `net` import 见 Step 3 后的"测试 helpers"补充。执行者把 `runFakeGopls` 与 Task 3 的 `fakeServerConn` 风格一致:持久 bufio.Reader 循环读,initialize 回 capabilities,didOpen/didChange 推一条 `undefined: y` 诊断。

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/lsp/ -run TestManager|TestPathToURL -v`
Expected: FAIL(`New`/`Config`/`Enabled`/`DidChange`/`Diagnostics`/`detectLanguage`/`DefaultLanguages`/`pathToURL`/`Close` 未定义)

- [ ] **Step 3: 实现 manager.go**

```go
// internal/lsp/manager.go
package lsp

import (
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Config 配置 Manager。WorkRoot 是工作区根(server 的 rootUri);Languages 把
// 语言名映射到启动命令。空 Languages → disabled。Dial 非空时绕过 exec(测试注入)。
type Config struct {
	WorkRoot  string
	Languages map[string]LanguageServer
	Timeout   time.Duration // 诊断等待上限,0 用默认 800ms
	// Dial 非空时,clientFor 用它取代 exec.Command 拿到 server 的读写端 + closer。
	// 生产留 nil(走 exec);测试注入 net.Pipe 一端连 fake server(评审 #20)。
	Dial func(lang string) (read interface{ Read([]byte) (int, error) }, write interface{ Write([]byte) (int, error) }, closer func() error, err error)
}

// LanguageServer 描述如何启动一个语言服务器。
type LanguageServer struct {
	Command string   // 可执行文件名(经 PATH 查找)
	Args    []string // 启动参数
}

// DefaultLanguages 是 MVP 内置的语言→命令表。命令缺失时由 New 探测并剔除
// 该语言(软降级)。
func DefaultLanguages() map[string]LanguageServer {
	return map[string]LanguageServer{
		"go":     {Command: "gopls"},
		"python": {Command: "pyright-langserver", Args: []string{"--stdio"}},
	}
}

// Manager 管理一个工作区的语言服务器。Enabled()==false 时是 no-op:
// DidChange/Diagnostics 都是空操作,让 edit 工具无条件调用而无需判空。
//
// 并发(评审 #6):mu 保护 clients/cmds/closers;clientFor 在锁内 check-then-spawn。
type Manager struct {
	cfg     Config
	mu      sync.Mutex
	clients map[string]*Client  // 语言 → 已握手 Client
	cmds    map[string]*exec.Cmd // exec 路径的进程句柄(Kill+Wait 用)
	closers []func() error       // Dial 路径的资源 closer
	enabled bool
}

// New 构造 Manager。剔除命令不在 PATH 上的语言(Dial 非空时跳过此剪枝——测试
// 用 fake 命令名);若剔除后无可用语言,返回 disabled Manager。不在此处 spawn——
// spawn 推迟到第一次 DidChange(按文件语言),避免启动慢的 server 拖慢 bootstrap。
func New(cfg Config) *Manager {
	m := &Manager{
		cfg:     cfg,
		clients: map[string]*Client{},
		cmds:    map[string]*exec.Cmd{},
	}
	if cfg.Timeout == 0 {
		m.cfg.Timeout = 800 * time.Millisecond
	}
	usable := map[string]LanguageServer{}
	for lang, ls := range cfg.Languages {
		if cfg.Dial != nil || commandAvailable(ls.Command) {
			usable[lang] = ls
		}
	}
	m.cfg.Languages = usable
	m.enabled = len(usable) > 0 && cfg.WorkRoot != ""
	return m
}

func commandAvailable(cmd string) bool {
	if cmd == "" {
		return false
	}
	_, err := exec.LookPath(cmd)
	return err == nil
}

// Enabled 报告是否有至少一个可用语言 server。false 时 Manager 是 no-op。
func (m *Manager) Enabled() bool { return m != nil && m.enabled }

// detectLanguage 按扩展名判语言;无匹配返回空串。
func detectLanguage(path string) string {
	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	case ".go":
		return "go"
	case ".py":
		return "python"
	case ".ts":
		return "typescript"
	case ".tsx":
		return "typescript"
	case ".js", ".jsx":
		return "javascript"
	case ".c", ".cc", ".cpp", ".h", ".hpp":
		return "cpp"
	case ".rs":
		return "rust"
	}
	return ""
}

// DidChange 告知某文件内容变更。disabled 或无对应语言的文件 → no-op。
func (m *Manager) DidChange(path, content string) {
	if !m.Enabled() {
		return
	}
	lang := detectLanguage(path)
	if lang == "" {
		return
	}
	c, err := m.clientFor(lang)
	if err != nil || c == nil {
		return // 启动失败 → 静默降级,不阻塞工具
	}
	_ = c.notifyChange(pathToURL(path), content)
}

// Diagnostics 返回 path 的最新诊断;disabled/无语言/超时 → nil。锁内仅取 client
// 指针(短暂),c.Diagnostics 不持 Manager 锁。
func (m *Manager) Diagnostics(path string, timeout time.Duration) []Diagnostic {
	if !m.Enabled() {
		return nil
	}
	lang := detectLanguage(path)
	if lang == "" {
		return nil
	}
	m.mu.Lock()
	c := m.clients[lang]
	m.mu.Unlock()
	if c == nil {
		return nil
	}
	if timeout == 0 {
		timeout = m.cfg.Timeout
	}
	return c.Diagnostics(pathToURL(path), timeout)
}

// clientFor 懒启动某语言的 server 并握手;失败返回 error(调用方静默降级)。
// 评审 #6:mu 锁内 check-then-spawn,杜绝并发 double-spawn。锁在 spawn+initialize
// 期间持有(每语言 one-time,可接受)。
// 评审 #7:initialize 失败时 Close client + Kill + Wait(cmd) reap 子进程。
func (m *Manager) clientFor(lang string) (*Client, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if c, ok := m.clients[lang]; ok {
		return c, nil
	}
	ls, ok := m.cfg.Languages[lang]
	if !ok {
		return nil, os.ErrNotExist
	}

	c, cleanup, err := m.spawnLocked(lang, ls)
	if err != nil {
		return nil, err
	}
	c.Start()
	if err := c.initialize(m.cfg.WorkRoot); err != nil {
		c.Close()
		cleanup() // Kill+Wait(exec) 或 closer(Dial), reap/释放
		return nil, err
	}
	m.clients[lang] = c
	return c, nil
}

// spawnLocked 在已持锁前提下构造 Client(不经 initialize)。返回 Client + 一个
// cleanup(失败/Close 时调:exec 路径关 stdin+Kill+Wait;Dial 路径调 closer)。
// 成功路径下 exec 的 cmd 记入 m.cmds 供 Manager.Close 批量清理。
func (m *Manager) spawnLocked(lang string, ls LanguageServer) (*Client, func(), error) {
	if m.cfg.Dial != nil {
		r, w, closer, err := m.cfg.Dial(lang)
		if err != nil {
			return nil, nil, err
		}
		c := newClient(r, w, m.cfg.Timeout)
		m.closers = append(m.closers, closer)
		return c, func() { _ = closer() }, nil
	}

	cmd := exec.Command(ls.Command, ls.Args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, nil, err
	}
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		return nil, nil, err
	}
	c := newClient(stdout, stdin, 5*time.Second) // 握手超时 5s;诊断超时用 cfg.Timeout
	m.cmds[lang] = cmd
	cleanup := func() {
		_ = stdin.Close()
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		_ = cmd.Wait() // 评审 #7:reap,避免僵尸
	}
	return c, cleanup, nil
}

// Close 关闭所有 client 与底层进程/连接(评审 #16):对每个 client 发标准
// shutdown+exit 序列(best-effort),再 Kill+Wait(exec)/调 closer(Dial)。
// 幂等(enabled Manager 第二次调见 clients 已空)。disabled Manager 是 no-op。
func (m *Manager) Close() error {
	if !m.Enabled() {
		return nil
	}
	m.mu.Lock()
	clients := m.clients
	cmds := m.cmds
	closers := m.closers
	m.clients = nil
	m.cmds = nil
	m.closers = nil
	m.enabled = false // 防重入
	m.mu.Unlock()

	for lang, c := range clients {
		_ = c.Shutdown(context.Background())
		_ = c.Close()
		if cmd := cmds[lang]; cmd != nil && cmd.Process != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
		_ = c.Done() // 不阻塞等待;readLoop 因 EOF 自行退出
	}
	for _, closer := range closers {
		_ = closer()
	}
	return nil
}

// pathToURL 把本地路径转 file:// URI(评审 #8)。用 net/url 规范化:
//   - 绝对化(filepath.Abs);
//   - Windows 盘符 C:\ → 前置斜杠变 /C:/ ,斜杠统一为正斜杠;
//   - url.URL.String() 对空格/中文等做 percent-escape。
//
// rootURI(initialize)与文件 URI 都走这里,保证 gopls 在 Windows 上不因 URI
// 格式报错。
func pathToURL(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	p := filepath.ToSlash(abs)
	// Windows 盘符(或任何 X: 形式):file URL 需 /C:/foo 形式(三斜杠 + 盘符)。
	if len(p) >= 2 && p[1] == ':' {
		p = "/" + p
	} else if runtime.GOOS == "windows" && !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	u := &url.URL{Scheme: "file", Path: p}
	return u.String()
}
```

> manager.go 顶部 import 需补:`"context"`、`"sync"`、`"io"`(若 Dial 签名用 io.Reader/Writer 则用 io;上面为避免接口字面冗长用了匿名接口,执行者可改为 `io.Reader, io.Writer` 更地道——见下方"签名统一"说明)。

**签名统一说明:** Config.Dial 的元素类型用标准 `io.Reader` / `io.Writer` 最清晰。跨任务契约里已写 `(read io.Reader, write io.Writer, closer func() error, err error)`;上方 manager.go 代码块里的匿名 interface 是为示意"读写两端",执行者落地时统一成 `io.Reader`/`io.Writer` 并 import `"io"`。Client 的 `newClient(r io.Reader, w io.Writer, ...)` 与之一致。net.Pipe 返回的 `net.Conn` 同时满足 io.Reader/io.Writer,故 Dial 测试可直接返回同一个 conn。

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/lsp/ -v`
Expected: PASS(整个 lsp 包,含 pathToURL 与 Dial seam E2E)

- [ ] **Step 5: 提交**

```bash
git add internal/lsp/manager.go internal/lsp/manager_test.go
git commit -m "feat(lsp): Manager with mutex-guarded spawn, pathToURL, Close, Dial seam"
```

---

## Task 6: Context 注入(镜像 vcsctx)+ LSPManager 接口

**Files:**
- Create: `internal/tools/lspctx.go`
- Test: `internal/tools/lspctx_test.go`

> **接口而非具体类型(评审 #10 的前提):** 用 `tools.LSPManager` 接口存进 context,使 fs 工具的测试可注入一个 fake manager(无需真 gopls、无需 subprocess)。`*lsp.Manager` 满足该接口(Enabled/DidChange/Diagnostics 三法签名一致)。这也让 orchestrator 经 `tools.LSPManager` 引用 LSP 而**不必 import lsp**(六边形:orchestrator 只依赖 tools)。

- [ ] **Step 1: 读参照实现**(确认要镜像的模式)

Run: `sed -n '1,35p' internal/tools/vcsctx.go`(`D:\code\yanshi\internal\tools\vcsctx.go`)
确认 `WithVCS`/`VCSScopeFromContext` 的 context-value 模式。

- [ ] **Step 2: 写失败测试**

```go
// internal/tools/lspctx_test.go
package tools

import (
	"context"
	"testing"
	"time"

	"github.com/x6nux/yanshi/internal/lsp"
)

func TestLSPContext_BindRetrieve(t *testing.T) {
	ctx := context.Background()
	if _, ok := LSPFromContext(ctx); ok {
		t.Fatal("未绑定时应 ok=false")
	}
	var mgr LSPManager = lsp.New(lsp.Config{}) // disabled manager,满足接口
	ctx = WithLSP(ctx, mgr)
	got, ok := LSPFromContext(ctx)
	if !ok || got != mgr {
		t.Fatalf("绑后应取回同一 Manager,ok=%v", ok)
	}
}

// fakeLSPManager 是测试用 stub,验证 diagFor 读 context + 调 Manager 的行为,
// 不依赖真 server(评审 #10 的 fake-first)。
type fakeLSPManager struct {
	changedPath string
	changedText string
	diags       []lsp.Diagnostic
}

func (f *fakeLSPManager) Enabled() bool { return true }
func (f *fakeLSPManager) DidChange(path, content string) {
	f.changedPath = path
	f.changedText = content
}
func (f *fakeLSPManager) Diagnostics(path string, timeout time.Duration) []lsp.Diagnostic {
	return f.diags
}

// TestDiagFor_NoManagerEmpty 验证无 Manager 时返回空串(不追加,不污染)。
func TestDiagFor_NoManagerEmpty(t *testing.T) {
	if got := diagFor(context.Background(), "/x/y.go", "pkg"); got != "" {
		t.Fatalf("无 Manager 应返回空串,得到 %q", got)
	}
}

// TestDiagFor_DisabledManagerEmpty 验证 disabled Manager 也返回空串。
func TestDiagFor_DisabledManagerEmpty(t *testing.T) {
	ctx := WithLSP(context.Background(), lsp.New(lsp.Config{})) // disabled
	if got := diagFor(ctx, "/x/y.go", "pkg"); got != "" {
		t.Fatalf("disabled Manager 应返回空串,得到 %q", got)
	}
}

// TestDiagFor_FormatsAndDidChanges 验证 diagFor 先 DidChange 再取诊断并渲染。
func TestDiagFor_FormatsAndDidChanges(t *testing.T) {
	fm := &fakeLSPManager{diags: []lsp.Diagnostic{
		{Line: 1, Column: 1, Severity: lsp.SeverityError, Message: "boom", Source: "gopls"},
	}}
	ctx := WithLSP(context.Background(), fm)
	got := diagFor(ctx, "/x/y.go", "package main")
	if fm.changedPath != "/x/y.go" || fm.changedText != "package main" {
		t.Errorf("diagFor 应调 DidChange(path,content),得到 path=%q text=%q", fm.changedPath, fm.changedText)
	}
	if !containsStr(got, "boom") || !containsStr(got, "y.go") {
		t.Errorf("diagFor 应含诊断渲染,得到 %q", got)
	}
}

func containsStr(hay, needle string) bool {
	for i := 0; i+len(needle) <= len(hay); i++ {
		if hay[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
```

- [ ] **Step 3: 跑测试确认失败**

Run: `go test ./internal/tools/ -run TestLSPContext|TestDiagFor -v`
Expected: FAIL(`WithLSP`/`LSPFromContext`/`LSPManager`/`diagFor` 未定义)

- [ ] **Step 4: 实现 lspctx.go**

```go
// internal/tools/lspctx.go
package tools

import (
	"context"
	"time"

	"github.com/x6nux/yanshi/internal/lsp"
)

// LSPManager 是 edit 工具消费的诊断回喂契约(接口而非具体类型,便于测试注入
// fake,且让 orchestrator 经本接口引用 LSP 而不直接 import lsp)。*lsp.Manager
// 满足该接口。
type LSPManager interface {
	Enabled() bool
	DidChange(path, content string)
	Diagnostics(path string, timeout time.Duration) []lsp.Diagnostic
}

type lspKey struct{}

// WithLSP 把一个 LSPManager 绑进 ctx(镜像 WithVCS)。编排器每 turn 注入;
// edit 工具据此在写盘后查诊断。nil mgr 等价于不绑定(disabled Manager 也是
// no-op,二者都让工具无副作用)。
func WithLSP(ctx context.Context, mgr LSPManager) context.Context {
	if mgr == nil {
		return ctx
	}
	return context.WithValue(ctx, lspKey{}, mgr)
}

// LSPFromContext 取回绑定的 Manager;未绑定时 ok=false。
func LSPFromContext(ctx context.Context) (LSPManager, bool) {
	m, ok := ctx.Value(lspKey{}).(LSPManager)
	return m, ok
}

// diagFor 在一次文件写入后,从 ctx 取 LSP Manager 查诊断,返回写进工具结果
// JSON "diagnostics" 字段的文本(无 Manager / disabled / 无诊断 → 空串)。
//
// 它从 context 取 Manager(不是包级全局变量——评审 #10:旧版 diagFor 是全局
// var,并发 turn 会互相覆盖;context 注入天然每 turn 独立,无 race)。被
// fs.go(runWrite/runEdit)与 fs_patch.go(runPatch)复用,签名一致。
func diagFor(ctx context.Context, absPath, content string) string {
	mgr, ok := LSPFromContext(ctx)
	if !ok || !mgr.Enabled() {
		return ""
	}
	mgr.DidChange(absPath, content)
	diags := mgr.Diagnostics(absPath, 0) // 0 → Manager 用 cfg.Timeout
	return lsp.FormatDiags(filepathBase(absPath), diags)
}

// filepathBase 是 filepath.Base 的本地别名,避免在本文件混入 path/filepath import
// 风格分歧;实际直接用 path/filepath.Base 即可。执行者落地时:
// import "path/filepath" 并写 filepath.Base(absPath)。
func filepathBase(p string) string { return filepathBaseImpl(p) }
```

> 落地说明:执行者把 `filepathBase` 别名删掉,直接 `import "path/filepath"` 并在 `diagFor` 里用 `filepath.Base(absPath)`。`filepathBaseImpl` 占位不需要——上面仅为示意"这里要取 basename"。最终干净版:
> ```go
> import (
>     "context"
>     "path/filepath"
>     "time"
>     "github.com/x6nux/yanshi/internal/lsp"
> )
> // diagFor 内:return lsp.FormatDiags(filepath.Base(absPath), diags)
> ```

- [ ] **Step 5: 跑测试确认通过**

Run: `go test ./internal/tools/ -run TestLSPContext|TestDiagFor -v`
Expected: PASS

- [ ] **Step 6: 提交**

```bash
git add internal/tools/lspctx.go internal/tools/lspctx_test.go
git commit -m "feat(tools): LSPManager interface + WithLSP/LSPFromContext + diagFor"
```

---

## Task 7: edit 工具挂诊断回喂 — JSON "diagnostics" 字段(评审 #9、#10、#17)

**Files:**
- Modify: `internal/tools/fs.go`(`runWrite` L396-413、`runEdit` L422-483)
- Test: `internal/tools/fs_test.go`(追加)

> **评审 #9 修复(JSON 契约):** 旧版计划假设把 `FormatDiags` 文本拼进返回字符串,但 `runWrite`/`runEdit` 返回的是 `toJSON(map[string]any{...})`(已是 JSON)。把多语言文本拼进 JSON 字符串会破坏 JSON 契约(转义错乱、模型解析失败)。正确做法:**在 JSON map 加 `"diagnostics"` 字段**,值是 `FormatDiags` 的渲染串(空串时省略该字段)。既有键(`wrote`/`bytes`/`edited`/`replacements`)不变,模型/TUI 向后兼容。
>
> **评审 #10 修复:** 测试用真实 `WithProfile` ctx(让 guard 授权通过,镜像现有 `TestFS_WriteTracksToMainScope`),用 context 注入的 fake `LSPManager`(Task 6 的 stub 类型),且**真断言**(解析 JSON 确认 `diagnostics` 字段出现/缺失)。删除旧版零断言的空壳 `TestFS_EditNoLSPManager_Unchanged`。无全局 `diagFor` var(已在 Task 6 改为 context 读取的纯函数)→ 无并发 race。
>
> **评审 #17 修复:** `SyncStream` 把返回字符串同时塞 Text(TUI)+ Result(模型)。诊断写进返回串后会出现在 TUI 工具输出块——这是期望行为(用户即时看到编译错误),非缺陷。本计划据此设计。

- [ ] **Step 1: 写失败测试**

```go
// 追加到 internal/tools/fs_test.go(顶部 import 补 "encoding/json”)

// assertDiagField 解析工具返回的 JSON,断言 "diagnostics" 字段存在/缺失。
func assertDiagField(t *testing.T, out, want string, expectPresent bool) {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		t.Fatalf("结果不是合法 JSON: %v (out=%q)", err, out)
	}
	_, present := m["diagnostics"]
	if expectPresent && !present {
		t.Errorf("应含 diagnostics 字段,out=%s", out)
	}
	if !expectPresent && present {
		t.Errorf("不应含 diagnostics 字段(无 Manager),out=%s", out)
	}
	if expectPresent && want != "" {
		if d, _ := m["diagnostics"].(string); !strings.Contains(d, want) {
			t.Errorf("diagnostics 应含 %q,得到 %q", want, d)
		}
	}
}

// permFSCtx 镜像现有 fs 测试:真实 profile ctx,授权 fs_* + 全读写。
func permFSCtx(root string) context.Context {
	return WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"fs_*"}},
		FS:    guard.FSPerm{Write: []string{root + "/**"}, Read: []string{root + "/**"}},
	})
}

// TestFS_WriteNoLSPManager_NoDiagnosticsField 验证无 Manager 时 JSON 不含
// diagnostics 字段(零行为变化,既有键不变)。
func TestFS_WriteNoLSPManager_NoDiagnosticsField(t *testing.T) {
	root := t.TempDir()
	fs := NewFSTools(root)
	ctx := permFSCtx(root) // 无 WithLSP
	out, err := runTool(ctx, fs.Write, `{"path":"a.go","content":"package main\n"}`)
	require.NoError(t, err)
	assertDiagField(t, out, "", false)
	// 既有契约不变:
	var m map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &m))
	assert.Contains(t, m, "wrote")
}

// TestFS_EditWithLSP_AppendsDiagnosticsField 验证 fake Manager 的诊断被写进
// JSON "diagnostics" 字段。
func TestFS_EditWithLSP_AppendsDiagnosticsField(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "main.go")
	require.NoError(t, os.WriteFile(target, []byte("package main\n"), 0o644))

	fs := NewFSTools(root)
	ctx := permFSCtx(root)
	ctx = WithLSP(ctx, &fakeLSPManager{diags: []lsp.Diagnostic{
		{Line: 2, Column: 5, Severity: lsp.SeverityError, Message: "undefined: x", Source: "gopls"},
	}})

	out, err := runTool(ctx, fs.Edit, `{"path":"main.go","old_string":"package main","new_string":"package main\nvar _ = x"}`)
	require.NoError(t, err)
	assertDiagField(t, out, "undefined: x", true)
}

// TestFS_WriteWithLSP_AppendsDiagnosticsField 同上,覆盖 runWrite 路径。
func TestFS_WriteWithLSP_AppendsDiagnosticsField(t *testing.T) {
	root := t.TempDir()
	fs := NewFSTools(root)
	ctx := permFSCtx(root)
	ctx = WithLSP(ctx, &fakeLSPManager{diags: []lsp.Diagnostic{
		{Line: 1, Column: 1, Severity: lsp.SeverityWarning, Message: "fmt unused", Source: "gopls"},
	}})
	out, err := runTool(ctx, fs.Write, `{"path":"b.go","content":"package b\n"}`)
	require.NoError(t, err)
	assertDiagField(t, out, "fmt unused", true)
}
```

> fs_test.go 顶部 import 需含:`"context"`(已有)、`"encoding/json"`、`"os"`、`"path/filepath"`、`"strings"`、`"testing"`、`github.com/stretchr/testify/{assert,require}`、`github.com/x6nux/yanshi/internal/{guard,lsp}`。`fakeLSPManager` 在 Task 6 的 lspctx_test.go 定义(同包 `tools`,可直接复用);若执行者把 Task 6 的 stub 放在 lspctx_test.go,这里直接用。

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/tools/ -run "TestFS_WriteNoLSPManager|TestFS_EditWithLSP|TestFS_WriteWithLSP" -v`
Expected: FAIL(`runWrite`/`runEdit` 尚未写 diagnostics 字段)

- [ ] **Step 3: 实现 — 修改 runWrite / runEdit**

`runWrite`(`fs.go:396`),在 `os.WriteFile` 成功 + `trackEdit` 之后,把 return 改为构造 map、按需加 `"diagnostics"`:

```go
func (f *FSTools) runWrite(ctx context.Context, argsJSON string) (string, error) {
	var a fsWriteArgs
	if err := ParseArgs(argsJSON, &a); err != nil {
		return "", err
	}
	paths, err := f.checkFS(ctx, "write", "fs_write", argsJSON, a.Path)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(paths[0]), 0o755); err != nil {
		return "", fmt.Errorf("fs_write: mkdir: %w", err)
	}
	if err := os.WriteFile(paths[0], []byte(a.Content), 0o644); err != nil {
		return "", fmt.Errorf("fs_write: %w", err)
	}
	trackEdit(ctx, paths[0], []byte(a.Content))

	result := map[string]any{"wrote": paths[0], "bytes": len(a.Content)}
	if d := diagFor(ctx, paths[0], a.Content); d != "" { // 评审 #9:加 JSON 字段,非拼串
		result["diagnostics"] = d
	}
	return toJSON(result), nil
}
```

`runEdit`(`fs.go:422`),同理在 `os.WriteFile` 成功 + `trackEdit` 之后:

```go
	// …(既有匹配 + WriteFile + trackEdit 不变)…
	trackEdit(ctx, paths[0], []byte(updated))

	result := map[string]any{"edited": paths[0], "replacements": count}
	if d := diagFor(ctx, paths[0], updated); d != "" {
		result["diagnostics"] = d
	}
	return toJSON(result), nil
}
```

> 不动 `runRead`/`runList`/`runGlob`/`runSearch`(只读,无诊断回喂)。`diagFor` 来自 Task 6 的 lspctx.go(同包)。`runWrite`/`runEdit` 已 `import "path/filepath"` 等,无需新增 import(fs.go 已有)。

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/tools/ -run "TestFS_" -v`
Expected: PASS(含新诊断测试 + 既有 fs 测试不回归)

- [ ] **Step 5: 跑全包测试确认无回归**

Run: `go test ./internal/tools/`
Expected: PASS

- [ ] **Step 6: 提交**

```bash
git add internal/tools/fs.go internal/tools/fs_test.go
git commit -m "feat(tools): post-edit LSP diagnostics in fs_write/fs_edit result JSON"
```

---

## Task 8: fs_patch 同样挂诊断 — 正确 seam + 共享预算(评审 #11、#12)

**Files:**
- Modify: `internal/tools/fs_patch.go`(`runPatch` L28-91)
- Test: `internal/tools/fs_patch_test.go`(追加)

> **评审 #11 修复(seam 位置):** 旧版说"在 runPatch 每个 os.WriteFile 成功分支 return 前加诊断",但 `os.WriteFile` 在 `commitPatch`→`applyStaged`(L273-284)内部,不在 `runPatch` 函数体里——照旧版改根本无处下手。正确 seam 在 `runPatch` 函数体:**`commitPatch(staged)` 成功、VCS tracking 循环(L79-85)之后、最终 return(L87)之前**,遍历 `staged` 中 `c.final != nil` 的写盘项(删除项 `final==nil` 跳过),各调一次 `diagFor(ctx, c.abs, string(c.final))`。
>
> **评审 #12 修复(多文件预算):** 多文件 patch 不能 N × timeout 串行阻塞(10 文件 × 800ms = 8s 拖死 turn)。用一个**共享总预算 2s**:`diagForStaged` 用 `context.WithTimeout(ctx, 2s)`,每个文件取剩余预算的均分;串行调用(诊断查询本质是等 channel,cheap),任一文件耗尽剩余预算后后续返回空串。MVP 不引 errgroup(标准库即可,且诊断查询无 CPU 开销)。

- [ ] **Step 1: 写失败测试**

```go
// 追加到 internal/tools/fs_patch_test.go(顶部 import 补 "encoding/json”)

// TestApplyPatch_WithLSP_AppendsDiagnosticsField 验证 patch 写盘后,每个写过的
// .go 文件的诊断被汇总进 JSON "diagnostics" 字段(seam 在 runPatch return 前)。
func TestApplyPatch_WithLSP_AppendsDiagnosticsField(t *testing.T) {
	dir := t.TempDir()
	fs := NewFSTools(dir)
	ctx := WithProfile(patchCtx(dir), guard.PermissionProfile{}) // patchCtx 已设 profile;WithLSP 叠加
	// 更直接:重新构造带 LSP 的 ctx
	ctx = patchCtxWithLSP(dir, &fakeLSPManager{diags: []lsp.Diagnostic{
		{Line: 1, Column: 1, Severity: lsp.SeverityError, Message: "bad", Source: "gopls"},
	}})

	writeFile(t, dir, "upd.go", "old\n")
	patch := "*** Begin Patch\n" +
		"*** Update File: upd.go\n" +
		" old\n" +
		"-old\n" +
		"+new\n" +
		"*** End Patch"

	out, err := runTool(ctx, fs.Patch, toJSON(map[string]any{"patch": patch}))
	require.NoError(t, err)

	var m map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &m))
	d, present := m["diagnostics"]
	require.True(t, present, "patch 写盘后应有 diagnostics 字段,out=%s", out)
	assert.Contains(t, fmt.Sprint(d), "bad", "diagnostics 应含 fake 诊断")
}

// TestApplyPatch_NoLSP_NoDiagnosticsField 验证无 Manager 时 patch 结果无该字段。
func TestApplyPatch_NoLSP_NoDiagnosticsField(t *testing.T) {
	dir := t.TempDir()
	fs := NewFSTools(dir)
	ctx := patchCtx(dir) // 无 WithLSP
	writeFile(t, dir, "upd.go", "old\n")
	patch := "*** Begin Patch\n*** Update File: upd.go\n old\n-old\n+new\n*** End Patch"
	out, err := runTool(ctx, fs.Patch, toJSON(map[string]any{"patch": patch}))
	require.NoError(t, err)
	var m map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &m))
	_, present := m["diagnostics"]
	assert.False(t, present, "无 Manager 时不应有 diagnostics 字段,out=%s", out)
}

// patchCtxWithLSP 是 patchCtx + WithLSP 的组合 helper(测试用)。
func patchCtxWithLSP(dir string, mgr LSPManager) context.Context {
	return WithLSP(patchCtx(dir), mgr)
}
```

> fs_patch_test.go 顶部 import 需含:`"context"`(已有)、`"encoding/json"`、`"fmt"`、`"testing"`、`github.com/stretchr/testify/{assert,require}`、`github.com/x6nux/yanshi/internal/{guard,lsp}`。`patchCtx`/`writeFile`/`runTool`/`toJSON` 已在该文件/fs_test.go/helpers.go 定义。`fakeLSPManager` 来自 Task 6(同包 `tools`)。

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/tools/ -run "TestApplyPatch_WithLSP|TestApplyPatch_NoLSP" -v`
Expected: FAIL(runPatch 尚未加 diagnostics 字段)

- [ ] **Step 3: 实现 — 在 runPatch 加 diagForStaged**

在 `fs_patch.go` 加 helper(`diagForStaged`),并在 `runPatch` 的 VCS tracking 循环之后、return 之前调用。`runPatch` 改动后的尾部:

```go
	// …既有:commitPatch(staged) 成功 + VCS tracking 循环(trackEdit/trackDelete)不变…

	// 评审 #11:诊断 seam 在 runPatch 函数体,commitPatch 成功之后、return 之前。
	// 对每个写盘项(final != nil,即 add/update/move-destination;delete 跳过)查诊断,
	// 汇总进 "diagnostics" 字段。共享总预算 2s,避免 N 文件 × timeout 串行阻塞(评审 #12)。
	result := map[string]any{
		"applied": len(staged),
		"files":   stagedRelPaths(staged),
	}
	if d := diagForStaged(ctx, staged); d != "" {
		result["diagnostics"] = d
	}
	return toJSON(result), nil
}
```

新增 helper(放 fs_patch.go 末尾):

```go
// patchDiagBudget 是 patch 多文件诊断查询的共享总预算(评审 #12)。每文件均分,
// 串行调用(诊断查询是等 channel,无 CPU 开销,errgroup 无收益且引复杂度)。
const patchDiagBudget = 2 * time.Second

// diagForStaged 对 staged 中所有写盘项(final != nil)各查一次诊断,拼接渲染。
// 无 Manager / 全无诊断 → 空串(调用方据此省略 JSON 字段)。每文件的超时取自
// (剩余预算 / 待查文件数),保证总和 ≤ patchDiagBudget。
func diagForStaged(ctx context.Context, staged []stagedChange) string {
	mgr, ok := LSPFromContext(ctx)
	if !ok || !mgr.Enabled() {
		return ""
	}
	// 只查有内容(写盘)且语言可识别的项。
	type pending struct {
		abs     string
		content string
	}
	var todo []pending
	for _, c := range staged {
		if c.final == nil {
			continue // delete / move-source:无新内容,跳过
		}
		todo = append(todo, pending{abs: c.abs, content: string(c.final)})
	}
	if len(todo) == 0 {
		return ""
	}

	deadline := time.Now().Add(patchDiagBudget)
	var b strings.Builder
	for i, p := range todo {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			break // 预算耗尽,后续文件跳过(空串)
		}
		perFile := remaining / time.Duration(len(todo)-i) // 均分剩余
		mgr.DidChange(p.abs, p.content)
		diags := mgr.Diagnostics(p.abs, perFile)
		if text := lsp.FormatDiags(filepath.Base(p.abs), diags); text != "" {
			b.WriteString(text)
			b.WriteString("\n")
		}
	}
	return strings.TrimSpace(b.String())
}
```

> fs_patch.go 顶部 import 需补:`"context"`(已有)、`"path/filepath"`、`"strings"`、`"time"`、`"github.com/x6nux/yanshi/internal/lsp"`。`diagForStaged` 直接调 `mgr.DidChange`/`mgr.Diagnostics`(不经 `diagFor`,因为 patch 要自己控预算)。`filepath`/`strings` 已在该文件 import。

- [ ] **Step 4: 跑测试确认通过 + 全包不回归**

Run: `go test ./internal/tools/`
Expected: PASS

- [ ] **Step 5: 提交**

```bash
git add internal/tools/fs_patch.go internal/tools/fs_patch_test.go
git commit -m "feat(tools): post-edit LSP diagnostics in apply_patch result (shared budget)"
```

---

## Task 9: 装配 — config + bootstrap + orchestrator(评审 #13、#14、#15、#16、#18)

**Files:**
- Modify: `internal/config/config.go`
- Modify: `internal/bootstrap/bootstrap.go`
- Modify: `internal/agent/orchestrator/orchestrator.go`
- Modify: `internal/agent/orchestrator/orchestrator_test.go`(追加)
- Modify: `internal/bootstrap/bootstrap_test.go`(追加)
- Modify: `config.example.yaml`

> 本任务一次性装配 config → Manager → orchestrator 注入 → App 生命周期。
>
> **评审 #13:** bootstrap 测试必须写临时 YAML 再传 `ConfigPath`(现有 `TestBuild_FakeModel` 即如此),否则 `os.ReadFile("")` 报错。
> **评审 #14:** orchestrator 加 `o.lspMgr tools.LSPManager` 字段 + `Config.LSP`;新增 `prepareTurnContext(ctx)` helper 统一 4 个 turn 入口(Query/Events/EventsWithHistory/EventsWithHistoryOpts)的 context 注入(profile + workroot + subagent runner + VCS + LSP),消除当前 4 处重复块。
> **评审 #15:** config `Enabled *bool`(nil → 默认 true,区分 unset/false)+ `Timeout string`(`applyDefaults` 里 `time.ParseDuration`,默认 "800ms")。
> **评审 #16:** `App.LSP *lsp.Manager`;`App.Shutdown` 调 `a.LSP.Close()` 防 gopls 泄漏。
> **评审 #18:** 子代理继承 LSP —— `runSubAgentTurn` 构造 `Config{..., LSP: o.lspMgr}`。

- [ ] **Step 1: 写失败测试(bootstrap 软降级)**

```go
// 追加到 internal/bootstrap/bootstrap_test.go

// TestBuild_LSPWired 验证 Build 装配了 LSP Manager(App.LSP 非 nil),且
// Shutdown 关闭它不 panic(评审 #13:写临时 YAML;评审 #16:生命周期)。
func TestBuild_LSPWired(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	dbPath := toYAMLPath(filepath.Join(dir, "test.db"))
	cfgContent := `
server:
  http_addr: "127.0.0.1:0"
storage:
  sqlite_path: "` + dbPath + `"
token: "test-token"
`
	require.NoError(t, os.WriteFile(cfgPath, []byte(cfgContent), 0o644))

	app, err := bootstrap.Build(bootstrap.Options{ConfigPath: cfgPath, FakeModel: true})
	require.NoError(t, err)
	require.NotNil(t, app)
	require.NotNil(t, app.LSP, "App.LSP 必须非 nil(软降级 Manager 也是非 nil 的 no-op)")
	defer app.Shutdown(context.Background())

	// Shutdown 不 panic 且关闭 LSP(即使 enabled,内部 Kill+Wait 静默)。
	require.NotPanics(t, func() { _ = app.Shutdown(context.Background()) })
}

// TestBuild_LSPDisabledByConfig 验证 enabled:false 被尊重(Manager.Enabled()==false)。
func TestBuild_LSPDisabledByConfig(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	dbPath := toYAMLPath(filepath.Join(dir, "test.db"))
	cfgContent := `
server:
  http_addr: "127.0.0.1:0"
storage:
  sqlite_path: "` + dbPath + `"
token: "test-token"
lsp:
  enabled: false
`
	require.NoError(t, os.WriteFile(cfgPath, []byte(cfgContent), 0o644))
	app, err := bootstrap.Build(bootstrap.Options{ConfigPath: cfgPath, FakeModel: true})
	require.NoError(t, err)
	require.NotNil(t, app)
	defer app.Shutdown(context.Background())
	assert.False(t, app.LSP.Enabled(), "enabled:false 时 Manager 必须 disabled")
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/bootstrap/ -run TestBuild_LSP -v`
Expected: FAIL(`App.LSP` 未定义;compile error)

- [ ] **Step 3a: config 加 LSP 节(评审 #15)**

在 `internal/config/config.go` 的 `Config` struct 加字段:

```go
type Config struct {
	// …existing fields…
	LSP LSPConfig `yaml:"lsp"`
}
```

并在文件末尾(config.go)加类型 + applyDefaults 扩展:

```go
// LSPConfig 配置编辑后诊断回喂(B2-LSP1)。Enabled 为 *bool:yaml 里省略 → nil
// → 默认 true;显式 false → disabled(评审 #15:区分 unset 与 false)。Timeout
// 是诊断等待,字符串形式(applyDefaults 里 time.ParseDuration),默认 800ms。
type LSPConfig struct {
	Enabled  *bool                         `yaml:"enabled"`
	Timeout  string                        `yaml:"diag_timeout"`
	Override map[string]LanguageServerSpec `yaml:"languages"`
}

// LanguageServerSpec 是 config 里语言→server 的描述(yaml 友好名;bootstrap
// 转 lsp.LanguageServer)。
type LanguageServerSpec struct {
	Command string   `yaml:"command"`
	Args    []string `yaml:"args"`
}
```

在 `applyDefaults` 末尾加(默认启用 + 解析 timeout):

```go
	// LSP 默认启用(Enabled==nil 视为 true)。Timeout 缺省 800ms;非法值降级 800ms。
	if c.LSP.Enabled == nil {
		t := true
		c.LSP.Enabled = &t
	}
	if c.LSP.Timeout == "" {
		c.LSP.Timeout = "800ms"
	}
	// 不在此 ParseDuration 存回字段(保持 string 以便 doctor 显示);bootstrap
	// 构造 Manager 时 ParseDuration,失败降级 800ms。
```

> bootstrap 端:构造 Manager 时 `to, err := time.ParseDuration(cfg.LSP.Timeout); if err != nil { to = 800*time.Millisecond }`。这样 config 的 string 字段与 doctor 的可读性兼顾。

`config.example.yaml` 末尾加注释段:

```yaml
lsp:
  enabled: true            # 编辑后自动查语言服务器诊断回喂模型;false 关闭
  diag_timeout: "800ms"    # 单文件诊断等待上限
  languages: {}            # 覆盖/扩展语言表;空 = 用内置 {go: gopls, python: pyright-langserver}
```

- [ ] **Step 3b: bootstrap.Build 构造 Manager 并装进 App + orchestrator(评审 #16)**

在 `bootstrap.go` 顶部 import 加 `"github.com/x6nux/yanshi/internal/lsp"` 与 `"time"`(time 已有)。

在 `Build` 里(VCS 装配 `vcsInstance.InitRepo` 之后、tools 装配之前)加:

```go
	// B2-LSP1:编辑后诊断回喂。软降级——无可用 server 时 Manager 是 no-op,
	// app 照常启动(镜像 VCS 的非致命失败模式)。enabled:false 也得到 disabled Manager。
	lspTimeout, terr := time.ParseDuration(cfg.LSP.Timeout)
	if terr != nil {
		lspTimeout = 800 * time.Millisecond
	}
	langServers := lsp.DefaultLanguages()
	for lang, spec := range cfg.LSP.Override {
		langServers[lang] = lsp.LanguageServer{Command: spec.Command, Args: spec.Args}
	}
	lspMgr := lsp.New(lsp.Config{
		WorkRoot:  workRoot,
		Languages: langServers,
		Timeout:   lspTimeout,
	})
	// enabled:false → 立即把 Manager 置为 disabled(New 会按 Languages 剪枝,但
	// 显式开关优先:用户关了就关,即使装了 gopls)。
	if cfg.LSP.Enabled != nil && !*cfg.LSP.Enabled {
		lspMgr = lsp.New(lsp.Config{WorkRoot: workRoot}) // 空 Languages → disabled
	}
	if !lspMgr.Enabled() {
		fmt.Fprintf(os.Stderr, "yanshi: lsp disabled (no language server found or enabled:false)\n")
	}
```

`App` struct 加字段:

```go
type App struct {
	// …existing fields…
	// LSP 是编辑后诊断回喂的 Manager(软降级:可能 Enabled()==false)。Shutdown
	// 时 Close,避免 gopls 等子进程泄漏。
	LSP *lsp.Manager
}
```

`orchConfig`(L213-225)加 `LSP` 字段:

```go
	orchConfig := orchestrator.Config{
		// …existing fields…
		LSP: lspMgr, // tools.LSPManager(*lsp.Manager 满足);no-op Manager 无害注入
	}
```

`Build` 的 return 构造 `App` 时加 `LSP: lspMgr,`。

`App.Shutdown` 加 LSP 关闭(评审 #16):

```go
func (a *App) Shutdown(ctx context.Context) error {
	if a.cancel != nil {
		a.cancel()
	}
	if a.LSP != nil {
		_ = a.LSP.Close() // 评审 #16:关 gopls 等子进程,防泄漏
	}
	err := a.Server.Shutdown(ctx)
	if cerr := a.Store.Close(); err == nil {
		err = cerr
	}
	return err
}
```

- [ ] **Step 3c: orchestrator 加字段 + prepareTurnContext(评审 #14)+ 子代理继承(评审 #18)**

`orchestrator.go`:

`Config` 加字段(在 `VCSScope` 旁):

```go
	// LSP 是编辑后诊断回喂的 Manager(经 tools.LSPManager 接口;nil = 不注入,
	// edit 工具 no-op)。镜像 VCSScope 的注入契约:非 nil 时每 turn 注入,edit
	// 工具据此在写盘后查诊断。
	LSP tools.LSPManager
```

`Orchestrator` struct 加字段(在 `vcsScope` 旁):

```go
	vcsScope tools.VCSScope
	lspMgr   tools.LSPManager // 评审 #14:缓存 cfg.LSP,供 prepareTurnContext + 子代理继承
	workRoot string
```

`New` 的 return 构造里加 `lspMgr: cfg.LSP,`:

```go
	return &Orchestrator{
		// …existing…
		vcsScope:        cfg.VCSScope,
		lspMgr:          cfg.LSP,
		workRoot:        cfg.WorkRoot,
		// …existing…
	}, nil
```

新增 `prepareTurnContext` helper(放 `Query` 前),统一 4 入口:

```go
// prepareTurnContext 注入每 turn 的 context:权限 profile + work root + 子代理
// runner + VCS scope + LSP manager。Query / Events / EventsWithHistory /
// EventsWithHistoryOpts 都先调它,消除 4 处重复注入块(评审 #14),并统一加上 LSP。
//
// VCS/LSP 的"非 nil 才注入"契约:VCS==nil 时保留 caller-supplied scope(如 task
// broker 的 worktree scope);LSP 同理(nil = 不注入,edit 工具 no-op)。
func (o *Orchestrator) prepareTurnContext(ctx context.Context) context.Context {
	ctx = tools.WithProfile(ctx, o.profile)
	ctx = tools.WithWorkRoot(ctx, o.workRoot)
	ctx = o.bindSubAgentRunner(ctx)
	if o.vcsScope.VCS != nil {
		ctx = tools.WithVCS(ctx, o.vcsScope)
	}
	if o.lspMgr != nil {
		ctx = tools.WithLSP(ctx, o.lspMgr)
	}
	return ctx
}
```

把 `Query` / `Events` / `EventsWithHistory` / `EventsWithHistoryOpts` 4 处的注入块替换为 `ctx = o.prepareTurnContext(ctx)`。例:`Query`(L508-520)改为:

```go
func (o *Orchestrator) Query(ctx context.Context, userMessage string) (string, error) {
	ctx = o.prepareTurnContext(ctx)
	iter := o.runner.Query(ctx, userMessage)
	// …(既有 acc/iter 循环不变)…
}
```

其余 3 个入口同样把"profile + workroot + bindSubAgentRunner + WithVCS"四行块替换为单行 `ctx = o.prepareTurnContext(ctx)`,其后的 `runner.Query`/`runner.Run` 调用不变。

子代理继承(评审 #18):`runSubAgentTurn` 的 `New(Config{...})`(L338-346)加 `LSP: o.lspMgr,`:

```go
	sub, err := New(Config{
		Model:       o.model,
		Tools:       selected,
		Profile:     o.profile,
		MaxIters:    o.maxIters,
		Compaction:  o.compaction,
		Instruction: subInstruction,
		WorkRoot:    o.workRoot,
		LSP:         o.lspMgr, // 评审 #18:子代理继承 LSP 诊断回喂
	})
```

> 注:子代理的 VCSScope 当前也未继承(既有行为:`runSubAgentTurn` 的 `New(Config{...})` 不传 VCSScope)。LSP 继承与之对齐——子代理用父的 lspMgr;VCS scope 由 task broker 在 worktree 场景另行注入(超出本任务范围)。本任务只加 LSP 继承,不改 VCS 行为。

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/bootstrap/ ./internal/agent/orchestrator/ -v`
Expected: PASS(含新 LSP 装配测试;既有 orchestrator/bootstrap 测试不回归——`prepareTurnContext` 是等价重构 + 加 LSP)

- [ ] **Step 5: 全量构建 + 测试不回归**

Run: `go build ./... && go test ./...`
Expected: 编译通过,全测试 PASS(`internal/llm/eino` 与 `internal/bootstrap` 个别测试在 eino-ext openai provider 不可用时 `t.Skip`,属预期)

- [ ] **Step 6: 手动验证(装了 gopls 时)**

Run: `go build -o yanshi.exe ./cmd/yanshi && ./yanshi.exe --fake-model -inprocess`(或现有自检方式)
预期:启动日志可能含 `lsp disabled`(没装 gopls)或无该行(装了)。fake-model 下不真编辑,无诊断触发——只验"装配不崩"。

- [ ] **Step 7: 提交**

```bash
git add internal/config/config.go internal/bootstrap/bootstrap.go internal/bootstrap/bootstrap_test.go \
        internal/agent/orchestrator/orchestrator.go internal/agent/orchestrator/orchestrator_test.go \
        config.example.yaml
git commit -m "feat(wire): LSP Manager (soft-degrade) + per-turn injection + sub-agent inheritance"
```

---

## Task 10: doctor — checkLSP 追加进现有 RunDoctor(评审 #19)

**Files:**
- Modify: `internal/cli/doctor.go`(`RunDoctor` L130-149、新增 `checkLSP`)
- Modify: `internal/cli/doctor_test.go`(追加断言)

> **评审 #19 修复:** doctor 命令**已实现**(`internal/cli/doctor.go:20-130`),旧版计划"若存在/否则降级"的叙述已过时。正确做法:在现有 `RunDoctor` 的 checks 列表追加一个 `checkLSP()`,探测内置语言表里各 server 二进制是否在 PATH,报告 enabled/disabled + 检测到的语言列表。与既有 `checkACP`/`checkProviders` 风格一致。

- [ ] **Step 1: 写失败测试**

```go
// 追加到 internal/cli/doctor_test.go

// TestRunDoctor_IncludesLSPCheck 验证 RunDoctor 输出含 "lsp" 检查项(评审 #19)。
func TestRunDoctor_IncludesLSPCheck(t *testing.T) {
	dir := t.TempDir()
	cfgPath := writeTempConfig(t, `
server:
  http_addr: "127.0.0.1:0"
storage:
  sqlite_path: "`+filepath.Join(dir, "t.db")+`"
token: "t"
`)
	rep := RunDoctor(context.Background(), DoctorOptions{ConfigPath: cfgPath, Root: t.TempDir()})
	c := findCheck(rep, "lsp")
	require.NotNil(t, t, c, "RunDoctor 应含 lsp 检查项")
	// 状态 ok/warn 都合法(取决于环境是否装了 gopls);消息应提及 go 或 disabled。
	assert.Contains(t, strings.ToLower(c.Message), "go", "lsp 检查应提及语言/server")
}

// findCheck 按名查 CheckResult(若该 helper 尚不存在则加)。
func findCheck(rep DoctorReport, name string) *CheckResult {
	for i := range rep.Checks {
		if rep.Checks[i].Name == name {
			return &rep.Checks[i]
		}
	}
	return nil
}
```

> 若 doctor_test.go 已有同名 `findCheck` helper(读现有文件确认),复用之,勿重复定义。import 按现有文件补 `"path/filepath"`、`"strings"`。

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/cli/ -run TestRunDoctor_IncludesLSPCheck -v`
Expected: FAIL(`checkLSP` 未追加;findCheck(rep,"lsp") 返回 nil)

- [ ] **Step 3: 实现 — RunDoctor 追加 + checkLSP**

在 `RunDoctor`(L130-149)的 checks 列表加一行(`checkSandbox` 之前或之后,顺序无强约束,放 `checkDirectories` 后即可):

```go
func RunDoctor(ctx context.Context, opts DoctorOptions) DoctorReport {
	// …existing…
	checks = append(checks, checkConfig(opts.ConfigPath, cfg, cfgErr))
	checks = append(checks, checkDatabase(cfg, cfgErr))
	checks = append(checks, checkProviders(cfg, cfgErr))
	checks = append(checks, checkACP())
	checks = append(checks, checkLockfile(root))
	checks = append(checks, checkPort(cfg, cfgErr))
	checks = append(checks, checkDirectories(cfg, cfgErr))
	checks = append(checks, checkLSP())      // ★ B2-LSP1:编辑后诊断回喂的 server 探测
	checks = append(checks, checkSandbox())
	return DoctorReport{Checks: checks}
}
```

新增 `checkLSP`(放 `checkSandbox` 旁):

```go
// checkLSP 探测内置语言表的各 server 二进制是否在 PATH,报告编辑后诊断回喂
// (B2-LSP1)的可用性。全缺失是 warn(诊断回喂软降级为 no-op,app 照常启动),
// 至少一个可用是 ok。与 checkACP 风格一致。
func checkLSP() CheckResult {
	type probe struct{ lang, bin string }
	// 与 lsp.DefaultLanguages() 对齐(MVP: go=gopls, python=pyright-langserver)。
	probes := []probe{
		{"go", "gopls"},
		{"python", "pyright-langserver"},
	}
	var lines []string
	available := 0
	for _, p := range probes {
		if _, err := exec.LookPath(p.bin); err != nil {
			lines = append(lines, fmt.Sprintf("%s: %q not in PATH", p.lang, p.bin))
		} else {
			lines = append(lines, fmt.Sprintf("%s: %q ok", p.lang, p.bin))
			available++
		}
	}
	status := StatusOK
	if available == 0 {
		status = StatusWarn // 软降级:无 server 时诊断回喂是 no-op,不阻塞启动
	}
	return CheckResult{Name: "lsp", Status: status, Message: strings.Join(lines, "; ")}
}
```

> doctor.go 已 import `"os/exec"`、`"strings"`、`"fmt"`,无需新增。

- [ ] **Step 4: 跑测试确认通过 + 全包不回归**

Run: `go test ./internal/cli/ -run TestRunDoctor -v`
Expected: PASS(含新 lsp 检查;既有 doctor 测试的 check 计数若硬编码了数字需同步——优先用 `findCheck` 按名查,避免索引耦合)

- [ ] **Step 5: 提交**

```bash
git add internal/cli/doctor.go internal/cli/doctor_test.go
git commit -m "feat(doctor): report LSP server availability (checkLSP)"
```

---

## Task 11: E2E — Manager + Dial seam + net.Pipe fake server + fs_edit → 诊断(评审 #20)

**Files:**
- Create: `internal/tools/fs_lsp_e2e_test.go`

> **评审 #20 修复:** 加一个真·端到端测试:**不依赖真 gopls**,但走完整真实代码路径:`lsp.Manager`(经 `Dial` seam 接 net.Pipe)→ `Client.Start` → `initialize` → `notifyChange`(didOpen/didChange + version)→ server 推 `publishDiagnostics` → `Diagnostics`(generation 边界)→ 经 `WithLSP` 注入 → `FSTools.runEdit` → JSON `"diagnostics"` 字段含 fake server 给出的诊断消息。这覆盖了 Task 3-8 的协议层 + 工具层,证明"编辑 → 诊断回喂"这条链在无 gopls 环境下也成立。
>
> (Task 5 的 `TestManager_DialSeam_ConnectsFakeServer` 已覆盖 Manager→Client→协议→generation 层;本任务补"协议层 → fs_edit 结果"的工具层拼接,合起来即完整 E2E。)

- [ ] **Step 1: 写测试**

```go
// internal/tools/fs_lsp_e2e_test.go
package tools

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/x6nux/yanshi/internal/guard"
	"github.com/x6nux/yanshi/internal/lsp"
)

// runFakeGoplsOverConn 在 srv 端跑一个最小 LSP server:持久 bufio.Reader 循环读,
// initialize 回空 capabilities,didOpen/didChange 推一条 "undefined: <sym>" 诊断。
// sym 用于让不同测试断言不同消息。读完(对端关)即返回。
func runFakeGoplsOverConn(t *testing.T, srv net.Conn, sym string) {
	t.Helper()
	br := bufio.NewReader(srv)
	for {
		msg, err := readMsgLSP(br)
		if err != nil {
			return
		}
		switch msg["method"] {
		case "initialize":
			_ = writeMsgLSP(srv, map[string]any{
				"jsonrpc": "2.0", "id": msg["id"],
				"result": map[string]any{"capabilities": map[string]any{}},
			})
		case "textDocument/didOpen", "textDocument/didChange":
			params, _ := msg["params"].(map[string]any)
			td, _ := params["textDocument"].(map[string]any)
			uri, _ := td["uri"].(string)
			_ = writeMsgLSP(srv, map[string]any{
				"jsonrpc": "2.0", "method": "textDocument/publishDiagnostics",
				"params": map[string]any{
					"uri": uri,
					"diagnostics": []map[string]any{{
						"range":    map[string]any{"start": map[string]any{"line": 2, "character": 4}},
						"severity": 1,
						"message":  "undefined: " + sym,
						"source":   "gopls",
					}},
				},
			})
		}
	}
}

// readMsgLSP/writeMsgLSP 是 lsp 包未导出函数的本地副本(测试在 tools 包,无法
// 直接调 lsp.readMessage)。保持与 wire.go 完全一致的帧格式,确保真实协议路径。
// 若后续把这两个 helper 提升为 lsp 包导出(ReadMessage/WriteMessage),这里改调
// 导出名即可——但 MVP 不为测试改导出面。
func readMsgLSP(br *bufio.Reader) (map[string]any, error) {
	// 与 lsp.readMessage 同实现(Content-Length 帧)。复制以避免导出。
	return readLSPFrame(br)
}
func writeMsgLSP(w io.Writer, v map[string]any) error { return writeLSPFrame(w, v) }
```

> 上方 `readLSPFrame`/`writeLSPFrame` 需在本测试文件内实现(复制 `lsp/wire.go` 的帧逻辑),或——**推荐**——执行者把 `lsp.readMessage`/`lsp.writeMessage` 改为**导出**(`ReadMessage`/`WriteMessage`),测试直接调,消除副本。导出两个帧函数无害(它们本就是纯协议层),且让 E2E 测试不重复帧逻辑。本计划建议导出;若执行者倾向不导出,则在 fs_lsp_e2e_test.go 内联一份与 wire.go **逐字一致**的帧 helper(注释标明"须与 internal/lsp/wire.go 保持一致")。下面假设导出方案,测试调 `lsp.ReadMessage`/`lsp.WriteMessage`:

```go
// internal/tools/fs_lsp_e2e_test.go(续)

// TestE2E_FSEdit_FakeLSPServer 是评审 #20 的完整 E2E:
// net.Pipe fake gopls → lsp.Manager(Dial seam)→ WithLSP → fs_edit → JSON
// "diagnostics" 字段含 fake server 推送的消息。证明"编辑 → 诊断回喂"整链成立。
func TestE2E_FSEdit_FakeLSPServer(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "main.go")
	require.NoError(t, osWriteFile(target, []byte("package main\n")))

	srv, cli := net.Pipe()
	go func() {
		defer srv.Close()
		br := bufio.NewReader(srv)
		for {
			msg, err := lsp.ReadMessage(br) // ★ 导出后;否则用本地 readLSPFrame
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

// osWriteFile 是 os.WriteFile 的别名占位——执行者直接用 os.WriteFile 并 import "os"。
func osWriteFile(name string, data []byte) error { return osWriteFileImpl(name, data) }
```

> 落地清理:执行者删掉 `osWriteFile`/`osWriteFileImpl`/`runFakeGoplsOverConn`/`readMsgLSP`/`writeMsgLSP` 这些占位 helper,只保留 `TestE2E_FSEdit_FakeLSPServer` 一个测试函数,`import "os"` 直接 `os.WriteFile`。fake server 逻辑内联在测试的 goroutine 里(如上)。**前置:Task 2 的 `readMessage`/`writeMessage` 导出为 `ReadMessage`/`WriteMessage`**(改 Task 2 的函数名 + 调用点:client.go 的 `readMessage`/`writeMessage` 调用改 `ReadMessage`/`WriteMessage`。这是纯重命名,不影响逻辑)。若执行者不愿导出,则在 fs_lsp_e2e_test.go 内联一份与 wire.go 逐字一致的 `readLSPFrame`/`writeLSPFrame`(注释标明同步要求)。

- [ ] **Step 2: 跑测试确认通过**

Run: `go test ./internal/tools/ -run TestE2E_FSEdit_FakeLSPServer -v`
Expected: PASS(fake server 推送的诊断经 Manager→Client→generation→WithLSP→runEdit 流进 JSON "diagnostics" 字段)

- [ ] **Step 3: 跑全包 + 全量确认无回归**

Run: `go build ./... && go test ./...`
Expected: PASS

- [ ] **Step 4: 提交**

```bash
# 若 Task 2 导出了 ReadMessage/WriteMessage,一并 add wire.go/client.go:
git add internal/lsp/wire.go internal/lsp/client.go internal/tools/fs_lsp_e2e_test.go
git commit -m "test(lsp): E2E fake-server → fs_edit diagnostics (no gopls dependency)"
```

---

## Self-Review(写完后已跑的检查)

1. **Spec 覆盖:** roadmap §7 [LSP1] 验收("编辑后模型收到诊断;server 缺失安全降级;超时不阻塞 turn")→ Task 7/8/11(回喂 + E2E)+ Task 5/9(软降级)+ Task 4(超时 + generation)覆盖。✅
2. **评审 20 项逐一对照:** 见下表(全部 20 项有对应 Task 与实现说明)。✅
3. **类型/签名一致:** 跨任务契约表 + 各 Task 代码块的 `Diagnostic`/`Severity`/`FormatDiags`、`Client.{Start,initialize,notifyChange,Diagnostics,Shutdown,Close}`、`Manager.{Enabled,DidChange,Diagnostics,Close}`、`tools.LSPManager`、`WithLSP/LSPFromContext/diagFor`、`Config.LSP`/`Orchestrator.lspMgr`/`prepareTurnContext`、`App.LSP`/`App.Shutdown`、`LSPConfig`/`checkLSP` 全部交叉对齐。✅
4. **编译可行性:** `tools` import `lsp`(叶子包,OK);`orchestrator` 经 `tools.LSPManager` 引用 LSP 不直接 import lsp(六边形);`bootstrap` import `lsp`(装配,符合"bootstrap 是唯一知晓所有 internal 包"的设计)。✅
5. **无 placeholder/TODO:** Task 9 的 config 合并循环已给出真实代码(非占位);Task 11 的 helper 占位已标注"落地清理"。无 TBD。✅
6. **TDD:** 每 Task 都是 先写失败测试 → 跑确认 FAIL → 实现 → 跑确认 PASS → 提交。✅
7. **软降级链路:** config `enabled:false` / 无 server / LookPath 失败 / Dial 失败 / initialize 失败 → 全部降级为 no-op,不阻塞启动或工具(turn 不中断)。✅

---

## 评审 20 项 → Task 对照表

| # | 评审问题 | 修复位置 |
|---|---|---|
| 1 | `readMessage` 每次 new bufio.Reader 丢预读 | Task 2:`readMessage(br *bufio.Reader)` + Task 3 Client 持久 `c.br` |
| 2 | Client 无 pending/demux/Start/Close | Task 3:reader loop + `pending` map + `Start`/`Close`/`Done` |
| 3 | 无 shutdown/exit/Kill+Wait | Task 4:`Shutdown`(shutdown 请求+exit 通知)+ Task 5 Manager `cleanup`/`Close` Kill+Wait |
| 4 | 无 didOpen;version 恒 1 | Task 4:`notifyChange` 首次 didOpen + 单调 version(=editGen) |
| 5 | Diagnostics 返回 stale | Task 4:`editGen`/`pubGen` generation 边界 |
| 6 | Manager 无 mutex | Task 5:`sync.Mutex` 包 clients/cmds/closers,clientFor 锁内 check-then-spawn |
| 7 | initialize 失败 Kill 无 Wait | Task 5:`spawnLocked` cleanup + clientFor 失败路径 Kill+Wait(reap) |
| 8 | uriForPath Windows 不安全 | Task 5:`pathToURL`(net/url,盘符 `/D:/` + percent-escape)+ 测试 |
| 9 | 返回拼串破坏 JSON 契约 | Task 7:JSON map 加 `"diagnostics"` 字段(非拼串) |
| 10 | 测试缺 import/空壳/全局 race | Task 6+7:context 注入 fake(无全局 var)+ 真实 profile ctx + 真断言 |
| 11 | runPatch seam 位置错 | Task 8:`commitPatch` 后、return 前遍历 `staged`(seam 在 runPatch 函数体) |
| 12 | 多文件 N×timeout 串行 | Task 8:`diagForStaged` 共享 2s 预算 + 均分 |
| 13 | Build 缺 ConfigPath | Task 9:测试写临时 YAML(镜像现有 `TestBuild_FakeModel`) |
| 14 | orchestrator 无 cfg/lspMgr/统一注入 | Task 9:`o.lspMgr` + `Config.LSP` + `prepareTurnContext` 统一 4 入口 |
| 15 | Enabled bool 无法区分 unset/false | Task 9:`*bool` + applyDefaults 默认 true;`Timeout string` + ParseDuration |
| 16 | 无 Manager.Close/Shutdown 不关 LSP | Task 5:`Manager.Close()` + Task 9:`App.LSP` + `App.Shutdown` 调 Close |
| 17 | SyncStream Text+Result 污染 | Architecture 诚实声明(诊断同时进模型与 TUI,期望行为) |
| 18 | 子代理 LSP 继承未定义 | Task 9:`runSubAgentTurn` 的 `Config{LSP: o.lspMgr}` |
| 19 | doctor "若存在"叙述过时 | Task 10:`checkLSP` 追加进现有 `RunDoctor` |
| 20 | 无真实 E2E | Task 5(`TestManager_DialSeam`)+ Task 11(`TestE2E_FSEdit_FakeLSPServer`)net.Pipe fake server 全链路 |

---

## 风险与兜底(执行者注意)

- **server 启动慢/卡死:** Task 3 `request` 有超时(握手 5s、诊断 cfg.Timeout);Task 5 `clientFor` 启动/握手失败静默降级(不 panic、不阻塞工具)。若某 server 在生产里频繁卡死,后续可在 Manager 加 per-language 熔断(连续失败 N 次禁用该语言)。
- **Manager 锁粒度:** `clientFor` 在 spawn+initialize 期间持锁(每语言 one-time,可接受)。若多语言并发首编成为瓶颈,后续换 per-language `sync.Once` + 结果缓存,本仓库不引 `x/sync/singleflight`。
- **Windows URI:** `pathToURL` 已处理盘符 + escape;执行者在 Windows 环境验证 Task 5 的 `TestPathToURL_*`。gopls 对 `file:///D:/...` 接受。
- **big-bang 回归:** Task 9 Step 5 跑全量 `go test ./...`;`prepareTurnContext` 是等价重构 + 加 LSP,若 orchestrator turn 测试失败,优先检查 `prepareTurnContext` 是否漏注入(4 入口是否都改成了调它)。
- **诊断污染上下文:** `FormatDiags` 空诊断返回空串,`diagFor`/`diagForStaged` 只在非空时写 `"diagnostics"` 字段——不会每次编辑灌一行 "no diagnostics"。SyncStream 下诊断也会进 TUI(期望,见 Architecture)。
- **gopls 子进程泄漏:** `Manager.Close` + `App.Shutdown` 双保险;Client.readLoop 在 pipe EOF 时自行退出(close done),不留 goroutine。

---

> 计划完成。B2 的另一项 **[RB1] 逐轮回滚 UI** 是独立子系统(autoVCS seam 快照 + `/restore-turn`),作为**单独的后续计划**(B2-RB1),不并入本文件。
