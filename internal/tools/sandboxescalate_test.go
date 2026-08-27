package tools

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/x6nux/yanshi/internal/guard"
	"github.com/x6nux/yanshi/internal/sandbox"
)

// fakeSandbox is a Sandbox whose Report is whatever the test says it is. That
// is the entire dependency the escalation loop has on the sandbox package —
// which is deliberate, because three other agents are rewriting the real
// backends this round and a test coupled to their in-flight state would
// measure them rather than this.
type fakeSandbox struct{ rep sandbox.CapabilityReport }

func (f *fakeSandbox) Prepare(context.Context, *exec.Cmd, sandbox.CommandSpec) error { return nil }
func (f *fakeSandbox) Report() sandbox.CapabilityReport                              { return f.rep }
func (f *fakeSandbox) Close() error                                                  { return nil }

func newFakeSandbox(rep sandbox.CapabilityReport) sandbox.Sandbox { return &fakeSandbox{rep: rep} }

// escalationTestCtx builds the context the loop reads: a permission profile
// (RequireApproval fails closed without one), an enforcing sandbox, and
// optionally a permission callback.
func escalationTestCtx(t *testing.T, enforcing bool, ask func(PermissionRequest) PermissionDecision) context.Context {
	t.Helper()
	ctx := WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"*"}},
	})
	rep := sandbox.CapabilityReport{
		Platform: "linux", Requested: sandbox.ReadOnly, Backend: "landlock",
		Effective: sandbox.DegradedHostGuard, Enforced: false,
	}
	if enforcing {
		rep.Effective = sandbox.OSIsolated
		rep.Enforced = true
	}
	ctx = WithSandbox(ctx, newFakeSandbox(rep))
	if ask != nil {
		ctx = WithPermissionCallback(ctx, ask)
	}
	return ctx
}

// deniedAttempt is the shape of a command the sandbox refused.
func deniedAttempt() SandboxAttempt {
	return SandboxAttempt{ExitCode: 1, Stderr: "cat: /etc/shadow: Permission denied"}
}

