package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/x6nux/yanshi/internal/acp"
)

// fakeDelegateSession is a scripted delegateSession: it records what it was
// asked, replays a canned transcript, and reports whether Cancel/Close ran.
type fakeDelegateSession struct {
	transcript []string
	stopReason string
	promptErr  error
	promptWait time.Duration

	gotTask   string
	cancelled bool
	closed    bool
	cancelErr error
}

func (f *fakeDelegateSession) Prompt(ctx context.Context, task string, onEvent func(acp.Event)) (string, error) {
	f.gotTask = task
	for _, chunk := range f.transcript {
		if onEvent != nil {
			onEvent(acp.Event{Kind: "agent_message_chunk", Text: chunk})
		}
	}
	if f.promptWait > 0 {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(f.promptWait):
		}
	}
	return f.stopReason, f.promptErr
}

func (f *fakeDelegateSession) Cancel(ctx context.Context) error {
	// The whole point of cancelQuietly is that this must be reachable with a
	// LIVE context even when the turn context is already dead.
	if ctx.Err() != nil {
		f.cancelErr = ctx.Err()
	}
	f.cancelled = true
	return nil
}

func (f *fakeDelegateSession) Close() { f.closed = true }

// withFakeSpawn swaps the package spawn seam for the duration of a test and
// hands back the recorded SpawnOptions. It also neutralises the PATH probe:
// no CI runner has codex installed, and without this the happy path is
// unreachable from a unit test.
func withFakeSpawn(t *testing.T, session delegateSession, spawnErr error) *acp.SpawnOptions {
	t.Helper()
	saved := acpSpawn
	savedLook := acpLookPath
	acpLookPath = func(f string) (string, error) { return f, nil }
	t.Cleanup(func() { acpSpawn = saved; acpLookPath = savedLook })
	var seen acp.SpawnOptions
	acpSpawn = func(_ context.Context, opts acp.SpawnOptions) (delegateSession, error) {
		seen = opts
		if spawnErr != nil {
			return nil, spawnErr
		}
		return session, nil
	}
	return &seen
}

// TestACPDelegateRefusals covers every input the tool must reject as a tool
// RESULT rather than a Go error, because the model can recover from each one.
func TestACPDelegateRefusals(t *testing.T) {
	cases := []struct {
		name     string
		args     string
		wantSubs []string
	}{
		{"empty task", `{"agent":"codex","task":"   "}`, []string{"✗", "task is required"}},
		{"missing agent", `{"task":"do a thing"}`, []string{"✗", "agent is required"}},
		{"unknown agent", `{"agent":"nosuchcli","task":"do a thing"}`, []string{"✗", "unknown agent", "nosuchcli"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// No spawn seam installed: these must all refuse before spawning.
			out, err := runACPDelegate(context.Background(), ACPDelegateConfig{}, tc.args)
			if err != nil {
				t.Fatalf("refusal must be a tool result, got Go error: %v", err)
			}
			for _, sub := range tc.wantSubs {
				if !strings.Contains(out, sub) {
					t.Errorf("result %q missing %q", out, sub)
				}
			}
		})
	}
}

// TestACPDelegateMalformedArgsIsAGoError pins the one input that is NOT a
// model-recoverable refusal: unparseable JSON means the tool-call machinery
// itself misbehaved, and errcnt's circuit breaker should see it.
func TestACPDelegateMalformedArgsIsAGoError(t *testing.T) {
	if _, err := runACPDelegate(context.Background(), ACPDelegateConfig{}, `{not json`); err == nil {
		t.Fatal("malformed args must return a Go error")
	}
}

// TestACPDelegateDepthLimit proves the nesting guard reads BOTH counters and
// takes the max, so a chain that mixes in-process sub-agents with cross-process
// delegation cannot slip past a limit each half respects alone.
func TestACPDelegateDepthLimit(t *testing.T) {
	cases := []struct {
		name     string
		ctxDepth int
		envDepth string
		refuse   bool
	}{
		{"fresh turn", 0, "", false},
		{"one level in-process", 1, "", false},
		{"at limit in-process", MaxSubAgentDepth, "", true},
		{"at limit via env", 0, "3", true},
		{"env below limit", 0, "1", false},
		{"malformed env ignored", 0, "not-a-number", false},
		// The mixed chain: neither counter alone is at the limit, but the env
		// one is what the process actually inherited.
		{"env dominates ctx", 1, "3", true},
		{"ctx dominates env", MaxSubAgentDepth, "1", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(ACPDepthEnv, tc.envDepth)
			ctx := WithSubAgentDepth(context.Background(), tc.ctxDepth)
			_, over := acpDepthExceeded(ctx)
			if over != tc.refuse {
				t.Fatalf("acpDepthExceeded over=%v, want %v", over, tc.refuse)
			}
		})
	}
}

