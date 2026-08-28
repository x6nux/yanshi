package tools

import (
	"context"
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

func scopeOfIn(t *testing.T, interpreter, cmd string) (scope any, err error) {
	t.Helper()
	s, e := scopeFromAction(guard.Action{Tool: "shell_run", Shell: cmd, Interpreter: interpreter})
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

// TestApprovalScopeDistinguishesTheLanguage is the over-normalization half of
// W-B-06's warning, which the batch that quoted the warning as satisfied then
// shipped.
//
// The guard learned to read a PowerShell command with the PowerShell reader and
// this function did not, so the POSIX reader kept eating the backslashes.
// `C:\temp` is the C-drive root's temp and `C:temp` is temp under the current
// directory — two directories — and both produced Prefix=[-Recurse C:temp].
// Approving the relative one for the session admitted the absolute one with no
// callback consulted at all.
//
// Each pair below is TWO SPELLINGS THAT MEAN DIFFERENT THINGS, so a shared
// scope is a shared approval. The last pair is the cross-language one, which a
// shared reader alone does not fix: the same text handed to two languages is
// two commands, which is why the language is in the scope as well.
func TestApprovalScopeDistinguishesTheLanguage(t *testing.T) {
	for _, pair := range []struct{ aInterp, a, bInterp, b string }{
		{"powershell", `Remove-Item -Recurse C:\temp`, "powershell", `Remove-Item -Recurse C:temp`},
		{"powershell", `Write-Output k > C:\a\b.txt`, "powershell", `Write-Output k > C:ab.txt`},
		{"powershell", `Remove-Item C:\Users\me\.ssh`, "powershell", `Remove-Item C:Usersme.ssh`},
		{"powershell", `Remove-Item -Recurse C:temp`, "sh", `Remove-Item -Recurse C:temp`},
		{"cmd", `rd /s C:temp`, "sh", `rd /s C:temp`},
	} {
		a, err := scopeOfIn(t, pair.aInterp, pair.a)
		if err != nil {
			t.Fatalf("scopeFromAction(%q, interpreter=%q) = error %v", pair.a, pair.aInterp, err)
		}
		b, err := scopeOfIn(t, pair.bInterp, pair.b)
		if err != nil {
			t.Fatalf("scopeFromAction(%q, interpreter=%q) = error %v", pair.b, pair.bInterp, err)
		}
		if reflect.DeepEqual(a, b) {
			t.Errorf("scopeFromAction(%q as %q) equals scopeFromAction(%q as %q):\n  %+v\n"+
				"these are two different commands, and one approval must not cover both",
				pair.a, pair.aInterp, pair.b, pair.bInterp, a)
		}
	}
	// The other direction still holds: the same command in the same language
	// re-typed hits the same approval. Without this the test above could be
	// satisfied by putting something unique in every scope.
	x, _ := scopeOfIn(t, "powershell", `Remove-Item -Recurse  "C:\temp"`)
	y, _ := scopeOfIn(t, "powershell", `Remove-Item -Recurse C:\temp`)
	if !reflect.DeepEqual(x, y) {
		t.Errorf("two spellings of one PowerShell command produce different scopes:\n got %+v\nwant %+v", x, y)
	}
	// POSIX is the empty language token, so a scope recorded before the field
	// existed still matches an ordinary sh command. A rename of the constant
	// that broke this would silently re-prompt for every persisted approval.
	p, _ := scopeOf(t, "go test ./x")
	q, _ := scopeOfIn(t, "bash", "go test ./x")
	if !reflect.DeepEqual(p, q) {
		t.Errorf("an unset interpreter and an explicit POSIX one produce different scopes:\n got %+v\nwant %+v", q, p)
	}
}

// TestPowerShellPromptReachesTheCallback is the SILENT-DENIAL half of the same
// divergence, and it is the half no guard-level test could see.
//
// Any PowerShell command ending in `\` — `Get-ChildItem C:\` is the ordinary
// spelling — made the POSIX reader report a trailing escape. A scope error is a
// hard DenyErr raised at step (3) of Authorize, BEFORE the callback, so the
// guard said Prompt and the user never saw one: neither allowed nor asked.
//
// The assertion is on the CALLBACK, not on the guard's Decision, because both
// ends were already correct and the break was in the hop between them.
func TestPowerShellPromptReachesTheCallback(t *testing.T) {
	prof := guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"*"}},
		FS:    guard.FSPerm{Read: []string{"**"}, Write: []string{"**"}},
		Shell: guard.ShellPerm{Policy: "allowlist", Patterns: []string{"git *"}},
		Net:   guard.NetPerm{Allow: true},
	}
	for _, cmd := range []string{
		`Get-ChildItem C:\`,
		`Get-ChildItem C:\Users\me\`,
		`Remove-Item -Recurse C:\temp\`,
	} {
		action := guard.Action{Tool: "shell_run", Shell: cmd, Interpreter: "powershell"}
		if d := guard.New().Check(prof, action); d.Verdict != guard.Prompt {
			t.Fatalf("Check(%q) = %v (%q); this test is only meaningful for a command the guard "+
				"wants to ASK about", cmd, d.Verdict, d.Reason)
		}
		asked := 0
		ctx := WithProfile(context.Background(), prof)
		ctx = WithPermissionCallback(ctx, func(PermissionRequest) PermissionDecision {
			asked++
			return PermissionAllow
		})
		if err := Authorize(ctx, action, "{}"); err != nil {
			t.Errorf("Authorize(%q) = %v; the guard said Prompt, so the user has to be asked "+
				"rather than refused by the approval-scope step", cmd, err)
		}
		if asked != 1 {
			t.Errorf("Authorize(%q) consulted the callback %d times, want 1 — a Prompt that never "+
				"reaches a callback is neither an allow nor an ask", cmd, asked)
		}
	}
}
