package tools

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/guard"
	"github.com/x6nux/yanshi/internal/secrets"
)

// slowStream is a StreamFunc that emits `chunks` pieces of text with `gap`
// between them and then closes, or aborts early if the context is cancelled.
// It reports whether it ran to completion via the done channel, which is how
// the tests tell "kept running in the background" from "was killed at the
// deadline".
func slowStream(chunks int, gap time.Duration, done chan<- bool) StreamFunc {
	return func(ctx context.Context, _ string) <-chan ToolChunk {
		ch := make(chan ToolChunk)
		go func() {
			defer close(ch)
			for i := 0; i < chunks; i++ {
				select {
				case <-ctx.Done():
					if done != nil {
						done <- false
					}
					return
				case <-time.After(gap):
				}
				select {
				case ch <- ToolChunk{Result: "piece\n"}:
				case <-ctx.Done():
					if done != nil {
						done <- false
					}
					return
				}
			}
			if done != nil {
				done <- true
			}
		}()
		return ch
	}
}

// bgCtx builds a tool-execution context that authorizes everything and binds a
// background manager. The wide-open profile is deliberate: these tests are
// about the offload path, and every one of them would still pass if the guard
// were the thing doing the work, which is what a narrower profile would hide.
func bgCtx(t *testing.T, mgr *BackgroundManager) context.Context {
	t.Helper()
	ctx := WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"*"}},
	})
	ctx = WithWorkRoot(ctx, t.TempDir())
	return WithBackgroundManager(ctx, mgr)
}

// drain collects a Stream channel's Result text.
func drainChunks(ch <-chan ToolChunk) string {
	var b strings.Builder
	for c := range ch {
		b.WriteString(c.Result)
	}
	return b.String()
}

func TestIsBackgroundable_Table(t *testing.T) {
	cases := []struct {
		tool string
		want bool
		why  string
	}{
		{"shell_run", true, "the archetype: a build that needs ten minutes still answers at minute ten"},
		{"run_tests", true, "same"},
		{"task_gate_run", true, "same"},
		{"fs_read", false, "over before the deadline exists; a handle would cost more than the file"},
		{"web_fetch", false, "a fetch that has not answered in 30s will not become useful at minute two"},
		{"agent_wait", false, "NoTimeout: bounded by the turn, so there is no deadline to offload at"},
		{"", false, "empty is not a tool"},
	}
	for _, tc := range cases {
		t.Run(tc.tool, func(t *testing.T) {
			assert.Equal(t, tc.want, IsBackgroundable(tc.tool), tc.why)
		})
	}
}

// TestOffloadReturnsAHandleAndKeepsRunning is the core T3 claim: at the
// deadline the model gets an ANSWER (not a deadline error), and the work
// continues to completion afterwards.
func TestOffloadReturnsAHandleAndKeepsRunning(t *testing.T) {
	mgr := NewBackgroundManager()
	t.Cleanup(func() { mgr.Close() })
	finished := make(chan bool, 1)

	tool := NewGuardedTool("shell_run", "Bash", "d", 40*time.Millisecond, nil,
		slowStream(6, 25*time.Millisecond, finished))

	out, err := tool.InvokableRun(bgCtx(t, mgr), `{"command":"make"}`)
	require.NoError(t, err)
	assert.Contains(t, out, "moved to the background",
		"the deadline must produce a handle, not a discarded run")
	assert.NotContains(t, out, "context deadline exceeded")

	select {
	case ok := <-finished:
		require.True(t, ok, "the run must have finished on its own, not been cancelled at the deadline")
	case <-time.After(3 * time.Second):
		t.Fatal("the backgrounded run never finished")
	}

	// The manager holds the completed run with its full output.
	require.Eventually(t, func() bool {
		runs := mgr.List()
		return len(runs) == 1 && runs[0].State == BackgroundCompleted
	}, 2*time.Second, 10*time.Millisecond)
	assert.Contains(t, mgr.List()[0].Result, "piece")
}

