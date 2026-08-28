package tools

import (
	"fmt"
	"reflect"
	"testing"

	"github.com/x6nux/yanshi/internal/guard"
)

func scopeOf(t *testing.T, cmd string) (scope any, err error) {
	t.Helper()
	s, e := scopeFromAction(guard.Action{Tool: "shell_run", Shell: cmd})
	return s, e
}

// TestApprovalScopeSeparatesRedirectTargets is the one that found a live hole.
//
// approval.Manager matches a recorded rule with reflect.DeepEqual on the Scope,
// and the Scope was built from the program word plus its ARGUMENT VECTOR only.
// A redirection target is not an argument — execpolicy puts it in
// Segment.Redirects — so it was not in the scope at all. Measured:
//
//	"echo x > out.txt"        Program="echo" Args=["x"] Redirects=[{> out.txt}]
//	"echo x >> /etc/sudoers"  Program="echo" Args=["x"] Redirects=[{>> /etc/sudoers}]
//
// Identical scopes. Approving the first for the session auto-allowed the second
// with no prompt — the same class of miss as reading `>&file` as a descriptor
// duplication, one layer further along: the guard's FS dimension judges the
// target correctly, and then the approval cache remembers a decision that was
// never about that target.
func TestApprovalScopeSeparatesRedirectTargets(t *testing.T) {
	distinct := []string{
		"echo x",
		"echo x > out.txt",
		"echo x > other.txt",
		"echo x >> out.txt",
		"echo x >> /etc/sudoers",
		"echo x > ~/.ssh/authorized_keys",
		"echo x < in.txt",
		"echo x > out.txt 2> err.txt",
	}
	seen := map[string]string{}
	for _, cmd := range distinct {
		s, err := scopeOf(t, cmd)
		if err != nil {
			t.Fatalf("scopeFromAction(%q) = error %v; every one of these is a single "+
				"executable segment", cmd, err)
		}
		key := reflectKey(s)
		if prev, dup := seen[key]; dup {
			t.Errorf("scopeFromAction(%q) produces the same approval scope as %q — approving "+
				"one silently approves the other", cmd, prev)
			continue
		}
		seen[key] = cmd
	}
}

// TestApprovalScopeIsStableAcrossSpellings is the other direction: an approval
// has to survive being re-typed. W-B-06's "同一命令重试不重复弹窗" is only true if
// insignificant whitespace and quoting do not produce a new scope.
//
// The normalization is free rather than implemented: the scope is built from
// PARSED WORDS, so whitespace between them and the quotes around them are gone
// by construction. Argument ORDER is deliberately not normalized — two orders
// are two different commands, and collapsing them would be the over-
// normalization the spec warns is a security hole rather than an annoyance.
func TestApprovalScopeIsStableAcrossSpellings(t *testing.T) {
	groups := [][]string{
		{"go test ./x", "go  test   ./x", `go test "./x"`, `go test './x'`},
		{"echo hi > out.txt", "echo hi>out.txt", `echo "hi" >  out.txt`, `echo hi > "out.txt"`},
		{`grep -r "a b" .`, `grep -r 'a b' .`},
	}
	for _, group := range groups {
		want, err := scopeOf(t, group[0])
		if err != nil {
			t.Fatalf("scopeFromAction(%q) = error %v", group[0], err)
		}
		for _, cmd := range group[1:] {
			got, err := scopeOf(t, cmd)
			if err != nil {
				t.Fatalf("scopeFromAction(%q) = error %v", cmd, err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("scopeFromAction(%q) != scopeFromAction(%q):\n got %+v\nwant %+v — "+
					"the same command re-typed must hit the same approval", cmd, group[0], got, want)
			}
		}
	}
	// Argument order is NOT normalized.
	a, _ := scopeOf(t, "cp src dst")
	b, _ := scopeOf(t, "cp dst src")
	if reflect.DeepEqual(a, b) {
		t.Error("scopeFromAction collapses argument order; two orders are two different commands " +
			"and one approval must not cover both")
	}
}

// TestApprovalScopeAcceptsCommandsTheStrictLexerRefuses is the reason this
// changed reader at all.
//
// The scope used to come from execpolicy.Parse, which rejects globs and $VAR
// because the RULE ENGINE cannot honestly match a word whose value it cannot
// see. That strictness is right for rules and wrong here: a scope error is a
// hard DenyErr at the approval step, so `ls *.go` under a glob profile was
// refused outright — the guard said Prompt and the user never saw one.
//
// ParseCommandList is lenient about word CONTENT and strict about STRUCTURE,
// which is exactly the pair of properties a scope needs.
func TestApprovalScopeAcceptsCommandsTheStrictLexerRefuses(t *testing.T) {
	for _, cmd := range []string{
		"ls *.go",
		"echo $HOME",
		"grep -r foo ./**/*.go",
		"rm -rf ./build/*",
	} {
		if _, err := scopeOf(t, cmd); err != nil {
			t.Errorf("scopeFromAction(%q) = error %v; a command the guard is willing to prompt "+
				"about must be describable as an approval scope, or the prompt never happens", cmd, err)
		}
	}
}

// TestApprovalScopeStillRefusesChains keeps the fail-closed half. A user cannot
// pre-approve a chained command: the scope names ONE program, so recording a
// chain would record an approval for its first segment and apply it to the
// whole string.
func TestApprovalScopeStillRefusesChains(t *testing.T) {
	for _, cmd := range []string{
		"ls && rm -rf /",
		"ls; rm -rf /",
		"ls | tee out",
		"ls || true",
		"$(rm -rf /)",
		"ls `rm -rf /`",
		"(rm -rf /)",
		"ls &",
		"ls \n rm -rf /",
		`echo "unterminated`,
	} {
		if _, err := scopeOf(t, cmd); err == nil {
			t.Errorf("scopeFromAction(%q) = nil error; a chained or unreadable command must not "+
				"be describable as one approval scope", cmd)
		}
	}
}

// reflectKey renders a scope as a comparison key. reflect.DeepEqual is what
// approval.Manager.Match uses, and %#v distinguishes exactly the same things
// for these flat structs.
func reflectKey(s any) string {
	return fmt.Sprintf("%#v", s)
}
