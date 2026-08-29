package shell

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/x6nux/yanshi/internal/execbroker"
	"github.com/x6nux/yanshi/internal/guard"
)

// This file checks the elevation-shim wiring on the REAL production factory,
// which is the half internal/execbroker's own tests cannot reach: that package
// builds its own Server and drives it directly, so a childLaunchPosture that
// forgot to call interceptElevation, or that installed the shims before the
// PATH was built rather than after, would leave every one of its tests green.
//
// ⚠️ Every probe here uses `doas` rather than `sudo`. That is a safety
// requirement, not a stylistic one: these tests run a real /bin/sh with the
// HOST's PATH (childLaunchPosture builds the child environment from
// os.Environ(), which is the whole point of it), so a bug that left the shim
// off PATH would resolve the name against the operator's real programs. `doas`
// is not installed on macOS or on any CI image here, so the failure mode of a
// missing shim is a loud "not found" instead of a real privilege escalation
// attempt on the machine running the suite.

// TestMain makes this test binary answer as the shim.
//
// The shims are symlinks to whatever binary called execbroker.Listen, which in
// production is yanshi and here is the test binary. Without this, a child that
// resolves `doas` through the shim runs `go test` with the wrong flags and
// exits 2 — a failure that looks like a broken test rather than like the
// missing dispatch it is. Routing through the same IsShimInvocation/RunShim
// pair cmd/yanshi uses keeps the two in step.
func TestMain(m *testing.M) {
	if name, ok := execbroker.IsShimInvocation(os.Args[0]); ok {
		cwd, _ := os.Getwd()
		if err := execbroker.RunShim(name, os.Args, cwd); err != nil {
			fmt.Fprintln(os.Stderr, err)
		}
		os.Exit(execbroker.ShimExitCode)
	}
	os.Exit(m.Run())
}

