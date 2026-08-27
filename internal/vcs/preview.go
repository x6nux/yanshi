// internal/vcs/preview.go
//
// V1 + V4: the ONE computation behind both the rollback preview and the
// rollback itself.
//
// # Why a plan object at all
//
// Before this file, RevertToSeam had only an execute mode. The operator pressed
// the button and found out afterwards which files had been rewritten, which had
// been deleted, and whether the uncommitted edit they had been holding in an
// editor buffer was still there. A rollback is the one operation in this package
// whose whole purpose is to destroy current state, so "find out afterwards" is
// the wrong order.
//
// The obvious fix — write a preview function that walks the trees and prints
// what it finds — is the wrong fix, and QwenPaw's checkpoints module is where
// the right shape comes from: its restore path builds a RestorePlan first and
// then either RENDERS it (--dry-run) or APPLIES it (--confirm), so a preview
// cannot describe a different operation from the one that runs. A second
// walker would start out agreeing with the first and then drift on the first
// edge case that is fixed in only one of them — ignore rules, path validation,
// symlink refusal, mode preservation. Every one of those decisions lives in
// planRestoreLocked here, and applyRestorePlanLocked consumes its output
// verbatim: it never re-derives a path, a blob, or an op.
//
// # Why apply re-plans instead of taking the previewed plan
//
// PlanRestore returns a plan with a ConfirmToken. ApplyRestore does NOT accept
// the previewed plan as input; it builds a FRESH one under the repo lane and
// then checks the token. Accepting the caller's plan would mean acting on a
// snapshot of the world taken before the lock was held — the head may have
// moved, files may have been written, blobs may have been GC'd. Re-planning
// under the lane and comparing tokens turns all of that into an explicit
// "the plan changed, look again" error instead of a silent stale write.
//
// # The dirty flag is the load-bearing part of the preview
//
// RestoreChange.Dirty means: the bytes ON DISK at this path do not match the
// bytes the CURRENT head says should be there. A revert overwrites the working
// copy from stored history, so every dirty path is uncommitted work that the
// operation destroys and no seam can bring back. That is the single fact an
// operator most needs before confirming, and it is not derivable from the two
// commit trees alone — it requires reading the working copy, which is why the
// planner opens a work root rather than staying purely in the database.

package vcs

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/x6nux/yanshi/internal/guard"
)

// RestoreOp classifies what one planned change does to the working copy.
type RestoreOp string

// The three effects a restore can have on a single path.
const (
	// RestoreCreate writes a path that does not currently exist on disk.
	RestoreCreate RestoreOp = "create"
	// RestoreOverwrite replaces the bytes of an existing path.
	RestoreOverwrite RestoreOp = "overwrite"
	// RestoreDelete removes a path that the target tree does not contain.
	RestoreDelete RestoreOp = "delete"
)

// ErrPlanStale reports that the repository moved between PlanRestore and
// ApplyRestore, so the confirmed plan no longer describes what would happen.
// The caller must preview again rather than proceed.
var ErrPlanStale = errors.New("vcs: restore plan is stale; re-run the preview")

// RestoreChange is one path's entry in a RestorePlan.
//
// LinesBefore counts the lines currently ON DISK (not the lines the head tree
// records), because those are the lines the operator would actually lose.
// LinesAdded/LinesRemoved come from a line-level diff of the same two byte
// slices; when either side is too large for the exact diff they degrade to the
// whole-file counts and Approx is set, so a consumer never has to guess whether
// a number is exact.
type RestoreChange struct {
	Path         string
	Op           RestoreOp
	LinesBefore  int
	LinesAfter   int
	LinesAdded   int
	LinesRemoved int
	Approx       bool
	// TargetHash is the blob hash the path will hold afterwards; empty for
	// RestoreDelete.
	TargetHash string
	// Dirty reports that the working copy currently differs from the FROM
	// commit at this path — uncommitted local work this change destroys.
	Dirty bool

	// preHash is the content hash observed ON DISK at plan time, or "" when the
	// path was absent. Unexported because it is V5 machinery, not something a
	// preview renders: apply compares it against a fresh read to detect a
	// writer that moved between the preview and the confirmation.
	preHash string
}

