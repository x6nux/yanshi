package tools

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/x6nux/yanshi/internal/guard"
	"github.com/x6nux/yanshi/internal/shell"
)

// L2 (v3): the factory MUST return non-nil Process/Console. Manager.Start
// (Task 17) immediately calls proc.PID()/console.PTY() and spawns the pump
// goroutine — a nil process panics there. These fakes mirror the shape of
// Task 17's fakeProcess/fakeConsole (kept here because this test lives in
// package tools, not shell, so it cannot reference the shell-internal fakes).
type fakeShellProcess struct{}

func (fakeShellProcess) Wait() error { return nil }
func (fakeShellProcess) PID() int    { return 1 }
func (fakeShellProcess) Kill() error { return nil }
func (fakeShellProcess) Capabilities() shell.ProcessCapabilities {
	return shell.ProcessCapabilities{CanKillTree: false}
}

type fakeShellConsole struct{}

func (fakeShellConsole) Read([]byte) (int, error)    { return 0, io.EOF }
func (fakeShellConsole) Write(p []byte) (int, error) { return len(p), nil }
func (fakeShellConsole) Close() error                { return nil }
func (fakeShellConsole) Resize(uint16, uint16) error { return nil }
func (fakeShellConsole) PTY() bool                   { return false }

type fakeShellFactory struct{}

func (fakeShellFactory) Start(context.Context, shell.LaunchSpec) (shell.Process, shell.Console, error) {
	return fakeShellProcess{}, fakeShellConsole{}, nil
}

// recordingShellFactory captures the LaunchSpec the manager hands to the
// launcher so a test can assert on the effective working directory without
// spawning a real process.
type recordingShellFactory struct{ specs []shell.LaunchSpec }

func (f *recordingShellFactory) Start(_ context.Context, spec shell.LaunchSpec) (shell.Process, shell.Console, error) {
	f.specs = append(f.specs, spec)
	return fakeShellProcess{}, fakeShellConsole{}, nil
}

// TestShellV2StartFeedsWorkdirToGuard pins the destructive-deletion baseline.
// guard.checkDestructive classifies "rm -rf <abs path>" against Action.Workdir;
// when Workdir is empty it fails safe and treats every absolute path as
// out-of-scope, so a v2 shell that forgets Workdir degrades into prompting on
// every absolute deletion AND loses the "deleting the work root itself"
// judgement entirely. The PermissionRequest is the existing observation seam:
// it mirrors Action.Workdir verbatim (permctx.go). NOTE both WithProfile and
// WithPermissionCallback are required — Authorize returns DenyErr before ever
// consulting the callback when no profile is bound.
func TestShellV2StartFeedsWorkdirToGuard(t *testing.T) {
	root := t.TempDir()
	manager := shell.NewManager(shell.Config{Root: root, MaxOutputBytes: 256, Factory: fakeShellFactory{}})
	defer func() { _ = manager.Close() }()
	var got PermissionRequest
	ctx := WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"shell_start"}},
		// Empty policy + empty patterns => guard.Check returns Prompt, which is
		// what routes the action through the interactive callback.
		Shell: guard.ShellPerm{},
	})
	ctx = WithPermissionCallback(ctx, func(req PermissionRequest) PermissionDecision {
		got = req
		return PermissionDeny
	})
	ctx = WithShellManager(ctx, manager)
	v := NewShellV2Tools(root)
	_, _ = runTool(ctx, v.Start, `{"command":"rm -rf /tmp/xyz"}`)
	if got.Tool != "shell_start" {
		t.Fatalf("permission callback was not consulted: %+v", got)
	}
	if got.Workdir != root {
		t.Fatalf("guard action Workdir = %q, want %q", got.Workdir, root)
	}
}

// TestShellV2StartDefaultsDirToRoot mirrors legacy shell_run: an omitted
// "workdir" arg must run the process at the work root, not at the server's
// process cwd (which is arbitrary and outside the sandboxed tree).
func TestShellV2StartDefaultsDirToRoot(t *testing.T) {
	root := t.TempDir()
	factory := &recordingShellFactory{}
	manager := shell.NewManager(shell.Config{Root: root, MaxOutputBytes: 256, Factory: factory})
	defer func() { _ = manager.Close() }()
	ctx := WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"shell_start", "task_shell_start"}},
		Shell: guard.ShellPerm{Policy: "allowlist", Patterns: []string{"echo *"}},
	})
	ctx = WithShellManager(ctx, manager)
	v := NewShellV2Tools(root)
	if _, err := runTool(ctx, v.Start, `{"command":"echo hi"}`); err != nil {
		t.Fatal(err)
	}
	if _, err := runTool(ctx, v.TaskStart, `{"command":"echo hi"}`); err != nil {
		t.Fatal(err)
	}
	if len(factory.specs) != 2 {
		t.Fatalf("factory saw %d launches, want 2", len(factory.specs))
	}
	for i, spec := range factory.specs {
		if spec.Dir != root {
			t.Fatalf("launch %d Dir = %q, want work root %q", i, spec.Dir, root)
		}
	}
}

