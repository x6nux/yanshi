// Package archtest provides zero-dependency test helpers for architecture
// governance tests (GOV1/GOV2/GOV3). All files in this package are _test.go
// files and live in package archtest.
//
// The helpers rely exclusively on the standard library (go/parser, go/token,
// go/ast, encoding/json, os/exec) to avoid import-cycle risk with the
// internal packages they are designed to test.
package archtest

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
)

// ---------------------------------------------------------------------------
// Module root and path helpers
// ---------------------------------------------------------------------------

var (
	moduleRootOnce sync.Once
	moduleRootVal  string
)

// mustModuleRoot returns the absolute path of the directory containing go.mod
// by walking up from the working directory. It panics on failure and is safe
// for use in package-level var initialization (no *testing.T parameter).
func mustModuleRoot() string {
	moduleRootOnce.Do(func() {
		dir, err := os.Getwd()
		if err != nil {
			panic(err)
		}
		for {
			if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
				moduleRootVal = dir
				return
			}
			parent := filepath.Dir(dir)
			if parent == dir {
				panic("yanshi: go.mod not found from " + dir)
			}
			dir = parent
		}
	})
	return moduleRootVal
}

// moduleRoot returns the absolute path to the module root (the directory
// that contains go.mod).
func moduleRoot(t *testing.T) string {
	t.Helper()
	return mustModuleRoot()
}

// modulePath reads go.mod and returns the module path declared on the
// "module <path>" line (e.g. "github.com/x6nux/yanshi").
func modulePath(t *testing.T) string {
	t.Helper()
	root := moduleRoot(t)
	data, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "module ") {
			return strings.TrimSpace(line[len("module "):])
		}
	}
	t.Fatal("module directive not found in go.mod")
	return ""
}

// ---------------------------------------------------------------------------
// Import graph construction
// ---------------------------------------------------------------------------

// pkgJSON mirrors the fields of go list -json that we need for building an
// import graph.
type pkgJSON struct {
	ImportPath string   `json:"ImportPath"`
	Imports    []string `json:"Imports"`
}

// buildImportGraph runs "go list -json -deps ./..." and streams the output
// through a json.Decoder. It returns a map from every module-internal
// package path to its sorted list of direct internal dependencies.
//
// External dependencies are discarded; only packages whose ImportPath starts
// with the module path (or equals the module path) are retained.
func buildImportGraph(t *testing.T) map[string][]string {
	t.Helper()
	mp := modulePath(t)
	root := moduleRoot(t)

	cmd := exec.Command("go", "list", "-json", "-deps", "./...")
	cmd.Dir = root

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	graph := make(map[string][]string)
	dec := json.NewDecoder(stdout)
	for {
		var pkg pkgJSON
		if err := dec.Decode(&pkg); err != nil {
			if err == io.EOF {
				break
			}
			t.Fatal(err)
		}
		if !isModulePkg(pkg.ImportPath, mp) {
			continue
		}
		var deps []string
		for _, imp := range pkg.Imports {
			if isModulePkg(imp, mp) {
				deps = append(deps, imp)
			}
		}
		sort.Strings(deps)
		graph[pkg.ImportPath] = deps
	}

	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	return graph
}

// isModulePkg reports whether pkg belongs to the module identified by mp.
func isModulePkg(pkg, mp string) bool {
	return pkg == mp || strings.HasPrefix(pkg, mp+"/")
}

// closureInternalDeps returns the transitive closure of internal module
// dependencies for pkg, excluding pkg itself.
func closureInternalDeps(graph map[string][]string, pkg string) map[string]struct{} {
	deps := make(map[string]struct{})
	var visit func(string)
	visit = func(p string) {
		for _, dep := range graph[p] {
			if _, ok := deps[dep]; !ok {
				deps[dep] = struct{}{}
				visit(dep)
			}
		}
	}
	visit(pkg)
	delete(deps, pkg)
	return deps
}

// ---------------------------------------------------------------------------
// Pure code line counting
// ---------------------------------------------------------------------------