// RestorePlan is the complete, immutable description of a restore before any
// byte is written. It is what a preview renders and what an apply executes.
type RestorePlan struct {
	RepoID       string
	RootPath     string
	FromCommit   string
	TargetCommit string
	// Selectors are the glob patterns that narrowed the plan (V4). Empty means
	// the whole tree, which is the V1 whole-repo rollback.
	Selectors []string
	// Changes is sorted by Path, so a plan is deterministic and its token is
	// stable across runs.
	Changes []RestoreChange
	// Unchanged counts paths that the selector matched but that already hold
	// the target bytes. They are excluded from Changes and never written.
	Unchanged int
	// ConfirmToken pins this exact plan AND the working-copy state it was
	// computed against. It is two hashes joined by "-": the first covers the
	// intent (endpoints, selectors, per-path op and target hash), the second
	// covers what was observed on disk.
	//
	// Two halves rather than one, because the two kinds of drift need different
	// answers. A changed head means the operator's confirmation refers to a
	// different operation (ErrPlanStale). A changed working copy means
	// something outside the VCS wrote the very files the restore is about to
	// overwrite (ErrExternalMutation) — the failure mode V5's freeze provably
	// cannot prevent, only detect. A single opaque token would collapse both
	// into "try again" and lose the one message worth reading.
	ConfirmToken string

	// targetBytes holds the resolved blob content for every non-delete change.
	// Unexported: it is apply's working set, not part of the preview contract,
	// and a consumer that serialized a plan should not be carrying file
	// contents around.
	targetBytes map[string][]byte
}

// DirtyPaths returns the paths whose uncommitted working-copy content this plan
// would destroy. An empty result means the plan only rewrites paths that match
// the current head.
func (p *RestorePlan) DirtyPaths() []string {
	var out []string
	for _, c := range p.Changes {
		if c.Dirty {
			out = append(out, c.Path)
		}
	}
	return out
}

// Counts returns how many changes create, overwrite and delete a path.
func (p *RestorePlan) Counts() (create, overwrite, del int) {
	for _, c := range p.Changes {
		switch c.Op {
		case RestoreCreate:
			create++
		case RestoreOverwrite:
			overwrite++
		case RestoreDelete:
			del++
		}
	}
	return create, overwrite, del
}

// IsEmpty reports whether the plan would touch nothing.
func (p *RestorePlan) IsEmpty() bool { return len(p.Changes) == 0 }

// PlanRestore computes — without touching the working copy — what restoring
// repoID to targetCommit would do. selectors are optional repo-relative glob
// patterns (V4 selective restore); nil or empty plans the whole tree (V1).
//
// The repo lane is taken for the duration: the plan reads the head, the stored
// trees and the working copy, and a concurrent commit between those reads would
// produce a plan describing a state that never existed.
func (v *VCS) PlanRestore(repoID, targetCommit string, selectors []string) (*RestorePlan, error) {
	unlock := v.lockRepo(repoID)
	defer unlock()
	return v.planRestoreLocked(repoID, targetCommit, selectors)
}

// PlanRevertToSeam is the V1 entry point: preview the whole-tree rollback that
// RevertToSeam(seamID) would perform. It resolves the seam to its commit and
// then plans exactly the tree swap the revert executes, so the preview and the
// execution cannot disagree about scope.
func (v *VCS) PlanRevertToSeam(repoID, seamID string) (*RestorePlan, error) {
	seam, err := v.FindSeam(seamID)
	if err != nil {
		return nil, fmt.Errorf("vcs: plan revert: load seam: %w", err)
	}
	if seam.RepoID != repoID {
		return nil, fmt.Errorf("vcs: plan revert: seam %s belongs to repo %s, not %s",
			seamID, seam.RepoID, repoID)
	}
	return v.PlanRestore(repoID, seam.CommitID, nil)
}

// planRestoreLocked is the shared core. Callers must hold the repo lane.
func (v *VCS) planRestoreLocked(repoID, targetCommit string, selectors []string) (*RestorePlan, error) {
	c, err := v.getCommit(targetCommit)
	if err != nil {
		return nil, fmt.Errorf("vcs: plan: load commit %s: %w", targetCommit, err)
	}
	if c.RepoID != repoID {
		return nil, fmt.Errorf("vcs: plan: commit %s belongs to repo %s, not %s",
			targetCommit, c.RepoID, repoID)
	}
	if c.WorktreeID != "" {
		return nil, fmt.Errorf("vcs: plan: commit %s is worktree commit %s",
			targetCommit, c.WorktreeID)
	}
	r, err := v.getRepo(repoID)
	if err != nil {
		return nil, fmt.Errorf("vcs: plan: load repo: %w", err)
	}

	fromTree := v.commitTree(r.MainHead)
	targetTree := v.commitTree(targetCommit)
	paths, err := selectPaths(sortedTreeUnion(fromTree, targetTree), selectors)
	if err != nil {
		return nil, err
	}
	for _, p := range paths {
		if err := validateRelPath(p); err != nil {
			return nil, err
		}
	}

	// V6: the working-copy reads below go through the confined root handle for
	// the same reason the writes do — a "docs" directory swapped to a symlink
	// would otherwise let the planner report on, and hash, a file outside the
	// repo.
	workRoot, err := openWorkRoot(r.RootPath)
	if err != nil {
		return nil, err
	}
	defer workRoot.Close()

	plan := &RestorePlan{
		RepoID:       repoID,
		RootPath:     r.RootPath,
		FromCommit:   r.MainHead,
		TargetCommit: targetCommit,
		Selectors:    append([]string(nil), selectors...),
		targetBytes:  map[string][]byte{},
	}
	for _, path := range paths {
		change, content, keep, err := v.planOnePath(workRoot, path, fromTree, targetTree)
		if err != nil {
			return nil, err
		}
		if !keep {
			plan.Unchanged++
			continue
		}
		if change.Op != RestoreDelete {
			plan.targetBytes[path] = content
		}
		plan.Changes = append(plan.Changes, change)
	}
	sortChanges(plan.Changes)
	plan.ConfirmToken = planToken(plan) + "-" + workingToken(plan)
	return plan, nil
}

