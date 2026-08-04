package archtest

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// GOV9 — documentation symbol references must resolve
// ---------------------------------------------------------------------------
//
// The review checklist's F3 rule says: cite code by SYMBOL, not by line number,
// because "符号改名时 grep 找得回来，行号漂移找不回来". That rule bought a real
// improvement — docs/superpowers/acceptance-breakdown.md dropped every Go line
// number — but it bought only half of what it promised. A symbol reference is
// recoverable by grep only if somebody actually greps it, and nobody does that
// on a document with hundreds of references. A renamed symbol rots exactly as
// silently as a drifted line number; it just rots into a name that reads
// plausible instead of a number that reads plausible.
//
// The rule broke on its own author first: the checklist's F3 section pointed at
// a helper by a name that a rename six commits earlier had already retired, and
// the checklist is the ONE document that section governs. That is the shape of
// failure this gate exists to make loud. Two more turned up in the ledger's own
// lookup table on the gate's first run — both evidence citations, both naming
// tests that do not exist.
//
// WHAT IS SCANNED: live documents only. Dated archives under
// docs/superpowers/{plans,notes,specs} are records of a moment, exactly like
// the d2HistoricalDocs carve-out in removal_test.go — rewriting them to track
// a rename would falsify the record. reference/ holds vendored third-party
// material and is not ours to correct.
//
// WHAT IS FLAGGED: a reference whose PATH resolves but whose SYMBOL does not.
// An unresolvable path is skipped on purpose, and that skip carries three jobs
// at once:
//
//   - It is the self-reference escape hatch. A document that must NAME a
//     phantom — the checklist's section H does, and F3 now does too — writes it
//     without a resolvable path prefix, and this gate stays quiet. Without that
//     hatch the gate would forbid documenting the very failure it detects.
//   - It lets templates keep placeholder paths (`pkg/path::TestName` in
//     ADR-0011 and in the W0 plan).
//   - It keeps foreign `::` syntaxes out — Rust paths and pytest node IDs both
//     use the separator and neither names anything in this module.
//
// There is deliberately NO exemption table, on the same reasoning as GOV7 and
// GOV8's reconciliation half: the escape hatch above is principled and costs
// one edit, so an exemption table would only be a way to keep a wrong name.
//
// WHAT IS NOT SCANNED, and cannot be: a BARE symbol name in backticks, with no
// path in front of it. That half of the checklist's F3 rule stays manual, and
// the reason is measured rather than assumed. A trial gate over backticked
// `Test*` names found 5 unresolvable ones among 256 in the live docs, and all
// 5 were legitimate: two were deliberately-quoted OLD names inside rename
// records, two were illustrative placeholders (`TestX` and friends), and one
// was a real Go type that simply is not a test function. Zero true positives,
// five false ones — and a gate that reddens on honest history is a gate that
// gets deleted, which is the larger hole (same trade-off `unconditionalSkip`
// records in ADR-0011).
//
// The asymmetry is structural, not incidental. Writing the path prefix IS the
// signal "I am pointing at this, and it is supposed to exist"; omitting it is
// how a document says "I am naming something that does not exist, on purpose".
// Strip the prefix and both intents produce identical text, so no analysis can
// separate a dead pointer from a deliberate record. A bare name is therefore
// unprotected by construction — and it has already cost this repository a real
// miss: a rename left two live pointers to the ctxcompact property-test entry
// helper standing in ADR-0011 and in the acceptance breakdown, written as bare
// backticked names, and this gate stayed green through all of it.
//
// Lookup is file-scoped with a package-scoped fallback. Splitting a file is
// routine here (the 1000-line rule forces it) and moving a symbol to a sibling
// file in the same package does not break a grep, so it must not redden.

