package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/agent/registry"
	"github.com/x6nux/yanshi/internal/guard"
)

// drainTool collects a lifecycle tool's streamed result.
func drainTool(ch <-chan ToolChunk) string {
	var b strings.Builder
	for c := range ch {
		b.WriteString(c.Result)
	}
	return b.String()
}

// lifecycleCtx binds a Manager and a permissive profile — everything the agent
// lifecycle tools read out of context.
func lifecycleCtx(mgr *registry.Manager) context.Context {
	ctx := WithManager(context.Background(), mgr)
	return WithProfile(ctx, guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"*"}},
	})
}

// TestLifecycleToolsHaveObservableEffects is the lifecycle clause, asserted on
// what each tool DID rather than on it not panicking.
//
// agent_lifecycle_full_test.go had five tests of this shape:
//
//	ch := tools.streamAgentSendInput(ctx, `{"agent_id":"nonexistent",...}`)
//	for range ch {
//	}
//
// — a spy with no assertion at all, on a non-existent agent, so the tool could
// have been an empty function body. Two more swallowed their result (`_ =
// result`) and one reported failure with t.Logf, which never fails a test. The
// clause is "全部生命周期操作可用", and none of those could tell working from
// gutted.
//
// Each sub-test below names the state change it looks for.
//
// ledger: B1/M04#1 全部生命周期操作可用
func TestLifecycleToolsHaveObservableEffects(t *testing.T) {
	at := NewAgentTools(nil)

	t.Run("send_input actually queues the text", func(t *testing.T) {
		mgr, _ := newTestManager(t)
		release := make(chan struct{})
		t.Cleanup(func() { close(release) })
		id, err := mgr.Spawn(context.Background(), registry.SpawnRequest{
			Prompt: "p",
			Runner: registry.RunnerFunc(func(ctx context.Context, _, _ string) (string, error) {
				// Never reads its mailbox, so everything sent stays queued and
				// the queue depth becomes observable.
				select {
				case <-release:
				case <-ctx.Done():
				}
				return "done", nil
			}),
		})
		require.NoError(t, err)

		// The mailbox is bounded. Filling it is the only way a caller outside
		// the registry can see that the text went somewhere: a tool that
		// accepted the input and dropped it would never fill anything.
		ctx := lifecycleCtx(mgr)
		var lastErr string
		for i := 0; i < 32; i++ {
			out := drainTool(at.streamAgentSendInput(ctx,
				`{"agent_id":"`+id+`","text":"FOLLOW_UP"}`))
			require.NotContains(t, out, "not found", out)
			if strings.Contains(out, "full") || strings.Contains(out, "busy") ||
				strings.Contains(out, "error") {
				lastErr = out
				break
			}
		}
		require.NotEmpty(t, lastErr,
			"32 follow-ups were all accepted by an agent that never reads its mailbox; "+
				"the text is being dropped rather than queued")
	})

	t.Run("assign changes the recorded assignment", func(t *testing.T) {
		mgr, _ := newTestManager(t)
		release := make(chan struct{})
		t.Cleanup(func() { close(release) })
		id, err := mgr.Spawn(context.Background(), registry.SpawnRequest{
			Prompt: "p",
			Runner: registry.RunnerFunc(func(ctx context.Context, _, _ string) (string, error) {
				select {
				case <-release:
				case <-ctx.Done():
				}
				return "done", nil
			}),
		})
		require.NoError(t, err)

		out := drainTool(at.streamAgentAssign(lifecycleCtx(mgr),
			`{"agent_id":"`+id+`","assignment":"NEW_ASSIGNMENT"}`))
		require.NotContains(t, out, "not found", out)

		rec, ok := mgr.Result(id)
		require.True(t, ok)
		assert.Equal(t, "NEW_ASSIGNMENT", rec.Assignment,
			"agent_assign returned successfully without changing the assignment")
	})

	t.Run("cancel reaches a terminal status", func(t *testing.T) {
		mgr, _ := newTestManager(t)
		id, err := mgr.Spawn(context.Background(), registry.SpawnRequest{
			Prompt: "p",
			Runner: registry.RunnerFunc(func(ctx context.Context, _, _ string) (string, error) {
				<-ctx.Done()
				return "", ctx.Err()
			}),
		})
		require.NoError(t, err)

		out := drainTool(at.streamAgentCancel(lifecycleCtx(mgr), `{"agent_id":"`+id+`"}`))
		require.NotContains(t, out, "not found", out)

		rec, err := mgr.Wait(context.Background(), id, registry.WaitOpts{Timeout: 5 * time.Second})
		require.NoError(t, err)
		assert.True(t, rec.Status.Terminal(),
			"agent_cancel returned but the agent is still %s", rec.Status)
		assert.Equal(t, registry.StatusCancelled, rec.Status)
	})

	t.Run("wait blocks until terminal and reports the outcome", func(t *testing.T) {
		mgr, _ := newTestManager(t)
		release := make(chan struct{})
		id, err := mgr.Spawn(context.Background(), registry.SpawnRequest{
			Prompt: "p",
			Runner: registry.RunnerFunc(func(ctx context.Context, _, _ string) (string, error) {
				select {
				case <-release:
				case <-ctx.Done():
				}
				return "THE_ANSWER", nil
			}),
		})
		require.NoError(t, err)

		done := make(chan string, 1)
		go func() {
			done <- drainTool(at.streamAgentWait(lifecycleCtx(mgr),
				`{"agent_id":"`+id+`","timeout_s":10}`))
		}()

		// Still running: wait must not have returned yet. A wait that returns
		// immediately would satisfy every assertion below.
		select {
		case out := <-done:
			t.Fatalf("agent_wait returned while the agent was still running: %s", out)
		case <-time.After(100 * time.Millisecond):
		}

		close(release)
		select {
		case out := <-done:
			assert.Contains(t, out, "THE_ANSWER",
				"agent_wait returned without the agent's result")
		case <-time.After(5 * time.Second):
			t.Fatal("agent_wait never returned after the agent finished")
		}
	})

	t.Run("result carries the finished agent's output", func(t *testing.T) {
		mgr, _ := newTestManager(t)
		id, err := mgr.Spawn(context.Background(), registry.SpawnRequest{
			Prompt: "p",
			Runner: registry.RunnerFunc(func(context.Context, string, string) (string, error) {
				return "FINAL_OUTPUT", nil
			}),
		})
		require.NoError(t, err)
		_, err = mgr.Wait(context.Background(), id, registry.WaitOpts{Timeout: 5 * time.Second})
		require.NoError(t, err)

		out := drainTool(at.streamAgentResult(lifecycleCtx(mgr), `{"agent_id":"`+id+`"}`))
		assert.Contains(t, out, "FINAL_OUTPUT",
			"agent_result returned without the result it exists to return")
	})
}