// planOnePath decides a single path's entry. keep is false when the path
// already holds the target bytes and needs no write at all — those are counted
// as Unchanged rather than written, which is what makes a selective restore of
// two files touch two files and not the whole tree.
func (v *VCS) planOnePath(
	workRoot *os.Root, path string, fromTree, targetTree map[string]string,
) (change RestoreChange, content []byte, keep bool, err error) {
	targetHash, inTarget := targetTree[path]
	onDisk, diskErr := rootReadFile(workRoot, path)
	exists := diskErr == nil
	if diskErr != nil && !errors.Is(diskErr, fs.ErrNotExist) {
		return RestoreChange{}, nil, false, fmt.Errorf("vcs: plan: read %s: %w", path, diskErr)
	}

	// Dirty compares the working copy against the FROM tree, not against the
	// target: the question the operator is asking is "what am I about to lose",
	// and what is lost is whatever is on disk and not yet in history.
	fromHash, inFrom := fromTree[path]
	preHash := ""
	if exists {
		preHash = hashContent(onDisk)
	}
	dirty := (exists != inFrom) || (exists && inFrom && preHash != fromHash)

	if !inTarget {
		if !exists {
			// Nothing to delete; the path is already absent.
			return RestoreChange{}, nil, false, nil
		}
		before := countLines(onDisk)
		return RestoreChange{
			Path: path, Op: RestoreDelete,
			LinesBefore: before, LinesRemoved: before,
			Dirty: dirty, preHash: preHash,
		}, nil, true, nil
	}

	target, err := v.getBlob(targetHash)
	if err != nil {
		return RestoreChange{}, nil, false,
			fmt.Errorf("vcs: plan: blob %s for %s: %w", targetHash, path, err)
	}
	if exists && bytes.Equal(onDisk, target) {
		return RestoreChange{}, nil, false, nil
	}
	op := RestoreOverwrite
	if !exists {
		op = RestoreCreate
	}
	added, removed, approx := diffLineCounts(onDisk, target)
	return RestoreChange{
		Path: path, Op: op,
		LinesBefore: countLines(onDisk), LinesAfter: countLines(target),
		LinesAdded: added, LinesRemoved: removed, Approx: approx,
		TargetHash: targetHash, Dirty: dirty, preHash: preHash,
	}, target, true, nil
}

// selectPaths narrows a path list to those matching any selector. An empty
// selector list means "everything" (the whole-tree rollback).
//
// Matching uses guard.MatchGlob, the same matcher the ignore rules and the
// permission profiles use, so an operator who has learned one glob dialect in
// this program has learned all of them. A selector that matches nothing is an
// error rather than an empty plan: silently restoring zero files in response to
// a typo is the failure mode a confirmation step exists to prevent.
func selectPaths(all []string, selectors []string) ([]string, error) {
	if len(selectors) == 0 {
		return all, nil
	}
	var out []string
	matched := make([]bool, len(selectors))
	for _, p := range all {
		for i, sel := range selectors {
			ok, err := guard.MatchGlob(sel, p)
			if err != nil {
				return nil, fmt.Errorf("vcs: bad restore selector %q: %w", sel, err)
			}
			if ok {
				matched[i] = true
				out = append(out, p)
				break
			}
		}
	}
	for i, ok := range matched {
		if !ok {
			return nil, fmt.Errorf("vcs: restore selector %q matched no tracked path", selectors[i])
		}
	}
	return out, nil
}