// pureCodeLines counts the number of lines in the Go file at path that
// contain at least one non-whitespace byte that is outside any comment span.
//
// The function uses the byte-span approach:
//  1. parse with parser.ParseComments to collect comment intervals [start,end)
//  2. for each line (via token.File.LineStart), scan its bytes
//  3. a line counts if any non-whitespace byte is not inside a comment span
//
// This correctly handles // line comments, /* */ block comments, line-end
// comments, and multi-line block comments. Blank lines and pure-comment
// lines are excluded.
func pureCodeLines(t *testing.T, path string) int {
	t.Helper()
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, src, parser.ParseComments)
	if err != nil {
		t.Fatal(err)
	}

	// Collect sorted comment byte intervals [start, end).
	type span struct{ start, end int }
	var comments []span
	for _, cg := range f.Comments {
		for _, c := range cg.List {
			start := fset.Position(c.Pos()).Offset
			end := fset.Position(c.End()).Offset
			comments = append(comments, span{start, end})
		}
	}
	sort.Slice(comments, func(i, j int) bool {
		return comments[i].start < comments[j].start
	})

	// inComment checks whether a byte offset falls inside any comment span.
	inComment := func(pos int) bool {
		for _, c := range comments {
			if pos >= c.start && pos < c.end {
				return true
			}
			if pos < c.start {
				return false
			}
		}
		return false
	}

	tf := fset.File(f.Pos())
	count := 0
	for line := 1; line <= tf.LineCount(); line++ {
		start := tf.Offset(tf.LineStart(line))
		var end int
		if line == tf.LineCount() {
			end = tf.Size()
		} else {
			end = tf.Offset(tf.LineStart(line + 1))
		}

		hasCode := false
		for pos := start; pos < end && pos < len(src); pos++ {
			ch := src[pos]
			if ch != ' ' && ch != '\t' && ch != '\n' && ch != '\r' {
				if !inComment(pos) {
					hasCode = true
					break
				}
			}
		}
		if hasCode {
			count++
		}
	}
	return count
}

// ---------------------------------------------------------------------------
// File discovery
// ---------------------------------------------------------------------------

// goFiles recursively finds all non-test .go files under the specified root
// directories. Results are sorted alphabetically.
func goFiles(t *testing.T, roots ...string) []string {
	t.Helper()
	var files []string
	for _, root := range roots {
		err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return nil // continue walking
			}
			if strings.HasSuffix(path, "_test.go") {
				return nil
			}
			if strings.HasSuffix(path, ".go") {
				files = append(files, path)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	sort.Strings(files)
	return files
}

// ---------------------------------------------------------------------------
// Exported declaration scanner
// ---------------------------------------------------------------------------

// exportedDecl describes a single exported declaration in a Go source file.
type exportedDecl struct {
	File   string
	Line   int
	Kind   string // "func", "type", "var", "const", or "method"
	Name   string
	HasDoc bool
}

// scanExported parses a Go source file with parser.ParseComments and returns
// all exported symbols: package-level functions and methods on exported
// types, as well as exported types, variables, and constants.
//
// Doc attribution: for FuncDecl, HasDoc reflects d.Doc != nil. For GenDecl
// items (types, vars, consts), HasDoc is true when either the spec itself
// has a doc comment or the enclosing GenDecl has one.
func scanExported(t *testing.T, path string) []exportedDecl {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
	if err != nil {
		t.Fatal(err)
	}

	// First pass: collect exported type names so we can filter methods.
	exportedTypes := make(map[string]bool)
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.TYPE {
			continue
		}
		for _, spec := range gd.Specs {
			ts, ok := spec.(*ast.TypeSpec)
			if !ok || !ts.Name.IsExported() {
				continue
			}
			exportedTypes[ts.Name.Name] = true
		}
	}

	// Second pass: collect exported declarations.
	var results []exportedDecl
	for _, decl := range f.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			if !d.Name.IsExported() {
				continue
			}
			kind := "func"
			if d.Recv != nil {
				recvName := recvTypeName(d.Recv)
				if !exportedTypes[recvName] {
					continue
				}
				kind = "method"
			}
			pos := fset.Position(d.Pos())
			results = append(results, exportedDecl{
				File:   path,
				Line:   pos.Line,
				Kind:   kind,
				Name:   d.Name.Name,
				HasDoc: d.Doc != nil,
			})

		case *ast.GenDecl:
			switch d.Tok {
			case token.TYPE:
				for _, spec := range d.Specs {
					ts := spec.(*ast.TypeSpec)
					if !ts.Name.IsExported() {
						continue
					}
					pos := fset.Position(ts.Pos())
					hasDoc := ts.Doc != nil || d.Doc != nil
					results = append(results, exportedDecl{
						File:   path,
						Line:   pos.Line,
						Kind:   "type",
						Name:   ts.Name.Name,
						HasDoc: hasDoc,
					})
				}

			case token.VAR, token.CONST:
				kind := "var"
				if d.Tok == token.CONST {
					kind = "const"
				}
				for _, spec := range d.Specs {
					vs := spec.(*ast.ValueSpec)
					for _, name := range vs.Names {
						if !name.IsExported() {
							continue
						}
						pos := fset.Position(name.Pos())
						hasDoc := vs.Doc != nil || d.Doc != nil
						results = append(results, exportedDecl{
							File:   path,
							Line:   pos.Line,
							Kind:   kind,
							Name:   name.Name,
							HasDoc: hasDoc,
						})
					}
				}
			}
		}
	}
	return results
}

