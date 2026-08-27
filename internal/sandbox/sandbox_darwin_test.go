//go:build darwin

package sandbox

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// This file contains the process-level tests: they build a real Config, run
// the real Prepare, and START the resulting command. Nothing here is a
// simulation — every assertion below is about what the macOS kernel actually
// did to a child process.
//
// Why that is necessary and a text assertion is not: a profile that is
// syntactically perfect and semantically wrong produces exactly the same
// string-comparison result as a correct one. Two real defects found only by
// running these: (a) an unresolved /var/folders workspace produces a rule the
// kernel never matches, so WorkspaceWrite silently degrades to ReadOnly, and
// (b) an unescaped quote in a path produces a profile that COMPILES and grants
// blanket write. Both pass any test that only inspects the generated text.
//
// Every enforcement assertion below is paired with an unsandboxed control run
// of the same command. Without the control an assertion of the form "the write
// failed" proves nothing — the write could have failed because the directory
// did not exist, because the disk was full, or because the test itself was
// wrong. The control is what makes the sandbox the only remaining explanation.

// outsideDir returns a directory that is genuinely outside every path the
// WorkspaceWrite tier grants, and registers its cleanup.
//
// t.TempDir() is NOT usable for this and that is the whole reason this helper
// exists. On macOS t.TempDir() lives under TMPDIR, and TMPDIR is in
// ScratchPaths — so a test that used it as its "outside" location would be
// writing into a directory the tier deliberately grants, observe the write
// succeed, and report a sandbox escape that is actually correct behaviour.
// Three tests in this file failed exactly that way on first run.
//
// The location chosen is under the user's home directory, which no tier grants
// (~/Library/Caches and ~/.cache are granted; the home root is not). The
// helper verifies that against the live ScratchPaths rather than assuming it,
// so a future addition to the scratch list turns this into a skip with an
// explanation instead of a mystery failure.
func outsideDir(t *testing.T) string {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home directory to place an out-of-sandbox path in: %v", err)
	}
	dir, err := os.MkdirTemp(home, ".yanshi-sandbox-test-")
	if err != nil {
		t.Skipf("cannot create an out-of-sandbox directory under %s: %v", home, err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	resolved := ResolvePath(dir)
	for _, granted := range ScratchPaths() {
		if resolved == granted || strings.HasPrefix(resolved+string(filepath.Separator), granted+string(filepath.Separator)) {
			t.Skipf("the chosen out-of-sandbox path %s is inside granted scratch %s; "+
				"this test cannot distinguish enforcement from configuration here", resolved, granted)
		}
	}
	return dir
}

// shortTempDir returns a temp directory with a path short enough for an AF_UNIX
// socket name.
//
// sockaddr_un.sun_path is 104 bytes on macOS, and t.TempDir() paths (which
// embed the full test name under /var/folders/…) routinely exceed that: the
// unix-socket test first failed with "nc: File name too long", which reads
// like a sandbox refusal and is not one. Placing the socket under /tmp keeps
// it well inside the limit. That directory is granted scratch, which is
// irrelevant here — this test asserts socket behaviour, not the write
// boundary, which TestWorkspaceWriteTierBoundary owns.
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "ys")
	if err != nil {
		t.Skipf("cannot create a short temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// requireEnforcing builds a sandbox for cfg and skips the test when this host
// does not actually enforce.
//
// A skip rather than a failure because the same test binary must be runnable
// on a macOS where Apple has finally removed sandbox-exec, inside a CI
// container, or under an outer sandbox that forbids nesting. What it must
// never do is silently pass: a skip is visible in `go test -v` output, an
// unconditional pass is not. The skip message names the reason the backend
// itself reported, so an operator sees WHY rather than just "skipped".
func requireEnforcing(t *testing.T, cfg Config) Sandbox {
	t.Helper()
	sb := New(cfg)
	rep := sb.Report()
	if !rep.Enforced {
		t.Skipf("Seatbelt not enforcing on this host (%s): %s", rep.Backend, rep.Reason)
	}
	if rep.Effective != OSIsolated {
		t.Fatalf("backend enforces but does not report OSIsolated: %#v", rep)
	}
	t.Cleanup(func() {
		if err := sb.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return sb
}

// runResult is what runSandboxed and runUnsandboxed report back.
type runResult struct {
	exitCode int
	output   string
	err      error
}

// ok reports whether the command completed successfully.
func (r runResult) ok() bool { return r.err == nil && r.exitCode == 0 }

// String renders the result for a failure message. The child's combined output
// is included because "exit 1" alone never explains anything.
func (r runResult) String() string {
	return fmt.Sprintf("exit=%d err=%v output=%q", r.exitCode, r.err, strings.TrimSpace(r.output))
}

// runSandboxed prepares a /bin/sh -c command through sb and runs it, returning
// the outcome. It goes through Prepare rather than constructing an argv by
// hand so the test exercises the code path production uses; a bug in the
// wrapping shows up here rather than being papered over by a test-only argv.
func runSandboxed(t *testing.T, sb Sandbox, tier AccessTier, script string, env ...string) runResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", script)
	cmd.Env = append(os.Environ(), env...)
	spec := CommandSpec{Path: "/bin/sh", Args: []string{"-c", script}, Tier: tier}
	if err := sb.Prepare(ctx, cmd, spec); err != nil {
		t.Fatalf("Prepare(%s): %v", tier, err)
	}
	if cmd.Path != sandboxExecPath {
		t.Fatalf("Prepare did not wrap the command: Path=%q", cmd.Path)
	}
	return runCmd(cmd)
}

// runUnsandboxed runs the identical script with no sandbox. This is the
// control every enforcement assertion is measured against.
func runUnsandboxed(t *testing.T, script string, env ...string) runResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", script)
	cmd.Env = append(os.Environ(), env...)
	return runCmd(cmd)
}

// runCmd executes cmd and normalises the outcome. A non-zero exit is reported
// as an exit code with err==nil, because for these tests a refusal IS the
// expected result and treating it as a Go error would make every success path
// read backwards.
func runCmd(cmd *exec.Cmd) runResult {
	out, err := cmd.CombinedOutput()
	res := runResult{output: string(out)}
	var ee *exec.ExitError
	switch {
	case err == nil:
		res.exitCode = 0
	case errors.As(err, &ee):
		res.exitCode = ee.ExitCode()
	default:
		res.err = err
		res.exitCode = -1
	}
	return res
}

// TestSeatbeltReportsEnforcement pins that a healthy macOS host produces an
// OSIsolated report naming the seatbelt backend.
//
// The backend string is asserted because it is not decoration:
// tools.BackendKindFor routes on it to pick which violation-diagnostic regex
// table to match a failed command's stderr against. A backend renamed to
// something the router does not recognise falls through to the
// match-every-table path, which still works but stops being a statement about
// what actually refused.
func TestSeatbeltReportsEnforcement(t *testing.T) {
	sb := requireEnforcing(t, Config{Enabled: true, WorkspaceRoot: t.TempDir(), Tier: ReadOnly})
	rep := sb.Report()
	if rep.Platform != "darwin" {
		t.Errorf("Platform = %q, want darwin", rep.Platform)
	}
	if !strings.Contains(rep.Backend, "seatbelt") {
		t.Errorf("Backend = %q; tools.BackendKindFor routes on this substring", rep.Backend)
	}
	if rep.CanKillTree {
		t.Error("Seatbelt is an access policy, not a process container; it cannot kill trees")
	}
	if rep.Requested != ReadOnly {
		t.Errorf("Requested = %v, want ReadOnly", rep.Requested)
	}
}

// TestReadOnlyTierRefusesWrites is deliverable 1: a real child under the
// ReadOnly tier cannot create a file, and the file genuinely does not exist
// afterwards.
//
// The existence check is separate from the exit code on purpose. An exit code
// says the shell reported a failure; only stat'ing the path proves nothing was
// written. A backend that let the write through and then failed for an
// unrelated reason would pass an exit-code-only assertion.
func TestReadOnlyTierRefusesWrites(t *testing.T) {
	ws := t.TempDir()
	sb := requireEnforcing(t, Config{Enabled: true, WorkspaceRoot: ws, Tier: ReadOnly})

	target := filepath.Join(ws, "should_not_exist")
	script := fmt.Sprintf("echo x > %q", target)

	got := runSandboxed(t, sb, ReadOnly, script)
	if got.ok() {
		t.Fatalf("ReadOnly tier permitted a write: %s", got)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("ReadOnly tier: %s exists after a refused write (stat err=%v)", target, err)
	}

	// Control: the identical script with no sandbox must succeed, which is
	// what makes the sandbox the only explanation for the failure above.
	if control := runUnsandboxed(t, script); !control.ok() {
		t.Fatalf("control run failed, so the refusal above proves nothing: %s", control)
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("control run did not create %s: %v", target, err)
	}
	t.Logf("sandboxed: %s", got)
}

// TestReadOnlyTierStillPermitsReadsAndOutput pins the other half of ReadOnly:
// the tier is a WRITE restriction, and a child that cannot read its own source
// or write to stdout is not a read-only sandbox, it is a broken one.
//
// /dev/null is asserted specifically because `cmd > /dev/null` appears in a
// large fraction of real shell commands and its failure would look to the
// model like the command itself was broken rather than like a policy decision.
func TestReadOnlyTierStillPermitsReadsAndOutput(t *testing.T) {
	ws := t.TempDir()
	readable := filepath.Join(ws, "input.txt")
	if err := os.WriteFile(readable, []byte("payload\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sb := requireEnforcing(t, Config{Enabled: true, WorkspaceRoot: ws, Tier: ReadOnly})

	got := runSandboxed(t, sb, ReadOnly, fmt.Sprintf("cat %q", readable))
	if !got.ok() {
		t.Fatalf("ReadOnly tier refused a read: %s", got)
	}
	if !strings.Contains(got.output, "payload") {
		t.Fatalf("ReadOnly read produced the wrong content: %s", got)
	}
	if devnull := runSandboxed(t, sb, ReadOnly, "echo x > /dev/null"); !devnull.ok() {
		t.Fatalf("ReadOnly tier broke `> /dev/null`: %s", devnull)
	}
	if stdout := runSandboxed(t, sb, ReadOnly, "echo to-stdout"); !stdout.ok() ||
		!strings.Contains(stdout.output, "to-stdout") {
		t.Fatalf("ReadOnly tier broke stdout: %s", stdout)
	}
}

// TestWorkspaceWriteTierBoundary is deliverable 2: writes inside the workspace
// succeed and writes outside it are refused, in the same process-level run.
//
// Both halves are required. Inside-only would pass on a sandbox that enforces
// nothing; outside-only would pass on a sandbox whose workspace rule never
// matches — which is exactly the /var/folders symlink bug, where the inside
// write failed with the identical EPERM as the outside write. Asserting both
// against the same profile is what distinguishes "the boundary is in the right
// place" from "everything is denied".
func TestWorkspaceWriteTierBoundary(t *testing.T) {
	ws := t.TempDir()
	outsideRoot := outsideDir(t)
	sb := requireEnforcing(t, Config{Enabled: true, WorkspaceRoot: ws, Tier: WorkspaceWrite})

	inside := filepath.Join(ws, "inside.txt")
	outside := filepath.Join(outsideRoot, "outside.txt")

	gotIn := runSandboxed(t, sb, WorkspaceWrite, fmt.Sprintf("echo in > %q", inside))
	if !gotIn.ok() {
		t.Fatalf("WorkspaceWrite refused a write INSIDE the workspace: %s\n"+
			"(this is the symptom of an unresolved symlinked workspace root)", gotIn)
	}
	if data, err := os.ReadFile(inside); err != nil || strings.TrimSpace(string(data)) != "in" {
		t.Fatalf("inside write did not land: data=%q err=%v", data, err)
	}

	gotOut := runSandboxed(t, sb, WorkspaceWrite, fmt.Sprintf("echo out > %q", outside))
	if gotOut.ok() {
		t.Fatalf("WorkspaceWrite permitted a write OUTSIDE the workspace: %s", gotOut)
	}
	if _, err := os.Stat(outside); !os.IsNotExist(err) {
		t.Fatalf("%s exists after a refused write (stat err=%v)", outside, err)
	}

	// Control: unsandboxed, the outside write succeeds.
	if control := runUnsandboxed(t, fmt.Sprintf("echo out > %q", outside)); !control.ok() {
		t.Fatalf("control run failed, so the refusal above proves nothing: %s", control)
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatalf("control run did not create %s: %v", outside, err)
	}
	t.Logf("inside: %s | outside(sandboxed): %s", gotIn, gotOut)
}

// TestWorkspaceWriteRefusesPrefixSiblings pins that (subpath) is a path-
// component match and not a string-prefix match.
//
// If it were a prefix match, a workspace at /x/ws would silently also grant
// write to /x/wsEVIL — a sibling an attacker could create by name. The
// behaviour was measured before the generator was written, but measuring once
// during development is not the same as pinning it: this test is what fails if
// a future edit swaps (subpath) for (regex) or (prefix).
func TestWorkspaceWriteRefusesPrefixSiblings(t *testing.T) {
	root := outsideDir(t)
	ws := filepath.Join(root, "ws")
	sibling := filepath.Join(root, "wsEVIL")
	for _, d := range []string{ws, sibling} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	sb := requireEnforcing(t, Config{Enabled: true, WorkspaceRoot: ws, Tier: WorkspaceWrite})

	victim := filepath.Join(sibling, "leak.txt")
	got := runSandboxed(t, sb, WorkspaceWrite, fmt.Sprintf("echo leak > %q", victim))
	if got.ok() {
		t.Fatalf("a name-prefix sibling of the workspace was writable: %s", got)
	}
	if _, err := os.Stat(victim); !os.IsNotExist(err) {
		t.Fatalf("%s exists (stat err=%v)", victim, err)
	}
	// Control: the workspace itself is writable, so the refusal above is about
	// the sibling and not about the profile being broken outright.
	if in := runSandboxed(t, sb, WorkspaceWrite,
		fmt.Sprintf("echo ok > %q", filepath.Join(ws, "ok.txt"))); !in.ok() {
		t.Fatalf("the workspace itself was not writable: %s", in)
	}
}

// TestSymlinkedWorkspaceRootIsResolved is the process-level regression test for
// the /var/folders bug.
//
// t.TempDir() on macOS already returns a symlinked path, so every other test
// here exercises the resolution implicitly. This one makes it explicit and
// unmissable by adding a second, deliberate level of indirection: the config
// names a symlink, and a write through the symlink's TARGET must still be
// permitted. Before ResolvePath existed, this failed.
func TestSymlinkedWorkspaceRootIsResolved(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "real-workspace")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "linked-workspace")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}
	// Configure with the SYMLINK; write through the REAL path.
	sb := requireEnforcing(t, Config{Enabled: true, WorkspaceRoot: link, Tier: WorkspaceWrite})
	target := filepath.Join(real, "through-real-path.txt")
	got := runSandboxed(t, sb, WorkspaceWrite, fmt.Sprintf("echo x > %q", target))
	if !got.ok() {
		t.Fatalf("a symlinked workspace root was not resolved, so the tier is silently read-only: %s", got)
	}
}

// TestFullAccessTierImposesNoWriteRestriction pins the top rung of the tier
// ladder. The escalation path in internal/tools walks ReadOnly →
// WorkspaceWrite → FullAccess after an operator approves; if the top rung
// still refused, an approved escalation would fail identically to the refusal
// that prompted it and the operator would have no way to tell the difference.
func TestFullAccessTierImposesNoWriteRestriction(t *testing.T) {
	ws := t.TempDir()
	outside := outsideDir(t)
	sb := requireEnforcing(t, Config{Enabled: true, WorkspaceRoot: ws, Tier: FullAccess})

	target := filepath.Join(outside, "full-access.txt")
	got := runSandboxed(t, sb, FullAccess, fmt.Sprintf("echo x > %q", target))
	if !got.ok() {
		t.Fatalf("FullAccess refused a write outside the workspace: %s", got)
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("FullAccess write did not land: %v", err)
	}
}

// TestPerInvocationTierOverridesGlobalTier pins that Prepare honours
// CommandSpec.Tier and not Config.Tier.
//
// This is the whole point of secproc's UseSandboxTier: git_diff declares
// ReadOnly and run_tests declares WorkspaceWrite. If Prepare read the global
// tier instead, both would silently run at whatever the operator configured
// and the per-tool declarations would be decoration. The sandbox here is
// configured FullAccess and the invocation asks for ReadOnly — the strictest
// possible disagreement, so a bug reading the wrong field is unmissable.
func TestPerInvocationTierOverridesGlobalTier(t *testing.T) {
	ws := t.TempDir()
	sb := requireEnforcing(t, Config{Enabled: true, WorkspaceRoot: ws, Tier: FullAccess})

	target := filepath.Join(ws, "per-invocation.txt")
	script := fmt.Sprintf("echo x > %q", target)

	strict := runSandboxed(t, sb, ReadOnly, script)
	if strict.ok() {
		t.Fatalf("invocation asked for ReadOnly but the write succeeded; "+
			"Prepare is reading Config.Tier instead of CommandSpec.Tier: %s", strict)
	}
	// Same sandbox, permissive invocation: proves the refusal above was the
	// tier and not a broken sandbox.
	if loose := runSandboxed(t, sb, FullAccess, script); !loose.ok() {
		t.Fatalf("FullAccess invocation on the same sandbox failed: %s", loose)
	}
}

// TestNetworkDenyRefusesEgress is deliverable 3: a real outbound connection is
// refused under NetworkDeny and succeeds without it.
//
// The server is a loopback listener in this process rather than a public host.
// A test that reached the internet would fail in an offline CI runner and,
// worse, would be indistinguishable from a working denial when the network was
// merely down — the control run is what makes that distinction, and a control
// that depends on the internet is not a control.
func TestNetworkDenyRefusesEgress(t *testing.T) {
	url := startLoopbackServer(t)
	ws := t.TempDir()

	denied := requireEnforcing(t, Config{
		Enabled: true, WorkspaceRoot: ws, Tier: ReadOnly, NetworkDeny: true,
	})
	script := fmt.Sprintf("/usr/bin/curl -s --max-time 5 %q", url)

	got := runSandboxed(t, denied, ReadOnly, script)
	if got.ok() {
		t.Fatalf("NetworkDeny permitted an outbound connection: %s", got)
	}

	// Control A: the same sandbox WITHOUT NetworkDeny reaches the server.
	// This is the tighter control — it differs from the run above in exactly
	// one config field, so nothing else can explain the difference.
	allowed := requireEnforcing(t, Config{
		Enabled: true, WorkspaceRoot: ws, Tier: ReadOnly, NetworkDeny: false,
	})
	if ctl := runSandboxed(t, allowed, ReadOnly, script); !ctl.ok() ||
		!strings.Contains(ctl.output, "yanshi-ok") {
		t.Fatalf("sandboxed control (NetworkDeny=false) could not reach the server, "+
			"so the denial above proves nothing: %s", ctl)
	}
	// Control B: unsandboxed.
	if ctl := runUnsandboxed(t, script); !ctl.ok() || !strings.Contains(ctl.output, "yanshi-ok") {
		t.Fatalf("unsandboxed control could not reach the server: %s", ctl)
	}
	t.Logf("denied: %s", got)
}

// TestNetworkDenyStillPermitsUnixSockets pins the carve-out that keeps local
// IPC working.
//
// (deny network*) is not an IP-level switch: measured, it denies AF_UNIX bind
// too, which breaks anything using a local socket for IPC that has nothing to
// do with egress. The carve-out restores that, and the previous test proves it
// did not also restore outbound TCP. Both halves are needed — a carve-out that
// quietly re-enabled IP would be a hole, and no carve-out at all would be a
// usability failure attributed to the wrong cause.
func TestNetworkDenyStillPermitsUnixSockets(t *testing.T) {
	// The socket lives in a SHORT path: sockaddr_un.sun_path is 104 bytes on
	// macOS and a t.TempDir() path blows through it, producing "File name too
	// long" — which looks like a sandbox refusal and is not one.
	dir := shortTempDir(t)
	sb := requireEnforcing(t, Config{
		Enabled: true, WorkspaceRoot: dir, Tier: WorkspaceWrite, NetworkDeny: true,
	})
	sock := filepath.Join(dir, "s")
	// nc -lU / nc -U is present on every macOS. The listener is backgrounded
	// and the client connects to it, so a successful round trip requires both
	// bind and connect on AF_UNIX.
	script := fmt.Sprintf(
		"rm -f %[1]q; /usr/bin/nc -lU %[1]q > %[2]q & sleep 1; echo unix-ok | /usr/bin/nc -U %[1]q; sleep 1; cat %[2]q",
		sock, filepath.Join(dir, "out.txt"))
	got := runSandboxed(t, sb, WorkspaceWrite, script)
	if !strings.Contains(got.output, "unix-ok") {
		t.Fatalf("NetworkDeny broke AF_UNIX sockets, which are not egress: %s", got)
	}
}

// TestNetworkDenyWithProxyPermitsLoopback pins the AllowLoopback carve-out.
//
// bootstrap publishes http_proxy pointing at a loopback listener. A child
// denied all IP cannot reach the one egress path it was given, so the netpolicy
// proxy and the sandbox would be individually correct and jointly useless.
// ProxyURL being set is the signal that turns the carve-out on.
func TestNetworkDenyWithProxyPermitsLoopback(t *testing.T) {
	url := startLoopbackServer(t)
	ws := t.TempDir()
	sb := requireEnforcing(t, Config{
		Enabled: true, WorkspaceRoot: ws, Tier: ReadOnly,
		NetworkDeny: true, ProxyURL: url,
	})
	got := runSandboxed(t, sb, ReadOnly, fmt.Sprintf("/usr/bin/curl -s --max-time 5 %q", url))
	if !got.ok() || !strings.Contains(got.output, "yanshi-ok") {
		t.Fatalf("a configured proxy URL did not re-permit loopback, so the child "+
			"cannot reach its own egress proxy: %s", got)
	}
}

// startLoopbackServer runs an HTTP server on 127.0.0.1 for the lifetime of the
// test and returns its URL. Loopback rather than a public host so the network
// tests are hermetic: an offline runner must still be able to tell a working
// denial from a dead network.
func startLoopbackServer(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, "yanshi-ok")
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})
	return "http://" + ln.Addr().String() + "/"
}

