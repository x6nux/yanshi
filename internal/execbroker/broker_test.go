package execbroker

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// TestMain makes this test binary act as its own shim.
//
// The end-to-end tests below need a real program to sit on a child's PATH under
// the name `sudo`, and the shim is a symlink to the yanshi binary. Substituting
// a hand-written stand-in would test a stand-in; routing the test binary through
// the SAME IsShimInvocation/RunShim pair cmd/yanshi uses means the thing under
// test is the production client half.
func TestMain(m *testing.M) {
	if name, ok := IsShimInvocation(os.Args[0]); ok {
		cwd, _ := os.Getwd()
		if err := RunShim(name, os.Args, cwd); err != nil {
			fmt.Fprintln(os.Stderr, err)
		}
		os.Exit(ShimExitCode)
	}
	os.Exit(m.Run())
}

// recorder is a Decider that captures what it was asked and answers with a
// fixed verdict.
type recorder struct {
	mu     sync.Mutex
	seen   []Request
	deny   string
	answer error
}

func (r *recorder) decide(_ context.Context, req Request) error {
	r.mu.Lock()
	r.seen = append(r.seen, req)
	r.mu.Unlock()
	if r.deny != "" {
		return fmt.Errorf("%s", r.deny)
	}
	return r.answer
}

func (r *recorder) requests() []Request {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Request(nil), r.seen...)
}

// fakeSudoDir writes a harmless stand-in for the real elevation program and
// returns the directory holding it.
//
// ⚠️ This is not decoration. The approval path ends in syscall.Exec of whatever
// `sudo` PATH resolves to, so a test that left the system directories on PATH
// would run the REAL sudo on the machine running the suite. Every PATH built
// below is shimDir + this directory and nothing else, and the script is driven
// through an absolute /bin/sh so it needs no other program.
func fakeSudoDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\necho \"FAKE-SUDO ran: $*\"\n"
	path := filepath.Join(dir, "sudo")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake sudo: %v", err)
	}
	return dir
}

// runScript executes a /bin/sh script under a PATH containing only the shim
// directory and the fake-program directory, and returns its combined output and
// exit code.
func runScript(t *testing.T, server *Server, fakeDir, script string) (string, int) {
	t.Helper()
	cmd := exec.Command("/bin/sh", "-c", script)
	cmd.Dir = t.TempDir()
	cmd.Env = append(server.Env(),
		"PATH="+server.ShimDir()+string(os.PathListSeparator)+fakeDir,
		"HOME="+t.TempDir(),
	)
	out, err := cmd.CombinedOutput()
	code := 0
	var ee *exec.ExitError
	if err != nil {
		if !asExitError(err, &ee) {
			t.Fatalf("running the script failed for a reason other than its exit status: %v\n%s", err, out)
		}
		code = ee.ExitCode()
	}
	return string(out), code
}

func asExitError(err error, target **exec.ExitError) bool {
	ee, ok := err.(*exec.ExitError)
	if ok {
		*target = ee
	}
	return ok
}

// newTestServer starts a broker whose shims point at this test binary.
func newTestServer(t *testing.T, dec Decider) *Server {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	exe, err = filepath.Abs(exe)
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	server, err := Listen(ctx, exe, dec)
	if err != nil {
		cancel()
		t.Skipf("broker unavailable on this host: %v", err)
	}
	t.Cleanup(func() {
		cancel()
		_ = server.Close()
	})
	return server
}

// TestNestedElevationIsAdjudicatedWithoutRerunningTheScript is the acceptance
// this package exists for.
//
// The script's third line is the one under test. What has to be true is not
// merely "sudo was refused" — it is that the LINE was refused while the lines
// around it ran exactly once each. A control that worked by killing the script
// and asking about the whole thing again would re-execute line 1, which for a
// build step or a migration is not a retry, it is a second run.
func TestNestedElevationIsAdjudicatedWithoutRerunningTheScript(t *testing.T) {
	rec := &recorder{deny: "profile forbids elevation"}
	server := newTestServer(t, rec.decide)
	fake := fakeSudoDir(t)

	out, code := runScript(t, server, fake, `
echo LINE-1
sudo -n whoami
echo "LINE-3 saw exit $?"
`)
	if strings.Count(out, "LINE-1") != 1 {
		t.Errorf("line 1 ran %d times, want exactly 1:\n%s", strings.Count(out, "LINE-1"), out)
	}
	if !strings.Contains(out, fmt.Sprintf("LINE-3 saw exit %d", ShimExitCode)) {
		t.Errorf("the script did not continue past the refusal with the shim's exit code:\n%s", out)
	}
	if strings.Contains(out, "FAKE-SUDO ran") {
		t.Errorf("the real program was executed despite the denial:\n%s", out)
	}
	if !strings.Contains(out, "profile forbids elevation") {
		t.Errorf("the denial reason never reached the child's output:\n%s", out)
	}
	if code != 0 {
		t.Errorf("the script itself should exit 0 (its last command succeeded), got %d:\n%s", code, out)
	}

	reqs := rec.requests()
	if len(reqs) != 1 {
		t.Fatalf("the broker was asked %d times, want exactly 1: %+v", len(reqs), reqs)
	}
	if reqs[0].Program != "sudo" {
		t.Errorf("program = %q, want sudo", reqs[0].Program)
	}
	if strings.Join(reqs[0].Args, " ") != "-n whoami" {
		t.Errorf("args = %v, want [-n whoami]", reqs[0].Args)
	}
	if reqs[0].Dir == "" {
		t.Error("the request carried no working directory, so the approval dialog cannot show one")
	}
	if reqs[0].Token != "" {
		t.Error("the token must be cleared before the Decider sees it: it is a shared secret, not a field to log")
	}
}

