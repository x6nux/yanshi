package guard

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/execpolicy"
)

// This file holds the two switches inside checkShell against the vocabularies
// their callers depend on. Both reconciliations parse the real source with
// go/ast rather than restating the case labels in a slice, because a restated
// copy is drift with extra steps: it is exactly the artefact whose staleness
// the reconciliation is supposed to detect.
//
// Reading the repository at test time makes every Test in this file OPAQUE TO
// `go test -overlay` (docs/superpowers/review-checklist.md, section A): the
// overlay changes what the compiler sees, not what parser.ParseFile reads back
// off disk. A reviewer gutting checkShell through an overlay would get a
// meaningless PASS here. The overlay-immunity table in the checklist lists this
// file for that reason; probe it by editing the worktree.

// switchCases returns the string case labels of the switch statement inside
// funcName whose tag prints as tagExpr, plus whether that switch has a default
// clause. A missing switch is reported through found=false so the caller can
// fail with a message about the reconciliation being blind rather than about a
// set mismatch.
func switchCases(t *testing.T, file, funcName, tagExpr string) (labels []string, hasDefault, found bool) {
	t.Helper()
	fset := token.NewFileSet()
	parsed, err := parser.ParseFile(fset, file, nil, 0)
	require.NoError(t, err, "parse %s", file)

	print := func(n ast.Node) string {
		var buf bytes.Buffer
		require.NoError(t, printer.Fprint(&buf, fset, n))
		return buf.String()
	}

	for _, decl := range parsed.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil || fn.Name.Name != funcName {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			sw, ok := n.(*ast.SwitchStmt)
			if !ok || sw.Tag == nil || print(sw.Tag) != tagExpr {
				return true
			}
			found = true
			for _, stmt := range sw.Body.List {
				clause, ok := stmt.(*ast.CaseClause)
				if !ok {
					continue
				}
				if clause.List == nil {
					hasDefault = true
					continue
				}
				for _, expr := range clause.List {
					lit, ok := expr.(*ast.BasicLit)
					require.True(t, ok && lit.Kind == token.STRING,
						"%s: case label %q in `switch %s` is not a string literal; the "+
							"reconciliation cannot see it", file, print(expr), tagExpr)
					v, err := strconv.Unquote(lit.Value)
					require.NoError(t, err)
					labels = append(labels, v)
				}
			}
			return false
		})
	}
	sort.Strings(labels)
	return labels, hasDefault, found
}

// TestShellPolicyCatalogEqualsCheckShellSwitch is the reconciliation
// TestShellPolicyCatalogMatchesCheckShell only half performs.
//
// That test drives Check with every catalog entry and with a handful of
// hand-written bogus strings. The first loop is a real check of one direction:
// a catalog value checkShell does not handle shows up as a structural HardDeny.
// The second loop is NOT a check of the other direction — four literals cannot
// notice that checkShell grew a case the catalog omits, and a value checkShell
// accepts but ShellPolicies() leaves out makes a WORKING config fail to load,
// which is the same class of harm this whole area exists to prevent.
//
// Set equality against the parsed switch closes it. Adding `case "permitall":`
// to checkShell without extending ShellPolicies() fails here, and so does the
// reverse.
func TestShellPolicyCatalogEqualsCheckShellSwitch(t *testing.T) {
	labels, _, found := switchCases(t, "guard.go", "checkShell", "p.Shell.Policy")
	require.True(t, found,
		"no `switch p.Shell.Policy` found in checkShell — the switch was renamed or "+
			"restructured and this reconciliation is now blind; re-point it before trusting it")
	// Self-proof that the extractor is live before its result is used as
	// evidence (review-checklist.md, C-bis): a parser that silently returned
	// nothing would make the set comparison below read as a clean pass in one
	// direction and a mass failure in the other.
	require.NotEmpty(t, labels, "the extractor found the switch but no case labels")
	// The policy switch has no default clause — its fail-closed landing is the
	// `return hardDeny(...)` after the switch, which is why hasDefault is
	// deliberately ignored here. That landing is pinned behaviourally by
	// TestShellPolicyCatalogMatchesCheckShell's bogus-value loop.

	want := append([]string(nil), ShellPolicies()...)
	sort.Strings(want)
	require.Equal(t, want, labels,
		"ShellPolicies() and checkShell's switch disagree.\n"+
			"catalog: %v\nswitch:  %v\n"+
			"An extra catalog entry becomes a structural HardDeny at the first shell_run; "+
			"an extra switch case makes a config the guard can enforce fail to load.",
		want, labels)
}

