package tui

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/x6nux/yanshi/internal/difflib"
)

// countOps tallies the number of Insert and Delete ops in a difflib edit
// script. Used to render the compact "+N -M 行" hint in the collapsed
// tool-call block (renderDiff) without a second pass over rendered text.
func countOps(ops []difflib.Op) (add, del int) {
	for _, o := range ops {
		switch o.Kind {
		case difflib.Insert:
			add++
		case difflib.Delete:
			del++
		}
	}
	return add, del
}

// diffGutterWidth returns the column width needed to right-align every
// old/new line number appearing in ops, so the gutter stays fixed-width for
// the whole diff (a 9-line hunk and a 900-line hunk both align cleanly
// within themselves; W-E-02 doesn't need cross-diff alignment).
func diffGutterWidth(ops []difflib.Op) int {
	max := 0
	for _, o := range ops {
		if o.OldLine > max {
			max = o.OldLine
		}
		if o.NewLine > max {
			max = o.NewLine
		}
	}
	if max == 0 {
		return 1
	}
	return len(strconv.Itoa(max))
}

// diffGutterText renders one op's two-column line-number gutter: old line
// number, new line number, each right-aligned to width and blank on the side
// that does not apply (Delete has no NewLine, Insert has no OldLine) — the
// same convention GitHub's split/unified diff gutters use, so a line missing
// from one side reads as a gap rather than a misleading "0".
func diffGutterText(o difflib.Op, width int) string {
	oldLn, newLn := "", ""
	if o.OldLine > 0 {
		oldLn = strconv.Itoa(o.OldLine)
	}
	if o.NewLine > 0 {
		newLn = strconv.Itoa(o.NewLine)
	}
	return fmt.Sprintf("%*s %*s ", width, oldLn, width, newLn)
}

// renderColoredDiff renders a difflib edit script (as produced by
// difflib.Compute) with each line prefixed by a line-number gutter
// (diffGutterText) and colored by its kind, indented by 4 spaces so the diff
// visually nests under the tool-call header like the ⎿ result line of
// renderNormal. The sigil byte is kept inside the colored segment (not the
// gutter) so a reader can still distinguish + / - / " " at a glance
// independent of color support.
func renderColoredDiff(ops []difflib.Op) string {
	const pad = "    "
	width := diffGutterWidth(ops)
	var b strings.Builder
	for i, o := range ops {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(pad)
		b.WriteString(diffGutterStyle.Render(diffGutterText(o, width)))
		switch o.Kind {
		case difflib.Delete:
			b.WriteString(diffDelStyle.Render("-" + o.Line))
		case difflib.Insert:
			b.WriteString(diffAddStyle.Render("+" + o.Line))
		default:
			b.WriteString(diffCtxStyle.Render(" " + o.Line))
		}
	}
	return b.String()
}
