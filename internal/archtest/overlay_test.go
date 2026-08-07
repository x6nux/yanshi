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
	"sdk_endpoints_test.go",
	"bench_test.go",
	"ctxinject_test.go",
	"deps_test.go",
	"docs_test.go",
	"docsymbols_ablation_test.go",
	"docsymbols_test.go",
	"helpers_test.go",
	"lines_test.go",
	"overlay_test.go",
	"release_docs_test.go",
	"removal_test.go",
	"sdkci_test.go",
	"slashcmd_test.go",
	"status_evidence_test.go",
	"status_test.go",
	"toolcontract_test.go",
}

// runtimeDiskReads are the standard-library calls that read bytes off the
// filesystem (or hand the job to a subprocess) while the test binary runs.
// Values are "<pkg>.<Func>" as they appear at the call site.
//
// exec.Command is included because buildImportGraph shells out to `go list`:
// a subprocess inherits none of the parent's -overlay mapping, which makes it
// the most overlay-opaque read of all.
// os.OpenFile, os.DirFS and the io/fs walkers are here because leaving them
// out was a hole with a name: os.Open, os.Stat and os.Lstat were all listed
// while their sibling os.OpenFile was not, and `os.DirFS(root)` handed to
// fs.WalkDir reads exactly what filepath.WalkDir reads through an interface
// the table could not see. All three were measured to pass unnoticed.
var runtimeDiskReads = map[string]bool{
	"os.ReadFile":           true,
	"os.ReadDir":            true,
	"os.Open":               true,
	"os.OpenFile":           true,
	"os.Stat":               true,
	"os.Lstat":              true,
	"os.DirFS":              true,
	"os.Readlink":           true,
	"fs.WalkDir":            true,
	"fs.ReadFile":           true,
	"fs.ReadDir":            true,
	"fs.Glob":               true,
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

	// funcs maps a name to every declaration carrying it; owner maps a name to
	// the file that declares it. METHODS ARE INCLUDED, keyed by their bare
	// method name, which is why funcs holds a slice: a method may share a name
	// with a package-level function.
	//
	// Skipping receivers (the earlier shape) was the widest of the three holes
	// this gate had. A helper that reads the repo is just as opaque to -overlay
	// when it hangs off a type, and moving one there silently dropped its file
	// out of the mirror — the gate would then advise a reviewer to probe it with
	// -overlay, which is the direction of error the gate exists to prevent.
	funcs := map[string][]*ast.FuncDecl{}
	owner := map[string]string{}
	for _, pkg := range pkgs {
		for path, file := range pkg.Files {
			base := filepath.Base(path)
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Body == nil {
					continue
				}
				funcs[fn.Name.Name] = append(funcs[fn.Name.Name], fn)
				if fn.Recv == nil {
					owner[fn.Name.Name] = base
				}
			}
		}
	}

	reads := map[string]bool{}
	calls := map[string][]string{}
	for name, decls := range funcs {
		for _, fn := range decls {
			direct, callees := scanFuncBody(fn)
			reads[name] = reads[name] || direct
			calls[name] = append(calls[name], callees...)
		}
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
// up in runtimeDiskReads. A qualified call that is not in that table is not
// assumed to read disk — the table is an allow/deny list the maintainer
// curates, and guessing there would make the mirror unstable.
//
// A qualified call IS however recorded as a callee under its bare selector
// name, so `x.readsTheRepo()` links to a method declared in this package. That
// over-approximates (a call to t.Helper() looks for a local "Helper"), but only
// in the safe direction: a spurious edge can add a file to the mirror, i.e.
// advise a worktree probe where overlay would have worked, whereas a missing
// edge produces a silent false PASS.
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
			if pkgIdent, ok := f.X.(*ast.Ident); ok &&
				runtimeDiskReads[pkgIdent.Name+"."+f.Sel.Name] {
				direct = true
			}
			if !seen[f.Sel.Name] {
				seen[f.Sel.Name] = true
				callees = append(callees, f.Sel.Name)
			}
		}
		return true
	})
	return direct, callees
}