// docSymbolRefRe matches a `<path>::<Symbol>` reference. The symbol side allows
// a dotted qualifier (Type.Method, Type.Field).
//
// There is deliberately NO trailing-`*` prefix form. The first version had one
// — `path::Family_*` was meant to name a family of sibling tests — and it made
// the gate's verdict depend on TYPOGRAPHY: markdown bold is `**`, so writing
// `**pkg::Symbol**` fed the closing asterisks straight into that capture and
// silently demoted the name to a prefix, which then matched any longer real
// symbol that started with it. Bold is this repository's dominant emphasis
// style, so a purely decorative edit could strip a citation of its protection
// forever. The feature had zero users repo-wide when it was removed (the one
// real prefix citation was retired in the same commit that added the gate), so
// the cost of dropping it is nil and the cost of keeping it is a bypass. Cite a
// family by naming one concrete member, or by naming the package.
var docSymbolRefRe = regexp.MustCompile(`([A-Za-z0-9_./-]+)::([A-Za-z_][A-Za-z0-9_]*(?:\.[A-Za-z_][A-Za-z0-9_]*)*)`)

// docSymbolRef is one `path::Symbol` citation found in a document.
type docSymbolRef struct {
	Doc    string // module-relative, slash-separated
	Line   int
	Path   string // left of "::"
	Symbol string // right of "::"
	Raw    string
}

// goSymbolIndex maps a module-relative Go file path to the set of names it
// declares at package level, including "Type.Method", "Type.Field" and
// "Interface.Method" composites.
type goSymbolIndex struct {
	byFile map[string]map[string]bool
	byDir  map[string][]string // dir -> files in it
}

// docScanSkipDirs are directories the document scan never descends into.
// The superpowers subdirectories are dated archives (see the GOV9 comment);
// reference/ is vendored third-party material.
var docScanSkipDirs = map[string]bool{
	".git":                   true,
	"node_modules":           true,
	"reference":              true,
	"docs/superpowers/plans": true,
	"docs/superpowers/notes": true,
	"docs/superpowers/specs": true,
}

// buildGoSymbolIndex parses every .go file under root (tests and third_party
// included — both are legitimate citation targets) and records the package-level
// names each file declares.
//
// Build constraints are deliberately ignored: parser.ParseFile does not apply
// them, so a citation of a GOOS-specific file such as the bubbletea fork's
// Windows key handling resolves on every platform. A gate whose verdict changed
// with GOOS would be worse than no gate.
func buildGoSymbolIndex(t *testing.T, root string) *goSymbolIndex {
	t.Helper()
	idx := &goSymbolIndex{byFile: map[string]map[string]bool{}, byDir: map[string][]string{}}
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel := short(p, root)
		if d.IsDir() {
			if docScanSkipDirs[rel] || d.Name() == ".git" || d.Name() == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") {
			return nil
		}
		f, perr := parser.ParseFile(token.NewFileSet(), p, nil, 0)
		if perr != nil {
			return nil // unparseable files cannot be cited meaningfully
		}
		idx.byFile[rel] = declaredNames(f)
		dir := filepath.ToSlash(filepath.Dir(rel))
		idx.byDir[dir] = append(idx.byDir[dir], rel)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return idx
}

// declaredNames collects every package-level name in f, plus the composite
// "Owner.Member" forms that documentation uses to cite a method or a field.
func declaredNames(f *ast.File) map[string]bool {
	names := map[string]bool{}
	for _, decl := range f.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			names[d.Name.Name] = true
			if recv := recvTypeName(d.Recv); recv != "" {
				names[recv+"."+d.Name.Name] = true
			}
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				switch s := spec.(type) {
				case *ast.TypeSpec:
					names[s.Name.Name] = true
					addTypeMembers(names, s)
				case *ast.ValueSpec:
					for _, n := range s.Names {
						names[n.Name] = true
					}
				}
			}
		}
	}
	return names
}

// addTypeMembers records struct fields and interface methods as "Type.Member".
func addTypeMembers(names map[string]bool, ts *ast.TypeSpec) {
	var fields *ast.FieldList
	switch t := ts.Type.(type) {
	case *ast.StructType:
		fields = t.Fields
	case *ast.InterfaceType:
		fields = t.Methods
	default:
		return
	}
	if fields == nil {
		return
	}
	for _, f := range fields.List {
		for _, n := range f.Names {
			names[ts.Name.Name+"."+n.Name] = true
		}
	}
}

