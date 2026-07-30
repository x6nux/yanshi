package guard

import (
	"testing"

	"github.com/x6nux/yanshi/internal/execpolicy"
)

// TestGuardExecPolicyMapsRuleIDAndHardDeny (Task 6): execpolicy allow/deny
// outcomes are surfaced via Decision.RuleID/Justification while preserving the
// "ordinary go test should pass" semantics fixed in Task 5.
func TestGuardExecPolicyMapsRuleIDAndHardDeny(t *testing.T) {
	p := PermissionProfile{
		Tools: ToolsPerm{Allow: []string{"shell_run"}},
		Shell: ShellPerm{Rules: []execpolicy.Rule{
			{ID: "go-test", Program: "go", Prefix: []string{"test"}, Decision: "allow", Justification: "ordinary tests"},
			{ID: "no-real", Program: "go", Prefix: []string{"test"}, Decision: "deny", DenyFlags: []string{"-tags=e2e_real"}, Justification: "real E2E gated"},
		}},
	}
	allow := New().Check(p, Action{Tool: "shell_run", Shell: "go test ./internal/tools"})
	if allow.Verdict != Allow || allow.RuleID != "go-test" {
		t.Fatalf("allow=%#v", allow)
	}
	deny := New().Check(p, Action{Tool: "shell_run", Shell: "go test -tags=e2e_real ./internal/acp"})
	if deny.Verdict != HardDeny || deny.RuleID != "no-real" || deny.Promptable {
		t.Fatalf("deny=%#v", deny)
	}
}

// TestGuardExecPolicyParserFailureIsHardDeny: when execpolicy.Parse rejects a
// command (e.g. forbidden expansion), the guard must HardDeny with RuleID
// "parse-error" so an interactive callback cannot override it.
func TestGuardExecPolicyParserFailureIsHardDeny(t *testing.T) {
	p := PermissionProfile{
		Tools: ToolsPerm{Allow: []string{"shell_run"}},
		Shell: ShellPerm{Rules: []execpolicy.Rule{{ID: "printf", Program: "printf", Decision: "allow"}}},
	}
	dec := New().Check(p, Action{Tool: "shell_run", Shell: `printf ${IFS}`})
	if dec.Verdict != HardDeny || dec.RuleID != "parse-error" {
		t.Fatalf("parse failure=%#v", dec)
	}
}