func TestShellV2StartUsesRealToolName(t *testing.T) {
	manager := shell.NewManager(shell.Config{Root: t.TempDir(), MaxOutputBytes: 256, Factory: fakeShellFactory{}})
	ctx := WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"shell_start"}},
		Shell: guard.ShellPerm{Policy: "allowlist", Patterns: []string{"echo *"}},
	})
	ctx = WithShellManager(ctx, manager)
	v := NewShellV2Tools(t.TempDir())
	out, err := runTool(ctx, v.Start, `{"command":"echo hi"}`)
	if err != nil {
		t.Fatal(err)
	}
	// Session JSON uses "id":"session-..." — the canonical id field. We look
	// for the "session-" prefix so the test doesn't bind to the exact JSON key
	// spelling (which is "id", not "session_id").
	if !strings.Contains(out, "session-") {
		t.Fatalf("start result=%q", out)
	}
	// Clean up so the manager doesn't leak goroutines.
	_ = manager.Close()
}

func TestShellV2WriteAuthorizesAsWriteToolNotShellString(t *testing.T) {
	manager := shell.NewManager(shell.Config{Root: t.TempDir(), MaxOutputBytes: 256, Factory: fakeShellFactory{}})
	ctx := WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"shell_start"}}, // missing shell_write_stdin
		Shell: guard.ShellPerm{Policy: "allowlist", Patterns: []string{"*"}},
	})
	ctx = WithShellManager(ctx, manager)
	v := NewShellV2Tools(t.TempDir())
	out, _ := runTool(ctx, v.Write, `{"id":"missing","data":"x"}`)
	if !strings.Contains(out, "permission denied") {
		t.Fatalf("write must Authorize as shell_write_stdin, got %q", out)
	}
	_ = manager.Close()
}

// TestShellV2WorkdirArgCannotWidenTheDestructiveBoundary is W-2.
//
// "workdir" is a shellStartArgs field and it is in the tool schema, so the model
// writes it — and v2 handed it straight to guard.Action.Workdir, which is the
// line the destructive-deletion dimension measures "inside vs outside" against.
// Measured with the real root at /work/project:
//
//	{"workdir":"/","command":"rm -rf /home/user"}   Prompt              -> Allow
//	{"workdir":"/","command":"rm -rf /work"}        structural HardDeny -> Allow
//
// The second is the gate itself: an ancestor of the work root is catastrophic
// until the request declares the root to be `/`. No name has to be guessed.
//
// The assertion is on the ACTION rather than only on a callback, because the
// widening spellings include ones that never reach a callback at all (a
// catastrophic verdict is refused before the callback is consulted), and a test
// that could only observe the ones that do would be blind to exactly the tier
// that matters most.
func TestShellV2WorkdirArgCannotWidenTheDestructiveBoundary(t *testing.T) {
	root := t.TempDir()
	v := NewShellV2Tools(root)
	for _, arg := range []string{
		"/",
		filepath.Dir(root),
		filepath.Join(root, "..", ".."),
		"/etc",
		"relative-nonsense",
		"",
	} {
		act, _, err := v.launchAction("shell_start", shellStartArgs{Command: "rm -rf /home/user", Workdir: arg})
		if err != nil {
			t.Fatal(err)
		}
		if act.Workdir != root {
			t.Errorf("launchAction(workdir=%q).Workdir = %q, want the server's root %q — the model "+
				"moved the boundary its own deletion is judged against", arg, act.Workdir, root)
		}
	}
	// The verdict this protects, end to end: deleting an ancestor of the work
	// root is the structural floor whatever the request says the root is.
	g := guard.New()
	prof := guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"*"}},
		FS:    guard.FSPerm{Read: []string{"**"}, Write: []string{"**"}},
		Shell: guard.ShellPerm{Policy: "denylist"},
	}
	act, _, err := v.launchAction("shell_start", shellStartArgs{
		Command: "rm -rf " + filepath.Dir(root), Workdir: "/",
	})
	if err != nil {
		t.Fatal(err)
	}
	if d := g.Check(prof, act); d.Verdict != guard.HardDeny || d.Overridable {
		t.Fatalf("Check(rm -rf <parent of root>, workdir arg=/) = %+v, want a structural HardDeny", d)
	}
}

