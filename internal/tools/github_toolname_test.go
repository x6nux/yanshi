package tools

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// TestGHSpecCarriesTheRealToolName pins the name guard authorizes against.
//
// ghSpec hardcoded Tool: "github" while the four registered tools are
// github_pr_context / github_comment / github_approve / github_merge. The
// factory profile allows those four names, so every call died at the guard
// with `denied: tool "github" not permitted` -- all four tools were dead out
// of the box, and the failure named a tool that does not exist, which is a
// hard error to trace back to a spec builder.
//
// Asserting on the spec rather than on a full guard round trip is deliberate:
// the defect is that the two names disagree, and the spec is where the wrong
// one was written. A test that drove the guard would also pass if someone
// "fixed" it by adding "github" to the allow list, which would grant a name
// no tool answers to.
func TestGHSpecCarriesTheRealToolName(t *testing.T) {
	for _, name := range []string{"github_pr_context", "github_comment", "github_approve", "github_merge"} {
		spec := ghSpec(name, "pr", "view")
		if spec.Tool != name {
			t.Errorf("ghSpec(%q).Tool = %q: guard authorizes this string, and no tool is registered under it",
				name, spec.Tool)
		}
		if spec.Program != "gh" {
			t.Errorf("ghSpec(%q).Program = %q, want gh", name, spec.Program)
		}
		if len(spec.Args) == 0 || spec.Args[0] != "pr" {
			t.Errorf("ghSpec(%q) lost its args: %v", name, spec.Args)
		}
		if strings.Contains(strings.Join(spec.Args, " "), name) {
			t.Errorf("ghSpec(%q) leaked the tool name into gh's argv: %v", name, spec.Args)
		}
	}
}

// TestEveryGHSpecCallSiteNamesARegisteredTool is the static half.
//
// The unit test above proves ghSpec propagates whatever it is given; this
// proves every caller gives it a name that a tool is actually registered
// under. The two are different failures: the first is a builder bug, the
// second is a caller typo, and a typo here produces the same silent
// out-of-the-box death -- the guard refuses a name nothing answers to, and the
// error message points at the profile rather than at the call site.
//
// Parsing the source is the only way to see this. There is no runtime moment
// where the set of ghSpec call sites is observable.
func TestEveryGHSpecCallSiteNamesARegisteredTool(t *testing.T) {
	gh := NewGitHubTools(nil)
	registered := map[string]bool{}
	for _, tl := range []*GuardedTool{gh.PRContext, gh.Comment, gh.Approve, gh.Merge} {
		registered[tl.name] = true
	}
	if len(registered) == 0 {
		t.Fatal("no github tools registered: the harness cannot check anything")
	}

	fset := token.NewFileSet()
	af, err := parser.ParseFile(fset, "github.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	ast.Inspect(af, func(n ast.Node) bool {
		ce, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		id, ok := ce.Fun.(*ast.Ident)
		if !ok || id.Name != "ghSpec" || len(ce.Args) == 0 {
			return true
		}
		lit, ok := ce.Args[0].(*ast.BasicLit)
		if !ok {
			t.Errorf("%s: ghSpec's tool name must be a literal so it can be checked here",
				fset.Position(ce.Pos()))
			return true
		}
		name := strings.Trim(lit.Value, `"`)
		calls++
		if !registered[name] {
			t.Errorf("%s: ghSpec(%q) names no registered tool; guard will refuse it",
				fset.Position(ce.Pos()), name)
		}
		return true
	})
	if calls != len(registered) {
		t.Errorf("found %d ghSpec call sites for %d registered github tools: "+
			"a tool that never builds a spec cannot work, and two sharing one name hides a bug",
			calls, len(registered))
	}
}
