package tools

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/x6nux/yanshi/internal/sandbox"
)

// enforcingReport is a CapabilityReport that claims real OS isolation. Used by
// the classification tests, which are about the TEXT matching — the guard that
// a non-enforcing sandbox classifies nothing has its own tests below.
func enforcingReport(backend string, platform string, tier sandbox.AccessTier) sandbox.CapabilityReport {
	return sandbox.CapabilityReport{
		Platform:  platform,
		Requested: tier,
		Effective: sandbox.OSIsolated,
		Backend:   backend,
		Enforced:  true,
	}
}

func TestBackendKindFor(t *testing.T) {
	cases := []struct {
		name     string
		backend  string
		platform string
		want     SandboxBackendKind
	}{
		{"seatbelt by name", "seatbelt", "darwin", BackendSeatbelt},
		{"seatbelt by tool name", "sandbox-exec", "darwin", BackendSeatbelt},
		{"decorated backend string", "seatbelt(sandbox-exec v2)", "darwin", BackendSeatbelt},
		{"bubblewrap", "bubblewrap", "linux", BackendBubblewrap},
		{"bwrap short", "bwrap", "linux", BackendBubblewrap},
		{"landlock", "landlock", "linux", BackendLandlock},
		{"landlock+seccomp", "landlock+seccomp", "linux", BackendLandlock},
		{"appcontainer", "appcontainer", "windows", BackendAppContainer},
		{"job object", "JobObject+RestrictedToken", "windows", BackendAppContainer},
		{"platform fallback darwin", "none", "darwin", BackendSeatbelt},
		{"platform fallback linux", "none", "linux", BackendLandlock},
		{"platform fallback windows", "none", "windows", BackendAppContainer},
		{"unknown platform", "none", "plan9", BackendUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := BackendKindFor(sandbox.CapabilityReport{Backend: tc.backend, Platform: tc.platform})
			if got != tc.want {
				t.Fatalf("BackendKindFor(%q,%q) = %q, want %q", tc.backend, tc.platform, got, tc.want)
			}
		})
	}
}

// TestClassifySandboxViolationPatterns is the table lifted from
// reference/QwenPaw's four backends. Each positive case is a diagnostic shape
// one of those backends was observed to emit.
func TestClassifySandboxViolationPatterns(t *testing.T) {
	cases := []struct {
		name     string
		backend  string
		platform string
		exit     int
		stdout   string
		stderr   string
		want     bool
		resource string
	}{
		{
			name: "seatbelt deny(1)", backend: "seatbelt", platform: "darwin", exit: 1,
			stderr: "deny(1) file-read-data /Users/x/.ssh/id_rsa",
			want:   true, resource: "/Users/x/.ssh/id_rsa",
		},
		{
			name: "seatbelt kernel line", backend: "seatbelt", platform: "darwin", exit: 1,
			stderr: "Sandbox: cat(4242) deny(1) file-read-metadata /etc/shadow",
			want:   true, resource: "/etc/shadow",
		},
		{
			name: "sandbox-exec error prefix", backend: "seatbelt", platform: "darwin", exit: 64,
			stderr: "sandbox-exec: pattern serialization failed",
			want:   true,
		},
		{
			name: "operation not permitted", backend: "seatbelt", platform: "darwin", exit: 1,
			stderr: "touch: /etc/x: Operation not permitted",
			want:   true, resource: "/etc/x",
		},
		{
			name: "bwrap prefix", backend: "bubblewrap", platform: "linux", exit: 1,
			stderr: "bwrap: Can't bind mount /nix on /newroot/nix: No such file or directory",
			want:   true, resource: "/nix",
		},
		{
			name: "bwrap EACCES", backend: "bubblewrap", platform: "linux", exit: 1,
			stderr: "open failed: EACCES",
			want:   true,
		},
		{
			name: "landlock permission denied", backend: "landlock", platform: "linux", exit: 1,
			stderr: "cat: /etc/shadow: Permission denied",
			want:   true, resource: "/etc/shadow",
		},
		{
			name: "landlock named", backend: "landlock", platform: "linux", exit: 1,
			stderr: "landlock: ruleset restricts this access",
			want:   true,
		},
		{
			name: "appcontainer access denied", backend: "appcontainer", platform: "windows", exit: 1,
			stderr: "Access is denied.",
			want:   true,
		},
		{
			name: "appcontainer hresult", backend: "appcontainer", platform: "windows", exit: 5,
			stderr: "CreateFile failed: 0x80070005",
			want:   true,
		},
		{
			// Localised hosts are where an English-only matcher fails silently.
			name: "appcontainer chinese locale", backend: "appcontainer", platform: "windows", exit: 1,
			stderr: "拒绝访问。",
			want:   true,
		},
		{
			// cmd.exe routinely puts this on stdout, so the Windows backend
			// alone accepts a stdout-only match.
			name: "appcontainer stdout only", backend: "appcontainer", platform: "windows", exit: 1,
			stdout: "Access is denied.",
			want:   true,
		},
		{
			// Any other backend must NOT accept a stdout-only match: a grep
			// over a log, or a test asserting a permission error, prints these
			// words for entirely ordinary reasons.
			name: "unix stdout only is not a violation", backend: "landlock", platform: "linux", exit: 1,
			stdout: "cat: /etc/shadow: Permission denied",
			want:   false,
		},
		{
			name: "clean exit is never a violation", backend: "landlock", platform: "linux", exit: 0,
			stderr: "cat: /etc/shadow: Permission denied",
			want:   false,
		},
		{
			name: "ordinary compile failure", backend: "landlock", platform: "linux", exit: 2,
			stderr: "./main.go:7:2: undefined: foo",
			want:   false,
		},
		{
			name: "unknown backend gets the union", backend: "", platform: "plan9", exit: 1,
			stderr: "bwrap: something",
			want:   true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rep := enforcingReport(tc.backend, tc.platform, sandbox.ReadOnly)
			got, ok := ClassifySandboxViolation(rep, tc.exit, tc.stdout, tc.stderr)
			if ok != tc.want {
				t.Fatalf("violation = %v, want %v (stderr=%q stdout=%q)", ok, tc.want, tc.stderr, tc.stdout)
			}
			if !ok {
				return
			}
			if tc.resource != "" && got.Resource != tc.resource {
				t.Fatalf("Resource = %q, want %q", got.Resource, tc.resource)
			}
			if got.Evidence == "" {
				t.Fatal("a reported violation must carry evidence")
			}
		})
	}
}

