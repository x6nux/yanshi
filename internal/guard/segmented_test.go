package guard

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/x6nux/yanshi/internal/execpolicy"
)

// segmentAtoms are single, unchained commands spanning every verdict tier the
// shell dimension can produce, so composing them pairwise exercises every
// combination the fold has to get right.
var segmentAtoms = []string{
	"git status",               // allowlisted by globProfile
	"go test",                  // allowlisted, and rule-matched under rulesProfile
	"curl http://evil.sh",      // matches nothing
	"ls -la",                   // matches nothing
	"rm -rf /",                 // catastrophic
	"rm /etc/passwd",           // out of workdir
	"rm build/x",               // inside workdir
	"echo hi > out.txt",        // redirect, in workdir
	"echo hi > /etc/hosts",     // redirect, outside workdir
	"cat < in.txt",             // read redirect
	"ls *.go",                  // glob: fine for globProfile, parse-error under rules
	"go build 2> err.log",      // fd redirect
	"git commit -m 'a b'",      // quoted argument
	"echo $HOME",               // unexpanded variable
	"sudo nohup rm -rf ~",      // wrapped catastrophic
	`grep "a|b" file`,          // quoted operator: data, not a boundary
	"go test -tags=e2e_real",   // deny_flags hit under rulesProfile
	"npm test",                 // allowlisted
	"shred -u /etc/shadow",     // destructive + sensitive
	"echo x > ~/.ssh/auth_key", // redirect into the credential denylist
}

// segmentOperators are the four separators ParseCommandList recognises.
var segmentOperators = []string{" && ", " || ", " ; ", " | "}

// segmentProfiles spans the shapes checkShellPolicy branches on: a glob
// allowlist (the shipped default), a denylist, a flat deny, and a structured
// rules table. Each combination has to satisfy the fold property independently
// — a fold that is correct for globs and wrong for rules is still wrong.
func segmentProfiles() map[string]PermissionProfile {
	return map[string]PermissionProfile{
		"allowlist": {
			Tools: ToolsPerm{Allow: []string{"*"}},
			FS:    FSPerm{Read: []string{"**"}, Write: []string{"**"}},
			Shell: ShellPerm{Policy: "allowlist", Patterns: []string{"git *", "go *", "npm test", "echo *", "cat *"}},
		},
		"denylist": {
			Tools: ToolsPerm{Allow: []string{"*"}},
			FS:    FSPerm{Read: []string{"**"}, Write: []string{"**"}},
			Shell: ShellPerm{Policy: "denylist", Patterns: []string{"curl *", "rm *"}},
		},
		"deny": {
			Tools: ToolsPerm{Allow: []string{"*"}},
			FS:    FSPerm{Read: []string{"**"}, Write: []string{"**"}},
			Shell: ShellPerm{Policy: "deny"},
		},
		"rules": {
			Tools: ToolsPerm{Allow: []string{"*"}},
			FS:    FSPerm{Read: []string{"**"}, Write: []string{"**"}},
			Shell: ShellPerm{Rules: []execpolicy.Rule{
				{ID: "no-real-e2e", Program: "go", Prefix: []string{"test"}, Decision: "deny", DenyFlags: []string{"-tags=e2e_real"}},
				{ID: "go-test", Program: "go", Prefix: []string{"test"}, Decision: "allow"},
				{ID: "git", Program: "git", Decision: "prompt"},
			}},
		},
		"emptyFSWrite": {
			Tools: ToolsPerm{Allow: []string{"*"}},
			FS:    FSPerm{Read: []string{"**"}},
			Shell: ShellPerm{Policy: "allowlist", Patterns: []string{"*"}},
		},
	}
}

const segTestWorkdir = "/work/project"

func segAction(cmd string) Action {
	return Action{Tool: "shell_run", Shell: cmd, Workdir: segTestWorkdir}
}

