package tools

import "github.com/x6nux/yanshi/internal/difflib"

// unifiedDiff returns a unified-style line diff between a (original) and b
// (modified) as a sequence of "-" / "+" lines. Unchanged lines are OMITTED to
// keep dry-run output compact (callers render per-file ---/+++ headers).
//
// The LCS computation itself lives in internal/difflib, shared with
// internal/cli/tui's colored transcript rendering (W-E-02) — see that
// package's doc comment for why a shared leaf package rather than a direct
// import between tools and tui.
func unifiedDiff(a, b string) string {
	return difflib.Compact(difflib.Compute(a, b))
}