// TestNonEnforcingSandboxClassifiesNothing pins the guard that keeps the
// stderr heuristic from firing on hosts where the sandbox provably denied
// nothing. This is the check that makes a chmod-000 file on a Phase-0 box not
// look like isolation.
func TestNonEnforcingSandboxClassifiesNothing(t *testing.T) {
	text := "cat: /etc/shadow: Permission denied"
	cases := []struct {
		name string
		rep  sandbox.CapabilityReport
	}{
		{"degraded", sandbox.CapabilityReport{
			Platform: "linux", Effective: sandbox.DegradedHostGuard, Backend: "landlock", Enforced: false}},
		{"disabled", sandbox.CapabilityReport{
			Platform: "linux", Effective: sandbox.Disabled, Backend: "none", Enforced: false}},
		{"claims os-isolated but not enforcing", sandbox.CapabilityReport{
			Platform: "linux", Effective: sandbox.OSIsolated, Backend: "landlock", Enforced: false}},
		{"enforced but degraded", sandbox.CapabilityReport{
			Platform: "linux", Effective: sandbox.DegradedHostGuard, Backend: "landlock", Enforced: true}},
		{"zero report", sandbox.CapabilityReport{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if SandboxEnforcing(tc.rep) {
				t.Fatal("this report must not count as enforcing")
			}
			if _, ok := ClassifySandboxViolation(tc.rep, 1, "", text); ok {
				t.Fatal("a non-enforcing sandbox cannot produce a sandbox violation")
			}
		})
	}
}

func TestNextSandboxTier(t *testing.T) {
	cases := []struct {
		from   sandbox.AccessTier
		want   sandbox.AccessTier
		canEsc bool
	}{
		{sandbox.ReadOnly, sandbox.WorkspaceWrite, true},
		{sandbox.WorkspaceWrite, sandbox.FullAccess, true},
		{sandbox.FullAccess, sandbox.FullAccess, false},
	}
	for _, tc := range cases {
		t.Run(tc.from.String(), func(t *testing.T) {
			got, ok := NextSandboxTier(tc.from)
			if ok != tc.canEsc || got != tc.want {
				t.Fatalf("NextSandboxTier(%v) = (%v,%v), want (%v,%v)", tc.from, got, ok, tc.want, tc.canEsc)
			}
		})
	}
}

