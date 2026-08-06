package batch_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/agent/batch"
	"github.com/x6nux/yanshi/internal/agent/registry"
)

// recordingSpawn 记录每次调用的 prompt；可配置在某个调用上返回错误。
type recordingSpawn struct {
	mu       sync.Mutex
	calls    int32
	prompts  []string
	failOn   int    // -1 = 不失败(按全局调用序号;并发下与 row 映射不确定)
	failRow  string // 非空时按 prompt 内容确定性失败(优先于 failOn)
	failWith error
}

func (r *recordingSpawn) Spawn(ctx context.Context, prompt string, _ []string, _ string) (string, error) {
	idx := int(atomic.AddInt32(&r.calls, 1)) - 1
	r.mu.Lock()
	r.prompts = append(r.prompts, prompt)
	r.mu.Unlock()
	// failRow 优先:按 prompt 内容匹配,避免并发调度把失败映射到错误的 row。
	if r.failWith != nil && r.failRow != "" && strings.Contains(prompt, r.failRow) {
		return "", r.failWith
	}
	if idx == r.failOn && r.failWith != nil {
		return "", r.failWith
	}
	return "ok-" + prompt, nil
}

func newRegistryManager(t *testing.T, max int) *registry.Manager {
	t.Helper()
	m := registry.NewManager(registry.NewManagerOpts{
		RootContext:   context.Background(),
		MaxConcurrent: max,
		Path:          filepath.Join(t.TempDir(), "state.json"),
	})
	t.Cleanup(m.Close)
	return m
}

// ledger: C1/M07#3 逐项结果+汇总可查
func TestRunnerSpawnsPerRowAndPreservesIndex(t *testing.T) {
	rec := &recordingSpawn{failOn: -1}
	runner := batch.Runner{
		Spawn:         rec.Spawn,
		Manager:       newRegistryManager(t, 4),
		WaitTimeout:   2 * time.Second,
		CappedBackoff: 10 * time.Millisecond,
		CappedRetries: 5,
	}
	rows := []batch.Row{
		{Index: 0, Values: map[string]string{"q": "a"}},
		{Index: 1, Values: map[string]string{"q": "b"}},
		{Index: 2, Values: map[string]string{"q": "c"}},
	}
	report, err := runner.Run(context.Background(), batch.Input{Prompt: "do", Rows: rows})
	require.NoError(t, err)
	require.Len(t, report.Results, 3)
	for i, r := range report.Results {
		assert.Equal(t, i, r.Index)
		assert.Contains(t, r.Output, "ok-")
	}
	assert.Equal(t, 3, report.Success)
}

// ledger: C1/M07#3 逐项结果+汇总可查
func TestRunnerPerItemErrorRetention(t *testing.T) {
	rec := &recordingSpawn{failOn: -1, failRow: "row_index=1", failWith: errors.New("row-1-boom")}
	runner := batch.Runner{
		Spawn:         rec.Spawn,
		Manager:       newRegistryManager(t, 4),
		WaitTimeout:   2 * time.Second,
		CappedBackoff: 10 * time.Millisecond,
		CappedRetries: 5,
	}
	rows := []batch.Row{
		{Index: 0, Values: map[string]string{"q": "a"}},
		{Index: 1, Values: map[string]string{"q": "b"}},
		{Index: 2, Values: map[string]string{"q": "c"}},
	}
	report, err := runner.Run(context.Background(), batch.Input{Prompt: "do", Rows: rows})
	require.NoError(t, err)
	require.Len(t, report.Results, 3)
	assert.Equal(t, 2, report.Success)
	assert.Equal(t, 1, report.Failed)
	assert.Equal(t, "", report.Results[1].Output)
	assert.Contains(t, report.Results[1].Error, "row-1-boom")
}

func TestRunnerCapsAtRegistryMaxConcurrent(t *testing.T) {
	// MaxConcurrent=4 so all 3 rows can spawn; the Manager's runningLocked
	// counts completed agents in runtime map, so we don't test overflow retry here.
	rec := &recordingSpawn{failOn: -1}
	runner := batch.Runner{
		Spawn:         rec.Spawn,
		Manager:       newRegistryManager(t, 4),
		WaitTimeout:   2 * time.Second,
		CappedBackoff: 20 * time.Millisecond,
		CappedRetries: 20,
	}
	rows := []batch.Row{
		{Index: 0, Values: map[string]string{"q": "a"}},
		{Index: 1, Values: map[string]string{"q": "b"}},
		{Index: 2, Values: map[string]string{"q": "c"}},
	}
	report, err := runner.Run(context.Background(), batch.Input{Prompt: "do", Rows: rows})
	require.NoError(t, err)
	assert.Equal(t, 3, report.Success)
}