// TestEscalationApprovedRetriesOnceAtHigherTier is path 1: the operator says
// yes, the command runs a second time, and the second run is at a strictly
// higher tier.
func TestEscalationApprovedRetriesOnceAtHigherTier(t *testing.T) {
	var tiers []sandbox.AccessTier
	var prompts int
	ctx := escalationTestCtx(t, true, func(PermissionRequest) PermissionDecision {
		prompts++
		return PermissionAllow
	})
	_, esc, err := EscalateOnSandboxViolation(ctx, "shell_run", "cat /etc/shadow", sandbox.ReadOnly,
		func(_ context.Context, tier sandbox.AccessTier) (SandboxAttempt, error) {
			tiers = append(tiers, tier)
			if len(tiers) == 1 {
				return deniedAttempt(), nil
			}
			return SandboxAttempt{ExitCode: 0, Stdout: "root:x:0:0"}, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	if esc.Outcome != EscalationRetried {
		t.Fatalf("Outcome = %q, want %q", esc.Outcome, EscalationRetried)
	}
	if len(tiers) != 2 {
		t.Fatalf("ran %d times, want exactly 2", len(tiers))
	}
	if tiers[0] != sandbox.ReadOnly || tiers[1] != sandbox.WorkspaceWrite {
		t.Fatalf("tiers = %v, want [read-only workspace-write]", tiers)
	}
	if tiers[1] <= tiers[0] {
		t.Fatal("the retry must run at a STRICTLY higher tier")
	}
	if prompts != 1 {
		t.Fatalf("asked the operator %d times, want exactly 1", prompts)
	}
	if esc.FromTier != sandbox.ReadOnly || esc.ToTier != sandbox.WorkspaceWrite {
		t.Fatalf("tier record = %v->%v", esc.FromTier, esc.ToTier)
	}
}

// TestEscalationRetriesAtMostOnce: a command denied at BOTH tiers must not
// walk the operator up the whole ladder one dialog at a time.
func TestEscalationRetriesAtMostOnce(t *testing.T) {
	runs, prompts := 0, 0
	ctx := escalationTestCtx(t, true, func(PermissionRequest) PermissionDecision {
		prompts++
		return PermissionAllow
	})
	_, esc, err := EscalateOnSandboxViolation(ctx, "shell_run", "cat /etc/shadow", sandbox.ReadOnly,
		func(context.Context, sandbox.AccessTier) (SandboxAttempt, error) {
			runs++
			return deniedAttempt(), nil
		})
	if err != nil {
		t.Fatal(err)
	}
	if runs != 2 {
		t.Fatalf("ran %d times, want exactly 2 (one retry, never a loop)", runs)
	}
	if prompts != 1 {
		t.Fatalf("prompted %d times, want exactly 1", prompts)
	}
	if esc.Outcome != EscalationRetried {
		t.Fatalf("Outcome = %q", esc.Outcome)
	}
}

// TestEscalationDeniedExplainsTheFailure is path 2: the operator says no, the
// command is NOT re-run, and the model gets a usable explanation instead of a
// bare exit code.
func TestEscalationDeniedExplainsTheFailure(t *testing.T) {
	runs := 0
	ctx := escalationTestCtx(t, true, func(PermissionRequest) PermissionDecision {
		return PermissionDeny
	})
	_, esc, err := EscalateOnSandboxViolation(ctx, "shell_run", "cat /etc/shadow", sandbox.ReadOnly,
		func(context.Context, sandbox.AccessTier) (SandboxAttempt, error) {
			runs++
			return deniedAttempt(), nil
		})
	if err != nil {
		t.Fatal(err)
	}
	if runs != 1 {
		t.Fatalf("ran %d times, want 1 — a denial must not retry", runs)
	}
	if esc.Outcome != EscalationDenied {
		t.Fatalf("Outcome = %q, want %q", esc.Outcome, EscalationDenied)
	}
	if esc.Retried() {
		t.Fatal("Retried() must be false on a denial")
	}
	for _, want := range []string{"sandbox denied access", "/etc/shadow", "did not approve"} {
		if !strings.Contains(esc.Explanation, want) {
			t.Fatalf("explanation %q is missing %q", esc.Explanation, want)
		}
	}
}

// TestEscalationTimeoutIsNeverAnApproval is path 3, and the single most
// important assertion in this file.
//
// A timeout arrives here exactly as the WS transport delivers it: S5's
// awaitDecision returns PermissionDeny when the prompt expires, when the
// connection is latched unattended, and when the turn aborts. All three are
// modelled as the callback returning PermissionDeny — which is what the real
// callback does — and none of them may retry.
func TestEscalationTimeoutIsNeverAnApproval(t *testing.T) {
	// The three ways S5 reports "nobody answered", plus the shapes a
	// mis-implemented callback could produce.
	timeoutish := []struct {
		name     string
		decision PermissionDecision
	}{
		{"prompt expired", PermissionDeny},
		{"unattended latch", PermissionDeny},
		{"turn aborted", PermissionDeny},
		{"empty decision", PermissionDecision("")},
		{"garbage decision", PermissionDecision("maybe")},
		{"timeout spelled as a word", PermissionDecision("timeout")},
	}
	for _, tc := range timeoutish {
		t.Run(tc.name, func(t *testing.T) {
			runs := 0
			ctx := escalationTestCtx(t, true, func(PermissionRequest) PermissionDecision {
				return tc.decision
			})
			_, esc, err := EscalateOnSandboxViolation(ctx, "shell_run", "cat /etc/shadow",
				sandbox.ReadOnly, func(context.Context, sandbox.AccessTier) (SandboxAttempt, error) {
					runs++
					return deniedAttempt(), nil
				})
			if err != nil {
				t.Fatal(err)
			}
			if runs != 1 {
				t.Fatalf("%q produced %d runs — a non-approval MUST NOT retry", tc.decision, runs)
			}
			if esc.Retried() {
				t.Fatalf("%q was treated as an approval", tc.decision)
			}
			if esc.Outcome != EscalationDenied {
				t.Fatalf("Outcome = %q, want %q", esc.Outcome, EscalationDenied)
			}
			if esc.ToTier != esc.FromTier {
				t.Fatalf("tier moved from %v to %v without an approval", esc.FromTier, esc.ToTier)
			}
		})
	}
}

// TestEscalationNoCallbackFailsClosed is path 4: SSE has no interactive
// channel, so there is nobody to ask and nothing is escalated.
func TestEscalationNoCallbackFailsClosed(t *testing.T) {
	runs := 0
	ctx := escalationTestCtx(t, true, nil) // no callback
	_, esc, err := EscalateOnSandboxViolation(ctx, "shell_run", "cat /etc/shadow", sandbox.ReadOnly,
		func(context.Context, sandbox.AccessTier) (SandboxAttempt, error) {
			runs++
			return deniedAttempt(), nil
		})
	if err != nil {
		t.Fatal(err)
	}
	if runs != 1 {
		t.Fatalf("ran %d times with no callback bound, want 1", runs)
	}
	if esc.Outcome != EscalationNoCallback {
		t.Fatalf("Outcome = %q, want %q", esc.Outcome, EscalationNoCallback)
	}
	if !strings.Contains(esc.Explanation, "no interactive approval channel") {
		t.Fatalf("explanation must say why nobody was asked: %q", esc.Explanation)
	}
}

// TestEscalationNeverAllowsWithoutExplicitApproval is the exhaustive form of
// the property the four paths above sample. Every PermissionDecision the type
// admits, plus several it does not, must fail to produce a retry unless it is
// exactly PermissionAllow.
//
// AlwaysAllow / AllowSession / AllowPersistent are in the ALLOW column because
// RequireApproval accepts PermissionAllow and PermissionAlwaysAllow — its
// documented contract. Pinning the whole vocabulary here means a future edit
// to that switch cannot silently widen what counts as consent to a privilege
// increase.
func TestEscalationNeverAllowsWithoutExplicitApproval(t *testing.T) {
	every := []PermissionDecision{
		PermissionAllow, PermissionDeny, PermissionAlwaysAllow,
		PermissionAllowSession, PermissionAllowPersistent,
		"", "allow ", "ALLOW", "yes", "true", "1", "ok", "timeout", "expired",
	}
	// RequireApproval's contract: only these two admit.
	admits := map[PermissionDecision]bool{
		PermissionAllow:       true,
		PermissionAlwaysAllow: true,
	}
	for _, d := range every {
		d := d
		t.Run(string("decision="+d), func(t *testing.T) {
			runs := 0
			ctx := escalationTestCtx(t, true, func(PermissionRequest) PermissionDecision { return d })
			_, esc, err := EscalateOnSandboxViolation(ctx, "shell_run", "cat /etc/shadow",
				sandbox.ReadOnly, func(context.Context, sandbox.AccessTier) (SandboxAttempt, error) {
					runs++
					return deniedAttempt(), nil
				})
			if err != nil {
				t.Fatal(err)
			}
			wantRetry := admits[d]
			if esc.Retried() != wantRetry {
				t.Fatalf("decision %q: Retried() = %v, want %v", d, esc.Retried(), wantRetry)
			}
			wantRuns := 1
			if wantRetry {
				wantRuns = 2
			}
			if runs != wantRuns {
				t.Fatalf("decision %q: %d runs, want %d", d, runs, wantRuns)
			}
		})
	}
}

// TestEscalationSkipsNonViolations: an ordinary command failure must never
// open an escalation dialog. This is the false-positive cost of the whole
// feature and it must be zero for the common case.
func TestEscalationSkipsNonViolations(t *testing.T) {
	cases := []struct {
		name    string
		attempt SandboxAttempt
	}{
		{"success", SandboxAttempt{ExitCode: 0, Stdout: "ok"}},
		{"compile error", SandboxAttempt{ExitCode: 2, Stderr: "./main.go:7:2: undefined: foo"}},
		{"test failure", SandboxAttempt{ExitCode: 1, Stdout: "FAIL\tpkg\t0.1s"}},
		{"clean exit despite scary stderr", SandboxAttempt{ExitCode: 0, Stderr: "Permission denied"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prompts := 0
			ctx := escalationTestCtx(t, true, func(PermissionRequest) PermissionDecision {
				prompts++
				return PermissionAllow
			})
			_, esc, err := EscalateOnSandboxViolation(ctx, "shell_run", "go build ./...",
				sandbox.ReadOnly, func(context.Context, sandbox.AccessTier) (SandboxAttempt, error) {
					return tc.attempt, nil
				})
			if err != nil {
				t.Fatal(err)
			}
			if prompts != 0 {
				t.Fatalf("an ordinary failure opened %d escalation dialogs", prompts)
			}
			if esc.Outcome != EscalationNone {
				t.Fatalf("Outcome = %q, want %q", esc.Outcome, EscalationNone)
			}
			if esc.Explanation != "" {
				t.Fatalf("a non-violation must carry no explanation, got %q", esc.Explanation)
			}
		})
	}
}

// TestEscalationDegradedSandboxNeverEscalates: on a host with no OS isolation
// the sandbox denied nothing, so a "Permission denied" from a chmod-000 file
// must not become a tier-increase dialog. Without this every Phase-0
// deployment would prompt constantly.
func TestEscalationDegradedSandboxNeverEscalates(t *testing.T) {
	prompts := 0
	ctx := escalationTestCtx(t, false, func(PermissionRequest) PermissionDecision {
		prompts++
		return PermissionAllow
	})
	_, esc, err := EscalateOnSandboxViolation(ctx, "shell_run", "cat /etc/shadow", sandbox.ReadOnly,
		func(context.Context, sandbox.AccessTier) (SandboxAttempt, error) {
			return deniedAttempt(), nil
		})
	if err != nil {
		t.Fatal(err)
	}
	if prompts != 0 {
		t.Fatalf("a degraded sandbox opened %d dialogs", prompts)
	}
	if esc.Outcome != EscalationNone {
		t.Fatalf("Outcome = %q, want %q", esc.Outcome, EscalationNone)
	}
}

// TestEscalationNoSandboxBoundNeverEscalates: same, for the many call paths
// (unit tests, SSE, headless) that bind no sandbox at all.
func TestEscalationNoSandboxBoundNeverEscalates(t *testing.T) {
	prompts := 0
	ctx := WithProfile(context.Background(), guard.PermissionProfile{Tools: guard.ToolsPerm{Allow: []string{"*"}}})
	ctx = WithPermissionCallback(ctx, func(PermissionRequest) PermissionDecision {
		prompts++
		return PermissionAllow
	})
	_, esc, err := EscalateOnSandboxViolation(ctx, "shell_run", "cat /etc/shadow", sandbox.ReadOnly,
		func(context.Context, sandbox.AccessTier) (SandboxAttempt, error) {
			return deniedAttempt(), nil
		})
	if err != nil {
		t.Fatal(err)
	}
	if prompts != 0 || esc.Outcome != EscalationNone {
		t.Fatalf("no sandbox bound: prompts=%d outcome=%q", prompts, esc.Outcome)
	}
}

// TestEscalationTierExhausted: at FullAccess there is no higher tier, so the
// operator is not asked a question whose only possible answer changes nothing.
func TestEscalationTierExhausted(t *testing.T) {
	prompts, runs := 0, 0
	ctx := escalationTestCtx(t, true, func(PermissionRequest) PermissionDecision {
		prompts++
		return PermissionAllow
	})
	_, esc, err := EscalateOnSandboxViolation(ctx, "shell_run", "cat /etc/shadow", sandbox.FullAccess,
		func(context.Context, sandbox.AccessTier) (SandboxAttempt, error) {
			runs++
			return deniedAttempt(), nil
		})
	if err != nil {
		t.Fatal(err)
	}
	if prompts != 0 {
		t.Fatalf("asked to escalate beyond the top tier %d times", prompts)
	}
	if runs != 1 {
		t.Fatalf("ran %d times at the top tier", runs)
	}
	if esc.Outcome != EscalationTierExhausted {
		t.Fatalf("Outcome = %q, want %q", esc.Outcome, EscalationTierExhausted)
	}
	if !strings.Contains(esc.Explanation, "widest available tier") {
		t.Fatalf("explanation = %q", esc.Explanation)
	}
}

// TestEscalationLaunchErrorShortCircuits: a command that never started cannot
// have been refused by a sandbox, so no classification and no dialog.
func TestEscalationLaunchErrorShortCircuits(t *testing.T) {
	prompts := 0
	ctx := escalationTestCtx(t, true, func(PermissionRequest) PermissionDecision {
		prompts++
		return PermissionAllow
	})
	wantErr := context.Canceled
	_, esc, err := EscalateOnSandboxViolation(ctx, "shell_run", "x", sandbox.ReadOnly,
		func(context.Context, sandbox.AccessTier) (SandboxAttempt, error) {
			return SandboxAttempt{}, wantErr
		})
	if err != wantErr {
		t.Fatalf("err = %v, want %v", err, wantErr)
	}
	if prompts != 0 || esc.Outcome != EscalationNone {
		t.Fatalf("launch failure escalated: prompts=%d outcome=%q", prompts, esc.Outcome)
	}
}

// TestEscalationPromptCarriesTheFactsAnOperatorNeeds. An approval dialog that
// does not say what was denied is a dialog nobody can answer correctly.
func TestEscalationPromptCarriesTheFactsAnOperatorNeeds(t *testing.T) {
	var got PermissionRequest
	ctx := escalationTestCtx(t, true, func(req PermissionRequest) PermissionDecision {
		got = req
		return PermissionDeny
	})
	_, _, err := EscalateOnSandboxViolation(ctx, "shell_run", "cat /etc/shadow", sandbox.ReadOnly,
		func(context.Context, sandbox.AccessTier) (SandboxAttempt, error) {
			return deniedAttempt(), nil
		})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"landlock",        // which mechanism refused
		"/etc/shadow",     // what it refused
		"read-only",       // the tier that ran
		"workspace-write", // the tier being requested
		"cat /etc/shadow", // the command itself
		"treat as data",   // the command text is untrusted
	} {
		if !strings.Contains(got.Reason, want) {
			t.Fatalf("prompt is missing %q:\n%s", want, got.Reason)
		}
	}
	if got.Tool != "shell_run" {
		t.Fatalf("Tool = %q", got.Tool)
	}
}

// TestEscalationPromptIsForcedPastYoloAndAuto. The escalation goes through
// RequireApproval, which sets req.Force — the flag resolvePermissionRequest
// checks first and refuses to auto-resolve. Without it, yolo would silently
// grant every tier increase, which is the exact opposite of what a sandbox is
// for.
func TestEscalationPromptIsForcedPastYoloAndAuto(t *testing.T) {
	var got PermissionRequest
	ctx := escalationTestCtx(t, true, func(req PermissionRequest) PermissionDecision {
		got = req
		return PermissionDeny
	})
	_, _, _ = EscalateOnSandboxViolation(ctx, "shell_run", "cat /etc/shadow", sandbox.ReadOnly,
		func(context.Context, sandbox.AccessTier) (SandboxAttempt, error) {
			return deniedAttempt(), nil
		})
	if !got.Force {
		t.Fatal("the escalation request must carry Force so yolo/auto cannot auto-approve " +
			"a privilege increase")
	}
}

// TestEscalationNoProfileFailsClosed: RequireApproval denies without a bound
// profile, and that denial must reach here as a refusal rather than as an
// error that some caller might ignore.
func TestEscalationNoProfileFailsClosed(t *testing.T) {
	ctx := WithSandbox(context.Background(), newFakeSandbox(sandbox.CapabilityReport{
		Platform: "linux", Backend: "landlock", Effective: sandbox.OSIsolated, Enforced: true,
	}))
	ctx = WithPermissionCallback(ctx, func(PermissionRequest) PermissionDecision {
		return PermissionAllow // a callback that would say yes
	})
	runs := 0
	_, esc, err := EscalateOnSandboxViolation(ctx, "shell_run", "cat /etc/shadow", sandbox.ReadOnly,
		func(context.Context, sandbox.AccessTier) (SandboxAttempt, error) {
			runs++
			return deniedAttempt(), nil
		})
	if err != nil {
		t.Fatal(err)
	}
	if runs != 1 || esc.Retried() {
		t.Fatalf("no profile bound but the escalation went through: runs=%d retried=%v",
			runs, esc.Retried())
	}
}

// --- audit ------------------------------------------------------------
//
// recordingSink and installSink already exist in auditsink_test.go and are
// reused verbatim. A second copy would be the "duplicate logic" this repo's
// conventions forbid, and worse: the two would drift and a reader could no
// longer tell which one a given test installed.

// TestEscalationIsAudited: a granted privilege increase that leaves no trail
// is the failure S6 exists to prevent, and an escalation is the single most
// consequential grant the agent can obtain at runtime.
func TestEscalationIsAudited(t *testing.T) {
	cases := []struct {
		name         string
		decision     PermissionDecision
		baseTier     sandbox.AccessTier
		wantDecision string
		wantReason   string
	}{
		{"approved", PermissionAllow, sandbox.ReadOnly, "allow", string(EscalationRetried)},
		{"denied", PermissionDeny, sandbox.ReadOnly, "deny", string(EscalationDenied)},
		{"exhausted", PermissionAllow, sandbox.FullAccess, "deny", string(EscalationTierExhausted)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sink := &recordingSink{}
			installSink(t, sink)

			ctx := escalationTestCtx(t, true, func(PermissionRequest) PermissionDecision {
				return tc.decision
			})
			_, _, err := EscalateOnSandboxViolation(ctx, "shell_run", "cat /etc/shadow", tc.baseTier,
				func(context.Context, sandbox.AccessTier) (SandboxAttempt, error) {
					return deniedAttempt(), nil
				})
			if err != nil {
				t.Fatal(err)
			}
			var found *PermissionAuditRecord
			for i, rec := range sink.all() {
				if strings.HasPrefix(rec.Source, "sandbox_escalation") {
					found = &sink.all()[i]
					break
				}
			}
			if found == nil {
				t.Fatalf("no sandbox_escalation audit record; got %+v", sink.all())
			}
			if found.Decision != tc.wantDecision {
				t.Fatalf("Decision = %q, want %q", found.Decision, tc.wantDecision)
			}
			if found.ReasonCode != tc.wantReason {
				t.Fatalf("ReasonCode = %q, want %q", found.ReasonCode, tc.wantReason)
			}
			if !strings.Contains(found.CmdDigest, "cat /etc/shadow") {
				t.Fatalf("CmdDigest = %q, must identify the command", found.CmdDigest)
			}
		})
	}
}

