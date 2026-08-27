package acp

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/x6nux/yanshi/internal/guard"
	"github.com/x6nux/yanshi/internal/sandbox"
	"github.com/x6nux/yanshi/internal/secproc"
)

// stubFactory is a secproc.Factory that records the spec and hands back a
// canned StartedProcess.
type stubFactory struct {
	seen    secproc.SecureProcessSpec
	proc    *secproc.StartedProcess
	startEr error
}

func (f *stubFactory) Start(_ context.Context, spec secproc.SecureProcessSpec) (*secproc.StartedProcess, error) {
	f.seen = spec
	if f.startEr != nil {
		return nil, f.startEr
	}
	return f.proc, nil
}

// deadPipe is a WriteCloser that reports every write as a broken pipe, which
// is what a child that exited immediately looks like from the parent.
type deadPipe struct{ closed bool }

func (d *deadPipe) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }
func (d *deadPipe) Close() error              { d.closed = true; return nil }

// swapAuthorizer installs an authorizer for the duration of a test and
// restores the previous one. The production authorizer is a process-wide
// variable set by tools' init; leaving a test's replacement behind would make
// every later test in this package run without the firewall.
func swapAuthorizer(t *testing.T, a secproc.Authorizer) {
	t.Helper()
	prev := secproc.SwapAuthorizer(a)
	t.Cleanup(func() { secproc.SwapAuthorizer(prev) })
}

// withStubFactory binds a factory and an allow-everything authorizer for the
// duration of a test.
func withStubFactory(t *testing.T, f *stubFactory) context.Context {
	t.Helper()
	swapAuthorizer(t, func(context.Context, guard.Action, string) error { return nil })
	return secproc.WithFactory(context.Background(), f)
}

// TestSpawnSecureFailsClosedWithoutAReaper: a Factory that forgets Wait leaves
// a child nobody can reap. Accepting it would leak one zombie per delegation.
func TestSpawnSecureFailsClosedWithoutAReaper(t *testing.T) {
	f := &stubFactory{proc: &secproc.StartedProcess{
		Stdout: strings.NewReader(""), Stdin: &deadPipe{},
	}}
	ctx := withStubFactory(t, f)
	if _, err := SpawnSecure(ctx, SpawnOptions{Agent: "codex"}); err == nil ||
		!strings.Contains(err.Error(), "reaper") {
		t.Fatalf("want a fail-closed reaper error, got %v", err)
	}
}

// TestSpawnSecureFailsClosedWithoutStdin: an ACP agent is driven by writing
// JSON-RPC to its stdin. A Factory that supplies none yields a client that can
// never send a request and would hang on the first Initialize until the
// context expired — a timeout instead of a diagnosis.
func TestSpawnSecureFailsClosedWithoutStdin(t *testing.T) {
	reaped := false
	f := &stubFactory{proc: &secproc.StartedProcess{
		Wait: func() error { reaped = true; return nil }, Stdout: strings.NewReader(""),
	}}
	ctx := withStubFactory(t, f)
	_, err := SpawnSecure(ctx, SpawnOptions{Agent: "codex"})
	if err == nil || !strings.Contains(err.Error(), "stdin") {
		t.Fatalf("want a fail-closed stdin error, got %v", err)
	}
	if !reaped {
		t.Error("the already-started child was not reaped before returning the error")
	}
}

// TestSpawnSecureRequestsWorkspaceWriteTier pins the sandbox tier. ReadOnly
// would make every delegation fail on the agent's first edit; FullAccess would
// hand an untrusted CLI the whole machine. Neither failure is visible without
// running a real agent, which is why it is asserted on the spec.
func TestSpawnSecureRequestsWorkspaceWriteTier(t *testing.T) {
	f := &stubFactory{proc: &secproc.StartedProcess{
		Wait: func() error { return nil }, Stdout: strings.NewReader(""), Stdin: &deadPipe{},
	}}
	ctx := withStubFactory(t, f)
	// The handshake fails (the stub pipe is dead), which is fine: the spec was
	// already recorded by then.
	_, _ = SpawnSecure(ctx, SpawnOptions{Agent: "codex", Cwd: "/tmp/project"})
	if f.seen.UseSandboxTier != sandbox.WorkspaceWrite {
		t.Errorf("sandbox tier = %v, want WorkspaceWrite", f.seen.UseSandboxTier)
	}
	if f.seen.Tool != "acp_delegate" {
		t.Errorf("spec.Tool = %q; the Authorize firewall keys on this", f.seen.Tool)
	}
	if f.seen.Dir != "/tmp/project" {
		t.Errorf("spec.Dir = %q, want the requested cwd", f.seen.Dir)
	}
	if len(f.seen.AllowEnv) != 0 {
		t.Errorf("AllowEnv = %v; a delegated agent must inherit no credentials", f.seen.AllowEnv)
	}
}

// TestSpawnSecureUnknownAgentDoesNotLaunch: the argv lookup must fail before
// anything is spawned, or an unknown name becomes an attempt to exec "".
func TestSpawnSecureUnknownAgentDoesNotLaunch(t *testing.T) {
	f := &stubFactory{proc: &secproc.StartedProcess{Wait: func() error { return nil }}}
	ctx := withStubFactory(t, f)
	if _, err := SpawnSecure(ctx, SpawnOptions{Agent: "nope"}); err == nil {
		t.Fatal("unknown agent must fail")
	}
	if f.seen.Program != "" {
		t.Fatalf("unknown agent still reached the factory: %+v", f.seen)
	}
}