// TestLifecycleToolsRejectUnknownAgents is the error path the five spy tests
// were pointed at, with the assertion they were missing.
//
// They all passed "nonexistent" and asserted nothing, so a tool that silently
// succeeded on an unknown id — reporting to the model that a follow-up had been
// delivered to an agent that does not exist — read exactly the same.
//
// ledger: B1/M04#1 全部生命周期操作可用
func TestLifecycleToolsRejectUnknownAgents(t *testing.T) {
	mgr, _ := newTestManager(t)
	ctx := lifecycleCtx(mgr)
	at := NewAgentTools(nil)

	for name, run := range map[string]func() string{
		"wait":       func() string { return drainTool(at.streamAgentWait(ctx, `{"agent_id":"ghost"}`)) },
		"result":     func() string { return drainTool(at.streamAgentResult(ctx, `{"agent_id":"ghost"}`)) },
		"send_input": func() string { return drainTool(at.streamAgentSendInput(ctx, `{"agent_id":"ghost","text":"x"}`)) },
		"assign":     func() string { return drainTool(at.streamAgentAssign(ctx, `{"agent_id":"ghost","assignment":"x"}`)) },
		"cancel":     func() string { return drainTool(at.streamAgentCancel(ctx, `{"agent_id":"ghost"}`)) },
	} {
		t.Run(name, func(t *testing.T) {
			out := run()
			require.NotEmpty(t, out,
				"agent_%s said nothing about an agent that does not exist; the model reads "+
					"silence as success", name)
			assert.Containsf(t, out, "not found",
				"agent_%s did not say the agent does not exist: %q. \"not running\" reads as "+
					"\"it exists and is idle\", so a model that hallucinated an id goes on to "+
					"ask for its result", name, out)
		})
	}
}