// TestSegmentedShellIsNeverMorePermissiveThanItsSegments is INF1's central
// safety property, and the one the spec asked for before a line of the parser
// was written: judging a chain segment by segment must never produce a verdict
// milder than the strictest verdict any of its segments earns alone.
//
// It is the only thing standing between "the metacharacter HardDeny was
// refined" and "the metacharacter HardDeny was removed". A fold that returned
// the LAST segment's decision, or the first Allow it found, or that collapsed
// the two HardDeny tiers, passes every hand-written example in this file and
// fails here.
//
// Severity, not equality, is the assertion: a chain may legitimately be
// STRICTER than any single segment (the destructive gate can grade the pair
// differently), and pinning equality would forbid future tightening.
func TestSegmentedShellIsNeverMorePermissiveThanItsSegments(t *testing.T) {
	g := New()
	checked := 0
	for pname, prof := range segmentProfiles() {
		for _, a := range segmentAtoms {
			for _, b := range segmentAtoms {
				for _, op := range segmentOperators {
					chain := a + op + b
					if _, err := execpolicy.ParseCommandList(chain); err != nil {
						// A chain the segmenter refuses is a structural
						// HardDeny by construction; the property is about the
						// chains that DO get split.
						continue
					}
					want := severity(g.Check(prof, segAction(a)))
					if s := severity(g.Check(prof, segAction(b))); s > want {
						want = s
					}
					got := severity(g.Check(prof, segAction(chain)))
					if got < want {
						t.Errorf("profile %s: Check(%q) severity %d < max(Check(%q), Check(%q)) = %d",
							pname, chain, got, a, b, want)
					}
					checked++
				}
			}
		}
	}
	// Self-proof that the corpus is live (review-checklist.md, C-bis): a
	// generator that produced nothing, or whose every chain failed to parse,
	// would report a clean pass while asserting about the empty set.
	if checked < len(segmentAtoms)*len(segmentAtoms) {
		t.Fatalf("only %d chains reached the assertion; the corpus is not exercising the fold", checked)
	}
}

// TestSegmentedShellMatchesWholeStringForUnchainedCommands is the other half of
// the safety argument: for every command that carries no control operator, the
// segmented path must return exactly what the whole-string path returned.
// Segment.Text is a verbatim slice precisely so this holds, and a future
// "normalize the segment before matching" would break it here rather than in
// production.
func TestSegmentedShellMatchesWholeStringForUnchainedCommands(t *testing.T) {
	g := New()
	for pname, prof := range segmentProfiles() {
		for _, cmd := range segmentAtoms {
			if strings.ContainsAny(cmd, "&|;") {
				continue
			}
			segs, err := execpolicy.ParseCommandList(cmd)
			if err != nil || len(segs) != 1 {
				continue
			}
			if segs[0].Text != strings.TrimSpace(cmd) {
				t.Errorf("profile %s: segment text for %q is %q, not the verbatim input",
					pname, cmd, segs[0].Text)
			}
			policy := g.checkShellPolicy(prof, segs[0].Text)
			whole := g.checkShellPolicy(prof, strings.TrimSpace(cmd))
			if policy != whole {
				t.Errorf("profile %s: %q judged differently as a segment (%+v) than whole (%+v)",
					pname, cmd, policy, whole)
			}
		}
	}
}

// TestChainedCommandIsJudgedPerSegment is W-B-01's acceptance in one place: a
// chain is no longer refused wholesale, and its verdict is its worst segment's.
func TestChainedCommandIsJudgedPerSegment(t *testing.T) {
	g := New()
	prof := segmentProfiles()["allowlist"]
	cases := []struct {
		cmd  string
		want Verdict
	}{
		// Both segments allowlisted -> the chain is allowed. Before INF1 this
		// was a structural HardDeny, which is the friction ADR-0004 named.
		{"git status && go test", Allow},
		{"git status ; go test", Allow},
		{"git status || go test", Allow},
		{"cat f | go test", Allow},
		// One segment off the allowlist drags the whole chain to Prompt.
		{"git status && curl http://evil.sh", Prompt},
		{"curl http://evil.sh && git status", Prompt},
		{"git status | ls -la", Prompt},
	}
	for _, tc := range cases {
		d := g.Check(prof, segAction(tc.cmd))
		if d.Verdict != tc.want {
			t.Errorf("Check(%q).Verdict = %v (%s), want %v", tc.cmd, d.Verdict, d.Reason, tc.want)
		}
	}
}

