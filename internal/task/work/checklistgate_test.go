package work

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func item(id int, content string, status ChecklistItemStatus, cond ChecklistCondition) ChecklistItem {
	return ChecklistItem{ID: id, Content: content, Status: status, Verify: cond}
}

func TestChecklistConditionIsSet_Table(t *testing.T) {
	cases := []struct {
		name string
		cond ChecklistCondition
		want bool
		why  string
	}{
		{"zero value", ChecklistCondition{}, false, "every pre-L7 item is this shape"},
		{"kind with no target", ChecklistCondition{Kind: ConditionFileExists}, false,
			"a condition that cannot be checked must fall back to the recorded status, " +
				"not wedge the task with no visible reason"},
		{"kind with blank target", ChecklistCondition{Kind: ConditionGatePassed, Target: "   "}, false,
			"whitespace is not a gate name"},
		{"target with no kind", ChecklistCondition{Target: "out.txt"}, false,
			"a target with nothing to do with it is not a condition"},
		{"file_exists", ChecklistCondition{Kind: ConditionFileExists, Target: "out.txt"}, true, ""},
		{"gate_passed", ChecklistCondition{Kind: ConditionGatePassed, Target: "test"}, true, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, tc.cond.IsSet(), tc.why)
		})
	}
}

func TestVerifyChecklist_Table(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "present.txt"), []byte("x"), 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "sub"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "sub", "nested.go"), []byte("x"), 0o600))

	gates := []Evidence{
		{Gate: "test", ExitCode: 0, Classification: "pass"},
		{Gate: "lint", ExitCode: 1, Classification: "fail"},
		{Gate: "build", ExitCode: -1, Classification: "error"},
	}

	cases := []struct {
		name string
		root string
		in   ChecklistItem
		want ChecklistItemStatus
		why  string
	}{
		{
			name: "no condition keeps the recorded status",
			root: root,
			in:   item(1, "think about it", ChecklistDone, ChecklistCondition{}),
			want: ChecklistDone,
			why:  "there is no other information about an unconditioned item",
		},
		{
			name: "no condition keeps pending too",
			root: root,
			in:   item(1, "think about it", ChecklistPending, ChecklistCondition{}),
			want: ChecklistPending,
		},
		{
			name: "an existing file ticks the item the model never ticked",
			root: root,
			in:   item(1, "write it", ChecklistPending, ChecklistCondition{Kind: ConditionFileExists, Target: "present.txt"}),
			want: ChecklistDone,
			why:  "the system decides, not the model's self-report",
		},
		{
			name: "a nested path resolves under the root",
			root: root,
			in:   item(1, "write it", ChecklistPending, ChecklistCondition{Kind: ConditionFileExists, Target: "sub/nested.go"}),
			want: ChecklistDone,
		},
		{
			name: "a missing file UN-TICKS an item the model marked done",
			root: root,
			in:   item(1, "write it", ChecklistDone, ChecklistCondition{Kind: ConditionFileExists, Target: "absent.txt"}),
			want: ChecklistPending,
			why:  "'the model said it wrote the file' and 'the file is there' differ exactly when it counts",
		},
		{
			name: "an escaping path is unsatisfied, not an error",
			root: root,
			in:   item(1, "write it", ChecklistDone, ChecklistCondition{Kind: ConditionFileExists, Target: "../outside.txt"}),
			want: ChecklistPending,
			why:  "a path outside the project is not a file this task produced",
		},
		{
			name: "no root makes every file condition unsatisfied",
			root: "",
			in:   item(1, "write it", ChecklistDone, ChecklistCondition{Kind: ConditionFileExists, Target: "present.txt"}),
			want: ChecklistPending,
			why: "an unverifiable claim is not a verified one; the caller that forgot the root " +
				"must see a blocked task, not a waved-through one",
		},
		{
			name: "a passing gate ticks the item",
			root: root,
			in:   item(1, "tests green", ChecklistPending, ChecklistCondition{Kind: ConditionGatePassed, Target: "test"}),
			want: ChecklistDone,
		},
		{
			name: "a failing gate un-ticks it",
			root: root,
			in:   item(1, "lint clean", ChecklistDone, ChecklistCondition{Kind: ConditionGatePassed, Target: "lint"}),
			want: ChecklistPending,
			why:  "exit code 1 is not exit code 0",
		},
		{
			name: "a gate that errored is not a pass",
			root: root,
			in:   item(1, "builds", ChecklistDone, ChecklistCondition{Kind: ConditionGatePassed, Target: "build"}),
			want: ChecklistPending,
			why:  "a negative exit code means the command never really ran",
		},
		{
			name: "a gate that was never recorded is unsatisfied",
			root: root,
			in:   item(1, "e2e green", ChecklistDone, ChecklistCondition{Kind: ConditionGatePassed, Target: "e2e"}),
			want: ChecklistPending,
			why:  "no evidence is not evidence of success",
		},
		{
			name: "an unknown kind is unsatisfied",
			root: root,
			in:   item(1, "?", ChecklistDone, ChecklistCondition{Kind: "sacrifice_a_goat", Target: "x"}),
			want: ChecklistPending,
			why:  "a kind this build cannot check is a claim it cannot confirm",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := VerifyChecklist(Checklist{Items: []ChecklistItem{tc.in}}, tc.root, gates)
			require.Len(t, got.Items, 1)
			assert.Equal(t, tc.want, got.Items[0].Status, tc.why)
		})
	}
}

