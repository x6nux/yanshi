package orchestrator

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/schema"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/guard"
	einollm "github.com/x6nux/yanshi/internal/llm/eino"
	"github.com/x6nux/yanshi/internal/tools"
)

// wfs22Harness builds two GuardedTools ("par_a" / "par_b") whose bodies record
// their start (enter callback), then block until release closes or ctx is done.
type wfs22Harness struct {
	enter   func(name string)
	release chan struct{}
	ran     sync.Map // name -> true, set by the tool body
	tools   []BaseTool
}

func newWFS22Harness(enter func(name string)) *wfs22Harness {
	h := &wfs22Harness{enter: enter, release: make(chan struct{})}
	body := func(name string) tools.StreamFunc {
		return tools.SyncStream(func(ctx context.Context, _ string) (string, error) {
			h.ran.Store(name, true)
			if h.enter != nil {
				h.enter(name)
			}
			select {
			case <-h.release:
				return name + " ok", nil
			case <-ctx.Done():
				return "", ctx.Err()
			}
		})
	}
	h.tools = []BaseTool{
		tools.NewGuardedTool("par_a", "Par A", "parallel test tool a", 10_000_000, nil, body("par_a")),
		tools.NewGuardedTool("par_b", "Par B", "parallel test tool b", 10_000_000, nil, body("par_b")),
	}
	return h
}

func (h *wfs22Harness) ranNames() []string {
	var out []string
	h.ran.Range(func(k, _ any) bool {
		out = append(out, k.(string))
		return true
	})
	return out
}

func wfs22Profile(allow ...string) guard.PermissionProfile {
	return guard.PermissionProfile{Tools: guard.ToolsPerm{Allow: allow}}
}

func wfs22Script(final string) *einollm.FakeModel {
	both := schema.AssistantMessage("", []schema.ToolCall{
		{ID: "c1", Type: "function", Function: schema.FunctionCall{Name: "par_a", Arguments: `{}`}},
		{ID: "c2", Type: "function", Function: schema.FunctionCall{Name: "par_b", Arguments: `{}`}},
	})
	return einollm.NewFakeModelWithMessages([]*schema.Message{both, schema.AssistantMessage(final, nil)}, nil)
}

func drainToolResults(t *testing.T, iter *adk.AsyncIterator[*adk.AgentEvent]) []string {
	t.Helper()
	var results []string
	for {
		ev, ok := iter.Next()
		if !ok {
			break
		}
		require.NoError(t, ev.Err)
		if ev.Output == nil || ev.Output.MessageOutput == nil {
			continue
		}
		mv := ev.Output.MessageOutput
		var msg *schema.Message
		if mv.IsStreaming && mv.MessageStream != nil {
			m, err := mv.GetMessage()
			if err != nil || m == nil {
				continue
			}
			msg = m
		} else {
			msg = mv.Message
		}
		if msg != nil && msg.Role == schema.Tool {
			results = append(results, msg.Content)
		}
	}
	return results
}

// TestWFS22ToolCallsRunInParallel pins the dispatch clause: two tool calls in
// ONE model message overlap in time. Each body signals enter() and then blocks
// until BOTH have arrived; a sequential dispatcher never produces the second
// signal and the test times out red.
//
// 变异：给 runnerFor 里 ToolsNodeConfig 设 ExecuteSequentially: true（串行分
// 发），本测试在 5s 超出处变红。
func TestWFS22ToolCallsRunInParallel(t *testing.T) {
	arrived := make(chan string, 2)
	h := newWFS22Harness(func(name string) { arrived <- name })
	mdl := wfs22Script("done")

	o, err := New(Config{Model: mdl, Tools: h.tools, Profile: wfs22Profile("par_a", "par_b")})
	require.NoError(t, err)

	done := make(chan error, 1)
	go func() {
		_, err := o.Query(context.Background(), "run both")
		done <- err
	}()

	// Both bodies must be running CONCURRENTLY: two arrivals while neither
	// has been released. 5s is generous for an in-process barrier.
	for i := 0; i < 2; i++ {
		select {
		case <-arrived:
		case <-time.After(5 * time.Second):
			close(h.release)
			t.Fatalf("only %d of 2 tool bodies started before the timeout — dispatch is not parallel", i)
		}
	}
	close(h.release)
	require.NoError(t, <-done)

	got := h.ranNames()
	assert.Len(t, got, 2, "both parallel calls must complete, got %v", got)
}