// TestApprovedElevationExecsTheRealProgram pins the other direction.
//
// Without it, a broker that denied everything would pass every assertion above
// — and "the control is on" and "the control is a wall" are different products.
func TestApprovedElevationExecsTheRealProgram(t *testing.T) {
	rec := &recorder{}
	server := newTestServer(t, rec.decide)
	fake := fakeSudoDir(t)

	out, code := runScript(t, server, fake, `
echo LINE-1
sudo -n whoami
echo "LINE-3 saw exit $?"
`)
	if !strings.Contains(out, "FAKE-SUDO ran: -n whoami") {
		t.Errorf("the approved program did not run, or ran with mangled arguments:\n%s", out)
	}
	if !strings.Contains(out, "LINE-3 saw exit 0") {
		t.Errorf("the exit code the script observed is the shim's, not the program's:\n%s", out)
	}
	if code != 0 {
		t.Errorf("script exit = %d:\n%s", code, out)
	}
}

// TestShimFailsClosedWithoutABroker pins the branch an attacker reaches by
// removing an environment variable.
//
// It is the single most important assertion in this file: every other property
// is about a broker that answers, and this one is about the case where none
// does. A shim that ran the program when it could not ask would make the whole
// control opt-out.
func TestShimFailsClosedWithoutABroker(t *testing.T) {
	rec := &recorder{}
	server := newTestServer(t, rec.decide)
	fake := fakeSudoDir(t)

	// Same shim, same PATH, but the broker coordinates stripped — exactly what
	// `env -u YANSHI_EXEC_BROKER sudo …` inside a script would produce.
	cmd := exec.Command("/bin/sh", "-c", "sudo -n whoami; echo \"exit $?\"")
	cmd.Dir = t.TempDir()
	cmd.Env = []string{
		"PATH=" + server.ShimDir() + string(os.PathListSeparator) + fake,
		"HOME=" + t.TempDir(),
	}
	out, _ := cmd.CombinedOutput()
	text := string(out)
	if strings.Contains(text, "FAKE-SUDO ran") {
		t.Fatalf("the shim ran the program with no broker to approve it:\n%s", text)
	}
	if !strings.Contains(text, fmt.Sprintf("exit %d", ShimExitCode)) {
		t.Errorf("expected the shim's refusal exit code:\n%s", text)
	}
	if !strings.Contains(text, SocketEnv) {
		t.Errorf("the refusal must name what was missing so an operator can act:\n%s", text)
	}
	if len(rec.requests()) != 0 {
		t.Error("the broker was contacted despite the environment being stripped")
	}
}

// TestShimFailsClosedOnABadToken pins the second half of the same property: a
// shim that reaches SOMETHING is not a shim that reached the right thing.
func TestShimFailsClosedOnABadToken(t *testing.T) {
	rec := &recorder{}
	server := newTestServer(t, rec.decide)
	fake := fakeSudoDir(t)

	env := server.Env()
	for i, entry := range env {
		if strings.HasPrefix(entry, TokenEnv+"=") {
			env[i] = TokenEnv + "=0000000000000000"
		}
	}
	cmd := exec.Command("/bin/sh", "-c", "sudo -n whoami; echo \"exit $?\"")
	cmd.Dir = t.TempDir()
	cmd.Env = append(env,
		"PATH="+server.ShimDir()+string(os.PathListSeparator)+fake,
		"HOME="+t.TempDir())
	out, _ := cmd.CombinedOutput()
	text := string(out)
	if strings.Contains(text, "FAKE-SUDO ran") {
		t.Fatalf("a forged token was accepted:\n%s", text)
	}
	if !strings.Contains(text, fmt.Sprintf("exit %d", ShimExitCode)) {
		t.Errorf("expected the shim's refusal exit code:\n%s", text)
	}
	if len(rec.requests()) != 0 {
		t.Error("the Decider ran for a request that failed authentication")
	}
}