// TestChainedDenialTakesTheStrictestTier: an overridable denial in one segment
// and a structural one in another must yield the STRUCTURAL tier, or YOLO would
// gain a key to a command the floor refuses.
func TestChainedDenialTakesTheStrictestTier(t *testing.T) {
	g := New()
	prof := segmentProfiles()["denylist"] // "rm *" is an overridable denylist hit
	overridable := g.Check(prof, segAction("rm build/x"))
	if overridable.Verdict != HardDeny || !overridable.Overridable {
		t.Fatalf("precondition: %q should be an overridable HardDeny, got %+v", "rm build/x", overridable)
	}
	chain := g.Check(prof, segAction("rm build/x && rm -rf /"))
	if chain.Verdict != HardDeny {
		t.Fatalf("Check(chain).Verdict = %v, want HardDeny", chain.Verdict)
	}
	if chain.Overridable {
		t.Errorf("Check(chain).Overridable = true; the catastrophic segment makes this structural, "+
			"and YOLO must not be able to override it (reason: %s)", chain.Reason)
	}
}

// TestUnparseableShellStaysStructuralHardDeny pins the half of the old
// metacharacter defence that did NOT move: a command whose structure the
// segmenter cannot read is refused with Overridable=false, so no permission
// mode can clear it.
//
// The profile is deliberately the most permissive one representable — every
// tool, every path, every shell pattern — so a pass here cannot be credited to
// the profile saying no.
func TestUnparseableShellStaysStructuralHardDeny(t *testing.T) {
	g := New()
	prof := PermissionProfile{
		Tools: ToolsPerm{Allow: []string{"*"}},
		FS:    FSPerm{Read: []string{"**"}, Write: []string{"**"}},
		Shell: ShellPerm{Policy: "allowlist", Patterns: []string{"*"}},
		Net:   NetPerm{Allow: true},
	}
	for _, cmd := range []string{
		"ls $(whoami)",
		"ls `whoami`",
		"diff <(a) <(b)",
		"(rm -rf /)",
		"cat <<EOF",
		"sleep 5 &",
		"ls\nrm -rf /",
		`echo "unterminated`,
	} {
		d := g.Check(prof, segAction(cmd))
		if d.Verdict != HardDeny {
			t.Errorf("Check(%q).Verdict = %v, want HardDeny", cmd, d.Verdict)
			continue
		}
		if d.Overridable {
			t.Errorf("Check(%q).Overridable = true; unreadable structure is the floor, not a profile opinion", cmd)
		}
	}
}

// TestDestructiveGateSeesEverySegmentOfAChain is the change without which INF1
// would be a pure loosening.
//
// The deletion classifier used to answer DestructionNone for anything with a
// control operator, on the explicit understanding that checkShell's
// metacharacter HardDeny would refuse it instead. Once checkShell judges
// chains, that handoff has no receiver: `ls && rm -rf /` presents the deletion
// gate with a chain it declines to grade and the shell dimension with two
// individually-plausible commands. Deleting classifyEverySegment turns every
// case below into an Allow or a Prompt.
func TestDestructiveGateSeesEverySegmentOfAChain(t *testing.T) {
	g := New()
	// Maximally permissive: only the structural floor can refuse anything here.
	prof := PermissionProfile{
		Tools: ToolsPerm{Allow: []string{"*"}},
		FS:    FSPerm{Read: []string{"**"}, Write: []string{"**"}},
		Shell: ShellPerm{Policy: "allowlist", Patterns: []string{"*"}},
		Net:   NetPerm{Allow: true},
	}
	catastrophic := []string{
		"ls && rm -rf /",
		"rm -rf / && ls",
		"ls ; rm -rf /",
		"ls | rm -rf /",
		"ls || rm -rf ~",
		"echo hi && sudo nohup rm -rf /",
		"git status && rm -rf / > /dev/null",
	}
	for _, cmd := range catastrophic {
		d := g.Check(prof, segAction(cmd))
		if d.Verdict != HardDeny || d.Overridable {
			t.Errorf("Check(%q) = {%v overridable=%v reason=%q}, want a structural HardDeny",
				cmd, d.Verdict, d.Overridable, d.Reason)
		}
	}
	outOfScope := []string{
		"ls && rm /etc/passwd",
		"rm /etc/passwd ; ls",
	}
	for _, cmd := range outOfScope {
		d := g.Check(prof, segAction(cmd))
		if d.Verdict != Prompt {
			t.Errorf("Check(%q).Verdict = %v (%s), want Prompt", cmd, d.Verdict, d.Reason)
		}
	}
	// The in-workdir control: splitting must not manufacture a denial for a
	// deletion the gate has always allowed.
	inScope := filepath.ToSlash(segTestWorkdir) + "/build"
	if d := g.Check(prof, segAction("ls && rm -rf "+inScope)); d.Verdict != Allow {
		t.Errorf("Check(in-workdir chain).Verdict = %v (%s), want Allow", d.Verdict, d.Reason)
	}
}