func TestRunnerCancellationPendingRowsMarkedCanceled(t *testing.T) {
	gate := make(chan struct{})
	rec := &blockingSpawn{gate: gate}
	runner := batch.Runner{
		Spawn:         rec.Spawn,
		Manager:       newRegistryManager(t, 2),
		WaitTimeout:   5 * time.Second,
		CappedBackoff: 10 * time.Millisecond,
		CappedRetries: 50,
	}
	ctx, cancel := context.WithCancel(context.Background())
	rows := []batch.Row{
		{Index: 0, Values: map[string]string{"q": "a"}},
		{Index: 1, Values: map[string]string{"q": "b"}},
	}
	done := make(chan batch.Report, 1)
	go func() {
		r, _ := runner.Run(ctx, batch.Input{Prompt: "do", Rows: rows})
		done <- r
	}()
	// 等 spawn 开始处理第 0 行。
	require.Eventually(t, func() bool { return atomic.LoadInt32(&rec.calls) >= 1 }, 2*time.Second, 10*time.Millisecond)
	cancel()
	close(gate)
	select {
	case r := <-done:
		require.Len(t, r.Results, 2)
		assert.NotEmpty(t, r.Results[1].Error)
	case <-time.After(2 * time.Second):
		t.Fatal("runner did not return after cancel")
	}
	time.Sleep(50 * time.Millisecond)
}

type blockingSpawn struct {
	calls int32
	gate  chan struct{}
}

