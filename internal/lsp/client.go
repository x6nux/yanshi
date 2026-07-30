package lsp

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"path/filepath"
	"runtime"
	"strings"
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

// readLoop 持续读入站消息并 demux,直到 ReadMessage 出错(EOF/pipe 关)。
// 退出时 close(done),让所有阻塞在 select 上的 request 解除并返回 errClosed。
func (c *Client) readLoop() {
	defer c.closeOnce.Do(func() { close(c.done) })
	for {
		msg, err := ReadMessage(c.br) // ★ 复用持久 c.br
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
			timeout = rem
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

// writeMsg 串行化写:server 要求请求/通知按序到达 stdin。
func (c *Client) writeMsg(m map[string]any) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return WriteMessage(c.w, m)
}

// initialize 发 LSP initialize 请求并等待响应,然后发 initialized 通知。
// rootURI 用 pathToURL 规范化(Windows 安全)。Start() 必须已调用。
func (c *Client) initialize(rootPath string) error {
	rootURI := pathToURL(rootPath)
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

// Close 置 closed 标志(让后续 request 立即返回 errClosed)并 close(done) 以唤醒
// 阻塞的 select。它不关闭 r/w —— 那些由 Manager 拥有(exec 的 stdin pipe / Dial 的
// closer)。readLoop 退出时也会 closeOnce.Do(close(done)) —— Close 先跑的话该 defer
// 变成 no-op,由 Close 自己负责 close(done)(review #3 修复:之前 Close 只消费
// closeOnce 设 closed,导致 readLoop 的 defer 不再 close(done),阻塞的 request 永
// 远等不到 <-c.done)。
func (c *Client) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	c.mu.Unlock()
	c.closeOnce.Do(func() { close(c.done) })
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

// storeDiags 把一条 publishDiagnostics 的 params 解进 diags,并盖写 pubGen =
// 此刻的 editGen(评审 #5)。readLoop 在收到该通知时调用。
// LSP 行/列是 0-based,这里 +1 转成 1-based 供模型/人定位。
func (c *Client) storeDiags(msg map[string]any) {
	params, _ := msg["params"].(map[string]any)
	if params == nil {
		return
	}
	uri, _ := params["uri"].(string)
	// Use the diagnostic's version (from the params top-level, per LSP spec)
	// when available, so rapid notifyChange calls don't stamp stale diagnostics
	// from a prior version as covering the current editGen.
	verFloat, _ := params["version"].(float64)
	pubVersion := int64(verFloat)
	raw, _ := params["diagnostics"].([]any)
	out := make([]Diagnostic, 0, len(raw))
	for _, r := range raw {
		if d, ok := r.(map[string]any); ok {
			out = append(out, parseDiagnostic(d))
		}
	}
	c.diagMu.Lock()
	c.diags[uri] = out
	if pubVersion > 0 {
		c.pubGen[uri] = pubVersion
	} else {
		c.pubGen[uri] = c.editGen[uri]
	}
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
		return cloneDiag(d)
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
			c.diagMu.Lock()
			stale := c.diags[uri]
			c.diagMu.Unlock()
			return cloneDiag(stale)
		}
		time.Sleep(15 * time.Millisecond)
	}
}

// cloneDiag 返回 diags 的副本,避免调用方在 Manager 锁外改到 Client 内部切片。
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

func toInt64Or(v any, def int64) int64 {
	if n, ok := toInt64(v); ok {
		return n
	}
	return def
}
