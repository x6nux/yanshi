package tui

import (
	"strings"

	"github.com/x6nux/yanshi/internal/difflib"
)

// unifiedDiff produces a line-level unified diff of oldS vs newS: each output
// line is prefixed with a single sigil " " (context), "-" (removed), "+"
// (added), joined by "\n" with no trailing newline so it composes cleanly
// with lipgloss rendering. The sigil is left bare (no ANSI) so the caller can
// color each line by inspecting its first byte.
//
// The LCS computation itself lives in internal/difflib, shared with
// internal/tools' compact dry-run diff bodies (W-E-02) — see that package's
// doc comment for why a shared leaf package rather than a direct import
// between tui and tools.
func unifiedDiff(oldS, newS string) string {
	return difflib.Unified(difflib.Compute(oldS, newS))
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
