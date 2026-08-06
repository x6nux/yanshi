package lsp

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// TestRealGoplsReportsTheBrokenLine is the only test in the tree that runs a
// real language server.
//
// Everything else — including fs_lsp_e2e_test.go's net.Pipe fixture — speaks
// LSP to something this repo wrote. That proves the client's framing and
// dispatch, and proves nothing about whether the subprocess path works: the
// exec.Command spawn, the marker gate, the initialize handshake, the
// publishDiagnostics push a real server sends unprompted rather than in reply,
// and the wait that has to be long enough for a cold gopls to index a module.
// Each of those has a way to be wrong that a fake, written against the same
// understanding as the client, cannot expose.
//
// The assertion is the LINE NUMBER, not merely "some diagnostic arrived". A
// client that reported a diagnostic against the wrong line — off-by-one between
// LSP's 0-based lines and the editor's 1-based ones is the classic form — would
// send the model to look at working code.
//
// Skips when gopls is not on PATH. CI installs it (see the lsp-e2e job in
// ci.yml); without that step this test would be a permanent skip, which reads
// as a pass and is the failure mode the acceptance breakdown warned about for
// this clause.
//
// ledger: B2/LSP1#4 Go/Python/TS 至少一种端到端可用
func TestRealGoplsReportsTheBrokenLine(t *testing.T) {
	if _, err := exec.LookPath("gopls"); err != nil {
		t.Skip("gopls not on PATH; the lsp-e2e CI job installs it")
	}

	root := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(root, name)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	// go.mod is not decoration: it is the marker DefaultLanguages requires, and
	// gopls without one answers every request with an error.
	write("go.mod", "module lsptest\n\ngo 1.21\n")
	// Line 4 (1-based) is the broken one.
	main := write("main.go", "package main\n\nfunc main() {\n\tundefinedHelper()\n}\n")

	m := New(Config{
		WorkRoot:  root,
		Languages: DefaultLanguages(),
		// A cold gopls indexes the module before it answers. This is the wait
		// for a real subprocess, not the production default.
		Timeout: 30 * time.Second,
	})
	defer m.Close()

	if !m.Enabled() {
		t.Fatal("gopls is on PATH and go.mod is present, but the manager came up disabled")
	}

	body, err := os.ReadFile(main)
	if err != nil {
		t.Fatal(err)
	}
	m.DidChange(main, string(body))

	start := time.Now()
	diags := m.Diagnostics(main, 30*time.Second)
	elapsed := time.Since(start)
	if len(diags) == 0 {
		t.Fatal("a real gopls reported no diagnostics for a file that does not compile")
	}

	// The generous 30s above is a CI ceiling, not the production budget —
	// bootstrap's default is 800ms (config lsp.timeout), and the edit path caps
	// at tools.LSPDiagBudget. A test that only ever runs with 30s cannot notice
	// gopls getting slower, and what that looks like in production is not an
	// error: it is an empty diagnostics section, which reads as "this file is
	// fine". Measured on darwin/arm64: ~120ms cold, including the module index.
	//
	// The bound here is 4x the production default rather than the default
	// itself, so a slower CI box does not make this flaky while a real
	// regression — anything approaching an order of magnitude — still lands.
	const productionDefault = 800 * time.Millisecond
	if elapsed > 4*productionDefault {
		t.Errorf("a cold gopls took %v for one file; production allows %v, after which the "+
			"model gets an empty diagnostics section that reads as 'no problems'",
			elapsed, productionDefault)
	}

	var found bool
	for _, d := range diags {
		if d.Line == 4 {
			found = true
		}
	}
	if !found {
		t.Errorf("no diagnostic on line 4, where the undefined call is: %+v\n"+
			"  a wrong line number sends the model to read working code", diags)
	}
}
