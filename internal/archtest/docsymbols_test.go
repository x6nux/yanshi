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
	"unicode"
	"unicode/utf8"
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
// `Test*` names in the live docs found exactly 5 unresolvable ones, and all 5
// were legitimate: two were deliberately-quoted OLD names inside rename
// records, two were illustrative placeholders (`TestX` and friends), and one
// was a real Go type that simply is not a test function. Zero true positives,
// five false ones — and a gate that reddens on honest history is a gate that
// gets deleted, which is the larger hole (same trade-off `unconditionalSkip`
// records in ADR-0011). The denominator is deliberately not recorded here: it
// moves every time anybody cites a test, which is exactly the rot the
// checklist's F1 rule is about.
//
// ALSO NOT SCANNED, and easy to miss because it looks protected: the `::Symbol`
// SHORTHAND, written with no path at all when the previous citation on the same
// line already established one. The regexp below requires at least one path
// character before the separator, so every one of these falls outside the gate
// — they are neither a protected `path::Symbol` citation nor a bare name
// carrying the "I mean a phantom" signal, and the acceptance breakdown is full
// of them. They were audited by hand when this note was written and all
// resolved; there is no machine check, and inventing one would mean guessing
// which earlier citation on the line a shorthand inherits from. Treat them like
// bare names during a review: read them, do not trust them. (An audit script
// must also expect one false positive that is not ours — GitHub Actions
// workflow annotations are spelled `::error::`.)
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
// a dotted qualifier (Type.Method, Type.Field). It is applied to a line that
// stripMarkdownEmphasis has already run over — see that function for why the
// two cannot be separated.
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

// emphasisRunRe matches a run of markdown emphasis delimiters. Markdown has
// exactly two of them, `*` and `_`, and `_` is also a legal Go identifier and
// path character — that overlap is the whole problem stripMarkdownEmphasis
// exists to solve.
var emphasisRunRe = regexp.MustCompile(`[*_]+`)

