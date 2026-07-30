package tools

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUnifiedDiff_Modify(t *testing.T) {
	a := "line1\nold\nline3\n"
	b := "line1\nnew\nline3\n"
	got := unifiedDiff(a, b)
	// context lines omitted; only the change shows as -old / +new.
	assert.Contains(t, got, "-old\n")
	assert.Contains(t, got, "+new\n")
	assert.NotContains(t, got, "line1", "unchanged lines are omitted")
	assert.NotContains(t, got, "line3")
}

func TestUnifiedDiff_AllAdded(t *testing.T) {
	got := unifiedDiff("", "a\nb\n")
	require.True(t, strings.HasPrefix(got, "+a\n"), "got %q", got)
	assert.Contains(t, got, "+b\n")
}

func TestUnifiedDiff_AllRemoved(t *testing.T) {
	got := unifiedDiff("a\nb\n", "")
	assert.Contains(t, got, "-a\n")
	assert.Contains(t, got, "-b\n")
	assert.NotContains(t, got, "+")
}

func TestUnifiedDiff_NoChange(t *testing.T) {
	assert.Equal(t, "", unifiedDiff("same\nsame\n", "same\nsame\n"))
}

func TestSplitDiffLines(t *testing.T) {
	assert.Nil(t, splitDiffLines(""))
	assert.Equal(t, []string{"a", "b"}, splitDiffLines("a\nb\n"))
	assert.Equal(t, []string{"a", "b"}, splitDiffLines("a\nb")) // no trailing newline
}
