package tools

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/x6nux/yanshi/internal/guard"
	"github.com/x6nux/yanshi/internal/sandbox"
	"github.com/x6nux/yanshi/internal/secproc"
)

// realCaptureFactory is a secproc.Factory that spawns an ACTUAL process.
//
// The escalation loop is wired into runSecureCapture, and every existing test
// of that function substitutes secureCommandRunner — which replaces the very
// function under test. So none of them can show that the wiring is live. These
// tests go the other way: they leave runSecureCapture alone and inject a real
// factory beneath it, so a real child really runs and its real stderr really
// reaches the classifier.
//
// tierSeen records the UseSandboxTier of every spawn, which is how the retry's
// tier increase is observed from outside.
type realCaptureFactory struct {
	tierSeen []sandbox.AccessTier
}

func (f *realCaptureFactory) Start(ctx context.Context, spec secproc.SecureProcessSpec) (*secproc.StartedProcess, error) {
	f.tierSeen = append(f.tierSeen, spec.UseSandboxTier)
	cmd := exec.CommandContext(ctx, spec.Program, spec.Args...)
	cmd.Dir = spec.Dir
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &secproc.StartedProcess{
		Wait: cmd.Wait, PID: cmd.Process.Pid, Stdout: stdout, Stderr: stderr,
	}, nil
}

// deniedFileCtx creates a file the current user genuinely cannot read and
// returns its path plus a context wired with a real factory, an enforcing
// sandbox report, a permissive profile and the given callback.
func deniedFileCtx(t *testing.T, ask func(PermissionRequest) PermissionDecision) (string, context.Context, *realCaptureFactory) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("chmod 000 does not deny the owner on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("running as root: DAC permissions do not apply")
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

	factory := &realCaptureFactory{}
	ctx := WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"*"}},
		// "denylist" with no patterns is how this guard spells an
		// unrestricted shell; there is deliberately no "allow" value.
		Shell: guard.ShellPerm{Policy: "denylist"},
	})
	backend := "landlock"
	if runtime.GOOS == "darwin" {
		backend = "seatbelt"
	}
	ctx = WithSandbox(ctx, newFakeSandbox(sandbox.CapabilityReport{
		Platform: runtime.GOOS, Backend: backend,
		Effective: sandbox.OSIsolated, Enforced: true, Requested: sandbox.ReadOnly,
	}))
	ctx = WithSecureProcessFactory(ctx, factory)
	if ask != nil {
		ctx = WithPermissionCallback(ctx, ask)
	}
	return secret, ctx, factory
}

