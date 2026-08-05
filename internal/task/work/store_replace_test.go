package work

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSetChecklistReplacesRatherThanAppends pins the "Set" in SetChecklist.
//
// It clears the existing rows before inserting. Without that DELETE the call
// appends, so setting a three-item checklist twice leaves six items — the task
// silently grows a duplicate of everything each time its plan is revised, and
// no error is raised because every insert succeeded.
//
// Measured W3 review round 12: removing the DELETE reddened nothing.
func TestSetChecklistReplacesRatherThanAppends(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	task := &WorkTask{ID: "t1", Title: "t", Prompt: "p", Status: StatusPending}
	require.NoError(t, s.Create(ctx, task))

	first := Checklist{Items: []ChecklistItem{
		{ID: 1, Content: "a", Status: ChecklistPending},
		{ID: 2, Content: "b", Status: ChecklistPending},
	}}
	require.NoError(t, s.SetChecklist(ctx, "t1", first))

	// Revise the plan: two different items, same task.
	second := Checklist{Items: []ChecklistItem{
		{ID: 1, Content: "c", Status: ChecklistPending},
		{ID: 2, Content: "d", Status: ChecklistPending},
	}}
	require.NoError(t, s.SetChecklist(ctx, "t1", second))

	got, err := s.Get(ctx, "t1")
	require.NoError(t, err)
	require.Len(t, got.Checklist.Items, 2,
		"a second Set must replace the checklist, not add to it")
	assert.Equal(t, "c", got.Checklist.Items[0].Content)
	assert.Equal(t, "d", got.Checklist.Items[1].Content)
}

// TestSetChecklistRejectsDuplicateItemIDs pins that a malformed checklist is
// refused rather than silently trimmed.
//
// The inserts are plain INSERTs, so a repeated item_id violates the primary key
// and the whole transaction rolls back. Switch them to INSERT OR IGNORE — the
// obvious "make it robust" edit — and the duplicate is dropped instead: the
// call succeeds, the task ends up with fewer items than the caller listed, and
// nothing anywhere says so. A checklist quietly missing a step is worse than a
// rejected write, because the agent will report the remaining steps as the
// whole plan.
//
// Measured W3 review round 13: the OR IGNORE variant reddened nothing.
func TestSetChecklistRejectsDuplicateItemIDs(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	require.NoError(t, s.Create(ctx, &WorkTask{
		ID: "t3", Title: "t", Prompt: "p", Status: StatusPending,
	}))

	err := s.SetChecklist(ctx, "t3", Checklist{Items: []ChecklistItem{
		{ID: 1, Content: "a", Status: ChecklistPending},
		{ID: 1, Content: "b", Status: ChecklistPending}, // same id
	}})
	require.Error(t, err, "a duplicate item id must fail the write, not be dropped")

	// The failed Set must leave nothing behind.
	got, gerr := s.Get(ctx, "t3")
	require.NoError(t, gerr)
	assert.Empty(t, got.Checklist.Items,
		"a rolled-back Set must not leave a partial checklist")
}
