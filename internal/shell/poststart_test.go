package shell

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"

	"github.com/x6nux/yanshi/internal/sandbox"
	"github.com/x6nux/yanshi/internal/secproc"
)

// This file pins that the post-spawn half of the sandbox seam is actually
// CALLED by both production factories, and that a refusal from it fails the
// launch.
//
// It exists because the seam has exactly one production implementation and that
// implementation only works on Windows. Nobody developing on macOS or Linux will
// ever observe it running, which is the precise shape of this repo's most common
// defect: a mechanism that is written, reviewed, merged, and never invoked. The
// type assertion that dispatches to it fails SILENTLY — a backend that stopped
// implementing sandbox.PostStartSandbox would simply never be called, every
// child would run uncontained, and the capability report would go on claiming
// CanKillTree.
//
// A fake rather than a mock, per the package convention: postStartSpy is a
// working sandbox that happens to count calls.

// postStartSpy is a sandbox.Sandbox that also implements
// sandbox.PostStartSandbox, recording the pids it was handed.
//
// It deliberately does NOT wrap the command in Prepare. The Windows backend
// does not either — it has no launcher to insert — so a spy that rewrote argv
// would be testing a shape no PostStart backend has.
type postStartSpy struct {
	pids      []int
	prepared  int
	postErr   error
	closed    bool
	postCalls int
}

func (s *postStartSpy) Prepare(context.Context, *exec.Cmd, sandbox.CommandSpec) error {
	s.prepared++
	return nil
}

func (s *postStartSpy) Report() sandbox.CapabilityReport {
	return sandbox.CapabilityReport{
		Platform: "test", Effective: sandbox.DegradedHostGuard,
		Backend: "jobobject-spy", Reason: "test fake", CanKillTree: true,
	}
}

func (s *postStartSpy) Close() error { s.closed = true; return nil }

func (s *postStartSpy) PostStart(pid int) error {
	s.postCalls++
	s.pids = append(s.pids, pid)
	return s.postErr
}

// plainSandbox implements only sandbox.Sandbox, standing in for darwin, linux
// and the degraded stub. The launch path must tolerate it — the type assertion
// simply fails — rather than requiring every backend to grow a method.
type plainSandbox struct{ prepared int }

func (s *plainSandbox) Prepare(context.Context, *exec.Cmd, sandbox.CommandSpec) error {
	s.prepared++
	return nil
}

func (s *plainSandbox) Report() sandbox.CapabilityReport {
	return sandbox.CapabilityReport{
		Platform: "test", Effective: sandbox.DegradedHostGuard,
		Backend: "no-poststart", Reason: "test fake",
	}
}

func (s *plainSandbox) Close() error { return nil }

// trueSpec returns a LaunchSpec for a command that exits immediately and exists
// on every platform this test runs on.
func trueSpec(t *testing.T) LaunchSpec {
	t.Helper()
	if prog, err := exec.LookPath("cmd.exe"); err == nil {
		return LaunchSpec{Program: prog, Args: []string{"/c", "exit 0"}}
	}
	prog, err := exec.LookPath("sh")
	if err != nil {
		t.Skipf("no shell available to spawn a probe child: %v", err)
	}
	return LaunchSpec{Program: prog, Args: []string{"-c", "exit 0"}}
}

// TestSecureLaunchFactoryCallsPostStart is the wiring proof for the shell v2
// path.
//
// It asserts three things a passing build cannot: that PostStart runs at all,
// that it is handed the pid of the process that was actually started (not 0, and
// not the parent's), and that it runs AFTER the process exists. The pid check is
// what makes this more than a call counter — a wiring that called PostStart
// before Start would compile, would increment the counter, and would hand over
// pid 0.
func TestSecureLaunchFactoryCallsPostStart(t *testing.T) {
	spy := &postStartSpy{}
	f := NewSecureLaunchFactory(SecureLaunchFactory{Sandbox: spy})
	proc, console, err := f.Start(context.Background(), trueSpec(t))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() {
		if console != nil {
			_ = console.Close()
		}
		_ = proc.Wait()
	}()

	if spy.postCalls != 1 {
		t.Fatalf("PostStart was called %d times, want 1. The optional sandbox seam is "+
			"not wired into SecureLaunchFactory.Start, so a Job Object backend would "+
			"report CanKillTree while containing nothing.", spy.postCalls)
	}
	if len(spy.pids) != 1 || spy.pids[0] <= 0 {
		t.Fatalf("PostStart got pid %v; it must receive the started child's real pid, "+
			"which means it has to run after Start rather than before", spy.pids)
	}
	if spy.pids[0] != proc.PID() {
		t.Fatalf("PostStart got pid %d but the factory returned pid %d; the seam was "+
			"handed the wrong process", spy.pids[0], proc.PID())
	}
	if spy.prepared != 1 {
		t.Errorf("Prepare ran %d times, want 1", spy.prepared)
	}
}

