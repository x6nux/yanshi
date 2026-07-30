package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"time"
)

// Manager 管理 MCP server 的连接生命周期。
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

// NewManager creates a Manager from a config map (server name -> ServerConfig).
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

// Enabled returns true when at least one MCP server is configured.
func (m *Manager) Enabled() bool {
	return m != nil && len(m.servers) > 0
}

// StartAll starts all enabled MCP servers and returns their statuses.
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
		cli, err := m.startOne(ctx, m.servers[name])
		if err != nil {
			m.mu.Lock()
			m.status[name] = StatusFailed
			m.errs[name] = err.Error()
			m.mu.Unlock()
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
		cmd := exec.Command(cfg.Command, cfg.Args...)
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

	timeout := 30 * time.Second
	if m.health.StartupTimeout > 0 {
		timeout = m.health.StartupTimeout
	} else if cfg.Timeout > 0 {
		timeout = cfg.Timeout
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
	temp := make(map[string]ToolDescriptor, len(tools))
	for _, td := range tools {
		td.ServerName = cfg.Name
		td.Qualified = QualifyToolName(cfg.Name, td.ToolName)
		if _, exists := m.toolMap[td.Qualified]; exists {
			m.mu.Unlock()
			_ = cli.Close()
			return nil, fmt.Errorf("mcp: tool name collision on %q (server %q)", td.Qualified, cfg.Name)
		}
		temp[td.Qualified] = td
	}
	// Atomic swap into toolMap after all names validated (review #2 orphan fix).
	for k, v := range temp {
		m.toolMap[k] = v
	}
	m.mu.Unlock()
	return cli, nil
}

// newHTTPClientFor 根据 cfg.OAuth 选择 bearer 或 OAuth 2.0 client-credentials token。
func newHTTPClientFor(cfg *ServerConfig) *HTTPClient {
	if cfg.OAuth != nil {
		src := NewClientCredentialsSource(cfg.OAuth.TokenURL, cfg.OAuth.ClientID, cfg.OAuth.ClientSecret, cfg.OAuth.Scopes, nil)
		return NewHTTPClientWithTokenSource(cfg.URL, src)
	}
	return NewHTTPClient(cfg.URL, cfg.Bearer)
}

// ListAllTools returns all discovered tools across all servers.
func (m *Manager) ListAllTools(context.Context) ([]ToolDescriptor, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]ToolDescriptor, 0, len(m.toolMap))
	for _, td := range m.toolMap {
		out = append(out, td)
	}
	return out, nil
}

// LookupTool finds a tool descriptor by its qualified name.
func (m *Manager) LookupTool(_ context.Context, q string) (ToolDescriptor, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	td, ok := m.toolMap[q]
	return td, ok
}

// CallTool invokes a tool by its qualified name.
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

// Disable stops and removes an MCP server.
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

// Enable starts a named MCP server and returns nil on success.
func (m *Manager) Enable(ctx context.Context, name string) error {
	m.mu.Lock()
	cfg := m.servers[name]
	m.mu.Unlock()
	if cfg == nil {
		return fmt.Errorf("mcp: unknown server %q", name)
	}
	cli, err := m.startOne(ctx, cfg)
	if err != nil {
		m.mu.Lock()
		m.status[name] = StatusFailed
		m.errs[name] = err.Error()
		m.mu.Unlock()
		return err
	}
	m.mu.Lock()
	m.clients[name] = cli
	m.status[name] = StatusReady
	delete(m.errs, name)
	m.mu.Unlock()
	return nil
}

// Validate pings each active server and returns their statuses.
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
			m.mu.Lock()
			m.status[n] = StatusFailed
			m.errs[n] = "ping: " + err.Error()
			m.mu.Unlock()
		} else {
			m.mu.Lock()
			m.status[n] = StatusReady
			delete(m.errs, n)
			m.mu.Unlock()
		}
	}
	return m.Snapshot(ctx)
}

// Reload stops and restarts a named MCP server.
func (m *Manager) Reload(ctx context.Context, name string) error {
	_ = m.Disable(ctx, name)
	return m.Enable(ctx, name)
}

// Snapshot returns the current status of all configured servers.
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

// Shutdown stops all MCP servers and releases resources.
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
