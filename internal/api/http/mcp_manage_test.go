package http

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/x6nux/yanshi/internal/mcp"
	"github.com/x6nux/yanshi/internal/tools"
)

// startFakeMCP brings up a fake HTTP MCP server with the given tool names and
// a real Manager connected to it.
func startFakeMCP(t *testing.T, server string, toolNames ...string) (*mcp.Manager, context.Context) {
	t.Helper()
	descs := make([]mcp.ToolDescriptor, 0, len(toolNames))
	for _, n := range toolNames {
		descs = append(descs, mcp.ToolDescriptor{ToolName: n, Description: n + " does a thing"})
	}
	srv, _ := mcp.NewFakeHTTPServer(descs)
	t.Cleanup(srv.Close)

	mgr := mcp.NewManager(map[string]*mcp.ServerConfig{
		server: {Enabled: true, Transport: mcp.TransportHTTP, URL: srv.URL},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	for _, st := range mgr.StartAll(ctx) {
		if st.Error != "" {
			t.Fatalf("server %s failed to start: %s", st.Name, st.Error)
		}
	}
	t.Cleanup(mgr.Shutdown)
	return mgr, ctx
}

// TestMCPStatusCarriesTheManagersOwnErrorText covers the display clause.
//
// The existing TUI-side assertions check that the rendered block contains the
// template's own labels — "MCP", "status" — which is true of a render function
// replaced by a hardcoded constant. What has to be shown is the MANAGER's data:
// a failure reason that appears nowhere in the template, and a tool count that
// matches what the server actually offers.
//
// ledger: A3/C13#1 展示 server/tool/status/error
func TestMCPStatusCarriesTheManagersOwnErrorText(t *testing.T) {
	mgr, ctx := startFakeMCP(t, "good", "alpha", "beta")

	// A second server that cannot be reached. The URL is what produces the
	// reason text, so nothing in the render template can have anticipated it.
	const unreachable = "http://127.0.0.1:1/UNREACHABLE_MARKER"
	broken := mcp.NewManager(map[string]*mcp.ServerConfig{
		"broken": {Enabled: true, Transport: mcp.TransportHTTP, URL: unreachable},
	})
	brokenStatuses := broken.StartAll(ctx)
	t.Cleanup(broken.Shutdown)
	if len(brokenStatuses) == 0 || brokenStatuses[0].Error == "" {
		t.Fatalf("the unreachable server came up clean; this fixture proves nothing: %+v", brokenStatuses)
	}

	snap := MCPStatusSnapshot(append(mgr.Snapshot(ctx), broken.Snapshot(ctx)...))
	byName := map[string]int{}
	for i, s := range snap {
		byName[s.Name] = i
	}

	good, ok := byName["good"]
	if !ok {
		t.Fatalf("the healthy server is missing from the snapshot: %+v", snap)
	}
	if snap[good].Status != "ready" {
		t.Errorf("healthy server status=%q, want ready", snap[good].Status)
	}
	// The count has to come from the server, not from a constant.
	if snap[good].ToolCount != 2 || len(snap[good].Tools) != 2 {
		t.Errorf("tool count %d (%d briefs), want 2 — the fake server offers exactly two",
			snap[good].ToolCount, len(snap[good].Tools))
	}

	bad, ok := byName["broken"]
	if !ok {
		t.Fatalf("the failed server is missing from the snapshot: %+v", snap)
	}
	if snap[bad].Error == "" {
		t.Fatal("the failed server carries no error text; the operator has no reason to act on")
	}
	if !strings.Contains(snap[bad].Error, "UNREACHABLE_MARKER") {
		t.Errorf("error %q does not carry the manager's own reason (a marker no template "+
			"could contain): a generic message tells the operator nothing", snap[bad].Error)
	}
}

// TestMCPDisableRemovesToolsFromTheModelsView covers enable/disable.
//
// The breakdown named the cheap version: asserting a checkbox bit flipped. What
// "takes effect" means is that the model stops being offered the tools — the UI
// state is downstream of that, not evidence for it. The assertion is a
// before/after diff of the constructed tool registry, the same list the
// orchestrator is handed.
//
// ledger: A3/C13#2 enable/disable 生效
func TestMCPDisableRemovesToolsFromTheModelsView(t *testing.T) {
	mgr, ctx := startFakeMCP(t, "fsserver", "read_file", "write_file")

	names := func() map[string]bool {
		out := map[string]bool{}
		for _, tl := range tools.NewMCPTools(mgr) {
			info, err := tl.Info(ctx)
			if err != nil {
				t.Fatal(err)
			}
			out[info.Name] = true
		}
		return out
	}

	before := names()
	if len(before) != 2 {
		t.Fatalf("expected 2 registered tools before disabling, got %v", before)
	}

	if err := mgr.Disable(ctx, "fsserver"); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	if during := names(); len(during) != 0 {
		t.Errorf("after disabling, the model can still call %v — the server is stopped but "+
			"its tools are still advertised, so every call fails", during)
	}

	if err := mgr.Enable(ctx, "fsserver"); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	after := names()
	if len(after) != len(before) {
		t.Errorf("re-enabling did not restore the tools: before=%v after=%v", before, after)
	}
	for n := range before {
		if !after[n] {
			t.Errorf("tool %q did not come back after re-enabling", n)
		}
	}
}

// TestMCPStatusFollowsTheClientNotACachedFlag covers the consistency clause.
//
// A status field that is written once at startup and never revisited reports
// "ready" for a server that died an hour ago — and its tools keep being
// advertised. The status has to be derived from the connection as it is now.
//
// The disable path is the observable one here: it closes the client, and the
// next Snapshot must reflect that WITHOUT any extra refresh call, which is what
// the /mcp view relies on.
//
// ledger: A3/C13#3 状态与 client 实际连接一致
func TestMCPStatusFollowsTheClientNotACachedFlag(t *testing.T) {
	mgr, ctx := startFakeMCP(t, "srv", "tool_one")

	first := MCPStatusSnapshot(mgr.Snapshot(ctx))
	if len(first) != 1 || first[0].Status != "ready" {
		t.Fatalf("server did not come up ready: %+v", first)
	}

	if err := mgr.Disable(ctx, "srv"); err != nil {
		t.Fatalf("Disable: %v", err)
	}

	// No refresh, no restart — the very next snapshot.
	second := MCPStatusSnapshot(mgr.Snapshot(ctx))
	if len(second) != 1 {
		t.Fatalf("the server vanished from the snapshot instead of changing status: %+v", second)
	}
	if second[0].Status == "ready" {
		t.Error("status still reads ready after the client was closed: it is a cached flag, " +
			"not the connection state, so a dead server keeps advertising its tools")
	}
	if second[0].ToolCount != 0 {
		t.Errorf("a stopped server still reports %d tools", second[0].ToolCount)
	}
}
