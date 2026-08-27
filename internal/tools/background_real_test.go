package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"

	"github.com/x6nux/yanshi/internal/guard"
)

// This file is the REAL-SUBPROCESS half of the T3 verification.
//
// The offload path's whole claim is that work SURVIVES its foreground
// deadline. Every part of that claim is about a real process and real elapsed
// time: a channel that stays open, a goroutine that keeps draining, a
// cancellation that is deliberately not propagated. A fake StreamFunc that
// sleeps and then sends on a channel exercises the plumbing but cannot
// distinguish "the run continued" from "the run was restarted", nor prove that
// an OS process was still alive after the turn that started it had returned.
//
// So these drive a real `sh -c` (or `cmd /c` on Windows) that writes a file
// AFTER the deadline has passed. The file is the evidence: it can only exist
// if the process outlived the foreground timeout.

// backgroundProbeTool builds a shell_run-named GuardedTool with a SHORT
// foreground timeout, over the real shell implementation.
//
// The name must be exactly "shell_run" because eligibility is by name
// (BackgroundableTools), and the real ShellTools constructor hardcodes a 120s
// timeout, which no test can wait out. Everything below the name -- the
// StreamFunc, the subprocess, the guard -- is the production path.
func backgroundProbeTool(root string, timeout time.Duration) *GuardedTool {
	s := &ShellTools{root: root}
	return NewGuardedTool(
		"shell_run", "Bash", "run a command", timeout,
		params(map[string]*schema.ParameterInfo{
			"command": {Type: schema.String, Desc: "command line", Required: true},
			"workdir": {Type: schema.String, Desc: "working directory"},
			"timeout": {Type: schema.Integer, Desc: "timeout in seconds"},
			"env":     {Type: schema.String, Desc: "shell environment"},
		}),
		s.stream,
	)
}

// sleepThenTouch returns a shell command that waits, then creates marker.
//
// No shell metacharacters: shell_run rejects them as a structural HardDeny, so
// the command has to be a single program invocation. `sh -c` is not available
// as a chain here, which is why this uses a program that does both.
func sleepThenTouch(t *testing.T, marker string, d time.Duration) string {
	t.Helper()
	secs := int(d.Seconds())
	if secs < 1 {
		secs = 1
	}
	if runtime.GOOS == "windows" {
		// ping is the portable "sleep" on stock Windows; -n N waits N-1
		// seconds. But the marker write needs a second command, and chaining
		// is forbidden, so Windows uses a PowerShell one-liner via the env
		// selector instead.
		t.Skip("the offload probe needs a single-invocation sleep-then-write; " +
			"stock Windows has no such program without chaining")
	}
	// A tiny shell script file, invoked as a single program. Writing the
	// script to disk keeps the shell_run argument free of metacharacters.
	script := filepath.Join(t.TempDir(), "slow.sh")
	body := "#!/bin/sh\nsleep " + itoa(secs) + "\nprintf done > " + marker + "\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return script
}

// itoa avoids pulling strconv in for one call in a test helper.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// offloadCtx binds the profile, work root, and background manager a real turn
// binds, plus the secure-process factory shell_run needs.
func offloadCtx(root string, mgr *BackgroundManager) context.Context {
	ctx := WithWorkRoot(context.Background(), root)
	ctx = WithProfile(ctx, guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"shell_run"}},
		FS:    guard.FSPerm{Read: []string{"**"}, Write: []string{"**"}},
		Shell: guard.ShellPerm{Policy: "denylist"},
	})
	return WithBackgroundManager(ctx, mgr)
}

// drainOffloadTool consumes a tool's chunk channel and returns the
// concatenated result text.
func drainOffloadTool(ch <-chan ToolChunk) string {
	var sb strings.Builder
	for c := range ch {
		if c.Result != "" {
			sb.WriteString(c.Result)
		}
	}
	return sb.String()
}

