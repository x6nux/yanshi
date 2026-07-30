package tui

import "strings"

// diffOpKind tags a diff line as equal (context), deleted (from old), or
// inserted (in new). The kind drives both the leading sigil (" "/"-/"+") and
// the color used when rendering the diff inside a tool-call block.
type diffOpKind int

const (
	opEq  diffOpKind = iota // unchanged context line
	opDel                   // line present in old, absent in new
	opIns                   // line present in new, absent in old
)

// diffOp is one elementary line of the LCS edit script.
type diffOp struct {
	kind diffOpKind
	line string
}

// unifiedDiff produces a line-level unified diff of oldS vs newS using a
// classic LCS dynamic-programming table. Each output line is prefixed with a
// single sigil: " " (context), "-" (removed), "+" (added). The sigil is left
// bare (no ANSI) so the caller can color each line by inspecting its first
// byte; the result is trimmed of the trailing newline so it composes cleanly
// with lipgloss rendering.
//
// Why LCS over Myers: for a chat transcript the inputs are short (typical
// fs_edit hunks are tens of lines), so the O(m·n) table is negligible, and
// the textbook DP has no third-party dependency. Myers would be asymptotically
// better on large inputs but adds complexity we do not need here.
//
// Edge cases: equal inputs collapse to all-context output; empty old → all
// "+"; empty new → all "-"; both empty → "".
func unifiedDiff(oldS, newS string) string {
	a := splitDiffLines(oldS)
	b := splitDiffLines(newS)
	ops := lcsDiff(a, b)
	if len(ops) == 0 {
		return ""
	}
	var sb strings.Builder
	for i, o := range ops {
		if i > 0 {
			sb.WriteByte('\n')
		}
		switch o.kind {
		case opEq:
			sb.WriteString(" " + o.line)
		case opDel:
			sb.WriteString("-" + o.line)
		case opIns:
			sb.WriteString("+" + o.line)
		}
	}
	return sb.String()
}

// splitDiffLines splits s on "\n", dropping a single trailing newline so a
// final "\n" (the common case for file content) does not invent a spurious
// empty last line. Returns nil for "" so lcsDiff handles the all-empty case
// as a true zero-length slice.
func splitDiffLines(s string) []string {
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

// lcsDiff computes an edit script turning a into b via a textbook LCS DP:
//
//	dp[i][j] = length of the longest common subsequence of a[:i] and b[:j]
//
// The DP fills (m+1)·(n+1); we then backtrack from (m,n) to (0,0) emitting
// opEq when a[i-1]==b[j-1] and consuming from the side with the larger LCS
// otherwise. The backtrack emits ops in reverse order, so we in-place reverse
// before returning.
//
// Tie-break: when dp[i-1][j] == dp[i][j-1] we prefer to emit opIns (consume
// from b). This keeps insertions visually above the matching context, which
// reads as "the new version adds these lines" — the conventional ordering in
// unified diff output.
func lcsDiff(a, b []string) []diffOp {
	m, n := len(a), len(b)
	dp := make([][]int, m+1)
	for i := range dp {
		dp[i] = make([]int, n+1)
	}
	for i := 1; i <= m; i++ {
		for j := 1; j <= n; j++ {
			if a[i-1] == b[j-1] {
				dp[i][j] = dp[i-1][j-1] + 1
			} else if dp[i-1][j] >= dp[i][j-1] {
				dp[i][j] = dp[i-1][j]
			} else {
				dp[i][j] = dp[i][j-1]
			}
		}
	}
	var ops []diffOp
	i, j := m, n
	for i > 0 || j > 0 {
		switch {
		case i > 0 && j > 0 && a[i-1] == b[j-1]:
			ops = append(ops, diffOp{kind: opEq, line: a[i-1]})
			i--
			j--
		case j > 0 && (i == 0 || dp[i][j-1] >= dp[i-1][j]):
			// Prefer insert on tie so new lines read above following context.
			ops = append(ops, diffOp{kind: opIns, line: b[j-1]})
			j--
		default:
			ops = append(ops, diffOp{kind: opDel, line: a[i-1]})
			i--
		}
	}
	// Reverse: backtrack collected end-to-start.
	for l, r := 0, len(ops)-1; l < r; l, r = l+1, r-1 {
		ops[l], ops[r] = ops[r], ops[l]
	}
	return ops
}

// countDiffAddDel tallifies the number of "+" and "-" lines in a unified diff
// (produced by unifiedDiff). Used to render the compact "+N -M 行" hint in the
// collapsed tool-call block without re-walking the source strings.
func countDiffAddDel(diff string) (add, del int) {
	for _, ln := range strings.Split(diff, "\n") {
		if len(ln) == 0 {
			continue
		}
		switch ln[0] {
		case '+':
			add++
		case '-':
			del++
		}
	}
	return add, del
}
