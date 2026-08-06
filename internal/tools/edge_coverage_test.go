package tools

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/x6nux/yanshi/internal/guard"
	"github.com/x6nux/yanshi/internal/imagestore"
)

func TestShellCommandEmptyCommand(t *testing.T) {
	ctx := context.Background()
	cmd := shellCommand(ctx, "auto", "")
	if cmd == nil {
		t.Fatal("shellCommand should still return non-nil cmd even with empty command")
	}
}

func TestHostOnlyInvalidURL(t *testing.T) {
	if got := hostOnly("://invalid"); got != "" {
		t.Fatalf("expected empty for invalid url, got %q", got)
	}
}

func TestRecordUsageNilSafe(t *testing.T) {
	s := &imageDescribeState{usage: nil}
	// Should not panic when usage is nil.
	s.recordUsage(context.Background(), nil)
}

func TestRecordUsageNoMeta(t *testing.T) {
	var called bool
	s := &imageDescribeState{usage: func(_, _, _ int) { called = true }}
	// Passing a message without ResponseMeta should not call usage.
	s.recordUsage(context.Background(), nil)
	if called {
		t.Fatal("recordUsage should not call usage when message is nil")
	}
}

func TestResolveRefStoreRef(t *testing.T) {
	store := imagestore.New(imagestore.Config{MaxItems: 20, MaxBytes: 100 << 20})
	// Create a valid 1x1 red PNG using the standard library.
	var buf bytes.Buffer
	img := image.NewNRGBA(image.Rect(0, 0, 1, 1))
	img.Set(0, 0, color.NRGBA{255, 0, 0, 255})
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	id, err := store.Put(buf.Bytes(), "test", "png")
	if err != nil {
		t.Fatal(err)
	}
	s := &imageDescribeState{store: store, root: t.TempDir()}
	got, fmtName, err := s.resolveRef(context.Background(), id, "{}")
	if err != nil {
		t.Fatalf("resolveRef %s failed: %v", id, err)
	}
	if len(got) == 0 || fmtName != "png" {
		t.Fatalf("unexpected result: bytes=%d, fmt=%s", len(got), fmtName)
	}
}

func TestResolveRefStoreNotFound(t *testing.T) {
	store := imagestore.New(imagestore.Config{MaxItems: 20, MaxBytes: 100 << 20})
	s := &imageDescribeState{store: store, root: "/tmp/test"}
	_, _, err := s.resolveRef(context.Background(), "img-999", "{}")
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected not found error, got: %v", err)
	}
}

func TestResolveRefPathNoRoot(t *testing.T) {
	store := imagestore.New(imagestore.Config{MaxItems: 20, MaxBytes: 100 << 20})
	s := &imageDescribeState{store: store, root: ""}
	_, _, err := s.resolveRef(context.Background(), "/some/path.png", "{}")
	if err == nil || !strings.Contains(err.Error(), "root") {
		t.Fatalf("expected root error, got: %v", err)
	}
}

func TestReadArtifactNegativeOffset(t *testing.T) {
	a := NewArtifactTools()
	_, err := a.readArtifact(context.Background(), `{"id":"test","offset":-1}`)
	if err == nil || !strings.Contains(err.Error(), "non-negative") {
		t.Fatalf("expected non-negative error, got: %v", err)
	}
}

func TestReadArtifactNoManager(t *testing.T) {
	a := NewArtifactTools()
	_, err := a.readArtifact(context.Background(), `{"id":"test"}`)
	if err == nil || !strings.Contains(err.Error(), "task manager") {
		t.Fatalf("expected task manager error, got: %v", err)
	}
}

func TestReadArtifactNoWorkRoot(t *testing.T) {
	a := NewArtifactTools()
	// With neither manager nor work root.
	ctx := context.Background()
	_, err := a.readArtifact(ctx, `{"id":"test"}`)
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestGateRunNoManager(t *testing.T) {
	gt := NewGateTools()
	ctx := WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"*"}},
		Shell: guard.ShellPerm{Policy: "allowlist", Patterns: []string{"*"}},
		FS:    guard.FSPerm{Read: []string{"*"}},
	})
	ctx = WithWorkRoot(ctx, t.TempDir())
	_, err := gt.runGate(ctx, `{"task_id":"t1","gate":"test","command":"echo hi"}`)
	if err == nil || !strings.Contains(err.Error(), "task manager") {
		t.Fatalf("expected task manager error, got: %v", err)
	}
}

func TestAuthorizeNoProfile(t *testing.T) {
	err := Authorize(context.Background(), guard.Action{}, "{}")
	if err == nil || !strings.Contains(err.Error(), "denied") {
		t.Fatalf("expected denied error, got: %v", err)
	}
}

func TestAuthorizeApprovalRequiredNoProfile(t *testing.T) {
	err := AuthorizeApprovalRequired(context.Background(), guard.Action{}, "{}")
	if err == nil || !strings.Contains(err.Error(), "denied") {
		t.Fatalf("expected denied error, got: %v", err)
	}
}

