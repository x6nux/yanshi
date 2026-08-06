package tools

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/task/work"
)

// runGateTool runs task_gate_run and returns the recorded Evidence.
func runGateTool(t *testing.T, ctx context.Context, taskID, gate, env, command string) (work.Evidence, error) {
	t.Helper()
	argsJSON, err := json.Marshal(map[string]string{
		"task_id": taskID, "gate": gate, "command": command, "env": env,
	})
	require.NoError(t, err)
	out, err := runTool(ctx, NewGateTools().Run, string(argsJSON))
	if err != nil {
		return work.Evidence{}, err
	}
	// A guarded tool reports its own failures in the result string with a "✗"
	// marker rather than as a Go error, so a caller that only checks err reads
	// every refusal as a success.
	if strings.HasPrefix(strings.TrimSpace(out), "✗") {
		return work.Evidence{}, errors.New(out)
	}
	var payload struct {
		Evidence work.Evidence `json:"evidence"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &payload), "gate output: %s", out)
	return payload.Evidence, nil
}

// catCmd returns a single-argv command that prints a file. Pipes and
// redirection are refused by the guard's metacharacter rule, so writing the
// bytes first and reading them back is the only way to make a gate produce
// large output.
func catCmd(path string) (env, command string) {
	if runtime.GOOS == "windows" {
		return "cmd", `cmd /c type ` + filepath.FromSlash(path)
	}
	return "sh", `cat ` + path
}

// TestGateEvidenceIsCompleteInTheDatabase is the evidence-structure clause,
// read back out of SQLite.
//
// TestTaskGateRun_PassEvidence asserts four fields (Classification, ExitCode,
// Gate, Command) — all of which the tool copies straight from its own
// arguments — against a FakeManager. ID, Cwd, DurationMs, RecordedAt and
// Summary were never checked anywhere, and nothing made the round trip through
// the store the evidence is supposed to live in.
//
// ledger: A2/DT2#1 gate 证据结构完整
func TestGateEvidenceIsCompleteInTheDatabase(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "work.db")
	mgr := fileBackedManager(t, dbPath)
	task, err := mgr.Create(context.Background(), work.CreateReq{Title: "x", Prompt: "p"})
	require.NoError(t, err)

	root := t.TempDir()
	ctx := gateCtx(t, mgr, root)
	env, command := gatePrintCmd()

	before := time.Now().Unix()
	ev, err := runGateTool(t, ctx, task.ID, "test", env, command)
	require.NoError(t, err)
	after := time.Now().Unix()

	// Read it back rather than trusting the tool's own return value: the store
	// is where it has to survive.
	got, err := mgr.Read(context.Background(), task.ID)
	require.NoError(t, err)
	require.Len(t, got.Gates, 1, "the gate evidence is not attached to the task")
	stored := got.Gates[0]

	assert.Equal(t, ev, stored, "what the tool returned and what the store kept differ")

	assert.NotEmpty(t, stored.ID, "evidence has no id, so two runs of the same gate are indistinguishable")
	assert.Equal(t, "test", stored.Gate)
	assert.Equal(t, command, stored.Command)
	assert.Equal(t, "pass", stored.Classification)
	assert.Zero(t, stored.ExitCode)
	assert.Contains(t, stored.Summary, "hello", "the output summary was not kept")

	// Cwd defaults to the work root, resolved. Empty here would mean the
	// evidence cannot be reproduced: the same command means different things in
	// different directories.
	require.NotEmpty(t, stored.Cwd)
	resolvedRoot, err := filepath.EvalSymlinks(root)
	require.NoError(t, err)
	assert.Equal(t, resolvedRoot, stored.Cwd)

	assert.GreaterOrEqual(t, stored.RecordedAt, before,
		"RecordedAt predates the run")
	assert.LessOrEqual(t, stored.RecordedAt, after,
		"RecordedAt is in the future")
}

// TestGateRecordsRealDuration is the duration half of the exit-code/duration
// clause.
//
// DurationMs appears in the test suite only as an input when constructing an
// Evidence literal (DurationMs: 10 / 1 / 5). Setting it to a constant zero in
// gate.go reddens nothing: the field was written and never read back.
//
// ledger: A2/DT2#4 退出码/duration 准确
func TestGateRecordsRealDuration(t *testing.T) {
	mgr := fileBackedManager(t, filepath.Join(t.TempDir(), "work.db"))
	task, err := mgr.Create(context.Background(), work.CreateReq{Title: "x", Prompt: "p"})
	require.NoError(t, err)
	ctx := gateCtx(t, mgr, t.TempDir())

	env, command := "sh", `sleep 0.3`
	if runtime.GOOS == "windows" {
		// No sleep on stock Windows; ping to localhost is the usual stand-in.
		env, command = "cmd", `cmd /c ping -n 2 127.0.0.1`
	}

	started := time.Now()
	ev, err := runGateTool(t, ctx, task.ID, "slow", env, command)
	require.NoError(t, err)
	wall := time.Since(started)

	assert.GreaterOrEqual(t, ev.DurationMs, int64(200),
		"a command that took %v was recorded as %dms; the duration is not being measured",
		wall, ev.DurationMs)
	assert.LessOrEqual(t, ev.DurationMs, wall.Milliseconds()+500,
		"the recorded duration (%dms) exceeds the wall time of the whole call (%v)",
		ev.DurationMs, wall)
}

// TestGateClassifiesAFailedLaunchAsError covers the exitCode = -1 branch.
//
// A command that never started is not the same as one that ran and failed, and
// the classification says so ("error" vs "fail") — but the branch that produces
// it had no test at all.
//
// ledger: A2/DT2#4 退出码/duration 准确
func TestGateClassifiesAFailedLaunchAsError(t *testing.T) {
	mgr := fileBackedManager(t, filepath.Join(t.TempDir(), "work.db"))
	task, err := mgr.Create(context.Background(), work.CreateReq{Title: "x", Prompt: "p"})
	require.NoError(t, err)
	ctx := gateCtx(t, mgr, t.TempDir())

	// A shell that does not exist: CombinedOutput fails without an ExitError.
	ev, err := runGateTool(t, ctx, task.ID, "broken", "sh", "yanshi-no-such-binary-9f3a")
	require.NoError(t, err, "a command that cannot run is structured evidence, not a Go error")

	// Depending on the platform the shell wrapper may run and exit non-zero
	// (fail) or fail to launch at all (error). Both are non-pass, and the
	// distinction that matters is that a missing binary is never "pass".
	assert.NotEqual(t, "pass", ev.Classification,
		"a command that does not exist was classified as passing")
	assert.NotZero(t, ev.ExitCode, "a failed command recorded exit code 0")
	assert.Equal(t, work.ClassificationFromExitCode(ev.ExitCode), ev.Classification,
		"the classification does not follow from the exit code it was recorded with")
}

// TestGateSpillsLargeOutputToAnArtifact is the artifact clause.
//
// TestTaskGateRun_SpillToArtifact asserts the OPPOSITE of its name: its own
// comment says large output is hard to produce, so it checks
// `assert.Empty(LogArtifactID)` on a small one. Deleting the whole WriteArtifact
// branch, or flipping `>` to `<`, leaves it green.
//
// Large output is producible — write the bytes first, then cat them back. The
// pipe that made it look impossible is refused by the guard's metacharacter
// rule; a file is not.
//
// ledger: A2/DT2#2 大输出成 artifact
func TestGateSpillsLargeOutputToAnArtifact(t *testing.T) {
	mgr := fileBackedManager(t, filepath.Join(t.TempDir(), "work.db"))
	task, err := mgr.Create(context.Background(), work.CreateReq{Title: "x", Prompt: "p"})
	require.NoError(t, err)

	root := t.TempDir()
	// One recognisable line repeated past the threshold, so the artifact's
	// content can be checked rather than just its length.
	const line = "GATE_SPILL_MARKER 0123456789abcdef\n"
	big := strings.Repeat(line, (SpillThreshold/len(line))+64)
	require.Greater(t, len(big), SpillThreshold)
	bigPath := filepath.Join(root, "big.log")
	require.NoError(t, os.WriteFile(bigPath, []byte(big), 0o600))

	ctx := gateCtx(t, mgr, root)
	env, command := catCmd(bigPath)
	ev, err := runGateTool(t, ctx, task.ID, "bulk", env, command)
	require.NoError(t, err)

	require.NotEmpty(t, ev.LogArtifactID,
		"%d bytes of output were not spilled to an artifact (threshold %d); the summary "+
			"alone would put the whole log in the model's context", len(big), SpillThreshold)

	art, err := mgr.ReadArtifact(context.Background(), ev.LogArtifactID)
	require.NoError(t, err, "the artifact id in the evidence does not resolve")
	assert.Equal(t, task.ID, art.TaskID)
	assert.Greater(t, art.Size, int64(SpillThreshold),
		"the artifact is smaller than the threshold that triggered it")

	// The bytes have to be retrievable, otherwise the spill is a deletion with
	// extra steps. ContentRef is root-relative by design (Manager.ReadArtifact
	// returns metadata only; artifact_read resolves it against the work root),
	// so the resolution happens here.
	require.NotEmpty(t, art.ContentRef)
	refPath, err := work.SecureArtifactPath(root, art.ContentRef)
	require.NoError(t, err, "the artifact's content_ref does not resolve inside the work root")
	stored, err := os.ReadFile(refPath)
	require.NoError(t, err, "the artifact's content_ref does not point at a readable file")
	assert.Equal(t, len(big), len(stored), "the spilled log lost bytes")
	assert.True(t, strings.HasPrefix(string(stored), "GATE_SPILL_MARKER"),
		"the artifact does not hold the command's output")
}

// TestGateAttachesToTheNamedTaskOnly is the attachment clause.
//
// runGate never checked task_id. The schema declares
// FOREIGN KEY(task_id) REFERENCES task_work(id), but internal/store never sets
// PRAGMA foreign_keys=ON and SQLite leaves it off, so evidence for a mistyped
// id was written and silently orphaned. Every gate tool test used
// work.FakeManager, which DOES reject unknown ids — the fake was stricter than
// the real thing, which is the worst arrangement: it produced a passing test
// asserting exactly the safety the shipped code lacked.
//
// ledger: A2/DT2#3 挂到正确 task
func TestGateAttachesToTheNamedTaskOnly(t *testing.T) {
	mgr := fileBackedManager(t, filepath.Join(t.TempDir(), "work.db"))
	ctx := context.Background()
	a, err := mgr.Create(ctx, work.CreateReq{Title: "a", Prompt: "p"})
	require.NoError(t, err)
	b, err := mgr.Create(ctx, work.CreateReq{Title: "b", Prompt: "p"})
	require.NoError(t, err)

	gctx := gateCtx(t, mgr, t.TempDir())
	env, command := gatePrintCmd()
	_, err = runGateTool(t, gctx, a.ID, "test", env, command)
	require.NoError(t, err)

	gotA, err := mgr.Read(ctx, a.ID)
	require.NoError(t, err)
	require.Len(t, gotA.Gates, 1)

	gotB, err := mgr.Read(ctx, b.ID)
	require.NoError(t, err)
	assert.Empty(t, gotB.Gates, "evidence recorded against task A shows up on task B")

	// An id that does not exist must be refused, not written into the void.
	_, err = runGateTool(t, gctx, "wt-does-not-exist", "test", env, command)
	require.Error(t, err,
		"a gate for a non-existent task succeeded; the evidence is in the database "+
			"attached to nothing and no task will ever show it")
	assert.Contains(t, err.Error(), "wt-does-not-exist",
		"the error does not name the id that was wrong")
}