// TestWFS22EachParallelCallIsAuthorized pins the authorization clause: in one
// parallel batch, the allowed tool runs and the unallowed tool is DENIED. A
// dispatcher that skipped Authorize for one of the parallel calls would let
// par_b execute and this goes red.
//
// 变异：删掉 GuardedTool.Stream 里的 Authorize/AuthorizeApprovalRequired 调用
// （或只短路并行批次里的第二个调用），本测试变红 —— par_b 的 body 会执行、
// ran 里出现 par_b、结果不再带 permission denied。
func TestWFS22EachParallelCallIsAuthorized(t *testing.T) {
	h := newWFS22Harness(nil)
	close(h.release) // bodies (if they run) return immediately

	mdl := wfs22Script("done")
	// par_b is NOT in the allow list.
	o, err := New(Config{Model: mdl, Tools: h.tools, Profile: wfs22Profile("par_a")})
	require.NoError(t, err)

	joined := strings.Join(drainToolResults(t, o.Events(context.Background(), "run both")), "\n")

	assert.NotContains(t, h.ranNames(), "par_b",
		"par_b is not allowed by the profile; its body ran anyway — the parallel path skipped Authorize")
	assert.Contains(t, joined, "permission denied",
		"par_b's denial must feed back as a tool result: %s", joined)
	assert.Contains(t, joined, "par_a ok", "the allowed tool must run normally")
}

// TestWFS22CancellationReachesParallelCalls pins cancellation propagation:
// both in-flight calls observe the turn context being cancelled and the turn
// ends instead of hanging.
func TestWFS22CancellationReachesParallelCalls(t *testing.T) {
	started := make(chan string, 2)
	h := newWFS22Harness(func(name string) { started <- name })
	mdl := wfs22Script("unreachable — must be cut off by the cancel")

	o, err := New(Config{Model: mdl, Tools: h.tools, Profile: wfs22Profile("par_a", "par_b")})
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		// The turn ends when cancellation unwinds it; the outcome (Go error or
		// truncated result) is the ADK's choice — the assertion is that it
		// ENDS instead of hanging.
		_, _ = o.Query(ctx, "run both")
	}()

	// Wait until both bodies are inside their blocking select, then cancel.
	for i := 0; i < 2; i++ {
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			cancel()
			t.Fatalf("only %d of 2 bodies started — cannot exercise parallel cancellation", i)
		}
	}
	cancel()

	select {
	case <-done:
		// The turn ended. Hanging is the failure being tested for.
	case <-time.After(5 * time.Second):
		t.Fatal("turn did not end after cancellation")
	}
	close(h.release) // let any straggler body return
}

// TestWFS22ParallelFailuresKeepBreakerConsistent drives a parallel batch whose
// two calls FAIL SIMULTANEOUSLY (a barrier holds both bodies until both are
// running, then releases them together) and asserts both feed back as results
// without aborting the turn. The simultaneous release is what makes this a
// -race guard for the W-F-22 counter fix: the consecutive-error breaker is
// shared per turn, so two overlapping InvokableRun calls mutating a plain
// *int was a data race (add/store are atomic now).
func TestWFS22ParallelFailuresKeepBreakerConsistent(t *testing.T) {
	started := make(chan string, 2)
	release := make(chan struct{})
	body := func(name string) tools.StreamFunc {
		return tools.SyncStream(func(ctx context.Context, _ string) (string, error) {
			started <- name
			<-release
			return "", assert.AnError
		})
	}
	failA := tools.NewGuardedTool("par_a", "Par A", "parallel failing tool a", 10_000_000, nil, body("par_a"))
	failB := tools.NewGuardedTool("par_b", "Par B", "parallel failing tool b", 10_000_000, nil, body("par_b"))
	mdl := wfs22Script("done")
	o, err := New(Config{Model: mdl, Tools: []BaseTool{failA, failB}, Profile: wfs22Profile("par_a", "par_b")})
	require.NoError(t, err)

	go func() {
		for i := 0; i < 2; i++ {
			select {
			case <-started:
			case <-time.After(5 * time.Second):
				close(release)
				return
			}
		}
		close(release) // both bodies wake together → overlapping counter updates
	}()

	// The consecutive-error breaker is bound per turn at the TRANSPORT layer
	// (ws.go / chat.go bind it before driving the orchestrator; the
	// orchestrator itself does not). Bind it the same way production does so
	// the parallel failing calls actually share one counter.
	ctx := tools.WithErrCounter(context.Background())
	joined := strings.Join(drainToolResults(t, o.Events(ctx, "run both")), "\n")
	assert.Contains(t, joined, "✗", "both failing calls must feed error results back to the model")
}
