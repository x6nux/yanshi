package execpolicy

import "testing"

func TestEvaluateDenyRuleOnlyAppliesWhenDenyFlagMatches(t *testing.T) {
	cmd, err := Parse(`go test ./...`)
	if err != nil {
		t.Fatal(err)
	}
	rules := []Rule{
		{ID: "no-real-e2e", Program: "go", Prefix: []string{"test"}, Decision: "deny", DenyFlags: []string{"-tags=e2e_real"}, Justification: "real E2E requires explicit operator approval"},
		{ID: "go-test", Program: "go", Prefix: []string{"test"}, Decision: "allow", Justification: "ordinary Go tests are safe"},
	}
	got := Evaluate(cmd, rules)
	if got.Verdict != "allow" || got.RuleID != "go-test" {
		t.Fatalf("ordinary go test must not be denied by no-real-e2e rule: %#v", got)
	}
}

func TestEvaluateDenyFlagReturnsRuleID(t *testing.T) {
	cmd, err := Parse(`go test -tags=e2e_real ./internal/acp/...`)
	if err != nil {
		t.Fatal(err)
	}
	got := Evaluate(cmd, []Rule{{ID: "no-real-e2e", Program: "go", Prefix: []string{"test"}, Decision: "deny", DenyFlags: []string{"-tags=e2e_real"}, Justification: "real E2E is gated"}})
	if got.Verdict != "hard_deny" || got.RuleID != "no-real-e2e" || got.Justification == "" {
		t.Fatalf("deny result must carry RuleID/Justification: %#v", got)
	}
}

func TestParsePipelineAndRedirects(t *testing.T) {
	cmd, err := Parse(`printf ok | cat >> out 2>&1`)
	if err != nil {
		t.Fatal(err)
	}
	if len(cmd.Segments) != 2 || len(cmd.Segments[1].Redirects) != 2 {
		t.Fatalf("parsed command = %#v", cmd)
	}
}

func TestProgramNormalizationAbsoluteAndWindowsExe(t *testing.T) {
	cases := map[string]string{
		`/usr/bin/go test`:            "go",
		`C:\\Go\\bin\\GO.EXE version`: "go",
	}
	for raw, want := range cases {
		cmd, err := Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		if got := cmd.Segments[0].Program; got != want {
			t.Fatalf("Parse(%q) program=%q, want=%q", raw, got, want)
		}
	}
}

func TestHasPrefix_LongerPrefixFails(t *testing.T) {
	if hasPrefix([]string{"a", "b"}, []string{"a", "b", "c"}) {
		t.Fatal("prefix longer than args should not match")
	}
}

func TestContainsAny_EmptyFlagsReturnsFalse(t *testing.T) {
	if containsAny([]string{"-flag"}, nil) {
		t.Fatal("empty flags list must not match")
	}
	if containsAny([]string{"-flag"}, []string{}) {
		t.Fatal("empty flags list must not match")
	}
}

func TestEvaluate_ControlTokenHardDeny(t *testing.T) {
	cmd, err := Parse(`go test && echo done`)
	if err != nil {
		t.Fatal(err)
	}
	got := Evaluate(cmd, []Rule{{Program: "go", Decision: "allow"}, {Program: "echo", Decision: "allow"}})
	if got.Verdict != "hard_deny" || got.RuleID != "control-token" {
		t.Fatalf("&& must be hard_denied: %#v", got)
	}
}

func TestEvaluate_EmptyCommandHardDeny(t *testing.T) {
	got := Evaluate(Command{}, []Rule{{Program: "go", Decision: "allow"}})
	if got.Verdict != "hard_deny" || got.RuleID != "empty-command" {
		t.Fatalf("empty command must be hard_denied: %#v", got)
	}
}

func TestEvaluate_UnknownDecisionHardDeny(t *testing.T) {
	cmd, err := Parse(`go test`)
	if err != nil {
		t.Fatal(err)
	}
	got := Evaluate(cmd, []Rule{{ID: "weird", Program: "go", Decision: "maybe"}})
	if got.Verdict != "hard_deny" || got.RuleID != "weird" {
		t.Fatalf("unknown decision must be hard_denied: %#v", got)
	}
}

func TestEvaluate_UnmatchedSegmentHardDeny(t *testing.T) {
	cmd, err := Parse(`python parse.py`)
	if err != nil {
		t.Fatal(err)
	}
	got := Evaluate(cmd, []Rule{{Program: "go", Decision: "allow"}})
	if got.Verdict != "hard_deny" || got.RuleID != "unmatched-segment" {
		t.Fatalf("unmatched segment must be hard_denied: %#v", got)
	}
}

