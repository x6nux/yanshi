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

// NewStdioClient creates an MCP client communicating over stdin/stdout pipes.
func NewStdioClient(r io.Reader, w io.Writer) *StdioClient {
	return &StdioClient{r: bufio.NewReader(r), w: w, timeout: 30 * time.Second}
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
func (c *StdioClient) Initialize(ctx context.Context, rootURI string) error {
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
func (c *StdioClient) Close() error {
	if c.cmd != nil && c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
		_ = c.cmd.Wait()
	}
	if closer, ok := c.w.(io.Closer); ok {
		_ = closer.Close()
	}
	return nil
}

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
