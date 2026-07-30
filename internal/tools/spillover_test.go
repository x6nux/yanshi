package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSpillIfTooLong_UnderThresholdUnchanged(t *testing.T) {
	ctx := WithWorkRoot(context.Background(), t.TempDir())
	in := strings.Repeat("x", SpillThreshold) // exactly at threshold
	got := spillIfTooLong(ctx, "shell_run", in)
	assert.Equal(t, in, got, "result at threshold must pass through unchanged")
}

func TestSpillIfTooLong_OverThresholdSpills(t *testing.T) {
	root := t.TempDir()
	ctx := WithWorkRoot(context.Background(), root)
	var b strings.Builder
	for i := 0; i < 200; i++ {
		b.WriteString(strings.Repeat("a", 1023) + "\n") // ~1 KiB/line → ~200 KiB
	}
	in := b.String()
	require.Greater(t, len(in), SpillThreshold)

	got := spillIfTooLong(ctx, "shell_run", in)
	assert.Contains(t, got, "[spilled:")
	assert.Contains(t, got, ".yanshi/tmp/spillover/shell_run-", "temp path surfaced to model")
	assert.Contains(t, got, "lines omitted")
	assert.Contains(t, got, "Use summarize(path)")

	// Exactly one temp file written, holding the full original result.
	entries, err := os.ReadDir(filepath.Join(root, spillDir))
	require.NoError(t, err)
	require.Len(t, entries, 1, "exactly one spill file")
	data, err := os.ReadFile(filepath.Join(root, spillDir, entries[0].Name()))
	require.NoError(t, err)
	assert.Equal(t, in, string(data), "spill file holds the full original result")
}

func TestSpillIfTooLong_FallsBackToDot(t *testing.T) {
	t.Cleanup(func() { _ = os.RemoveAll(filepath.Join(".", spillDir)) })
	in := strings.Repeat("x", SpillThreshold+1)
	got := spillIfTooLong(context.Background(), "web_fetch", in) // no WithWorkRoot
	assert.Contains(t, got, "[spilled:")
	assert.Less(t, len(got), len(in), "preview must be shorter than the original")
}

func TestSpillPreview_HeadAndTail(t *testing.T) {
	pad := strings.Repeat("x", 700)
	var lines []string
	for i := 0; i < 100; i++ {
		lines = append(lines, fmt.Sprintf("LINE-%03d %s", i, pad))
	}
	in := strings.Join(lines, "\n")
	require.Greater(t, len(in), SpillThreshold)

	got := spillPreview(in, ".yanshi/tmp/spillover/x.txt")
	assert.Contains(t, got, "LINE-000", "head includes first line")
	assert.Contains(t, got, "LINE-014", "head includes 15th line (spillHeadLines)")
	assert.Contains(t, got, "lines omitted")
	assert.Contains(t, got, "LINE-099", "tail includes last line")
	assert.NotContains(t, got, "LINE-050", "middle line must be omitted")
}

func TestSpillPreview_SmallTotalShowsRemainderAsTail(t *testing.T) {
	// 20 lines (total between head and head+tail): head=15, then a 5-line
	// remainder tail with no omission. spillPreview is a pure formatter that
	// does not check SpillThreshold, so no byte-size precondition is needed;
	// keeping lines small also ensures the 15-line head fits spillHeadBudget.
	pad := strings.Repeat("x", 700)
	var lines []string
	for i := 0; i < 20; i++ {
		lines = append(lines, fmt.Sprintf("LINE-%03d %s", i, pad))
	}
	in := strings.Join(lines, "\n")

	got := spillPreview(in, ".yanshi/tmp/spillover/x.txt")
	assert.Contains(t, got, "LINE-000", "head present")
	assert.Contains(t, got, "LINE-014", "head ends at 15th line")
	assert.Contains(t, got, "LINE-019", "tail remainder includes last line")
	assert.NotContains(t, got, "lines omitted", "nothing omitted when total ≤ head+tail")
}

func TestSweep_RemovesSpillFiles(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, spillDir)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.txt"), []byte("x"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "b.txt"), []byte("y"), 0o644))

	Sweep(root)
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Empty(t, entries, "Sweep must remove all spill files")
}

func TestSweep_MissingDirNoOp(t *testing.T) {
	Sweep(t.TempDir()) // must not panic / error
}
