//go:build linux

package sandbox

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// This file holds the Linux-only tests: argv rewriting by both backends, the
// degradation chain, and real process-level enforcement.
//
// The process-level tests skip when the mechanism under test is not actually
// available on the host, and the skip is decided by the SAME probe production
// uses -- not by a reimplemented check. A test that decided availability its
// own way could skip on a host where the backend works (hiding a regression)
// or run on one where it does not (a flake attributed to the sandbox).

// requireEnforcingBwrap skips unless bubblewrap really enforces here.
func requireEnforcingBwrap(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath(bwrapProgram)
	if err != nil {
		t.Skipf("bwrap not on PATH: %v", err)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		t.Skipf("bwrap path unusable: %v", err)
	}
	if reason, ok := probeBwrapAt(abs); !ok {
		t.Skipf("bwrap does not enforce on this host: %s", reason)
	}
	return abs
}

// TestBwrapPrepareRewritesArgv pins the argv rewrite: the launcher leads, the
// mount plan follows, `--` terminates bwrap's options, and the original
// program and arguments survive verbatim after it.
func TestBwrapPrepareRewritesArgv(t *testing.T) {
	b := &bubblewrap{
		cfg:       Config{Tier: ReadOnly},
		bwrapPath: "/usr/bin/bwrap",
		report:    CapabilityReport{Enforced: true},
	}
	cmd := exec.Command("/bin/echo", "hello", "world")
	if err := b.Prepare(context.Background(), cmd, CommandSpec{Tier: ReadOnly}); err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	if cmd.Path != "/usr/bin/bwrap" {
		t.Errorf("cmd.Path = %q, want the launcher", cmd.Path)
	}
	if cmd.Args[0] != "/usr/bin/bwrap" {
		t.Errorf("argv[0] = %q, want the launcher", cmd.Args[0])
	}
	sep := argIndex(cmd.Args, "--")
	if sep < 0 {
		t.Fatalf("no -- separator in %v", cmd.Args)
	}
	tail := cmd.Args[sep+1:]
	want := []string{"/bin/echo", "hello", "world"}
	if strings.Join(tail, " ") != strings.Join(want, " ") {
		t.Errorf("target argv = %v, want %v", tail, want)
	}
	if argIndex(cmd.Args[:sep], "--ro-bind") < 0 {
		t.Errorf("mount plan missing before --: %v", cmd.Args)
	}
}

// TestBwrapPrepareUsesCmdPathNotArgv0 pins that a cosmetic argv[0] cannot
// redirect which binary runs. A caller that sets Args[0] to a friendly name is
// normal; letting that string select the executable would be a way to have the
// sandbox launch something other than what was authorized.
func TestBwrapPrepareUsesCmdPathNotArgv0(t *testing.T) {
	b := &bubblewrap{
		cfg:       Config{Tier: ReadOnly},
		bwrapPath: "/usr/bin/bwrap",
		report:    CapabilityReport{Enforced: true},
	}
	cmd := exec.Command("/bin/echo", "x")
	cmd.Args[0] = "/bin/evil"

	if err := b.Prepare(context.Background(), cmd, CommandSpec{Tier: ReadOnly}); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	sep := argIndex(cmd.Args, "--")
	if cmd.Args[sep+1] != "/bin/echo" {
		t.Errorf("program = %q, want /bin/echo (cmd.Path), not the argv[0] lie",
			cmd.Args[sep+1])
	}
}

// TestBwrapPrepareFoldsDirIntoChdir pins that cmd.Dir is moved into --chdir
// and cleared. Leaving it set makes the Go runtime chdir the bwrap process in
// the HOST namespace before the sandbox exists; when the sandbox then hides
// that path the launch fails with an error naming the directory, with no
// visible connection to the sandbox's own /tmp policy.
func TestBwrapPrepareFoldsDirIntoChdir(t *testing.T) {
	dir := t.TempDir()
	b := &bubblewrap{
		cfg:       Config{Tier: WorkspaceWrite, WorkspaceRoot: dir},
		bwrapPath: "/usr/bin/bwrap",
		workspace: ResolvePath(dir),
		report:    CapabilityReport{Enforced: true},
	}
	cmd := exec.Command("/bin/true")
	cmd.Dir = dir

	if err := b.Prepare(context.Background(), cmd, CommandSpec{Tier: WorkspaceWrite}); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if cmd.Dir != "" {
		t.Errorf("cmd.Dir must be cleared after folding into --chdir, got %q", cmd.Dir)
	}
	// t.TempDir may sit under /tmp, which the private tmpfs hides; in that case
	// --chdir is correctly omitted. Either outcome is valid, but Dir must be
	// cleared regardless -- that is the property under test.
	if idx := argIndex(cmd.Args, "--chdir"); idx >= 0 {
		if got := cmd.Args[idx+1]; got != ResolvePath(dir) {
			t.Errorf("--chdir = %q, want %q", got, ResolvePath(dir))
		}
	}
}

