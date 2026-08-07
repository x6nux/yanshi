package main

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/agent/goalloop"
	einollm "github.com/x6nux/yanshi/internal/llm/eino"
)

// TestAutoTierAsksTheModelAndOutranksTheKeywordTable pins the wiring that made
// LLMTierer reachable.
//
// LLMTierer was written, tested and given its own UsageSink field, and had zero
// production call sites: -tier auto ran RuleTierer and nothing else, so the
// classifier — and the G02 sink wiring inside it — could never execute outside
// its own unit tests. The keyword table cannot distinguish "fix the typo in the
// migration that corrupts every row" from "fix the typo in the README", and the
// tier picks the execution path, the evaluator set and the skill body.
func TestAutoTierAsksTheModelAndOutranksTheKeywordTable(t *testing.T) {
	// "fix a typo" is the RuleTierer's clearest quick-fix signal, so a result
	// of anything else can only have come from the model.
	const text = "fix a typo in the migration"
	ruleTier, _ := goalloop.RuleTierer{}.Tier(context.Background(), text)
	require.Equal(t, goalloop.TierQuickFix, ruleTier, "fixture no longer exercises the disagreement")

	m := einollm.NewFakeModel([]string{"autonomous"}, nil)
	sink := &goalloop.UsageSink{}

	got := refineTierWithModel(context.Background(), m, text, ruleTier, sink)
	require.Equal(t, goalloop.TierAutonomous, got,
		"the model's answer must win over the keyword table, or -tier auto is still just RuleTierer")
	require.Equal(t, 1, m.GenerateCalls, "the classifier never called the model")
	require.NotZero(t, sink.Snapshot().Total,
		"the classification call is unmetered, so the run's token budget under-reports by exactly it")
}

// TestAutoTierFallsBackToTheKeywordTable covers the two ways the model can fail
// to answer. Both must land on the rule tier rather than on TierStandard: a
// silent downgrade of an autonomous goal to a single standard turn is the
// failure mode that would be hardest to notice from the outside.
func TestAutoTierFallsBackToTheKeywordTable(t *testing.T) {
	const text = "build an autonomous multi-agent system from scratch"
	ruleTier, _ := goalloop.RuleTierer{}.Tier(context.Background(), text)

	t.Run("no model wired", func(t *testing.T) {
		require.Equal(t, ruleTier, refineTierWithModel(context.Background(), nil, text, ruleTier, nil))
	})

	t.Run("reply names no known tier", func(t *testing.T) {
		m := einollm.NewFakeModel([]string{"I'm afraid I can't do that"}, nil)
		require.Equal(t, ruleTier,
			refineTierWithModel(context.Background(), m, text, ruleTier, &goalloop.UsageSink{}))
		require.Equal(t, 1, m.GenerateCalls)
	})
}

// TestRunGoalCallsTheRefinerOnlyWhenTheTierIsNotForced pins the CALL SITE, not
// the function.
//
// The two tests above drive refineTierWithModel directly, so they stay green
// against a runGoal that never calls it — which is precisely the shape that
// left LLMTierer unreachable for the whole of S0 (written, tested, wired to a
// sink, zero callers). runGoal needs a real bootstrap to execute, so the call
// site is checked structurally instead: it must exist, and it must sit under a
// `!forced` guard, because -tier t0..t4 is the user overriding the classifier
// and spending a model call to be overruled is worse than not spending it.
func TestRunGoalCallsTheRefinerOnlyWhenTheTierIsNotForced(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "main.go", nil, 0)
	require.NoError(t, err)

	var guarded, total int
	ast.Inspect(f, func(n ast.Node) bool {
		ifs, ok := n.(*ast.IfStmt)
		if !ok {
			return true
		}
		calls := countCallsTo(ifs.Body, "refineTierWithModel")
		total += calls
		if calls == 0 {
			return true
		}
		un, ok := ifs.Cond.(*ast.UnaryExpr)
		if ok && un.Op == token.NOT {
			if id, ok := un.X.(*ast.Ident); ok && id.Name == "forced" {
				guarded += calls
			}
		}
		return true
	})

	require.Equal(t, 1, total,
		"refineTierWithModel has no call site inside an if statement in main.go — "+
			"-tier auto is back to running RuleTierer and nothing else")
	require.Equal(t, 1, guarded,
		"the call is not guarded by !forced, so an explicit -tier t0..t4 pays for a "+
			"classification whose answer is discarded")
}

// countCallsTo reports how many times name is called anywhere inside n.
func countCallsTo(n ast.Node, name string) int {
	count := 0
	ast.Inspect(n, func(x ast.Node) bool {
		call, ok := x.(*ast.CallExpr)
		if !ok {
			return true
		}
		if id, ok := call.Fun.(*ast.Ident); ok && id.Name == name {
			count++
		}
		return true
	})
	return count
}
