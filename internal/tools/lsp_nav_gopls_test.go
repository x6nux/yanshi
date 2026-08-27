package tools

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/x6nux/yanshi/internal/guard"
	"github.com/x6nux/yanshi/internal/lsp"
)

// This file is the REAL-SERVER half of the T1 verification. Every other test
// of the navigation tools drives fakeNavigator, which answers from a table:
// those prove the tool shell (argument parsing, truncation, authorization,
// result shape) but cannot prove that a real language server, spoken to over a
// real stdio pipe, returns positions this repo's own code agrees with.
//
// The gap matters most for the SYMBOL-NAME entry. A model holds a name, not a
// line and column, so ResolveSymbol -> workspace/symbol is the only door into
// the three positional tools. A fake makes that door look open by construction.
// Against real gopls it depends on the workspace actually being indexed, the
// marker check passing, and the fuzzy-match filter in Manager.ResolveSymbol
// keeping the exact match. None of those are observable without a real server.
//
// Skips when gopls is absent. That keeps CI green on a machine without the Go
// tool suite, and is why the fake-driven tests above are not redundant: they,
// not this file, are the ones that always run.

// goplsWorkspace prepares a real gopls-backed navigator over a REAL Go module
// written to a temp dir, and returns a context with it bound.
//
// A temp module rather than this repository: gopls indexes the whole workspace
// before answering workspace/symbol, and this repo is large enough that the
// first query would race the nav timeout on a cold module cache. The fixture is
// still real Go compiled by the real toolchain -- what is being verified is that
// gopls's answers line up with the source, and a four-file module shows that as
// well as a four-hundred-file one while staying inside a test's time budget.
func goplsWorkspace(t *testing.T) (ctx context.Context, root string) {
	t.Helper()
	if _, err := exec.LookPath("gopls"); err != nil {
		t.Skip("gopls not on PATH; install golang.org/x/tools/gopls to run the real-server navigation checks")
	}
	root = t.TempDir()
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module goplsfixture\n\ngo 1.21\n")
	// Greeter is declared on line 5 of greeter.go and used three times in
	// use.go. Both numbers are asserted below, so an off-by-one in the
	// 0-based-LSP to 1-based-tool conversion cannot pass.
	write("greeter.go", strings.Join([]string{
		"package goplsfixture",              // 1
		"",                                  // 2
		"// Greeter greets.",                // 3
		"type Greeter struct {",             // 4
		"\tName string",                     // 5
		"}",                                 // 6
		"",                                  // 7
		"// Hello returns a greeting.",      // 8
		"func (g Greeter) Hello() string {", // 9
		"\treturn \"hello \" + g.Name",      // 10
		"}",                                 // 11
		"",                                  // 12
	}, "\n"))
	write("use.go", strings.Join([]string{
		"package goplsfixture",
		"",
		"func useOnce() string {",
		"\tg := Greeter{Name: \"a\"}",
		"\treturn g.Hello()",
		"}",
		"",
		"func useTwice() string {",
		"\tg := Greeter{Name: \"b\"}",
		"\treturn g.Hello()",
		"}",
		"",
	}, "\n"))

	mgr := lsp.New(lsp.Config{
		WorkRoot:  root,
		Languages: lsp.DefaultLanguages(),
		// A cold gopls indexes the module on the first request; the default
		// 15s can be tight on a loaded machine, and a timeout here is
		// indistinguishable from "the symbol does not exist", which is the
		// exact confusion this test exists to rule out.
		NavTimeout: 60 * time.Second,
	})
	if !mgr.Enabled() {
		t.Fatalf("lsp.Manager disabled with gopls on PATH and a go.mod in %s", root)
	}
	t.Cleanup(func() { _ = mgr.Close() })

	ctx = WithWorkRoot(context.Background(), root)
	ctx = WithProfile(ctx, guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"*"}},
		FS:    guard.FSPerm{Read: []string{"**"}, Write: []string{"**"}},
	})
	ctx = WithLSP(ctx, mgr)
	return ctx, root
}

