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
	// ListResourcesPage 是分页形态的 resources/list：cursor 为空取首页，
	// 返回值里的 nextCursor 为空表示没有更多页。
	ListResourcesPage(ctx context.Context, cursor string) ([]ResourceDescriptor, string, error)
	// SubscribeResource 请求 server 在 uri 变更时推送
	// notifications/resources/updated。resources/subscribe 在 MCP 里是可选
	// 能力，server 可以拒绝 —— 调用方按非致命处理。
	SubscribeResource(ctx context.Context, uri string) error
	UnsubscribeResource(ctx context.Context, uri string) error
	ReadResource(ctx context.Context, uri string) (json.RawMessage, error)
	Ping(ctx context.Context) error
	Close() error
}