// TestOffloadIsIdempotent. QwenPaw's offload notice tells the model "do not
// re-run the same tool", and a sentence in a prompt is not a mechanism. A
// repeat of the SAME call while the first is still going must be refused.
func TestOffloadIsIdempotent(t *testing.T) {
	mgr := NewBackgroundManager()
	t.Cleanup(func() { mgr.Close() })

	tool := NewGuardedTool("run_tests", "Run tests", "d", 30*time.Millisecond, nil,
		slowStream(20, 20*time.Millisecond, nil))
	ctx := bgCtx(t, mgr)

	first, err := tool.InvokableRun(ctx, `{"framework":"go"}`)
	require.NoError(t, err)
	require.Contains(t, first, "moved to the background")

	second, err := tool.InvokableRun(ctx, `{"framework":"go"}`)
	require.NoError(t, err)
	assert.Contains(t, second, "ALREADY running")
	assert.Contains(t, second, "NOT started a second time")
	assert.Len(t, mgr.List(), 1, "the repeat must not have started a second run")

	// A DIFFERENT argument blob is a different call and is not refused.
	third, err := tool.InvokableRun(ctx, `{"framework":"cargo"}`)
	require.NoError(t, err)
	assert.Contains(t, third, "moved to the background")
	assert.Len(t, mgr.List(), 2)
}

// TestNoOffloadWithoutAManager pins the nil gate. Without a manager nothing
// can own the run after the turn, so offloading would leak a subprocess: the
// tool must keep its plain deadline behaviour.
func TestNoOffloadWithoutAManager(t *testing.T) {
	ctx := WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"*"}},
	})
	tool := NewGuardedTool("shell_run", "Bash", "d", 20*time.Millisecond, nil,
		slowStream(10, 20*time.Millisecond, nil))
	out, err := tool.InvokableRun(ctx, `{}`)
	require.NoError(t, err)
	assert.NotContains(t, out, "moved to the background")
}

// TestNonBackgroundableToolIsNotOffloaded. A manager is bound and the tool
// still takes the ordinary path, because the eligibility list is what decides.
func TestNonBackgroundableToolIsNotOffloaded(t *testing.T) {
	mgr := NewBackgroundManager()
	t.Cleanup(func() { mgr.Close() })
	tool := NewGuardedTool("fs_read", "Read", "d", 20*time.Millisecond, nil,
		slowStream(10, 20*time.Millisecond, nil))
	out, err := tool.InvokableRun(bgCtx(t, mgr), `{}`)
	require.NoError(t, err)
	assert.NotContains(t, out, "moved to the background")
	assert.Empty(t, mgr.List(), "nothing may have been adopted")
}

// TestNoTimeoutToolIsNotOffloaded. NoTimeout means "bounded by the turn",
// which is agent_wait's whole contract; there is no deadline at which to
// offload and detaching it from the turn would remove its only bound.
func TestNoTimeoutToolIsNotOffloaded(t *testing.T) {
	mgr := NewBackgroundManager()
	t.Cleanup(func() { mgr.Close() })
	// Registered under a backgroundable NAME so the only thing declining the
	// offload is the NoTimeout branch.
	tool := NewGuardedTool("shell_run", "Bash", "d", NoTimeout, nil,
		SyncStream(func(context.Context, string) (string, error) { return "quick", nil }))
	out, err := tool.InvokableRun(bgCtx(t, mgr), `{}`)
	require.NoError(t, err)
	assert.Equal(t, "quick", out)
	assert.Empty(t, mgr.List())
}

// TestFastToolFinishesInTheForeground. The offload path must be invisible to a
// call that beats its deadline: same result, nothing adopted.
func TestFastToolFinishesInTheForeground(t *testing.T) {
	mgr := NewBackgroundManager()
	t.Cleanup(func() { mgr.Close() })
	tool := NewGuardedTool("shell_run", "Bash", "d", 5*time.Second, nil,
		SyncStream(func(context.Context, string) (string, error) { return "exit 0", nil }))
	out, err := tool.InvokableRun(bgCtx(t, mgr), `{}`)
	require.NoError(t, err)
	assert.Equal(t, "exit 0", out)
	assert.Empty(t, mgr.List(), "a call that beat its deadline must not be adopted")
}

// TestTurnCancelBeforeTheDeadlineKillsTheRun. A user pressing Ctrl-C wants the
// command stopped, not silently promoted to a background job they never asked
// for.
func TestTurnCancelBeforeTheDeadlineKillsTheRun(t *testing.T) {
	mgr := NewBackgroundManager()
	t.Cleanup(func() { mgr.Close() })
	finished := make(chan bool, 1)

	ctx, cancel := context.WithCancel(bgCtx(t, mgr))
	tool := NewGuardedTool("shell_run", "Bash", "d", 5*time.Second, nil,
		slowStream(20, 30*time.Millisecond, finished))

	go func() { time.Sleep(60 * time.Millisecond); cancel() }()
	_, err := tool.InvokableRun(ctx, `{}`)
	require.NoError(t, err)

	select {
	case ok := <-finished:
		assert.False(t, ok, "the run must have been cancelled, not completed")
	case <-time.After(3 * time.Second):
		t.Fatal("the cancelled run never unwound")
	}
	assert.Empty(t, mgr.List(), "a turn cancel is not an offload")
}