// TestBwrapPrepareRefusesSelfWrap pins that double-wrapping is refused rather
// than silently produced: the inner bwrap needs privileges the outer one just
// removed, so it fails in a way attributable to the wrong layer.
func TestBwrapPrepareRefusesSelfWrap(t *testing.T) {
	b := &bubblewrap{
		cfg:       Config{Tier: ReadOnly},
		bwrapPath: "/usr/bin/bwrap",
		report:    CapabilityReport{Enforced: true},
	}
	if err := b.Prepare(context.Background(), exec.Command("/usr/bin/bwrap", "--version"),
		CommandSpec{Tier: ReadOnly}); err == nil {
		t.Error("wrapping bwrap in itself must be refused")
	}
	// Also by basename, so a differently-located bwrap is caught.
	if err := b.Prepare(context.Background(), exec.Command("/usr/local/bin/bwrap"),
		CommandSpec{Tier: ReadOnly}); err == nil {
		t.Error("wrapping a differently-located bwrap must also be refused")
	}
}

// TestBwrapPrepareUsesSpecTierNotConfigTier is the tier-plumbing pin. Reading
// cfg.Tier here would make a tool's declared ReadOnly and another's declared
// WorkspaceWrite indistinguishable -- every tool would silently run at
// whatever the operator configured globally, and secproc's UseSandboxTier
// would be decorative.
func TestBwrapPrepareUsesSpecTierNotConfigTier(t *testing.T) {
	dir := t.TempDir()
	b := &bubblewrap{
		cfg:       Config{Tier: FullAccess, WorkspaceRoot: dir},
		bwrapPath: "/usr/bin/bwrap",
		workspace: ResolvePath(dir),
		report:    CapabilityReport{Enforced: true},
	}
	cmd := exec.Command("/bin/true")
	if err := b.Prepare(context.Background(), cmd, CommandSpec{Tier: ReadOnly}); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	// spec.Tier=ReadOnly must produce the read-only plan even though
	// cfg.Tier=FullAccess would have produced --bind / /.
	if argIndex(cmd.Args, "--ro-bind") < 0 {
		t.Errorf("spec.Tier=ReadOnly must render the read-only plan, got %v", cmd.Args)
	}
	if hasPair(cmd.Args, "--bind", "/", "/") {
		t.Errorf("cfg.Tier=FullAccess must not leak into a ReadOnly spec, got %v", cmd.Args)
	}
}

// TestBwrapDegradedPrepareIsANoOp pins that a degraded backend leaves the
// command untouched rather than failing every spawn. This is only safe because
// Report() already told the truth, and the assertion pairs with it: a degraded
// report must never say Enforced.
func TestBwrapDegradedPrepareIsANoOp(t *testing.T) {
	b := &bubblewrap{
		cfg: Config{Tier: ReadOnly},
		report: CapabilityReport{
			Effective: DegradedHostGuard,
			Enforced:  false,
			Reason:    "test",
		},
	}
	cmd := exec.Command("/bin/echo", "hi")
	before := strings.Join(cmd.Args, " ")
	if err := b.Prepare(context.Background(), cmd, CommandSpec{Tier: ReadOnly}); err != nil {
		t.Fatalf("degraded Prepare must not error: %v", err)
	}
	if strings.Join(cmd.Args, " ") != before || cmd.Path != "/bin/echo" {
		t.Errorf("degraded Prepare must not rewrite the command, got %v", cmd.Args)
	}
	if b.Report().Enforced {
		t.Error("a degraded report must never claim Enforced")
	}
}