// TestEscalationAuditRecordsTheTierTransition: "allow" alone does not say what
// was granted. The archive has to answer "how much wider".
func TestEscalationAuditRecordsTheTierTransition(t *testing.T) {
	sink := &recordingSink{}
	installSink(t, sink)

	ctx := escalationTestCtx(t, true, func(PermissionRequest) PermissionDecision {
		return PermissionAllow
	})
	_, _, err := EscalateOnSandboxViolation(ctx, "shell_run", "cat /etc/shadow", sandbox.ReadOnly,
		func(context.Context, sandbox.AccessTier) (SandboxAttempt, error) {
			return deniedAttempt(), nil
		})
	if err != nil {
		t.Fatal(err)
	}
	want := "sandbox_escalation:read-only->workspace-write"
	for _, rec := range sink.all() {
		if rec.Source == want {
			return
		}
	}
	t.Fatalf("no record with Source=%q; got %+v", want, sink.all())
}

// TestEscalationAuditDigestIsBounded: the digest is caller-influenced text and
// must not carry an unbounded command into the archive.
func TestEscalationAuditDigestIsBounded(t *testing.T) {
	sink := &recordingSink{}
	installSink(t, sink)

	huge := "echo " + strings.Repeat("A", 100000)
	ctx := escalationTestCtx(t, true, func(PermissionRequest) PermissionDecision {
		return PermissionDeny
	})
	_, _, err := EscalateOnSandboxViolation(ctx, "shell_run", huge, sandbox.ReadOnly,
		func(context.Context, sandbox.AccessTier) (SandboxAttempt, error) {
			return deniedAttempt(), nil
		})
	if err != nil {
		t.Fatal(err)
	}
	for _, rec := range sink.all() {
		if len(rec.CmdDigest) > maxAuditDigestBytes {
			t.Fatalf("digest is %d bytes, cap is %d", len(rec.CmdDigest), maxAuditDigestBytes)
		}
	}
}

