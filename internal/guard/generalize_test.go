package guard

import (
	"strings"
	"testing"

	"github.com/x6nux/yanshi/internal/execpolicy"
)

// findRule returns the first rule with the given decision, so assertions can
// talk about "the allow rule" without depending on emission order.
func findRule(rules []execpolicy.Rule, decision string) (execpolicy.Rule, bool) {
	for _, r := range rules {
		if r.Decision == decision {
			return r, true
		}
	}
	return execpolicy.Rule{}, false
}

// TestRuleSetApprove_Widening is the table for what an approval turns into.
// The Widened column is the security-relevant one: it distinguishes "this
// approval now covers a family" from "this approval produced no rule at all,
// so the command keeps asking".
func TestRuleSetApprove_Widening(t *testing.T) {
	cases := []struct {
		name        string
		cmd         string
		wantWidened bool
		wantProgram string
		wantPrefix  []string
	}{
		// Ordinary developer commands: widened to program + subcommand.
		{"go test with a package arg", "go test ./internal/guard", true, "go", []string{"test"}},
		{"npm run build", "npm run build", true, "npm", []string{"run", "build"}},
		{"cargo build with a flag", "cargo build --release", true, "cargo", []string{"build"}},
		{"git status", "git status", true, "git", []string{"status"}},
		{"bare program", "make", true, "make", nil},
		{"program with only a path arg", "go ./x", true, "go", nil},

		// Gate 1: high-risk verbs produce NO rule, so they ask every time.
		{"rm is never widened", "rm -rf ./build", false, "", nil},
		{"dd is never widened", "dd if=/dev/zero of=x", false, "", nil},
		{"chmod is never widened", "chmod 755 script.sh", false, "", nil},
		{"chown is never widened", "chown me file", false, "", nil},
		{"sudo is never widened", "sudo apt update", false, "", nil},
		{"curl is never widened", "curl https://example.com", false, "", nil},
		{"kill is never widened", "kill 123", false, "", nil},
		{"an interpreter is never widened", "python script.py", false, "", nil},
		{"bash is never widened", "bash deploy.sh", false, "", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var s RuleSet
			got := s.Approve(c.cmd)
			if got.Widened != c.wantWidened {
				t.Fatalf("Widened = %v, want %v (reason %q)", got.Widened, c.wantWidened, got.Reason)
			}
			allow, ok := findRule(got.Rules, "allow")
			if !c.wantWidened {
				if ok || len(got.Rules) != 0 {
					t.Fatalf("a high-risk verb must produce no rule; got %+v", got.Rules)
				}
				return
			}
			if !ok {
				t.Fatal("no allow rule was produced; the approval would not stop the next prompt")
			}
			if allow.Program != c.wantProgram {
				t.Fatalf("Program = %q, want %q", allow.Program, c.wantProgram)
			}
			if strings.Join(allow.Prefix, " ") != strings.Join(c.wantPrefix, " ") {
				t.Fatalf("Prefix = %v, want %v", allow.Prefix, c.wantPrefix)
			}
		})
	}
}

// TestRuleSetApprove_WidenedRulesActuallyMatchTheFamily is the behavioural
// half: the emitted rules must make the NEXT similar command evaluate to allow
// through the real execpolicy evaluator, and must NOT admit a different
// program or a different subcommand. Asserting only on the rule's fields would
// leave "does this rule do anything" untested.
func TestRuleSetApprove_WidenedRulesActuallyMatchTheFamily(t *testing.T) {
	var s RuleSet
	s.Approve("go test ./internal/guard")
	rules := s.Rules()

	shouldAllow := []string{
		"go test ./internal/tools",
		"go test ./...",
		"go test -run TestFoo ./x",
	}
	for _, cmd := range shouldAllow {
		t.Run("allow/"+cmd, func(t *testing.T) {
			parsed, err := execpolicy.Parse(cmd)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if got := execpolicy.Evaluate(parsed, rules).Verdict; got != "allow" {
				t.Fatalf("verdict = %q, want allow; the generalization did not cover its own family", got)
			}
		})
	}
	shouldNotAllow := []string{
		"go build ./...",  // different subcommand
		"npm test",        // different program
		"go vet ./...",    // different subcommand
		"gofmt -w ./x.go", // different program
	}
	for _, cmd := range shouldNotAllow {
		t.Run("deny/"+cmd, func(t *testing.T) {
			parsed, err := execpolicy.Parse(cmd)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if got := execpolicy.Evaluate(parsed, rules).Verdict; got == "allow" {
				t.Fatalf("verdict = allow; approving `go test` must not authorize %q", cmd)
			}
		})
	}
}

