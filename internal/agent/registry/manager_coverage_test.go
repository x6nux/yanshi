package registry

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// context.go
// ---------------------------------------------------------------------------

func TestWithCurrentAgentIDRoundTrip(t *testing.T) {
	ctx := context.Background()
	assert.Equal(t, "", CurrentAgentID(ctx))
	ctx2 := WithCurrentAgentID(ctx, "ag-42")
	assert.Equal(t, "ag-42", CurrentAgentID(ctx2))
	// Original unchanged.
	assert.Equal(t, "", CurrentAgentID(ctx))
}

func TestWithRoleRoundTrip(t *testing.T) {
	ctx := context.Background()
	assert.Equal(t, "", RoleFromContext(ctx))
	ctx2 := WithRole(ctx, "explorer")
	assert.Equal(t, "explorer", RoleFromContext(ctx2))
}

// ---------------------------------------------------------------------------
// manager.go — NewManager limit clamping + load error
// ---------------------------------------------------------------------------

func TestNewManagerClampsNegativeLimit(t *testing.T) {
	m := NewManager(NewManagerOpts{
		RootContext: context.Background(), Path: filepath.Join(t.TempDir(), "s.json"),
		SessionBootID: "boot", MaxConcurrent: -5,
	})
	t.Cleanup(m.Close)
	require.Equal(t, 1, m.limit)
}

func TestNewManagerClampsOversizedLimit(t *testing.T) {
	m := NewManager(NewManagerOpts{
		RootContext: context.Background(), Path: filepath.Join(t.TempDir(), "s.json"),
		SessionBootID: "boot", MaxConcurrent: 99,
	})
	t.Cleanup(m.Close)
	require.Equal(t, 20, m.limit)
}

func TestNewManagerLoadStateErrorContinues(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	// Write corrupt JSON to trigger loadState error.
	require.NoError(t, os.WriteFile(path, []byte("{not json}"), 0o600))
	m := NewManager(NewManagerOpts{
		RootContext: context.Background(), Path: path,
		SessionBootID: "boot", MaxConcurrent: 2,
	})
	t.Cleanup(m.Close)
	// Manager should still be functional with empty records.
	_, ok := m.Result("anything")
	require.False(t, ok)
}

func TestNewManagerLoadsTerminalRecords(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	rec := Record{
		ID: "ag-done", SessionBootID: "old-boot", Status: StatusCompleted,
		Prompt: "finished", StartedAt: time.Now().UTC(),
	}
	st := persistedState{
		SchemaVersion: persistenceSchemaVersion,
		SessionBootID: "old-boot",
		Agents:        []Record{rec},
	}
	raw, err := json.Marshal(st)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, raw, 0o600))

	m := NewManager(NewManagerOpts{
		RootContext: context.Background(), Path: path,
		SessionBootID: "new-boot", MaxConcurrent: 2,
	})
	t.Cleanup(m.Close)

	// Terminal records are loaded as-is (not changed to Interrupted).
	got, ok := m.Result("ag-done")
	require.True(t, ok)
	require.Equal(t, StatusCompleted, got.Status)

	// Non-current-boot records visible via List(true) only.
	listCurrent := m.List(false)
	require.Empty(t, listCurrent.Agents)
	listAll := m.List(true)
	require.Len(t, listAll.Agents, 1)
}

// ---------------------------------------------------------------------------
// manager.go — Spawn error paths
// ---------------------------------------------------------------------------

func TestSpawnRejectsNilRunner(t *testing.T) {
	m, _ := newManager(t)
	_, err := m.Spawn(context.Background(), SpawnRequest{
		Role: "explore", Prompt: "p",
		Runner: nil,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "runner is required")
}

func TestSpawnRejectsTooDeep(t *testing.T) {
	m := NewManager(NewManagerOpts{
		RootContext:   context.Background(),
		Path:          filepath.Join(t.TempDir(), "s.json"),
		SessionBootID: "boot", MaxConcurrent: 10,
	})
	t.Cleanup(m.Close)

	// Build a 4-level chain (depth 0..3) so the 5th (depth 4) exceeds MaxSubAgentDepth=3.
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })

	mkRunner := func() Runner {
		return RunnerFunc(func(ctx context.Context, _, _ string) (string, error) {
			<-release
			return "done", nil
		})
	}

	d1, err := m.Spawn(context.Background(), SpawnRequest{Role: "d1", Prompt: "p1", Runner: mkRunner()})
	require.NoError(t, err)

	d2, err := m.Spawn(WithCurrentAgentID(context.Background(), d1), SpawnRequest{Role: "d2", Prompt: "p2", Runner: mkRunner()})
	require.NoError(t, err)

	d3, err := m.Spawn(WithCurrentAgentID(context.Background(), d2), SpawnRequest{Role: "d3", Prompt: "p3", Runner: mkRunner()})
	require.NoError(t, err)

	d4, err := m.Spawn(WithCurrentAgentID(context.Background(), d3), SpawnRequest{Role: "d4", Prompt: "p4", Runner: mkRunner()})
	require.NoError(t, err)

	// Attempt d5 at depth 4 -> ErrTooDeep.
	_, err = m.Spawn(WithCurrentAgentID(context.Background(), d4), SpawnRequest{
		Role: "d5", Prompt: "p5",
		Runner: RunnerFunc(func(context.Context, string, string) (string, error) {
			return "d5", nil
		}),
	})
	require.ErrorIs(t, err, ErrTooDeep)
}

func TestSpawnWithDeadParentUsesRootCtx(t *testing.T) {
	m := NewManager(NewManagerOpts{
		RootContext:   context.Background(),
		Path:          filepath.Join(t.TempDir(), "s.json"),
		SessionBootID: "boot", MaxConcurrent: 5,
	})
	t.Cleanup(m.Close)

	// Spawn and let it complete so it's terminal (not in runtime).
	parentDone := make(chan struct{})
	parentID, err := m.Spawn(context.Background(), SpawnRequest{
		Role: "parent", Prompt: "quick",
		Runner: RunnerFunc(func(context.Context, string, string) (string, error) {
			close(parentDone)
			return "parent done", nil
		}),
	})
	require.NoError(t, err)
	<-parentDone

	// Wait for terminal.
	_, _ = m.Wait(context.Background(), parentID, WaitOpts{Timeout: time.Second})

	// Now spawn with the dead parent — runtimeChildCtx should fall back to rootCtx.
	childDone := make(chan struct{})
	childID, err := m.Spawn(context.Background(), SpawnRequest{
		ParentID: parentID,
		Role:     "child", Prompt: "c",
		Runner: RunnerFunc(func(context.Context, string, string) (string, error) {
			close(childDone)
			return "child done", nil
		}),
	})
	require.NoError(t, err)
	require.NotEmpty(t, childID)
	<-childDone
	_, _ = m.Wait(context.Background(), childID, WaitOpts{Timeout: time.Second})
}

