package mcp

import (
	"context"
	"encoding/json"
)

// Client 是 MCP server 的统一协议接口。StdioClient 与 HTTPClient 都实现此接口。
// Manager 只依赖此接口，便于用 FakeClient 测试。
type Client interface {
	Initialize(ctx context.Context, rootURI string) error
	ListTools(ctx context.Context) ([]ToolDescriptor, error)
	CallTool(ctx context.Context, name string, arguments json.RawMessage) (json.RawMessage, error)
	ListResources(ctx context.Context) ([]ResourceDescriptor, error)
	ReadResource(ctx context.Context, uri string) (json.RawMessage, error)
	Ping(ctx context.Context) error
	Close() error
}