// TestACPDelegateDepthRefusalIsAToolResult drives the refusal through the real
// entry point, so a future refactor that keeps acpDepthExceeded correct while
// forgetting to consult it still fails.
func TestACPDelegateDepthRefusalIsAToolResult(t *testing.T) {
	t.Setenv(ACPDepthEnv, "9")
	spawned := withFakeSpawn(t, &fakeDelegateSession{stopReason: "end_turn"}, nil)
	out, err := runACPDelegate(context.Background(), ACPDelegateConfig{},
		`{"agent":"codex","task":"recurse forever"}`)
	if err != nil {
		t.Fatalf("depth refusal must be a tool result: %v", err)
	}
	if !strings.Contains(out, "nesting depth") {
		t.Fatalf("result %q does not name the depth limit", out)
	}
	if spawned.Agent != "" {
		t.Fatal("refused delegation must not spawn an agent")
	}
}

// TestACPDelegateEnvCarriesNextDepth is the other half of the loop guard: the
// counter has to be INCREMENTED into the child, or the child reads the parent's
// value and the chain never terminates.
func TestACPDelegateEnvCarriesNextDepth(t *testing.T) {
	t.Setenv(ACPDepthEnv, "1")
	session := &fakeDelegateSession{stopReason: "end_turn"}
	seen := withFakeSpawn(t, session, nil)
	ctx := WithWorkRoot(context.Background(), t.TempDir())
	if _, err := delegateToAgent(ctx, ACPDelegateConfig{},
		acpDelegateArgs{Agent: "codex", Task: "work"}); err != nil {
		t.Fatalf("delegate: %v", err)
	}
	want := ACPDepthEnv + "=2"
	found := false
	for _, e := range seen.Env {
		if e == want {
			found = true
		}
	}
	if !found {
		t.Fatalf("spawn env %v does not carry %q", seen.Env, want)
	}
}

// TestACPDelegateNoWorkRootRefuses pins the fail-closed direction: with no VCS
// and no work root there is no directory the agent may legitimately be pointed
// at, and defaulting to the process cwd would silently aim it at whatever
// directory the daemon happened to start in.
func TestACPDelegateNoWorkRootRefuses(t *testing.T) {
	seen := withFakeSpawn(t, &fakeDelegateSession{stopReason: "end_turn"}, nil)
	out, err := delegateToAgent(context.Background(), ACPDelegateConfig{},
		acpDelegateArgs{Agent: "codex", Task: "work"})
	if err != nil {
		t.Fatalf("refusal must be a tool result: %v", err)
	}
	if !strings.Contains(out, "work root") {
		t.Fatalf("result %q does not explain the missing work root", out)
	}
	if seen.Agent != "" {
		t.Fatal("must not spawn without a work root")
	}
}

// TestACPDelegateHappyPathWithoutVCS checks the result shape and, critically,
// that the no-VCS degradation is REPORTED rather than silent — a model told
// only "done" would assume the worktree isolation the tool description
// promises actually happened.
func TestACPDelegateHappyPathWithoutVCS(t *testing.T) {
	session := &fakeDelegateSession{
		transcript: []string{"reading files", " ...done"},
		stopReason: "end_turn",
	}
	seen := withFakeSpawn(t, session, nil)
	root := t.TempDir()
	ctx := WithWorkRoot(context.Background(), root)

	out, err := delegateToAgent(ctx, ACPDelegateConfig{}, acpDelegateArgs{Agent: "codex", Task: "fix the bug"})
	if err != nil {
		t.Fatalf("delegate: %v", err)
	}
	var res acpDelegateResult
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("result is not JSON: %v (%s)", err, out)
	}
	if res.Agent != "codex" || res.StopReason != "end_turn" {
		t.Errorf("result = %+v", res)
	}
	if res.Transcript != "reading files ...done" {
		t.Errorf("transcript = %q", res.Transcript)
	}
	if res.Worktree != "" || res.Merged {
		t.Errorf("no VCS bound, but result claims a worktree: %+v", res)
	}
	if !strings.Contains(res.Note, "autoVCS is not configured") {
		t.Errorf("silent degradation: note = %q", res.Note)
	}
	if session.gotTask != "fix the bug" {
		t.Errorf("agent got task %q", session.gotTask)
	}
	if seen.Cwd != root {
		t.Errorf("agent cwd = %q, want the work root %q", seen.Cwd, root)
	}
	if !session.closed {
		t.Error("session was not closed — the child would be left unreaped")
	}
}

