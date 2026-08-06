package tui

import (
	"strings"
	"testing"

	"github.com/x6nux/yanshi/internal/proto"
)

// mcpPaletteModel returns a model whose palette snapshot holds the given
// servers, with the palette already populated from them.
func mcpPaletteModel(t *testing.T, servers ...proto.MCPServerStatus) model {
	t.Helper()
	m := newTestModel(t)
	m.paletteMCPServers = servers
	m.paletteItems = m.paletteMCPItems()
	return m
}

// TestPaletteGroupsMCPToolsUnderTheirServer covers the grouping clause.
//
// TestPaletteKeepsMCPGroupHeaders already proves headers survive filtering. It
// does not prove they group: a list where every header is followed by the wrong
// server's tools, or where the header text is a constant, passes it. The
// assertions here are adjacency (a server's tools are contiguous and sit under
// its own header) and derivation (the header carries the server's name, not a
// fixed string).
//
// ledger: A3/MCP1#1 palette 含 MCP 工具分组
func TestPaletteGroupsMCPToolsUnderTheirServer(t *testing.T) {
	m := mcpPaletteModel(t,
		proto.MCPServerStatus{Name: "filesystem", Status: "ready", Tools: []proto.MCPToolBrief{
			{Name: "mcp_filesystem_read"}, {Name: "mcp_filesystem_write"},
		}},
		proto.MCPServerStatus{Name: "database", Status: "ready", Tools: []proto.MCPToolBrief{
			{Name: "mcp_database_query"},
		}},
	)

	// header, 2 tools, header, 1 tool
	if len(m.paletteItems) != 5 {
		t.Fatalf("got %d palette items, want 5: %+v", len(m.paletteItems), m.paletteItems)
	}

	var currentServer string
	seenHeaders := map[string]bool{}
	for _, it := range m.paletteItems {
		switch it.kind {
		case cmdMCPGroup:
			switch {
			case strings.Contains(it.name, "filesystem"):
				currentServer = "filesystem"
			case strings.Contains(it.name, "database"):
				currentServer = "database"
			default:
				t.Fatalf("header %q names neither server: the label is not derived from "+
					"the server name", it.name)
			}
			if seenHeaders[currentServer] {
				t.Errorf("server %q got a second header: its tools are not contiguous", currentServer)
			}
			seenHeaders[currentServer] = true
		case cmdMCPTool:
			if currentServer == "" {
				t.Errorf("tool %q appears before any header", it.name)
				continue
			}
			if !strings.Contains(it.name, currentServer) {
				t.Errorf("tool %q sits under the %q header", it.name, currentServer)
			}
		}
	}
	if len(seenHeaders) != 2 {
		t.Errorf("got %d headers, want one per server", len(seenHeaders))
	}
}

// TestPaletteShowsFailedServerToolsAsUnavailable covers the greying clause.
//
// "Visible and greyed" — not filtered. A failed server whose tools vanish is
// indistinguishable from one that exposes none, and the operator is left
// wondering whether they mistyped a name or whether the server is down.
//
// Three things have to hold together, and any one alone is satisfiable by a
// wrong implementation: the rows are still there, they read as unusable in
// PLAIN TEXT (colour is absent on a non-TTY and in a screenshot), and selecting
// one does not paste a name into the input that would come back as "unknown
// tool" with nothing tying the failure to the row.
//
// ledger: A3/MCP1#2 disabled/failed 可见标灰
func TestPaletteShowsFailedServerToolsAsUnavailable(t *testing.T) {
	m := mcpPaletteModel(t,
		proto.MCPServerStatus{Name: "broken", Status: "failed", Error: "connection refused",
			Tools: []proto.MCPToolBrief{{Name: "mcp_broken_do", Description: "does a thing"}}},
		proto.MCPServerStatus{Name: "fine", Status: "ready",
			Tools: []proto.MCPToolBrief{{Name: "mcp_fine_do", Description: "does a thing"}}},
	)

	var broken, fine *command
	for i := range m.paletteItems {
		switch m.paletteItems[i].name {
		case "mcp_broken_do":
			broken = &m.paletteItems[i]
		case "mcp_fine_do":
			fine = &m.paletteItems[i]
		}
	}
	if broken == nil {
		t.Fatal("the failed server's tool was filtered out; it must be visible and marked")
	}
	if fine == nil {
		t.Fatal("the healthy server's tool went missing")
	}
	if !broken.disabled {
		t.Error("the failed server's tool is not marked disabled")
	}
	if fine.disabled {
		t.Error("a ready server's tool was marked disabled; the marker carries no information then")
	}

	out := m.paletteBlock()
	if !strings.Contains(out, "mcp_broken_do") {
		t.Errorf("the failed tool does not render at all:\n%s", out)
	}
	if !strings.Contains(out, "(unavailable)") {
		t.Errorf("the rendered row carries no plain-text marker; styling alone is invisible "+
			"on a non-TTY:\n%s", out)
	}
	if strings.Contains(strings.Replace(out, "(unavailable)", "", 1), "(unavailable)") {
		t.Errorf("the healthy tool was marked unavailable too:\n%s", out)
	}

	// Selecting it must be refused.
	for i, it := range m.paletteItems {
		if it.name == "mcp_broken_do" {
			m.paletteSel = i
		}
	}
	before := m.input.Value()
	m.paletteComplete()
	if m.input.Value() != before {
		t.Errorf("selecting an unavailable tool pasted %q into the input; the call would "+
			"come back as an unknown tool with nothing linking it to the palette row",
			m.input.Value())
	}
}

// TestPaletteMoveTerminatesWhenEveryItemIsAHeader is a hang guard.
//
// paletteMove skipped headers with an unbounded loop. Every configured server
// exposing zero tools — what a fleet of failed servers looks like — makes the
// palette all headers, and the loop then spins forever with the UI frozen and
// nothing logged. The test would hang rather than fail, which is the honest
// signal for this defect: the go test timeout is what reports it.
func TestPaletteMoveTerminatesWhenEveryItemIsAHeader(t *testing.T) {
	m := mcpPaletteModel(t,
		proto.MCPServerStatus{Name: "a", Status: "failed"},
		proto.MCPServerStatus{Name: "b", Status: "failed"},
	)
	if len(m.paletteItems) != 2 {
		t.Fatalf("fixture is not all-headers: %+v", m.paletteItems)
	}
	m.paletteMove(1)
	m.paletteMove(-1)
}
