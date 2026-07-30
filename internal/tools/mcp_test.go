package tools

import (
	"context"
	"strings"
	"testing"

	"github.com/x6nux/yanshi/internal/guard"
	"github.com/x6nux/yanshi/internal/mcp"
)

func TestMCPToolBridge_PermissionAndCall(t *testing.T) {
	ts, _ := mcp.NewFakeHTTPServer([]mcp.ToolDescriptor{{ToolName: "echo"}})
	defer ts.Close()
	mgr := mcp.NewManager(map[string]*mcp.ServerConfig{"s": {Name: "s", Enabled: true, Transport: mcp.TransportHTTP, URL: ts.URL}})
	mgr.StartAll(context.Background())
	list := NewMCPTools(mgr)
	if len(list) != 1 {
		t.Fatalf("len=%d", len(list))
	}
	// MCP 工具必须同时通过 MCP.Allow 和通用 Tools.Allow。
	allowed := WithMCP(WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"mcp_*"}},
		MCP:   guard.ToolsPerm{Allow: []string{"mcp_s_*"}},
	}), mgr)
	res, err := list[0].InvokableRun(allowed, `{}`)
	if err != nil || !strings.Contains(res, `"ok":true`) {
		t.Fatalf("res=%s err=%v", res, err)
	}

	// fail-closed: MCP.Allow 空 → 拒绝，即使 Tools.Allow 是 "*"。
	noMCP := WithMCP(WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"*"}},
	}), mgr)
	res, _ = list[0].InvokableRun(noMCP, `{}`)
	if !strings.Contains(res, "permission denied") {
		t.Fatalf("expected MCP deny, got %s", res)
	}

	// fail-closed: MCP.Allow 放行但通用 Tools.Allow 未覆盖 → 仍拒绝。
	mcpOnly := WithMCP(WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"fs_*"}},
		MCP:   guard.ToolsPerm{Allow: []string{"mcp_s_*"}},
	}), mgr)
	res, _ = list[0].InvokableRun(mcpOnly, `{}`)
	if !strings.Contains(res, "permission denied") {
		t.Fatalf("expected Tools deny, got %s", res)
	}
}

// TestMCPToolBridge_NoManagerBound covers the MCPFromContext !ok branch at
// mcp.go:38-40: permission passes but no MCP manager is bound to the context.
func TestMCPToolBridge_NoManagerBound(t *testing.T) {
	ts, _ := mcp.NewFakeHTTPServer([]mcp.ToolDescriptor{{ToolName: "echo"}})
	defer ts.Close()
	mgr := mcp.NewManager(map[string]*mcp.ServerConfig{"s": {Name: "s", Enabled: true, Transport: mcp.TransportHTTP, URL: ts.URL}})
	mgr.StartAll(context.Background())
	list := NewMCPTools(mgr)
	if len(list) != 1 {
		t.Fatalf("len=%d", len(list))
	}
	// Profile allows the tool, but no MCP manager is bound via WithMCP.
	noManager := WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"mcp_*"}},
		MCP:   guard.ToolsPerm{Allow: []string{"mcp_s_*"}},
	})
	res, err := list[0].InvokableRun(noManager, `{}`)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if !strings.Contains(res, "MCP manager not bound") {
		t.Fatalf("expected not-bound error, got %s", res)
	}
}

// TestMCPToolBridge_CallError covers the CallTool error branch at mcp.go:42-44.
// Close the fake server after building tools so the CallTool HTTP request fails.
func TestMCPToolBridge_CallError(t *testing.T) {
	ts, _ := mcp.NewFakeHTTPServer([]mcp.ToolDescriptor{{ToolName: "echo"}})
	mgr := mcp.NewManager(map[string]*mcp.ServerConfig{"s": {Name: "s", Enabled: true, Transport: mcp.TransportHTTP, URL: ts.URL}})
	mgr.StartAll(context.Background())
	list := NewMCPTools(mgr)
	if len(list) != 1 {
		t.Fatalf("len=%d", len(list))
	}
	// Kill the server so the next CallTool fails at the HTTP layer.
	ts.Close()

	bound := WithMCP(WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"mcp_*"}},
		MCP:   guard.ToolsPerm{Allow: []string{"mcp_s_*"}},
	}), mgr)
	res, err := list[0].InvokableRun(bound, `{}`)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	// The CallTool error is surfaced as an errorResult, not a Go error.
	if strings.Contains(res, `"ok":true`) {
		t.Fatalf("expected error result, got %s", res)
	}
}
