// Package instruct loads hierarchical project instruction files (AGENTS.md).
//
// The orchestrator's system prompt is static, baked once at bootstrap from the
// repository-root instruction file. instruct.LoadHierarchical additionally
// merges the per-directory AGENTS.md chain for a given target path so that
// file-touching tools (fs_read) can surface the instructions relevant to the
// specific directory the agent is operating in — parent directives first, child
// (more specific) directives last so the child overrides the parent by order.
//
// At each directory level the first present file is read, trying the modern
// AGENTS.md, then the legacy AGENT.md, then CLAUDE.md. Both per-item and total
// sizes are hard-capped with a truncation marker so a runaway instruction file
// cannot blow out the context window.
package instruct

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// maxItemBytes caps a single instruction file's contribution. A larger file is
// truncated to this many bytes plus a marker, so one giant AGENTS.md cannot
// monopolize the budget.
const maxItemBytes = 32 << 10 // 32 KiB

// maxTotalBytes caps the merged instruction text across ALL levels. Once the
// running total reaches this, the current segment is truncated and remaining
// levels are dropped, so a deep directory chain cannot exceed the budget.
const maxTotalBytes = 64 << 10 // 64 KiB

// instructionFiles is the per-level fallback order. AGENTS.md is the modern
// convention; AGENT.md is the legacy name this codebase historically read;
// CLAUDE.md is the Claude-Code-compatible fallback.
var instructionFiles = []string{"AGENTS.md", "AGENT.md", "CLAUDE.md"}

// LoadHierarchical returns merged project instructions for the directory chain
// from rootDir down to targetDir (both directories). At each level the first
// present file in instructionFiles is read; levels are concatenated parent→child
// so more-specific (child) instructions appear last. Each item AND the total are
// hard-capped (see maxItemBytes / maxTotalBytes) with a truncation marker.
//
// Returns "" when no instruction file exists at any level in the chain. If
// targetDir is outside rootDir, only the rootDir level is read (the walk never
// escapes the project root).
func LoadHierarchical(rootDir, targetDir string) string {
	return loadChain(rootDir, targetDir, true)
}

// NestedInstructions is LoadHierarchical EXCLUDING the rootDir level. It returns
// only instructions from directories strictly below rootDir, so injecting it
// into a per-file tool result does not duplicate the root-level instructions
// already baked into the orchestrator's system prompt by bootstrap. Returns ""
// for a target directly in rootDir or when no nested instruction file exists.
func NestedInstructions(rootDir, targetDir string) string {
	return loadChain(rootDir, targetDir, false)
}

// loadChain is the shared walker. includeRoot=false skips the rootDir level
// (NestedInstructions). The chain is built rootDir→targetDir (parent first); if
// targetDir is not under rootDir, the chain collapses to rootDir only.
func loadChain(rootDir, targetDir string, includeRoot bool) string {
	rootClean := filepath.Clean(rootDir)
	targetClean := filepath.Clean(targetDir)
	if !isWithin(targetClean, rootClean) {
		targetClean = rootClean
	}

	// Build the directory chain from targetClean up to rootClean, then reverse
	// to parent→child order. Prepending while walking up yields root-first.
	var chain []string
	d := targetClean
	for {
		if d == rootClean {
			chain = append([]string{rootClean}, chain...)
			break
		}
		chain = append([]string{d}, chain...)
		parent := filepath.Dir(d)
		if parent == d {
			// Reached the filesystem root without hitting rootClean (should not
			// happen after the isWithin guard, but defend against edge cases).
			break
		}
		d = parent
	}

	var parts []string
	total := 0
	for _, dir := range chain {
		if !includeRoot && dir == rootClean {
			continue
		}
		body, ok := readInstruction(dir)
		if !ok {
			continue
		}
		body = capItem(body)
		segment := fmt.Sprintf("## Instructions (%s)\n%s", relOrSelf(rootClean, dir), body)
		if total+len(segment) > maxTotalBytes {
			remaining := maxTotalBytes - total
			if remaining <= 0 {
				break
			}
			segment = segment[:remaining] + "\n[... truncated: total instruction cap reached ...]"
			parts = append(parts, segment)
			break
		}
		parts = append(parts, segment)
		total += len(segment)
	}
	return strings.Join(parts, "\n\n")
}

// readInstruction reads the first present instruction file in dir (per the
// fallback order). Returns (content, true) when one exists, else ("", false).
// A read error for a present file is silently ignored so a permissions issue on
// one file does not abort the whole chain.
func readInstruction(dir string) (string, bool) {
	for _, name := range instructionFiles {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err == nil {
			return string(data), true
		}
	}
	return "", false
}

// capItem truncates a single instruction file's bytes to maxItemBytes with a
// marker. No-op when already within the cap.
func capItem(s string) string {
	if len(s) <= maxItemBytes {
		return s
	}
	return s[:maxItemBytes] + "\n[... truncated: single instruction file cap reached ...]"
}

// relOrSelf returns dir relative to root ("." when they are equal), falling back
// to the absolute dir on error. Used only for the human-readable per-level
// header.
func relOrSelf(root, dir string) string {
	if dir == root {
		return "."
	}
	rel, err := filepath.Rel(root, dir)
	if err != nil {
		return dir
	}
	return rel
}

// isWithin reports whether child is equal to root or a descendant of it. Both
// inputs must be cleaned.
func isWithin(child, root string) bool {
	if child == root {
		return true
	}
	return strings.HasPrefix(child, root+string(filepath.Separator))
}
