package ctxcompact

import (
	"encoding/json"
	"regexp"
	"strings"

	"github.com/cloudwego/eino/schema"
)

const (
	recentWorkingSetWindow = 12 // msgs scanned back from the tail to seed working-set paths
	maxWorkingSetPaths     = 24 // cap so a sprawling session can't pin everything
)

var (
	// pathRe matches repo-relative file paths (dir/file.ext). Conservative:
	// requires a slash + extension to avoid pinning every English sentence
	// that happens to contain a dot.
	pathRe = regexp.MustCompile(`(?:[A-Za-z0-9._-]+/)+[A-Za-z0-9._-]+\.(?:go|rs|md|json|ya?ml|txt|toml|ts|js|py)`)

	errorMarkers = []string{"error:", "error ", "failed", "panic", "traceback", "stack trace", "assertion failed", "test failed"}
	diffMarkers  = []string{"diff --git", "+++ b/", "--- a/", "```diff", "apply_patch"}
)

// deriveWorkingSetPaths scans the recent tail (plus any tool inputs) for
// repo-relative file paths. These define the "what we're editing" set; any
// message mentioning them is pinned so the summary never drops live file
// context (bug③).
func deriveWorkingSetPaths(msgs []*schema.Message, seedIndices []int) []string {
	seen := map[string]bool{}
	var out []string
	add := func(p string) {
		p = strings.TrimSpace(p)
		if p == "" || seen[p] {
			return
		}
		seen[p] = true
		out = append(out, p)
		if len(out) >= maxWorkingSetPaths {
			return
		}
	}

	// 1. seed indices first (explicit pins, e.g. the most recent tool calls)
	for i := len(seedIndices) - 1; i >= 0; i-- {
		idx := seedIndices[i]
		if idx < 0 || idx >= len(msgs) {
			continue
		}
		for _, p := range extractPaths(msgs[idx]) {
			add(p)
		}
	}
	// 2. recent window, newest first
	start := len(msgs) - recentWorkingSetWindow
	if start < 0 {
		start = 0
	}
	for i := len(msgs) - 1; i >= start; i-- {
		for _, p := range extractPaths(msgs[i]) {
			add(p)
		}
		if len(out) >= maxWorkingSetPaths {
			break
		}
	}
	return out
}

func extractPaths(m *schema.Message) []string {
	if m == nil {
		return nil
	}
	var paths []string
	// Free-text scan: Content (covers user/assistant/tool text — no separate
	// Tool-role branch needed) + ReasoningContent. pathRe's slash+extension
	// requirement keeps this from pinning every sentence with a dot.
	paths = append(paths, pathRe.FindAllString(m.Content, -1)...)
	paths = append(paths, pathRe.FindAllString(m.ReasoningContent, -1)...)
	for _, tc := range m.ToolCalls {
		// Explicit tool arguments name files directly — a stronger signal than
		// free-text heuristics, so accept the value verbatim (trimmed) instead of
		// re-filtering through pathRe (which would drop bare names like "main.go"
		// or "README.md" that lack a slash).
		var obj map[string]any
		if json.Unmarshal([]byte(tc.Function.Arguments), &obj) == nil {
			for _, key := range []string{"path", "file", "target"} {
				if s, ok := obj[key].(string); ok {
					if t := strings.TrimSpace(s); t != "" {
						paths = append(paths, t)
					}
				}
			}
		}
	}
	return paths
}

func isErrorMarker(text string) bool {
	low := strings.ToLower(text)
	for _, m := range errorMarkers {
		if strings.Contains(low, m) {
			return true
		}
	}
	return false
}

func isDiffMarker(text string) bool {
	for _, m := range diffMarkers {
		if strings.Contains(text, m) {
			return true
		}
	}
	return false
}

// shouldPin reports whether a message must be preserved verbatim because it
// mentions a working-set path, carries an error, or carries a diff/patch.
func shouldPin(m *schema.Message, workingSetPaths map[string]bool) bool {
	if m == nil {
		return false
	}
	for p := range workingSetPaths {
		if strings.Contains(m.Content, p) || strings.Contains(m.ReasoningContent, p) {
			return true
		}
	}
	return isErrorMarker(m.Content) || isDiffMarker(m.Content)
}