// TestRuleSetApprove_HighRiskVerbsProduceNoRule is the behavioural half of
// gate 1. Approving one `rm` must not admit ANY `rm` — including a superset of
// the approved argument vector, which is why the exact-match option was
// unavailable: execpolicy prefix matching cannot express "and nothing more".
func TestRuleSetApprove_HighRiskVerbsProduceNoRule(t *testing.T) {
	var s RuleSet
	got := s.Approve("rm -rf ./build")
	if len(got.Rules) != 0 || len(s.Rules()) != 0 {
		t.Fatalf("a high-risk verb must leave the rule set empty; got %+v", s.Rules())
	}
	if !strings.Contains(got.Reason, "high-risk") {
		t.Errorf("reason %q should say why the command was not generalized", got.Reason)
	}
	// With no rules at all, nothing is admitted — including the command that
	// was approved. It keeps asking, which is the intended cost.
	for _, cmd := range []string{"rm -rf ./build", "rm -rf /", "rm -rf ./build ./other"} {
		parsed, err := execpolicy.Parse(cmd)
		if err != nil {
			continue // the execpolicy lexer refuses some of these outright
		}
		if v := execpolicy.Evaluate(parsed, s.Rules()).Verdict; v == "allow" {
			t.Fatalf("no rule was recorded, yet %q evaluated to allow", cmd)
		}
	}
}

// TestRuleSetApprove_WidenedRuleDoesNotCarryDangerousFlags pins the companion
// deny rule. A generalized allow keeps only the program and a subcommand, so
// without the deny companion `git push` approved once would silently cover
// `git push --force`.
func TestRuleSetApprove_WidenedRuleDoesNotCarryDangerousFlags(t *testing.T) {
	var s RuleSet
	got := s.Approve("git push origin main")
	if !got.Widened {
		t.Fatalf("expected a widened approval, got %q", got.Reason)
	}
	deny, ok := findRule(got.Rules, "deny")
	if !ok {
		t.Fatal("a widened approval must emit a companion deny rule for irreversible flags")
	}
	if len(deny.DenyFlags) == 0 {
		t.Fatal("the companion deny rule has no flags, so it is inert")
	}
	rules := s.Rules()
	for _, cmd := range []string{
		"git push --force origin main",
		"git push --no-verify origin main",
		"git push --delete origin branch",
	} {
		t.Run(cmd, func(t *testing.T) {
			parsed, err := execpolicy.Parse(cmd)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if got := execpolicy.Evaluate(parsed, rules).Verdict; got == "allow" {
				t.Fatalf("a generalized `git push` must not extend to %q", cmd)
			}
		})
	}
	// The ordinary form still passes, or the deny rule would have eaten the
	// approval it was meant to accompany.
	parsed, err := execpolicy.Parse("git push origin other-branch")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := execpolicy.Evaluate(parsed, rules).Verdict; got != "allow" {
		t.Fatalf("the ordinary form must still be allowed; verdict = %q", got)
	}
}

