// internal/agent/orchestrator/subagent_redact_test.go
//
// W-A-02 fix round 2: managed sub-agents (agent_spawn/agent_resume, both in
// DefaultOrchestratorProfile's allow list) ran with no redactor at all. The
// registry Manager is built with RootContext: context.Background() -- a bare
// context -- so a managed turn's ctx carries nothing from the main turn
// except what managedTurnRunner.Run re-binds explicitly, and Redactor was
// missing from that list. Every shell_run/fs_read result a managed sub-agent
// produced reached its own model call unredacted.
//
// Two call graphs needed the fix, and this file covers each independently:
//   - managedTurnRunner.Run's own binding, exercised via TestManagedTurnRunner
//     by calling Run directly with a bare ctx -- the exact shape the real
//     registry.Manager hands it (see manager.go: parentCtx := m.rootCtx for a
//     first-level spawn).
//   - runSubAgentTurn's inline New(Config{...}) fallback, exercised via
//     TestRunSubAgentTurnInlineFallbackBindsRedactor by calling
//     o.runSubAgentTurn directly with a ctx that has neither WithManager nor
//     WithRedactor bound -- the shape agent_batch's row runner and the legacy
//     SubAgentRunner hand it when reached from an already-detached context.
//     This test is deliberately independent of managedTurnRunner.Run so it
//     cannot pass on the strength of the OTHER fix alone.
package orchestrator

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/agent/registry"
	"github.com/x6nux/yanshi/internal/guard"
	einollm "github.com/x6nux/yanshi/internal/llm/eino"
	"github.com/x6nux/yanshi/internal/secrets"
	"github.com/x6nux/yanshi/internal/tools"
)

// newRedactorProbeTool builds a tool that records whether a *secrets.Redactor
// was resolvable from its execution context, into the two pointers supplied
// by the caller. Using a captured variable rather than the tool's return text
// keeps the assertion independent of what a FakeModel does with that text --
// FakeModelWithMessages plays back canned messages regardless of tool output.
func newRedactorProbeTool(sawIt *bool, got **secrets.Redactor) *tools.GuardedTool {
	return tools.NewGuardedTool("probe_redactor", "Probe", "records redactor presence", 5*time.Second, nil,
		func(ctx context.Context, _ string) <-chan tools.ToolChunk {
			ch := make(chan tools.ToolChunk, 1)
			if r, ok := tools.RedactorFromContext(ctx); ok {
				*sawIt = true
				*got = r
			}
			ch <- tools.ToolChunk{Result: "checked"}
			close(ch)
			return ch
		})
}

// probeToolCallThenAnswer returns the two-message FakeModel script every test
// in this file drives: one turn that calls probe_redactor, one that answers.
func probeToolCallThenAnswer() []*schema.Message {
	call := schema.AssistantMessage("", []schema.ToolCall{
		{ID: "c1", Type: "function", Function: schema.FunctionCall{
			Name: "probe_redactor", Arguments: `{}`,
		}},
	})
	answer := schema.AssistantMessage("done", nil)
	return []*schema.Message{call, answer}
}

