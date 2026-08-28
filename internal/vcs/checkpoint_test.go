package vcs

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/store"
)

// TestCheckpoint_RestoresSelectedDimensionOnly (files half): restoring the file
// dimension puts the working copy back and leaves the store's two dimensions
// alone. The store-side half of this clause lives in
// internal/store/checkpoint_restore_test.go; both halves are needed because
// "only this dimension moved" cannot be observed from inside one package.
func TestCheckpointFiles_RestoresOnlyTheWorkingCopy(t *testing.T) {
	v, repoID, root := setupSeamTestRepo(t)
	target := commitWith(t, v, repoID, root, "target", map[string]string{
		"kept.txt": "original\n",
	})
	require.NotEmpty(t, target)

	sid, err := v.store.CreateSession("with files")
	require.NoError(t, err)
	require.NoError(t, v.store.AppendMessage(sid, 0, "user", "hello"))
	_, err = v.store.WriteMemoryScoped("note", "a memory", store.MemoryFilter{})
	require.NoError(t, err)

	cp, err := v.CreateCheckpoint("before the edit", sid, repoID)
	require.NoError(t, err)
	require.Equal(t, target, cp.FileCommit, "the checkpoint records the CURRENT head")

	// Move the working copy away, and the memory table with it.
	commitWith(t, v, repoID, root, "advance", map[string]string{"kept.txt": "changed\n"})
	_, err = v.store.WriteMemoryScoped("note", "written after", store.MemoryFilter{})
	require.NoError(t, err)

	plan, err := v.PlanCheckpointFiles(repoID, cp.ID)
	require.NoError(t, err)
	require.False(t, plan.IsEmpty(), "the preview must describe real work")

	undo, applied, err := v.RestoreCheckpointFiles(repoID, cp.ID, plan.ConfirmToken)
	require.NoError(t, err)
	require.NotEmpty(t, undo.ID)
	require.NotEmpty(t, applied.Changes)

	got, err := os.ReadFile(filepath.Join(root, "kept.txt"))
	require.NoError(t, err)
	assert.Equal(t, "original\n", string(got), "the working copy came back")

	mems, err := v.store.RecallMemory(50)
	require.NoError(t, err)
	assert.Len(t, mems, 2, "the memory dimension must NOT have moved")
}

// TestCheckpointFiles_SnapshotsBeforeRestore: the automatic snapshot names the
// head the working copy stood at, so the file restore is itself undoable.
func TestCheckpointFiles_SnapshotsBeforeRestore(t *testing.T) {
	v, repoID, root := setupSeamTestRepo(t)
	target := commitWith(t, v, repoID, root, "target", map[string]string{"f.txt": "one\n"})
	cp, err := v.CreateCheckpoint("target", "", repoID)
	require.NoError(t, err)
	require.Equal(t, target, cp.FileCommit)

	advanced := commitWith(t, v, repoID, root, "advance", map[string]string{"f.txt": "two\n"})

	plan, err := v.PlanCheckpointFiles(repoID, cp.ID)
	require.NoError(t, err)
	undo, _, err := v.RestoreCheckpointFiles(repoID, cp.ID, plan.ConfirmToken)
	require.NoError(t, err)
	assert.Equal(t, advanced, undo.FileCommit,
		"the undo point must name where the working copy was, not where it went")
	assert.Contains(t, undo.Label, cp.ID)

	got, err := os.ReadFile(filepath.Join(root, "f.txt"))
	require.NoError(t, err)
	require.Equal(t, "one\n", string(got))

	// And the undo point really undoes it.
	undoPlan, err := v.PlanCheckpointFiles(repoID, undo.ID)
	require.NoError(t, err)
	_, _, err = v.RestoreCheckpointFiles(repoID, undo.ID, undoPlan.ConfirmToken)
	require.NoError(t, err)
	got, err = os.ReadFile(filepath.Join(root, "f.txt"))
	require.NoError(t, err)
	assert.Equal(t, "two\n", string(got))
}

// TestCheckpointFiles_DryRunProducesPlanWithoutMutating: the preview describes
// the change and does not make it. This is PlanRestore's existing property; the
// test is here because W-D-06 now depends on it through a different entry point,
// and a checkpoint wrapper that quietly applied would look identical from the
// store side.
func TestCheckpointFiles_DryRunProducesPlanWithoutMutating(t *testing.T) {
	v, repoID, root := setupSeamTestRepo(t)
	commitWith(t, v, repoID, root, "target", map[string]string{"f.txt": "one\n"})
	cp, err := v.CreateCheckpoint("target", "", repoID)
	require.NoError(t, err)
	commitWith(t, v, repoID, root, "advance", map[string]string{"f.txt": "two\n"})

	plan, err := v.PlanCheckpointFiles(repoID, cp.ID)
	require.NoError(t, err)
	create, overwrite, del := plan.Counts()
	assert.Equal(t, 1, overwrite)
	assert.Zero(t, create)
	assert.Zero(t, del)

	got, err := os.ReadFile(filepath.Join(root, "f.txt"))
	require.NoError(t, err)
	assert.Equal(t, "two\n", string(got), "a dry run must not touch the working copy")

	head, err := v.RepoMainHead(repoID)
	require.NoError(t, err)
	assert.NotEqual(t, cp.FileCommit, head, "nor move the head")
}

// TestCheckpointFiles_StalePlanIsRejected: the confirm token is what makes the
// preview binding. Without this the plan/apply split would be decoration —
// apply would happily act on a world that changed after the operator looked.
func TestCheckpointFiles_StalePlanIsRejected(t *testing.T) {
	v, repoID, root := setupSeamTestRepo(t)
	commitWith(t, v, repoID, root, "target", map[string]string{"f.txt": "one\n"})
	cp, err := v.CreateCheckpoint("target", "", repoID)
	require.NoError(t, err)
	commitWith(t, v, repoID, root, "advance", map[string]string{"f.txt": "two\n"})

	plan, err := v.PlanCheckpointFiles(repoID, cp.ID)
	require.NoError(t, err)

	// Somebody writes the very file the restore is about to overwrite.
	require.NoError(t, os.WriteFile(filepath.Join(root, "f.txt"), []byte("three\n"), 0o644))

	_, _, err = v.RestoreCheckpointFiles(repoID, cp.ID, plan.ConfirmToken)
	require.Error(t, err)
	assert.True(t,
		errors.Is(err, ErrExternalMutation) || errors.Is(err, ErrPlanStale),
		"a drifted working copy must be named, got %v", err)
}

// TestCheckpointFiles_NoRepoHasNoFileDimension: a deployment with no repository
// still gets checkpoints, and the file dimension refuses rather than pretending.
func TestCheckpointFiles_NoRepoHasNoFileDimension(t *testing.T) {
	v, repoID, _ := setupSeamTestRepo(t)

	cp, err := v.CreateCheckpoint("no repo", "", "")
	require.NoError(t, err)
	assert.Empty(t, cp.FileCommit)

	_, err = v.PlanCheckpointFiles(repoID, cp.ID)
	assert.ErrorIs(t, err, store.ErrNoCheckpointDimension)
	_, _, err = v.RestoreCheckpointFiles(repoID, cp.ID, "whatever")
	assert.ErrorIs(t, err, store.ErrNoCheckpointDimension)

	_, err = v.PlanCheckpointFiles(repoID, "nope")
	assert.Error(t, err, "an unknown checkpoint id is an error")
}