// TestManagedSubAgentUsageReachesTheRegistry is the usage half of the
// queryability clause.
//
// tools.UsageSinkFrom had zero production callers: managedTurnRunner bound a
// sink that forwarded to mgr.AddUsage, and nothing ever CALLED the sink, so
// agent_list and agent_result reported Usage{} for every sub-agent no matter
// how many tokens it burned. The pre-existing test called mgr.AddUsage by hand,
// which is the broken segment skipped over.
//
// This drives the sink the way production does — the runner reports through the
// context — and then reads the registry back.
//
// ledger: B1/M04#2 线程树/深度/并发/usage 可查
func TestManagedSubAgentUsageReachesTheRegistry(t *testing.T) {
	mgr, _ := newTestManager(t)

	id, err := mgr.Spawn(context.Background(), registry.SpawnRequest{
		Prompt: "p",
		Runner: registry.RunnerFunc(func(ctx context.Context, agentID, _ string) (string, error) {
			// What managedTurnRunner binds, and what runSubAgentTurn calls at
			// the end of a nested turn.
			ctx = WithUsageSink(ctx, UsageSink(func(u registry.Usage) {
				_ = mgr.AddUsage(agentID, u)
			}))
			sink := UsageSinkFrom(ctx)
			require.NotNil(t, sink, "the usage sink is not readable from the runner's context")
			sink(registry.Usage{PromptTokens: 120, CompletionTokens: 30, TotalTokens: 150})
			return "ok", nil
		}),
	})
	require.NoError(t, err)
	_, err = mgr.Wait(context.Background(), id, registry.WaitOpts{Timeout: 5 * time.Second})
	require.NoError(t, err)

	rec, ok := mgr.Result(id)
	require.True(t, ok)
	assert.Equal(t, int64(150), rec.Usage.TotalTokens,
		"the sub-agent's spend never reached the registry, so agent_list and agent_result "+
			"report zero tokens for work that cost real money")
	assert.Equal(t, int64(120), rec.Usage.PromptTokens)
	assert.Equal(t, int64(30), rec.Usage.CompletionTokens)

	// And it is visible through the tool the model actually calls.
	out := drainTool(NewAgentTools(nil).streamAgentResult(lifecycleCtx(mgr),
		`{"agent_id":"`+id+`"}`))
	var payload map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &payload), "agent_result output: %s", out)
	assert.Contains(t, out, "150", "agent_result does not surface the usage it recorded: %s", out)
}

// TestCancelledAgentsLeaveNothingBehind is the leak clause.
//
// finishTerminal used to change records[id].Status and nothing else: no
// delete(m.runtime, id), no rt.cancel(). Every terminated agent left a
// runtimeAgent behind — a mailbox channel, an event sink, and a child context
// that was never cancelled — and because runningLocked counts runtime entries,
// the cap became a lifetime budget.
//
// The test that nominally covered this,
// registry::TestManager_ConcurrentSpawnCancel, could not run at all: it passed
// t.TempDir() (a DIRECTORY) as the state file path, so writeAtomic failed on
// every Spawn, every id was filtered out by `if err == nil`, and the Cancel
// loop below it executed zero times. It also asserted nothing.
//
// ledger: B1/M04#3 取消不泄漏
func TestCancelledAgentsLeaveNothingBehind(t *testing.T) {
	mgr, _ := newTestManager(t)
	at := NewAgentTools(nil)
	ctx := lifecycleCtx(mgr)

	const n = 4
	ids := make([]string, 0, n)
	for i := 0; i < n; i++ {
		id, err := mgr.Spawn(context.Background(), registry.SpawnRequest{
			Prompt: "p",
			Runner: registry.RunnerFunc(func(rctx context.Context, _, _ string) (string, error) {
				<-rctx.Done()
				return "", rctx.Err()
			}),
		})
		require.NoErrorf(t, err, "spawn %d failed, so this test would assert on an empty set", i)
		ids = append(ids, id)
	}
	require.Equal(t, n, mgr.List(false).Running)

	for _, id := range ids {
		out := drainTool(at.streamAgentCancel(ctx, `{"agent_id":"`+id+`"}`))
		require.NotContains(t, out, "not found", out)
	}
	for _, id := range ids {
		_, err := mgr.Wait(context.Background(), id, registry.WaitOpts{Timeout: 5 * time.Second})
		require.NoError(t, err)
	}

	assert.Equal(t, 0, mgr.List(false).Running,
		"cancelled agents are still counted as running")

	// The black-box consequence: with the slots leaked, this Spawn is refused.
	extra, err := mgr.Spawn(context.Background(), registry.SpawnRequest{
		Prompt: "p",
		Runner: registry.RunnerFunc(func(context.Context, string, string) (string, error) {
			return "ok", nil
		}),
	})
	require.NoError(t, err,
		"a spawn was refused after every agent had been cancelled: the cancelled agents "+
			"still hold their concurrency slots")
	_, err = mgr.Wait(context.Background(), extra, registry.WaitOpts{Timeout: 5 * time.Second})
	require.NoError(t, err)
}
