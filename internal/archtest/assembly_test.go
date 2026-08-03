// Package archtest — GOV4 assembly reachability.
//
// GOV4 catches the repo's dominant failure mode: a component package is
// written, tested, and green, but never wired into the composition root, so
// it is dead code at runtime. The 2026-07-31 audit found this pattern behind
// 53% of "partially implemented" features. See
// docs/superpowers/specs/2026-08-03-yanshi-roadmap-design.md §4.2.
package archtest

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// assemblyExceptions maps an exported Build* function in internal/bootstrap
// to the work package that will wire it into Build.
//
// Exempted functions are treated as ADDITIONAL BFS ROOTS, not as skipped
// nodes. That is deliberate: exempting BuildC1 makes BuildRLM and
// BuildAutomation reachable through it, so W1's single fix (calling BuildC1
// from Build) turns all three green in one commit.
//
// Entries may only be REMOVED, never added. A dead entry — the function is
// now reachable from Build without needing to be a root — fails the test.
var assemblyExceptions = map[string]string{}

// bootstrapCallGraph parses every non-test .go file in internal/bootstrap and
// returns (same-package call graph, exported Build* name → "file:line").
//
// Only unqualified calls (*ast.Ident) are edges: a call like foo() is a
// same-package call, whereas pkg.Foo() is not and cannot reach a local
// Build* function.
func bootstrapCallGraph(t *testing.T) (map[string]map[string]bool, map[string]string) {
	t.Helper()
	root := moduleRoot(t)
	files := goFiles(t, filepath.Join(root, "internal", "bootstrap"))

	graph := make(map[string]map[string]bool)
	builds := make(map[string]string)

	fset := token.NewFileSet()
	for _, path := range files {
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Recv != nil {
				continue // methods are outside the Build* contract
			}
			name := fd.Name.Name
			if fd.Name.IsExported() && strings.HasPrefix(name, "Build") {
				pos := fset.Position(fd.Name.Pos())
				builds[name] = short(pos.Filename, root) + ":" + strconv.Itoa(pos.Line)
			}
			callees := make(map[string]bool)
			if fd.Body != nil {
				ast.Inspect(fd.Body, func(n ast.Node) bool {
					if ce, ok := n.(*ast.CallExpr); ok {
						if id, ok := ce.Fun.(*ast.Ident); ok {
							callees[id.Name] = true
						}
					}
					return true
				})
			}
			graph[name] = callees
		}
	}
	return graph, builds
}

// reachableFrom returns the set of function names reachable from roots by
// following same-package call edges.
func reachableFrom(graph map[string]map[string]bool, roots []string) map[string]bool {
	seen := make(map[string]bool)
	queue := append([]string(nil), roots...)
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if seen[cur] {
			continue
		}
		seen[cur] = true
		for callee := range graph[cur] {
			if !seen[callee] {
				queue = append(queue, callee)
			}
		}
	}
	return seen
}

// TestGOV4BuildFunctionsReachable verifies every exported Build* function in
// internal/bootstrap is transitively reachable from Build — i.e. it is
// actually part of the assembly line rather than dead code.
func TestGOV4BuildFunctionsReachable(t *testing.T) {
	graph, builds := bootstrapCallGraph(t)

	if _, ok := builds["Build"]; !ok {
		t.Fatal("GOV4: composition root Build not found in internal/bootstrap — " +
			"the analyzer is looking at the wrong package")
	}

	roots := []string{"Build"}
	for name := range assemblyExceptions {
		roots = append(roots, name)
	}
	sort.Strings(roots)
	reachable := reachableFrom(graph, roots)

	var unreachable []string
	for name, loc := range builds {
		if !reachable[name] {
			unreachable = append(unreachable, name+"  ("+loc+")")
		}
	}
	sort.Strings(unreachable)
	if len(unreachable) > 0 {
		t.Errorf("GOV4: %d exported Build* function(s) in internal/bootstrap are "+
			"unreachable from Build — they are dead code at runtime:\n  %s\n\n"+
			"Fix: call them (directly or transitively) from bootstrap.Build. If the\n"+
			"wiring is deferred to a later work package, add an entry to\n"+
			"assemblyExceptions naming that package.",
			len(unreachable), strings.Join(unreachable, "\n  "))
	}

	// Dead-entry check: recompute reachability WITHOUT the exception roots.
	// An exempted function that is now reachable on its own has been wired
	// up, so its entry is stale and must be deleted.
	base := reachableFrom(graph, []string{"Build"})
	var dead []string
	for name := range assemblyExceptions {
		if base[name] {
			dead = append(dead, name)
		}
	}
	sort.Strings(dead)
	if len(dead) > 0 {
		t.Errorf("GOV4: %d stale assemblyExceptions entr(ies) — these functions are "+
			"now reachable from Build and their exemptions must be DELETED:\n  %s",
			len(dead), strings.Join(dead, "\n  "))
	}
}
