package eino

// L-5: ADR-0025 decision point 3 claims the discovery/catalog boundary is
// "kept up by the absence of an import relationship" between
// discovery*.go (W-C-03/W-C-05/W-C-06/W-C-15) and modelcatalog.go /
// contextwindow.go / pricing.go (ADR-0024's compiled-in model catalog and
// its two derived views). That claim does not describe a mechanism: all
// seven files share `package eino`. A same-package reference to a
// package-scope name never needs an import statement — there is nothing to
// import, so "no import relationship" was true by construction and would
// stay true even if discovery.go started calling ResolveContextWindow
// directly. Nothing enforced the boundary and nothing would have caught it
// breaking; the review (C3) named this the architecturally most important
// Low of the batch. See docs/adr/0025-discovery-is-a-read-only-cache-not-a-catalog-writer.md,
// corrected in the same commit that adds this file, for the wording fix
// this test is the mechanism for.
//
// The gate: collect every package-scope identifier the three catalog files
// declare (types, vars, consts, and plain — non-method — top-level funcs;
// methods are deliberately excluded, see catalogTopLevelIdentifiers's own
// comment), then walk every identifier the four discovery files reference
// and fail if any of them names a catalog identifier. This is a name-level
// heuristic, not a type-resolved one — it cannot tell "calls
// ResolveContextWindow" apart from "coincidentally declares an unrelated
// local variable also named ResolveContextWindow" — but the catalog names
// are distinctive enough (ModelPricing, contextWindowCatalog,
// mustParseModelCatalog, ...) that this is not a real risk in practice: the
// test below (TestGOV_DiscoveryDoesNotReferenceCatalog) passes clean
// against the actual current trees of both file sets, and a
// positive-control probe (temporarily adding a real cross-reference, run
// manually during this batch's remediation, restored via cp — never git
// checkout/stash, per this repo's mutation-probe discipline) confirmed the
// gate goes red when the boundary is actually crossed.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// catalogBoundaryFiles are ADR-0025's catalog side: the compiled-in model
// catalog (modelcatalog.go) and its two derived views (contextwindow.go,
// pricing.go).
var catalogBoundaryFiles = []string{"modelcatalog.go", "contextwindow.go", "pricing.go"}

// discoveryBoundaryFiles are ADR-0025's discovery side: the four files
// implementing W-C-03/W-C-05/W-C-06/W-C-15.
var discoveryBoundaryFiles = []string{"discovery.go", "discovery_ollama.go", "discovery_lmstudio.go", "discovery_cache.go"}

// catalogIdent records where a catalog file declares a package-scope name,
// for the gate's failure message.
type catalogIdent struct {
	file string
	line int
}

// catalogTopLevelIdentifiers parses each of catalogBoundaryFiles and
// collects every name it declares at package scope: type, var and const
// specs, and plain (non-method) top-level funcs.
//
// Method names (e.g. Ledger's Add and Cost, in pricing.go) are deliberately
// excluded: a method is never referenced by a bare identifier — only via a
// selector on some value (ledger.Add(...)) — so including "Add" in the
// catalog set would make an unrelated plain top-level func the discovery
// side later declares named Add (no relation to Ledger.Add — package-level
// funcs and methods live in different namespaces, so that declaration
// compiles fine) register as a false "reference to the catalog" the moment
// its own *ast.FuncDecl.Name identifier gets walked. Restricting the
// catalog set to package-scope names sidesteps that whole class of false
// positive, at the (accepted) cost of not policing a discovery file that
// somehow obtains a *Ledger or ModelPricing value and calls a method on it
// — that path is already caught by the type name itself (Ledger,
// ModelPricing) appearing at the point the value was constructed or typed.
func catalogTopLevelIdentifiers(t *testing.T) map[string]catalogIdent {
	t.Helper()
	fset := token.NewFileSet()
	idents := map[string]catalogIdent{}
	record := func(name string, pos token.Position) {
		if name == "_" {
			return
		}
		if _, exists := idents[name]; !exists {
			idents[name] = catalogIdent{file: pos.Filename, line: pos.Line}
		}
	}
	for _, path := range catalogBoundaryFiles {
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		for _, decl := range f.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				if d.Recv != nil {
					continue // method — see the doc comment above
				}
				record(d.Name.Name, fset.Position(d.Name.Pos()))
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					switch s := spec.(type) {
					case *ast.TypeSpec:
						record(s.Name.Name, fset.Position(s.Name.Pos()))
					case *ast.ValueSpec:
						for _, n := range s.Names {
							record(n.Name, fset.Position(n.Pos()))
						}
					}
				}
			}
		}
	}
	return idents
}

// discoveryIdentUse is one occurrence of an identifier in a discovery file.
type discoveryIdentUse struct {
	name string
	pos  token.Position
}

// discoveryFileIdentifiers parses path and returns every *ast.Ident node's
// name and position — every identifier the file mentions anywhere:
// declarations, parameters, locals, calls, type references, field
// accesses. The gate does not attempt to separate "declares a new local
// name" from "references an existing one" (see catalogTopLevelIdentifiers's
// comment on why that distinction doesn't matter here in practice for this
// specific, distinctively-named catalog set).
func discoveryFileIdentifiers(t *testing.T, path string) []discoveryIdentUse {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	var uses []discoveryIdentUse
	ast.Inspect(f, func(n ast.Node) bool {
		id, ok := n.(*ast.Ident)
		if !ok || id.Name == "_" {
			return true
		}
		uses = append(uses, discoveryIdentUse{name: id.Name, pos: fset.Position(id.Pos())})
		return true
	})
	return uses
}

// TestGOV_DiscoveryDoesNotReferenceCatalog is ADR-0025 decision point 3's
// enforcement mechanism (L-5): no identifier declared by the catalog files
// may appear anywhere in the discovery files. See the package-level doc
// comment above for the full rationale and the heuristic's known shape.
func TestGOV_DiscoveryDoesNotReferenceCatalog(t *testing.T) {
	catalog := catalogTopLevelIdentifiers(t)
	if len(catalog) == 0 {
		t.Fatal("catalogTopLevelIdentifiers returned nothing — the parser found no declarations at all, which means this test isn't actually checking anything; that's a bug in the gate, not a clean bill of health")
	}
	for _, path := range discoveryBoundaryFiles {
		for _, use := range discoveryFileIdentifiers(t, path) {
			decl, isCatalogName := catalog[use.name]
			if !isCatalogName {
				continue
			}
			t.Errorf("%s:%d: references %q, a same-package identifier declared by the catalog (%s:%d) — ADR-0025 forbids discovery from depending on the catalog; if this is a genuine new cross-reference, it needs a new ADR arguing why decision point 4 (\"人工桥接是唯一允许的桥接\") should be overturned, not a code change alone",
				use.pos.Filename, use.pos.Line, use.name, decl.file, decl.line)
		}
	}
}