// TestProductionLaunchInstallsTheElevationShims pins that the shims reach a
// child spawned through SecureLaunchFactory — the factory shell v2 uses.
func TestProductionLaunchInstallsTheElevationShims(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the shims are symlinks; execbroker.Listen reports ErrUnsupported on windows")
	}
	factory := NewSecureLaunchFactory(SecureLaunchFactory{})
	proc, console, err := factory.Start(context.Background(), LaunchSpec{
		Program: "/bin/sh",
		Args:    []string{"-c", `command -v doas; echo "broker=${YANSHI_EXEC_BROKER:+set}"`},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	out := drainConsole(t, console)
	_ = proc.Wait()

	if !strings.Contains(out, "broker=set") {
		t.Errorf("the child did not receive the broker address; the token would have been "+
			"scrubbed or the env rebuilt after interception:\n%s", out)
	}
	shim := firstLine(out)
	if shim == "" {
		t.Fatalf("`command -v doas` found nothing: the shim directory is not on the child's PATH:\n%s", out)
	}
	if filepath.Base(shim) != "doas" {
		t.Fatalf("resolved %q, which is not a doas shim:\n%s", shim, out)
	}
	if _, err := os.Lstat(shim); err == nil {
		t.Logf("child resolved doas to the shim at %s", shim)
	}
}

// TestNestedElevationFailsClosedWithoutAnAuthorizer is the fail-closed
// assertion on the production path.
//
// internal/shell does not import internal/tools, so no Authorizer is registered
// in this test binary and secproc.Authorize answers ErrNoAuthorizer. That is
// exactly the state a composition root that forgot to wire tools would be in,
// and the required outcome is a refusal — not a pass-through on the reasoning
// that "there is no policy to violate".
func TestNestedElevationFailsClosedWithoutAnAuthorizer(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the shims are symlinks; execbroker.Listen reports ErrUnsupported on windows")
	}
	factory := NewSecureLaunchFactory(SecureLaunchFactory{})
	proc, console, err := factory.Start(context.Background(), LaunchSpec{
		Program: "/bin/sh",
		Args:    []string{"-c", `echo BEFORE; doas -n true; echo "AFTER exit=$?"`},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	out := drainConsole(t, console)
	_ = proc.Wait()

	if strings.Contains(out, "not found") {
		t.Fatalf("the shim was not on PATH, so this probe measured nothing:\n%s", out)
	}
	if !strings.Contains(out, "BEFORE") || !strings.Contains(out, "AFTER exit=") {
		t.Fatalf("the script did not run to completion:\n%s", out)
	}
	if !strings.Contains(out, "AFTER exit=126") {
		t.Errorf("the elevation was not refused with the shim's exit code:\n%s", out)
	}
	if !strings.Contains(out, "no authorizer registered") {
		t.Errorf("the refusal reason did not reach the child, so an operator sees a bare 126:\n%s", out)
	}
}

// TestElevationBrokerIsReclaimedOnReap pins that the shim directory does not
// accumulate one entry per spawn for the life of the server.
//
// The teardown hangs off Wait rather than off the launch context, because the
// shell v2 launch context is deliberately detached from the turn
// (context.WithoutCancel in Manager.Start) and may not be cancelled for hours.
// A leak here is not merely untidy: each entry is a directory of symlinks named
// `sudo` pointing at the yanshi binary.
func TestElevationBrokerIsReclaimedOnReap(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the shims are symlinks; execbroker.Listen reports ErrUnsupported on windows")
	}
	factory := NewSecureLaunchFactory(SecureLaunchFactory{})
	proc, console, err := factory.Start(context.Background(), LaunchSpec{
		Program: "/bin/sh",
		Args:    []string{"-c", `echo "$YANSHI_EXEC_SHIM_DIR"`},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	dir := strings.TrimSpace(drainConsole(t, console))
	if dir == "" {
		t.Fatal("the child never saw a shim directory")
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("shim directory %q did not exist while the child ran: %v", dir, err)
	}
	if err := proc.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("shim directory %q survived the reap: %v", dir, err)
	}
}

// TestElevationDeciderQuotesTheInterceptedArgv pins the argv → command-string
// round trip the guard is handed.
//
// The decider is what turns exact argv into an Action.Shell string, and a
// quoting bug there does not fail loudly: it produces a DIFFERENT command from
// the one that will run, which the operator then approves. A space in an
// argument is the cheapest way to see it.
func TestElevationDeciderQuotesTheInterceptedArgv(t *testing.T) {
	got := execbroker.CommandLine("sudo", []string{"rm", "-rf", "/a b", "it's"})
	want := `'sudo' 'rm' '-rf' '/a b' 'it'\''s'`
	if got != want {
		t.Fatalf("CommandLine =\n  %s\nwant\n  %s", got, want)
	}
	// The round trip must survive a real shell: this is what proves the quoting
	// is POSIX rather than merely self-consistent.
	out, err := exec.Command("/bin/sh", "-c", "for a in "+got+"; do echo \"[$a]\"; done").Output()
	if err != nil {
		t.Fatalf("re-lexing the quoted command failed: %v", err)
	}
	wantWords := "[sudo]\n[rm]\n[-rf]\n[/a b]\n[it's]\n"
	if string(out) != wantWords {
		t.Fatalf("a real shell split the quoted command differently:\n got=%q\nwant=%q", out, wantWords)
	}
}

// TestQuotedElevationIsNotAStructuralHardDeny pins the interaction between the
// quoting above and the guard's segmenter.
//
// checkShell treats a command its reader cannot parse as a STRUCTURAL HardDeny —
// the one tier yolo and auto cannot override. If CommandLine produced something
// the segmenter read as a chain, every intercepted elevation carrying a
// semicolon or an ampersand in an argument would be permanently un-approvable,
// no matter what the operator did. That is a denial rather than a bypass, so it
// would never show up as a security finding; it would show up as `sudo apt
// install 'a;b'` being impossible and nobody knowing why.
//
// The profile here allows everything, so any HardDeny that appears is
// structural by construction rather than policy.
func TestQuotedElevationIsNotAStructuralHardDeny(t *testing.T) {
	permissive := guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"*"}},
		FS:    guard.FSPerm{Read: []string{"**"}, Write: []string{"**"}},
		Shell: guard.ShellPerm{Policy: "denylist"},
		Net:   guard.NetPerm{Allow: true},
	}
	for _, args := range [][]string{
		{"apt-get", "install", "a;b"},
		{"sh", "-c", "make && make install"},
		{"tee", "/tmp/x > /tmp/y"},
		{"printf", "%s\n", "back`tick`"},
	} {
		line := execbroker.CommandLine("sudo", args)
		d := guard.New().Check(permissive, guard.Action{Tool: "shell_run", Shell: line})
		if d.Verdict == guard.HardDeny && !d.Overridable {
			t.Errorf("quoted elevation %s was refused structurally (%s); no operator "+
				"decision can ever approve it", line, d.Reason)
		}
	}
}

// drainConsole reads a console to EOF.
func drainConsole(t *testing.T, console Console) string {
	t.Helper()
	defer console.Close()
	var sb strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := console.Read(buf)
		sb.Write(buf[:n])
		if err != nil {
			return sb.String()
		}
	}
}

// firstLine returns the first non-empty line of s.
func firstLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return line
		}
	}
	return ""
}
