package tools

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/guard"
	"github.com/x6nux/yanshi/internal/task/work"
)

// This file verifies L7 -- the checklist as a GATE rather than a decoration --
// across the whole chain, with a real command and a real file.
//
// The gate's own package tests cover the decision function thoroughly, but
// they hand it an Evidence value constructed in Go. That leaves the command
// condition's real path untested end to end: task_gate_run has to actually
// spawn a process, read its real exit code, persist it as Evidence on the
// task, and only then can the gate resolve a gate_passed item. Every one of
// those steps is somewhere the exit code could be lost, defaulted to zero, or
// attached to the wrong task -- and a gate that silently reads a defaulted
// zero is a gate that always passes, which is indistinguishable from no gate
// at all.
//
// The file condition is exercised here too, for the same reason: against a
// real path under the manager's real verify root, not a fixture string.

// gateChainEnv is a real work manager, a real work root, and the real
// task_gate_run tool.
type gateChainEnv struct {
	mgr  *work.Manager
	root string
	gate *GuardedTool
	ctx  context.Context
}

// newGateChainEnv wires the real manager (with the verify root the composition
// root supplies) and the real gate tool over a temp project.
func newGateChainEnv(t *testing.T) *gateChainEnv {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	require.NoError(t, err)
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	st, err := work.FromDB(db, nil)
	require.NoError(t, err)

	root := t.TempDir()
	if resolved, rerr := filepath.EvalSymlinks(root); rerr == nil {
		root = resolved
	}
	// WithVerifyRoot is what bootstrap.Build calls; without it every
	// file_exists condition resolves against "" and can never be satisfied.
	mgr := work.NewManager(st, nil, work.ArtifactPolicy{}).WithVerifyRoot(root)

	ctx := WithWorkRoot(context.Background(), root)
	ctx = WithProfile(ctx, guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"*"}},
		FS:    guard.FSPerm{Read: []string{root, root + "/**"}, Write: []string{root, root + "/**"}},
		Shell: guard.ShellPerm{Policy: "denylist"},
	})
	ctx = WithTaskManager(ctx, mgr)

	return &gateChainEnv{mgr: mgr, root: root, gate: NewGateTools().Run, ctx: ctx}
}

