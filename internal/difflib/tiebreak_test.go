package difflib

import (
	"strings"
	"testing"
)

// This file pins RE-7: merging internal/tools' and internal/cli/tui's
// duplicate LCS implementations into this package's single Compute silently
// changed the tie-break internal/tools' unifiedDiff used to have.
//
// The old internal/tools code (git show 6134d62:internal/tools/fs_diff.go,
// deleted by the merge) filled its DP table BACKWARD (dp[i][j] = LCS length
// of la[i:]/lb[j:]) and, on a length tie during the forward reconstruction
// loop, preferred to emit a Delete from a ("if dp[i+1][j] >= dp[i][j+1])").
// Compute fills its DP table FORWARD and backtracks from (m,n), and on the
// mirrored tie prefers to emit an Insert from b (see Compute's own doc
// comment). Both are valid minimal (shortest) edit scripts — LCS ties are
// exactly the inputs where more than one shortest script exists — but they
// are not the SAME script, and nothing in difflib.go's merge-rationale
// comment mentioned this before RE-7; it only discussed SplitLines's
// reconciliation.
//
// refUnifiedDiff below is a byte-for-byte reimplementation of the deleted
// internal/tools code (including its own line-splitting, not a call to this
// package's SplitLines) so this test has an independent oracle: sharing a
// parser or a helper with the code under test would make any property here
// vacuously true instead of actually exercising the tie-break difference
// (see the project's "property tests need an independent oracle" lesson).

// refUnifiedDiff is the old internal/tools/fs_diff.go algorithm, unmodified
// apart from renaming (unifiedDiff -> refUnifiedDiff, splitDiffLines ->
// refSplitLines) to avoid colliding with this package's exports. Do not
// "clean it up" to match Compute's style — the whole point is that it stays
// a faithful copy of what was actually deleted.
func refUnifiedDiff(a, b string) string {
	la := refSplitLines(a)
	lb := refSplitLines(b)
	n, m := len(la), len(lb)
	dp := make([][]int, n+1)
	for i := range dp {
		dp[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if la[i] == lb[j] {
				dp[i][j] = dp[i+1][j+1] + 1
				continue
			}
			dp[i][j] = dp[i+1][j]
			if dp[i][j+1] > dp[i][j] {
				dp[i][j] = dp[i][j+1]
			}
		}
	}
	var out strings.Builder
	i, j := 0, 0
	for i < n && j < m {
		if la[i] == lb[j] {
			i++
			j++
			continue
		}
		if dp[i+1][j] >= dp[i][j+1] {
			out.WriteString("-" + la[i] + "\n")
			i++
		} else {
			out.WriteString("+" + lb[j] + "\n")
			j++
		}
	}
	for i < n {
		out.WriteString("-" + la[i] + "\n")
		i++
	}
	for j < m {
		out.WriteString("+" + lb[j] + "\n")
		j++
	}
	return out.String()
}

func refSplitLines(s string) []string {
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

// allSequences returns every sequence of 0..maxLen letters drawn from
// alphabet, each rendered as a "\n"-joined line blob ready to feed both
// diff functions. With alphabet={A,B,C} and maxLen=3 this returns
// 1+3+9+27=40 blobs.
func allSequences(alphabet []string, maxLen int) []string {
	var out []string
	var gen func(prefix []string, depth int)
	gen = func(prefix []string, depth int) {
		out = append(out, strings.Join(prefix, "\n"))
		if depth == maxLen {
			return
		}
		for _, c := range alphabet {
			gen(append(append([]string{}, prefix...), c), depth+1)
		}
	}
	gen(nil, 0)
	return out
}

// sigilCounts returns the number of "-" and "+" prefixed lines in a Compact-
// shaped diff body (used to check both algorithms agree on edit DISTANCE
// even on the 36 pairs where they disagree on which equally-minimal script
// to emit).
func sigilCounts(body string) (dels, inss int) {
	for _, ln := range strings.Split(body, "\n") {
		switch {
		case strings.HasPrefix(ln, "-"):
			dels++
		case strings.HasPrefix(ln, "+"):
			inss++
		}
	}
	return dels, inss
}

// TestTieBreak_ChangedFromOldToolsAlgorithm exhaustively compares Compute's
// (via Compact, the shape internal/tools' old unifiedDiff produced — see
// Compact's doc comment) output against refUnifiedDiff's, over every ordered
// pair of {A,B,C}-alphabet line sequences of length <= 3 (40 sequences, 1600
// ordered pairs). This is the "recompute" RE-7 asked to be left behind
// rather than a magic number restated in prose: rerun this test after any
// future change to Compute's tie-break and it will report the new count.
//
// Currently pins exactly 36 differing pairs (2.25% of 1600) — the same
// figure the review measured. A differing pair is never a bug by itself:
// both sides still produce a script of the SAME total edit distance (same
// -/+ line counts), asserted below for every one of the 36; the difference
// is purely which of several equally-minimal LCS alignments each tie-break
// picks.
func TestTieBreak_ChangedFromOldToolsAlgorithm(t *testing.T) {
	seqs := allSequences([]string{"A", "B", "C"}, 3)
	if len(seqs) != 40 {
		t.Fatalf("allSequences produced %d sequences, want 40", len(seqs))
	}

	total := 0
	differing := 0
	for _, a := range seqs {
		for _, b := range seqs {
			total++
			old := refUnifiedDiff(a, b)
			neu := Compact(Compute(a, b))
			if old == neu {
				continue
			}
			differing++

			oldDel, oldIns := sigilCounts(old)
			newDel, newIns := sigilCounts(neu)
			if oldDel != newDel || oldIns != newIns {
				t.Fatalf(
					"a=%q b=%q: differing scripts disagree on edit distance too "+
						"(old -%d/+%d, new -%d/+%d) — this is a real regression, not "+
						"just a tie-break preference:\nold:\n%s\nnew:\n%s",
					a, b, oldDel, oldIns, newDel, newIns, old, neu,
				)
			}
		}
	}

	if total != 1600 {
		t.Fatalf("total ordered pairs = %d, want 1600", total)
	}
	if differing != 36 {
		t.Fatalf(
			"differing pairs = %d, want 36 (2.25%% of 1600) — the tie-break's "+
				"measured footprint changed; if this is an intentional Compute "+
				"change, update this number (and difflib.go's doc comment) rather "+
				"than silently letting it drift like the original undocumented "+
				"merge did",
			differing,
		)
	}
}