// TestRuleSetDemote is gate 2: once a generalized family has been implicated in
// a refusal, it is demoted for the rest of the session — its widened rule is
// removed and later approvals in the family are recorded exactly.
func TestRuleSetDemote(t *testing.T) {
	var s RuleSet
	first := s.Approve("go test ./internal/guard")
	if !first.Widened {
		t.Fatalf("setup: expected a widened approval, got %q", first.Reason)
	}
	// The widened rule covers a sibling command.
	parsed, _ := execpolicy.Parse("go test ./internal/tools")
	if got := execpolicy.Evaluate(parsed, s.Rules()).Verdict; got != "allow" {
		t.Fatalf("setup: sibling should be covered, verdict = %q", got)
	}

	if !s.Demote("go test ./internal/tools") {
		t.Fatal("Demote reported no family was demoted, but `go test` had been generalized")
	}

	// The widened rule is gone: the sibling is no longer covered.
	if got := execpolicy.Evaluate(parsed, s.Rules()).Verdict; got == "allow" {
		t.Fatal("after demotion the widened rule must no longer admit the family")
	}

	// A later approval in the same family produces NO rule: it asks every time.
	second := s.Approve("go test ./internal/store")
	if second.Widened {
		t.Fatalf("after demotion, approvals must not widen; got a widened rule (%q)", second.Reason)
	}
	if len(second.Rules) != 0 {
		t.Fatalf("a demoted family must produce no rule; got %+v", second.Rules)
	}
	if !strings.Contains(second.Reason, "demoted") {
		t.Errorf("reason %q should say the family was demoted", second.Reason)
	}
	if len(s.Rules()) != 0 {
		t.Fatalf("the demoted family left rules behind: %+v", s.Rules())
	}
}

// TestRuleSetDemote_IsScopedToTheFamily proves demotion is surgical: refusing
// something in `go test` must not un-generalize `npm run build`. A demotion
// that took everything with it would make one bad widening reset the session.
func TestRuleSetDemote_IsScopedToTheFamily(t *testing.T) {
	var s RuleSet
	s.Approve("go test ./x")
	s.Approve("npm run build")
	s.Demote("go test ./x")

	npm, _ := execpolicy.Parse("npm run build --watch")
	if got := execpolicy.Evaluate(npm, s.Rules()).Verdict; got != "allow" {
		t.Fatalf("demoting `go test` must not affect the `npm run build` family; verdict = %q", got)
	}
	gotest, _ := execpolicy.Parse("go test ./y")
	if got := execpolicy.Evaluate(gotest, s.Rules()).Verdict; got == "allow" {
		t.Fatal("the demoted family must no longer be covered")
	}
}

// TestRuleSetDemote_ReportsWhetherAnythingWasDemoted pins the return value,
// which a caller uses to tell a real demotion (rules were removed) from a call
// about a family that had no rules to remove.
func TestRuleSetDemote_ReportsWhetherAnythingWasDemoted(t *testing.T) {
	var fresh RuleSet
	if fresh.Demote("go test ./x") {
		t.Fatal("nothing had been approved, so no rule could be removed")
	}

	var s RuleSet
	s.Approve("go test ./x")
	if !s.Demote("go test ./x") {
		t.Fatal("an approved family must report as demoted")
	}
	if s.Demote("go test ./x") {
		t.Fatal("a second demotion of the same family has nothing left to remove")
	}
}

// TestRuleSetDemote_IsStickyEvenBeforeAnyApproval pins that a refusal recorded
// BEFORE the family was ever generalized still prevents a later widening.
// A refusal is evidence about the family regardless of whether a rule happened
// to exist at the time, and forgetting it would let the very next approval
// re-create the widening that was just refused.
func TestRuleSetDemote_IsStickyEvenBeforeAnyApproval(t *testing.T) {
	var s RuleSet
	s.Demote("go test ./x")
	got := s.Approve("go test ./y")
	if got.Widened {
		t.Fatalf("a family demoted before its first approval must not be widened later (reason %q)", got.Reason)
	}
	if len(s.Rules()) != 0 {
		t.Fatalf("no rule should have been stored; got %+v", s.Rules())
	}
}

// TestRuleSetApprove_RefusesNonSingleSegmentCommands pins that no rule is
// produced for shapes the guard structurally refuses to run. Recording one
// would create a rule for a command the guard hard-denies anyway — a rule that
// can only ever be confusing, and at worst be reachable if the metachar gate
// is ever narrowed.
func TestRuleSetApprove_RefusesNonSingleSegmentCommands(t *testing.T) {
	for _, cmd := range []string{
		"",
		"   ",
		"ls && rm -rf /",
		"cat a | grep b",
		"echo hi > file",
		"echo `whoami`",
	} {
		t.Run(cmd, func(t *testing.T) {
			var s RuleSet
			got := s.Approve(cmd)
			if len(got.Rules) != 0 {
				t.Fatalf("a non-single-segment command must produce no rule; got %+v", got.Rules)
			}
			if len(s.Rules()) != 0 {
				t.Fatal("nothing should have been stored")
			}
		})
	}
}