// stripMarkdownEmphasis removes emphasis delimiter runs from a line before
// citations are extracted from it, keeping only the runs that sit INSIDE a word.
//
// Deleting the trailing-`*` prefix form fixed the bold bypass for asterisks and
// only for asterisks, because `*` is not a path or identifier character: the
// regexp simply stopped at it. `_` is a different animal — it is in both
// character classes — so an underscore-emphasized citation was swallowed whole
// rather than merely demoted:
//
//	_internal/pkg/thing.go::NewNam_
//	  path   -> "_internal/pkg/thing.go"   (leading _ eaten by the path class)
//	  symbol -> "NewNam_"                  (trailing _ eaten by the symbol class)
//
// candidates() then resolves nothing for that path, and unresolvedDocSymbols
// treats an unresolvable path as "not a citation of this module" and skips it.
// So `_..._` and `__...__` were not weakened citations, they were INVISIBLE
// ones — strictly worse than the `**` bypass that motivated removing the prefix
// form, and reachable by the same purely decorative edit.
//
// Fixing this by dropping `_` from the path class is not available: real paths
// here are full of it (plan_property_test.go, feature_status.go). So emphasis is
// removed lexically instead, which also repairs a third shape the old regexp
// missed entirely — `**path**::Symbol`, where the delimiters land between the
// path and the separator and no match was produced at all.
//
// The keep rule is "intraword runs stay": a run whose neighbours on BOTH sides
// are letters or digits is part of an identifier, never emphasis (CommonMark
// forbids intraword `_` emphasis for the same reason). Everything else goes.
// That preserves plan_property_test.go and TestFamily_A while stripping the
// leading and trailing runs of every emphasis form. Runs of `*` are covered by
// the same rule at no extra cost.
//
// One shape it does rewrite: a glob such as `tools/*.go::Foo` loses its
// asterisk, because the neighbours are `/` and `.` rather than word characters.
// The VERDICT is unaffected — "tools/.go" resolves to no file, just as the old
// regexp's ".go" did — so the rewrite is cosmetic in the only place it is
// observable. TestStripMarkdownEmphasisKeepsIntrawordRuns pins that as an
// outcome rather than as unchanged text, so the distinction cannot rot into an
// assumption.
func stripMarkdownEmphasis(line string) string {
	locs := emphasisRunRe.FindAllStringIndex(line, -1)
	if locs == nil {
		return line
	}
	wordAt := func(r rune, ok bool) bool {
		return ok && (unicode.IsLetter(r) || unicode.IsDigit(r))
	}
	var b strings.Builder
	prev := 0
	for _, loc := range locs {
		before, beforeOK := utf8.DecodeLastRuneInString(line[:loc[0]])
		after, afterOK := utf8.DecodeRuneInString(line[loc[1]:])
		if wordAt(before, beforeOK > 0) && wordAt(after, afterOK > 0) {
			continue // intraword: part of an identifier, not emphasis
		}
		b.WriteString(line[prev:loc[0]])
		prev = loc[1]
	}
	b.WriteString(line[prev:])
	return b.String()
}

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
		for _, m := range docSymbolRefRe.FindAllStringSubmatch(stripMarkdownEmphasis(line), -1) {
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
// markdown emphasis, or in any other decoration this repository's documents
// use, must be judged exactly like a bare one.
//
// The failure this pins was real and silent, and it had two layers. With the
// old regexp the closing `**` of a bold span landed in the prefix capture, so
// `**pkg::NewNam**` was read as "any symbol starting with NewNam", which the
// real NewName satisfied. Removing that prefix form fixed only the asterisk
// half: `_` is BOTH an emphasis delimiter and a legal path/identifier
// character, so `_pkg::NewNam_` did not get demoted to a prefix, it got eaten —
// the leading underscore joined the path, the path stopped resolving, and
// unresolvedDocSymbols dropped the whole citation as "not ours". The first
// version of this probe enumerated three asterisk spellings and missed the one
// other character markdown uses for emphasis, which was also the one character
// the path class contained.
//
// The table is therefore an ENUMERATION, not a sample: markdown has exactly two
// emphasis characters, and every decoration this repository's live documents
// put around a citation is listed. Anything that reddens here is a bypass.
func TestGOV9DetectsRenamedSymbolUnderMarkdownEmphasis(t *testing.T) {
	dir := withSyntheticModule(t, map[string]string{
		"internal/pkg/thing.go": "package pkg\n\nfunc NewName() {}\n",
	})
	idx := buildGoSymbolIndex(t, dir)

	for _, body := range []string{
		// baseline
		"plain: internal/pkg/thing.go::NewNam",
		// asterisk emphasis — all three spellings
		"bold: **internal/pkg/thing.go::NewNam**",
		"italic: *internal/pkg/thing.go::NewNam*",
		"bold+italic: ***internal/pkg/thing.go::NewNam***",
		// underscore emphasis — the half the first probe missed
		"underscore italic: _internal/pkg/thing.go::NewNam_",
		"underscore bold: __internal/pkg/thing.go::NewNam__",
		"underscore bold+italic: ___internal/pkg/thing.go::NewNam___",
		"mixed delimiters: *__internal/pkg/thing.go::NewNam__*",
		// emphasis that covers only the path, so the delimiters land between
		// the path and the separator — no match at all under the old regexp
		"partial bold: **internal/pkg/thing.go**::NewNam",
		"partial underscore: __internal/pkg/thing.go__::NewNam",
		// non-emphasis decorations: these never overlapped the character
		// classes, and the table exists so a future widening cannot break them
		// without saying so
		"strikethrough: ~~internal/pkg/thing.go::NewNam~~",
		"code span: `internal/pkg/thing.go::NewNam`",
		"html bold: <b>internal/pkg/thing.go::NewNam</b>",
		"html em: <em>internal/pkg/thing.go::NewNam</em>",
		"html code: <code>internal/pkg/thing.go::NewNam</code>",
		"link text: [internal/pkg/thing.go::NewNam](x.md)",
		"> blockquote: internal/pkg/thing.go::NewNam",
		"- dash bullet: internal/pkg/thing.go::NewNam",
		"* star bullet: internal/pkg/thing.go::NewNam",
		"+ plus bullet: internal/pkg/thing.go::NewNam",
		"1. ordered item: internal/pkg/thing.go::NewNam",
		"### heading: internal/pkg/thing.go::NewNam",
		"| table cell | internal/pkg/thing.go::NewNam |",
		"parenthesised: (internal/pkg/thing.go::NewNam)",
		"CJK quotes: 「internal/pkg/thing.go::NewNam」",
		"CJK full stop: internal/pkg/thing.go::NewNam。",
		"footnote marker: internal/pkg/thing.go::NewNam[^1]",
	} {
		bad := unresolvedDocSymbols(idx, parseDocSymbolRefs("d.md", body))
		if len(bad) != 1 {
			t.Errorf("decoration bypassed the gate in %q: got %+v", body, bad)
		}
	}
}

// TestStripMarkdownEmphasisKeepsIntrawordRuns is the reverse probe for the
// emphasis stripper, and it guards the reason `_` could not simply be dropped
// from the path character class: underscores inside words are load-bearing here.
//
// Both directions are checked on the same inputs — the text must survive
// unchanged, and a real citation dressed in emphasis must still resolve rather
// than merely stop being invisible.
func TestStripMarkdownEmphasisKeepsIntrawordRuns(t *testing.T) {
	for _, keep := range []string{
		"internal/ctxcompact/plan_property_test.go::runGeneratedProperty",
		"internal/pkg/thing_test.go::TestFamily_A",
		"snake_case_all_over::Some_Name",
		"a*b and 2*3",
	} {
		if got := stripMarkdownEmphasis(keep); got != keep {
			t.Errorf("stripMarkdownEmphasis(%q) = %q, want it unchanged", keep, got)
		}
	}

	dir := withSyntheticModule(t, map[string]string{
		"internal/pkg/thing_test.go": "package pkg\n\nfunc TestFamily_A() {}\n",
	})
	idx := buildGoSymbolIndex(t, dir)
	for _, ref := range []string{
		"internal/pkg/thing_test.go::TestFamily_A",
		"_internal/pkg/thing_test.go::TestFamily_A_",
		"__internal/pkg/thing_test.go::TestFamily_A__",
		"**internal/pkg/thing_test.go::TestFamily_A**",
	} {
		if bad := unresolvedDocSymbols(idx, parseDocSymbolRefs("d.md", ref)); len(bad) != 0 {
			t.Errorf("false positive on a live citation %q: %+v", ref, bad)
		}
	}

	// A glob path is the one shape the stripper rewrites. Its verdict is what
	// matters and it must stay "not a citation of this module", exactly as it
	// was when the regexp simply stopped at the asterisk.
	if bad := unresolvedDocSymbols(idx, parseDocSymbolRefs("d.md", "internal/pkg/*.go::Nope")); len(bad) != 0 {
		t.Errorf("a glob path must stay unresolvable, not become a finding: %+v", bad)
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
