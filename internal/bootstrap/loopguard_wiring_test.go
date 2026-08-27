package bootstrap_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"reflect"
	"sort"
	"testing"

	"github.com/x6nux/yanshi/internal/config"
)

// TestLoopGuardConfigReachesTheOrchestrator pins the composition root's half of
// the per-turn stop conditions.
//
// internal/loopguard tests the gates and internal/agent/orchestrator tests that
// a LoopGuardConfig installs them, but NEITHER side notices if bootstrap stops
// forwarding the operator's YAML. The zero LoopGuardConfig is "every gate off",
// which is a legal, silent, fully-green state: an operator who configured
// max_tool_calls: 20 would get no budget, no warning, and no failing test.
// Measured before this test existed: replacing the RepetitionEnabled assignment
// with a literal false left internal/bootstrap, internal/agent/orchestrator and
// internal/archtest all green.
//
// The field list is derived by REFLECTION over config.LoopGuardConfig rather
// than hand-listed, so adding a field to that struct and forgetting to forward
// it turns this red on the next run. A hand-listed set would freeze at today's
// eight fields and quietly stop covering the ninth -- the exact failure mode of
// a checklist that has to be updated by the person it is meant to catch.
//
// Checked at the source because orchestrator.Config is consumed by Build and
// not retained on App: there is nothing to observe at runtime. What is asserted
// is narrow on purpose -- that each field's value comes from cfg.LoopGuard, not
// what the orchestrator later does with it.
func TestLoopGuardConfigReachesTheOrchestrator(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "bootstrap.go", nil, 0)
	if err != nil {
		t.Fatalf("parse bootstrap.go: %v", err)
	}

	// Collect "FieldName: cfg.LoopGuard.X" pairs from the composite literal
	// that builds orchestrator.LoopGuardConfig.
	forwarded := map[string]string{}
	ast.Inspect(file, func(n ast.Node) bool {
		lit, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		sel, ok := lit.Type.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "LoopGuardConfig" {
			return true
		}
		for _, elt := range lit.Elts {
			kv, ok := elt.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			key, ok := kv.Key.(*ast.Ident)
			if !ok {
				continue
			}
			// Expect the value to be cfg.LoopGuard.<Something>.
			val, ok := kv.Value.(*ast.SelectorExpr)
			if !ok {
				continue
			}
			inner, ok := val.X.(*ast.SelectorExpr)
			if !ok || inner.Sel.Name != "LoopGuard" {
				continue
			}
			root, ok := inner.X.(*ast.Ident)
			if !ok || root.Name != "cfg" {
				continue
			}
			forwarded[key.Name] = val.Sel.Name
		}
		return true
	})

	if len(forwarded) == 0 {
		t.Fatal("bootstrap.go no longer builds an orchestrator.LoopGuardConfig from cfg.LoopGuard: " +
			"every per-turn stop condition the operator configured is silently discarded, " +
			"and the zero value that replaces it means 'all gates off'")
	}

	var missing []string
	rt := reflect.TypeOf(config.LoopGuardConfig{})
	for i := 0; i < rt.NumField(); i++ {
		name := rt.Field(i).Name
		src, ok := forwarded[name]
		if !ok {
			missing = append(missing, name)
			continue
		}
		// Guard against a copy-paste swap: RepetitionWarnAfter must be fed by
		// cfg.LoopGuard.RepetitionWarnAfter, not by a sibling of the same type.
		// Two int fields crossed this way type-check and test green everywhere
		// else.
		if src != name {
			t.Errorf("orchestrator.LoopGuardConfig.%s is fed from cfg.LoopGuard.%s, not cfg.LoopGuard.%s: "+
				"the operator's value for one limit is being applied to another", name, src, name)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("config.LoopGuardConfig field(s) %v are never forwarded to the orchestrator: "+
			"the operator can set them in YAML and they take no effect. Add them to the "+
			"orchestrator.LoopGuardConfig literal in bootstrap.go.", missing)
	}
}
