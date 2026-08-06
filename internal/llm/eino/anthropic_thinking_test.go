package eino

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

// TestAnthropicRequestCarriesThinkingBudget pins the provider half of UX8.
//
// Everything above the provider was real: /think sets an effort, the
// orchestrator forwards it, the classifier emits thinking frames and the TUI
// folds them under Ctrl+O. And anthropicRequest had no thinking field at all —
// `budget_tokens` did not appear anywhere in the repository. ReasoningEffort
// maps only onto openai.WithReasoningEffort, which the Anthropic adapter never
// decodes, so /think high on a Claude model changed nothing on the wire while
// every layer above it behaved as though it had.
func TestAnthropicRequestCarriesThinkingBudget(t *testing.T) {
	m := &AnthropicModel{config: AnthropicModelConfig{Model: "claude-opus-4-8", MaxTokens: 8000}}
	in := []*schema.Message{schema.UserMessage("hi")}

	t.Run("no effort leaves the field absent", func(t *testing.T) {
		req, err := m.buildRequest(in, &model.Options{}, &outputSchemaOptions{}, false)
		if err != nil {
			t.Fatal(err)
		}
		body, err := json.Marshal(req)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(body), "thinking") {
			t.Fatalf("a text-mode turn must be byte-identical to before: %s", body)
		}
	})

	t.Run("an effort turns thinking on with a budget", func(t *testing.T) {
		req, err := m.buildRequest(in, &model.Options{},
			&outputSchemaOptions{ThinkingEffort: "high"}, false)
		if err != nil {
			t.Fatal(err)
		}
		body, err := json.Marshal(req)
		if err != nil {
			t.Fatal(err)
		}
		var got struct {
			MaxTokens int `json:"max_tokens"`
			Thinking  *struct {
				Type         string `json:"type"`
				BudgetTokens int    `json:"budget_tokens"`
			} `json:"thinking"`
		}
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatal(err)
		}
		if got.Thinking == nil {
			t.Fatalf("no thinking object on the wire: %s", body)
		}
		if got.Thinking.Type != "enabled" {
			t.Errorf("thinking.type = %q, want enabled", got.Thinking.Type)
		}
		if got.Thinking.BudgetTokens <= 0 {
			t.Errorf("budget_tokens = %d: thinking with no budget is rejected by the API",
				got.Thinking.BudgetTokens)
		}
		// Anthropic requires max_tokens > budget_tokens: the budget is taken
		// OUT of max_tokens, so a budget at or above it leaves no room for an
		// answer and the API rejects the request.
		if got.Thinking.BudgetTokens >= got.MaxTokens {
			t.Errorf("budget_tokens %d >= max_tokens %d: the API rejects this, and the "+
				"symptom is a 400 on every thinking turn",
				got.Thinking.BudgetTokens, got.MaxTokens)
		}
	})

	t.Run("higher effort buys a larger budget", func(t *testing.T) {
		budget := func(effort string) int {
			req, err := m.buildRequest(in, &model.Options{},
				&outputSchemaOptions{ThinkingEffort: effort}, false)
			if err != nil {
				t.Fatal(err)
			}
			if req.Thinking == nil {
				t.Fatalf("effort %q produced no thinking block", effort)
			}
			return req.Thinking.BudgetTokens
		}
		low, medium, high := budget("low"), budget("medium"), budget("high")
		if !(low < medium && medium < high) {
			t.Errorf("budgets are not monotonic: low=%d medium=%d high=%d — the effort "+
				"knob would then be cosmetic", low, medium, high)
		}
	})
}

// TestThinkingOptionReachesTheAnthropicAdapter covers the plumbing that the
// unit test above cannot: the option has to survive GetImplSpecificOptions.
//
// This is where a second impl-option struct would fail silently.
// GetImplSpecificOptions type-asserts each option's setter against func(*T)
// and SKIPS the ones that do not match, so a thinking option carried on its
// own struct would be invisible to the decoder that reads outputSchemaOptions
// — no error, no warning, just a field that is never set.
func TestThinkingOptionReachesTheAnthropicAdapter(t *testing.T) {
	opt := ThinkingOption("medium")
	if opt == nil {
		t.Fatal("ThinkingOption returned nil for a valid effort")
	}
	got := model.GetImplSpecificOptions(&outputSchemaOptions{}, *opt)
	if got.ThinkingEffort != "medium" {
		t.Fatalf("the option did not survive decoding: %+v — a separate options "+
			"struct is invisible to this decoder", got)
	}

	// A schema option and a thinking option must both survive together: they
	// share one struct precisely so this works.
	schemaOpt := OutputSchemaOption(json.RawMessage(`{"type":"object"}`))
	if schemaOpt == nil {
		t.Fatal("OutputSchemaOption returned nil")
	}
	both := model.GetImplSpecificOptions(&outputSchemaOptions{}, *opt, *schemaOpt)
	if both.ThinkingEffort != "medium" || len(both.Schema) == 0 {
		t.Fatalf("options collided: %+v", both)
	}

	if ThinkingOption("") != nil || ThinkingOption("off") != nil {
		t.Error("an absent or off effort must produce no option at all")
	}
}

// TestThinkingOptionHasAProductionCaller is the wiring assertion.
//
// The unit tests above prove the option decodes and the request carries the
// field; neither can tell whether anything sends it. That distinction is the
// exact shape of the defect being fixed — every layer above the provider was
// already correct, and the one missing piece was that nothing reached the
// provider. A test that only drove buildRequest would have been green on the
// broken code too.
func TestThinkingOptionHasAProductionCaller(t *testing.T) {
	fset := token.NewFileSet()
	src, err := os.ReadFile(filepath.Join("..", "..", "agent", "orchestrator", "orchestrator.go"))
	if err != nil {
		t.Fatal(err)
	}
	af, err := parser.ParseFile(fset, "orchestrator.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	ast.Inspect(af, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if ok && sel.Sel.Name == "ThinkingOption" {
			found = true
		}
		return true
	})
	if !found {
		t.Error("nothing in the orchestrator calls einollm.ThinkingOption: the request " +
			"field exists and no turn ever sets it")
	}
}
