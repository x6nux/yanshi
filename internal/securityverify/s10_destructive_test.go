package securityverify

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/x6nux/yanshi/internal/guard"
)

// tier is the observable outcome an operator experiences, derived from the
// Decision the way the real callers derive it: tools.Authorize returns before
// consulting any callback on a structural HardDeny, routes an overridable one
// through the mode gate (yolo/auto can pass it), and escalates a Prompt.
type tier string

const (
	tierStructural  tier = "structural-harddeny"  // no mode can pass it
	tierOverridable tier = "overridable-harddeny" // yolo passes, default denies silently
	tierPrompt      tier = "prompt"               // asks; yolo still asks for OutOfScope
	tierAllow       tier = "allow"
)

func tierOf(d guard.Decision) tier {
	switch d.Verdict {
	case guard.Allow:
		return tierAllow
	case guard.Prompt:
		return tierPrompt
	case guard.HardDeny:
		if d.Overridable {
			return tierOverridable
		}
		return tierStructural
	}
	return tier(fmt.Sprintf("unknown(%v)", d.Verdict))
}

// shellTier runs one command through the real guard under a permissive
// profile. The profile is deliberately wide (shell policy allowlist with a
// "*" pattern) so that ANY denial observed comes from a structural gate and
// not from the profile simply not listing the command.
func shellTier(t *testing.T, cmd, workdir string) (tier, string) {
	t.Helper()
	prof := guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"*"}},
		FS:    guard.FSPerm{Read: []string{"**"}, Write: []string{"**"}},
		Shell: guard.ShellPerm{Policy: "allowlist", Patterns: []string{"*"}},
		Net:   guard.NetPerm{Allow: true, Hosts: []string{"*"}},
	}
	d := guard.New().Check(prof, guard.Action{Tool: "shell_run", Shell: cmd, Workdir: workdir})
	return tierOf(d), d.Reason
}