// TestShellV2WorkdirArgMayNarrowTheDestructiveBoundary is the other half of the
// rule, and it is not decoration: a boundary that always snapped back to the
// root would make "the model cannot widen it" true by making the field inert,
// and a reader could not tell the two apart.
//
// Moving the boundary INWARDS can only tighten. Every deletion the outer root
// graded in-scope is still in-scope or becomes out-of-scope (a Prompt), and the
// subdirectory itself joins the catastrophic set as its own root.
func TestShellV2WorkdirArgMayNarrowTheDestructiveBoundary(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "pkg")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	v := NewShellV2Tools(root)
	act, _, err := v.launchAction("shell_start", shellStartArgs{Command: "rm -rf " + sub, Workdir: sub})
	if err != nil {
		t.Fatal(err)
	}
	// EvalSymlinks may rewrite the temp dir prefix (/var -> /private/var on
	// macOS), so compare the classification rather than the string.
	if guard.ClassifyDestruction("rm -rf "+sub, act.Workdir) != guard.DestructionCatastrophic {
		t.Errorf("workdir=%q was not honoured: deleting the declared working directory itself "+
			"must be catastrophic, and act.Workdir = %q", sub, act.Workdir)
	}
	// Control: at the root, the same deletion is an ordinary in-scope one.
	if guard.ClassifyDestruction("rm -rf "+sub, root) == guard.DestructionCatastrophic {
		t.Error("the control is wrong: deleting a subdirectory of the root is already catastrophic, " +
			"so this test proves nothing about narrowing")
	}
}

// TestShellV2LaunchRunsWhereTheCallerAskedEvenThoughTheGuardDoesNot separates the
// two uses of "workdir" that W-2 collapsed. Where the process RUNS is still the
// caller's choice; only the line the guard judges against is the server's.
func TestShellV2LaunchRunsWhereTheCallerAskedEvenThoughTheGuardDoesNot(t *testing.T) {
	root := t.TempDir()
	elsewhere := t.TempDir()
	factory := &recordingShellFactory{}
	manager := shell.NewManager(shell.Config{Root: root, MaxOutputBytes: 256, Factory: factory})
	defer func() { _ = manager.Close() }()
	var got PermissionRequest
	ctx := WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"shell_start"}},
		Shell: guard.ShellPerm{Policy: "allowlist", Patterns: []string{"echo *"}},
	})
	ctx = WithPermissionCallback(ctx, func(req PermissionRequest) PermissionDecision {
		got = req
		return PermissionAllow
	})
	ctx = WithShellManager(ctx, manager)
	v := NewShellV2Tools(root)
	if _, err := runTool(ctx, v.Start, `{"command":"echo hi","workdir":`+strconv.Quote(elsewhere)+`}`); err != nil {
		t.Fatal(err)
	}
	if len(factory.specs) != 1 || factory.specs[0].Dir != elsewhere {
		t.Fatalf("launch Dir = %+v, want %q", factory.specs, elsewhere)
	}
	if got.Tool != "" && got.Workdir != root {
		t.Fatalf("the callback saw Workdir %q; the guard boundary must stay at the root %q", got.Workdir, root)
	}
}

// TestShellV2HandsTheResolvedInterpreterToGuard is D-2.
//
// internal/tools/shell.go has set guard.Action.Interpreter since W-B-05, and it
// was the ONLY non-test caller that did. shell_v2.go resolved the very same
// `prog` from shell.ShellArgv for its own LaunchSpec and then did not hand it
// over, so guard read a cmd/PowerShell command with the POSIX reader — which is
// the situation W-B-05 exists to prevent.
//
// On a POSIX host both spellings select the same reader, so this asserts the
// FIELD rather than a verdict; the platform where the two differ is the one
// where the field was added.
func TestShellV2HandsTheResolvedInterpreterToGuard(t *testing.T) {
	v := NewShellV2Tools(t.TempDir())
	for _, tool := range []string{"shell_start", "task_shell_start"} {
		act, _, err := v.launchAction(tool, shellStartArgs{Command: "echo hi"})
		if err != nil {
			t.Fatal(err)
		}
		want, _, err := shell.ShellArgv("", "echo hi")
		if err != nil {
			t.Fatal(err)
		}
		if act.Interpreter != want {
			t.Errorf("%s: Action.Interpreter = %q, want the interpreter ShellArgv resolved (%q); "+
				"guard picks its reader from this field", tool, act.Interpreter, want)
		}
		if act.Tool != tool {
			t.Errorf("Action.Tool = %q, want %q", act.Tool, tool)
		}
	}
}