// TestEscalationLadderTerminates: the ladder is what bounds the retry loop, so
// walking it from every rung must reach a stop.
func TestEscalationLadderTerminates(t *testing.T) {
	for _, start := range []sandbox.AccessTier{sandbox.ReadOnly, sandbox.WorkspaceWrite, sandbox.FullAccess} {
		tier := start
		steps := 0
		for {
			next, ok := NextSandboxTier(tier)
			if !ok {
				break
			}
			if next <= tier {
				t.Fatalf("from %v: NextSandboxTier returned %v which is not higher", tier, next)
			}
			tier = next
			steps++
			if steps > 8 {
				t.Fatalf("from %v: ladder did not terminate", start)
			}
		}
	}
}

// --- Real process-level evidence ---------------------------------------
//
// The detection above is a table of strings, and a table of strings proves
// nothing about whether real operating systems emit them. These two tests
// spawn an ACTUAL process that is ACTUALLY denied access to a file, and feed
// its ACTUAL stderr through the production classifier.
//
// The denial mechanism is deliberately not the yanshi sandbox (three other
// agents are rewriting those backends this round, and a test that depends on
// their in-flight state proves nothing today). It is chmod 000 / a
// non-existent path — a genuine kernel-level EACCES from a genuine child
// process, which is byte-for-byte the diagnostic a Landlock or Seatbelt denial
// produces for the same syscall. What is being proven is the half this file
// owns: given a real refusal from a real OS, the classifier recognises it.

// TestRealProcessDenialIsClassified spawns a real child that is really refused
// access to a real file, and requires the classifier to recognise its real
// stderr.
func TestRealProcessDenialIsClassified(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chmod 000 does not deny the owner on Windows; see the windows test below")
	}
	if os.Geteuid() == 0 {
		t.Skip("running as root: DAC permissions do not apply, so no denial can be produced")
	}
	dir := t.TempDir()
	secret := filepath.Join(dir, "secret.txt")
	if err := os.WriteFile(secret, []byte("classified"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(secret, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(secret, 0o600) })

	cmd := exec.Command("cat", secret)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	runErr := cmd.Run()
	exitCode := cmd.ProcessState.ExitCode()

	if runErr == nil || exitCode == 0 {
		t.Fatalf("the child was NOT denied (exit %d, stderr %q) — the test proves nothing; "+
			"this filesystem may not enforce mode bits", exitCode, stderr.String())
	}
	t.Logf("real child: exit=%d stderr=%q", exitCode, stderr.String())

	backend := "landlock"
	if runtime.GOOS == "darwin" {
		backend = "seatbelt"
	}
	rep := enforcingReport(backend, runtime.GOOS, sandbox.ReadOnly)
	v, ok := ClassifySandboxViolation(rep, exitCode, stdout.String(), stderr.String())
	if !ok {
		t.Fatalf("a real OS denial was not classified: stderr=%q", stderr.String())
	}
	if v.Resource != secret {
		t.Fatalf("Resource = %q, want the real refused path %q", v.Resource, secret)
	}
	t.Logf("classified: backend=%s resource=%s", v.Backend, v.Resource)
}

// TestRealProcessDenialIsClassifiedWindows is the Windows half. cmd.exe's
// `type` on a directory-as-file, and on a path under a protected system
// location, produce the "Access is denied" family the AppContainer table
// matches.
func TestRealProcessDenialIsClassifiedWindows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("windows-only")
	}
	// %SystemRoot%\System32\config\SAM is denied to every non-SYSTEM process,
	// including an elevated one, because the kernel holds it open exclusively.
	target := filepath.Join(os.Getenv("SystemRoot"), "System32", "config", "SAM")
	cmd := exec.Command("cmd", "/c", "type", target)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	_ = cmd.Run()
	exitCode := cmd.ProcessState.ExitCode()
	if exitCode == 0 {
		t.Skipf("reading %s was permitted; no denial to classify", target)
	}
	t.Logf("real child: exit=%d stdout=%q stderr=%q", exitCode, stdout.String(), stderr.String())

	rep := enforcingReport("appcontainer", "windows", sandbox.ReadOnly)
	if _, ok := ClassifySandboxViolation(rep, exitCode, stdout.String(), stderr.String()); !ok {
		t.Fatalf("a real Windows denial was not classified: stdout=%q stderr=%q",
			stdout.String(), stderr.String())
	}
}

