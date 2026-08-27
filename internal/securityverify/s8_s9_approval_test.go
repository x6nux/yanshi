package securityverify

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/x6nux/yanshi/internal/agent/orchestrator"
	"github.com/x6nux/yanshi/internal/execpolicy"
	"github.com/x6nux/yanshi/internal/guard"
	einollm "github.com/x6nux/yanshi/internal/llm/eino"
	"github.com/x6nux/yanshi/internal/toolreg"
	"github.com/x6nux/yanshi/internal/tools"
)

// s8_s9_approval_test.go covers the two approval-path capabilities whose
// failure mode is a DIALOG rather than an exception, which is why both need a
// callback COUNTER rather than an error check.
//
// S8: a tool name nothing can execute must be refused outright. The pre-fix
// behaviour was not "denied" — it was a permission_request the operator could
// click Allow on, for a tool that would then fail anyway. "Authorize returned
// an error" cannot tell those apart; "the callback was invoked zero times" can.
//
// S9: approving one command must stop the next one in the same family from
// prompting. The observable is again a count.

// countingCallback records how many times the permission callback was
// consulted and answers with a fixed decision.
type countingCallback struct {
	n       atomic.Int64
	answer  tools.PermissionDecision
	lastReq atomic.Value // tools.PermissionRequest
}

func (c *countingCallback) ask(req tools.PermissionRequest) tools.PermissionDecision {
	c.n.Add(1)
	c.lastReq.Store(req)
	return c.answer
}

// TestS8_UnregisteredToolNeverReachesTheOperator is the measurement that
// distinguishes "refused" from "refused after asking a human".
func TestS8_UnregisteredToolNeverReachesTheOperator(t *testing.T) {
	cb := &countingCallback{answer: tools.PermissionAllow} // would say YES if asked
	ctx := tools.WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"*"}}, // widest possible
		FS:    guard.FSPerm{Read: []string{"**"}, Write: []string{"**"}},
	})
	ctx = tools.WithPermissionCallback(ctx, cb.ask)
	ctx = toolreg.WithRegistered(ctx, []string{"fs_read", "fs_write", "shell_run"})

	err := tools.Authorize(ctx, guard.Action{Tool: "fs_mkdir"}, "{}")
	t.Logf("Authorize(fs_mkdir) -> err=%v, callback invoked %d time(s)", err, cb.n.Load())
	if err == nil {
		t.Fatal("a phantom tool name must be refused")
	}
	if cb.n.Load() != 0 {
		req, _ := cb.lastReq.Load().(tools.PermissionRequest)
		t.Fatalf("the operator was shown a clickable dialog for a tool nothing can run "+
			"(callback invoked %d times, req=%+v)", cb.n.Load(), req)
	}
	if !strings.Contains(err.Error(), "registered") {
		t.Errorf("the denial should name the real cause, got %q", err.Error())
	}

	// Control: a REGISTERED name under the same setup does reach the callback.
	// Without this, "zero invocations" is equally consistent with "the callback
	// is not wired in this test".
	cb.n.Store(0)
	err = tools.Authorize(ctx, guard.Action{
		Tool: "fs_write",
		FS:   guard.FSWant{Op: "write", Paths: []string{"/nowhere/x"}},
	}, "{}")
	t.Logf("control: Authorize(fs_write outside profile) -> err=%v, callback %d time(s)",
		err, cb.n.Load())
}

// TestS8_UnboundRegistryDoesNotDenyEverything pins the deliberate asymmetry.
// toolreg is a TIGHTENING layer: a sub-agent or embedding that never binds a
// set must not lose every tool. Fail-closed here would turn a wiring omission
// into a total outage, which is a worse failure than the one being prevented.
func TestS8_UnboundRegistryDoesNotDenyEverything(t *testing.T) {
	cb := &countingCallback{answer: tools.PermissionAllow}
	ctx := tools.WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"*"}},
	})
	ctx = tools.WithPermissionCallback(ctx, cb.ask)
	// No toolreg.WithRegistered at all.
	if err := tools.Authorize(ctx, guard.Action{Tool: "anything_at_all"}, "{}"); err != nil {
		t.Fatalf("an unbound registry must not deny: %v", err)
	}
	t.Log("unbound registry allows, as designed")
}