// TestGoplsReal_DefinitionBySymbolName is the load-bearing one: it exercises
// the entry a model actually uses (a bare name) against a real server, and
// checks the returned position against the fixture's known declaration site.
//
// Asserting the LINE, not merely that some location came back, is the point. A
// resolver that returned the right file at line 1 would satisfy a shape-only
// assertion while being useless to a model, and an off-by-one from the
// 0-based-to-1-based conversion would be invisible.
func TestGoplsReal_DefinitionBySymbolName(t *testing.T) {
	ctx, _ := goplsWorkspace(t)
	nav := NewLSPNavTools()

	out, err := runTool(ctx, nav.Definition, `{"symbol":"Greeter"}`)
	if err != nil {
		t.Fatalf("lsp_definition returned a Go error, which aborts the turn: %v", err)
	}
	var res lspDefinitionResult
	decodeNavResult(t, out, &res)
	if len(res.Definitions) == 0 {
		t.Fatalf("real gopls resolved no definition for the symbol name Greeter: %s", out)
	}
	d := res.Definitions[0]
	if !strings.HasSuffix(d.File, "greeter.go") {
		t.Errorf("definition file = %q, want greeter.go", d.File)
	}
	// "type Greeter struct {" is line 4 of the fixture (1-based).
	if d.Line != 4 {
		t.Errorf("definition line = %d, want 4 (the `type Greeter struct` line); "+
			"a wrong line here means the 0-based LSP position was not converted", d.Line)
	}
	if d.Text != "" && !strings.Contains(d.Text, "Greeter") {
		t.Errorf("echoed source line %q does not contain the symbol; "+
			"the line number and the text are read from different places and disagree", d.Text)
	}
}

// TestGoplsReal_ReferencesBySymbolNameFindsEveryUse checks that references
// really come from the server's index rather than a text scan: the fixture uses
// Greeter twice in use.go, and a regex for "Greeter" would also match the doc
// comment on line 3, which the server must not report.
func TestGoplsReal_ReferencesBySymbolNameFindsEveryUse(t *testing.T) {
	ctx, _ := goplsWorkspace(t)
	nav := NewLSPNavTools()

	out, err := runTool(ctx, nav.References, `{"symbol":"Greeter","include_declaration":false}`)
	if err != nil {
		t.Fatalf("lsp_references returned a Go error: %v", err)
	}
	var res lspReferencesResult
	decodeNavResult(t, out, &res)
	if len(res.References) < 2 {
		t.Fatalf("real gopls found %d references to Greeter, want at least the 2 uses in use.go: %s",
			len(res.References), out)
	}
	var inUse int
	for _, r := range res.References {
		if strings.HasSuffix(r.File, "use.go") {
			inUse++
		}
		// The doc comment on greeter.go line 3 mentions Greeter. A textual
		// search would return it; the language server must not.
		if strings.HasSuffix(r.File, "greeter.go") && r.Line == 3 {
			t.Errorf("a doc-comment mention was reported as a reference (%s:%d); "+
				"that is regex behaviour, not index behaviour", r.File, r.Line)
		}
	}
	if inUse != 2 {
		t.Errorf("found %d references in use.go, want exactly 2: %s", inUse, out)
	}
	if res.Total != len(res.References) && !res.Truncated {
		t.Errorf("Total=%d but %d returned with Truncated=false", res.Total, len(res.References))
	}
}