// TestDestructiveFloorDoesNotMaskAStricterDimension pins the Check() fold that
// replaced checkDestructive's Prompt short-circuit.
//
// checkDestructive runs first and can return a Prompt (out-of-workdir
// deletion). Returning it immediately would report a milder verdict than the
// shell dimension is about to produce for the SAME command — and after INF1
// that is reachable: a chain can carry an out-of-scope deletion in one segment
// and a rules-table parse-error in another. Restore the short-circuit and this
// drops from HardDeny to Prompt, i.e. from "no mode may run this" to "one click".
func TestDestructiveFloorDoesNotMaskAStricterDimension(t *testing.T) {
	g := New()
	// A rules profile that ADMITS rm, so the only thing refusing
	// `rm /etc/passwd` is the destructive dimension's out-of-workdir Prompt.
	// The other half of the chain (`ls *.go`) is a strict-lexer parse error,
	// i.e. a structural HardDeny from the shell dimension.
	prof := PermissionProfile{
		Tools: ToolsPerm{Allow: []string{"*"}},
		FS:    FSPerm{Read: []string{"**"}, Write: []string{"**"}},
		Shell: ShellPerm{Rules: []execpolicy.Rule{
			{ID: "rm-ok", Program: "rm", Decision: "allow"},
			{ID: "ls-ok", Program: "ls", Decision: "allow"},
		}},
	}
	// Precondition: each half alone.
	if d := g.Check(prof, segAction("rm /etc/passwd")); d.Verdict != Prompt {
		t.Fatalf("precondition: %q = %v (%s), want Prompt", "rm /etc/passwd", d.Verdict, d.Reason)
	}
	if d := g.Check(prof, segAction("ls *.go")); d.Verdict != HardDeny || d.Overridable {
		t.Fatalf("precondition: %q = {%v overridable=%v}, want a structural HardDeny",
			"ls *.go", d.Verdict, d.Overridable)
	}
	d := g.Check(prof, segAction("rm /etc/passwd && ls *.go"))
	if d.Verdict != HardDeny || d.Overridable {
		t.Errorf("Check(chain) = {%v overridable=%v reason=%q}; the destructive Prompt must not "+
			"mask the shell dimension's structural refusal", d.Verdict, d.Overridable, d.Reason)
	}
}

// TestDestructiveFloorStillReportsItsOwnReason is the discriminating half of
// the fold: when nothing downstream is stricter, the operator must still be
// told it was the out-of-workdir deletion that stopped them, not something
// generic. moreSevere keeps the first argument on a tie for exactly this.
func TestDestructiveFloorStillReportsItsOwnReason(t *testing.T) {
	g := New()
	prof := PermissionProfile{
		Tools: ToolsPerm{Allow: []string{"*"}},
		FS:    FSPerm{Read: []string{"**"}, Write: []string{"**"}},
		Shell: ShellPerm{Policy: "allowlist", Patterns: []string{"*"}},
		Net:   NetPerm{Allow: true},
	}
	d := g.Check(prof, segAction("rm /etc/passwd"))
	if d.Verdict != Prompt || !strings.Contains(d.Reason, "outside the working directory") {
		t.Errorf("Check = {%v %q}, want a Prompt naming the out-of-workdir deletion", d.Verdict, d.Reason)
	}
}

