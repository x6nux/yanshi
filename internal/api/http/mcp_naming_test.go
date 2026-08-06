package http

import (
	"context"
	"testing"
	"time"

	"github.com/x6nux/yanshi/internal/mcp"
	"github.com/x6nux/yanshi/internal/tools"
)

// TestPaletteNamesMatchTheRegisteredToolNames reconciles the two ends.
//
// The palette shows td.Qualified and the orchestrator registers td.Qualified,
// which sounds like it cannot drift — but the two names travel through
// different code to get there. The palette's comes from Snapshot, through
// MCPStatusSnapshot, into an mcp_status frame; the registry's comes from
// ListAllTools, through NewMCPTools, into NewGuardedTool, and is read back out
// of Info(). A prefix added on one side, a case change, a collision suffix, and
// the operator types a name the model was never given.
//
// The breakdown named the trap for this clause: calling one formatter twice and
// asserting the results match is a tautology. Here one side is the wire
// snapshot and the other is the constructed tool registry — the same shape a
// mismatch would actually take.
//
// A real Manager over the package's fake HTTP server drives both.
//
// ledger: A3/MCP1#3 命名与模型可见一致
func TestPaletteNamesMatchTheRegisteredToolNames(t *testing.T) {
	srv, _ := mcp.NewFakeHTTPServer([]mcp.ToolDescriptor{
		{ToolName: "read_file", Description: "read a file"},
		{ToolName: "write_file", Description: "write a file"},
	})
	defer srv.Close()

	mgr := mcp.NewManager(map[string]*mcp.ServerConfig{
		"fsserver": {Enabled: true, Transport: mcp.TransportHTTP, URL: srv.URL},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, st := range mgr.StartAll(ctx) {
		if st.Error != "" {
			t.Fatalf("server %s failed to start: %s", st.Name, st.Error)
		}
	}
	defer mgr.Shutdown()

	// Palette side: exactly what the TUI renders rows from.
	paletteNames := map[string]bool{}
	for _, srvStatus := range MCPStatusSnapshot(mgr.Snapshot(ctx)) {
		for _, brief := range srvStatus.Tools {
			paletteNames[brief.Name] = true
		}
	}

	// Registry side: exactly what the orchestrator hands the model.
	registryNames := map[string]bool{}
	for _, tl := range tools.NewMCPTools(mgr) {
		info, err := tl.Info(ctx)
		if err != nil {
			t.Fatal(err)
		}
		registryNames[info.Name] = true
	}

	if len(paletteNames) == 0 || len(registryNames) == 0 {
		t.Fatalf("one side is empty (palette=%d registry=%d); this test cannot fail",
			len(paletteNames), len(registryNames))
	}

	for name := range paletteNames {
		if !registryNames[name] {
			t.Errorf("palette offers %q, which the model was never given: %v", name, keys(registryNames))
		}
	}
	for name := range registryNames {
		if !paletteNames[name] {
			t.Errorf("the model has %q but the palette does not offer it: %v", name, keys(paletteNames))
		}
	}
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