// TestVerifyChecklistDoesNotMutateItsInput. The verified copy is persisted by
// the caller; rewriting the argument in place would corrupt a snapshot the
// caller may still be comparing against.
func TestVerifyChecklistDoesNotMutateItsInput(t *testing.T) {
	in := Checklist{Items: []ChecklistItem{
		item(1, "write it", ChecklistDone, ChecklistCondition{Kind: ConditionFileExists, Target: "nope.txt"}),
	}}
	got := VerifyChecklist(in, t.TempDir(), nil)
	assert.Equal(t, ChecklistDone, in.Items[0].Status, "the input must be untouched")
	assert.Equal(t, ChecklistPending, got.Items[0].Status)

	empty := VerifyChecklist(Checklist{}, t.TempDir(), nil)
	assert.Empty(t, empty.Items)
}

func TestUnmetSummary_Table(t *testing.T) {
	cases := []struct {
		name  string
		items []ChecklistItem
		want  string
	}{
		{"empty checklist", nil, ""},
		{"all done", []ChecklistItem{
			item(1, "a", ChecklistDone, ChecklistCondition{}),
			item(2, "b", ChecklistDone, ChecklistCondition{}),
		}, ""},
		{"one pending", []ChecklistItem{
			item(1, "write the parser", ChecklistPending, ChecklistCondition{}),
		}, "1 unmet: #1 write the parser"},
		{"in_progress counts as unmet", []ChecklistItem{
			item(1, "half done", ChecklistInProgress, ChecklistCondition{}),
		}, "1 unmet: #1 half done"},
		{"names them rather than counting", []ChecklistItem{
			item(1, "write the parser", ChecklistPending, ChecklistCondition{}),
			item(2, "done", ChecklistDone, ChecklistCondition{}),
			item(3, "tests pass", ChecklistPending, ChecklistCondition{}),
		}, "2 unmet: #1 write the parser; #3 tests pass"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, Checklist{Items: tc.items}.UnmetSummary())
		})
	}
}

// TestInProgressIsNotDone states separately what the summary table implies: a
// gate passable by moving every item halfway is not a gate.
func TestInProgressIsNotDone(t *testing.T) {
	c := Checklist{Items: []ChecklistItem{
		item(1, "a", ChecklistInProgress, ChecklistCondition{}),
		item(2, "b", ChecklistDone, ChecklistCondition{}),
	}}
	unmet := c.UnmetItems()
	require.Len(t, unmet, 1)
	assert.Equal(t, 1, unmet[0].ID)
}

// storeManager builds a real SQLite-backed Manager, which is the only way to
// check the migration and the persistence of the verify columns.
func storeManager(t *testing.T) (*Manager, *Store) {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	require.NoError(t, err)
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	st, err := FromDB(db, nil)
	require.NoError(t, err)
	return NewManager(st, nil, ArtifactPolicy{}), st
}