// TestRedirectTargetIsJudgedByTheFSDimension pins INF1's third constraint. The
// program of `echo x > ~/.ssh/authorized_keys` is `echo`; a shell dimension
// that reads only the program has read the harmless half of the command.
func TestRedirectTargetIsJudgedByTheFSDimension(t *testing.T) {
	g := New()
	// The write list is empty, so ANY write target is refused while the
	// program itself is allowlisted by "*". A redirect that never reached
	// checkFS would come back Allow.
	prof := segmentProfiles()["emptyFSWrite"]
	if d := g.Check(prof, segAction("echo hi")); d.Verdict != Allow {
		t.Fatalf("precondition: a redirect-free command must be allowed, got %v (%s)", d.Verdict, d.Reason)
	}
	for _, cmd := range []string{
		"echo hi > out.txt",
		"echo hi >> out.txt",
		"echo hi &> out.txt",
		"go build 2> err.log",
		"git status && echo hi > out.txt",
	} {
		if d := g.Check(prof, segAction(cmd)); d.Verdict == Allow {
			t.Errorf("Check(%q) = Allow; the redirect target never reached the FS dimension", cmd)
		}
	}
	// A read redirect is judged as a READ: with reads permitted and writes not,
	// `cat < in.txt` must pass while `echo > out.txt` must not. Getting the
	// direction backwards would look like it works in one direction only.
	if d := g.Check(prof, segAction("cat < in.txt")); d.Verdict != Allow {
		t.Errorf("Check(%q) = %v (%s), want Allow — `<` is a read", "cat < in.txt", d.Verdict, d.Reason)
	}
	// A descriptor duplication has no path and must not be invented one.
	if d := g.Check(prof, segAction("go build 2>&1")); d.Verdict != Allow {
		t.Errorf("Check(%q) = %v (%s), want Allow — 2>&1 names no file", "go build 2>&1", d.Verdict, d.Reason)
	}
}

// TestRedirectIntoTheCredentialDenylistPrompts is the concrete example
// ADR-0004's supplement names: the built-in sensitive-path gate has to be
// reachable from a redirection, not only from fs_write.
func TestRedirectIntoTheCredentialDenylistPrompts(t *testing.T) {
	if homeDir() == "" {
		t.Skip("no HOME/USERPROFILE: the credential denylist is home-relative and cannot resolve")
	}
	g := New()
	prof := PermissionProfile{
		Tools: ToolsPerm{Allow: []string{"*"}},
		FS:    FSPerm{Read: []string{"**"}, Write: []string{"**"}},
		Shell: ShellPerm{Policy: "allowlist", Patterns: []string{"*"}},
		Net:   NetPerm{Allow: true},
	}
	cmd := "echo ssh-rsa AAAA > ~/.ssh/authorized_keys"
	d := g.Check(prof, segAction(cmd))
	if d.Verdict != Prompt {
		t.Fatalf("Check(%q) = %v (%s), want Prompt from the credential denylist", cmd, d.Verdict, d.Reason)
	}
	if !strings.Contains(d.Reason, ".ssh") {
		t.Errorf("Reason = %q; want it to name the credential path it refused", d.Reason)
	}
}

