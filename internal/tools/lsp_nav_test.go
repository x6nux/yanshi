package tools

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/x6nux/yanshi/internal/guard"
	"github.com/x6nux/yanshi/internal/lsp"
)

// fakeNavigator is a deterministic LSPNavigator. It is a fake, not a mock: it
// answers from a table and records what it was asked, with no expectations to
// set up and no ordering to satisfy.
//
// It also satisfies LSPManager, because that is the contract WithLSP binds and
// the shape production really has: *lsp.Manager implements both. A fake that
// implemented only the navigator half could not be bound at all, and pretending
// otherwise would test a wiring that does not exist.
type fakeNavigator struct {
	enabled bool

	defs     []lsp.Location
	refs     []lsp.Location
	hover    *lsp.Hover
	docSyms  []lsp.SymbolInfo
	resolved []lsp.SymbolInfo

	defErr     error
	refErr     error
	hoverErr   error
	docSymErr  error
	resolveErr error

	// The last* fields record the (path, line, col) each positional method was
	// asked about, so a test can assert that name resolution really produced
	// the anchor rather than the tool defaulting to 1:1.
	lastPath           string
	lastLine           int
	lastCol            int
	lastIncludeDecl    bool
	resolveQueries     []string
	docSymbolPathAsked string
}

func (f *fakeNavigator) Enabled() bool { return f.enabled }

// --- LSPManager half (diagnostics), unused by the navigation tools ---

func (f *fakeNavigator) DidChange(_, _ string)   {}
func (f *fakeNavigator) OpenDocuments() []string { return nil }
func (f *fakeNavigator) Diagnostics(string, time.Duration) []lsp.Diagnostic {
	return nil
}

// --- LSPNavigator half ---

func (f *fakeNavigator) Definition(_ context.Context, path string, line, col int) ([]lsp.Location, error) {
	f.lastPath, f.lastLine, f.lastCol = path, line, col
	return f.defs, f.defErr
}

func (f *fakeNavigator) References(_ context.Context, path string, line, col int, includeDecl bool) ([]lsp.Location, error) {
	f.lastPath, f.lastLine, f.lastCol, f.lastIncludeDecl = path, line, col, includeDecl
	return f.refs, f.refErr
}

func (f *fakeNavigator) Hover(_ context.Context, path string, line, col int) (*lsp.Hover, error) {
	f.lastPath, f.lastLine, f.lastCol = path, line, col
	return f.hover, f.hoverErr
}

func (f *fakeNavigator) DocumentSymbols(_ context.Context, path string) ([]lsp.SymbolInfo, error) {
	f.docSymbolPathAsked = path
	return f.docSyms, f.docSymErr
}

func (f *fakeNavigator) ResolveSymbol(_ context.Context, name string) ([]lsp.SymbolInfo, error) {
	f.resolveQueries = append(f.resolveQueries, name)
	return f.resolved, f.resolveErr
}

// navTestEnv builds a work root with real files on disk plus a context wired
// the way the orchestrator wires one.
type navTestEnv struct {
	root string
	ctx  context.Context
	nav  *fakeNavigator
}

func newNavTestEnv(t *testing.T, nav *fakeNavigator) *navTestEnv {
	t.Helper()
	root := t.TempDir()
	// Real files: the source-line snippets the tools attach are read from
	// disk, so a fixture of fake paths would leave that whole code path
	// exercised-but-unobserved.
	mustWrite(t, filepath.Join(root, "lib.go"),
		"package p\n\nfunc Greet(name string) string {\n\treturn name\n}\n")
	mustWrite(t, filepath.Join(root, "use.go"),
		"package p\n\nfunc use() {\n\t_ = Greet(\"a\")\n\t_ = Greet(\"b\")\n}\n")

	ctx := WithWorkRoot(context.Background(), root)
	ctx = WithProfile(ctx, guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"*"}},
		FS:    guard.FSPerm{Read: []string{"**"}, Write: []string{"**"}},
	})
	if nav != nil {
		ctx = WithLSP(ctx, nav)
	}
	return &navTestEnv{root: root, ctx: ctx, nav: nav}
}

func mustWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (e *navTestEnv) abs(name string) string { return filepath.Join(e.root, name) }

func decodeNavResult(t *testing.T, raw string, out any) {
	t.Helper()
	if err := json.Unmarshal([]byte(raw), out); err != nil {
		t.Fatalf("tool result is not JSON (%v): %s", err, raw)
	}
}

// --- unavailability: must be a RESULT, never a Go error ---

// TestLSPNav_UnavailableIsAResultNotAnError pins the single most consequential
// error-handling choice in this file.
//
// A Go error returned from a tool aborts the whole ReAct turn (ADR-0001). "No
// language server installed" is not a failure of the turn — it is an answer,
// and the model should fall back to fs_search and keep going. Returning an
// error here would make an ordinary machine without gopls unable to complete
// any turn in which the model tried navigation once.
func TestLSPNav_UnavailableIsAResultNotAnError(t *testing.T) {
	cases := []struct {
		name string
		bind func(ctx context.Context) context.Context
	}{
		{"no manager bound at all", func(ctx context.Context) context.Context { return ctx }},
		{"manager bound but disabled", func(ctx context.Context) context.Context {
			return WithLSP(ctx, &fakeNavigator{enabled: false})
		}},
	}
	tools := NewLSPNavTools()
	for _, tc := range cases {
		for _, tool := range tools.Tools() {
			t.Run(tc.name+"/"+tool.name, func(t *testing.T) {
				env := newNavTestEnv(t, nil)
				ctx := tc.bind(env.ctx)
				got, err := runTool(ctx, tool, `{"symbol":"Greet","file":"lib.go","query":"Greet"}`)
				if err != nil {
					t.Fatalf("returned a Go error, which aborts the turn: %v", err)
				}
				var res lspUnavailable
				decodeNavResult(t, got, &res)
				if res.Available {
					t.Errorf("reported lsp_available=true with no usable server: %s", got)
				}
				if res.Reason == "" || res.Advice == "" {
					t.Errorf("unavailability result must say why and what to do instead; got %s", got)
				}
				if !strings.Contains(res.Advice, "fs_search") {
					t.Errorf("advice must name the fallback tool, else the model retries "+
						"the same call: %q", res.Advice)
				}
			})
		}
	}
}

// TestLSPNav_DiagnosticsOnlyManagerDegrades pins the seam between LSPManager
// and LSPNavigator. A manager that implements only the diagnostics contract is
// legal (several tests bind one); the nav tools must notice via the type
// assertion rather than assume.
func TestLSPNav_DiagnosticsOnlyManagerDegrades(t *testing.T) {
	env := newNavTestEnv(t, nil)
	nav, unavailable := lspNavFromContext(WithLSP(env.ctx, stubDiagManager{}))
	if nav != nil {
		t.Fatal("a diagnostics-only manager must not be accepted as a navigator")
	}
	var res lspUnavailable
	decodeNavResult(t, unavailable, &res)
	if !strings.Contains(res.Reason, "diagnostics only") {
		t.Errorf("reason should say the manager lacks navigation: %q", res.Reason)
	}
}

// stubDiagManager satisfies LSPManager exactly and nothing more.
type stubDiagManager struct{}

func (stubDiagManager) Enabled() bool           { return true }
func (stubDiagManager) DidChange(_, _ string)   {}
func (stubDiagManager) OpenDocuments() []string { return nil }
func (stubDiagManager) Diagnostics(string, time.Duration) []lsp.Diagnostic {
	return nil
}

// --- definition ---

