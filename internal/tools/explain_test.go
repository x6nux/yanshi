package tools

import (
	"context"
	"strings"
	"testing"

	"github.com/x6nux/yanshi/internal/execpolicy"
	"github.com/x6nux/yanshi/internal/guard"
)

// TestExecPolicyJustificationReachesTheUser drives a real execpolicy rule
// through the real guard and asserts the operator's justification comes back
// in the error the caller actually receives.
//
// The end-to-end shape is the point. A package-local test that fills
// Decision.Justification and reads it back proves the field is assignable, not
// that anyone is told: before this, Justification had zero non-test readers
// outside internal/guard, and all four Authorize exits wrote only
// Decision.Reason. The rule engine's explanation existed and never left the
// building. Assert on DenyErr -- the thing a denied tool call surfaces -- so a
// future refactor that drops the explanation again fails here.
//
// ledger: A1/S06#2 规则结果可解释
func TestExecPolicyJustificationReachesTheUser(t *testing.T) {
	const justification = "real-CLI e2e tests cost money and need credentials"

	profile := guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"shell_run"}},
		Shell: guard.ShellPerm{Rules: []execpolicy.Rule{{
			ID:            "no-real-e2e",
			Program:       "go",
			Prefix:        []string{"test"},
			Decision:      "deny",
			DenyFlags:     []string{"-tags=e2e_real"},
			Justification: justification,
		}}},
	}

	ctx := WithProfile(context.Background(), profile)
	err := Authorize(ctx, guard.Action{
		Tool:  "shell_run",
		Shell: "go test -tags=e2e_real ./internal/acp",
	}, `{"command":"go test -tags=e2e_real ./internal/acp"}`)
	if err == nil {
		t.Fatal("a deny rule must produce an error")
	}
	if !strings.Contains(err.Error(), justification) {
		t.Fatalf("the rule's justification never reached the caller: %q", err.Error())
	}
}

// TestExplainDecisionShapes covers the three renderings explainDecision has to
// produce. Reason-only is the common case (most guard denials carry no
// execpolicy rule at all) and must stay byte-identical, because callers and
// tests across the tree match on those strings.
func TestExplainDecisionShapes(t *testing.T) {
	cases := []struct {
		name string
		dec  guard.Decision
		want string
	}{
		{"reason only", guard.Decision{Reason: "shell command not on allowlist"}, "shell command not on allowlist"},
		{"justification only", guard.Decision{Justification: "why"}, "why"},
		{"both", guard.Decision{Reason: "deny flag matched", Justification: "costs money"}, "deny flag matched (costs money)"},
		{"neither", guard.Decision{}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := explainDecision(tc.dec); got != tc.want {
				t.Fatalf("explainDecision = %q, want %q", got, tc.want)
			}
		})
	}
}