// TestLandlockPrepareRewritesArgv pins the helper argv the parent builds, and
// checks it against the SAME parser the child uses. Building the argv here and
// parsing it with a test-local copy would let the two sides drift; a
// divergence would surface only as a probe failure with no indication of which
// half was wrong.
func TestLandlockPrepareRewritesArgv(t *testing.T) {
	l := &landlockBackend{
		cfg:     Config{Tier: ReadOnly},
		selfExe: "/usr/bin/yanshi",
		abi:     3,
		report:  CapabilityReport{Enforced: true},
	}
	cmd := exec.Command("/bin/echo", "hello", "world")
	if err := l.Prepare(context.Background(), cmd, CommandSpec{Tier: ReadOnly}); err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	if cmd.Path != "/usr/bin/yanshi" {
		t.Errorf("cmd.Path = %q, want the re-exec of self", cmd.Path)
	}
	token, program, args, err := SplitLandlockHelperArgs(cmd.Args)
	if err != nil {
		t.Fatalf("the argv Prepare built must parse with the helper's own parser: %v", err)
	}
	if program != "/bin/echo" {
		t.Errorf("program = %q", program)
	}
	if strings.Join(args, " ") != "/bin/echo hello world" {
		t.Errorf("target argv = %v", args)
	}
	rules, err := DecodeLandlockRules(token)
	if err != nil {
		t.Fatalf("the token Prepare built must decode: %v", err)
	}
	if len(rules.WritePaths) != 0 {
		t.Errorf("a ReadOnly spec must carry no write grants, got %v", rules.WritePaths)
	}
}

// TestLandlockPrepareUsesSpecTier is the tier-plumbing pin for the fallback
// backend, mirroring the bubblewrap one.
func TestLandlockPrepareUsesSpecTier(t *testing.T) {
	dir := t.TempDir()
	l := &landlockBackend{
		cfg:       Config{Tier: ReadOnly, WorkspaceRoot: dir},
		selfExe:   "/usr/bin/yanshi",
		workspace: ResolvePath(dir),
		report:    CapabilityReport{Enforced: true},
	}
	cmd := exec.Command("/bin/true")
	if err := l.Prepare(context.Background(), cmd, CommandSpec{Tier: WorkspaceWrite}); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	token, _, _, err := SplitLandlockHelperArgs(cmd.Args)
	if err != nil {
		t.Fatalf("split: %v", err)
	}
	rules, err := DecodeLandlockRules(token)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !contains(rules.WritePaths, ResolvePath(dir)) {
		t.Errorf("spec.Tier=WorkspaceWrite must grant the workspace even though "+
			"cfg.Tier=ReadOnly, got %v", rules.WritePaths)
	}
}

// TestLandlockReportIsHonestAboutItsLimits is the anti-over-claim pin, and it
// is the one that protects a downstream decision rather than a local property.
//
// internal/tools' escalation path consults this report to decide whether an
// observed failure was a sandbox denial. Landlock controls the filesystem and
// nothing else -- no network, no pids, no resource limits. If the Reason
// claimed otherwise, a plain connection refusal (a down server, a DNS failure)
// would be classified as a sandbox denial and raise an escalation prompt
// asking the operator to grant privilege for something privilege cannot fix.
func TestLandlockReportIsHonestAboutItsLimits(t *testing.T) {
	l := &landlockBackend{cfg: Config{Tier: WorkspaceWrite, NetworkDeny: true}}
	l.report = CapabilityReport{
		Platform:  "linux",
		Effective: OSIsolated,
		Backend:   "landlock",
		Reason: "landlock ABI 3 enforcing FILESYSTEM access only (tier=workspace-write); " +
			"NOT enforced by this backend: network egress (Config.NetworkDeny=true is " +
			"ignored here), process/pid isolation, and resource limits — bubblewrap was unavailable",
		Enforced: true,
	}
	reason := strings.ToLower(l.Report().Reason)
	for _, must := range []string{"filesystem", "network", "not enforced"} {
		if !strings.Contains(reason, must) {
			t.Errorf("the landlock Reason must mention %q so callers do not "+
				"misclassify non-filesystem failures as sandbox denials: %s", must, reason)
		}
	}
	if l.Report().CanKillTree {
		t.Error("landlock has no pid namespace and must not claim kill-tree")
	}
}