// TestExecPolicyVerdictsAreHandledByCheckShell pins the claim CLAUDE.md and
// docs/user-guide/guard.md make about checkShell's `switch result.Verdict`
// default branch: it is defensive, and no rules table can reach it.
//
// The claim matters because the branch is a STRUCTURAL HardDeny — no
// permission mode, yolo included, can override it. Today nothing reaches it
// only because execpolicy.Evaluate funnels every unrecognised rule decision
// through its own hard() constructor. That is an implementation detail, not a
// contract: anyone adding a verdict that Evaluate returns directly would
// silently create a denial no operator can clear, and the failure would look
// like a bug in the rules table rather than in the verdict vocabulary.
//
// The check is static plus behavioural, because neither half suffices alone.
// Static analysis derives what Evaluate CAN return (a matrix only samples what
// it does return); the behavioural matrix proves the derived set is not
// vacuous (a parser that found nothing would make the subset assertion trivially
// true).
func TestExecPolicyVerdictsAreHandledByCheckShell(t *testing.T) {
	declared := execPolicyVerdictSet(t)

	// Behavioural self-proof (review-checklist.md, C-bis): drive the real
	// Evaluate and require the derived set to actually contain what comes back,
	// and to contain each of the three verdicts a live policy engine produces.
	observed := map[string]bool{}
	record := func(r execpolicy.Result) {
		require.Contains(t, declared, r.Verdict,
			"Evaluate returned %q, which the static derivation missed — the derivation "+
				"is stale and every conclusion below it is void", r.Verdict)
		observed[r.Verdict] = true
	}
	rule := func(decision string) execpolicy.Rule {
		return execpolicy.Rule{ID: "r-" + decision, Program: "go", Prefix: []string{"test"}, Decision: decision}
	}
	cmd, err := execpolicy.Parse("go test ./...")
	require.NoError(t, err)
	for _, decision := range []string{"allow", "prompt", "deny", "hard_deny", "warn", "", "ALLOW"} {
		record(execpolicy.Evaluate(cmd, []execpolicy.Rule{rule(decision)}))
	}
	record(execpolicy.Evaluate(cmd, []execpolicy.Rule{{ID: "other", Program: "git", Decision: "allow"}}))
	record(execpolicy.Evaluate(execpolicy.Command{}, []execpolicy.Rule{rule("allow")}))
	denyFlag := execpolicy.Rule{ID: "no-e2e", Program: "go", Prefix: []string{"test"},
		Decision: "deny", DenyFlags: []string{"./..."}}
	record(execpolicy.Evaluate(cmd, []execpolicy.Rule{denyFlag}))
	for _, want := range []string{"allow", "prompt", "hard_deny"} {
		require.True(t, observed[want],
			"the matrix never produced %q, so it is not exercising Evaluate as intended", want)
	}

	handled, hasDefault, found := switchCases(t, "guard.go", "checkShell", "result.Verdict")
	require.True(t, found,
		"no `switch result.Verdict` found in checkShell — re-point this reconciliation")
	require.NotEmpty(t, handled, "the extractor found the switch but no case labels")
	require.True(t, hasDefault,
		"the default branch must stay: it is the fail-closed landing for a verdict this "+
			"reconciliation would otherwise only report at test time")

	handledSet := map[string]bool{}
	for _, h := range handled {
		handledSet[h] = true
	}
	for verdict := range declared {
		require.True(t, handledSet[verdict],
			"execpolicy.Evaluate can return %q but checkShell has no case for it, so it "+
				"lands in the default branch: a STRUCTURAL HardDeny that yolo and auto "+
				"cannot override. Add `case %q` with the tier it belongs to, and update "+
				"the structural-HardDeny enumerations in CLAUDE.md and "+
				"docs/user-guide/guard.md.", verdict, verdict)
	}
}

// execPolicySourceFiles returns every non-test .go file in dir.
//
// The derivation below reads the whole package directory rather than a single
// named file. A one-file derivation goes blind the moment someone splits the
// package or moves a constructor, and it goes blind SILENTLY: the set shrinks,
// the subset assertion against checkShell gets easier, and nothing reports it.
func execPolicySourceFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err, "read %s", dir)
	var files []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		files = append(files, filepath.Join(dir, name))
	}
	require.NotEmpty(t, files, "no non-test sources under %s — the derivation is blind", dir)
	return files
}