func TestLSPDefinition_ByExplicitPosition(t *testing.T) {
	nav := &fakeNavigator{enabled: true}
	env := newNavTestEnv(t, nav)
	nav.defs = []lsp.Location{{Path: env.abs("lib.go"), URI: "file:///lib.go", Line: 3, Column: 6}}

	tl := NewLSPNavTools()
	got, err := runTool(env.ctx, tl.Definition, `{"file":"use.go","line":4,"character":6}`)
	if err != nil {
		t.Fatal(err)
	}
	var res lspDefinitionResult
	decodeNavResult(t, got, &res)

	if len(res.Definitions) != 1 {
		t.Fatalf("got %d definitions: %s", len(res.Definitions), got)
	}
	d := res.Definitions[0]
	if d.File != "lib.go" {
		t.Errorf("file = %q, want the root-relative %q", d.File, "lib.go")
	}
	if d.Line != 3 {
		t.Errorf("line = %d, want 3", d.Line)
	}
	// The source line is the difference between a coordinate and an answer.
	if !strings.Contains(d.Text, "func Greet") {
		t.Errorf("definition carries no source line; got %q", d.Text)
	}
	// Explicit coordinates must be sent through unchanged, and must NOT go
	// through symbol resolution.
	if nav.lastLine != 4 || nav.lastCol != 6 {
		t.Errorf("asked the server about %d:%d, want 4:6", nav.lastLine, nav.lastCol)
	}
	if len(nav.resolveQueries) != 0 {
		t.Errorf("explicit coordinates must not trigger a symbol search; got %v", nav.resolveQueries)
	}
}

// TestLSPDefinition_BySymbolNameResolvesFirst is the usability pin.
//
// definition/references/hover are POSITIONAL and a model holds a NAME. Without
// the name entry the three are reachable only by guessing coordinates, which is
// the regex hunt they exist to replace — so a regression that quietly dropped
// name support would leave four registered tools that a model cannot invoke.
func TestLSPDefinition_BySymbolNameResolvesFirst(t *testing.T) {
	nav := &fakeNavigator{enabled: true}
	env := newNavTestEnv(t, nav)
	nav.resolved = []lsp.SymbolInfo{{
		Name: "Greet", Kind: lsp.SymbolFunction,
		Location: lsp.Location{Path: env.abs("lib.go"), URI: "file:///lib.go", Line: 3, Column: 6},
	}}
	nav.defs = []lsp.Location{{Path: env.abs("lib.go"), Line: 3, Column: 6}}

	tl := NewLSPNavTools()
	got, err := runTool(env.ctx, tl.Definition, `{"symbol":"Greet"}`)
	if err != nil {
		t.Fatal(err)
	}
	var res lspDefinitionResult
	decodeNavResult(t, got, &res)
	if len(res.Definitions) != 1 || res.Definitions[0].Line != 3 {
		t.Fatalf("wrong definitions: %s", got)
	}
	if len(nav.resolveQueries) != 1 || nav.resolveQueries[0] != "Greet" {
		t.Fatalf("name was not resolved through workspace/symbol: %v", nav.resolveQueries)
	}
	// The anchor must come from the resolution, not from a 1:1 default.
	if nav.lastLine != 3 || nav.lastCol != 6 {
		t.Errorf("positional request used %d:%d; the resolved symbol was at 3:6", nav.lastLine, nav.lastCol)
	}
}

// TestLSPDefinition_AmbiguousNameListsCandidates pins the disambiguation
// affordance. Answering for the first match without saying there were others
// makes an ambiguous query indistinguishable from an unambiguous one.
func TestLSPDefinition_AmbiguousNameListsCandidates(t *testing.T) {
	nav := &fakeNavigator{enabled: true}
	env := newNavTestEnv(t, nav)
	nav.resolved = []lsp.SymbolInfo{
		{Name: "Close", Kind: lsp.SymbolMethod, Container: "Manager",
			Location: lsp.Location{Path: env.abs("lib.go"), Line: 3, Column: 6}},
		{Name: "Close", Kind: lsp.SymbolMethod, Container: "Client",
			Location: lsp.Location{Path: env.abs("use.go"), Line: 4, Column: 6}},
	}
	nav.defs = []lsp.Location{{Path: env.abs("lib.go"), Line: 3, Column: 6}}

	tl := NewLSPNavTools()
	got, err := runTool(env.ctx, tl.Definition, `{"symbol":"Close"}`)
	if err != nil {
		t.Fatal(err)
	}
	var res lspDefinitionResult
	decodeNavResult(t, got, &res)
	if len(res.Candidates) != 2 {
		t.Fatalf("expected both candidates listed, got %d: %s", len(res.Candidates), got)
	}
	if res.Note == "" || !strings.Contains(res.Note, "matched 2") {
		t.Errorf("note must tell the model the name was ambiguous: %q", res.Note)
	}
	if res.Candidates[0].Container != "Manager" || res.Candidates[1].Container != "Client" {
		t.Errorf("candidates must carry their containers so the model can pick: %+v", res.Candidates)
	}
	if res.Candidates[0].Kind != "method" {
		t.Errorf("kind should render as a label, got %q", res.Candidates[0].Kind)
	}
}

