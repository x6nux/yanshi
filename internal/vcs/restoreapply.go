// internal/vcs/restoreapply.go
//
// The execute half of V1/V4: turn a RestorePlan into working-copy bytes.
//
// This file writes; preview.go decides. The split is the point (see
// preview.go's header): applyRestorePlanLocked reads Path, Op and the resolved
// blob content out of the plan and never asks the trees, the ignore rules or
// the filesystem what SHOULD happen. If the preview said "overwrite three
// files", exactly those three files are written.
//
// Two properties every apply here holds:
//
//   - COMPENSATED. The pre-operation bytes, existence and mode of every touched
//     path are snapshotted before the first mutation, and any failure restores
//     them. This reuses snapshotWorkingFiles / restoreWorkingFiles, the same
//     pair MaterializeMain and RevertToSeam use, rather than a second
//     rollback implementation.
//   - FINGERPRINTED (V5). The touched paths are hashed immediately before the
//     first write and again after the last one. A path that changed underneath
//     the operation is reported as ErrExternalMutation instead of being quietly
//     overwritten by, or quietly overwriting, whatever a subprocess was doing.
//     See freeze.go for why detection is all that is portably possible.

package vcs

import (
	"errors"
	"fmt"
	"os"
)

// ApplyRestore executes a previously previewed plan (V4's selective restore,
// and the shared engine under V1's rollback).
//
// confirmToken must be the token from the plan the caller showed the operator.
// The plan is recomputed here, under the repo lane and under a working-copy
// freeze, and a token mismatch fails with ErrPlanStale — so a confirmation
// always applies to the state that was actually previewed.
//
// The applied plan is returned so the caller can report exactly what happened
// without re-deriving it.
func (v *VCS) ApplyRestore(
	repoID, targetCommit string, selectors []string, confirmToken string,
) (*RestorePlan, error) {
	thaw, err := v.freezeWorkingCopy(repoID)
	if err != nil {
		return nil, err
	}
	defer thaw()

	unlock := v.lockRepo(repoID)
	defer unlock()

	plan, err := v.planRestoreLocked(repoID, targetCommit, selectors)
	if err != nil {
		return nil, err
	}
	if err := plan.verifyConfirmToken(confirmToken); err != nil {
		return nil, err
	}
	if err := v.applyRestorePlanLocked(plan, true); err != nil {
		return nil, err
	}
	return plan, nil
}

// verifyConfirmToken checks the caller's token against this freshly recomputed
// plan and classifies any disagreement.
//
// The intent half is checked first on purpose. If the head moved, the whole
// operation is different and the working-copy comparison would be about a
// different set of paths — reporting an external mutation there would name the
// wrong cause.
func (p *RestorePlan) verifyConfirmToken(token string) error {
	intent, observed, ok := splitConfirmToken(token)
	if !ok {
		return fmt.Errorf("%w: %q is not a token produced by a preview", ErrPlanStale, token)
	}
	if want := planToken(p); intent != want {
		return fmt.Errorf(
			"%w: the preview described a different operation (previewed %s, now %s)",
			ErrPlanStale, intent, want)
	}
	if want := workingToken(p); observed != want {
		// Same operation, different files underneath it. Recompute the per-path
		// difference so the error names the paths rather than two hashes: this
		// is the message an operator has to act on.
		return externalMutationError("between preview and apply", p.driftedPaths())
	}
	return nil
}

// driftedPaths reports which paths account for a working-token mismatch.
//
// The caller's token is a hash, so the individual pre-hashes it covered cannot
// be recovered from it. What CAN be recovered is which paths are candidates:
// every path whose current on-disk content differs from what the FROM tree
// records is one an external writer plausibly touched. That is exactly
// RestoreChange.Dirty, computed by the fresh plan a moment ago.
//
// If nothing reads as dirty the mismatch came from a path the previewed plan
// listed and this one does not (or vice versa), so every planned path is
// reported rather than an empty list — an ErrExternalMutation naming no path
// would be unactionable.
func (p *RestorePlan) driftedPaths() []string {
	drifted := p.DirtyPaths()
	if len(drifted) > 0 {
		return drifted
	}
	all := make([]string, 0, len(p.Changes))
	for _, c := range p.Changes {
		all = append(all, c.Path)
	}
	return all
}

// applyRestorePlanLocked writes the plan's changes. Callers must hold the repo
// lane; ApplyRestore additionally holds the freeze.
//
// track controls whether the written paths also enter the scope's pending
// changeset. A selective restore (V4) SHOULD be tracked: it moves the working
// copy away from head deliberately, and leaving that untracked would make the
// next commit look like the agent had made those edits by hand — or, worse,
// let a later commit silently drop them. A whole-tree revert (V1) must NOT be
// tracked: RevertToSeam moves main_head to the target commit, so the working
// copy already matches the new head and a pending changeset would immediately
// re-commit the tree as a no-op delta.
func (v *VCS) applyRestorePlanLocked(plan *RestorePlan, track bool) error {
	return v.applyRestorePlanLockedWithHook(plan, track, nil)
}

// restoreHook is called after each path is written. It is nil in production;
// same-package tests use it to simulate the ONE thing this package cannot
// otherwise schedule deterministically — a non-VCS writer touching the working
// copy midway through the expansion.
//
// A hook rather than a real racing goroutine because the window is
// microseconds wide: a test that spawned a competing writer would pass or fail
// on scheduler luck, which for a check that only ever fires under a race is the
// worst possible foundation. The same reasoning (and the same shape) produced
// materializeHook in revert.go.
type restoreHook func(path string) error