// TestBackgroundCancelStopsTheRun covers the model-facing cancel and the
// state it produces. The state flips only when the goroutine unwinds, which is
// what "cancellation requested" in the tool result reflects.
func TestBackgroundCancelStopsTheRun(t *testing.T) {
	mgr := NewBackgroundManager()
	t.Cleanup(func() { mgr.Close() })

	tool := NewGuardedTool("shell_run", "Bash", "d", 30*time.Millisecond, nil,
		slowStream(50, 30*time.Millisecond, nil))
	out, err := tool.InvokableRun(bgCtx(t, mgr), `{}`)
	require.NoError(t, err)
	require.Contains(t, out, "moved to the background")

	id := mgr.List()[0].ID
	require.True(t, mgr.Cancel(id))
	assert.False(t, mgr.Cancel(id+"-nope"), "an unknown id is not cancellable")

	require.Eventually(t, func() bool {
		run, _ := mgr.Get(id)
		return run.State == BackgroundCancelled
	}, 2*time.Second, 10*time.Millisecond,
		"the state must flip only once the run actually unwound")

	assert.False(t, mgr.Cancel(id), "a terminal run is not cancellable")
}

// TestBackgroundCloseCleansUp is the process-exit requirement. Every running
// offload must be cancelled and waited for, and a call racing shutdown must
// NOT be adopted by a manager nobody will wait for.
func TestBackgroundCloseCleansUp(t *testing.T) {
	mgr := NewBackgroundManager()
	finished := make(chan bool, 1)

	tool := NewGuardedTool("shell_run", "Bash", "d", 30*time.Millisecond, nil,
		slowStream(100, 20*time.Millisecond, finished))
	out, err := tool.InvokableRun(bgCtx(t, mgr), `{}`)
	require.NoError(t, err)
	require.Contains(t, out, "moved to the background")

	assert.True(t, mgr.Close(), "every run must unwind within the grace period")
	select {
	case ok := <-finished:
		assert.False(t, ok, "Close must have cancelled it")
	case <-time.After(3 * time.Second):
		t.Fatal("the run outlived Close")
	}

	// After Close, Adopt refuses, so a racing call falls back to the ordinary
	// deadline rather than being detached with nobody to wait for it.
	assert.Nil(t, mgr.Adopt("shell_run", "{}", func() {}))
	after, err := tool.InvokableRun(bgCtx(t, mgr), `{"x":1}`)
	require.NoError(t, err)
	assert.NotContains(t, after, "moved to the background")

	assert.True(t, mgr.Close(), "Close is idempotent")
}

// TestCompletionNoticeIsNotAToolMessage is the pairing constraint stated as a
// test. The notice must be renderable as a USER message — it carries no tool
// call id and nothing about it invites being attached to one — because
// ctxcompact.EnforceToolCallPairs pairs tool results against assistant
// tool_calls, and this run's call was answered iterations ago by the offload
// acknowledgement.
func TestCompletionNoticeIsNotAToolMessage(t *testing.T) {
	run := BackgroundRun{
		ID: "bg-1", Tool: "run_tests", State: BackgroundCompleted,
		StartedAt: time.Now().Add(-2 * time.Minute), EndedAt: time.Now(),
		Result: "ok  github.com/x/y  118.4s",
	}
	notice := CompletionNotice(run)
	assert.Contains(t, notice, "<system-notification>",
		"a user-role message carrying tool output must say so, or the model reads it as the human speaking")
	assert.Contains(t, notice, "bg-1")
	assert.Contains(t, notice, "run_tests")
	assert.Contains(t, notice, "118.4s")

	msg := schema.UserMessage(notice)
	require.Equal(t, schema.User, msg.Role,
		"role=tool here would be an unpairable message: EnforceToolCallPairs cannot match it and providers reject it")
	require.Empty(t, msg.ToolCallID)
}

// TestCompletionNoticeCarriesFailure. A failed background run must report the
// error, not an empty body that reads as success.
func TestCompletionNoticeCarriesFailure(t *testing.T) {
	notice := CompletionNotice(BackgroundRun{
		ID: "bg-2", Tool: "shell_run", State: BackgroundFailed,
		StartedAt: time.Now(), EndedAt: time.Now(),
		Error: "exit status 2",
	})
	assert.Contains(t, notice, "failed")
	assert.Contains(t, notice, "exit status 2")

	empty := CompletionNotice(BackgroundRun{ID: "bg-3", Tool: "shell_run", State: BackgroundCompleted})
	assert.Contains(t, empty, "(no output)", "an empty body must say so rather than trailing off")
}