// TestFinishIsGatedOnTheChecklist is L7's central claim, run against BOTH
// manager implementations.
//
// Before this, Checklist had no consumer but CompletionPct: a task could be
// finished with every item pending and nothing noticed. Running the real
// Manager and the FakeManager through the same table is what stops the gate
// from being enforced in production and absent from the tests meant to prove
// it (the fake is what every tool test uses).
func TestFinishIsGatedOnTheChecklist(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "done.txt"), []byte("x"), 0o600))

	impls := map[string]func() ManagerLike{
		"Manager": func() ManagerLike {
			m, _ := storeManager(t)
			return m.WithVerifyRoot(root)
		},
		"FakeManager": func() ManagerLike { return NewFakeManager().WithVerifyRoot(root) },
	}

	cases := []struct {
		name      string
		checklist Checklist
		status    Status
		wantErr   bool
		why       string
	}{
		{
			name: "no checklist completes freely", checklist: Checklist{}, status: StatusCompleted, wantErr: false,
			why: "a task that never used a checklist must behave exactly as before",
		},
		{
			name: "all items done completes",
			checklist: Checklist{Items: []ChecklistItem{
				item(1, "a", ChecklistDone, ChecklistCondition{}),
			}},
			status: StatusCompleted, wantErr: false,
		},
		{
			name: "a pending item blocks completion",
			checklist: Checklist{Items: []ChecklistItem{
				item(1, "write the parser", ChecklistPending, ChecklistCondition{}),
			}},
			status: StatusCompleted, wantErr: true,
			why: "this is the whole feature: the checklist is a condition, not a decoration",
		},
		{
			name: "a satisfied file condition completes even when the model never ticked it",
			checklist: Checklist{Items: []ChecklistItem{
				item(1, "write it", ChecklistPending, ChecklistCondition{Kind: ConditionFileExists, Target: "done.txt"}),
			}},
			status: StatusCompleted, wantErr: false,
			why: "the system decides the tick",
		},
		{
			name: "an unsatisfied file condition blocks even when the model DID tick it",
			checklist: Checklist{Items: []ChecklistItem{
				item(1, "write it", ChecklistDone, ChecklistCondition{Kind: ConditionFileExists, Target: "never.txt"}),
			}},
			status: StatusCompleted, wantErr: true,
			why: "self-reported completion is exactly what the gate must not take on trust",
		},
		{
			name: "failing is NOT gated",
			checklist: Checklist{Items: []ChecklistItem{
				item(1, "write the parser", ChecklistPending, ChecklistCondition{}),
			}},
			status: StatusFailed, wantErr: false,
			why: "unfinished items are what failing MEANS; gating it would leave the task " +
				"stuck at running with no legal exit",
		},
	}

	for implName, newImpl := range impls {
		for _, tc := range cases {
			t.Run(implName+"/"+tc.name, func(t *testing.T) {
				ctx := context.Background()
				mgr := newImpl()
				task, err := mgr.Create(ctx, CreateReq{Title: "t", Prompt: "p"})
				require.NoError(t, err)
				if len(tc.checklist.Items) > 0 {
					_, err = mgr.SetChecklist(ctx, task.ID, tc.checklist)
					require.NoError(t, err)
				}
				require.NoError(t, mgr.Start(ctx, task.ID))

				err = mgr.Finish(ctx, task.ID, tc.status, "note")
				if !tc.wantErr {
					require.NoError(t, err, tc.why)
					got, rerr := mgr.Read(ctx, task.ID)
					require.NoError(t, rerr)
					assert.Equal(t, tc.status, got.Status)
					return
				}
				require.Error(t, err, tc.why)
				assert.True(t, errors.Is(err, ErrChecklistIncomplete),
					"the refusal must be distinguishable from a store failure, got %v", err)

				got, rerr := mgr.Read(ctx, task.ID)
				require.NoError(t, rerr)
				assert.Equal(t, StatusRunning, got.Status, "a blocked completion must not move the task")
			})
		}
	}
}

