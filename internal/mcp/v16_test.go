package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

// helperEnv makes the test binary re-enter itself as a stdio MCP server.
//
// The Manager's stdio path spawns a real subprocess: exec.Command, pipes,
// framing, the initialize handshake, and teardown. fakeStdioProcess drives the
// CLIENT over io.Pipe and never touches any of that — so "stdio servers can be
// configured and connected" had no Manager-level coverage at all. Re-entering
// the test binary is the standard way to get a real process without shipping a
// fixture binary or depending on anything being installed.
const helperEnv = "YANSHI_MCP_STDIO_HELPER"

// TestHelperStdioMCPServer is not a test. It is the subprocess body, and it
// returns immediately unless the parent asked for it.
func TestHelperStdioMCPServer(t *testing.T) {
	if os.Getenv(helperEnv) != "1" {
		t.Skip("subprocess helper; runs only when the parent sets " + helperEnv)
	}
	in := bufio.NewReader(os.Stdin)
	for {
		msg, err := ReadMessage(in)
		if err != nil {
			return
		}
		id := msg["id"]
		method, _ := msg["method"].(string)
		if id == nil {
			continue // notification
		}
		resp := map[string]any{"jsonrpc": "2.0", "id": id}
		switch method {
		case "initialize":
			resp["result"] = map[string]any{
				"protocolVersion": "2025-06-18", "capabilities": map[string]any{},
			}
		case "tools/list":
			resp["result"] = map[string]any{"tools": []map[string]any{
				{"name": "stdio_alpha", "description": "a", "inputSchema": map[string]any{"type": "object"}},
				{"name": "stdio_beta", "description": "b", "inputSchema": map[string]any{"type": "object"}},
			}}
		case "resources/list":
			resp["result"] = map[string]any{"resources": []map[string]any{}}
		case "ping":
			resp["result"] = map[string]any{}
		default:
			resp["result"] = map[string]any{}
		}
		data, _ := json.Marshal(resp)
		_, _ = os.Stdout.Write(append(data, '\n'))
	}
}

// managerToolNames returns the qualified names the manager holds for a server.
func managerToolNames(t *testing.T, m *Manager, ctx context.Context, server string) []string {
	t.Helper()
	var out []string
	for _, st := range m.Snapshot(ctx) {
		if st.Name != server {
			continue
		}
		for _, td := range st.Tools {
			out = append(out, td.Qualified)
		}
	}
	return out
}

// TestManagerConnectsOverBothTransports covers the transport clause at the
// Manager level.
//
// Both transports had client-level tests — StdioClient over io.Pipe, HTTPClient
// against httptest — and neither exercised the Manager: the config-to-connection
// path, the spawn, and the tool enumeration that follows. A transport that was
// wired up wrongly in ServerConfig handling would leave both of those green.
//
// ledger: A3/V16#1 stdio/HTTP server 可配可连
func TestManagerConnectsOverBothTransports(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	t.Run("stdio", func(t *testing.T) {
		mgr := NewManager(map[string]*ServerConfig{"viastdio": {
			Enabled: true, Transport: TransportStdio,
			Command: os.Args[0],
			Args:    []string{"-test.run=^TestHelperStdioMCPServer$"},
			Env:     map[string]string{helperEnv: "1"},
		}})
		defer mgr.Shutdown()
		for _, st := range mgr.StartAll(ctx) {
			if st.Error != "" {
				t.Fatalf("stdio server did not connect: %s", st.Error)
			}
		}
		got := managerToolNames(t, mgr, ctx, "viastdio")
		want := []string{"mcp_viastdio_stdio_alpha", "mcp_viastdio_stdio_beta"}
		assertSameNames(t, got, want)
	})

	t.Run("http", func(t *testing.T) {
		srv, _ := NewFakeHTTPServer([]ToolDescriptor{
			{ToolName: "http_alpha"}, {ToolName: "http_beta"},
		})
		defer srv.Close()
		mgr := NewManager(map[string]*ServerConfig{"viahttp": {
			Enabled: true, Transport: TransportHTTP, URL: srv.URL,
		}})
		defer mgr.Shutdown()
		for _, st := range mgr.StartAll(ctx) {
			if st.Error != "" {
				t.Fatalf("http server did not connect: %s", st.Error)
			}
		}
		got := managerToolNames(t, mgr, ctx, "viahttp")
		want := []string{"mcp_viahttp_http_alpha", "mcp_viahttp_http_beta"}
		assertSameNames(t, got, want)
	})
}