// TestManagedTurnRunnerBindsRedactor drives the real managed sub-agent entry
// point (managedTurnRunner.Run, the concrete registry.Runner agent_spawn and
// agent_resume ultimately invoke) with a bare context -- exactly what the
// real registry.Manager hands a first-level spawn's Run call, per
// manager.go's `parentCtx := m.rootCtx`. Before the fix this context reached
// the tool call with no redactor bound at any point in the chain.
func TestManagedTurnRunnerBindsRedactor(t *testing.T) {
	var sawIt bool
	var got *secrets.Redactor
	probe := newRedactorProbeTool(&sawIt, &got)

	red := secrets.NewRedactor()
	red.Register("managed-sub-agent-secret-XYZ")

	model := einollm.NewFakeModelWithMessages(probeToolCallThenAnswer(), nil)
	profile := guard.PermissionProfile{Tools: guard.ToolsPerm{Allow: []string{"probe_redactor"}}}
	orch, err := New(Config{
		Model:    model,
		Tools:    []BaseTool{probe},
		Profile:  profile,
		Redactor: red,
	})
	require.NoError(t, err)

	mgr := registry.NewManager(registry.NewManagerOpts{RootContext: context.Background(), Path: t.TempDir()})
	t.Cleanup(mgr.Close)

	runner := &managedTurnRunner{
		o:       orch,
		mgr:     mgr,
		profile: orch.ProfileForTest(),
		allowed: []string{"probe_redactor"},
	}

	// context.Background(), not a context descending from any turn: this is
	// the bare shape managedTurnRunner.Run actually receives in production.
	out, err := runner.Run(context.Background(), "sub-1", "run the probe")
	require.NoError(t, err)
	assert.Equal(t, "done", out)

	require.True(t, sawIt, "managedTurnRunner.Run must bind the redactor onto the "+
		"sub-agent's execution context; a tool it ran could not resolve one via "+
		"tools.RedactorFromContext")
	assert.Same(t, red, got, "the sub-agent must see the orchestrator's own redactor, not a copy or a different instance")
}

// TestManagedSubAgentTurnEndToEndReachesTheProbe forces runSubAgentTurn's
// Manager-routing check (orchestrator.go, `if mgr := tools.ManagerFromContext(ctx); mgr != nil`)
// to be TRUE, so execution is driven through the full production chain a real
// agent_spawn/agent_resume call takes: bindManagedRunner -> runSubAgentTurn's
// managed branch -> tools.ManagedSubAgentRun -> registry.Manager.Spawn -> the
// runtime goroutine -> managedTurnRunner.Run. Round 3 fix.
//
// Round 2's TestManagedTurnRunnerBindsRedactor calls managedTurnRunner.Run
// directly; this test additionally proves the OUTER dispatch (the part a real
// agent_spawn tool call goes through, which round 2 did not exercise) also
// reaches a working Run() and a probe tool sees a redactor.
//
// It does NOT give fix #1 (managedTurnRunner.Run's own WithRedactor bind)
// independent regression coverage, and this was verified empirically, not
// assumed -- see the three-experiment log in task-2-report.txt's round 3
// section. The reason is structural, not a test-authoring gap: Run() never
// rebinds WithManager/WithManagedRunnerFactory onto the context it hands
// runSubAgentTurn, so that second call ALWAYS takes the inline
// New(Config{...}) fallback (fix #2) -- and fix #2's binding is gated on
// `sub.redactor != nil`, where sub.redactor is read directly off the Go
// struct field `o.redactor` (the same field fix #1 reads), independent of
// whatever fix #1 did or didn't bind onto ctx. Deleting fix #1's line alone
// does not change what `o.redactor` is, so fix #2 supplies the value on its
// own every time this test runs, with fix #1 present or absent. Coverage for
// fix #1 alone is TestManagedTurnRunnerRunSourcePinsTheRedactorBind (the
// source-pin), which this test does not replace.
func TestManagedSubAgentTurnEndToEndReachesTheProbe(t *testing.T) {
	var sawIt bool
	var got *secrets.Redactor
	probe := newRedactorProbeTool(&sawIt, &got)

	red := secrets.NewRedactor()
	red.Register("managed-e2e-secret-XYZ")

	model := einollm.NewFakeModelWithMessages(probeToolCallThenAnswer(), nil)
	profile := guard.PermissionProfile{Tools: guard.ToolsPerm{Allow: []string{"probe_redactor"}}}

	mgr := registry.NewManager(registry.NewManagerOpts{
		RootContext: context.Background(),
		Path:        filepath.Join(t.TempDir(), "state.json"),
	})
	t.Cleanup(mgr.Close)

	orch, err := New(Config{
		Model:           model,
		Tools:           []BaseTool{probe},
		Profile:         profile,
		Redactor:        red,
		SubagentManager: mgr,
	})
	require.NoError(t, err)

	ctx := orch.bindManagedRunner(context.Background())
	require.NotNil(t, tools.ManagerFromContext(ctx), "test setup: Manager must be bound for the routing check to take the managed branch")
	require.NotNil(t, tools.ManagedRunnerFactoryFromContext(ctx), "test setup: factory must be bound for the routing check to take the managed branch")

	out, err := orch.runSubAgentTurn(ctx, "run the probe", []string{"probe_redactor"}, "", 0)
	require.NoError(t, err)
	assert.Equal(t, "done", out)

	require.True(t, sawIt, "the managed sub-agent's probe tool could not resolve a redactor via tools.RedactorFromContext")
	assert.Same(t, red, got, "the sub-agent must see the orchestrator's own redactor, not a copy or a different instance")
}