// TestBlockedCompletionPersistsTheVerdict. Being refused without being told
// WHICH item blocked you is a dead end; the verified checklist and a timeline
// entry are what make the refusal actionable.
func TestBlockedCompletionPersistsTheVerdict(t *testing.T) {
	root := t.TempDir()
	mgr, _ := storeManager(t)
	mgr = mgr.WithVerifyRoot(root)
	ctx := context.Background()

	task, err := mgr.Create(ctx, CreateReq{Title: "t", Prompt: "p"})
	require.NoError(t, err)
	_, err = mgr.SetChecklist(ctx, task.ID, Checklist{Items: []ChecklistItem{
		item(1, "write it", ChecklistDone, ChecklistCondition{Kind: ConditionFileExists, Target: "never.txt"}),
		item(2, "already fine", ChecklistDone, ChecklistCondition{}),
	}})
	require.NoError(t, err)
	require.NoError(t, mgr.Start(ctx, task.ID))

	err = mgr.Finish(ctx, task.ID, StatusCompleted, "done!")
	require.ErrorIs(t, err, ErrChecklistIncomplete)
	assert.Contains(t, err.Error(), "#1 write it", "the error must name what to do next, not just count")

	got, err := mgr.Read(ctx, task.ID)
	require.NoError(t, err)
	assert.Equal(t, ChecklistPending, got.Checklist.Items[0].Status,
		"the machine's verdict must be persisted, or 'why was I blocked' needs a manual re-run")
	assert.Equal(t, ChecklistDone, got.Checklist.Items[1].Status)

	var blocked bool
	for _, e := range got.Timeline {
		if e.Kind == "completion_blocked" {
			blocked = true
			assert.Contains(t, e.Summary, "#1 write it")
		}
	}
	assert.True(t, blocked, "the refusal must be on the timeline")
}

// TestGateConditionUsesRecordedEvidence closes the "command exit code" half of
// L7 through the path it is actually reachable by: task_gate_run records
// Evidence, and the checklist reads it.
//
// The indirection is the point. Running a command from this package would be a
// subprocess launched OUTSIDE tools.Authorize, with the command string coming
// from the model — an unauthenticated shell wearing a checklist item's name.
func TestGateConditionUsesRecordedEvidence(t *testing.T) {
	mgr, _ := storeManager(t)
	mgr = mgr.WithVerifyRoot(t.TempDir())
	ctx := context.Background()

	task, err := mgr.Create(ctx, CreateReq{Title: "t", Prompt: "p"})
	require.NoError(t, err)
	_, err = mgr.SetChecklist(ctx, task.ID, Checklist{Items: []ChecklistItem{
		item(1, "tests pass", ChecklistPending, ChecklistCondition{Kind: ConditionGatePassed, Target: "test"}),
	}})
	require.NoError(t, err)
	require.NoError(t, mgr.Start(ctx, task.ID))

	// Nothing recorded yet: no evidence is not evidence of success.
	require.ErrorIs(t, mgr.Finish(ctx, task.ID, StatusCompleted, ""), ErrChecklistIncomplete)

	// A FAILING gate is still not a pass.
	require.NoError(t, mgr.RecordGate(ctx, task.ID, Evidence{
		ID: NewID("ev"), Gate: "test", ExitCode: 1,
		Classification: ClassificationFromExitCode(1), Summary: "2 failures",
	}))
	require.ErrorIs(t, mgr.Finish(ctx, task.ID, StatusCompleted, ""), ErrChecklistIncomplete)

	// Exit zero ticks it, and the completion goes through.
	require.NoError(t, mgr.RecordGate(ctx, task.ID, Evidence{
		ID: NewID("ev"), Gate: "test", ExitCode: 0,
		Classification: ClassificationFromExitCode(0), Summary: "ok",
	}))
	require.NoError(t, mgr.Finish(ctx, task.ID, StatusCompleted, "all green"))

	got, err := mgr.Read(ctx, task.ID)
	require.NoError(t, err)
	assert.Equal(t, StatusCompleted, got.Status)
	assert.Equal(t, ChecklistDone, got.Checklist.Items[0].Status)
}