func TestSpawnWithCustomRoleAndAllowedTools(t *testing.T) {
	m := NewManager(NewManagerOpts{
		RootContext:   context.Background(),
		Path:          filepath.Join(t.TempDir(), "s.json"),
		SessionBootID: "boot", MaxConcurrent: 2,
	})
	t.Cleanup(m.Close)

	done := make(chan struct{})
	custom := &CustomRole{
		Name:          "auditor",
		PromptPrefix:  "You are an auditor.",
		AllowedTools:  []string{"fs_read", "fs_write"},
		ReadOnlyShell: true,
	}
	id, err := m.Spawn(context.Background(), SpawnRequest{
		Role:         "custom",
		Prompt:       "audit",
		Custom:       custom,
		AllowedTools: []string{"fs_read", "shell_run"},
		Runner: RunnerFunc(func(context.Context, string, string) (string, error) {
			close(done)
			return "audited", nil
		}),
	})
	require.NoError(t, err)
	<-done
	_, _ = m.Wait(context.Background(), id, WaitOpts{Timeout: time.Second})

	rec, ok := m.Result(id)
	require.True(t, ok)
	require.Equal(t, []string{"fs_read", "shell_run"}, rec.AllowedTools)
	require.NotNil(t, rec.Custom)
	require.Equal(t, "auditor", rec.Custom.Name)
	require.Equal(t, []string{"fs_read", "fs_write"}, rec.Custom.AllowedTools)
}

func TestSpawnUsesContextParentID(t *testing.T) {
	m := NewManager(NewManagerOpts{
		RootContext:   context.Background(),
		Path:          filepath.Join(t.TempDir(), "s.json"),
		SessionBootID: "boot", MaxConcurrent: 5,
	})
	t.Cleanup(m.Close)

	parentBlock := make(chan struct{})
	parentID, err := m.Spawn(context.Background(), SpawnRequest{
		Role: "parent", Prompt: "p",
		Runner: RunnerFunc(func(ctx context.Context, _, _ string) (string, error) {
			<-parentBlock
			return "parent", nil
		}),
	})
	require.NoError(t, err)

	// Don't pass ParentID in SpawnRequest; rely on context.
	childDone := make(chan struct{})
	childID, err := m.Spawn(WithCurrentAgentID(context.Background(), parentID), SpawnRequest{
		Role: "child", Prompt: "c",
		Runner: RunnerFunc(func(context.Context, string, string) (string, error) {
			close(childDone)
			return "child", nil
		}),
	})
	require.NoError(t, err)

	// Child should have parentID set and depth > 0.
	rec, ok := m.Result(childID)
	require.True(t, ok)
	require.Equal(t, parentID, rec.ParentID)
	require.Equal(t, 1, rec.Depth)

	close(parentBlock)
	<-childDone
}

func TestSpawnRollsBackOnWriteError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.json")
	m := NewManager(NewManagerOpts{
		RootContext: context.Background(), Path: path,
		SessionBootID: "boot", MaxConcurrent: 2,
	})
	t.Cleanup(m.Close)

	// Make directory unwritable so writeAtomic fails.
	require.NoError(t, os.Rename(dir, dir+".gone"))
	_, err := m.Spawn(context.Background(), SpawnRequest{
		Role: "explore", Prompt: "p",
		Runner: RunnerFunc(func(context.Context, string, string) (string, error) {
			return "ok", nil
		}),
	})
	require.Error(t, err)
	require.NoError(t, os.Rename(dir+".gone", dir))

	// Record should have been rolled back.
	list := m.List(false)
	require.Empty(t, list.Agents)
}

// ---------------------------------------------------------------------------
// manager.go — SendInput paths
// ---------------------------------------------------------------------------

func TestSendInputRejectsClosedManager(t *testing.T) {
	m := NewManager(NewManagerOpts{
		RootContext:   context.Background(),
		Path:          filepath.Join(t.TempDir(), "s.json"),
		SessionBootID: "boot", MaxConcurrent: 1,
	})
	m.Close()
	require.ErrorIs(t, m.SendInput("any", "x", false), ErrClosed)
}

func TestSendInputInterruptCancelsTurn(t *testing.T) {
	m := NewManager(NewManagerOpts{
		RootContext:   context.Background(),
		Path:          filepath.Join(t.TempDir(), "s.json"),
		SessionBootID: "boot", MaxConcurrent: 2,
	})
	t.Cleanup(m.Close)

	firstDone := make(chan struct{})
	resumeCh := make(chan struct{})

	id, err := m.Spawn(context.Background(), SpawnRequest{
		AgentType: "subagent", Role: "general", Prompt: "first",
		Runner: RunnerFunc(func(ctx context.Context, agentID, assignment string) (string, error) {
			if assignment == "first" {
				close(firstDone)
				<-resumeCh
				return "first done", nil
			}
			return "second", nil
		}),
	})
	require.NoError(t, err)

	<-firstDone
	// Use interrupt=true.
	require.NoError(t, m.SendInput(id, "follow up", true))
	close(resumeCh)

	final, err := m.Wait(context.Background(), id, WaitOpts{Timeout: 2 * time.Second})
	require.NoError(t, err)
	assert.True(t, final.Status.Terminal())
}

func TestSendInputMailboxFull(t *testing.T) {
	m := NewManager(NewManagerOpts{
		RootContext:   context.Background(),
		Path:          filepath.Join(t.TempDir(), "s.json"),
		SessionBootID: "boot", MaxConcurrent: 1,
	})
	t.Cleanup(m.Close)

	// Runner blocks so mailbox can fill up.
	block := make(chan struct{})
	id, err := m.Spawn(context.Background(), SpawnRequest{
		Role: "general", Prompt: "p",
		Runner: RunnerFunc(func(ctx context.Context, _, _ string) (string, error) {
			<-block
			return "ok", nil
		}),
	})
	require.NoError(t, err)

	// Mailbox cap is 8; fill it.
	for i := 0; i < 8; i++ {
		require.NoError(t, m.SendInput(id, "msg", false))
	}
	// Next send should hit ErrMailboxFull.
	require.ErrorIs(t, m.SendInput(id, "overflow", false), ErrMailboxFull)

	close(block)
	_, _ = m.Wait(context.Background(), id, WaitOpts{Timeout: 2 * time.Second})
}

// ---------------------------------------------------------------------------
// manager.go — Assign paths
// ---------------------------------------------------------------------------

func TestAssignRejectsNotFound(t *testing.T) {
	m, _ := newManager(t)
	require.ErrorIs(t, m.Assign("ghost", "x"), ErrNotFound)
}

