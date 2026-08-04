package guard

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"sort"
	"strconv"
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

// execPolicyVerdictSet derives, from the source of internal/execpolicy, every
// string execpolicy.Evaluate can put in Result.Verdict.
//
// Two shapes produce a verdict and both are covered:
//   - a string literal in a Result composite literal (hard's "hard_deny"), and
//   - an identifier (Evaluate's `decision`), which is admitted only by a
//     `switch <ident>` whose default diverts everything else; that switch's
//     case labels are the reachable values.
//
// Any third shape fails the test rather than being ignored. Silently skipping
// an unrecognised initialiser is how a derivation like this rots into a
// permanently empty set that agrees with everything.
func execPolicyVerdictSet(t *testing.T) map[string]bool {
	t.Helper()
	const file = "../execpolicy/policy.go"
	fset := token.NewFileSet()
	parsed, err := parser.ParseFile(fset, file, nil, 0)
	require.NoError(t, err, "parse %s", file)

	verdicts := map[string]bool{}
	var idents []string
	ast.Inspect(parsed, func(n ast.Node) bool {
		lit, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		if name, ok := lit.Type.(*ast.Ident); !ok || name.Name != "Result" {
			return true
		}
		for _, elt := range lit.Elts {
			kv, ok := elt.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			if key, ok := kv.Key.(*ast.Ident); !ok || key.Name != "Verdict" {
				continue
			}
			switch v := kv.Value.(type) {
			case *ast.BasicLit:
				require.Equal(t, token.STRING, v.Kind, "%s: non-string Verdict literal", file)
				s, err := strconv.Unquote(v.Value)
				require.NoError(t, err)
				verdicts[s] = true
			case *ast.Ident:
				idents = append(idents, v.Name)
			default:
				t.Fatalf("%s: Result.Verdict is initialised from an expression this "+
					"derivation cannot follow (%T). Extend execPolicyVerdictSet, or the "+
					"reconciliation against checkShell silently stops covering it.", file, v)
			}
		}
		return true
	})
	require.NotEmpty(t, verdicts,
		"%s: no literal Verdict found — the derivation is broken, not the code", file)

	for _, name := range idents {
		labels, hasDefault, found := switchCases(t, file, "Evaluate", name)
		require.True(t, found,
			"%s: Result.Verdict is set from %q but Evaluate has no `switch %s` restricting "+
				"it, so its reachable values are unbounded", file, name, name)
		require.True(t, hasDefault,
			"%s: `switch %s` has no default, so a value outside its cases would flow into "+
				"Result.Verdict unchecked", file, name)
		require.NotEmpty(t, labels, "%s: `switch %s` has no case labels", file, name)
		for _, l := range labels {
			verdicts[l] = true
		}
	}
	return verdicts
}