func TestLSPDefinition_MissingArgs(t *testing.T) {
	nav := &fakeNavigator{enabled: true}
	env := newNavTestEnv(t, nav)
	tl := NewLSPNavTools()
	// Neither symbol nor file+line: an operational error, which surfaces as a
	// result chunk (GuardedTool converts it), so InvokableRun returns nil err.
	got, err := runTool(env.ctx, tl.Definition, `{}`)
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	if !strings.Contains(got, "symbol") {
		t.Errorf("result should tell the model what to pass; got %q", got)
	}
}

func TestLSPDefinition_UnknownSymbol(t *testing.T) {
	nav := &fakeNavigator{enabled: true} // resolved stays empty
	env := newNavTestEnv(t, nav)
	tl := NewLSPNavTools()
	got, err := runTool(env.ctx, tl.Definition, `{"symbol":"NoSuchThing"}`)
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	if !strings.Contains(got, "NoSuchThing") {
		t.Errorf("result should name the symbol that was not found; got %q", got)
	}
}

// TestLSPDefinition_UnlocatableSymbolsAreFiltered pins the WorkspaceSymbol
// carve-out: a symbol may legally come back with a uri and no range, and
// treating line 0 as a position would send line -1 to the server.
func TestLSPDefinition_UnlocatableSymbolsAreFiltered(t *testing.T) {
	nav := &fakeNavigator{enabled: true}
	env := newNavTestEnv(t, nav)
	nav.resolved = []lsp.SymbolInfo{
		{Name: "Greet", Location: lsp.Location{Path: env.abs("lib.go")}}, // no range
		{Name: "Greet", Location: lsp.Location{Path: env.abs("lib.go"), Line: 3, Column: 6}},
	}
	nav.defs = []lsp.Location{{Path: env.abs("lib.go"), Line: 3, Column: 6}}

	tl := NewLSPNavTools()
	if _, err := runTool(env.ctx, tl.Definition, `{"symbol":"Greet"}`); err != nil {
		t.Fatal(err)
	}
	if nav.lastLine != 3 {
		t.Errorf("anchored at line %d; the rangeless candidate must have been skipped", nav.lastLine)
	}
}

// --- references ---

