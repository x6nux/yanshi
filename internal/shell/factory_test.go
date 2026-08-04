package shell

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"testing"

	"github.com/x6nux/yanshi/internal/netpolicy"
	"github.com/x6nux/yanshi/internal/sandbox"
	"github.com/x6nux/yanshi/internal/secproc"
)

// recordingFactory captures the env slice that DefaultSecureFactory hands off
// to the OS layer, so we can assert PrepareEnv stripped the inherited vars.
type recordingFactory struct {
	gotEnv []string
}

func (r *recordingFactory) Start(_ context.Context, spec LaunchSpec) (Process, Console, error) {
	r.gotEnv = append([]string(nil), spec.Env...)
	// Return a no-op Process + Console so DefaultSecureFactory can complete.
	return &noopProcess{}, &noopConsole{}, nil
}

type noopProcess struct{}

func (noopProcess) Wait() error                       { return nil }
func (noopProcess) PID() int                          { return 1 }
func (noopProcess) Kill() error                       { return nil }
func (noopProcess) Capabilities() ProcessCapabilities { return ProcessCapabilities{CanKillTree: false} }

type noopConsole struct{}

func (noopConsole) Read([]byte) (int, error)    { return 0, errConsoleClosed }
func (noopConsole) Write(p []byte) (int, error) { return len(p), nil }
func (noopConsole) Close() error                { return nil }
func (noopConsole) Resize(uint16, uint16) error { return nil }
func (noopConsole) PTY() bool                   { return false }

var errConsoleClosed = errors.New("console closed")

func TestConsoleReaderPreservesNonEOFError(t *testing.T) {
	_, err := (consoleReader{r: noopConsole{}}).Read(make([]byte, 8))
	if !errors.Is(err, errConsoleClosed) {
		t.Fatalf("non-EOF console error must survive, got %v", err)
	}
}

func TestDefaultSecureFactoryStripsInheritedProxyEnv(t *testing.T) {
	rec := &recordingFactory{}
	f := DefaultSecureFactory{
		OS:       rec,
		Policy:   &netpolicy.Policy{Default: "allow"},
		ProxyURL: "http://127.0.0.1:9090",
		Sandbox:  sandbox.New(sandbox.Config{Enabled: true, Tier: sandbox.ReadOnly}),
	}
	spec := secproc.SecureProcessSpec{
		Tool:    "shell_run",
		Shell:   "go version",
		Program: "go",
		Args:    []string{"version"},
		Env:     []string{"PATH=/usr/bin", "http_proxy=leak", "HTTPS_PROXY=leak"},
	}
	if _, err := f.Start(context.Background(), spec); err != nil {
		t.Fatalf("start: %v", err)
	}
	joined := strings.Join(rec.gotEnv, "\n")
	if strings.Contains(strings.ToLower(joined), "http_proxy=leak") {
		t.Fatalf("inherited proxy var survived: %v", rec.gotEnv)
	}
	if !strings.Contains(joined, "HTTP_PROXY=http://127.0.0.1:9090") {
		t.Fatalf("managed proxy var missing: %v", rec.gotEnv)
	}
}

func TestDefaultSecureFactoryFailsClosedWhenNoOSFactory(t *testing.T) {
	f := DefaultSecureFactory{}
	if _, err := f.Start(context.Background(), secproc.SecureProcessSpec{Program: "go"}); err == nil {
		t.Fatal("missing OS factory must fail closed")
	}
}

