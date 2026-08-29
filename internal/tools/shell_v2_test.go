package tools

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"reflect"
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

// TestShellV2EveryToolAuthorizesUnderItsOwnName drives every tool the v2
// surface constructs and requires each one to consult the guard, exactly once,
// under its own registered name, before it does anything else.
//
// # What it replaces, and why the thing it replaces was a hazard
//
// CLAUDE.md used to tell a reader to check this invariant by hand:
//
//	grep -c 'NewGuardedTool(' internal/tools/shell_v2.go
//	grep -c 'Authorize('      internal/tools/shell_v2.go
//	# "the two numbers must be equal"
//
// The counts were 9/9 when that was written and are not equal now, because
// shell_start and task_shell_start were refactored to share authorizeLaunch and
// shell_resize was added. The underlying invariant never broke — one Authorize
// serving two launches is the correct shape — but the PROXY for it did, so the
// instruction now reports a violation that does not exist. A reader who trusts
// it goes looking for a missing Authorize; a reader who has been burned once
// stops running it. Both outcomes are worse than no instruction.
//
// This is CLAUDE.md's own rule about bare counts, turned on a count it was
// itself printing: a number describing another file's current contents cannot
// survive that file being refactored, and nobody comes back to update it.
//
// # Why reflection over the struct rather than a list of the ten
//
// A written-out list is the same defect one level up: it is correct on the day
// it is written and silently incomplete the day an eleventh tool is added —
// which is exactly how shell_resize broke the grep. Walking the *GuardedTool
// fields of ShellV2Tools means a new tool is covered by construction, and the
// only way to escape this test is to build a v2 tool that is not a field of the
// struct the composition root registers from.
//
// # Why the assertion is the callback and not just "it returned an error"
//
// An empty PermissionProfile denies everything, so a tool that never called
// Authorize would still fail — on its own merits, with a manager bound and no
// such session — and a test that only checked for AN error would pass while
// proving nothing. What cannot happen without Authorize is the permission
// callback firing with the tool's own name, so that is what is asserted. The
// shell manager is bound for the same reason in the other direction: without
// it, a tool that skipped the guard would fail with "runtime unavailable" and
// look sufficiently denied to anyone reading the output rather than the check.
func TestShellV2EveryToolAuthorizesUnderItsOwnName(t *testing.T) {
	root := t.TempDir()
	manager := shell.NewManager(shell.Config{Root: root, MaxOutputBytes: 256, Factory: fakeShellFactory{}})
	defer func() { _ = manager.Close() }()

	// One args object that satisfies every v2 tool's shape at once: encoding/json
	// ignores the fields a given tool does not declare, and every handler
	// unmarshals before it authorizes, so a shape error would mask the thing
	// under test. "nosuch" is a session id nothing can resolve, which is what
	// makes a guard-skipping tool fail visibly rather than act.
	const args = `{"command":"echo hi","id":"nosuch","data":"x","rows":24,"cols":80,` +
		`"max_bytes":16,"timeout_s":1}`

	v := NewShellV2Tools(root)
	rv := reflect.ValueOf(v).Elem()
	rt := rv.Type()
	guardedToolType := reflect.TypeOf((*GuardedTool)(nil))
	checked := 0
	for i := 0; i < rt.NumField(); i++ {
		field := rt.Field(i)
		if !field.IsExported() || field.Type != guardedToolType {
			continue
		}
		gt, _ := rv.Field(i).Interface().(*GuardedTool)
		if gt == nil {
			t.Errorf("ShellV2Tools.%s is nil; NewShellV2Tools left a tool unbuilt", field.Name)
			continue
		}
		info, err := gt.Info(context.Background())
		if err != nil {
			t.Fatalf("ShellV2Tools.%s: Info: %v", field.Name, err)
		}

		var asked []string
		ctx := WithShellManager(context.Background(), manager)
		ctx = WithProfile(ctx, guard.PermissionProfile{})
		ctx = WithPermissionCallback(ctx, func(req PermissionRequest) PermissionDecision {
			asked = append(asked, req.Tool)
			return PermissionDeny
		})
		out, runErr := runTool(ctx, gt, args)
		if runErr != nil {
			t.Fatalf("ShellV2Tools.%s (%s): unexpected transport error: %v", field.Name, info.Name, runErr)
		}
		switch {
		case len(asked) == 0:
			t.Errorf("ShellV2Tools.%s (%s) never reached the guard: the permission callback was "+
				"not consulted, so this tool acts on an empty profile that permits nothing. "+
				"Every v2 tool must call Authorize itself — there is no secproc backstop on "+
				"this path. It returned: %s", field.Name, info.Name, out)
		case len(asked) != 1 || asked[0] != info.Name:
			t.Errorf("ShellV2Tools.%s authorized as %v, want exactly one consult under %q. "+
				"Authorizing under another tool's name gates this action with that tool's "+
				"profile entry, which is how a narrow allowlist silently covers a wide action",
				field.Name, asked, info.Name)
		case !strings.Contains(out, "permission denied"):
			t.Errorf("ShellV2Tools.%s (%s) consulted the guard, was denied, and acted anyway: %s",
				field.Name, info.Name, out)
		}
		checked++
	}
	if checked == 0 {
		t.Fatal("no *GuardedTool fields found on ShellV2Tools — this test passed without " +
			"examining anything, which is the failure mode it exists to prevent")
	}
}