// TestLSPReferences_TruncatesAndPages pins the bound.
//
// A reference query on a popular symbol legitimately returns thousands of hits,
// each carrying a path, a line number, and a line of source. Returning them all
// does not merely waste tokens — it evicts the conversation the answer was
// needed in, so the tool destroys the context it was asked to inform.
func TestLSPReferences_TruncatesAndPages(t *testing.T) {
	nav := &fakeNavigator{enabled: true}
	env := newNavTestEnv(t, nav)
	for i := 1; i <= 200; i++ {
		nav.refs = append(nav.refs, lsp.Location{Path: env.abs("use.go"), Line: i, Column: 1})
	}

	tl := NewLSPNavTools()

	t.Run("default limit applies with no limit argument", func(t *testing.T) {
		got, err := runTool(env.ctx, tl.References, `{"file":"lib.go","line":3,"character":6}`)
		if err != nil {
			t.Fatal(err)
		}
		var res lspReferencesResult
		decodeNavResult(t, got, &res)
		if res.Returned != lspRefLimitDefault {
			t.Errorf("returned %d references with no limit set, want the default %d",
				res.Returned, lspRefLimitDefault)
		}
		if res.Total != 200 {
			t.Errorf("total = %d, want 200 — the model must learn how many it did NOT see", res.Total)
		}
		if !res.Truncated {
			t.Error("truncated flag not set; a silently cut list reads as a complete one")
		}
		if !strings.Contains(res.Note, "offset=") {
			t.Errorf("note must tell the model how to page: %q", res.Note)
		}
	})

	t.Run("limit is capped at the maximum", func(t *testing.T) {
		got, err := runTool(env.ctx, tl.References, `{"file":"lib.go","line":3,"limit":100000}`)
		if err != nil {
			t.Fatal(err)
		}
		var res lspReferencesResult
		decodeNavResult(t, got, &res)
		if res.Returned > lspRefLimitMax {
			t.Errorf("returned %d, above the hard cap %d — the limit argument lets the "+
				"model ask for more, not opt out of the bound", res.Returned, lspRefLimitMax)
		}
	})

	t.Run("offset pages forward without overlap", func(t *testing.T) {
		got, err := runTool(env.ctx, tl.References, `{"file":"lib.go","line":3,"limit":10,"offset":50}`)
		if err != nil {
			t.Fatal(err)
		}
		var res lspReferencesResult
		decodeNavResult(t, got, &res)
		if res.Returned != 10 {
			t.Fatalf("returned %d, want 10", res.Returned)
		}
		if res.References[0].Line != 51 {
			t.Errorf("first reference at line %d, want 51 (offset 50 into a 1..200 list)",
				res.References[0].Line)
		}
	})

	t.Run("offset past the end yields an empty page, not an error", func(t *testing.T) {
		got, err := runTool(env.ctx, tl.References, `{"file":"lib.go","line":3,"offset":9999}`)
		if err != nil {
			t.Fatal(err)
		}
		var res lspReferencesResult
		decodeNavResult(t, got, &res)
		if res.Returned != 0 || res.Total != 200 {
			t.Errorf("want an empty page over a total of 200, got returned=%d total=%d",
				res.Returned, res.Total)
		}
	})
}

func TestLSPReferences_PassesIncludeDeclaration(t *testing.T) {
	for _, want := range []bool{true, false} {
		nav := &fakeNavigator{enabled: true}
		env := newNavTestEnv(t, nav)
		nav.refs = []lsp.Location{{Path: env.abs("use.go"), Line: 4, Column: 6}}
		tl := NewLSPNavTools()
		args := `{"file":"lib.go","line":3,"include_declaration":false}`
		if want {
			args = `{"file":"lib.go","line":3,"include_declaration":true}`
		}
		if _, err := runTool(env.ctx, tl.References, args); err != nil {
			t.Fatal(err)
		}
		if nav.lastIncludeDecl != want {
			t.Errorf("include_declaration reached the manager as %v, want %v", nav.lastIncludeDecl, want)
		}
	}
}

// --- hover ---

func TestLSPHover_RendersContentsAndPosition(t *testing.T) {
	nav := &fakeNavigator{enabled: true}
	env := newNavTestEnv(t, nav)
	nav.resolved = []lsp.SymbolInfo{{Name: "Greet",
		Location: lsp.Location{Path: env.abs("lib.go"), Line: 3, Column: 6}}}
	nav.hover = &lsp.Hover{Contents: "func Greet(name string) string"}

	tl := NewLSPNavTools()
	got, err := runTool(env.ctx, tl.Hover, `{"symbol":"Greet"}`)
	if err != nil {
		t.Fatal(err)
	}
	var res lspHoverResult
	decodeNavResult(t, got, &res)
	if res.Contents != "func Greet(name string) string" {
		t.Errorf("contents = %q", res.Contents)
	}
	if res.File != "lib.go" || res.Line != 3 {
		t.Errorf("hover should report where it looked: %s:%d", res.File, res.Line)
	}
}