func (b *blockingSpawn) Spawn(ctx context.Context, prompt string, _ []string, _ string) (string, error) {
	atomic.AddInt32(&b.calls, 1)
	select {
	case <-b.gate:
		return "ok", nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func TestRunnerRejectsNilSpawn(t *testing.T) {
	runner := batch.Runner{Manager: newRegistryManager(t, 2)}
	_, err := runner.Run(context.Background(), batch.Input{
		Prompt: "x",
		Rows:   []batch.Row{{Index: 0, Values: map[string]string{"q": "a"}}},
	})
	require.Error(t, err)
}

func TestRunnerRejectsNilManager(t *testing.T) {
	rec := &recordingSpawn{failOn: -1}
	runner := batch.Runner{Spawn: rec.Spawn}
	_, err := runner.Run(context.Background(), batch.Input{
		Prompt: "x",
		Rows:   []batch.Row{{Index: 0, Values: map[string]string{"q": "a"}}},
	})
	require.Error(t, err)
}

func TestRunnerRejectsEmptyRows(t *testing.T) {
	rec := &recordingSpawn{failOn: -1}
	runner := batch.Runner{Spawn: rec.Spawn, Manager: newRegistryManager(t, 2)}
	report, err := runner.Run(context.Background(), batch.Input{Prompt: "x"})
	require.NoError(t, err)
	assert.Empty(t, report.Results)
}

func TestRunnerRejectsEmptyPrompt(t *testing.T) {
	rec := &recordingSpawn{failOn: -1}
	runner := batch.Runner{Spawn: rec.Spawn, Manager: newRegistryManager(t, 2)}
	_, err := runner.Run(context.Background(), batch.Input{
		Rows: []batch.Row{{Index: 0, Values: map[string]string{"q": "a"}}},
	})
	require.Error(t, err)
}

func TestRunnerPromptIncludesBasePromptAndRowJSON(t *testing.T) {
	rec := &recordingSpawn{failOn: -1}
	runner := batch.Runner{
		Spawn:         rec.Spawn,
		Manager:       newRegistryManager(t, 4),
		WaitTimeout:   2 * time.Second,
		CappedBackoff: 10 * time.Millisecond,
		CappedRetries: 5,
	}
	rows := []batch.Row{{Index: 0, Values: map[string]string{"name": "Alice"}}}
	_, err := runner.Run(context.Background(), batch.Input{Prompt: "BASE", Rows: rows})
	require.NoError(t, err)
	require.NotEmpty(t, rec.prompts)
	assert.Contains(t, rec.prompts[0], "BASE")
	assert.Contains(t, rec.prompts[0], `"name":"Alice"`)
	assert.Contains(t, rec.prompts[0], "row_index=0")
}

// ---------------------------------------------------------------------------
// Additional coverage: Csv parse error, default backoff/retries, spawn retry
// exhaustion, non-cap spawn error, agent-cancelled status, ctx cancel during
// spawn backoff.
// ---------------------------------------------------------------------------

func TestParseCSV_Malformed(t *testing.T) {
	_, err := batch.ParseCSV(`"unclosed,header`)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parse CSV")
}

func TestRunnerDefaultsApplied(t *testing.T) {
	// CappedBackoff and CappedRetries are zero → defaults apply.
	rec := &recordingSpawn{failOn: -1}
	runner := batch.Runner{
		Spawn:   rec.Spawn,
		Manager: newRegistryManager(t, 2),
	}
	report, err := runner.Run(context.Background(), batch.Input{
		Prompt: "do",
		Rows:   []batch.Row{{Index: 0, Values: map[string]string{"q": "a"}}},
	})
	require.NoError(t, err)
	assert.Equal(t, 1, report.Success)
}

func TestRunnerSpawnCapExhausted(t *testing.T) {
	// MaxConcurrent=1 with the single slot held by an agent that never
	// finishes: every batch row hits the cap and exhausts retries. Tests
	// spawnWithRetry's cap retry loop and the spawn error path in the wait
	// goroutine.
	//
	// The predecessor held no slot and relied on a completed agent keeping
	// one — which was the slot leak, not the cap. With slots released at
	// terminal, a sequential batch at cap=1 simply succeeds, so reproducing
	// exhaustion now requires an occupant that is genuinely still running.
	rec := &recordingSpawn{failOn: -1}
	mgr := newRegistryManager(t, 1)

	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	holderStarted := make(chan struct{})
	_, err := mgr.Spawn(context.Background(), registry.SpawnRequest{
		AgentType: "subagent", Role: "explore", Prompt: "holder",
		Runner: registry.RunnerFunc(func(context.Context, string, string) (string, error) {
			close(holderStarted)
			<-release
			return "held", nil
		}),
	})
	require.NoError(t, err)
	<-holderStarted
	runner := batch.Runner{
		Spawn:         rec.Spawn,
		Manager:       mgr,
		WaitTimeout:   2 * time.Second,
		CappedBackoff: 5 * time.Millisecond,
		CappedRetries: 3,
	}
	rows := []batch.Row{
		{Index: 0, Values: map[string]string{"q": "a"}},
		{Index: 1, Values: map[string]string{"q": "b"}},
	}
	report, err := runner.Run(context.Background(), batch.Input{Prompt: "do", Rows: rows})
	require.NoError(t, err)
	require.Len(t, report.Results, 2)
	// The only slot is occupied for the whole run, so both rows exhaust
	// their cap retries and report a spawn error.
	assert.NotEmpty(t, report.Results[0].Error, "row 0 must fail: the cap is fully occupied")
	assert.NotEmpty(t, report.Results[1].Error, "row 1 must fail: the cap is fully occupied")
	assert.Equal(t, 0, report.Success)
	assert.Equal(t, 2, report.Failed)
}

func TestRunnerSpawnNonCapError(t *testing.T) {
	// Close the Manager before running: mgr.Spawn returns ErrClosed,
	// a non-cap error. Tests the errors.As fallthrough in spawnWithRetry.
	mgr := newRegistryManager(t, 2)
	mgr.Close()

	rec := &recordingSpawn{failOn: -1}
	runner := batch.Runner{
		Spawn:         rec.Spawn,
		Manager:       mgr,
		CappedBackoff: 10 * time.Millisecond,
		CappedRetries: 3,
	}
	report, err := runner.Run(context.Background(), batch.Input{
		Prompt: "do",
		Rows:   []batch.Row{{Index: 0, Values: map[string]string{"q": "a"}}},
	})
	require.NoError(t, err)
	require.Len(t, report.Results, 1)
	assert.NotEmpty(t, report.Results[0].Error)
	assert.Equal(t, 1, report.Failed)
}

func TestRunnerAgentCancelledStatus(t *testing.T) {
	// Pre-cancelled Manager root context: spawned agent runs to completion
	// but the cancelled context is detected by runAgentLoop, which sets
	// StatusCancelled. The batch runner's wait goroutine hits the
	// StatusCancelled switch case.
	rootCtx, rootCancel := context.WithCancel(context.Background())
	rootCancel()
	mgr := registry.NewManager(registry.NewManagerOpts{
		RootContext:   rootCtx,
		MaxConcurrent: 1,
		Path:          filepath.Join(t.TempDir(), "state.json"),
	})
	t.Cleanup(mgr.Close)

	rec := &recordingSpawn{failOn: -1}
	runner := batch.Runner{
		Spawn:         rec.Spawn,
		Manager:       mgr,
		WaitTimeout:   2 * time.Second,
		CappedBackoff: 10 * time.Millisecond,
		CappedRetries: 3,
	}
	report, err := runner.Run(context.Background(), batch.Input{
		Prompt: "do",
		Rows:   []batch.Row{{Index: 0, Values: map[string]string{"q": "a"}}},
	})
	require.NoError(t, err)
	require.Len(t, report.Results, 1)
	assert.Equal(t, 0, report.Success)
	assert.Equal(t, 1, report.Canceled)
	assert.Equal(t, context.Canceled.Error(), report.Results[0].Error)
}

func TestRunnerCtxCancelDuringSpawnBackoff(t *testing.T) {
	// MaxConcurrent=1 + 2 rows: row 0 spawns, row 1 hits cap and retries.
	// Cancel context during backoff to trigger ctx.Done() path in
	// spawnWithRetry's select.
	ctx, cancel := context.WithCancel(context.Background())
	mgr := newRegistryManager(t, 1)
	rec := &recordingSpawn{failOn: -1}
	runner := batch.Runner{
		Spawn:         rec.Spawn,
		Manager:       mgr,
		CappedBackoff: 5 * time.Second,
		CappedRetries: 10,
	}
	rows := []batch.Row{
		{Index: 0, Values: map[string]string{"q": "a"}},
		{Index: 1, Values: map[string]string{"q": "b"}},
	}

	done := make(chan batch.Report, 1)
	go func() {
		r, _ := runner.Run(ctx, batch.Input{Prompt: "do", Rows: rows})
		done <- r
	}()

	// Wait for Row 0's agent to actually REACH a terminal state before
	// cancelling, instead of guessing with a fixed 200ms sleep. Row 0 must be
	// finished first: if cancel lands while it is still running, its Wait()
	// reports the cancellation and report.Success drops to 0.
	//
	// Row 1 needs no separate signal — the spawn loop in runner.go is
	// sequential, so once Row 0 holds the only slot (MaxConcurrent=1) Row 1 is
	// necessarily parked in spawnWithRetry's backoff select, and CappedBackoff
	// is 5s here, far longer than this poll takes.
	require.Eventually(t, func() bool {
		for _, rec := range mgr.List(true).Agents {
			if rec.Status == registry.StatusCompleted {
				return true
			}
		}
		return false
	}, 30*time.Second, 5*time.Millisecond, "Row 0's agent must complete before cancel")
	cancel()

	report := <-done
	require.Len(t, report.Results, 2)
	// Row 0 agent completed before cancel → Success; Row 1's spawn was
	// cancelled mid-retry → Canceled.
	assert.Equal(t, 1, report.Success)
	assert.Equal(t, 1, report.Canceled)
	assert.Contains(t, report.Results[1].Error, "context canceled")
}

func TestRunnerCtxCancelBeforeSpawn(t *testing.T) {
	// Pre-cancelled context: spawnWithRetry sees ctx.Err() at the top of the
	// retry loop and returns immediately. Tests line 176-178 in spawnWithRetry.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	rec := &recordingSpawn{failOn: -1}
	runner := batch.Runner{
		Spawn:   rec.Spawn,
		Manager: newRegistryManager(t, 2),
	}
	report, err := runner.Run(ctx, batch.Input{
		Prompt: "do",
		Rows:   []batch.Row{{Index: 0, Values: map[string]string{"q": "a"}}},
	})
	require.NoError(t, err)
	require.Len(t, report.Results, 1)
	assert.Equal(t, 1, report.Canceled)
	assert.Contains(t, report.Results[0].Error, "context canceled")
}