// TestDrainNoticesIsDestructive. A finished run must be announced exactly
// once; a read-without-clear would re-announce it on every iteration for the
// rest of the session.
func TestDrainNoticesIsDestructive(t *testing.T) {
	mgr := NewBackgroundManager()
	t.Cleanup(func() { mgr.Close() })

	h := mgr.Adopt("shell_run", "{}", func() {})
	require.NotNil(t, h)
	h.Finish("done", nil)

	first := mgr.DrainNotices()
	require.Len(t, first, 1)
	assert.Equal(t, BackgroundCompleted, first[0].State)
	assert.Nil(t, mgr.DrainNotices(), "a drained notice must not come back")
}

// TestFinishIsOnceOnly. The drain goroutine's normal exit and a hard-limit or
// shutdown cancel can race; a second Finish would overwrite a real result with
// the cancellation.
func TestFinishIsOnceOnly(t *testing.T) {
	mgr := NewBackgroundManager()
	t.Cleanup(func() { mgr.Close() })

	h := mgr.Adopt("run_tests", "{}", func() {})
	require.NotNil(t, h)
	h.Finish("the real result", nil)
	h.Finish("overwritten", errors.New("late cancel"))

	run, ok := mgr.Get(h.ID())
	require.True(t, ok)
	assert.Equal(t, "the real result", run.Result)
	assert.Equal(t, BackgroundCompleted, run.State)
	assert.Empty(t, run.Error)
}

// TestBackgroundStateIsTerminal_Table pins the enum's own predicate, which the
// idempotency check and Cancel both branch on.
func TestBackgroundStateIsTerminal_Table(t *testing.T) {
	cases := map[BackgroundState]bool{
		BackgroundRunning:   false,
		BackgroundCompleted: true,
		BackgroundFailed:    true,
		BackgroundCancelled: true,
	}
	for state, want := range cases {
		assert.Equal(t, want, state.IsTerminal(), string(state))
	}
}

// TestBackgroundResultIsSpilled. A background run is exactly the shape that
// produces a megabyte of test output; reinjecting it verbatim would spend the
// window the offload just bought.
func TestBackgroundResultIsSpilled(t *testing.T) {
	mgr := NewBackgroundManager()
	t.Cleanup(func() { mgr.Close() })

	huge := strings.Repeat("a line of test output that goes on and on\n", 4000)
	require.Greater(t, len(huge), SpillThreshold)

	tool := NewGuardedTool("run_tests", "Run tests", "d", 20*time.Millisecond, nil,
		func(ctx context.Context, _ string) <-chan ToolChunk {
			ch := make(chan ToolChunk)
			go func() {
				defer close(ch)
				time.Sleep(60 * time.Millisecond)
				select {
				case ch <- ToolChunk{Result: huge}:
				case <-ctx.Done():
				}
			}()
			return ch
		})

	_, err := tool.InvokableRun(bgCtx(t, mgr), `{}`)
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		runs := mgr.List()
		return len(runs) == 1 && runs[0].State.IsTerminal()
	}, 3*time.Second, 10*time.Millisecond)

	got := mgr.List()[0].Result
	assert.Less(t, len(got), SpillThreshold, "the reinjected result must be capped")
	assert.Contains(t, got, "[spilled:", "and must point at the full text")
}

