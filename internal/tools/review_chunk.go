package tools

import "strings"

const reviewChunkLimit = 48 * 1024

// chunkDiff splits diff text into <=limit-byte chunks without cutting through
// a single hunk. The splitter walks line-by-line and flushes the accumulated
// buffer whenever it exceeds `limit` AND the next line starts a new hunk
// boundary ("@@ " or file separator "--- " / "+++ " / "diff --git ").
func chunkDiff(diff string, limit int) []string {
	if limit <= 0 {
		limit = reviewChunkLimit
	}
	if len(diff) <= limit {
		return []string{diff}
	}
	var chunks []string
	var cur strings.Builder
	for _, line := range strings.SplitAfter(diff, "\n") {
		// Flush at hunk boundary if buffer is already over the limit.
		if cur.Len() > limit && (strings.HasPrefix(line, "@@ ") || strings.HasPrefix(line, "--- ") || strings.HasPrefix(line, "diff --git ")) {
			chunks = append(chunks, cur.String())
			cur.Reset()
		}
		cur.WriteString(line)
	}
	if cur.Len() > 0 {
		chunks = append(chunks, cur.String())
	}
	// Guarantee no chunk is larger than limit + (one line) — never mid-hunk.
	var safe []string
	for _, c := range chunks {
		if len(c) <= limit {
			safe = append(safe, c)
			continue
		}
		// Fallback: hard split on line boundaries (still no mid-line cut).
		for len(c) > limit {
			cut := strings.LastIndexByte(c[:limit], '\n')
			if cut <= 0 {
				cut = limit
			}
			safe = append(safe, c[:cut])
			c = c[cut:]
		}
		if len(c) > 0 {
			safe = append(safe, c)
		}
	}
	return safe
}
