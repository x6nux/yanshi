package proto

import (
	"testing"
)

func TestMCPFrames(t *testing.T) {
	cf := NewMCPAction("s", "enable")
	if cf.Type != "mcp_action" || cf.MCPServer != "s" || cf.MCPAction != "enable" {
		t.Fatalf("NewMCPAction: %+v", cf)
	}
	sf := NewMCPStatusFrame([]MCPServerStatus{{Name: "s", Status: "ready", Tools: []MCPToolBrief{}}})
	if sf.Type != "mcp_status" || len(sf.MCPServers) != 1 || sf.MCPServers[0].Name != "s" {
		t.Fatalf("NewMCPStatusFrame: %+v", sf)
	}
}
