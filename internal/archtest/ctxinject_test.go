// Package archtest — GOV6 context injection closure.
//
// A context injector (func With<X>(ctx, ...) context.Context) with no
// production call site means the value is never bound, so every consumer
// downstream silently reads the zero value. The 2026-07-31 audit found
// registry.WithRole in exactly this state: the whole consumer chain
// (PromptPrefix, RolePolicy, output contract) existed and was tested, but
// ran against an empty role forever. See spec §4.2 GOV6.
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

// ctxInjectExceptions maps "<module-relative pkg>.<Func>" to the work package
// that will add the missing production call site.
//
// Entries may only be REMOVED, never added. A dead entry — the injector now
// has a production call site — fails the test.
var ctxInjectExceptions = map[string]string{}

// ctxInjector is one exported context-injecting function declaration.
type ctxInjector struct {
	Pkg  string // module-relative package dir, e.g. "internal/agent/registry"
	Name string
	Loc  string // "file:line", module-relative
}

func (c ctxInjector) key() string { return c.Pkg + "." + c.Name }

// isContextContext reports whether e is the type expression context.Context.
func isContextContext(e ast.Expr) bool {
	sel, ok := e.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	id, ok := sel.X.(*ast.Ident)
	return ok && id.Name == "context" && sel.Sel.Name == "Context"
}

// pkgDirOf returns the module-relative directory of a file path.
func pkgDirOf(path, root string) string {
	return filepath.ToSlash(filepath.Dir(short(path, root)))
}

// findCtxInjectors scans internal/ for exported functions whose signature is
// func With<X>(ctx context.Context, ...) context.Context.
func findCtxInjectors(t *testing.T) []ctxInjector {
	t.Helper()
	root := moduleRoot(t)
	files := goFiles(t, filepath.Join(root, "internal"))

	var out []ctxInjector
	fset := token.NewFileSet()
	for _, path := range files {
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Recv != nil || !fd.Name.IsExported() {
				continue
			}
			if !strings.HasPrefix(fd.Name.Name, "With") {
				continue
			}
			// Exactly one result, of type context.Context.
			if fd.Type.Results == nil || len(fd.Type.Results.List) != 1 {
				continue
			}
			if !isContextContext(fd.Type.Results.List[0].Type) {
				continue
			}
			// First parameter must be context.Context.
			if fd.Type.Params == nil || len(fd.Type.Params.List) == 0 {
				continue
			}
			if !isContextContext(fd.Type.Params.List[0].Type) {
				continue
			}
			pos := fset.Position(fd.Name.Pos())
			out = append(out, ctxInjector{
				Pkg:  pkgDirOf(path, root),
				Name: fd.Name.Name,
				Loc:  short(pos.Filename, root) + ":" + strconv.Itoa(pos.Line),
			})
		}
	}
	return out
}

// findCtxInjectorCalls scans all non-test .go under internal/ and cmd/ and
// returns the set of "<pkg>.<Func>" keys that are actually CALLED.
//
// Qualified calls (pkg.WithX) resolve the selector's package alias back to a
// full import path via the file's import list, then to a module-relative dir.
// Matching on the bare function name would let a same-named function in a
// different package mask a real violation.
func findCtxInjectorCalls(t *testing.T) map[string]bool {
	t.Helper()
	root := moduleRoot(t)
	mp := modulePath(t)
	files := goFiles(t,
		filepath.Join(root, "internal"),
		filepath.Join(root, "cmd"),
	)

	called := make(map[string]bool)
	fset := token.NewFileSet()
	for _, path := range files {
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		selfPkg := pkgDirOf(path, root)

		// alias -> module-relative package dir, for this file's imports.
		aliases := make(map[string]string)
		for _, imp := range f.Imports {
			ipath := strings.Trim(imp.Path.Value, `"`)
			if !strings.HasPrefix(ipath, mp+"/") {
				continue // not a module-internal import
			}
			rel := strings.TrimPrefix(ipath, mp+"/")
			name := filepath.Base(rel)
			if imp.Name != nil {
				name = imp.Name.Name
			}
			aliases[name] = rel
		}

		ast.Inspect(f, func(n ast.Node) bool {
			ce, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			switch fn := ce.Fun.(type) {
			case *ast.Ident: // same-package call: WithX(...)
				if strings.HasPrefix(fn.Name, "With") {
					called[selfPkg+"."+fn.Name] = true
				}
			case *ast.SelectorExpr: // qualified call: pkg.WithX(...)
				id, ok := fn.X.(*ast.Ident)
				if !ok || !strings.HasPrefix(fn.Sel.Name, "With") {
					return true
				}
				if dir, ok := aliases[id.Name]; ok {
					called[dir+"."+fn.Sel.Name] = true
				}
			}
			return true
		})
	}
	return called
}

// TestGOV6ContextInjectorsHaveCallSites verifies every exported context
// injector under internal/ is actually called from non-test code.
func TestGOV6ContextInjectorsHaveCallSites(t *testing.T) {
	injectors := findCtxInjectors(t)
	if len(injectors) < 10 {
		t.Fatalf("GOV6: only %d context injectors found — the scanner is almost "+
			"certainly broken (the repo has dozens)", len(injectors))
	}
	called := findCtxInjectorCalls(t)

	var orphans []string
	for _, inj := range injectors {
		k := inj.key()
		if called[k] {
			continue
		}
		if _, exempt := ctxInjectExceptions[k]; exempt {
			continue
		}
		orphans = append(orphans, k+"  ("+inj.Loc+")")
	}
	sort.Strings(orphans)
	if len(orphans) > 0 {
		t.Errorf("GOV6: %d context injector(s) have no production call site — the "+
			"bound value is never set, so every consumer reads the zero value:\n  %s\n\n"+
			"Fix: add the missing call, or delete the injector if it is dead. If the\n"+
			"wiring is deferred, add an entry to ctxInjectExceptions naming the work package.",
			len(orphans), strings.Join(orphans, "\n  "))
	}

	// Dead-entry check: an exempted injector that now has a call site has
	// been wired up, so its exemption must be deleted.
	var dead []string
	for k := range ctxInjectExceptions {
		if called[k] {
			dead = append(dead, k)
		}
	}
	sort.Strings(dead)
	if len(dead) > 0 {
		t.Errorf("GOV6: %d stale ctxInjectExceptions entr(ies) — these injectors now "+
			"have production call sites and their exemptions must be DELETED:\n  %s",
			len(dead), strings.Join(dead, "\n  "))
	}
}