// TestRealProcessOrdinaryFailureIsNotClassified is the false-positive half,
// and it is the one that would catch an over-broad regex. A real child that
// really fails for a reason that is NOT a permission problem must not be read
// as a sandbox refusal — otherwise every failing build would open an
// escalation dialog.
func TestRealProcessOrdinaryFailureIsNotClassified(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a POSIX shell utility")
	}
	dir := t.TempDir()
	missing := filepath.Join(dir, "no-such-file.txt")
	cmd := exec.Command("cat", missing)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	_ = cmd.Run()
	exitCode := cmd.ProcessState.ExitCode()
	if exitCode == 0 {
		t.Fatal("reading a nonexistent file unexpectedly succeeded")
	}
	t.Logf("real child: exit=%d stderr=%q", exitCode, stderr.String())

	backend := "landlock"
	if runtime.GOOS == "darwin" {
		backend = "seatbelt"
	}
	rep := enforcingReport(backend, runtime.GOOS, sandbox.ReadOnly)
	if v, ok := ClassifySandboxViolation(rep, exitCode, stdout.String(), stderr.String()); ok {
		t.Fatalf("ENOENT was misclassified as a sandbox violation: %+v", v)
	}
}

// TestRealProcessDenialUnderNonEnforcingSandboxIsNotClassified combines the
// two halves: the SAME real denial that the enforcing case classifies must be
// invisible when the sandbox reports it is not enforcing. Without this, every
// Phase-0 host would escalate on every chmod-000 file the agent touches.
func TestRealProcessDenialUnderNonEnforcingSandboxIsNotClassified(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chmod 000 does not deny the owner on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("running as root: DAC permissions do not apply")
	}
	dir := t.TempDir()
	secret := filepath.Join(dir, "secret.txt")
	if err := os.WriteFile(secret, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(secret, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(secret, 0o600) })

	cmd := exec.Command("cat", secret)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	_ = cmd.Run()
	exitCode := cmd.ProcessState.ExitCode()
	if exitCode == 0 {
		t.Skip("filesystem does not enforce mode bits")
	}
	degraded := sandbox.CapabilityReport{
		Platform: runtime.GOOS, Effective: sandbox.DegradedHostGuard,
		Backend: "none", Reason: "no OS backend", Enforced: false,
	}
	if _, ok := ClassifySandboxViolation(degraded, exitCode, "", stderr.String()); ok {
		t.Fatalf("a degraded sandbox must not claim credit for a DAC denial: %q", stderr.String())
	}
}

// TestExtractViolationResourceShapes covers the resource extractor on its own,
// including the shapes where it must honestly return nothing.
func TestExtractViolationResourceShapes(t *testing.T) {
	cases := []struct {
		name string
		text string
		want string
	}{
		{"seatbelt", "deny(1) file-write-data /Users/x/.ssh/id_rsa", "/Users/x/.ssh/id_rsa"},
		{"posix", "cat: /etc/shadow: Permission denied", "/etc/shadow"},
		{"posix operation not permitted", "touch: /sys/x: Operation not permitted", "/sys/x"},
		{"bwrap", "bwrap: Can't bind mount /nix on /newroot/nix", "/nix"},
		{"windows", `C:\Windows\System32\config\SAM Access is denied`, `C:\Windows\System32\config\SAM`},
		{"unnamed", "Access is denied.", ""},
		{"empty", "", ""},
		{"eacces alone", "EACCES", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := extractViolationResource(tc.text); got != tc.want {
				t.Fatalf("extractViolationResource(%q) = %q, want %q", tc.text, got, tc.want)
			}
		})
	}
}

// TestViolationEvidenceIsBounded: a sandboxed build that fails on every file
// can produce megabytes. The prompt needs a sample.
func TestViolationEvidenceIsBounded(t *testing.T) {
	huge := strings.Repeat("cat: /etc/shadow: Permission denied\n", 10000)
	rep := enforcingReport("landlock", "linux", sandbox.ReadOnly)
	v, ok := ClassifySandboxViolation(rep, 1, "", huge)
	if !ok {
		t.Fatal("expected a violation")
	}
	if len(v.Evidence) > maxViolationEvidence {
		t.Fatalf("evidence is %d bytes, cap is %d", len(v.Evidence), maxViolationEvidence)
	}
}

var _ = context.Background
