package registry

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// context.go — WithCurrentAgentID, WithRole, RoleFromContext
// ---------------------------------------------------------------------------

func TestContextRoundtrip(t *testing.T) {
	ctx := WithCurrentAgentID(context.Background(), "ag-42")
	assert.Equal(t, "ag-42", CurrentAgentID(ctx))
	// Unbound yields empty.
	assert.Empty(t, CurrentAgentID(context.Background()))
}

func TestWithRoleRoundtrip(t *testing.T) {
	ctx := WithRole(context.Background(), "tester")
	assert.Equal(t, "tester", RoleFromContext(ctx))
	// Unbound yields empty.
	assert.Empty(t, RoleFromContext(context.Background()))
}

// ---------------------------------------------------------------------------
// manager.go: Spawn edge cases
// ---------------------------------------------------------------------------

func TestSpawn_NilRunner(t *testing.T) {
	m := NewManager(NewManagerOpts{
		RootContext: context.Background(),
		Path:        t.TempDir() + "/state.json",
	})
	t.Cleanup(m.Close)

	_, err := m.Spawn(context.Background(), SpawnRequest{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "runner is required")
}

func TestSpawn_ClosedManager(t *testing.T) {
	m := NewManager(NewManagerOpts{
		RootContext: context.Background(),
		Path:        t.TempDir() + "/state.json",
	})
	m.Close()
	_, err := m.Spawn(context.Background(), SpawnRequest{
		Runner: simpleRunner(t, ""),
	})
	require.ErrorIs(t, err, ErrClosed)
}

// simpleRunner returns a Runner that returns the given result.
func simpleRunner(t *testing.T, result string) Runner {
	t.Helper()
	return RunnerFunc(func(ctx context.Context, agentID, assignment string) (string, error) {
		return result, nil
	})
}

var _ Runner = RunnerFunc(nil)

// ---------------------------------------------------------------------------
// manager.go: SendInput edge cases
// ---------------------------------------------------------------------------

func TestSendInput_Closed(t *testing.T) {
	m := newTestManager(t, 2)
	m.Close()
	err := m.SendInput("ag-1", "hello", false)
	require.ErrorIs(t, err, ErrClosed)
}

func TestSendInput_NotRunning(t *testing.T) {
	m := newTestManager(t, 2)
	t.Cleanup(m.Close)
	err := m.SendInput("nonexistent", "hello", false)
	require.ErrorIs(t, err, ErrNotRunning)
}

func TestSendInput_NotAccepting(t *testing.T) {
	m := newTestManager(t, 2)
	t.Cleanup(m.Close)
	id, err := m.Spawn(context.Background(), SpawnRequest{
		Runner: simpleRunner(t, "ok"),
	})
	require.NoError(t, err)
	// Wait briefly for agent to finish (it's no longer accepting).
	_, _ = m.Wait(context.Background(), id, WaitOpts{Timeout: time.Second})
	// Agent is no longer running; SendInput should fail.
	err = m.SendInput(id, "hello", false)
	require.ErrorIs(t, err, ErrNotRunning)
}

// ---------------------------------------------------------------------------
// manager.go: Assign edge cases
// ---------------------------------------------------------------------------

func TestAssign_NotFound(t *testing.T) {
	m := newTestManager(t, 2)
	t.Cleanup(m.Close)
	err := m.Assign("ghost", "task")
	require.ErrorIs(t, err, ErrNotFound)
}

// ---------------------------------------------------------------------------
// manager.go: Wait edge cases
// ---------------------------------------------------------------------------

func TestWait_NotFound(t *testing.T) {
	m := newTestManager(t, 2)
	t.Cleanup(m.Close)
	_, err := m.Wait(context.Background(), "ghost", WaitOpts{})
	require.ErrorIs(t, err, ErrNotFound)
}

func TestWait_Timeout(t *testing.T) {
	m := newTestManager(t, 2)
	t.Cleanup(m.Close)

	// Spawn an agent that blocks forever.
	blocked := make(chan struct{})
	id, err := m.Spawn(context.Background(), SpawnRequest{
		Runner: RunnerFunc(func(ctx context.Context, _, _ string) (string, error) {
			<-blocked
			return "done", nil
		}),
	})
	require.NoError(t, err)
	defer close(blocked)

	_, err = m.Wait(context.Background(), id, WaitOpts{Timeout: 10 * time.Millisecond})
	require.Error(t, err)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
}

// ---------------------------------------------------------------------------
// manager.go: Cancel edge cases
// ---------------------------------------------------------------------------

func TestCancel_NotFound(t *testing.T) {
	m := newTestManager(t, 2)
	t.Cleanup(m.Close)
	err := m.Cancel("ghost")
	require.ErrorIs(t, err, ErrNotFound)
}

// ---------------------------------------------------------------------------
// manager.go: AddUsage edge cases
// ---------------------------------------------------------------------------

func TestAddUsage_Closed(t *testing.T) {
	m := newTestManager(t, 2)
	m.Close()
	err := m.AddUsage("ag-1", Usage{TotalTokens: 10})
	require.ErrorIs(t, err, ErrClosed)
}

func TestAddUsage_NotFound(t *testing.T) {
	m := newTestManager(t, 2)
	t.Cleanup(m.Close)
	err := m.AddUsage("ghost", Usage{TotalTokens: 10})
	require.ErrorIs(t, err, ErrNotFound)
}

// ---------------------------------------------------------------------------
// manager.go: List with archived
// ---------------------------------------------------------------------------

func TestList_Archived(t *testing.T) {
	m := newTestManager(t, 2)
	t.Cleanup(m.Close)
	// No agents → empty results regardless of archived setting.
	all := m.List(true)
	assert.Empty(t, all.Agents)
	current := m.List(false)
	assert.Empty(t, current.Agents)
}

// ---------------------------------------------------------------------------
// manager.go: copyStrings / copyCustomRole edge cases
// ---------------------------------------------------------------------------

func TestCopyStrings_Nil(t *testing.T) {
	assert.Nil(t, copyStrings(nil))
}

func TestCopyStrings_NonNil(t *testing.T) {
	src := []string{"a", "b"}
	dst := copyStrings(src)
	assert.Equal(t, src, dst)
	// Verify it's a copy, not a reference.
	dst[0] = "x"
	assert.Equal(t, "a", src[0])
}

func TestCopyCustomRole_Nil(t *testing.T) {
	assert.Nil(t, copyCustomRole(nil))
}

func TestCopyCustomRole_NonNilNoTools(t *testing.T) {
	src := &CustomRole{Name: "tester"}
	dst := copyCustomRole(src)
	require.NotNil(t, dst)
	assert.Equal(t, "tester", dst.Name)
	assert.Nil(t, dst.AllowedTools)
}

func TestCopyCustomRole_WithTools(t *testing.T) {
	src := &CustomRole{Name: "coder", AllowedTools: []string{"fs.*", "shell.*"}}
	dst := copyCustomRole(src)
	require.NotNil(t, dst)
	assert.Equal(t, src.Name, dst.Name)
	assert.Equal(t, src.AllowedTools, dst.AllowedTools)
	// Verify it's a copy.
	dst.AllowedTools[0] = "modified"
	assert.Equal(t, "fs.*", src.AllowedTools[0])
}

// ---------------------------------------------------------------------------
// manager.go: restoreRecord
// ---------------------------------------------------------------------------

func TestRestoreRecord_RemovesRuntime(t *testing.T) {
	m := newTestManager(t, 2)
	t.Cleanup(m.Close)

	rec := Record{ID: "ag-test", Status: StatusRunning}
	m.mtx.Lock()
	m.records["ag-test"] = rec
	m.runtime["ag-test"] = &runtimeAgent{done: make(chan struct{})}
	m.mtx.Unlock()

	cancel := func() {}
	m.restoreRecord("ag-test", rec, cancel)

	m.mtx.RLock()
	_, hasRecord := m.records["ag-test"]
	_, hasRuntime := m.runtime["ag-test"]
	m.mtx.RUnlock()
	assert.True(t, hasRecord, "record should be restored")
	assert.False(t, hasRuntime, "runtime should be removed")
}

// ---------------------------------------------------------------------------
// manager.go: restoreUsage
// ---------------------------------------------------------------------------

func TestRestoreUsage(t *testing.T) {
	m := newTestManager(t, 2)
	t.Cleanup(m.Close)

	old := Usage{PromptTokens: 5, CompletionTokens: 3, TotalTokens: 8}
	m.mtx.Lock()
	m.records["ag-u1"] = Record{ID: "ag-u1", Usage: Usage{TotalTokens: 100}}
	m.mtx.Unlock()

	m.restoreUsage("ag-u1", old)

	rec, ok := m.Result("ag-u1")
	require.True(t, ok)
	assert.Equal(t, int64(5), rec.Usage.PromptTokens)
	assert.Equal(t, int64(3), rec.Usage.CompletionTokens)
	assert.Equal(t, int64(8), rec.Usage.TotalTokens)
}

// ---------------------------------------------------------------------------
// manager.go: mapStatusToEvent
// ---------------------------------------------------------------------------

func TestMapStatusToEvent_Interrupted(t *testing.T) {
	ev := mapStatusToEvent(StatusInterrupted)
	assert.Equal(t, EventType("interrupted"), ev)
}

func TestMapStatusToEvent_Fallback(t *testing.T) {
	ev := mapStatusToEvent(Status("unknown"))
	assert.Equal(t, EventFailed, ev)
}

// newTestManager creates a fresh Manager for testing convenience.
func newTestManager(t *testing.T, max int) *Manager {
	t.Helper()
	return NewManager(NewManagerOpts{
		RootContext:   context.Background(),
		MaxConcurrent: max,
		Path:          t.TempDir() + "/state.json",
	})
}

// ---------------------------------------------------------------------------
// manager.go: Spawn with depth overflow
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// manager.go: Resume edge cases
// ---------------------------------------------------------------------------

func TestResume_NilRunner(t *testing.T) {
	m := newTestManager(t, 2)
	t.Cleanup(m.Close)
	_, err := m.Resume(context.Background(), "ag-nil", ResumeRequest{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "runner is required")
}

func TestResume_ClosedManager(t *testing.T) {
	m := newTestManager(t, 2)
	m.Close()
	_, err := m.Resume(context.Background(), "ag-1", ResumeRequest{Runner: simpleRunner(t, "ok")})
	require.ErrorIs(t, err, ErrClosed)
}

func TestResume_NotFound(t *testing.T) {
	m := newTestManager(t, 2)
	t.Cleanup(m.Close)
	_, err := m.Resume(context.Background(), "ghost", ResumeRequest{Runner: simpleRunner(t, "ok")})
	require.ErrorIs(t, err, ErrNotFound)
}

func TestResume_AlreadyRunning(t *testing.T) {
	m := newTestManager(t, 2)
	t.Cleanup(m.Close)
	id, err := m.Spawn(context.Background(), SpawnRequest{Runner: simpleRunner(t, "ok")})
	require.NoError(t, err)

	_, err = m.Resume(context.Background(), id, ResumeRequest{Runner: simpleRunner(t, "ok")})
	require.Error(t, err)
	// The exact message is timing-dependent: Resume hits "is already running"
	// when the spawned runner hasn't finished yet (rec.Status == Running), or
	// "runtime already active" when it has finished but the runtime entry is
	// still live. Both confirm Resume refuses a busy agent — accept either.
	msg := err.Error()
	assert.True(t, strings.Contains(msg, "already running") || strings.Contains(msg, "already active"),
		"expected already-running/active error, got %q", msg)
}

// ---------------------------------------------------------------------------
// manager.go: runtimeChildCtx with missing parent
// ---------------------------------------------------------------------------

func TestRuntimeChildCtx_NoParentFallback(t *testing.T) {
	m := newTestManager(t, 2)
	t.Cleanup(m.Close)
	// Empty parentID → should use rootCtx, not panic.
	ctx := m.runtimeChildCtx("")
	assert.NotNil(t, ctx)
}

// ---------------------------------------------------------------------------
// manager.go: finishTerminal with unknown record
// ---------------------------------------------------------------------------

func TestFinishTerminal_UnknownRecord(t *testing.T) {
	m := newTestManager(t, 2)
	t.Cleanup(m.Close)
	// Should not panic.
	m.finishTerminal("ghost", StatusCompleted, "", "")
}

// ---------------------------------------------------------------------------
// manager.go: sinkLocked with missing agent
// ---------------------------------------------------------------------------

func TestSinkLocked_NoAgent(t *testing.T) {
	m := newTestManager(t, 2)
	t.Cleanup(m.Close)
	sink := m.sinkLocked("ghost")
	assert.Nil(t, sink)
}

// ---------------------------------------------------------------------------
// manager.go: recordRole with missing agent
// ---------------------------------------------------------------------------

func TestRecordRole_Missing(t *testing.T) {
	m := newTestManager(t, 2)
	t.Cleanup(m.Close)
	role := m.recordRole("ghost")
	assert.Empty(t, role)
}

// ---------------------------------------------------------------------------
// manager.go: runningLocked with empty maps
// ---------------------------------------------------------------------------

func TestRunningLocked_Empty(t *testing.T) {
	m := newTestManager(t, 2)
	t.Cleanup(m.Close)
	m.mtx.Lock()
	count := m.runningLocked()
	m.mtx.Unlock()
	assert.Equal(t, 0, count)
}

// ---------------------------------------------------------------------------
// manager.go: NewManager bounds
// ---------------------------------------------------------------------------

func TestNewManager_Bounds(t *testing.T) {
	// limit 0 → default 10
	m0 := NewManager(NewManagerOpts{
		RootContext:   context.Background(),
		MaxConcurrent: 0,
		Path:          t.TempDir() + "/s0.json",
	})
	assert.Equal(t, 10, m0.limit)
	m0.Close()

	// limit negative → clamp to 1
	m1 := NewManager(NewManagerOpts{
		RootContext:   context.Background(),
		MaxConcurrent: -5,
		Path:          t.TempDir() + "/s1.json",
	})
	assert.Equal(t, 1, m1.limit)
	m1.Close()

	// limit > 20 → clamp to 20
	m2 := NewManager(NewManagerOpts{
		RootContext:   context.Background(),
		MaxConcurrent: 50,
		Path:          t.TempDir() + "/s2.json",
	})
	assert.Equal(t, 20, m2.limit)
	m2.Close()
}