// TestDefaultSecureFactoryCallsPostStart is the same proof for the secproc path.
//
// Both paths need it separately because they are genuinely separate launch
// pipelines — different spec type, different return type, no shared Start — and
// wiring one is not wiring the other. That asymmetry is documented in
// procfactory.go's header and is exactly the kind of thing a single test would
// silently half-cover.
func TestDefaultSecureFactoryCallsPostStart(t *testing.T) {
	spy := &postStartSpy{}
	spec := trueSpec(t)
	f := DefaultSecureFactory{OS: OSProcessFactory{}, Sandbox: spy}
	started, err := f.Start(context.Background(), secproc.SecureProcessSpec{
		Program: spec.Program,
		Args:    spec.Args,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = started.Wait() }()

	if spy.postCalls != 1 {
		t.Fatalf("PostStart was called %d times, want 1. The optional sandbox seam is "+
			"not wired into DefaultSecureFactory.Start.", spy.postCalls)
	}
	if len(spy.pids) != 1 || spy.pids[0] != started.PID {
		t.Fatalf("PostStart got pids %v but the factory reported pid %d", spy.pids, started.PID)
	}
}

// TestPostStartRefusalFailsTheLaunch pins the fail-closed direction on both
// paths.
//
// A PostStart error means the child is NOT contained — and by contract the
// backend has already killed it. Returning the process anyway would hand the
// caller something the capability report describes as contained, which is the
// over-claim the sandbox package exists to prevent. The caller must get an error
// and no process.
func TestPostStartRefusalFailsTheLaunch(t *testing.T) {
	refusal := errors.New("could not contain the child")
	spec := trueSpec(t)

	t.Run("shell v2 path", func(t *testing.T) {
		spy := &postStartSpy{postErr: refusal}
		f := NewSecureLaunchFactory(SecureLaunchFactory{Sandbox: spy})
		proc, console, err := f.Start(context.Background(), spec)
		if err == nil {
			if console != nil {
				_ = console.Close()
			}
			if proc != nil {
				_ = proc.Wait()
			}
			t.Fatal("Start returned a process the sandbox could not contain")
		}
		if !errors.Is(err, refusal) {
			t.Errorf("the containment failure was rewritten: %v", err)
		}
		if proc != nil || console != nil {
			t.Error("a failed launch must return neither a process nor a console")
		}
	})

	t.Run("secproc path", func(t *testing.T) {
		spy := &postStartSpy{postErr: refusal}
		f := DefaultSecureFactory{OS: OSProcessFactory{}, Sandbox: spy}
		started, err := f.Start(context.Background(), secproc.SecureProcessSpec{
			Program: spec.Program, Args: spec.Args,
		})
		if err == nil {
			_ = started.Wait()
			t.Fatal("Start returned a process the sandbox could not contain")
		}
		if !errors.Is(err, refusal) {
			t.Errorf("the containment failure was rewritten: %v", err)
		}
		if started != nil {
			t.Error("a failed launch must return no process")
		}
	})
}

// TestSandboxWithoutPostStartLaunchesNormally is the compatibility half, and it
// is why the seam is an optional interface rather than a method on Sandbox.
//
// darwin, linux, the degraded stub and every test double in the tree implement
// only sandbox.Sandbox. The launch path must treat a failed type assertion as
// "nothing to do" rather than as an error — otherwise adding this seam would
// have broken every non-Windows platform.
func TestSandboxWithoutPostStartLaunchesNormally(t *testing.T) {
	plain := &plainSandbox{}
	f := NewSecureLaunchFactory(SecureLaunchFactory{Sandbox: plain})
	proc, console, err := f.Start(context.Background(), trueSpec(t))
	if err != nil {
		t.Fatalf("a sandbox that does not implement PostStartSandbox broke the launch: %v", err)
	}
	if console != nil {
		_ = console.Close()
	}
	_ = proc.Wait()
	if plain.prepared != 1 {
		t.Errorf("Prepare ran %d times, want 1", plain.prepared)
	}
}

// TestNilSandboxSkipsPostStart covers the configuration most tests run under:
// no sandbox at all. postStart must not dereference it.
func TestNilSandboxSkipsPostStart(t *testing.T) {
	var p childLaunchPosture
	if err := p.postStart(1234); err != nil {
		t.Fatalf("postStart with a nil sandbox must be a no-op, got: %v", err)
	}
}

// TestPostStartUsesTheSamePostureThatPrepared guards the extraction of
// SecureLaunchFactory.posture().
//
// prepare() and postStart() must be asked of the same posture: if Start built a
// second struct literal, a field added to childLaunchPosture would be populated
// in one and zero in the other, and the sandbox seam would run against a posture
// nothing was launched under. The observable consequence is that the sandbox the
// factory holds is the sandbox that gets called — which is what this asserts, by
// giving the factory one sandbox and checking that one received the callback.
func TestPostStartUsesTheSamePostureThatPrepared(t *testing.T) {
	spy := &postStartSpy{}
	other := &postStartSpy{}
	f := NewSecureLaunchFactory(SecureLaunchFactory{Sandbox: spy})
	proc, console, err := f.Start(context.Background(), trueSpec(t))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if console != nil {
		_ = console.Close()
	}
	_ = proc.Wait()

	if spy.prepared != 1 || spy.postCalls != 1 {
		t.Fatalf("the factory's own sandbox saw prepare=%d postStart=%d; both must be 1",
			spy.prepared, spy.postCalls)
	}
	if other.prepared != 0 || other.postCalls != 0 {
		t.Fatal("a sandbox the factory never held was called")
	}
}

// TestPostStartErrorNamesTheChild is a diagnostics assertion: an operator or a
// log reader must be able to tell a containment failure from an ordinary spawn
// failure, because the responses differ (one is a host capability problem, the
// other is a bad command).
func TestPostStartErrorNamesTheChild(t *testing.T) {
	spy := &postStartSpy{postErr: errors.New("AssignProcessToJobObject(4242) failed: access denied")}
	f := NewSecureLaunchFactory(SecureLaunchFactory{Sandbox: spy})
	_, _, err := f.Start(context.Background(), trueSpec(t))
	if err == nil {
		t.Fatal("expected a containment failure")
	}
	if !strings.Contains(err.Error(), "AssignProcessToJobObject") {
		t.Errorf("the backend's diagnostic was lost on the way out: %v", err)
	}
}