func TestLSPHover_EmptyResultExplainsItself(t *testing.T) {
	nav := &fakeNavigator{enabled: true}
	env := newNavTestEnv(t, nav)
	nav.resolved = []lsp.SymbolInfo{{Name: "Greet",
		Location: lsp.Location{Path: env.abs("lib.go"), Line: 3, Column: 6}}}
	nav.hover = nil

	tl := NewLSPNavTools()
	got, err := runTool(env.ctx, tl.Hover, `{"symbol":"Greet"}`)
	if err != nil {
		t.Fatal(err)
	}
	var res lspHoverResult
	decodeNavResult(t, got, &res)
	if res.Note == "" {
		t.Error("an empty hover with no note is indistinguishable from a broken tool")
	}
}

// --- symbols ---

func TestLSPSymbols_FileOutlineAndWorkspaceSearch(t *testing.T) {
	nav := &fakeNavigator{enabled: true}
	env := newNavTestEnv(t, nav)
	nav.docSyms = []lsp.SymbolInfo{
		{Name: "Greet", Kind: lsp.SymbolFunction,
			Location: lsp.Location{Path: env.abs("lib.go"), Line: 3, Column: 6}},
	}
	nav.resolved = []lsp.SymbolInfo{
		{Name: "Greet", Kind: lsp.SymbolFunction, Container: "p",
			Location: lsp.Location{Path: env.abs("lib.go"), Line: 3, Column: 6}},
	}
	tl := NewLSPNavTools()

	t.Run("file gives an outline", func(t *testing.T) {
		got, err := runTool(env.ctx, tl.Symbols, `{"file":"lib.go"}`)
		if err != nil {
			t.Fatal(err)
		}
		var res lspSymbolsResult
		decodeNavResult(t, got, &res)
		if len(res.Symbols) != 1 || res.Symbols[0].Name != "Greet" || res.Symbols[0].Line != 3 {
			t.Fatalf("outline wrong: %s", got)
		}
		if res.Symbols[0].Kind != "function" {
			t.Errorf("kind = %q, want a readable label", res.Symbols[0].Kind)
		}
		// The path handed to the manager must be the jailed absolute one.
		if nav.docSymbolPathAsked != env.abs("lib.go") {
			t.Errorf("manager asked about %q, want %q", nav.docSymbolPathAsked, env.abs("lib.go"))
		}
	})

	t.Run("query searches the workspace", func(t *testing.T) {
		got, err := runTool(env.ctx, tl.Symbols, `{"query":"Greet"}`)
		if err != nil {
			t.Fatal(err)
		}
		var res lspSymbolsResult
		decodeNavResult(t, got, &res)
		if len(res.Symbols) != 1 || res.Symbols[0].Container != "p" {
			t.Fatalf("workspace search wrong: %s", got)
		}
	})

	t.Run("neither file nor query is an argument error", func(t *testing.T) {
		got, err := runTool(env.ctx, tl.Symbols, `{}`)
		if err != nil {
			t.Fatalf("unexpected Go error: %v", err)
		}
		if !strings.Contains(got, "file") || !strings.Contains(got, "query") {
			t.Errorf("result should name both entry points; got %q", got)
		}
	})
}

// --- path jail ---

