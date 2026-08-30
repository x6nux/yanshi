// Package difflib computes and renders line-level diffs shared by
// internal/tools (compact dry-run diff bodies) and internal/cli/tui (colored,
// line-numbered diff panels in the transcript). It lives in its own leaf
// package — zero internal dependencies — because internal/tools and
// internal/cli/tui have no dependency edge between them: internal/cli/tui
// already depends on internal/tools transitively (tui -> cli -> tools), so
// tools importing tui directly would form an import cycle Go's compiler
// rejects outright, and tui importing tools directly would still cross the
// hexagonal layering CLAUDE.md documents for no shared benefit.
//
// Before this package existed the two call sites carried independent copies
// of the same O(n·m) LCS algorithm. The spec that introduced diff line
// numbers and tiered coloring (W-E-02) called the duplication out by name:
// without merging, highlighting and line numbers would have to be written
// twice, and W-F's planned turn-level aggregate diff would become a third
// copy.
package difflib

import "strings"

// OpKind tags one line of a diff edit script.
type OpKind int

const (
	// Equal marks a context line present in both the old and new text.
	Equal OpKind = iota
	// Delete marks a line present only in the old text.
	Delete
	// Insert marks a line present only in the new text.
	Insert
)

// Op is one elementary line of an LCS edit script, as produced by Compute.
// OldLine/NewLine are 1-based positions in their respective inputs; the side
// that does not apply is 0 (NewLine for a Delete, OldLine for an Insert) so a
// renderer can tell "not applicable" apart from "line 0" without a second
// return value.
type Op struct {
	Kind    OpKind
	Line    string
	OldLine int
	NewLine int
}

// Compute returns the line-level LCS edit script turning a into b, using a
// classic O(n·m) dynamic-programming table backtracked from (len(a),len(b))
// to (0,0). Inputs are short in every known caller (fs_edit hunks, whole
// small files for fs_write) so the quadratic table is negligible; Myers would
// be asymptotically better on large inputs but adds complexity none of the
// callers need.
//
// Tie-break: when the LCS length via consuming from a equals the length via
// consuming from b, Compute prefers to consume from b (emit Insert). This
// keeps new lines reading above the following matched context, the
// conventional ordering in unified diff output.
//
// This is NOT the tie-break internal/tools' predecessor unifiedDiff had
// (deleted by this merge): that forward-DP implementation broke the same
// ties toward Delete-from-a. Both are valid — an LCS tie means more than one
// equally-minimal edit script exists — but they are different scripts, and
// this reconciliation was previously undocumented (only SplitLines's
// reconciliation was called out). Over every ordered pair of {A,B,C}-alphabet
// line sequences of length <= 3 (1600 pairs), exactly 36 produce a different
// (still equally minimal — same total -/+ line count) script; see
// TestTieBreak_ChangedFromOldToolsAlgorithm in tiebreak_test.go, which
// recomputes this figure against a reimplementation of the deleted
// algorithm rather than restating it as a number that can silently drift.
//
// Edge cases: equal inputs collapse to all-Equal ops; empty a → all Insert;
// empty b → all Delete; both empty → nil.
func Compute(a, b string) []Op {
	la := SplitLines(a)
	lb := SplitLines(b)
	m, n := len(la), len(lb)
	dp := make([][]int, m+1)
	for i := range dp {
		dp[i] = make([]int, n+1)
	}
	for i := 1; i <= m; i++ {
		for j := 1; j <= n; j++ {
			switch {
			case la[i-1] == lb[j-1]:
				dp[i][j] = dp[i-1][j-1] + 1
			case dp[i-1][j] >= dp[i][j-1]:
				dp[i][j] = dp[i-1][j]
			default:
				dp[i][j] = dp[i][j-1]
			}
		}
	}
	var rev []Op
	i, j := m, n
	for i > 0 || j > 0 {
		switch {
		case i > 0 && j > 0 && la[i-1] == lb[j-1]:
			rev = append(rev, Op{Kind: Equal, Line: la[i-1], OldLine: i, NewLine: j})
			i--
			j--
		case j > 0 && (i == 0 || dp[i][j-1] >= dp[i-1][j]):
			rev = append(rev, Op{Kind: Insert, Line: lb[j-1], NewLine: j})
			j--
		default:
			rev = append(rev, Op{Kind: Delete, Line: la[i-1], OldLine: i})
			i--
		}
	}
	ops := make([]Op, len(rev))
	for k, o := range rev {
		ops[len(rev)-1-k] = o
	}
	return ops
}

// Compact renders ops as the "-"/"+"-sigil-only body used by dry-run patch
// previews: Equal lines are omitted entirely so unchanged context does not
// bloat the preview. Each emitted line is terminated with "\n" (including the
// last), matching the format internal/tools has always produced — callers
// there wrap this body with their own "---"/"+++"/"@@" headers.
func Compact(ops []Op) string {
	var b strings.Builder
	for _, o := range ops {
		switch o.Kind {
		case Delete:
			b.WriteString("-" + o.Line + "\n")
		case Insert:
			b.WriteString("+" + o.Line + "\n")
		}
	}
	return b.String()
}

// SplitLines splits s into lines: \r\n and lone \r fold to \n first, then
// exactly one trailing \n is dropped (not TrimRight — a second trailing \n is
// a genuine blank last line and must survive as an empty element) before
// splitting on \n. Returns nil for "" (zero lines).
//
// Exported because internal/tools also uses it outside of Compute, to split
// a whole file's bytes for per-line "+" prefixing when rendering a brand-new
// file's dry-run preview (fs_patch.go's prefixLines) — same line-splitting
// convention, unrelated to diffing.
//
// This reconciles the two predecessor implementations, which disagreed here:
// internal/tools dropped one trailing split element (this behavior);
// internal/cli/tui's TrimRight silently swallowed every trailing blank line.
// No existing test observed the difference (both agree on 0 or 1 trailing
// newline), so adopting tools' more-correct behavior for tui's former inputs
// is not a regression.
func SplitLines(s string) []string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	if s == "" {
		return nil
	}
	lines := strings.Split(s, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}
