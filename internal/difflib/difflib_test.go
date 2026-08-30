package difflib

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCompute_Modify proves Compute produces a minimal line-level LCS diff:
// unchanged lines are Equal, the changed line is one Delete + one Insert. The
// classic invariant — a line never appears as both Equal and Delete/Insert —
// catches a broken DP table (swapped indices, off-by-one in backtrack).
func TestCompute_Modify(t *testing.T) {
	ops := Compute("a\nb\nc", "a\nx\nc")
	var kinds []OpKind
	for _, o := range ops {
		kinds = append(kinds, o.Kind)
	}
	require.Equal(t, []OpKind{Equal, Delete, Insert, Equal}, kinds)
	assert.Equal(t, "b", ops[1].Line)
	assert.Equal(t, "x", ops[2].Line)
}

// TestCompute_LineNumbers proves OldLine/NewLine are 1-based and 0 on the
// side that does not apply.
func TestCompute_LineNumbers(t *testing.T) {
	ops := Compute("a\nb\nc", "a\nx\nc")
	// a: Equal, old=1 new=1
	assert.Equal(t, Op{Kind: Equal, Line: "a", OldLine: 1, NewLine: 1}, ops[0])
	// b: Delete, old=2 new=0
	assert.Equal(t, Op{Kind: Delete, Line: "b", OldLine: 2, NewLine: 0}, ops[1])
	// x: Insert, old=0 new=2
	assert.Equal(t, Op{Kind: Insert, Line: "x", OldLine: 0, NewLine: 2}, ops[2])
	// c: Equal, old=3 new=3
	assert.Equal(t, Op{Kind: Equal, Line: "c", OldLine: 3, NewLine: 3}, ops[3])
}

// TestCompute_AllInsert proves the all-insert case (empty old) yields only
// Insert ops with correctly increasing NewLine.
func TestCompute_AllInsert(t *testing.T) {
	ops := Compute("", "x\ny")
	require.Len(t, ops, 2)
	assert.Equal(t, Op{Kind: Insert, Line: "x", NewLine: 1}, ops[0])
	assert.Equal(t, Op{Kind: Insert, Line: "y", NewLine: 2}, ops[1])
}

// TestCompute_AllDelete proves the all-delete case (empty new) yields only
// Delete ops with correctly increasing OldLine.
func TestCompute_AllDelete(t *testing.T) {
	ops := Compute("x\ny", "")
	require.Len(t, ops, 2)
	assert.Equal(t, Op{Kind: Delete, Line: "x", OldLine: 1}, ops[0])
	assert.Equal(t, Op{Kind: Delete, Line: "y", OldLine: 2}, ops[1])
}

// TestCompute_BothEmpty proves both-empty input yields a nil/empty op slice.
func TestCompute_BothEmpty(t *testing.T) {
	assert.Empty(t, Compute("", ""))
}

// TestCompute_Identical proves equal inputs produce only Equal ops.
func TestCompute_Identical(t *testing.T) {
	ops := Compute("a\nb", "a\nb")
	for _, o := range ops {
		assert.Equal(t, Equal, o.Kind)
	}
	require.Len(t, ops, 2)
}

// TestCompact_OmitsContext proves Compact drops Equal lines and keeps only
// sigil-prefixed +/- lines, each newline-terminated.
func TestCompact_OmitsContext(t *testing.T) {
	got := Compact(Compute("line1\nold\nline3\n", "line1\nnew\nline3\n"))
	assert.Contains(t, got, "-old\n")
	assert.Contains(t, got, "+new\n")
	assert.NotContains(t, got, "line1")
	assert.NotContains(t, got, "line3")
}

// TestCompact_NoChange proves Compact returns "" when nothing differs.
func TestCompact_NoChange(t *testing.T) {
	assert.Equal(t, "", Compact(Compute("same\nsame\n", "same\nsame\n")))
}

// TestUnified_KeepsContext proves Unified renders every line, context
// included, with the leading sigil byte per line.
func TestUnified_KeepsContext(t *testing.T) {
	got := Unified(Compute("a\nb\nc", "a\nx\nc"))
	if !strings.Contains(got, "-b") || !strings.Contains(got, "+x") {
		t.Fatalf("diff missing add/remove lines:\n%s", got)
	}
	if !strings.Contains(got, " a") || !strings.Contains(got, " c") {
		t.Fatalf("diff missing context lines:\n%s", got)
	}
}

// TestUnified_Identical proves an all-equal diff renders only context (" ")
// lines, never + or -.
func TestUnified_Identical(t *testing.T) {
	got := Unified(Compute("a\nb", "a\nb"))
	for _, ln := range strings.Split(got, "\n") {
		if len(ln) == 0 || ln[0] != ' ' {
			t.Fatalf("identical inputs should be all-context: got %q in %q", ln, got)
		}
	}
}

// TestSplitLines_TrailingNewline proves SplitLines drops exactly one trailing
// newline artifact but keeps a genuine trailing blank line (two trailing
// newlines) as an empty final element — the behavior that reconciled the two
// predecessor implementations (see SplitLines's doc comment).
func TestSplitLines_TrailingNewline(t *testing.T) {
	assert.Nil(t, SplitLines(""))
	assert.Equal(t, []string{"a", "b"}, SplitLines("a\nb\n"))
	assert.Equal(t, []string{"a", "b"}, SplitLines("a\nb"))
	assert.Equal(t, []string{"a", ""}, SplitLines("a\n\n"))
}

// TestSplitLines_CRLF proves \r\n and lone \r both fold to \n before
// splitting.
func TestSplitLines_CRLF(t *testing.T) {
	assert.Equal(t, []string{"a", "b"}, SplitLines("a\r\nb"))
	assert.Equal(t, []string{"a", "b"}, SplitLines("a\rb"))
}