// TestRunSecureCaptureEscalatesARealDenial is the end-to-end proof that S12 is
// WIRED, not merely implemented. A real `cat` on a real unreadable file, run
// through the real runSecureCapture, must produce a real escalation prompt and
// a real second spawn at a higher tier.
func TestRunSecureCaptureEscalatesARealDenial(t *testing.T) {
	var prompts []string
	secret, ctx, factory := deniedFileCtx(t, func(req PermissionRequest) PermissionDecision {
		prompts = append(prompts, req.Reason)
		return PermissionAllow
	})

	res, err := runSecureCapture(ctx, secproc.SecureProcessSpec{
		Tool: "shell_run", Program: "cat", Args: []string{secret},
		UseSandboxTier: sandbox.ReadOnly,
	}, 20*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("real result: exit=%d stderr=%q", res.ExitCode, res.Stderr)
	t.Logf("tiers actually spawned: %v", factory.tierSeen)

	if len(prompts) != 1 {
		t.Fatalf("a real sandbox-shaped denial produced %d escalation prompts, want 1", len(prompts))
	}
	if len(factory.tierSeen) != 2 {
		t.Fatalf("spawned %d processes, want 2 (original + one retry)", len(factory.tierSeen))
	}
	if factory.tierSeen[0] != sandbox.ReadOnly || factory.tierSeen[1] != sandbox.WorkspaceWrite {
		t.Fatalf("tiers = %v, want [read-only workspace-write]", factory.tierSeen)
	}
	if !strings.Contains(prompts[0], secret) {
		t.Fatalf("the prompt did not name the refused file:\n%s", prompts[0])
	}
	t.Logf("escalation prompt:\n%s", prompts[0])
}

// TestRunSecureCaptureDeniedEscalationExplainsToTheModel: when the operator
// says no, the model must receive the explanation, not a bare exit code. The
// explanation is appended to Stderr, which is what commandFailureTail renders.
func TestRunSecureCaptureDeniedEscalationExplainsToTheModel(t *testing.T) {
	secret, ctx, factory := deniedFileCtx(t, func(PermissionRequest) PermissionDecision {
		return PermissionDeny
	})

	res, err := runSecureCapture(ctx, secproc.SecureProcessSpec{
		Tool: "shell_run", Program: "cat", Args: []string{secret},
		UseSandboxTier: sandbox.ReadOnly,
	}, 20*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if len(factory.tierSeen) != 1 {
		t.Fatalf("spawned %d processes after a denial, want 1", len(factory.tierSeen))
	}
	if !strings.Contains(res.Stderr, "sandbox denied access") {
		t.Fatalf("the model was not told why:\n%s", res.Stderr)
	}
	// The explanation must reach the text the tools actually show the model.
	tail := commandFailureTail(res)
	if !strings.Contains(tail, "sandbox denied access") {
		t.Fatalf("commandFailureTail dropped the explanation:\n%s", tail)
	}
	t.Logf("model sees:\n%s", tail)
}

// TestRunSecureCaptureOrdinaryFailureIsUntouched: the common case must be
// byte-identical to its pre-S12 behaviour. A failing build that gained an
// escalation prompt would be a regression for every user.
func TestRunSecureCaptureOrdinaryFailureIsUntouched(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a POSIX shell utility")
	}
	prompts := 0
	dir := t.TempDir()
	factory := &realCaptureFactory{}
	ctx := WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"*"}},
	})
	ctx = WithSandbox(ctx, newFakeSandbox(sandbox.CapabilityReport{
		Platform: runtime.GOOS, Backend: "landlock",
		Effective: sandbox.OSIsolated, Enforced: true,
	}))
	ctx = WithSecureProcessFactory(ctx, factory)
	ctx = WithPermissionCallback(ctx, func(PermissionRequest) PermissionDecision {
		prompts++
		return PermissionAllow
	})

	res, err := runSecureCapture(ctx, secproc.SecureProcessSpec{
		Tool: "shell_run", Program: "cat", Args: []string{filepath.Join(dir, "nope.txt")},
		UseSandboxTier: sandbox.ReadOnly,
	}, 20*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode == 0 {
		t.Fatal("expected the command to fail")
	}
	if prompts != 0 {
		t.Fatalf("an ENOENT opened %d escalation prompts", prompts)
	}
	if len(factory.tierSeen) != 1 {
		t.Fatalf("spawned %d processes, want 1", len(factory.tierSeen))
	}
	if strings.Contains(res.Stderr, "sandbox denied access") {
		t.Fatalf("an ordinary failure was labelled a sandbox denial:\n%s", res.Stderr)
	}
}

// TestRunSecureCaptureSuccessIsUntouched: the happiest path must not have
// grown a second spawn or a prompt.
func TestRunSecureCaptureSuccessIsUntouched(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a POSIX shell utility")
	}
	prompts := 0
	factory := &realCaptureFactory{}
	ctx := WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"*"}},
	})
	ctx = WithSandbox(ctx, newFakeSandbox(sandbox.CapabilityReport{
		Platform: runtime.GOOS, Backend: "landlock",
		Effective: sandbox.OSIsolated, Enforced: true,
	}))
	ctx = WithSecureProcessFactory(ctx, factory)
	ctx = WithPermissionCallback(ctx, func(PermissionRequest) PermissionDecision {
		prompts++
		return PermissionAllow
	})

	res, err := runSecureCapture(ctx, secproc.SecureProcessSpec{
		Tool: "shell_run", Program: "echo", Args: []string{"hello"},
		UseSandboxTier: sandbox.ReadOnly,
	}, 20*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 0 || !strings.Contains(res.Stdout, "hello") {
		t.Fatalf("unexpected result: %+v", res)
	}
	if prompts != 0 || len(factory.tierSeen) != 1 {
		t.Fatalf("a successful command spawned %d times with %d prompts",
			len(factory.tierSeen), prompts)
	}
	if res.Stderr != "" {
		t.Fatalf("Stderr = %q, want empty", res.Stderr)
	}
}