// TestRuleSetApprove_DecodesObfuscatedCommands proves the approval path shares
// the destructive gate's decoder. Recording the opaque outer form would file a
// hex-encoded `rm` under a rule that reads as harmless.
func TestRuleSetApprove_DecodesObfuscatedCommands(t *testing.T) {
	var s RuleSet
	got := s.Approve(`$'\x72\x6d' -rf ./build`)
	if got.Widened {
		t.Fatal("a decoded `rm` must hit the high-risk gate, not be widened")
	}
	if len(got.Rules) != 0 {
		t.Fatalf("a decoded high-risk verb must produce no rule; got %+v", got.Rules)
	}
	if got.Family != "rm" {
		t.Fatalf("Family = %q, want %q — the encoding was recorded verbatim instead of decoded", got.Family, "rm")
	}
}

// TestSubcommandLike pins which argument tokens may enter a generalized
// prefix. Everything carrying a VALUE is excluded: a path in the prefix makes
// the rule useless, and a glob or expansion in it puts an unresolved,
// attacker-influenceable token into the matcher's exact-comparison position.
func TestSubcommandLike(t *testing.T) {
	cases := []struct {
		arg  string
		want bool
	}{
		{"test", true},
		{"run", true},
		{"build", true},
		{"install", true},

		{"", false},
		{"-v", false},
		{"--release", false},
		{"./internal", false},
		{"internal/guard", false},
		{`C:\x`, false},
		{"*.go", false},
		{"$HOME", false},
		{"~/x", false},
		{"KEY=value", false},
		{"%PATH%", false},
		{".hidden", false},
		{`"quoted"`, false},
		{"日本語", false},
	}
	for _, c := range cases {
		t.Run(c.arg, func(t *testing.T) {
			if got := subcommandLike(c.arg); got != c.want {
				t.Fatalf("subcommandLike(%q) = %v, want %v", c.arg, got, c.want)
			}
		})
	}
}

// TestGeneralizedPrefixIsCapped pins the cap. Without it the "prefix" would
// grow until it was an exact match wearing a generalization's name, and the
// feature would silently stop reducing prompts.
func TestGeneralizedPrefixIsCapped(t *testing.T) {
	got := generalizedPrefix([]string{"a", "b", "c", "d"})
	if len(got) != maxGeneralizedPrefix {
		t.Fatalf("prefix = %v, want %d elements", got, maxGeneralizedPrefix)
	}
	// The run stops at the first non-subcommand token even below the cap.
	if got := generalizedPrefix([]string{"run", "./path", "build"}); len(got) != 1 || got[0] != "run" {
		t.Fatalf("prefix = %v, want [run] — the run must stop at the first path", got)
	}
}

// TestWithSessionRules_OrderAndOptOut pins the merge contract: session rules go
// AFTER the profile's own, and a profile with no rules at all is left alone.
func TestWithSessionRules_OrderAndOptOut(t *testing.T) {
	var s RuleSet
	s.Approve("go test ./x")

	// A profile that does not use execpolicy is returned untouched — grafting a
	// rules table onto it would switch it to a different matcher entirely.
	globProfile := PermissionProfile{Shell: ShellPerm{Policy: "allowlist", Patterns: []string{"go *"}}}
	if got := s.WithSessionRules(globProfile); len(got.Shell.Rules) != 0 {
		t.Fatal("a rules-free profile must not be silently converted to a rules profile")
	}

	base := execpolicy.Rule{ID: "operator", Program: "go", Prefix: []string{"vet"}, Decision: "prompt"}
	rulesProfile := PermissionProfile{Shell: ShellPerm{Rules: []execpolicy.Rule{base}}}
	merged := s.WithSessionRules(rulesProfile)
	if len(merged.Shell.Rules) <= 1 {
		t.Fatal("session rules were not merged in")
	}
	if merged.Shell.Rules[0].ID != "operator" {
		t.Fatal("the operator's own rules must come first")
	}
	// The caller's profile must not have been mutated in place.
	if len(rulesProfile.Shell.Rules) != 1 {
		t.Fatal("WithSessionRules mutated the caller's profile")
	}
}

