// internal/vcs/freeze.go
//
// V5: a working-copy freeze for the duration of a rollback — and an honest
// account of what it can and cannot stop.
//
// # What it stops
//
// While a repo is frozen, every VCS write entry point for that repo returns
// ErrWorkingCopyFrozen INSTEAD of queueing on the repo lane. The distinction
// matters and is the whole reason the flag exists: the lane alone already
// serialises writers, but a writer that BLOCKS on it resumes afterwards holding
// content it read before the rollback and commits that stale content into
// history as if it were current. Failing fast turns a silent stale write into
// a visible, retryable error.
//
// # What it CANNOT stop, and this is not a limitation that can be engineered away
//
// A freeze flag lives in this process's memory and is consulted by code paths
// that call into this package. The writers that most often damage a working
// copy during a rollback do neither:
//
//   - a compiler, formatter or code generator started by shell_run, writing its
//     output while the tree is half-expanded;
//   - a background worker or watch process from an earlier turn;
//   - an editor autosave, another yanshi process, anything at all with a file
//     descriptor.
//
// None of them ask this package for permission. There is no portable way to
// deny them: POSIX advisory locks are advisory, mandatory locking is a Linux
// mount option nobody enables, and freezing a whole filesystem needs root. So
// this package does not pretend. It DETECTS instead of promising: every apply
// fingerprints the paths it is about to touch before writing and again after,
// and a mismatch is reported to the caller rather than silently overwritten.
//
// Detection is strictly weaker than prevention and is deliberately advertised
// as such. A subprocess that writes a file and this rollback that rewrites it
// are still a race; what changes is that the operator learns it happened.
//
// # The freeze has its own race, too
//
// checkNotFrozen runs BEFORE the lane is acquired, which is what makes it fail
// fast. A writer that passed the check one instruction before the freeze went
// up will then block on the lane and proceed once the rollback releases it.
// That window is real and is covered by the same fingerprint comparison — which
// is another way of saying the fingerprints, not the flag, are the load-bearing
// half of V5.

package vcs

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sort"
	"strings"
)

// ErrWorkingCopyFrozen reports that a rollback or selective restore is
// currently rewriting this repository's working copy, so the attempted VCS
// write was refused rather than queued. Retry once the operation finishes.
var ErrWorkingCopyFrozen = errors.New("vcs: working copy is frozen by a restore in progress")

// ErrExternalMutation reports that the working copy changed underneath a
// restore — between the preview and the apply, or between the apply's first
// write and its verification. The offending paths are named in the message.
//
// It never means the restore's own writes failed; those surface as their own
// errors. It means something OUTSIDE this package touched the same files, and
// the caller must look at them before trusting either version.
var ErrExternalMutation = errors.New("vcs: working copy was modified outside the VCS during a restore")

// freezeWorkingCopy marks repoID frozen and returns the thaw function. It is
// safe to call while holding the repo lane (it takes only the freeze index
// mutex, never a lane).
//
// Freezes do not nest: a second freeze on an already-frozen repo is an error
// rather than a counter bump, because two concurrent rollbacks of one working
// copy is not a state this package can make coherent, and a refcount would
// quietly permit it.
func (v *VCS) freezeWorkingCopy(repoID string) (func(), error) {
	v.freezeMu.Lock()
	defer v.freezeMu.Unlock()
	if v.frozen == nil {
		v.frozen = map[string]bool{}
	}
	if v.frozen[repoID] {
		return nil, fmt.Errorf("%w: repo %s", ErrWorkingCopyFrozen, repoID)
	}
	v.frozen[repoID] = true
	return func() {
		v.freezeMu.Lock()
		delete(v.frozen, repoID)
		v.freezeMu.Unlock()
	}, nil
}

// checkNotFrozen is the guard every VCS write entry point calls BEFORE taking
// the repo lane. See the file header for why "before" is the point.
func (v *VCS) checkNotFrozen(repoID string) error {
	v.freezeMu.Lock()
	defer v.freezeMu.Unlock()
	if v.frozen[repoID] {
		return fmt.Errorf("%w: repo %s", ErrWorkingCopyFrozen, repoID)
	}
	return nil
}

// WorkingCopyFrozen reports whether repoID is currently mid-restore. Exported
// for callers (the tool layer, a status endpoint) that want to explain a
// refusal rather than surface a bare error.
func (v *VCS) WorkingCopyFrozen(repoID string) bool {
	v.freezeMu.Lock()
	defer v.freezeMu.Unlock()
	return v.frozen[repoID]
}

// lockRepoUnlessFrozen is the fail-fast variant of lockRepo used by the write
// entry points. It returns ErrWorkingCopyFrozen instead of the unlock function
// when a restore holds the repo.
func (v *VCS) lockRepoUnlessFrozen(repoID string) (func(), error) {
	if err := v.checkNotFrozen(repoID); err != nil {
		return nil, err
	}
	return v.lockRepo(repoID), nil
}

// fingerprint is the observed state of one working-copy path: the content hash,
// or the empty string when the path is absent.
//
// Content hashing rather than mtime+size is deliberate. mtime has one-second
// granularity on some filesystems and is trivially preserved by a writer that
// restores it, and size collides on any same-length edit — a config value
// flipped from "true" to "fals"+"e" would be invisible. The paths compared here
// are exactly the paths the restore is already reading and writing in full, so
// hashing them costs one extra pass over bytes that are in page cache anyway.
type fingerprint map[string]string

// fingerprintPaths hashes every listed path through the confined root handle.
// A missing path records the empty string, so "absent" and "present but empty"
// stay distinguishable (hashContent of no bytes is not "").
func fingerprintPaths(root *os.Root, paths []string) (fingerprint, error) {
	fp := make(fingerprint, len(paths))
	for _, p := range paths {
		data, err := rootReadFile(root, p)
		if errors.Is(err, fs.ErrNotExist) {
			fp[p] = ""
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("vcs: fingerprint %s: %w", p, err)
		}
		fp[p] = hashContent(data)
	}
	return fp, nil
}

// diffFingerprints returns the sorted paths where want and got disagree.
func diffFingerprints(want, got fingerprint) []string {
	var out []string
	for p, w := range want {
		if got[p] != w {
			out = append(out, p)
		}
	}
	for p, g := range got {
		if _, ok := want[p]; !ok && g != "" {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

// externalMutationError formats an ErrExternalMutation naming the paths and the
// phase it was detected in.
func externalMutationError(phase string, paths []string) error {
	return fmt.Errorf("%w (%s): %s", ErrExternalMutation, phase, strings.Join(paths, ", "))
}

// freezeState is the exported view of a repo freeze, used by callers that
// report status. It is a value type on purpose: handing out the live map would
// let a caller mutate the guard.
type freezeState struct {
	RepoID string
	Frozen bool
}

// freezeSnapshot lists the currently frozen repos. Used by tests and by the
// worktree scan, which must not report a worktree as orphaned merely because a
// restore is in flight.
func (v *VCS) freezeSnapshot() []freezeState {
	v.freezeMu.Lock()
	defer v.freezeMu.Unlock()
	out := make([]freezeState, 0, len(v.frozen))
	for id := range v.frozen {
		out = append(out, freezeState{RepoID: id, Frozen: true})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RepoID < out[j].RepoID })
	return out
}
