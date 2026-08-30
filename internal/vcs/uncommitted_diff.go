package vcs

import (
	"sort"
	"unicode/utf8"
)

// UncommittedFile is one path's old/new text for a scope's pending
// (uncommitted) changeset — the content-bearing counterpart to Uncommitted's
// path→hash map, for W-E-13's /diff command. OldText is empty for Op=="added"
// (no prior blob exists); NewText is empty for Op=="deleted" (the row's
// blob_hash is empty per recordDelete, so there is nothing to fetch). Each
// side is checked independently with utf8.ValidString; a binary side is left
// empty (never fed into difflib.Compute) even if its sibling side is valid
// text — Op is preserved either way so the caller can always list the path.
type UncommittedFile struct {
	Path    string
	Op      string // added | modified | deleted
	OldText string
	NewText string
}

// UncommittedDiff resolves a scope's pending vcs_uncommitted rows into
// old/new text pairs, ready for difflib.Compute + the W-E-02 renderer. The
// old side comes from the scope's head tree (scopeHeadTree), the new side
// from the row's own blob_hash — the same two lookups commitScope folds
// together when it turns this changeset into a commit, so a path that
// UncommittedDiff can't read is a path CommitMain/CommitWorktree can't fold
// either. Returns an empty (non-nil) slice for an unknown or empty scope.
// A row whose blob is missing or non-UTF8 is never skipped — it is always
// appended to the result with that side's text left empty, and Op preserved
// either way (matching UncommittedFile's own doc comment), so a corrupt or
// binary blob on one path never hides any other pending edit and the caller
// can always list every path.
func (v *VCS) UncommittedDiff(scopeType, scopeID string) ([]UncommittedFile, error) {
	head := v.scopeHeadTree(scopeType, scopeID)
	rows, err := v.store.DB.Query(
		"SELECT path, blob_hash, op FROM vcs_uncommitted WHERE scope_type=? AND scope_id=?",
		scopeType, scopeID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []UncommittedFile{}
	for rows.Next() {
		var p, h, op string
		if rows.Scan(&p, &h, &op) != nil {
			continue
		}
		var oldText, newText string
		if oldHash, ok := head[p]; ok {
			if b, err := v.getBlob(oldHash); err == nil && utf8.ValidString(string(b)) {
				oldText = string(b)
			}
		}
		if op != "deleted" && h != "" {
			if b, err := v.getBlob(h); err == nil && utf8.ValidString(string(b)) {
				newText = string(b)
			}
		}
		out = append(out, UncommittedFile{Path: p, Op: op, OldText: oldText, NewText: newText})
	}
	if rerr := rows.Err(); rerr != nil {
		return nil, rerr
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// CommitRangeDiff resolves the path-level Diff between refA and refB (two
// commit ids in the same repo's history) into old/new text pairs, in the
// same UncommittedFile shape UncommittedDiff produces — /diff (W-E-13, RE-1)
// renders both through identical response-building code in ws_diff.go
// regardless of which VCS query produced the changeset: UncommittedDiff for
// the (structurally near-always-empty, see SessionBaseline's doc comment)
// literal pending rows, CommitRangeDiff for the durable commit history a
// session's baseline seam actually lets /diff show something meaningful.
//
// refA=="" is a valid "no baseline" input, not an error: Diff's
// commitTree("") is the empty tree, so every path in refB's tree diffs as
// added. Text resolution mirrors UncommittedDiff — a missing or non-UTF8
// blob on either side is never skipped, just left empty on that side (Op is
// preserved either way).
func (v *VCS) CommitRangeDiff(repoID, refA, refB string) ([]UncommittedFile, error) {
	changes, err := v.Diff(repoID, refA, refB)
	if err != nil {
		return nil, err
	}
	out := make([]UncommittedFile, 0, len(changes))
	for _, c := range changes {
		var oldText, newText string
		if c.OldHash != "" {
			if b, err := v.getBlob(c.OldHash); err == nil && utf8.ValidString(string(b)) {
				oldText = string(b)
			}
		}
		if c.NewHash != "" {
			if b, err := v.getBlob(c.NewHash); err == nil && utf8.ValidString(string(b)) {
				newText = string(b)
			}
		}
		out = append(out, UncommittedFile{Path: c.Path, Op: c.Op, OldText: oldText, NewText: newText})
	}
	return out, nil
}
