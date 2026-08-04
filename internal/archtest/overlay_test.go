package archtest

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// Overlay-immunity boundary (review-checklist.md, section A)
// ---------------------------------------------------------------------------

// overlayImmuneGateFiles mirrors the claim docs/superpowers/review-checklist.md
// makes in section A: every gate in this package reads the repository from
// disk AT RUNTIME, so `go test -overlay` cannot be used to probe it.
//
// The reason that claim needs a machine check is that its failure mode is
// silent. `-overlay` only changes which source files the GO COMMAND compiles;
// it does not change the bytes a running test reads back with os.ReadFile /
// parser.ParseFile / filepath.WalkDir, and it does not propagate into the
// `go list` subprocess buildImportGraph spawns. A reviewer who guts a symbol
// through an overlay and probes one of these gates gets a PASS that means
// nothing — which is exactly how one review round produced two blockers for
// defects that did not exist.
//
// This is a MIRROR, not a debt table: both directions fail.
//   - An entry here whose gate no longer reaches a runtime disk read is dead,
//     and the checklist's blanket "all of internal/archtest" would then be
//     too wide. Delete the entry and narrow the checklist together.
//   - A new gate file that DOES reach a runtime disk read but is missing here
//     is the dangerous direction: the checklist would silently stop covering
//     it. Add it here and keep the checklist's list honest.
//
// The list is not limited to the numbered GOV gates: helpers_test.go carries
// self-tests for moduleRoot/buildImportGraph/pureCodeLines, and this file
// carries the mirror itself. Both read the repo, so both belong here — the
// rule is "declares a Test that reaches a disk read", not "is a GOV gate".
var overlayImmuneGateFiles = []string{
	"acceptance_pin_test.go",
	"assembly_test.go",
	"ctxinject_test.go",
	"deps_test.go",
	"docs_test.go",
	"docsymbols_ablation_test.go",
	"docsymbols_test.go",
	"helpers_test.go",
	"lines_test.go",
	"overlay_test.go",
	"removal_test.go",
	"slashcmd_test.go",
	"status_evidence_test.go",
	"status_test.go",
}

// runtimeDiskReads are the standard-library calls that read bytes off the
// filesystem (or hand the job to a subprocess) while the test binary runs.
// Values are "<pkg>.<Func>" as they appear at the call site.
//
// exec.Command is included because buildImportGraph shells out to `go list`:
// a subprocess inherits none of the parent's -overlay mapping, which makes it
// the most overlay-opaque read of all.
var runtimeDiskReads = map[string]bool{
	"os.ReadFile":           true,
	"os.ReadDir":            true,
	"os.Open":               true,
	"os.Stat":               true,
	"os.Lstat":              true,
	"filepath.Walk":         true,
	"filepath.WalkDir":      true,
	"filepath.Glob":         true,
	"filepath.EvalSymlinks": true,
	"parser.ParseFile":      true,
	"parser.ParseDir":       true,
	"exec.Command":          true,
	"exec.CommandContext":   true,
}

// TestGateFilesReadFromDiskAtRuntime turns "go test -overlay is ineffective
// against internal/archtest" from a fact a reviewer has to remember into one
// the build checks.
//
// It parses this package's own test files, marks every function that calls a
// runtime disk read, propagates that mark backwards through the intra-package
// call graph to a fixpoint, and then reports which files declare a Test that
// reaches a read. That set must equal overlayImmuneGateFiles exactly.
func TestGateFilesReadFromDiskAtRuntime(t *testing.T) {
	dir := filepath.Join(moduleRoot(t), "internal", "archtest")
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, dir, nil, 0)
	if err != nil {
		t.Fatalf("ParseDir(%s): %v", dir, err)
	}

	// funcs maps a function name to its declaration; owner maps it to the
	// base name of the file that declares it. Archtest test helpers are all
	// package-level functions without receivers, so a name is a unique key.
	funcs := map[string]*ast.FuncDecl{}
	owner := map[string]string{}
	for _, pkg := range pkgs {
		for path, file := range pkg.Files {
			base := filepath.Base(path)
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Recv != nil || fn.Body == nil {
					continue
				}
				funcs[fn.Name.Name] = fn
				owner[fn.Name.Name] = base
			}
		}
	}

	reads := map[string]bool{}
	calls := map[string][]string{}
	for name, fn := range funcs {
		direct, callees := scanFuncBody(fn)
		reads[name] = direct
		calls[name] = callees
	}

	// Backward fixpoint: a function touches disk if it reads directly or
	// calls something in this package that does. Iterating to stability is
	// what lets deps_test.go inherit buildImportGraph's `go list` subprocess
	// and lines_test.go inherit mustModuleRoot's os.Stat walk.
	for changed := true; changed; {
		changed = false
		for name := range funcs {
			if reads[name] {
				continue
			}
			for _, callee := range calls[name] {
				if reads[callee] {
					reads[name] = true
					changed = true
					break
				}
			}
		}
	}

	got := map[string]bool{}
	for name := range funcs {
		if strings.HasPrefix(name, "Test") && reads[name] {
			got[owner[name]] = true
		}
	}

	want := map[string]bool{}
	for _, f := range overlayImmuneGateFiles {
		want[f] = true
	}

	var missing, dead []string
	for f := range got {
		if !want[f] {
			missing = append(missing, f)
		}
	}
	for f := range want {
		if !got[f] {
			dead = append(dead, f)
		}
	}
	sort.Strings(missing)
	sort.Strings(dead)

	if len(missing) > 0 {
		t.Errorf("these gate files reach a runtime disk read but are not listed in "+
			"overlayImmuneGateFiles: %v\n"+
			"A reviewer probing them with `go test -overlay` gets a silent false PASS. "+
			"Add them here and to the overlay-immunity table in "+
			"docs/superpowers/review-checklist.md.", missing)
	}
	if len(dead) > 0 {
		t.Errorf("these entries in overlayImmuneGateFiles no longer reach a runtime disk "+
			"read (or no longer exist): %v\n"+
			"The checklist's blanket 'all of internal/archtest is overlay-immune' is now "+
			"too wide. Delete the entry and narrow the checklist in the same change.", dead)
	}
}

// scanFuncBody reports whether fn calls a runtime disk read directly, plus the
// names of the package-level functions it calls (for the backward fixpoint).
//
// Unqualified calls are recorded by name; qualified ones (pkg.Func) are looked
// up in runtimeDiskReads. A qualified call that is not in that table is
// ignored rather than assumed to read disk — the table is an allow/deny list
// the maintainer curates, and guessing in either direction would make the
// mirror unstable.
func scanFuncBody(fn *ast.FuncDecl) (direct bool, callees []string) {
	seen := map[string]bool{}
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch f := call.Fun.(type) {
		case *ast.Ident:
			if !seen[f.Name] {
				seen[f.Name] = true
				callees = append(callees, f.Name)
			}
		case *ast.SelectorExpr:
			pkgIdent, ok := f.X.(*ast.Ident)
			if !ok {
				return true
			}
			if runtimeDiskReads[pkgIdent.Name+"."+f.Sel.Name] {
				direct = true
			}
		}
		return true
	})
	return direct, callees
}