// TestLandlockConstructedReportMatchesTheHonestyContract drives the REAL
// constructor rather than a hand-built report, so the assertion above cannot
// pass against a struct literal while production builds a different string.
// Skips when Landlock is unavailable, which is the only honest thing to do on
// a host that cannot construct the backend.
func TestLandlockConstructedReportMatchesTheHonestyContract(t *testing.T) {
	if _, err := landlockABI(); err != nil {
		t.Skipf("landlock unavailable: %v", err)
	}
	sb, reason := newLandlock(Config{
		Tier: WorkspaceWrite, WorkspaceRoot: t.TempDir(), NetworkDeny: true,
	})
	if sb == nil {
		t.Skipf("landlock did not construct: %s", reason)
	}
	rep := sb.Report()
	if rep.Effective != OSIsolated || rep.Backend != "landlock" {
		t.Fatalf("unexpected report: %+v", rep)
	}
	low := strings.ToLower(rep.Reason)
	for _, must := range []string{"filesystem", "network", "not enforced"} {
		if !strings.Contains(low, must) {
			t.Errorf("constructed Reason must mention %q, got: %s", must, rep.Reason)
		}
	}
	if rep.CanKillTree {
		t.Error("landlock must not claim kill-tree")
	}
}

// TestNewPlatformSandboxDegradesHonestly pins the end of the chain: when
// neither backend can enforce, the report must say DegradedHostGuard and must
// name BOTH failures. A generic "no sandbox available" would send an operator
// installing a package that is already installed, when the real cause is
// disabled user namespaces.
func TestNewPlatformSandboxDegradesHonestly(t *testing.T) {
	sb := newPlatformSandbox(Config{Enabled: true, Tier: WorkspaceWrite, WorkspaceRoot: t.TempDir()})
	rep := sb.Report()

	switch rep.Effective {
	case OSIsolated:
		// A backend enforces here. Then it must be one of the two real ones
		// and must have proven itself -- never a claim with no mechanism.
		if rep.Backend != "bubblewrap" && rep.Backend != "landlock" {
			t.Errorf("OSIsolated must name a real backend, got %q", rep.Backend)
		}
		if !rep.Enforced {
			t.Error("OSIsolated must set Enforced")
		}
	case DegradedHostGuard:
		if rep.Enforced {
			t.Error("a degraded report must not claim Enforced")
		}
		low := strings.ToLower(rep.Reason)
		if !strings.Contains(low, "bubblewrap") || !strings.Contains(low, "landlock") {
			t.Errorf("the degraded Reason must name both failed backends, got: %s", rep.Reason)
		}
	default:
		t.Errorf("unexpected Effective %q", rep.Effective)
	}
}

// TestBwrapReallyDeniesAWrite is the process-level enforcement test: a real
// bwrap, a real read-only plan, a real write that must fail. Everything else
// in this package tests argument STRINGS; this is the only place that confirms
// the strings mean what the comments claim.
func TestBwrapReallyDeniesAWrite(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("bubblewrap is linux-only")
	}
	bwrapPath := requireEnforcingBwrap(t)

	target := filepath.Join(t.TempDir(), "should-not-exist")
	args := BuildBwrapArgs(BwrapInput{Tier: ReadOnly, NetworkDeny: true})
	full := append(args, "--", "/bin/sh", "-c", "exec 3>"+target)

	cmd := exec.Command(bwrapPath, full...)
	cmd.Env = []string{}
	if err := cmd.Run(); err == nil {
		t.Fatalf("a ReadOnly plan allowed a write to %s", target)
	}
	if _, err := os.Stat(target); err == nil {
		t.Fatalf("the write actually landed at %s", target)
	}
}

// TestBwrapReallyAllowsAWorkspaceWrite is the necessary counterpart: a sandbox
// that denies everything would pass the denial test above while being useless.
// This confirms the workspace carve-out genuinely works end to end.
func TestBwrapReallyAllowsAWorkspaceWrite(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("bubblewrap is linux-only")
	}
	bwrapPath := requireEnforcingBwrap(t)

	ws := t.TempDir()
	target := filepath.Join(ws, "written")
	args := BuildBwrapArgs(BwrapInput{
		Tier:          WorkspaceWrite,
		WorkspaceRoot: ws,
		NetworkDeny:   true,
	})
	full := append(args, "--", "/bin/sh", "-c", "echo ok > "+target)

	cmd := exec.Command(bwrapPath, full...)
	cmd.Env = []string{}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("workspace write failed: %v\n%s", err, out)
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("workspace file not created: %v", err)
	}
	if strings.TrimSpace(string(data)) != "ok" {
		t.Errorf("workspace file contents = %q", data)
	}
}