// isResultType reports whether expr names execpolicy.Result (or a pointer to
// it) as written inside the execpolicy package itself.
func isResultType(expr ast.Expr) bool {
	switch t := expr.(type) {
	case *ast.Ident:
		return t.Name == "Result"
	case *ast.StarExpr:
		return isResultType(t.X)
	}
	return false
}

// registerElidedResultLits marks the type-elided element literals of a
// []Result / map[K]Result composite literal, so `[]Result{{Verdict: "x"}}` is
// followed as well as `[]Result{Result{Verdict: "x"}}`. Without this the inner
// literal has a nil Type and looks like a literal of some unrelated type.
func registerElidedResultLits(lit *ast.CompositeLit, elided map[*ast.CompositeLit]bool) {
	var elem ast.Expr
	switch t := lit.Type.(type) {
	case *ast.ArrayType:
		elem = t.Elt
	case *ast.MapType:
		elem = t.Value
	default:
		return
	}
	if !isResultType(elem) {
		return
	}
	for _, e := range lit.Elts {
		if kv, ok := e.(*ast.KeyValueExpr); ok {
			e = kv.Value
		}
		if inner, ok := e.(*ast.CompositeLit); ok && inner.Type == nil {
			elided[inner] = true
		}
	}
}

// resultVerdictFieldIndex returns the position of Verdict in the declared field
// order of `type Result struct`, which is what a POSITIONAL composite literal
// (`Result{"hard_deny", …}`) indexes into. Resolving it from the struct
// declaration rather than hard-coding 0 means reordering the struct cannot
// silently re-point the derivation at the wrong field.
func resultVerdictFieldIndex(t *testing.T, files []string) int {
	t.Helper()
	for _, file := range files {
		fset := token.NewFileSet()
		parsed, err := parser.ParseFile(fset, file, nil, 0)
		require.NoError(t, err, "parse %s", file)
		for _, decl := range parsed.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.TYPE {
				continue
			}
			for _, spec := range gen.Specs {
				ts, ok := spec.(*ast.TypeSpec)
				if !ok || ts.Name.Name != "Result" {
					continue
				}
				st, ok := ts.Type.(*ast.StructType)
				require.True(t, ok, "%s: Result is not a struct type", file)
				idx := 0
				for _, field := range st.Fields.List {
					if len(field.Names) == 0 {
						idx++ // embedded field still occupies a position
						continue
					}
					for _, n := range field.Names {
						if n.Name == "Verdict" {
							return idx
						}
						idx++
					}
				}
				t.Fatalf("%s: type Result has no Verdict field", file)
			}
		}
	}
	t.Fatalf("no `type Result struct` found in %v — positional literals cannot be resolved", files)
	return -1
}

// recordResultVerdict feeds the expression a Result literal puts into Verdict
// to record, handling both the keyed and the positional literal forms.
func recordResultVerdict(t *testing.T, file string, lit *ast.CompositeLit, verdictField int, record func(ast.Expr)) {
	t.Helper()
	if len(lit.Elts) == 0 {
		return
	}
	if _, keyed := lit.Elts[0].(*ast.KeyValueExpr); keyed {
		for _, elt := range lit.Elts {
			kv, ok := elt.(*ast.KeyValueExpr)
			require.True(t, ok, "%s: Result literal mixes keyed and positional elements", file)
			if key, ok := kv.Key.(*ast.Ident); !ok || key.Name != "Verdict" {
				continue
			}
			record(kv.Value)
		}
		return
	}
	// Positional. Go requires a value for every field, so Verdict's declared
	// position indexes the literal directly.
	require.Greater(t, len(lit.Elts), verdictField,
		"%s: positional Result literal has %d elements, too few to reach Verdict at position %d",
		file, len(lit.Elts), verdictField)
	record(lit.Elts[verdictField])
}

