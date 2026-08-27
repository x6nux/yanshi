package archtest

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"testing"
)

// TestCodelinesLimitMatchesGate pins cmd/codelines' pureLineLimit to the gate's.
//
// codelines is the preflight everyone runs before touching a large file, but it
// cannot import the gate's constant: pureLineLimit is declared in lines_test.go,
// and _test.go files are not importable. So there are two copies of one number,
// and nothing in the compiler relates them.
//
// A drifted copy is worse than no preflight. Too low and it reports splits that
// the gate does not want, so people split cohesive files for nothing. Too high
// and it reports clean on a file the gate will redden — which is how a preflight
// stops being consulted at all. This test is the only thing that couples them.
//
// It reads the constant out of the source rather than linking against it, for
// the same reason: there is no link to make.
func TestCodelinesLimitMatchesGate(t *testing.T) {
	root := moduleRoot(t)
	path := filepath.Join(root, "cmd", "codelines", "main.go")

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}

	got, found := 0, false
	for _, decl := range file.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, name := range vs.Names {
				if name.Name != "pureLineLimit" || i >= len(vs.Values) {
					continue
				}
				lit, ok := vs.Values[i].(*ast.BasicLit)
				if !ok || lit.Kind != token.INT {
					t.Fatalf("cmd/codelines pureLineLimit is not an integer literal; "+
						"this test reads it lexically and cannot evaluate %T", vs.Values[i])
				}
				n, err := strconv.Atoi(lit.Value)
				if err != nil {
					t.Fatalf("cmd/codelines pureLineLimit = %q: %v", lit.Value, err)
				}
				got, found = n, true
			}
		}
	}

	if !found {
		t.Fatal("cmd/codelines/main.go declares no pureLineLimit constant. " +
			"If the preflight tool stopped mirroring the gate's threshold, it is " +
			"reporting against a number nothing keeps true — either restore the " +
			"constant or delete the flagging behaviour entirely.")
	}
	if got != pureLineLimit {
		t.Errorf("cmd/codelines pureLineLimit = %d, gate pureLineLimit = %d. "+
			"The preflight and the gate must agree; update both or neither.", got, pureLineLimit)
	}
}