// TestRunSubAgentTurnInlineFallbackBindsRedactor isolates the inline
// New(Config{...}) fallback in runSubAgentTurn (the branch taken whenever no
// Manager/factory is bound on ctx -- agent_batch's per-row runner and the
// legacy SubAgentRunner both reach it this way). ctx here carries neither
// WithManager nor WithRedactor, and managedTurnRunner.Run is never called, so
// this test cannot pass on the strength of that other fix -- only the
// Redactor field on the inline Config literal can make it pass.
func TestRunSubAgentTurnInlineFallbackBindsRedactor(t *testing.T) {
	var sawIt bool
	var got *secrets.Redactor
	probe := newRedactorProbeTool(&sawIt, &got)

	red := secrets.NewRedactor()
	red.Register("inline-fallback-secret-XYZ")

	model := einollm.NewFakeModelWithMessages(probeToolCallThenAnswer(), nil)
	profile := guard.PermissionProfile{Tools: guard.ToolsPerm{Allow: []string{"probe_redactor"}}}
	orch, err := New(Config{
		Model:    model,
		Tools:    []BaseTool{probe},
		Profile:  profile,
		Redactor: red,
	})
	require.NoError(t, err)

	// A bare ctx with neither WithManager nor WithRedactor bound -- the shape
	// this path actually receives from batch.rowRunner.Run and from the
	// legacy tools.SubAgentRunner closure when invoked off a detached ctx.
	out, err := orch.runSubAgentTurn(context.Background(), "run the probe", []string{"probe_redactor"}, "", 0)
	require.NoError(t, err)
	assert.Equal(t, "done", out)

	require.True(t, sawIt, "runSubAgentTurn's inline New(Config{...}) fallback must set "+
		"Redactor: o.redactor, or the nested sub-Orchestrator it builds runs with none")
	assert.Same(t, red, got, "the sub-agent must see the orchestrator's own redactor, not a copy or a different instance")
}

