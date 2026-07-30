package guard

import (
	"testing"

	"github.com/x6nux/yanshi/internal/execpolicy"
)

func TestDecisionVerdictOrderAndPromptable(t *testing.T) {
	if HardDeny == Allow || HardDeny == Prompt || Prompt == Allow {
		t.Fatal("verdicts must be distinct")
	}
	// The typed constructors are the single source of truth for Promptable.
	// A bare Decision{Verdict: X} literal must NOT auto-fill Promptable —
	// the firewall depends on Promptable being set explicitly only by the
	// allow/prompt/hardDeny helpers.
	if (Decision{Verdict: HardDeny}).Promptable {
		t.Fatal("HardDeny must never be Promptable")
	}
	if !(prompt("x")).Promptable {
		t.Fatal("prompt() helper must mark Promptable")
	}
	if !(prompt("x").Verdict == Prompt) {
		t.Fatal("prompt() helper must set Verdict=Prompt")
	}
	if (allow()).Promptable {
		t.Fatal("allow() helper must not mark Promptable")
	}
	if (hardDeny("x")).Promptable {
		t.Fatal("hardDeny() helper must not mark Promptable")
	}
	if (hardDeny("x")).Overridable {
		t.Fatal("hardDeny() helper must not mark Overridable (structural floor)")
	}
	if !(overridableDeny("x")).Overridable {
		t.Fatal("overridableDeny() helper must mark Overridable")
	}
	if (overridableDeny("x")).Promptable {
		t.Fatal("overridableDeny() helper must not mark Promptable")
	}
	if (overridableDeny("x")).Verdict != HardDeny {
		t.Fatal("overridableDeny() helper must set Verdict=HardDeny")
	}
}

func TestDecisionIsAllowed(t *testing.T) {
	if !(Decision{Verdict: Allow}.IsAllowed()) {
		t.Fatal("IsAllowed() must be true for Verdict=Allow")
	}
	if (Decision{Verdict: Prompt}.IsAllowed()) {
		t.Fatal("IsAllowed() must be false for Verdict=Prompt")
	}
	if (Decision{Verdict: HardDeny}.IsAllowed()) {
		t.Fatal("IsAllowed() must be false for Verdict=HardDeny")
	}
}

func TestCheckEmptyToolsAllowIsOverridableHardDeny(t *testing.T) {
	g := New()
	dec := g.Check(PermissionProfile{}, Action{Tool: "shell_run"})
	if dec.Verdict != HardDeny || dec.Promptable || !dec.Overridable {
		t.Fatalf("empty Tools.Allow must be HardDeny + Overridable (profile default), got %#v", dec)
	}
}

func TestCheckShellMetacharIsHardDeny(t *testing.T) {
	g := New()
	prof := PermissionProfile{
		Tools: ToolsPerm{Allow: []string{"shell_run"}},
		Shell: ShellPerm{Policy: "allowlist", Patterns: []string{"*"}},
	}
	dec := g.Check(prof, Action{Tool: "shell_run", Shell: "ls; rm -rf /"})
	if dec.Verdict != HardDeny || dec.Promptable || dec.Overridable {
		t.Fatalf("metachar command must be structural HardDeny (not Overridable), got %#v", dec)
	}
}

// TestCheckProfilePolicyDeniesAreOverridable proves every profile-configured
// deny is Overridable so YOLO/Auto may set it aside: empty allowlists, shell
// policy="deny", net.allow=false, a denylist match, and execpolicy hard_deny
// (including the unmatched-segment default). Only non-profile structural
// guards stay non-Overridable (see TestCheckStructuralDeniesAreNotOverridable).
func TestCheckProfilePolicyDeniesAreOverridable(t *testing.T) {
	g := New()
	cases := []struct {
		name string
		prof PermissionProfile
		act  Action
	}{
		{
			name: "shell policy=deny",
			prof: PermissionProfile{Tools: ToolsPerm{Allow: []string{"shell_run"}}, Shell: ShellPerm{Policy: "deny"}},
			act:  Action{Tool: "shell_run", Shell: "ls"},
		},
		{
			name: "net allow=false",
			prof: PermissionProfile{Tools: ToolsPerm{Allow: []string{"web_fetch"}}, Net: NetPerm{Allow: false}},
			act:  Action{Tool: "web_fetch", NetHost: "example.com"},
		},
		{
			name: "empty fs write list",
			prof: PermissionProfile{Tools: ToolsPerm{Allow: []string{"fs_write"}}, FS: FSPerm{Read: []string{"**"}}},
			act:  Action{Tool: "fs_write", FS: FSWant{Op: "write", Paths: []string{"/x"}}},
		},
		{
			name: "denylist match",
			prof: PermissionProfile{Tools: ToolsPerm{Allow: []string{"shell_run"}}, Shell: ShellPerm{Policy: "denylist", Patterns: []string{"git push *"}}},
			act:  Action{Tool: "shell_run", Shell: "git push --force"},
		},
		{
			name: "execpolicy hard_deny (unmatched)",
			prof: PermissionProfile{Tools: ToolsPerm{Allow: []string{"shell_run"}}, Shell: ShellPerm{Rules: []execpolicy.Rule{{ID: "go-version", Program: "go", Prefix: []string{"version"}, Decision: "allow"}}}},
			act:  Action{Tool: "shell_run", Shell: "rm foo", Workdir: "/proj"},
		},
		{
			name: "empty MCP allowlist",
			prof: PermissionProfile{Tools: ToolsPerm{Allow: []string{"*"}}},
			act:  Action{Tool: "mcp_srv_tool"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dec := g.Check(c.prof, c.act)
			if dec.Verdict != HardDeny || !dec.Overridable {
				t.Fatalf("%s: expected Overridable HardDeny, got %#v", c.name, dec)
			}
		})
	}
}

// TestCheckStructuralDeniesAreNotOverridable proves the floor that holds even
// under YOLO/Auto: shell metachars (injection guard), unknown policy (misconfig),
// and execpolicy parse-error (malformed syntax). These are command-safety guards,
// not profile policy, so they stay non-Overridable.
func TestCheckStructuralDeniesAreNotOverridable(t *testing.T) {
	g := New()
	cases := []struct {
		name string
		prof PermissionProfile
		act  Action
	}{
		{
			name: "unknown policy",
			prof: PermissionProfile{Tools: ToolsPerm{Allow: []string{"shell_run"}}, Shell: ShellPerm{Policy: "bogus"}},
			act:  Action{Tool: "shell_run", Shell: "ls"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dec := g.Check(c.prof, c.act)
			if dec.Verdict != HardDeny || dec.Overridable {
				t.Fatalf("%s: expected structural (non-Overridable) HardDeny, got %#v", c.name, dec)
			}
		})
	}
}