// TestWithSessionRules_CannotOverrideAnOperatorPrompt is the safety property
// the ordering buys. execpolicy resolves "prompt wins over allow", so an
// operator rule that asks for confirmation still asks even when a session
// approval would have admitted the command.
func TestWithSessionRules_CannotOverrideAnOperatorPrompt(t *testing.T) {
	var s RuleSet
	s.Approve("go test ./x")
	prof := PermissionProfile{Shell: ShellPerm{Rules: []execpolicy.Rule{
		{ID: "ask-about-tests", Program: "go", Prefix: []string{"test"}, Decision: "prompt"},
	}}}
	merged := s.WithSessionRules(prof)
	parsed, _ := execpolicy.Parse("go test ./y")
	if got := execpolicy.Evaluate(parsed, merged.Shell.Rules).Verdict; got != "prompt" {
		t.Fatalf("verdict = %q; a session approval must not downgrade an operator's prompt rule", got)
	}
}

// TestWithSessionRules_CannotOverrideAnOperatorDeny is the stronger half: a
// deny rule whose flags match returns hard_deny immediately regardless of
// position, so no approval can ever buy past it.
func TestWithSessionRules_CannotOverrideAnOperatorDeny(t *testing.T) {
	var s RuleSet
	s.Approve("go test ./x")
	prof := PermissionProfile{Shell: ShellPerm{Rules: []execpolicy.Rule{
		{ID: "no-real-e2e", Program: "go", Prefix: []string{"test"}, Decision: "deny", DenyFlags: []string{"-tags=e2e_real"}},
	}}}
	merged := s.WithSessionRules(prof)
	parsed, _ := execpolicy.Parse("go test -tags=e2e_real ./y")
	if got := execpolicy.Evaluate(parsed, merged.Shell.Rules).Verdict; got != "hard_deny" {
		t.Fatalf("verdict = %q; a session approval must not buy past an operator deny rule", got)
	}
}

// TestRuleSetApprove_ReapprovalReplacesRatherThanAccumulates pins that a family
// keeps exactly one set of rules. Accumulation would leave a demoted family's
// widened rule behind a narrower one, where execpolicy's ordering could still
// reach it — which is precisely the failure Demote exists to prevent.
func TestRuleSetApprove_ReapprovalReplacesRatherThanAccumulates(t *testing.T) {
	var s RuleSet
	s.Approve("go test ./a")
	first := len(s.Rules())
	s.Approve("go test ./b")
	if got := len(s.Rules()); got != first {
		t.Fatalf("rule count grew from %d to %d for the same family; rules accumulate instead of replacing", first, got)
	}
}

// TestRuleSetRules_ReturnsACopy pins that the snapshot does not share backing
// storage with the live set. The caller merges it into a profile that outlives
// the lock, so a shared array would be a data race on the authorization path.
func TestRuleSetRules_ReturnsACopy(t *testing.T) {
	var s RuleSet
	s.Approve("go test ./x")
	snap := s.Rules()
	if len(snap) == 0 {
		t.Fatal("setup: no rules")
	}
	snap[0].Decision = "tampered"
	if s.Rules()[0].Decision == "tampered" {
		t.Fatal("Rules() shares backing storage with the live rule set")
	}
}

// TestRuleSetIsConcurrencySafe exercises the lock under -race. Approvals arrive
// on the WebSocket reader goroutine while a turn's tool calls read the rules.
func TestRuleSetIsConcurrencySafe(t *testing.T) {
	var s RuleSet
	done := make(chan struct{})
	go func() {
		for i := 0; i < 200; i++ {
			s.Approve("go test ./x")
			s.Demote("npm run build")
		}
		close(done)
	}()
	for i := 0; i < 200; i++ {
		_ = s.Rules()
		s.Approve("npm run build")
	}
	<-done
}