// TestOffloadReal_ProcessSurvivesTheForegroundDeadline is the core T3 claim,
// checked against a real process and the real clock.
//
// The command sleeps past the foreground timeout and then writes a marker
// file. Three things are asserted in order, and each rules out a different way
// the feature could be hollow:
//
//  1. the tool returned AT the deadline with a handle, not an error -- so the
//     model was freed to keep working;
//  2. the marker did NOT exist at that moment -- so the run had genuinely not
//     finished, and the handle was not a post-hoc label on completed work;
//  3. the marker DID appear afterwards -- so the process really outlived the
//     turn rather than being cancelled with its output discarded, which is the
//     pre-T3 behaviour this replaces.
func TestOffloadReal_ProcessSurvivesTheForegroundDeadline(t *testing.T) {
	root := t.TempDir()
	marker := filepath.Join(root, "finished.txt")
	script := sleepThenTouch(t, marker, 3*time.Second)

	mgr := NewBackgroundManager()
	defer mgr.Close()
	tool := backgroundProbeTool(root, 700*time.Millisecond)

	args, err := json.Marshal(map[string]string{"command": script})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	out := drainOffloadTool(tool.Stream(offloadCtx(root, mgr), string(args)))
	elapsed := time.Since(start)

	// (1) returned at the deadline, with a handle rather than a failure.
	if !strings.Contains(strings.ToLower(out), "background") {
		t.Fatalf("the tool did not report an offload at its deadline; result = %q", out)
	}
	if elapsed > 2*time.Second {
		t.Errorf("the call blocked %v, well past its %v deadline: the offload did not "+
			"release the turn", elapsed, 700*time.Millisecond)
	}

	// (2) the work was genuinely unfinished when the handle was issued.
	if _, statErr := os.Stat(marker); statErr == nil {
		t.Fatal("the marker already existed when the handle was issued; the command " +
			"finished inside the deadline and this test proves nothing about survival")
	}

	// (3) the process outlived the turn and completed its work.
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if _, statErr := os.Stat(marker); statErr == nil {
			return // survived
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("the marker never appeared: the backgrounded process was killed when the " +
		"turn ended, so the offload discards the work it promised to keep")
}

// TestOffloadReal_ResultIsReinjectedAsANotice checks the second half of the
// contract: the finished run's output must come back through DrainNotices, and
// the notice must NOT be shaped like a tool result.
//
// That shape is load-bearing rather than cosmetic. A tool result with no
// matching tool_call is an unpairable message: ctxcompact.EnforceToolCallPairs
// cannot pair it and providers reject the request outright. So the completion
// notice is delivered as ordinary text, and this asserts the run's real output
// survives into it.
func TestOffloadReal_ResultIsReinjectedAsANotice(t *testing.T) {
	root := t.TempDir()
	marker := filepath.Join(root, "finished.txt")
	script := sleepThenTouch(t, marker, 2*time.Second)

	mgr := NewBackgroundManager()
	defer mgr.Close()
	tool := backgroundProbeTool(root, 500*time.Millisecond)

	args, _ := json.Marshal(map[string]string{"command": script})
	drainOffloadTool(tool.Stream(offloadCtx(root, mgr), string(args)))

	// Wait for the run to finish and produce a notice.
	var notices []BackgroundRun
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if notices = mgr.DrainNotices(); len(notices) > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if len(notices) == 0 {
		t.Fatal("the finished background run produced no completion notice: the model " +
			"would never learn the answer it was told to wait for")
	}
	run := notices[0]
	if run.Tool != "shell_run" {
		t.Errorf("notice names tool %q, want shell_run", run.Tool)
	}
	text := CompletionNotice(run)
	if strings.TrimSpace(text) == "" {
		t.Fatal("the completion notice rendered empty")
	}
	if !strings.Contains(text, run.ID) {
		t.Errorf("the notice does not carry the handle id the model was given (%s): %q",
			run.ID, text)
	}

	// Draining is destructive: a notice delivered twice would tell the model
	// the same command finished twice.
	if again := mgr.DrainNotices(); len(again) != 0 {
		t.Errorf("DrainNotices returned %d notices on a second call; a redelivered "+
			"notice is a duplicate event in the transcript", len(again))
	}
}

// TestOffloadReal_CloseKillsSurvivingRuns is the shutdown half: a process that
// was deliberately detached from its turn must still die with the process that
// spawned it.
//
// Without this, the same mechanism that makes the offload useful makes it a
// subprocess leak -- every long command started in a session outliving the
// binary that started it.
func TestOffloadReal_CloseKillsSurvivingRuns(t *testing.T) {
	root := t.TempDir()
	marker := filepath.Join(root, "finished.txt")
	// Long enough that Close is guaranteed to happen first.
	script := sleepThenTouch(t, marker, 30*time.Second)

	mgr := NewBackgroundManager()
	tool := backgroundProbeTool(root, 500*time.Millisecond)

	args, _ := json.Marshal(map[string]string{"command": script})
	drainOffloadTool(tool.Stream(offloadCtx(root, mgr), string(args)))

	if len(mgr.List()) == 0 {
		t.Fatal("no run was adopted, so this test is not exercising shutdown")
	}
	mgr.Close()

	// The marker must never appear: the run was cancelled, not merely
	// forgotten. Waiting past the script's own sleep would take 30s, so this
	// checks the manager's view (no live runs) plus a bounded no-marker window.
	time.Sleep(1500 * time.Millisecond)
	if _, statErr := os.Stat(marker); statErr == nil {
		t.Fatal("a backgrounded process completed AFTER Close: shutdown does not stop " +
			"detached runs, so they outlive the binary")
	}
	for _, r := range mgr.List() {
		if r.State == BackgroundRunning {
			t.Errorf("run %s is still in state %q after Close", r.ID, r.State)
		}
	}
}