// TestBwrapReallyDeniesOutsideWorkspace pins the boundary of the workspace
// tier: granting the workspace must not grant its siblings. Without this, a
// bind that accidentally targeted the PARENT directory would pass both tests
// above while confining nothing useful.
func TestBwrapReallyDeniesOutsideWorkspace(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("bubblewrap is linux-only")
	}
	bwrapPath := requireEnforcingBwrap(t)

	base := t.TempDir()
	ws := filepath.Join(base, "ws")
	if err := mkdirAllForTest(ws); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(base, "outside")
	if err := mkdirAllForTest(outside); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(outside, "leak")

	args := BuildBwrapArgs(BwrapInput{
		Tier: WorkspaceWrite, WorkspaceRoot: ws, NetworkDeny: true,
	})
	full := append(args, "--", "/bin/sh", "-c", "echo leaked > "+target)

	cmd := exec.Command(bwrapPath, full...)
	cmd.Env = []string{}
	if err := cmd.Run(); err == nil {
		t.Fatalf("a sibling of the workspace was writable: %s", target)
	}
	if _, err := os.Stat(target); err == nil {
		t.Fatalf("the sibling write landed at %s", target)
	}
}

// TestBwrapReallyDeniesNetwork confirms NetworkDeny is not merely an argument
// string. It uses a loopback connect, which needs no external host and no DNS,
// so a failure here is the namespace and not the environment.
func TestBwrapReallyDeniesNetwork(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("bubblewrap is linux-only")
	}
	bwrapPath := requireEnforcingBwrap(t)

	ln, err := listenLoopbackForTest()
	if err != nil {
		t.Skipf("cannot listen on loopback: %v", err)
	}
	defer ln.Close()
	addr := ln.Addr().String()

	// With the net namespace unshared, the host's loopback listener is
	// unreachable: the sandbox has its own empty network stack.
	args := BuildBwrapArgs(BwrapInput{Tier: ReadOnly, NetworkDeny: true})
	full := append(args, "--", "/bin/sh", "-c",
		"exec 3<>/dev/tcp/"+strings.Replace(addr, ":", "/", 1))
	cmd := exec.Command(bwrapPath, full...)
	cmd.Env = []string{}
	if err := cmd.Run(); err == nil {
		t.Errorf("NetworkDeny did not prevent a loopback connect to %s", addr)
	}
}

// TestLandlockHelperReallyConfines is the process-level test for the fallback
// backend. It re-execs the ACTUAL test binary through the helper entry point,
// so the argv grammar, the decoder, applyLandlock and the final execve are all
// exercised as one path -- which is what makes it a test of the backend rather
// than of applyLandlock in isolation.
func TestLandlockHelperReallyConfines(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("landlock is linux-only")
	}
	if _, err := landlockABI(); err != nil {
		t.Skipf("landlock unavailable: %v", err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Skipf("cannot resolve test binary: %v", err)
	}
	if reason, ok := probeLandlockAt(exe); !ok {
		t.Skipf("landlock does not enforce on this host: %s", reason)
	}

	rules := BuildLandlockRules(BwrapInput{Tier: ReadOnly}, func(p string) bool {
		_, err := os.Stat(p)
		return err == nil
	})
	token, err := EncodeLandlockRules(rules)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	target := filepath.Join(t.TempDir(), "landlock-should-not-exist")
	cmd := exec.Command(exe, landlockHelperArg, token, "--",
		"/bin/sh", "-c", "exec 3>"+target)
	cmd.Env = []string{}
	if err := cmd.Run(); err == nil {
		t.Fatalf("the read-only landlock policy allowed a write to %s", target)
	}
	if _, err := os.Stat(target); err == nil {
		t.Fatalf("the write actually landed at %s", target)
	}
}

