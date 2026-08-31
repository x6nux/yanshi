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

// ServerHandler 是 server 主动推送消息的回调。readLoop 将 notification
// 和 server 发起的 request 统一投递给它。method 是 JSON-RPC method 字段值；
// params 是原始 params map。
//
// params 的内容来自外部 MCP server，属于不可信输入（untrusted external
// data）—— 消费方不得在未标注的情况下将其用于提示词或安全判定（参照把 PR
// 正文标为数据防提示注入的做法）。这是给消费端的标注约定：本包当前没有
// 机器判据钉住这个安全属性（测试只钉转发忠实性），消费接线落地时必须一并
// 补上真正的标注测试。
type ServerHandler func(method string, params map[string]any)

// StdioClient 通过 stdin/stdout pipe 与 MCP server 子进程通信。
// cmd 持有子进程引用，Close 时 Kill+Wait 回收；未托管子进程（如测试用 io.Pipe）
// 时 cmd 为 nil，Close 只关闭 writer。
//
// readLoop goroutine 持续读入站消息并 demux：带 id 的响应投递到
// pending[id] channel，通知按 method 路由到 ServerHandler，server
// 发起的请求（带 id + method）由 handler 处理后回复。
type StdioClient struct {
	r       *bufio.Reader
	rawR    io.Reader // 原始 reader，Close 时如实现 io.Closer 则关闭
	w       io.Writer
	cmd     *exec.Cmd
	// writeMu 只串行化 stdin 写入（WriteLineMessage 是 header+body 两次
	// Write，交错会撕裂帧）。它绝不能在阻塞 I/O 之外再被 readLoop 需要 ——
	// 曾与 pending 表共用一把锁：写阻塞在 pipe 上时 deliver 拿不到锁，响应
	// 永远投递不出去，对端停读 stdin 即整个 client 楔死（连 Close 都挂）。
	writeMu sync.Mutex
	nextID  int64
	timeout time.Duration

	// mu 只保护 pending 表与 closed 标记 —— 两者都是内存操作，临界区无 I/O。
	mu sync.Mutex

	pending map[int64]chan map[string]any
	done    chan struct{}
	closed  bool
	closeOnce sync.Once

	handler  ServerHandler // nil = 忽略 server 主动推送
	handlerMu sync.RWMutex

	// protocol is the MCP revision the session settled on at Initialize. Only
	// written by Initialize before any concurrent reader can observe it; the
	// readLoop goroutine it starts does not read it.
	protocol string
}

// NewStdioClient creates an MCP client communicating over stdin/stdout pipes.
func NewStdioClient(r io.Reader, w io.Writer) *StdioClient {
	return &StdioClient{
		r:       bufio.NewReader(r),
		rawR:    r,
		w:       w,
		timeout: 30 * time.Second,
		pending: make(map[int64]chan map[string]any),
		done:    make(chan struct{}),
	}
}

// SetHandler 注册 server 主动推送的回调（notification 与 server 发起的 request）。
// 应在 Initialize 之前调用以收到早期消息 —— readLoop 在 Initialize 时启动；
// 晚注册不报错，只是收不到注册前的那些消息。回调在 readLoop goroutine 中
// 同步执行（有测试钉住：阻塞的 handler 会停住后续消息的分发），耗时操作
// （如工具表刷新）必须自行 spawn goroutine。
func (c *StdioClient) SetHandler(h ServerHandler) {
	c.handlerMu.Lock()
	c.handler = h
	c.handlerMu.Unlock()
}

// SetCmd 绑定底层子进程，使 Close 能 Kill+Wait 回收它。链式返回 *StdioClient。
// Manager.startOne 在 cmd.Start() 之后调用；测试用 io.Pipe 时无需调用。
func (c *StdioClient) SetCmd(cmd *exec.Cmd) *StdioClient {
	c.cmd = cmd
	return c
}

// SetTimeout overrides the default IO timeout. Returns the client for chaining.
func (c *StdioClient) SetTimeout(d time.Duration) *StdioClient {
	if d > 0 {
		c.timeout = d
	}
	return c
}

// Initialize performs the MCP initialize handshake over stdio.
// It starts the readLoop goroutine so that the server's response and any
// notifications during the handshake are properly dispatched.
func (c *StdioClient) Initialize(ctx context.Context, rootURI string) error {
	go c.readLoop()
	id := atomic.AddInt64(&c.nextID, 1)
	resp, err := c.doRequest(ctx, id, map[string]any{
		"jsonrpc": "2.0", "id": id, "method": "initialize",
		"params": map[string]any{
			"protocolVersion": "2025-06-18",
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]any{"name": "yanshi-mcp", "version": "0.1"},
		},
	})
	if err != nil {
		return err
	}
	if e, ok := resp["error"]; ok {
		return fmt.Errorf("mcp: initialize error: %v", e)
	}
	result, _ := resp["result"].(map[string]any)
	offered, _ := result["protocolVersion"].(string)
	// 版本协商失败是硬错误：没有谈妥的版本，后续每个方法的语义按哪一版解释
	// 无人知晓（见 negotiateProtocolVersion）。
	version, err := negotiateProtocolVersion(offered)
	if err != nil {
		return err
	}
	c.protocol = version
	return c.notify("notifications/initialized", map[string]any{})
}

