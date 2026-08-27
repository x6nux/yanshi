package lsp

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestRealGoplsNavigatesTheWorkspace is the navigation counterpart to
// TestRealGoplsReportsTheBrokenLine.
//
// Everything else covering nav.go speaks LSP to a fake this repo wrote against
// the same understanding as the client. That proves the parsers accept the
// shapes we believe servers emit; it cannot prove a real server emits them.
// The specific things only a real server can expose here:
//
//   - which of the three definition shapes gopls actually chooses (it sends
//     LocationLink when the client does not declare linkSupport=false, and this
//     client declares no capabilities at all);
//   - that a positional request against a document opened by ensureOpen — not
//     by an edit — is answered rather than rejected for an unknown document;
//   - that workspace/symbol returns anything at all before the module has been
//     fully indexed, which is the case every first call is in.
//
// The assertions are LINE NUMBERS and NAMES, not "some result arrived": a
// navigation tool that returns the wrong line sends the model to edit the
// wrong code, which is worse than returning nothing.
//
// Skips when gopls is not on PATH; the lsp-e2e CI job installs it.
func TestRealGoplsNavigatesTheWorkspace(t *testing.T) {
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
	write("go.mod", "module navtest\n\ngo 1.21\n")

	// Line numbers are load-bearing below, so the layout is spelled out:
	//   lib.go line 3: func Greet(name string) string
	//   use.go line 4: _ = Greet("a")   (a reference)
	//   use.go line 5: _ = Greet("b")   (another)
	lib := write("lib.go", "package navtest\n\nfunc Greet(name string) string {\n\treturn \"hi \" + name\n}\n")
	use := write("use.go", "package navtest\n\nfunc use() {\n\t_ = Greet(\"a\")\n\t_ = Greet(\"b\")\n}\n")

	m := New(Config{
		WorkRoot:  root,
		Languages: DefaultLanguages(),
		// A cold gopls indexes the module before it answers a symbol query.
		NavTimeout: 60 * time.Second,
	})
	defer m.Close()
	if !m.Enabled() {
		t.Fatal("gopls is on PATH and go.mod is present, but the manager came up disabled")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	// --- definition: from the CALL on use.go:4 to the DECLARATION on lib.go:3
	// Column 6 lands inside "Greet" on "\t_ = Greet(...)": tab(1) _(2) space(3)
	// =(4) space(5) G(6).
	defs, err := m.Definition(ctx, use, 4, 6)
	if err != nil {
		t.Fatalf("Definition: %v", err)
	}
	if len(defs) == 0 {
		t.Fatal("real gopls found no definition for a call to a function declared in the same package")
	}
	if got := filepath.Base(defs[0].Path); got != "lib.go" {
		t.Errorf("definition is in %q, want lib.go (locations: %+v)", got, defs)
	}
	if defs[0].Line != 3 {
		t.Errorf("definition on line %d, want 3 — the declaration line. A wrong line "+
			"sends the model to edit unrelated code. Got %+v", defs[0].Line, defs)
	}

	// --- references: both call sites, excluding the declaration.
	refs, err := m.References(ctx, lib, 3, 6, false)
	if err != nil {
		t.Fatalf("References: %v", err)
	}
	lines := map[string]bool{}
	for _, r := range refs {
		lines[filepath.Base(r.Path)+":"+itoa(r.Line)] = true
	}
	for _, want := range []string{"use.go:4", "use.go:5"} {
		if !lines[want] {
			t.Errorf("references missing %s; got %v\n"+
				"  this is the case fs_search cannot do correctly, so a miss here "+
				"is the whole reason the tool exists", want, keysOf(lines))
		}
	}

	// includeDeclaration must actually change the answer. If the flag were
	// dropped on the way to the server, both calls would return the same set
	// and the parameter would be decoration.
	withDecl, err := m.References(ctx, lib, 3, 6, true)
	if err != nil {
		t.Fatalf("References(includeDeclaration=true): %v", err)
	}
	if len(withDecl) <= len(refs) {
		t.Errorf("includeDeclaration=true returned %d refs, includeDeclaration=false returned %d; "+
			"the flag is not reaching the server", len(withDecl), len(refs))
	}

	// --- hover: the signature, at the declaration.
	h, err := m.Hover(ctx, lib, 3, 6)
	if err != nil {
		t.Fatalf("Hover: %v", err)
	}
	if h == nil || h.Contents == "" {
		t.Fatal("real gopls returned no hover for a function declaration")
	}
	if !strings.Contains(h.Contents, "Greet") {
		t.Errorf("hover does not mention the symbol it was asked about:\n%s", h.Contents)
	}

	// --- documentSymbol: the outline of lib.go.
	syms, err := m.DocumentSymbols(ctx, lib)
	if err != nil {
		t.Fatalf("DocumentSymbols: %v", err)
	}
	var foundOutline bool
	for _, s := range syms {
		if s.Name == "Greet" {
			foundOutline = true
			if s.Location.Line != 3 {
				t.Errorf("outline puts Greet on line %d, want 3", s.Location.Line)
			}
		}
	}
	if !foundOutline {
		t.Errorf("documentSymbol did not list Greet; got %+v", syms)
	}

	// --- workspace/symbol: the name-to-position bridge. Without it the three
	// positional requests above are unreachable from a model, which holds a
	// name and not a coordinate.
	found, err := m.ResolveSymbol(ctx, "Greet")
	if err != nil {
		t.Fatalf("ResolveSymbol: %v", err)
	}
	var resolved bool
	for _, s := range found {
		if s.Name == "Greet" && filepath.Base(s.Location.Path) == "lib.go" && s.Location.Line == 3 {
			resolved = true
		}
	}
	if !resolved {
		t.Errorf("ResolveSymbol(%q) did not return lib.go:3; got %+v\n"+
			"  without this, a model that knows only a NAME cannot reach "+
			"definition/references/hover at all", "Greet", found)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