// recvTypeName extracts the base type name from a receiver field list.
// It handles plain types (*ast.Ident) and pointer types (*ast.StarExpr)
// and returns "" for any other shape.
func recvTypeName(recv *ast.FieldList) string {
	if recv == nil || len(recv.List) == 0 {
		return ""
	}
	switch t := recv.List[0].Type.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.StarExpr:
		if ident, ok := t.X.(*ast.Ident); ok {
			return ident.Name
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// Temporary module for self-tests
// ---------------------------------------------------------------------------

// withSyntheticModule creates a temporary directory containing a minimal
// go.mod ("module example.com/synth") and the provided .go files. The caller
// passes a map from relative file path (e.g. "x.go") to file content.
//
// The directory is automatically cleaned up by testing.T.TempDir. Returns
// the temp directory path.
func withSyntheticModule(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()

	goMod := "module example.com/synth\n\ngo 1.26\n"
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(goMod), 0644); err != nil {
		t.Fatal(err)
	}
	for name, content := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// ---------------------------------------------------------------------------
// Path manipulation helpers (used at package level, no *testing.T)
// ---------------------------------------------------------------------------

// abs returns an absolute path formed by joining rel to the module root.
// It panics if the module root cannot be determined.
func abs(rel string) string {
	return filepath.Join(mustModuleRoot(), rel)
}

// short returns the /-separated relative path from root to path. If path
// cannot be made relative, it is returned unchanged.
func short(path, root string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return path
	}
	return strings.ReplaceAll(rel, string(filepath.Separator), "/")
}

// ---------------------------------------------------------------------------
// Self-tests
// ---------------------------------------------------------------------------

// TestPureCodeLinesCountsExactly creates a synthetic module with a known
// fixture containing 8 pure code lines and verifies that pureCodeLines
// returns exactly 8.
//
// The fixture has these "pure" lines (non-empty, non-comment):
//
//	1:  package x
//	2:  func f1() {}
//	3:  func f2() {
//	4:      x := 1
//	5:  }
//	6:  func f3() {} // hi          (code exists before line-end comment)
//	7:  func f4() {}
//	8:  func f5() int { return 3 }
func TestPureCodeLinesCountsExactly(t *testing.T) {
	dir := withSyntheticModule(t, map[string]string{
		"x.go": `package x

// comment 1
func f1() {}
func f2() {
    x := 1
}
/* comment 2 */
func f3() {} // hi
func f4() {}
func f5() int { return 3 }`,
	})
	n := pureCodeLines(t, filepath.Join(dir, "x.go"))
	if n != 8 {
		t.Fatalf("pureCodeLines = %d, want 8", n)
	}
}

// TestModuleRootFindsGoMod verifies that moduleRoot returns an absolute path
// to a directory that actually contains go.mod.
func TestModuleRootFindsGoMod(t *testing.T) {
	root := moduleRoot(t)
	if !filepath.IsAbs(root) {
		t.Fatal("moduleRoot returned non-absolute path")
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("moduleRoot returned %q, which does not contain go.mod: %v", root, err)
	}
}

// TestBuildImportGraphRuns verifies that buildImportGraph produces a non-empty
// graph containing the internal/guard package.
func TestBuildImportGraphRuns(t *testing.T) {
	graph := buildImportGraph(t)
	if len(graph) == 0 {
		t.Fatal("buildImportGraph returned empty graph")
	}
	guardPath := modulePath(t) + "/internal/guard"
	if _, ok := graph[guardPath]; !ok {
		t.Fatalf("buildImportGraph result missing expected key %q; keys=%v", guardPath, mapKeys(graph))
	}
}

// mapKeys returns the string keys of m for diagnostic output.
func mapKeys(m map[string][]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
