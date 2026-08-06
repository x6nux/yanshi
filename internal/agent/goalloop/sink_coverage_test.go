package goalloop

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// usageModel is a BaseChatModel that reports a fixed usage on every call, so a
// component that forwards it can be told apart from one that does not.
type usageModel struct{ reply string }

func (m usageModel) msg() *schema.Message {
	out := schema.AssistantMessage(m.reply, nil)
	out.ResponseMeta = &schema.ResponseMeta{
		Usage: &schema.TokenUsage{PromptTokens: 30, CompletionTokens: 10, TotalTokens: 40},
	}
	return out
}

func (m usageModel) Generate(context.Context, []*schema.Message, ...model.Option) (*schema.Message, error) {
	return m.msg(), nil
}

func (m usageModel) Stream(context.Context, []*schema.Message, ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	return schema.StreamReaderFromArray([]*schema.Message{m.msg()}), nil
}

// TestEveryLLMComponentReportsToTheSharedSink walks the components one by one.
//
// "planner/evaluators/tier 全接 Sink" was asserted of the LOOP: a budget test
// pre-charges the sink and checks the loop stops. That holds regardless of
// which components report, so a component silently dropped from the wiring —
// or one whose Sink field is set and never read — would cost real tokens that
// no budget ever sees, and every existing test would stay green.
//
// Each sub-test drives ONE component with its own sink, so a failure names the
// component rather than the loop.
//
// ledger: B0/TD1#3 planner/evaluator/tier 的模型调用都进同一个 sink
func TestEveryLLMComponentReportsToTheSharedSink(t *testing.T) {
	ctx := context.Background()
	m := usageModel{reply: "quick-fix"}

	t.Run("planner", func(t *testing.T) {
		sink := &UsageSink{}
		planModel := usageModel{reply: `{"goal":"g","tests":[],"steps":["s1"]}`}
		plan, err := LLMPlanner{Model: planModel, Sink: sink}.Plan(ctx, Goal{Text: "do a thing"})
		require.NoError(t, err)
		require.NotEmpty(t, plan.Steps)
		assert.Positive(t, sink.Snapshot().Total(),
			"LLMPlanner spent tokens the shared sink never saw")
	})

	t.Run("planner charges even when the reply is unparseable", func(t *testing.T) {
		// addUsage runs BEFORE the JSON parse, and it has to: a model that
		// answered with prose still billed for the call. Reporting only on
		// successfully parsed plans lets a model stuck emitting garbage burn
		// the budget without ever touching it.
		sink := &UsageSink{}
		_, err := LLMPlanner{Model: m, Sink: sink}.Plan(ctx, Goal{Text: "do a thing"})
		require.Error(t, err, "the fixture must be unparseable for this to test anything")
		assert.Positive(t, sink.Snapshot().Total(),
			"a planner call that failed to parse was not charged; a model emitting garbage "+
				"burns the budget without the budget noticing")
	})

	t.Run("intent evaluator", func(t *testing.T) {
		sink := &UsageSink{}
		_, _ = IntentEvaluator{Model: m, Sink: sink}.Evaluate(ctx,
			Goal{Text: "do a thing"}, Plan{Steps: []string{"s"}}, ".")
		assert.Positive(t, sink.Snapshot().Total(),
			"IntentEvaluator spent tokens the shared sink never saw")
	})

	t.Run("quality evaluator", func(t *testing.T) {
		sink := &UsageSink{}
		_, _ = QualityEvaluator{Model: m, Sink: sink}.Evaluate(ctx,
			Goal{Text: "do a thing"}, Plan{Steps: []string{"s"}}, ".")
		assert.Positive(t, sink.Snapshot().Total(),
			"QualityEvaluator spent tokens the shared sink never saw")
	})

	t.Run("tierer", func(t *testing.T) {
		sink := &UsageSink{}
		_, _ = LLMTierer{Model: m, Sink: sink}.Tier(ctx, "add a field")
		assert.Positive(t, sink.Snapshot().Total(),
			"LLMTierer spent tokens the shared sink never saw")
	})

	t.Run("EvaluatorsForTier passes the sink through", func(t *testing.T) {
		// The wiring point: a tier whose evaluators are built without the sink
		// would leave every one of them unmetered, and the per-component tests
		// above would still pass because they construct their own.
		sink := &UsageSink{}
		for _, tier := range []Tier{TierQuickFix, TierStandard, TierDesigned, TierTeam, TierAutonomous} {
			for _, e := range EvaluatorsForTier(tier, m, sink) {
				switch ev := e.(type) {
				case IntentEvaluator:
					assert.Samef(t, sink, ev.Sink,
						"tier %v builds an IntentEvaluator with a different sink", tier)
				case QualityEvaluator:
					assert.Samef(t, sink, ev.Sink,
						"tier %v builds a QualityEvaluator with a different sink", tier)
				}
			}
		}
		// And at least one tier really does get a metered evaluator, so the
		// loop above is not vacuously true.
		var sawMetered bool
		for _, e := range EvaluatorsForTier(TierAutonomous, m, sink) {
			switch e.(type) {
			case IntentEvaluator, QualityEvaluator:
				sawMetered = true
			}
		}
		require.True(t, sawMetered,
			"no tier builds a model-calling evaluator, so the assertions above check nothing")
	})
}

// TestNoLLMComponentLacksASinkField is the structural half.
//
// The per-component tests can only cover components someone remembered to
// list. This one parses the package and requires every struct that holds a
// model.BaseChatModel to hold a *UsageSink too — a new LLM-calling component
// added without one is unmetered by construction, and nothing else would say
// so.
//
// ledger: B0/TD1#3 planner/evaluator/tier 的模型调用都进同一个 sink
func TestNoLLMComponentLacksASinkField(t *testing.T) {
	fset := token.NewFileSet()
	files, err := filepath.Glob("*.go")
	require.NoError(t, err)

	var offenders []string
	for _, f := range files {
		if len(f) > 8 && f[len(f)-8:] == "_test.go" {
			continue
		}
		parsed, err := parser.ParseFile(fset, f, nil, 0)
		require.NoError(t, err)
		ast.Inspect(parsed, func(n ast.Node) bool {
			ts, ok := n.(*ast.TypeSpec)
			if !ok {
				return true
			}
			st, ok := ts.Type.(*ast.StructType)
			if !ok || st.Fields == nil {
				return true
			}
			var hasModel, hasSink bool
			for _, field := range st.Fields.List {
				switch typ := field.Type.(type) {
				case *ast.SelectorExpr:
					if pkg, ok := typ.X.(*ast.Ident); ok &&
						pkg.Name == "model" && typ.Sel.Name == "BaseChatModel" {
						hasModel = true
					}
				case *ast.StarExpr:
					if id, ok := typ.X.(*ast.Ident); ok && id.Name == "UsageSink" {
						hasSink = true
					}
				}
			}
			if hasModel && !hasSink {
				offenders = append(offenders, ts.Name.Name+" ("+f+")")
			}
			return true
		})
	}

	assert.Emptyf(t, offenders,
		"these types hold a chat model and no *UsageSink, so every token they spend is "+
			"invisible to the goal budget: %v", offenders)
}