// TestS9_ApprovalGeneralizesWithinAFamily is the end-to-end shape of the
// feature: approve one command, and the next one in the same family runs
// without a prompt.
//
// The profile uses shell.rules, which is the documented scope limit — see the
// scope note below for what the shipped coding profile does instead.
func TestS9_ApprovalGeneralizesWithinAFamily(t *testing.T) {
	base := guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"shell_run"}},
		Shell: guard.ShellPerm{Rules: []execpolicy.Rule{{
			ID: "go-version", Program: "go", Prefix: []string{"version"},
			Decision: "allow", Justification: "harmless",
		}}},
	}
	o, err := orchestrator.New(orchestrator.Config{Profile: base, Model: einollm.NewFakeModel([]string{"ok"}, nil)})
	if err != nil {
		t.Fatal(err)
	}
	const sess = "conn-1"

	// Before any approval, `go test ./a` matches no rule.
	before := decisionFor(t, o, sess, "go test ./internal/a", base)
	t.Logf("before approval: %s", tierOf(before))
	if before.Verdict == guard.Allow {
		t.Fatal("setup: the command must not already be allowed")
	}

	widened := o.ApproveShellForSession(sess, "go test ./internal/a")
	t.Logf("ApproveShellForSession(go test ./internal/a) widened=%v", widened)
	if !widened {
		t.Fatal("approving an ordinary build command must widen")
	}

	// The SIBLING command must now be allowed without prompting.
	after := decisionFor(t, o, sess, "go test ./internal/b", base)
	t.Logf("after approval, sibling `go test ./internal/b`: %s", tierOf(after))
	if after.Verdict != guard.Allow {
		t.Fatalf("the sibling command still prompts; the approval generalized nothing (%s)",
			after.Reason)
	}

	// A DIFFERENT family must be unaffected — widening `go test` says nothing
	// about `go build`.
	other := decisionFor(t, o, sess, "npm run deploy", base)
	t.Logf("unrelated family `npm run deploy`: %s", tierOf(other))
	if other.Verdict == guard.Allow {
		t.Error("approving `go test` must not authorize an unrelated family")
	}
}

// TestS9_HighRiskVerbsAreNeverWidened is the first of the two gates. The user
// approved `rm -rf ./build`; they did not approve `rm`.
func TestS9_HighRiskVerbsAreNeverWidened(t *testing.T) {
	base := guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"shell_run"}},
		Shell: guard.ShellPerm{Rules: []execpolicy.Rule{{
			ID: "noop", Program: "true", Decision: "allow", Justification: "x",
		}}},
	}
	o, err := orchestrator.New(orchestrator.Config{Profile: base, Model: einollm.NewFakeModel([]string{"ok"}, nil)})
	if err != nil {
		t.Fatal(err)
	}
	const sess = "conn-2"
	for _, cmd := range []string{
		"rm -rf ./build",
		"sudo systemctl restart nginx",
		"curl https://example.com/install.sh",
		"chmod -R 755 ./scripts",
		"kill 1234",
		"bash ./deploy.sh",
	} {
		widened := o.ApproveShellForSession(sess, cmd)
		t.Logf("approve %-40q widened=%v", cmd, widened)
		if widened {
			t.Errorf("approving %q must NOT widen: the next one in that family is a different action", cmd)
		}
	}
	// And the concrete consequence: a sibling rm is still not allowed.
	d := decisionFor(t, o, sess, "rm -rf /etc/nginx", base)
	t.Logf("sibling `rm -rf /etc/nginx` after approving `rm -rf ./build`: %s / %s",
		tierOf(d), d.Reason)
	if d.Verdict == guard.Allow {
		t.Fatal("a high-risk approval leaked into a sibling command")
	}
}