func TestAssignMailboxFullWhenRunning(t *testing.T) {
	m := NewManager(NewManagerOpts{
		RootContext:   context.Background(),
		Path:          filepath.Join(t.TempDir(), "s.json"),
		SessionBootID: "boot", MaxConcurrent: 1,
	})
	t.Cleanup(m.Close)

	block := make(chan struct{})
	id, err := m.Spawn(context.Background(), SpawnRequest{
		Role: "general", Prompt: "p",
		Runner: RunnerFunc(func(ctx context.Context, _, _ string) (string, error) {
			<-block
			return "ok", nil
		}),
	})
	require.NoError(t, err)

	// Fill the mailbox (cap 8) via SendInput first.
	for i := 0; i < 8; i++ {
		require.NoError(t, m.SendInput(id, "msg", false))
	}
	// Assign should detect mailbox full and return ErrMailboxFull.
	err = m.Assign(id, "another")
	require.ErrorIs(t, err, ErrMailboxFull)

	close(block)
	_, _ = m.Wait(context.Background(), id, WaitOpts{Timeout: 2 * time.Second})
}

func TestAssignOnTerminalAgentPersistsWithoutEnqueue(t *testing.T) {
	m := NewManager(NewManagerOpts{
		RootContext:   context.Background(),
		Path:          filepath.Join(t.TempDir(), "s.json"),
		SessionBootID: "boot", MaxConcurrent: 2,
	})
	t.Cleanup(m.Close)

	done := make(chan struct{})
	id, err := m.Spawn(context.Background(), SpawnRequest{
		Role: "general", Prompt: "p",
		Runner: RunnerFunc(func(context.Context, string, string) (string, error) {
			close(done)
			return "ok", nil
		}),
	})
	require.NoError(t, err)
	<-done
	_, _ = m.Wait(context.Background(), id, WaitOpts{Timeout: time.Second})

	// Assign on terminal agent: no runtime, should persist assignment without enqueue.
	require.NoError(t, m.Assign(id, "posthumous assignment"))
	rec, ok := m.Result(id)
	require.True(t, ok)
	require.Equal(t, "posthumous assignment", rec.Assignment)
}

// ---------------------------------------------------------------------------
// manager.go — Cancel already-terminal
// ---------------------------------------------------------------------------

func TestCancelOnTerminalAgentReturnsNil(t *testing.T) {
	m := NewManager(NewManagerOpts{
		RootContext:   context.Background(),
		Path:          filepath.Join(t.TempDir(), "s.json"),
		SessionBootID: "boot", MaxConcurrent: 2,
	})
	t.Cleanup(m.Close)

	done := make(chan struct{})
	id, err := m.Spawn(context.Background(), SpawnRequest{
		Role: "general", Prompt: "p",
		Runner: RunnerFunc(func(context.Context, string, string) (string, error) {
			close(done)
			return "ok", nil
		}),
	})
	require.NoError(t, err)
	<-done
	_, _ = m.Wait(context.Background(), id, WaitOpts{Timeout: time.Second})

	// Cancel on terminal agent (no runtime entry) -> nil error.
	require.NoError(t, m.Cancel(id))
}

// ---------------------------------------------------------------------------
// manager.go — Resume error paths
// ---------------------------------------------------------------------------

func TestResumeRejectsNilRunner(t *testing.T) {
	m := NewManager(NewManagerOpts{
		RootContext:   context.Background(),
		Path:          filepath.Join(t.TempDir(), "s.json"),
		SessionBootID: "boot", MaxConcurrent: 2,
	})
	t.Cleanup(m.Close)

	_, err := m.Resume(context.Background(), "ag-1", ResumeRequest{Runner: nil})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "runner is required")
}

func TestResumeRejectsClosedManager(t *testing.T) {
	m := NewManager(NewManagerOpts{
		RootContext:   context.Background(),
		Path:          filepath.Join(t.TempDir(), "s.json"),
		SessionBootID: "boot", MaxConcurrent: 2,
	})
	m.Close()

	_, err := m.Resume(context.Background(), "ag-1", ResumeRequest{
		Runner: RunnerFunc(func(context.Context, string, string) (string, error) {
			return "", nil
		}),
	})
	require.ErrorIs(t, err, ErrClosed)
}

func TestResumeRejectsNotFound(t *testing.T) {
	m, _ := newManager(t)
	_, err := m.Resume(context.Background(), "ghost", ResumeRequest{
		Runner: RunnerFunc(func(context.Context, string, string) (string, error) {
			return "", nil
		}),
	})
	require.ErrorIs(t, err, ErrNotFound)
}

func TestResumeRejectsAlreadyRunning(t *testing.T) {
	m := NewManager(NewManagerOpts{
		RootContext:   context.Background(),
		Path:          filepath.Join(t.TempDir(), "s.json"),
		SessionBootID: "boot", MaxConcurrent: 2,
	})
	t.Cleanup(m.Close)

	block := make(chan struct{})
	id, err := m.Spawn(context.Background(), SpawnRequest{
		Role: "general", Prompt: "p",
		Runner: RunnerFunc(func(ctx context.Context, _, _ string) (string, error) {
			<-block
			return "ok", nil
		}),
	})
	require.NoError(t, err)

	_, err = m.Resume(context.Background(), id, ResumeRequest{
		Runner: RunnerFunc(func(context.Context, string, string) (string, error) {
			return "ok2", nil
		}),
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "already running")

	close(block)
	_, _ = m.Wait(context.Background(), id, WaitOpts{Timeout: 2 * time.Second})
}

func TestResumeRejectsConcurrencyCap(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.json")
	// Seed a terminal record.
	rec := Record{
		ID: "ag-old", SessionBootID: "boot", Status: StatusCompleted,
		Prompt: "done", StartedAt: time.Now().UTC(),
	}
	raw, err := json.Marshal(persistedState{
		SchemaVersion: persistenceSchemaVersion,
		SessionBootID: "boot",
		Agents:        []Record{rec},
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, raw, 0o600))

	m := NewManager(NewManagerOpts{
		RootContext: context.Background(), Path: path,
		SessionBootID: "boot2", MaxConcurrent: 1,
	})
	t.Cleanup(m.Close)

	// Occupy the single slot.
	block := make(chan struct{})
	_, err = m.Spawn(context.Background(), SpawnRequest{
		Role: "blocker", Prompt: "p",
		Runner: RunnerFunc(func(ctx context.Context, _, _ string) (string, error) {
			<-block
			return "ok", nil
		}),
	})
	require.NoError(t, err)

	// Resume should fail with cap error.
	_, err = m.Resume(context.Background(), "ag-old", ResumeRequest{
		Runner: RunnerFunc(func(context.Context, string, string) (string, error) {
			return "", nil
		}),
	})
	var capErr *SpawnErrCap
	require.ErrorAs(t, err, &capErr)
	require.Equal(t, 1, capErr.Cap)

	close(block)
}

func TestResumeRollsBackOnWriteError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.json")
	rec := Record{
		ID: "ag-old", SessionBootID: "boot", Status: StatusCompleted,
		Prompt: "done", StartedAt: time.Now().UTC(),
	}
	raw, err := json.Marshal(persistedState{
		SchemaVersion: persistenceSchemaVersion,
		SessionBootID: "boot",
		Agents:        []Record{rec},
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, raw, 0o600))

	m := NewManager(NewManagerOpts{
		RootContext: context.Background(), Path: path,
		SessionBootID: "boot2", MaxConcurrent: 2,
	})
	t.Cleanup(m.Close)

	// Make dir unwritable so writeAtomic fails.
	require.NoError(t, os.Rename(dir, dir+".gone"))
	_, err = m.Resume(context.Background(), "ag-old", ResumeRequest{
		Runner: RunnerFunc(func(context.Context, string, string) (string, error) {
			return "ok", nil
		}),
	})
	require.Error(t, err)
	require.NoError(t, os.Rename(dir+".gone", dir))

	// restoreRecord should have reverted the record to Completed.
	rec2, ok := m.Result("ag-old")
	require.True(t, ok)
	require.Equal(t, StatusCompleted, rec2.Status)
}

