// Package execprobe runs a command (typically "tool --version") with a hard
// timeout that survives both a process that won't exit AND a CreateProcess
// syscall that itself hangs. The latter occurs on Windows with App Execution
// Aliases — python3.exe / python.exe stubs under %LOCALAPPDATA%\...\WindowsApps\
// that, when invoked non-interactively, block inside CreateProcess trying to
// redirect to the Microsoft Store. A context deadline alone cannot kill what
// hasn't been created yet, so execprobe also detaches the call after the
// deadline, abandoning the (possibly stuck) goroutine so callers never block.
//
// Both buildEnvInfo (orchestrator startup → system instruction) and
// buildStartupInfo (TUI banner) funnel through Run, so a single stalled probe
// can't hang process boot.
package execprobe

import (
	"context"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// deadline per probe. Generous for any legitimately slow command (dotnet
// first-run, AV scan), yet with ~14 probes the worst case stays bounded. Most
// resolve in milliseconds; only truly stalled ones hit the cap.
const deadline = 3 * time.Second

// Run executes cmd with args and returns the first line of its combined output,
// trimmed. Returns "" on any error, on timeout, or if the probe is abandoned
// (CreateProcess hung). A missed probe is non-fatal: callers only surface tool
// availability to the model / banner — the model can still discover tools via
// shell_run.
//
// The goroutine+select structure is what makes the timeout reliable: if the
// result arrives we use it; otherwise ctx.Done() fires at the deadline and we
// return "" without waiting for CombinedOutput. The leaked goroutine resolves
// eventually (or not — the OS reclaims it at exit) and holds no locks.
func Run(cmd string, args ...string) string {
	// Short-circuit Windows App Execution Aliases (see isWindowsAppExecutionAlias):
	// they block for the full deadline without this check, and never yield version
	// output anyway. LookPath resolves without executing, so this is cheap.
	if isWindowsAppExecutionAlias(cmd) {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()
	c := exec.CommandContext(ctx, cmd, args...)

	type result struct {
		out []byte
		err error
	}
	ch := make(chan result, 1)
	go func() {
		out, err := c.CombinedOutput()
		ch <- result{out, err}
	}()

	select {
	case r := <-ch:
		if r.err != nil || ctx.Err() == context.DeadlineExceeded {
			return ""
		}
		lines := strings.SplitN(string(r.out), "\n", 2)
		return strings.TrimSpace(lines[0])
	case <-ctx.Done():
		return ""
	}
}

// isWindowsAppExecutionAlias reports whether cmd resolves to a Windows App
// Execution Alias — a 0-byte reparse point under %LOCALAPPDATA%\...\
// WindowsApps\ (python3.exe, python.exe, and friends). These aliases redirect
// to the Microsoft Store: invoked non-interactively they block for the full
// deadline trying to open the Store UI, and never return version output, so
// probing them is pure latency with no upside. exec.LookPath resolves the path
// WITHOUT executing the target, so this check is fast and sidesteps the hang.
// Returns false off Windows and when cmd isn't on PATH at all.
func isWindowsAppExecutionAlias(cmd string) bool {
	if runtime.GOOS != "windows" {
		return false
	}
	p, err := exec.LookPath(cmd)
	if err != nil {
		return false
	}
	return strings.Contains(strings.ToLower(p), `\windowsapps\`)
}