// TestManagedTurnRunnerRunSourcePinsTheRedactorBind is a structural
// regression detector for managedTurnRunner.Run's own WithRedactor call.
//
// It exists because no behavioral test can give this binding independent
// coverage: TestManagedTurnRunnerBindsRedactor and even
// TestManagedSubAgentTurnEndToEndReachesTheProbe (which forces the FULL
// production dispatch -- bindManagedRunner -> the Manager-routing branch of
// runSubAgentTurn -> tools.ManagedSubAgentRun -> registry.Manager.Spawn ->
// managedTurnRunner.Run) were both empirically found to still PASS with this
// binding deleted. The reason is structural, not a test-authoring gap: Run()
// never rebinds tools.WithManager/WithManagedRunnerFactory onto the ctx it
// hands r.o.runSubAgentTurn, so that call always takes the inline
// New(Config{...}) fallback (fix #2, round 2), and fix #2's own
// WithRedactor call is gated on `sub.redactor != nil` -- read directly off
// the Go struct field `o.redactor`, the SAME field this binding reads,
// independent of whatever ctx carries. Fix #2 alone supplies the value on
// every call, with this binding present or absent, forcing this coverage to
// live outside the behavioral layer.
//
// Round 3: a plain substring scan (this test's round-2 form) was empirically
// shown to still PASS when the binding was neutered as
// `if false { // ctx = tools.WithRedactor(ctx, r.o.redactor) }` -- present
// as source text, dead at runtime. A substring check cannot distinguish a
// live statement from the same text inside a comment or an unreachable
// branch. This version parses the AST instead: comments are not statements
// (so a commented-out binding is invisible to it, correctly), and a literal
// `if false { ... }` guard is walked but its body is marked dead and
// excluded from the search.
func TestManagedTurnRunnerRunSourcePinsTheRedactorBind(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "orchestrator.go", nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse orchestrator.go: %v", err)
	}

	var runFn *ast.FuncDecl
	for _, decl := range file.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok || fd.Name.Name != "Run" || fd.Recv == nil || len(fd.Recv.List) != 1 {
			continue
		}
		star, ok := fd.Recv.List[0].Type.(*ast.StarExpr)
		if !ok {
			continue
		}
		if ident, ok := star.X.(*ast.Ident); ok && ident.Name == "managedTurnRunner" {
			runFn = fd
			break
		}
	}
	if runFn == nil {
		t.Fatal("managedTurnRunner.Run has moved, been renamed, or changed receiver; this guard needs rewriting")
	}

	found := false
	var walk func(stmts []ast.Stmt, dead bool)
	walk = func(stmts []ast.Stmt, dead bool) {
		for _, stmt := range stmts {
			switch s := stmt.(type) {
			case *ast.IfStmt:
				branchDead := dead || isLiteralFalse(s.Cond)
				walk(s.Body.List, branchDead)
				switch e := s.Else.(type) {
				case *ast.BlockStmt:
					walk(e.List, dead) // the else of `if false` is reachable, not dead
				case *ast.IfStmt:
					walk([]ast.Stmt{e}, dead)
				}
			case *ast.BlockStmt:
				walk(s.List, dead)
			case *ast.AssignStmt:
				if !dead {
					for _, rhs := range s.Rhs {
						if isWithRedactorOfRedactorField(rhs) {
							found = true
						}
					}
				}
			case *ast.ExprStmt:
				if !dead && isWithRedactorOfRedactorField(s.X) {
					found = true
				}
			}
		}
	}
	walk(runFn.Body.List, false)

	if !found {
		t.Error("managedTurnRunner.Run no longer has a LIVE (reachable, uncommented) call to " +
			"tools.WithRedactor(ctx, r.o.redactor) -- without it, a managed sub-agent turn only " +
			"gets a redactor by accident, via runSubAgentTurn's inline fallback branch, which is " +
			"not reachable when a Manager/factory IS bound on ctx (the actual production shape " +
			"for agent_spawn/agent_resume). This check walks the AST specifically so a binding " +
			"that is commented out or wrapped in `if false { ... }` is correctly reported as absent.")
	}
}

// isLiteralFalse reports whether e is exactly the identifier `false` --
// enough to catch `if false { ... }`, the literal mutation this guard exists
// to detect. It does not attempt general constant folding.
func isLiteralFalse(e ast.Expr) bool {
	ident, ok := e.(*ast.Ident)
	return ok && ident.Name == "false"
}

// isWithRedactorOfRedactorField reports whether e is a call of the shape
// tools.WithRedactor(<anything>, <anything>.redactor) -- the second argument
// must end in a `.redactor` field selector, matching r.o.redactor without
// hardcoding the exact receiver chain.
func isWithRedactorOfRedactorField(e ast.Expr) bool {
	call, ok := e.(*ast.CallExpr)
	if !ok || len(call.Args) != 2 {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "WithRedactor" {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	if !ok || pkg.Name != "tools" {
		return false
	}
	argSel, ok := call.Args[1].(*ast.SelectorExpr)
	return ok && argSel.Sel.Name == "redactor"
}