// TestScratchGrantIsDirectoryWide states the honest boundary of the
// WorkspaceWrite tier out loud, as an executable assertion.
//
// The tier grants TMPDIR, /tmp, /var/tmp, ~/Library/Caches and ~/.cache
// WHOLESALE, not per-sandbox. A sandboxed child can therefore write to a file
// in shared scratch that it did not create and that another process owns. That
// is a real limitation with two consequences: a sandboxed child can poison a
// build cache a later unsandboxed build reads back, and two concurrent
// sandboxes are not isolated from each other's temp files.
//
// This test exists so that boundary cannot move without someone noticing — in
// either direction. If a future change narrows the grant to a per-sandbox temp
// directory, this fails and the person making the change updates the
// documented boundary deliberately instead of leaving ScratchPaths' doc comment
// describing a world that no longer exists.
func TestScratchGrantIsDirectoryWide(t *testing.T) {
	ws := t.TempDir()
	// A file in shared scratch that this sandbox did NOT create and that is
	// nowhere near its workspace.
	foreign, err := os.CreateTemp("/tmp", "yanshi-foreign-")
	if err != nil {
		t.Skipf("cannot create a foreign scratch file: %v", err)
	}
	foreignPath := foreign.Name()
	_ = foreign.Close()
	t.Cleanup(func() { _ = os.Remove(foreignPath) })

	sb := requireEnforcing(t, Config{Enabled: true, WorkspaceRoot: ws, Tier: WorkspaceWrite})
	got := runSandboxed(t, sb, WorkspaceWrite, fmt.Sprintf("echo clobbered > %q", foreignPath))
	if !got.ok() {
		t.Fatalf("the documented scratch grant has narrowed: a WorkspaceWrite child could "+
			"not write to shared scratch. That may be an improvement, but ScratchPaths' "+
			"doc comment still describes the wide grant — update it: %s", got)
	}
	data, err := os.ReadFile(foreignPath)
	if err != nil || !strings.Contains(string(data), "clobbered") {
		t.Fatalf("scratch write reported success but did not land: data=%q err=%v", data, err)
	}
	t.Log("confirmed: WorkspaceWrite grants shared scratch directory-wide, " +
		"so a sandboxed child can overwrite another process's temp files")
}

