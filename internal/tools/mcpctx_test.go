package tools

import (
	"context"
	"testing"

	"github.com/x6nux/yanshi/internal/mcp"
)

func TestMCPContext(t *testing.T) {
	if _, ok := MCPFromContext(context.Background()); ok {
		t.Fatal("unexpected")
	}
	mgr := mcp.NewManager(nil)
	got, ok := MCPFromContext(WithMCP(context.Background(), mgr))
	if !ok || got != mgr {
		t.Fatal("round-trip failed")
	}
}
