package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/x6nux/yanshi/internal/netpolicy"
)

// Manager 管理 MCP server 的连接生命周期。
type Manager struct {
	mu      sync.Mutex
	servers map[string]*ServerConfig
	clients map[string]Client
	toolMap map[string]ToolDescriptor
	// resourceMap caches each server's advertised resources, keyed by server
	// name. Collected once at start rather than on every Snapshot: Snapshot is
	// called from the status frame path and a per-call resources/list round
	// trip would put N network waits on a UI refresh.
	resourceMap       map[string][]ResourceDescriptor
	status            map[string]ConnectionStatus
	errs              map[string]string
	health            HealthConfig
	reconnectInflight map[string]struct{}
	// tokens persists OAuth material for authorization_code servers. nil is a
	// legitimate state (no secrets backend configured); buildTokenSource then
	// declines to build an authorization_code source and says so.
	tokens TokenStore
}

// SetTokenStore binds the credential backend used by authorization_code
// servers. Call it BEFORE StartAll: the source is constructed once per server
// at start, so a store bound afterwards is read by nothing.
func (m *Manager) SetTokenStore(store TokenStore) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tokens = store
}

// tokenStore reads the bound store under the lock. startOne runs without the
// mutex held (it does network I/O), so it cannot touch m.tokens directly.
func (m *Manager) tokenStore() TokenStore {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.tokens
}

// TokenStoreFor returns a token source for one configured server, so the CLI
// login leg can perform the interactive exchange against the same client id,
// endpoints and store the running manager would use.
//
// It returns an error rather than nil for an unconfigured server so `yanshi
// auth mcp-login nosuchserver` names the mistake instead of silently doing
// nothing.
func (m *Manager) TokenStoreFor(server string) (*AuthCodeSource, error) {
	m.mu.Lock()
	cfg, ok := m.servers[server]
	store := m.tokens
	m.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("mcp: no server named %q is configured", server)
	}
	if cfg.OAuth == nil {
		return nil, fmt.Errorf("mcp: server %q has no oauth block", server)
	}
	grant, valid := NormalizeGrant(cfg.OAuth.Grant)
	if !valid || grant != GrantAuthorizationCode {
		return nil, fmt.Errorf("mcp: server %q uses grant %q; only %s has an interactive login",
			server, cfg.OAuth.Grant, GrantAuthorizationCode)
	}
	if store == nil {
		return nil, fmt.Errorf("mcp: no credential store is configured; " +
			"set secrets.backend so the tokens can be persisted")
	}
	return NewAuthCodeSource(AuthCodeConfig{
		Server: server, TokenURL: cfg.OAuth.TokenURL,
		ClientID: cfg.OAuth.ClientID, ClientSecret: cfg.OAuth.ClientSecret,
		Scopes: cfg.OAuth.Scopes, Store: store,
	})
}

// OAuthEndpoints returns the authorization endpoint and scopes configured for
// server, for the CLI to build the browser URL from.
func (m *Manager) OAuthEndpoints(server string) (authURL, clientID string, scopes []string, err error) {
	m.mu.Lock()
	cfg, ok := m.servers[server]
	m.mu.Unlock()
	if !ok || cfg.OAuth == nil {
		return "", "", nil, fmt.Errorf("mcp: server %q has no oauth block", server)
	}
	if strings.TrimSpace(cfg.OAuth.AuthorizationURL) == "" {
		return "", "", nil, fmt.Errorf("mcp: server %q has no oauth.authorization_url; "+
			"the browser flow cannot start without it", server)
	}
	return cfg.OAuth.AuthorizationURL, cfg.OAuth.ClientID,
		append([]string(nil), cfg.OAuth.Scopes...), nil
}

