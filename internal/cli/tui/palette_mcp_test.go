package tui

import (
	"github.com/x6nux/yanshi/internal/proto"
	"strings"
	"testing"
)

// TestPaletteKeepsMCPGroupHeaders pins a filter that discarded the thing it
// was meant to organise.
//
// paletteMCPItems emits a "── server ──" header before each server's tools so
// the user can tell which server a tool belongs to -- MCP tool names are
// qualified but long, and two servers can expose similarly-named tools. The
// filter then required HasPrefix(item.name, typedPrefix), which a header
// beginning with "──" can never satisfy. Every header was dropped and the
// tools arrived as one flat list with no indication of origin.
//
// A header is not a match candidate; it is a label for the matches under it.
// So the rule is: keep a header when at least one tool beneath it survived,
// and drop it when none did -- an empty group is worse than no group.
//
// ledger: A3/MCP1#1 palette 含 MCP 工具分组
func TestPaletteKeepsMCPGroupHeaders(t *testing.T) {
	m := newTestModel(t)
	m.paletteMCPServers = []proto.MCPServerStatus{
		{Name: "files", Status: "ready", Tools: []proto.MCPToolBrief{
			{Name: "mcp_files_read", Description: "read"},
			{Name: "mcp_files_write", Description: "write"},
		}},
		{Name: "db", Status: "ready", Tools: []proto.MCPToolBrief{
			{Name: "mcp_db_query", Description: "query"},
		}},
	}

	t.Run("a matching group keeps its header", func(t *testing.T) {
		m.input.SetValue("/mcp_files")
		m.updatePalette()

		var headers, tools int
		for _, it := range m.paletteItems {
			if it.kind == cmdMCPGroup {
				headers++
				if !strings.Contains(it.name, "files") {
					t.Errorf("kept a header for a server with no surviving tools: %q", it.name)
				}
			}
			if it.kind == cmdMCPTool {
				tools++
			}
		}
		if tools != 2 {
			t.Fatalf("want 2 matching tools, got %d", tools)
		}
		if headers != 1 {
			t.Fatalf("want exactly 1 group header (files), got %d: %+v", headers, m.paletteItems)
		}
	})

	t.Run("a header precedes its tools", func(t *testing.T) {
		m.input.SetValue("/mcp_")
		m.updatePalette()
		seenHeader := false
		for _, it := range m.paletteItems {
			if it.kind == cmdMCPGroup {
				seenHeader = true
			}
			if it.kind == cmdMCPTool && !seenHeader {
				t.Fatalf("tool %q appeared before any group header: %+v", it.name, m.paletteItems)
			}
		}
	})

	t.Run("no matches means no headers", func(t *testing.T) {
		m.input.SetValue("/zzz")
		m.updatePalette()
		for _, it := range m.paletteItems {
			if it.kind == cmdMCPGroup {
				t.Fatalf("an empty group is worse than no group: %q", it.name)
			}
		}
	})
}
