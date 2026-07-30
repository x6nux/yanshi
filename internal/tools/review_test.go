package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/x6nux/yanshi/internal/task/work"
)

// makeManyFileDiff builds a deterministic large diff: n files, each with
// `lines` added lines of patterned content. Used to drive chunk-loop tests
// that require more than one chunk.
func makeManyFileDiff(n, lines int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		b.WriteString("--- a/pkg/file")
		b.WriteString(strconv.Itoa(i))
		b.WriteString(".go\n+++ b/pkg/file")
		b.WriteString(strconv.Itoa(i))
		b.WriteString(".go\n@@ -0,0 +1,")
		b.WriteString(strconv.Itoa(lines))
		b.WriteString(" @@\n")
		for j := 0; j < lines; j++ {
			b.WriteString("+line content ")
			b.WriteString(strconv.Itoa(i))
			b.WriteString("-")
			b.WriteString(strconv.Itoa(j))
			b.WriteString("\n")
		}
	}
	return b.String()
}

func TestChunkDiffSplitsAt48KiBBoundary(t *testing.T) {
	// Each file = header + N lines (~19 bytes each). 4 files * 800 lines ≈ 60 KiB.
	diff := makeManyFileDiff(4, 800)
	chunks := chunkDiff(diff, 48*1024)
	if len(chunks) < 2 {
		t.Fatalf("expected >=2 chunks, got %d", len(chunks))
	}
	for i, c := range chunks {
		if len(c) > 48*1024+64 {
			t.Fatalf("chunk %d too big: %d", i, len(c))
		}
	}
	joined := strings.Join(chunks, "")
	if !strings.Contains(joined, "file0.go") || !strings.Contains(joined, "file3.go") {
		t.Fatalf("chunks lost file markers")
	}
}

func TestChunkDiffPreservesCompleteHunks(t *testing.T) {
	diff := "header line\n--- a/x.go\n+++ b/x.go\n@@ -0,0 +1,3 @@\n+a\n+b\n+c\n"
	chunks := chunkDiff(diff, 20)
	joined := strings.Join(chunks, "")
	if !strings.Contains(joined, "+a\n+b\n+c\n") {
		t.Fatalf("hunk fragmented: %q", joined)
	}
}

// TestChunkDiffZeroLimitDefaults covers the limit<=0 default branch at
// review_chunk.go:12-14. A zero/negative limit falls back to reviewChunkLimit.
func TestChunkDiffZeroLimitDefaults(t *testing.T) {
	small := "short diff\n"
	chunks := chunkDiff(small, 0)
	if len(chunks) != 1 || chunks[0] != small {
		t.Fatalf("zero-limit chunks=%q", chunks)
	}
}

// TestChunkDiffLongLineFallback covers the cut<=0 hard-split fallback at
// review_chunk.go:41-43. A single line longer than limit forces a mid-buffer
// cut because there is no newline within c[:limit].
func TestChunkDiffLongLineFallback(t *testing.T) {
	// One hunk with a single very long line (no newline within the limit).
	long := strings.Repeat("x", 100)
	diff := "@@ -0,0 +1 @@" + long + "\n"
	chunks := chunkDiff(diff, 20)
	joined := strings.Join(chunks, "")
	if joined != diff {
		t.Fatalf("joined=%q want %q", joined, diff)
	}
	if len(chunks) < 2 {
		t.Fatalf("expected hard-split into multiple chunks, got %d", len(chunks))
	}
}

func TestStreamReviewInvokesSubAgentPerChunk(t *testing.T) {
	manager := work.NewFakeManager()
	ctx := WithWorkRoot(WithTaskManager(context.Background(), manager), t.TempDir())
	inject := func(ctx context.Context, prompt string, allowedTools []string, instructionOverride string) (string, error) {
		return `{"findings":[{"file":"pkg/x.go","line":1,"severity":"high","message":"bug"}]}`, nil
	}
	ctx = WithSubAgentRunner(ctx, inject)
	diff := makeManyFileDiff(2, 800)
	out, err := streamReview(ctx, reviewInput{Diff: diff, TaskID: "task-7"})
	if err != nil {
		t.Fatal(err)
	}
	var res reviewResult
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatal(err)
	}
	if res.ChunksReviewed < 1 || len(res.Findings) < 1 {
		t.Fatalf("res=%+v", res)
	}
	if res.Findings[0].File != "pkg/x.go" || res.Findings[0].Severity != "high" {
		t.Fatalf("finding=%+v", res.Findings[0])
	}
}