// AuthCodeServers lists the configured servers that use the authorization_code
// grant, in sorted order. `yanshi auth mcp-login` with no argument prints it,
// so an operator does not have to grep their config for which names are valid.
func (m *Manager) AuthCodeServers() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for name, cfg := range m.servers {
		if cfg.OAuth == nil {
			continue
		}
		if grant, ok := NormalizeGrant(cfg.OAuth.Grant); ok && grant == GrantAuthorizationCode {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
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

// stdioServerEnv builds the environment for a stdio MCP server.
//
// The inherited half is CREDENTIAL-SCRUBBED. It used to be a raw os.Environ(),
// which handed every provider API key, cloud credential and VCS token in
// yanshi's own environment to a program named by a config file — the same class
// of program the guard's MCP dimension exists to gate, launched before any of
// that gating applies. An MCP server is a third-party binary; nothing about
// speaking that protocol implies a claim on the operator's OpenAI key.
//
// cfg.Env is layered AFTER the scrub and therefore always wins. That is the
// escape hatch and it is the right shape: a server that genuinely needs a token
// gets it because an operator wrote it down in mcp.servers.<name>.env, which is
// a decision with a name attached, rather than because it happened to be
// exported in the shell that started yanshi.
//
// Extracted from startOne so this is assertable without spawning a server —
// startOne's other job is the handshake, and a test that had to complete one
// could not check the environment of a server that failed to start.
func stdioServerEnv(cfg *ServerConfig) []string {
	env := netpolicy.ScrubbedEnviron()
	for k, v := range cfg.Env {
		env = append(env, k+"="+v)
	}
	return env
}

func (m *Manager) startOne(ctx context.Context, cfg *ServerConfig) (Client, error) {
	var cli Client
	switch cfg.Transport {
	case TransportHTTP:
		h := newHTTPClientFor(cfg, m.tokenStore())
		h.SetTimeout(cfg.Timeout)
		cli = h
	case TransportStdio:
		cmd := exec.Command(cfg.Command, cfg.Args...)
		cmd.Env = stdioServerEnv(cfg)
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
		// W-F-12: the per-server allow/deny filter is registration-side
		// narrowing only — dropped here, before anything downstream (the
		// guard's mcp dimension, GOV5's profile reconciliation) can see the
		// name. Empty allow admits all, so every config predating the field
		// registers exactly what it registered before.
		if !cfg.AdmitsTool(td.ToolName) {
			continue
		}
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

	// Resources are advertised alongside tools but were never collected:
	// ListResources had an interface entry and two implementations and ZERO
	// callers, so ServerStatus.Resources was permanently nil and /mcp could
	// only ever report that a server exposes none.
	//
	// A failure here is NOT fatal. resources/list is optional in MCP and a
	// server that only exposes tools answers with an error; refusing to start
	// it would turn a missing optional capability into an unusable server.
	if resources, rerr := cli.ListResources(startCtx); rerr == nil && len(resources) > 0 {
		m.mu.Lock()
		if m.resourceMap == nil {
			m.resourceMap = map[string][]ResourceDescriptor{}
		}
		m.resourceMap[cfg.Name] = resources
		m.mu.Unlock()
	}
	return cli, nil
}

// newHTTPClientFor 根据 cfg.OAuth 选择 bearer / client-credentials / authorization_code。
//
// An authorization_code server whose token source cannot be constructed —
// no store bound, missing client_id — falls back to the configured bearer
// rather than to an unauthenticated client. The fallback is not a silent
// downgrade: buildTokenSource is the only place that can tell, and it logs the
// reason. Returning an error instead would make one misconfigured server abort
// the whole MCP subsystem, which is the opposite of the soft-degrade posture
// StartAll already has.
func newHTTPClientFor(cfg *ServerConfig, store TokenStore) *HTTPClient {
	if src := buildTokenSource(cfg, store); src != nil {
		return NewHTTPClientWithTokenSource(cfg.URL, src)
	}
	return NewHTTPClient(cfg.URL, cfg.Bearer)
}

// buildTokenSource maps an OAuth config to a TokenSource, or nil when the
// server has none (or when one could not be built, having logged why).
func buildTokenSource(cfg *ServerConfig, store TokenStore) TokenSource {
	if cfg.OAuth == nil {
		return nil
	}
	grant, ok := NormalizeGrant(cfg.OAuth.Grant)
	if !ok {
		slog.Warn("mcp: unknown oauth grant; falling back to the configured bearer",
			"server", cfg.Name, "grant", cfg.OAuth.Grant)
		return nil
	}
	if grant == GrantClientCredentials {
		return NewClientCredentialsSource(
			cfg.OAuth.TokenURL, cfg.OAuth.ClientID, cfg.OAuth.ClientSecret, cfg.OAuth.Scopes, nil)
	}
	if store == nil {
		slog.Warn("mcp: authorization_code configured but no credential store is available; "+
			"falling back to the configured bearer (run `yanshi auth mcp-login` after configuring secrets)",
			"server", cfg.Name)
		return nil
	}
	src, err := NewAuthCodeSource(AuthCodeConfig{
		Server: cfg.Name, TokenURL: cfg.OAuth.TokenURL,
		ClientID: cfg.OAuth.ClientID, ClientSecret: cfg.OAuth.ClientSecret,
		Scopes: cfg.OAuth.Scopes, Store: store,
	})
	if err != nil {
		slog.Warn("mcp: authorization_code source could not be built; falling back to the configured bearer",
			"server", cfg.Name, "error", err.Error())
		return nil
	}
	return src
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
		st.Resources = m.resourceMap[name]
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
	m.resourceMap = map[string][]ResourceDescriptor{}
}
