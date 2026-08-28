package bootstrap_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// selfHealAllowedSites is the complete set of places allowed to switch database
// self-healing ON, keyed by "<path>:<enclosing func>".
//
// This is an AUTHORIZATION list, not a debt table. Setting SelfHeal true grants
// a process permission to rename the user's history out of the way, so the set
// is deliberately tiny and every entry names why it owns the database. Adding a
// name here is a data-safety decision that belongs in review, which is the only
// reason this test can see it at all — nothing else in the build distinguishes
// `SelfHeal: true` from any other struct field.
var selfHealAllowedSites = map[string]string{
	"cmd/yanshi/main.go:runServe": "the daemon; it IS the backend for the project and owns the database while it runs",
	"cmd/yanshi/main.go:runTUI":   "the interactive TUI; refusing to start leaves the user with no tool to repair the file",
}

// TestSelfHealIsEnabledOnlyAtOwningEntryPoints pins where healing may be turned
// on. Without it nothing at all connects the narrow doc comment on
// Options.SelfHeal to the code: a new Build caller could copy an existing
// options literal — `SelfHeal: true` included — and no test would notice that a
// per-editor-window ACP server had just been granted the right to quarantine a
// database three other processes are using.
//
// Both directions fail. An unlisted site is an unreviewed grant; a listed site
// that no longer sets it is a stale authorization that would silently
// pre-approve the next function to take that name.
func TestSelfHealIsEnabledOnlyAtOwningEntryPoints(t *testing.T) {
	root := repoRoot(t)
	found := map[string]bool{}

	require.NoError(t, filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "third_party", "reference", "node_modules", "sdk":
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		fset := token.NewFileSet()
		file, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return nil // not our business to police unparseable files
		}
		rel, rerr := filepath.Rel(root, path)
		require.NoError(t, rerr)
		rel = filepath.ToSlash(rel)

		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			ast.Inspect(fn, func(n ast.Node) bool {
				// BOTH spellings, because they are equally a grant and an
				// authorization scan that saw only one would be evadable by
				// reformatting: `Options{SelfHeal: true}` (a composite literal
				// field) and `opts.SelfHeal = true` (an assignment, which is
				// how runTUI does it so exec and headless keep the default).
				switch node := n.(type) {
				case *ast.KeyValueExpr:
					if key, ok := node.Key.(*ast.Ident); ok && key.Name == "SelfHeal" && isTrue(node.Value) {
						found[rel+":"+fn.Name.Name] = true
					}
				case *ast.AssignStmt:
					for i, lhs := range node.Lhs {
						sel, ok := lhs.(*ast.SelectorExpr)
						if !ok || sel.Sel.Name != "SelfHeal" || i >= len(node.Rhs) {
							continue
						}
						if isTrue(node.Rhs[i]) {
							found[rel+":"+fn.Name.Name] = true
						}
					}
				}
				return true
			})
		}
		return nil
	}))

	for site := range found {
		_, allowed := selfHealAllowedSites[site]
		assert.Truef(t, allowed,
			"%s enables database self-healing but is not an owning entry point.\n"+
				"Healing renames the user's history away. If this process really owns the\n"+
				"database, add it to selfHealAllowedSites with the reason; otherwise pass\n"+
				"the option through from a caller that does.", site)
	}
	for site, why := range selfHealAllowedSites {
		assert.Truef(t, found[site],
			"%s is authorized to self-heal (%q) but no longer does.\n"+
				"Remove the entry — a stale authorization silently pre-approves whatever\n"+
				"function next takes that name.", site, why)
	}
}

// repoRoot walks up from the test's working directory to the module root.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs(".")
	require.NoError(t, err)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		require.NotEqual(t, parent, dir, "reached the filesystem root without finding go.mod")
		dir = parent
	}
}

// isTrue reports whether e is the literal `true`. Only a literal counts: a
// forwarded variable (opts.SelfHeal) is a pass-through, not a grant, and the
// grant it forwards is caught at whichever call site wrote the literal.
func isTrue(e ast.Expr) bool {
	id, ok := e.(*ast.Ident)
	return ok && id.Name == "true"
}