// TestQuotedOperatorIsNotASegmentBoundary guards the terminating condition the
// old top-level handoff hid. destructive.go's own splitter is quote-aware while
// its hasControlOperator check is a bare strings.Contains, so feeding it a
// string like this one and recursing on the result does not shrink the input —
// measured as a stack overflow before classifyEverySegment took over the
// splitting. This is the shape that would bring it back.
func TestQuotedOperatorIsNotASegmentBoundary(t *testing.T) {
	g := New()
	prof := segmentProfiles()["allowlist"]
	for _, cmd := range []string{
		`grep "a|b" file`,
		`grep 'a && b' file`,
		`echo "a;b"`,
		`sh -c 'grep "a|b" x'`,
	} {
		// The assertion is that this RETURNS. A verdict of any kind is fine.
		d := g.Check(prof, segAction(cmd))
		if d.Verdict != Allow && d.Verdict != Prompt && d.Verdict != HardDeny {
			t.Errorf("Check(%q) produced an impossible verdict %v", cmd, d.Verdict)
		}
	}
}

// TestMoreSevereIsATotalOrderOverTheFourTiers pins the fold primitive itself.
// Every safety claim above is stated in terms of it, so a moreSevere that
// silently preferred the wrong side would make those tests assert nothing.
func TestMoreSevereIsATotalOrderOverTheFourTiers(t *testing.T) {
	tiers := []Decision{
		allow(),
		prompt("p"),
		overridableDeny("od"),
		hardDeny("sd"),
	}
	for i, a := range tiers {
		for j, b := range tiers {
			got := moreSevere(a, b)
			wantIdx := i
			if j > i {
				wantIdx = j
			}
			if severity(got) != severity(tiers[wantIdx]) {
				t.Errorf("moreSevere(%d,%d) severity = %d, want %d",
					i, j, severity(got), severity(tiers[wantIdx]))
			}
		}
	}
	// Ties keep the first argument so a left-to-right fold reports the earliest
	// offending segment.
	first := prompt("first")
	second := prompt("second")
	if got := moreSevere(first, second); got.Reason != "first" {
		t.Errorf("moreSevere tie kept %q, want the first argument", got.Reason)
	}
	// …except that a witness carrying an execpolicy RuleID beats a bare Allow,
	// which is what keeps the explainability signal alive through the fold.
	witness := Decision{Verdict: Allow, RuleID: "go-test", Justification: "ordinary Go tests"}
	if got := moreSevere(allow(), witness); got.RuleID != "go-test" {
		t.Errorf("moreSevere dropped the explaining Allow: %+v", got)
	}
}

// TestSegmentedShellReportsTheOffendingSegment: a denial for a chain has to
// name the segment that caused it, not the whole string. An operator told
// `shell command "git status && curl http://evil.sh" not on allowlist` cannot
// tell which half to fix.
func TestSegmentedShellReportsTheOffendingSegment(t *testing.T) {
	g := New()
	prof := segmentProfiles()["allowlist"]
	d := g.Check(prof, segAction("git status && curl http://evil.sh"))
	if !strings.Contains(d.Reason, "curl http://evil.sh") {
		t.Errorf("Reason = %q, want it to name the offending segment", d.Reason)
	}
	if strings.Contains(d.Reason, "git status") {
		t.Errorf("Reason = %q, want it to exclude the segment that was fine", d.Reason)
	}
}

// TestCheckIsStillStatelessAcrossSegments re-asserts ADR-0004's first decision
// against the new loop: repeated Checks of the same command, and of different
// commands interleaved, must return identical verdicts. A fold that accumulated
// into a receiver field instead of a local would pass every test above once and
// drift on the second call.
func TestCheckIsStillStatelessAcrossSegments(t *testing.T) {
	g := New()
	prof := segmentProfiles()["allowlist"]
	baseline := map[string]Decision{}
	for _, cmd := range segmentAtoms {
		baseline[cmd] = g.Check(prof, segAction(cmd))
	}
	for round := 0; round < 3; round++ {
		for _, cmd := range segmentAtoms {
			// Interleave a chain so any cross-call state would show up.
			g.Check(prof, segAction(cmd+" && rm -rf /"))
			if got := g.Check(prof, segAction(cmd)); got != baseline[cmd] {
				t.Fatalf("round %d: Check(%q) drifted from %+v to %+v", round, cmd, baseline[cmd], got)
			}
		}
	}
}
