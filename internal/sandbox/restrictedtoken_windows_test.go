//go:build windows

package sandbox

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

// This file holds the tests that require a real Windows kernel to say anything
// about the restricted-token backend: they create real tokens, write real ACEs,
// spawn real processes and assert what the access check did.
//
// ⚠️ HONESTY NOTE, and it is not boilerplate. This backend was written on
// darwin/arm64 and NOTHING IN IT HAS EVER BEEN EXECUTED. It has been
// type-checked by `GOOS=windows go vet ./...`, which compiles the test files
// too, so the API usage is verified against x/sys/windows and against this
// package's own symbols — and nothing more. The first real run is a Windows CI
// leg.
//
// The consequence is why newRestrictedTokens probes before it commits: every
// way this code can be wrong ends in a probe that does not enforce, which
// leaves the report at DegradedHostGuard and the backend behaving exactly as it
// did before the token existed. A mistake here degrades yanshi; it does not
// break it.
//
// Every enforcement assertion below is paired with a CONTROL: the identical
// write performed WITHOUT the token. Without it "the write failed" proves
// nothing — a read-only directory, a full disk or a wrong path produce the same
// exit status, and the sandbox would get credit for a denial it did not make.

// restrictedTokenTestsOptionalEnv lets a host on which DACLs demonstrably do
// not bind downgrade the tests below from a failure to a skip.
//
// It must be set EXPLICITLY. See requireConfiningRestrictedToken.
const restrictedTokenTestsOptionalEnv = "YANSHI_RESTRICTEDTOKEN_TESTS_OPTIONAL"

// requireConfiningRestrictedToken fails, rather than skips, when this host does
// not confine writes (W-B-25).
//
// # Why there is no unconditional "unsupported" branch
//
// CreateRestrictedToken has shipped in every Windows since 2000 and this file
// is //go:build windows, so — exactly as with job objects — there is no state
// that legitimately answers "this platform does not have the mechanism". Every
// skip would therefore be a run that did not happen, and W-B-25's only evidence
// is this leg. A pending item whose CI leg can only skip never converts while
// the board stays green, which is the defect B3 had to come back and fix for
// seccomp and this batch fixed for landlock and job objects.
//
// The one genuine environmental case is a filesystem that does not carry DACLs:
// a workspace on FAT32 or exFAT, or a network share whose server ignores them.
// Every grant then succeeds and confines nothing. A runner in that state sets
// restrictedTokenTestsOptionalEnv where it is configured — one deliberate act,
// recorded next to the runner, rather than a default that answers "fine" for
// the healthy hosts too.
func requireConfiningRestrictedToken(t *testing.T) {
	t.Helper()
	reason, ok := probeRestrictedToken()
	if ok {
		return
	}
	if os.Getenv(restrictedTokenTestsOptionalEnv) != "" {
		t.Skipf("restricted tokens do not confine here and %s is set: %s",
			restrictedTokenTestsOptionalEnv, reason)
	}
	t.Fatalf("a WRITE_RESTRICTED token did not confine writes on this Windows host: %s\n\n"+
		"This is a FAILURE rather than a skip on purpose. CreateRestrictedToken is present "+
		"on every Windows, so this is not an unsupported platform — it is either this "+
		"package's Win32 being wrong (the regression these tests exist to catch) or a "+
		"volume whose filesystem does not carry DACLs. Skipping makes both "+
		"indistinguishable from a verified run. If this runner genuinely cannot enforce, "+
		"set %s where it is configured.", reason, restrictedTokenTestsOptionalEnv)
}

// TestRestrictedTokenProbeAcceptsAWorkingHost asserts the self-check passes
// here.
//
// It is the counterpart to the negative cases in restrictedtoken_test.go: those
// pin that a failed probe degrades, and this pins that the probe is not simply
// refusing everything. A self-check that never succeeded would degrade the
// backend on every host and every degradation test would still be green.
func TestRestrictedTokenProbeAcceptsAWorkingHost(t *testing.T) {
	requireConfiningRestrictedToken(t)
}

