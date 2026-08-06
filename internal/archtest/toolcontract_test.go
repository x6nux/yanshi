package archtest

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// retiredToolSymbols are names the tool-interface rewrite removed. A functional
// test cannot express "this is gone" — it can only exercise what exists — so
// the clause is met by a prohibition, and the prohibition has to be enforced or
// it is just a note about what was true on the day it was written.
//
// Both are currently at zero declarations and zero call sites. That is not the
// property being defended: what this stops is one of them coming BACK, which is
// how the pre-rewrite progress plumbing would return — a second output channel
// alongside ToolChunk, with the model receiving whatever the TUI receives.
var retiredToolSymbols = []string{
	"ToolProgressCallback",
	"lineProgressWriter",
}

// TestRetiredToolSymbolsDoNotReturn scans identifiers, not text.
//
// internal/tools/shell.go carries a comment that names lineProgressWriter to
// explain what the current StreamFunc replaced. A grep-based gate would have to
// choose between failing on that sentence and being deleted, and the repo has
// hit that exact fork three times already (the CI bench gate, the cliff prefix
// gate, the goreleaser LICENSE gate all matched their own explanations). Parsing
// sidesteps the choice: an identifier in a comment is not an identifier.
//
// ledger: M1/SPEC-TOOLIF#3 废弃 JSON 包装与 ToolProgressCallback/lineProgressWriter
func TestRetiredToolSymbolsDoNotReturn(t *testing.T) {
	var offenders []string
	scanned := 0

	for _, root := range []string{abs("internal"), abs("cmd")} {
		err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			fset := token.NewFileSet()
			// Comments are deliberately NOT parsed: ParseFile without
			// ParseComments leaves them out of the AST entirely, so the
			// shell.go sentence explaining the retirement cannot trip this.
			file, perr := parser.ParseFile(fset, path, nil, 0)
			if perr != nil {
				return nil // not compilable on this platform; other gates own that
			}
			scanned++
			ast.Inspect(file, func(n ast.Node) bool {
				id, ok := n.(*ast.Ident)
				if !ok {
					return true
				}
				for _, retired := range retiredToolSymbols {
					if id.Name == retired {
						pos := fset.Position(id.Pos())
						offenders = append(offenders,
							short(pos.Filename, mustModuleRoot())+":"+itoa(pos.Line)+": "+retired)
					}
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatalf("walking %s: %v", root, err)
		}
	}

	// Without this the gate would report clean if the walk found nothing —
	// a wrong root, a rename of internal/, a WalkDir that errored into the
	// skip branch on every file.
	if scanned < 100 {
		t.Fatalf("only %d non-test .go files scanned; the walk is not reaching the tree", scanned)
	}
	if len(offenders) > 0 {
		t.Errorf("retired tool-progress symbols are back in production code:\n  %s\n"+
			"  output goes through ToolChunk's fields; a second progress channel is what "+
			"the rewrite removed", strings.Join(offenders, "\n  "))
	}
}
