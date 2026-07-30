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

// NewHTTPClient creates an HTTP MCP client with an optional bearer token.
func NewHTTPClient(baseURL, bearer string) *HTTPClient {
	var source TokenSource
	if bearer != "" {
		source = &StaticTokenSource{Value: bearer}
	}
	return NewHTTPClientWithTokenSource(baseURL, source)
}

// NewHTTPClientWithTokenSource creates an HTTP MCP client with a custom token
// source (useful for OAuth-based authentication).
func NewHTTPClientWithTokenSource(baseURL string, source TokenSource) *HTTPClient {
	return &HTTPClient{
		baseURL: baseURL,
		tokens:  source,
		httpCli: &http.Client{Timeout: 30 * time.Second},
	}
}

// SetTimeout overrides the default HTTP client timeout.
func (c *HTTPClient) SetTimeout(d time.Duration) {
	if d > 0 {
		c.httpCli.Timeout = d
	}
}

// Initialize performs the MCP initialize handshake over HTTP.
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

// ListTools returns the list of tools advertised by the MCP server.
func (c *HTTPClient) ListTools(ctx context.Context) ([]ToolDescriptor, error) {
	resp, err := c.request(ctx, "tools/list", nil)
	if err != nil {
		return nil, err
	}
	return parseToolList("http", resp)
}

// CallTool invokes a tool on the MCP server and returns its result.
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

// ListResources returns resources advertised by the MCP server.
func (c *HTTPClient) ListResources(ctx context.Context) ([]ResourceDescriptor, error) {
	resp, err := c.request(ctx, "resources/list", nil)
	if err != nil {
		return nil, err
	}
	return parseResourceList("http", resp)
}

// ReadResource reads a resource by URI from the MCP server.
func (c *HTTPClient) ReadResource(ctx context.Context, uri string) (json.RawMessage, error) {
	resp, err := c.request(ctx, "resources/read", map[string]any{"uri": uri})
	if err != nil {
		return nil, err
	}
	return extractResultRaw(resp)
}

// Ping checks liveness of the MCP server.
func (c *HTTPClient) Ping(ctx context.Context) error {
	_, err := c.request(ctx, "ping", nil)
	return err
}

// Close releases the HTTP client and cancels the MCP session.
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