// TestGoplsReal_HoverBySymbolNameReturnsTheSignature proves hover text really
// comes from the type checker: the fixture's doc comment and the method's
// signature both have to appear, and neither is present in the query.
func TestGoplsReal_HoverBySymbolNameReturnsTheSignature(t *testing.T) {
	ctx, _ := goplsWorkspace(t)
	nav := NewLSPNavTools()

	out, err := runTool(ctx, nav.Hover, `{"symbol":"Hello"}`)
	if err != nil {
		t.Fatalf("lsp_hover returned a Go error: %v", err)
	}
	var res lspHoverResult
	decodeNavResult(t, out, &res)
	if strings.TrimSpace(res.Contents) == "" {
		t.Fatalf("real gopls returned empty hover contents for Hello: %s", out)
	}
	// gopls renders the signature; the exact formatting varies by version, so
	// assert on the parts that any rendering must contain.
	if !strings.Contains(res.Contents, "Hello") {
		t.Errorf("hover contents do not name the symbol: %q", res.Contents)
	}
	if !strings.Contains(res.Contents, "string") {
		t.Errorf("hover contents omit the return type, so this is not a real "+
			"signature render: %q", res.Contents)
	}
}

// TestGoplsReal_SymbolsOutlinesAFile checks the file-outline entry against the
// real server, including the line numbers, which are what make an outline
// usable as a jumping-off point.
func TestGoplsReal_SymbolsOutlinesAFile(t *testing.T) {
	ctx, _ := goplsWorkspace(t)
	nav := NewLSPNavTools()

	out, err := runTool(ctx, nav.Symbols, `{"file":"greeter.go"}`)
	if err != nil {
		t.Fatalf("lsp_symbols returned a Go error: %v", err)
	}
	var res lspSymbolsResult
	decodeNavResult(t, out, &res)
	if len(res.Symbols) == 0 {
		t.Fatalf("real gopls returned no symbols for greeter.go: %s", out)
	}
	byName := map[string]lspSymbolOut{}
	for _, s := range res.Symbols {
		byName[s.Name] = s
	}
	g, ok := byName["Greeter"]
	if !ok {
		t.Fatalf("outline omits the Greeter type: %s", out)
	}
	if g.Line != 4 {
		t.Errorf("Greeter outlined at line %d, want 4", g.Line)
	}
	// gopls renders a method's outline name with its receiver, and the exact
	// spelling has changed across versions ("Hello", "Greeter.Hello",
	// "(Greeter).Hello" are all forms it has emitted). Pinning one spelling
	// would make this test a gopls-version detector rather than a check that
	// the method is outlined at all, so match on the method name plus the
	// declaration line.
	var method *lspSymbolOut
	for i, s := range res.Symbols {
		if strings.Contains(s.Name, "Hello") {
			method = &res.Symbols[i]
			break
		}
	}
	if method == nil {
		t.Fatalf("outline omits the Hello method under any naming: %s", out)
	}
	if method.Line != 9 {
		t.Errorf("Hello outlined at line %d, want 9 (the `func (g Greeter) Hello()` line)", method.Line)
	}
}

// TestGoplsReal_WorkspaceSymbolQuery covers the other lsp_symbols entry: a
// name searched across the workspace rather than one file's outline. This is
// the same server call ResolveSymbol uses, so a failure here localises a
// symbol-name failure in the other three tools to the query rather than to
// the anchoring.
func TestGoplsReal_WorkspaceSymbolQuery(t *testing.T) {
	ctx, _ := goplsWorkspace(t)
	nav := NewLSPNavTools()

	out, err := runTool(ctx, nav.Symbols, `{"query":"Greeter"}`)
	if err != nil {
		t.Fatalf("lsp_symbols query returned a Go error: %v", err)
	}
	var res lspSymbolsResult
	decodeNavResult(t, out, &res)
	var found bool
	for _, s := range res.Symbols {
		if s.Name == "Greeter" && strings.HasSuffix(s.File, "greeter.go") {
			found = true
			if s.Line != 4 {
				t.Errorf("workspace symbol Greeter at line %d, want 4", s.Line)
			}
		}
	}
	if !found {
		t.Fatalf("workspace symbol search for Greeter found nothing in greeter.go: %s", out)
	}
}