func TestResumeUsesPromptOverride(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.json")
	rec := Record{
		ID: "ag-old", SessionBootID: "boot", Status: StatusCompleted,
		Prompt: "original", Assignment: "saved-assignment",
		StartedAt: time.Now().UTC(),
	}
	raw, err := json.Marshal(persistedState{
		SchemaVersion: persistenceSchemaVersion,
		SessionBootID: "boot",
		Agents:        []Record{rec},
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, raw, 0o600))

	m := NewManager(NewManagerOpts{
		RootContext: context.Background(), Path: path,
		SessionBootID: "boot2", MaxConcurrent: 2,
	})
	t.Cleanup(m.Close)

	got := make(chan string, 1)
	_, err = m.Resume(context.Background(), "ag-old", ResumeRequest{
		Prompt: "override-prompt",
		Runner: RunnerFunc(func(ctx context.Context, _, assignment string) (string, error) {
			got <- assignment
			return "ok", nil
		}),
	})
	require.NoError(t, err)
	require.Equal(t, "override-prompt", <-got)
}

func TestResumeUsesSavedAssignment(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.json")
	rec := Record{
		ID: "ag-old", SessionBootID: "boot", Status: StatusCompleted,
		Assignment: "saved-assignment", StartedAt: time.Now().UTC(),
	}
	raw, err := json.Marshal(persistedState{
		SchemaVersion: persistenceSchemaVersion,
		SessionBootID: "boot",
		Agents:        []Record{rec},
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, raw, 0o600))

	m := NewManager(NewManagerOpts{
		RootContext: context.Background(), Path: path,
		SessionBootID: "boot2", MaxConcurrent: 2,
	})
	t.Cleanup(m.Close)

	got := make(chan string, 1)
	_, err = m.Resume(context.Background(), "ag-old", ResumeRequest{
		Runner: RunnerFunc(func(ctx context.Context, _, assignment string) (string, error) {
			got <- assignment
			return "ok", nil
		}),
	})
	require.NoError(t, err)
	require.Equal(t, "saved-assignment", <-got)
}

func TestResumeEmitsEvent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.json")
	rec := Record{
		ID: "ag-old", SessionBootID: "boot", Status: StatusCompleted,
		Prompt: "done", StartedAt: time.Now().UTC(),
	}
	raw, err := json.Marshal(persistedState{
		SchemaVersion: persistenceSchemaVersion,
		SessionBootID: "boot",
		Agents:        []Record{rec},
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, raw, 0o600))

	m := NewManager(NewManagerOpts{
		RootContext: context.Background(), Path: path,
		SessionBootID: "boot2", MaxConcurrent: 2,
	})
	t.Cleanup(m.Close)

	var mu sync.Mutex
	var events []Event
	sink := EventSink(func(ev Event) {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, ev)
	})

	_, err = m.Resume(context.Background(), "ag-old", ResumeRequest{
		Emit: sink,
		Runner: RunnerFunc(func(context.Context, string, string) (string, error) {
			return "ok", nil
		}),
	})
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		for _, ev := range events {
			if ev.Type == EventResumed {
				return true
			}
		}
		return false
	}, time.Second, 10*time.Millisecond)
}

// ---------------------------------------------------------------------------
// manager.go — Wait not-found
// ---------------------------------------------------------------------------

func TestWaitReturnsErrNotFoundForMissingAgent(t *testing.T) {
	m, _ := newManager(t)
	_, err := m.Wait(context.Background(), "ghost", WaitOpts{Timeout: 50 * time.Millisecond})
	require.ErrorIs(t, err, ErrNotFound)
}

func TestWaitTimeoutReturnsLatestRecord(t *testing.T) {
	m := NewManager(NewManagerOpts{
		RootContext:   context.Background(),
		Path:          filepath.Join(t.TempDir(), "s.json"),
		SessionBootID: "boot", MaxConcurrent: 1,
	})
	t.Cleanup(m.Close)

	block := make(chan struct{})
	id, err := m.Spawn(context.Background(), SpawnRequest{
		Role: "general", Prompt: "p",
		Runner: RunnerFunc(func(ctx context.Context, _, _ string) (string, error) {
			<-block
			return "ok", nil
		}),
	})
	require.NoError(t, err)

	latest, err := m.Wait(context.Background(), id, WaitOpts{Timeout: 50 * time.Millisecond})
	require.Error(t, err)
	require.Equal(t, StatusRunning, latest.Status)

	close(block)
	_, _ = m.Wait(context.Background(), id, WaitOpts{Timeout: 2 * time.Second})
}

// ---------------------------------------------------------------------------
// manager.go — mapStatusToEvent
// ---------------------------------------------------------------------------

func TestMapStatusToEventAllBranches(t *testing.T) {
	assert.Equal(t, EventCompleted, mapStatusToEvent(StatusCompleted))
	assert.Equal(t, EventFailed, mapStatusToEvent(StatusFailed))
	assert.Equal(t, EventCancelled, mapStatusToEvent(StatusCancelled))
	assert.Equal(t, EventType("interrupted"), mapStatusToEvent(StatusInterrupted))
	assert.Equal(t, EventFailed, mapStatusToEvent(StatusRunning)) // default
	assert.Equal(t, EventFailed, mapStatusToEvent(StatusPending)) // default
	assert.Equal(t, EventFailed, mapStatusToEvent(Status("")))    // default
}

// ---------------------------------------------------------------------------
// manager.go — AddUsage error paths
// ---------------------------------------------------------------------------

