package shell

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"

	"github.com/x6nux/yanshi/internal/netpolicy"
	"github.com/x6nux/yanshi/internal/sandbox"
)

// envValue looks a KEY=VALUE entry up case-insensitively. Windows spells the
// search path "Path", POSIX spells it "PATH" — the assertion below must hold
// on both, so the lookup cannot be a plain prefix match.
func envValue(env []string, key string) (string, bool) {
	for _, item := range env {
		i := strings.IndexByte(item, '=')
		if i < 0 {
			continue
		}
		if strings.EqualFold(item[:i], key) {
			return item[i+1:], true
		}
	}
	return "", false
}

// spySandbox is a sandbox.Sandbox that records what Prepare was asked to do
// and mutates the exec.Cmd the way a real Phase 1+ backend would (argv
// wrapping + env injection). It exists so the test can prove the sandbox seam
// is actually invoked and that its mutations survive into the LaunchSpec —
// a seam that is never called is indistinguishable from a dropped one.
type spySandbox struct {
	got    sandbox.CommandSpec
	calls  int
	err    error
	report sandbox.CapabilityReport
}

func (s *spySandbox) Prepare(_ context.Context, cmd *exec.Cmd, spec sandbox.CommandSpec) error {
	s.calls++
	s.got = spec
	if s.err != nil {
		return s.err
	}
	cmd.Path = "sandbox-wrapper"
	cmd.Args = append([]string{"sandbox-wrapper", "--"}, cmd.Args...)
	cmd.Env = append(cmd.Env, "SANDBOX_APPLIED=1")
	return nil
}

func (s *spySandbox) Report() sandbox.CapabilityReport { return s.report }
func (s *spySandbox) Close() error                     { return nil }

func TestSecureLaunchFactoryFillsHostEnv(t *testing.T) {
	f := NewSecureLaunchFactory(SecureLaunchFactory{})
	spec, err := f.prepareLaunch(context.Background(), LaunchSpec{Program: "go", Args: []string{"version"}})
	if err != nil {
		t.Fatal(err)
	}
	path, ok := envValue(spec.Env, "PATH")
	if !ok || path == "" {
		t.Fatalf("child env must carry the host PATH (callers pass an empty Env); got %v", spec.Env)
	}
}

func TestSecureLaunchFactoryKeepsProxyEnv(t *testing.T) {
	f := NewSecureLaunchFactory(SecureLaunchFactory{
		Policy:   &netpolicy.Policy{Default: "allow"},
		ProxyURL: "http://127.0.0.1:18080",
	})
	spec, err := f.prepareLaunch(context.Background(), LaunchSpec{
		Program: "go",
		Args:    []string{"version"},
		// A caller-supplied (or inherited) proxy var must not shadow the
		// managed one — that is the TOCTOU bypass netpolicy.PrepareEnv exists
		// to close, and widening the env must not reopen it.
		Env: []string{"http_proxy=http://evil.example", "CALLER_VAR=1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := envValue(spec.Env, "HTTP_PROXY"); got != "http://127.0.0.1:18080" {
		t.Fatalf("HTTP_PROXY = %q, want the managed proxy", got)
	}
	if got, _ := envValue(spec.Env, "HTTPS_PROXY"); got != "http://127.0.0.1:18080" {
		t.Fatalf("HTTPS_PROXY = %q, want the managed proxy", got)
	}
	if got, ok := envValue(spec.Env, "CALLER_VAR"); !ok || got != "1" {
		t.Fatalf("caller-supplied env entries must survive, got %q (present=%v)", got, ok)
	}
	if _, ok := envValue(spec.Env, "PATH"); !ok {
		t.Fatal("host PATH must survive alongside the caller env")
	}
}

func TestSecureLaunchFactoryAppliesSandbox(t *testing.T) {
	sb := &spySandbox{report: sandbox.CapabilityReport{
		Requested: sandbox.WorkspaceWrite,
		Effective: sandbox.DegradedHostGuard,
	}}
	f := NewSecureLaunchFactory(SecureLaunchFactory{Sandbox: sb})
	spec, err := f.prepareLaunch(context.Background(), LaunchSpec{
		Program: "go", Args: []string{"version"}, Dir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if sb.calls != 1 {
		t.Fatalf("Sandbox.Prepare calls = %d, want 1", sb.calls)
	}
	if sb.got.Tier != sandbox.WorkspaceWrite {
		t.Fatalf("CommandSpec.Tier = %v, want the tier the sandbox reports as requested", sb.got.Tier)
	}
	if sb.got.Path != "go" || len(sb.got.Args) != 1 || sb.got.Args[0] != "version" {
		t.Fatalf("CommandSpec program/args = %q %v, want the launch spec's", sb.got.Path, sb.got.Args)
	}
	if spec.Program != "sandbox-wrapper" {
		t.Fatalf("Program = %q, want the sandbox's rewritten argv0", spec.Program)
	}
	if len(spec.Args) != 3 || spec.Args[0] != "--" || spec.Args[1] != "go" || spec.Args[2] != "version" {
		t.Fatalf("Args = %v, want the sandbox's rewritten argv tail", spec.Args)
	}
	if _, ok := envValue(spec.Env, "SANDBOX_APPLIED"); !ok {
		t.Fatal("env mutations made by the sandbox must survive into the LaunchSpec")
	}
}

func TestSecureLaunchFactorySandboxErrorFailsClosed(t *testing.T) {
	boom := errors.New("sandbox refused")
	sb := &spySandbox{err: boom}
	rec := &recordingFactory{}
	f := NewSecureLaunchFactory(SecureLaunchFactory{OS: rec, Sandbox: sb})
	if _, _, err := f.Start(context.Background(), LaunchSpec{Program: "go"}); !errors.Is(err, boom) {
		t.Fatalf("Start err = %v, want the sandbox error (fail-closed)", err)
	}
	if rec.gotEnv != nil {
		t.Fatal("OS factory must not be reached when the sandbox refuses")
	}
}

func TestSecureLaunchFactoryStartPassesPreparedSpec(t *testing.T) {
	rec := &recordingFactory{}
	f := NewSecureLaunchFactory(SecureLaunchFactory{OS: rec})
	proc, console, err := f.Start(context.Background(), LaunchSpec{Program: "go", Args: []string{"version"}})
	if err != nil {
		t.Fatal(err)
	}
	if proc == nil || console == nil {
		t.Fatal("Start must forward the OS factory's Process/Console")
	}
	if _, ok := envValue(rec.gotEnv, "PATH"); !ok {
		t.Fatalf("OS factory received env without PATH: %v", rec.gotEnv)
	}
}

func TestSecureLaunchFactoryDefaultsOSFactory(t *testing.T) {
	f := NewSecureLaunchFactory(SecureLaunchFactory{})
	if _, ok := f.OS.(OSProcessFactory); !ok {
		t.Fatalf("constructor must default OS to OSProcessFactory, got %T", f.OS)
	}
}
