package tools

import (
	"context"
	"strings"
	"testing"

	"github.com/x6nux/yanshi/internal/guard"
	"github.com/x6nux/yanshi/internal/toolreg"
)

func wfs23Spec() ClientToolSpec {
	return ClientToolSpec{
		Name:        "client_ping",
		Description: "Ping the client host and return its latency.",
		Parameters:  []byte(`{"type":"object","properties":{"target":{"type":"string"}},"required":["target"]}`),
	}
}

// TestWFS23SpecValidation pins the untrusted-input gate: the name namespace is
// physically separated (client_ prefix, lowercase identifier — a built-in name
// cannot be impersonated), description and schema are size-capped (the schema
// goes into model requests; an uncapped one is a free context bomb), and the
// parameters must be a JSON object.
func TestWFS23SpecValidation(t *testing.T) {
	ok := wfs23Spec()
	if err := ok.Validate(); err != nil {
		t.Fatalf("valid spec rejected: %v", err)
	}

	for name, mutate := range map[string]func(*ClientToolSpec){
		"missing prefix":   func(s *ClientToolSpec) { s.Name = "fs_read" },
		"empty name":       func(s *ClientToolSpec) { s.Name = "" },
		"bare prefix":      func(s *ClientToolSpec) { s.Name = "client_" },
		"uppercase":        func(s *ClientToolSpec) { s.Name = "client_Ping" },
		"leading digit":    func(s *ClientToolSpec) { s.Name = "client_1ping" },
		"description bomb": func(s *ClientToolSpec) { s.Description = strings.Repeat("x", maxClientToolDescBytes+1) },
		"schema bomb": func(s *ClientToolSpec) {
			s.Parameters = []byte(`{"x":"` + strings.Repeat("y", maxClientToolSchemaBytes) + `"}`)
		},
		"non-object params": func(s *ClientToolSpec) { s.Parameters = []byte(`[1,2,3]`) },
		"garbage params":    func(s *ClientToolSpec) { s.Parameters = []byte(`{not json`) },
	} {
		t.Run(name, func(t *testing.T) {
			spec := wfs23Spec()
			mutate(&spec)
			if err := spec.Validate(); err == nil {
				t.Fatalf("spec must be rejected")
			}
		})
	}

	t.Run("no-parameters spec is valid", func(t *testing.T) {
		spec := wfs23Spec()
		spec.Parameters = nil
		if err := spec.Validate(); err != nil {
			t.Fatalf("no-arg spec rejected: %v", err)
		}
	})
}

// TestWFS23ClientToolRoundTrip drives the built tool through its real
// InvokableRun: the invoke callback receives the model's args JSON, its text
// comes back as the result, and its error comes back as an error result — the
// same GuardedTool pipeline as built-in tools, not a second one.
func TestWFS23ClientToolRoundTrip(t *testing.T) {
	var gotArgs string
	tool, err := NewClientTool(wfs23Spec(), func(ctx context.Context, argsJSON string) (string, error) {
		gotArgs = argsJSON
		return "pong 42ms", nil
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	ctx := WithProfile(WithErrCounter(context.Background()), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"client_*"}},
	})
	out, err := tool.InvokableRun(ctx, `{"target":"host"}`)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if gotArgs != `{"target":"host"}` {
		t.Fatalf("invoke received %q", gotArgs)
	}
	if !strings.Contains(out, "pong 42ms") {
		t.Fatalf("result missing client text: %q", out)
	}

	// Error path: the client's failure becomes an error result, not a turn
	// abort — and does NOT trip the breaker (like any operational failure it
	// does, but the point here is the text made it back).
	tool2, _ := NewClientTool(wfs23Spec(), func(ctx context.Context, argsJSON string) (string, error) {
		return "", context.DeadlineExceeded
	})
	out2, _ := tool2.InvokableRun(ctx, `{}`)
	if !strings.Contains(out2, "✗") {
		t.Fatalf("client failure must surface as an error result: %q", out2)
	}
}

// TestWFS23UnregisteredNameIsRefusedSilently is THE acceptance clause: with a
// toolreg set bound (as every production turn has), an INJECTED name passes
// the runtime check while a name that was never injected is refused
// structurally — before any callback could be consulted, so there is no dialog
// to click. The two denial texts differ ("unregistered tool" vs the generic
// permission denial), which is what makes "no dialog" observable.
func TestWFS23UnregisteredNameIsRefusedSilently(t *testing.T) {
	ctx := toolreg.WithRegistered(WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"client_*"}},
	}), []string{"client_ping", "fs_read"})

	if err := Authorize(ctx, guard.Action{Tool: "client_ping"}, `{}`); err != nil {
		t.Fatalf("injected+registered name must pass toolreg: %v", err)
	}
	err := Authorize(ctx, guard.Action{Tool: "client_pear"}, `{}`)
	if err == nil {
		t.Fatal("a name that was never injected must be refused")
	}
	if !strings.Contains(err.Error(), "unregistered tool") {
		t.Fatalf("refusal must be the structural toolreg denial, got %v", err)
	}
	// The structural denial happens BEFORE the profile lookup: bind NO
	// profile at all and the injected name's authorization still reports the
	// generic profile denial — never an unregistered refusal, and never a
	// callback consult (none is bound; a Prompt path would fail closed with
	// the generic text, which the assertion below distinguishes).
	bare := toolreg.WithRegistered(context.Background(), []string{"client_ping"})
	if err := Authorize(bare, guard.Action{Tool: "client_ping"}, `{}`); err == nil ||
		!strings.Contains(err.Error(), "no permission profile") {
		t.Fatalf("registered name without profile must report the generic denial, got %v", err)
	}
}

// TestWFS23NilInvokeRejected keeps the honest-failure rule: a tool without an
// invoke callback must not be constructible — calling it would hang or lie.
func TestWFS23NilInvokeRejected(t *testing.T) {
	if _, err := NewClientTool(wfs23Spec(), nil); err == nil {
		t.Fatal("nil invoke must be rejected at construction")
	}
}