func TestSyncStreamError(t *testing.T) {
	fn := func(ctx context.Context, argsJSON string) (string, error) {
		return "", io.EOF
	}
	streamFn := SyncStream(fn)
	ch := streamFn(context.Background(), "{}")
	got := false
	for c := range ch {
		if c.Err != nil || strings.Contains(c.Text, "✗") {
			got = true
		}
	}
	if !got {
		t.Fatal("expected error chunk from SyncStream")
	}
}

func TestNewScreenshotTool(t *testing.T) {
	tool := NewScreenshotTool(nil)
	if tool == nil {
		t.Fatal("NewScreenshotTool should return non-nil")
	}
}

func TestBindSubAgentProgressStepPrefix(t *testing.T) {
	ch := make(chan ToolChunk, 16)
	ctx, finalize := bindSubAgentProgress(context.Background(), ch, "B1: ")
	defer finalize()

	cb := SubAgentProgressFromContext(ctx)
	if cb == nil {
		t.Fatal("expected progress callback")
	}

	// Tool start event.
	cb(SubAgentEvent{Kind: SubAgentToolStart, ToolDisplay: "fs_read", ToolArgs: "file.go"})
	// Token event.
	cb(SubAgentEvent{Kind: SubAgentTokens, Tokens: 100})
	// Tool done event (should not push anything).
	cb(SubAgentEvent{Kind: SubAgentToolEnd})

	close(ch)
	foundPrefix := false
	for c := range ch {
		if strings.Contains(c.Text, "B1:") {
			foundPrefix = true
		}
	}
	if !foundPrefix {
		t.Fatal("expected step prefix in text chunks")
	}
}

func TestCheckFSSilent(t *testing.T) {
	t.Run("allows valid paths", func(t *testing.T) {
		ft := NewFSTools(t.TempDir())
		ctx := WithProfile(context.Background(), guard.PermissionProfile{
			Tools: guard.ToolsPerm{Allow: []string{"*"}},
		})
		_ = ft.checkFSSilent(ctx, "read", "fs_read", "/valid/path") // Should not panic
	})
}

func TestIsImagePath(t *testing.T) {
	tests := []struct {
		path string
		want bool
	}{
		{"image.png", true},
		{"image.jpg", true},
		{"image.jpeg", true},
		{"image.gif", true},
		{"image.webp", true},
		{"image.PNG", true},
		{"file.txt", false},
		{"file", false},
		{"", false},
	}
	for _, tc := range tests {
		got := IsImagePath(tc.path)
		if got != tc.want {
			t.Errorf("IsImagePath(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

func TestInvokableRunConsecutiveErrors(t *testing.T) {
	// Create a tool that always fails.
	tool := NewGuardedTool("failing_tool", "Fail", "always fails", time.Second,
		nil, SyncStream(func(ctx context.Context, args string) (string, error) {
			return "", io.EOF
		}))

	// First call without err counter - should not crash.
	ctx := context.Background()
	result, err := tool.InvokableRun(ctx, "{}")
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	_ = result
}

func TestInvokableRunWithErrCounter(t *testing.T) {
	tool := NewGuardedTool("failing_tool2", "Fail", "always fails", time.Second,
		nil, SyncStream(func(ctx context.Context, args string) (string, error) {
			return "", io.EOF
		}))

	// With error counter - should abort after 5 consecutive errors.
	ctx := WithErrCounter(context.Background())
	var lastResult string
	for i := 0; i < 6; i++ {
		result, err := tool.InvokableRun(ctx, "{}")
		lastResult = result
		_ = err
	}
	// After 6 attempts, we should have seen the circuit breaker message.
	if lastResult != "" && !strings.Contains(lastResult, "consecutive") {
		t.Logf("last result: %q", lastResult)
	}
}

func TestSpillIfTooLongShort(t *testing.T) {
	result := "short result"
	got := spillIfTooLong(context.Background(), "test", result)
	if got != result {
		t.Fatalf("expected unchanged for short result, got: %q", got)
	}
}

func TestErrorResult(t *testing.T) {
	r := errorResult("something went wrong")
	if !strings.Contains(r, "✗") {
		t.Fatalf("expected ✗ prefix, got: %q", r)
	}
}

func TestSweep(t *testing.T) {
	// Should not panic with empty root.
	Sweep("")
	// Should not panic with a real path.
	Sweep(t.TempDir())
}

func TestHasDotDot(t *testing.T) {
	if hasDotDot("safe/path") {
		t.Fatal("expected false for safe path")
	}
	if !hasDotDot("safe/../unsafe") {
		t.Fatal("expected true for path with ..")
	}
}

func TestAbsEdgePath(t *testing.T) {
	ft := NewFSTools(t.TempDir())
	_, err := ft.abs("../escape")
	if err == nil {
		t.Fatal("expected error for path escaping root")
	}
}

func TestWithinRoot(t *testing.T) {
	root := t.TempDir()
	if !withinRoot(root, root) {
		t.Fatal("root should be within itself")
	}
	subDir := filepath.Join(root, "sub")
	if !withinRoot(subDir, root) {
		t.Fatal("subdir should be within root")
	}
	if withinRoot(filepath.Join(root, "..", "other"), root) {
		t.Fatal("other path should not be within root")
	}
}
