package tools

import (
	"context"
	"testing"
	"time"
)

// TestWithToolChunkCallback covers WithToolChunkCallback.
func TestWithToolChunkCallback(t *testing.T) {
	// With nil callback — should return ctx unchanged.
	base := context.Background()
	ctx := WithToolChunkCallback(base, nil)
	if ctx != base {
		t.Fatal("WithToolChunkCallback(nil) should return ctx unchanged")
	}
	// With non-nil callback — should store in context.
	var got ToolChunk
	cb := func(_ string, c ToolChunk) { got = c }
	ctx2 := WithToolChunkCallback(base, cb)
	if ctx2 == base {
		t.Fatal("WithToolChunkCallback(non-nil) should return new ctx")
	}
	retrieved := ToolChunkCallbackFromContext(ctx2)
	if retrieved == nil {
		t.Fatal("ToolChunkCallbackFromContext should return non-nil callback")
	}
	chunk := ToolChunk{Text: "hello", Result: "world", Status: "ok"}
	retrieved("test", chunk)
	if got.Text != "hello" || got.Result != "world" || got.Status != "ok" {
		t.Fatalf("callback did not capture chunk: %+v", got)
	}
	// Unbound context — should return nil.
	unbound := ToolChunkCallbackFromContext(base)
	if unbound != nil {
		t.Fatal("ToolChunkCallbackFromContext on bare ctx should return nil")
	}
}

// TestGuardedToolDisplayName covers DisplayName.
func TestGuardedToolDisplayName(t *testing.T) {
	g := NewGuardedTool("test_tool", "Test Tool", "a test", time.Second, nil, nil)
	if g.DisplayName() != "Test Tool" {
		t.Fatalf("DisplayName: got %q, want %q", g.DisplayName(), "Test Tool")
	}
}

// TestGuardedToolDefaultTimeout covers DefaultTimeout.
func TestGuardedToolDefaultTimeout(t *testing.T) {
	g := NewGuardedTool("test_tool", "Test", "desc", 30*time.Second, nil, nil)
	if g.DefaultTimeout() != 30*time.Second {
		t.Fatalf("DefaultTimeout: got %v, want %v", g.DefaultTimeout(), 30*time.Second)
	}
}