func (c *StdioClient) doRequest(ctx context.Context, id int64, req any) (map[string]any, error) {
	ch := make(chan map[string]any, 1)
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, fmt.Errorf("mcp: client closed")
	}
	c.pending[id] = ch
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}()

	c.writeMu.Lock()
	err := WriteLineMessage(c.w, req)
	c.writeMu.Unlock()
	if err != nil {
		return nil, err
	}

	deadline := time.After(c.timeout)
	select {
	case msg := <-ch:
		return msg, nil
	case <-deadline:
		return nil, fmt.Errorf("mcp: timeout waiting for id=%d", id)
	case <-c.done:
		return nil, fmt.Errorf("mcp: client closed")
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (c *StdioClient) notify(method string, params any) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return WriteLineMessage(c.w, map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

// readLoop 持续读入站消息并 demux，直到 ReadMessage 出错（EOF/pipe 关）。
// 三类消息分发：
//   - 响应（有 id，无 method）→ 投递给 pending[id] channel
//   - notification（无 id，有 method）→ 交给 handleServerMessage
//   - server 发起的 request（有 id 且有 method）→ 交给 handleServerRequest
//
// 退出时 close(done)，唤醒所有阻塞在 select 上的 doRequest。
func (c *StdioClient) readLoop() {
	defer c.closeOnce.Do(func() { close(c.done) })
	for {
		msg, err := ReadMessage(c.r)
		if err != nil {
			return
		}
		id, hasID := toInt64(msg["id"])
		method, hasMethod := msg["method"].(string)

		switch {
		case hasID && !hasMethod:
			// Response to a client request.
			c.deliver(id, msg)
		case hasMethod && !hasID:
			// Server-initiated notification.
			c.handleServerMessage(method, msg)
		case hasID && hasMethod:
			// Server-initiated request — respond and notify handler.
			c.handleServerRequest(id, method, msg)
		default:
			// Malformed JSON-RPC (no id, no method) — drop.
		}
	}
}

// deliver 投递响应到等待的 pending channel（非阻塞：channel 带 1 缓冲）。
func (c *StdioClient) deliver(id int64, msg map[string]any) {
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

// handleServerMessage 将 notification 投递给注册的 ServerHandler。
func (c *StdioClient) handleServerMessage(method string, msg map[string]any) {
	c.handlerMu.RLock()
	h := c.handler
	c.handlerMu.RUnlock()
	if h == nil {
		return
	}
	params, _ := msg["params"].(map[string]any)
	h(method, params)
}

// handleServerRequest 处理 server 发起的请求：回复 JSON-RPC error（"method not
// found"）并将原始 params 投递给 handler。params 按不可信外部输入的标注约定
// 交给消费方（见 ServerHandler；本包不提供机器判据，约定由消费端落实）。
//
// 回复 error 而非让 server 挂在超时上，因为当前 client 没有通用的
// request dispatcher——具体协议的 request handler 由上层 handler 自行实现，
// 这里只保证 server 不会因无响应而阻塞。
//
// 响应写放在 goroutine 中而非 readLoop 内联执行：write 阻塞（server 不消费
// stdin）时 readLoop 仍能继续分发后续通知/请求。写串行化走 writeMu，与
// pending 表的锁互不相干。
func (c *StdioClient) handleServerRequest(id int64, method string, msg map[string]any) {
	// Notify handler about the server request (untrusted params).
	c.handlerMu.RLock()
	h := c.handler
	c.handlerMu.RUnlock()
	params, _ := msg["params"].(map[string]any)
	if h != nil {
		h(method, params)
	}

	// Reply with "method not found" so the server doesn't hang. The write
	// runs in its own goroutine on writeMu only: a blocked write (server not
	// consuming stdin) must not stall the readLoop's dispatch of subsequent
	// messages, and must not hold the pending-table lock either.
	go func() {
		resp := map[string]any{
			"jsonrpc": "2.0",
			"id":      id,
			"error": map[string]any{
				"code":    -32601,
				"message": "method not found",
			},
		}
		c.writeMu.Lock()
		_ = WriteLineMessage(c.w, resp)
		c.writeMu.Unlock()
	}()
}

// ListTools returns tools advertised by the MCP server over stdio.
func (c *StdioClient) ListTools(ctx context.Context) ([]ToolDescriptor, error) {
	id := atomic.AddInt64(&c.nextID, 1)
	resp, err := c.doRequest(ctx, id, map[string]any{"jsonrpc": "2.0", "id": id, "method": "tools/list"})
	if err != nil {
		return nil, err
	}
	return parseToolList("stdio", resp)
}

// CallTool invokes a tool on the MCP server and returns its result.
func (c *StdioClient) CallTool(ctx context.Context, name string, args json.RawMessage) (json.RawMessage, error) {
	id := atomic.AddInt64(&c.nextID, 1)
	resp, err := c.doRequest(ctx, id, map[string]any{"jsonrpc": "2.0", "id": id, "method": "tools/call", "params": map[string]any{"name": name, "arguments": args}})
	if err != nil {
		return nil, err
	}
	return extractToolResult(resp)
}

// ListResources returns resources advertised by the MCP server over stdio.
func (c *StdioClient) ListResources(ctx context.Context) ([]ResourceDescriptor, error) {
	id := atomic.AddInt64(&c.nextID, 1)
	resp, err := c.doRequest(ctx, id, map[string]any{"jsonrpc": "2.0", "id": id, "method": "resources/list"})
	if err != nil {
		return nil, err
	}
	return parseResourceList("stdio", resp)
}

// ReadResource reads a resource by URI from the MCP server over stdio.
func (c *StdioClient) ReadResource(ctx context.Context, uri string) (json.RawMessage, error) {
	id := atomic.AddInt64(&c.nextID, 1)
	resp, err := c.doRequest(ctx, id, map[string]any{"jsonrpc": "2.0", "id": id, "method": "resources/read", "params": map[string]any{"uri": uri}})
	if err != nil {
		return nil, err
	}
	return extractResultRaw(resp)
}

// Ping checks liveness of the MCP server over stdio.
func (c *StdioClient) Ping(ctx context.Context) error {
	id := atomic.AddInt64(&c.nextID, 1)
	_, err := c.doRequest(ctx, id, map[string]any{"jsonrpc": "2.0", "id": id, "method": "ping"})
	return err
}

// Close kills the subprocess and closes the IO pipes.
// It signals the readLoop goroutine to exit and wakes any blocked doRequest.
func (c *StdioClient) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	c.mu.Unlock()

	if c.cmd != nil && c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
		_ = c.cmd.Wait()
	}
	// Closing the writer breaks any blocked write; closing the reader (if
	// reachable) breaks readLoop's blocking ReadMessage.
	if closer, ok := c.w.(io.Closer); ok {
		_ = closer.Close()
	}
	if closer, ok := c.rawR.(io.Closer); ok {
		_ = closer.Close()
	}
	// Signal readLoop to exit and wake blocked doRequest calls.
	// closeOnce guards against double-close if readLoop already exited.
	c.closeOnce.Do(func() { close(c.done) })
	return nil
}

// Done returns a channel that is closed when the readLoop goroutine exits
// (EOF/pipe closed) or Close is called. Callers can use it to wait for the
// client's reader to stop.
func (c *StdioClient) Done() <-chan struct{} { return c.done }

// --- helpers shared by StdioClient and HTTPClient ---

func parseToolList(server string, resp map[string]any) ([]ToolDescriptor, error) {
	res, _ := resp["result"].(map[string]any)
	if res == nil {
		return nil, fmt.Errorf("mcp: tools/list: no result")
	}
	raw, _ := res["tools"].([]any)
	out := make([]ToolDescriptor, 0, len(raw))
	for _, v := range raw {
		m, _ := v.(map[string]any)
		name, _ := m["name"].(string)
		if name == "" {
			continue
		}
		schemaBytes, _ := json.Marshal(m["inputSchema"])
		out = append(out, ToolDescriptor{ServerName: server, ToolName: name, Qualified: QualifyToolName(server, name), Description: strOr(m, "description"), InputSchema: string(schemaBytes)})
	}
	return out, nil
}

func parseResourceList(server string, resp map[string]any) ([]ResourceDescriptor, error) {
	res, _ := resp["result"].(map[string]any)
	if res == nil {
		return nil, fmt.Errorf("mcp: resources/list: no result")
	}
	raw, _ := res["resources"].([]any)
	out := make([]ResourceDescriptor, 0, len(raw))
	for _, v := range raw {
		m, _ := v.(map[string]any)
		uri, _ := m["uri"].(string)
		if uri == "" {
			continue
		}
		out = append(out, ResourceDescriptor{ServerName: server, URI: uri, Name: strOr(m, "name"), Description: strOr(m, "description"), MimeType: strOr(m, "mimeType")})
	}
	return out, nil
}

func extractToolResult(resp map[string]any) (json.RawMessage, error) {
	res, _ := resp["result"].(map[string]any)
	if res == nil {
		return nil, fmt.Errorf("mcp: tools/call: no result")
	}
	content, _ := res["content"].([]any)
	if len(content) == 0 {
		return json.RawMessage(`{}`), nil
	}
	first, _ := content[0].(map[string]any)
	text, _ := first["text"].(string)
	return json.RawMessage(text), nil
}

func extractResultRaw(resp map[string]any) (json.RawMessage, error) {
	if e, ok := resp["error"]; ok {
		return nil, fmt.Errorf("mcp: error: %v", e)
	}
	b, _ := json.Marshal(resp["result"])
	return b, nil
}

func strOr(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}

func toInt64(v any) (int64, bool) {
	n, ok := v.(float64)
	return int64(n), ok
}