// TestShimFailsClosedWhenTheBrokerIsGone covers the parent dying mid-script,
// which is the state that matters most: the child is still running and there is
// nobody left to ask.
//
// It simulates the death by taking the LISTENER and the SOCKET away while
// leaving the shims in place, because that is what a killed process leaves
// behind — the kernel closes its sockets, and nothing cleans its temp
// directory. The full Close is a different event with a different outcome, and
// TestRemovingTheShimsEndsInterception below records that one rather than
// letting this test quietly cover both.
func TestShimFailsClosedWhenTheBrokerIsGone(t *testing.T) {
	rec := &recorder{}
	server := newTestServer(t, rec.decide)
	fake := fakeSudoDir(t)
	env := append(server.Env(),
		"PATH="+server.ShimDir()+string(os.PathListSeparator)+fake,
		"HOME="+t.TempDir())

	// Closing the listener is enough: Go unlinks a unix socket when its
	// listener closes, which is the same thing the kernel effectively leaves
	// behind when the owning process dies.
	_ = server.listener.Close()
	if _, err := os.Stat(filepath.Join(server.ShimDir(), "s")); !os.IsNotExist(err) {
		t.Fatalf("the socket outlived its listener (%v); this test is not simulating a dead parent", err)
	}

	cmd := exec.Command("/bin/sh", "-c", "sudo -n whoami; echo \"exit $?\"")
	cmd.Dir = t.TempDir()
	cmd.Env = env
	out, _ := cmd.CombinedOutput()
	if strings.Contains(string(out), "FAKE-SUDO ran") {
		t.Fatalf("the program ran after the broker went away:\n%s", out)
	}
	if !strings.Contains(string(out), fmt.Sprintf("exit %d", ShimExitCode)) {
		t.Errorf("expected the shim's refusal exit code:\n%s", out)
	}
	if len(rec.requests()) != 0 {
		t.Error("a request reached a closed broker")
	}
}

// TestRemovingTheShimsEndsInterception records the boundary of this control,
// as a measured fact rather than as a sentence in a doc comment.
//
// Once the shim directory is gone, the child's PATH falls through to the next
// entry and finds the real program. That is NOT a fail-closed outcome, and
// pretending otherwise would be the more dangerous mistake: an operator who
// believed the shims survived teardown would draw the wrong conclusion about an
// orphaned background job.
//
// It is bounded rather than fixed, and the bound is the reason it is acceptable:
// Close runs when the launched process is reaped, so reaching this state means
// a process outlived the launch that started it. Such a process gets the
// behaviour every child had before this package existed. If this test ever
// starts failing because the shims survive Close, that is a change worth
// noticing, not a bug to silence.
func TestRemovingTheShimsEndsInterception(t *testing.T) {
	rec := &recorder{deny: "would be denied if asked"}
	server := newTestServer(t, rec.decide)
	fake := fakeSudoDir(t)
	env := append(server.Env(),
		"PATH="+server.ShimDir()+string(os.PathListSeparator)+fake,
		"HOME="+t.TempDir())
	if err := server.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	cmd := exec.Command("/bin/sh", "-c", "sudo -n whoami; echo \"exit $?\"")
	cmd.Dir = t.TempDir()
	cmd.Env = env
	out, _ := cmd.CombinedOutput()
	if !strings.Contains(string(out), "FAKE-SUDO ran") {
		t.Fatalf("the documented residue changed: after Close the child no longer "+
			"reaches the real program.\nIf that is deliberate, update this test and "+
			"Server.Close's comment together:\n%s", out)
	}
	if len(rec.requests()) != 0 {
		t.Error("the closed broker was still answering")
	}
}

// TestCloseRemovesTheShimDirectory pins that a launch does not leave symlinks
// named `sudo` lying around in the temp directory forever.
func TestCloseRemovesTheShimDirectory(t *testing.T) {
	rec := &recorder{}
	server := newTestServer(t, rec.decide)
	dir := server.ShimDir()
	if _, err := os.Lstat(filepath.Join(dir, "sudo")); err != nil {
		t.Fatalf("the sudo shim was never created: %v", err)
	}
	if err := server.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("shim directory survived Close: %v", err)
	}
	// Idempotent: the ctx watchdog calls this too.
	_ = server.Close()
}

// TestListenRefusesANilDecider pins the fail-closed constructor. A nil Decider
// would make every intercepted elevation succeed, which is strictly worse than
// no interception because the report would say it is on.
func TestListenRefusesANilDecider(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	if _, err := Listen(context.Background(), exe, nil); err == nil {
		t.Fatal("Listen accepted a nil Decider")
	}
	if _, err := Listen(context.Background(), "relative/path", func(context.Context, Request) error {
		return nil
	}); err == nil {
		t.Fatal("Listen accepted a relative exe path, which decides which binary answers as sudo")
	}
}