// TestDefaultSecureFactoryGivesChildTheHostEnvironment is the regression test
// for the "child sees only three proxy variables" bug.
//
// DefaultSecureFactory used to build the child env from spec.Env alone. No
// secproc caller populates that field — not shell_run, not run_tests, not
// github_*, not diagnostics — so netpolicy.PrepareEnv(nil, proxy) returned
// exactly HTTP_PROXY/HTTPS_PROXY/NO_PROXY and every spawned process ran with
// no PATH, no HOME and no GOMODCACHE. `go version` answered "command not
// found"; `go test` answered "module cache not found".
//
// PATH and HOME are asserted by name rather than by env length because they
// are the two the failure was actually reported through, and an assertion on
// "more than three entries" would pass for any three junk variables.
func TestDefaultSecureFactoryGivesChildTheHostEnvironment(t *testing.T) {
	rec := &recordingFactory{}
	f := DefaultSecureFactory{OS: rec, Policy: &netpolicy.Policy{Default: "allow"}}
	if _, err := f.Start(context.Background(), secproc.SecureProcessSpec{
		Tool: "shell_run", Shell: "go version", Program: "go", Args: []string{"version"},
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	for _, key := range hostEnvKeysUnderTest() {
		if v, ok := envValue(rec.gotEnv, key); !ok || v == "" {
			t.Fatalf("child env is missing %s — every PATH-resolved binary fails to spawn; got %v", key, rec.gotEnv)
		}
	}
	// The proxy hardening this factory already had must survive the widening.
	if v, _ := envValue(rec.gotEnv, "HTTP_PROXY"); v != "http://127.0.0.1:0" {
		t.Fatalf("HTTP_PROXY = %q, want the managed dead-port proxy", v)
	}
}

// hostEnvKeysUnderTest names the host variables the child must inherit,
// per-platform (Windows spells the home directory USERPROFILE and has no
// single PATH-equivalent alias worth asserting beyond PATH itself).
func hostEnvKeysUnderTest() []string {
	if runtime.GOOS == "windows" {
		return []string{"PATH", "USERPROFILE"}
	}
	return []string{"PATH", "HOME"}
}

// TestDefaultSecureFactoryCallerEnvWinsOverHost pins the layering order: the
// host provides the baseline, an explicit spec.Env entry overrides it. Without
// this the widening could silently ignore what a caller asked for.
func TestDefaultSecureFactoryCallerEnvWinsOverHost(t *testing.T) {
	rec := &recordingFactory{}
	f := DefaultSecureFactory{OS: rec}
	if _, err := f.Start(context.Background(), secproc.SecureProcessSpec{
		Program: "go", Env: []string{"YANSHI_ENV_PROBE=caller-wins", "http_proxy=leak"},
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	if v, ok := envValue(rec.gotEnv, "YANSHI_ENV_PROBE"); !ok || v != "caller-wins" {
		t.Fatalf("caller env entry lost: %q (present=%v)", v, ok)
	}
	if _, ok := envValue(rec.gotEnv, "PATH"); !ok {
		t.Fatal("host PATH must survive alongside caller entries")
	}
	if strings.Contains(strings.ToLower(strings.Join(rec.gotEnv, "\n")), "http_proxy=leak") {
		t.Fatalf("smuggled proxy var survived: %v", rec.gotEnv)
	}
}

// TestDefaultSecureFactoryAppliesSandboxWithSpecTier proves the sandbox seam
// runs on the secproc path and receives THIS invocation's UseSandboxTier.
// Start used to contain a bare `_ = f.Sandbox`, so the sandbox handed to the
// factory by bootstrap was held and never consulted, and the tier every
// secproc caller carefully sets was dropped on the floor.
func TestDefaultSecureFactoryAppliesSandboxWithSpecTier(t *testing.T) {
	sb := &spySandbox{report: sandbox.CapabilityReport{Requested: sandbox.ReadOnly}}
	rec := &recordingFactory{}
	f := DefaultSecureFactory{OS: rec, Sandbox: sb}
	if _, err := f.Start(context.Background(), secproc.SecureProcessSpec{
		Program: "go", Args: []string{"test"}, UseSandboxTier: sandbox.WorkspaceWrite,
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	if sb.calls != 1 {
		t.Fatalf("Sandbox.Prepare called %d times, want 1", sb.calls)
	}
	if sb.got.Tier != sandbox.WorkspaceWrite {
		t.Fatalf("Prepare got tier %v, want the spec's WorkspaceWrite", sb.got.Tier)
	}
	// spySandbox rewrites argv the way a real Phase 1+ backend would; the
	// mutation must reach the OS factory or the seam is decorative.
	if !strings.Contains(strings.Join(rec.gotEnv, "\n"), "SANDBOX_APPLIED=1") {
		t.Fatalf("sandbox env mutation did not reach the OS factory: %v", rec.gotEnv)
	}
}