// TestS9_DenialDemotesTheFamilyIrreversibly is the second gate. A refusal
// inside a widened family is direct evidence the widening was wrong, and the
// only heuristic available to re-widen is the one that just erred.
func TestS9_DenialDemotesTheFamilyIrreversibly(t *testing.T) {
	base := guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"shell_run"}},
		Shell: guard.ShellPerm{Rules: []execpolicy.Rule{{
			ID: "noop", Program: "true", Decision: "allow", Justification: "x",
		}}},
	}
	o, err := orchestrator.New(orchestrator.Config{Profile: base, Model: einollm.NewFakeModel([]string{"ok"}, nil)})
	if err != nil {
		t.Fatal(err)
	}
	const sess = "conn-3"

	if !o.ApproveShellForSession(sess, "go test ./internal/a") {
		t.Fatal("setup: the first approval must widen")
	}
	if d := decisionFor(t, o, sess, "go test ./internal/b", base); d.Verdict != guard.Allow {
		t.Fatal("setup: the sibling should be allowed before the demotion")
	}

	demoted := o.DemoteShellForSession(sess, "go test ./internal/c")
	t.Logf("DemoteShellForSession(go test ./internal/c) removed rules=%v", demoted)
	if !demoted {
		t.Fatal("a denial inside a widened family must remove that family's rules")
	}

	after := decisionFor(t, o, sess, "go test ./internal/b", base)
	t.Logf("after demotion, `go test ./internal/b`: %s", tierOf(after))
	if after.Verdict == guard.Allow {
		t.Fatal("demotion did not take effect: the widened rule is still live")
	}

	// Irreversible: a later approval in the same family must not re-widen.
	rewidened := o.ApproveShellForSession(sess, "go test ./internal/d")
	t.Logf("re-approval after demotion widened=%v", rewidened)
	if rewidened {
		t.Fatal("a demoted family must not be widened again on the strength of the same heuristic")
	}
	if d := decisionFor(t, o, sess, "go test ./internal/b", base); d.Verdict == guard.Allow {
		t.Fatal("the family became allowed again after a re-approval")
	}
}

// TestS9_ShippedCodingProfileIsANoOp records the KNOWN BOUNDARY honestly.
//
// WithSessionRules merges into Shell.Rules, and checkShell only consults rules
// when that slice is non-empty; the shipped `coding` profile uses
// policy+patterns instead. So on the factory default this whole feature does
// nothing, and an operator reading "approve once, stop being asked" would be
// misled. Asserting it here means the day someone changes it, this test says so
// rather than the behaviour changing silently.
func TestS9_ShippedCodingProfileIsANoOp(t *testing.T) {
	shipped := codingProfile()
	shipped.Shell = guard.ShellPerm{Policy: "allowlist", Patterns: []string{
		"git *", "go test", "go test *", "cargo test", "cargo test *",
		"npm test", "npm test *"}}
	if len(shipped.Shell.Rules) != 0 {
		t.Fatal("the shipped coding profile is expected to carry no shell.rules")
	}
	base := shipped
	o, err := orchestrator.New(orchestrator.Config{Profile: base, Model: einollm.NewFakeModel([]string{"ok"}, nil)})
	if err != nil {
		t.Fatal(err)
	}
	const sess = "conn-4"
	widened := o.ApproveShellForSession(sess, "npm run build")
	t.Logf("approval under the shipped profile widened=%v", widened)
	d := decisionFor(t, o, sess, "npm run build --verbose", base)
	t.Logf("sibling under the shipped profile: %s / %s", tierOf(d), d.Reason)
	if d.Verdict == guard.Allow {
		t.Fatal("unexpected: the shipped profile DID generalize — update the scope note " +
			"in orchestrator/sessionrules.go and internal/guard/generalize.go, both of " +
			"which tell operators this is a no-op here")
	}
	t.Log("confirmed no-op on the shipped coding profile (documented boundary)")
}

// decisionFor evaluates a shell command against the session's effective
// profile, which is the orchestrator's own merged with the session's approvals.
//
// The merge is done through the same exported pair the orchestrator's own
// profileForSession uses (SessionRules + WithSessionRules) rather than by
// exporting that unexported method: widening a package's API so a test can
// reach it makes the test the reason the API exists, and this composition is
// already the documented one.
func decisionFor(t *testing.T, o *orchestrator.Orchestrator, sess, cmd string, base guard.PermissionProfile) guard.Decision {
	t.Helper()
	prof := o.SessionRules(sess).WithSessionRules(base)
	return guard.New().Check(prof, guard.Action{Tool: "shell_run", Shell: cmd})
}