// TestOffloadedResultIsRedactedBeforeFinish is the fix for a real leak found
// in W-A-02 fix round 1: InvokableRun's redactForModel choke point only sees
// the FOREGROUND path. A call promoted to the background finishes inside
// pumpOffloadable, which calls handle.Finish directly — bypassing
// InvokableRun entirely — and BackgroundRun.Result then reaches the model
// verbatim via hygiene.go's CompletionNotice injection. This drives the real
// offload path (real BackgroundManager, real GuardedTool.Stream ->
// streamWithOffload -> pumpOffloadable) end to end and asserts on
// BackgroundRun.Result, which is exactly the string handle.Finish received.
// It fails if the redactForModel call at guard_offload.go's handle.Finish
// site is reverted.
func TestOffloadedResultIsRedactedBeforeFinish(t *testing.T) {
	mgr := NewBackgroundManager()
	t.Cleanup(func() { mgr.Close() })

	const key = "sk-test-DEADBEEFdeadbeef0123456789"
	r := secrets.NewRedactor()
	r.Register(key)
	ctx := WithRedactor(bgCtx(t, mgr), r)

	tool := NewGuardedTool("shell_run", "Bash", "d", 20*time.Millisecond, nil,
		func(ctx context.Context, _ string) <-chan ToolChunk {
			ch := make(chan ToolChunk)
			go func() {
				defer close(ch)
				time.Sleep(60 * time.Millisecond) // past the 20ms foreground deadline
				select {
				case ch <- ToolChunk{Result: "OPENAI_API_KEY=" + key + "\nPATH=/usr/bin"}:
				case <-ctx.Done():
				}
			}()
			return ch
		})

	out, err := tool.InvokableRun(ctx, `{}`)
	require.NoError(t, err)
	require.Contains(t, out, "moved to the background")

	require.Eventually(t, func() bool {
		runs := mgr.List()
		return len(runs) == 1 && runs[0].State.IsTerminal()
	}, 2*time.Second, 10*time.Millisecond)

	got := mgr.List()[0].Result
	assert.NotContains(t, got, key,
		"a registered secret reached BackgroundRun.Result unredacted — this is the "+
			"exact leak CompletionNotice then forwards to the model verbatim")
	assert.Contains(t, got, "PATH=/usr/bin", "redaction must not eat the rest of the output")
}

// TestBackgroundToolsQueryTheManager covers the three model-facing tools.
func TestBackgroundToolsQueryTheManager(t *testing.T) {
	mgr := NewBackgroundManager()
	t.Cleanup(func() { mgr.Close() })
	ctx := bgCtx(t, mgr)
	bt := NewBackgroundTools()

	empty := drainChunks(bt.List.Stream(ctx, `{}`))
	assert.Contains(t, empty, "No tool calls have been moved to the background")

	h := mgr.Adopt("run_tests", `{"framework":"go"}`, func() {})
	require.NotNil(t, h)

	listed := drainChunks(bt.List.Stream(ctx, `{}`))
	assert.Contains(t, listed, h.ID())
	assert.Contains(t, listed, "running")

	got := drainChunks(bt.Result.Stream(ctx, `{"id":"`+h.ID()+`"}`))
	assert.Contains(t, got, "run_tests")
	assert.Contains(t, got, "elapsed")

	missing := drainChunks(bt.Result.Stream(ctx, `{"id":"bg-999"}`))
	assert.Contains(t, missing, "no background run with id bg-999")

	cancelled := drainChunks(bt.Cancel.Stream(ctx, `{"id":"`+h.ID()+`"}`))
	assert.Contains(t, cancelled, "Cancellation requested")

	h.Finish("", errors.New("stopped"))
	stale := drainChunks(bt.Cancel.Stream(ctx, `{"id":"`+h.ID()+`"}`))
	assert.Contains(t, stale, "unknown or already finished")
}

// TestBackgroundToolsWithoutAManager. In a sub-agent or headless invocation
// nothing can have been backgrounded, so the honest answer is "this scope has
// none" rather than a failure the model has to interpret.
func TestBackgroundToolsWithoutAManager(t *testing.T) {
	ctx := WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"*"}},
	})
	bt := NewBackgroundTools()
	for _, tc := range []struct {
		name string
		tool *GuardedTool
		args string
	}{
		{"list", bt.List, `{}`},
		{"result", bt.Result, `{"id":"bg-1"}`},
		{"cancel", bt.Cancel, `{"id":"bg-1"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Contains(t, drainChunks(tc.tool.Stream(ctx, tc.args)),
				"no background runs in this scope")
		})
	}
}

// TestNilBackgroundManagerIsSafe. WithBackgroundManager drops a nil, so every
// reader gets ok=false — but the methods must also survive a nil receiver,
// because a caller holding an *BackgroundManager field that was never set is
// the shape this repo's other nil-gated seams keep producing.
func TestNilBackgroundManagerIsSafe(t *testing.T) {
	var mgr *BackgroundManager
	assert.Nil(t, mgr.List())
	assert.Nil(t, mgr.DrainNotices())
	assert.Nil(t, mgr.Adopt("shell_run", "{}", nil))
	assert.False(t, mgr.Cancel("bg-1"))
	assert.True(t, mgr.Close())
	_, ok := mgr.Get("bg-1")
	assert.False(t, ok)
	_, running := mgr.Active("shell_run", "{}")
	assert.False(t, running)

	ctx := WithBackgroundManager(context.Background(), nil)
	_, bound := BackgroundManagerFromContext(ctx)
	assert.False(t, bound, "a nil manager must not appear bound")

	var h *BackgroundHandle
	assert.Empty(t, h.ID())
	h.Finish("x", nil) // must not panic
}
