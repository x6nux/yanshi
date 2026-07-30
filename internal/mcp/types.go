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

// OAuthConfig 配置 OAuth 2.0 client-credentials token 获取。
// ClientSecret 已在 config.Load 的 ${VAR} 展开阶段解析；不要写入日志/status。
type OAuthConfig struct {
	TokenURL     string   `json:"token_url" yaml:"token_url"`
	ClientID     string   `json:"client_id" yaml:"client_id"`
	ClientSecret string   `json:"-" yaml:"client_secret"`
	Scopes       []string `json:"scopes,omitempty" yaml:"scopes,omitempty"`
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
