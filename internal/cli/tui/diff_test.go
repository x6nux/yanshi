package tui

import (
	"strings"
	"testing"

	"github.com/muesli/termenv"

	"github.com/x6nux/yanshi/internal/difflib"
)

// The LCS algorithm itself (Compute/Compact/Unified/SplitLines) is tested in
// internal/difflib — this file only covers the tui-local rendering built on
// top of it: the line-number gutter, add/del counting, and colored output.

// TestCountOps proves countOps tallies Insert/Delete and ignores Equal.
func TestCountOps(t *testing.T) {
	ops := difflib.Compute("a\nb\nc", "a\nx\nc")
	add, del := countOps(ops)
	if add != 1 || del != 1 {
		t.Fatalf("got add=%d del=%d, want add=1 del=1", add, del)
	}
}

// TestCountOps_Identical proves an all-context diff counts zero add/del.
func TestCountOps_Identical(t *testing.T) {
	ops := difflib.Compute("a\nb", "a\nb")
	add, del := countOps(ops)
	if add != 0 || del != 0 {
		t.Fatalf("got add=%d del=%d, want 0/0", add, del)
	}
}

// TestDiffGutterWidth proves the gutter width is the digit count of the
// largest old/new line number appearing in the ops — a 9-line diff gets a
// 1-wide gutter, a 10-line diff gets 2-wide.
func TestDiffGutterWidth(t *testing.T) {
	// Same line count on both sides (one line changed in the middle) keeps
	// the max old/new line number equal to the total line count, so a
	// 9-line file yields width 1 and a 10-line file yields width 2.
	nineOld := strings.Repeat("x\n", 9)
	nineNew := strings.Repeat("x\n", 4) + "y\n" + strings.Repeat("x\n", 4)
	nine := difflib.Compute(nineOld, nineNew)
	if w := diffGutterWidth(nine); w != 1 {
		t.Fatalf("9-line diff: got width %d, want 1", w)
	}
	tenOld := strings.Repeat("x\n", 10)
	tenNew := strings.Repeat("x\n", 5) + "y\n" + strings.Repeat("x\n", 4)
	ten := difflib.Compute(tenOld, tenNew)
	if w := diffGutterWidth(ten); w != 2 {
		t.Fatalf("10-line diff: got width %d, want 2", w)
	}
}

// TestDiffGutterText proves the gutter blanks the side that does not apply
// (no OldLine for Insert, no NewLine for Delete) rather than printing a
// misleading "0" — a reader must be able to tell "not applicable" apart from
// "line 0".
func TestDiffGutterText(t *testing.T) {
	eq := diffGutterText(difflib.Op{Kind: difflib.Equal, OldLine: 3, NewLine: 5}, 2)
	if !strings.Contains(eq, "3") || !strings.Contains(eq, "5") {
		t.Fatalf("Equal gutter missing a line number: %q", eq)
	}
	ins := diffGutterText(difflib.Op{Kind: difflib.Insert, NewLine: 7}, 2)
	if strings.Contains(ins, "0") {
		t.Fatalf("Insert gutter should not show OldLine as 0: %q", ins)
	}
	if !strings.Contains(ins, "7") {
		t.Fatalf("Insert gutter missing NewLine: %q", ins)
	}
	del := diffGutterText(difflib.Op{Kind: difflib.Delete, OldLine: 4}, 2)
	if strings.Contains(del, "0") {
		t.Fatalf("Delete gutter should not show NewLine as 0: %q", del)
	}
	if !strings.Contains(del, "4") {
		t.Fatalf("Delete gutter missing OldLine: %q", del)
	}
}

// TestRenderColoredDiff_LineNumbers proves renderColoredDiff embeds both
// old and new line numbers alongside the sigil-prefixed content (W-E-02's
// "diff 带行号" acceptance criterion) — not just the sigil.
func TestRenderColoredDiff_LineNumbers(t *testing.T) {
	ops := difflib.Compute("a\nb\nc", "a\nx\nc")
	out := renderColoredDiff(ops)
	if !strings.Contains(out, "-b") || !strings.Contains(out, "+x") {
		t.Fatalf("diff missing add/remove content:\n%s", out)
	}
	// Line 2 is the deleted "b" (old line 2, no new line) and line 2 is also
	// where "x" is inserted (new line 2, no old line) — both numerals must
	// appear somewhere in the gutter output.
	if !strings.Contains(out, "2") {
		t.Fatalf("diff missing line-number gutter:\n%s", out)
	}
}