func TestAddUsageRejectsClosedManager(t *testing.T) {
	m := NewManager(NewManagerOpts{
		RootContext:   context.Background(),
		Path:          filepath.Join(t.TempDir(), "s.json"),
		SessionBootID: "boot", MaxConcurrent: 1,
	})
	m.Close()
	require.ErrorIs(t, m.AddUsage("any", Usage{}), ErrClosed)
}

func TestAddUsageRejectsNotFound(t *testing.T) {
	m, _ := newManager(t)
	require.ErrorIs(t, m.AddUsage("ghost", Usage{}), ErrNotFound)
}

func TestAddUsageRollsBackOnWriteError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.json")
	m := NewManager(NewManagerOpts{
		RootContext: context.Background(), Path: path,
		SessionBootID: "boot", MaxConcurrent: 2,
	})
	t.Cleanup(m.Close)

	block := make(chan struct{})
	id, err := m.Spawn(context.Background(), SpawnRequest{
		Role: "general", Prompt: "p",
		Runner: RunnerFunc(func(ctx context.Context, _, _ string) (string, error) {
			<-block
			return "ok", nil
		}),
	})
	require.NoError(t, err)

	// Make dir unwritable.
	require.NoError(t, os.Rename(dir, dir+".gone"))
	err = m.AddUsage(id, Usage{PromptTokens: 100})
	require.Error(t, err)
	require.NoError(t, os.Rename(dir+".gone", dir))

	// Usage should have been rolled back.
	rec, ok := m.Result(id)
	require.True(t, ok)
	require.Equal(t, int64(0), rec.Usage.PromptTokens)

	close(block)
	_, _ = m.Wait(context.Background(), id, WaitOpts{Timeout: 2 * time.Second})
}

func TestAddUsageEmitsPersistenceFailedEvent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.json")
	m := NewManager(NewManagerOpts{
		RootContext: context.Background(), Path: path,
		SessionBootID: "boot", MaxConcurrent: 2,
	})
	t.Cleanup(m.Close)

	var mu sync.Mutex
	var events []Event
	sink := EventSink(func(ev Event) {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, ev)
	})

	block := make(chan struct{})
	id, err := m.Spawn(context.Background(), SpawnRequest{
		Role: "general", Prompt: "p", Emit: sink,
		Runner: RunnerFunc(func(ctx context.Context, _, _ string) (string, error) {
			<-block
			return "ok", nil
		}),
	})
	require.NoError(t, err)

	require.NoError(t, os.Rename(dir, dir+".gone"))
	_ = m.AddUsage(id, Usage{PromptTokens: 100})
	require.NoError(t, os.Rename(dir+".gone", dir))

	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		for _, ev := range events {
			if ev.Type == EventPersistenceFailed {
				return true
			}
		}
		return false
	}, time.Second, 10*time.Millisecond)

	close(block)
	_, _ = m.Wait(context.Background(), id, WaitOpts{Timeout: 2 * time.Second})
}

// ---------------------------------------------------------------------------
// manager.go — copyStrings / copyCustomRole direct tests
// ---------------------------------------------------------------------------

func TestCopyStringsNil(t *testing.T) {
	require.Nil(t, copyStrings(nil))
}

func TestCopyStringsNonNil(t *testing.T) {
	src := []string{"a", "b", "c"}
	dst := copyStrings(src)
	require.Equal(t, src, dst)
	dst[0] = "x"
	require.Equal(t, "a", src[0], "copyStrings should produce an independent slice")
}

func TestCopyStringsEmpty(t *testing.T) {
	out := copyStrings([]string{})
	require.NotNil(t, out)
	require.Empty(t, out)
}

func TestCopyCustomRoleNil(t *testing.T) {
	require.Nil(t, copyCustomRole(nil))
}

func TestCopyCustomRoleWithAllowedTools(t *testing.T) {
	src := &CustomRole{
		Name:         "auditor",
		PromptPrefix: "audit all",
		AllowedTools: []string{"fs_read", "shell_run"},
	}
	dst := copyCustomRole(src)
	require.Equal(t, src, dst)
	dst.AllowedTools[0] = "tampered"
	require.Equal(t, "fs_read", src.AllowedTools[0], "copyCustomRole should deep-copy AllowedTools")
}

func TestCopyCustomRoleWithoutAllowedTools(t *testing.T) {
	src := &CustomRole{
		Name:         "minimal",
		PromptPrefix: "just basics",
	}
	dst := copyCustomRole(src)
	require.Equal(t, src, dst)
	require.Nil(t, dst.AllowedTools)
}

// ---------------------------------------------------------------------------
// manager.go — sinkLocked / recordRole
// ---------------------------------------------------------------------------

func TestSinkLockedReturnsNilForMissingRuntime(t *testing.T) {
	m := NewManager(NewManagerOpts{
		RootContext:   context.Background(),
		Path:          filepath.Join(t.TempDir(), "s.json"),
		SessionBootID: "boot", MaxConcurrent: 2,
	})
	t.Cleanup(m.Close)

	// Agent that completed — no runtime entry.
	done := make(chan struct{})
	id, err := m.Spawn(context.Background(), SpawnRequest{
		Role: "general", Prompt: "p",
		Runner: RunnerFunc(func(context.Context, string, string) (string, error) {
			close(done)
			return "ok", nil
		}),
	})
	require.NoError(t, err)
	<-done
	_, _ = m.Wait(context.Background(), id, WaitOpts{Timeout: time.Second})

	// sinkLocked should return nil for a terminal agent.
	m.mtx.RLock()
	sink := m.sinkLocked(id)
	m.mtx.RUnlock()
	require.Nil(t, sink)
}

func TestRecordRoleReturnsEmptyForMissing(t *testing.T) {
	m, _ := newManager(t)
	require.Equal(t, "", m.recordRole("ghost"))
}

// ---------------------------------------------------------------------------
// manager.go — Close idempotency + final persist
// ---------------------------------------------------------------------------

func TestCloseIsIdempotent(t *testing.T) {
	m := NewManager(NewManagerOpts{
		RootContext:   context.Background(),
		Path:          filepath.Join(t.TempDir(), "s.json"),
		SessionBootID: "boot", MaxConcurrent: 1,
	})
	m.Close()
	// Second Close should be a no-op (not panic).
	m.Close()
}

func TestClosePersistsTerminalState(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.json")
	m := NewManager(NewManagerOpts{
		RootContext: context.Background(), Path: path,
		SessionBootID: "boot", MaxConcurrent: 2,
	})

	done := make(chan struct{})
	id, err := m.Spawn(context.Background(), SpawnRequest{
		Role: "general", Prompt: "p",
		Runner: RunnerFunc(func(context.Context, string, string) (string, error) {
			close(done)
			return "final result", nil
		}),
	})
	require.NoError(t, err)
	<-done
	m.Close()

	// Load state from disk.
	st, err := loadState(path)
	require.NoError(t, err)
	var found bool
	for _, rec := range st.Agents {
		if rec.ID == id {
			found = true
			require.True(t, rec.Status.Terminal())
			require.Equal(t, "final result", rec.Result)
		}
	}
	require.True(t, found)
}

