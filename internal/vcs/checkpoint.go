// internal/vcs/checkpoint.go
//
// W-D-06's file dimension: the three functions that let a store.Checkpoint name
// a moment in the working copy.
//
// EVERY LINE HERE IS GLUE, DELIBERATELY. The store owns the checkpoint row and
// the session/memory dimensions; this package already owns file history,
// preview, freeze, external-mutation detection and apply. What was missing was
// only the join between them, and the join has to live on this side because
// internal/vcs imports internal/store and not the other way round.
//
// The freeze, the confirm token and the plan/apply split are NOT reimplemented:
// PlanCheckpointFiles is PlanRestore and RestoreCheckpointFiles is
// ApplyRestore. That is what buys the file dimension the same two properties
// the store dimensions get structurally — a dry run that describes exactly what
// apply will do, and writers held off while it happens — without a second
// mechanism that could drift from the first.
package vcs

import (
	"fmt"

	"github.com/x6nux/yanshi/internal/store"
)

// CreateCheckpoint records a checkpoint whose file dimension is repoID's
// CURRENT main head.
//
// Reading the head here rather than making the caller pass it is the point of
// the function: a caller that fetched the head, did something else and then
// created the checkpoint would record a moment that never existed.
//
// repoID may be empty, for a deployment with no repository configured. The
// checkpoint is still created — the session and memory dimensions do not need a
// repo — and its file dimension reports store.ErrNoCheckpointDimension when
// anyone tries to restore it.
func (v *VCS) CreateCheckpoint(label, sessionID, repoID string) (store.Checkpoint, error) {
	head := ""
	if repoID != "" {
		h, err := v.RepoMainHead(repoID)
		if err != nil {
			return store.Checkpoint{}, fmt.Errorf("vcs: checkpoint head: %w", err)
		}
		head = h
	}
	return v.store.CreateCheckpoint(label, sessionID, head)
}

// PlanCheckpointFiles previews restoring the working copy to a checkpoint's
// commit, WITHOUT touching it. The returned plan's ConfirmToken is what
// RestoreCheckpointFiles must be given.
func (v *VCS) PlanCheckpointFiles(repoID, checkpointID string) (*RestorePlan, error) {
	commit, err := v.checkpointCommit(checkpointID)
	if err != nil {
		return nil, err
	}
	return v.PlanRestore(repoID, commit, nil)
}

// RestoreCheckpointFiles rolls the working copy back to a checkpoint's commit,
// after taking an automatic checkpoint of where it stood.
//
// THE SNAPSHOT IS TAKEN BEFORE THE FREEZE, NOT INSIDE IT, and the order is
// forced rather than chosen: the store's own write lane and this package's repo
// lane are different locks, and a snapshot taken while the repo lane is held
// would be one nested lock acquisition away from a deadlock with any path that
// takes them the other way round. Taking it first costs a window in which the
// undo point exists and the restore has not run — which is the harmless
// direction, since an unused undo point is a row and a missing one is a lost
// working copy.
//
// confirmToken must come from PlanCheckpointFiles. ApplyRestore re-plans under
// the lane and rejects a token that no longer describes the same operation, so
// a confirmation always applies to the state that was previewed.
func (v *VCS) RestoreCheckpointFiles(
	repoID, checkpointID, confirmToken string,
) (store.Checkpoint, *RestorePlan, error) {
	commit, err := v.checkpointCommit(checkpointID)
	if err != nil {
		return store.Checkpoint{}, nil, err
	}
	undo, err := v.CreateCheckpoint("before restoring "+checkpointID+" (files)", "", repoID)
	if err != nil {
		return store.Checkpoint{}, nil, err
	}
	plan, err := v.ApplyRestore(repoID, commit, nil, confirmToken)
	if err != nil {
		return store.Checkpoint{}, nil, err
	}
	return undo, plan, nil
}

// checkpointCommit resolves a checkpoint id to the commit its file dimension
// names, or reports that it has none.
func (v *VCS) checkpointCommit(checkpointID string) (string, error) {
	cp, err := v.store.CheckpointByID(checkpointID)
	if err != nil {
		return "", err
	}
	if cp.FileCommit == "" {
		return "", fmt.Errorf("%w: no file commit (checkpoint %s)",
			store.ErrNoCheckpointDimension, cp.ID)
	}
	return cp.FileCommit, nil
}