// applyRestorePlanLockedWithHook is the deterministic core of the apply.
func (v *VCS) applyRestorePlanLockedWithHook(
	plan *RestorePlan, track bool, hook restoreHook,
) error {
	if plan.IsEmpty() {
		return nil
	}
	paths := make([]string, 0, len(plan.Changes))
	for _, c := range plan.Changes {
		paths = append(paths, c.Path)
	}

	workRoot, err := openWorkRoot(plan.RootPath)
	if err != nil {
		return err
	}
	defer workRoot.Close()

	// V5, pre-write half. This covers a NARROW window and it is worth being
	// precise about which one: the gap between planRestoreLocked's read of the
	// working copy and the first byte written here, both inside the repo lane.
	// The much wider preview→confirm window is NOT this check's job — an apply
	// re-plans, so a fresh plan would simply agree with whatever a subprocess
	// wrote. That window is covered by the observed half of ConfirmToken (see
	// verifyConfirmToken), which carries the ORIGINAL preview's hashes back in
	// from the caller and is the only thing able to contradict a fresh read.
	//
	// Kept rather than dropped as redundant because RevertToSeam applies a plan
	// it built itself, with no token in the loop at all; for that path this is
	// the only pre-write check there is.
	before, err := fingerprintPaths(workRoot, paths)
	if err != nil {
		return err
	}
	if drifted := diffFingerprints(plan.observedFingerprint(), before); len(drifted) > 0 {
		return externalMutationError("between planning and writing", drifted)
	}

	snapshot, err := snapshotWorkingFiles(plan.RootPath, paths)
	if err != nil {
		return fmt.Errorf("vcs: restore: snapshot: %w", err)
	}
	compensate := func(cause error) error {
		if rollbackErr := restoreWorkingFiles(plan.RootPath, paths, snapshot); rollbackErr != nil {
			return errors.Join(cause, fmt.Errorf("vcs: restore: compensation failed: %w", rollbackErr))
		}
		return cause
	}

	for _, c := range plan.Changes {
		if c.Op == RestoreDelete {
			if err := rootRemove(workRoot, c.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
				return compensate(fmt.Errorf("vcs: restore: remove %s: %w", c.Path, err))
			}
			continue
		}
		mode := os.FileMode(0o644)
		if prior := snapshot[c.Path]; prior.exists {
			mode = prior.mode
		}
		if err := rootReplaceFile(workRoot, c.Path, plan.targetBytes[c.Path], mode); err != nil {
			return compensate(fmt.Errorf("vcs: restore: write %s: %w", c.Path, err))
		}
		if hook != nil {
			if err := hook(c.Path); err != nil {
				return compensate(err)
			}
		}
	}

	// V5, post-write half: the disk must now hold exactly what the plan said it
	// would. A difference here means someone wrote DURING the expansion — the
	// window the freeze flag cannot close for non-VCS writers.
	after, err := fingerprintPaths(workRoot, paths)
	if err != nil {
		return compensate(err)
	}
	if drifted := diffFingerprints(plan.expectedFingerprint(), after); len(drifted) > 0 {
		return compensate(externalMutationError("during apply", drifted))
	}

	if !track {
		return nil
	}
	if err := v.trackRestoredPaths(plan); err != nil {
		return compensate(err)
	}
	return nil
}

// trackRestoredPaths records the applied changes in main's pending changeset so
// the next commit reflects them. Writes become blob upserts; deletions become
// op="deleted" rows, which is what makes a selective restore able to REMOVE a
// file that the agent added after the target commit.
func (v *VCS) trackRestoredPaths(plan *RestorePlan) error {
	for _, c := range plan.Changes {
		if c.Op == RestoreDelete {
			if _, err := v.store.DB.Exec(
				"INSERT INTO vcs_uncommitted (scope_type, scope_id, path, blob_hash, op) VALUES ('main', ?, ?, '', 'deleted')\n"+
					"ON CONFLICT(scope_type, scope_id, path) DO UPDATE SET blob_hash='', op='deleted'",
				plan.RepoID, c.Path,
			); err != nil {
				return fmt.Errorf("vcs: restore: track delete %s: %w", c.Path, err)
			}
			continue
		}
		op := v.deriveOp("main", plan.RepoID, c.Path, c.TargetHash)
		if _, err := v.store.DB.Exec(
			"INSERT INTO vcs_uncommitted (scope_type, scope_id, path, blob_hash, op) VALUES ('main', ?, ?, ?, ?)\n"+
				"ON CONFLICT(scope_type, scope_id, path) DO UPDATE SET blob_hash=excluded.blob_hash, op=excluded.op",
			plan.RepoID, c.Path, c.TargetHash, op,
		); err != nil {
			return fmt.Errorf("vcs: restore: track write %s: %w", c.Path, err)
		}
	}
	return nil
}

// observedFingerprint is what the planner saw on disk for every touched path.
//
// It is captured at plan time (RestoreChange.preHash) rather than re-derived
// from the trees, because "what the tree says should be there" and "what is
// there" are exactly the two things a restore must not confuse — deriving it
// would make the check compare the plan against itself and always pass.
func (p *RestorePlan) observedFingerprint() fingerprint {
	fp := make(fingerprint, len(p.Changes))
	for _, c := range p.Changes {
		fp[c.Path] = c.preHash
	}
	return fp
}

// expectedFingerprint is what the disk must hold once the plan is applied.
func (p *RestorePlan) expectedFingerprint() fingerprint {
	fp := make(fingerprint, len(p.Changes))
	for _, c := range p.Changes {
		if c.Op == RestoreDelete {
			fp[c.Path] = ""
			continue
		}
		fp[c.Path] = c.TargetHash
	}
	return fp
}