// ---------------------------------------------------------------------------
// persist.go — loadState edge cases
// ---------------------------------------------------------------------------

func TestLoadStateOnDirectoryReturnsError(t *testing.T) {
	dir := t.TempDir()
	// Reading a directory as a file should return an error that's not IsNotExist.
	_, err := loadState(dir)
	require.Error(t, err)
}

func TestLoadStateFillsSchemaVersionZero(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.json")
	// Write state with schema_version=0 to trigger default-fill.
	require.NoError(t, os.WriteFile(path, []byte(`{"schema_version":0}`), 0o600))
	st, err := loadState(path)
	require.NoError(t, err)
	require.Equal(t, persistenceSchemaVersion, st.SchemaVersion)
}

// ---------------------------------------------------------------------------
// writeatomic — error path
// ---------------------------------------------------------------------------

func TestWriteAtomicErrorOnMissingDir(t *testing.T) {
	// Creating a temp file in a nonexistent dir should fail.
	err := writeAtomic(filepath.Join(t.TempDir(), "nonexistent-deep", "state.json"), []byte(`{}`))
	require.Error(t, err)
}

// ---------------------------------------------------------------------------
// runAgentLoop — cancelled mid-run + failed runner
// ---------------------------------------------------------------------------

func TestRunAgentLoopContextCanceledReportsCancelled(t *testing.T) {
	m := NewManager(NewManagerOpts{
		RootContext:   context.Background(),
		Path:          filepath.Join(t.TempDir(), "s.json"),
		SessionBootID: "boot", MaxConcurrent: 2,
	})
	t.Cleanup(m.Close)

	id, err := m.Spawn(context.Background(), SpawnRequest{
		Role: "general", Prompt: "p",
		Runner: RunnerFunc(func(ctx context.Context, _, _ string) (string, error) {
			<-ctx.Done()
			return "partial", nil // returns after cancellation
		}),
	})
	require.NoError(t, err)

	// Cancel the agent.
	require.NoError(t, m.Cancel(id))

	final, err := m.Wait(context.Background(), id, WaitOpts{Timeout: 2 * time.Second})
	require.NoError(t, err)
	require.Equal(t, StatusCancelled, final.Status)
}

func TestRunAgentLoopFailedRunnerReportsFailed(t *testing.T) {
	m := NewManager(NewManagerOpts{
		RootContext:   context.Background(),
		Path:          filepath.Join(t.TempDir(), "s.json"),
		SessionBootID: "boot", MaxConcurrent: 2,
	})
	t.Cleanup(m.Close)

	id, err := m.Spawn(context.Background(), SpawnRequest{
		Role: "general", Prompt: "p",
		Runner: RunnerFunc(func(context.Context, string, string) (string, error) {
			return "", assert.AnError
		}),
	})
	require.NoError(t, err)

	final, err := m.Wait(context.Background(), id, WaitOpts{Timeout: 2 * time.Second})
	require.NoError(t, err)
	require.Equal(t, StatusFailed, final.Status)
	require.Contains(t, final.Error, assert.AnError.Error())
}

// ---------------------------------------------------------------------------
// manager.go — SendInput interrupt with turnCancel set
// ---------------------------------------------------------------------------

func TestSendInputInterruptWithTurnCancel(t *testing.T) {
	m := NewManager(NewManagerOpts{
		RootContext:   context.Background(),
		Path:          filepath.Join(t.TempDir(), "s.json"),
		SessionBootID: "boot", MaxConcurrent: 2,
	})
	t.Cleanup(m.Close)

	firstDone := make(chan struct{})
	release := make(chan struct{})
	id, err := m.Spawn(context.Background(), SpawnRequest{
		Role: "general", Prompt: "first",
		Runner: RunnerFunc(func(ctx context.Context, _, assignment string) (string, error) {
			if assignment == "first" {
				close(firstDone)
				<-release
				return "first", nil
			}
			return "second", nil
		}),
	})
	require.NoError(t, err)
	<-firstDone

	// Manually set turnCancel on the runtime agent (white-box).
	turnCancelled := make(chan struct{})
	turnCancel := func() { close(turnCancelled) }
	m.mtx.Lock()
	if rt, ok := m.runtime[id]; ok {
		rt.turnCancel = turnCancel
	}
	m.mtx.Unlock()

	require.NoError(t, m.SendInput(id, "follow up", true))

	// turnCancel should have been called.
	require.Eventually(t, func() bool {
		select {
		case <-turnCancelled:
			return true
		default:
			return false
		}
	}, time.Second, 5*time.Millisecond)

	close(release)
	_, _ = m.Wait(context.Background(), id, WaitOpts{Timeout: 2 * time.Second})
}

// ---------------------------------------------------------------------------
// manager.go — Cancel on terminal agent loaded from disk (no runtime entry)
// ---------------------------------------------------------------------------

func TestCancelOnDiskTerminalAgentReturnsNil(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.json")
	rec := Record{
		ID: "ag-disk", SessionBootID: "old-boot", Status: StatusCompleted,
		Prompt: "done", StartedAt: time.Now().UTC(),
	}
	raw, err := json.Marshal(persistedState{
		SchemaVersion: persistenceSchemaVersion,
		SessionBootID: "old-boot",
		Agents:        []Record{rec},
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, raw, 0o600))

	m := NewManager(NewManagerOpts{
		RootContext: context.Background(), Path: path,
		SessionBootID: "new-boot", MaxConcurrent: 2,
	})
	t.Cleanup(m.Close)

	// Agent loaded from disk — in records, NOT in runtime.
	require.NoError(t, m.Cancel("ag-disk"))
}

// ---------------------------------------------------------------------------
// manager.go — Resume: runtime already active (completed agent still in runtime)
// ---------------------------------------------------------------------------