// TestEscalationHangingCallbackNeverEscalates is the timeout path in its
// HARDEST form: a callback that does not answer at all until the turn's
// context is cancelled.
//
// TestEscalationTimeoutIsNeverAnApproval covers the shapes S5 RETURNS for an
// expiry, which is the realistic case -- awaitDecision converts every one of
// them into a decision value before this code sees it. But that test can only
// prove the loop handles a returned deny; it cannot prove the loop has no
// "carry on if nobody answered in time" branch, because a callback that
// answers immediately never exercises waiting.
//
// This one blocks until the caller's context is done and then returns the
// zero-value decision, which is what a genuinely abandoned prompt degrades to.
// The assertion is the tier: an escalation is a privilege increase, and the
// one outcome that must be unreachable without a human saying yes is a retry
// at a wider tier.
func TestEscalationHangingCallbackNeverEscalates(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	base := escalationTestCtx(t, true, func(PermissionRequest) PermissionDecision {
		// Nobody is at the keyboard. Wait for the turn to be torn down, then
		// answer with the zero value -- the shape an abandoned prompt has once
		// its channel is closed without a send.
		<-ctx.Done()
		return PermissionDecision("")
	})
	// Cancel shortly after the prompt is issued, standing in for the turn
	// being abandoned or the connection dropping.
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	defer cancel()

	var tiers []sandbox.AccessTier
	_, esc, err := EscalateOnSandboxViolation(base, "shell_run", "cat /etc/shadow",
		sandbox.ReadOnly, func(_ context.Context, tier sandbox.AccessTier) (SandboxAttempt, error) {
			tiers = append(tiers, tier)
			return deniedAttempt(), nil
		})
	if err != nil {
		t.Fatal(err)
	}
	if len(tiers) != 1 {
		t.Fatalf("the command ran %d times (%v); an unanswered prompt MUST NOT retry",
			len(tiers), tiers)
	}
	if esc.Retried() {
		t.Fatal("an unanswered prompt was treated as consent to widen the sandbox")
	}
	if esc.ToTier != esc.FromTier {
		t.Fatalf("tier widened from %v to %v with nobody answering", esc.FromTier, esc.ToTier)
	}
	if esc.Outcome == EscalationRetried {
		t.Fatalf("Outcome = %q on an unanswered prompt", esc.Outcome)
	}
}