func TestParse_RedirectBeforeExecutableRejected(t *testing.T) {
	_, err := Parse(`> /dev/null`)
	if err == nil {
		t.Fatal("redirect before executable must be rejected")
	}
}

func TestParse_MissingRedirectTargetRejected(t *testing.T) {
	_, err := Parse(`cat >`)
	if err == nil {
		t.Fatal("redirect without target must be rejected")
	}
}

func TestParse_StandalonePipeRejected(t *testing.T) {
	_, err := Parse(`echo hello |`)
	if err == nil {
		t.Fatal("trailing pipe must be rejected")
	}
}

func TestEvaluate_DenyFlagWithEqualsForm(t *testing.T) {
	// Verify that deny flags with = form are detected.
	cmd, err := Parse(`go test -tags=e2e_real ./...`)
	if err != nil {
		t.Fatal(err)
	}
	got := Evaluate(cmd, []Rule{
		{ID: "deny-e2e", Program: "go", Prefix: []string{"test"}, Decision: "deny", DenyFlags: []string{"-tags=e2e_real"}},
	})
	if got.Verdict != "hard_deny" || got.RuleID != "deny-e2e" {
		t.Fatalf("-tags=e2e_real with = form must be denied: %#v", got)
	}
}

// TestDenyFlagsSurviveFlagSpelling pins that a deny rule's flag cannot be
// evaded by respelling it.
//
// containsAny matched only the exact token and the flag+"=" form, so three
// spellings of the same flag walked past the rule and were allowed by the
// permissive rule behind it. Measured before the fix:
//
//	go test -tags=e2e_real ./x   -> hard_deny   (caught)
//	go test -tags e2e_real ./x   -> allow       (bypass)
//	go test --tags=e2e_real ./x  -> allow       (bypass)
//	go test -tags=e2e_real,foo   -> allow       (bypass)
//
// That is the no-real-e2e rule this package's own doc comment holds up as its
// worked example, so the flagship use case was the one being evaded.
//
// The second half of the table is not decoration. Widening a flag match is
// one edit away from turning a targeted deny rule into a blanket ban on the
// program, which is the regression policy.go's header records; these cases
// fail if that happens.
func TestDenyFlagsSurviveFlagSpelling(t *testing.T) {
	rules := []Rule{
		{ID: "no-real-e2e", Program: "go", Prefix: []string{"test"}, Decision: "deny",
			DenyFlags: []string{"-tags=e2e_real"}, Justification: "real E2E is gated"},
		{ID: "go-test", Program: "go", Prefix: []string{"test"}, Decision: "allow",
			Justification: "ordinary Go tests are safe"},
		{ID: "go-build", Program: "go", Prefix: []string{"build"}, Decision: "allow",
			Justification: "building is safe"},
	}

	cases := []struct {
		cmd     string
		verdict string
		ruleID  string
		why     string
	}{
		// Tightening: every spelling of the denied flag must be caught.
		{`go test -tags=e2e_real ./internal/acp`, "hard_deny", "no-real-e2e", "canonical form"},
		{`go test -tags e2e_real ./internal/acp`, "hard_deny", "no-real-e2e", "space-separated value"},
		{`go test --tags=e2e_real ./internal/acp`, "hard_deny", "no-real-e2e", "double-dash spelling"},
		{`go test -tags='e2e_real' ./internal/acp`, "hard_deny", "no-real-e2e", "quoted value, stripped by the lexer"},
		{`go test -tags=e2e_real,foo ./internal/acp`, "hard_deny", "no-real-e2e", "denied tag among others"},

		// Not over-denying: the rule targets one flag, not the program.
		{`go test ./...`, "allow", "go-test", "an ordinary test run"},
		{`go build ./...`, "allow", "go-build", "an ordinary build"},
		{`go test -run TestFoo ./internal/acp`, "allow", "go-test", "an unrelated flag"},
		{`go test -tags=integration ./internal/acp`, "allow", "go-test", "a different tag entirely"},
		{`go test -tags=e2e_realistic ./x`, "allow", "go-test", "a tag that merely starts the same"},
	}

	for _, tc := range cases {
		cmd, err := Parse(tc.cmd)
		if err != nil {
			t.Fatalf("%s: parse: %v", tc.cmd, err)
		}
		got := Evaluate(cmd, rules)
		if got.Verdict != tc.verdict || got.RuleID != tc.ruleID {
			t.Errorf("%s (%s):\n  got  verdict=%q rule=%q\n  want verdict=%q rule=%q",
				tc.cmd, tc.why, got.Verdict, got.RuleID, tc.verdict, tc.ruleID)
		}
	}
}