func assertSameNames(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d tools %v, want %d %v", len(got), got, len(want), want)
	}
	have := map[string]bool{}
	for _, g := range got {
		have[g] = true
	}
	for _, w := range want {
		if !have[w] {
			t.Errorf("tool %q is missing; the manager enumerated %v", w, got)
		}
	}
}

// TestSameToolNameOnTwoServersCoexists covers the collision clause.
//
// The breakdown allowed either outcome: the two coexist under server-prefixed
// names, or a diagnostic names BOTH servers. This implementation takes the
// first branch — QualifyToolName namespaces every tool — and this pins that it
// really is namespacing rather than one server silently overwriting the other.
//
// The overwrite is the failure worth guarding: it is invisible (both servers
// report ready, the tool count looks right on each) and it routes calls to the
// wrong server, which answers plausibly because it implements a tool by that
// name too.
//
// ledger: A3/V16#4 命名冲突可诊断
func TestSameToolNameOnTwoServersCoexists(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Both servers advertise a tool called exactly "search".
	one, _ := NewFakeHTTPServer([]ToolDescriptor{{ToolName: "search", Description: "from one"}})
	defer one.Close()
	two, _ := NewFakeHTTPServer([]ToolDescriptor{{ToolName: "search", Description: "from two"}})
	defer two.Close()

	mgr := NewManager(map[string]*ServerConfig{
		"alpha": {Enabled: true, Transport: TransportHTTP, URL: one.URL},
		"bravo": {Enabled: true, Transport: TransportHTTP, URL: two.URL},
	})
	defer mgr.Shutdown()
	for _, st := range mgr.StartAll(ctx) {
		if st.Error != "" {
			t.Fatalf("server %s failed: %s — a same-named tool must not stop either server "+
				"from starting", st.Name, st.Error)
		}
	}

	all, err := mgr.ListAllTools(ctx)
	if err != nil {
		t.Fatal(err)
	}
	byQualified := map[string]ToolDescriptor{}
	for _, td := range all {
		if _, dup := byQualified[td.Qualified]; dup {
			t.Fatalf("two descriptors share the qualified name %q", td.Qualified)
		}
		byQualified[td.Qualified] = td
	}
	if len(byQualified) != 2 {
		t.Fatalf("got %d tools, want 2 — one server's tool overwrote the other's, so calls "+
			"route to whichever started last: %v", len(byQualified), keysOf(byQualified))
	}
	for _, want := range []string{"mcp_alpha_search", "mcp_bravo_search"} {
		td, ok := byQualified[want]
		if !ok {
			t.Errorf("no tool named %q; qualified names are %v", want, keysOf(byQualified))
			continue
		}
		// The descriptor must be the one from ITS OWN server: a namespaced name
		// pointing at the other server's descriptor is the same routing bug
		// wearing a different name.
		wantServer := strings.TrimSuffix(strings.TrimPrefix(want, "mcp_"), "_search")
		if td.ServerName != wantServer {
			t.Errorf("%q carries ServerName %q", want, td.ServerName)
		}
	}
	if byQualified["mcp_alpha_search"].Description == byQualified["mcp_bravo_search"].Description {
		t.Error("both entries carry the same description: they came from one server")
	}
}

func keysOf(m map[string]ToolDescriptor) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
