package tools

import (
	"context"

	"github.com/x6nux/yanshi/internal/mcp"
)

type mcpCtxKey struct{}

// WithMCP 将 MCP Manager 注入 context。若 mgr 为 nil 则返回原 ctx。
func WithMCP(ctx context.Context, mgr *mcp.Manager) context.Context {
	if mgr == nil {
		return ctx
	}
	return context.WithValue(ctx, mcpCtxKey{}, mgr)
}

// MCPFromContext 从 context 中取出 MCP Manager。
func MCPFromContext(ctx context.Context) (*mcp.Manager, bool) {
	m, ok := ctx.Value(mcpCtxKey{}).(*mcp.Manager)
	return m, ok
}