// execPolicyVerdictSet derives, from the source of internal/execpolicy, every
// string execpolicy.Evaluate can put in Result.Verdict.
//
// SCOPE, stated before the result is trusted. The derivation reads EVERY
// non-test .go file in the package directory (execPolicySourceFiles), not one
// named file, and it follows three syntactic shapes:
//
//   - a KEYED composite literal (`Result{Verdict: "hard_deny"}`), including the
//     type-elided form nested in a []Result / map[K]Result literal;
//   - a POSITIONAL composite literal (`Result{"hard_deny", …}`), resolved
//     against the declared field order of Result. This shape was a silent miss
//     for as long as the walk only looked at KeyValueExpr elements — a
//     positional element simply failed the type assertion and was skipped;
//   - an ASSIGNMENT to a .Verdict field (`r.Verdict = …`), which is not a
//     composite literal at all and so was invisible to a walk that matched only
//     CompositeLit nodes.
//
// The assignment case deliberately OVER-approximates: it matches any selector
// named Verdict regardless of the receiver's static type, because a file-local
// AST carries no type information. Inside this package Verdict is a field of
// Result alone, and over-approximating only ever makes the reconciliation
// against checkShell stricter.
//
// In every shape, a value the derivation cannot follow — a call, a conversion,
// a concatenation, a multi-value assignment — fails the test rather than being
// skipped. Silently skipping an unrecognised initialiser is how a derivation
// like this rots into a permanently empty set that agrees with everything.
//
// What is NOT covered, said plainly: a verdict that reaches Result.Verdict
// through a value flowing out of this package's own helpers by some fourth
// route (reflection, unsafe, a generic container the walk does not model) would
// still be missed. The three shapes above are the ones Go offers for writing a
// struct field directly, which is why they are enumerated rather than
// approximated.
func execPolicyVerdictSet(t *testing.T) map[string]bool {
	t.Helper()
	const dir = "../execpolicy"
	files := execPolicySourceFiles(t, dir)
	verdictField := resultVerdictFieldIndex(t, files)

	verdicts := map[string]bool{}
	type identRef struct{ file, fn, name string }
	var idents []identRef

	for _, file := range files {
		fset := token.NewFileSet()
		parsed, err := parser.ParseFile(fset, file, nil, 0)
		require.NoError(t, err, "parse %s", file)

		render := func(n ast.Node) string {
			var buf bytes.Buffer
			require.NoError(t, printer.Fprint(&buf, fset, n))
			return buf.String()
		}

		for _, decl := range parsed.Decls {
			enclosing := ""
			if fd, ok := decl.(*ast.FuncDecl); ok {
				enclosing = fd.Name.Name
			}
			record := func(v ast.Expr) {
				switch e := v.(type) {
				case *ast.BasicLit:
					require.Equal(t, token.STRING, e.Kind, "%s: non-string Verdict literal", file)
					s, err := strconv.Unquote(e.Value)
					require.NoError(t, err)
					verdicts[s] = true
				case *ast.Ident:
					idents = append(idents, identRef{file: file, fn: enclosing, name: e.Name})
				default:
					t.Fatalf("%s: Result.Verdict is set from an expression this derivation "+
						"cannot follow (%T: %s). Extend execPolicyVerdictSet, or the "+
						"reconciliation against checkShell silently stops covering it.",
						file, v, render(v))
				}
			}
			elided := map[*ast.CompositeLit]bool{}
			ast.Inspect(decl, func(n ast.Node) bool {
				switch node := n.(type) {
				case *ast.AssignStmt:
					for i, lhs := range node.Lhs {
						sel, ok := lhs.(*ast.SelectorExpr)
						if !ok || sel.Sel.Name != "Verdict" {
							continue
						}
						require.Equal(t, len(node.Lhs), len(node.Rhs),
							"%s: %s is assigned from a multi-value expression this derivation "+
								"cannot follow", file, render(lhs))
						record(node.Rhs[i])
					}
				case *ast.CompositeLit:
					registerElidedResultLits(node, elided)
					if !isResultType(node.Type) && !elided[node] {
						return true
					}
					recordResultVerdict(t, file, node, verdictField, record)
				}
				return true
			})
		}
	}
	require.NotEmpty(t, verdicts,
		"%s: no literal Verdict found — the derivation is broken, not the code", dir)

	for _, ref := range idents {
		require.NotEmpty(t, ref.fn,
			"%s: Result.Verdict is set from identifier %q outside any function body, so no "+
				"switch can bound its reachable values", ref.file, ref.name)
		labels, hasDefault, found := switchCases(t, ref.file, ref.fn, ref.name)
		require.True(t, found,
			"%s: %s sets Result.Verdict from %q but has no `switch %s` restricting it, so "+
				"its reachable values are unbounded", ref.file, ref.fn, ref.name, ref.name)
		require.True(t, hasDefault,
			"%s: `switch %s` in %s has no default, so a value outside its cases would flow "+
				"into Result.Verdict unchecked", ref.file, ref.name, ref.fn)
		require.NotEmpty(t, labels, "%s: `switch %s` has no case labels", ref.file, ref.name)
		for _, l := range labels {
			verdicts[l] = true
		}
	}
	return verdicts
}
