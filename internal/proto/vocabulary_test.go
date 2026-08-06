package proto

import (
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// declaredTypes extracts every ServerFrame.Type value assigned in frame.go.
//
// Reading the source rather than a hand-kept list is the point: a list would
// have to be updated by the same person who forgot to add the frame to
// goldenFrames, and it would be right exactly when the thing it guards is
// already right.
func declaredTypes(t *testing.T) map[string]bool {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "frame.go", nil, 0)
	if err != nil {
		t.Fatalf("parse frame.go: %v", err)
	}
	out := map[string]bool{}
	ast.Inspect(file, func(n ast.Node) bool {
		lit, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		// ONLY ServerFrame. frame.go declares ClientFrame in the same file and
		// its Type vocabulary is the request direction — a different contract,
		// with no golden corpus and no SSE emission. Counting both made the
		// first version of this test report 41 "missing" types that golden was
		// never meant to carry.
		name, ok := lit.Type.(*ast.Ident)
		if !ok || name.Name != "ServerFrame" {
			return true
		}
		for _, elt := range lit.Elts {
			kv, ok := elt.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			if key, ok := kv.Key.(*ast.Ident); !ok || key.Name != "Type" {
				continue
			}
			switch v := kv.Value.(type) {
			case *ast.BasicLit:
				if v.Kind == token.STRING {
					if s, err := strconv.Unquote(v.Value); err == nil && s != "" {
						out[s] = true
					}
				}
			case *ast.Ident:
				// A named constant, e.g. SubagentEventType.
				if s := constValue(file, v.Name); s != "" {
					out[s] = true
				}
			}
		}
		return true
	})
	return out
}

// constValue resolves a string constant declared in the given file.
func constValue(file *ast.File, name string) string {
	var found string
	ast.Inspect(file, func(n ast.Node) bool {
		spec, ok := n.(*ast.ValueSpec)
		if !ok {
			return true
		}
		for i, ident := range spec.Names {
			if ident.Name != name || i >= len(spec.Values) {
				continue
			}
			if lit, ok := spec.Values[i].(*ast.BasicLit); ok && lit.Kind == token.STRING {
				if s, err := strconv.Unquote(lit.Value); err == nil {
					found = s
				}
			}
		}
		return true
	})
	return found
}

// TestGoldenCoversEveryDeclaredFrameType pins the golden corpus against the
// frame vocabulary itself.
//
// goldenFrames() is what freezes the wire format, and nothing checked that it
// actually covered it. It did not: NewTaskUpdate(nil) short-circuits to a ZERO
// ServerFrame, so the golden file froze `event: ` (empty) and
// `data: {"type":""}` for that row — a frame type absent from the corpus, plus
// an empty-string type present in it that no constructor can produce. Both
// directions matter: a missing type means the format is unfrozen, an extra one
// means the golden file documents a frame that does not exist.
//
// ledger: E1/COV2#2 全帧往返
func TestGoldenCoversEveryDeclaredFrameType(t *testing.T) {
	declared := declaredTypes(t)
	if len(declared) < 20 {
		t.Fatalf("only %d declared frame types found; the AST scan is broken and "+
			"every assertion below would be vacuous", len(declared))
	}

	golden := map[string]bool{}
	for _, f := range goldenFrames() {
		golden[f.Type] = true
	}

	var missing, extra []string
	for typ := range declared {
		if !golden[typ] {
			missing = append(missing, typ)
		}
	}
	for typ := range golden {
		if !declared[typ] {
			extra = append(extra, strconv.Quote(typ))
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)

	if len(missing) > 0 {
		t.Errorf("goldenFrames() does not cover %d declared frame type(s): %s\n"+
			"  the golden file is what freezes the wire format; an uncovered type is "+
			"free to change shape without any test noticing",
			len(missing), strings.Join(missing, ", "))
	}
	if len(extra) > 0 {
		t.Errorf("goldenFrames() emits %d type(s) no constructor declares: %s\n"+
			"  the usual cause is a constructor that short-circuits to a zero frame "+
			"for the argument the golden row passes it",
			len(extra), strings.Join(extra, ", "))
	}
}
