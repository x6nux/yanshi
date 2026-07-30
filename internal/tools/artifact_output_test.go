package tools

import (
	"context"
	"strings"
	"testing"

	"github.com/x6nux/yanshi/internal/task/work"
)

func TestWriteArtifactOrSpillUsesTaskManager(t *testing.T) {
	manager := work.NewFakeManager()
	root := t.TempDir()
	ctx := WithWorkRoot(WithTaskManager(context.Background(), manager), root)
	content := strings.Repeat("x", SpillThreshold+1)
	got := writeArtifactOrSpill(ctx, "task-7", "git-diff", content)
	if got.Degraded || got.ArtifactRef == "" || got.Size != int64(len(content)) {
		t.Fatalf("got=%+v", got)
	}
	stored, err := manager.ReadArtifact(ctx, got.ArtifactRef)
	if err != nil {
		t.Fatal(err)
	}
	if stored.TaskID != "task-7" || stored.Label != "git-diff" || stored.Size != int64(len(content)) {
		t.Fatalf("stored=%+v", stored)
	}
}

func TestWriteArtifactOrSpillMarksFallbackDegraded(t *testing.T) {
	ctx := WithWorkRoot(context.Background(), t.TempDir())
	got := writeArtifactOrSpill(ctx, "task-7", "git-diff", strings.Repeat("x", SpillThreshold+1))
	if !got.Degraded || got.ArtifactRef != "" {
		t.Fatalf("got=%+v", got)
	}
	if !strings.HasPrefix(got.Summary, "[degraded: task artifact manager unavailable; using temporary spillover]\n") {
		t.Fatalf("summary=%q", got.Summary)
	}
}

// TestWriteArtifactOrSpillNoWorkRoot covers the root=="" default branch at
// artifact_output.go:18-19: when no WorkRoot is set on the context, the
// artifact is written under ".".
func TestWriteArtifactOrSpillNoWorkRoot(t *testing.T) {
	manager := work.NewFakeManager()
	// No WithWorkRoot: root defaults to "." inside writeArtifactOrSpill.
	ctx := WithTaskManager(context.Background(), manager)
	content := strings.Repeat("x", SpillThreshold+1)
	got := writeArtifactOrSpill(ctx, "task-7", "git-diff", content)
	if got.Degraded || got.ArtifactRef == "" {
		t.Fatalf("expected successful artifact, got=%+v", got)
	}
}