// TestLandlockHelperAllowsGrantedWrite is the counterpart confirming the
// policy is not simply deny-everything.
func TestLandlockHelperAllowsGrantedWrite(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("landlock is linux-only")
	}
	if _, err := landlockABI(); err != nil {
		t.Skipf("landlock unavailable: %v", err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Skipf("cannot resolve test binary: %v", err)
	}
	if reason, ok := probeLandlockAt(exe); !ok {
		t.Skipf("landlock does not enforce on this host: %s", reason)
	}

	ws := t.TempDir()
	rules := BuildLandlockRules(BwrapInput{
		Tier: WorkspaceWrite, WorkspaceRoot: ws,
	}, func(p string) bool {
		_, err := os.Stat(p)
		return err == nil
	})
	token, err := EncodeLandlockRules(rules)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	target := filepath.Join(ws, "written")
	cmd := exec.Command(exe, landlockHelperArg, token, "--",
		"/bin/sh", "-c", "echo ok > "+target)
	cmd.Env = []string{}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("granted workspace write failed: %v\n%s", err, out)
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("granted write did not land: %v", err)
	}
}

// TestLandlockHelperRefusesUnappliablePolicyRatherThanExecUnconfined closes
// the half of the fail-closed contract the malformed-token test above does NOT
// reach, and it exists because a mutation probe proved that gap was real.
//
// The test above feeds tokens that fail during DECODE, so the helper returns
// before applyLandlock is ever called. Deleting applyLandlock's error check --
// replacing `if err := applyLandlock(rules); err != nil { return ... }` with
// `_ = applyLandlock(rules)`, which makes the helper exec the target with NO
// confinement whenever the policy cannot be installed -- left that test fully
// green. Measured: the mutated binary passed `-test.run TestLandlock` in a
// container.
//
// This test uses a WELL-FORMED token naming a path that does not exist, so
// decoding succeeds and the failure necessarily happens inside applyLandlock.
// It is host-independent by construction, which is what makes it a usable
// guard rather than one that skips on the machines that matter:
//
//   - where Landlock is unavailable, landlockABI() fails;
//   - where it is available, the O_PATH open of the missing path fails.
//
// Either way applyLandlock returns an error, and the assertion is not that the
// helper errors but that the target program DID NOT RUN.
func TestLandlockHelperRefusesUnappliablePolicyRatherThanExecUnconfined(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("landlock is linux-only")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Skipf("cannot resolve test binary: %v", err)
	}

	// Well-formed, decodable, and impossible to install: the path is absolute
	// and clean (so validation passes) but absent (so add_rule cannot succeed).
	missing := filepath.Join(t.TempDir(), "definitely-absent-directory")
	token, err := EncodeLandlockRules(LandlockRules{WritePaths: []string{missing}})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if _, err := DecodeLandlockRules(token); err != nil {
		t.Fatalf("precondition: the token must decode cleanly so the failure "+
			"lands in applyLandlock, not in the decoder: %v", err)
	}

	sentinel := filepath.Join(t.TempDir(), "ran-unconfined")
	cmd := exec.Command(exe, landlockHelperArg, token, "--",
		"/bin/sh", "-c", "touch "+sentinel)
	cmd.Env = []string{}
	runErr := cmd.Run()

	if _, err := os.Stat(sentinel); err == nil {
		t.Fatal("the target RAN UNCONFINED after the policy could not be applied — " +
			"the helper must never fall through to exec when applyLandlock fails")
	}
	if runErr == nil {
		t.Error("the helper must exit nonzero when it cannot apply the policy")
	}
}

// TestLandlockHelperRefusesMalformedRatherThanExecUnconfined is the
// fail-closed pin, and it is the most important test for this backend.
//
// A helper that fell through to exec on a bad token would run the target
// program with NO confinement while the parent's capability report says
// os-isolated. The assertion is therefore not merely "it errors" but "the
// target program did not run": the sentinel file must not exist.
func TestLandlockHelperRefusesMalformedRatherThanExecUnconfined(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("landlock is linux-only")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Skipf("cannot resolve test binary: %v", err)
	}

	for name, token := range map[string]string{
		"not base64": "!!!not-base64!!!",
		"not json":   "bm90IGpzb24",
		"relative":   "eyJ3IjpbInJlbGF0aXZlIl19",
	} {
		sentinel := filepath.Join(t.TempDir(), "ran-unconfined")
		cmd := exec.Command(exe, landlockHelperArg, token, "--",
			"/bin/sh", "-c", "touch "+sentinel)
		cmd.Env = []string{}
		if err := cmd.Run(); err == nil {
			t.Errorf("%s: helper must fail rather than exec", name)
		}
		if _, err := os.Stat(sentinel); err == nil {
			t.Errorf("%s: the target RAN UNCONFINED — the helper must never fall "+
				"through to exec on a policy it could not apply", name)
		}
	}
}