// TestStreamReviewRunnerError covers the runner-error branch at review.go:41-48.
// A failing runner is logged as an info finding, not a fatal error.
func TestStreamReviewRunnerError(t *testing.T) {
	manager := work.NewFakeManager()
	ctx := WithWorkRoot(WithTaskManager(context.Background(), manager), t.TempDir())
	inject := func(ctx context.Context, prompt string, allowedTools []string, instructionOverride string) (string, error) {
		return "", fmt.Errorf("sub-agent crashed")
	}
	ctx = WithSubAgentRunner(ctx, inject)
	out, err := streamReview(ctx, reviewInput{Diff: "small diff\n", TaskID: "task-7"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "sub-agent failed") {
		t.Fatalf("expected runner-error finding, got %s", out)
	}
}

// TestStreamReviewDecodeError covers the decode-error branch at review.go:50-56.
// A runner returning invalid JSON (with a { so extractJSONObject extracts it) is
// logged as an info finding.
func TestStreamReviewDecodeError(t *testing.T) {
	manager := work.NewFakeManager()
	ctx := WithWorkRoot(WithTaskManager(context.Background(), manager), t.TempDir())
	inject := func(ctx context.Context, prompt string, allowedTools []string, instructionOverride string) (string, error) {
		return "{not valid json}", nil
	}
	ctx = WithSubAgentRunner(ctx, inject)
	out, err := streamReview(ctx, reviewInput{Diff: "small diff\n", TaskID: "task-7"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "decode failed") {
		t.Fatalf("expected decode-error finding, got %s", out)
	}
}

func TestStreamReviewDedupesAndSortsFindings(t *testing.T) {
	calls := 0
	inject := func(ctx context.Context, prompt string, allowedTools []string, instructionOverride string) (string, error) {
		calls++
		return `{"findings":[{"file":"a.go","line":1,"severity":"high","message":"dup"}]}`, nil
	}
	ctx := WithWorkRoot(WithTaskManager(context.Background(), work.NewFakeManager()), t.TempDir())
	ctx = WithSubAgentRunner(ctx, inject)
	out, err := streamReview(ctx, reviewInput{Diff: makeManyFileDiff(3, 900), TaskID: "t1"})
	if err != nil {
		t.Fatal(err)
	}
	var res reviewResult
	_ = json.Unmarshal([]byte(out), &res)
	if calls < 2 {
		t.Fatalf("expected >=2 sub-agent calls, got %d", calls)
	}
	if len(res.Findings) != 1 {
		t.Fatalf("expected dedupe to 1, got %d (%+v)", len(res.Findings), res.Findings)
	}
}

func TestStreamReviewWritesArtifactWhenDiffTooLarge(t *testing.T) {
	manager := work.NewFakeManager()
	ctx := WithWorkRoot(WithTaskManager(context.Background(), manager), t.TempDir())
	// Return large findings to trigger the artifact spill path.
	inject := func(ctx context.Context, prompt string, allowedTools []string, instructionOverride string) (string, error) {
		// Each chunk returns enough findings to exceed SpillThreshold when combined.
		var findings []string
		for i := 0; i < 500; i++ {
			findings = append(findings, fmt.Sprintf(`{"file":"file%d.go","line":%d,"severity":"medium","message":"%s"}`,
				i, i, strings.Repeat("x", 100)))
		}
		return `{"findings":[` + strings.Join(findings, ",") + `]}`, nil
	}
	ctx = WithSubAgentRunner(ctx, inject)
	out, err := streamReview(ctx, reviewInput{Diff: makeManyFileDiff(5, 1500), TaskID: "task-7"})
	if err != nil {
		t.Fatal(err)
	}
	var res reviewResult
	_ = json.Unmarshal([]byte(out), &res)
	artifacts, _ := manager.ListArtifacts(context.Background(), "task-7")
	if len(artifacts) < 1 {
		t.Fatalf("expected artifact stored, got %d (res=%+v)", len(artifacts), res)
	}
}
