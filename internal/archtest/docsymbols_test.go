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
// WHAT IS SCANNED: live documents only — every live .md, plus the one file
// named by ledgerDoc, which is where the reasoning behind every open ledger
// item lives and cites Go symbols the same way the breakdown does. Dated
// archives under docs/superpowers/{plans,notes,specs} are records of a moment,
// exactly like the d2HistoricalDocs carve-out in removal_test.go — rewriting
// them to track a rename would falsify the record. reference/ holds vendored
// third-party material and is not ours to correct.
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
// `Test*` names in the live docs found a handful of unresolvable ones and every
// single one was legitimate: deliberately-quoted OLD names inside rename
// records, illustrative placeholders, a real Go type that simply is not a test
// function, and the bare word Test — which names one of goalloop's three
// evaluators. Zero true positives — and a gate that reddens on honest history
// is a gate that gets deleted, which is the larger hole (same trade-off
// `unconditionalSkip` records in ADR-0011).
//
// NEITHER the numerator nor the denominator is recorded here, and the omission
// of the numerator is a correction rather than a style choice: an earlier
// version of this note wrote "exactly 5", the checklist copied it, and by the
// next reading it was 6. Both counts move every time anybody cites a test,
// which is the rot the checklist's F1 rule is about. Recompute them; the
// checklist's F3 section carries the two commands.
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
//
// The path capture must START WITH AN ASCII LETTER, and that restriction is
// load-bearing rather than cosmetic. Every path this module can name begins
// with a letter — a top-level directory (internal, cmd, docs, sdk, skills,
// third_party) or a bare file name — so nothing legitimate is lost. What it
// buys is that a stray leading character glued to the path by decoration can no
// longer swallow the citation whole: `_` and `-` and digits are all in the
// continuation class, and any of them sitting in front of `internal/...` used
// to produce an unresolvable path, which unresolvedDocSymbols then discarded as
// "not ours". Starting the match at the first letter re-anchors the citation on
// the real path, so the verdict is decided by the symbol instead of being
// skipped. See stripMarkdownEmphasis for the case that made this necessary.
var docSymbolRefRe = regexp.MustCompile(`([A-Za-z][A-Za-z0-9_./-]*)::([A-Za-z_][A-Za-z0-9_]*(?:\.[A-Za-z_][A-Za-z0-9_]*)*)`)

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
// are word characters is part of an identifier, never emphasis (CommonMark
// forbids intraword `_` emphasis for the same reason). Everything else goes.
// That preserves plan_property_test.go and TestFamily_A while stripping the
// leading and trailing runs of every emphasis form. Runs of `*` are covered by
// the same rule at no extra cost.
//
// "Word character" here means ASCII letter or digit, and the ASCII restriction
// is the second half of this fix rather than an accident of transcription. The
// first version asked unicode.IsLetter, which answers TRUE for CJK — and this
// repository's documents are majority Chinese and do not put a space before an
// opening delimiter. So `见_path::Sym_`, `**注意**_path::Sym_` (the `**_` runs
// merge into one) and every other citation whose opening `_` follows a Han
// character were classified as intraword, kept, glued onto the path, and the
// citation went INVISIBLE — the exact failure mode this function was written to
// close, surviving in the shape that is most likely to occur here. The
// identifiers and paths the keep rule protects (plan_property_test.go,
// TestFamily_A) are ASCII by construction: Go source in this module has no
// non-ASCII identifiers and no non-ASCII file names. Treating a Han neighbour
// as a non-word character therefore costs nothing and restores the strip.
//
// A leading run whose left neighbour is an ASCII alnum (`1_path::Sym_`) is
// still kept here — it is genuinely indistinguishable from an identifier at
// this layer — and is caught one layer down instead, by docSymbolRefRe's
// letter-start anchor. The two rules are complementary and both are pinned by
// TestGOV9DetectsRenamedSymbolUnderMarkdownEmphasis.
//
// Two shapes on the SYMBOL side are rewritten as a side effect, and both are
// currently harmless: `path.go::_helper` becomes `::helper` and `path.go::Foo_`
// becomes `::Foo`, because a delimiter next to `:` or next to end-of-line has a
// non-word neighbour and is stripped as emphasis. Harmless because this module
// declares no package-level identifier that begins or ends with an underscore
// (the only leading-underscore declaration anywhere is the blank `var _`), so
// no citation can currently be mis-resolved this way. It is written down
// because the day one appears, this gate would quietly judge a citation of it
// against a DIFFERENT name — a false negative if `helper` exists, a confusing
// false positive if it does not. Nothing enforces the precondition; a reviewer
// adding such an identifier should come back here.
//
// One shape on the PATH side is rewritten too: a glob such as `tools/*.go::Foo` loses its
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
		if !ok || r > unicode.MaxASCII {
			return false // see the ASCII note above: CJK is a letter, but not a word here
		}
		return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
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
			if docScanSkipDirs[rel] || isNestedCheckoutDir(rel, d.Name()) {
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

// ledgerDoc is the one non-markdown file GOV9 scans.
//
// It is not markdown, so it was outside the scan for as long as the scan was
// spelled ".md only" — and that omission was not neutral. The ledger is the
// single document GOV8 reads before a verdict is flipped, its comment blocks
// carry the reasoning behind every open item, and those comments cite Go
// symbols exactly the way the breakdown does. A review probe made the gap
// concrete: appending `internal/bootstrap/bootstrap.go::NoSuchSymbolZZZ` to
// this file left TestGOV9DocSymbolReferencesResolve green, while the same text
// in any live .md reddened it.
//
// One attribution about this file has now been wrong three times running, so it
// is corrected at the source rather than in prose somewhere else. Citations here
// were written with ABBREVIATED path prefixes (`bootstrap.go::Build`,
// `testrun.go::runTests`, …), and the claim that those prefixes "did not
// resolve, so GOV9 discarded them" is FALSE. candidates() matches a `.go` path
// as a FILE-PATH SUFFIX — `strings.HasSuffix(f, "/"+path)` — so a bare filename
// anchors onto internal/bootstrap/bootstrap.go by itself. deglued() is not what
// does this and structurally cannot be: it returns nil for any path without a
// `/`, precisely so a deliberate phantom bare filename is never re-anchored.
//
// The consequence that matters to anyone writing docs: an abbreviated prefix is
// FULLY PROTECTED the moment its file is in scan scope. `bootstrap.go::Phantom`
// is not an escape hatch — it reddens the gate exactly like a fully-spelled
// path would. The only escape hatch is a symbol with NO resolvable path prefix
// at all. Widening the scan was therefore the entire fix; spelling the prefixes
// out was cosmetic, and is worth doing only because a full path survives a file
// move that a suffix match would silently re-aim.
const ledgerDoc = "docs/feature-status.yaml"

// liveDocs returns the module-relative paths of every document GOV9 holds to
// the symbol-reference rule: every live markdown file, plus ledgerDoc.
func liveDocs(t *testing.T, root string) []string {
	t.Helper()
	var docs []string
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel := short(p, root)
		if d.IsDir() {
			if docScanSkipDirs[rel] || isNestedCheckoutDir(rel, d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if (strings.HasSuffix(p, ".md") && rel != "CHANGELOG.md") || rel == ledgerDoc {
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

// deglued re-resolves a path whose first segment carries a decoration-glued
// prefix, and it is the third and last layer of the emphasis defence.
//
// stripMarkdownEmphasis removes a delimiter run only when the run is not
// intraword, and docSymbolRefRe re-anchors the match on the first ASCII letter.
// Between them they cover every glue whose left neighbour is non-ASCII, a
// digit, or a non-letter path character. What neither can cover is an ASCII
// LETTER on the left — `see_internal/pkg/thing.go::Sym` is, at both of those
// layers, byte-for-byte a legal path that happens not to exist, and the "path
// does not resolve, so this is not our citation" skip swallowed it whole.
//
// So: when a path fails to resolve, retry once per `_` inside its FIRST
// `/`-segment. Confining the retry to the first segment is what keeps this from
// turning into a general suffix search — a deliberately phantom bare filename
// such as `no_such_file.go` must NOT be re-anchored onto a real `file.go`
// somewhere in the tree, and a path with no `/` at all is left alone entirely.
// A citation that only resolves after this trim is judged normally: if its
// symbol is real, nothing is reported (the decoration was cosmetic); if the
// symbol is dead, it is reported, which is the whole point.
//
// The three layers were swept together against every leading-character shape
// that could plausibly precede a citation in these documents — full-width
// underscore, zero-width space, CJK quotation marks, the em dash, the
// enumeration comma, full-width parentheses, a backslash-escaped underscore,
// accented Latin, an emoji, and a bare `.`/`-`/`/`/digit abutting the path.
// All of them redden on a dead symbol and none of them redden on a live one.
//
// Exactly ONE residual survives, and it is stated rather than papered over: an
// ASCII LETTER abutting the path with no delimiter at all in between
// (`xinternal/pkg/thing.go::Sym`, or a bare filename as `see_thing.go::Sym`).
// There is nothing left to detect at that point — the text is byte-for-byte a
// legal path that this module does not contain, indistinguishable from the
// deliberate phantom the skip exists to permit. It is also not a shape any
// markup produces: no emphasis, escape, or punctuation convention puts a bare
// letter against a path, so it can only arise from a typo, which renders
// visibly wrong. TestGOV9DetectsRenamedSymbolUnderMarkdownEmphasis pins the
// swept shapes; this paragraph is the honest edge of the sweep.
func (idx *goSymbolIndex) deglued(path string) []string {
	slash := strings.IndexByte(path, '/')
	if slash < 0 {
		return nil
	}
	for i := 0; i < slash; i++ {
		if path[i] != '_' {
			continue
		}
		if files := idx.candidates(path[i+1:]); len(files) > 0 {
			return files
		}
	}
	return nil
}

// unresolvedDocSymbols returns the citations whose path resolves but whose
// symbol does not. Split out from the test so the self-tests below can drive it
// against synthetic trees in both directions.
func unresolvedDocSymbols(idx *goSymbolIndex, refs []docSymbolRef) []docSymbolRef {
	var bad []docSymbolRef
	for _, r := range refs {
		files := idx.candidates(r.Path)
		if len(files) == 0 {
			files = idx.deglued(r.Path)
		}
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
		// A WORD CHARACTER IMMEDIATELY BEFORE THE OPENING DELIMITER. Every row
		// above this point has whitespace or start-of-line there, and that
		// uniformity was itself the hole: the intraword keep rule looks at both
		// neighbours, so it never fired for any of them. In a majority-Chinese
		// document nothing separates the prose from the delimiter, which makes
		// these the LIKELIEST spellings in this repository, not the exotic ones.
		"CJK then underscore: 见_internal/pkg/thing.go::NewNam_",
		"bold then underscore: **注意**_internal/pkg/thing.go::NewNam_",
		"CJK then underscore bold: 见__internal/pkg/thing.go::NewNam__",
		"CJK on both sides: 见_internal/pkg/thing.go::NewNam_了",
		"digit then underscore: 1_internal/pkg/thing.go::NewNam_",
		"latin then underscore: see_internal/pkg/thing.go::NewNam_",
		"CJK then asterisk: 见*internal/pkg/thing.go::NewNam*",
		"digit then asterisk: 1*internal/pkg/thing.go::NewNam*",
		// Leading characters that are in the path continuation class but cannot
		// begin a real path. Before the letter-start anchor these glued
		// themselves on and made the citation unresolvable, hence invisible.
		"hyphen glued: ->internal/pkg/thing.go::NewNam",
		"dot-slash prefix: ./internal/pkg/thing.go::NewNam",
		// The rest of the leading-character sweep. Nothing here is markdown
		// emphasis; each is a character that could sit against the opening
		// delimiter in a Chinese-language document, and each was run end to end
		// against the live gate before being written down.
		"full-width underscore: ＿internal/pkg/thing.go::NewNam＿",
		"zero-width space: ​_internal/pkg/thing.go::NewNam_",
		"CJK quotes hugging: 「_internal/pkg/thing.go::NewNam_」",
		"em dash: ——_internal/pkg/thing.go::NewNam_",
		"enumeration comma: 、_internal/pkg/thing.go::NewNam_",
		"full-width parens: （_internal/pkg/thing.go::NewNam_）",
		"escaped underscore: \\_internal/pkg/thing.go::NewNam\\_",
		"accented latin: é_internal/pkg/thing.go::NewNam_",
		"emoji: 🔥_internal/pkg/thing.go::NewNam_",
		"digit abutting: 1internal/pkg/thing.go::NewNam",
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
		// The same rows as the forward probe's word-character block, but on a
		// citation that is alive. Making a decorated dead pointer VISIBLE is
		// only half the fix; if it cost a false positive on the live spelling
		// the gate would be deleted rather than obeyed (checklist section B).
		"见_internal/pkg/thing_test.go::TestFamily_A_",
		"**注意**_internal/pkg/thing_test.go::TestFamily_A_",
		"见_internal/pkg/thing_test.go::TestFamily_A_了",
		"1_internal/pkg/thing_test.go::TestFamily_A_",
		"see_internal/pkg/thing_test.go::TestFamily_A_",
		"1*internal/pkg/thing_test.go::TestFamily_A*",
		"->internal/pkg/thing_test.go::TestFamily_A",
		"./internal/pkg/thing_test.go::TestFamily_A",
		"＿internal/pkg/thing_test.go::TestFamily_A＿",
		"「_internal/pkg/thing_test.go::TestFamily_A_」",
		"\\_internal/pkg/thing_test.go::TestFamily_A\\_",
		"é_internal/pkg/thing_test.go::TestFamily_A_",
		"🔥_internal/pkg/thing_test.go::TestFamily_A_",
		"1internal/pkg/thing_test.go::TestFamily_A",
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
		"internal/pkg/file.go":       "package pkg\n\nfunc Real() {}\n",
	})
	idx := buildGoSymbolIndex(t, dir)

	for _, ref := range []string{
		// deglued() must not turn into a general suffix search: a phantom bare
		// filename that merely ENDS in a real one stays a phantom. file.go is
		// in the synthetic tree above precisely so this row can fail.
		"no_such_file.go::Whatever",
		"a_b_c.go::Whatever",
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

// TestGOV9ScansTheLedger pins the ledger inside GOV9's scan.
//
// Without this the widening is one character away from silent reversal: the
// filter is a single boolean and a stale citation in the ledger produces no
// output whatsoever when the file drops out of scope, so the regression is
// invisible in exactly the way the gate exists to prevent. The reverse probe
// (the ledger citing something real, staying green) is not written separately
// — TestGOV9DocSymbolReferencesResolve is that probe on every run.
func TestGOV9ScansTheLedger(t *testing.T) {
	root := moduleRoot(t)

	var inScope bool
	for _, doc := range liveDocs(t, root) {
		if doc == ledgerDoc {
			inScope = true
		}
	}
	if !inScope {
		t.Fatalf("%s is not in liveDocs — its Go citations have no machine protection", ledgerDoc)
	}

	// The scan reaching the file is only half of it: the citations inside must
	// also survive parseDocSymbolRefs, which was written for markdown and runs
	// stripMarkdownEmphasis over every line. A yaml comment is not markdown, so
	// assert a real citation from this very file still parses out.
	//
	// It has to be a citation from a COMMENT line specifically, and `len(refs) >
	// 0` does not say that. Most citations in this file sit on `evidence:` VALUE
	// lines, which GOV8's resolveTestRef has covered all along; the marginal
	// value of GOV9 reading the ledger is exactly the minority that live in `#`
	// comments, and a count over both halves cannot tell them apart. Measured:
	// teaching parseDocSymbolRefs to skip `#`-leading lines left this test
	// GREEN under the old assertion.
	body, err := os.ReadFile(filepath.Join(root, ledgerDoc))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(body), "\n")
	refs := parseDocSymbolRefs(ledgerDoc, string(body))
	if len(refs) == 0 {
		t.Fatalf("no path::Symbol citations parsed out of %s — the parse is broken, "+
			"not the ledger", ledgerDoc)
	}
	var fromComment int
	for _, r := range refs {
		if r.Line >= 1 && r.Line <= len(lines) &&
			strings.HasPrefix(strings.TrimSpace(lines[r.Line-1]), "#") {
			fromComment++
		}
	}
	if fromComment == 0 {
		t.Fatalf("%d path::Symbol citations parsed out of %s but NONE from a `#` "+
			"comment line — the comment blocks are what GOV8 does not already "+
			"cover, so the scan is buying nothing here", len(refs), ledgerDoc)
	}
}

// goLineCiteRe matches a Go source citation carrying a line number, in either
// the `<file>.go:<line>` or the `<file>.go:<line>-<line>` spelling (written
// with placeholders here for the same reason the pattern is). It is written as a
// concatenation so that this very file does not contain a literal instance of
// the pattern it forbids — the scan below reads two data files, not this one,
// but a reviewer grepping the repository for the shape should not land here.
var goLineCiteRe = regexp.MustCompile(`\.go` + `:[0-9]+`)

// goLineCiteFreeDocs are the two documents that promise, in their own text, to
// cite Go code by symbol rather than by line, and are the inputs GOV8 reads
// before a ledger entry is flipped.
//
// Only these two. Elsewhere a line number is legitimate: the audit reports and
// the dated archives under docs/superpowers/ are snapshots of a moment, and
// rewriting them to chase a refactor falsifies the record — the same carve-out
// d2HistoricalDocs and the GOV9 scan itself make.
var goLineCiteFreeDocs = []string{
	"docs/superpowers/acceptance-breakdown.md",
	"docs/feature-status.yaml",
}

// TestNoGoLineCitationsInLedgerInputs fails when either ledger input cites Go
// code by line number.
//
// This is the companion to GOV9 and it exists because the promise failed on its
// own author, twice, in the same shape GOV9 was built for. The breakdown states
// that its Go line citations "have been reduced to zero"; the commit that wrote
// that sentence also rewrote two property-test files and left three citations
// pointing at lines that had moved by six to ten — while its own commit message
// announced that it had just repaired three drifted line numbers in the ledger.
// A sentence asserting a count is a wish until something computes the count, so
// this test computes it.
//
// The yaml half was written when the ledger was outside GOV9's reach entirely
// (that scan read .md only) and its citations therefore had no machine
// protection at all. GOV9 now covers the ledger too — see ledgerDoc — so the
// two gates finally divide the same file the way they divide the breakdown:
// GOV9 takes the symbol names, this test takes the line numbers. Line numbers
// there were all still accurate when this test was written, which is exactly
// when to remove the rot source — afterwards there is no way to tell drift
// from a citation that was always wrong.
//
// KNOWN OVER-REACH, recorded because it has no instance yet and would be
// mistaken for a real finding if one appeared: the pattern also matches QUOTED
// COMPILER AND TEST OUTPUT, where the line number is part of the transcript
// rather than a citation of anything — a pasted `<file>.go:<line>:<col>:
// undefined: x` diagnostic is reported exactly like a hand-written pointer.
// (Spelled with placeholders on purpose: this file must not contain a literal
// instance of the shape it forbids, for the reason goLineCiteRe records.)
// Neither of the two documents
// contains such a transcript today. If one ever needs to, the fix is to exempt
// the shape (a `:col:` suffix, or a fenced block), not to weaken the pattern:
// the surrounding-context test that would tell a transcript from a citation is
// the same guessing game the `::Symbol` shorthand was left out of.
//
// DELIBERATELY NOT EXTENDED TO NON-GO LINE CITATIONS, and the reasoning is
// recorded here so it is not re-litigated every review. The breakdown cites
// several non-Go file types by line too. The live list is whatever this prints
// — do NOT re-inline it here, because the whole job of this paragraph is to
// draw the boundary of what the gate does not cover, and an enumeration that
// has rotted sends the next person looking for citations that do not exist
// (the first version of this sentence named yaml and ts; the same commit had
// just rewritten the sole yaml citation into a key name, and ts had never
// appeared in this branch at all):
//
//	grep -ohE '[A-Za-z0-9_.-]+\.[a-z]+:[0-9]+' docs/superpowers/acceptance-breakdown.md |
//	  sed 's/.*\.//; s/:.*//' | sort | uniq -c | sort -rn
//
// At least one of those citations HAS already drifted (an observability block
// reference kept its old numbers after two config sections were inserted above
// it), so the rot is real, not hypothetical. The extension is still wrong
// today:
//
//   - The Go half works because it has a MACHINE-CHECKED replacement. GOV9
//     resolves `path.go::Symbol`, so trading a drifting number for a symbol
//     name trades a silent failure for a caught one.
//   - No such resolver exists for the other file types. Rewriting a line
//     citation into a yaml key path, a shell variable or a markdown heading
//     trades a drifting NUMBER for a drifting NAME — and the checklist's own
//     F3 finding is that a stale name is WORSE, because it still reads as
//     legitimate while a number that points at unrelated content is visibly
//     wrong to anyone who opens the file.
//   - A naive pattern also over-reaches badly here: the breakdown quotes a
//     loopback address with a port, which any "extension colon digits" regex
//     reads as a citation. Restricting to an extension whitelist fixes that
//     but is exactly the per-filetype work that the resolver would need
//     anyway.
//
// So the precondition for extending this gate is a non-Go anchor resolver (a
// GOV9 for yaml key paths first, since that is the only one with unambiguous
// structure). Until then, non-Go citations stay on the manual side, under
// section F3 of the review checklist.
func TestNoGoLineCitationsInLedgerInputs(t *testing.T) {
	root := moduleRoot(t)
	for _, doc := range goLineCiteFreeDocs {
		body, err := os.ReadFile(filepath.Join(root, doc))
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(body), "\n") {
			if m := goLineCiteRe.FindString(line); m != "" {
				t.Errorf("%s:%d cites Go code by line number (%q): %s\n"+
					"\tline numbers drift silently on every insertion. Cite the enclosing\n"+
					"\tsymbol instead — `path.go::Symbol` for the breakdown, which GOV9 then\n"+
					"\tholds to resolving.", doc, i+1, m, strings.TrimSpace(line))
			}
		}
	}
}