// TestACPDelegateScopesTheAgentToItsDirectory is the security half: the profile
// handed to the agent must confine it to the directory it was given. Without
// this, a delegated agent inherits nothing at all (GuardPolicy denies) or —
// worse, if someone "fixes" that by passing the caller's profile — the whole
// filesystem the caller could reach.
func TestACPDelegateScopesTheAgentToItsDirectory(t *testing.T) {
	seen := withFakeSpawn(t, &fakeDelegateSession{stopReason: "end_turn"}, nil)
	root := t.TempDir()
	ctx := WithWorkRoot(context.Background(), root)
	if _, err := delegateToAgent(ctx, ACPDelegateConfig{},
		acpDelegateArgs{Agent: "codex", Task: "work"}); err != nil {
		t.Fatalf("delegate: %v", err)
	}
	policy, ok := seen.Policy.(*acp.GuardPolicy)
	if !ok {
		t.Fatalf("spawn policy is %T, want *acp.GuardPolicy — an unpoliced agent is unrestricted", seen.Policy)
	}
	wantGlob := acp.PathToGlob(root)
	if len(policy.Profile.FS.Write) != 1 || policy.Profile.FS.Write[0] != wantGlob {
		t.Errorf("FS.Write = %v, want exactly [%s]", policy.Profile.FS.Write, wantGlob)
	}
	if policy.Profile.Net.Allow {
		t.Error("delegated agent must not get network access by default")
	}
}

// TestACPDelegateCancelsOnPromptFailure pins the teardown ORDER that keeps a
// half-written file out of the merged worktree: the agent is told to stop
// before the pipes are closed, and the cancel runs on a live context even
// though the turn context is the thing that died.
func TestACPDelegateCancelsOnPromptFailure(t *testing.T) {
	session := &fakeDelegateSession{promptErr: errors.New("deadline exceeded")}
	withFakeSpawn(t, session, nil)
	ctx, cancel := context.WithCancel(WithWorkRoot(context.Background(), t.TempDir()))
	cancel() // the turn context is already dead when teardown starts

	if _, err := delegateToAgent(ctx, ACPDelegateConfig{},
		acpDelegateArgs{Agent: "codex", Task: "work"}); err == nil {
		t.Fatal("a failed prompt must surface as an error")
	}
	if !session.cancelled {
		t.Error("agent was never told to stop; it can keep writing into the worktree")
	}
	if session.cancelErr != nil {
		t.Errorf("cancel ran on a dead context (%v) — the notification could never be sent", session.cancelErr)
	}
	if !session.closed {
		t.Error("session not closed")
	}
}

// TestACPDelegateTimeoutIsClamped proves timeout_seconds can only NARROW the
// ceiling. A model that asks for a day must not get one.
func TestACPDelegateTimeoutIsClamped(t *testing.T) {
	cases := []struct {
		name      string
		requested int
		wantMax   time.Duration
	}{
		{"unset uses the ceiling", 0, ACPDelegateTimeout},
		{"narrower is honoured", 30, 30 * time.Second},
		{"wider is clamped", 86400, ACPDelegateTimeout},
		{"negative is ignored", -5, ACPDelegateTimeout},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var seenDeadline time.Duration
			saved := acpSpawn
			t.Cleanup(func() { acpSpawn = saved })
			acpSpawn = func(ctx context.Context, _ acp.SpawnOptions) (delegateSession, error) {
				dl, ok := ctx.Deadline()
				if !ok {
					t.Fatal("delegation ran without a deadline")
				}
				seenDeadline = time.Until(dl)
				return &fakeDelegateSession{stopReason: "end_turn"}, nil
			}
			args, _ := json.Marshal(acpDelegateArgs{
				Agent: "codex", Task: "work", TimeoutSeconds: tc.requested,
			})
			ctx := WithWorkRoot(context.Background(), t.TempDir())
			savedLook := acpLookPath
			acpLookPath = func(f string) (string, error) { return f, nil }
			t.Cleanup(func() { acpLookPath = savedLook })
			if _, err := runACPDelegate(ctx, ACPDelegateConfig{}, string(args)); err != nil {
				t.Fatalf("delegate: %v", err)
			}
			// A second of slack: the clock advances between clamping and the
			// deadline read.
			if seenDeadline > tc.wantMax || seenDeadline < tc.wantMax-2*time.Second {
				t.Errorf("deadline %v, want ~%v", seenDeadline, tc.wantMax)
			}
		})
	}
}

// TestTailBytes pins that truncation is visible. A silently clipped transcript
// reads to the model as an agent that stopped mid-sentence.
func TestTailBytes(t *testing.T) {
	if got := tailBytes("short", 100); got != "short" {
		t.Errorf("under the limit must pass through, got %q", got)
	}
	long := strings.Repeat("x", 50) + "TAIL"
	got := tailBytes(long, 10)
	if !strings.Contains(got, "truncated") {
		t.Errorf("truncation is invisible: %q", got)
	}
	if !strings.HasSuffix(got, "TAIL") {
		t.Errorf("kept the head instead of the tail: %q", got)
	}
}
