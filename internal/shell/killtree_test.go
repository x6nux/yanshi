package shell

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestKillTakesTheWholeProcessTree is the cancellation clause.
//
// Capabilities() has advertised CanKillTree on Unix since the capability bit
// was written, while the comment beside it said the syscall wiring was still
// pending — so the promise was live and the behaviour was not. Process.Kill
// signals the direct child only: `sh -c 'sleep 300 & ...'` leaves the
// grandchild running with its parent reaped, which the caller reads as a clean
// cancellation while the work carries on holding whatever it held.
//
// The grandchild writes its pid to a file before sleeping, so the test can
// check for it by pid rather than by scraping process listings.
//
// ledger: A1/T07/T08#4 进程树取消干净
func TestKillTakesTheWholeProcessTree(t *testing.T) {
	if !CanKillTreeOnPlatform() {
		t.Skipf("%s does not promise tree kills", runtime.GOOS)
	}

	dir := t.TempDir()
	pidFile := filepath.Join(dir, "grandchild.pid")

	// The shell starts a background sleep (the grandchild), records its pid,
	// then waits — so killing only the shell leaves the sleep behind.
	script := "sleep 300 & echo $! > " + pidFile + "; wait"

	proc, _, err := OSProcessFactory{}.Start(context.Background(), LaunchSpec{
		Program: "sh", Args: []string{"-c", script}, Dir: dir,
	})
	if err != nil {
		t.Fatal(err)
	}

	grandPID := waitForPID(t, pidFile)
	if !processAlive(grandPID) {
		t.Fatalf("the grandchild (pid %d) was never running; the fixture proves nothing", grandPID)
	}

	if err := proc.Kill(); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	_ = proc.Wait()

	// Give the signal a moment to be delivered and reaped.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !processAlive(grandPID) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	// Do not leak it into the rest of the suite.
	_ = syscallKill(grandPID)
	t.Errorf("the grandchild (pid %d) survived Kill: Capabilities reports CanKillTree, "+
		"so a caller that cancelled this command believes the work stopped", grandPID)
}

// TestContextCancelAlsoTakesTheTree covers the other cancellation path.
//
// exec.CommandContext installs its own cancel func, which calls Process.Kill
// directly. Without overriding it, a ctx-cancelled command reaps the shell and
// leaves the tree — the same defect through a different door, and the door
// callers are most likely to use.
//
// ledger: A1/T07/T08#4 进程树取消干净
func TestContextCancelAlsoTakesTheTree(t *testing.T) {
	if !CanKillTreeOnPlatform() {
		t.Skipf("%s does not promise tree kills", runtime.GOOS)
	}

	dir := t.TempDir()
	pidFile := filepath.Join(dir, "grandchild.pid")
	script := "sleep 300 & echo $! > " + pidFile + "; wait"

	ctx, cancel := context.WithCancel(context.Background())
	proc, _, err := OSProcessFactory{}.Start(ctx, LaunchSpec{
		Program: "sh", Args: []string{"-c", script}, Dir: dir,
	})
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	grandPID := waitForPID(t, pidFile)

	cancel()
	_ = proc.Wait()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !processAlive(grandPID) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	_ = syscallKill(grandPID)
	t.Errorf("the grandchild (pid %d) survived context cancellation", grandPID)
}

func waitForPID(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		b, err := os.ReadFile(path)
		if err == nil {
			if pid, cerr := strconv.Atoi(strings.TrimSpace(string(b))); cerr == nil && pid > 0 {
				return pid
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("the grandchild never wrote its pid to %s", path)
	return 0
}

// processAlive reports whether the pid still names a live process. Signal 0 is
// the standard existence probe; a zombie would also answer yes, which is why
// the tests Wait on the parent first so the shell's children are reparented and
// reaped by init.
func processAlive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(sigZero()) == nil
}

// syscallKill is the cleanup path: best-effort, errors ignored by callers.
func syscallKill(pid int) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return p.Kill()
}

var _ = exec.Command // keep the import stable across build tags