// TestSpawnSecurePropagatesLaunchDenial: a guard denial from the Authorize
// firewall has to reach the caller verbatim, because the denial REASON is the
// only thing that tells an operator which profile clause refused.
func TestSpawnSecurePropagatesLaunchDenial(t *testing.T) {
	denied := errors.New("permission denied: acp_delegate not allowed")
	swapAuthorizer(t, func(context.Context, guard.Action, string) error { return denied })
	ctx := secproc.WithFactory(context.Background(), &stubFactory{})
	_, err := SpawnSecure(ctx, SpawnOptions{Agent: "codex"})
	if !errors.Is(err, denied) {
		t.Fatalf("denial was swallowed: %v", err)
	}
}

// TestStderrTailKeepsTheEnd: the tail is what carries the cause ("npx: command
// not found"), and it must be bounded so a crash-looping agent cannot grow the
// parent's heap.
func TestStderrTailKeepsTheEnd(t *testing.T) {
	tail := drainStderr(strings.NewReader(strings.Repeat("a", 100)+"CAUSE"), 10)
	<-tail.done
	got := tail.String()
	if len(got) > 10 {
		t.Errorf("tail %q exceeds the limit", got)
	}
	if !strings.HasSuffix(got, "CAUSE") {
		t.Errorf("tail %q dropped the end", got)
	}
}

// TestStderrTailNilReader: a Factory that merged both streams reports Stderr as
// nil rather than an empty reader; the drain must not panic or block.
func TestStderrTailNilReader(t *testing.T) {
	tail := drainStderr(nil, 10)
	<-tail.done
	if tail.String() != "" {
		t.Errorf("nil reader produced %q", tail.String())
	}
}

// TestWorktreeScopedProfileConfinesTheAgent pins the posture handed to a
// delegated agent. Every clause is load-bearing: dotted tool names because
// GuardPolicy synthesises fs.read/shell.run from ACP tool KINDS, and Net.Allow
// false because an agent that can reach the network can exfiltrate the project.
func TestWorktreeScopedProfileConfinesTheAgent(t *testing.T) {
	p := WorktreeScopedProfile("/work/wt1")
	glob := PathToGlob("/work/wt1")
	if len(p.FS.Read) != 1 || p.FS.Read[0] != glob {
		t.Errorf("FS.Read = %v, want [%s]", p.FS.Read, glob)
	}
	if len(p.FS.Write) != 1 || p.FS.Write[0] != glob {
		t.Errorf("FS.Write = %v, want [%s]", p.FS.Write, glob)
	}
	if p.Net.Allow {
		t.Error("Net.Allow must be false")
	}
	if p.Shell.Policy != "allowlist" {
		t.Errorf("Shell.Policy = %q, want allowlist", p.Shell.Policy)
	}
	for _, want := range []string{"fs.*", "shell.*"} {
		found := false
		for _, got := range p.Tools.Allow {
			if got == want {
				found = true
			}
		}
		if !found {
			t.Errorf("Tools.Allow %v is missing %q — GuardPolicy synthesises DOTTED names", p.Tools.Allow, want)
		}
	}
}

// TestPathToGlobUsesForwardSlashes: the guard's matcher is slash-based on every
// platform, so a Windows path that kept its backslashes would match nothing and
// deny every file the agent touches.
func TestPathToGlobUsesForwardSlashes(t *testing.T) {
	got := PathToGlob(`C:\work\wt`)
	if strings.Contains(got, `\`) {
		t.Errorf("PathToGlob(%q) = %q, still contains a backslash", `C:\work\wt`, got)
	}
	if !strings.HasSuffix(got, "/**") {
		t.Errorf("PathToGlob = %q, want a /** suffix", got)
	}
}

// TestVCSMCPCommandCarriesEveryBinding: the env map is a cross-process wire
// shape read back by `yanshi vcs-mcp`. A missing key does not fail the spawn —
// it produces an agent whose vcs_* tools bind to the wrong worktree, or to
// none, with no error anywhere.
func TestVCSMCPCommandCarriesEveryBinding(t *testing.T) {
	cmd := VCSMCPConfig{
		Binary: "/bin/yanshi", DBPath: "/db.sqlite", RepoID: "repo1",
		WorktreeID: "wt1", Agent: "codex", WorktreeDir: "/wts",
	}.VCSMCPCommand()

	if cmd["command"] != "/bin/yanshi" {
		t.Errorf("command = %v", cmd["command"])
	}
	args, _ := cmd["args"].([]string)
	if len(args) != 1 || args[0] != "vcs-mcp" {
		t.Errorf("args = %v, want [vcs-mcp]", args)
	}
	env, _ := cmd["env"].(map[string]string)
	want := map[string]string{
		"YANSHI_DB_PATH": "/db.sqlite", "YANSHI_REPO_ID": "repo1",
		"YANSHI_WT_ID": "wt1", "YANSHI_AGENT": "codex", "YANSHI_WORKTREE_DIR": "/wts",
	}
	for k, v := range want {
		if env[k] != v {
			t.Errorf("env[%s] = %q, want %q", k, env[k], v)
		}
	}
	if len(env) != len(want) {
		t.Errorf("env has %d keys, want exactly %d: %v", len(env), len(want), env)
	}
}
