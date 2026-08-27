package lsp

import (
	"bufio"
	"context"
	"net"
	"sync"
	"testing"
	"time"
)

// navFakeServer is a fake LSP server that answers navigation requests from a
// canned table and records every method it was sent.
//
// It is a fake, not a mock: it speaks the real framing (WriteMessage /
// ReadMessage) and the real JSON-RPC id correlation, so a client bug in
// either shows up here. What it fakes is only the semantic content of the
// answers.
type navFakeServer struct {
	mu       sync.Mutex
	methods  []string
	openURIs []string
	// results maps a request method to the "result" value to answer with.
	results map[string]any
}

func newNavFakeServer(t *testing.T, results map[string]any) (*Client, *navFakeServer, func()) {
	t.Helper()
	f := &navFakeServer{results: results}
	srv, cli := net.Pipe()
	go func() {
		defer srv.Close()
		br := bufio.NewReader(srv)
		for {
			msg, err := ReadMessage(br)
			if err != nil {
				return
			}
			method, _ := msg["method"].(string)
			f.mu.Lock()
			f.methods = append(f.methods, method)
			if method == "textDocument/didOpen" {
				if p, ok := msg["params"].(map[string]any); ok {
					if td, ok := p["textDocument"].(map[string]any); ok {
						uri, _ := td["uri"].(string)
						f.openURIs = append(f.openURIs, uri)
					}
				}
			}
			res, hasResult := f.results[method]
			f.mu.Unlock()

			if _, isRequest := msg["id"]; !isRequest {
				continue
			}
			if method == "initialize" {
				_ = WriteMessage(srv, map[string]any{"jsonrpc": "2.0", "id": msg["id"],
					"result": map[string]any{"capabilities": map[string]any{}}})
				continue
			}
			if !hasResult {
				res = nil
			}
			_ = WriteMessage(srv, map[string]any{"jsonrpc": "2.0", "id": msg["id"], "result": res})
		}
	}()

	c := newClient(cli, cli, 2*time.Second)
	c.Start()
	if err := c.initialize("/work"); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	return c, f, func() { c.Close(); cli.Close() }
}

func (f *navFakeServer) sawMethod(m string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, got := range f.methods {
		if got == m {
			n++
		}
	}
	return n
}

func (f *navFakeServer) opens() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.openURIs))
	copy(out, f.openURIs)
	return out
}

func TestClientNav_DefinitionReferencesHoverSymbols(t *testing.T) {
	loc := func(uri string, line, char int) map[string]any {
		return map[string]any{"uri": uri, "range": map[string]any{
			"start": map[string]any{"line": line, "character": char},
			"end":   map[string]any{"line": line, "character": char + 4},
		}}
	}
	c, fake, done := newNavFakeServer(t, map[string]any{
		"textDocument/definition": []any{loc("file:///work/a.go", 9, 5)},
		"textDocument/references": []any{
			loc("file:///work/a.go", 9, 5),
			loc("file:///work/b.go", 20, 1),
		},
		"textDocument/hover": map[string]any{
			"contents": map[string]any{"kind": "markdown", "value": "func Close() error"},
		},
		"textDocument/documentSymbol": []any{
			map[string]any{"name": "Close", "kind": float64(12),
				"selectionRange": map[string]any{
					"start": map[string]any{"line": 9, "character": 5},
					"end":   map[string]any{"line": 9, "character": 10}}},
		},
		"workspace/symbol": []any{
			map[string]any{"name": "Close", "kind": float64(12),
				"location": loc("file:///work/a.go", 9, 5)},
		},
	})
	defer done()

	ctx := context.Background()
	uri := "file:///work/a.go"
	if err := c.ensureOpen(uri, "package a\n"); err != nil {
		t.Fatalf("ensureOpen: %v", err)
	}

	defs, err := c.Definition(ctx, uri, 10, 6, time.Second)
	if err != nil {
		t.Fatalf("Definition: %v", err)
	}
	if len(defs) != 1 || defs[0].Line != 10 || defs[0].Column != 6 {
		t.Fatalf("Definition returned %+v", defs)
	}

	refs, err := c.References(ctx, uri, 10, 6, true, time.Second)
	if err != nil {
		t.Fatalf("References: %v", err)
	}
	if len(refs) != 2 {
		t.Fatalf("References returned %d, want 2: %+v", len(refs), refs)
	}

	h, err := c.Hover(ctx, uri, 10, 6, time.Second)
	if err != nil {
		t.Fatalf("Hover: %v", err)
	}
	if h == nil || h.Contents != "func Close() error" {
		t.Fatalf("Hover returned %+v", h)
	}

	syms, err := c.DocumentSymbols(ctx, uri, time.Second)
	if err != nil {
		t.Fatalf("DocumentSymbols: %v", err)
	}
	if len(syms) != 1 || syms[0].Name != "Close" || syms[0].Location.Line != 10 {
		t.Fatalf("DocumentSymbols returned %+v", syms)
	}

	ws, err := c.WorkspaceSymbols(ctx, "Close", time.Second)
	if err != nil {
		t.Fatalf("WorkspaceSymbols: %v", err)
	}
	if len(ws) != 1 || ws[0].Name != "Close" {
		t.Fatalf("WorkspaceSymbols returned %+v", ws)
	}

	for _, m := range []string{
		"textDocument/definition", "textDocument/references", "textDocument/hover",
		"textDocument/documentSymbol", "workspace/symbol",
	} {
		if fake.sawMethod(m) != 1 {
			t.Errorf("server saw %s %d times, want 1", m, fake.sawMethod(m))
		}
	}
}