// TestVerifyConditionsRoundTripThroughTheStore. A condition that does not
// survive a write/read cycle is a gate that silently stops gating after a
// restart — the failure mode would be a task completing that could not have
// completed before the process bounced.
func TestVerifyConditionsRoundTripThroughTheStore(t *testing.T) {
	mgr, _ := storeManager(t)
	ctx := context.Background()

	cond := ChecklistCondition{Kind: ConditionFileExists, Target: "sub/out.go"}
	task, err := mgr.Create(ctx, CreateReq{Title: "t", Prompt: "p"})
	require.NoError(t, err)
	_, err = mgr.SetChecklist(ctx, task.ID, Checklist{Items: []ChecklistItem{
		item(1, "write it", ChecklistPending, cond),
		item(2, "plain", ChecklistPending, ChecklistCondition{}),
	}})
	require.NoError(t, err)

	got, err := mgr.Read(ctx, task.ID)
	require.NoError(t, err)
	require.Len(t, got.Checklist.Items, 2)
	assert.Equal(t, cond, got.Checklist.Items[0].Verify)
	assert.Equal(t, ChecklistCondition{}, got.Checklist.Items[1].Verify,
		"an item with no condition must read back with the zero value, not a phantom one")
}

// TestCreateWithChecklistPersistsConditions covers the OTHER insert path,
// which has its own SQL statement and would drift independently.
func TestCreateWithChecklistPersistsConditions(t *testing.T) {
	mgr, st := storeManager(t)
	ctx := context.Background()
	cond := ChecklistCondition{Kind: ConditionGatePassed, Target: "lint"}

	task := &WorkTask{
		ID: NewID("wt"), Title: "t", Prompt: "p", Status: StatusPending,
		Checklist: Checklist{Items: []ChecklistItem{item(1, "lint clean", ChecklistPending, cond)}},
	}
	require.NoError(t, st.Create(ctx, task))

	got, err := mgr.Read(ctx, task.ID)
	require.NoError(t, err)
	require.Len(t, got.Checklist.Items, 1)
	assert.Equal(t, cond, got.Checklist.Items[0].Verify)
}

// TestMigrationAddsVerifyColumnsToAPreL7Database is the upgrade path.
//
// workSchema uses CREATE TABLE IF NOT EXISTS, so on a database written before
// L7 the new column declarations are a no-op and never appear. Without the
// ALTER, the very next SELECT names columns SQLite has never heard of and the
// whole work store fails to open — invisibly in tests (they all start from
// :memory:) and on the first real upgrade for everyone else.
func TestMigrationAddsVerifyColumnsToAPreL7Database(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	require.NoError(t, err)
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })

	// The pre-L7 table, exactly as it shipped.
	_, err = db.Exec(`CREATE TABLE task_work_checklist (
		task_id TEXT NOT NULL, item_id INTEGER NOT NULL,
		content TEXT NOT NULL, status TEXT NOT NULL,
		PRIMARY KEY(task_id, item_id))`)
	require.NoError(t, err)

	st, err := FromDB(db, nil)
	require.NoError(t, err, "the migration must upgrade an existing table, not fail on it")

	// The columns are there and usable end to end.
	mgr := NewManager(st, nil, ArtifactPolicy{})
	ctx := context.Background()
	task, err := mgr.Create(ctx, CreateReq{Title: "t", Prompt: "p"})
	require.NoError(t, err)
	cond := ChecklistCondition{Kind: ConditionFileExists, Target: "x.txt"}
	_, err = mgr.SetChecklist(ctx, task.ID, Checklist{Items: []ChecklistItem{item(1, "a", ChecklistPending, cond)}})
	require.NoError(t, err)
	got, err := mgr.Read(ctx, task.ID)
	require.NoError(t, err)
	assert.Equal(t, cond, got.Checklist.Items[0].Verify)

	// And running it a second time on the now-upgraded table is a no-op rather
	// than a duplicate-column error: ALTER TABLE ADD COLUMN is not idempotent.
	_, err = FromDB(db, nil)
	assert.NoError(t, err, "migrate must be re-runnable")
}
