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
	"path"
	"strings"
	"time"
)

// ServerTransport 标记 server 的传输方式。
type ServerTransport string

// MCP server transport protocols.
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
	OAuth     *OAuthConfig      `json:"oauth,omitempty" yaml:"oauth,omitempty"`
	// ToolAllow/ToolDeny narrow which of the server's advertised tools get
	// registered, by the server's own tool name (pre-qualification), exact or
	// `*`-glob. Empty allow admits all — the profile-level guard mcp
	// dimension remains the fail-closed authority; this is narrowing only.
	ToolAllow []string `json:"tool_allow,omitempty" yaml:"tool_allow,omitempty"`
	ToolDeny  []string `json:"tool_deny,omitempty" yaml:"tool_deny,omitempty"`
}

// AdmitsTool reports whether a tool advertised by this server (name is the
// server's own tool name, without the mcp_<server>_ qualification) passes the
// per-server allow/deny filter.
//
// Semantics, in order: a deny hit refuses — deny wins over allow, so a
// broad allow plus one exclusion is expressible; an empty allow admits
// everything not denied, because every config predating these fields must
// keep meaning exactly what it meant (all tools registered, guarded
// downstream by the profile's mcp dimension as before); a non-empty allow
// admits only what matches. Patterns are exact names or `*`-globs
// (path.Match); a malformed pattern matches nothing, which fails closed on
// the allow side and merely weakens the deny side — deny lists are written
// by the same hand as allow lists, and inventing a validation layer for one
// field is not this function's job.
func (cfg *ServerConfig) AdmitsTool(name string) bool {
	for _, pat := range cfg.ToolDeny {
		if ok, _ := path.Match(pat, name); ok {
			return false
		}
	}
	if len(cfg.ToolAllow) == 0 {
		return true
	}
	for _, pat := range cfg.ToolAllow {
		if ok, _ := path.Match(pat, name); ok {
			return true
		}
	}
	return false
}

// ConnectionStatus 表示一个 server 的实时连接状态。
type ConnectionStatus string

// MCP server connection status values.
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

// OAuthConfig 配置 MCP server 的 OAuth 2.0 token 获取。
// ClientSecret 已在 config.Load 的 ${VAR} 展开阶段解析；不要写入日志/status。
type OAuthConfig struct {
	TokenURL     string   `json:"token_url" yaml:"token_url"`
	ClientID     string   `json:"client_id" yaml:"client_id"`
	ClientSecret string   `json:"-" yaml:"client_secret"`
	Scopes       []string `json:"scopes,omitempty" yaml:"scopes,omitempty"`

	// Grant selects the OAuth flow. "" and "client_credentials" both mean the
	// machine-to-machine grant this package started with; "authorization_code"
	// selects the browser flow with PKCE.
	//
	// The empty string is kept as an alias rather than normalised away at load
	// time because every config that predates this field omits it, and a
	// missing value must keep meaning exactly what it meant before.
	Grant string `json:"grant,omitempty" yaml:"grant,omitempty"`

	// AuthorizationURL is the authorize endpoint. Required for
	// authorization_code and ignored for client_credentials, which has no
	// browser leg.
	AuthorizationURL string `json:"authorization_url,omitempty" yaml:"authorization_url,omitempty"`
}

// OAuth grant types accepted in OAuthConfig.Grant.
const (
	// GrantClientCredentials is the machine-to-machine grant. It is also what
	// an empty Grant means.
	GrantClientCredentials = "client_credentials"
	// GrantAuthorizationCode is the browser flow with PKCE, established
	// interactively by `yanshi auth mcp-login` and thereafter refreshed
	// without user interaction.
	GrantAuthorizationCode = "authorization_code"
)

// NormalizeGrant maps a configured grant string to its canonical form,
// reporting whether the value is one this package can speak.
//
// It rejects rather than defaulting on an unknown value. Defaulting would make
// a typo (`authorisation_code`) silently select the machine-to-machine grant,
// which then fails with "invalid_client" against a server expecting a user
// token — a failure that looks like a credential problem and is not.
func NormalizeGrant(s string) (string, bool) {
	switch strings.TrimSpace(strings.ToLower(s)) {
	case "", GrantClientCredentials:
		return GrantClientCredentials, true
	case GrantAuthorizationCode:
		return GrantAuthorizationCode, true
	default:
		return "", false
	}
}

// sanitizeName 把非字母数字字符替换为 `_`，合并连续 `_`，去掉前后 `_`，小写化。
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