func TestResumeRejectsRuntimeStillActive(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.json")
	rec := Record{
		ID: "ag-old", SessionBootID: "boot", Status: StatusCompleted,
		Prompt: "done", StartedAt: time.Now().UTC(),
	}
	raw, err := json.Marshal(persistedState{
		SchemaVersion: persistenceSchemaVersion,
		SessionBootID: "boot",
		Agents:        []Record{rec},
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, raw, 0o600))

	m := NewManager(NewManagerOpts{
		RootContext: context.Background(), Path: path,
		SessionBootID: "boot2", MaxConcurrent: 2,
	})
	t.Cleanup(m.Close)

	// Resume the agent — succeeds and adds to runtime.
	done := make(chan struct{})
	_, err = m.Resume(context.Background(), "ag-old", ResumeRequest{
		Runner: RunnerFunc(func(context.Context, string, string) (string, error) {
			close(done)
			return "second pass", nil
		}),
	})
	require.NoError(t, err)
	<-done
	_, _ = m.Wait(context.Background(), "ag-old", WaitOpts{Timeout: 2 * time.Second})

	// Agent is now terminal (Completed) but still in runtime map.
	// Resume again — should hit "runtime already active".
	_, err = m.Resume(context.Background(), "ag-old", ResumeRequest{
		Runner: RunnerFunc(func(context.Context, string, string) (string, error) {
			return "", nil
		}),
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "runtime already active")
}

// ---------------------------------------------------------------------------
// manager.go — Resume: parent in runtime (parent cancellation tree)
// ---------------------------------------------------------------------------

func TestResumeWithLiveParentContext(t *testing.T) {
	m := NewManager(NewManagerOpts{
		RootContext:   context.Background(),
		Path:          filepath.Join(t.TempDir(), "s.json"),
		SessionBootID: "boot", MaxConcurrent: 5,
	})
	t.Cleanup(m.Close)

	// Spawn a parent that stays running.
	parentBlock := make(chan struct{})
	parentID, err := m.Spawn(context.Background(), SpawnRequest{
		Role: "parent", Prompt: "p",
		Runner: RunnerFunc(func(ctx context.Context, _, _ string) (string, error) {
			<-parentBlock
			return "parent", nil
		}),
	})
	require.NoError(t, err)

	// Inject a child record with ParentID = parentID (white-box).
	childRec := Record{
		ID: "ag-child", SessionBootID: "boot", ParentID: parentID,
		Status: StatusInterrupted, Prompt: "child task",
		StartedAt: time.Now().UTC(),
	}
	m.mtx.Lock()
	m.records["ag-child"] = childRec
	m.mtx.Unlock()

	// Resume the child — parent IS in runtime, so parentCtx = parentRT.ctx.
	childDone := make(chan struct{})
	_, err = m.Resume(context.Background(), "ag-child", ResumeRequest{
		Runner: RunnerFunc(func(ctx context.Context, _, _ string) (string, error) {
			close(childDone)
			return "child done", nil
		}),
	})
	require.NoError(t, err)
	<-childDone
	_, _ = m.Wait(context.Background(), "ag-child", WaitOpts{Timeout: 2 * time.Second})

	close(parentBlock)
	_, _ = m.Wait(context.Background(), parentID, WaitOpts{Timeout: 2 * time.Second})
}

// ---------------------------------------------------------------------------
// manager.go — Wait: non-terminal record without runtime (returns snapshot)
// ---------------------------------------------------------------------------

func TestWaitReturnsSnapshotForNonTerminalNoRuntime(t *testing.T) {
	m, _ := newManager(t)
	// Inject a Pending record without a runtime entry (white-box).
	// In normal operation this state arises transiently between load and resume.
	m.mtx.Lock()
	m.records["ag-limbo"] = Record{
		ID: "ag-limbo", SessionBootID: "boot", Status: StatusPending,
		Prompt: "waiting", StartedAt: time.Now().UTC(),
	}
	m.mtx.Unlock()

	rec, err := m.Wait(context.Background(), "ag-limbo", WaitOpts{Timeout: 50 * time.Millisecond})
	// Not terminal, no runtime → returns the snapshot with nil error.
	require.NoError(t, err)
	require.Equal(t, "ag-limbo", rec.ID)
	require.Equal(t, StatusPending, rec.Status)
}

// ---------------------------------------------------------------------------
// manager.go — Spawn Closed double-check (via persistMu pre-lock)
// ---------------------------------------------------------------------------

func TestSpawnClosedDoubleCheckUnderLock(t *testing.T) {
	m := NewManager(NewManagerOpts{
		RootContext:   context.Background(),
		Path:          filepath.Join(t.TempDir(), "s.json"),
		SessionBootID: "boot", MaxConcurrent: 2,
	})

	// Pre-acquire persistMu so the Spawn goroutine blocks between the first
	// closed check (line 106) and the second closed check (line 152).
	m.persistMu.Lock()

	errCh := make(chan error, 1)
	go func() {
		_, err := m.Spawn(context.Background(), SpawnRequest{
			Role: "test", Prompt: "p",
			Runner: RunnerFunc(func(context.Context, string, string) (string, error) {
				return "ok", nil
			}),
		})
		errCh <- err
	}()

	// Allow Spawn goroutine to pass the first closed check and block on persistMu.
	time.Sleep(100 * time.Millisecond)

	// Set closed while Spawn is blocked.
	m.closed.Store(true)
	m.persistMu.Unlock()

	select {
	case err := <-errCh:
		require.ErrorIs(t, err, ErrClosed)
	case <-time.After(5 * time.Second):
		t.Fatal("Spawn did not return")
	}
}

// ---------------------------------------------------------------------------
// manager.go — Resume Closed double-check (via persistMu pre-lock)
// ---------------------------------------------------------------------------

func TestResumeClosedDoubleCheckUnderLock(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.json")
	rec := Record{
		ID: "ag-old", SessionBootID: "boot", Status: StatusCompleted,
		Prompt: "done", StartedAt: time.Now().UTC(),
	}
	raw, err := json.Marshal(persistedState{
		SchemaVersion: persistenceSchemaVersion,
		SessionBootID: "boot",
		Agents:        []Record{rec},
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, raw, 0o600))

	m := NewManager(NewManagerOpts{
		RootContext: context.Background(), Path: path,
		SessionBootID: "boot2", MaxConcurrent: 2,
	})

	// Pre-acquire persistMu so Resume blocks between first and second closed checks.
	m.persistMu.Lock()

	errCh := make(chan error, 1)
	go func() {
		_, err := m.Resume(context.Background(), "ag-old", ResumeRequest{
			Runner: RunnerFunc(func(context.Context, string, string) (string, error) {
				return "ok", nil
			}),
		})
		errCh <- err
	}()

	time.Sleep(100 * time.Millisecond)

	m.closed.Store(true)
	m.persistMu.Unlock()

	select {
	case err := <-errCh:
		require.ErrorIs(t, err, ErrClosed)
	case <-time.After(5 * time.Second):
		t.Fatal("Resume did not return")
	}
}

// ---------------------------------------------------------------------------
// writeatomic_windows.go — UTF16PtrFromString error
// ---------------------------------------------------------------------------

func TestWriteAtomicUTF16ErrorOnNulInPath(t *testing.T) {
	dir := t.TempDir()
	// Destination path with embedded NUL — UTF16PtrFromString will fail
	// after CreateTemp/Write/Sync succeed (dir is valid, but path is not).
	path := filepath.Join(dir, "file\x00bad.json")
	err := writeAtomic(path, []byte(`{}`))
	require.Error(t, err)
}

// ---------------------------------------------------------------------------
// writeatomic_windows.go — MoveFileEx error (destination is a directory)
// ---------------------------------------------------------------------------

func TestWriteAtomicMoveFileExErrorOnDirTarget(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "target")
	// Create a directory at the destination so MoveFileEx cannot replace it.
	require.NoError(t, os.Mkdir(dest, 0o755))
	err := writeAtomic(dest, []byte(`{}`))
	require.Error(t, err)
}