// TestSandboxedToolchainCanBuild is the usability counterpart to every denial
// test above: a sandbox nobody can run a compiler under is not a security
// control, it is an outage.
//
// This is the test the scratch-path list exists for. Run with the workspace
// carve-out alone, `go build` fails at "creating work dir"; add TMPDIR and it
// fails at the build cache; add the cache directory and it builds. Each
// failure was only visible after the previous one was fixed, which is why the
// list could not have been written from first principles.
//
// GOCACHE is redirected INTO the workspace deliberately rather than relying on
// the ~/Library/Caches carve-out: it demonstrates the escape hatch documented
// on ScratchPaths for the per-ecosystem package roots that are intentionally
// not writable, and it keeps the test from depending on the state of the
// developer's real build cache.
func TestSandboxedToolchainCanBuild(t *testing.T) {
	goBin := goToolPath(t)
	ws := t.TempDir()
	src := filepath.Join(ws, "prog")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(src, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module sandboxprobe\n\ngo 1.24\n")
	write("main.go", "package main\n\nimport \"fmt\"\n\nfunc main() { fmt.Println(\"sandboxed-build-ran\") }\n")

	sb := requireEnforcing(t, Config{Enabled: true, WorkspaceRoot: ws, Tier: WorkspaceWrite, NetworkDeny: true})

	out := filepath.Join(src, "prog.bin")
	build := fmt.Sprintf("cd %q && %q build -o %q .", src, goBin, out)
	// GOTOOLCHAIN=local: without it the go command may try to DOWNLOAD a
	// toolchain, which NetworkDeny correctly refuses — the test would then
	// fail for a reason that has nothing to do with the scratch paths.
	env := []string{
		"GOTOOLCHAIN=local",
		"GOCACHE=" + filepath.Join(ws, "gocache"),
		"GOPATH=" + filepath.Join(ws, "gopath"),
		"GOFLAGS=-mod=mod",
	}
	if got := runSandboxed(t, sb, WorkspaceWrite, build, env...); !got.ok() {
		t.Fatalf("a sandboxed toolchain could not build; the scratch-path list is incomplete: %s", got)
	}
	ran := runSandboxed(t, sb, WorkspaceWrite, fmt.Sprintf("%q", out), env...)
	if !ran.ok() || !strings.Contains(ran.output, "sandboxed-build-ran") {
		t.Fatalf("the sandboxed build produced a binary that does not run: %s", ran)
	}
}

// goToolPath locates the go command, skipping when it is absent. GOROOT is
// consulted first because `go env GOROOT` on this host points at a toolchain
// directory distinct from the /usr/local/go shim on PATH, and the shim may
// decide it needs to download a different version.
func goToolPath(t *testing.T) string {
	t.Helper()
	p, err := exec.LookPath("go")
	if err != nil {
		t.Skipf("go tool not on PATH: %v", err)
	}
	return p
}

// TestPathInjectionCannotWidenTheProfile is the process-level attacking test:
// the escaping is not asserted as a string here, it is asserted by the kernel.
//
// # Why the payloads look the way they do
//
// The obvious payload — `ws")) (allow file-write*) ;` — is a real escape when
// interpolated into a rule by hand, and it is what was measured during
// development. It is NOT a useful payload for THIS test, and discovering that
// is the reason this comment exists.
//
// The generator emits its carve-outs as a multi-line list:
//
//	(allow file-write*
//	  (subpath "<workspace>")
//	  (subpath "<scratch1>")
//	  …)
//
// A payload that closes the list and injects a rule leaves the REMAINING
// subpath lines stranded at top level, where they are a syntax error. So an
// unescaped profile built from that payload does not compile at all, the child
// fails to launch, and the test's "the write was refused" assertion passes —
// for entirely the wrong reason. Verified by deliberately removing the
// escaping: this test kept passing while quoteSBPL was a bare concatenation.
//
// The payload used below closes the list, injects a blanket allow, and then
// RE-OPENS a well-formed rule so the stranded subpath lines land inside it.
// The resulting profile compiles, the sandbox starts, and an out-of-workspace
// write succeeds with exit 0. Measured with the escaping removed:
//
//	payload 0: outside=exit=0 output="" victim_exists=true
//
// It uses (literal "…") rather than (subpath "…") for the re-open because a
// directory NAME cannot contain '/', so the payload has no way to spell a path
// separator — a constraint that also rules out the more obvious re-open forms.
// The other payloads are kept as breadth: they exercise the escaping on shapes
// that do not compile, which is worth covering even though a failure there
// would be a launch error rather than an escape.
func TestPathInjectionCannotWidenTheProfile(t *testing.T) {
	root := t.TempDir()
	outside := outsideDir(t)
	payloads := []struct {
		name string
		dir  string
		// escapes marks the payload measured to produce a COMPILING profile
		// with a widened policy when the escaping is removed. For those, a
		// launch failure is not an acceptable pass — see below.
		escapes bool
	}{
		{
			name:    "reopened-rule-keeps-profile-valid",
			dir:     `ws")) (allow file-write*) (allow file-write* (literal "y`,
			escapes: true,
		},
		// Not marked escapes: measured, the (allow default) form is refused by
		// the sandbox even with the escaping removed, so it cannot be used to
		// prove the escaping works. It is kept because it is the payload a
		// reader expects to see and its absence would look like an oversight;
		// the entry above is the one carrying the proof.
		{name: "reopened-allow-default", dir: `ws")) (allow default) (allow file-write* (literal "y`},
		{name: "quote-paren-rule", dir: `ws")) (allow file-write*) ;`},
		{name: "quote-allow-default", dir: `ws") (allow default) ;`},
		{name: "embedded-quote", dir: `ws"quoted`},
		{name: "backslash", dir: `ws\back`},
		{name: "space-and-paren", dir: `ws (paren) dir`},
	}
	for _, p := range payloads {
		t.Run(p.name, func(t *testing.T) {
			ws := filepath.Join(root, p.dir)
			if err := os.MkdirAll(ws, 0o755); err != nil {
				t.Skipf("filesystem rejects this directory name: %v", err)
			}
			sb := requireEnforcing(t, Config{Enabled: true, WorkspaceRoot: ws, Tier: WorkspaceWrite})

			victim := filepath.Join(outside, "pwned-"+p.name)
			got := runSandboxed(t, sb, WorkspaceWrite, fmt.Sprintf("echo pwn > %q", victim))
			if got.ok() {
				t.Fatalf("path injection widened the profile: an out-of-workspace write succeeded: %s", got)
			}
			if _, err := os.Stat(victim); !os.IsNotExist(err) {
				t.Fatalf("%s exists after the injection attempt (stat err=%v)", victim, err)
			}
			if !p.escapes {
				return
			}
			// For the payloads that CAN escape, refusing to launch is not a
			// pass. sandbox-exec exits 64/65 for a profile it cannot parse; if
			// that is what happened, the profile is malformed rather than
			// correctly escaped and this test would go on passing after the
			// escaping was deleted. Insist the sandbox actually ran and
			// refused the write.
			if strings.Contains(got.output, "sandbox-exec:") {
				t.Fatalf("the profile did not compile, so this test is not measuring the "+
					"escaping — it would pass with quoteSBPL removed: %s", got)
			}
			if !strings.Contains(got.output, "Operation not permitted") {
				t.Fatalf("expected a kernel refusal, got something else: %s", got)
			}
			// And the workspace itself must still be writable, which proves
			// the profile is a working WorkspaceWrite profile and not one that
			// denies everything.
			if in := runSandboxed(t, sb, WorkspaceWrite,
				fmt.Sprintf("echo ok > %q", filepath.Join(ws, "ok.txt"))); !in.ok() {
				t.Fatalf("the escaped profile denies its own workspace, so the refusal "+
					"above does not demonstrate a correct boundary: %s", in)
			}
		})
	}
}

// TestSignalScopePermitsOwnChildrenAndRefusesStrangers is the process-level
// pin for the one process rule with a security consequence.
//
// Measured: (target self) does NOT cover the process's own children, so a
// shell under it cannot signal the jobs it started; a bare (allow signal) DOES
// let the sandboxed child kill an unrelated host pid — the "cross-session kill"
// risk the guard's auto-approval prompt names by name. same-sandbox permits the
// first and refuses the second, and this test asserts both directions against
// a real kernel rather than against the profile text.
func TestSignalScopePermitsOwnChildrenAndRefusesStrangers(t *testing.T) {
	ws := t.TempDir()
	sb := requireEnforcing(t, Config{Enabled: true, WorkspaceRoot: ws, Tier: WorkspaceWrite})

	own := runSandboxed(t, sb, WorkspaceWrite,
		"sleep 30 & p=$!; kill -TERM $p && echo killed-own-child; wait 2>/dev/null")
	if !strings.Contains(own.output, "killed-own-child") {
		t.Fatalf("the sandboxed shell cannot signal its own jobs, which breaks every "+
			"timeout and cancel path: %s", own)
	}

	// A stranger: a process this test owns, started outside the sandbox.
	stranger := exec.Command("/bin/sleep", "120")
	if err := stranger.Start(); err != nil {
		t.Fatalf("could not start the stranger process: %v", err)
	}
	defer func() {
		_ = stranger.Process.Kill()
		_, _ = stranger.Process.Wait()
	}()

	attack := runSandboxed(t, sb, WorkspaceWrite,
		fmt.Sprintf("kill -TERM %d && echo KILLED-STRANGER", stranger.Process.Pid))
	if strings.Contains(attack.output, "KILLED-STRANGER") {
		t.Fatalf("the sandboxed child killed an unrelated host process: %s", attack)
	}
	// Confirm at the OS level rather than trusting the shell's report.
	// syscall.Kill with signal 0 probes liveness without delivering anything;
	// os.Process.Signal cannot express it (it rejects a nil Signal), which is
	// why this drops to the syscall directly.
	if err := syscall.Kill(stranger.Process.Pid, 0); err != nil {
		t.Fatalf("the stranger process (pid %d) is gone: %v — the sandboxed child killed it "+
			"despite the shell reporting otherwise", stranger.Process.Pid, err)
	}
	t.Logf("own-children: %s | stranger: %s", own, attack)
}

// TestPrepareRefusesToWrapItself pins the double-wrap guard. Nesting one
// profile inside another is not a privilege escalation (Seatbelt policies
// compose by intersection) but it produces an argv nobody can debug and an
// exit status attributable to the wrong layer, so it must be refused loudly
// rather than produced silently.
func TestPrepareRefusesToWrapItself(t *testing.T) {
	sb := requireEnforcing(t, Config{Enabled: true, WorkspaceRoot: t.TempDir(), Tier: ReadOnly})
	cmd := exec.Command(sandboxExecPath, "-p", "(version 1)(allow default)", "--", "/usr/bin/true")
	err := sb.Prepare(context.Background(), cmd, CommandSpec{Path: sandboxExecPath, Tier: ReadOnly})
	if err == nil {
		t.Fatalf("Prepare wrapped sandbox-exec in itself; argv is now %v", cmd.Args)
	}
	if !strings.Contains(err.Error(), "itself") {
		t.Errorf("unhelpful error for the double-wrap case: %v", err)
	}
}

// TestPrepareRejectsCommandsWithNoProgram pins the two defensive branches.
// Both are reachable from a caller that builds an exec.Cmd by struct literal
// rather than through exec.Command — which is exactly what
// shell.childLaunchPosture.prepare does.
func TestPrepareRejectsCommandsWithNoProgram(t *testing.T) {
	sb := requireEnforcing(t, Config{Enabled: true, WorkspaceRoot: t.TempDir(), Tier: ReadOnly})
	if err := sb.Prepare(context.Background(), nil, CommandSpec{Tier: ReadOnly}); err == nil {
		t.Error("Prepare accepted a nil command")
	}
	if err := sb.Prepare(context.Background(), &exec.Cmd{}, CommandSpec{Tier: ReadOnly}); err == nil {
		t.Error("Prepare accepted a command with no program")
	}
}

// TestPreparePreservesArgumentsVerbatim pins that the argv rewrite does not
// mangle the target's arguments.
//
// Arguments containing spaces, quotes and leading dashes are the ones a naive
// string-concatenation rewrite would destroy, and the destruction would look
// like the tool misbehaving rather than like the sandbox. The `--` separator
// is what keeps a dash-leading program path from being read as a sandbox-exec
// option.
func TestPreparePreservesArgumentsVerbatim(t *testing.T) {
	sb := requireEnforcing(t, Config{Enabled: true, WorkspaceRoot: t.TempDir(), Tier: ReadOnly})
	args := []string{"-n", "a b", `c"d`, "--flag=value", "-"}
	cmd := exec.Command("/bin/echo", args...)
	if err := sb.Prepare(context.Background(), cmd, CommandSpec{Path: "/bin/echo", Args: args, Tier: ReadOnly}); err != nil {
		t.Fatal(err)
	}
	sep := -1
	for i, a := range cmd.Args {
		if a == "--" {
			sep = i
			break
		}
	}
	if sep < 0 {
		t.Fatalf("argv has no -- separator: %v", cmd.Args)
	}
	got := cmd.Args[sep+1:]
	want := append([]string{"/bin/echo"}, args...)
	if len(got) != len(want) {
		t.Fatalf("argv after -- = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("argv[%d] after -- = %q, want %q (full: %v)", i, got[i], want[i], cmd.Args)
		}
	}
	// And it must actually run with those arguments intact.
	res := runCmd(cmd)
	for _, a := range args[1:] {
		if !strings.Contains(res.output, a) {
			t.Errorf("argument %q did not survive the wrap: %s", a, res)
		}
	}
}

// TestExitStatusPropagatesThroughTheWrapper pins that wrapping does not eat the
// child's exit code. sandbox-exec execs the target in place, so the code should
// arrive unchanged — but "should" is why this is a test: a future wrapper that
// forked instead would silently turn every non-zero exit into its own, and the
// tool layer decides whether to escalate based on that number.
func TestExitStatusPropagatesThroughTheWrapper(t *testing.T) {
	sb := requireEnforcing(t, Config{Enabled: true, WorkspaceRoot: t.TempDir(), Tier: FullAccess})
	for _, want := range []int{0, 1, 42} {
		got := runSandboxed(t, sb, FullAccess, fmt.Sprintf("exit %d", want))
		if got.exitCode != want {
			t.Errorf("exit %d came back as %s", want, got)
		}
	}
}

// TestDisabledSandboxDoesNotWrap pins that an operator who turned the sandbox
// off gets an unwrapped command. A disabled sandbox that still wrapped would
// be enforcing a policy nobody asked for, and the report would say "disabled"
// while the argv said otherwise.
func TestDisabledSandboxDoesNotWrap(t *testing.T) {
	sb := New(Config{Enabled: false, WorkspaceRoot: t.TempDir(), Tier: ReadOnly})
	if rep := sb.Report(); rep.Effective != Disabled || rep.Enforced {
		t.Fatalf("disabled sandbox reported %#v", rep)
	}
	cmd := exec.Command("/bin/echo", "hi")
	if err := sb.Prepare(context.Background(), cmd, CommandSpec{Path: "/bin/echo", Tier: ReadOnly}); err != nil {
		t.Fatal(err)
	}
	if cmd.Path != "/bin/echo" {
		t.Fatalf("a disabled sandbox rewrote the command to %q", cmd.Path)
	}
}

// TestProbeRejectsANonEnforcingLauncher is the fault-injection test for the
// self-check, and it is what makes the CapabilityReport trustworthy rather
// than aspirational.
//
// The threat it models is a future /usr/bin/sandbox-exec that accepts -p and
// execs the target without applying anything — the exact shape a deprecated
// tool degrades into. A self-check that only ran a PERMISSIVE profile would
// see success and report OSIsolated on a host with no sandbox at all.
//
// It calls the REAL probeLauncher with a stand-in path rather than
// reimplementing the two probes, and that distinction was earned the hard way.
// An earlier version of this test ran its own parameterised copy of the logic;
// a mutation probe that deleted the deny-default assertion from the production
// function left the entire suite green, because the test was asserting its own
// copy. The seam is why deleting that assertion now fails here.
func TestProbeRejectsANonEnforcingLauncher(t *testing.T) {
	dir := t.TempDir()

	// A pass-through launcher: consumes -p <profile> -- and execs the rest,
	// enforcing nothing. This is what a stubbed-out sandbox-exec looks like.
	passthrough := filepath.Join(dir, "passthrough")
	script := "#!/bin/sh\nwhile [ \"$1\" != \"--\" ] && [ $# -gt 0 ]; do shift; done\nshift\nexec \"$@\"\n"
	if err := os.WriteFile(passthrough, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	reason, ok := probeLauncher(passthrough)
	if ok {
		t.Fatal("probeLauncher accepted a launcher that enforces nothing; " +
			"the backend would report OSIsolated on a host with no sandbox")
	}
	if !strings.Contains(reason, "not enforcing") {
		t.Errorf("reason does not name the enforcement failure: %q", reason)
	}

	// A launcher that is present but rejects every profile: the SBPL-dialect-
	// changed case. It must be reported as a rejection, not as "not enforcing".
	rejecting := filepath.Join(dir, "rejecting")
	if err := os.WriteFile(rejecting, []byte("#!/bin/sh\nexit 65\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	reason, ok = probeLauncher(rejecting)
	if ok {
		t.Fatal("probeLauncher accepted a launcher that rejects every profile")
	}
	if !strings.Contains(reason, "rejected a permissive profile") {
		t.Errorf("reason misattributes a dialect failure: %q", reason)
	}

	// An absent launcher.
	reason, ok = probeLauncher(filepath.Join(dir, "does-not-exist"))
	if ok {
		t.Fatal("probeLauncher accepted a launcher that does not exist")
	}
	if !strings.Contains(reason, "not present") {
		t.Errorf("reason does not name the missing binary: %q", reason)
	}

	// And the real launcher must PASS, so the three refusals above are about
	// the stand-ins and not about probeLauncher refusing everything.
	if _, err := os.Stat(sandboxExecPath); err != nil {
		t.Skipf("%s absent: %v", sandboxExecPath, err)
	}
	if reason, ok := probeLauncher(sandboxExecPath); !ok {
		t.Fatalf("the real launcher failed the self-check: %s", reason)
	}
}

// TestDegradedBackendReportsHonestlyAndDoesNotWrap pins the behaviour on a
// host where the self-check fails.
//
// This is the branch that must never over-claim. It is exercised by
// constructing the backend directly with a failing probe result rather than by
// waiting for a macOS that has removed sandbox-exec — the whole point of the
// degradation path is that it is untestable on a healthy host unless it is
// reachable deliberately.
func TestDegradedBackendReportsHonestlyAndDoesNotWrap(t *testing.T) {
	dir := t.TempDir()
	passthrough := filepath.Join(dir, "passthrough")
	script := "#!/bin/sh\nwhile [ \"$1\" != \"--\" ] && [ $# -gt 0 ]; do shift; done\nshift\nexec \"$@\"\n"
	if err := os.WriteFile(passthrough, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	reason, ok := probeLauncher(passthrough)
	if ok {
		t.Fatal("the stand-in launcher was accepted; this test cannot build a degraded backend")
	}
	sb := &seatbelt{
		cfg: Config{Enabled: true, WorkspaceRoot: dir, Tier: WorkspaceWrite},
		report: CapabilityReport{
			Platform: "darwin", Requested: WorkspaceWrite,
			Effective: DegradedHostGuard, Backend: "seatbelt-unavailable",
			Reason: reason, Enforced: false,
		},
	}
	rep := sb.Report()
	if rep.Effective == OSIsolated || rep.Enforced {
		t.Fatalf("a degraded backend claimed isolation: %#v", rep)
	}
	if rep.Reason == "" {
		t.Fatal("a degraded backend must say why")
	}
	// Prepare must leave the command alone rather than failing every spawn.
	cmd := exec.Command("/bin/echo", "hi")
	if err := sb.Prepare(context.Background(), cmd, CommandSpec{Path: "/bin/echo", Tier: ReadOnly}); err != nil {
		t.Fatalf("a degraded backend must keep the host-guard path usable: %v", err)
	}
	if cmd.Path != "/bin/echo" {
		t.Fatalf("a degraded backend wrapped the command anyway: %q", cmd.Path)
	}
	// And it must still run, so "degraded" is a downgrade and not an outage.
	if res := runCmd(cmd); !res.ok() || !strings.Contains(res.output, "hi") {
		t.Fatalf("the unwrapped command did not run: %s", res)
	}
}

// TestFullAccessWithoutNetworkDenyDisclosesThatItRestrictsNothing is the
// over-claim guard for the one configuration where "the sandbox is working"
// and "the sandbox denies nothing" are the same state.
//
// At tier=full-access with network_deny=false the rendered profile is exactly
// `(version 1)(allow default)`. The mechanism is still live — the spawn is
// still wrapped, and a per-invocation ReadOnly spec still gets a real
// (deny default) profile, which is why the backend keeps reporting
// Effective=os-isolated rather than degrading. Degrading would be a lie in the
// other direction: it would tell tools.SandboxEnforcing that no denial can be
// a sandbox denial, and at the lower tiers those denials are real.
//
// But an operator reading "os-isolated" would reasonably believe SOMETHING is
// restricted, so the Reason has to say otherwise. Reason is the field
// bootstrap logs and the doctor row prints.
//
// The disclosure is verified against OBSERVED BEHAVIOUR, not against another
// copy of the same belief: the test runs a child at the configured tier and
// confirms it really can write outside the workspace.
func TestFullAccessWithoutNetworkDenyDisclosesThatItRestrictsNothing(t *testing.T) {
	ws := t.TempDir()
	sb := requireEnforcing(t, Config{
		Enabled: true, WorkspaceRoot: ws, Tier: FullAccess, NetworkDeny: false,
	})
	rep := sb.Report()

	if !strings.Contains(rep.Reason, "restricts nothing") {
		t.Errorf("the report claims os-isolated for a profile that is literally "+
			"(allow default), with no disclosure in the Reason. An operator — and the "+
			"doctor row that prints this — would read it as isolation that is not "+
			"happening.\nReason=%q", rep.Reason)
	}

	// Behavioural confirmation. If this ever starts failing, the disclosure has
	// itself become the false statement and must be removed rather than kept.
	outside := filepath.Join(outsideDir(t), "outside-full-access.txt")
	res := runSandboxed(t, sb, FullAccess, "echo written > "+outside)
	if !res.ok() {
		t.Fatalf("the full-access tier refused a write outside the workspace (%s); "+
			"the Reason's 'restricts nothing' disclosure is now false", res)
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatalf("full-access write reported success but produced no file: %v", err)
	}

	// The disclosure must NOT appear when the same tier really does restrict
	// something — a note that shows up unconditionally is noise operators learn
	// to skip, and it would stop distinguishing the dangerous configuration.
	for _, tc := range []struct {
		name string
		cfg  Config
	}{
		{"full-access with network denial",
			Config{Enabled: true, WorkspaceRoot: ws, Tier: FullAccess, NetworkDeny: true}},
		{"workspace-write renders (deny default)",
			Config{Enabled: true, WorkspaceRoot: ws, Tier: WorkspaceWrite, NetworkDeny: false}},
		{"read-only renders (deny default)",
			Config{Enabled: true, WorkspaceRoot: ws, Tier: ReadOnly, NetworkDeny: false}},
	} {
		if reason := New(tc.cfg).Report().Reason; strings.Contains(reason, "restricts nothing") {
			t.Errorf("%s: the vacuity note must not appear here: %q", tc.name, reason)
		}
	}
}