// sandboxUnderTest builds a real Windows sandbox over a workspace and a scratch
// directory, and returns it with an UNGRANTED sibling directory.
//
// # Why TEMP is redirected
//
// windowsScratchPaths grants the capability SID on the host's temp directory,
// because a confined child that cannot write TEMP cannot run a compiler. But
// t.TempDir() lives inside that directory, so a naive "outside" target would
// already be granted and every denial assertion would be vacuous — green, and
// proving nothing. Pointing TEMP at a subdirectory of this test's own tree is
// what makes "outside" genuinely outside.
func sandboxUnderTest(t *testing.T, tier AccessTier) (Sandbox, string, string, string) {
	t.Helper()
	base := t.TempDir()
	ws := filepath.Join(base, "workspace")
	scratch := filepath.Join(base, "scratch")
	outside := filepath.Join(base, "outside")
	for _, d := range []string{ws, scratch, outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// LOCALAPPDATA is cleared as well: windowsScratchPaths derives a third
	// candidate from it, and leaving it set would grant the host's real temp
	// directory — the one "outside" is a sibling of.
	t.Setenv("TEMP", scratch)
	t.Setenv("TMP", scratch)
	t.Setenv("LOCALAPPDATA", "")

	sb := New(Config{Enabled: true, Tier: tier, WorkspaceRoot: ws})
	t.Cleanup(func() { _ = sb.Close() })
	return sb, ws, scratch, outside
}

// runPrepared runs a redirect through the backend's own Prepare and reports
// whether it succeeded.
//
// Going through Prepare rather than attaching a token directly is the point:
// Prepare is the only method production calls, and a Prepare that forgot to
// attach the token would leave every direct-token test green while shipping
// unconfined children.
func runPrepared(t *testing.T, sb Sandbox, tier AccessTier, scratch, target string) error {
	t.Helper()
	prog := comspec()
	args := []string{"/c", `(echo yanshi)>"` + target + `"`}
	cmd := exec.Command(prog, args...)
	if err := sb.Prepare(context.Background(), cmd,
		CommandSpec{Path: prog, Args: args, Tier: tier}); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	cmd.Env = []string{
		"SystemRoot=" + os.Getenv("SystemRoot"),
		"windir=" + os.Getenv("windir"),
		"ComSpec=" + prog,
		"TEMP=" + scratch,
		"TMP=" + scratch,
	}
	return cmd.Run()
}

// TestRestrictedTokenConfinesWritesThroughPrepare is W-B-25's end-to-end
// assertion, and the only evidence that the backend confines anything.
//
// All three observations come from one sandbox, and each covers a way the other
// two can be green while the sandbox is useless:
//
//   - the workspace is writable, or the token refuses every command and the
//     operator reads a broken toolchain rather than a policy;
//   - a sibling of the workspace is not, or nothing is being confined;
//   - the CONTROL — the same write with no sandbox at all — succeeds, or the
//     denial above is explained by the directory rather than by the token.
func TestRestrictedTokenConfinesWritesThroughPrepare(t *testing.T) {
	requireConfiningRestrictedToken(t)
	sb, ws, scratch, outside := sandboxUnderTest(t, WorkspaceWrite)

	rep := sb.Report()
	if rep.Effective != OSIsolated || !rep.Enforced {
		t.Fatalf("the probe confined but the report does not claim it: %+v", rep)
	}

	t.Run("the workspace is writable", func(t *testing.T) {
		target := filepath.Join(ws, "written")
		if err := runPrepared(t, sb, WorkspaceWrite, scratch, target); err != nil {
			t.Fatalf("a confined child could not write inside its own workspace: %v", err)
		}
		if _, err := os.Stat(target); err != nil {
			t.Fatalf("the granted write did not land: %v", err)
		}
	})

	t.Run("a sibling of the workspace is not", func(t *testing.T) {
		target := filepath.Join(outside, "leaked")
		if err := runPrepared(t, sb, WorkspaceWrite, scratch, target); err == nil {
			t.Fatalf("a confined child wrote outside its workspace: %s", target)
		}
		if _, err := os.Stat(target); err == nil {
			t.Fatalf("the out-of-workspace write actually landed at %s", target)
		}
	})

	t.Run("control: the same write succeeds unsandboxed", func(t *testing.T) {
		target := filepath.Join(outside, "control")
		cmd := exec.Command(comspec(), "/c", `(echo yanshi)>"`+target+`"`)
		if err := cmd.Run(); err != nil {
			t.Fatalf("the control write failed too, so the denial above is not "+
				"attributable to the token: %v", err)
		}
	})
}

// TestReadOnlyTierCannotWriteTheWorkspaceEither pins the difference between the
// two confined tiers.
//
// The grant is an ACE on disk, so a read-only child that held the SAME
// capability SID as a workspace-write one would inherit that grant and the two
// tiers would silently collapse into one. The read-only token restricts to a
// second SID granted nowhere, and this is the only thing that checks it.
//
// It runs on the SAME sandbox as the writable case, because the tier is a
// per-invocation property: a backend that read cfg.Tier instead of spec.Tier
// would pass a test that built a read-only sandbox and fail this one.
func TestReadOnlyTierCannotWriteTheWorkspaceEither(t *testing.T) {
	requireConfiningRestrictedToken(t)
	sb, ws, scratch, _ := sandboxUnderTest(t, WorkspaceWrite)

	target := filepath.Join(ws, "readonly-should-not-exist")
	if err := runPrepared(t, sb, ReadOnly, scratch, target); err == nil {
		t.Fatalf("a read-only invocation wrote into the workspace: %s", target)
	}
	if _, err := os.Stat(target); err == nil {
		t.Fatalf("the read-only write actually landed at %s", target)
	}
	// The same sandbox must still allow the workspace-write tier, or this test
	// would also pass against a backend that simply denies everything.
	allowed := filepath.Join(ws, "written")
	if err := runPrepared(t, sb, WorkspaceWrite, scratch, allowed); err != nil {
		t.Fatalf("the workspace-write tier stopped working: %v", err)
	}
}

// TestFullAccessInvocationGetsNoToken pins the widest tier.
//
// full-access means "no write restriction". Attaching a restricted token to it
// would restrict, and the failure would be invisible: the report says the
// operator asked for nothing, so nothing would look wrong until a command that
// legitimately writes outside the workspace started failing.
func TestFullAccessInvocationGetsNoToken(t *testing.T) {
	requireConfiningRestrictedToken(t)
	sb, _, _, outside := sandboxUnderTest(t, WorkspaceWrite)

	prog := comspec()
	cmd := exec.Command(prog, "/c", "rem")
	if err := sb.Prepare(context.Background(), cmd,
		CommandSpec{Path: prog, Tier: FullAccess}); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if cmd.SysProcAttr != nil && cmd.SysProcAttr.Token != 0 {
		t.Fatal("a full-access invocation was given a restricted token")
	}
	// ...and behaviourally: it writes where a confined tier cannot.
	target := filepath.Join(outside, "fullaccess")
	if err := runPrepared(t, sb, FullAccess, os.TempDir(), target); err != nil {
		t.Fatalf("a full-access invocation was confined: %v", err)
	}
}

// TestPrepareMergesRatherThanReplacesSysProcAttr keeps the token from silently
// dropping whatever the caller configured.
//
// The probe children in this package set CREATE_NO_WINDOW, and internal/shell
// sets fields here for its own reasons. A wholesale assignment in attachToken
// would drop them, and the symptom — a console window on every spawn, a lost
// cancellation — is one nobody traces back to a sandbox change.
func TestPrepareMergesRatherThanReplacesSysProcAttr(t *testing.T) {
	requireConfiningRestrictedToken(t)
	sb, _, _, _ := sandboxUnderTest(t, WorkspaceWrite)

	prog := comspec()
	cmd := exec.Command(prog, "/c", "rem")
	cmd.SysProcAttr = &windows.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
	if err := sb.Prepare(context.Background(), cmd,
		CommandSpec{Path: prog, Tier: WorkspaceWrite}); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if cmd.SysProcAttr.CreationFlags == 0 {
		t.Error("Prepare replaced SysProcAttr instead of merging into it; the caller's " +
			"CreationFlags were lost")
	}
	if cmd.SysProcAttr.Token == 0 {
		t.Error("Prepare did not attach a token to a confined invocation, so the child " +
			"would run unrestricted while Report claims os-isolated")
	}
}

// TestCloseRevokesTheCapabilityGrant pins the teardown half.
//
// The grant is an ACE written onto the operator's own directory. Leaving it
// there after the sandbox is gone would mean the next yanshi run — or a
// concurrent one — inherits a write grant nobody asked for, and the ACE would
// accumulate on every path the config ever named.
//
// It is asserted behaviourally rather than by reading the DACL back: a fresh
// token restricted to the same capability SID must NOT be able to write the
// workspace once Close has run. Reading the ACL would test that an ACE is
// absent; this tests that the access it conferred is gone, which is the
// property that matters and the one a mis-built revoke entry would not deliver.
func TestCloseRevokesTheCapabilityGrant(t *testing.T) {
	requireConfiningRestrictedToken(t)
	sb, ws, scratch, _ := sandboxUnderTest(t, WorkspaceWrite)

	// Establish that the grant was in force, or the assertion after Close is
	// about a directory that was never writable in the first place.
	before := filepath.Join(ws, "before-close")
	if err := runPrepared(t, sb, WorkspaceWrite, scratch, before); err != nil {
		t.Fatalf("the workspace was not writable even before Close: %v", err)
	}
	if err := sb.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	tok, err := createRestrictedToken(sandboxCapabilitySID)
	if err != nil {
		t.Fatalf("could not build a token to test the revoked grant with: %v", err)
	}
	defer tok.Close()
	after := filepath.Join(ws, "after-close")
	if err := runUnderToken(tok, scratch, after); err == nil {
		t.Fatalf("the capability grant survived Close: a child holding the sandbox "+
			"capability still wrote %s", after)
	}
}