// TestS10S11_ObfuscatedDestructionMatrix runs the real attack spellings, not
// paraphrases of them, and pins the TIER of each. Tier matters more than
// "denied": an overridable denial is one `/mode yolo` away from running.
func TestS10S11_ObfuscatedDestructionMatrix(t *testing.T) {
	home := t.TempDir()
	if r, err := filepath.EvalSymlinks(home); err == nil {
		home = r
	}
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	workdir := filepath.Join(home, "proj")
	if err := os.MkdirAll(workdir, 0o755); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		cmd  string
		want tier
		why  string
	}{
		// ── S10: the payload is hidden from token inspection.
		{
			name: "ansic hex rm -rf / inside bash -c",
			// $'\x72\x6d -rf /' is byte-for-byte `rm -rf /` to bash.
			cmd:  `bash -c $'\x72\x6d -rf /'`,
			want: tierStructural,
			why:  "decoding must happen before classification, or the wrapper hides it",
		},
		{
			name: "plain bash -c wrapper",
			cmd:  `bash -c "rm -rf /"`,
			want: tierStructural,
			why:  "the wrapper turns the payload into one opaque argument",
		},
		{
			name: "sh -c with single quotes",
			cmd:  `sh -c 'rm -rf /'`,
			want: tierStructural,
		},
		{
			name: "chain inside a wrapper",
			// The OUTER string has no metacharacter, so checkShell's metachar
			// HardDeny does not fire; the inner chain must be split.
			cmd:  `bash -c "ls && rm -rf /"`,
			want: tierStructural,
			why:  "launders past both the metachar gate and the destructive gate",
		},
		{
			name: "ansic-encoded && at top level",
			// Neither the raw string nor lexShellLite sees an operator here.
			cmd:  `ls $'\x26\x26' rm -rf /`,
			want: tierStructural,
		},
		{
			name: "nested wrapper",
			cmd:  `bash -c "sh -c 'rm -rf /'"`,
			want: tierStructural,
		},
		{
			name: "env prefix before the wrapper",
			cmd:  `env FOO=1 bash -c "rm -rf /"`,
			want: tierStructural,
		},

		// ── S11: the PATH is spelled so it does not look catastrophic.
		{
			name: "home via dotdot collapse",
			cmd:  `rm -rf ` + filepath.Join(home, "proj", "..") + `/`,
			want: tierStructural,
			why:  "collapses to $HOME; normalising must precede matching",
		},
		{
			name: "tilde dotdot collapse",
			cmd:  `rm -rf ~/proj/../`,
			want: tierStructural,
		},
		{
			name: "HOME variable spelling",
			cmd:  `rm -rf $HOME`,
			want: tierStructural,
		},
		{
			name: "bare tilde",
			cmd:  `rm -rf ~`,
			want: tierStructural,
		},
		{
			name: "workdir ancestor",
			cmd:  `rm -rf ` + home,
			want: tierStructural,
			why:  "deleting an ancestor of the workdir deletes the workdir",
		},
		{
			name: "windows drive root",
			cmd:  `rm -rf C:\`,
			want: tierStructural,
		},
		{
			name: "windows drive root via dotdot",
			cmd:  `rm -rf C:\Users\..\..`,
			want: tierStructural,
			why:  "path.Clean has no volume notion; C:/Users/../.. must not become a relative path",
		},
		{
			name: "UNC share root",
			cmd:  `rm -rf \\server\share`,
			want: tierStructural,
		},
		{
			name: "system tree",
			cmd:  `rm -rf /etc`,
			want: tierStructural,
		},
		{
			name: "bare recursive wildcard",
			cmd:  `rm -rf *`,
			want: tierStructural,
		},

		// ── Controls. A guard that denies everything passes no review.
		{
			name: "ordinary build dir deletion inside the workdir",
			cmd:  `rm -rf ./build`,
			want: tierAllow,
			why:  "the single most common legitimate rm -rf; denying it is the failure mode",
		},
		{
			name: "printf with a legitimate ANSI-C newline",
			cmd:  `printf $'hello\n'`,
			want: tierAllow,
			why:  "$'...' is a normal shell feature; blanket-refusing it breaks real work",
		},
		{
			name: "grep with an ANSI-C tab",
			cmd:  `grep $'\t' file.txt`,
			want: tierAllow,
		},
		{
			name: "rm of a single file in the workdir",
			cmd:  `rm -f ./out.log`,
			want: tierAllow,
		},
		{
			name: "git clean",
			cmd:  `git clean -fdx`,
			want: tierAllow,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, reason := shellTier(t, tc.cmd, workdir)
			t.Logf("cmd=%s\n  tier=%s reason=%s", tc.cmd, got, reason)
			if got != tc.want {
				t.Fatalf("tier mismatch for %q: got %s want %s (%s)\n  reason: %s",
					tc.cmd, got, tc.want, tc.why, reason)
			}
		})
	}
}

// TestS10S11_OutOfScopeDeletionPrompts pins the OTHER tier the destructive gate
// produces: deleting outside the workdir is a Prompt, not a HardDeny. That
// distinction is the reason yolo is usable at all — and the reason yolo still
// stops for this one.
func TestS10S11_OutOfScopeDeletionPrompts(t *testing.T) {
	home := t.TempDir()
	if r, err := filepath.EvalSymlinks(home); err == nil {
		home = r
	}
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	workdir := filepath.Join(home, "proj")
	if err := os.MkdirAll(workdir, 0o755); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(home, "someone-elses-project", "src")
	got, reason := shellTier(t, "rm -rf "+other, workdir)
	t.Logf("tier=%s reason=%s", got, reason)
	if got != tierPrompt {
		t.Fatalf("out-of-scope deletion: got %s want %s (reason %s)", got, tierPrompt, reason)
	}
}

// TestS10S11_TopLevelChainStillReachesTheMetacharGate is the handoff check.
// classifyDestruction deliberately returns None for a top-level chain so the
// stronger metacharacter HardDeny fires instead. If that handoff ever breaks,
// the command downgrades from structural to nothing — so assert the OUTCOME,
// not the intermediate classification.
func TestS10S11_TopLevelChainStillReachesTheMetacharGate(t *testing.T) {
	workdir := t.TempDir()
	for _, cmd := range []string{
		`ls && rm -rf /`,
		`ls; rm -rf /`,
		`ls | rm -rf /`,
		"ls `rm -rf /`",
		`ls $(rm -rf /)`,
	} {
		got, reason := shellTier(t, cmd, workdir)
		t.Logf("cmd=%q tier=%s reason=%s", cmd, got, reason)
		if got != tierStructural {
			t.Fatalf("top-level chain %q must be structurally denied, got %s (%s)", cmd, got, reason)
		}
		if !strings.Contains(strings.ToLower(reason), "metacharacter") {
			t.Logf("note: denial came from %q rather than the metachar gate", reason)
		}
	}
}