// TestClientNav_ReferencesSendsIncludeDeclaration pins the one parameter the
// caller controls. Servers differ on the default, so omitting it makes the
// result depend on which server answered — the reference list is then either
// missing the declaration or not, with no way for the caller to tell which.
func TestClientNav_ReferencesSendsIncludeDeclaration(t *testing.T) {
	for _, want := range []bool{true, false} {
		got := make(chan bool, 1)
		srv, cli := net.Pipe()
		go func() {
			defer srv.Close()
			br := bufio.NewReader(srv)
			for {
				msg, err := ReadMessage(br)
				if err != nil {
					return
				}
				method, _ := msg["method"].(string)
				if method == "initialize" {
					_ = WriteMessage(srv, map[string]any{"jsonrpc": "2.0", "id": msg["id"],
						"result": map[string]any{}})
					continue
				}
				if method == "textDocument/references" {
					p, _ := msg["params"].(map[string]any)
					rc, _ := p["context"].(map[string]any)
					inc, present := rc["includeDeclaration"].(bool)
					if !present {
						got <- false
					} else {
						got <- inc
					}
					_ = WriteMessage(srv, map[string]any{"jsonrpc": "2.0", "id": msg["id"], "result": []any{}})
				}
			}
		}()
		c := newClient(cli, cli, 2*time.Second)
		c.Start()
		if err := c.initialize("/work"); err != nil {
			t.Fatalf("initialize: %v", err)
		}
		if _, err := c.References(context.Background(), "file:///w/a.go", 1, 1, want, time.Second); err != nil {
			t.Fatalf("References: %v", err)
		}
		select {
		case sent := <-got:
			if sent != want {
				t.Errorf("includeDeclaration sent as %v, want %v", sent, want)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("server never saw a references request carrying a context object")
		}
		c.Close()
		cli.Close()
	}
}

// TestClientNav_PositionsAreZeroBasedOnTheWire pins the one conversion every
// navigation request shares.
//
// Off-by-one here is the classic LSP bug and it is silent: the server answers
// about the neighbouring line and the model is sent to read working code. The
// existing diagnostics test pins the same conversion in the inbound direction;
// this pins the outbound one, which has no other coverage.
func TestClientNav_PositionsAreZeroBasedOnTheWire(t *testing.T) {
	type pos struct{ line, char float64 }
	got := make(chan pos, 1)
	srv, cli := net.Pipe()
	go func() {
		defer srv.Close()
		br := bufio.NewReader(srv)
		for {
			msg, err := ReadMessage(br)
			if err != nil {
				return
			}
			method, _ := msg["method"].(string)
			if method == "initialize" {
				_ = WriteMessage(srv, map[string]any{"jsonrpc": "2.0", "id": msg["id"], "result": map[string]any{}})
				continue
			}
			if method == "textDocument/definition" {
				p, _ := msg["params"].(map[string]any)
				pp, _ := p["position"].(map[string]any)
				l, _ := pp["line"].(float64)
				ch, _ := pp["character"].(float64)
				got <- pos{l, ch}
				_ = WriteMessage(srv, map[string]any{"jsonrpc": "2.0", "id": msg["id"], "result": nil})
			}
		}
	}()
	c := newClient(cli, cli, 2*time.Second)
	c.Start()
	defer func() { c.Close(); cli.Close() }()
	if err := c.initialize("/work"); err != nil {
		t.Fatalf("initialize: %v", err)
	}

	// 1-based (12, 7) in -> 0-based (11, 6) on the wire.
	if _, err := c.Definition(context.Background(), "file:///w/a.go", 12, 7, time.Second); err != nil {
		t.Fatalf("Definition: %v", err)
	}
	select {
	case p := <-got:
		if p.line != 11 || p.char != 6 {
			t.Errorf("wire position = (%v,%v), want (11,6): the caller passed 1-based (12,7)", p.line, p.char)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no definition request reached the server")
	}
}

// TestEnsureOpen_SendsDidOpenExactlyOncePerURI is the regression pin for the
// protocol violation the navigation work could have introduced.
//
// A server must be told didOpen once per document; a second one is a protocol
// violation and gopls responds by DROPPING the document, after which that file
// never produces another diagnostic and nothing reports an error. Navigation
// opens files it only reads, so the open set had to become separate state — if
// it were derived from the edit counter, the sequence "navigate then edit"
// would send two.
func TestEnsureOpen_SendsDidOpenExactlyOncePerURI(t *testing.T) {
	c, fake, done := newNavFakeServer(t, nil)
	defer done()

	uri := "file:///work/a.go"
	for i := 0; i < 3; i++ {
		if err := c.ensureOpen(uri, "package a\n"); err != nil {
			t.Fatalf("ensureOpen #%d: %v", i, err)
		}
	}
	// The edit path must see the file as already open and send didChange.
	if err := c.notifyChange(uri, "package a // edited\n"); err != nil {
		t.Fatalf("notifyChange: %v", err)
	}

	// Wait until the notification that followed the opens has been processed,
	// whichever one it turned out to be: didChange is correct, a second
	// didOpen is the bug. Waiting only for didChange would report the bug as a
	// bare timeout instead of naming it.
	waitFor(t, func() bool {
		return fake.sawMethod("textDocument/didChange") == 1 || fake.sawMethod("textDocument/didOpen") > 1
	})

	if n := fake.sawMethod("textDocument/didOpen"); n != 1 {
		t.Errorf("server saw %d didOpen for one uri, want 1; opens=%v\n"+
			"  a second didOpen makes gopls drop the document, and that file "+
			"silently stops producing diagnostics", n, fake.opens())
	}
	if n := fake.sawMethod("textDocument/didChange"); n != 1 {
		t.Errorf("server saw %d didChange, want 1 (the edit after the navigation opens)", n)
	}
}

// TestEnsureOpen_DoesNotMakeDiagnosticsBlock pins the other half of the same
// separation. Diagnostics waits for a publication covering the current edit
// generation; if a read-only open bumped that counter, the next Diagnostics
// call on a file nobody edited would block for its whole timeout and then
// return stale data.
func TestEnsureOpen_DoesNotMakeDiagnosticsBlock(t *testing.T) {
	c, _, done := newNavFakeServer(t, nil)
	defer done()

	uri := "file:///work/never-edited.go"
	if err := c.ensureOpen(uri, "package a\n"); err != nil {
		t.Fatalf("ensureOpen: %v", err)
	}
	start := time.Now()
	diags := c.Diagnostics(uri, 500*time.Millisecond)
	elapsed := time.Since(start)

	if diags != nil {
		t.Errorf("a file that was only READ should have no diagnostics, got %+v", diags)
	}
	if elapsed > 200*time.Millisecond {
		t.Errorf("Diagnostics blocked %v after a read-only open; opening a file for "+
			"navigation must not register as an edit awaiting a publication", elapsed)
	}
}

// TestClientNav_ServerErrorSurfaces confirms a JSON-RPC error response becomes
// a Go error rather than an empty result. "Server refused" and "symbol has no
// definition" must not look the same to the caller.
func TestClientNav_ServerErrorSurfaces(t *testing.T) {
	srv, cli := net.Pipe()
	go func() {
		defer srv.Close()
		br := bufio.NewReader(srv)
		for {
			msg, err := ReadMessage(br)
			if err != nil {
				return
			}
			if _, isReq := msg["id"]; !isReq {
				continue
			}
			if m, _ := msg["method"].(string); m == "initialize" {
				_ = WriteMessage(srv, map[string]any{"jsonrpc": "2.0", "id": msg["id"], "result": map[string]any{}})
				continue
			}
			_ = WriteMessage(srv, map[string]any{"jsonrpc": "2.0", "id": msg["id"],
				"error": map[string]any{"code": float64(-32603), "message": "no views"}})
		}
	}()
	c := newClient(cli, cli, 2*time.Second)
	c.Start()
	defer func() { c.Close(); cli.Close() }()
	if err := c.initialize("/work"); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	if _, err := c.Definition(context.Background(), "file:///w/a.go", 1, 1, time.Second); err == nil {
		t.Fatal("a JSON-RPC error response must surface as an error, not as 'no definition found'")
	}
}

// TestRequestUnbounded_IsNotCappedByClientTimeout pins the reason
// requestUnbounded exists.
//
// request() takes the MINIMUM of c.timeout and the context deadline, so a
// caller cannot widen the ceiling by passing a longer deadline. Navigation
// needs a wider one than the handshake budget: a cold workspace/symbol over a
// large module outlasts it, and the timeout is indistinguishable from "no such
// symbol".
func TestRequestUnbounded_IsNotCappedByClientTimeout(t *testing.T) {
	srv, cli := net.Pipe()
	go func() {
		defer srv.Close()
		br := bufio.NewReader(srv)
		for {
			msg, err := ReadMessage(br)
			if err != nil {
				return
			}
			if _, isReq := msg["id"]; !isReq {
				continue
			}
			if m, _ := msg["method"].(string); m == "initialize" {
				_ = WriteMessage(srv, map[string]any{"jsonrpc": "2.0", "id": msg["id"], "result": map[string]any{}})
				continue
			}
			// Slower than the client timeout below, faster than the nav bound.
			time.Sleep(180 * time.Millisecond)
			_ = WriteMessage(srv, map[string]any{"jsonrpc": "2.0", "id": msg["id"],
				"result": []any{map[string]any{"uri": "file:///w/a.go",
					"range": map[string]any{
						"start": map[string]any{"line": float64(0), "character": float64(0)},
						"end":   map[string]any{"line": float64(0), "character": float64(1)}}}}})
		}
	}()
	// 50ms client timeout: the handshake budget, far below what the server takes.
	c := newClient(cli, cli, 50*time.Millisecond)
	c.Start()
	defer func() { c.Close(); cli.Close() }()

	// initialize would time out at 50ms against a server this slow, so drive
	// the handshake through the same slow path deliberately skipped here: the
	// point under test is the request ceiling, not the handshake.
	if _, err := c.requestUnbounded(mustDeadline(t, 3*time.Second), "initialize", map[string]any{}); err != nil {
		t.Fatalf("initialize: %v", err)
	}

	locs, err := c.Definition(context.Background(), "file:///w/a.go", 1, 1, 3*time.Second)
	if err != nil {
		t.Fatalf("navigation must not inherit the %v handshake ceiling: %v", 50*time.Millisecond, err)
	}
	if len(locs) != 1 {
		t.Fatalf("got %d locations, want 1", len(locs))
	}

	// The narrow path is still narrow: request() (used by shutdown) keeps the
	// 50ms ceiling, so the two really are different bounds and not one bound
	// that got widened for everyone.
	if _, err := c.request(context.Background(), "shutdown", nil); err == nil {
		t.Error("request() should still be capped by the client timeout; " +
			"if it is not, requestUnbounded widened the ceiling for every caller")
	}
}

func mustDeadline(t *testing.T, d time.Duration) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	t.Cleanup(cancel)
	return ctx
}

// waitFor polls cond until it holds or the test budget runs out.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition never became true within 2s")
}