// TestLSPNav_PathJailRejectsEscapes pins that the navigation tools honour the
// same jail every fs tool does. They take a model-supplied path and read it
// from disk to attach source lines, so an unjailed path here would be an fs
// read outside the project through a tool nobody thought of as an fs tool.
func TestLSPNav_PathJailRejectsEscapes(t *testing.T) {
	nav := &fakeNavigator{enabled: true}
	env := newNavTestEnv(t, nav)
	tl := NewLSPNavTools()

	outside := filepath.Join(filepath.Dir(env.root), "outside.go")
	mustWrite(t, outside, "package x\n")

	for _, args := range []string{
		`{"file":"../outside.go","line":1}`,
		`{"file":"../../etc/passwd","line":1}`,
		`{"file":"` + strings.ReplaceAll(outside, `\`, `\\`) + `","line":1}`,
	} {
		got, err := runTool(env.ctx, tl.Definition, args)
		if err != nil {
			t.Fatalf("%s: unexpected Go error: %v", args, err)
		}
		if !strings.Contains(got, "root") {
			t.Errorf("%s: escaped the jail, result was %q", args, got)
		}
		if nav.lastPath != "" {
			t.Errorf("%s: a rejected path still reached the language server as %q", args, nav.lastPath)
		}
	}
}

// TestLSPNav_LocationsOutsideRootKeepURIAndDropSource pins the other half: a
// definition in the module cache is a legitimate answer whose contents are
// outside the jail. Reporting the location while withholding the source line is
// the honest middle — dropping the whole location would hide a real answer, and
// reading the file would breach the jail from the result side.
func TestLSPNav_LocationsOutsideRootKeepURIAndDropSource(t *testing.T) {
	nav := &fakeNavigator{enabled: true}
	env := newNavTestEnv(t, nav)
	outsideDir := t.TempDir()
	outside := filepath.Join(outsideDir, "dep.go")
	mustWrite(t, outside, "package dep\n\nfunc Secret() {}\n")
	nav.defs = []lsp.Location{{Path: outside, URI: "file://" + filepath.ToSlash(outside), Line: 3, Column: 6}}

	tl := NewLSPNavTools()
	got, err := runTool(env.ctx, tl.Definition, `{"file":"use.go","line":4,"character":6}`)
	if err != nil {
		t.Fatal(err)
	}
	var res lspDefinitionResult
	decodeNavResult(t, got, &res)
	if len(res.Definitions) != 1 {
		t.Fatalf("got %d definitions: %s", len(res.Definitions), got)
	}
	d := res.Definitions[0]
	if d.Line != 3 {
		t.Errorf("out-of-root location must still be reported: %+v", d)
	}
	if d.Text != "" {
		t.Errorf("source line for a file outside the work root leaked: %q", d.Text)
	}
	if strings.Contains(got, "func Secret") {
		t.Errorf("file contents outside the jail appeared in the result: %s", got)
	}
}

// TestLSPNav_NonFileURIKeepsRawURI pins the jdt:/untitled: case. Those are real
// answers with no path on this disk; fabricating one would send the model to
// open a file that does not exist.
func TestLSPNav_NonFileURIKeepsRawURI(t *testing.T) {
	nav := &fakeNavigator{enabled: true}
	env := newNavTestEnv(t, nav)
	nav.defs = []lsp.Location{{Path: "", URI: "jdt://contents/rt.jar/java.lang/String.class", Line: 42}}

	tl := NewLSPNavTools()
	got, err := runTool(env.ctx, tl.Definition, `{"file":"use.go","line":4}`)
	if err != nil {
		t.Fatal(err)
	}
	var res lspDefinitionResult
	decodeNavResult(t, got, &res)
	if len(res.Definitions) != 1 || res.Definitions[0].URI == "" {
		t.Fatalf("a non-file location must survive with its raw URI: %s", got)
	}
	if res.Definitions[0].File != "" {
		t.Errorf("a non-file location must not be given a fake path: %q", res.Definitions[0].File)
	}
}

// --- guard integration ---

// TestLSPNav_DeniedFSReadIsRefused pins that the profile governs these tools.
// They read files the model names, so a profile that forbids reading must
// forbid it here too — otherwise lsp_symbols is an unguarded fs_read.
func TestLSPNav_DeniedFSReadIsRefused(t *testing.T) {
	nav := &fakeNavigator{enabled: true}
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "lib.go"), "package p\n")
	ctx := WithWorkRoot(context.Background(), root)
	ctx = WithProfile(ctx, guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"*"}},
		FS:    guard.FSPerm{Read: nil}, // empty read allowlist: deny
	})
	ctx = WithLSP(ctx, nav)

	tl := NewLSPNavTools()
	got, err := runTool(ctx, tl.Symbols, `{"file":"lib.go"}`)
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	if !strings.Contains(strings.ToLower(got), "denied") {
		t.Errorf("an empty FS read allowlist must deny the outline; got %q", got)
	}
	if nav.docSymbolPathAsked != "" {
		t.Errorf("a denied file still reached the language server: %q", nav.docSymbolPathAsked)
	}
}