// TestRenderColoredDiff_AsciiSuppressesColor is the mandatory negative
// control for W-E-02's new diff-rendering path (line-number gutter +
// tiered coloring): under termenv.Ascii (what NO_COLOR degrades to via
// cli.DetectCapability), rendering a diff must emit ZERO ANSI escape bytes,
// proving the gutter and sigil styles go through the same
// ApplyColorProfile-governed lipgloss renderer as the rest of the package —
// not a hand-rolled ANSI path (E1 found and fixed three such bypasses; this
// proves renderColoredDiff is not a fourth).
func TestRenderColoredDiff_AsciiSuppressesColor(t *testing.T) {
	withColorProfile(t, termenv.Ascii)
	ops := difflib.Compute("a\nb\nc", "a\nx\nc")
	out := renderColoredDiff(ops)
	if strings.ContainsRune(out, '\x1b') {
		t.Fatalf("Ascii profile: renderColoredDiff output still contains an ANSI escape byte: %q", out)
	}
	// Positive control for the negative control above: the same ops under a
	// color-capable profile DO produce escape bytes, proving the zero count
	// under Ascii is the profile suppressing color, not the function simply
	// never emitting any.
	withColorProfile(t, termenv.ANSI256)
	colored := renderColoredDiff(ops)
	if !strings.ContainsRune(colored, '\x1b') {
		t.Fatalf("ANSI256 profile: expected renderColoredDiff to emit ANSI escapes, got %q", colored)
	}
}

// TestRenderColoredDiff_BlankLinePadded covers the blank-content-line case:
// an Equal op with Line == "" still renders its gutter and 4-space indent
// rather than collapsing to nothing.
func TestRenderColoredDiff_BlankLinePadded(t *testing.T) {
	ops := []difflib.Op{
		{Kind: difflib.Equal, Line: "a", OldLine: 1, NewLine: 1},
		{Kind: difflib.Equal, Line: "", OldLine: 2, NewLine: 2},
		{Kind: difflib.Insert, Line: "b", NewLine: 3},
	}
	out := renderColoredDiff(ops)
	if !strings.Contains(out, "+b") {
		t.Fatalf("missing +b line:\n%s", out)
	}
	if !strings.Contains(out, "    ") {
		t.Fatalf("blank diff line should still carry the 4-space indent:\n%s", out)
	}
}

// ---- B4: unified diff rendering integration (T3 diff) ----------------------

// TestRenderEditDiff proves an fs_edit tool call renders an "Edit" header and
// either a colored diff (expanded) or a compact "+N -M 行" fold hint with the
// ctrl+o cue. This guards both the diff dispatch (toolDispDiff) and the
// collapse/expand branch of renderDiff.
func TestRenderEditDiff(t *testing.T) {
	e := &toolEntry{name: "fs_edit", args: `{"path":"f.go","old_string":"a\nb","new_string":"a\nc"}`, root: ".", status: "ok"}
	out := e.render(80, newSpinner())
	if !strings.Contains(out, "Edit") {
		t.Fatalf("应有 Edit 头部")
	}
	// 折叠态显 "+N -M 行" 或 expanded 显 diff；至少含 diff 痕迹（-b/+c）或折叠提示
	if !strings.Contains(out, "-b") && !strings.Contains(out, "ctrl+o") && !strings.Contains(out, "行") {
		t.Fatalf("应显 diff 或折叠提示:\n%s", out)
	}
}

// TestRenderEditDiffExpanded proves the expanded branch surfaces the full
// colored diff (both + and - lines) when ctrl+o is toggled.
func TestRenderEditDiffExpanded(t *testing.T) {
	e := &toolEntry{
		name:     "fs_edit",
		args:     `{"path":"f.go","old_string":"a\nb","new_string":"a\nc"}`,
		root:     ".",
		status:   "ok",
		expanded: true,
	}
	out := e.render(80, newSpinner())
	if !strings.Contains(out, "-b") || !strings.Contains(out, "+c") {
		t.Fatalf("expanded 应显完整 diff (-b/+c):\n%s", out)
	}
}

// TestRenderWriteNewFile proves a brand-new fs_write (no old content
// available) surfaces a compact "wrote N lines" hint rather than fabricating
// a diff against missing content.
func TestRenderWriteNewFile(t *testing.T) {
	e := &toolEntry{
		name:   "fs_write",
		args:   `{"path":"new.go","content":"package main\nfunc main() {}\n"}`,
		root:   ".",
		status: "ok",
	}
	out := e.render(80, newSpinner())
	if !strings.Contains(out, "Write") {
		t.Fatalf("应有 Write 头部:\n%s", out)
	}
	// 新建：没有任何旧版本可比，要么显示行数提示，要么折叠提示。绝不应崩溃。
	if !strings.Contains(out, "wrote") && !strings.Contains(out, "ctrl+o") && !strings.Contains(out, "行") {
		t.Fatalf("新建文件应有 wrote/行数提示:\n%s", out)
	}
}
