package tools

import "strings"

// unifiedDiff returns a unified-style line diff between a (original) and b
// (modified) as a sequence of "-" / "+" lines. Unchanged lines are OMITTED to
// keep dry-run output compact (callers render per-file ---/+++ headers). It uses
// a classic LCS dynamic program (O(n·m) time and space) — acceptable for dry-run
// rendering where the files touched by a patch are small. Line endings are
// normalized (\r\n and lone \r fold to \n) before comparing.
func unifiedDiff(a, b string) string {
	la := splitDiffLines(a)
	lb := splitDiffLines(b)
	n, m := len(la), len(lb)
	// dp[i][j] = length of the longest common subsequence of la[i:] and lb[j:].
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
		// Prefer removing from a when the LCS via (i+1,j) is at least as long;
		// otherwise add from b. Either way unchanged lines are skipped.
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

// splitDiffLines splits s into lines for diffing, folding \r\n and lone \r to \n
// first and dropping a trailing empty element produced by a final newline. An
// empty input returns nil (zero lines).
func splitDiffLines(s string) []string {
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