// TestLSPNav_DeniedSnippetKeepsLocation pins the per-file static re-check in
// the reference renderer. A reference list can span dozens of files; a file the
// profile refuses loses its snippet and keeps its location, which is strictly
// more information than dropping it, and does so WITHOUT a dialog per file.
func TestLSPNav_DeniedSnippetKeepsLocation(t *testing.T) {
	nav := &fakeNavigator{enabled: true}
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "ok.go"), "package p\n\nvar Visible = 1\n")
	mustWrite(t, filepath.Join(root, "secret.go"), "package p\n\nvar Hidden = 2\n")
	ctx := WithWorkRoot(context.Background(), root)
	ctx = WithProfile(ctx, guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"*"}},
		FS:    guard.FSPerm{Read: []string{"**/ok.go"}},
	})
	ctx = WithLSP(ctx, nav)
	nav.refs = []lsp.Location{
		{Path: filepath.Join(root, "ok.go"), Line: 3, Column: 5},
		{Path: filepath.Join(root, "secret.go"), Line: 3, Column: 5},
	}

	tl := NewLSPNavTools()
	got, err := runTool(ctx, tl.References, `{"file":"ok.go","line":3,"character":5}`)
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	var res lspReferencesResult
	decodeNavResult(t, got, &res)
	if len(res.References) != 2 {
		t.Fatalf("both locations should be listed, got %d: %s", len(res.References), got)
	}
	byFile := map[string]lspLocationOut{}
	for _, r := range res.References {
		byFile[r.File] = r
	}
	if !strings.Contains(byFile["ok.go"].Text, "Visible") {
		t.Errorf("readable file lost its snippet: %+v", byFile["ok.go"])
	}
	if byFile["secret.go"].Text != "" {
		t.Errorf("a file the profile denies leaked its source line: %q", byFile["secret.go"].Text)
	}
	if strings.Contains(got, "Hidden") {
		t.Errorf("denied file contents appeared in the result: %s", got)
	}
}

// --- manager error propagation ---

func TestLSPNav_ManagerErrorsSurfaceAsResults(t *testing.T) {
	nav := &fakeNavigator{enabled: true, resolveErr: errors.New("gopls: no views in this session")}
	env := newNavTestEnv(t, nav)
	tl := NewLSPNavTools()
	got, err := runTool(env.ctx, tl.Definition, `{"symbol":"Greet"}`)
	if err != nil {
		t.Fatalf("a server error must not abort the turn: %v", err)
	}
	if !strings.Contains(got, "no views") {
		t.Errorf("the server's reason should reach the model; got %q", got)
	}
}

// --- registration surface ---

// TestLSPNavTools_Surface pins the tool names and the fact that all four are
// returned by Tools(). A tool built but left out of Tools() is dead code the
// composition root cannot register, which is this repo's dominant failure mode.
func TestLSPNavTools_Surface(t *testing.T) {
	tl := NewLSPNavTools()
	want := map[string]bool{
		"lsp_definition": false, "lsp_references": false,
		"lsp_hover": false, "lsp_symbols": false,
	}
	got := tl.Tools()
	if len(got) != len(want) {
		t.Fatalf("Tools() returned %d tools, want %d", len(got), len(want))
	}
	for _, g := range got {
		if _, ok := want[g.name]; !ok {
			t.Errorf("unexpected tool name %q", g.name)
			continue
		}
		want[g.name] = true
		if g.DisplayName() == "" {
			t.Errorf("%s has no display name", g.name)
		}
		if g.DefaultTimeout() != LSPNavTimeout {
			t.Errorf("%s timeout = %v, want %v", g.name, g.DefaultTimeout(), LSPNavTimeout)
		}
		if g.ApprovalRequired() {
			t.Errorf("%s is approval-gated; these are read-only lookups", g.name)
		}
	}
	for name, seen := range want {
		if !seen {
			t.Errorf("%s is missing from Tools(); the composition root cannot register it", name)
		}
	}
}