// runGateCommand invokes the real task_gate_run and returns the exit code it
// recorded.
func (e *gateChainEnv) runGateCommand(t *testing.T, taskID, gate, command string) int {
	t.Helper()
	args, err := json.Marshal(map[string]any{
		"task_id": taskID, "gate": gate, "command": command,
	})
	require.NoError(t, err)
	out, err := runTool(e.ctx, e.gate, string(args))
	require.NoError(t, err, "task_gate_run must not abort the turn: %s", out)

	var payload struct {
		Evidence work.Evidence `json:"evidence"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &payload),
		"gate result is not the documented shape: %s", out)
	return payload.Evidence.ExitCode
}

// TestL7Chain_CommandConditionUsesTheRealExitCode is the end-to-end command
// half of L7.
//
// A task carries one checklist item conditioned on the gate "tests". The gate
// is run twice for real: first with a command that FAILS, then with one that
// SUCCEEDS. Finish must be refused after the first and allowed after the
// second, with nothing in between but the real exit codes of two real
// processes.
//
// A gate that defaulted a missing exit code to zero, or that recorded evidence
// against the wrong task, would let the first Finish through -- and that is
// the failure that looks exactly like a working feature until the day it
// matters.
func TestL7Chain_CommandConditionUsesTheRealExitCode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses `false`/`true`; the Windows equivalents differ per shell")
	}
	e := newGateChainEnv(t)
	task, err := e.mgr.Create(context.Background(), work.CreateReq{Title: "ship it", Prompt: "p"})
	require.NoError(t, err)
	// The lifecycle is pending -> running -> completed; Finish from pending is
	// refused by the state machine, which would mask the gate's own verdict.
	require.NoError(t, e.mgr.Start(context.Background(), task.ID))

	_, err = e.mgr.SetChecklist(context.Background(), task.ID, work.Checklist{
		Items: []work.ChecklistItem{{
			ID: 1, Content: "the test suite passes", Status: work.ChecklistPending,
			Verify: work.ChecklistCondition{Kind: work.ConditionGatePassed, Target: "tests"},
		}},
	})
	require.NoError(t, err)

	// --- the failing run -------------------------------------------------
	if code := e.runGateCommand(t, task.ID, "tests", "false"); code == 0 {
		t.Fatal("a failing command recorded exit code 0; every gate condition would " +
			"then pass regardless of what the command did")
	}
	err = e.mgr.Finish(context.Background(), task.ID, work.StatusCompleted, "done")
	require.Error(t, err, "Finish must be refused while the gate's real exit code is nonzero")
	require.True(t, errors.Is(err, work.ErrChecklistIncomplete),
		"the refusal must be the checklist gate, not an unrelated error: %v", err)

	// --- the passing run -------------------------------------------------
	if code := e.runGateCommand(t, task.ID, "tests", "true"); code != 0 {
		t.Fatalf("a succeeding command recorded exit code %d", code)
	}
	err = e.mgr.Finish(context.Background(), task.ID, work.StatusCompleted, "done")
	require.NoError(t, err,
		"Finish must be allowed once the gate really exited zero; if it is still "+
			"refused, the newer evidence is not reaching the gate")
}

// TestL7Chain_FileConditionUsesTheRealFilesystem is the file half, against the
// manager's real verify root.
//
// The item is conditioned on a path that does not exist yet; Finish must be
// refused. The file is then really created and Finish must succeed. Nothing
// about the task changes in between -- only the filesystem -- which is the
// property that makes the condition a fact about the world rather than a claim
// by the model.
func TestL7Chain_FileConditionUsesTheRealFilesystem(t *testing.T) {
	e := newGateChainEnv(t)
	task, err := e.mgr.Create(context.Background(), work.CreateReq{Title: "write it", Prompt: "p"})
	require.NoError(t, err)
	require.NoError(t, e.mgr.Start(context.Background(), task.ID))

	_, err = e.mgr.SetChecklist(context.Background(), task.ID, work.Checklist{
		Items: []work.ChecklistItem{{
			ID: 1, Content: "the migration file exists", Status: work.ChecklistPending,
			Verify: work.ChecklistCondition{Kind: work.ConditionFileExists, Target: "migration.sql"},
		}},
	})
	require.NoError(t, err)

	err = e.mgr.Finish(context.Background(), task.ID, work.StatusCompleted, "done")
	require.Error(t, err, "Finish must be refused while the required file is absent")
	require.True(t, errors.Is(err, work.ErrChecklistIncomplete), "unexpected error: %v", err)

	require.NoError(t, os.WriteFile(filepath.Join(e.root, "migration.sql"), []byte("-- sql"), 0o644))

	err = e.mgr.Finish(context.Background(), task.ID, work.StatusCompleted, "done")
	require.NoError(t, err,
		"Finish must be allowed once the real file exists; if not, verifyRoot is not "+
			"the directory the file was written to")
}

// TestL7Chain_ModelCannotTickAConditionedItem is the trust boundary.
//
// The item is marked done BY THE MODEL while its condition is unsatisfied.
// Finish must still be refused: a condition exists precisely so the tick mark
// is the system's conclusion rather than the model's assertion, and a gate
// that honours a self-reported status has no effect on the only case it was
// built for.
func TestL7Chain_ModelCannotTickAConditionedItem(t *testing.T) {
	e := newGateChainEnv(t)
	task, err := e.mgr.Create(context.Background(), work.CreateReq{Title: "claim it", Prompt: "p"})
	require.NoError(t, err)
	require.NoError(t, e.mgr.Start(context.Background(), task.ID))

	_, err = e.mgr.SetChecklist(context.Background(), task.ID, work.Checklist{
		Items: []work.ChecklistItem{{
			ID: 1, Content: "the migration file exists",
			// The model says done.
			Status: work.ChecklistDone,
			// The world says otherwise.
			Verify: work.ChecklistCondition{Kind: work.ConditionFileExists, Target: "never-created.sql"},
		}},
	})
	require.NoError(t, err)

	err = e.mgr.Finish(context.Background(), task.ID, work.StatusCompleted, "done")
	require.Error(t, err,
		"a model-asserted 'done' overrode an unsatisfied condition: the gate trusts "+
			"the model, which is the failure it exists to prevent")

	// And the stored checklist must now show the corrected status, so an
	// operator asking "why was I blocked" can see it.
	got, err := e.mgr.Read(context.Background(), task.ID)
	require.NoError(t, err)
	require.Equal(t, work.ChecklistPending, got.Checklist.Items[0].Status,
		"the verified verdict must be persisted over the self-reported one")
}

// TestL7Chain_GateEvidenceIsRejectedForAnUnknownTask pins the ownership check.
//
// Evidence recorded against a hallucinated task id would be orphaned: written,
// attached to nothing, and invisible to every gate. The tool must refuse
// before running the command, so a mistyped id costs nothing rather than a
// two-minute test suite whose result is discarded.
//
// The refusal arrives as a tool RESULT, not a Go error: a model that mistyped
// an id should correct it and carry on, and a Go error out of a tool node
// aborts the whole turn (ADR-0001). So the assertion is on the result text,
// and on the id appearing in it -- a refusal that does not name what it
// refused leaves the model nothing to fix.
func TestL7Chain_GateEvidenceIsRejectedForAnUnknownTask(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses `true`; the Windows equivalent differs per shell")
	}
	e := newGateChainEnv(t)
	args, err := json.Marshal(map[string]any{
		"task_id": "task_does_not_exist", "gate": "tests", "command": "true",
	})
	require.NoError(t, err)

	out, err := runTool(e.ctx, e.gate, string(args))
	require.NoError(t, err, "the refusal must not abort the turn")
	require.Contains(t, out, "task_does_not_exist",
		"the refusal must name the id so the model can correct it: %s", out)

	// And nothing may have been recorded: an orphaned Evidence row is exactly
	// what the pre-check exists to prevent.
	var payload struct {
		Evidence work.Evidence `json:"evidence"`
	}
	if json.Unmarshal([]byte(out), &payload) == nil && payload.Evidence.ID != "" {
		t.Fatalf("evidence was recorded for an unknown task: %+v", payload.Evidence)
	}
}