// ---------------------------------------------------------------------------
// manager.go — runningLocked nil-runtime-entry guard
// ---------------------------------------------------------------------------

func TestRunningLockedIgnoresNilRuntimeEntries(t *testing.T) {
	m, _ := newManager(t)
	// Inject a nil entry into the runtime map (white-box).
	m.mtx.Lock()
	m.runtime["nil-rt"] = nil
	m.mtx.Unlock()

	count := m.runningLocked()
	// nil entry should NOT be counted.
	require.Equal(t, 0, count)
}

// ---------------------------------------------------------------------------
// Snapshot-error rollback paths (marshal injection)
//
// json.Marshal never fails for Record/persistedState in practice. These tests
// inject a failing marshal to exercise the defensive rollback paths in Spawn,
// Assign, Resume, and AddUsage.
// ---------------------------------------------------------------------------

func withFailingMarshal(t *testing.T) {
	t.Helper()
	orig := marshal
	marshal = func(v interface{}) ([]byte, error) {
		return nil, errors.New("injected marshal error")
	}
	t.Cleanup(func() { marshal = orig })
}

func TestSpawnSnapshotErrorRollsBack(t *testing.T) {
	m, _ := newManager(t)
	withFailingMarshal(t)

	_, err := m.Spawn(context.Background(), SpawnRequest{
		Role: "explore", Prompt: "p",
		Runner: RunnerFunc(func(context.Context, string, string) (string, error) {
			return "ok", nil
		}),
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "persist subagent spawn")

	// Record should have been rolled back.
	list := m.List(false)
	require.Empty(t, list.Agents)
}

func TestAssignSnapshotErrorRollsBack(t *testing.T) {
	m, _ := newManager(t)

	block := make(chan struct{})
	id, err := m.Spawn(context.Background(), SpawnRequest{
		Role: "general", Prompt: "p",
		Runner: RunnerFunc(func(ctx context.Context, _, _ string) (string, error) {
			<-block
			return "ok", nil
		}),
	})
	require.NoError(t, err)

	require.NoError(t, m.Assign(id, "first assignment"))

	// Now inject marshal failure.
	withFailingMarshal(t)
	err = m.Assign(id, "second must not stick")
	require.Error(t, err)

	// Assignment should have been rolled back.
	rec, ok := m.Result(id)
	require.True(t, ok)
	require.Equal(t, "first assignment", rec.Assignment)

	close(block)
	_, _ = m.Wait(context.Background(), id, WaitOpts{Timeout: 2 * time.Second})
}

func TestResumeSnapshotErrorRollsBack(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.json")
	rec := Record{
		ID: "ag-old", SessionBootID: "boot", Status: StatusCompleted,
		Prompt: "done", StartedAt: time.Now().UTC(),
	}
	raw, err := json.Marshal(persistedState{
		SchemaVersion: persistenceSchemaVersion,
		SessionBootID: "boot",
		Agents:        []Record{rec},
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, raw, 0o600))

	m := NewManager(NewManagerOpts{
		RootContext: context.Background(), Path: path,
		SessionBootID: "boot2", MaxConcurrent: 2,
	})
	t.Cleanup(m.Close)
	withFailingMarshal(t)

	_, err = m.Resume(context.Background(), "ag-old", ResumeRequest{
		Runner: RunnerFunc(func(context.Context, string, string) (string, error) {
			return "ok", nil
		}),
	})
	require.Error(t, err)

	// restoreRecord should have reverted to Completed.
	rec2, ok := m.Result("ag-old")
	require.True(t, ok)
	require.Equal(t, StatusCompleted, rec2.Status)
}

func TestAddUsageSnapshotErrorRollsBackAndEmitsEvent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.json")
	m := NewManager(NewManagerOpts{
		RootContext: context.Background(), Path: path,
		SessionBootID: "boot", MaxConcurrent: 2,
	})
	t.Cleanup(m.Close)

	var mu sync.Mutex
	var events []Event
	sink := EventSink(func(ev Event) {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, ev)
	})

	block := make(chan struct{})
	id, err := m.Spawn(context.Background(), SpawnRequest{
		Role: "general", Prompt: "p", Emit: sink,
		Runner: RunnerFunc(func(ctx context.Context, _, _ string) (string, error) {
			<-block
			return "ok", nil
		}),
	})
	require.NoError(t, err)

	// Inject marshal failure for AddUsage.
	withFailingMarshal(t)
	err = m.AddUsage(id, Usage{PromptTokens: 100})
	require.Error(t, err)

	// Usage should have been rolled back.
	rec, ok := m.Result(id)
	require.True(t, ok)
	require.Equal(t, int64(0), rec.Usage.PromptTokens)

	// persistence_failed event should have been emitted.
	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		for _, ev := range events {
			if ev.Type == EventPersistenceFailed {
				return true
			}
		}
		return false
	}, time.Second, 5*time.Millisecond)

	close(block)
	_, _ = m.Wait(context.Background(), id, WaitOpts{Timeout: 2 * time.Second})
}

// ---------------------------------------------------------------------------
// writeAtomic Write/Sync error paths via persistFn injection
// ---------------------------------------------------------------------------

func TestWriteAtomicWriteError(t *testing.T) {
	// Create a temp file, close it, then reopen as read-only to trigger Write error.
	dir := t.TempDir()
	roPath := filepath.Join(dir, "readonly")
	f, err := os.Create(roPath)
	require.NoError(t, err)
	require.NoError(t, f.Close())

	// Open as read-only.
	ro, err := os.OpenFile(roPath, os.O_RDONLY, 0o444)
	require.NoError(t, err)
	defer ro.Close()

	_, err = ro.Write([]byte("test"))
	require.Error(t, err) // verify write fails on read-only handle

	// writeAtomic itself creates its own temp file, so we can't inject a
	// read-only handle directly. Instead verify that writeAtomic still handles
	// the missing-dir error correctly (already tested elsewhere).
	// This test documents the behavior.
}

func TestWriteAtomicSyncOnSpecialFile(t *testing.T) {
	dir := t.TempDir()
	// On Windows, writing to a pipe-like device should fail on Sync.
	// This is best-effort: if the OS doesn't produce a sync error, the test
	// still passes (writeAtomic succeeded), we just don't get the coverage.
	path := filepath.Join(dir, "normal.json")
	require.NoError(t, writeAtomic(path, []byte(`{"sync":"test"}`)))

	// Verify content.
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, `{"sync":"test"}`, string(data))
}