// liveDocs returns the module-relative paths of every markdown document GOV9
// holds to the symbol-reference rule.
func liveDocs(t *testing.T, root string) []string {
	t.Helper()
	var docs []string
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel := short(p, root)
		if d.IsDir() {
			if docScanSkipDirs[rel] || d.Name() == ".git" || d.Name() == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(p, ".md") && rel != "CHANGELOG.md" {
			docs = append(docs, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(docs)
	return docs
}

// parseDocSymbolRefs extracts every `path::Symbol` citation from a document.
func parseDocSymbolRefs(doc, body string) []docSymbolRef {
	var refs []docSymbolRef
	for i, line := range strings.Split(body, "\n") {
		for _, m := range docSymbolRefRe.FindAllStringSubmatch(line, -1) {
			refs = append(refs, docSymbolRef{
				Doc: doc, Line: i + 1,
				Path: m[1], Symbol: m[2], Raw: m[0],
			})
		}
	}
	return refs
}

// candidates resolves a citation's path to the Go files it could name. A path
// ending in ".go" is matched as a file-path suffix; anything else as a package
// directory suffix. Suffix matching (rather than exact) is what lets a document
// write "guard.go::GuardedTool.Stream" without spelling the full path — the
// abbreviation is the house style and predates this gate.
//
// An empty result means "not ours": a placeholder, a foreign language's `::`,
// or a deliberately named phantom. Callers skip those.
func (idx *goSymbolIndex) candidates(path string) []string {
	var out []string
	if strings.HasSuffix(path, ".go") {
		for f := range idx.byFile {
			if f == path || strings.HasSuffix(f, "/"+path) {
				out = append(out, f)
			}
		}
	} else {
		clean := strings.TrimSuffix(path, "/")
		for dir, files := range idx.byDir {
			if dir == clean || strings.HasSuffix(dir, "/"+clean) {
				out = append(out, files...)
			}
		}
	}
	sort.Strings(out)
	return out
}

// resolves reports whether sym is declared in any of files, or — failing that —
// anywhere in those files' packages. The package-wide fallback keeps routine
// file splits from reddening the gate; see the GOV9 comment.
func (idx *goSymbolIndex) resolves(files []string, sym string) bool {
	match := func(names map[string]bool) bool { return names[sym] }
	for _, f := range files {
		if match(idx.byFile[f]) {
			return true
		}
	}
	seen := map[string]bool{}
	for _, f := range files {
		dir := filepath.ToSlash(filepath.Dir(f))
		if seen[dir] {
			continue
		}
		seen[dir] = true
		for _, sibling := range idx.byDir[dir] {
			if match(idx.byFile[sibling]) {
				return true
			}
		}
	}
	return false
}

// unresolvedDocSymbols returns the citations whose path resolves but whose
// symbol does not. Split out from the test so the self-tests below can drive it
// against synthetic trees in both directions.
func unresolvedDocSymbols(idx *goSymbolIndex, refs []docSymbolRef) []docSymbolRef {
	var bad []docSymbolRef
	for _, r := range refs {
		files := idx.candidates(r.Path)
		if len(files) == 0 {
			continue // not a citation of this module — see GOV9 comment
		}
		if !idx.resolves(files, r.Symbol) {
			bad = append(bad, r)
		}
	}
	return bad
}

// TestGOV9DocSymbolReferencesResolve fails when a live document cites a Go
// symbol that no longer exists at the path it names.
//
// This closes the half of the review checklist's F3 rule that the rule itself
// could not close: switching from line numbers to symbol names removed the
// silent numeric drift, but left silent NAME drift in its place, and a document
// with hundreds of citations is never re-grepped by hand.
func TestGOV9DocSymbolReferencesResolve(t *testing.T) {
	root := moduleRoot(t)
	idx := buildGoSymbolIndex(t, root)

	var refs []docSymbolRef
	for _, doc := range liveDocs(t, root) {
		body, err := os.ReadFile(filepath.Join(root, doc))
		if err != nil {
			t.Fatal(err)
		}
		refs = append(refs, parseDocSymbolRefs(doc, string(body))...)
	}
	if len(refs) == 0 {
		t.Fatal("no path::Symbol citations found in live docs — the scan is broken, " +
			"not the docs (docs/superpowers/acceptance-breakdown.md is built on them)")
	}

	for _, r := range unresolvedDocSymbols(idx, refs) {
		t.Errorf("%s:%d cites %q but %q declares no such symbol\n"+
			"\tthe path resolves, so this is a renamed or deleted symbol, not a placeholder.\n"+
			"\tfix the citation. To name a symbol that genuinely does not exist (documenting\n"+
			"\ta phantom), write it WITHOUT a resolvable path prefix.",
			r.Doc, r.Line, r.Raw, r.Path)
	}
}

// TestGOV9DetectsRenamedSymbol is the forward probe: a citation of a symbol
// that was renamed must be reported.
func TestGOV9DetectsRenamedSymbol(t *testing.T) {
	dir := withSyntheticModule(t, map[string]string{
		"internal/pkg/thing.go": "package pkg\n\nfunc NewName() {}\n",
	})
	idx := buildGoSymbolIndex(t, dir)
	refs := parseDocSymbolRefs("d.md", "see `internal/pkg/thing.go::OldName` for details")
	bad := unresolvedDocSymbols(idx, refs)
	if len(bad) != 1 || bad[0].Symbol != "OldName" {
		t.Fatalf("renamed symbol not reported, got %+v", bad)
	}
}

// TestGOV9DetectsRenamedSymbolUnderMarkdownEmphasis is the forward probe for
// the bypass that killed the trailing-`*` prefix form: a citation wrapped in
// markdown emphasis must be judged exactly like a bare one.
//
// The failure this pins was real and silent. With the old regexp the closing
// `**` of a bold span landed in the prefix capture, so `**pkg::NewNam**` was
// read as "any symbol starting with NewNam", which the real NewName satisfied
// — the gate went green on a dead citation because somebody bolded it. Every
// asterisk emphasis form is probed, since the verdict must not depend on how
// many of them an author wrapped the citation in.
func TestGOV9DetectsRenamedSymbolUnderMarkdownEmphasis(t *testing.T) {
	dir := withSyntheticModule(t, map[string]string{
		"internal/pkg/thing.go": "package pkg\n\nfunc NewName() {}\n",
	})
	idx := buildGoSymbolIndex(t, dir)

	for _, body := range []string{
		"bold: **internal/pkg/thing.go::NewNam**",
		"italic: *internal/pkg/thing.go::NewNam*",
		"bold+italic: ***internal/pkg/thing.go::NewNam***",
	} {
		bad := unresolvedDocSymbols(idx, parseDocSymbolRefs("d.md", body))
		if len(bad) != 1 {
			t.Errorf("emphasis form went undetected in %q: got %+v", body, bad)
		}
	}
}

// TestGOV9AcceptsLegitimateShapes is the reverse probe. Every entry here is a
// shape that exists in this repository's live docs today; a gate that reddened
// on any of them would be deleted rather than obeyed.
func TestGOV9AcceptsLegitimateShapes(t *testing.T) {
	dir := withSyntheticModule(t, map[string]string{
		"internal/pkg/thing.go": "package pkg\n\ntype T struct{ Field int }\n\n" +
			"func (t *T) Method() {}\n\nfunc Plain() {}\n",
		"internal/pkg/other.go":      "package pkg\n\nfunc Sibling() {}\n",
		"internal/pkg/thing_test.go": "package pkg\n\nfunc TestFamily_A() {}\n",
	})
	idx := buildGoSymbolIndex(t, dir)

	for _, ref := range []string{
		"internal/pkg/thing.go::Plain",       // exact file path
		"pkg/thing.go::T.Method",             // abbreviated path + method
		"thing.go::T.Field",                  // bare file name + struct field
		"internal/pkg::Plain",                // package directory
		"internal/pkg/thing.go::Sibling",     // moved to a sibling file in the package
		"**internal/pkg/thing.go::Plain**",   // bold emphasis around a live citation
		"*internal/pkg::TestFamily_A*",       // italic emphasis around a live citation
		"pkg/path::TestName",                 // template placeholder: path unresolvable
		"std::thread",                        // a foreign language's separator
		"tests/tests/t.py::TestClass",        // pytest node id
		"internal/nosuchpkg::DeliberateName", // a documented phantom
	} {
		if bad := unresolvedDocSymbols(idx, parseDocSymbolRefs("d.md", ref)); len(bad) != 0 {
			t.Errorf("false positive on %q: %+v", ref, bad)
		}
	}
}