// planToken derives the INTENT half of ConfirmToken: the endpoints, the
// selectors, and every change's path, op and resulting hash.
//
// It deliberately does NOT cover the line counts — those are presentation, and
// a cosmetic change to how a line is counted must not invalidate a token an
// operator is holding. It also does not cover the observed disk state; that is
// workingToken's job, and keeping them apart is what lets ApplyRestore say
// WHICH kind of drift happened.
func planToken(p *RestorePlan) string {
	h := sha256.New()
	write := func(s string) { h.Write([]byte(s)); h.Write([]byte{0}) }
	write(p.RepoID)
	write(p.FromCommit)
	write(p.TargetCommit)
	write(strconv.Itoa(len(p.Selectors)))
	for _, s := range p.Selectors {
		write(s)
	}
	for _, c := range p.Changes {
		write(c.Path)
		write(string(c.Op))
		write(c.TargetHash)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// workingToken derives the OBSERVED half: the content hash the planner read at
// each touched path, or the empty string where the path was absent.
//
// This half is why an external write is catchable at all. ApplyRestore
// re-plans under the lane, and a re-plan re-reads the disk — so the recomputed
// plan AGREES with whatever a subprocess just wrote and reports no drift. Only
// the hash the ORIGINAL preview carried, arriving back in the caller's token,
// can contradict it.
func workingToken(p *RestorePlan) string {
	h := sha256.New()
	write := func(s string) { h.Write([]byte(s)); h.Write([]byte{0}) }
	for _, c := range p.Changes {
		write(c.Path)
		write(c.preHash)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// splitConfirmToken separates a ConfirmToken into its intent and observed
// halves. ok is false for a token that was never produced by a preview.
func splitConfirmToken(token string) (intent, observed string, ok bool) {
	i := strings.IndexByte(token, '-')
	if i <= 0 || i == len(token)-1 {
		return "", "", false
	}
	return token[:i], token[i+1:], true
}

// countLines counts lines the way an editor does: a trailing newline does not
// start an extra line, and an empty file has zero.
func countLines(b []byte) int {
	if len(b) == 0 {
		return 0
	}
	n := bytes.Count(b, []byte{'\n'})
	if b[len(b)-1] != '\n' {
		n++
	}
	return n
}

// maxExactDiffLines bounds the O(n·m) LCS below. Beyond it the counts degrade
// to whole-file replacement and RestoreChange.Approx is set.
//
// The bound applies AFTER common prefix/suffix trimming, which is what makes it
// generous in practice: a one-line edit in a 100k-line file trims down to a
// 1×1 problem. It only bites when two genuinely unrelated large files sit at
// the same path, and there "every line changed" is close to true anyway.
const maxExactDiffLines = 4000

// diffLineCounts reports how many lines the restore adds and removes at one
// path. approx is true when the exact diff was skipped for size.
func diffLineCounts(before, after []byte) (added, removed int, approx bool) {
	a := splitLines(before)
	b := splitLines(after)

	// Trim the common prefix and suffix first: unchanged lines cannot appear in
	// either count, and removing them is what keeps the DP small for the normal
	// case of a small edit inside a large file.
	start := 0
	for start < len(a) && start < len(b) && a[start] == b[start] {
		start++
	}
	endA, endB := len(a), len(b)
	for endA > start && endB > start && a[endA-1] == b[endB-1] {
		endA--
		endB--
	}
	a, b = a[start:endA], b[start:endB]
	if len(a) == 0 || len(b) == 0 {
		return len(b), len(a), false
	}
	if len(a) > maxExactDiffLines || len(b) > maxExactDiffLines {
		return len(b), len(a), true
	}
	common := lcsLength(a, b)
	return len(b) - common, len(a) - common, false
}

// splitLines splits into lines without keeping a phantom final empty line for a
// trailing newline, matching countLines.
func splitLines(b []byte) []string {
	if len(b) == 0 {
		return nil
	}
	s := string(b)
	s = strings.TrimSuffix(s, "\n")
	return strings.Split(s, "\n")
}

// lcsLength returns the length of the longest common subsequence of two line
// slices, using the two-row DP (O(min) memory) rather than the full table.
func lcsLength(a, b []string) int {
	if len(b) < len(a) {
		a, b = b, a
	}
	prev := make([]int, len(a)+1)
	cur := make([]int, len(a)+1)
	for j := range b {
		for i := range a {
			if a[i] == b[j] {
				cur[i+1] = prev[i] + 1
			} else if prev[i+1] >= cur[i] {
				cur[i+1] = prev[i+1]
			} else {
				cur[i+1] = cur[i]
			}
		}
		prev, cur = cur, prev
		for i := range cur {
			cur[i] = 0
		}
	}
	return prev[len(a)]
}

// sortChanges keeps Changes deterministic. planRestoreLocked already feeds
// sorted paths, but a caller that builds a plan by other means must not be able
// to produce a different token for the same effect.
func sortChanges(changes []RestoreChange) {
	sort.Slice(changes, func(i, j int) bool { return changes[i].Path < changes[j].Path })
}
